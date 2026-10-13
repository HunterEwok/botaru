// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"net/http"
	"strings"

	"github.com/HunterEwok/botaru/impl/internal/core"
)

// P2Handler serves /v1/p2/<operation> on the separate admin listener (IDN-7).
func (s *Server) P2Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/p2/", s.p2)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		Problem(w, &core.Problem{Reason: "not_found", Detail: "no such path"})
	})
	return mux
}

func (s *Server) operator(token string) (core.Operator, bool) {
	if token == "" {
		return core.Operator{}, false
	}
	h := sha256Hex(token)
	for _, o := range s.cfg.Operators {
		if equalHex(h, strings.ToLower(o.TokenSHA256)) {
			return core.Operator{Principal: o.Principal, Tenant: o.Tenant}, true
		}
	}
	return core.Operator{}, false
}

func (s *Server) isHealthKey(token string) bool {
	if token == "" {
		return false
	}
	h := sha256Hex(token)
	for _, k := range s.cfg.HealthKeysSHA256 {
		if equalHex(h, strings.ToLower(k)) {
			return true
		}
	}
	return false
}

func (s *Server) p2(w http.ResponseWriter, r *http.Request) {
	op := strings.TrimPrefix(r.URL.Path, "/v1/p2/")
	info, ok := s.ops["P2."+op]
	if !ok {
		Problem(w, &core.Problem{Reason: "not_found", Detail: "no such operation"})
		return
	}
	if r.Method != http.MethodPost {
		Problem(w, &core.Problem{Reason: "not_found", Detail: "operations are POST"})
		return
	}
	ctx := r.Context()
	token := bearer(r)
	if s.isHealthKey(token) {
		if op != "health" {
			Problem(w, &core.Problem{Reason: "forbidden", Detail: "the health key opens only health"})
			return
		}
		if _, ok := s.body(w, r, info.Request); !ok {
			return
		}
		writeJSON(w, s.c.Health(ctx))
		return
	}
	oper, ok := s.operator(token)
	if !ok {
		Problem(w, &core.Problem{Reason: "unauthenticated", Detail: "unknown P2 credentials"})
		return
	}
	m, ok := s.body(w, r, info.Request)
	if !ok {
		return
	}
	if v, has := m["op"]; has && op != "control.apply" && op != "manager.direct" && v != op {
		Problem(w, &core.Problem{Reason: "input_schema_invalid", Detail: "op does not match the path"})
		return
	}
	var out any
	var err error
	switch op {
	case "health":
		out = s.c.Health(ctx)
	case "apply":
		req, derr := decode[core.ApplyRequest](m)
		if derr != nil {
			s.fail(w, derr)
			return
		}
		out, err = s.c.Apply(ctx, oper, req)
	case "principal.add", "principal.off", "principal.on", "principal.set_identity":
		req, derr := decode[core.PrincipalRequest](m)
		if derr != nil {
			s.fail(w, derr)
			return
		}
		out, err = s.c.Principal(ctx, oper, req)
	case "grant", "ungrant":
		req, derr := decode[core.GrantRequest](m)
		if derr != nil {
			s.fail(w, derr)
			return
		}
		out, err = s.c.Grant(ctx, oper, req)
	case "worker.enroll":
		req, derr := decode[core.EnrollRequest](m)
		if derr != nil {
			s.fail(w, derr)
			return
		}
		out, err = s.c.Enroll(ctx, oper, req)
	case "audit.verify":
		tenant, _ := m["tenant"].(string)
		if tenant != oper.Tenant {
			Problem(w, &core.Problem{Reason: "forbidden", Detail: "the operator credential is for another tenant"})
			return
		}
		out, err = s.c.AuditVerify(ctx, tenant)
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
