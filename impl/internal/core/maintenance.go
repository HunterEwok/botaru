// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
)

// Maintenance windows of the monitoring system (MON-9): the adapter polls the maintenance
// status of the catalog nodes and sends ext.monitor.maintenance.v1 on every poll while a node
// is in maintenance; the core holds the farm's own work on that machine for the window.

const (
	// maintenanceReason and maintenanceSource mark the hold of a window (MON-9).
	maintenanceReason = "maintenance"
	maintenanceSource = "system:monitor"
	// maxMaintenanceUntil bounds the until of one event: two poll intervals of at most 300
	// seconds each, so an adapter cannot hold a machine for long with one poll.
	maxMaintenanceUntil = 600 * time.Second
	// maintenanceKeep is how long the row of an ended window is kept.
	maintenanceKeep = 30 * 24 * time.Hour
)

type maintenanceEvent struct {
	MaintenanceID string `json:"maintenanceid"`
	Resource      string `json:"resource"`
	State         string `json:"state"`
	Until         string `json:"until,omitempty"`
}

// maintenanceSignal takes one poll result of a node (MON-9). On the first active event of a
// window it sets a hold of scope host on the catalog host whose monitor_host is the node, with
// no_tool and pause_action (no_action for a host with maintenance_hold: true), reason
// maintenance, author contain_auto, source system:monitor and the event's until; each later
// active event of the window moves that until forward, never back. An ended event changes no
// hold, but a later active event of the same window id is a new period: it extends the hold
// of the last period while that still runs, and sets a new one otherwise. A gap in the polls
// alone changes no hold and starts no period. After a person released the hold, the window
// sets no new hold until an ended event closes that period. A node of the farm
// itself, or one no catalog host of the tenant binds, sets nothing.
func (c *Core) maintenanceSignal(ctx context.Context, w *Worker, cat *Catalog, data map[string]any) (*ActivityResult, error) {
	if err := c.spec.Validate("ext-monitor-maintenance", data); err != nil {
		return nil, refuse("input_schema_invalid", "the maintenance event is outside its schema")
	}
	var ev maintenanceEvent
	raw, _ := json.Marshal(data)
	if err := json.Unmarshal(raw, &ev); err != nil {
		return nil, refuse("input_schema_invalid", "the maintenance event does not decode")
	}
	host, ok := monitorHosts(cat, w.Tenant)[ev.Resource]
	if !ok || slices.Contains(c.cfg.FarmHosts, host.Node) || slices.Contains(c.cfg.FarmHosts, host.ID) {
		return &ActivityResult{Result: "ignored", Reason: "not_subject"}, nil
	}
	var until time.Time
	if ev.State == "active" {
		t, err := time.Parse(time.RFC3339Nano, ev.Until)
		if err != nil {
			return nil, refuse("input_schema_invalid", "an active maintenance event has until")
		}
		until = t
	}
	out := ActivityResult{Result: "ignored"}
	err := c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		if err := c.appendEvent(ctx, tx, now, w.Tenant, Event{Type: "ext.monitor.maintenance.v1", Subject: host.ID, Data: data}); err != nil {
			return err
		}
		var holdID string
		var released bool
		var endedAt, periodUntil *time.Time
		err := tx.QueryRow(ctx, `SELECT hold, released, ended_at, period_until FROM maintenance_windows WHERE node = $1 AND maintenance_id = $2 FOR UPDATE`,
			host.Node, ev.MaintenanceID).Scan(&holdID, &released, &endedAt, &periodUntil)
		found := err == nil
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		// Windows that ended long ago are forgotten.
		if _, err := tx.Exec(ctx, `DELETE FROM maintenance_windows WHERE ended_at < $1`, now.Add(-maintenanceKeep)); err != nil {
			return err
		}
		if ev.State == "ended" {
			// The period is over: the hold ends at its until; a later active event of the same
			// id (a periodic window) is a new period with a new hold (MON-9).
			_, err := tx.Exec(ctx, `UPDATE maintenance_windows SET ended_at = $3 WHERE node = $1 AND maintenance_id = $2`, host.Node, ev.MaintenanceID, now)
			return err
		}
		if until.After(now.Add(maxMaintenanceUntil)) {
			until = now.Add(maxMaintenanceUntil)
		}
		if !until.After(now) {
			return nil
		}
		// extend moves the until of an active hold of the window forward, never back: the
		// only way a hold's deadline grows (HLD-2).
		extend := func(h *holdRow) error {
			if !until.After(*h.until) {
				return nil
			}
			if _, err := tx.Exec(ctx, `UPDATE holds SET until = $2 WHERE id = $1`, h.ID, until); err != nil {
				return err
			}
			_, err := c.appendAudit(ctx, tx, now, Audit{
				Tenant: h.Tenant, Kind: "command", Actor: Actor{Principal: "system", Via: "system"},
				Command: "hold.extend", Target: &Target{Kind: "hold", ID: h.ID}, OK: true,
				After: map[string]any{"until": fmtTime(until), "source": maintenanceSource},
			})
			return err
		}
		if found {
			h, err := loadHold(ctx, tx, holdID, true)
			if err != nil {
				return err
			}
			live := h.State == "active" && h.until != nil && h.until.After(now)
			if endedAt == nil {
				// The same period: only an ended event ends it, so a gap in the polls alone
				// changes no hold and keeps a person's release (MON-9).
				if _, err := tx.Exec(ctx, `UPDATE maintenance_windows SET period_until = greatest(period_until, $3) WHERE node = $1 AND maintenance_id = $2`,
					host.Node, ev.MaintenanceID, until); err != nil {
					return err
				}
				if released {
					return nil
				}
				if h.State == "released" {
					// A person released it: no new hold while this period stays active.
					_, err := tx.Exec(ctx, `UPDATE maintenance_windows SET released = true WHERE node = $1 AND maintenance_id = $2`, host.Node, ev.MaintenanceID)
					return err
				}
				if live {
					out = ActivityResult{Result: "accepted"}
					return extend(h)
				}
				// The hold ran out by its until inside the period: a new hold for the rest.
			} else if live {
				// A new period while the hold of the last one still runs: that hold is
				// extended instead of a second one beside it.
				if _, err := tx.Exec(ctx, `UPDATE maintenance_windows SET ended_at = NULL, released = false, period_until = $3 WHERE node = $1 AND maintenance_id = $2`,
					host.Node, ev.MaintenanceID, until); err != nil {
					return err
				}
				out = ActivityResult{Result: "accepted"}
				return extend(h)
			}
		}
		effects := []string{"no_tool", "pause_action"}
		if host.MaintenanceHold {
			effects = []string{"no_tool", "no_action"}
		}
		actor := HoldActor{Principal: "system", Rank: "contain_auto", Tenant: w.Tenant, Via: "system", Source: maintenanceSource}
		id, _, err := c.insertHold(ctx, tx, now, actor, HoldScope{Kind: "host", Tenant: w.Tenant, Name: host.ID}, effects, maintenanceReason, &until)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO maintenance_windows (node, maintenance_id, tenant, hold, created_at, period_until) VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (node, maintenance_id) DO UPDATE SET hold = $4, created_at = $5, ended_at = NULL, released = false, period_until = $6`,
			host.Node, ev.MaintenanceID, w.Tenant, id, now, until); err != nil {
			return err
		}
		out = ActivityResult{Result: "accepted"}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// scheduleHostHeld says whether a slot of a schedule needs a machine under a maintenance hold:
// a value of its task params names a catalog host, or a catalog resource on a host, with an
// active hold of the window (MON-9). Such a slot is skipped with held although the hold has no
// no_enqueue (TIM-3).
func (c *Core) scheduleHostHeld(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, tenant string, params map[string]any) (bool, error) {
	if len(params) == 0 {
		return false, nil
	}
	spec, _ := tenantCatalog(cat, tenant)
	hostOf := map[string]string{}
	hosts, _ := spec["hosts"].([]any)
	for _, h := range hosts {
		hm, _ := h.(map[string]any)
		if id := asString(hm["id"]); id != "" {
			hostOf[id] = id
		}
	}
	resources, _ := spec["resources"].([]any)
	for _, r := range resources {
		rm, _ := r.(map[string]any)
		if id, h := asString(rm["id"]), asString(rm["host"]); id != "" && h != "" {
			if _, isHost := hostOf[id]; !isHost {
				hostOf[id] = h
			}
		}
	}
	var named []string
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case string:
			if h, ok := hostOf[x]; ok && !slices.Contains(named, h) {
				named = append(named, h)
			}
		case []any:
			for _, e := range x {
				walk(e)
			}
		case map[string]any:
			for _, e := range x {
				walk(e)
			}
		}
	}
	walk(params)
	for _, h := range named {
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM holds WHERE state = 'active' AND (until IS NULL OR until > $1) AND scope_kind = 'host'
			AND scope_tenant = $2 AND scope_name = $3 AND reason = $4 AND source = $5`, now, tenant, h, maintenanceReason, maintenanceSource).Scan(&n); err != nil {
			return false, err
		}
		if n > 0 {
			return true, nil
		}
	}
	return false, nil
}
