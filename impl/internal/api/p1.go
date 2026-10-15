// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"net/http"
	"strings"

	"github.com/HunterEwok/botaru/impl/internal/core"
)

// P1Handler serves /v1/p1/<operation>, /v1/p3/mcp and, if configured, /v1/p4/.
func (s *Server) P1Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/p1/", s.p1)
	mux.HandleFunc("/v1/p3/", s.p3)
	if s.p4 != nil {
		mux.Handle("/v1/p4/", s.p4)
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		Problem(w, &core.Problem{Reason: "not_found", Detail: "no such path"})
	})
	return mux
}

func (s *Server) p1(w http.ResponseWriter, r *http.Request) {
	op := strings.TrimPrefix(r.URL.Path, "/v1/p1/")
	info, ok := s.ops["P1."+op]
	if !ok {
		Problem(w, &core.Problem{Reason: "not_found", Detail: "no such operation"})
		return
	}
	if r.Method != http.MethodPost {
		Problem(w, &core.Problem{Reason: "not_found", Detail: "operations are POST"})
		return
	}
	if err := s.c.P1Ready(op); err != nil {
		s.fail(w, err)
		return
	}
	ctx := r.Context()
	// The session and memory operations take the handle of the lease as the bearer (MEM-1).
	if strings.HasPrefix(op, "session.") || strings.HasPrefix(op, "memory.") {
		m, ok := s.body(w, r, info.Request)
		if !ok {
			return
		}
		out, err := s.handleOp(ctx, op, bearer(r), m)
		if err != nil {
			s.fail(w, err)
			return
		}
		writeJSON(w, out)
		return
	}
	wk, err := s.c.AuthenticateWorker(ctx, bearer(r))
	if err != nil {
		s.fail(w, err)
		return
	}
	if op == "file.put" {
		s.filePut(ctx, w, r, wk, info.Request)
		return
	}
	m, ok := s.body(w, r, info.Request)
	if !ok {
		return
	}
	if op == "file.get" {
		s.fileGet(ctx, w, r, wk, m)
		return
	}
	lease := r.Header.Get("Botaru-Lease")
	var out any
	switch op {
	case "hello":
		req, err := decode[core.HelloRequest](m)
		if err != nil {
			s.fail(w, err)
			return
		}
		out, err = s.c.Hello(ctx, wk, req)
		if err != nil {
			s.fail(w, err)
			return
		}
	case "claim":
		req, err := decode[core.ClaimRequest](m)
		if err != nil {
			s.fail(w, err)
			return
		}
		res, err := s.c.Claim(ctx, wk, req)
		if err != nil {
			s.fail(w, err)
			return
		}
		out = res
	case "heartbeat":
		req, err := decode[core.HeartbeatRequest](m)
		if err != nil {
			s.fail(w, err)
			return
		}
		out, err = s.c.Heartbeat(ctx, wk, lease, req)
		if err != nil {
			s.fail(w, err)
			return
		}
	case "complete":
		req, err := decode[core.CompleteRequest](m)
		if err != nil {
			s.fail(w, err)
			return
		}
		out, err = s.c.Complete(ctx, wk, lease, req)
		if err != nil {
			s.fail(w, err)
			return
		}
	case "fail":
		req, err := decode[core.FailRequest](m)
		if err != nil {
			s.fail(w, err)
			return
		}
		out, err = s.c.Fail(ctx, wk, lease, req)
		if err != nil {
			s.fail(w, err)
			return
		}
	case "release":
		out, err = s.c.Release(ctx, wk, lease)
		if err != nil {
			s.fail(w, err)
			return
		}
	case "emit":
		req, err := decode[core.EmitRequest](m)
		if err != nil {
			s.fail(w, err)
			return
		}
		out, err = s.c.Emit(ctx, wk, lease, req)
		if err != nil {
			s.fail(w, err)
			return
		}
	case "obs.query":
		req, err := decode[core.ObsQueryRequest](m)
		if err != nil {
			s.fail(w, err)
			return
		}
		out, err = s.c.ObsQuery(ctx, wk, lease, req)
		if err != nil {
			s.fail(w, err)
			return
		}
	case "submit_activity":
		req, err := decode[core.Activity](m)
		if err != nil {
			s.fail(w, err)
			return
		}
		out, err = s.c.SubmitActivity(ctx, wk, req)
		if err != nil {
			s.fail(w, err)
			return
		}
	case "decide":
		req, err := decode[core.DecideRequest](m)
		if err != nil {
			s.fail(w, err)
			return
		}
		out, err = s.c.Decide(ctx, wk, onBehalf(r), req)
		if err != nil {
			s.fail(w, err)
			return
		}
	case "card.details", "card.remind":
		req, err := decode[core.CardOp](m)
		if err != nil {
			s.fail(w, err)
			return
		}
		if req.Op != op {
			s.fail(w, &core.Problem{Reason: "input_schema_invalid", Detail: "the op of the body is not the operation of the path"})
			return
		}
		if op == "card.details" {
			out, err = s.c.CardDetails(ctx, wk, onBehalf(r), req)
		} else {
			out, err = s.c.CardRemind(ctx, wk, onBehalf(r), req)
		}
		if err != nil {
			s.fail(w, err)
			return
		}
	case "campaign.control":
		req, err := decode[core.CampaignControlRequest](m)
		if err != nil {
			s.fail(w, err)
			return
		}
		out, err = s.c.CampaignControl(ctx, wk, onBehalf(r), req)
		if err != nil {
			s.fail(w, err)
			return
		}
	case "campaign.get":
		req, err := decode[core.CampaignGetRequest](m)
		if err != nil {
			s.fail(w, err)
			return
		}
		out, err = s.c.CampaignGet(ctx, wk, lease, req)
		if err != nil {
			s.fail(w, err)
			return
		}
	case "script.control":
		req, err := decode[core.ScriptControlRequest](m)
		if err != nil {
			s.fail(w, err)
			return
		}
		out, err = s.c.ScriptControl(ctx, wk, onBehalf(r), req)
		if err != nil {
			s.fail(w, err)
			return
		}
	case "script.get":
		req, err := decode[core.ScriptGetRequest](m)
		if err != nil {
			s.fail(w, err)
			return
		}
		out, err = s.c.ScriptGet(ctx, wk, lease, req)
		if err != nil {
			s.fail(w, err)
			return
		}
	case "msg.send":
		req, err := decode[core.MsgSendRequest](m)
		if err != nil {
			s.fail(w, err)
			return
		}
		out, err = s.c.MsgSend(ctx, wk, lease, req)
		if err != nil {
			s.fail(w, err)
			return
		}
	case "msg.receive":
		req, err := decode[core.MsgReceiveRequest](m)
		if err != nil {
			s.fail(w, err)
			return
		}
		out, err = s.c.MsgReceive(ctx, wk, lease, req)
		if err != nil {
			s.fail(w, err)
			return
		}
	case "msg.ack", "msg.nack":
		req, err := decode[core.MsgAckRequest](m)
		if err != nil {
			s.fail(w, err)
			return
		}
		out, err = s.c.MsgAck(ctx, wk, lease, req, op == "msg.nack")
		if err != nil {
			s.fail(w, err)
			return
		}
	default:
		Problem(w, &core.Problem{Reason: "not_found", Detail: "the operation is not implemented in this build"})
		return
	}
	writeJSON(w, out)
}

// onBehalf reads the headers of a channel call on behalf of a person; without them, or with a
// malformed value, the core refuses the call as forbidden.
func onBehalf(r *http.Request) *core.OnBehalf {
	ob, ok := core.ParseOnBehalf(r.Header.Get("Botaru-On-Behalf"), r.Header.Get("Botaru-Chat"), r.Header.Get("Botaru-Message"))
	if !ok {
		return nil
	}
	return &ob
}

// handleOp runs a P1 operation authenticated by a session or memory handle.
func (s *Server) handleOp(ctx context.Context, op, handle string, m map[string]any) (any, error) {
	switch op {
	case "session.load":
		req, err := decode[core.SessionLoadRequest](m)
		if err != nil {
			return nil, err
		}
		return s.c.SessionLoad(ctx, handle, req)
	case "session.append":
		req, err := decode[core.SessionAppendRequest](m)
		if err != nil {
			return nil, err
		}
		return s.c.SessionAppend(ctx, handle, req)
	case "memory.search":
		req, err := decode[core.MemorySearchRequest](m)
		if err != nil {
			return nil, err
		}
		return s.c.MemorySearch(ctx, handle, req)
	case "memory.propose":
		req, err := decode[core.MemoryProposeRequest](m)
		if err != nil {
			return nil, err
		}
		return s.c.MemoryPropose(ctx, handle, req)
	}
	return nil, &core.Problem{Reason: "not_found", Detail: "the operation is not implemented in this build"}
}
