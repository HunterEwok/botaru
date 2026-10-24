// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"errors"
	"fmt"
	"slices"
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
// until ctx ends: the work listener, fastLoops in a goroutine of their own and the other loops.
// RecoverLeases runs before it.
func (c *Core) Run(ctx context.Context) {
	// The age of a loop that never ticked counts from here, by the store's clock (OBS-4); if
	// the store does not answer, the first indicators pass sets it.
	_ = c.tx(ctx, func(_ pgx.Tx, now time.Time) error {
		c.startedAt.CompareAndSwap(0, now.UnixNano())
		return nil
	})
	c.running.Store(true)
	go c.listen(ctx)
	go c.runFast(ctx)
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if c.storeDown.Load() {
			c.leaveStoreDown(ctx)
			continue
		}
		// Only the way out of not_ready clears storeDown: a pass that began before the fast
		// loops set it does not.
		if err := c.tickLoops(ctx, slowLoops); err != nil && ctx.Err() == nil {
			c.log.Warn("background loop failed", "err", err)
			// A loop that fails while the store answers is that loop's fault: its tick grows
			// old and health reports it, but P1 stays ready (OBS-8).
			if c.st.Ping(ctx) != nil && !c.storeDown.Swap(true) {
				c.log.Warn("the store is unavailable: P1 answers not_ready until every loop ticks again")
			}
		}
	}
}

// leaveStoreDown is the way out of not_ready, with the fast loops held: it first extends the
// leases the outage let run out, as at a start, so that no loop reaps them (FEN-5), then runs a
// pass of every loop, and clears storeDown once all of them ticked, or once the store answers and
// only a loop's own fault is left (OBS-8). Until then P1 stays not_ready.
func (c *Core) leaveStoreDown(ctx context.Context) {
	c.fastMu.Lock()
	defer c.fastMu.Unlock()
	if err := c.endLeasesOfDowntime(ctx); err != nil {
		return
	}
	err := c.Tick(ctx)
	if err != nil {
		if ctx.Err() != nil || c.st.Ping(ctx) != nil {
			return
		}
		c.log.Warn("background loop failed", "err", err)
	}
	if c.storeDown.Swap(false) {
		c.log.Info("the store answers and the loops ticked again: ready")
	}
}

// fastLoops end leases, attempts and tasks on time; Run ticks them in a goroutine of their own,
// so a slow pass of an analytic loop does not hold them up. slowLoops are the others.
var fastLoops = []string{"reaper", "deadline", "stall"}

var slowLoops = func() []string {
	var out []string
	for _, l := range loopOrder {
		if !slices.Contains(fastLoops, l) {
			out = append(out, l)
		}
	}
	return out
}()

// runFast ticks fastLoops every second until ctx ends. While the store is down it waits: Run
// extends the leases of the outage first (FEN-5).
func (c *Core) runFast(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if err := c.tickFast(ctx); err != nil && ctx.Err() == nil {
			c.log.Warn("background loop failed", "err", err)
			if c.st.Ping(ctx) != nil && !c.storeDown.Swap(true) {
				c.log.Warn("the store is unavailable: P1 answers not_ready until every loop ticks again")
			}
		}
	}
}

// tickFast runs one pass of fastLoops unless the store is down; the way out of not_ready holds
// it, so no pass reaps a lease before the leases of the outage are extended (FEN-5).
func (c *Core) tickFast(ctx context.Context) error {
	c.fastMu.Lock()
	defer c.fastMu.Unlock()
	if c.storeDown.Load() {
		return nil
	}
	return c.tickLoops(ctx, fastLoops)
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
	return c.tickLoops(ctx, loopOrder)
}

// tickLoops runs one pass of each of loops in order, as Tick does.
func (c *Core) tickLoops(ctx context.Context, loops []string) error {
	var errs []error
	if err := c.loadRestoring(ctx); err != nil {
		errs = append(errs, err)
	}
	for _, loop := range loops {
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
	skippedAny := false
	for {
		done := true
		var row string
		err := c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
			var l leaseRow
			var mark *string
			err := tx.QueryRow(ctx, `SELECT work_id, lease_no, kind, tenant, worker_id, epoch, state, lease_until, reason FROM leases
				WHERE state = 'active' AND lease_until <= $1 AND NOT work_id = ANY($2) ORDER BY lease_until LIMIT 1 FOR UPDATE SKIP LOCKED`,
				now, c.skipped.ids("reaper", time.Now())).
				Scan(&l.WorkID, &l.LeaseNo, &l.Kind, &l.Tenant, &l.WorkerID, &l.Epoch, &l.State, &l.Until, &mark)
			if err == pgx.ErrNoRows {
				return nil
			}
			if err != nil {
				return err
			}
			done, row = false, l.WorkID
			reason, spends := reason, spends
			if mark != nil && *mark == "core_unavailable" {
				reason, spends = "core_unavailable", false
			}
			// A lease of a draining worker that expires after its drain deadline is not
			// spent (LCY-4).
			if drained, err := drainedLease(ctx, tx, &l); err != nil {
				return err
			} else if drained {
				reason, spends = "worker_drain", false
			}
			if _, err := tx.Exec(ctx, `UPDATE leases SET state = 'expired', reason = $3, closed_at = $4 WHERE work_id = $1 AND lease_no = $2`,
				l.WorkID, l.LeaseNo, reason, now); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `DELETE FROM handles WHERE work_id = $1 AND lease_no = $2`, l.WorkID, l.LeaseNo); err != nil {
				return err
			}
			// The grant of an ended attempt is released, as closeLease does (MGR-18).
			if l.Kind == "attempt" {
				if err := grantEnded(ctx, tx, now, l.WorkID); err != nil {
					return err
				}
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
		if err != nil && c.skipRow(ctx, "reaper", row, err) {
			skippedAny = true
			continue
		}
		if err != nil {
			return err
		}
		if done {
			return c.skippedErr("reaper", skippedAny)
		}
	}
}

// deadlines moves tasks past their deadline to failed (DLV-1) and raises the alarm of tasks
// nobody took before start_by (DSP-8). A task that is being worked
// on loses its lease and handles too, so the worker is fenced at its next call (FEN-4).
func (c *Core) deadlines(ctx context.Context) error {
	skippedAny := false
	for {
		done := true
		var row string
		err := c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
			var id, tenant, initiator, state string
			var rec []byte
			err := tx.QueryRow(ctx, `SELECT id, tenant, initiator, state, record FROM tasks WHERE state IN ('submitted', 'working') AND deadline_at <= $1
				AND NOT id = ANY($2) ORDER BY deadline_at LIMIT 1 FOR UPDATE SKIP LOCKED`, now, c.skipped.ids("deadline", time.Now())).Scan(&id, &tenant, &initiator, &state, &rec)
			if err == pgx.ErrNoRows {
				return nil
			}
			if err != nil {
				return err
			}
			done, row = false, id
			if state == "working" {
				if _, err := tx.Exec(ctx, `DELETE FROM handles WHERE work_id = $1`, id); err != nil {
					return err
				}
				if _, err := tx.Exec(ctx, `UPDATE leases SET state = 'revoked', reason = 'deadline_exceeded', closed_at = $2
					WHERE work_id = $1 AND state = 'active'`, id, now); err != nil {
					return err
				}
				if err := grantEnded(ctx, tx, now, id); err != nil {
					return err
				}
			}
			if err := c.settleMessages(ctx, tx, now, id); err != nil {
				return err
			}
			return c.failTask(ctx, tx, now, tenant, id, initiator, rec, "deadline_exceeded")
		})
		if err != nil && c.skipRow(ctx, "deadline", row, err) {
			skippedAny = true
			continue
		}
		if err != nil {
			return err
		}
		if done {
			if err := c.toolcallDeadlines(ctx); err != nil {
				return err
			}
			if err := c.repeatPrincipalLeft(ctx); err != nil {
				return err
			}
			if err := c.unclaimed(ctx); err != nil {
				return err
			}
			return c.skippedErr("deadline", skippedAny)
		}
	}
}
