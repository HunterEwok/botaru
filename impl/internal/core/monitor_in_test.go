// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/HunterEwok/botaru/impl/internal/ids"
	"github.com/HunterEwok/botaru/impl/internal/mcp"
	"github.com/jackc/pgx/v5"
)

// scriptBench is the tool bench with web1 bound to the node web1.example.org: the scripts
// app-api-restart (17), cache-clear (21, a choice of all or static) and free-input (30), the
// trigger 4401 of class disk_free, the role allowed all three, and alice in script-runners
// with monitor.script_run. The monitor profile is on and the adapter of demo said hello.
type scriptBench struct {
	*bench
	mon   *Worker
	carol string
}

func newScriptBench(t *testing.T) *scriptBench {
	t.Helper()
	b := newToolBenchWith(t, nil, func(ms []map[string]any) []map[string]any {
		for _, m := range ms {
			s := m["spec"].(map[string]any)
			switch m["kind"] {
			case "Catalog":
				h := s["hosts"].([]any)[0].(map[string]any)
				h["monitor_host"] = "web1.example.org"
				h["monitor_scripts_allowed"] = []any{
					map[string]any{"scriptid": "17", "name": "app-api-restart", "hosts": []any{"web1.example.org"}},
					map[string]any{"scriptid": "21", "name": "cache-clear", "hosts": []any{"web1.example.org"}, "choices": []any{"all", "static"}},
					map[string]any{"scriptid": "30", "name": "free-input", "hosts": []any{"web1.example.org"}},
				}
				s["monitor_triggers"] = []any{map[string]any{"triggerid": "4401", "class": "disk_free", "item": "vfs.fs.size[/data,pfree]"}}
			case "Role":
				s["monitor_scripts"] = []any{"app-api-restart", "cache-clear", "free-input"}
			}
		}
		return append(ms, map[string]any{
			"apiVersion": "botaru/v1alpha1", "kind": "Group", "metadata": map[string]any{"name": "script-runners", "tenant": "demo"},
			"spec": map[string]any{"caps": []any{"monitor.script_run"}, "data": []any{"summary"}},
		})
	})
	ctx := context.Background()
	if _, err := b.c.st.Pool.Exec(ctx, `INSERT INTO memberships (tenant, principal, grp, state, created_at) VALUES ('demo', $1, 'script-runners', 'active', now())`, b.alice); err != nil {
		t.Fatal(err)
	}
	carol := ids.PrincipalID()
	if _, err := b.c.Principal(ctx, b.op, PrincipalRequest{Op: "principal.add", Subject: carol, Identity: "telegram:1003"}); err != nil {
		t.Fatal(err)
	}
	return &scriptBench{bench: b, mon: b.monitorOn(t), carol: carol}
}

func (b *scriptBench) signal(t *testing.T, typ, update string, data map[string]any) *ActivityResult {
	t.Helper()
	a := Activity{Kind: "signal", Channel: "zabbix", UpdateID: update, Chat: Chat{Kind: "direct", ExternalID: "monitor"}, Event: &SignalEvent{Type: typ, Data: data}}
	a.Sender.Identity, a.Sender.Verified = "hook:monitor", true
	r, err := b.c.SubmitActivity(context.Background(), b.mon, a)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func (b *scriptBench) problem(t *testing.T, eventid, trigger, resource, state string) *ActivityResult {
	t.Helper()
	return b.signal(t, "ext.monitor.problem.v1", eventid+":"+state, map[string]any{
		"eventid": eventid, "triggerid": trigger, "resource": resource, "severity": 4, "state": state, "class": "disk_free"})
}

func (b *scriptBench) scripts(t *testing.T, hash string, list ...map[string]any) *ActivityResult {
	t.Helper()
	items := make([]any, len(list))
	for i, s := range list {
		items[i] = s
	}
	return b.signal(t, "ext.monitor.scripts.v1", hash, map[string]any{"resource": "web1", "hash": hash, "scripts": items})
}

// A problem of a catalog trigger raises the alarm of its class with origin monitor; a repeat
// of the same event gives nothing; a trigger outside the catalog gives alarm.monitor.problem
// whatever class the adapter claims; the alarm of a host resolves with its last open problem;
// a node outside the catalog does not enter (MON-5, blackbox:monitor/trigger-not-in-catalog).
func TestMonitorProblem(t *testing.T) {
	ctx := context.Background()
	b := newScriptBench(t)
	b.problem(t, "901", "4401", "web1", "open")
	if s := b.alarmState(t, "alarm.disk.free.low.v1", "demo"); s != "firing" {
		t.Fatalf("disk.free.low %q", s)
	}
	var origin string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT origin FROM alarm_episodes WHERE alarm = 'alarm.disk.free.low.v1'`).Scan(&origin); err != nil || origin != "monitor" {
		t.Fatalf("origin %q %v", origin, err)
	}
	n := countEvents(t, b.bench, "ext.monitor.problem.v1", "")
	b.problem(t, "901", "4401", "web1", "open")
	if countEvents(t, b.bench, "ext.monitor.problem.v1", "") != n {
		t.Fatal("a repeated webhook made a second event")
	}
	// A trigger outside the catalog: the class the adapter sent is not taken.
	b.problem(t, "902", "5000", "web1", "open")
	b.problem(t, "903", "5001", "web1", "open")
	if s := b.alarmState(t, "alarm.monitor.problem.v1", "demo"); s != "firing" {
		t.Fatalf("monitor.problem %q", s)
	}
	var class string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT data->>'class' FROM events WHERE type = 'ext.monitor.problem.v1' AND data->>'eventid' = '902'`).Scan(&class); err != nil || class != "unknown" {
		t.Fatalf("class %q %v", class, err)
	}
	b.problem(t, "902", "5000", "web1", "resolved")
	if s := b.alarmState(t, "alarm.monitor.problem.v1", "demo"); s != "firing" {
		t.Fatal("the alarm resolved while a problem of the host is open")
	}
	b.problem(t, "903", "5001", "web1", "resolved")
	if s := b.alarmState(t, "alarm.monitor.problem.v1", "demo"); s != "resolved" {
		t.Fatalf("after the last problem closed: %q", s)
	}
	if r := b.problem(t, "904", "4401", "db9", "open"); r.Result != "ignored" || r.Reason != "not_subject" {
		t.Fatalf("a node outside the catalog: %+v", r)
	}
	// Only the monitor adapter submits a signal.
	a := Activity{Kind: "signal", Channel: "telegram", UpdateID: "x", Chat: Chat{Kind: "direct", ExternalID: "1001"}, Event: &SignalEvent{Type: "ext.monitor.problem.v1"}}
	if _, err := b.c.SubmitActivity(ctx, b.channel, a); err == nil {
		t.Fatal("a channel submitted a signal")
	}
}

// The script list closes the enum: a catalog script that the monitoring system does not give,
// and a script with free input, are not offered; a run goes once as a monitor_script delivery
// under a grant, the model gets a code, and the output is a journal file that taints the task
// that reads it; an unchanged list gives no event; the note goes on the open problem of the
// host (MON-10, MON-8, blackbox:monitor/script-name-closed).
func TestScriptRun(t *testing.T) {
	ctx := context.Background()
	b := newScriptBench(t)
	b.scripts(t, "hash-0001", map[string]any{"scriptid": "17", "input": "none"},
		map[string]any{"scriptid": "21", "input": "list", "choices": []any{"static", "all"}},
		map[string]any{"scriptid": "30", "input": "none"})
	n := countEvents(t, b.bench, "ext.monitor.scripts.v1", "")
	b.scripts(t, "hash-0001", map[string]any{"scriptid": "17", "input": "none"})
	if countEvents(t, b.bench, "ext.monitor.scripts.v1", "") != n {
		t.Fatal("an unchanged list made an event")
	}
	// The monitoring system now gives 17 and 21 only; 30 takes free input there.
	b.scripts(t, "hash-0002", map[string]any{"scriptid": "17", "input": "none"},
		map[string]any{"scriptid": "21", "input": "list", "choices": []any{"static", "all"}})
	b.problem(t, "901", "4401", "web1", "open")

	att, h := b.attemptTools(t, "1", true)
	l, err := b.c.P3ToolsList(ctx, h)
	if err != nil {
		t.Fatal(err)
	}
	var schema string
	for _, x := range l.Tools {
		if m := x.(map[string]any); m["name"] == scriptToolName {
			raw, _ := json.Marshal(m)
			schema = string(raw)
		}
	}
	if schema == "" || !strings.Contains(schema, "app-api-restart") || !strings.Contains(schema, "cache-clear") || strings.Contains(schema, "free-input") {
		t.Fatalf("script tool %s", schema)
	}
	run := func(args map[string]any) *mcp.CallResult {
		t.Helper()
		r, err := b.c.P3ToolsCall(ctx, h, mcp.CallParams{Name: scriptToolName, Arguments: args})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	for _, args := range []map[string]any{
		{"resource": "web1", "script": "free-input"},
		{"resource": "web1", "script": "cache-clear", "input": "everything"},
		{"resource": "web1", "script": "cache-clear"},
		{"resource": "web1", "script": "app-api-restart", "input": "all"},
	} {
		if r := run(args); !r.IsError || !strings.Contains(callText(r), "input_schema_invalid") {
			t.Fatalf("%v: %s", args, callText(r))
		}
	}
	ch := callAsync(b.bench, h, mcp.CallParams{Name: scriptToolName, Arguments: map[string]any{"resource": "web1", "script": "cache-clear", "input": "static"}})
	waitScript(t, b.bench)
	var item *WorkItem
	for range 50 {
		if item = b.claim(t, b.mon, "delivery"); item == nil || dbody(item)["scriptid"] != nil {
			break
		}
		if _, err := b.c.Complete(ctx, b.mon, header(t, item), CompleteRequest{Receipt: &Receipt{ExternalID: "x"}}); err != nil {
			t.Fatal(err)
		}
	}
	if item == nil {
		g := recv(t, ch)
		t.Fatalf("no run for the adapter; the call gave %s %v", callText(g.r), g.err)
	}
	body := dbody(item)
	if body["scriptid"] != "21" || body["monitor_host"] != "web1.example.org" || body["input"] != "static" || body["grant_id"] == nil {
		t.Fatalf("monitor_script %v", body)
	}
	// The task's mark is one alice may see, so the journal file can be read for her task.
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE tasks SET record = record || '{"data_mark": "summary"}' WHERE id = $1`, att.WorkID); err != nil {
		t.Fatal(err)
	}
	canary := "CANARY-" + ids.Secret(6)
	code := 0
	if _, err := b.c.Complete(ctx, b.mon, header(t, item), CompleteRequest{Receipt: &Receipt{ExternalID: "r1", ExitCode: &code, Output: "cleared " + canary}}); err != nil {
		t.Fatal(err)
	}
	got := recv(t, ch)
	if got.err != nil {
		t.Fatal(got.err)
	}
	text := callText(got.r)
	var res serviceResult
	_ = json.Unmarshal([]byte(text), &res)
	if res.Code != "done" || res.GrantID != body["grant_id"] || strings.Contains(text, canary) {
		t.Fatalf("result %s", text)
	}
	validOut(t, b.bench, "service-control-result", res)
	// The note goes on the open problem, with closed fields only; the audit has no output.
	note := b.claim(t, b.mon, "delivery")
	if note == nil || dbody(note)["eventid"] != "901" || dbody(note)["template"] != "service_control_done" {
		t.Fatalf("note %v", note)
	}
	raw, _ := json.Marshal(dbody(note))
	var leaks int
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM audit WHERE record::text LIKE '%' || $1 || '%'`, canary).Scan(&leaks); err != nil || leaks != 0 || strings.Contains(string(raw), canary) {
		t.Fatalf("the output leaked: audit %d, note %s", leaks, raw)
	}
	// The output is a journal file of the task; reading it taints the task.
	var ref string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT file_ref FROM files WHERE producer = $1`, att.WorkID).Scan(&ref); err != nil {
		t.Fatal(err)
	}
	f, err := b.c.FileGet(ctx, b.agent, header(t, att), FileGetRequest{FileRef: ref})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(f.Content), canary) {
		t.Fatalf("file %q", f.Content)
	}
	var untrusted bool
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT coalesce((record->>'untrusted_seen')::boolean, false) FROM tasks WHERE id = $1`, att.WorkID).Scan(&untrusted); err != nil || !untrusted {
		t.Fatalf("untrusted_seen %v %v", untrusted, err)
	}
	// After untrusted text a script is refused.
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE tasks SET record = record || '{"data_mark": "raw_logs"}' WHERE id = $1`, att.WorkID); err != nil {
		t.Fatal(err)
	}
	if r := run(map[string]any{"resource": "web1", "script": "app-api-restart"}); !strings.Contains(callText(r), "forbidden") {
		t.Fatalf("after untrusted text: %s", callText(r))
	}
}

// Guards: an initiator without monitor.script_run, a hold, one run per host at a time, a run
// no adapter takes and a lease that runs out (D-73, HLD-3, APR-7).
func TestScriptRunGuards(t *testing.T) {
	ctx := context.Background()
	b := newScriptBench(t)
	b.scripts(t, "hash-0001", map[string]any{"scriptid": "17", "input": "none"})
	if _, err := b.c.st.Pool.Exec(ctx, `DELETE FROM memberships WHERE grp = 'script-runners'`); err != nil {
		t.Fatal(err)
	}
	_, h := b.attemptTools(t, "1", true)
	args := map[string]any{"resource": "web1", "script": "app-api-restart"}
	r, err := b.c.P3ToolsCall(ctx, h, mcp.CallParams{Name: scriptToolName, Arguments: args})
	if err != nil || !strings.Contains(callText(r), "forbidden") || !strings.Contains(callText(r), scriptCap) {
		t.Fatalf("without the capability: %s %v", callText(r), err)
	}
	if _, err := b.c.st.Pool.Exec(ctx, `INSERT INTO memberships (tenant, principal, grp, state, created_at) VALUES ('demo', $1, 'script-runners', 'active', now())`, b.alice); err != nil {
		t.Fatal(err)
	}
	_, h = b.attemptTools(t, "2", true)
	// Nobody takes the run: after the start window it ends as no_executor and is withdrawn.
	ch := callAsync(b.bench, h, mcp.CallParams{Name: scriptToolName, Arguments: args})
	if !queued(t, b.bench) {
		g := recv(t, ch)
		t.Fatalf("not queued: %v %v", g.r, g.err)
	}
	busy, err := b.c.P3ToolsCall(ctx, h, mcp.CallParams{Name: scriptToolName, Arguments: args})
	if err != nil || !strings.Contains(callText(busy), "resource_busy") {
		t.Fatalf("a second run on the host: %s %v", callText(busy), err)
	}
	b.advance(t, 6*60)
	b.loop(t, "monitor")
	got := recv(t, ch)
	if got.err != nil || !strings.Contains(callText(got.r), `"failed"`) {
		t.Fatalf("an untaken run: %s %v", callText(got.r), got.err)
	}
	if n := countKind(t, b.bench, "monitor_script", "withheld"); n != 1 {
		t.Fatalf("withheld runs %d", n)
	}
	// A taken run whose lease ends: exec_unknown, never run again.
	b.loop(t, "reaper")
	_, h = b.attemptTools(t, "3", true)
	ch = callAsync(b.bench, h, mcp.CallParams{Name: scriptToolName, Arguments: args})
	if !queued(t, b.bench) {
		g := recv(t, ch)
		t.Fatalf("not queued: %v %v", g.r, g.err)
	}
	item := b.claim(t, b.mon, "delivery")
	if item == nil || dbody(item)["scriptid"] != "17" {
		t.Fatalf("run %v", item)
	}
	if _, err := b.c.Fail(ctx, b.mon, header(t, item), FailRequest{Reason: "exec_unknown", Retryable: true}); err != nil {
		t.Fatal(err)
	}
	got = recv(t, ch)
	if got.err != nil || !strings.Contains(callText(got.r), "exec_unknown") {
		t.Fatalf("a lost run: %s %v", callText(got.r), got.err)
	}
	if b.claim(t, b.mon, "delivery") != nil {
		t.Fatal("a run was handed out again")
	}
	// A hold with no_tool on the host refuses the call.
	if _, err := b.c.HoldSet(ctx, OperatorActor(b.op), HoldSetRequest{
		Scope: HoldScope{Kind: "tenant", Tenant: "demo"}, Effects: []string{"no_tool"}, Reason: "manual"}); err != nil {
		t.Fatal(err)
	}
	if r, err := b.c.P3ToolsCall(ctx, h, mcp.CallParams{Name: scriptToolName, Arguments: args}); err != nil || !r.IsError {
		t.Fatalf("under a hold: %s %v", callText(r), err)
	}
}

func waitScript(t *testing.T, b *bench) {
	t.Helper()
	if !queued(t, b) {
		t.Fatal("the run was not queued")
	}
}

func queued(t *testing.T, b *bench) bool {
	t.Helper()
	for range 200 {
		var n int
		if err := b.c.st.Pool.QueryRow(context.Background(), `SELECT count(*) FROM toolcalls WHERE tool = $1 AND state = 'queued'`, scriptRight).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

func recv(t *testing.T, ch <-chan struct {
	r   *mcp.CallResult
	err error
}) struct {
	r   *mcp.CallResult
	err error
} {
	t.Helper()
	select {
	case g := <-ch:
		return g
	case <-time.After(20 * time.Second):
		t.Fatal("the call did not end")
	}
	panic("unreachable")
}

// A hold set after the call was queued withholds the run at hand-out, and a released run ends
// its call instead of going back to the queue (HLD-3, MON-10).
func TestScriptRunHandout(t *testing.T) {
	ctx := context.Background()
	b := newScriptBench(t)
	b.scripts(t, "hash-0001", map[string]any{"scriptid": "17", "input": "none"})
	_, h := b.attemptTools(t, "1", true)
	args := map[string]any{"resource": "web1", "script": "app-api-restart"}

	ch := callAsync(b.bench, h, mcp.CallParams{Name: scriptToolName, Arguments: args})
	if !queued(t, b.bench) {
		t.Fatal("the run was not queued")
	}
	item := b.claim(t, b.mon, "delivery")
	if item == nil || dbody(item)["scriptid"] != "17" {
		t.Fatalf("the run was not handed out: %+v", item)
	}
	if _, err := b.c.Release(ctx, b.mon, header(t, item)); err != nil {
		t.Fatal(err)
	}
	if again := b.claim(t, b.mon, "delivery"); again != nil && dbody(again)["scriptid"] == "17" {
		t.Fatal("a released run was handed out again")
	}
	if g := recv(t, ch); strings.Contains(callText(g.r), `"done"`) {
		t.Fatalf("a released run: %s", callText(g.r))
	}

	ch = callAsync(b.bench, h, mcp.CallParams{Name: scriptToolName, Arguments: args})
	if !queued(t, b.bench) {
		t.Fatal("the second run was not queued")
	}
	if _, err := b.c.HoldSet(ctx, OperatorActor(b.op), HoldSetRequest{
		Scope: HoldScope{Kind: "tenant", Tenant: "demo"}, Effects: []string{"no_action"}, Reason: "manual"}); err != nil {
		t.Fatal(err)
	}
	if item := b.claim(t, b.mon, "delivery"); item != nil && dbody(item)["scriptid"] == "17" {
		t.Fatal("a run was handed out under a hold set after it was queued")
	}
	if g := recv(t, ch); !strings.Contains(callText(g.r), "forbidden") {
		t.Fatalf("a held run: %s", callText(g.r))
	}
	var heldState string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT state FROM toolcalls WHERE tool = $1 ORDER BY created_at DESC LIMIT 1`, scriptRight).Scan(&heldState); err != nil || heldState != "denied" {
		t.Fatalf("a held run is recorded as %s %v", heldState, err)
	}

	// The note of an action a person approved for a bot names the bot of the asking task; a
	// person's own action names nobody (MON-8).
	var task, worker string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT work_id, worker_id FROM leases WHERE kind = 'attempt' ORDER BY created_at DESC LIMIT 1`).Scan(&task, &worker); err != nil {
		t.Fatal(err)
	}
	if err := b.c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		if _, err := tx.Exec(ctx, `INSERT INTO approvals (id, tenant, source_task, initiator, action, args_hash, state, record, nonce, expires_at, created_at, updated_at)
			VALUES ('ap_note', 'demo', $1, 'alice', 'service.control', 'h', 'executed', '{}', 'n', $2, $2, $2)`, task, now); err != nil {
			return err
		}
		if bot, err := noteBot(ctx, tx, "tk_action", "ap_note"); err != nil || bot != worker {
			t.Errorf("the bot of an approved action: %q %v", bot, err)
		}
		if bot, err := noteBot(ctx, tx, "tk_action", ""); err != nil || bot != "" {
			t.Errorf("a person's own action: %q %v", bot, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
