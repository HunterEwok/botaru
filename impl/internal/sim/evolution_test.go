// SPDX-License-Identifier: AGPL-3.0-only

package sim

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/HunterEwok/botaru/impl/internal/core"
	"github.com/HunterEwok/botaru/impl/internal/evals"
	"github.com/HunterEwok/botaru/impl/internal/spec"
	"github.com/HunterEwok/botaru/impl/internal/yamlx"
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
	if len(ops) != 1 || mapOf(ops[0])["field"] != evolveField || mapOf(ops[0])["value"] != EvolveAttempt {
		t.Errorf("evolve ops %v", ops)
	}
	if mapOf(p["benefit"])["metric"] != "farm.task.attempts" {
		t.Errorf("benefit %v", p["benefit"])
	}
}

// TestEvolveInstruction: the instruction of the analysis passes the unawareness lint (GOV-29)
// with the load check of the evolution mode and without it, and turning the mode off brings
// back the instruction of the farm git byte for byte.
func TestEvolveInstruction(t *testing.T) {
	ctx := context.Background()
	root, err := spec.FindRoot("")
	if err != nil {
		t.Fatal(err)
	}
	sp, err := spec.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(root, "examples/tasktypes/ops.insight.v1.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var tt map[string]any
	if err := yamlx.Unmarshal(raw, &tt); err != nil {
		t.Fatal(err)
	}
	base, _ := mapOf(tt["spec"])["instruction"].(string)
	for name, text := range map[string]string{"usual": base, "evolution": evolveTask} {
		if bad, err := core.UnawareText(sp, text); err != nil || bad {
			t.Errorf("the %s instruction fails the unawareness lint: %v %v", name, bad, err)
		}
	}
	if bad, _ := core.UnawareText(sp, evolveTask+"\nPropose to shorten the queue."); !bad {
		t.Error("the lint passes a paragraph with a word of each group")
	}
	r, err := initRepo(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := r.writeSet(ctx, []map[string]any{tt}, "set"); err != nil {
		t.Fatal(err)
	}
	s := &Stand{repo: r}
	for _, tc := range []struct {
		on   bool
		want string
	}{{true, evolveTask}, {true, evolveTask}, {false, base}, {false, base}} {
		if _, _, err := s.writeEvolve(ctx, tc.on); err != nil {
			t.Fatal(err)
		}
		if got, err := s.taskInstruction(insightType); err != nil || got != tc.want {
			t.Fatalf("on %v: %q %v, want %q", tc.on, got, err, tc.want)
		}
	}
}

// TestEditManifests: a rank run gives each model profile the model and the prices of its rank;
// one -model gives every profile that model.
func TestEditManifests(t *testing.T) {
	profiles := func() []map[string]any {
		var ms []map[string]any
		for _, r := range []any{1, json.Number("2"), int64(3), 4, 7} {
			ms = append(ms, map[string]any{"kind": "ModelProfile", "spec": map[string]any{"rank": r, "model": "x", "price": map[string]any{"input": 9.0}}})
		}
		return append(ms, map[string]any{"kind": "Role", "spec": map[string]any{"rank": 1}})
	}
	ranks := map[int]string{1: "m1", 2: "m2", 3: "m3", 4: "m4"}
	prices := map[string][2]float64{"m1": {0.1, 0.5}, "m2": {2, 10}, "m3": {4, 20}, "m4": {10, 50}}
	ms := editManifests(profiles(), Config{Real: true, RankModels: ranks, Prices: prices})
	for i, want := range []string{"m1", "m2", "m3", "m4", "m4"} {
		sp := mapOf(ms[i]["spec"])
		pr := mapOf(sp["price"])
		if sp["model"] != want || pr["input"] != prices[want][0] || pr["output"] != prices[want][1] {
			t.Errorf("profile %d: %v", i, sp)
		}
	}
	if _, ok := mapOf(ms[5]["spec"])["model"]; ok {
		t.Error("a role got a model")
	}
	ms = editManifests(profiles(), Config{Real: true, Model: "one", RankModels: ranks, Prices: map[string][2]float64{"one": {1, 2}}})
	for i := range 5 {
		if sp := mapOf(ms[i]["spec"]); sp["model"] != "one" || mapOf(sp["price"])["output"] != 2.0 {
			t.Errorf("profile %d: %v", i, sp)
		}
	}
	if ms := editManifests(profiles(), Config{RankModels: ranks}); mapOf(ms[0]["spec"])["model"] != "x" {
		t.Error("a mock run changed a profile")
	}
}

// TestWidenBudgets: a real model's call fits the budgets after the edit: the tree budget of a
// claimed type holds two reserves of the role's profile, the review line rises with the review
// types, the manager line holds the seat's worst day; nothing is lowered.
func TestWidenBudgets(t *testing.T) {
	md := func(n string) map[string]any { return map[string]any{"name": n} }
	ms := []map[string]any{
		{"kind": "ModelProfile", "metadata": md("rev"), "spec": map[string]any{"rank": 3, "max_output_tokens": json.Number("2000")}},
		{"kind": "ModelProfile", "metadata": md("mgr"), "spec": map[string]any{"rank": 4, "max_output_tokens": 8000}},
		{"kind": "Role", "metadata": md("reviewer"), "spec": map[string]any{"claims": []any{"review.x"}, "models": map[string]any{"base": "rev"}}},
		{"kind": "TaskType", "metadata": md("review.x"), "spec": map[string]any{"purpose": "review", "tree_budget_eur": 0.08}},
		{"kind": "TaskType", "metadata": md("big"), "spec": map[string]any{"purpose": "work", "tree_budget_eur": 5.0}},
		{"kind": "Governance", "metadata": md("farm"), "spec": map[string]any{"farm_manager": map[string]any{"model_profile": "mgr"}}},
		{"kind": "Tenant", "metadata": md("demo"), "spec": map[string]any{"budgets": map[string]any{"lines": map[string]any{
			"review": map[string]any{"month_eur": 50.0}, "manager": map[string]any{"day_eur": 2.0, "month_eur": 30.0}}}}},
	}
	editManifests(ms, Config{Real: true, RankModels: map[int]string{3: "opus", 4: "fable"},
		Prices: map[string][2]float64{"opus": {4, 20}, "fable": {10, 50}}})
	// One reserve of rev: 2000 * 20 + 60000 * 4 = 0.28 EUR.
	if got := mapOf(ms[3]["spec"])["tree_budget_eur"]; got != 0.56 {
		t.Errorf("review tree budget %v, want 0.56", got)
	}
	if got := mapOf(ms[4]["spec"])["tree_budget_eur"]; got != 5.0 {
		t.Errorf("an unclaimed type changed: %v", got)
	}
	lines := mapOf(mapOf(mapOf(ms[6]["spec"])["budgets"])["lines"])
	if got := mapOf(lines["review"])["month_eur"]; got != 350.0 {
		t.Errorf("review line %v, want 350", got)
	}
	// The worst day 1.5 * (0.6 + 1.2 + 0.8) = 3.90 is below four reserves of mgr (4 * 1.0).
	if m := mapOf(lines["manager"]); m["day_eur"] != 4.0 || m["month_eur"] != 60.0 {
		t.Errorf("manager line %v", m)
	}
}
