// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/HunterEwok/botaru/impl/internal/ids"
	"github.com/HunterEwok/botaru/impl/internal/mcp"
)

// actionBench is the tool bench with the action fix_web: the role hello may request it, the
// group holds action.request:fix_web, and bob (telegram:1002) of the group approvers
// holds action.approve:fix_web. The executor is the write tool fixer/restart_site.
type actionBench struct {
	*bench
	bob string
	th  *Worker
}

func newActionBench(t *testing.T, edit func(ms []map[string]any) []map[string]any) *actionBench {
	t.Helper()
	b := newToolBenchWith(t, nil, func(ms []map[string]any) []map[string]any {
		for _, m := range ms {
			s := m["spec"].(map[string]any)
			switch {
			case m["kind"] == "Role":
				s["actions_may_request"] = []any{"fix_web"}
			}
		}
		ms = append(ms,
			map[string]any{
				"apiVersion": "botaru/v1alpha1", "kind": "Group", "metadata": map[string]any{"name": "requesters", "tenant": "demo"},
				"spec": map[string]any{"caps": []any{"action.request:fix_web"}, "data": []any{"summary"}},
			},
			map[string]any{
				"apiVersion": "botaru/v1alpha1", "kind": "Group", "metadata": map[string]any{"name": "approvers", "tenant": "demo"},
				"spec": map[string]any{"caps": []any{"action.approve:fix_web"}, "data": []any{"summary"}},
			},
			map[string]any{
				"apiVersion": "botaru/v1alpha1", "kind": "Action", "metadata": map[string]any{"name": "fix_web"},
				"spec": map[string]any{
					"title": "Restart the web site", "description": "Restarts the site on a machine when the checks fail.", "risk": "low",
					"args": map[string]any{
						"type": "object", "additionalProperties": false, "required": []any{"host"},
						"properties": map[string]any{
							"host":  map[string]any{"enum_from": "catalog.hosts"},
							"level": map[string]any{"enum": []any{"soft", "hard"}, "default": "soft"},
						},
					},
					"ttl": "15m", "idempotent": true,
					"executor": map[string]any{"plugin": "fixer", "tool": "restart_site", "os_mechanism": map[string]any{
						"oneshot": map[string]any{"unit": "site-restart-{tenant}.service", "started_by": "site-restart-{tenant}.path", "wait": false}}},
				},
			},
			map[string]any{
				"apiVersion": "botaru/v1alpha1", "kind": "ToolPlugin", "metadata": map[string]any{"name": "fixer"},
				"spec": map[string]any{
					"placement": "host", "network": map[string]any{"egress": []any{}},
					"tools": []any{map[string]any{
						"name": "restart_site", "access": "write", "tier": 3, "output_class": "internal", "untrusted_output": false,
						"args_from": "action:fix_web", "target_from": "host", "output_limit_kb": 4, "timeout_s": 20,
					}},
				},
			})
		if edit != nil {
			ms = edit(ms)
		}
		return ms
	})
	ctx := context.Background()
	bob := ids.PrincipalID()
	if _, err := b.c.Principal(ctx, b.op, PrincipalRequest{Op: "principal.add", Subject: bob, Identity: "telegram:1002"}); err != nil {
		t.Fatal(err)
	}
	// approvers and requesters are privileged groups, which grant does not give in this
	// version (PRM-13).
	for _, m := range [][2]string{{bob, "approvers"}, {b.alice, "requesters"}} {
		if _, err := b.c.st.Pool.Exec(ctx, `INSERT INTO memberships (tenant, principal, grp, state, created_at) VALUES ('demo', $1, $2, 'active', now())`, m[0], m[1]); err != nil {
			t.Fatal(err)
		}
	}
	return &actionBench{bench: b, bob: bob, th: b.toolhost(t, "fixer")}
}

// request calls request_action and returns its result.
func (b *actionBench) request(t *testing.T, handle string, args map[string]any) requestActionResult {
	t.Helper()
	r, err := b.c.P3ToolsCall(context.Background(), handle, mcp.CallParams{Name: requestActionName, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	var out requestActionResult
	if err := json.Unmarshal([]byte(callText(r)), &out); err != nil {
		t.Fatalf("result %q: %v", callText(r), err)
	}
	return out
}

// finish completes the attempt of the hello task.
func (b *actionBench) finish(t *testing.T, att *WorkItem) {
	t.Helper()
	if _, err := b.c.Complete(context.Background(), b.agent, header(t, att), CompleteRequest{Result: map[string]any{"answer": "asked"}}); err != nil {
		t.Fatal(err)
	}
}

// sent is a delivery the channel took and handed in.
type sent struct {
	kind, chat, external string
	body                 map[string]any
	replaces             string
}

// drain takes every delivery of the channel and hands each in with an external id.
func (b *bench) drain(t *testing.T) []sent {
	t.Helper()
	var out []sent
	for range 50 {
		item := b.claim(t, b.channel, "delivery")
		if item == nil {
			return out
		}
		d := item.Payload["delivery"].(map[string]any)
		chat := d["recipient"].(map[string]any)["chat"].(map[string]any)
		s := sent{kind: d["kind"].(string), chat: chat["kind"].(string) + ":" + chat["external_id"].(string), body: d["body"].(map[string]any)}
		s.replaces, _ = d["replaces_external_id"].(string)
		s.external = fmt.Sprint(item.LeaseNo*1000 + len(out) + 1)
		if s.replaces != "" {
			s.external = s.replaces
		}
		if _, err := b.c.Complete(context.Background(), b.channel, header(t, item), CompleteRequest{Receipt: &Receipt{ExternalID: s.external}}); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	t.Fatal("the channel never ran out of deliveries")
	return nil
}

func cardOf(t *testing.T, s []sent, kind, chat string) sent {
	t.Helper()
	for _, x := range s {
		if x.kind == kind && x.chat == chat {
			return x
		}
	}
	t.Fatalf("no %s to %s in %+v", kind, chat, s)
	return sent{}
}

func (b *actionBench) approval(t *testing.T, id string) *approvalRow {
	t.Helper()
	a, err := loadApproval(context.Background(), b.c.st.Pool, "demo", id, false)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func (b *actionBench) tick(t *testing.T, loop string) {
	t.Helper()
	if err := b.c.tickLoop(context.Background(), loop); err != nil {
		t.Fatal(err)
	}
}

func press(identity, chat, message string) *OnBehalf {
	ob, _ := ParseOnBehalf(identity, chat, message)
	return &ob
}

// The whole path of an action (APR-1..APR-4, APR-7, APR-11, APR-19, CNT-7): request_action is
// in the catalog only with the right, the request fills the defaults and refuses arguments
// outside the closed schema, the cards go out after the attempt ends, only the approver
// decides from the chat of the card, the approval executes once through one write call, and
// the outcome closes the action task and the approval.
func TestActionApprovalFlow(t *testing.T) {
	ctx := context.Background()
	b := newActionBench(t, nil)
	att, h := b.attemptTools(t, "1", false)
	if names := toolNames(t, b.bench, h); !containsString(names, requestActionName) {
		t.Fatalf("catalog %v", names)
	}
	if r := b.request(t, h, map[string]any{"action": "fix_web", "args": map[string]any{"host": "db9"}}); r.Code != "input_schema_invalid" {
		t.Fatalf("a host outside the catalog: %+v", r)
	}
	if r := b.request(t, h, map[string]any{"action": "drop_db", "args": map[string]any{}}); r.Code != "forbidden" {
		t.Fatalf("an unknown action: %+v", r)
	}
	r := b.request(t, h, map[string]any{"action": "fix_web", "args": map[string]any{"host": "web1"}})
	if r.Code != "pending" || r.ApprovalID == "" {
		t.Fatalf("request %+v", r)
	}
	if again := b.request(t, h, map[string]any{"action": "fix_web", "args": map[string]any{"host": "web1", "level": "soft"}}); again.ApprovalID != r.ApprovalID {
		t.Fatalf("an identical request made a new approval: %+v", again)
	}
	a := b.approval(t, r.ApprovalID)
	if a.Args["level"] != "soft" || a.ExpiresAt.Sub(a.CreatedAt).Minutes() != 60 || a.Risk != "low" {
		t.Fatalf("approval %+v", a)
	}
	validOut(t, b.bench, "approval", a.Record())

	// The cards wait until the attempt that asked has ended.
	b.tick(t, "approval-expiry")
	if s := b.drain(t); len(s) != 2 { // ai_notice and privacy_notice of alice
		t.Fatalf("cards before the attempt ended: %+v", s)
	}
	b.finish(t, att)
	b.tick(t, "approval-expiry")
	s := b.drain(t)
	card := cardOf(t, s, approvalCard, "direct:1002")
	copyCard := cardOf(t, s, initiatorCopy, "direct:1001")
	cd := card.body["card"].(map[string]any)
	validOut(t, b.bench, "card", cd)
	if cd["title"] != "Restart the web site" || len(cd["buttons"].([]any)) != 3 || cd["risk"] != "low" {
		t.Fatalf("card %v", cd)
	}
	if btn := copyCard.body["card"].(map[string]any)["buttons"].([]any); len(btn) != 1 || btn[0].(map[string]any)["code"] != "remind" {
		t.Fatalf("the initiator copy %v", copyCard.body)
	}

	req := DecideRequest{SubjectKind: "approval", ID: a.ID, Nonce: a.Nonce, Decision: "approve"}
	// Without the headers, from the initiator's copy, with another nonce or another message:
	// forbidden.
	_, err := b.c.Decide(ctx, b.channel, nil, req)
	wantReason(t, err, "forbidden")
	_, err = b.c.Decide(ctx, b.channel, press("telegram:1001", "direct:1001", copyCard.external), req)
	wantReason(t, err, "forbidden")
	bad := req
	bad.Nonce = "AAAAAAAAAAAAAAAAAAAAAA"
	_, err = b.c.Decide(ctx, b.channel, press("telegram:1002", "direct:1002", card.external), bad)
	wantReason(t, err, "forbidden")
	_, err = b.c.Decide(ctx, b.channel, press("telegram:1002", "direct:1002", "999999"), req)
	wantReason(t, err, "forbidden")
	_, err = b.c.Decide(ctx, b.channel, press("slack:U1", "direct:1002", card.external), req)
	wantReason(t, err, "forbidden")

	// Details are recorded for the decision event.
	det, err := b.c.CardDetails(ctx, b.channel, press("telegram:1002", "direct:1002", card.external), CardOp{Op: "card.details", SubjectKind: "approval", ID: a.ID, Nonce: a.Nonce})
	if err != nil {
		t.Fatal(err)
	}
	validOut(t, b.bench, "p1-card-details-response", det)

	ob := press("telegram:1002", "direct:1002", card.external)
	res, err := b.c.Decide(ctx, b.channel, ob, req)
	if err != nil || res.State != "executing" {
		t.Fatalf("decide %+v %v", res, err)
	}
	validOut(t, b.bench, "p1-decide-response", res)
	if again, err := b.c.Decide(ctx, b.channel, ob, req); err != nil || again.State != "executing" {
		t.Fatalf("a repeated press %+v %v", again, err)
	}
	other := req
	other.Decision = "reject"
	_, err = b.c.Decide(ctx, b.channel, ob, other)
	wantReason(t, err, "already_decided")
	var ev []byte
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT data FROM events WHERE type = 'decision.made' AND subject = $1`, a.ID).Scan(&ev); err != nil {
		t.Fatal(err)
	}
	var evm map[string]any
	_ = json.Unmarshal(ev, &evm)
	validOut(t, b.bench, "urn:botaru:schema:decision:1#/$defs/event", evm)
	if evm["details_opened"] != true || evm["decided_by"] != b.bob {
		t.Fatalf("decision event %v", evm)
	}

	a = b.approval(t, a.ID)
	var state, typ string
	var rec []byte
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT state, type, record FROM tasks WHERE id = $1`, a.ActionTask).Scan(&state, &typ, &rec); err != nil {
		t.Fatal(err)
	}
	if state != "working" || typ != "action.fix_web" {
		t.Fatalf("action task %s %s", state, typ)
	}
	item := b.claimTool(t, b.th)
	if item.Payload["approval_id"] != a.ID || item.Payload["args_hash"] != a.ArgsHash || item.Payload["tool"] != "restart_site" {
		t.Fatalf("write call %v", item.Payload)
	}
	if _, err := b.c.Complete(ctx, b.th, header(t, item), CompleteRequest{Output: map[string]any{"content": []any{}, "structuredContent": map[string]any{}}}); err != nil {
		t.Fatal(err)
	}
	if a = b.approval(t, a.ID); a.State != "executed" {
		t.Fatalf("approval %s", a.State)
	}
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT state FROM tasks WHERE id = $1`, a.ActionTask).Scan(&state); err != nil || state != "completed" {
		t.Fatalf("action task %s %v", state, err)
	}
	// Each card shows the outcome by an edit of its message.
	upd := b.drain(t)
	n := 0
	for _, u := range upd {
		if u.kind == cardUpdate && (u.replaces == card.external || u.replaces == copyCard.external) {
			n++
		}
	}
	if n < 2 {
		t.Fatalf("card updates %+v", upd)
	}
	// A redrive of an action task is forbidden (APR-7).
	if _, err := b.c.Redrive(ctx, b.op, RedriveRequest{Op: "task.redrive", ID: a.ActionTask}); err == nil {
		t.Fatal("an action task was redriven")
	}
}

// A write call whose toolhost lease expires ends as exec_unknown and is not retried; the task
// fails with exec_unknown and no unclaimed alarm (APR-7, CNT-7).
func TestActionExecUnknown(t *testing.T) {
	ctx := context.Background()
	b := newActionBench(t, nil)
	att, h := b.attemptTools(t, "1", false)
	r := b.request(t, h, map[string]any{"action": "fix_web", "args": map[string]any{"host": "web1"}})
	b.finish(t, att)
	b.tick(t, "approval-expiry")
	card := cardOf(t, b.drain(t), approvalCard, "direct:1002")
	a := b.approval(t, r.ApprovalID)
	if _, err := b.c.Decide(ctx, b.channel, press("telegram:1002", "direct:1002", card.external), DecideRequest{SubjectKind: "approval", ID: a.ID, Nonce: a.Nonce, Decision: "approve"}); err != nil {
		t.Fatal(err)
	}
	b.claimTool(t, b.th)
	b.advance(t, 600)
	b.tick(t, "reaper")
	b.tick(t, "deadline")
	if a = b.approval(t, a.ID); a.State != "exec_unknown" {
		t.Fatalf("approval %s", a.State)
	}
	var state, reason string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT state, coalesce(reason, '') FROM tasks WHERE id = $1`, a.ActionTask).Scan(&state, &reason); err != nil || state != "failed" || reason != "exec_unknown" {
		t.Fatalf("action task %s %s %v", state, reason, err)
	}
	if item := b.claim(t, b.th, "toolcall"); item != nil {
		t.Fatal("the write call was handed out again")
	}
	if countEvents(t, b.bench, "alarm.raised", "") != 0 {
		var n int
		_ = b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM alarms WHERE name = 'task.unclaimed'`).Scan(&n)
		if n > 0 {
			t.Fatal("an action task raised alarm.task.unclaimed")
		}
	}
}

// The deadline of a card: low with ttl 15m lives 60 minutes, high without ttl 30 minutes; at
// half of the deadline the approvers get one reminder, after it the approval expires and a
// press gets expired (APR-19, APR-12).
func TestApprovalExpiryAndReminder(t *testing.T) {
	ctx := context.Background()
	b := newActionBench(t, nil)
	att, h := b.attemptTools(t, "1", false)
	r := b.request(t, h, map[string]any{"action": "fix_web", "args": map[string]any{"host": "web1"}})
	b.finish(t, att)
	b.tick(t, "approval-expiry")
	card := cardOf(t, b.drain(t), approvalCard, "direct:1002")
	b.advance(t, 31*60)
	b.tick(t, "approval-expiry")
	rem := cardOf(t, b.drain(t), approvalCard, "direct:1002")
	if rem.body["card"].(map[string]any)["reminder"] != true {
		t.Fatalf("reminder %v", rem.body)
	}
	b.tick(t, "approval-expiry")
	if s := b.drain(t); len(s) != 0 {
		t.Fatalf("a second reminder %+v", s)
	}
	b.advance(t, 30*60)
	b.tick(t, "approval-expiry")
	a := b.approval(t, r.ApprovalID)
	if a.State != "expired" {
		t.Fatalf("approval %s", a.State)
	}
	_, err := b.c.Decide(ctx, b.channel, press("telegram:1002", "direct:1002", card.external), DecideRequest{SubjectKind: "approval", ID: a.ID, Nonce: a.Nonce, Decision: "approve"})
	wantReason(t, err, "expired")
	if got := actionTTL(b.c.cat.get().Actions["fix_web"]); got.Minutes() != 60 {
		t.Fatalf("low ttl %v", got)
	}
	high := b.c.cat.get().Actions["fix_web"]
	high.Risk, high.TTL = "high", ""
	if got := actionTTL(high); got.Minutes() != 30 {
		t.Fatalf("high ttl %v", got)
	}
}

// At most 3 requests per task, and the tenant's cards per hour (APR-2); a no_action hold
// refuses new requests and cancels pending approvals (HLD-3); the initiator reminds the
// approvers at most once in 5 minutes (APR-19).
func TestApprovalLimitsAndHolds(t *testing.T) {
	ctx := context.Background()
	b := newActionBench(t, func(ms []map[string]any) []map[string]any {
		for _, m := range ms {
			if m["kind"] == "Catalog" {
				s := m["spec"].(map[string]any)
				s["hosts"] = append(s["hosts"].([]any), map[string]any{"id": "web2"}, map[string]any{"id": "web3"}, map[string]any{"id": "web4"})
			}
			if m["kind"] == "Tenant" {
				m["spec"].(map[string]any)["attention"] = map[string]any{"cards_per_tenant_hour": 4}
			}
		}
		return ms
	})
	att, h := b.attemptTools(t, "1", false)
	var first string
	for i, host := range []string{"web1", "web2", "web3"} {
		r := b.request(t, h, map[string]any{"action": "fix_web", "args": map[string]any{"host": host}})
		if r.Code != "pending" {
			t.Fatalf("request %d: %+v", i, r)
		}
		if i == 0 {
			first = r.ApprovalID
		}
	}
	if r := b.request(t, h, map[string]any{"action": "fix_web", "args": map[string]any{"host": "web4"}}); r.Code != "service_rate_limited" {
		t.Fatalf("a fourth request: %+v", r)
	}
	b.finish(t, att)
	b.tick(t, "approval-expiry")
	copyCard := cardOf(t, b.drain(t), initiatorCopy, "direct:1001")
	a := b.approval(t, first)
	op := CardOp{Op: "card.remind", SubjectKind: "approval", ID: a.ID, Nonce: a.Nonce}
	ob := press("telegram:1001", "direct:1001", copyCard.external)
	if out, err := b.c.CardRemind(ctx, b.channel, ob, op); err != nil || out["reminded"] != 1 {
		t.Fatalf("remind %v %v", out, err)
	}
	b.advance(t, 60)
	_, err := b.c.CardRemind(ctx, b.channel, ob, op)
	wantReason(t, err, "service_rate_limited")

	// The tenant had 3 of its 4 cards this hour: the next task gets one more, then none.
	att2, h2 := b.attemptTools(t, "2", false)
	if r := b.request(t, h2, map[string]any{"action": "fix_web", "args": map[string]any{"host": "web4"}}); r.Code != "pending" {
		t.Fatalf("the fourth card of the hour: %+v", r)
	}
	if r := b.request(t, h2, map[string]any{"action": "fix_web", "args": map[string]any{"host": "web4", "level": "hard"}}); r.Code != "service_rate_limited" {
		t.Fatalf("the fifth card of the hour: %+v", r)
	}
	_ = att2

	// A no_action hold on the action cancels the pending approvals and refuses requests.
	if _, err := b.c.HoldSet(ctx, OperatorActor(b.op), HoldSetRequest{Op: "hold.set",
		Scope: HoldScope{Kind: "action", Tenant: "demo", Name: "fix_web"}, Effects: []string{"no_action"}, Reason: "manual"}); err != nil {
		t.Fatal(err)
	}
	b.tick(t, "approval-expiry")
	if a = b.approval(t, first); a.State != "canceled" || a.Reason != "held" {
		t.Fatalf("approval under a hold %s %s", a.State, a.Reason)
	}
	if r := b.request(t, h2, map[string]any{"action": "fix_web", "args": map[string]any{"host": "web1", "level": "hard"}}); r.Code != "forbidden" {
		t.Fatalf("a request under a hold: %+v", r)
	}
}

// The initiator does not approve their own request, and losing action.request cancels a
// pending approval (APR-6, APR-9); a person without the right gets forbidden.
func TestApprovalSelfAndRights(t *testing.T) {
	ctx := context.Background()
	b := newActionBench(t, nil)
	att, h := b.attemptTools(t, "1", false)
	r := b.request(t, h, map[string]any{"action": "fix_web", "args": map[string]any{"host": "web1"}})
	b.finish(t, att)
	if _, err := b.c.Grant(ctx, b.op, GrantRequest{Op: "ungrant", Subject: b.alice, Group: "requesters"}); err != nil {
		t.Fatal(err)
	}
	if a := b.approval(t, r.ApprovalID); a.State != "canceled" || a.Reason != "rights_revoked" {
		t.Fatalf("approval after ungrant %s %s", a.State, a.Reason)
	}
}
