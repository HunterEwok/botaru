// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// loopReason is one reason of the closed list of INS-6 why a tenant's improvement cycle is off;
// Role is empty for a reason of the whole tenant.
type loopReason struct {
	Role   string `json:"role,omitempty"`
	Reason string `json:"reason"`
}

// loopOff gives the reasons the improvement cycle of a tenant is disabled (INS-6): no role
// with analysis, an insight line of 0 for a role, a review line of 0, no present_to address,
// no active person with proposal.decide. The veto lens executors come with the commission.
func loopOff(ctx context.Context, q pgx.Tx, cat *Catalog, tenant string) ([]loopReason, error) {
	tv := cat.Tenants[tenant]
	if tv == nil {
		return nil, nil
	}
	var out []loopReason
	lines := tv.Spec.Budgets.Lines
	zero := func(line string, perRole bool) bool {
		l, ok := lines[line]
		if !ok || l.MonthEUR == nil || *l.MonthEUR <= 0 {
			return true
		}
		return perRole && l.PerRoleMonthEUR != nil && *l.PerRoleMonthEUR <= 0
	}
	roles := 0
	for _, name := range slices.Sorted(mapKeys(cat.Roles)) {
		// A role counts for the tenant when the tenant's system may start one of its analyses.
		ins := cat.Roles[name].Insight
		if ins == nil || !slices.ContainsFunc(ins.Types, func(t string) bool {
			return hasCap(tv, groupClosure(tv, []string{"system-ops"}), "task:"+t)
		}) {
			continue
		}
		roles++
		if zero("insight", true) {
			out = append(out, loopReason{Role: name, Reason: "insight_line_zero"})
		}
	}
	if roles == 0 {
		out = append(out, loopReason{Reason: "no_insight_role"})
	}
	if zero("review", false) {
		out = append(out, loopReason{Reason: "review_line_zero"})
	}
	if tv.Spec.Insights.PresentTo == nil || tv.Spec.Insights.PresentTo == "" {
		out = append(out, loopReason{Reason: "no_present_to"})
	}
	var deciders int
	if err := q.QueryRow(ctx, `SELECT count(DISTINCT p.id) FROM principals p JOIN memberships m ON m.tenant = p.tenant AND m.principal = p.id
		WHERE p.tenant = $1 AND p.state = 'active' AND p.kind = 'human' AND m.state = 'active' AND (m.until IS NULL OR m.until > botaru_now()) AND m.grp = ANY($2)`,
		tenant, decideGroups(tv)).Scan(&deciders); err != nil {
		return nil, err
	}
	if deciders == 0 {
		out = append(out, loopReason{Reason: "no_decider"})
	}
	return out, nil
}

// decideGroups are the groups whose closure holds proposal.decide for some category (DIG-3).
func decideGroups(tv *TenantView) []string {
	out := []string{}
	for _, g := range slices.Sorted(mapKeys(tv.Groups)) {
		if slices.ContainsFunc(groupClosure(tv, []string{g}), func(gg string) bool {
			return slices.ContainsFunc(tv.Groups[gg].Caps, func(c string) bool {
				return c == "proposal.decide" || strings.HasPrefix(c, "proposal.decide:")
			})
		}) {
			out = append(out, g)
		}
	}
	return out
}

// doctorInsights shows the improvement cycle state of each tenant in scope (INS-6).
func (c *Core) doctorInsights(ctx context.Context, tx pgx.Tx, _ time.Time, cat *Catalog, tenant string, add func(DoctorCheck)) error {
	for _, t := range slices.Sorted(mapKeys(cat.Tenants)) {
		if tenant != "farm" && t != tenant {
			continue
		}
		reasons, err := loopOff(ctx, tx, cat, t)
		if err != nil {
			return err
		}
		// The value is the code list, one reason per item, a role after a colon.
		codes := make([]string, 0, len(reasons))
		for _, r := range reasons {
			if r.Role != "" {
				codes = append(codes, r.Reason+":"+r.Role)
			} else {
				codes = append(codes, r.Reason)
			}
		}
		add(DoctorCheck{ID: "insights.loop_disabled", Tenant: t, OK: len(reasons) == 0, Severity: sevOf(len(reasons) == 0, "warn"), Value: strings.Join(codes, ",")})
	}
	return nil
}
