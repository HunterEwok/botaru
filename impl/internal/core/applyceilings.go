// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"fmt"
	"slices"

	"github.com/HunterEwok/botaru/impl/internal/manifest"
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

// weeksPerMonthTenths is 4.4 weeks in a month, as tenths (BUD-13).
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

// reviewPrice is the price of the reviews of one proposal: the tree_budget_eur of the review
// type of each lens that has an executor. COM-6 reserves it in the review line when a proposal
// moves to review, and BUD-13 multiplies it, so both compute one number.
func reviewPrice(cat *Catalog, lenses []string, exec map[string]lensExecutor) int64 {
	var sum int64
	for _, l := range lenses {
		if e, ok := exec[l]; ok {
			sum += money.FromEUR(cat.TaskTypes[e.Type].TreeBudgetEUR)
		}
	}
	return sum
}

// proposalReviewPrice is the review price COM-6 reserves for a proposal of the tenant, from the
// manifests alone: the commission lenses (the veto lenses and the charter seat lenses) with a
// fallback role, or whose seat role may review them, and, from v0.2, the farm_manager lens when
// the tenant works with the manager and the seat role claims its type. A tenant whose veto
// lenses have no executor sends no reviews (COM-7): its price is 0.
func (c *Core) proposalReviewPrice(cat *Catalog, tenant string) int64 {
	lenses, exec := c.commissionLenses(cat, tenant, false, "")
	defs := c.lensDefs()
	sr := seatRole(cat, tenant)
	for _, l := range lenses {
		if _, ok := exec[l]; !ok && sr != "" && c.independentReviewer(cat, sr, defs[l].ReviewType, "") {
			exec[l] = lensExecutor{Lens: l, Type: defs[l].ReviewType, Role: sr, Source: "seat"}
		}
	}
	if !c.vetoCovered(lenses, exec) {
		return 0
	}
	if managerEnabled(cat, tenant) && sr != "" &&
		slices.Contains(cat.Roles[sr].Claims, managerLensType) && cat.TaskTypes[managerLensType].Purpose == "review" {
		lenses = append(lenses, managerLens)
		exec[managerLens] = lensExecutor{Lens: managerLens, Type: managerLensType, Role: sr, Source: "seat"}
	}
	return reviewPrice(cat, lenses, exec)
}

// reviewNeedOf gives the need of BUD-13 of a tenant and whether the formula applies: the
// tenant's insight or aggregate line is above 0.
func (c *Core) reviewNeedOf(cat *Catalog, tenant string) (int64, bool) {
	tv := cat.Tenants[tenant]
	if tv == nil {
		return 0, false
	}
	lines := tv.Spec.Budgets.Lines
	above := func(l string) bool { m := lines[l].MonthEUR; return m != nil && *m > 0 }
	if !above("insight") && !above("aggregate") {
		return 0, false
	}
	var weeks []int
	for _, name := range slices.Sorted(mapKeys(cat.Roles)) {
		if r := cat.Roles[name]; r.Insight != nil && r.Insight.MaxPerWeek != nil {
			weeks = append(weeks, *r.Insight.MaxPerWeek)
		}
	}
	return reviewNeed(weeks, c.proposalReviewPrice(cat, tenant)), true
}

// reviewShort is the missing amount of the review line of a tenant by BUD-13, and the need.
func (c *Core) reviewShort(cat *Catalog, tenant string) (short, need int64, ok bool) {
	need, ok = c.reviewNeedOf(cat, tenant)
	if !ok {
		return 0, 0, false
	}
	var have int64
	if l := cat.Tenants[tenant].Spec.Budgets.Lines["review"].MonthEUR; l != nil {
		have = money.FromEUR(*l)
	}
	return max(need-have, 0), need, true
}

// lintReviewLine rejects a tenant whose review line is below the need of BUD-13
// (review_line_too_small, with the missing amount), on the catalog of the set being applied.
func (c *Core) lintReviewLine(docs []manifest.Doc, S *lintSet, f *lintFindings) {
	cat, err := buildCatalog(docs)
	if err != nil {
		return // the decode error is a finding of its own
	}
	for _, o := range S.each("Tenant") {
		if o.name == farmTenant {
			continue
		}
		if short, need, ok := c.reviewShort(cat, o.name); ok && short > 0 {
			f.addDetail(o, "review_line_too_small", "/budgets/lines/review/month_eur",
				fmt.Sprintf("the review line needs %s EUR a month, %s EUR more (BUD-13)", eurCents(need), eurCents(short)))
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
	short, _, ok := c.reviewShort(cat, tenant)
	if !ok {
		return
	}
	ch := DoctorCheck{ID: "budget.review_shortfall_eur_micros", OK: short == 0, Severity: "info", Value: short, Tenant: tenant}
	if short > 0 {
		ch.Severity = "fail"
	}
	add(ch)
}
