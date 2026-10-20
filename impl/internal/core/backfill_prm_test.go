// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/HunterEwok/botaru/impl/internal/mcp"
)

// setTaskRights sets the tier ceiling and the mark of a task record.
func setTaskRights(t *testing.T, b *bench, task string, tier int, mark string) {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"max_tier": tier, "data_mark": mark})
	if _, err := b.c.st.Pool.Exec(context.Background(), `UPDATE tasks SET record = record || $2::jsonb WHERE id = $1`, task, string(raw)); err != nil {
		t.Fatal(err)
	}
}

// PRM-3: a right removed from the initiator cancels its task in work with rights_revoked, and
// a disabled initiator cancels its queued task with person_inactive;
// neither spends an attempt.
func TestBackfillRevokedRightsCancel(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newBench(t)
	task := b.hello(t, "1")
	att := b.claim(t, b.agent, "attempt")
	if att == nil || att.WorkID != task {
		t.Fatal("no attempt")
	}
	if _, err := b.c.Grant(ctx, b.op, GrantRequest{Op: "ungrant", Subject: b.alice, Group: "hello"}); err != nil {
		t.Fatal(err)
	}
	var state, reason string
	var handles int
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT state, coalesce(reason, ''), (SELECT count(*) FROM handles WHERE work_id = $1) FROM tasks WHERE id = $1`, task).Scan(&state, &reason, &handles); err != nil {
		t.Fatal(err)
	}
	if state != "canceled" || reason != "rights_revoked" || handles != 0 {
		t.Fatalf("PRM-3: after ungrant the task is %s(%s) with %d handles", state, reason, handles)
	}
	if _, err := b.c.Grant(ctx, b.op, GrantRequest{Op: "grant", Subject: b.alice, Group: "hello"}); err != nil {
		t.Fatal(err)
	}
	queued := b.hello(t, "2")
	if _, err := b.c.Principal(ctx, b.op, PrincipalRequest{Op: "principal.off", Subject: b.alice}); err != nil {
		t.Fatal(err)
	}
	if w := b.claim(t, b.agent, "attempt"); w != nil {
		t.Fatalf("PRM-3: a task of a disabled initiator was handed out: %s", w.WorkID)
	}
	var attempts int
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT state, coalesce(reason, ''), (SELECT count(*) FROM leases WHERE work_id = $1) FROM tasks WHERE id = $1`, queued).Scan(&state, &reason, &attempts); err != nil {
		t.Fatal(err)
	}
	if state != "canceled" || reason != "person_inactive" || attempts != 0 {
		t.Fatalf("PRM-3: the queued task of a disabled initiator is %s(%s) with %d leases", state, reason, attempts)
	}
}

// PRM-4: a tool is offered and callable only if it is in the role's tools, within the task's
// tier and with an output class not above the task's mark; each condition alone takes it away.
func TestBackfillToolConditions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newToolBenchWith(t, nil, func(ms []map[string]any) []map[string]any {
		for _, m := range ms {
			if m["kind"] == "Role" {
				m["spec"].(map[string]any)["tools"] = []any{"checks/latest", "logs/tail"}
			}
		}
		return ms
	})
	att, h := b.attemptTools(t, "1", true)
	// cmd/run is within the tier and the mark of the task, but not in the role's tools.
	if got := toolNames(t, b, h); !slices.Equal(got, []string{"checks_latest", "logs_tail"}) {
		t.Fatalf("PRM-4: the catalog of a role without cmd/run: %v", got)
	}
	r, err := b.c.P3ToolsCall(ctx, h, mcp.CallParams{Name: "cmd_run", Arguments: map[string]any{"host": "web1", "command": "disk_usage"}})
	if err != nil || !r.IsError || callText(r) != noPermission {
		t.Fatalf("PRM-4: a tool outside the role: %+v %v", r, err)
	}
	completeHello(t, b, att)
	for _, tc := range []struct {
		name string
		tier int
		mark string
	}{
		{"tier below the tool's", 0, "raw_logs"},
		{"mark below the output class", 1, "internal"},
	} {
		task := b.hello(t, tc.name)
		setTaskRights(t, b, task, tc.tier, tc.mark)
		a := b.claim(t, b.agent, "attempt")
		hh, _ := a.Payload["handles"].(map[string]any)["tools"].(string)
		if got := toolNames(t, b, hh); slices.Contains(got, "logs_tail") || !slices.Contains(got, "checks_latest") {
			t.Fatalf("PRM-4: %s: catalog %v", tc.name, got)
		}
		r, err := b.c.P3ToolsCall(ctx, hh, mcp.CallParams{Name: "logs_tail", Arguments: map[string]any{"source": "nginx-error"}})
		if err != nil || callText(r) != noPermission {
			t.Fatalf("PRM-4: %s: the call %+v %v", tc.name, r, err)
		}
		completeHello(t, b, a)
	}
}

// completeHello ends an attempt with a valid hello answer.
func completeHello(t *testing.T, b *bench, w *WorkItem) {
	t.Helper()
	if _, err := b.c.Complete(context.Background(), b.agent, header(t, w), CompleteRequest{Result: map[string]any{"answer": "ok"}}); err != nil {
		t.Fatal(err)
	}
}

// RT-4, PRM-3: what depends on the task is checked by the core at every call, not when the
// catalog was handed out: after the task's tier and mark drop, a tool the runtime already
// holds in its catalog is refused (the attempt is fenced, since its catalog no longer
// matches); no decision of an earlier call is reused.
func TestBackfillCheckedAtEveryCall(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newToolBench(t)
	att, h := b.attemptTools(t, "1", true)
	if got := toolNames(t, b, h); !slices.Contains(got, "logs_tail") {
		t.Fatalf("catalog %v", got)
	}
	args := map[string]any{"source": "nginx-error"}
	for range 2 {
		r, err := b.c.P3ToolsCall(ctx, h, mcp.CallParams{Name: "logs_tail", Arguments: args})
		if err != nil || callText(r) == noPermission {
			t.Fatalf("a call within the rights: %+v %v", r, err)
		}
	}
	setTaskRights(t, b, att.WorkID, 0, "summary")
	r, err := b.c.P3ToolsCall(ctx, h, mcp.CallParams{Name: "logs_tail", Arguments: args})
	if err == nil && (r == nil || callText(r) != noPermission) || err != nil && reasonOf(err) != "fenced" {
		t.Fatalf("RT-4: a call after the rights dropped: %+v %v", r, err)
	}
}

// LCY-7: a worker outside the P1 version window gets version_unsupported at hello, before any
// work; one that offers the current major among others is accepted.
func TestBackfillHelloVersionWindow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newBench(t)
	r, err := b.c.Enroll(ctx, b.op, EnrollRequest{Tenant: "demo", Role: "hello", Kind: "agent"})
	if err != nil {
		t.Fatal(err)
	}
	w, err := b.c.AuthenticateWorker(ctx, r.Token)
	if err != nil {
		t.Fatal(err)
	}
	for _, vs := range [][]string{{"v0"}, {"v2"}, {}} {
		if _, err := b.c.Hello(ctx, w, HelloRequest{Kind: "agent", ProtocolVersions: vs}); reasonOf(err) != "version_unsupported" {
			t.Fatalf("LCY-7: hello with %v: %v", vs, err)
		}
	}
	if _, err := b.c.Hello(ctx, w, HelloRequest{Kind: "agent", ProtocolVersions: []string{"v2", "v1"}}); err != nil {
		t.Fatalf("LCY-7: hello with the current major: %v", err)
	}
}

// SEC-4, OBS-3: a toolhost work item carries the minimum of its kind (tool, checked
// arguments, tenant, target, call id) without the goal, the initiator, the origin or the
// trace context of the caller; a foreign traceparent and baggage in the call's _meta change
// neither the tenant nor the task's own trace, and the plugin call gets no baggage. The agent
// attempt carries no external chat id or address of the person.
func TestBackfillWorkItemMinimum(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newToolBench(t)
	att, h := b.attemptTools(t, "1", false)
	raw, _ := json.Marshal(att.Payload)
	if strings.Contains(string(raw), "1001") || strings.Contains(string(raw), "telegram:") {
		t.Fatalf("SEC-4: the attempt carries an external id: %s", raw)
	}
	var trace string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT coalesce(record->>'traceparent', '') FROM tasks WHERE id = $1`, att.WorkID).Scan(&trace); err != nil {
		t.Fatal(err)
	}
	foreign := "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"
	var p mcp.CallParams
	if err := json.Unmarshal([]byte(`{"name": "checks_latest", "arguments": {"check": "www-health"},
		"_meta": {"traceparent": "`+foreign+`", "baggage": "tenant=demo2", "tracestate": "x=1"}}`), &p); err != nil {
		t.Fatal(err)
	}
	th := b.toolhost(t, "checks")
	done := callAsync(b, h, p)
	item := b.claimTool(t, th)
	body, _ := json.Marshal(item.Payload)
	for _, k := range []string{"goal", "initiator", "origin", "baggage", "tracestate", "demo2", foreign[3:35]} {
		if strings.Contains(string(body), k) {
			t.Errorf("SEC-4, OBS-3: the work item carries %q: %s", k, body)
		}
	}
	if item.Tenant != "demo" || item.Payload["tool"] != "latest" || item.Payload["target"] == nil || item.WorkID == "" {
		t.Fatalf("SEC-4: the work item %+v", item)
	}
	if args, _ := item.Payload["args"].(map[string]any); args["check"] != "www-health" {
		t.Fatalf("SEC-4: the checked arguments %v", item.Payload)
	}
	if _, err := b.c.Complete(ctx, th, header(t, item), CompleteRequest{Output: map[string]any{"content": []any{}, "structuredContent": map[string]any{"check": "www-health", "ok": 1, "total": 1}}}); err != nil {
		t.Fatal(err)
	}
	<-done
	var after, tenant string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT coalesce(record->>'traceparent', ''), tenant FROM tasks WHERE id = $1`, att.WorkID).Scan(&after, &tenant); err != nil {
		t.Fatal(err)
	}
	if after != trace || tenant != "demo" {
		t.Fatalf("OBS-3: the foreign trace changed the task: %q -> %q, tenant %s", trace, after, tenant)
	}
}

// PRM-3, PRM-8, CNT-1..CNT-3: a queued child canceled with rights_revoked keeps the detailed
// access reason in its event and answers its parent's continuation, which the lost right
// refuses with continuation.refused and the code text to the origin; a queued root task
// canceled in the same transaction tells the origin chat by the code text
// task.canceled.rights_revoked.
func TestBackfillRevokedChildContinuation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newChainBench(t, 1)
	b.hello(t, "1")
	att := b.claim(t, b.agent, "attempt")
	h := att.Payload["handles"].(map[string]any)["tools"].(string)
	r := childCall(t, b, h, map[string]any{"type": "hello.child.v1", "goal": "look it up"})
	if r.Code != "enqueued" {
		t.Fatalf("child %+v", r)
	}
	if _, err := b.c.Complete(ctx, b.agent, header(t, att), CompleteRequest{Result: map[string]any{"answer": "asked"}}); err != nil {
		t.Fatal(err)
	}
	queued := b.hello(t, "2")
	if _, err := b.c.Grant(ctx, b.op, GrantRequest{Op: "ungrant", Subject: b.alice, Group: "hello"}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{r.ID, queued} {
		var state, reason, access string
		if err := b.c.st.Pool.QueryRow(ctx, `SELECT t.state, coalesce(t.reason, ''), coalesce((SELECT e.data->>'access_reason' FROM events e WHERE e.type = 'task.canceled' AND e.subject = t.id), '')
			FROM tasks t WHERE t.id = $1`, id).Scan(&state, &reason, &access); err != nil {
			t.Fatal(err)
		}
		if state != "canceled" || reason != "rights_revoked" || access != "no_group_cap" {
			t.Fatalf("PRM-3: the queued task %s is %s(%s), access reason %q", id, state, reason, access)
		}
	}
	var refused int
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE type = 'continuation.refused' AND subject = $1`, r.ID).Scan(&refused); err != nil || refused != 1 {
		t.Fatalf("CNT-3: continuation.refused events of the child %d %v", refused, err)
	}
	texts := func(task, key string) int {
		t.Helper()
		var n int
		if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM deliveries WHERE task = $1 AND body->>'text_key' = $2 AND target_id = 'direct:1001'`, task, key).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if texts(r.ID, "task.continuation_refused") != 1 {
		t.Fatal("CNT-3: no task.continuation_refused text to the origin")
	}
	if texts(queued, "task.canceled.rights_revoked") != 1 || texts(r.ID, "task.canceled.rights_revoked") != 0 {
		t.Fatal("PRM-3: the task.canceled.rights_revoked text goes to the origin of the root task only")
	}
	if b.c.texts.Render("en", "task.canceled.rights_revoked", map[string]any{"reason": "no_group_cap"}) == "task.canceled.rights_revoked" {
		t.Fatal("the catalog has no task.canceled.rights_revoked")
	}
}
