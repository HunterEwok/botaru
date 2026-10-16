// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/manifest"
	"github.com/HunterEwok/botaru/impl/internal/toolspec"
)

// The wait of a single approval on a busy resource (ORC-12). An action with exclusive:
// resource has at most one released write call per tenant resource, counting campaign items
// and single approvals. An approved approval whose resource has one waits in approved with the
// reason resource_busy instead of failing; the approval loop executes the waiting ones in the
// order of their decision once the resource is free, with every check of the execution run
// again (the initiator, the arguments against the catalog, the holds of the write call).

// exclusiveLock is the advisory lock of one tenant resource of an exclusive action: the check
// and the release of a write call on it take turns (ORC-12).
const exclusiveLock = 7300161

// resourceBusy says whether a tenant resource has a released write call of an exclusive
// action's tool, a queued or leased one of a campaign item or a single approval, after taking
// the resource lock for this transaction.
func resourceBusy(ctx context.Context, tx pgx.Tx, tenant string, act manifest.Action, resource string) (bool, error) {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1, hashtext($2 || '/' || $3))`, exclusiveLock, tenant, resource); err != nil {
		return false, err
	}
	var busy bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM toolcalls WHERE tenant = $1 AND access = 'write' AND state IN ('queued', 'leased')
		AND tool = $2 AND (target_host = $3 OR target_resource = $3))`, tenant, toolspec.RightName(act.Executor.Plugin, act.Executor.Tool), resource).Scan(&busy)
	return busy, err
}

// approvalResource is the tenant resource the write call of an approval acts on: the catalog
// resource of the target, else its machine; "" when the tool has no fixed target.
func (c *Core) approvalResource(cat *Catalog, a *approvalRow, act manifest.Action) string {
	pl := cat.Plugins[act.Executor.Plugin]
	t, ok := pl.Tool(act.Executor.Tool)
	if !ok {
		return ""
	}
	params := map[string]map[string]any{}
	props, _ := act.Args["properties"].(map[string]any)
	for k, v := range props {
		if m, ok := v.(map[string]any); ok {
			params[k] = m
		}
	}
	target, _, err := c.writeTarget(cat, a.Tenant, t, params, a.Args)
	if err != nil {
		return ""
	}
	if target.resource != "" {
		return target.resource
	}
	return target.host
}

// approvalWaits puts an approval of an exclusive action whose resource is busy into the wait:
// state approved with the reason resource_busy, its cards updated. It reports whether the
// approval waits; the caller then creates no task and no write call.
func (c *Core) approvalWaits(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, a *approvalRow, act manifest.Action) (bool, error) {
	if act.Exclusive != "resource" {
		return false, nil
	}
	resource := c.approvalResource(cat, a, act)
	if resource == "" {
		return false, nil
	}
	busy, err := resourceBusy(ctx, tx, a.Tenant, act, resource)
	if err != nil {
		return false, err
	}
	// A freshly decided approval does not overtake older ones waiting for the same resource.
	if !busy && a.State == "pending" {
		if busy, err = c.olderWaiting(ctx, tx, cat, a, act, resource); err != nil {
			return false, err
		}
	}
	if !busy {
		return false, nil
	}
	if a.State == "approved" {
		return true, nil
	}
	if _, err := tx.Exec(ctx, `UPDATE approvals SET state = 'approved', reason = 'resource_busy', updated_at = $2 WHERE id = $1 AND state = 'pending'`, a.ID, now); err != nil {
		return false, err
	}
	a.State, a.Reason = "approved", "resource_busy"
	if err := c.appendEvent(ctx, tx, now, a.Tenant, Event{Type: "approval.approved", Subject: a.ID,
		Data: map[string]any{"approval": a.ID, "action": a.Action, "reason": "resource_busy"}}); err != nil {
		return false, err
	}
	if err := c.planCardUpdates(ctx, tx, now, a); err != nil {
		return false, err
	}
	return true, c.scriptApprovalMoved(ctx, tx, now, a)
}

// releaseWaitingApprovals executes the approvals that wait for their resource, in the order of
// their decision, each in its own transaction (ORC-12). One whose resource is still busy keeps
// waiting.
func (c *Core) releaseWaitingApprovals(ctx context.Context, cat *Catalog) error {
	var list []struct{ id, tenant string }
	if err := c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		list = list[:0]
		rows, err := tx.Query(ctx, `SELECT id, tenant FROM approvals WHERE state = 'approved' AND reason = 'resource_busy'
			ORDER BY decided_at NULLS LAST, id LIMIT 500`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var x struct{ id, tenant string }
			if err := rows.Scan(&x.id, &x.tenant); err != nil {
				return err
			}
			list = append(list, x)
		}
		return rows.Err()
	}); err != nil {
		return err
	}
	for _, x := range list {
		if err := c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
			a, err := loadApproval(ctx, tx, x.tenant, x.id, true)
			if err != nil {
				return err
			}
			tv := cat.Tenants[a.Tenant]
			if a.State != "approved" || a.Reason != "resource_busy" || tv == nil {
				return nil
			}
			// The holds of the approval stand while it waits (HLD-3, HLD-8): no_action cancels
			// it, pause_action keeps it waiting.
			scopes, err := c.approvalHoldScopes(ctx, tx, cat, a)
			if err != nil {
				return err
			}
			if held, err := heldBy(ctx, tx, now, []string{"no_action"}, scopes...); err != nil {
				return err
			} else if held != "" {
				return c.closeApproval(ctx, tx, now, a, "canceled", "held")
			}
			if held, err := heldBy(ctx, tx, now, []string{"pause_action"}, scopes...); err != nil || held != "" {
				return err
			}
			return c.executeApproval(ctx, tx, now, cat, tv, a)
		}); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// One bad row does not stop the others (log and go on).
			c.log.Error("releasing a waiting approval failed", "tenant", x.tenant, "approval", x.id, "error", err)
		}
	}
	return nil
}

// olderWaiting says whether another approval of the same tool already waits for the resource.
func (c *Core) olderWaiting(ctx context.Context, tx pgx.Tx, cat *Catalog, a *approvalRow, act manifest.Action, resource string) (bool, error) {
	rows, err := tx.Query(ctx, `SELECT `+approvalColumns+` FROM approvals WHERE tenant = $1 AND id <> $2 AND state = 'approved' AND reason = 'resource_busy'
		ORDER BY decided_at NULLS LAST, id LIMIT 500`, a.Tenant, a.ID)
	if err != nil {
		return false, err
	}
	others, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (*approvalRow, error) { return scanApproval(r) })
	if err != nil {
		return false, err
	}
	for _, o := range others {
		oa, ok := c.actionOf(cat, o.Action)
		if ok && oa.Executor.Plugin == act.Executor.Plugin && oa.Executor.Tool == act.Executor.Tool && c.approvalResource(cat, o, oa) == resource {
			return true, nil
		}
	}
	return false, nil
}
