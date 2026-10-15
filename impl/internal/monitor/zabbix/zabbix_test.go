// SPDX-License-Identifier: AGPL-3.0-only

package zabbix

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/HunterEwok/botaru/impl/internal/p1client"
)

const canary = "CANARY-7f3a"

// fakeZabbix answers the JSON-RPC methods the adapter uses and records the calls. Script and
// trigger names carry the canary, which must never reach the core.
type fakeZabbix struct {
	mu      sync.Mutex
	calls   []rpc
	events  map[string]map[string]any
	scripts []map[string]any
}

type rpc struct {
	Method string
	Params json.RawMessage
}

func (z *fakeZabbix) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
		ID     int             `json:"id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	z.mu.Lock()
	z.calls = append(z.calls, rpc{req.Method, req.Params})
	z.mu.Unlock()
	var result any
	switch req.Method {
	case "history.push":
		result = map[string]any{"response": "success", "data": []any{map[string]any{"itemid": "1"}}}
	case "host.get":
		result = []any{map[string]any{"hostid": "10501", "host": "web1.example.org"}}
	case "script.getscriptsbyhosts":
		// Zabbix 7.0 takes an array of host objects, each with a hostid.
		var hosts []struct {
			HostID string `json:"hostid"`
		}
		if json.Unmarshal(req.Params, &hosts) != nil || len(hosts) == 0 || hosts[0].HostID == "" {
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": -32602, "message": "invalid params"}})
			return
		}
		result = map[string]any{"10501": z.scripts}
	case "script.execute":
		result = map[string]any{"response": "success", "value": strings.Repeat("x", 5000) + canary}
	case "event.get":
		var p struct {
			EventIDs []string `json:"eventids"`
		}
		_ = json.Unmarshal(req.Params, &p)
		var out []any
		for _, id := range p.EventIDs {
			if e, ok := z.events[id]; ok {
				out = append(out, e)
			}
		}
		result = out
	case "event.acknowledge":
		result = map[string]any{"eventids": []any{"901"}}
	default:
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": -32601, "message": "no such method"}})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
}

func (z *fakeZabbix) called(method string) []json.RawMessage {
	z.mu.Lock()
	defer z.mu.Unlock()
	var out []json.RawMessage
	for _, c := range z.calls {
		if c.Method == method {
			out = append(out, c.Params)
		}
	}
	return out
}

// fakeCore is P1: it hands out the queued work items one by one and records every call.
type fakeCore struct {
	mu    sync.Mutex
	work  []map[string]any
	calls []coreCall
}

type coreCall struct {
	Op    string
	Lease string
	Body  map[string]any
}

func (c *fakeCore) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	op := strings.TrimPrefix(r.URL.Path, "/v1/p1/")
	b, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(b, &body)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, coreCall{op, r.Header.Get("Botaru-Lease"), body})
	w.Header().Set("Content-Type", "application/json")
	switch op {
	case "claim":
		if len(c.work) == 0 {
			_ = json.NewEncoder(w).Encode(map[string]any{})
			return
		}
		item := c.work[0]
		c.work = c.work[1:]
		_ = json.NewEncoder(w).Encode(map[string]any{"work": item})
	case "submit_activity":
		_ = json.NewEncoder(w).Encode(map[string]any{"result": "ignored"})
	default:
		_ = json.NewEncoder(w).Encode(map[string]any{"lease_state": "closed"})
	}
}

func (c *fakeCore) queue(id, kind string, payload map[string]any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.work = append(c.work, map[string]any{"work_id": id, "kind": kind, "lease_no": 1, "tenant": "demo",
		"token": map[string]any{"id": id}, "payload": payload})
}

func (c *fakeCore) ops(op string) []coreCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []coreCall
	for _, x := range c.calls {
		if x.Op == op {
			out = append(out, x)
		}
	}
	return out
}

func setup(t *testing.T) (*Adapter, *fakeZabbix, *fakeCore, *httptest.Server) {
	t.Helper()
	z := &fakeZabbix{events: map[string]map[string]any{}, scripts: []map[string]any{
		{"scriptid": "17", "name": "restart " + canary, "scope": "2", "manualinput": "0"},
		{"scriptid": "21", "name": "clear " + canary, "scope": "2", "manualinput": "1", "manualinput_validator_type": "1", "manualinput_validator": "all,static"},
		{"scriptid": "30", "name": "free " + canary, "scope": "2", "manualinput": "1", "manualinput_validator_type": "0", "manualinput_validator": ".*"},
		{"scriptid": "40", "name": "event " + canary, "scope": "4", "manualinput": "0"},
	}}
	zs := httptest.NewServer(z)
	t.Cleanup(zs.Close)
	core := &fakeCore{}
	cs := httptest.NewServer(core)
	t.Cleanup(cs.Close)
	a := New(Config{
		Core: &p1client.Client{BaseURL: cs.URL, Token: "wk-token"}, Tenant: "demo", API: zs.URL, APIToken: "api-token",
		Hosts: map[string]string{"web1.example.org": "web1"}, WebhookToken: "hook-secret",
	})
	hook := httptest.NewServer(a.Webhook())
	t.Cleanup(hook.Close)
	return a, z, core, hook
}

func noCanary(t *testing.T, core *fakeCore) {
	t.Helper()
	core.mu.Lock()
	defer core.mu.Unlock()
	raw, _ := json.Marshal(core.calls)
	if strings.Contains(string(raw), canary) {
		t.Fatalf("a Zabbix text reached the core: %s", raw)
	}
}

// Alarm episodes and the audit head go to the trapper items of the farm host as key=value
// strings (MON-1, MON-3, MON-12); a Zabbix failure is retryable.
func TestExport(t *testing.T) {
	ctx := context.Background()
	a, z, core, _ := setup(t)
	core.queue("dl_1", "delivery", map[string]any{"delivery": map[string]any{"kind": "monitor_alarm", "body": map[string]any{
		"seq": 1.0, "state": "firing", "tenant": "demo", "alarm": "disk.free.low", "subject": "web1", "severity": "page",
		"scope": "tenant", "origin": "monitor", "key": "demo:disk.free.low:web1", "values": map[string]any{"trigger_severity": 4.0}}}})
	core.queue("dl_2", "delivery", map[string]any{"delivery": map[string]any{"kind": "monitor_audit_head", "body": map[string]any{
		"tenant": "demo", "epoch": 1.0, "seq": 42.0, "hash": "sha256:ab", "sig": "c2ln", "key_id": "k1"}}})
	for range 2 {
		if ok, err := a.ServeOne(ctx, 0); !ok || err != nil {
			t.Fatal(ok, err)
		}
	}
	pushes := z.called("history.push")
	if len(pushes) != 2 || !strings.Contains(string(pushes[0]), `"key":"botaru.alarm[page]"`) ||
		!strings.Contains(string(pushes[0]), `seq=1 state=firing tenant=demo alarm=disk.free.low subject=web1 severity=page scope=tenant origin=monitor key=demo:disk.free.low:web1 value_trigger_severity=4`) ||
		!strings.Contains(string(pushes[1]), `botaru.audit.head[demo]`) || !strings.Contains(string(pushes[1]), `epoch=1 seq=42 hash=sha256:ab sig=c2ln key=k1`) {
		t.Fatalf("pushes %s", pushes)
	}
	if n := len(core.ops("complete")); n != 2 {
		t.Fatalf("completes %d", n)
	}
	if err := a.Tick(ctx); err != nil || !strings.Contains(string(z.called("history.push")[2]), "botaru.adapter.alive[demo]") {
		t.Fatalf("tick %v", err)
	}
}

// The script list goes to the core without names and only when it changed; a script with free
// input and a script outside the host scope are not in it; a run goes once with the input of
// the list, the output is cut to 4096 bytes; a host outside the catalog and a script outside
// the list are refused (MON-10, blackbox:monitor/script-name-closed).
func TestScripts(t *testing.T) {
	ctx := context.Background()
	a, z, core, _ := setup(t)
	for range 3 {
		if err := a.SyncScripts(ctx); err != nil {
			t.Fatal(err)
		}
	}
	subs := core.ops("submit_activity")
	if len(subs) != 1 {
		t.Fatalf("an unchanged list was sent again: %d", len(subs))
	}
	data := subs[0].Body["event"].(map[string]any)["data"].(map[string]any)
	raw, _ := json.Marshal(data["scripts"])
	if string(raw) != `[{"input":"none","scriptid":"17"},{"choices":["all","static"],"input":"list","scriptid":"21"}]` {
		t.Fatalf("list %s", raw)
	}
	run := func(id string, body map[string]any) coreCall {
		t.Helper()
		core.queue(id, "delivery", map[string]any{"delivery": map[string]any{"kind": "monitor_script", "body": body}})
		if _, err := a.ServeOne(ctx, 0); err != nil {
			t.Fatal(err)
		}
		core.mu.Lock()
		defer core.mu.Unlock()
		return core.calls[len(core.calls)-1]
	}
	got := run("dl_1", map[string]any{"resource": "web1", "monitor_host": "web1.example.org", "scriptid": "21", "input": "static", "bot_id": "wk_1", "grant_id": "gr_1", "timeout_s": 60.0})
	if got.Op != "complete" {
		t.Fatalf("run %+v", got)
	}
	out := got.Body["receipt"].(map[string]any)["output"].(string)
	if len(out) != 4096 || got.Body["receipt"].(map[string]any)["exit_code"] != nil {
		t.Fatalf("receipt output %d, %v", len(out), got.Body["receipt"])
	}
	exec := z.called("script.execute")
	if len(exec) != 1 || !strings.Contains(string(exec[0]), `"manualinput":"static"`) || !strings.Contains(string(exec[0]), `"hostid":"10501"`) {
		t.Fatalf("execute %s", exec)
	}
	for _, c := range []struct {
		body   map[string]any
		reason string
	}{
		{map[string]any{"resource": "db1", "monitor_host": "db1.example.org", "scriptid": "17"}, "forbidden"},
		{map[string]any{"resource": "web1", "monitor_host": "web1.example.org", "scriptid": "30"}, "script_unknown"},
		{map[string]any{"resource": "web1", "monitor_host": "web1.example.org", "scriptid": "21", "input": "everything"}, "script_unknown"},
	} {
		if got := run("dl_x", c.body); got.Op != "fail" || got.Body["reason"] != c.reason {
			t.Fatalf("%v: %+v", c.body, got)
		}
	}
	if len(z.called("script.execute")) != 1 {
		t.Fatal("a refused run reached Zabbix")
	}
	// A note is a message on the problem and nothing else.
	core.queue("dl_n", "delivery", map[string]any{"delivery": map[string]any{"kind": "monitor_note", "body": map[string]any{
		"eventid": "901", "bot_id": "wk_1", "template": "service_control_done",
		"fields": map[string]any{"script": "cache-clear", "verb": "run", "result": "success", "grant_id": "gr_1"}}}})
	if _, err := a.ServeOne(ctx, 0); err != nil {
		t.Fatal(err)
	}
	ack := z.called("event.acknowledge")
	if len(ack) != 1 || !strings.Contains(string(ack[0]), `"action":4`) || !strings.Contains(string(ack[0]), `[botaru wk_1] service_control_done script=cache-clear verb=run result=success grant_id=gr_1`) {
		t.Fatalf("ack %s", ack)
	}
	noCanary(t, core)
}

// The webhook needs the token; a problem goes to the core only after the API confirms it on
// the same host and trigger and in the same state, with closed fields only (MON-5, ING-1).
func TestWebhook(t *testing.T) {
	_, z, core, hook := setup(t)
	z.events["901"] = map[string]any{"eventid": "901", "objectid": "4401", "severity": "4", "r_eventid": "0", "name": "disk " + canary,
		"hosts": []any{map[string]any{"host": "web1.example.org"}}}
	post := func(token, body string) int {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, hook.URL, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if s := post("wrong", `{"eventid":"901","triggerid":"4401","host":"web1.example.org","value":"1"}`); s != http.StatusUnauthorized {
		t.Fatalf("a wrong token: %d", s)
	}
	for _, b := range []string{
		`{"eventid":"902","triggerid":"4401","host":"web1.example.org","value":"1"}`, // not in the API
		`{"eventid":"901","triggerid":"4407","host":"web1.example.org","value":"1"}`, // another trigger
		`{"eventid":"901","triggerid":"4401","host":"web1.example.org","value":"0"}`, // still open
		`{"eventid":"901","triggerid":"4401","host":"db9.example.org","value":"1"}`,  // outside the catalog
	} {
		post("hook-secret", b)
	}
	if n := len(core.ops("submit_activity")); n != 0 {
		t.Fatalf("an unconfirmed problem went to the core: %d", n)
	}
	if s := post("hook-secret", `{"eventid":"901","triggerid":"4401","host":"web1.example.org","value":"1"}`); s != http.StatusAccepted {
		t.Fatalf("status %d", s)
	}
	subs := core.ops("submit_activity")
	if len(subs) != 1 {
		t.Fatalf("submits %d", len(subs))
	}
	ev := subs[0].Body["event"].(map[string]any)
	raw, _ := json.Marshal(ev)
	if string(raw) != `{"data":{"eventid":"901","resource":"web1","severity":4,"state":"open","triggerid":"4401"},"type":"ext.monitor.problem.v1"}` ||
		subs[0].Body["update_id"] != "901:open" {
		t.Fatalf("event %s %v", raw, subs[0].Body["update_id"])
	}
	noCanary(t, core)
}

// Erasure: a subject scope is not_applicable, since a note holds no data of people; an id or
// tenant scope fails, since Zabbix cannot delete a message (RET-7).
func TestErase(t *testing.T) {
	ctx := context.Background()
	a, _, core, _ := setup(t)
	core.queue("er_1", "erasure", map[string]any{"op": "erase", "scope": map[string]any{"tenant": "demo", "subject": "p_1"}})
	core.queue("er_2", "erasure", map[string]any{"op": "erase", "scope": map[string]any{"tenant": "demo", "ids": []any{"901:1"}}})
	for range 2 {
		if _, err := a.ServeOne(ctx, 0); err != nil {
			t.Fatal(err)
		}
	}
	c := core.ops("complete")
	if len(c) != 2 || c[0].Body["erasure"].(map[string]any)["outcome"] != "not_applicable" || c[1].Body["erasure"].(map[string]any)["outcome"] != "failed" {
		t.Fatalf("erasure %+v", c)
	}
}
