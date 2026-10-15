// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/store"
)

// CoreDrainRequest is p2-core-drain-request.
type CoreDrainRequest struct {
	Op    string `json:"op"`
	Until string `json:"until"`
}

// CoreDrain prepares a core rollout (LCY-6): until the next core start, claim answers
// not_ready with retry_after_s and P2 health answers draining with the seconds left to the
// deadline; running attempts keep their leases and finish. The mark lives in the store, so every
// core copy drains; the next start clears it. Only an operator of farm drains the core.
func (c *Core) CoreDrain(ctx context.Context, op Operator, req CoreDrainRequest) (*Done, error) {
	if op.Tenant != "farm" {
		return nil, refuse("forbidden", "only an operator of farm drains the core")
	}
	d := 30 * time.Minute
	if req.Until != "" {
		var ok bool
		if d, ok = parseDuration(req.Until); !ok || d <= 0 {
			return nil, refuse("input_schema_invalid", "until is not a duration")
		}
	}
	err := c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		until := now.Add(d)
		if _, err := tx.Exec(ctx, `UPDATE farm_meta SET drain_until = $1 WHERE id = 1`, until); err != nil {
			return err
		}
		_, err := c.appendAudit(ctx, tx, now, Audit{
			Tenant: "farm", Kind: "command", Actor: Actor{Principal: op.Principal, Via: "farmctl"},
			Command: "core.drain", Target: &Target{Kind: "core", ID: "core"}, OK: true,
			After: map[string]any{"drain_until": fmtTime(until)},
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	return &Done{OK: true}, nil
}

// ClearDrain ends a drain at the start of a core (LCY-6): the new core hands out work again.
func (c *Core) ClearDrain(ctx context.Context) error {
	_, err := c.st.Pool.Exec(ctx, `UPDATE farm_meta SET drain_until = NULL WHERE id = 1 AND drain_until IS NOT NULL`)
	return err
}

// drainUntil is the deadline of a drain in progress, or nil.
func drainUntil(ctx context.Context, q store.Querier) (*time.Time, error) {
	var t *time.Time
	err := q.QueryRow(ctx, `SELECT drain_until FROM farm_meta WHERE id = 1`).Scan(&t)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	return t, err
}
