// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"fmt"
	"slices"

	"github.com/HunterEwok/botaru/impl/internal/money"
)

// Two money checks of apply that compare tenant limits with a ceiling: the provider workspace
// ceilings of the deployment (GW-9) and the review line against the reviews the analysis roles
// can ask for (BUD-13).

// WorkspaceCeilings are the spend ceilings set at the provider that the deployment describes
// (GW-9), by provider name of the model profiles: the organization's ceiling, the ceiling of
// each tenant's workspace and the limit of the quality test bench's workspace. Share is the part
// of a ceiling the farm limits may take, 0.9 by default; TestShare the part of the organization
// ceiling the test workspaces and the test bench may take together, Share by default.
type WorkspaceCeilings struct {
	Share     float64                     `yaml:"share"`
	TestShare float64                     `yaml:"test_share"`
	Providers map[string]ProviderCeilings `yaml:"providers"`
}

// ProviderCeilings are the ceilings at one provider in euros a month.
type ProviderCeilings struct {
	OrganizationEUR float64            `yaml:"organization_eur"`
	Workspaces      map[string]float64 `yaml:"workspaces"`
	TestBenchEUR    float64            `yaml:"test_bench_eur"`
}

// defaultWorkspaceShare is the share of a ceiling of GW-9 when the deployment sets none.
const defaultWorkspaceShare = 0.9

// share is part of a ceiling in micro-euros, rounded down.
func shareOf(eur, share float64) int64 { return int64(float64(money.FromEUR(eur)) * share) }

// lintWorkspaceCeilings is the one check of GW-9 at apply: a tenant's month_eur is not above
// the share of its workspace's ceiling at a provider whose ceilings the deployment describes,
// the month_eur of all tenants with a workspace there together not above the share of the
// organization's ceiling, and the test tenants with the test bench not above the test share of
// it. The finding goes on the tenant (workspace_ceiling_exceeded); a tenant with a month limit
// that may use a described provider without a described workspace is workspace_ceiling_missing.
func (c *Core) lintWorkspaceCeilings(S *lintSet, f *lintFindings) {
	wc := c.cfg.Workspaces
	share, test := wc.Share, wc.TestShare
	if share <= 0 {
		share = defaultWorkspaceShare
	}
	if test <= 0 {
		test = share
	}
	for _, prov := range slices.Sorted(mapKeys(wc.Providers)) {
		pc := wc.Providers[prov]
		var sum, testSum int64
		var tenants []lintObj
		for _, o := range S.each("Tenant") {
			if o.name == farmTenant {
				continue
			}
			month, ok := mnum(mmap(o.spec, "budgets"), "month_eur")
			if !ok {
				continue
			}
			allowed := mstrs(o.spec, "providers_allowed")
			ws, has := pc.Workspaces[o.name]
			if !has {
				if len(allowed) == 0 || slices.Contains(allowed, prov) {
					f.addDetail(o, "workspace_ceiling_missing", "/budgets/month_eur", fmt.Sprintf("the deployment describes no workspace of the tenant at provider %s (GW-9)", prov))
				}
				continue
			}
			m := money.FromEUR(month)
			if m > shareOf(ws, share) {
				f.addDetail(o, "workspace_ceiling_exceeded", "/budgets/month_eur", fmt.Sprintf("month_eur is above %.2f of the workspace ceiling %.2f EUR at %s (GW-9)", share, ws, prov))
			}
			sum += m
			if tenantKind(o.spec) == "test" {
				testSum += m
			}
			tenants = append(tenants, o)
		}
		if pc.OrganizationEUR <= 0 {
			continue
		}
		over := sum > shareOf(pc.OrganizationEUR, share)
		testOver := testSum+money.FromEUR(pc.TestBenchEUR) > shareOf(pc.OrganizationEUR, test)
		for _, o := range tenants {
			if over {
				f.addDetail(o, "workspace_ceiling_exceeded", "/budgets/month_eur", fmt.Sprintf("the month_eur of all tenants at %s is above %.2f of the organization ceiling %.2f EUR (GW-9)", prov, share, pc.OrganizationEUR))
			}
			if testOver && tenantKind(o.spec) == "test" {
				f.addDetail(o, "workspace_ceiling_exceeded", "/budgets/month_eur", fmt.Sprintf("the test tenants and the test bench at %s are above %.2f of the organization ceiling (GW-9)", prov, test))
			}
		}
	}
}

// weeksPerMonth is 4.4 weeks in a month, as tenths (BUD-13).
const weeksPerMonthTenths = 44

// reviewNeed is the formula of BUD-13 in micro-euros: the sum over the roles of max_per_week
// times 4.4 times the price of the reviews of one proposal, rounded up to a micro-euro.
func reviewNeed(maxPerWeek []int, price int64) int64 {
	weeks := 0
	for _, n := range maxPerWeek {
		weeks += n
	}
	return (int64(weeks)*price*weeksPerMonthTenths + 9) / 10
}

// reviewLensTypes are the review types of the lenses that review a proposal in a tenant, the
// lenses COM-6 reserves for: the veto lenses, the lenses of the charter's commission seats and,
// unless the tenant opted out of the manager (D-77), the farm_manager lens.
func (c *Core) reviewLensTypes(gov map[string]any, managerOn bool) []string {
	defs := c.lensDefs()
	var lenses []string
	for _, l := range slices.Sorted(mapKeys(defs)) {
		if defs[l].Veto {
			lenses = append(lenses, l)
		}
	}
	for _, s := range mlist(mmap(gov, "commission"), "seats") {
		l := mstr(asMap(s), "lens")
		if _, ok := defs[l]; ok && !slices.Contains(lenses, l) {
			lenses = append(lenses, l)
		}
	}
	types := make([]string, 0, len(lenses)+1)
	for _, l := range lenses {
		types = append(types, defs[l].ReviewType)
	}
	if managerOn {
		types = append(types, managerLensType)
	}
	return types
}

// reviewNeedOf gives the need of BUD-13 of a tenant and whether the formula applies: the
// tenant's insight or aggregate line is above 0. budget is the tree_budget_eur of a type.
func (c *Core) reviewNeedOf(ts, gov map[string]any, roles []map[string]any, budget func(string) float64) (int64, bool) {
	lines := mmap(mmap(ts, "budgets"), "lines")
	insight, _ := mnum(mmap(lines, "insight"), "month_eur")
	aggregate, _ := mnum(mmap(lines, "aggregate"), "month_eur")
	if insight <= 0 && aggregate <= 0 {
		return 0, false
	}
	var price int64
	for _, t := range c.reviewLensTypes(gov, mmap(ts, "farm_manager")["enabled"] != false) {
		price += money.FromEUR(budget(t))
	}
	var weeks []int
	for _, r := range roles {
		if n, ok := mnum(mmap(r, "insight"), "max_per_week"); ok {
			weeks = append(weeks, int(n))
		}
	}
	return reviewNeed(weeks, price), true
}

// lintReviewLine rejects a tenant whose review line is below the need of BUD-13
// (review_line_too_small, with the missing amount).
func (c *Core) lintReviewLine(S *lintSet, f *lintFindings) {
	var roles []map[string]any
	for _, o := range S.each("Role") {
		roles = append(roles, o.spec)
	}
	budget := func(t string) float64 { n, _ := mnum(S.get("TaskType", t, ""), "tree_budget_eur"); return n }
	for _, o := range S.each("Tenant") {
		if o.name == farmTenant {
			continue
		}
		var gov map[string]any
		for _, g := range S.each("Governance") {
			if g.tenant == o.name {
				gov = g.spec
			}
		}
		need, ok := c.reviewNeedOf(o.spec, gov, roles, budget)
		if !ok {
			continue
		}
		line, _ := mnum(mmap(mmap(mmap(o.spec, "budgets"), "lines"), "review"), "month_eur")
		if have := money.FromEUR(line); have < need {
			f.addDetail(o, "review_line_too_small", "/budgets/lines/review/month_eur",
				fmt.Sprintf("the review line needs %s EUR a month, %s EUR more (BUD-13)", eurCents(need), eurCents(need-have)))
		}
	}
}

// eurCents writes micro-euros as euros rounded up to a cent.
func eurCents(micros int64) string {
	cents := (micros + 9999) / 10000
	return fmt.Sprintf("%d.%02d", cents/100, cents%100)
}

// doctorReviewLine shows the missing amount of the review line of a tenant by BUD-13 in
// micro-euros: 0 when the line covers the need, a failing check when it does not.
func (c *Core) doctorReviewLine(cat *Catalog, tenant string, add func(DoctorCheck)) {
	tv := cat.Tenants[tenant]
	if tv == nil {
		return
	}
	var roles []map[string]any
	for _, name := range slices.Sorted(mapKeys(cat.Roles)) {
		r := cat.Roles[name]
		if r.Insight != nil && r.Insight.MaxPerWeek != nil {
			roles = append(roles, map[string]any{"insight": map[string]any{"max_per_week": float64(*r.Insight.MaxPerWeek)}})
		}
	}
	budget := func(t string) float64 { return cat.TaskTypes[t].TreeBudgetEUR }
	need, ok := c.reviewNeedOf(mmap(tv.Raw, "spec"), tenantSpecOf(cat, "Governance", tenant), roles, budget)
	if !ok {
		return
	}
	var have int64
	if l := tv.Spec.Budgets.Lines["review"].MonthEUR; l != nil {
		have = money.FromEUR(*l)
	}
	short := max(need-have, 0)
	ch := DoctorCheck{ID: "budget.review_shortfall_eur_micros", OK: short == 0, Severity: "info", Value: short, Tenant: tenant}
	if short > 0 {
		ch.Severity = "fail"
	}
	add(ch)
}
