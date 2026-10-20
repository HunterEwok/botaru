// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"testing"
)

func cloneSet(t *testing.T, ms []map[string]any) []map[string]any {
	t.Helper()
	raw, err := json.Marshal(ms)
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// withLimit is the applied set with the demo tenant's limits.max_queued at v.
func withLimit(t *testing.T, b *bench, v int) []map[string]any {
	t.Helper()
	ms := cloneSet(t, b.applied)
	ten := specOf(ms, "Tenant", "demo")
	lim, _ := ten["limits"].(map[string]any)
	if lim == nil {
		lim = map[string]any{}
		ten["limits"] = lim
	}
	lim["max_queued"] = v
	return ms
}

// acceptLimits takes two manifest proposals on the same field to presented and accepts the
// first; the second goes to parked(conflict) (PRP-9).
func acceptLimits(t *testing.T, b *bench) (string, string) {
	t.Helper()
	ctx := context.Background()
	b.helloDone(t, "1", "", "hi")
	b.loop(t, "observe")
	if _, err := b.analysis(t, "1", false, map[string]any{"proposals": []any{changeBody("slow", limitsChange(7))}}); err != nil {
		t.Fatal(err)
	}
	b.asOtherAuthor(t)
	if _, err := b.analysis(t, "1b", false, map[string]any{"proposals": []any{changeBody("queue_full", limitsChange(9))}}); err != nil {
		t.Fatal(err)
	}
	b.loop(t, "proposal")
	b.reviewAll(t, 4)
	b.loop(t, "proposal")
	sent := b.drain(t)
	var ids []string
	for _, rec := range proposals(t, b) {
		if rec["state"] != "presented" {
			t.Fatalf("state %v %v", rec["state"], rec["state_reason"])
		}
		ids = append(ids, rec["id"].(string))
	}
	card := cardOf(t, sent, proposalCard, "direct:1001")
	id := card.body["card"].(map[string]any)["id"].(string)
	var nonce string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT nonce FROM proposals WHERE id = $1`, id).Scan(&nonce); err != nil {
		t.Fatal(err)
	}
	if r, err := b.c.Decide(ctx, b.channel, press("telegram:1001", "direct:1001", card.external), DecideRequest{SubjectKind: "proposal", ID: id, Nonce: nonce, Decision: "accept"}); err != nil || r.State != "realizing" {
		t.Fatalf("accept %+v %v", r, err)
	}
	other := ids[0]
	if other == id {
		other = ids[1]
	}
	var state, reason string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT state, record->>'state_reason' FROM proposals WHERE id = $1`, other).Scan(&state, &reason); err != nil || state != "parked" || reason != "conflict" {
		t.Fatalf("other %s %s %v", state, reason, err)
	}
	return id, other
}

func recordOf(t *testing.T, b *bench, id string) map[string]any {
	t.Helper()
	var rec map[string]any
	if err := b.c.st.Pool.QueryRow(context.Background(), `SELECT record FROM proposals WHERE id = $1`, id).Scan(&rec); err != nil {
		t.Fatal(err)
	}
	return rec
}

// A commit with Botaru-Proposal that sets the accepted value realizes the proposal with its
// reverse edit and starts the trial; a commit of another tenant's object is refused; a commit
// with Botaru-Revert that returns the value reverts it, and harm_observed makes the trial
// worse; the conflicting proposal returns to screened (PRP-8, PRP-9, PRP-19, PRP-20, PRP-22).
func TestProposalCommitLink(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newCommissionBench(t, true, presentEdit)
	id, other := acceptLimits(t, b)
	ms := withLimit(t, b, 7)
	// A commit that names the proposal and also changes a global object is refused whole.
	bad := cloneSet(t, ms)
	for _, m := range bad {
		if m["kind"] == "ModelProfile" {
			m["spec"].(map[string]any)["tokens_per_s"] = 61
			break
		}
	}
	if res, err := b.c.Apply(ctx, b.op, ApplyRequest{Commit: "a000000000000000000000000000000000000001", Message: "Change\n\nBotaru-Proposal: " + id, Manifests: bad}); err == nil || res != nil {
		t.Fatalf("a commit outside the tenant's folder was applied: %+v", res)
	}
	if res, err := b.c.Apply(ctx, b.op, ApplyRequest{Commit: "a000000000000000000000000000000000000002", Message: "Raise the queue\n\nBotaru-Proposal: " + id, Manifests: ms}); err != nil || !res.Applied {
		t.Fatalf("apply %+v %v", res, err)
	}
	rec := recordOf(t, b, id)
	r := rec["realization"].(map[string]any)
	if rec["state"] != "realized" || r["link"] != "commit_trailer" || len(asList(r["revert_patch"])) != 1 || rec["trial"].(map[string]any)["state"] != "running" {
		t.Fatalf("state %v realization %v trial %v", rec["state"], r, rec["trial"])
	}
	if err := b.c.spec.Validate("urn:botaru:schema:proposal-record:1", rec); err != nil {
		t.Fatalf("record: %v", err)
	}
	// Each guard keeps the points behind its value before (PRP-22).
	for _, g := range asList(rec["trial"].(map[string]any)["guards"]) {
		if _, ok := g.(map[string]any)["before_samples"].(float64); !ok {
			t.Fatalf("guard without before_samples %v", g)
		}
	}
	prev := asList(r["revert_patch"])[0].(map[string]any)["previous"]
	back := withLimit(t, b, int(prev.(float64)))
	if res, err := b.c.Apply(ctx, b.op, ApplyRequest{Commit: "a000000000000000000000000000000000000003", Message: "Revert\n\nBotaru-Revert: " + id + " harm_observed", Manifests: back}); err != nil || !res.Applied {
		t.Fatalf("revert apply %+v %v", res, err)
	}
	rec = recordOf(t, b, id)
	if rec["state"] != "reverted" || rec["trial"].(map[string]any)["outcome"] != "worse" || rec["realization"].(map[string]any)["revert_reason"] != "harm_observed" {
		t.Fatalf("state %v trial %v", rec["state"], rec["trial"])
	}
	b.loop(t, "proposal")
	if s := recordOf(t, b, other)["state"]; s == "parked" {
		t.Fatalf("the conflicting proposal stayed parked")
	}
}

// After the window the code computes the trial outcome; without points it is undetermined
// (PRP-22).
func TestProposalTrialOutcome(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newCommissionBench(t, true, presentEdit)
	id, _ := acceptLimits(t, b)
	if res, err := b.c.Apply(ctx, b.op, ApplyRequest{Commit: "a000000000000000000000000000000000000002", Message: "Botaru-Proposal: " + id, Manifests: withLimit(t, b, 7)}); err != nil || !res.Applied {
		t.Fatalf("apply %+v %v", res, err)
	}
	b.advance(t, 15*24*3600)
	b.loop(t, "proposal")
	rec := recordOf(t, b, id)
	tr := rec["trial"].(map[string]any)
	if tr["outcome"] == nil || tr["after"] == nil {
		t.Fatalf("trial %v", tr)
	}
	if err := b.c.spec.Validate("urn:botaru:schema:proposal-record:1", rec); err != nil {
		t.Fatalf("record: %v", err)
	}
}

// "My outcomes" of the author passes its closed schema and goes into its analysis task
// (DIG-7, INS-5).
func TestMyOutcomes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newCommissionBench(t, true, presentEdit)
	acceptLimits(t, b)
	var worker string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT record->'origin'->>'worker' FROM proposals LIMIT 1`).Scan(&worker); err != nil {
		t.Fatal(err)
	}
	tx, err := b.c.st.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	mo, err := myOutcomes(ctx, tx, "demo", worker, myOutcomesRows)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.c.spec.Validate("urn:botaru:schema:insight-my-results:1", mo); err != nil {
		t.Fatalf("my outcomes %v: %v", mo, err)
	}
	if rows := mo["rows"].([]any); len(rows) != 2 || rows[0].(map[string]any)["human_outcome"] != "accepted" {
		t.Fatalf("rows %v", rows)
	}
}

// A commit whose object of another tenant a later apply replaced is still outside the scope
// of the proposal: the replaced versions count as well as the current manifests (PRP-19).
func TestProposalLinkCommitOldVersion(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newCommissionBench(t, true, presentEdit)
	id, _ := acceptLimits(t, b)
	commit := "a000000000000000000000000000000000000009"
	if _, err := b.c.st.Pool.Exec(ctx, `INSERT INTO manifest_versions (kind, tenant, name, doc, commit, replaced_by, replaced_at)
		VALUES ('Group', 'demo2', 'ops', '{}', $1, 'a00000000000000000000000000000000000000a', now())`, commit); err != nil {
		t.Fatal(err)
	}
	_, err := b.c.LinkCommit(ctx, Operator{Principal: b.alice, Tenant: "demo"}, LinkCommitRequest{Proposal: id, Commit: commit})
	wantReason(t, err, "proposal_scope")
}

// The metric outcome comes from the rest of the points only when the author's or its role's
// points are all there are; a few other points below min_samples leave it undetermined
// (PRP-22).
func TestMetricOutcome(t *testing.T) {
	t.Parallel()
	s := seriesDef{Outcome: &seriesOutcome{Compare: "window.p95", Better: "down", TolerancePct: 10, MinSamples: 5}}
	side := func(v float64, n int, author, role int) map[string]any {
		return map[string]any{"value": v, "samples": n,
			"without_author": map[string]any{"value": v, "samples": author}, "without_role": map[string]any{"value": v, "samples": role}}
	}
	for _, c := range []struct {
		before, after map[string]any
		out           string
		self          bool
	}{
		{side(100, 20, 20, 20), side(50, 20, 20, 20), "better", false},
		{side(100, 20, 0, 0), side(50, 20, 0, 0), "better", true},
		{side(100, 20, 3, 3), side(50, 20, 3, 3), "undetermined", false},
		{side(100, 20, 0, 0), side(50, 20, 3, 3), "undetermined", false},
		{side(100, 20, 20, 20), side(150, 20, 20, 20), "worse", false},
	} {
		if out, self := metricOutcome(s, c.before, c.after); out != c.out || self != c.self {
			t.Errorf("%v -> %v: %s %v, want %s %v", c.before, c.after, out, self, c.out, c.self)
		}
	}
}
