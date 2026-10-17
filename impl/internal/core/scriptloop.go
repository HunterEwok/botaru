// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/manifest"
)

// The end of script runs: cancellation, the ends of their tasks, campaigns and approvals, purge and the script loop.

// cancelRun ends a running or paused run before its end (SCR-11): in one transaction the
// unreleased and waiting steps are canceled with their approvals and gates, the tasks of
// released steps get a cancellation, their campaigns stop, and released write calls are not
// recalled. A deadline makes a running run failed and a paused one canceled.
func (c *Core) cancelRun(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, r *scriptRun, reason string) error {
	if r.State == "proposed" {
		return c.rejectRun(ctx, tx, now, r, reason)
	}
	if r.State != "running" && r.State != "paused" {
		return nil
	}
	steps, err := loadSteps(ctx, tx, r.ID)
	if err != nil {
		return err
	}
	state := "canceled"
	if reason == "deadline_exceeded" && r.State == "running" {
		state = "failed"
	}
	if _, err := tx.Exec(ctx, `UPDATE script_runs SET state = $2, reason = $3, updated_at = $4 WHERE id = $1`, r.ID, state, reason, now); err != nil {
		return err
	}
	r.State = state
	if _, err := tx.Exec(ctx, `UPDATE script_steps SET state = 'canceled', reason = $2, ended_at = $3 WHERE run = $1 AND state IN ('pending', 'waiting', 'released')`,
		r.ID, reason, now); err != nil {
		return err
	}
	var tasks []string
	for _, s := range steps {
		switch s.State {
		case "waiting":
			if s.Approval != "" {
				a, err := loadApproval(ctx, tx, r.Tenant, s.Approval, true)
				if err != nil {
					return err
				}
				// One that waits for its resource has not run yet either (ORC-12).
				if a.State == "pending" || a.State == "approved" {
					if err := c.closeApproval(ctx, tx, now, a, "canceled", reason); err != nil {
						return err
					}
				}
			}
			if s.Campaign != "" {
				cmp, err := loadCampaign(ctx, tx, r.Tenant, s.Campaign, true)
				if err != nil {
					return err
				}
				// An action fanout an approver already started runs while its step waits.
				if cmp.State == "proposed" {
					if err := c.rejectCampaign(ctx, tx, now, cmp, reason); err != nil {
						return err
					}
				} else if err := c.stopCampaign(ctx, tx, now, cat, cmp, reason); err != nil {
					return err
				}
			}
		case "released":
			if s.Task != "" && s.Kind == "task" {
				tasks = append(tasks, s.Task)
			}
			if s.Campaign != "" {
				cmp, err := loadCampaign(ctx, tx, r.Tenant, s.Campaign, true)
				if err != nil {
					return err
				}
				if err := c.stopCampaign(ctx, tx, now, cat, cmp, reason); err != nil {
					return err
				}
			}
		}
	}
	if len(tasks) > 0 {
		if err := c.cancelTaskIDs(ctx, tx, now, r.Tenant, tasks, reason); err != nil {
			return err
		}
	}
	r.State = "running"
	return c.closeRun(ctx, tx, now, r, state, reason)
}

// scriptTaskEnded closes the released step of a task that reached a terminal state, in the
// transaction of that state: ok with the closed result fields, unknown for an action whose
// outcome is unknown, failed otherwise (SCR-8, SCR-11).
func (c *Core) scriptTaskEnded(ctx context.Context, tx pgx.Tx, now time.Time, task, state, reason string) error {
	var runID, tenant string
	var n int
	err := tx.QueryRow(ctx, `SELECT s.run, r.tenant, s.n FROM script_steps s JOIN script_runs r ON r.id = s.run WHERE s.task = $1 AND s.state = 'released'`, task).Scan(&runID, &tenant, &n)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	r, err := loadRun(ctx, tx, tenant, runID, true)
	if err != nil {
		return err
	}
	steps, err := loadSteps(ctx, tx, runID)
	if err != nil {
		return err
	}
	s := steps[n-1]
	var typ string
	var rec, result []byte
	if err := tx.QueryRow(ctx, `SELECT type, record, result FROM tasks WHERE id = $1`, task).Scan(&typ, &rec, &result); err != nil {
		return err
	}
	var tr struct {
		Untrusted bool   `json:"untrusted_seen"`
		Source    string `json:"untrusted_source"`
	}
	_ = json.Unmarshal(rec, &tr)
	unt := scriptUntrusted{Value: tr.Untrusted}
	if tr.Untrusted {
		src := tr.Source
		if src == "" {
			src = "tool_output"
		}
		unt.Sources = []string{src}
	}
	stepState, stepReason := "failed", reason
	var fields map[string]any
	switch {
	case state == "completed":
		stepState, stepReason = "ok", ""
		var res map[string]any
		if len(result) > 0 && json.Unmarshal(result, &res) == nil {
			cat, err := c.Catalog(ctx)
			if err != nil {
				return err
			}
			fields = scriptFields(cat.TaskTypes[typ], res)
		}
	case reason == "exec_unknown":
		stepState = "unknown"
	}
	return c.endStep(ctx, tx, now, r, s, stepState, stepReason, fields, unt)
}

// scriptFields copies the result fields that code reads: values of closed kinds only (OUT-1,
// SCR-3).
func scriptFields(tt manifest.TaskType, result map[string]any) map[string]any {
	props, _ := tt.ResultSchema["properties"].(map[string]any)
	out := map[string]any{}
	for k, p := range props {
		m, _ := p.(map[string]any)
		if m["x-botaru-read-by"] != "code" {
			continue
		}
		switch v := result[k].(type) {
		case float64, bool:
			out[k] = v
		case string:
			if _, closed := m["enum"]; closed && len(v) <= 120 {
				out[k] = v
			} else if _, closed := m["enum_from"]; closed && len(v) <= 120 {
				out[k] = v
			}
		}
		if len(out) >= 32 {
			break
		}
	}
	return out
}

// scriptCampaignEnded closes the step of a fanout campaign: completed is ok, a failed or stopped
// campaign fails the step, and a campaign card a person rejected or let expire declines it.
func (c *Core) scriptCampaignEnded(ctx context.Context, tx pgx.Tx, now time.Time, cmp *campaignRow, state, reason string) error {
	if cmp.Rec.ScriptRun == "" {
		return nil
	}
	r, err := loadRun(ctx, tx, cmp.Tenant, cmp.Rec.ScriptRun, true)
	if err != nil {
		return err
	}
	steps, err := loadSteps(ctx, tx, r.ID)
	if err != nil {
		return err
	}
	i := slices.IndexFunc(steps, func(s *stepRow) bool { return s.ID == cmp.Rec.ScriptStep })
	if i < 0 {
		return nil
	}
	s := steps[i]
	if _, err := tx.Exec(ctx, `UPDATE script_steps SET campaign = $3 WHERE run = $1 AND n = $2 AND campaign IS NULL`, r.ID, s.N, cmp.ID); err != nil {
		return err
	}
	var untrusted bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM campaign_items WHERE campaign = $1 AND untrusted)`, cmp.ID).Scan(&untrusted); err != nil {
		return err
	}
	unt := scriptUntrusted{Value: untrusted}
	if untrusted {
		unt.Sources = []string{"campaign_item"}
	}
	switch state {
	case "completed":
		return c.endStep(ctx, tx, now, r, s, "ok", "", nil, unt)
	case "rejected":
		if reason == "" || reason == "expired" {
			return c.endStep(ctx, tx, now, r, s, "declined", reason, nil, unt)
		}
		return c.endStep(ctx, tx, now, r, s, "failed", reason, nil, unt)
	default:
		if reason == "" {
			reason = state
		}
		return c.endStep(ctx, tx, now, r, s, "failed", reason, nil, unt)
	}
}

// scriptApprovalMoved follows the approval of an action step: executing releases the step with
// the action task, a refusal or an expired card declines it, a cancellation fails it (SCR-7,
// SCR-11). The outcome of the execution closes the step through its task.
func (c *Core) scriptApprovalMoved(ctx context.Context, tx pgx.Tx, now time.Time, a *approvalRow) error {
	if kind, _ := a.RequestedBy["kind"].(string); kind != "script_step" {
		return nil
	}
	var runID, tenant string
	var n int
	err := tx.QueryRow(ctx, `SELECT s.run, r.tenant, s.n FROM script_steps s JOIN script_runs r ON r.id = s.run
		WHERE s.approval = $1 AND s.state IN ('waiting', 'released')`, a.ID).Scan(&runID, &tenant, &n)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	r, err := loadRun(ctx, tx, tenant, runID, true)
	if err != nil {
		return err
	}
	steps, err := loadSteps(ctx, tx, runID)
	if err != nil {
		return err
	}
	s := steps[n-1]
	switch a.State {
	case "executing":
		if err := shrinkRunReserve(ctx, tx, r.ID, 0, 1); err != nil {
			return err
		}
		return c.moveStep(ctx, tx, now, r, s, "released", "", map[string]any{"task": a.ActionTask})
	case "rejected":
		return c.endStep(ctx, tx, now, r, s, "declined", "", nil, scriptUntrusted{})
	case "expired":
		return c.endStep(ctx, tx, now, r, s, "declined", "expired", nil, scriptUntrusted{})
	case "canceled", "exec_failed":
		if s.State == "waiting" {
			return c.endStep(ctx, tx, now, r, s, "failed", a.Reason, nil, scriptUntrusted{})
		}
	}
	return nil
}

// purgeRuns turns finished runs into tombstones: the record keeps only the root, a step loses
// the closed fields of its result and its untrusted sources, and states, links and times stay
// as metadata (RET-2, RET-10).
// head is empty or a WITH clause the selection of runs reads.
func purgeRuns(ctx context.Context, tx pgx.Tx, head, where string, args ...any) (int64, error) {
	with := "WITH "
	if head != "" {
		with = head + ", "
	}
	var n int64
	err := tx.QueryRow(ctx, with+`r AS (UPDATE script_runs SET record = jsonb_build_object('root', record->'root', 'purged', true)
		WHERE id IN (`+where+`) AND state <> ALL ('{proposed,running,paused}') AND NOT record ? 'purged' RETURNING id),
		s AS (UPDATE script_steps SET fields = NULL, untrusted = NULL WHERE run IN (SELECT id FROM r))
		SELECT count(*) FROM r`, args...).Scan(&n)
	return n, err
}

// cancelRunsOf cancels the live runs anchored at a task that was canceled (SCR-11).
func (c *Core) cancelRunsOf(ctx context.Context, tx pgx.Tx, now time.Time, tenant, task, reason string) error {
	return c.cancelRuns(ctx, tx, now, tenant, `anchor_task = $2`, task, reason)
}

// cancelRunsOfTree cancels the live runs anchored in any task of the tree of root.
func (c *Core) cancelRunsOfTree(ctx context.Context, tx pgx.Tx, now time.Time, tenant, root, reason string) error {
	return c.cancelRuns(ctx, tx, now, tenant, `anchor_task IN (SELECT id FROM tasks WHERE tenant = $1 AND (id = $2 OR record->>'root' = $2))`, root, reason)
}

func (c *Core) cancelRuns(ctx context.Context, tx pgx.Tx, now time.Time, tenant, where string, arg any, reason string) error {
	rows, err := tx.Query(ctx, `SELECT id FROM script_runs WHERE tenant = $1 AND `+where+` AND state = ANY($3) ORDER BY id FOR UPDATE`, tenant, arg, scriptLive)
	if err != nil {
		return err
	}
	var list []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		list = append(list, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(list) == 0 {
		return err
	}
	cat, err := c.Catalog(ctx)
	if err != nil {
		return err
	}
	if reason == "" {
		reason = "initiator"
	}
	for _, id := range list {
		r, err := loadRun(ctx, tx, tenant, id, true)
		if err != nil {
			return err
		}
		if err := c.cancelRun(ctx, tx, now, cat, r, reason); err != nil {
			return err
		}
	}
	return nil
}

// scriptLoop is the background loop script: the run card deadline, the decision deadline of
// confirm steps, the run deadline, a disabled initiator and the steps without predecessors
// (SCR-8, SCR-11).
func (c *Core) scriptLoop(ctx context.Context) error {
	cat, err := c.Catalog(ctx)
	if err != nil {
		return err
	}
	var list []struct{ id, tenant string }
	if err := c.tx(ctx, func(tx pgx.Tx, _ time.Time) error {
		list = list[:0]
		rows, err := tx.Query(ctx, `SELECT id, tenant FROM script_runs WHERE state = ANY($1) ORDER BY created_at, id`, scriptLive)
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
			r, err := loadRun(ctx, tx, x.tenant, x.id, true)
			if err != nil {
				return err
			}
			pi, err := loadPrincipal(ctx, tx, now, r.Tenant, r.Initiator)
			if errors.Is(err, pgx.ErrNoRows) || err == nil && pi.State != "active" {
				return c.cancelRun(ctx, tx, now, cat, r, "person_inactive")
			}
			if err != nil {
				return err
			}
			switch {
			case r.State == "proposed" && !r.ExpiresAt.After(now):
				return c.rejectRun(ctx, tx, now, r, "expired")
			case r.DeadlineAt != nil && !r.DeadlineAt.After(now):
				return c.cancelRun(ctx, tx, now, cat, r, "deadline_exceeded")
			case r.State == "proposed":
				return nil
			}
			steps, err := loadSteps(ctx, tx, r.ID)
			if err != nil {
				return err
			}
			for _, s := range steps {
				if s.State == "waiting" && s.Gate != "" && s.ExpiresAt != nil && !s.ExpiresAt.After(now) {
					if err := c.endStep(ctx, tx, now, r, s, "declined", "expired", nil, scriptUntrusted{}); err != nil {
						return err
					}
				}
			}
			return c.releaseSteps(ctx, tx, now, cat, r)
		}); err != nil {
			return err
		}
	}
	return nil
}
