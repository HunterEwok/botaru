// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/money"
)

// P2 budget.set only lowers a limit for a while and always with an expiry; the limits
// themselves live only in the tenant manifest (MAN-5). It is an operator operation (PRM-14).
// The line names a tenant window, month or day, or a budget line of the purposes registry,
// whose month limit it lowers.

// BudgetSetRequest is p2-budget-set-request.
type BudgetSetRequest struct {
	Tenant string `json:"tenant"`
	Line   string `json:"line"`
	Limit  int64  `json:"limit"`
	Until  string `json:"until"`
}

// budgetLines are the budget lines of the purposes registry, with work.
func (c *Core) budgetLines() map[string]bool {
	out := map[string]bool{"work": true}
	if reg, err := c.spec.Registry("spec/model/task-purposes.yaml"); err == nil {
		for _, e := range reg.Entries {
			if l, _ := e["budget_line"].(string); l != "" {
				out[l] = true
			}
		}
	}
	return out
}

// gitLimit is the limit of a budget.set line in the manifest, in euros, or nil without one.
func gitLimit(tv *TenantView, line string) *float64 {
	b := tv.Spec.Budgets
	switch line {
	case "month":
		return b.MonthEUR
	case "day":
		if b.DayEUR == nil && b.MonthEUR != nil {
			v := *b.MonthEUR / 20
			return &v
		}
		return b.DayEUR
	}
	if l := b.Lines[line].MonthEUR; l != nil || line == "work" {
		return l
	}
	// A line other than work without money is 0 (BUD-7).
	zero := 0.0
	return &zero
}

// BudgetSet runs budget.set.
func (c *Core) BudgetSet(ctx context.Context, op Operator, req BudgetSetRequest) (*Done, error) {
	if req.Tenant != op.Tenant && op.Tenant != farmTenant {
		return nil, refuse("forbidden", "the operator credential is for another tenant")
	}
	tv, _, err := c.requireTenant(ctx, req.Tenant)
	if err != nil {
		return nil, err
	}
	if req.Line != "month" && req.Line != "day" && !c.budgetLines()[req.Line] {
		return nil, refuse("not_found", "no such limit or budget line")
	}
	until, err := time.Parse(time.RFC3339Nano, req.Until)
	if err != nil {
		return nil, refuse("input_schema_invalid", "until")
	}
	// Only a narrowing: the new limit is not above the one recorded in git (MAN-5).
	if g := gitLimit(tv, req.Line); g != nil && req.Limit*10_000 > money.FromEUR(*g) {
		return nil, refuse("forbidden", "budget.set only lowers the limit recorded in git")
	}
	var out Done
	err = c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		if !until.After(now) {
			return refuse("input_schema_invalid", "until is in the past")
		}
		if _, err := tx.Exec(ctx, `INSERT INTO budget_overrides (tenant, line, limit_cents, until, set_by, created_at) VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (tenant, line) DO UPDATE SET limit_cents = EXCLUDED.limit_cents, until = EXCLUDED.until, set_by = EXCLUDED.set_by, created_at = EXCLUDED.created_at`,
			req.Tenant, req.Line, req.Limit, until, op.Principal, now); err != nil {
			return err
		}
		seq, err := c.appendAudit(ctx, tx, now, Audit{
			Tenant: req.Tenant, Kind: "command", Actor: Actor{Principal: op.Principal, Via: "farmctl"},
			Command: "budget.set", Target: &Target{Kind: "budget", ID: req.Line}, OK: true,
			After: map[string]any{"limit_cents": req.Limit, "until": fmtTime(until)},
		})
		out = Done{OK: true, AuditSeq: seq}
		return err
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// budgetOverrides are the live lower limits of a tenant by line, in euros (MAN-5).
func budgetOverrides(ctx context.Context, tx pgx.Tx, now time.Time, tenant string) (map[string]float64, error) {
	rows, err := tx.Query(ctx, `SELECT line, limit_cents FROM budget_overrides WHERE tenant = $1 AND until > $2`, tenant, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]float64{}
	for rows.Next() {
		var line string
		var cents int64
		if err := rows.Scan(&line, &cents); err != nil {
			return nil, err
		}
		out[line] = float64(cents) / 100
	}
	return out, rows.Err()
}

// lowered is the lower of a limit and its override; an override narrows an unlimited window too.
func lowered(limit *float64, ov map[string]float64, line string) *float64 {
	v, ok := ov[line]
	if !ok || limit != nil && *limit <= v {
		return limit
	}
	return &v
}
