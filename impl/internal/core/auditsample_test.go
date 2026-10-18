// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// sampleEdit lets the deciders lift a veto of their category.
func sampleEdit(ms []map[string]any) {
	presentEdit(ms)
	for _, m := range ms {
		if m["kind"] == "Group" && m["metadata"].(map[string]any)["name"] == "deciders" {
			s := m["spec"].(map[string]any)
			s["caps"] = append(s["caps"].([]any), "proposal.override_veto:reliability")
		}
	}
}

// A proposal dismissed by a veto last month is drawn into the audit sample with the size, the
// inclusion probability and the seed recorded (DIG-5); its blind card has no scores and is
// answered first, "Would accept" reveals the scores and the veto, and "Lift veto" needs
// "Details" opened after that and marks the record veto_overridden (DIG-6, APR-14). P2
// commission.status counts the answer (DIG-5).
func TestAuditSample(t *testing.T) {
	ctx := context.Background()
	b := newCommissionBench(t, true, sampleEdit)
	b.helloDone(t, "1", "", "hi")
	b.loop(t, "observe")
	if _, err := b.analysis(t, "1", false, map[string]any{"proposals": []any{noneBody("slow", 1)}}); err != nil {
		t.Fatal(err)
	}
	b.loop(t, "proposal")
	ra := b.enroll(t, EnrollRequest{Tenant: "demo", Role: "reviewer-a", Kind: "agent"})
	rb := b.enroll(t, EnrollRequest{Tenant: "demo", Role: "reviewer-b", Kind: "agent"})
	if err := b.review(t, ra, map[string]any{"lens": "security", "score": 1, "veto": true, "confidence": "high"}); err != nil {
		t.Fatal(err)
	}
	if err := b.review(t, rb, map[string]any{"lens": "value", "score": 4, "confidence": "high"}); err != nil {
		t.Fatal(err)
	}
	b.loop(t, "proposal")
	if state, _ := proposalState(t, b); state != "dismissed" {
		t.Fatalf("state %s", state)
	}
	b.drain(t)
	// The verdict is of last month: the next pass draws the sample of that month.
	if _, err := b.c.st.Pool.Exec(ctx, `DELETE FROM audit_samples`); err != nil {
		t.Fatal(err)
	}
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE proposals SET record = jsonb_set(record, '{verdict,computed_at}',
		to_jsonb(to_char((date_trunc('month', botaru_now()) - interval '10 days') AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')))`); err != nil {
		t.Fatal(err)
	}
	b.loop(t, "proposal")
	state, rec := proposalState(t, b)
	s := mmapAny(rec["sample"])
	if state != "sample_presented" || s["inclusion_prob"] != 1.0 || s["seed_ref"] == nil {
		t.Fatalf("state %s sample %v", state, s)
	}
	var pop, size int
	var seed string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT population, size, seed FROM audit_samples WHERE id = $1`, s["seed_ref"]).Scan(&pop, &size, &seed); err != nil || pop != 1 || size != 1 || len(seed) != 64 {
		t.Fatalf("draw %d %d %q %v", pop, size, seed, err)
	}
	sent := b.drain(t)
	cardOf(t, sent, proposalDigest, "direct:1001")
	card := cardOf(t, sent, proposalSampleCard, "direct:1001")
	cd := card.body["card"].(map[string]any)
	validOut(t, b, "card", cd)
	ctxt := cd["context"].(map[string]any)
	if _, ok := ctxt["scores"]; ok || len(cd["buttons"].([]any)) != 3 || cd["buttons"].([]any)[0].(map[string]any)["code"] != "would_accept" {
		t.Fatalf("blind card %v", cd)
	}
	id, nonce := b.proposalNonce(t)
	on := press("telegram:1001", "direct:1001", card.external)
	if _, err := b.c.Decide(ctx, b.channel, on, DecideRequest{SubjectKind: "proposal", ID: id, Nonce: nonce, Decision: "override_veto"}); err == nil {
		t.Fatal("a veto was lifted before the blind answer")
	}
	if r, err := b.c.Decide(ctx, b.channel, on, DecideRequest{SubjectKind: "proposal", ID: id, Nonce: nonce, Decision: "would_accept"}); err != nil || r.State != "sample_judged" {
		t.Fatalf("would accept %+v %v", r, err)
	}
	p := b.proposalRow(t, id)
	judged := b.c.proposalCardFields(b.catalog(t), p, proposalSampleCard, false)
	if jc := judged["context"].(map[string]any); jc["scores"] == nil || judged["buttons"].([]any)[0].(map[string]any)["code"] != "override_veto" {
		t.Fatalf("judged card %v", judged)
	}
	// APR-14: no "Lift veto" before "Details" of the judged card.
	if _, err := b.c.Decide(ctx, b.channel, on, DecideRequest{SubjectKind: "proposal", ID: id, Nonce: nonce, Decision: "override_veto"}); reasonOf(err) != "details_not_opened" {
		t.Fatalf("lift without details: %v", err)
	}
	b.advance(t, 1)
	if _, err := b.c.CardDetails(ctx, b.channel, on, CardOp{Op: "card.details", SubjectKind: "proposal", ID: id, Nonce: nonce}); err != nil {
		t.Fatal(err)
	}
	if r, err := b.c.Decide(ctx, b.channel, on, DecideRequest{SubjectKind: "proposal", ID: id, Nonce: nonce, Decision: "override_veto"}); err != nil || r.State != "noted" {
		t.Fatalf("lift %+v %v", r, err)
	}
	if _, rec := proposalState(t, b); rec["veto_overridden"] != true || mmapAny(rec["sample"])["answer"] != "would_accept" {
		t.Fatalf("record %v", rec)
	}
	st, err := b.c.CommissionStatus(ctx, Operator{Tenant: "demo"}, TenantRequest{Tenant: "demo"})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.c.spec.Validate("urn:botaru:schema:p2-commission-status-response:1", st); err != nil {
		t.Fatalf("status %v: %v", st, err)
	}
	if as := st["audit_sample"].(map[string]any); as["answers"] != 1 || as["would_accept_share"] != 1.0 || as["commission_underrates"] != false {
		t.Fatalf("audit sample %v", as)
	}
}

// The size of the month sample is min(T, max(min_per_month, ceil(share × T))) (DIG-5).
func TestAuditSampleSize(t *testing.T) {
	for _, c := range [][4]int{{0, 0}, {3, 3}, {10, 4}, {40, 10}, {41, 11}} {
		if got := sampleSize(c[0], 4, 0.25); got != c[1] {
			t.Errorf("T %d: %d, want %d", c[0], got, c[1])
		}
	}
}

// reasonOf is the reason of a refusal, or "" for another error or none.
func reasonOf(err error) string {
	var p *Problem
	if errors.As(err, &p) {
		return p.Reason
	}
	return ""
}

func (b *bench) proposalRow(t *testing.T, id string) *proposalRow {
	t.Helper()
	var p *proposalRow
	if err := b.c.tx(context.Background(), func(tx pgx.Tx, _ time.Time) (err error) {
		p, err = readProposal(context.Background(), tx, id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return p
}

func (b *bench) catalog(t *testing.T) *Catalog {
	t.Helper()
	cat, err := b.c.Catalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return cat
}

// In a tenant with a required gate, "Lift veto" of a judged manifest change in the audit sample
// waits for a passed gate at the applied commit like "Accept" does (EVO-9); the lift then takes
// the proposal out of the review rule its veto taught (EVO-3).
func TestAuditSampleGate(t *testing.T) {
	ctx := context.Background()
	b := newCommissionBench(t, true, sampleEdit, gateEdit, evolutionEdit)
	alice := Operator{Principal: b.alice, Tenant: "demo"}
	b.helloDone(t, "1", "", "hi")
	b.loop(t, "observe")
	if _, err := b.analysis(t, "1", false, map[string]any{"proposals": []any{changeBody("slow", limitsChange(7))}}); err != nil {
		t.Fatal(err)
	}
	b.loop(t, "proposal")
	// Every lens reviews; the security lens vetoes.
	for _, role := range []string{"reviewer-a", "reviewer-b"} {
		w := b.enroll(t, EnrollRequest{Tenant: "demo", Role: role, Kind: "agent"})
		for att := b.claim(t, w, "attempt"); att != nil; att = b.claim(t, w, "attempt") {
			lens := strings.TrimSuffix(strings.TrimPrefix(att.Payload["task"].(map[string]any)["type"].(string), "review."), ".v1")
			res := map[string]any{"lens": lens, "score": 4, "confidence": "high", "forecast": reviewForecast(lens, 4)}
			if lens == "security" {
				res["score"], res["veto"], res["forecast"] = 1, true, reviewForecast(lens, 1)
			}
			if _, err := b.c.Complete(ctx, w, header(t, att), CompleteRequest{Result: res}); err != nil {
				t.Fatal(err)
			}
		}
	}
	b.loop(t, "proposal")
	if state, rec := proposalState(t, b); state != "dismissed" || rec["state_reason"] != "veto" {
		t.Fatalf("state %s %v", state, rec["state_reason"])
	}
	b.drain(t)
	var rules int
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM review_rules WHERE dropped_at IS NULL`).Scan(&rules); err != nil || rules != 1 {
		t.Fatalf("rules after the veto %d %v", rules, err)
	}
	if _, err := b.c.st.Pool.Exec(ctx, `DELETE FROM audit_samples`); err != nil {
		t.Fatal(err)
	}
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE proposals SET record = jsonb_set(record, '{verdict,computed_at}',
		to_jsonb(to_char((date_trunc('month', botaru_now()) - interval '10 days') AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')))`); err != nil {
		t.Fatal(err)
	}
	b.loop(t, "proposal")
	if state, _ := proposalState(t, b); state != "sample_presented" {
		t.Fatalf("state %s", state)
	}
	// While the card is blind, the review rule of its veto is not listed to people.
	if l, err := b.c.ReviewRuleList(ctx, alice, ReviewRuleListRequest{}); err != nil || len(asList(l["items"])) != 0 {
		t.Fatalf("a blind item's rule was listed: %v %v", l, err)
	}
	sent := b.drain(t)
	card := cardOf(t, sent, proposalSampleCard, "direct:1001")
	id, nonce := b.proposalNonce(t)
	on := press("telegram:1001", "direct:1001", card.external)
	if r, err := b.c.Decide(ctx, b.channel, on, DecideRequest{SubjectKind: "proposal", ID: id, Nonce: nonce, Decision: "would_accept"}); err != nil || r.State != "sample_judged" {
		t.Fatalf("would accept %+v %v", r, err)
	}
	b.advance(t, 1)
	if _, err := b.c.CardDetails(ctx, b.channel, on, CardOp{Op: "card.details", SubjectKind: "proposal", ID: id, Nonce: nonce}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.c.Decide(ctx, b.channel, on, DecideRequest{SubjectKind: "proposal", ID: id, Nonce: nonce, Decision: "override_veto"}); reasonOf(err) != "forbidden" {
		t.Fatalf("lift without a gate: %v", err)
	}
	read, err := b.c.ProposalGate(ctx, alice, GateRequest{Proposal: id})
	if err != nil {
		t.Fatal(err)
	}
	base, _ := read["applied_commit"].(string)
	if res, err := b.c.ProposalGate(ctx, alice, GateRequest{Proposal: id, Result: gateResult(base, "passed", "")}); err != nil || res["state"] != "sample_judged" {
		t.Fatalf("gate %v %v", res, err)
	}
	if r, err := b.c.Decide(ctx, b.channel, on, DecideRequest{SubjectKind: "proposal", ID: id, Nonce: nonce, Decision: "override_veto"}); err != nil || r.State != "realizing" {
		t.Fatalf("lift %+v %v", r, err)
	}
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM review_rules WHERE dropped_at IS NULL`).Scan(&rules); err != nil || rules != 0 {
		t.Fatalf("the rule of a lifted veto stayed: %d %v", rules, err)
	}
}
