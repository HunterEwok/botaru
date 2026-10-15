// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/store"
)

// The operations of the test-control port T (spec/ports/test-control.md). They exist only on a
// test bench (APR-9): the HTTP binding serves them on a separate listener with the test bench
// key, and every one of them refuses on a core without the test bench flag as well. They change
// only time and order, never records of tasks, decisions, principals or manifests, and each
// writes an audit record of the service principal into the farm chain (AUD-1).

// TestClock is the answer of every test-control operation: the store clock after the action.
type TestClock struct {
	Now string `json:"now"`
}

// TestLoops are the background loops of spec/ports/background-loops.md that this build runs, with
// their tick. A loop of the list that this build does not have is refused with not_found.
var testLoops = map[string]func(c *Core, ctx context.Context) error{
	"reaper":          (*Core).reap,
	"deadline":        (*Core).deadlines,
	"stall":           (*Core).stalls,
	"hold":            (*Core).holdLoop,
	"worker-liveness": (*Core).liveness,
	"alarm":           (*Core).alarmLoop,
	"monitor":         (*Core).monitorLoop,
	"audit-head":      (*Core).auditHeadLoop,
	"health":          (*Core).healthLoop,
	"approval-expiry": (*Core).approvalLoop,
	"erasure":         (*Core).erasureLoop,
	"schedule":        (*Core).scheduleLoop,
	"budget":          (*Core).budgetLoop,
	"retention":       (*Core).retentionLoop,
	"observe":         (*Core).observe,
	"proposal":        (*Core).proposalLoop,
}

// loopOrder is the order of one Tick: leases and deadlines first, then holds, then the loops
// that raise and repeat alarms about what the others left, and the health indicators last.
var loopOrder = []string{"reaper", "deadline", "stall", "hold", "schedule", "approval-expiry", "erasure", "budget", "retention", "observe", "proposal", "worker-liveness", "alarm", "monitor", "audit-head", "health"}

func (c *Core) requireTestBench() error {
	if !c.cfg.TestBench {
		return refuse("forbidden", "the test-control port exists only on a test bench")
	}
	return nil
}

// TestClockSet moves the store clock to at; the clock only goes forward (TIM-1).
func (c *Core) TestClockSet(ctx context.Context, at time.Time) (*TestClock, error) {
	return c.testClock(ctx, "test/clock.set", func(now time.Time) (time.Duration, error) {
		if at.Before(now) {
			return 0, refuse("input_schema_invalid", "the store clock only moves forward")
		}
		return at.Sub(now), nil
	})
}

// TestClockAdvance moves the store clock forward by seconds.
func (c *Core) TestClockAdvance(ctx context.Context, byS int64) (*TestClock, error) {
	return c.testClock(ctx, "test/clock.advance", func(time.Time) (time.Duration, error) {
		if byS < 0 {
			return 0, refuse("input_schema_invalid", "the store clock only moves forward")
		}
		return time.Duration(byS) * time.Second, nil
	})
}

func (c *Core) testClock(ctx context.Context, command string, shift func(now time.Time) (time.Duration, error)) (*TestClock, error) {
	if err := c.requireTestBench(); err != nil {
		return nil, err
	}
	var out TestClock
	err := c.st.Tx(ctx, func(tx pgx.Tx) error {
		// One shift at a time: the lock comes before the clock is read, so two concurrent
		// calls cannot both start from the same clock.
		if _, err := tx.Exec(ctx, `SELECT 1 FROM farm_meta WHERE id = 1 FOR UPDATE`); err != nil {
			return err
		}
		now, err := store.Now(ctx, tx)
		if err != nil {
			return err
		}
		d, err := shift(now)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE farm_meta SET clock_offset_us = clock_offset_us + $1 WHERE id = 1`, d.Microseconds()); err != nil {
			return err
		}
		now = now.Add(d)
		out.Now = fmtTime(now)
		_, err = c.appendAudit(ctx, tx, now, Audit{
			Tenant: "farm", Kind: "command", Actor: Actor{Principal: "system", Via: "system"},
			Command: command, OK: true, After: map[string]any{"shift_us": d.Microseconds()},
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// TestLoopTick runs one tick of a background loop by name; copies above 1 run that many ticks
// at the same time, to check the loop's lock.
func (c *Core) TestLoopTick(ctx context.Context, loop string, copies int) (*TestClock, error) {
	if err := c.requireTestBench(); err != nil {
		return nil, err
	}
	if _, ok := testLoops[loop]; !ok {
		return nil, refuse("not_found", "this build has no loop %s", loop)
	}
	if copies < 1 {
		copies = 1
	}
	// The request schema allows at most 4 copies; the core holds the same bound.
	if copies > 4 {
		return nil, refuse("input_schema_invalid", "at most 4 copies")
	}
	errs := make(chan error, copies)
	for range copies {
		go func() { errs <- c.tickLoop(ctx, loop) }()
	}
	var first error
	for range copies {
		if err := <-errs; err != nil && first == nil {
			first = err
		}
	}
	if first != nil {
		return nil, first
	}
	var out TestClock
	err := c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		out.Now = fmtTime(now)
		_, err := c.appendAudit(ctx, tx, now, Audit{
			Tenant: "farm", Kind: "command", Actor: Actor{Principal: "system", Via: "system"},
			Command: "test/loop.tick", Target: &Target{Kind: "loop", ID: loop}, OK: true,
			After: map[string]any{"copies": copies},
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}
