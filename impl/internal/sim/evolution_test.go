// SPDX-License-Identifier: AGPL-3.0-only

package sim

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/HunterEwok/botaru/impl/internal/evals"
)

// TestStandSuite checks the stand's cases on a set read back from the farm git, where numbers
// arrive as JSON numbers: a lowered queue limit fails the case it passed before.
func TestStandSuite(t *testing.T) {
	ctx := context.Background()
	work := t.TempDir()
	r, err := initRepo(work)
	if err != nil {
		t.Fatal(err)
	}
	holdout := filepath.Join(work, "holdout")
	if err := writeCases(holdout, hiddenCases); err != nil {
		t.Fatal(err)
	}
	set := func(queued int) []map[string]any {
		return []map[string]any{
			{"apiVersion": "botaru/v1alpha1", "kind": "Tenant", "metadata": map[string]any{"name": "demo"},
				"spec": map[string]any{"limits": map[string]any{"max_queued": queued, "max_enqueue_per_hour": 600}}},
			{"apiVersion": "botaru/v1alpha1", "kind": "Role", "metadata": map[string]any{"name": "ops-reader", "tenant": "demo"},
				"spec": map[string]any{"claims": []any{"ops.investigate.v1"}, "insight": map[string]any{"types": []any{"ops.insight.v1"}}}},
		}
	}
	for _, tc := range []struct {
		queued              int
		visQueue, holdQueue evals.Status
	}{{5000, evals.Passed, evals.Passed}, {1200, evals.Failed, evals.Passed}, {10, evals.Failed, evals.Failed}} {
		if _, _, _, err := r.writeSet(ctx, set(tc.queued), "set"); err != nil {
			t.Fatal(err)
		}
		vis, err := standSuite()(ctx, r.dir, nil, "")
		if err != nil {
			t.Fatal(err)
		}
		hold, err := standSuite()(ctx, r.dir, []string{"ops-reader"}, holdout)
		if err != nil {
			t.Fatal(err)
		}
		if vis["vis-ops-claims"].Status != evals.Passed || hold["hold-ops-enqueue"].Status != evals.Passed || hold["hold-ops-insight"].Status != evals.Passed {
			t.Errorf("max_queued %d: the fixed cases %v %v", tc.queued, vis, hold)
		}
		if vis["vis-ops-queue"].Status != tc.visQueue || hold["hold-ops-queue"].Status != tc.holdQueue {
			t.Errorf("max_queued %d: vis-ops-queue %s, hold-ops-queue %s; want %s, %s", tc.queued,
				vis["vis-ops-queue"].Status, hold["hold-ops-queue"].Status, tc.visQueue, tc.holdQueue)
		}
	}
}

// TestInsightAnswerEvolve checks the mock's manifest change in the evolution mode and the plain
// answer without it.
func TestInsightAnswerEvolve(t *testing.T) {
	seen := `result {"series":"farm.task.attempts","window":"7d","rollup":"sum","groups":[{"subject":"ops.investigate.v1","points":[{"value":3}]}]}`
	var out struct {
		Proposals []map[string]any `json:"proposals"`
	}
	if err := json.Unmarshal([]byte(insightAnswer(seen, false)), &out); err != nil || len(out.Proposals) != 1 {
		t.Fatalf("plain answer: %v %v", out, err)
	}
	if k := mapOf(out.Proposals[0]["change"])["kind"]; k != "none" {
		t.Errorf("plain change kind %v", k)
	}
	if err := json.Unmarshal([]byte(insightAnswer(seen, true)), &out); err != nil || len(out.Proposals) != 1 {
		t.Fatalf("evolve answer: %v %v", out, err)
	}
	p := out.Proposals[0]
	ops := asSlice(mapOf(mapOf(p["change"])["manifest_change"])["ops"])
	if len(ops) != 1 || mapOf(ops[0])["field"] != "spec.limits.max_queued" || mapOf(ops[0])["value"] != float64(EvolveQueued) {
		t.Errorf("evolve ops %v", ops)
	}
	if mapOf(p["benefit"])["metric"] != "farm.task.attempts" {
		t.Errorf("benefit %v", p["benefit"])
	}
}
