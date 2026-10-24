// SPDX-License-Identifier: AGPL-3.0-only

// Package api is the HTTP binding v1 of the core (spec/bindings/http/README.md): the P1 listener
// with the P4 path, and the separate P2 listener (admin.md). Bodies are strict I-JSON checked
// against the request schema of the operation map before the core sees them.
package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/HunterEwok/botaru/impl/internal/core"
	"github.com/HunterEwok/botaru/impl/internal/ijson"
	"github.com/HunterEwok/botaru/impl/internal/spec"
)

// OperatorCred is an operator credential of the deployment. The reference implementation
// scopes each to one tenant (core.Operator).
type OperatorCred struct {
	// TokenSHA256 is the hex SHA-256 of the bearer value; the deployment file holds no token.
	TokenSHA256 string `json:"token_sha256" yaml:"token_sha256"`
	Principal   string `json:"principal" yaml:"principal"`
	Tenant      string `json:"tenant" yaml:"tenant"`
}

// ProgramCred is the key of an external program (D-47): the deployment issues the key and keeps
// only its hash with the tenant and the key id; the core finds the service principal with the
// identity api:<key_id> in the tenant (IDN-4, TEN-2). During a rotation two entries with the same
// key id are valid.
type ProgramCred struct {
	TokenSHA256 string `json:"token_sha256" yaml:"token_sha256"`
	Tenant      string `json:"tenant" yaml:"tenant"`
	KeyID       string `json:"key_id" yaml:"key_id"`
}

// Config is the HTTP side of the deployment settings.
type Config struct {
	Operators []OperatorCred
	// HealthKeysSHA256 are the hex SHA-256 of the health keys.
	HealthKeysSHA256 []string
	// ReportKeysSHA256 are the hex SHA-256 of the report keys of the backup and reconciliation
	// processes: they open only backup.report and spend.reconcile (P2 "Who calls").
	ReportKeysSHA256 []string
	// Programs are the keys of the external programs; they open only P1 enqueue, task.get and
	// cancel.
	Programs []ProgramCred
	// TestKeySHA256 is the hex SHA-256 of the test bench key that opens the test-control port.
	TestKeySHA256 string
	MaxBodyBytes  int64
	MaxDepth      int
	Logger        *slog.Logger
}

// Server serves P1, P4 and P2 over a core.
type Server struct {
	c   *core.Core
	cfg Config
	ops map[string]opInfo // "P1.claim" -> info
	p4  http.Handler
	log *slog.Logger
}

type opInfo struct {
	Request string
	Lease   bool
}

// New builds a server. p4 serves /v1/p4/ on the P1 listener; it may be nil.
func New(c *core.Core, cfg Config, p4 http.Handler) (*Server, error) {
	if cfg.MaxBodyBytes == 0 {
		cfg.MaxBodyBytes = 4 << 20
	}
	if cfg.MaxDepth == 0 {
		cfg.MaxDepth = 64
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	reg, err := c.Spec().Registry("spec/ports/operations.yaml")
	if err != nil {
		return nil, err
	}
	ops := map[string]opInfo{}
	for _, e := range reg.Entries {
		port, _ := e["port"].(string)
		op, _ := e["op"].(string)
		req, _ := e["request"].(string)
		lease, _ := e["lease"].(bool)
		ops[port+"."+op] = opInfo{Request: req, Lease: lease}
	}
	return &Server{c: c, cfg: cfg, ops: ops, p4: p4, log: cfg.Logger}, nil
}

// statusOf maps a reason code to the HTTP status of the binding's table.
func statusOf(reason string) int {
	switch reason {
	case "input_schema_invalid", "version_unsupported", "data_schema_invalid", "address_invalid", "msg_type_not_allowed", "note_not_allowed", "schema_invalid":
		return 400
	case "unauthenticated", "handle_invalid":
		return 401
	case "forbidden", "operator_only", "self_approve", "privileged_group", "blocked", "identity_unknown", "held", "type_not_allowed",
		"data_class_exceeded", "details_not_opened", "proposal_scope", "sink_not_export", "rank_exceeded":
		return 403
	case "not_found":
		return 404
	case "fenced", "lease_mismatch", "already_decided", "already_terminal", "idempotency_conflict", "tenant_mismatch":
		return 409
	case "gone", "expired":
		return 410
	case "over_capacity", "service_rate_limited", "sender_quota", "inbox_full", "control_rate_limited", "provider_rate_limited", "provider_quota":
		return 429
	case "not_ready", "draining", "store_unavailable", "provider_unavailable":
		return 503
	case "provider_config", "provider_spend_cap":
		return 502
	}
	return 422
}

// Problem writes problem details (RFC 9457) for a reason.
func Problem(w http.ResponseWriter, p *core.Problem) {
	status := statusOf(p.Reason)
	body := map[string]any{"type": "urn:botaru:reason:" + p.Reason, "title": p.Reason, "status": status, "reason": p.Reason}
	if p.Detail != "" {
		body["detail"] = p.Detail
	}
	if len(p.Needs) > 0 {
		body["needs"] = p.Needs
	}
	if status == 503 && p.RetryAfterS == 0 {
		p.RetryAfterS = 5
	}
	if p.RetryAfterS > 0 {
		body["retry_after_s"] = p.RetryAfterS
		w.Header().Set("Retry-After", strconv.Itoa(p.RetryAfterS))
	}
	b, _ := json.Marshal(body)
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	if p, ok := core.AsProblem(err); ok {
		Problem(w, p)
		return
	}
	if errors.Is(err, context.Canceled) {
		return
	}
	// A value the store refuses (SQLSTATE class 22, a data exception) is refused again on every
	// retry, so it is the caller's input, not a passing failure (DLV-5).
	var pg *pgconn.PgError
	if errors.As(err, &pg) && strings.HasPrefix(pg.Code, "22") {
		s.log.Warn("the store refused a value", "code", pg.Code, "err", err)
		Problem(w, &core.Problem{Reason: "input_schema_invalid", Detail: "the store refused a value (SQLSTATE " + pg.Code + ")"})
		return
	}
	// The core neither loses nor starts work when the store fails: the caller retries (OBS-8).
	s.log.Error("request failed", "err", err)
	Problem(w, &core.Problem{Reason: "not_ready", Detail: "internal error"})
}

func writeJSON(w http.ResponseWriter, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		Problem(w, &core.Problem{Reason: "not_ready", Detail: "response encoding"})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(b)
}

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if v, ok := strings.CutPrefix(h, "Bearer "); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

// body reads a request body as strict I-JSON and checks it against the request schema.
func (s *Server) body(w http.ResponseWriter, r *http.Request, schema string) (map[string]any, bool) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.cfg.MaxBodyBytes))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			body := map[string]any{"type": "urn:botaru:reason:input_schema_invalid", "title": "input_schema_invalid", "status": 413,
				"reason": "input_schema_invalid", "detail": "the body is too large"}
			b, _ := json.Marshal(body)
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			_, _ = w.Write(b)
			return nil, false
		}
		Problem(w, &core.Problem{Reason: "input_schema_invalid", Detail: "the body could not be read"})
		return nil, false
	}
	return s.object(w, raw, schema)
}

// opBody reads the body of the operation name ("P1.enqueue") as body does, and refuses an
// operation or a field of its body that belongs to a deferred rule with version_unsupported
// and the path (D-114, MAN-11): the operation alone before its schema, a field after.
func (s *Server) opBody(w http.ResponseWriter, r *http.Request, name string, info opInfo) (map[string]any, bool) {
	if deferredOp(w, name, nil) {
		return nil, false
	}
	m, ok := s.body(w, r, info.Request)
	if !ok || deferredOp(w, name, m) {
		return nil, false
	}
	return m, true
}

// deferredOp writes the refusal of a deferred operation or body field, if any; body nil
// checks the operation alone. The hello body is checked for its deferred fields too.
func deferredOp(w http.ResponseWriter, name string, body map[string]any) bool {
	h, ok := spec.DeferredCall(spec.SurfaceOp, name, body)
	if !ok && name == "P1.hello" && body != nil {
		h, ok = spec.DeferredCall(spec.SurfaceHello, "", body)
	}
	if !ok {
		return false
	}
	detail := name + " is of the deferred rule " + h.Rule + " and not served by this core"
	if h.Ptr != "" {
		detail = h.Ptr + " of " + name + " is of the deferred rule " + h.Rule + " and not served by this core"
	}
	Problem(w, &core.Problem{Reason: "version_unsupported", Detail: detail})
	return true
}

// object parses and checks a JSON object body against a schema.
func (s *Server) object(w http.ResponseWriter, raw []byte, schema string) (map[string]any, bool) {
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	v, err := ijson.ParseWithLimits(raw, ijson.Limits{MaxDepth: s.cfg.MaxDepth})
	if err != nil {
		Problem(w, &core.Problem{Reason: "input_schema_invalid", Detail: err.Error()})
		return nil, false
	}
	m, ok := v.(map[string]any)
	if !ok {
		Problem(w, &core.Problem{Reason: "input_schema_invalid", Detail: "the body is not an object"})
		return nil, false
	}
	if schema != "" {
		if err := s.c.Spec().Validate(schema, m); err != nil {
			detail := "the body does not match " + schema
			var ve *spec.ValidationError
			if errors.As(err, &ve) && len(ve.Violations) > 0 {
				detail += " at " + ve.Violations[0].Path
			}
			Problem(w, &core.Problem{Reason: "input_schema_invalid", Detail: detail})
			return nil, false
		}
	}
	return m, true
}

// decode turns a checked body into the request type of the core.
func decode[T any](m map[string]any) (T, error) {
	var v T
	b, err := json.Marshal(m)
	if err != nil {
		return v, err
	}
	err = json.Unmarshal(b, &v)
	return v, err
}

func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func equalHex(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }
