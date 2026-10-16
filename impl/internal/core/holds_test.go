// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"testing"
	"time"

	"github.com/HunterEwok/botaru/impl/internal/ids"
	"github.com/HunterEwok/botaru/impl/internal/spec"
	"github.com/HunterEwok/botaru/impl/internal/testbench"
)

// withAdmins applies the hello-bot set with a group admins that holds admin:people,
// hold.release and hold.set:plugin, and makes bob a member with telegram:1002. A privileged
// group comes only through farmctl with a verification code (PRM-13), which this build lacks,
// so the test writes the membership into the store.
func (b *bench) withAdmins(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	root, err := spec.FindRoot("")
	if err != nil {
		t.Fatal(err)
	}
	ms, err := testbench.HelloBotManifests(root)
	if err != nil {
		t.Fatal(err)
	}
	ms = append(ms, map[string]any{
		"apiVersion": "botaru/v1alpha1", "kind": "Group",
		"metadata": map[string]any{"name": "admins", "tenant": "demo"},
		"spec":     map[string]any{"caps": []any{"admin:people", "hold.release", "hold.set:plugin"}, "data": []any{"summary"}},
	})
	res, err := b.c.Apply(ctx, b.op, ApplyRequest{Commit: "1111111111111111111111111111111111111111", Manifests: ms})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Applied {
		t.Fatalf("apply: %+v", res.Errors)
	}
	bob := ids.PrincipalID()
	if _, err := b.c.Principal(ctx, b.op, PrincipalRequest{Op: "principal.add", Subject: bob, Identity: "telegram:1002"}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.c.st.Pool.Exec(ctx, `INSERT INTO memberships (tenant, principal, grp, state, created_at) VALUES ('demo', $1, 'admins', 'active', now())`, bob); err != nil {
		t.Fatal(err)
	}
	return bob
}

func countEvents(t *testing.T, b *bench, typ, subject string) int {
	t.Helper()
	var n int
	if err := b.c.st.Pool.QueryRow(context.Background(), `SELECT count(*) FROM events WHERE type = $1 AND ($2 = '' OR subject = $2)`, typ, subject).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func wantReason(t *testing.T, err error, reason string) {
	t.Helper()
	p, ok := AsProblem(err)
	if !ok || p.Reason != reason {
		t.Fatalf("got %v, want %s", err, reason)
	}
}

func (b *bench) activity(t *testing.T, update string) *ActivityResult {
	t.Helper()
	a := Activity{Kind: "message", Channel: "telegram", UpdateID: update, Chat: Chat{Kind: "direct", ExternalID: "1001"}, Text: "/hello hi"}
	a.Sender.Identity, a.Sender.Verified = "telegram:1001", true
	res, err := b.c.SubmitActivity(context.Background(), b.channel, a)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// A hold with no_enqueue rejects a new task with held before the permission formula; its
// release gives an event and an audit record, and the next task passes (HLD-1, HLD-3).
func TestHoldNoEnqueue(t *testing.T) {
	ctx := context.Background()
	b := newBench(t)
	set, err := b.c.HoldSet(ctx, OperatorActor(b.op), HoldSetRequest{
		Scope: HoldScope{Kind: "tasktype", Tenant: "demo", Name: "hello.greet.v1"}, Effects: []string{"no_enqueue"}, Reason: "manual"})
	if err != nil {
		t.Fatal(err)
	}
	if set.ID == "" || set.AuditSeq == 0 || countEvents(t, b, "hold.set.v1", set.ID) != 1 {
		t.Fatalf("hold.set %+v", set)
	}
	if res := b.activity(t, "1"); res.Result != "reply" || res.Text != b.c.texts.Render("en", "task.rejected.held", nil) {
		t.Fatalf("held enqueue %+v", res)
	}
	list, err := b.c.HoldList(ctx, OperatorActor(b.op), TenantRequest{})
	if err != nil || len(list.Items) != 1 || list.Items[0].Scope.Name != "hello.greet.v1" || list.Items[0].Until != nil || list.Items[0].AuthorKind != "operator" {
		t.Fatalf("hold.list %+v %v", list, err)
	}
	rel, err := b.c.HoldRelease(ctx, OperatorActor(b.op), HoldReleaseRequest{ID: set.ID})
	if err != nil || rel.State != "released" || countEvents(t, b, "hold.released.v1", set.ID) != 1 {
		t.Fatalf("hold.release %+v %v", rel, err)
	}
	again, err := b.c.HoldRelease(ctx, OperatorActor(b.op), HoldReleaseRequest{ID: set.ID})
	if err != nil || again.State != "released" || countEvents(t, b, "hold.released.v1", set.ID) != 1 {
		t.Fatalf("repeated release %+v %v", again, err)
	}
	if res := b.activity(t, "2"); res.Result != "task" {
		t.Fatalf("after release %+v", res)
	}
	if n, broken, err := b.c.VerifyAudit(ctx, "demo"); err != nil || broken != 0 || n == 0 {
		t.Fatalf("audit %d %d %v", n, broken, err)
	}
}

// A hold with cancel_running on a type ends the running attempt at its heartbeat without
// spending it, and claim does not hand the task out while the hold acts; the hold ends by its
// until through the hold loop, never earlier (HLD-1, HLD-3, FEN-4).
func TestHoldCancelRunning(t *testing.T) {
	ctx := context.Background()
	b := newBench(t)
	res := b.activity(t, "1")
	b.c.st.Pool.Exec(ctx, `UPDATE contacts SET ai_notice_at = now()`)
	att := b.claim(t, b.agent, "attempt")
	if att == nil {
		t.Fatal("no attempt")
	}
	now, err := b.c.TestClockAdvance(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	at, err := time.Parse(time.RFC3339Nano, now.Now)
	if err != nil {
		t.Fatal(err)
	}
	until := at.Add(10 * time.Minute).Format(time.RFC3339Nano)
	set, err := b.c.HoldSet(ctx, OperatorActor(b.op), HoldSetRequest{
		Scope: HoldScope{Kind: "tasktype", Tenant: "demo", Name: "hello.greet.v1"}, Effects: []string{"cancel_running"}, Reason: "manual", Until: &until})
	if err != nil {
		t.Fatal(err)
	}
	hb, err := b.c.Heartbeat(ctx, b.agent, header(t, att), HeartbeatRequest{})
	if err != nil || !hb.CancelRequested {
		t.Fatalf("heartbeat %+v %v", hb, err)
	}
	var state, reason string
	var spent int
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT state, coalesce(reason, ''), attempts_spent FROM tasks WHERE id = $1`, res.Task).Scan(&state, &reason, &spent); err != nil {
		t.Fatal(err)
	}
	if state != "submitted" || reason != "held" || spent != 0 {
		t.Fatalf("task %s %s %d", state, reason, spent)
	}
	var handles int
	b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM handles WHERE work_id = $1`, res.Task).Scan(&handles)
	if handles != 0 {
		t.Fatalf("%d handles left", handles)
	}
	if _, err := b.c.Complete(ctx, b.agent, header(t, att), CompleteRequest{Result: map[string]any{"answer": "late"}}); err == nil {
		t.Fatal("a complete under a revoked lease passed")
	}
	if w := b.claim(t, b.agent, "attempt"); w != nil {
		t.Fatalf("claim under the hold gave %s", w.WorkID)
	}
	if err := b.c.expireHolds(ctx); err != nil {
		t.Fatal(err)
	}
	if countEvents(t, b, "hold.expired.v1", set.ID) != 0 {
		t.Fatal("the hold loop ended a hold before its until")
	}
	if _, err := b.c.TestClockAdvance(ctx, 601); err != nil {
		t.Fatal(err)
	}
	if w := b.claim(t, b.agent, "attempt"); w == nil || w.WorkID != res.Task || w.LeaseNo != 2 {
		t.Fatalf("after until: %+v", w)
	}
	if err := b.c.expireHolds(ctx); err != nil {
		t.Fatal(err)
	}
	if countEvents(t, b, "hold.expired.v1", set.ID) != 1 {
		t.Fatal("no hold.expired")
	}
}

// A worker quarantine gives that worker no work (HLD-3).
func TestHoldWorkerQuarantine(t *testing.T) {
	ctx := context.Background()
	b := newBench(t)
	b.activity(t, "1")
	b.c.st.Pool.Exec(ctx, `UPDATE contacts SET ai_notice_at = now()`)
	if _, err := b.c.HoldSet(ctx, OperatorActor(b.op), HoldSetRequest{
		Scope: HoldScope{Kind: "worker", Tenant: "demo", Name: b.agent.ID}, Effects: []string{"no_claim"}, Reason: "compromised"}); err != nil {
		t.Fatal(err)
	}
	if w := b.claim(t, b.agent, "attempt"); w != nil {
		t.Fatal("a quarantined worker got work")
	}
	other := b.enroll(t, EnrollRequest{Tenant: "demo", Role: "hello", Kind: "agent"})
	if w := b.claim(t, other, "attempt"); w == nil {
		t.Fatal("another worker of the role got no work")
	}
}

// A hold with no_delivery on a channel withholds its deliveries (HLD-3).
func TestHoldNoDelivery(t *testing.T) {
	ctx := context.Background()
	b := newBench(t)
	b.activity(t, "1")
	if _, err := b.c.HoldSet(ctx, OperatorActor(b.op), HoldSetRequest{
		Scope: HoldScope{Kind: "channel", Tenant: "demo", Name: "telegram"}, Effects: []string{"no_delivery"}, Reason: "manual"}); err != nil {
		t.Fatal(err)
	}
	if w := b.claim(t, b.channel, "delivery"); w != nil {
		t.Fatalf("delivery under the hold: %v", w.Payload)
	}
	var n int
	b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM deliveries WHERE state = 'withheld' AND reason = 'held'`).Scan(&n)
	if n != 2 {
		t.Fatalf("%d withheld deliveries, want ai_notice and privacy_notice", n)
	}
}

// Who sets and releases a hold (HLD-2, PRM-14, TEN-2).
func TestHoldWhoSetsAndReleases(t *testing.T) {
	ctx := context.Background()
	b := newBench(t)
	bob := b.withAdmins(t)
	op := OperatorActor(b.op)
	farmOp := OperatorActor(Operator{Principal: b.op.Principal, Tenant: "farm"})
	farm := HoldSetRequest{Scope: HoldScope{Kind: "farm"}, Effects: []string{"no_claim"}, Reason: "manual"}
	if _, err := b.c.HoldSet(ctx, op, farm); err == nil {
		t.Fatal("the operator credential of demo set a farm hold")
	}
	f, err := b.c.HoldSet(ctx, farmOp, farm)
	if err != nil {
		t.Fatal(err)
	}
	var tenant string
	b.c.st.Pool.QueryRow(ctx, `SELECT tenant FROM holds WHERE id = $1`, f.ID).Scan(&tenant)
	if tenant != "farm" {
		t.Fatalf("a farm hold is a record of %q", tenant)
	}
	other := HoldSetRequest{Scope: HoldScope{Kind: "tenant", Tenant: "demo2"}, Effects: []string{"no_enqueue"}, Reason: "manual"}
	_, err = b.c.HoldSet(ctx, op, other)
	wantReason(t, err, "tenant_mismatch")
	later := HoldSetRequest{Scope: HoldScope{Kind: "sink", Tenant: "demo", Name: "exports"}, Effects: []string{"no_enqueue"}, Reason: "manual"}
	_, err = b.c.HoldSet(ctx, op, later)
	wantReason(t, err, "input_schema_invalid")
	_, err = b.c.HoldSet(ctx, HoldActor{Principal: "bot:ops-reader", Rank: "bot", Tenant: "demo"}, farm)
	wantReason(t, err, "forbidden")

	person := HoldActor{Principal: bob, Rank: "tenant_admin", Tenant: "demo", Via: "channel"}
	_, err = b.c.HoldSet(ctx, person, farm)
	wantReason(t, err, "operator_only")
	plugin := HoldSetRequest{Scope: HoldScope{Kind: "plugin", Tenant: "demo", Name: "logs"}, Effects: []string{"no_tool"}, Reason: "manual"}
	if _, err := b.c.HoldSet(ctx, person, plugin); err != nil {
		t.Fatalf("a person with hold.set:plugin: %v", err)
	}
	_, err = b.c.HoldSet(ctx, person, HoldSetRequest{Scope: HoldScope{Kind: "tenant", Tenant: "demo"}, Effects: []string{"no_enqueue"}, Reason: "manual"})
	wantReason(t, err, "no_group_cap")
	alice := HoldActor{Principal: b.alice, Rank: "tenant_admin", Tenant: "demo", Via: "channel"}

	byOperator, err := b.c.HoldSet(ctx, op, HoldSetRequest{Scope: HoldScope{Kind: "tenant", Tenant: "demo"}, Effects: []string{"no_claim"}, Reason: "suspended"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = b.c.HoldRelease(ctx, person, HoldReleaseRequest{ID: byOperator.ID})
	wantReason(t, err, "forbidden")
	auto, err := b.c.HoldSet(ctx, HoldActor{Principal: "system", Rank: "contain_auto", Tenant: "demo", Via: "system", Source: "runaway_spend_hour"},
		HoldSetRequest{Scope: HoldScope{Kind: "role", Tenant: "demo", Name: "hello"}, Effects: []string{"no_claim"}, Reason: "runaway_spend"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = b.c.HoldRelease(ctx, alice, HoldReleaseRequest{ID: auto.ID})
	wantReason(t, err, "no_group_cap")
	if r, err := b.c.HoldRelease(ctx, person, HoldReleaseRequest{ID: auto.ID}); err != nil || r.State != "released" {
		t.Fatalf("a tenant admin releases a contain_auto hold: %+v %v", r, err)
	}
	_, err = b.c.HoldRelease(ctx, person, HoldReleaseRequest{ID: f.ID})
	wantReason(t, err, "operator_only")
	_, err = b.c.HoldRelease(ctx, OperatorActor(Operator{Principal: b.op.Principal, Tenant: "demo2"}), HoldReleaseRequest{ID: byOperator.ID})
	wantReason(t, err, "not_found")
	if r, err := b.c.HoldRelease(ctx, farmOp, HoldReleaseRequest{ID: f.ID}); err != nil || r.State != "released" {
		t.Fatalf("the farm operator releases a farm hold: %+v %v", r, err)
	}
}
