// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"testing"
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
