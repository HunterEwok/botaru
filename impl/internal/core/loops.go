// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// RecoverLeases extends the leases of the downtime (see endLeasesOfDowntime). It runs before
// the ports serve, so no holder is fenced before its lease is extended, and it retries until it
// succeeds or ctx ends.
func (c *Core) RecoverLeases(ctx context.Context) error {
	for pause := time.Second; ; pause = min(2*pause, 30*time.Second) {
		err := c.endLeasesOfDowntime(ctx)
		if err == nil {
			return nil
		}
		c.log.Warn("extending the leases of the downtime failed, retrying", "err", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pause):
		}
	}
}

// Run starts the background loops of the core (spec/ports/background-loops.md) and blocks
// until ctx ends: the work listener, reaper and deadline. RecoverLeases runs before it.
func (c *Core) Run(ctx context.Context) {
	// The age of a loop that never ticked counts from here, by the store's clock (OBS-4); if
	// the store does not answer, the first indicators pass sets it.
	_ = c.tx(ctx, func(_ pgx.Tx, now time.Time) error {
		c.startedAt.CompareAndSwap(0, now.UnixNano())
		return nil
	})
	c.running.Store(true)
	go c.listen(ctx)
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		err := c.Tick(ctx)
		switch {
		case err == nil:
			if c.storeDown.Swap(false) {
				c.log.Info("the store answers and every loop ticked again: ready")
			}
		case ctx.Err() != nil:
		default:
			c.log.Warn("background loop failed", "err", err)
			// A loop that fails while the store answers is that loop's fault: its tick grows
			// old and health reports it, but P1 stays ready (OBS-8).
			if c.st.Ping(ctx) != nil {
				if !c.storeDown.Swap(true) {
					c.log.Warn("the store is unavailable: P1 answers not_ready until every loop ticks again")
				}
			} else if c.storeDown.Swap(false) {
				c.log.Info("the store answers again: ready")
			}
		}
	}
}

// RunManual starts only the work listener: on a test bench that serves the test-control port
// the loops tick only by loop.tick, so a fixture decides the order of every tick. P2 health
// then reports the loop tick as old, which is expected on such a bench.
func (c *Core) RunManual(ctx context.Context) {
	c.manual.Store(true)
	c.listen(ctx)
}

// Tick runs one pass of every background loop of this build, in loopOrder. Each loop that
// finished its pass marks its tick in the store (OBS-4); a loop that failed does not, and the
// loops after it still run.
func (c *Core) Tick(ctx context.Context) error {
	var errs []error
	if err := c.loadRestoring(ctx); err != nil {
		errs = append(errs, err)
	}
	for _, loop := range loopOrder {
		if err := c.tickLoop(ctx, loop); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// tickLoop runs one pass of a loop and marks its tick.
func (c *Core) tickLoop(ctx context.Context, loop string) error {
	if err := testLoops[loop](c, ctx); err != nil {
		return fmt.Errorf("loop %s: %w", loop, err)
	}
	if err := c.markTick(ctx, loop); err != nil {
		return fmt.Errorf("loop %s: marking the tick: %w", loop, err)
	}
	return nil
}

// endLeasesOfDowntime gives every active lease a full lease period from readiness, so a holder
// that is still alive keeps its work: a lease granted before the downtime ends before readiness
// plus one period, so this covers every lease that overlaps the downtime. A lease that still
// carries the mark from an earlier start is not extended again, so another core copy starting
// later does not keep an abandoned lease alive; a renewal clears the mark. The mark core_unavailable stays on
// the lease until the holder renews it; one that is not renewed ends with core_unavailable,
// which does not spend the attempt (FEN-5).
func (c *Core) endLeasesOfDowntime(ctx context.Context) error {
	return c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		_, err := tx.Exec(ctx, `UPDATE leases SET lease_until = $1, reason = 'core_unavailable' WHERE state = 'active' AND lease_until < $1 AND reason IS DISTINCT FROM 'core_unavailable'`,
			now.Add(time.Duration(c.cfg.LeaseS)*time.Second))
		return err
	})
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
			var mark *string
			err := tx.QueryRow(ctx, `SELECT work_id, lease_no, kind, tenant, worker_id, epoch, state, lease_until, reason FROM leases
				WHERE state = 'active' AND lease_until <= $1 ORDER BY lease_until LIMIT 1 FOR UPDATE SKIP LOCKED`, now).
				Scan(&l.WorkID, &l.LeaseNo, &l.Kind, &l.Tenant, &l.WorkerID, &l.Epoch, &l.State, &l.Until, &mark)
			if err == pgx.ErrNoRows {
				return nil
			}
			if err != nil {
				return err
			}
			done = false
			reason, spends := reason, spends
			if mark != nil && *mark == "core_unavailable" {
				reason, spends = "core_unavailable", false
			}
			if _, err := tx.Exec(ctx, `UPDATE leases SET state = 'expired', reason = $3, closed_at = $4 WHERE work_id = $1 AND lease_no = $2`,
				l.WorkID, l.LeaseNo, reason, now); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `DELETE FROM handles WHERE work_id = $1 AND lease_no = $2`, l.WorkID, l.LeaseNo); err != nil {
				return err
			}
			switch l.Kind {
			case "attempt":
				_, err = c.endAttempt(ctx, tx, now, l.Tenant, &l, reason, spends, true, 0, "")
			case "delivery":
				_, err = c.endDelivery(ctx, tx, now, l.Tenant, &l, reason, true, 0)
			case "toolcall":
				err = c.reapToolcall(ctx, tx, now, &l, reason)
			case "erasure":
				err = c.endErasureStep(ctx, tx, now, &l, "", "", reason, true)
			}
			if err == nil && l.Kind == "attempt" {
				err = c.settleMessages(ctx, tx, now, l.WorkID)
			}
			return err
		})
		if err != nil || done {
			return err
		}
	}
}

// deadlines moves tasks past their deadline to failed (DLV-1) and raises the alarm of tasks
// nobody took before start_by (DSP-8). A task that is being worked
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
			if err := c.settleMessages(ctx, tx, now, id); err != nil {
				return err
			}
			return c.failTask(ctx, tx, now, tenant, id, initiator, rec, "deadline_exceeded")
		})
		if err != nil {
			return err
		}
		if done {
			if err := c.toolcallDeadlines(ctx); err != nil {
				return err
			}
			return c.unclaimed(ctx)
		}
	}
}
