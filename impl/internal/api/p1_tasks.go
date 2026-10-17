// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"net/http"
	"strings"

	"github.com/HunterEwok/botaru/impl/internal/core"
)

// programOps are the P1 operations an external program calls with its key
// (spec/ports/p1-worker-protocol.md, "Called by").
var programOps = map[string]bool{"enqueue": true, "task.get": true, "cancel": true}

// program finds the program credential of a bearer value.
func (s *Server) program(token string) (ProgramCred, bool) {
	if token == "" {
		return ProgramCred{}, false
	}
	h := sha256Hex(token)
	for _, p := range s.cfg.Programs {
		if equalHex(h, strings.ToLower(p.TokenSHA256)) {
			return p, true
		}
	}
	return ProgramCred{}, false
}

// programCall serves a P1 call with a program key and reports whether the bearer was one.
func (s *Server) programCall(ctx context.Context, w http.ResponseWriter, r *http.Request, op string, info opInfo) bool {
	cred, ok := s.program(bearer(r))
	if !ok {
		return false
	}
	if !programOps[op] {
		Problem(w, &core.Problem{Reason: "forbidden", Detail: "a program key opens only enqueue, task.get and cancel"})
		return true
	}
	p, err := s.c.AuthenticateProgram(ctx, cred.Tenant, cred.KeyID)
	if err != nil {
		s.fail(w, err)
		return true
	}
	m, ok := s.opBody(w, r, "P1."+op, info)
	if !ok {
		return true
	}
	var out any
	switch op {
	case "enqueue":
		req, derr := decode[core.EnqueueRequest](m)
		if derr != nil {
			s.fail(w, derr)
			return true
		}
		out, err = s.c.ProgramEnqueue(ctx, p, req)
	case "task.get":
		req, derr := decode[core.TaskIDRequest](m)
		if derr != nil {
			s.fail(w, derr)
			return true
		}
		out, err = s.c.ProgramTaskGet(ctx, p, req)
	case "cancel":
		req, derr := decode[core.TaskIDRequest](m)
		if derr != nil {
			s.fail(w, derr)
			return true
		}
		out, err = s.c.ProgramCancel(ctx, p, req)
	}
	if err != nil {
		s.fail(w, err)
		return true
	}
	writeJSON(w, out)
	return true
}

// workerTaskOp serves enqueue and campaign.propose of a worker under its attempt lease; task.get
// and cancel are for the initiator or a program, not for a worker (PRM-9).
func (s *Server) workerTaskOp(ctx context.Context, wk *core.Worker, lease, op string, m map[string]any) (any, error) {
	switch op {
	case "enqueue":
		req, err := decode[core.EnqueueRequest](m)
		if err != nil {
			return nil, err
		}
		return s.c.LeaseEnqueue(ctx, wk, lease, req)
	case "campaign.propose":
		return s.c.P1CampaignPropose(ctx, wk, lease, m)
	}
	return nil, &core.Problem{Reason: "forbidden", Detail: "task.get and cancel are for the initiator or an external program"}
}
