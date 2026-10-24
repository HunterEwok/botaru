// SPDX-License-Identifier: AGPL-3.0-only

package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/HunterEwok/botaru/impl/internal/core"
	"github.com/HunterEwok/botaru/impl/internal/ids"
	"github.com/HunterEwok/botaru/impl/internal/manifest"
	"github.com/HunterEwok/botaru/impl/internal/spec"
	"github.com/HunterEwok/botaru/impl/internal/store"
	"github.com/HunterEwok/botaru/impl/internal/testbench"
)

// recProvider records each provider request and answers with answer.
type recProvider struct {
	mu     sync.Mutex
	bodies []map[string]any
	keys   []string
	answer func(w http.ResponseWriter, n int)
}

func (p *recProvider) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(b, &body)
	p.mu.Lock()
	p.bodies = append(p.bodies, body)
	p.keys = append(p.keys, r.Header.Get("x-api-key"))
	n := len(p.bodies)
	p.mu.Unlock()
	p.answer(w, n)
}

// seen returns a copy of the recorded bodies and keys.
func (p *recProvider) seen() ([]map[string]any, []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]map[string]any(nil), p.bodies...), append([]string(nil), p.keys...)
}

func (p *recProvider) calls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.bodies)
}

// answerUsage answers a message with the given usage and request id.
func answerUsage(usage string) func(w http.ResponseWriter, n int) {
	return func(w http.ResponseWriter, n int) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("request-id", "req_"+string(rune('0'+n)))
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"hi"}],` +
			`"stop_reason":"end_turn","usage":` + usage + `}`))
	}
}

// p4Bench is a core with the hello-bot set, an agent and a channel, and P4 over a recording
// mock provider with the key "key-demo" of the tenant demo.
type p4Bench struct {
	c       *core.Core
	st      *store.Store
	agent   *core.Worker
	channel *core.Worker
	prov    *recProvider
	g       *Gateway
}

func newP4Bench(t *testing.T, edit func(ms []map[string]any), answer func(w http.ResponseWriter, n int)) *p4Bench {
	t.Helper()
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
	ms, err := testbench.SharedHelloBotManifests(root)
	if err != nil {
		t.Fatal(err)
	}
	if edit != nil {
		edit(ms)
	}
	op := core.Operator{Principal: ids.PrincipalID(), Tenant: "demo"}
	if res, err := c.Apply(ctx, farmOf(op), core.ApplyRequest{Commit: strings.Repeat("0", 40), Manifests: ms, Personas: personas}); err != nil || !res.Applied {
		t.Fatalf("apply: %+v %v", res, err)
	}
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
	b := &p4Bench{c: c, st: st, agent: enroll(core.EnrollRequest{Tenant: "demo", Role: "hello", Kind: "agent"}),
		channel: enroll(core.EnrollRequest{Tenant: "demo", Kind: "channel", Target: "telegram"}), prov: &recProvider{answer: answer}}
	srv := httptest.NewServer(b.prov)
	t.Cleanup(srv.Close)
	b.g, err = New(c, Config{TestBench: true, Providers: map[string]Provider{
		"provider-a": {Binding: "anthropic", BaseURL: srv.URL, Keys: map[string]string{"demo": "key-demo"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// attempt submits a hello request, delivers the notices and claims the attempt: it returns the
// task, the attempt and its model handle.
func (b *p4Bench) attempt(t *testing.T, update string) (string, *core.WorkItem, string) {
	t.Helper()
	ctx := context.Background()
	zero := 0
	a := core.Activity{Kind: "message", Channel: "telegram", UpdateID: update, Chat: core.Chat{Kind: "direct", ExternalID: "1001"}, Text: "/hello hi"}
	a.Sender.Identity, a.Sender.Verified = "telegram:1001", true
	res, err := b.c.SubmitActivity(ctx, b.channel, a)
	if err != nil || res.Result != "task" {
		t.Fatalf("submit: %+v %v", res, err)
	}
	for {
		r, err := b.c.Claim(ctx, b.channel, core.ClaimRequest{Kinds: []string{"delivery"}, WaitS: &zero})
		if err != nil {
			t.Fatal(err)
		}
		if r.Work == nil {
			break
		}
		h, _ := core.EncodeToken(r.Work.Token)
		if _, err := b.c.Complete(ctx, b.channel, h, core.CompleteRequest{Receipt: &core.Receipt{ExternalID: "n" + update + r.Work.WorkID}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := b.st.Pool.Exec(ctx, `UPDATE contacts SET ai_notice_at = now() WHERE ai_notice_at IS NULL`); err != nil {
		t.Fatal(err)
	}
	r, err := b.c.Claim(ctx, b.agent, core.ClaimRequest{Kinds: []string{"attempt"}, WaitS: &zero})
	if err != nil || r.Work == nil {
		t.Fatalf("attempt: %v", err)
	}
	return res.Task, r.Work, r.Work.Payload["handles"].(map[string]any)["model"].(string)
}

// call sends one runtime request to P4 under the handle.
func (b *p4Bench) call(handle, body string) (int, map[string]any) {
	req := httptest.NewRequest(http.MethodPost, "/v1/p4/anthropic/v1/messages", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+handle)
	rec := httptest.NewRecorder()
	b.g.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

type usageLine struct {
	State   string
	Micros  int64
	Tokens  map[string]int64
	Request string
}

func (b *p4Bench) usageLines(t *testing.T, task string) []usageLine {
	t.Helper()
	rows, err := b.st.Pool.Query(context.Background(), `SELECT state, eur_micros, coalesce(tokens, '{}'), coalesce(request_id, '') FROM spend
		WHERE task = $1 AND unit = 'tokens' ORDER BY time, id`, task)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []usageLine
	for rows.Next() {
		var u usageLine
		var tok []byte
		if err := rows.Scan(&u.State, &u.Micros, &tok, &u.Request); err != nil {
			t.Fatal(err)
		}
		_ = json.Unmarshal(tok, &u.Tokens)
		out = append(out, u)
	}
	return out
}

const p4Request = `{"model":"base","max_tokens":100000,"system":"You are the farm bot.","messages":[{"role":"user","content":"hi"}]}`

// Before each call P4 reserves the worst case and sets the output limit no higher than the
// profile's max_output_tokens; after the answer it charges the usage of the answer, cache write
// and read included, so the provider calls and the usage lines are as many (GW-2). P4 sets the
// cache point on the last system block, and the second call of the run reads the cache at the
// profile's prices (GW-16). The call goes with the tenant's own key (GW-3).
func TestBackfillP4ReserveChargeCache(t *testing.T) {
	cw, cr := 1.25, 0.1
	b := newP4Bench(t, func(ms []map[string]any) {
		for _, m := range ms {
			if m["kind"] == "ModelProfile" {
				price := m["spec"].(map[string]any)["price"].(map[string]any)
				price["cache_write"], price["cache_read"] = cw, cr
			}
		}
	}, func(w http.ResponseWriter, n int) {
		if n == 1 {
			answerUsage(`{"input_tokens":100,"output_tokens":20,"cache_creation_input_tokens":400,"cache_read_input_tokens":0}`)(w, n)
			return
		}
		answerUsage(`{"input_tokens":100,"output_tokens":20,"cache_creation_input_tokens":0,"cache_read_input_tokens":400}`)(w, n)
	})
	task, _, handle := b.attempt(t, "1")
	for i := range 2 {
		if code, out := b.call(handle, p4Request); code != 200 {
			t.Fatalf("call %d: %d %v", i, code, out)
		}
	}
	lines := b.usageLines(t, task)
	// GW-2: one usage line per provider call, each committed with the answer's usage.
	if b.prov.calls() != 2 || len(lines) != 2 {
		t.Fatalf("provider calls %d, usage lines %d", b.prov.calls(), len(lines))
	}
	bodies, keys := b.prov.seen()
	for i, body := range bodies {
		// GW-2: the output limit is the profile's max_output_tokens, not the runtime's.
		if body["max_tokens"] != 8000.0 {
			t.Errorf("call %d: max_tokens %v", i, body["max_tokens"])
		}
		// GW-16: P4 sets the cache point after the unchanging part.
		sys, _ := body["system"].([]any)
		if len(sys) == 0 || sys[len(sys)-1].(map[string]any)["cache_control"] == nil {
			t.Errorf("call %d: no cache point %v", i, body["system"])
		}
		// GW-3: the call goes with the key of the tenant's workspace.
		if keys[i] != "key-demo" {
			t.Errorf("call %d: key %q", i, keys[i])
		}
	}
	price := manifest.Price{Input: 1, Output: 4, CacheWrite: &cw, CacheRead: &cr}
	first, _ := Cost(price, Usage{InputTokens: 100, OutputTokens: 20, CacheCreationInputTokens: 400})
	second, _ := Cost(price, Usage{InputTokens: 100, OutputTokens: 20, CacheReadInputTokens: 400})
	if l := lines[0]; l.State != "committed" || l.Micros != first || l.Tokens["cache_write"] != 400 || l.Request != "req_1" {
		t.Errorf("first usage line %+v, want %d", l, first)
	}
	// GW-16: the second call reads the cache, and its usage line says so.
	if l := lines[1]; l.State != "committed" || l.Micros != second || l.Tokens["cache_read"] <= 0 {
		t.Errorf("second usage line %+v, want %d", l, second)
	}
}

// A provider call that went out and got no answer is charged at its full reserve as a line
// marked estimated, which stays after the task ends until a reconciliation (BUD-11); P4 does
// not send it again once the body went out (GW-12).
func TestBackfillP4NoAnswer(t *testing.T) {
	ctx := context.Background()
	b := newP4Bench(t, nil, func(w http.ResponseWriter, n int) {
		// The provider takes the request and drops the connection.
		hj, ok := w.(http.Hijacker)
		if !ok {
			panic("no hijacker")
		}
		conn, _, err := hj.Hijack()
		if err == nil {
			_ = conn.Close()
		}
	})
	task, att, handle := b.attempt(t, "1")
	code, out := b.call(handle, p4Request)
	if code != 503 || out["reason"] != "provider_unavailable" {
		t.Fatalf("a dropped call: %d %v", code, out)
	}
	// GW-12: exactly one provider call, no retry after the body went out.
	if n := b.prov.calls(); n != 1 {
		t.Fatalf("provider calls %d", n)
	}
	cat, err := b.c.Catalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := cat.Profile("demo", "base-std")
	bodies, _ := b.prov.seen()
	payload, _ := json.Marshal(bodies[0])
	want := Reserve(p, len(payload), 8000)
	lines := b.usageLines(t, task)
	// BUD-11: one line at the full reserve of the call, marked estimated.
	if len(lines) != 1 || lines[0].State != "estimated" || lines[0].Micros < want-64 || lines[0].Micros > want+64 {
		t.Fatalf("usage lines %+v, want one estimated at about %d", lines, want)
	}
	h, _ := core.EncodeToken(att.Token)
	if _, err := b.c.Complete(ctx, b.agent, h, core.CompleteRequest{Result: map[string]any{"answer": "Hello!"}}); err != nil {
		t.Fatal(err)
	}
	// BUD-11: the reserve stays charged after the task ended, until a reconciliation.
	if after := b.usageLines(t, task); len(after) != 1 || after[0].State != "estimated" || after[0].Micros != lines[0].Micros {
		t.Fatalf("after the task: %+v", after)
	}
	var released, reconciled bool
	if err := b.st.Pool.QueryRow(ctx, `SELECT released_at IS NOT NULL, reconciled_at IS NOT NULL FROM spend WHERE task = $1 AND unit = 'tokens'`, task).Scan(&released, &reconciled); err != nil || released || reconciled {
		t.Fatalf("released %v reconciled %v %v", released, reconciled, err)
	}
	// GW-12: the answer deadline comes from the profile, twice max_output_tokens / tokens_per_s,
	// at least 60 seconds.
	if d := deadline(manifest.ModelProfile{MaxOutputTokens: 8000, TokensPerS: 60}); d != 266*time.Second {
		t.Errorf("deadline %v", d)
	}
	if d := deadline(manifest.ModelProfile{MaxOutputTokens: 1000, TokensPerS: 100}); d != 60*time.Second {
		t.Errorf("short deadline %v", d)
	}
}

// Only P4 holds the provider key and goes to the provider: a handle whose lease ended opens
// nothing, and a tenant without a key of its own gets provider_config, both without a provider
// call (GW-3). The runtime does not set the region: a region field in its request is outside
// the allowlist; P4 sets it from the tenant's subprocessor entry (GW-14). A streaming request is
// refused (DLV-11).
func TestBackfillP4KeyAndRegion(t *testing.T) {
	ctx := context.Background()
	b := newP4Bench(t, func(ms []map[string]any) {
		for _, m := range ms {
			if m["kind"] == "Tenant" {
				for _, s := range m["spec"].(map[string]any)["privacy"].(map[string]any)["subprocessors"].([]any) {
					if sm := s.(map[string]any); sm["provider"] == "provider-a" {
						sm["region"] = "us"
					}
				}
			}
		}
	}, answerUsage(`{"input_tokens":10,"output_tokens":5}`))
	_, att, handle := b.attempt(t, "1")
	// GW-14: a region field of the runtime is refused before the provider.
	if code, out := b.call(handle, `{"model":"base","max_tokens":10,"inference_geo":"global","messages":[{"role":"user","content":"hi"}]}`); code != 400 || out["reason"] != "input_schema_invalid" {
		t.Fatalf("a runtime region: %d %v", code, out)
	}
	// DLV-11: no streaming of model text; the answer goes whole after complete.
	if code, out := b.call(handle, `{"model":"base","max_tokens":10,"stream":true,"messages":[{"role":"user","content":"hi"}]}`); code != 400 || out["reason"] != "input_schema_invalid" {
		t.Fatalf("a streaming request: %d %v", code, out)
	}
	if b.prov.calls() != 0 {
		t.Fatal("a refused request reached the provider")
	}
	if code, out := b.call(handle, p4Request); code != 200 {
		t.Fatalf("call: %d %v", code, out)
	}
	// GW-14: the region is the subprocessor entry's, set by P4.
	if bodies, _ := b.prov.seen(); bodies[0]["inference_geo"] != "us" {
		got := bodies[0]["inference_geo"]
		t.Fatalf("region %v", got)
	}
	h, _ := core.EncodeToken(att.Token)
	if _, err := b.c.Complete(ctx, b.agent, h, core.CompleteRequest{Result: map[string]any{"answer": "Hello!"}}); err != nil {
		t.Fatal(err)
	}
	// GW-3: the handle is valid only while its lease lives.
	if code, out := b.call(handle, p4Request); code < 400 || (out["reason"] != "fenced" && out["reason"] != "handle_invalid") {
		t.Fatalf("a handle after its lease: %d %v", code, out)
	}
	_, _, h2 := b.attempt(t, "2")
	b.g.cfg.Providers["provider-a"] = Provider{Binding: "anthropic", BaseURL: b.g.cfg.Providers["provider-a"].BaseURL, Keys: map[string]string{"other": "key-other"}}
	// GW-3: without the tenant's own key there is no call, not one with another tenant's key.
	if code, out := b.call(h2, p4Request); code != 502 || out["reason"] != "provider_config" {
		t.Fatalf("a tenant without a key: %d %v", code, out)
	}
	if b.prov.calls() != 1 {
		t.Fatalf("provider calls %d", b.prov.calls())
	}
}
