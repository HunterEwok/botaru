// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// CommissionStatus is P2 commission.status (COM-6, COM-7, DIG-4, DIG-5, GOV-14): the lenses of
// the tenant with the single current source of each executor, the seats of a lens, the usage of
// the review line over 30 days, since when the v0.2 governance rules hold, the audit sample
// over 180 days and the decision statistics over 90 days. Shadow review is not built, so every
// lens shows shadow off.
func (c *Core) CommissionStatus(ctx context.Context, op Operator, req TenantRequest) (map[string]any, error) {
	tenant, err := viewTenant(op, req.Tenant)
	if err != nil {
		return nil, err
	}
	cat, err := c.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	tv := cat.Tenants[tenant]
	if tv == nil {
		return nil, refuse("not_found", "no such tenant")
	}
	var out map[string]any
	err = c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		defs := c.lensDefs()
		lenses, exec := c.commissionLenses(cat, tenant, false, "")
		list := []any{}
		for _, l := range lenses {
			rows, err := tx.Query(ctx, `SELECT id, coalesce(holder, '') FROM seats WHERE tenant = $1 AND kind = 'commission' AND lens = $2 ORDER BY id`, tenant, l)
			if err != nil {
				return err
			}
			type seat struct{ id, holder string }
			var seats []seat
			for rows.Next() {
				var s seat
				if err := rows.Scan(&s.id, &s.holder); err != nil {
					rows.Close()
					return err
				}
				seats = append(seats, s)
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}
			entry := map[string]any{"lens": l, "veto": defs[l].Veto, "executor": "none", "fallback_role": nil, "shadow": "off"}
			if e, ok := exec[l]; ok {
				entry["executor"], entry["fallback_role"] = "fallback_role", e.Role
			}
			held, err := c.seatExecutor(ctx, tx, now, tenant, l)
			if err != nil {
				return err
			}
			if held != nil && seatRole(cat, tenant) != "" {
				entry["executor"] = "seats"
			}
			if len(seats) > 0 {
				out := []any{}
				for _, s := range seats {
					n, _ := seatNumber(s.id)
					m := map[string]any{"id": n, "holder": nil}
					if s.holder != "" {
						m["holder"] = s.holder
					}
					out = append(out, m)
				}
				entry["seats"] = out
			}
			list = append(list, entry)
		}
		line := c.budgetLine("review")
		used, err := windowUsed(ctx, tx, tenant, spendFilter{line: line}, now.Add(-30*24*time.Hour), "")
		if err != nil {
			return err
		}
		rl := map[string]any{"spent_eur": float64(used) / 1e6}
		if l, ok := tv.Spec.Budgets.Lines[line]; ok && l.MonthEUR != nil {
			rl["month_eur"] = *l.MonthEUR
		}
		sample, err := auditSampleStatus(ctx, tx, now, tenant)
		if err != nil {
			return err
		}
		stats, err := decisionStats(ctx, tx, now, tenant)
		if err != nil {
			return err
		}
		since, err := governanceSince(ctx, tx, tenant)
		if err != nil {
			return err
		}
		out = map[string]any{"tenant": tv.Name, "lenses": list, "review_line": rl, "audit_sample": sample, "decision_stats": stats, "governance_since": since}
		return nil
	})
	return out, err
}

// seatNumber is the number of a charter seat from its id "seat-<n>" or "<n>".
func seatNumber(id string) (int, bool) {
	n := 0
	digits := false
	for _, r := range id {
		if r >= '0' && r <= '9' {
			n, digits = n*10+int(r-'0'), true
		} else if digits {
			break
		}
	}
	return n, digits && n > 0
}
