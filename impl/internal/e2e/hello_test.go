// SPDX-License-Identifier: AGPL-3.0-only

// Package e2e plays the hello-bot sequence (examples/hello-bot/sequence.yaml) over the wire:
// the core with its P1, P2 and P4 listeners, the mock model provider, the agent worker and the
// Telegram channel against a fake Bot API server.
package e2e

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/HunterEwok/botaru/impl/internal/agent"
	"github.com/HunterEwok/botaru/impl/internal/api"
	"github.com/HunterEwok/botaru/impl/internal/channel/telegram"
	"github.com/HunterEwok/botaru/impl/internal/core"
	"github.com/HunterEwok/botaru/impl/internal/gateway"
	"github.com/HunterEwok/botaru/impl/internal/ids"
	"github.com/HunterEwok/botaru/impl/internal/mockprovider"
	"github.com/HunterEwok/botaru/impl/internal/p1client"
	"github.com/HunterEwok/botaru/impl/internal/pgtest"
	"github.com/HunterEwok/botaru/impl/internal/spec"
	"github.com/HunterEwok/botaru/impl/internal/store"
	"github.com/HunterEwok/botaru/impl/internal/testbench"
	"github.com/HunterEwok/botaru/impl/internal/texts"
)

func TestMain(m *testing.M) {
	// The test binary is also the plugin process the toolhost starts (tools_test.go).
	if mode := os.Getenv("BOTARU_E2E_PLUGIN"); mode != "" {
		os.Exit(fakePlugin(mode))
	}
	pgtest.Main(m)
}

// fakeBotAPI is a fake Telegram Bot API server: it hands out queued updates once and records
// the messages sent.
type fakeBotAPI struct {
	mu      sync.Mutex
	updates []map[string]any
	sent    []map[string]any
	nextID  int64
}

func (f *fakeBotAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	method := r.URL.Path[strings.LastIndexByte(r.URL.Path, '/')+1:]
	if !strings.HasPrefix(r.URL.Path, "/botTEST:TOKEN/") {
		w.WriteHeader(404)
		_, _ = io.WriteString(w, `{"ok":false,"error_code":404,"description":"Not Found"}`)
		return
	}
	var req map[string]any
	_ = json.NewDecoder(r.Body).Decode(&req)
	f.mu.Lock()
	defer f.mu.Unlock()
	var result any
	switch method {
	case "getUpdates":
		var offset float64
		if o, ok := req["offset"].(float64); ok {
			offset = o
		}
		var out []map[string]any
		for _, u := range f.updates {
			if u["update_id"].(float64) >= offset {
				out = append(out, u)
			}
		}
		result = out
		if out == nil {
			result = []any{}
		}
	case "sendMessage":
		f.nextID++
		f.sent = append(f.sent, req)
		result = map[string]any{"message_id": 4700 + f.nextID, "chat": map[string]any{"id": req["chat_id"]}}
	default:
		w.WriteHeader(400)
		_, _ = io.WriteString(w, `{"ok":false,"error_code":400,"description":"unknown method"}`)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result})
}

type rig struct {
	c       *core.Core
	p1, p2  *httptest.Server
	mock    *mockprovider.Server
	bot     *fakeBotAPI
	opToken string
	root    string
	botURL  string
}

func hashHex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func newRig(t *testing.T) *rig {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, pgtest.DSN(t))
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
	sp, err := spec.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := texts.Load(root, "")
	if err != nil {
		t.Fatal(err)
	}
	c, err := core.New(ctx, core.Config{TokenKey: []byte("e2e-key-e2e-key-e2e-key-e2e-key-0"), TestBench: true, Operator: "the test bench operator"}, st, sp, tx)
	if err != nil {
		t.Fatal(err)
	}
	sc, err := mockprovider.Load(filepath.Join(root, "examples/hello-bot/mock-model.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	r := &rig{c: c, mock: mockprovider.New(sc), bot: &fakeBotAPI{}, root: root}
	mockSrv := httptest.NewServer(r.mock)
	t.Cleanup(mockSrv.Close)
	gw, err := gateway.New(c, gateway.Config{TestBench: true, Providers: map[string]gateway.Provider{
		"provider-a": {Binding: "anthropic", BaseURL: mockSrv.URL, Keys: map[string]string{"demo": "mock-key-demo"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	r.opToken = "op-" + ids.Secret(16)
	srv, err := api.New(c, api.Config{
		Operators:        []api.OperatorCred{{TokenSHA256: hashHex(r.opToken), Principal: ids.PrincipalID(), Tenant: "demo"}},
		HealthKeysSHA256: []string{hashHex("health-key")},
		ReportKeysSHA256: []string{hashHex("report-key")},
	}, gw)
	if err != nil {
		t.Fatal(err)
	}
	r.p1 = httptest.NewServer(srv.P1Handler())
	r.p2 = httptest.NewServer(srv.P2Handler())
	t.Cleanup(r.p1.Close)
	t.Cleanup(r.p2.Close)
	botSrv := httptest.NewServer(r.bot)
	t.Cleanup(botSrv.Close)
	r.botURL = botSrv.URL
	return r
}

func (r *rig) p2call(t *testing.T, token, op string, req any, out any) int {
	t.Helper()
	b, _ := json.Marshal(req)
	hr, _ := http.NewRequest(http.MethodPost, r.p2.URL+"/v1/p2/"+op, strings.NewReader(string(b)))
	hr.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(hr)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if out != nil && resp.StatusCode == 200 {
		if err := json.Unmarshal(body, out); err != nil {
			t.Fatal(err)
		}
	}
	if resp.StatusCode != 200 && out != nil {
		t.Fatalf("%s: %d %s", op, resp.StatusCode, body)
	}
	return resp.StatusCode
}

func rawPost(t *testing.T, url, token, lease, body string) (int, map[string]any) {
	t.Helper()
	hr, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if token != "" {
		hr.Header.Set("Authorization", "Bearer "+token)
	}
	if lease != "" {
		hr.Header.Set("Botaru-Lease", lease)
	}
	resp, err := http.DefaultClient.Do(hr)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestHelloBotOverTheWire(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)

	// Initial state: the hello-bot manifests on the test bench, alice with telegram:1001 in
	// group hello, an agent of role hello and the telegram channel.
	ms, err := testbench.HelloBotManifests(r.root)
	if err != nil {
		t.Fatal(err)
	}
	var applied struct {
		Applied bool `json:"applied"`
	}
	r.p2call(t, r.opToken, "apply", map[string]any{"commit": strings.Repeat("a", 40), "manifests": ms}, &applied)
	if !applied.Applied {
		t.Fatal("apply refused")
	}
	alice := ids.PrincipalID()
	var done map[string]any
	r.p2call(t, r.opToken, "principal.add", map[string]any{"op": "principal.add", "subject": alice, "identity": "telegram:1001"}, &done)
	r.p2call(t, r.opToken, "grant", map[string]any{"op": "grant", "subject": alice, "group": "hello"}, &done)
	var agentEnroll, chanEnroll struct {
		Token   string `json:"token"`
		P2Token string `json:"p2_token"`
	}
	r.p2call(t, r.opToken, "worker.enroll", map[string]any{"tenant": "demo", "role": "hello", "kind": "agent"}, &agentEnroll)
	r.p2call(t, r.opToken, "worker.enroll", map[string]any{"tenant": "demo", "role": "telegram", "kind": "channel", "target": "telegram"}, &chanEnroll)
	if chanEnroll.P2Token == "" {
		t.Fatal("the first channel process of the driver got no P2 secret")
	}

	// Binding checks before the sequence.
	if code := r.p2call(t, agentEnroll.Token, "health", map[string]any{}, nil); code != 401 {
		t.Fatalf("a worker token on P2: %d", code)
	}
	if code := r.p2call(t, "health-key", "status", map[string]any{}, nil); code != 403 {
		t.Fatalf("the health key on status: %d", code)
	}
	// A report key opens only the reports, as a farm operator without a person.
	for _, op := range []string{"restore.complete", "core.drain", "status"} {
		if code := r.p2call(t, "report-key", op, map[string]any{}, nil); code != 403 {
			t.Fatalf("the report key on %s: %d", op, code)
		}
	}
	now := time.Now().UTC()
	if code := r.p2call(t, "report-key", "backup.report", map[string]any{"kind": "backup", "location_id": "offsite-1", "ok": true,
		"started_at": now.Add(-time.Minute).Format(time.RFC3339), "finished_at": now.Format(time.RFC3339)}, nil); code != 200 {
		t.Fatalf("the report key on backup.report: %d", code)
	}
	var health map[string]any
	r.p2call(t, "health-key", "health", map[string]any{}, &health)
	if health["alive"] != true {
		t.Fatalf("health %v", health)
	}
	if code, _ := rawPost(t, r.p1.URL+"/v1/p1/hello", agentEnroll.Token, "", `{"kind":"agent","kind":"agent","protocol_versions":["v1"]}`); code != 400 {
		t.Fatalf("a repeated key: %d", code)
	}
	if code, _ := rawPost(t, r.p1.URL+"/v1/p4/anthropic/v1/models", "x", "", `{}`); code != 404 {
		t.Fatalf("a P4 path outside the binding: %d", code)
	}

	bot := r.bot
	bot.updates = []map[string]any{{
		"update_id": 1.0,
		"message": map[string]any{
			"message_id": 10, "from": map[string]any{"id": 1001, "is_bot": false},
			"chat": map[string]any{"id": 1001, "type": "private"}, "text": "/hello hi",
		},
	}}
	agentClient := &p1client.Client{BaseURL: r.p1.URL, Token: agentEnroll.Token}
	chanClient := &p1client.Client{BaseURL: r.p1.URL, Token: chanEnroll.Token}
	tx, _ := texts.Load(r.root, "")
	ch := telegram.New(telegram.Config{Core: chanClient, Channel: "telegram", BotAPI: r.botURL, BotToken: "TEST:TOKEN", Texts: tx, PollTimeoutS: 1})
	ag := agent.New(agent.Config{Core: agentClient})
	if err := ch.Hello(ctx); err != nil {
		t.Fatal(err)
	}
	if err := ag.Hello(ctx); err != nil {
		t.Fatal(err)
	}

	if code, body := rawPost(t, r.p1.URL+"/v1/p1/heartbeat", agentEnroll.Token, "", `{}`); code != 409 || body["reason"] != "fenced" {
		t.Fatalf("a call under a lease without the header: %d %v", code, body)
	}
	if code, body := rawPost(t, r.p1.URL+"/v1/p1/heartbeat", agentEnroll.Token, "bm90LWEtdG9rZW4", `{}`); code != 409 || body["reason"] != "lease_mismatch" {
		t.Fatalf("a changed token: %d %v", code, body)
	}

	start := time.Now()
	if w, err := agentClient.Claim(ctx, []string{"attempt"}, 1); err != nil || w != nil {
		t.Fatalf("an empty claim: %v %v", w, err)
	}
	if time.Since(start) > 6*time.Second {
		t.Fatal("an empty claim with wait_s 1 took too long")
	}

	if err := ch.PollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if ok, err := ch.DeliverOnce(ctx, 1); err != nil || !ok {
			t.Fatalf("notice %d: %v %v", i, ok, err)
		}
	}
	if ok, err := ag.Once(ctx, 1); err != nil || !ok {
		t.Fatalf("attempt: %v %v", ok, err)
	}
	if ok, err := ch.DeliverOnce(ctx, 1); err != nil || !ok {
		t.Fatalf("task_result: %v %v", ok, err)
	}
	if err := ch.PollOnce(ctx); err != nil { // the offset moved: the update is not submitted again
		t.Fatal(err)
	}
	if ok, _ := ch.DeliverOnce(ctx, 0); ok {
		t.Fatal("an unexpected fourth delivery")
	}

	bot.mu.Lock()
	sent := append([]map[string]any(nil), bot.sent...)
	bot.mu.Unlock()
	if len(sent) != 3 {
		t.Fatalf("sent %d messages: %v", len(sent), sent)
	}
	wantPrefix := []string{"This is the bot of tenant demo. Answers are written by an external provider's model; the conversation is stored by operator the test bench operator", "Data is processed by", "[AI answer]\nHello, alice! I am the farm bot @demo_bot."}
	for i, m := range sent {
		if m["chat_id"] != 1001.0 {
			t.Errorf("message %d went to chat %v", i, m["chat_id"])
		}
		text, _ := m["text"].(string)
		if !strings.HasPrefix(text, wantPrefix[i]) {
			t.Errorf("message %d: %q", i, text)
		}
		if strings.Contains(text, "{") { // no raw placeholder reaches a person
			t.Errorf("message %d has a raw placeholder: %q", i, text)
		}
	}
	if !strings.Contains(sent[1]["text"].(string), "Full text: https://docs.example.org/botaru/privacy (version 3)") {
		t.Errorf("privacy notice without version and link: %q", sent[1]["text"])
	}

	calls := r.mock.Calls()
	if len(calls) != 1 || calls[0].Model != "model-a-small-2026-09" || len(calls[0].Tools) != 0 || calls[0].Region != "" {
		t.Fatalf("mock provider calls: %+v", calls)
	}
	var micros int64
	var state string
	if err := r.c.Store().Pool.QueryRow(ctx, `SELECT eur_micros, state FROM spend`).Scan(&micros, &state); err != nil {
		t.Fatal(err)
	}
	if micros != 260 || state != "committed" { // 180 input at 1 EUR/M + 20 output at 4 EUR/M
		t.Fatalf("usage line: %d %s", micros, state)
	}
	var verify map[string]any
	r.p2call(t, r.opToken, "audit.verify", map[string]any{"tenant": "demo"}, &verify)
	if verify["ok"] != true {
		t.Fatalf("audit chain: %v", verify)
	}
	var cmds []string
	rows, err := r.c.Store().Pool.Query(ctx, `SELECT record->>'command' FROM audit WHERE tenant = 'demo' ORDER BY seq`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		cmds = append(cmds, s)
	}
	rows.Close()
	for _, want := range []string{"task.enqueue", "task.claim", "task.complete", "delivery.complete"} {
		found := false
		for _, c := range cmds {
			found = found || c == want
		}
		if !found {
			t.Errorf("no audit record %s in %v", want, cmds)
		}
	}
}
