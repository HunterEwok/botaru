// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/ids"
	"github.com/HunterEwok/botaru/impl/internal/manifest"
	"github.com/HunterEwok/botaru/impl/internal/mcp"
	"github.com/HunterEwok/botaru/impl/internal/money"
)

// managerWorkFiles are the example manifests of the directive, the daily review and the reading.
var managerWorkFiles = []string{"examples/tasktypes/manager.directive.v1.yaml", "examples/tasktypes/manager.review.v1.yaml",
	"examples/tasktypes/manager.inspect.v1.yaml", "examples/roles/farm-manager-reader/role.yaml"}

// directors is a group of demo with manager.direct, control.apply:hold and :drain and control.approve; a
// privileged group, so the tests write its memberships into the store.
var directors = map[string]any{"apiVersion": "botaru/v1alpha1", "kind": "Group", "metadata": map[string]any{"name": "directors", "tenant": "demo"},
	"spec": map[string]any{"caps": []any{"manager.direct", "control.apply:hold", "control.apply:drain", "control.approve"}, "data": []any{"summary"}}}

// workEdit lets the tenant's consent group for the manager allow every seat type and keeps the
// reader role to the plugins of the bench.
func workEdit(ms []map[string]any) {
	for _, m := range ms {
		md, _ := m["metadata"].(map[string]any)
		spec, _ := m["spec"].(map[string]any)
		switch {
		case m["kind"] == "Group" && md["name"] == "farm-manager-ops":
			caps, _ := spec["caps"].([]any)
			spec["caps"] = append(caps, "task:"+directiveType, "task:"+reviewType, "task:"+inspectType)
		case m["kind"] == "Role" && md["name"] == "farm-manager-reader":
			delete(spec, "tools")
		}
	}
}

func newManagerWorkBench(t *testing.T) *bench {
	t.Helper()
	return newManagerBenchWith(t, managerWorkFiles, []map[string]any{directors}, workEdit)
}

// seatCall calls a built-in tool through a tools handle and returns its answer.
func seatCall(t *testing.T, b *bench, handle, name string, args map[string]any) map[string]any {
	t.Helper()
	r, err := b.c.P3ToolsCall(context.Background(), handle, mcp.CallParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(callText(r)), &out); err != nil {
		t.Fatalf("result %q: %v", callText(r), err)
	}
	return out
}

func taskRecord(t *testing.T, b *bench, id string) (string, int, map[string]any, map[string]any) {
	t.Helper()
	var typ string
	var class int
	var rec, body []byte
	if err := b.c.st.Pool.QueryRow(context.Background(), `SELECT type, priority_class, record, body FROM tasks WHERE id = $1`, id).Scan(&typ, &class, &rec, &body); err != nil {
		t.Fatal(err)
	}
	var r, bd map[string]any
	if err := json.Unmarshal(rec, &r); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(body, &bd); err != nil {
		t.Fatal(err)
	}
	return typ, class, r, bd
}

// A directive of a person with manager.direct is a manage task in the tenant with the closed
// fields in the input block and the person's text as the goal; tenant farm and a bad scope are
// refused. The seat applies operations only within the directive's scope (MGR-4).
func TestManagerDirect(t *testing.T) {
	ctx := context.Background()
	b := newManagerWorkBench(t)
	_, err := b.c.ManagerDirect(ctx, b.farmOp(), ManagerDirectRequest{Scope: ControlTarget{Kind: "role", ID: "hello"}})
	wantReason(t, err, "manager_disabled")
	_, err = b.c.ManagerDirect(ctx, b.op, ManagerDirectRequest{Scope: ControlTarget{Kind: "task", ID: "x"}})
	wantReason(t, err, "input_schema_invalid")
	d, err := b.c.ManagerDirect(ctx, b.op, ManagerDirectRequest{Scope: ControlTarget{Kind: "resource", ID: "db1"}, Op: "hold", Text: "Pause writes to db1."})
	if err != nil {
		t.Fatal(err)
	}
	typ, class, rec, body := taskRecord(t, b, d.ID)
	dir := asMap(rec["directive"])
	if typ != directiveType || class != 2 || rec["purpose"] != "manage" || rec["executor_role"] != "farm-manager" || body["goal"] != "Pause writes to db1." ||
		dir["op"] != "hold" || dir["via"] != "farmctl" || asMap(dir["scope"])["id"] != "db1" {
		t.Fatalf("directive task %s %d %v %v", typ, class, rec, body)
	}
	w := b.enroll(t, EnrollRequest{Tenant: "demo", Kind: "agent", Role: "farm-manager"})
	if _, err := b.c.SeatAssign(ctx, b.farmOp(), SeatRequest{Seat: managerSeatID}); err != nil {
		t.Fatal(err)
	}
	item := b.claim(t, w, "attempt")
	if item == nil || item.Payload["task"].(map[string]any)["id"] != d.ID {
		t.Fatalf("the directive was not handed out: %+v", item)
	}
	handle := item.Payload["handles"].(map[string]any)["tools"].(string)
	hold := func(kind, id string) string {
		out := seatCall(t, b, handle, controlToolName, map[string]any{"op": "hold", "target": map[string]any{"kind": kind, "id": id},
			"effects": []any{"no_claim"}, "ttl": "30m"})
		return out["code"].(string)
	}
	if code := hold("resource", "db2"); code != "target_not_implicated" {
		t.Fatalf("a target outside the scope: %s", code)
	}
	if code := hold("resource", "db1"); code != "applied" {
		t.Fatalf("the scope: %s", code)
	}
	out := seatCall(t, b, handle, controlToolName, map[string]any{"op": "drain", "target": map[string]any{"kind": "resource", "id": "db1"}})
	if out["code"] != "forbidden" {
		t.Fatalf("another operation than the directive's: %v", out)
	}
}

// /manager in a private chat parses the closed fields and needs manager.direct; a forwarded or
// quoted /manager creates no task (MGR-4).
func TestManagerCommand(t *testing.T) {
	ctx := context.Background()
	b := newManagerWorkBench(t)
	send := func(update, text string, quoted bool) *ActivityResult {
		a := Activity{Kind: "message", Channel: "telegram", UpdateID: update, Chat: Chat{Kind: "direct", ExternalID: "1001"}, Text: text, Quoted: quoted}
		a.Sender.Identity, a.Sender.Verified = "telegram:1001", true
		res, err := b.c.SubmitActivity(ctx, b.channel, a)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	count := func() int {
		var n int
		if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE type = $1`, directiveType).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if res := send("m1", "/manager drain worker w_7 it loops", true); res.Result != "reply" || count() != 0 {
		t.Fatalf("a quoted directive: %+v, %d tasks", res, count())
	}
	if res := send("m2", "/manager", false); res.Result != "reply" || count() != 0 {
		t.Fatalf("a bare /manager: %+v", res)
	}
	if res := send("m3", "/manager drain worker w_7 it loops", false); res.Result != "reply" || count() != 0 {
		t.Fatalf("a directive without manager.direct: %+v, %d tasks", res, count())
	}
	b.direct(t, b.alice)
	if res := send("m4", "/manager drain worker w_7 it loops", false); res.Result != "reply" || count() != 1 {
		t.Fatalf("a directive: %+v, %d tasks", res, count())
	}
	a := Activity{Kind: "message", Channel: "telegram", UpdateID: "m5", Chat: Chat{Kind: "direct", ExternalID: "1001"}, Text: "/manager drain worker w_7 see file",
		Attachments: []string{"file_1"}}
	a.Sender.Identity, a.Sender.Verified = "telegram:1001", true
	if res, err := b.c.SubmitActivity(ctx, b.channel, a); err != nil || res.Result != "reply" || count() != 1 {
		t.Fatalf("a directive with an attachment: %+v %v, %d tasks", res, err, count())
	}
	var rec []byte
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT record FROM tasks WHERE type = $1`, directiveType).Scan(&rec); err != nil {
		t.Fatal(err)
	}
	d := taskDirective(rec)
	if d == nil || d.Via != "channel" || d.By != b.alice || d.Op != "drain" || d.Scope != (ControlTarget{Kind: "worker", ID: "w_7"}) {
		t.Fatalf("directive %+v", d)
	}
}

// The set lint refuses a manage type in may_enqueue and in a continuation (MGR-4); three
// readings of a worst-case incident at manager-std prices cost 0.624 EUR (MGR-20).
func TestManagerLint(t *testing.T) {
	c := lintCore(t)
	codes := lintCodes(t, c, `
apiVersion: botaru/v1alpha1
kind: TaskType
metadata: {name: manager.directive.v1}
spec: {title: D, worker_kind: agent, purpose: manage, tier: 1, data_class: internal, proposable: false}
---
apiVersion: botaru/v1alpha1
kind: TaskType
metadata: {name: ops.watch.v1}
spec: {title: W, worker_kind: agent, purpose: work, tier: 1, data_class: internal, continuation: {type: manager.directive.v1}}
---
apiVersion: botaru/v1alpha1
kind: Role
metadata: {name: ops-runner}
spec: {worker_kind: agent, claims: [ops.watch.v1], may_enqueue: [manager.directive.v1], memory: none}
`)
	if !slices.Contains(codes, "manage_enqueued") {
		t.Fatalf("codes %v", codes)
	}
	if got := seatWorstReadMicros(4.0, 16.0, 8000); got != 624000 {
		t.Fatalf("worst case %v", got)
	}
}

// The core enqueues manager.review.v1 once a day in a tenant with the manager, with
// daily_review_max_eur as its tree budget; manager.cost shows the manager line with the review
// as a line of its own; work outside sev1 stops at 70% of the line's day (MGR-20, MGR-41).
func TestManagerDaily(t *testing.T) {
	ctx := context.Background()
	b := newManagerWorkBench(t)
	b.loop(t, "identity")
	b.loop(t, "identity")
	var id string
	var n int
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT min(id), count(*) FROM tasks WHERE type = $1 AND tenant = 'demo'`, reviewType).Scan(&id, &n); err != nil || n != 1 {
		t.Fatalf("reviews %d %v", n, err)
	}
	var farmDays int
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM manager_days WHERE tenant = $1`, farmTenant).Scan(&farmDays); err != nil || farmDays != 1 {
		t.Fatalf("the farm's day mark %d %v", farmDays, err)
	}
	_, _, rec, _ := taskRecord(t, b, id)
	if rec["tree_budget_micros"] != float64(400000) || rec["executor_role"] != "farm-manager" || rec["seat"] != managerSeatID {
		t.Fatalf("review record %v", rec)
	}
	now := b.now(t)
	if _, err := b.c.st.Pool.Exec(ctx, `INSERT INTO spend (id, tenant, task, line, unit, state, eur_micros, provider, time) VALUES
		($1, 'demo', $2, 'manager', 'tokens', 'committed', 300000, 'mock', $4), ($3, 'demo', NULL, 'manager', 'tokens', 'committed', 1200000, 'mock', $4)`,
		ids.New(ids.Spend), id, ids.New(ids.Spend), now); err != nil {
		t.Fatal(err)
	}
	cost, err := b.c.ManagerCost(ctx, b.op, ManagerCostRequest{})
	if err != nil || cost["spent"] != int64(150) || asMap(cost["lines"])["review"] != int64(30) {
		t.Fatalf("cost %v %v", cost, err)
	}
	// 1.5 EUR of the 2 EUR day: a directive is refused, a sev1 ask still fits.
	_, err = b.c.ManagerDirect(ctx, b.op, ManagerDirectRequest{Scope: ControlTarget{Kind: "resource", ID: "db1"}})
	wantReason(t, err, "budget_exhausted")
	cat, err := b.c.Catalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// The review's open reserve counts too: 1.9 EUR of 2 with the ask.
	if r := checkAsk(t, b, cat.Tenants["demo"], budgetAsk{subject: "system", extra: money.FromEUR(0.05), line: "manager"}); r != "budget_exhausted" {
		t.Fatalf("an ask outside sev1: %q", r)
	}
	if r := checkAsk(t, b, cat.Tenants["demo"], budgetAsk{subject: "system", extra: money.FromEUR(0.05), line: "manager", sev1: true}); r != "" {
		t.Fatalf("a sev1 ask: %q", r)
	}
}

// A reading: the decision role asks for it with inspect_request; it runs beside the decision
// attempt on a worker of the reader role; its findings are checked against the read log and
// stored untrusted, and the decision role sees them without the note. A finding's target goes
// to a person on a control card, whose approval applies it as that person with the origin
// finding:<id> (MGR-25, MGR-26, MGR-28).
func TestManagerReading(t *testing.T) {
	ctx := context.Background()
	b := newManagerWorkBench(t)
	bob := b.withApprovers(t)
	w := b.enroll(t, EnrollRequest{Tenant: "demo", Kind: "agent", Role: "farm-manager"})
	reader := b.enroll(t, EnrollRequest{Tenant: "demo", Kind: "agent", Role: "farm-manager-reader"})
	b.diskAlarm(t, true, 3)
	b.advance(t, 6*60)
	b.loop(t, "incident")
	if _, err := b.c.SeatAssign(ctx, b.farmOp(), SeatRequest{Seat: managerSeatID}); err != nil {
		t.Fatal(err)
	}
	item := b.claim(t, w, "attempt")
	if item == nil {
		t.Fatal("no decision attempt")
	}
	handle := item.Payload["handles"].(map[string]any)["tools"].(string)
	incident := incidentsOf(t, b, "disk_full")[0].ID
	out := seatCall(t, b, handle, inspectToolName, map[string]any{"op": "request", "incident": incident})
	inspect, _ := out["inspect"].(string)
	if out["code"] != "enqueued" || inspect == "" {
		t.Fatalf("inspect_request %v", out)
	}
	agent := b.agent.ID
	// read claims the next reading and logs that it read the tool output about db1 and the hello
	// worker.
	read := func() *WorkItem {
		ritem := b.claim(t, reader, "attempt")
		if ritem == nil {
			t.Fatal("the reading did not run beside the decision attempt")
		}
		inspect = ritem.Payload["task"].(map[string]any)["id"].(string)
		err := b.c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
			var rec []byte
			if err := tx.QueryRow(ctx, `SELECT record FROM tasks WHERE id = $1`, inspect).Scan(&rec); err != nil {
				return err
			}
			if err := b.c.logSeatRead(ctx, tx, now, "demo", inspect, ritem.LeaseNo, rec, seatRead{Kind: "tool_arg", Object: "db1"}); err != nil {
				return err
			}
			return b.c.logSeatRead(ctx, tx, now, "demo", inspect, ritem.LeaseNo, rec, seatRead{Kind: "tool_arg", Object: agent})
		})
		if err != nil {
			t.Fatal(err)
		}
		return ritem
	}
	finding := func(evidence string) map[string]any {
		return map[string]any{"kind": "worker_misbehaving", "subject": map[string]any{"kind": "worker", "id": agentLower(agent)},
			"evidence": []any{evidence}, "values": map[string]any{"errors": 12}, "confidence": "high", "note": "It writes a lot."}
	}
	// Evidence outside the read log ends the attempt with output_schema_invalid (OUT-1).
	ritem := read()
	_, err := b.c.Complete(ctx, reader, header(t, ritem), CompleteRequest{Result: map[string]any{"findings": []any{finding("db9")}}})
	wantReason(t, err, "output_schema_invalid")
	if out := seatCall(t, b, handle, inspectToolName, map[string]any{"op": "request", "incident": incident}); out["code"] != "enqueued" {
		t.Fatalf("a second reading %v", out)
	}
	ritem = read()
	if _, err := b.c.Complete(ctx, reader, header(t, ritem), CompleteRequest{Result: map[string]any{"findings": []any{finding("db1")}}}); err != nil {
		t.Fatal(err)
	}
	out = seatCall(t, b, handle, inspectToolName, map[string]any{"op": "findings", "inspect": inspect})
	list, _ := out["findings"].([]any)
	if out["code"] != "ok" || len(list) != 1 {
		t.Fatalf("findings %v", out)
	}
	_, _, drec, _ := taskRecord(t, b, item.Payload["task"].(map[string]any)["id"].(string))
	if drec["untrusted_seen"] != true {
		t.Fatalf("the decision task that read findings is not in distrust mode: %v", drec)
	}
	f := asMap(list[0])
	if f["untrusted"] != true || f["note"] != nil || f["subject_in_code_targets"] != false {
		t.Fatalf("finding %v", f)
	}
	reads, err := b.c.ManagerReads(ctx, b.op, ManagerReadsRequest{})
	if err != nil || len(reads["items"].([]any)) != 4 {
		t.Fatalf("reads %v %v", reads, err)
	}
	// The finding's target is not involved in the incident: a control card, no hold.
	out = seatCall(t, b, handle, controlToolName, map[string]any{"op": "hold", "target": map[string]any{"kind": "worker", "id": agent},
		"basis": map[string]any{"incident": incident}, "finding": f["id"], "effects": []any{"no_claim"}, "ttl": "30m"})
	if out["code"] != "card" {
		t.Fatalf("a finding's target: %v", out)
	}
	var holds int
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM holds WHERE author_kind = 'seat_holder'`).Scan(&holds); err != nil || holds != 0 {
		t.Fatalf("holds %d %v", holds, err)
	}
	card := cardOf(t, b.drain(t), controlCard, "direct:1002")
	body := asMap(card.body["card"])
	if facts := asMap(asMap(body["context"])["finding_facts"]); facts["kind"] != "worker_misbehaving" || asMap(facts["values"])["errors"] != float64(12) || facts["note"] != nil {
		t.Fatalf("finding facts on the card %v", body["context"])
	}
	req := DecideRequest{SubjectKind: "control", ID: body["id"].(string), Nonce: body["nonce"].(string), Decision: "approve"}
	res, err := b.c.Decide(ctx, b.channel, press("telegram:1002", "direct:1002", card.external), req)
	if err != nil || res.State != "applied" {
		t.Fatalf("decide %+v %v", res, err)
	}
	var origin, actor, approved string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT origin, actor, approved_by FROM control_ops WHERE target_id = $1`, agent).Scan(&origin, &actor, &approved); err != nil {
		t.Fatal(err)
	}
	if origin != "finding:"+f["id"].(string) || actor != bob || approved != bob {
		t.Fatalf("control op %s %s %s", origin, actor, approved)
	}
	// The delivered card is updated to its final state, without buttons.
	up := cardOf(t, b.drain(t), cardUpdate, "direct:1002")
	if ub := asMap(up.body["card"]); up.replaces != card.external || ub["state"] != "applied" || len(ub["buttons"].([]any)) != 0 {
		t.Fatalf("card update %+v", up)
	}
	// A rejected card applies nothing and leaves a line in the incident's notice log.
	out = seatCall(t, b, handle, controlToolName, map[string]any{"op": "hold", "target": map[string]any{"kind": "worker", "id": agent},
		"basis": map[string]any{"incident": incident}, "finding": f["id"], "effects": []any{"no_claim"}, "ttl": "20m"})
	if out["code"] != "card" {
		t.Fatalf("a second card: %v", out)
	}
	card = cardOf(t, b.drain(t), controlCard, "direct:1002")
	body = asMap(card.body["card"])
	req = DecideRequest{SubjectKind: "control", ID: body["id"].(string), Nonce: body["nonce"].(string), Decision: "reject"}
	if res, err := b.c.Decide(ctx, b.channel, press("telegram:1002", "direct:1002", card.external), req); err != nil || res.State != "rejected" {
		t.Fatalf("reject %+v %v", res, err)
	}
	var logged bool
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT notice_log @> '[{"kind": "card", "op": "hold", "state": "rejected"}]' FROM incidents WHERE id = $1`, incident).Scan(&logged); err != nil || !logged {
		t.Fatalf("the rejected card is not in the notice log: %v", err)
	}
	// A revert of the incident ends its pending cards too.
	out = seatCall(t, b, handle, controlToolName, map[string]any{"op": "hold", "target": map[string]any{"kind": "worker", "id": agent},
		"basis": map[string]any{"incident": incident}, "finding": f["id"], "effects": []any{"no_claim"}, "ttl": "15m"})
	if out["code"] != "card" {
		t.Fatalf("a third card: %v", out)
	}
	if _, err := b.c.ControlRevert(ctx, b.op, ControlRevertRequest{Incident: incident}); err != nil {
		t.Fatal(err)
	}
	var pending int
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM control_cards WHERE state = 'pending'`).Scan(&pending); err != nil || pending != 0 {
		t.Fatalf("pending cards after a revert %d %v", pending, err)
	}
	if _, err := b.c.ManagerReads(ctx, b.op, ManagerReadsRequest{}); err != nil {
		t.Fatal(err)
	}
	var audited int
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM audit WHERE record->>'command' = 'manager.reads'`).Scan(&audited); err != nil || audited != 2 {
		t.Fatalf("reads of the read log in the audit %d %v", audited, err)
	}
}

// A release by a directive reaches only a hold whose operation was within the directive's
// scope; an incident scope is the incident as the basis (MGR-4).
func TestDirectiveScope(t *testing.T) {
	ctx := context.Background()
	b := newManagerWorkBench(t)
	if _, err := b.c.st.Pool.Exec(ctx, `INSERT INTO control_ops (id, tenant, op, target_kind, target_id, incident, actor_kind, actor, epoch, hold, created_at) VALUES
		('c1', 'demo', 'hold', 'resource', 'db1', NULL, 'seat', 'x', 1, 'h1', now()),
		('c2', 'demo', 'hold', 'resource', 'db2', 'inc_a', 'seat', 'x', 1, 'h2', now())`); err != nil {
		t.Fatal(err)
	}
	res := ControlTarget{Kind: "resource", ID: "db1"}
	inc := ControlTarget{Kind: "incident", ID: "inc_a"}
	cases := []struct {
		scope ControlTarget
		req   ControlRequest
		ok    bool
	}{
		{res, ControlRequest{Op: "release", Target: ControlTarget{Kind: "hold", ID: "h1"}}, true},
		{res, ControlRequest{Op: "release", Target: ControlTarget{Kind: "hold", ID: "h2"}}, false},
		{res, ControlRequest{Op: "release", Target: ControlTarget{Kind: "hold", ID: "h9"}}, false},
		{res, ControlRequest{Op: "hold", Target: res}, true},
		{inc, ControlRequest{Op: "release", Target: ControlTarget{Kind: "hold", ID: "h2"}, Basis: ControlBasis{Incident: "inc_a"}}, true},
		{inc, ControlRequest{Op: "release", Target: ControlTarget{Kind: "hold", ID: "h1"}, Basis: ControlBasis{Incident: "inc_a"}}, false},
		{inc, ControlRequest{Op: "hold", Target: res, Basis: ControlBasis{Incident: "inc_a"}}, true},
		{inc, ControlRequest{Op: "hold", Target: res}, false},
	}
	for i, k := range cases {
		err := b.c.tx(ctx, func(tx pgx.Tx, _ time.Time) error {
			p, err := directiveScopeHolds(ctx, tx, "demo", &directive{Scope: k.scope}, k.req)
			if err == nil && (p == nil) != k.ok {
				t.Errorf("case %d: %v", i, p)
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	// A directive names only operations this version serves, and a task operation only in an
	// incident.
	_, err := b.c.ManagerDirect(ctx, b.op, ManagerDirectRequest{Scope: res, Op: "notify"})
	wantReason(t, err, "input_schema_invalid")
	_, err = b.c.ManagerDirect(ctx, b.op, ManagerDirectRequest{Scope: res, Op: "cancel"})
	wantReason(t, err, "input_schema_invalid")
}

func agentLower(id string) string {
	b := []byte(id)
	for i, ch := range b {
		if ch >= 'A' && ch <= 'Z' {
			b[i] = ch + 'a' - 'A'
		}
	}
	return string(b)
}

// withApprovers adds bob (telegram:1002) to group directors.
func (b *bench) withApprovers(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	bob := ids.PrincipalID()
	if _, err := b.c.Principal(ctx, b.op, PrincipalRequest{Op: "principal.add", Subject: bob, Identity: "telegram:1002"}); err != nil {
		t.Fatal(err)
	}
	b.direct(t, bob)
	return bob
}

// direct makes a person a member of group directors.
func (b *bench) direct(t *testing.T, person string) {
	t.Helper()
	if _, err := b.c.st.Pool.Exec(context.Background(), `INSERT INTO memberships (tenant, principal, grp, state, created_at) VALUES ('demo', $1, 'directors', 'active', now())`, person); err != nil {
		t.Fatal(err)
	}
}

// Without the purpose incident_review the core enqueues no reading, and apply cancels the
// running ones (MGR-20).
func TestManagerReadingPurposeOff(t *testing.T) {
	ctx := context.Background()
	off := func(ms []map[string]any) {
		for _, m := range ms {
			if m["kind"] != "Tenant" {
				continue
			}
			priv := asMap(asMap(m["spec"])["privacy"])
			var keep []any
			for _, p := range priv["purposes"].([]any) {
				if asMap(p)["purpose"] != "incident_review" {
					keep = append(keep, p)
				}
			}
			priv["purposes"] = keep
		}
	}
	b := newManagerBenchWith(t, managerWorkFiles, []map[string]any{directors}, workEdit, off)
	w := b.enroll(t, EnrollRequest{Tenant: "demo", Kind: "agent", Role: "farm-manager"})
	b.diskAlarm(t, true, 3)
	b.advance(t, 6*60)
	b.loop(t, "incident")
	if _, err := b.c.SeatAssign(ctx, b.farmOp(), SeatRequest{Seat: managerSeatID}); err != nil {
		t.Fatal(err)
	}
	item := b.claim(t, w, "attempt")
	handle := item.Payload["handles"].(map[string]any)["tools"].(string)
	out := seatCall(t, b, handle, inspectToolName, map[string]any{"op": "request", "incident": incidentsOf(t, b, "disk_full")[0].ID})
	if out["code"] != "purpose_off" {
		t.Fatalf("a reading without the purpose: %v", out)
	}
	doc, err := b.c.Doctor(ctx, b.op, TenantRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for _, ch := range doc.Checks {
		if ch.ID == "manager.reader_workers" {
			t.Fatalf("a reader warning without the purpose: %+v", ch)
		}
	}
}

// The seat's memory is a section in each tenant: when the seat passes, the old holder's facts of
// a tenant go to the new holder by an offer of that tenant, and the old ones are sealed (MGR-27).
func TestManagerSeatFacts(t *testing.T) {
	ctx := context.Background()
	b := newManagerBench(t)
	now := b.now(t)
	for i := 1; i <= 10; i++ {
		at := now.Add(-time.Duration(i) * time.Hour)
		if _, err := b.c.st.Pool.Exec(ctx, `INSERT INTO obs_points (tenant, series, subject, at, num, enum, source, data_mark, key, created_at) VALUES
			('demo', 'farm.attempt.duration', 'task_type:'||$3, $1, $2, NULL, 'core', 'internal', $4, $1)`, at, float64(60*i), incidentRespondType, fmt.Sprintf("d%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := b.c.SeatAssign(ctx, b.farmOp(), SeatRequest{Seat: managerSeatID}); err != nil {
		t.Fatal(err)
	}
	old := b.managerSeat(t).Holder
	s := factSubject{Type: "tasktype", ID: incidentRespondType}
	err := b.c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		from := now.Add(-24 * time.Hour)
		v, ok, err := computeFact(ctx, tx, "demo", "task.duration.p95_min", s, from, now)
		if err != nil || !ok {
			t.Fatalf("value %v %v", ok, err)
		}
		_, err = b.c.insertFact(ctx, tx, now, "demo", old, "how_long", "task.duration.p95_min", s, v, from, now, false, false, actingOf{}, nil)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.c.SeatAssign(ctx, b.farmOp(), SeatRequest{Seat: managerSeatID}); err != nil {
		t.Fatal(err)
	}
	holder := b.managerSeat(t).Holder
	if holder == old {
		t.Fatal("the seat did not pass")
	}
	var tenant, state string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT tenant, recipient_state FROM fact_offers WHERE from_bot = $1 AND recipient = $2`, old, holder).Scan(&tenant, &state); err != nil {
		t.Fatal(err)
	}
	if tenant != "demo" || state != "copied" {
		t.Fatalf("offer %s %s", tenant, state)
	}
	var n int
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM facts WHERE tenant = 'demo' AND bot = $1 AND state = 'active'`, holder).Scan(&n); err != nil || n != 1 {
		t.Fatalf("copied %d %v", n, err)
	}
	if n := factCount(t, b, old, "sealed"); n != 1 {
		t.Fatalf("sealed %d", n)
	}
	// A tenant that turns the manager off keeps no seat memory.
	cat, err := b.c.Catalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	off := *cat
	off.Tenants = maps.Clone(cat.Tenants)
	tv := *cat.Tenants["demo"]
	tv.Raw = maps.Clone(tv.Raw)
	spec := maps.Clone(asMap(tv.Raw["spec"]))
	spec["farm_manager"] = map[string]any{"enabled": false}
	tv.Raw["spec"] = spec
	off.Tenants["demo"] = &tv
	if err := b.c.tx(ctx, func(tx pgx.Tx, now time.Time) error { return b.c.managerDisabled(ctx, tx, now, &off) }); err != nil {
		t.Fatal(err)
	}
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM facts WHERE tenant = 'demo' AND bot IN ($1, $2)`, old, holder).Scan(&n); err != nil || n != 0 {
		t.Fatalf("seat facts after the opt-out %d %v", n, err)
	}
}

// From v0.3 work outside sev1 stops at 70% of the manager line's month; sev1 and the fallback
// profile run on (MGR-49).
func TestManagerMonthLineV03(t *testing.T) {
	ctx := context.Background()
	b := newManagerBench(t)
	day, month := 100.0, 10.0
	tv := &TenantView{Name: "demo"}
	tv.Spec.Budgets.Lines = map[string]manifest.LineBudget{"manager": {DayEUR: &day, MonthEUR: &month}}
	if _, err := b.c.st.Pool.Exec(ctx, `INSERT INTO spend (id, tenant, line, unit, state, eur_micros, time)
		VALUES ('spd_mgr', 'demo', 'manager', 'tokens', 'committed', 7500000, botaru_now())`); err != nil {
		t.Fatal(err)
	}
	check := func(ask budgetAsk) *Problem {
		var p *Problem
		err := b.c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
			p = b.c.checkBudget(ctx, tx, now, tv, ask)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	if p := check(budgetAsk{subject: "system", line: "manager", extra: 1000}); p == nil || p.Reason != "budget_exhausted" {
		t.Fatalf("outside sev1 over 70%% of the month: %v", p)
	}
	if p := check(budgetAsk{subject: "system", line: "manager", extra: 1000, sev1: true}); p != nil {
		t.Fatalf("sev1: %v", p)
	}
	if p := check(budgetAsk{subject: "system", line: "manager", extra: 1000, fallback: true}); p != nil {
		t.Fatalf("fallback: %v", p)
	}
	// Once the month is spent, sev1 runs only from the day's 30% reserve above today's other
	// work: 10 + 30 EUR.
	if _, err := b.c.st.Pool.Exec(ctx, `INSERT INTO spend (id, tenant, line, unit, state, eur_micros, time)
		VALUES ('spd_mgr2', 'demo', 'manager', 'tokens', 'committed', 2500000, botaru_now())`); err != nil {
		t.Fatal(err)
	}
	if p := check(budgetAsk{subject: "system", line: "manager", extra: money.FromEUR(29), sev1: true}); p != nil {
		t.Fatalf("sev1 within the reserve: %v", p)
	}
	if p := check(budgetAsk{subject: "system", line: "manager", extra: money.FromEUR(31), sev1: true}); p == nil {
		t.Fatal("sev1 past the reserve with the month spent")
	}
	err := b.c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		spent, err := managerMonthSpent(ctx, tx, now, tv)
		if err != nil || !spent {
			t.Fatalf("month spent %v %v", spent, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// The tenant lint of the manager line from v0.3: the day covers the review, the analyses and
// three reads, the month covers the takeover ceiling (MGR-49).
func TestManagerLineShortV03(t *testing.T) {
	profile := map[string]any{"price": map[string]any{"input": 1.0, "output": 1.0}, "max_output_tokens": 1000.0}
	fm := map[string]any{"daily_review_max_eur": 0.5}
	ts := func(day, month float64) map[string]any {
		return map[string]any{"budgets": map[string]any{"lines": map[string]any{"manager": map[string]any{"day_eur": day, "month_eur": month}}}}
	}
	if got := managerLineShort(ts(0.5, 5), fm, profile); !slices.Contains(got, "/budgets/lines/manager/day_eur") || !slices.Contains(got, "/budgets/lines/manager/month_eur") {
		t.Fatalf("v0.3 short %v", got)
	}
	if got := managerLineShort(ts(5, 20), fm, profile); len(got) != 0 {
		t.Fatalf("v0.3 enough %v", got)
	}
}
