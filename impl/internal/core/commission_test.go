// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/HunterEwok/botaru/impl/internal/manifest"
	"github.com/HunterEwok/botaru/impl/internal/spec"
	"github.com/HunterEwok/botaru/impl/internal/testbench"
)

// newCommissionBench is the analysis bench with the demo commission: the charter seats with
// the fallback reviewers reviewer-a and reviewer-b, the lens review types and the commission
// thresholds; decide names whether alice may decide on reliability proposals.
func newCommissionBench(t *testing.T, decide bool) *bench {
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
		return append(append(ms, extra...), map[string]any{
			"apiVersion": "botaru/v1alpha1", "kind": "Group", "metadata": map[string]any{"name": "deciders", "tenant": "demo"},
			"spec": map[string]any{"caps": []any{"proposal.decide:reliability"}},
		})
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
