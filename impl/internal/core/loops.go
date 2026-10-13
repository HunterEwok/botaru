// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// Run starts the background loops of the core (spec/ports/background-loops.md) and blocks
// until ctx ends: the work listener, reaper and deadline.
func (c *Core) Run(ctx context.Context) {
	if err := c.endLeasesOfDowntime(ctx); err != nil {
		c.log.Error("ending the leases of the downtime failed", "err", err)
	}
	go c.listen(ctx)
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if err := c.Tick(ctx); err != nil && ctx.Err() == nil {
			c.log.Warn("background loop failed", "err", err)
		}
	}
}

// Tick runs one pass of the reaper and deadline loops. The test-control port calls it
// directly (loop.tick).
func (c *Core) Tick(ctx context.Context) error {
	if err := c.reap(ctx); err != nil {
		return err
	}
	if err := c.deadlines(ctx); err != nil {
		return err
	}
	markTick()
	return nil
}

// endLeasesOfDowntime ends the leases that expired while the core was not running with
// core_unavailable, which does not spend the attempt (FEN-5).
func (c *Core) endLeasesOfDowntime(ctx context.Context) error {
	return c.reapWith(ctx, "core_unavailable", false)
}

// reap ends expired leases: an attempt gets lease_expired and spends the attempt (FEN-1, FEN-3);
// a delivery returns to the queue or dies by the attempt ceiling.
func (c *Core) reap(ctx context.Context) error {
	return c.reapWith(ctx, "lease_expired", true)
}

func (c *Core) reapWith(ctx context.Context, reason string, spends bool) error {
	for {
		done := true
		err := c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
			var l leaseRow
			err := tx.QueryRow(ctx, `SELECT work_id, lease_no, kind, tenant, worker_id, epoch, state, lease_until FROM leases
				WHERE state = 'active' AND lease_until <= $1 ORDER BY lease_until LIMIT 1 FOR UPDATE SKIP LOCKED`, now).
				Scan(&l.WorkID, &l.LeaseNo, &l.Kind, &l.Tenant, &l.WorkerID, &l.Epoch, &l.State, &l.Until)
			if err == pgx.ErrNoRows {
				return nil
			}
			if err != nil {
				return err
			}
			done = false
			if _, err := tx.Exec(ctx, `UPDATE leases SET state = 'expired', reason = $3, closed_at = $4 WHERE work_id = $1 AND lease_no = $2`,
				l.WorkID, l.LeaseNo, reason, now); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `DELETE FROM handles WHERE work_id = $1 AND lease_no = $2`, l.WorkID, l.LeaseNo); err != nil {
				return err
			}
			switch l.Kind {
			case "attempt":
				_, err = c.endAttempt(ctx, tx, now, l.Tenant, &l, reason, spends, true, 0)
			case "delivery":
				_, err = c.endDelivery(ctx, tx, now, l.Tenant, &l, reason, true, 0)
			}
			return err
		})
		if err != nil || done {
			return err
		}
	}
}

// deadlines moves tasks past their deadline to failed (DLV-1). A task that is being worked
// on loses its lease and handles too, so the worker is fenced at its next call (FEN-4).
func (c *Core) deadlines(ctx context.Context) error {
	for {
		done := true
		err := c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
			var id, tenant, initiator, state string
			var rec []byte
			err := tx.QueryRow(ctx, `SELECT id, tenant, initiator, state, record FROM tasks WHERE state IN ('submitted', 'working') AND deadline_at <= $1
				ORDER BY deadline_at LIMIT 1 FOR UPDATE SKIP LOCKED`, now).Scan(&id, &tenant, &initiator, &state, &rec)
			if err == pgx.ErrNoRows {
				return nil
			}
			if err != nil {
				return err
			}
			done = false
			if state == "working" {
				if _, err := tx.Exec(ctx, `DELETE FROM handles WHERE work_id = $1`, id); err != nil {
					return err
				}
				if _, err := tx.Exec(ctx, `UPDATE leases SET state = 'revoked', reason = 'deadline_exceeded', closed_at = $2
					WHERE work_id = $1 AND state = 'active'`, id, now); err != nil {
					return err
				}
			}
			return c.failTask(ctx, tx, now, tenant, id, initiator, rec, "deadline_exceeded")
		})
		if err != nil || done {
			return err
		}
	}
}
