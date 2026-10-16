// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// WorkerDrain is P2 worker.drain (LCY-4, HLD-2): a hold of the worker scope with no_claim moves
// the worker to draining, so it gets no new work and finishes what runs. From the deadline the
// heartbeat answer asks for release, and a lease that expires after it is not spent.
func (c *Core) WorkerDrain(ctx context.Context, op Operator, req WorkerRequest) (*Done, error) {
	var deadline *time.Time
	if req.Deadline != "" {
		t, err := time.Parse(time.RFC3339Nano, req.Deadline)
		if err != nil {
			return nil, refuse("input_schema_invalid", "deadline")
		}
		deadline = &t
	}
	var out Done
	err := c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		var tenant, state string
		err := tx.QueryRow(ctx, `SELECT tenant, state FROM workers WHERE id = $1 FOR UPDATE`, req.Worker).Scan(&tenant, &state)
		if errors.Is(err, pgx.ErrNoRows) || err == nil && tenant != op.Tenant {
			return refuse("not_found", "no such worker in the tenant")
		}
		if err != nil {
			return err
		}
		if state != "active" && state != "draining" {
			return refuse("forbidden", "a %s worker is not drained", state)
		}
		hold, _, err := c.insertHold(ctx, tx, now, OperatorActor(op), HoldScope{Kind: "worker", Tenant: tenant, Name: strings.ToLower(req.Worker)},
			[]string{"no_claim"}, "worker_drain", nil)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE workers SET state = 'draining', drain_deadline = $2 WHERE id = $1`, req.Worker, deadline); err != nil {
			return err
		}
		after := map[string]any{"state": "draining", "hold": hold, "deadline": nil}
		if deadline != nil {
			after["deadline"] = fmtTime(*deadline)
		}
		seq, err := c.appendAudit(ctx, tx, now, Audit{
			Tenant: tenant, Kind: "command", Actor: Actor{Principal: op.Principal, Via: "farmctl"},
			Command: "worker.drain", Target: &Target{Kind: "worker", ID: req.Worker}, OK: true,
			Before: map[string]any{"state": state}, After: after,
		})
		out = Done{OK: true, ID: req.Worker, State: "draining", AuditSeq: seq}
		return err
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// drainPassed says whether a worker is draining and its drain deadline has come (LCY-4).
func drainPassed(ctx context.Context, tx pgx.Tx, now time.Time, worker string) (bool, error) {
	var passed bool
	err := tx.QueryRow(ctx, `SELECT coalesce(state = 'draining' AND drain_deadline <= $2, false) FROM workers WHERE id = $1`, worker, now).Scan(&passed)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return passed, err
}

// drainedLease says whether an expired attempt lease belongs to a draining worker and expired
// after the worker's drain deadline: such a lease ends with worker_drain without spending the
// attempt (LCY-4). The other kinds of work keep their own expiry rules.
func drainedLease(ctx context.Context, tx pgx.Tx, l *leaseRow) (bool, error) {
	if l.Kind != "attempt" || l.WorkerID == "" {
		return false, nil
	}
	var drained bool
	err := tx.QueryRow(ctx, `SELECT coalesce(state = 'draining' AND drain_deadline < $2, false) FROM workers WHERE id = $1`, l.WorkerID, l.Until).Scan(&drained)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return drained, err
}

// purgeRuntimeStore records, at the revoke of a worker whose runtime declared
// session: runtime_internal or memory: runtime_internal, an erasure request with the worker as
// its scope and the reason worker_revoke for the runtime's profile store (LCY-4, RT-1, RT-2).
func (c *Core) purgeRuntimeStore(ctx context.Context, tx pgx.Tx, now time.Time, tenant, worker, author string) error {
	var internal bool
	if err := tx.QueryRow(ctx, `SELECT `+runtimeInternal+` FROM workers WHERE id = $1`, worker).Scan(&internal); err != nil {
		return err
	}
	if !internal {
		return nil
	}
	cat, err := c.Catalog(ctx)
	if err != nil {
		return err
	}
	tv := cat.Tenants[tenant]
	if tv == nil {
		tv = &TenantView{Name: tenant}
	}
	_, err = c.requestErasure(ctx, tx, now, tv, erasureSpec{Scope: ErasureScope{Tenant: tenant, Worker: worker}, Reason: "worker_revoke", Author: author})
	return err
}

// purgeRetiredRuntime enqueues, in the retirement transaction of an identity whose worker's
// runtime declared session or memory runtime_internal at hello, an erasure request with reason
// bot_retired and the worker as scope: the core cannot seal what the runtime keeps itself, so
// the runtime deletes it with the E4 purge (MEM-9, RET-4, RET-7). An unbound identity or a
// revoked worker has nothing here: revoking the worker already cleared its runtime store
// (worker_revoke, LCY-4).
func (c *Core) purgeRetiredRuntime(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, b *botRow) error {
	if b.Worker == "" || b.State == "unbound" {
		return nil
	}
	var internal bool
	err := tx.QueryRow(ctx, `SELECT `+runtimeInternal+` FROM workers WHERE id = $1 AND state <> 'revoked'`, b.Worker).Scan(&internal)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && !internal {
		return nil
	}
	if err != nil {
		return err
	}
	tv := cat.Tenants[b.Tenant]
	if tv == nil {
		tv = &TenantView{Name: b.Tenant}
	}
	_, err = c.requestErasure(ctx, tx, now, tv, erasureSpec{Scope: ErasureScope{Tenant: b.Tenant, Worker: b.Worker}, Reason: "bot_retired"})
	return err
}

// runtimeStepOutcome closes the runtime step of a worker_revoke request. A revoked worker
// takes no more work, so the core cannot send its runtime the purge: a worker that never ran
// an attempt holds nothing of the tenant (not_applicable with the basis tenant_absent), one
// that did fails the step with not_ready, and the request stays open with its alarm (RET-7)
// until the operator clears the runtime's store by hand.
func runtimeStepOutcome(ctx context.Context, tx pgx.Tx, worker string) (state, basis, reason string, err error) {
	var ran bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM leases WHERE worker_id = $1 AND kind = 'attempt')`, worker).Scan(&ran); err != nil {
		return "", "", "", err
	}
	if ran {
		return "failed", "", "not_ready", nil
	}
	return "not_applicable", "tenant_absent", "", nil
}

// runtimeInternal is the condition on a workers row that its runtime declared session or
// memory runtime_internal at hello (RT-1, RT-2, RT-5).
const runtimeInternal = `coalesce(hello->'runtime'->>'session' = 'runtime_internal' OR hello->'runtime'->>'memory' = 'runtime_internal', false)`
