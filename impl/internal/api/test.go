// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/HunterEwok/botaru/impl/internal/core"
)

// TestHandler serves the test-control port /v1/test/<operation> (spec/ports/test-control.md) on
// its own listener. Only the test bench key opens it: a worker token, an operator credential and
// a health key get unauthenticated. The deployment serves it only on a test bench (APR-9), and
// the core refuses every operation on a core without the test bench flag as well.
func (s *Server) TestHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/test/", s.test)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		Problem(w, &core.Problem{Reason: "not_found", Detail: "no such path"})
	})
	return mux
}

func (s *Server) isTestKey(token string) bool {
	if token == "" || s.cfg.TestKeySHA256 == "" {
		return false
	}
	return equalHex(sha256Hex(token), strings.ToLower(s.cfg.TestKeySHA256))
}

func (s *Server) test(w http.ResponseWriter, r *http.Request) {
	op := strings.TrimPrefix(r.URL.Path, "/v1/test/")
	info, ok := s.ops["T."+op]
	if !ok {
		Problem(w, &core.Problem{Reason: "not_found", Detail: "no such operation"})
		return
	}
	if r.Method != http.MethodPost {
		Problem(w, &core.Problem{Reason: "not_found", Detail: "operations are POST"})
		return
	}
	if !s.isTestKey(bearer(r)) {
		Problem(w, &core.Problem{Reason: "unauthenticated", Detail: "the test-control port takes only the test bench key"})
		return
	}
	m, ok := s.body(w, r, info.Request)
	if !ok {
		return
	}
	req, derr := decode[testRequest](m)
	if derr != nil {
		Problem(w, &core.Problem{Reason: "input_schema_invalid", Detail: "the body could not be read"})
		return
	}
	ctx := r.Context()
	var out any
	var err error
	switch op {
	case "clock.set", "clock.advance":
		if req.Op != "" && req.Op != op {
			Problem(w, &core.Problem{Reason: "input_schema_invalid", Detail: "op does not match the path"})
			return
		}
		if op == "clock.set" {
			at, perr := time.Parse(time.RFC3339Nano, req.At)
			if perr != nil {
				Problem(w, &core.Problem{Reason: "input_schema_invalid", Detail: "clock.set needs at"})
				return
			}
			out, err = s.c.TestClockSet(ctx, at)
		} else {
			if req.ByS == nil {
				Problem(w, &core.Problem{Reason: "input_schema_invalid", Detail: "clock.advance needs by_s"})
				return
			}
			out, err = s.c.TestClockAdvance(ctx, *req.ByS)
		}
	case "loop.tick":
		out, err = s.c.TestLoopTick(ctx, req.Loop, req.Copies)
	case "process.stop":
		r, derr := decode[core.ProcessStopRequest](m)
		if derr != nil {
			Problem(w, &core.Problem{Reason: "input_schema_invalid", Detail: "the body could not be read"})
			return
		}
		out, err = s.c.TestProcessStop(ctx, r)
	case "store.fault":
		// store.fault has no request schema (D-114); the core checks the fields it reads.
		r, derr := decode[core.StoreFaultRequest](m)
		if derr != nil {
			Problem(w, &core.Problem{Reason: "input_schema_invalid", Detail: "the body could not be read"})
			return
		}
		out, err = s.c.TestStoreFault(ctx, r)
	case "store.snapshot":
		out, err = s.c.TestStoreSnapshot(ctx, req.Label)
	case "store.restore":
		out, err = s.c.TestStoreRestore(ctx, req.Label)
	case "authorize.eval":
		// The body is the authz-request of a table row in request, with the tenant kind of the
		// row (conformance/runner.md, "Test bench").
		body, _ := m["request"].(map[string]any)
		kind, _ := m["tenant_kind"].(string)
		out, err = s.c.TestAuthorize(ctx, body, kind)
	case "function.eval":
		r, derr := decode[core.FunctionEvalRequest](m)
		if derr != nil {
			Problem(w, &core.Problem{Reason: "input_schema_invalid", Detail: "the body could not be read"})
			return
		}
		out, err = s.c.TestFunctionEval(ctx, r)
	case "event.inject":
		r, derr := decode[core.EventInjectRequest](m)
		if derr != nil {
			Problem(w, &core.Problem{Reason: "input_schema_invalid", Detail: "the body could not be read"})
			return
		}
		out, err = s.c.TestEventInject(ctx, r)
	default:
		Problem(w, &core.Problem{Reason: "not_found", Detail: "the operation is not implemented in this build"})
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, out)
}

// testRequest holds the fields of the test-control request schemas this build reads.
type testRequest struct {
	Op     string `json:"op"`
	At     string `json:"at"`
	ByS    *int64 `json:"by_s"`
	Loop   string `json:"loop"`
	Copies int    `json:"copies"`
	Label  string `json:"label"`
}
