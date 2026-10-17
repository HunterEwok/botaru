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

// appliedSets keeps the manifest set a commission bench applied, for a later apply.
var appliedSets = map[*bench][]map[string]any{}

// newCommissionBench is the analysis bench with the demo commission: the charter seats with
// the fallback reviewers reviewer-a and reviewer-b, the lens review types and the commission
// thresholds; decide names whether alice may decide on reliability proposals; edits change the
// manifests before apply.
func newCommissionBench(t *testing.T, decide bool, edits ...func([]map[string]any)) *bench {
	t.Helper()
	return newCommissionBenchWith(t, decide, nil, edits...)
}

// newCommissionBenchWith is the commission bench with more manifests.
func newCommissionBenchWith(t *testing.T, decide bool, more []map[string]any, edits ...func([]map[string]any)) *bench {
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
	var kept []map[string]any
	b := newToolBenchWith(t, &kept, func(ms []map[string]any) []map[string]any {
		ms = insightEdit(map[string]any{"max_open_proposals": 5})(ms)
		for _, m := range ms {
			if m["kind"] == "Group" && m["metadata"].(map[string]any)["name"] == "system-ops" {
				s := m["spec"].(map[string]any)
				s["caps"] = append(s["caps"].([]any), "task:review.security.v1", "task:review.operational.v1", "task:review.cost.v1", "task:review.value.v1")
			}
		}
		ms = append(append(append(ms, extra...), more...), map[string]any{
			"apiVersion": "botaru/v1alpha1", "kind": "Group", "metadata": map[string]any{"name": "deciders", "tenant": "demo"},
			"spec": map[string]any{"caps": []any{"proposal.decide:reliability"}, "data": []any{"summary", "internal"}},
		})
		for _, e := range edits {
			e(ms)
		}
		return ms
	})
	appliedSets[b] = kept
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

// review claims the next lens review of w and hands in result. A result without a forecast
// of a proposal lens gets one that agrees with its score, as review-result.v2 of the v0.3 claim
// requires (COM-15, COM-16); the necessity review of an escalation has none.
func (b *bench) review(t *testing.T, w *Worker, result map[string]any) error {
	t.Helper()
	if _, ok := result["forecast"]; !ok && result["lens"] != necessityLens {
		if score, ok := result["score"].(int); ok {
			result["forecast"] = reviewForecast(asString(result["lens"]), score)
		}
	}
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

// reviewOf is the review of a lens in a proposal record, with the record of its task.
func reviewOf(t *testing.T, b *bench, rec map[string]any, lens string) (map[string]any, map[string]any) {
	t.Helper()
	for _, r := range asList(rec["reviews"]) {
		rm := r.(map[string]any)
		if rm["lens"] != lens {
			continue
		}
		var task map[string]any
		if err := b.c.st.Pool.QueryRow(context.Background(), `SELECT record FROM tasks WHERE id = $1`, rm["task"]).Scan(&task); err != nil {
			t.Fatal(err)
		}
		return rm, task
	}
	t.Fatalf("no %s review in %v", lens, rec["reviews"])
	return nil, nil
}

// A review reserves its type's tree budget, the sum the review line was checked for, even
// when the history of the type is cheaper (COM-6, BUD-14).
func TestCommissionReviewReserve(t *testing.T) {
	ctx := context.Background()
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
	b.loop(t, "proposal")
	// The finished reviews cost nothing: the p90 of the types is 0 from now on.
	b.asOtherAuthor(t)
	if _, err := b.analysis(t, "2", false, map[string]any{"proposals": []any{noneBody("queue_full", 1)}}); err != nil {
		t.Fatal(err)
	}
	b.loop(t, "proposal")
	var n int
	var min, max int64
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*), coalesce(min(s.eur_micros), 0), coalesce(max(s.eur_micros), 0) FROM spend s JOIN tasks t ON t.id = s.task
		WHERE t.type LIKE 'review.%' AND t.state = 'submitted' AND s.unit = 'task' AND s.state = 'reserved'`).Scan(&n, &min, &max); err != nil {
		t.Fatal(err)
	}
	if n != 2 || min != 80000 || max != 80000 {
		t.Fatalf("review reserves %d %d..%d", n, min, max)
	}
}

// A seat whose holder is the author's own bot does not review the proposal: the lens goes to
// its fallback role (COM-2, GOV-17).
func TestCommissionSeatNotAuthor(t *testing.T) {
	ctx := context.Background()
	b := newCommissionBench(t, true, func(ms []map[string]any) {
		specOf(ms, "Governance", "demo")["commission"].(map[string]any)["seat_role"] = "reviewer-a"
	})
	author := b.botOf(t, b.agent.ID)
	b.helloDone(t, "1", "", "hi")
	b.loop(t, "observe")
	if _, err := b.analysis(t, "1", false, map[string]any{"proposals": []any{noneBody("slow", 1)}}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.c.SeatAssign(ctx, b.op, SeatRequest{Seat: "commission:1", Bot: author.ID}); err != nil {
		t.Fatal(err)
	}
	b.loop(t, "proposal")
	state, rec := proposalState(t, b)
	if state != "in_review" {
		t.Fatalf("state %s %v", state, rec["state_reason"])
	}
	r, task := reviewOf(t, b, rec, "security")
	if r["executor"] != "fallback_role" || task["seat"] != nil {
		t.Fatalf("the author's seat reviews: %v %v", r, task)
	}
}

// A held seat reviews its lens even when the lens has no fallback executor: the seated lens
// counts for the veto cover and the review money (COM-6, COM-7).
func TestCommissionSeatWithoutFallback(t *testing.T) {
	ctx := context.Background()
	b := newCommissionBench(t, true, func(ms []map[string]any) {
		specOf(ms, "Governance", "demo")["commission"].(map[string]any)["seat_role"] = "reviewer-b"
	})
	holder := b.botOf(t, b.agent.ID)
	if _, err := b.c.SeatAssign(ctx, b.op, SeatRequest{Seat: "commission:1", Bot: holder.ID}); err != nil {
		t.Fatal(err)
	}
	b.helloDone(t, "1", "", "hi")
	b.loop(t, "observe")
	if _, err := b.analysis(t, "1", false, map[string]any{"proposals": []any{noneBody("slow", 1)}}); err != nil {
		t.Fatal(err)
	}
	// The fallback role of the security seat no longer claims the lens; the seat role does.
	cat, err := b.c.Catalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	narrow := *cat
	narrow.Roles = maps.Clone(cat.Roles)
	ra, rb := narrow.Roles["reviewer-a"], narrow.Roles["reviewer-b"]
	ra.Claims = []string{"review.operational.v1"}
	rb.Claims = append(slices.Clone(rb.Claims), "review.security.v1")
	narrow.Roles["reviewer-a"], narrow.Roles["reviewer-b"] = ra, rb
	_, rec := proposalState(t, b)
	if err := b.c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		return b.c.stepProposal(ctx, tx, now, &narrow, rec["id"].(string))
	}); err != nil {
		t.Fatal(err)
	}
	state, rec := proposalState(t, b)
	if state != "in_review" {
		t.Fatalf("state %s %v", state, rec["state_reason"])
	}
	r, task := reviewOf(t, b, rec, "security")
	if r["executor"] != "seat" || task["seat"] != "commission:1" || task["executor_role"] != "reviewer-b" {
		t.Fatalf("security review %v %v", r, task)
	}
	if v, _ := rec["review_reserved_eur"].(float64); v < 0.16-1e-9 {
		t.Fatalf("reserved %v", rec["review_reserved_eur"])
	}
}
