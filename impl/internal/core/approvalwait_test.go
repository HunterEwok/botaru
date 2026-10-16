// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// A single approval of an exclusive action on a machine with a released write call of a
// campaign item waits approved with resource_busy instead of failing, and runs through its one
// write call once the machine is free; then a campaign item on the machine waits for the
// approval's call, as single approvals count too (ORC-12).
func TestApprovalWaitsBusyResource(t *testing.T) {
	ctx := context.Background()
	b := newCampaignActionBench(t)
	tmpl := map[string]any{"kind": "action", "action": "fix_web", "args": map[string]any{}}
	att, h := b.attemptTools(t, "1", false)
	first := proposeCall(t, b.bench, h, map[string]any{"template": tmpl, "items": map[string]any{"resources": []any{"web1"}}})
	b.finish(t, att)
	att, h = b.attemptTools(t, "2", false)
	req := b.request(t, h, map[string]any{"action": "fix_web", "args": map[string]any{"host": "web1"}})
	if first.State != "proposed" || req.Code != "pending" {
		t.Fatalf("proposal %+v request %+v", first, req)
	}
	b.finish(t, att)
	// The approval's cards go out once its attempt ended, by the approval loop.
	b.tick(t, "approval-expiry")
	s := b.drain(t)
	cmp := b.campaign(t, first.Campaign)
	var ccard sent
	for _, x := range s {
		if x.kind == campaignApprovalCard && x.body["card"].(map[string]any)["id"] == cmp.ID {
			ccard = x
		}
	}
	bob := func(external string) *OnBehalf { return press("telegram:1002", "direct:1002", external) }
	if _, err := b.c.Decide(ctx, b.channel, bob(ccard.external), DecideRequest{SubjectKind: "campaign", ID: cmp.ID, Nonce: cmp.Nonce, Decision: "approve"}); err != nil {
		t.Fatal(err)
	}
	if st := b.itemStates(t, cmp.ID); st != "released" {
		t.Fatalf("campaign item %s", st)
	}
	a := b.approval(t, req.ApprovalID)
	card := cardOf(t, s, approvalCard, "direct:1002")
	res, err := b.c.Decide(ctx, b.channel, bob(card.external), DecideRequest{SubjectKind: "approval", ID: a.ID, Nonce: a.Nonce, Decision: "approve"})
	if err != nil || res.State != "approved" {
		t.Fatalf("decide %+v %v", res, err)
	}
	calls := func() int {
		t.Helper()
		var n int
		if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM toolcalls WHERE approval_id = $1`, a.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if a = b.approval(t, a.ID); a.State != "approved" || a.Reason != "resource_busy" || calls() != 0 {
		t.Fatalf("the approval did not wait: %s %s %d", a.State, a.Reason, calls())
	}
	// A repeat press gets the same answer; the loop keeps it waiting while web1 is busy.
	if res, err := b.c.Decide(ctx, b.channel, bob(card.external), DecideRequest{SubjectKind: "approval", ID: a.ID, Nonce: a.Nonce, Decision: "approve"}); err != nil || res.State != "approved" {
		t.Fatalf("repeat %+v %v", res, err)
	}
	b.tick(t, "approval-expiry")
	if calls() != 0 {
		t.Fatal("the approval ran on a busy machine")
	}
	item := b.claimTool(t, b.th)
	if item.Payload["approval_id"] != cmp.ID {
		t.Fatalf("the toolhost got %v", item.Payload["approval_id"])
	}
	if _, err := b.c.Complete(ctx, b.th, header(t, item), CompleteRequest{Output: map[string]any{"content": []any{}, "structuredContent": map[string]any{}}}); err != nil {
		t.Fatal(err)
	}
	b.tick(t, "approval-expiry")
	if a = b.approval(t, a.ID); a.State != "executing" || a.Reason != "" || calls() != 1 {
		t.Fatalf("after the machine was free: %s %s %d", a.State, a.Reason, calls())
	}
	// Now a campaign item on web1 waits for the approval's write call.
	att, h = b.attemptTools(t, "3", false)
	second := proposeCall(t, b.bench, h, map[string]any{"template": tmpl, "items": map[string]any{"resources": []any{"web1"}}})
	b.finish(t, att)
	s = b.drain(t)
	cmp2 := b.campaign(t, second.Campaign)
	for _, x := range s {
		if x.kind == campaignApprovalCard && x.body["card"].(map[string]any)["id"] == cmp2.ID {
			ccard = x
		}
	}
	if _, err := b.c.Decide(ctx, b.channel, bob(ccard.external), DecideRequest{SubjectKind: "campaign", ID: cmp2.ID, Nonce: cmp2.Nonce, Decision: "approve"}); err != nil {
		t.Fatal(err)
	}
	var wait string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT state || ':' || coalesce(wait, '') FROM campaign_items WHERE campaign = $1 AND n = 1`, cmp2.ID).Scan(&wait); err != nil {
		t.Fatal(err)
	}
	if wait != "pending:resource_busy" {
		t.Fatalf("the campaign item did not wait for the single approval: %s", wait)
	}
}

// waitingApproval brings a single fix_web approval on web1 to the wait behind a released
// campaign item; free completes the item's write call, so the machine is free.
func waitingApproval(t *testing.T) (b *actionBench, id string, calls func() int, free func()) {
	t.Helper()
	ctx := context.Background()
	b = newCampaignActionBench(t)
	tmpl := map[string]any{"kind": "action", "action": "fix_web", "args": map[string]any{}}
	att, h := b.attemptTools(t, "1", false)
	first := proposeCall(t, b.bench, h, map[string]any{"template": tmpl, "items": map[string]any{"resources": []any{"web1"}}})
	b.finish(t, att)
	att, h = b.attemptTools(t, "2", false)
	req := b.request(t, h, map[string]any{"action": "fix_web", "args": map[string]any{"host": "web1"}})
	b.finish(t, att)
	b.tick(t, "approval-expiry")
	s := b.drain(t)
	cmp := b.campaign(t, first.Campaign)
	bob := func(external string) *OnBehalf { return press("telegram:1002", "direct:1002", external) }
	for _, x := range s {
		if x.kind == campaignApprovalCard && x.body["card"].(map[string]any)["id"] == cmp.ID {
			if _, err := b.c.Decide(ctx, b.channel, bob(x.external), DecideRequest{SubjectKind: "campaign", ID: cmp.ID, Nonce: cmp.Nonce, Decision: "approve"}); err != nil {
				t.Fatal(err)
			}
		}
	}
	a := b.approval(t, req.ApprovalID)
	card := cardOf(t, s, approvalCard, "direct:1002")
	if _, err := b.c.Decide(ctx, b.channel, bob(card.external), DecideRequest{SubjectKind: "approval", ID: a.ID, Nonce: a.Nonce, Decision: "approve"}); err != nil {
		t.Fatal(err)
	}
	if a = b.approval(t, a.ID); a.State != "approved" || a.Reason != "resource_busy" {
		t.Fatalf("not waiting: %s %s", a.State, a.Reason)
	}
	calls = func() int {
		t.Helper()
		var n int
		if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM toolcalls WHERE approval_id = $1`, a.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	free = func() {
		t.Helper()
		item := b.claimTool(t, b.th)
		if _, err := b.c.Complete(ctx, b.th, header(t, item), CompleteRequest{Output: map[string]any{"content": []any{}, "structuredContent": map[string]any{}}}); err != nil {
			t.Fatal(err)
		}
	}
	return b, a.ID, calls, free
}

// The holds of an approval stand while it waits for its resource (ORC-12, HLD-3, HLD-8):
// pause_action keeps it waiting after the machine is free, its release lets it run; no_action
// cancels it with held. The same request again is the waiting approval, not a second one (APR-2).
func TestWaitingApprovalHolds(t *testing.T) {
	ctx := context.Background()
	b, id, calls, free := waitingApproval(t)
	w := b.approval(t, id)
	if err := b.c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		cat, err := b.c.Catalog(ctx)
		if err != nil {
			return err
		}
		got, code, err := b.c.openApproval(ctx, tx, now, cat.Tenants["demo"], approvalRow{Tenant: "demo", SourceTask: w.SourceTask, Initiator: w.Initiator,
			Action: w.Action, ArgsHash: w.ArgsHash, Args: w.Args, Risk: w.Risk, RequestedBy: w.RequestedBy, ExpiresAt: now.Add(time.Hour)})
		if err == nil && (got != id || code != "pending") {
			t.Errorf("the same request again: %s %s, want the waiting %s", got, code, id)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	hold, err := b.c.HoldSet(ctx, OperatorActor(b.op), HoldSetRequest{Scope: HoldScope{Kind: "action", Tenant: "demo", Name: "fix_web"}, Effects: []string{"pause_action"}, Reason: "manual"})
	if err != nil {
		t.Fatal(err)
	}
	free()
	b.tick(t, "approval-expiry")
	if a := b.approval(t, id); a.State != "approved" || calls() != 0 {
		t.Fatalf("ran under pause_action: %s %d", a.State, calls())
	}
	if _, err := b.c.HoldRelease(ctx, OperatorActor(b.op), HoldReleaseRequest{ID: hold.ID}); err != nil {
		t.Fatal(err)
	}
	b.tick(t, "approval-expiry")
	if a := b.approval(t, id); a.State != "executing" || calls() != 1 {
		t.Fatalf("after the release: %s %d", a.State, calls())
	}

	b, id, calls, free = waitingApproval(t)
	if _, err := b.c.HoldSet(ctx, OperatorActor(b.op), HoldSetRequest{Scope: HoldScope{Kind: "role", Tenant: "demo", Name: "hello"}, Effects: []string{"no_action"}, Reason: "manual"}); err != nil {
		t.Fatal(err)
	}
	free()
	b.tick(t, "approval-expiry")
	if a := b.approval(t, id); a.State != "canceled" || a.Reason != "held" || calls() != 0 {
		t.Fatalf("under no_action: %s %s %d", a.State, a.Reason, calls())
	}
}
