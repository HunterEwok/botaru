// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// The tenant lifecycle (LCY-1, spec/lifecycles/tenant.md): active, suspended, offboarding,
// deleted. suspended is a hold of the tenant scope with the reason suspended; offboarding and
// deleted are rows of tenant_states, and a deleted row is the tombstone of the tenant id (RET-3).
// These are operator operations (PRM-14); the service tenant farm never leaves active.

// TenantLifecycleRequest is p2-tenant-lifecycle-request.
type TenantLifecycleRequest struct {
	Op           string `json:"op"`
	Tenant       string `json:"tenant"`
	ExportWindow string `json:"export_window"`
}

// tenantStopEffects are the effects of suspended and offboarding.
var tenantStopEffects = []string{"no_enqueue", "no_claim", "no_tool", "no_action", "no_delivery"}

// defaultExportWindow is the export window of tenant.offboard, also its ceiling; the window is
// at least a day, so an export queued at offboarding can still run before the deletion.
const (
	defaultExportWindow = 30 * 24 * time.Hour
	minExportWindow     = 24 * time.Hour
)

// tenantState is the state of a tenant past active, with its export window; "" for active.
func tenantState(ctx context.Context, tx pgx.Tx, tenant string) (state string, exportUntil time.Time, err error) {
	err = tx.QueryRow(ctx, `SELECT state, export_until FROM tenant_states WHERE tenant = $1`, tenant).Scan(&state, &exportUntil)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", time.Time{}, nil
	}
	return state, exportUntil, err
}

// TenantLifecycle is P2 tenant.suspend, tenant.offboard and tenant.delete (LCY-1).
func (c *Core) TenantLifecycle(ctx context.Context, op Operator, req TenantLifecycleRequest) (*Done, error) {
	if req.Tenant == farmTenant {
		return nil, refuse("forbidden", "the service tenant farm is never suspended, offboarded or deleted")
	}
	if op.Tenant != farmTenant && op.Tenant != req.Tenant {
		return nil, refuse("forbidden", "the operator credential is for another tenant")
	}
	window := defaultExportWindow
	if req.ExportWindow != "" {
		d, ok := parseDuration(req.ExportWindow)
		if !ok || req.Op != "tenant.offboard" {
			return nil, refuse("input_schema_invalid", "export_window is a duration, only with tenant.offboard")
		}
		if d > defaultExportWindow || d < minExportWindow {
			return nil, refuse("input_schema_invalid", "the export window is at least one day and at most 30 days")
		}
		window = d
	}
	tv, _, err := c.requireTenant(ctx, req.Tenant)
	if err != nil {
		return nil, err
	}
	var out Done
	err = c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		// One lifecycle operation of a tenant at a time.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(7300114, hashtext($1))`, req.Tenant); err != nil {
			return err
		}
		state, exportUntil, err := tenantState(ctx, tx, req.Tenant)
		if err != nil {
			return err
		}
		switch req.Op {
		case "tenant.suspend":
			if state != "" {
				return refuse("forbidden", "a tenant in %s is not suspended", state)
			}
			var hold string
			err := tx.QueryRow(ctx, `SELECT id FROM holds WHERE state = 'active' AND scope_kind = 'tenant' AND scope_tenant = $1 AND reason = 'suspended'
				AND (until IS NULL OR until > $2) LIMIT 1`, req.Tenant, now).Scan(&hold)
			if err == nil {
				out = Done{OK: true, ID: hold, State: "suspended"} // a repeat changes nothing (IDM-3)
				return nil
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			if hold, _, err = c.insertHold(ctx, tx, now, OperatorActor(op), HoldScope{Kind: "tenant", Tenant: req.Tenant}, tenantStopEffects, "suspended", nil); err != nil {
				return err
			}
			if err := c.cancelTenantApprovals(ctx, tx, now, req.Tenant); err != nil {
				return err
			}
			notified, err := c.tenantHeldNotice(ctx, tx, now, tv, hold, "suspended")
			if err != nil {
				return err
			}
			if err := c.appendEvent(ctx, tx, now, req.Tenant, Event{Type: "tenant.suspended.v1", Subject: req.Tenant, Data: map[string]any{"tenant": req.Tenant, "hold": hold}}); err != nil {
				return err
			}
			more := map[string]any{"hold": hold, "admins_notified": notified}
			if notified == 0 {
				// Nobody to tell in the farm: the audit record and the log say so (HLD-3).
				more["admins_notice"] = "no_recipient"
				c.log.Warn("tenant suspended: no admin:people holder has a private chat with a channel, so no notice was sent", "hold", hold)
			}
			seq, err := c.tenantAudit(ctx, tx, now, op, req.Tenant, req.Op, "active", "suspended", more)
			out = Done{OK: true, ID: hold, State: "suspended", AuditSeq: seq}
			return err
		case "tenant.offboard":
			steps := []string{"provider_key_revoke"}
			if state != "" {
				if state == "deleted" {
					return refuse("gone", "the tenant is deleted")
				}
				out = Done{OK: true, State: state, OperatorSteps: steps}
				return nil
			}
			hold, _, err := c.insertHold(ctx, tx, now, OperatorActor(op), HoldScope{Kind: "tenant", Tenant: req.Tenant}, tenantStopEffects, "offboarding", nil)
			if err != nil {
				return err
			}
			until := now.Add(window)
			if _, err := tx.Exec(ctx, `INSERT INTO tenant_states (tenant, state, hold, offboarded_at, export_until) VALUES ($1, 'offboarding', $2, $3, $4)`,
				req.Tenant, hold, now, until); err != nil {
				return err
			}
			// The credentials of the tenant's workers and channels stop at the next call; the
			// leases end, and with them the handles of the runtime and the model (SEC-3).
			rows, err := tx.Query(ctx, `SELECT id, state FROM workers WHERE tenant = $1 AND state <> 'revoked' ORDER BY id FOR UPDATE`, req.Tenant)
			if err != nil {
				return err
			}
			var ws [][2]string
			for rows.Next() {
				var w [2]string
				if err := rows.Scan(&w[0], &w[1]); err != nil {
					rows.Close()
					return err
				}
				ws = append(ws, w)
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}
			for _, w := range ws {
				if _, err := c.revokeWorker(ctx, tx, now, req.Tenant, w[0], w[1], op.Principal, "worker.revoke", "offboarding"); err != nil {
					return err
				}
			}
			if _, err := tx.Exec(ctx, `DELETE FROM driver_secrets WHERE tenant = $1`, req.Tenant); err != nil {
				return err
			}
			// The delivery keys of the tenant's source channels go with its channels (ING-3).
			if _, err := tx.Exec(ctx, `DELETE FROM ingress_deliveries WHERE tenant = $1`, req.Tenant); err != nil {
				return err
			}
			if err := c.cancelTenantApprovals(ctx, tx, now, req.Tenant); err != nil {
				return err
			}
			// The channel workers are revoked above and no channel enrolls while the tenant is
			// offboarding, so a notice to the admins could not go out: none is planned, and the
			// audit record and the log say so; the operator tells the admins outside the farm
			// (HLD-3).
			c.log.Warn("tenant offboarded: its channels are revoked, so the admins get no notice in the farm", "hold", hold)
			if err := c.appendEvent(ctx, tx, now, req.Tenant, Event{Type: "tenant.offboarding.v1", Subject: req.Tenant,
				Data: map[string]any{"tenant": req.Tenant, "hold": hold, "export_until": fmtTime(until)}}); err != nil {
				return err
			}
			seq, err := c.tenantAudit(ctx, tx, now, op, req.Tenant, req.Op, "active", "offboarding", map[string]any{"hold": hold, "export_until": fmtTime(until), "workers_revoked": len(ws), "admins_notified": 0, "admins_notice": "channels_revoked"})
			out = Done{OK: true, ID: hold, State: "offboarding", AuditSeq: seq, OperatorSteps: steps}
			return err
		case "tenant.delete":
			switch {
			case state == "deleted":
				out = Done{OK: true, State: "deleted"}
				return nil
			case state != "offboarding":
				return refuse("forbidden", "only an offboarded tenant is deleted")
			case now.Before(exportUntil):
				return refuse("export_window_open", "the export window ends at %s", fmtTime(exportUntil))
			}
			erasure, err := c.requestErasure(ctx, tx, now, tv, erasureSpec{Scope: ErasureScope{Tenant: req.Tenant}, Reason: "tenant_delete", Author: op.Principal})
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE tenant_states SET state = 'deleted', deleted_at = $2, erasure = $3 WHERE tenant = $1`, req.Tenant, now, erasure); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `DELETE FROM monitor_accounts WHERE tenant = $1`, req.Tenant); err != nil {
				return err
			}
			if err := c.appendEvent(ctx, tx, now, req.Tenant, Event{Type: "tenant.deleted.v1", Subject: req.Tenant, Data: map[string]any{"tenant": req.Tenant, "erasure": erasure}}); err != nil {
				return err
			}
			seq, err := c.tenantAudit(ctx, tx, now, op, req.Tenant, req.Op, "offboarding", "deleted", map[string]any{"erasure_request": erasure})
			out = Done{OK: true, ID: erasure, State: "deleted", AuditSeq: seq}
			return err
		}
		return refuse("input_schema_invalid", "unknown op %s", req.Op)
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// tenantAudit writes the audit record of a lifecycle transition in the tenant's chain.
func (c *Core) tenantAudit(ctx context.Context, tx pgx.Tx, now time.Time, op Operator, tenant, command, before, after string, more map[string]any) (int64, error) {
	a := map[string]any{"state": after}
	for k, v := range more {
		a[k] = v
	}
	return c.appendAudit(ctx, tx, now, Audit{
		Tenant: tenant, Kind: "command", Actor: Actor{Principal: op.Principal, Via: "farmctl"},
		Command: command, Target: &Target{Kind: "tenant", ID: tenant}, OK: true,
		Before: map[string]any{"state": before}, After: a,
	})
}

// cancelTenantApprovals cancels every pending approval of a tenant with the reason held, as a
// suspension or an offboarding does (LCY-1).
func (c *Core) cancelTenantApprovals(ctx context.Context, tx pgx.Tx, now time.Time, tenant string) error {
	rows, err := tx.Query(ctx, `SELECT `+approvalColumns+` FROM approvals WHERE tenant = $1 AND state IN ('pending', 'approved') ORDER BY id FOR UPDATE`, tenant)
	if err != nil {
		return err
	}
	var as []*approvalRow
	for rows.Next() {
		a, err := scanApproval(rows)
		if err != nil {
			rows.Close()
			return err
		}
		as = append(as, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, a := range as {
		if err := c.closeApproval(ctx, tx, now, a, "canceled", "held"); err != nil {
			return err
		}
	}
	return nil
}

// tenantResumed publishes tenant.resumed when the last suspension of a tenant ends (LCY-1).
func (c *Core) tenantResumed(ctx context.Context, tx pgx.Tx, now time.Time, h Hold) error {
	if h.Scope.Kind != "tenant" || h.Reason != "suspended" {
		return nil
	}
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM holds WHERE state = 'active' AND scope_kind = 'tenant' AND scope_tenant = $1 AND reason = 'suspended'
		AND id <> $2 AND (until IS NULL OR until > $3)`, h.Scope.Tenant, h.ID, now).Scan(&n); err != nil || n > 0 {
		return err
	}
	return c.appendEvent(ctx, tx, now, h.Scope.Tenant, Event{Type: "tenant.resumed.v1", Subject: h.Scope.Tenant, Data: map[string]any{"tenant": h.Scope.Tenant, "hold": h.ID}})
}

// tenantHeldNotice tells the tenant's admins (admin:people) about a suspension or an
// offboarding in their direct chats. The notice names its hold, and the delivery handoff lets
// it pass that tenant hold: deliveries are withheld except messages to admins about the hold
// itself (HLD-3).
func (c *Core) tenantHeldNotice(ctx context.Context, tx pgx.Tx, now time.Time, tv *TenantView, hold, state string) (int, error) {
	chats, err := c.adminChats(ctx, tx, now, tv, "", "admin:people")
	if err != nil {
		return 0, err
	}
	for _, a := range chats {
		if _, err := c.planDelivery(ctx, tx, now, plannedDelivery{
			Tenant: tv.Name, Kind: "notice", Addr: "admins", TargetKind: "chat",
			TargetID: Chat{Kind: "direct", ExternalID: a.ExternalID}.ID(), ExecKind: "channel", ExecName: a.Channel,
			Recipient: a.Principal, DataOrigin: "code",
			Body: map[string]any{"form": "text_code", "text_key": "notice.tenant_held", "about_hold": hold,
				"params": map[string]any{"tenant": tv.Name, "state": state, "hold": hold}},
		}); err != nil {
			return 0, err
		}
	}
	return len(chats), nil
}
