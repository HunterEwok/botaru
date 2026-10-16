// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/spec"
)

// The views status.workers and status.campaigns (PRF-1, REP-8, STA-3). Like status.tasks they
// answer from the snapshot of the task: the workers of the tenant with the core observation and
// the last self-report side by side, and the campaigns of the window with their item counts.
// No free text of a worker or a campaign reaches the model: closed fields, ids and counts only.

const (
	statusWorkersName   = "status.workers"
	statusCampaignsName = "status.campaigns"
	// stateWorkerType is the self-report type the skeleton defines (bot-state.schema.json).
	stateWorkerType = "state.worker.v1"
)

// statusViewNames are the status.* views this build serves, in the order of the catalog.
var statusViewNames = []string{statusTasksName, statusProposalsName, statusWorkersName, statusCampaignsName}

// statusViewDescriptions are the descriptions of the views in the P3 catalog.
var statusViewDescriptions = map[string]string{
	statusTasksName:     "Tasks of the tenant in the report window: type, title, state and reason codes only; rows above your class only count in hidden.",
	statusProposalsName: "Proposals of the tenant from recommended on: kind, state and category only; rows above your class only count in hidden.",
	statusWorkersName: "Workers of the tenant: what the core observes (presence, lifecycle, holds, leases, last seen) and the last self-reported phase " +
		"side by side with a mismatch code; the core observation prevails.",
	statusCampaignsName: "Campaigns of the tenant in the report window: kind, type, state and item counts only; rows above your class only count in hidden.",
}

// snapshotWorkers adds the workers of the tenant to a snapshot (STA-3). A worker carries no
// data mark, so a reader of the audience sees them when every principal of it reads anything.
func (c *Core) snapshotWorkers(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, tenant string, f *viewFilter, snap *viewSnapshot) error {
	snap.Workers = []map[string]any{}
	if !f.readableMark("") {
		return nil
	}
	rows, err := tx.Query(ctx, `SELECT w.id, w.role, w.state, w.last_seen, w.stale_since IS NOT NULL, w.plugins,
		(SELECT count(*) FROM leases l WHERE l.worker_id = w.id AND l.state = 'active'),
		(SELECT max(coalesce(l.closed_at, l.created_at)) FROM leases l WHERE l.worker_id = w.id),
		(SELECT max(l.created_at) FROM leases l WHERE l.worker_id = w.id)
		FROM workers w WHERE w.tenant = $1 AND w.state <> 'revoked' ORDER BY w.id LIMIT $2`, tenant, maxStatusRows+1)
	if err != nil {
		return err
	}
	type wrow struct {
		id, role, state     string
		seen                *time.Time
		stale               bool
		plugins             []string
		leases              int
		lastLease, lastTake *time.Time
	}
	var ws []wrow
	for rows.Next() {
		var r wrow
		var plugins []byte
		if err := rows.Scan(&r.id, &r.role, &r.state, &r.seen, &r.stale, &plugins, &r.leases, &r.lastLease, &r.lastTake); err != nil {
			rows.Close()
			return err
		}
		_ = json.Unmarshal(plugins, &r.plugins)
		ws = append(ws, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(ws) > maxStatusRows {
		snap.WorkersMore = len(ws) - maxStatusRows
		ws = ws[:maxStatusRows]
	}
	staleAfterState := stateStaleAfter(cat.MessageTypes[stateWorkerType])
	for _, r := range ws {
		presence := "connected"
		switch {
		case r.seen == nil:
			presence = "gone"
		case r.stale || now.Sub(*r.seen) > staleAfter:
			presence = "stale"
		}
		observed := map[string]any{"presence": presence, "lifecycle": r.state, "leases": r.leases}
		if r.seen != nil {
			observed["last_seen_age_s"] = max(int64(now.Sub(*r.seen)/time.Second), 0)
		}
		holds, err := workerHolds(ctx, tx, now, tenant, r.id, r.role)
		if err != nil {
			return err
		}
		if len(holds) > 0 {
			observed["holds"] = holds
		}
		rep, err := lastSelfReport(ctx, tx, tenant, r.id)
		if err != nil {
			return err
		}
		row := map[string]any{"worker": strings.ToLower(r.id), "role": r.role, "observed": observed, "reported": nil, "mismatch": nil}
		// The self-report column and the mismatch code are of STA-3 (v0.2, deferred from the
		// claim): before the claim reaches v0.2 the row has the core observation only.
		if rep != nil && claimsV02() {
			row["reported"] = map[string]any{"state": rep.phase, "source": "state_message", "at": fmtTime(rep.at)}
			waiting, err := workWaits(ctx, tx, now, cat, tenant, r.role)
			if err != nil {
				return err
			}
			if code := mismatchCode(now, presence, r.leases, r.lastLease, r.lastTake, r.plugins, rep, staleAfterState, waiting); code != "" {
				row["mismatch"] = code
			}
		}
		snap.Workers = append(snap.Workers, row)
	}
	return nil
}

// selfReport is the last state.worker.v1 value of a worker: only the phase, the plugin names
// and the time are read (STA-3, INJ-1).
type selfReport struct {
	phase   string
	plugins []string
	at      time.Time
}

func lastSelfReport(ctx context.Context, tx pgx.Tx, tenant, worker string) (*selfReport, error) {
	var fields []byte
	var at time.Time
	err := tx.QueryRow(ctx, `SELECT envelope->'data'->'fields', created_at FROM messages WHERE tenant = $1 AND from_worker = $2 AND type = $3
		ORDER BY seq DESC LIMIT 1`, tenant, worker, stateWorkerType).Scan(&fields, &at)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var f struct {
		Phase   string `json:"phase"`
		Plugins []struct {
			Name string `json:"name"`
		} `json:"plugins"`
	}
	_ = json.Unmarshal(fields, &f)
	switch f.Phase {
	case "idle", "busy", "degraded", "draining", "maintenance":
	default:
		return nil, nil
	}
	r := &selfReport{phase: f.Phase, at: at}
	for _, p := range f.Plugins {
		r.plugins = append(r.plugins, p.Name)
	}
	if f.Plugins == nil {
		r.plugins = nil
	} else if r.plugins == nil {
		r.plugins = []string{}
	}
	return r, nil
}

// workerHolds are the ids of the active holds on the worker or its role.
func workerHolds(ctx context.Context, tx pgx.Tx, now time.Time, tenant, worker, role string) ([]string, error) {
	rows, err := tx.Query(ctx, `SELECT id FROM holds WHERE state = 'active' AND (until IS NULL OR until > $4) AND scope_tenant = $1
		AND (scope_kind = 'worker' AND scope_name = $2 OR scope_kind = 'role' AND scope_name = $3) ORDER BY id LIMIT 50`,
		tenant, strings.ToLower(worker), role, now)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// workWaits says whether a task of a type the role claims has waited unclaimed for longer than
// two long-poll periods of claim.
func workWaits(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, tenant, role string) (bool, error) {
	claims := cat.Roles[role].Claims
	if len(claims) == 0 {
		return false, nil
	}
	var waits bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM tasks WHERE tenant = $1 AND state = 'submitted' AND type = ANY($2) AND created_at < $3)`,
		tenant, claims, now.Add(-staleAfter)).Scan(&waits)
	return waits, err
}

// mismatchCode is the code of state-mismatch.yaml for a worker, the first of the table whose
// condition holds, or "" (STA-3). The core observation prevails: the code only informs.
func mismatchCode(now time.Time, presence string, leases int, lastLease, lastTake *time.Time, plugins []string, rep *selfReport, stateStale time.Duration, waiting bool) string {
	switch {
	case (rep.phase == "idle" || rep.phase == "busy") && presence != "connected":
		return "reported_ready_unreachable"
	case rep.phase == "idle" && presence == "connected" && waiting && (lastTake == nil || now.Sub(*lastTake) > staleAfter):
		return "reported_idle_not_claiming"
	case rep.phase == "busy" && leases == 0 && (lastLease == nil || now.Sub(*lastLease) > stateStale):
		return "reported_busy_no_lease"
	case rep.phase == "draining" && lastTake != nil && lastTake.After(rep.at):
		return "reported_draining_still_claiming"
	case rep.plugins != nil && !samePlugins(rep.plugins, plugins):
		return "reported_plugins_differ"
	case now.Sub(rep.at) > stateStale:
		return "state_stale"
	}
	return ""
}

func samePlugins(a, b []string) bool {
	x, y := slices.Clone(a), slices.Clone(b)
	slices.Sort(x)
	slices.Sort(y)
	return slices.Equal(slices.Compact(x), slices.Compact(y))
}

// snapshotCampaigns adds the campaigns of the window to a snapshot: a campaign changed in the
// window or still live, with the scope of a campaign report only that campaign and with the
// scope of a root only the campaigns of that tree. A campaign
// every principal of the audience may not read (its item type and mark) is left out
// uncounted; one above the mark counts only in hidden (REP-7, PRM-9).
func (c *Core) snapshotCampaigns(ctx context.Context, tx pgx.Tx, cat *Catalog, tenant, only, root string, from time.Time, f *viewFilter, snap *viewSnapshot) error {
	snap.Campaigns = []map[string]any{}
	rows, err := tx.Query(ctx, `SELECT id, state, coalesce(pause_reason, ''), coalesce(stop_reason, ''), record, updated_at FROM campaigns
		WHERE tenant = $1 AND (updated_at >= $2 OR state IN ('proposed', 'running', 'paused', 'stopping')) AND ($3 = '' OR id = $3)
		AND ($4 = '' OR record->>'root' = $4)
		ORDER BY updated_at DESC, id LIMIT 1000`, tenant, from, only, root)
	if err != nil {
		return err
	}
	type crow struct {
		id, state, pause, stop string
		rec                    campaignRec
		at                     time.Time
	}
	var cs []crow
	for rows.Next() {
		var r crow
		var raw []byte
		if err := rows.Scan(&r.id, &r.state, &r.pause, &r.stop, &raw, &r.at); err != nil {
			rows.Close()
			return err
		}
		if err := json.Unmarshal(raw, &r.rec); err != nil {
			rows.Close()
			return err
		}
		cs = append(cs, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, r := range cs {
		typ := r.rec.itemType()
		if !f.readable(typ, r.rec.DataMark) {
			continue
		}
		if r.rec.DataMark != "" && !c.markWithin(r.rec.DataMark, f.mark) {
			snap.CampaignsHidden++
			continue
		}
		if len(snap.Campaigns) >= maxStatusRows {
			snap.CampaignsMore++
			continue
		}
		n, err := campaignCount(ctx, tx, r.id)
		if err != nil {
			return err
		}
		title := cat.TaskTypes[typ].Title
		if a := r.rec.action(); a != "" {
			title = cat.Actions[a].Title
		}
		if title == "" {
			title = typ
		}
		row := map[string]any{"campaign": r.id, "kind": r.rec.kind(), "type": typ, "title": cut(title, 200), "state": r.state,
			"items": n.total(), "counts": n.asMap(), "updated_at": fmtTime(r.at)}
		if r.pause != "" {
			row["pause_reason"] = r.pause
		}
		if r.stop != "" {
			row["stop_reason"] = r.stop
		}
		snap.Campaigns = append(snap.Campaigns, row)
	}
	return nil
}

// claimsV02 says whether the effective claim reaches v0.2, for v0.2 behaviour deferred from
// the claim (STA-3).
func claimsV02() bool { return !spec.VersionLater("v0.2", spec.Effective()) }

// snapshotViews are the status views of the role of the task's latest attempt.
func snapshotViews(ctx context.Context, tx pgx.Tx, cat *Catalog, task string) ([]string, error) {
	var role string
	err := tx.QueryRow(ctx, `SELECT w.role FROM leases l JOIN workers w ON w.id = l.worker_id WHERE l.work_id = $1 ORDER BY l.lease_no DESC LIMIT 1`, task).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return cat.Roles[role].Views, nil
}
