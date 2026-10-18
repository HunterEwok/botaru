// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"testing"
)

// gateEdit makes the merge gate required for demo and lets the deciders run it.
func gateEdit(ms []map[string]any) {
	specOf(ms, "Tenant", "demo")["insights"].(map[string]any)["gate"] = map[string]any{"required": true}
	for _, m := range ms {
		if m["kind"] == "Group" && m["metadata"].(map[string]any)["name"] == "deciders" {
			s := m["spec"].(map[string]any)
			s["caps"] = append(s["caps"].([]any), "proposal.gate:reliability")
		}
	}
}

func gateResult(base, verdict, code string) *GateResult {
	steps := []GateStep{{Step: "diff", Status: "passed"}, {Step: "lint", Status: "passed"}, {Step: "coupling", Status: "passed"},
		{Step: "regression", Status: "not_run"}, {Step: "holdout", Status: "not_run"}}
	if verdict == "failed" {
		steps[2] = GateStep{Step: "coupling", Status: "failed", Code: code}
	}
	return &GateResult{Verdict: verdict, Code: code, BaseSHA: base, Steps: steps, Fields: 1, DiffLines: 2}
}

// A tenant with a required gate shows a manifest change only after a passed gate at the
// applied commit, accepts it only while that result is current, and a failed gate dismisses it
// with gate_failed and archives its variant as rejected_at_gate with the step code (EVO-9).
func TestProposalGate(t *testing.T) {
	ctx := context.Background()
	b := newCommissionBench(t, true, presentEdit, gateEdit)
	alice := Operator{Principal: b.alice, Tenant: "demo"}
	b.helloDone(t, "1", "", "hi")
	b.loop(t, "observe")
	if _, err := b.analysis(t, "1", false, map[string]any{"proposals": []any{changeBody("slow", limitsChange(7))}}); err != nil {
		t.Fatal(err)
	}
	b.loop(t, "proposal")
	b.reviewAll(t, 4)
	b.loop(t, "proposal")
	b.drain(t)
	state, rec := proposalState(t, b)
	if state != "recommended" {
		t.Fatalf("a change without a gate left recommended: %s %v", state, rec["state_reason"])
	}
	id := rec["id"].(string)

	// The read: what the gate needs.
	read, err := b.c.ProposalGate(ctx, alice, GateRequest{Proposal: id})
	if err != nil {
		t.Fatal(err)
	}
	validOut(t, b, "p2-proposal-gate-response", read)
	base, _ := read["applied_commit"].(string)
	if base == "" || read["limits"].(map[string]any)["required"] != true || len(asList(read["ops"])) != 1 {
		t.Fatalf("read %v", read)
	}
	// Without the capability.
	op := Operator{Principal: b.op.Principal, Tenant: "demo"}
	if _, err := b.c.ProposalGate(ctx, op, GateRequest{Proposal: id}); reasonOf(err) != "forbidden" {
		t.Fatalf("no capability: %v", err)
	}
	// A result of another commit, or an inconsistent one, is refused.
	if _, err := b.c.ProposalGate(ctx, alice, GateRequest{Proposal: id, Result: gateResult("b000000000000000000000000000000000000001", "passed", "")}); reasonOf(err) != "conflict" {
		t.Fatalf("another commit: %v", err)
	}
	if _, err := b.c.ProposalGate(ctx, alice, GateRequest{Proposal: id, Result: gateResult(base, "failed", "")}); reasonOf(err) != "input_schema_invalid" {
		t.Fatalf("a failed gate without a code: %v", err)
	}
	pass := gateResult(base, "passed", "")
	res, err := b.c.ProposalGate(ctx, alice, GateRequest{Proposal: id, Result: pass})
	if err != nil || res["state"] != "recommended" {
		t.Fatalf("passed %v %v", res, err)
	}
	validOut(t, b, "p2-proposal-gate-response", res)
	if err := b.c.spec.Validate("urn:botaru:schema:proposal-record:1", jsonMap(t, recordOf(t, b, id))); err != nil {
		t.Fatalf("record: %v", err)
	}
	// One verdict per commit.
	if _, err := b.c.ProposalGate(ctx, alice, GateRequest{Proposal: id, Result: pass}); err != nil {
		t.Fatalf("a repeat: %v", err)
	}
	if _, err := b.c.ProposalGate(ctx, alice, GateRequest{Proposal: id, Result: gateResult(base, "failed", "coupling_broken")}); reasonOf(err) != "conflict" {
		t.Fatalf("another verdict at the same commit: %v", err)
	}
	// Now the digest shows it.
	b.loop(t, "proposal")
	sent := b.drain(t)
	if state, _ := proposalState(t, b); state != "presented" {
		t.Fatalf("state after the gate %s", state)
	}
	card := cardOf(t, sent, proposalCard, "direct:1001")
	var nonce string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT nonce FROM proposals WHERE id = $1`, id).Scan(&nonce); err != nil {
		t.Fatal(err)
	}
	// A new applied commit makes the gate stale: "Accept" is refused.
	newBase := "a0000000000000000000000000000000000000f1"
	if r, err := b.c.Apply(ctx, b.op, ApplyRequest{Commit: newBase, Manifests: cloneSet(t, appliedSets[b])}); err != nil || !r.Applied {
		t.Fatalf("apply %+v %v", r, err)
	}
	if _, err := b.c.Decide(ctx, b.channel, press("telegram:1001", "direct:1001", card.external), DecideRequest{SubjectKind: "proposal", ID: id, Nonce: nonce, Decision: "accept"}); reasonOf(err) != "forbidden" {
		t.Fatalf("accept with a stale gate: %v", err)
	}
	// The gate at the new commit fails: dismissed, never decided.
	res, err = b.c.ProposalGate(ctx, alice, GateRequest{Proposal: id, Result: gateResult(newBase, "failed", "coupling_broken")})
	if err != nil || res["state"] != "dismissed" {
		t.Fatalf("failed %v %v", res, err)
	}
	if rec := recordOf(t, b, id); rec["state_reason"] != "gate_failed" {
		t.Fatalf("reason %v", rec["state_reason"])
	}
	v := variantOf(t, b, id)
	var reason string
	var metrics map[string]any
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT reason_code, gate_metrics FROM scaffold_variants WHERE variant_id = $1`, v.ID).Scan(&reason, &metrics); err != nil {
		t.Fatal(err)
	}
	if v.Outcome != "rejected_at_gate" || reason != "coupling_broken" || metrics["state"] != "failed" || metrics["base_sha"] != newBase {
		t.Fatalf("variant %+v %s %v", v, reason, metrics)
	}
	if _, err := b.c.ProposalGate(ctx, alice, GateRequest{Proposal: id, Result: gateResult(newBase, "passed", "")}); reasonOf(err) != "not_found" {
		t.Fatalf("a dismissed proposal: %v", err)
	}
}
