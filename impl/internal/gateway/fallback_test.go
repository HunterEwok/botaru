// SPDX-License-Identifier: AGPL-3.0-only

package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/HunterEwok/botaru/impl/internal/core"
	"github.com/HunterEwok/botaru/impl/internal/ids"
	"github.com/HunterEwok/botaru/impl/internal/manifest"
	"github.com/HunterEwok/botaru/impl/internal/spec"
	"github.com/HunterEwok/botaru/impl/internal/store"
	"github.com/HunterEwok/botaru/impl/internal/testbench"
)

// fallbackProvider answers 529 for the slot model and a short answer for any other model.
type fallbackProvider struct{ models []string }

func (p *fallbackProvider) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model string `json:"model"`
	}
	b, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(b, &req)
	p.models = append(p.models, req.Model)
	if req.Model == "model-a-small-2026-09" {
		w.WriteHeader(529)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"overloaded_error","message":"overloaded"}}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"` + req.Model + `","content":[{"type":"text","text":"hi"}],` +
		`"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}`))
}

// fallbackSet is the hello-bot set with the slot profile base-std falling back, in order, to
// base-big (not in the tenant's models_allowed), base-new (no eval of the role on it) and
// base-alt (allowed and attested when attest gives its fingerprint).
func fallbackSet(t *testing.T, root string, attest string) []map[string]any {
	t.Helper()
	ms, err := testbench.SharedHelloBotManifests(root)
	if err != nil {
		t.Fatal(err)
	}
	var base map[string]any
	for _, m := range ms {
		spec := m["spec"].(map[string]any)
		switch {
		case m["kind"] == "ModelProfile" && m["metadata"].(map[string]any)["name"] == "base-std":
			spec["fallback"] = []any{"base-big", "base-new", "base-alt"}
			base = m
		case m["kind"] == "Tenant":
			spec["models_allowed"] = append(spec["models_allowed"].([]any), "base-new", "base-alt")
		}
	}
	for _, name := range []string{"base-big", "base-new", "base-alt"} {
		b, _ := json.Marshal(base)
		var p map[string]any
		_ = json.Unmarshal(b, &p)
		p["metadata"] = map[string]any{"name": name}
		ps := p["spec"].(map[string]any)
		ps["model"], ps["fallback"], ps["pool"] = "model-"+name, []any{}, "pool-"+name
		// The fallback is the most expensive model of the call: 3x the output price (GW-6).
		ps["price"].(map[string]any)["output"] = 12.0
		ps["max_output_tokens"] = 1000.0
		ms = append(ms, p)
	}
	if attest != "" {
		ms = append(ms, map[string]any{"apiVersion": "botaru/v1alpha1", "kind": "EvalReports", "metadata": map[string]any{"name": "index"},
			"spec": map[string]any{"reports": []any{map[string]any{"subject": map[string]any{"kind": "role", "name": "hello"}, "fingerprint": attest,
				"suite": "hello", "date": "2026-10-01", "cases": 10, "failures": 0, "security_failures": 0}}}})
	}
	return ms
}

// The slot profile answers overloaded: the call goes to the first fallback that is in the
// tenant's models_allowed and carries a pass attestation of the role's eval on it, the usage
// line names it with the fallback marker, the event model.fallback.v1 is written, and the
// reserve was at the most expensive model of the call. Without the attestation the fallback is
// not used and the call answers the slot's reason (GW-5, GW-6, blackbox:gateway/fallback-checked).
func TestFallbackChecked(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, testbench.MigratedDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	root, err := spec.FindRoot("")
	if err != nil {
		t.Fatal(err)
	}
	sp, err := testbench.SharedSpec(root)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := testbench.SharedTexts(root)
	if err != nil {
		t.Fatal(err)
	}
	c, err := core.New(ctx, core.Config{TokenKey: []byte("0123456789abcdef0123456789abcdef"), TestBench: true}, st, sp, tx)
	if err != nil {
		t.Fatal(err)
	}
	personas, err := testbench.SharedPersonas(root)
	if err != nil {
		t.Fatal(err)
	}
	op := core.Operator{Principal: ids.PrincipalID(), Tenant: "demo"}
	apply := func(ms []map[string]any) {
		t.Helper()
		res, err := c.Apply(ctx, op, core.ApplyRequest{Commit: strings.Repeat("0", 40), Manifests: ms, Personas: personas})
		if err != nil || !res.Applied {
			t.Fatalf("apply: %+v %v", res, err)
		}
	}
	apply(fallbackSet(t, root, ""))
	alice := ids.PrincipalID()
	if _, err := c.Principal(ctx, op, core.PrincipalRequest{Op: "principal.add", Subject: alice, Identity: "telegram:1001"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Grant(ctx, op, core.GrantRequest{Op: "grant", Subject: alice, Group: "hello"}); err != nil {
		t.Fatal(err)
	}
	enroll := func(req core.EnrollRequest) *core.Worker {
		r, err := c.Enroll(ctx, op, req)
		if err != nil {
			t.Fatal(err)
		}
		w, err := c.AuthenticateWorker(ctx, r.Token)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.Hello(ctx, w, core.HelloRequest{Kind: req.Kind, ProtocolVersions: []string{"v1"}}); err != nil {
			t.Fatal(err)
		}
		w.State = "active"
		return w
	}
	agent := enroll(core.EnrollRequest{Tenant: "demo", Role: "hello", Kind: "agent"})
	channel := enroll(core.EnrollRequest{Tenant: "demo", Kind: "channel", Target: "telegram"})
	if _, err := st.Pool.Exec(ctx, `UPDATE contacts SET ai_notice_at = now()`); err != nil {
		t.Fatal(err)
	}
	zero := 0
	handle := func(update string) string {
		t.Helper()
		a := core.Activity{Kind: "message", Channel: "telegram", UpdateID: update, Chat: core.Chat{Kind: "direct", ExternalID: "1001"}, Text: "/hello hi"}
		a.Sender.Identity, a.Sender.Verified = "telegram:1001", true
		if res, err := c.SubmitActivity(ctx, channel, a); err != nil || res.Result != "task" {
			t.Fatalf("submit: %+v %v", res, err)
		}
		for {
			r, err := c.Claim(ctx, channel, core.ClaimRequest{Kinds: []string{"delivery"}, WaitS: &zero})
			if err != nil {
				t.Fatal(err)
			}
			if r.Work == nil {
				break
			}
			h, _ := core.EncodeToken(r.Work.Token)
			if _, err := c.Complete(ctx, channel, h, core.CompleteRequest{Receipt: &core.Receipt{ExternalID: "n" + update + r.Work.WorkID}}); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := st.Pool.Exec(ctx, `UPDATE contacts SET ai_notice_at = now() WHERE ai_notice_at IS NULL`); err != nil {
			t.Fatal(err)
		}
		r, err := c.Claim(ctx, agent, core.ClaimRequest{Kinds: []string{"attempt"}, WaitS: &zero})
		if err != nil || r.Work == nil {
			t.Fatalf("attempt: %v", err)
		}
		return r.Work.Payload["handles"].(map[string]any)["model"].(string)
	}
	prov := &fallbackProvider{}
	srv := httptest.NewServer(prov)
	t.Cleanup(srv.Close)
	g, err := New(c, Config{TestBench: true, Providers: map[string]Provider{
		"provider-a": {Binding: "anthropic", BaseURL: srv.URL, Keys: map[string]string{"demo": "k"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	call := func(h string) (int, map[string]any) {
		req := httptest.NewRequest(http.MethodPost, "/v1/p4/anthropic/v1/messages",
			strings.NewReader(`{"model":"base","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Authorization", "Bearer "+h)
		rec := httptest.NewRecorder()
		g.ServeHTTP(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}

	// Without an attestation no fallback is used: the call answers the slot's reason.
	if code, out := call(handle("1")); code != 503 || out["reason"] != "provider_unavailable" {
		t.Fatalf("unattested fallback: %d %v", code, out)
	}
	if strings.Join(prov.models, ",") != "model-a-small-2026-09" {
		t.Fatalf("calls %v", prov.models)
	}
	var n int
	if err := st.Pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE type = 'model.fallback.v1'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("events %d %v", n, err)
	}

	// The role's eval passed on base-alt; base-big is outside models_allowed and base-new has
	// no attestation, so base-alt takes the call.
	fp, err := c.FallbackFingerprint(ctx, "hello", "demo", "base", "base-alt")
	if err != nil || fp == "" {
		t.Fatalf("fingerprint %q %v", fp, err)
	}
	apply(fallbackSet(t, root, fp))
	if _, err := st.Pool.Exec(ctx, `DELETE FROM provider_pairs`); err != nil {
		t.Fatal(err)
	}
	prov.models = nil
	if code, out := call(handle("2")); code != 200 || out["model"] != "model-base-alt" {
		t.Fatalf("fallback call: %d %v", code, out)
	}
	if strings.Join(prov.models, ",") != "model-a-small-2026-09,model-base-alt" {
		t.Fatalf("calls %v", prov.models)
	}
	var profile, slot string
	var fallback bool
	if err := st.Pool.QueryRow(ctx, `SELECT profile, slot_profile, fallback FROM spend WHERE fallback ORDER BY time DESC LIMIT 1`).Scan(&profile, &slot, &fallback); err != nil {
		t.Fatal(err)
	}
	if profile != "base-alt" || slot != "base-std" || !fallback {
		t.Fatalf("usage line %s %s %v", profile, slot, fallback)
	}
	var data []byte
	if err := st.Pool.QueryRow(ctx, `SELECT data FROM events WHERE type = 'model.fallback.v1'`).Scan(&data); err != nil {
		t.Fatal(err)
	}
	var ev map[string]any
	if err := json.Unmarshal(data, &ev); err != nil || ev["reason"] != "overloaded" || ev["fallback_profile"] != "base-alt" || ev["profile"] != "base-std" {
		t.Fatalf("event %s", data)
	}
	// The slot's circuit opens during the attempt: the call goes to the fallback at once, as for
	// unavailable.
	h3 := handle("3")
	if _, err := st.Pool.Exec(ctx, `INSERT INTO provider_pairs (tenant, pool, state, until, updated_at) VALUES ('demo', 'family-a', 'open', botaru_now() + interval '1 hour', botaru_now())
		ON CONFLICT (tenant, pool) DO UPDATE SET state = 'open', until = EXCLUDED.until`); err != nil {
		t.Fatal(err)
	}
	prov.models = nil
	if code, out := call(h3); code != 200 || out["model"] != "model-base-alt" || strings.Join(prov.models, ",") != "model-base-alt" {
		t.Fatalf("open circuit: %d %v %v", code, out, prov.models)
	}
	// Only a 404 on the model is retired (binding "Errors").
	if fallbackReason(404, []byte(`{"type":"error","error":{"type":"not_found_error","message":"model: model-x"}}`)) != "retired" ||
		fallbackReason(404, []byte(`{"type":"error","error":{"type":"not_found_error","message":"not found"}}`)) != "" {
		t.Fatal("404 on the model")
	}
	on := &core.ModelGrant{Profile: manifest.ModelProfile{FallbackOn: []string{"overloaded"}}}
	if !core.SwitchesOn(on, "overloaded") || core.SwitchesOn(on, "rate_limited") {
		t.Fatal("fallback reasons")
	}
}
