// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
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
	wk, err := s.c.AuthenticateWorker(ctx, bearer(r))
	if err != nil {
		s.fail(w, err)
		return
	}
	m, ok := s.body(w, r, info.Request)
	if !ok {
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
	default:
		Problem(w, &core.Problem{Reason: "not_found", Detail: "the operation is not implemented in this build"})
		return
	}
	writeJSON(w, out)
}
