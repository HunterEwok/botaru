// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"strconv"
	"strings"
	"testing"

	"github.com/HunterEwok/botaru/impl/internal/manifest"
)

// Tests of the workspace ceilings and the review line at apply (GW-9, BUD-13).

func lintDetails(t *testing.T, c *Core, docs string) map[string]string {
	t.Helper()
	ds, err := manifest.ParseYAML(strings.NewReader(docs), "test")
	if err != nil {
		t.Fatal(err)
	}
	index := make([]int, len(ds))
	for i := range ds {
		index[i] = i
		if ds[i].Kind == "Role" || ds[i].Kind == "TaskType" {
			ds[i].Tenant = ""
		}
	}
	errs, err := c.lintManifests(ds, index, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, e := range errs {
		out[e.Code+" "+e.Name] = e.Detail
	}
	return out
}

// A tenant's month limit within the share of its workspace ceiling, the tenants together within
// the share of the organization ceiling, and a tenant without a described workspace is named
// (GW-9).
func TestLintWorkspaceCeilings(t *testing.T) {
	t.Parallel()
	c := lintCore(t)
	c.cfg.Workspaces = WorkspaceCeilings{Providers: map[string]ProviderCeilings{
		"provider-a": {OrganizationEUR: 1000, Workspaces: map[string]float64{"demo": 100, "demo3": 1000}},
	}}
	tenant := func(name string, month int) string {
		return "apiVersion: botaru/v1alpha1\nkind: Tenant\nmetadata: {name: " + name + "}\nspec: {kind: test, farm_manager: {enabled: false}, budgets: {month_eur: " + strconv.Itoa(month) + "}}\n---\n"
	}
	got := lintDetails(t, c, tenant("demo", 90)+tenant("demo2", 10))
	if _, ok := got["workspace_ceiling_missing demo2"]; !ok || len(got) != 1 {
		t.Fatalf("within the ceiling %v", got)
	}
	got = lintDetails(t, c, tenant("demo", 91))
	if _, ok := got["workspace_ceiling_exceeded demo"]; !ok {
		t.Fatalf("above the workspace ceiling %v", got)
	}
	got = lintDetails(t, c, tenant("demo", 50)+tenant("demo3", 880))
	if _, ok := got["workspace_ceiling_exceeded demo"]; !ok {
		t.Fatalf("above the organization ceiling %v", got)
	}
}

// The review line covers max_per_week x 4.4 x the price COM-6 reserves for the reviews of
// one proposal, the lenses with an executor; a line below it is rejected with the missing
// amount, and without an executor of the veto lenses no review is reserved (BUD-13, COM-6).
func TestLintReviewLine(t *testing.T) {
	t.Parallel()
	c := lintCore(t)
	var types []string
	for _, d := range c.lensDefs() {
		if d.Veto {
			types = append(types, d.ReviewType)
		}
	}
	var docs strings.Builder
	for _, ty := range types {
		docs.WriteString("apiVersion: botaru/v1alpha1\nkind: TaskType\nmetadata: {name: " + ty + "}\nspec: {tree_budget_eur: 0.06, purpose: review}\n---\n")
	}
	docs.WriteString("apiVersion: botaru/v1alpha1\nkind: Role\nmetadata: {name: analyst}\nspec: {insight: {max_per_week: 3}}\n---\n")
	reviewer := "apiVersion: botaru/v1alpha1\nkind: Role\nmetadata: {name: reviewer}\nspec: {profile: reviewer, claims: [" + strings.Join(types, ", ") + "]}\n---\n"
	tenant := func(review string) string {
		return "apiVersion: botaru/v1alpha1\nkind: Tenant\nmetadata: {name: demo}\nspec: {kind: test, budgets: {lines: {insight: {month_eur: 5}, review: {month_eur: " + review + "}}}}\n"
	}
	need := reviewNeed([]int{3}, int64(len(types))*60000)
	got := lintDetails(t, c, docs.String()+reviewer+tenant("0.5"))
	d, ok := got["review_line_too_small demo"]
	if !ok || !strings.Contains(d, eurCents(need)+" EUR a month, "+eurCents(need-500000)+" EUR more") {
		t.Fatalf("a short review line %v (need %d)", got, need)
	}
	if _, ok := lintDetails(t, c, docs.String()+reviewer+tenant("10"))["review_line_too_small demo"]; ok {
		t.Fatal("a covering review line rejected")
	}
	if _, ok := lintDetails(t, c, docs.String()+tenant("0.5"))["review_line_too_small demo"]; ok {
		t.Fatal("a review line rejected for lenses without an executor")
	}
	if reviewNeed([]int{3}, 240000) != 3168000 || eurCents(1168000) != "1.17" {
		t.Fatal("the formula")
	}
}
