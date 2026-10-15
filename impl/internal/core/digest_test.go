// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"testing"
)

// presentEdit sends the digest every minute to the direct chats of the deciders.
func presentEdit(ms []map[string]any) {
	ins := specOf(ms, "Tenant", "demo")["insights"].(map[string]any)
	ins["present_to"] = "group:deciders"
	ins["digest"] = map[string]any{"schedule": "* * * * *", "max_items": 5}
}

// reviewAll hands in every pending lens review with score.
func (b *bench) reviewAll(t *testing.T, score int) {
	t.Helper()
	ra := b.enroll(t, EnrollRequest{Tenant: "demo", Role: "reviewer-a", Kind: "agent"})
	rb := b.enroll(t, EnrollRequest{Tenant: "demo", Role: "reviewer-b", Kind: "agent"})
	_, rec := proposalState(t, b)
	for _, r := range rec["reviews"].([]any) {
		rm := r.(map[string]any)
		if rm["state"] != "pending" {
			continue
		}
		w := ra
		if l := rm["lens"]; l == "cost" || l == "value" {
			w = rb
		}
		if err := b.review(t, w, map[string]any{"lens": rm["lens"], "score": score, "confidence": "high"}); err != nil {
			t.Fatal(err)
		}
	}
}

func (b *bench) proposalNonce(t *testing.T) (string, string) {
	t.Helper()
	var id string
	var nonce *string
	if err := b.c.st.Pool.QueryRow(context.Background(), `SELECT id, nonce FROM proposals`).Scan(&id, &nonce); err != nil {
		t.Fatal(err)
	}
	return id, deref(nonce)
}

// A recommended proposal goes into the digest and its card to the person who may decide;
// "Postpone" returns it to the next digest with a new card, and "Accept" of kind none notes it
// (DIG-1, DIG-2, DIG-3, PRP-6).
func TestProposalDigestDecide(t *testing.T) {
	ctx := context.Background()
	b := newCommissionBench(t, true, presentEdit)
	proposeNone(t, b)
	b.reviewAll(t, 3)
	b.loop(t, "proposal")
	if state, rec := proposalState(t, b); state != "presented" {
		t.Fatalf("state %s %v", state, rec["state_reason"])
	}
	sent := b.drain(t)
	cardOf(t, sent, proposalDigest, "direct:1001")
	card := cardOf(t, sent, proposalCard, "direct:1001")
	cd := card.body["card"].(map[string]any)
	validOut(t, b, "card", cd)
	if n := len(cd["buttons"].([]any)); n != 4 {
		t.Fatalf("buttons %v", cd["buttons"])
	}
	id, nonce := b.proposalNonce(t)
	if _, err := b.c.Decide(ctx, b.channel, press("telegram:1001", "direct:1001", card.external), DecideRequest{SubjectKind: "proposal", ID: id, Nonce: "x", Decision: "accept"}); err == nil {
		t.Fatal("a wrong nonce decided")
	}
	r, err := b.c.Decide(ctx, b.channel, press("telegram:1001", "direct:1001", card.external), DecideRequest{SubjectKind: "proposal", ID: id, Nonce: nonce, Decision: "defer"})
	if err != nil || r.State != "recommended" {
		t.Fatalf("defer %+v %v", r, err)
	}
	cardOf(t, b.drain(t), cardUpdate, "direct:1001")
	// The next slot presents it again with a new card.
	b.advance(t, 61)
	b.loop(t, "proposal")
	if state, _ := proposalState(t, b); state != "presented" {
		t.Fatalf("state %s", state)
	}
	card = cardOf(t, b.drain(t), proposalCard, "direct:1001")
	_, nonce2 := b.proposalNonce(t)
	if nonce2 == nonce || nonce2 == "" {
		t.Fatal("the new card has the old nonce")
	}
	r, err = b.c.Decide(ctx, b.channel, press("telegram:1001", "direct:1001", card.external), DecideRequest{SubjectKind: "proposal", ID: id, Nonce: nonce2, Decision: "accept"})
	if err != nil || r.State != "noted" {
		t.Fatalf("accept %+v %v", r, err)
	}
	state, rec := proposalState(t, b)
	d := rec["decision"].(map[string]any)
	if state != "noted" || d["decision"] != "accept" || d["defer_count"] != float64(1) {
		t.Fatalf("state %s decision %v", state, d)
	}
	if err := b.c.spec.Validate("urn:botaru:schema:proposal-record:1", rec); err != nil {
		t.Fatalf("record: %v", err)
	}
}

// "Accept" of a manifest change makes the person who accepted its executor with the deadline
// and the trial record with the values before; without the change by realize_by the
// realization fails with the trial canceled (PRP-18, PRP-21).
func TestProposalRealizeTimeout(t *testing.T) {
	ctx := context.Background()
	b := newCommissionBench(t, true, presentEdit)
	b.helloDone(t, "1", "", "hi")
	b.loop(t, "observe")
	if _, err := b.analysis(t, "1", false, map[string]any{"proposals": []any{changeBody("slow", limitsChange(7))}}); err != nil {
		t.Fatal(err)
	}
	b.loop(t, "proposal")
	b.reviewAll(t, 4)
	b.loop(t, "proposal")
	if state, rec := proposalState(t, b); state != "presented" {
		t.Fatalf("state %s %v %v", state, rec["state_reason"], rec["verdict"])
	}
	card := cardOf(t, b.drain(t), proposalCard, "direct:1001")
	id, nonce := b.proposalNonce(t)
	r, err := b.c.Decide(ctx, b.channel, press("telegram:1001", "direct:1001", card.external), DecideRequest{SubjectKind: "proposal", ID: id, Nonce: nonce, Decision: "accept"})
	if err != nil || r.State != "realizing" {
		t.Fatalf("accept %+v %v", r, err)
	}
	state, rec := proposalState(t, b)
	tr := rec["trial"].(map[string]any)
	if state != "realizing" || tr["state"] != "baseline" || len(tr["guards"].([]any)) == 0 {
		t.Fatalf("state %s trial %v", state, tr)
	}
	if err := b.c.spec.Validate("urn:botaru:schema:proposal-record:1", rec); err != nil {
		t.Fatalf("record: %v", err)
	}
	rc := cardOf(t, b.drain(t), realizationCard, "direct:1001")
	validOut(t, b, "card", rc.body["card"])
	b.advance(t, 8*24*3600)
	b.loop(t, "proposal")
	state, rec = proposalState(t, b)
	if state != "realization_failed" || rec["state_reason"] != "timeout" || rec["trial"].(map[string]any)["outcome"] != "canceled" {
		t.Fatalf("state %s %v %v", state, rec["state_reason"], rec["trial"])
	}
}
