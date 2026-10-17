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
						// From v0.3 people see the accuracy of every holder in its lens, with
						// the number of scored and missed forecasts (COM-17, GOV-36).
						a, err := lensAccuracy(ctx, tx, now, tenant, l, s.holder)
						if err != nil {
							return err
						}
						m["accuracy"], m["scored_forecasts"], m["missed_forecasts"] = a.view(), a.N, a.Missed
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
		acc, err := c.accountabilityStatus(ctx, tx, cat, tenant)
		if err != nil {
			return err
		}
		for k, v := range acc {
			out[k] = v
		}
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

// accountabilityStatus is the v0.3 part of commission.status (GOV-36, GOV-42): since when the
// v0.3 rules hold, the mode with accountability_on_since and the forecasts needed, the confirmed
// links and the departures that happened or wait.
func (c *Core) accountabilityStatus(ctx context.Context, tx pgx.Tx, cat *Catalog, tenant string) (map[string]any, error) {
	since, on, err := v03State(ctx, tx, tenant)
	if err != nil {
		return nil, err
	}
	var links, waiting, done int
	if err := tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM attributions WHERE tenant = $1 AND kind = 'rejected_proposal' AND state = 'confirmed'),
		(SELECT count(*) FROM link_departures WHERE tenant = $1 AND state = 'waiting'), (SELECT count(*) FROM link_departures WHERE tenant = $1 AND state = 'done')`,
		tenant).Scan(&links, &waiting, &done); err != nil {
		return nil, err
	}
	ts := func(t *time.Time) any {
		if t == nil {
			return nil
		}
		return fmtTime(*t)
	}
	return map[string]any{"governance_v03_since": ts(since), "accountability": map[string]any{
		"mode": accountabilityMode(cat, tenant), "accountability_on_since": ts(on), "min_forecasts": minForecasts(cat, tenant),
		"rejection_links_confirmed": links, "departures_waiting": waiting, "departures_done": done}}, nil
}
