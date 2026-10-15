// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/manifest"
	"github.com/HunterEwok/botaru/impl/internal/spec"
	"github.com/HunterEwok/botaru/impl/internal/testbench"
)

// newCommissionBench is the analysis bench with the demo commission: the charter seats with
// the fallback reviewers reviewer-a and reviewer-b, the lens review types and the commission
// thresholds; decide names whether alice may decide on reliability proposals; edits change the
// manifests before apply.
func newCommissionBench(t *testing.T, decide bool, edits ...func([]map[string]any)) *bench {
	t.Helper()
	root, err := spec.FindRoot("")
	if err != nil {
		t.Fatal(err)
	}
	var files []string
	for _, f := range []string{"examples/roles/reviewer-a/role.yaml", "examples/roles/reviewer-b/role.yaml",
		"examples/tasktypes/review.security.v1.yaml", "examples/tasktypes/review.operational.v1.yaml",
		"examples/tasktypes/review.cost.v1.yaml", "examples/tasktypes/review.value.v1.yaml",
		"examples/model-profiles/review-a.yaml", "examples/model-profiles/review-b.yaml",
		"examples/tenants/demo/governance.yaml", "examples/tenants/demo/commission.yaml"} {
		files = append(files, filepath.Join(root, f))
	}
	docs, err := manifest.ReadFiles(files)
	if err != nil {
		t.Fatal(err)
	}
	var extra []map[string]any
	for _, d := range docs {
		extra = append(extra, d.Value)
	}
	sp, err := spec.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := testbench.DropUnsupported(sp, extra); err != nil {
		t.Fatal(err)
	}
	b := newToolBenchWith(t, nil, func(ms []map[string]any) []map[string]any {
		ms = insightEdit(map[string]any{"max_open_proposals": 5})(ms)
		for _, m := range ms {
			if m["kind"] == "Group" && m["metadata"].(map[string]any)["name"] == "system-ops" {
				s := m["spec"].(map[string]any)
				s["caps"] = append(s["caps"].([]any), "task:review.security.v1", "task:review.operational.v1", "task:review.cost.v1", "task:review.value.v1")
			}
		}
		ms = append(append(ms, extra...), map[string]any{
			"apiVersion": "botaru/v1alpha1", "kind": "Group", "metadata": map[string]any{"name": "deciders", "tenant": "demo"},
			"spec": map[string]any{"caps": []any{"proposal.decide:reliability"}},
		})
		for _, e := range edits {
			e(ms)
		}
		return ms
	})
	if decide {
		// proposal.decide is privileged; the grant flow with a code comes later, so the bench
		// writes the membership.
		if _, err := b.c.st.Pool.Exec(context.Background(), `INSERT INTO memberships (tenant, principal, grp, state, created_at) VALUES ('demo', $1, 'deciders', 'active', now())`, b.alice); err != nil {
			t.Fatal(err)
		}
	}
	return b
}

func proposalState(t *testing.T, b *bench) (string, map[string]any) {
	t.Helper()
	recs := proposals(t, b)
	if len(recs) != 1 {
		t.Fatalf("records %d, rejections %v", len(recs), rejections(t, b))
	}
	return recs[0]["state"].(string), recs[0]
}

// review claims the next lens review of w and hands in result.
func (b *bench) review(t *testing.T, w *Worker, result map[string]any) error {
	t.Helper()
	att := b.claim(t, w, "attempt")
	if att == nil {
		t.Fatal("no review attempt")
	}
	_, err := b.c.Complete(context.Background(), w, header(t, att), CompleteRequest{Result: result})
	return err
}

// A proposal of kind none goes to the security and value lenses of its seats' fallback roles,
// each review only to its executor, and the code verdict recommends it (COM-2, COM-4, COM-7,
// COM-14).
func TestCommissionReview(t *testing.T) {
	b := newCommissionBench(t, true)
	b.helloDone(t, "1", "", "hi")
	b.loop(t, "observe")
	if _, err := b.analysis(t, "1", false, map[string]any{"proposals": []any{noneBody("slow", 1)}}); err != nil {
		t.Fatal(err)
	}
	b.loop(t, "proposal")
	state, rec := proposalState(t, b)
	if state != "in_review" {
		t.Fatalf("state %s %v", state, rec["state_reason"])
	}
	if n := len(rec["reviews"].([]any)); n != 2 {
		t.Fatalf("reviews %v", rec["reviews"])
	}
	// The author's role does not claim reviews; each reviewer gets only its own lens.
	if att := b.claim(t, b.agent, "attempt"); att != nil {
		t.Fatalf("the author's worker got %v", att.Payload)
	}
	ra := b.enroll(t, EnrollRequest{Tenant: "demo", Role: "reviewer-a", Kind: "agent"})
	rb := b.enroll(t, EnrollRequest{Tenant: "demo", Role: "reviewer-b", Kind: "agent"})
	if err := b.review(t, ra, map[string]any{"lens": "security", "score": 3, "confidence": "high"}); err != nil {
		t.Fatal(err)
	}
	if err := b.review(t, rb, map[string]any{"lens": "value", "score": 3, "confidence": "medium"}); err != nil {
		t.Fatal(err)
	}
	b.loop(t, "proposal")
	state, rec = proposalState(t, b)
	v := rec["verdict"].(map[string]any)
	if state != "recommended" || v["outcome"] != "recommended" || v["weighted_mean"] != float64(3) {
		t.Fatalf("state %s verdict %v", state, v)
	}
	if err := b.c.spec.Validate("urn:botaru:schema:proposal-record:1", rec); err != nil {
		t.Fatalf("record: %v", err)
	}
}

// Without a person who may decide, a proposal is dismissed before any review (COM-5).
func TestCommissionNoDecider(t *testing.T) {
	b := newCommissionBench(t, false)
	b.helloDone(t, "1", "", "hi")
	b.loop(t, "observe")
	if _, err := b.analysis(t, "1", false, map[string]any{"proposals": []any{noneBody("slow", 1)}}); err != nil {
		t.Fatal(err)
	}
	b.loop(t, "proposal")
	if state, rec := proposalState(t, b); state != "dismissed" || rec["state_reason"] != "other" || len(rec["reviews"].([]any)) != 0 {
		t.Fatalf("state %s %v", state, rec["state_reason"])
	}
}

// A veto from the security lens dismisses the proposal (COM-4).
func TestCommissionVeto(t *testing.T) {
	b := newCommissionBench(t, true)
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
	if state, rec := proposalState(t, b); state != "dismissed" || rec["state_reason"] != "veto" {
		t.Fatalf("state %s %v", state, rec["state_reason"])
	}
}

// A review that hands in another lens's result is refused and does not count (COM-3).
func TestCommissionWrongLens(t *testing.T) {
	ctx := context.Background()
	b := newCommissionBench(t, true)
	b.helloDone(t, "1", "", "hi")
	b.loop(t, "observe")
	if _, err := b.analysis(t, "1", false, map[string]any{"proposals": []any{noneBody("slow", 1)}}); err != nil {
		t.Fatal(err)
	}
	b.loop(t, "proposal")
	ra := b.enroll(t, EnrollRequest{Tenant: "demo", Role: "reviewer-a", Kind: "agent"})
	if err := b.review(t, ra, map[string]any{"lens": "operational", "score": 4, "confidence": "high"}); err == nil {
		t.Fatal("a review of another lens was accepted")
	}
	var n int
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE type = 'review.security.v1' AND state = 'completed'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("completed reviews %d %v", n, err)
	}
}

// specOf is the spec of the manifest of kind and name in ms.
func specOf(ms []map[string]any, kind, name string) map[string]any {
	for _, m := range ms {
		if m["kind"] == kind && m["metadata"].(map[string]any)["name"] == name {
			return m["spec"].(map[string]any)
		}
	}
	return nil
}

func proposeNone(t *testing.T, b *bench) {
	t.Helper()
	b.helloDone(t, "1", "", "hi")
	b.loop(t, "observe")
	if _, err := b.analysis(t, "1", false, map[string]any{"proposals": []any{noneBody("slow", 1)}}); err != nil {
		t.Fatal(err)
	}
	b.loop(t, "proposal")
}

// Without seats a veto lens goes to a reviewer role that claims its type (COM-8).
func TestCommissionNoSeats(t *testing.T) {
	b := newCommissionBench(t, true, func(ms []map[string]any) {
		specOf(ms, "Governance", "demo")["commission"].(map[string]any)["seats"] = []any{}
	})
	proposeNone(t, b)
	state, rec := proposalState(t, b)
	reviews := rec["reviews"].([]any)
	if state != "in_review" || len(reviews) != 1 || reviews[0].(map[string]any)["lens"] != "security" {
		t.Fatalf("state %s reviews %v", state, reviews)
	}
	ra := b.enroll(t, EnrollRequest{Tenant: "demo", Role: "reviewer-a", Kind: "agent"})
	if err := b.review(t, ra, map[string]any{"lens": "security", "score": 3, "confidence": "high"}); err != nil {
		t.Fatal(err)
	}
}

// A veto lens whose seat role cannot review it sends no review: the verdict parks the
// proposal as veto_lens_missing through in_review, and it is screened again once the lens has
// an executor (COM-4, COM-7, PRP-6). The lint keeps such a charter out of apply, so the test
// narrows a copy of the catalog.
func TestCommissionVetoLensMissing(t *testing.T) {
	ctx := context.Background()
	b := newCommissionBench(t, true)
	b.helloDone(t, "1", "", "hi")
	b.loop(t, "observe")
	if _, err := b.analysis(t, "1", false, map[string]any{"proposals": []any{noneBody("slow", 1)}}); err != nil {
		t.Fatal(err)
	}
	cat, err := b.c.Catalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	narrow := *cat
	narrow.Roles = maps.Clone(cat.Roles)
	ra := narrow.Roles["reviewer-a"]
	ra.Claims = []string{"review.operational.v1"}
	narrow.Roles["reviewer-a"] = ra
	_, rec := proposalState(t, b)
	id := rec["id"].(string)
	step := func(c *Catalog) {
		t.Helper()
		if err := b.c.tx(ctx, func(tx pgx.Tx, now time.Time) error { return b.c.stepProposal(ctx, tx, now, c, id) }); err != nil {
			t.Fatal(err)
		}
	}
	step(&narrow)
	if state, rec := proposalState(t, b); state != "in_review" || len(rec["reviews"].([]any)) != 0 {
		t.Fatalf("state %s reviews %v", state, rec["reviews"])
	}
	step(&narrow)
	if state, rec := proposalState(t, b); state != "parked" || rec["state_reason"] != "veto_lens_missing" {
		t.Fatalf("state %s %v", state, rec["state_reason"])
	}
	step(cat)
	if state, _ := proposalState(t, b); state != "screened" {
		t.Fatalf("state %s", state)
	}
}

// A refusal other than money keeps the proposal waiting in screened with its code, parks it
// after review_wait_max, and park_ttl ends it (COM-6, PRP-6).
func TestCommissionWaitsForCap(t *testing.T) {
	b := newCommissionBench(t, true, func(ms []map[string]any) {
		s := specOf(ms, "Group", "system-ops")
		s["caps"] = slices.DeleteFunc(slices.Clone(s["caps"].([]any)), func(c any) bool { return strings.HasPrefix(c.(string), "task:review.") })
	})
	proposeNone(t, b)
	if state, rec := proposalState(t, b); state != "screened" || rec["waiting_for"] != "no_group_cap" {
		t.Fatalf("state %s %v %v", state, rec["state_reason"], rec["waiting_for"])
	}
	b.advance(t, 8*24*3600)
	b.loop(t, "proposal")
	if state, rec := proposalState(t, b); state != "parked" || rec["state_reason"] != "review_budget" || rec["waiting_for"] != "no_group_cap" {
		t.Fatalf("state %s %v %v", state, rec["state_reason"], rec["waiting_for"])
	}
	for range 4 {
		b.advance(t, 8*24*3600)
		b.loop(t, "proposal")
	}
	if state, _ := proposalState(t, b); state != "expired" {
		t.Fatalf("state %s", state)
	}
}

// A veto lens review that never comes parks the proposal; it is reviewed again at most
// maxReviewRounds times, then stays parked until park_ttl (COM-4, COM-7, PRP-6).
func TestCommissionVetoReviewMissing(t *testing.T) {
	b := newCommissionBench(t, true)
	proposeNone(t, b)
	rb := b.enroll(t, EnrollRequest{Tenant: "demo", Role: "reviewer-b", Kind: "agent"})
	if err := b.review(t, rb, map[string]any{"lens": "value", "score": 3, "confidence": "medium"}); err != nil {
		t.Fatal(err)
	}
	for range 10 {
		b.advance(t, 49*3600)
		b.loop(t, "proposal")
	}
	if state, rec := proposalState(t, b); state != "parked" || rec["state_reason"] != "veto_lens_missing" || rec["review_round"] != float64(maxReviewRounds) {
		t.Fatalf("state %s %v round %v", state, rec["state_reason"], rec["review_round"])
	}
}

// A review handed in on time counts even when the loop reads it after the deadline (COM-4).
func TestCommissionLateRead(t *testing.T) {
	b := newCommissionBench(t, true)
	proposeNone(t, b)
	ra := b.enroll(t, EnrollRequest{Tenant: "demo", Role: "reviewer-a", Kind: "agent"})
	rb := b.enroll(t, EnrollRequest{Tenant: "demo", Role: "reviewer-b", Kind: "agent"})
	if err := b.review(t, ra, map[string]any{"lens": "security", "score": 3, "confidence": "high"}); err != nil {
		t.Fatal(err)
	}
	if err := b.review(t, rb, map[string]any{"lens": "value", "score": 3, "confidence": "medium"}); err != nil {
		t.Fatal(err)
	}
	b.advance(t, 49*3600)
	b.loop(t, "proposal")
	if state, rec := proposalState(t, b); state != "recommended" {
		t.Fatalf("state %s %v", state, rec["verdict"])
	}
}
