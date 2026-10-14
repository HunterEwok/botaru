// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HunterEwok/botaru/impl/internal/ids"
	"github.com/HunterEwok/botaru/impl/internal/manifest"
	"github.com/HunterEwok/botaru/impl/internal/mcp"
	"github.com/HunterEwok/botaru/impl/internal/spec"
)

// serviceBench is the action bench with nginx (start, stop, restart) and api on web1, the role
// allowed nginx, the plugin service, alice in operators with service.control:nginx, bob also
// in svc-approvers with action.approve:service.control, and carol (telegram:1003) without
// rights. The tenant runs /run fix <host> (fix_web, by card) and /run clean (the routine
// clean_tmp, at most 2 an hour).
type serviceBench struct {
	*actionBench
	svc   *Worker
	carol string
}

func newServiceBench(t *testing.T) *serviceBench {
	t.Helper()
	root, err := spec.FindRoot("")
	if err != nil {
		t.Fatal(err)
	}
	b := newActionBench(t, func(ms []map[string]any) []map[string]any {
		for _, m := range ms {
			s := m["spec"].(map[string]any)
			switch m["kind"] {
			case "Catalog":
				h := s["hosts"].([]any)[0].(map[string]any)
				h["services"] = []any{
					map[string]any{"name": "nginx", "unit": "nginx.service", "control": []any{"start", "stop", "restart"}, "via": "host"},
					map[string]any{"name": "api", "unit": "api.service", "control": []any{"restart"}, "via": "host"},
				}
			case "Role":
				s["service_control"] = []any{"nginx"}
			case "Tenant":
				s["run"] = map[string]any{
					"fix":   map[string]any{"action": "fix_web", "args": []any{"host"}},
					"clean": map[string]any{"action": "clean_tmp", "args": []any{"host"}},
				}
			case "ToolPlugin":
				if m["metadata"].(map[string]any)["name"] == "fixer" {
					s["tools"] = append(s["tools"].([]any), map[string]any{
						"name": "clean_tmp", "access": "write", "tier": 3, "output_class": "internal", "untrusted_output": false,
						"args_from": "action:clean_tmp", "target_from": "host", "output_limit_kb": 4, "timeout_s": 20,
					})
				}
			}
		}
		docs, err := manifest.ReadFiles([]string{filepath.Join(root, "examples/tools/service/tool.yaml")})
		if err != nil {
			t.Fatal(err)
		}
		ms = append(ms, docs[0].Value,
			map[string]any{
				"apiVersion": "botaru/v1alpha1", "kind": "Group", "metadata": map[string]any{"name": "operators", "tenant": "demo"},
				"spec": map[string]any{"caps": []any{"service.control:nginx", "action.request:clean_tmp"}, "data": []any{"summary"}},
			},
			map[string]any{
				"apiVersion": "botaru/v1alpha1", "kind": "Group", "metadata": map[string]any{"name": "leads", "tenant": "demo"},
				"spec": map[string]any{"caps": []any{"team.view:requesters", "task.read:hello.greet.v1"}, "data": []any{"summary"}},
			},
			map[string]any{
				"apiVersion": "botaru/v1alpha1", "kind": "Group", "metadata": map[string]any{"name": "svc-approvers", "tenant": "demo"},
				"spec": map[string]any{"caps": []any{"action.approve:service.control"}, "data": []any{"summary"}},
			},
			map[string]any{
				"apiVersion": "botaru/v1alpha1", "kind": "Action", "metadata": map[string]any{"name": "clean_tmp"},
				"spec": map[string]any{
					"title": "Clean temporary files", "description": "Removes old temporary files of the site.", "risk": "routine",
					"args": map[string]any{
						"type": "object", "additionalProperties": false, "required": []any{"host"},
						"properties": map[string]any{"host": map[string]any{"enum_from": "catalog.hosts"}},
					},
					"idempotent": true, "writes_only": []any{"res:web1:tmp"}, "runs_per_hour": 2, "runs_per_day": 5,
					"executor": map[string]any{"plugin": "fixer", "tool": "clean_tmp", "os_mechanism": map[string]any{
						"oneshot": map[string]any{"unit": "clean-{tenant}.service", "started_by": "clean-{tenant}.path", "wait": false}}},
				},
			})
		return ms
	})
	ctx := context.Background()
	carol := ids.PrincipalID()
	if _, err := b.c.Principal(ctx, b.op, PrincipalRequest{Op: "principal.add", Subject: carol, Identity: "telegram:1003"}); err != nil {
		t.Fatal(err)
	}
	for _, m := range [][2]string{{b.alice, "operators"}, {b.bob, "svc-approvers"}} {
		if _, err := b.c.st.Pool.Exec(ctx, `INSERT INTO memberships (tenant, principal, grp, state, created_at) VALUES ('demo', $1, $2, 'active', now())`, m[0], m[1]); err != nil {
			t.Fatal(err)
		}
	}
	return &serviceBench{actionBench: b, svc: b.toolhost(t, "service"), carol: carol}
}

// control calls service_control and returns its result.
func (b *serviceBench) control(t *testing.T, handle, service, verb string) (serviceResult, *mcp.CallResult) {
	t.Helper()
	r, err := b.c.P3ToolsCall(context.Background(), handle, mcp.CallParams{Name: serviceToolName, Arguments: map[string]any{"service": service, "verb": verb}})
	if err != nil {
		t.Fatal(err)
	}
	var out serviceResult
	_ = json.Unmarshal([]byte(callText(r)), &out)
	return out, r
}

// say sends a direct message of a person to the bot.
func (b *serviceBench) say(t *testing.T, ext, update, text string) *ActivityResult {
	t.Helper()
	a := Activity{Kind: "message", Channel: "telegram", UpdateID: update, Chat: Chat{Kind: "direct", ExternalID: ext}, Text: text}
	a.Sender.Identity, a.Sender.Verified = "telegram:"+ext, true
	res, err := b.c.SubmitActivity(context.Background(), b.channel, a)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func (b *serviceBench) grants(t *testing.T) []map[string]any {
	t.Helper()
	rows, err := b.c.st.Pool.Query(context.Background(), `SELECT record FROM audit WHERE tenant = 'demo' AND record->>'kind' = 'service_grant' ORDER BY seq`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		out = append(out, m)
	}
	return out
}

// A bot restarts nginx without a card under a service_grant; the model gets the closed result
// and the bot's own task goes on; a stop goes as a card and runs after the decision (MON-8,
// APR-18).
func TestServiceControlBot(t *testing.T) {
	ctx := context.Background()
	b := newServiceBench(t)
	if _, h := b.attemptTools(t, "1", false); containsString(toolNames(t, b.bench, h), serviceToolName) {
		t.Fatal("a tier 0 task got service/control")
	}
	att, h := b.attemptTools(t, "2", true)
	if !containsString(toolNames(t, b.bench, h), serviceToolName) {
		t.Fatalf("catalog %v", toolNames(t, b.bench, h))
	}
	for _, args := range [][2]string{{"api", "restart"}, {"nginx", "reload"}} {
		if _, r := b.control(t, h, args[0], args[1]); !r.IsError || !strings.Contains(callText(r), "input_schema_invalid") {
			t.Fatalf("%v: %s", args, callText(r))
		}
	}
	ch := callAsync(b.bench, h, mcp.CallParams{Name: serviceToolName, Arguments: map[string]any{"service": "nginx", "verb": "restart"}})
	item := b.claimTool(t, b.svc)
	if item.Payload["grant_id"] == nil || item.Payload["approval_id"] != nil || item.Payload["tool"] != "control" {
		t.Fatalf("write call %v", item.Payload)
	}
	if res := fmt.Sprint(item.Payload["resolved"]); !strings.Contains(res, "service.unit:nginx.service") {
		t.Fatalf("resolved %v", item.Payload["resolved"])
	}
	if _, err := b.c.Complete(ctx, b.svc, header(t, item), CompleteRequest{Output: map[string]any{"content": []any{}, "structuredContent": map[string]any{"state": "active"}}}); err != nil {
		t.Fatal(err)
	}
	got := <-ch
	if got.err != nil {
		t.Fatal(got.err)
	}
	var res serviceResult
	_ = json.Unmarshal([]byte(callText(got.r)), &res)
	if res.Code != "done" || res.State != "active" || res.GrantID != item.Payload["grant_id"] {
		t.Fatalf("result %s", callText(got.r))
	}
	validOut(t, b.bench, "service-control-result", res)
	gs := b.grants(t)
	if len(gs) != 1 || gs[0]["target"].(map[string]any)["id"] != res.GrantID {
		t.Fatalf("grants %v", gs)
	}
	validOut(t, b.bench, "audit-record", gs[0])
	var state string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT state FROM tasks WHERE id = $1`, att.WorkID).Scan(&state); err != nil || state != "working" {
		t.Fatalf("the bot's task after the grant call: %s %v", state, err)
	}

	// stop goes by card to a person with action.approve:service.control.
	r, _ := b.control(t, h, "nginx", "stop")
	if r.Code != "approval_required" || r.ApprovalID == "" {
		t.Fatalf("stop %+v", r)
	}
	b.finish(t, att)
	b.tick(t, "approval-expiry")
	card := cardOf(t, b.drain(t), approvalCard, "direct:1002")
	a := b.approval(t, r.ApprovalID)
	if a.Action != serviceAction || a.Args["verb"] != "stop" {
		t.Fatalf("approval %+v", a)
	}
	validOut(t, b.bench, "approval", a.Record())
	if _, err := b.c.Decide(ctx, b.channel, press("telegram:1002", "direct:1002", card.external), DecideRequest{SubjectKind: "approval", ID: a.ID, Nonce: a.Nonce, Decision: "approve"}); err != nil {
		t.Fatal(err)
	}
	item = b.claimTool(t, b.svc)
	if item.Payload["approval_id"] != a.ID || item.Payload["args"].(map[string]any)["verb"] != "stop" {
		t.Fatalf("approved write call %v", item.Payload)
	}
}

// The initiator's own right, holds, untrusted text, the limits and one operation at a time
// (D-73, INJ-15, MON-8, HLD-3).
func TestServiceControlGuards(t *testing.T) {
	ctx := context.Background()
	b := newServiceBench(t)
	_, h := b.attemptTools(t, "1", true)

	// After untrusted text a restart goes by card.
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE tasks SET record = record || '{"untrusted_seen": true, "untrusted_source": "logs/tail"}' WHERE state = 'working'`); err != nil {
		t.Fatal(err)
	}
	if r, _ := b.control(t, h, "nginx", "restart"); r.Code != "approval_required" {
		t.Fatalf("restart after untrusted text %+v", r)
	}
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE tasks SET record = record || '{"untrusted_seen": false}' WHERE state = 'working'`); err != nil {
		t.Fatal(err)
	}
	// One operation per service at a time: the queued restart above is not a call yet, so
	// make one.
	ch := callAsync(b.bench, h, mcp.CallParams{Name: serviceToolName, Arguments: map[string]any{"service": "nginx", "verb": "restart"}})
	item := b.claimTool(t, b.svc)
	if r, _ := b.control(t, h, "nginx", "restart"); r.Code != "resource_busy" {
		t.Fatalf("a second operation at once %+v", r)
	}
	if _, err := b.c.Complete(ctx, b.svc, header(t, item), CompleteRequest{Output: map[string]any{"content": []any{}, "structuredContent": map[string]any{"state": "active"}}}); err != nil {
		t.Fatal(err)
	}
	<-ch
	// The seventh action on a service in an hour is refused and raises the alarm.
	if _, err := b.c.st.Pool.Exec(ctx, `INSERT INTO toolcalls (id, tenant, task, tool, access, args, target_plugin, target_placement, untrusted_output, state, grant_id, created_at, updated_at)
		SELECT 'c_' || lpad(g::text, 26, '0'), tenant, task, tool, access, args, target_plugin, target_placement, untrusted_output, 'done', 'sg_X', created_at, updated_at
		FROM toolcalls, generate_series(1, 5) g WHERE grant_id IS NOT NULL`); err != nil {
		t.Fatal(err)
	}
	if r, _ := b.control(t, h, "nginx", "restart"); r.Code != "service_rate_limited" {
		t.Fatalf("the seventh restart %+v", r)
	}
	var alarms int
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE type = 'alarm.service.control.rate.v1'`).Scan(&alarms); err != nil || alarms != 1 {
		t.Fatalf("alarms %d %v", alarms, err)
	}
	if _, err := b.c.st.Pool.Exec(ctx, `DELETE FROM toolcalls WHERE grant_id = 'sg_X'`); err != nil {
		t.Fatal(err)
	}
	// A hold on the machine stops it, and the model sees forbidden.
	if _, err := b.c.HoldSet(ctx, OperatorActor(b.op), HoldSetRequest{Op: "hold.set",
		Scope: HoldScope{Kind: "host", Tenant: "demo", Name: "web1"}, Effects: []string{"no_action"}, Reason: "manual"}); err != nil {
		t.Fatal(err)
	}
	if r, _ := b.control(t, h, "nginx", "restart"); r.Code != "forbidden" {
		t.Fatalf("under a hold %+v", r)
	}
	// Without the initiator's own right: forbidden with needs.
	if _, err := b.c.st.Pool.Exec(ctx, `DELETE FROM memberships WHERE principal = $1 AND grp = 'operators'`, b.alice); err != nil {
		t.Fatal(err)
	}
	_, h2 := b.attemptTools(t, "2", true)
	if r, _ := b.control(t, h2, "nginx", "restart"); r.Code != "forbidden" || len(r.Needs) != 1 || r.Needs[0] != "service.control:nginx" {
		t.Fatalf("without the initiator's right %+v", r)
	}
}

// /service from a person (PRM-18): a restart runs under a grant with the person as author and
// the outcome comes back as code text; a stop becomes an approval; without the right the
// person is told the group.
func TestServiceCommand(t *testing.T) {
	ctx := context.Background()
	b := newServiceBench(t)
	res := b.say(t, "1001", "10", "/service restart nginx")
	if res.Result != "reply" || !strings.Contains(res.Text, "accepted") {
		t.Fatalf("restart %+v", res)
	}
	if again := b.say(t, "1001", "10", "/service restart nginx"); again.Result != "task" || !again.Existing {
		t.Fatalf("a repeated update %+v", again)
	}
	gs := b.grants(t)
	if len(gs) != 1 || gs[0]["service_grant"].(map[string]any)["author"].(map[string]any)["principal"] != b.alice {
		t.Fatalf("grants %v", gs)
	}
	validOut(t, b.bench, "audit-record", gs[0])
	item := b.claimTool(t, b.svc)
	if _, err := b.c.Complete(ctx, b.svc, header(t, item), CompleteRequest{Output: map[string]any{"content": []any{}, "structuredContent": map[string]any{"state": "active"}}}); err != nil {
		t.Fatal(err)
	}
	var state, typ string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT state, type FROM tasks WHERE record->>'grant_id' = $1`, item.Payload["grant_id"]).Scan(&state, &typ); err != nil || state != "completed" || typ != "action.service.control" {
		t.Fatalf("grant task %s %s %v", state, typ, err)
	}
	var done bool
	for _, s := range b.drain(t) {
		if s.kind == "task_result" && s.body["text_key"] == "cmd.action.done" {
			done = true
		}
	}
	if !done {
		t.Fatal("no outcome to the person")
	}

	if res := b.say(t, "1001", "11", "/service stop nginx"); res.Result != "reply" || !strings.Contains(res.Text, "awaits") {
		t.Fatalf("stop %+v", res)
	}
	cardOf(t, b.drain(t), approvalCard, "direct:1002")
	if res := b.say(t, "1001", "12", "/service reload nginx"); !strings.Contains(res.Text, "no such service") {
		t.Fatalf("a service outside the catalog list %+v", res)
	}
	if res := b.say(t, "1003", "13", "/service restart nginx"); !strings.Contains(res.Text, "operators") {
		t.Fatalf("without the right %+v", res)
	}
}

// /run (APR-17): an action by card, a routine action under a grant within its counters.
func TestRunCommand(t *testing.T) {
	b := newServiceBench(t)
	if res := b.say(t, "1001", "20", "/run nope"); !strings.Contains(res.Text, "no such pipeline") {
		t.Fatalf("unknown %+v", res)
	}
	if res := b.say(t, "1001", "21", "/run fix web1"); !strings.Contains(res.Text, "awaits") {
		t.Fatalf("fix %+v", res)
	}
	if res := b.say(t, "1001", "22", "/run fix db9"); !strings.Contains(res.Text, "do not fit") {
		t.Fatalf("fix outside the catalog %+v", res)
	}
	for i, update := range []string{"23", "24"} {
		if res := b.say(t, "1001", update, "/run clean web1"); !strings.Contains(res.Text, "started") {
			t.Fatalf("clean %d %+v", i, res)
		}
	}
	if res := b.say(t, "1001", "25", "/run clean web1"); !strings.Contains(res.Text, "limit") {
		t.Fatalf("the third routine run in an hour %+v", res)
	}
	gs := b.grants(t)
	if len(gs) != 2 || gs[0]["service_grant"].(map[string]any)["action"] != "clean_tmp" {
		t.Fatalf("grants %v", gs)
	}
	item := b.claimTool(t, b.th)
	if item.Payload["grant_id"] == nil || item.Payload["tool"] != "clean_tmp" {
		t.Fatalf("routine write call %v", item.Payload)
	}
	if res := b.say(t, "1003", "26", "/run clean web1"); !strings.Contains(res.Text, "operators") {
		t.Fatalf("without the right %+v", res)
	}
}

// A /run argument from a marked catalog resource is checked at execution with the mark it was
// requested with, not dropped as outside the catalog (MAN-10).
func TestRunMarkedHost(t *testing.T) {
	ctx := context.Background()
	b := newActionBench(t, func(ms []map[string]any) []map[string]any {
		for _, m := range ms {
			s := m["spec"].(map[string]any)
			switch m["kind"] {
			case "Catalog":
				s["resources"] = append(asList(s["resources"]), map[string]any{"id": "sites", "title": "Sites", "host": "web1", "list": "sites", "values": []any{"web1"}, "data_class": "internal"})
			case "Action":
				s["args"].(map[string]any)["properties"].(map[string]any)["host"] = map[string]any{"enum_from": "catalog.sites"}
			case "Tenant":
				s["run"] = map[string]any{"fix": map[string]any{"action": "fix_web", "args": []any{"host"}}}
			case "Group":
				if m["metadata"].(map[string]any)["name"] == "requesters" {
					s["data"] = []any{"summary", "internal"}
				}
			}
		}
		return ms
	})
	sb := &serviceBench{actionBench: b}
	if res := sb.say(t, "1001", "50", "/run fix web1"); !strings.Contains(res.Text, "awaits") {
		t.Fatalf("fix %+v", res)
	}
	s := b.drain(t)
	card := cardOf(t, s, approvalCard, "direct:1002")
	var id string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT id FROM approvals WHERE state = 'pending'`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	a := b.approval(t, id)
	if _, err := b.c.Decide(ctx, b.channel, press("telegram:1002", "direct:1002", card.external),
		DecideRequest{SubjectKind: "approval", ID: a.ID, Nonce: a.Nonce, Decision: "approve"}); err != nil {
		t.Fatal(err)
	}
	if a = b.approval(t, id); a.State != "executing" {
		t.Fatalf("approval %s %s", a.State, a.Reason)
	}
}
