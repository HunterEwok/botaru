// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// The task views and the cancellation of P1 (task.get, cancel: an external program) and P2
// (task.show, task.cancel, task.list, team.tasks, about: the person of the P2 credential).
// A task someone may not read answers exactly as a missing one: not_found (PRM-9).

// taskRecordFields are the fields of task-record a view shows. The reply address (origin and
// the conversation computed from it) is left out: it names a person's chat (PRM-9, SEC-4).
var taskRecordFields = []string{"id", "tenant", "type", "idempotency_key", "initiator", "enqueued_by", "parent", "root", "depth",
	"correlationid", "refs", "continues", "max_tier", "data_mark", "untrusted_seen", "untrusted_source", "personal_subjects",
	"approval_id", "priority_class", "fairness_key", "state", "reason", "lease_no", "attempts_spent", "runs_counted",
	"waiting_reason", "run_at", "followup_until", "campaign", "campaign_item", "script_run", "script_step", "acting_bot",
	"bot_epoch", "seat_epoch", "incident", "trace"}

// taskRow is what a view reads of a task.
type taskRow struct {
	id, tenant, typ, state, reason, initiator, key string
	prio, leaseNo, spent, runs                     int
	rec                                            map[string]any
}

func loadTaskRow(ctx context.Context, tx pgx.Tx, tenant, id string, lock bool) (*taskRow, error) {
	q := `SELECT id, tenant, type, state, coalesce(reason, ''), initiator, idempotency_key, priority_class, lease_no, attempts_spent, runs_counted, record
		FROM tasks WHERE id = $1 AND tenant = $2`
	if lock {
		q += ` FOR UPDATE`
	}
	var t taskRow
	var raw []byte
	err := tx.QueryRow(ctx, q, id, tenant).Scan(&t.id, &t.tenant, &t.typ, &t.state, &t.reason, &t.initiator, &t.key, &t.prio, &t.leaseNo, &t.spent, &t.runs, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal(raw, &t.rec)
	return &t, nil
}

func (t *taskRow) mark() string { s, _ := t.rec["data_mark"].(string); return s }

// view is the task-record of the task, without the reply address.
func (t *taskRow) view() map[string]any {
	out := map[string]any{}
	for _, k := range taskRecordFields {
		if v, ok := t.rec[k]; ok && v != nil {
			out[k] = v
		}
	}
	for k, v := range map[string]any{"id": t.id, "tenant": t.tenant, "type": t.typ, "state": t.state, "initiator": t.initiator,
		"idempotency_key": t.key, "priority_class": t.prio, "lease_no": t.leaseNo, "attempts_spent": t.spent, "runs_counted": t.runs} {
		out[k] = v
	}
	if t.reason != "" {
		out["reason"] = t.reason
	}
	if s, ok := t.rec["scope"].(map[string]any); ok {
		if rs, ok := s["resources"].([]any); ok {
			out["scope"] = rs
		}
	}
	return out
}

// TaskView is p1-task-get-response and p2-task-show-response.
type TaskView struct {
	Task      map[string]any `json:"task"`
	Artifacts []string       `json:"artifacts,omitempty"`
}

// artifactsOf are the stored files the task produced (ART-1).
func artifactsOf(ctx context.Context, tx pgx.Tx, tenant, task string) ([]string, error) {
	rows, err := tx.Query(ctx, `SELECT file_ref FROM files WHERE tenant = $1 AND producer = $2 AND origin = 'produced' AND state = 'stored'
		ORDER BY created_at, file_ref LIMIT 100`, tenant, task)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// mayReadTask applies PRM-9: the initiator, or an active principal of the tenant with
// task.read:<type> and data:<mark>.
func (c *Core) mayReadTask(ctx context.Context, tx pgx.Tx, now time.Time, tv *TenantView, principal string, t *taskRow) (bool, error) {
	if t.initiator == principal {
		return true, nil
	}
	p, err := loadPrincipal(ctx, tx, now, tv.Name, principal)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if p.State != "active" {
		return false, nil
	}
	groups := groupClosure(tv, p.Groups)
	return hasCap(tv, groups, "task.read:"+t.typ) && hasData(tv, groups, t.mark()), nil
}

// readTask reads a task for a principal and writes the read to the audit (PRM-9, AUD-1).
func (c *Core) readTask(ctx context.Context, tenant, principal, id, command string, actor Actor) (*TaskView, error) {
	tv, _, err := c.requireTenant(ctx, tenant)
	if err != nil {
		return nil, err
	}
	var out *TaskView
	err = c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		t, err := loadTaskRow(ctx, tx, tenant, id, false)
		if err != nil {
			return err
		}
		ok := t != nil
		if ok {
			if ok, err = c.mayReadTask(ctx, tx, now, tv, principal, t); err != nil {
				return err
			}
		}
		if !ok {
			return refuse("not_found", "no such task")
		}
		arts, err := artifactsOf(ctx, tx, tenant, id)
		if err != nil {
			return err
		}
		out = &TaskView{Task: t.view(), Artifacts: arts}
		_, err = c.appendAudit(ctx, tx, now, Audit{
			Tenant: tenant, Kind: "read", Actor: actor, Command: command, Target: &Target{Kind: "task", ID: id}, OK: true,
			After: map[string]any{"data_mark": t.mark()},
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// TaskIDRequest is p1-task-get-request and p1-cancel-request.
type TaskIDRequest struct {
	Task string `json:"task"`
}

// ProgramTaskGet is P1 task.get of an external program (PRM-9).
func (c *Core) ProgramTaskGet(ctx context.Context, p *Program, req TaskIDRequest) (*TaskView, error) {
	return c.readTask(ctx, p.Tenant, p.Principal, req.Task, "task.get", programActor(p))
}

// CancelResult is p1-cancel-response.
type CancelResult struct {
	Task   string `json:"task"`
	State  string `json:"state"`
	Reason string `json:"reason,omitempty"`
}

// cancelTask cancels a task of the tenant with its tree and the approvals the tree asked for,
// in one transaction (APR-6); only a root task is cancelled. A task that already ended with a cancellation gives the same
// answer again (IDM-3); one that ended otherwise is already_terminal, unless an approval or a
// script run of its tree still waits, which the cancellation withdraws (PRM-17).
func (c *Core) cancelTask(ctx context.Context, tx pgx.Tx, now time.Time, t *taskRow, reason string, actor Actor, command string) (*CancelResult, error) {
	// The cancellation acts on a whole tree, so it names the root: a child is cancelled with its
	// tree, never alone with its descendants left running (APR-6).
	if root, _ := t.rec["root"].(string); root != "" && root != t.id {
		return nil, refuse("input_schema_invalid", "the task is a child of %s; cancel the root task", root)
	}
	switch t.state {
	case "submitted", "working":
	case "canceled":
		return &CancelResult{Task: t.id, State: "canceled", Reason: t.reason}, nil
	default:
		var waiting bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM approvals WHERE tenant = $1 AND state IN ('pending', 'approved') AND source_task IN
			(SELECT id FROM tasks WHERE tenant = $1 AND (id = $2 OR record->>'root' = $2)))
			OR EXISTS (SELECT 1 FROM script_runs WHERE tenant = $1 AND state = ANY($3) AND anchor_task IN
			(SELECT id FROM tasks WHERE tenant = $1 AND (id = $2 OR record->>'root' = $2)))`, t.tenant, t.id, scriptLive).Scan(&waiting); err != nil {
			return nil, err
		}
		if !waiting {
			return nil, refuse("already_terminal", "the task ended as %s", t.state)
		}
	}
	if err := c.cancelTree(ctx, tx, now, t.tenant, t.id, reason); err != nil {
		return nil, err
	}
	if _, err := c.appendAudit(ctx, tx, now, Audit{
		Tenant: t.tenant, Kind: "command", Actor: actor, Command: command, Target: &Target{Kind: "task", ID: t.id}, OK: true, Reason: reason,
	}); err != nil {
		return nil, err
	}
	return &CancelResult{Task: t.id, State: "canceled", Reason: reason}, nil
}

// ProgramCancel is P1 cancel of an external program for its own task: the outcome is
// canceled(initiator) (PRM-9, APR-6, IDM-3).
func (c *Core) ProgramCancel(ctx context.Context, p *Program, req TaskIDRequest) (*CancelResult, error) {
	var out *CancelResult
	err := c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		t, err := loadTaskRow(ctx, tx, p.Tenant, req.Task, true)
		if err != nil {
			return err
		}
		active, err := programActive(ctx, tx, p)
		if err != nil {
			return err
		}
		if t == nil || t.initiator != p.Principal || !active || strings.HasPrefix(t.typ, "action.") {
			return refuse("not_found", "no such task")
		}
		out, err = c.cancelTask(ctx, tx, now, t, "initiator", programActor(p), "task.cancel")
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// P2TaskRequest is p2-task-request.
type P2TaskRequest struct {
	Op   string `json:"op"`
	Task string `json:"task"`
}

// TaskShow is P2 task.show for the person of the credential (PRM-9, DSP-2).
func (c *Core) TaskShow(ctx context.Context, op Operator, req P2TaskRequest) (*TaskView, error) {
	return c.readTask(ctx, op.Tenant, op.Principal, req.Task, "task.show", Actor{Principal: op.Principal, Via: "farmctl"})
}

// TaskCancel is P2 task.cancel: the initiator cancels with the outcome canceled(initiator); an
// operator of the tenant cancels another's task with canceled(admin) (APR-6). A task of another
// tenant is not_found.
func (c *Core) TaskCancel(ctx context.Context, op Operator, req P2TaskRequest) (*Done, error) {
	var out *Done
	err := c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		t, err := loadTaskRow(ctx, tx, op.Tenant, req.Task, true)
		if err != nil {
			return err
		}
		if t == nil || strings.HasPrefix(t.typ, "action.") {
			return refuse("not_found", "no such task")
		}
		reason := "admin"
		if t.initiator == op.Principal {
			reason = "initiator"
		}
		r, err := c.cancelTask(ctx, tx, now, t, reason, Actor{Principal: op.Principal, Via: "farmctl"}, "task.cancel")
		if err != nil {
			return err
		}
		out = &Done{OK: true, ID: t.id, State: r.State}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// TaskListRequest is p2-task-list-request.
type TaskListRequest struct {
	Op    string `json:"op"`
	Group string `json:"group,omitempty"`
	State string `json:"state,omitempty"`
	Limit int    `json:"limit,omitempty"`
	After string `json:"after,omitempty"`
}

// TaskListItem is one row of p2-task-list-response.
type TaskListItem struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	Title     string `json:"title,omitempty"`
	State     string `json:"state"`
	CreatedAt string `json:"created_at"`
}

// TaskList is p2-task-list-response.
type TaskList struct {
	Items []TaskListItem `json:"items,omitempty"`
	Text  string         `json:"text,omitempty"`
	Next  string         `json:"next,omitempty"`
}

// listCursor reads the cursor <created_at unix micros>.<task id>.
func listCursor(s string) (time.Time, string, bool) {
	us, id, ok := strings.Cut(s, ".")
	n, err := strconv.ParseInt(us, 10, 64)
	if !ok || err != nil || id == "" {
		return time.Time{}, "", false
	}
	return time.UnixMicro(n).UTC(), id, true
}

// TaskList runs task.list, team.tasks and about: code tables without model text (PRM-15).
// task.list is the person's own root tasks; team.tasks the tasks of a group's people for a
// person with team.view:<group>, only of types the person may read (DLV-14, PRM-9); about the
// code text of the bot.
func (c *Core) TaskList(ctx context.Context, op Operator, req TaskListRequest) (*TaskList, error) {
	tv, cat, err := c.requireTenant(ctx, op.Tenant)
	if err != nil {
		return nil, err
	}
	if req.Op == "about" {
		locale := c.pickLocale("", tv.Spec.Locale)
		return &TaskList{Text: c.texts.Render(locale, "cmd.about.text", map[string]any{"tenant": tv.Name})}, nil
	}
	limit := req.Limit
	if limit == 0 {
		limit = 100
	}
	from, fromID := time.Time{}, ""
	if req.After != "" {
		var ok bool
		if from, fromID, ok = listCursor(req.After); !ok {
			return nil, refuse("input_schema_invalid", "after")
		}
	}
	var out TaskList
	err = c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		out = TaskList{}
		p, err := loadPrincipal(ctx, tx, now, tv.Name, op.Principal)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		var groups []string
		if p != nil && p.State == "active" {
			groups = groupClosure(tv, p.Groups)
		}
		where, args := `t.initiator = $2 AND coalesce(t.record->>'root', t.id) = t.id`, []any{tv.Name, op.Principal}
		if req.Op == "team.tasks" {
			group := req.Group
			if group == "" {
				for _, g := range slices.Sorted(mapKeys(tv.Groups)) {
					if hasCap(tv, groups, "team.view:"+g) {
						group = g
						break
					}
				}
			}
			if group == "" || !hasCap(tv, groups, "team.view:"+group) {
				pr := refuse("forbidden", "team.view of the group is needed")
				pr.Needs = []string{"team.view:" + orDash(group)}
				return pr
			}
			// The members of now, and only rows of types and data the person may read (PRM-9).
			var types, marks []string
			for _, typ := range slices.Sorted(mapKeys(cat.TaskTypes)) {
				if hasCap(tv, groups, "task.read:"+typ) {
					types = append(types, typ)
				}
			}
			for _, g := range groups {
				marks = append(marks, tv.Groups[g].Data...)
			}
			where, args[1] = `t.initiator IN (SELECT principal FROM memberships WHERE tenant = $1 AND grp = $2 AND state = 'active'
				AND (until IS NULL OR until > $3)) AND t.type = ANY($4) AND coalesce(t.record->>'data_mark', '') = ANY($5)`, group
			args = append(args, now, nonNil(types), nonNil(marks))
		}
		q := `SELECT t.id, t.type, t.state, t.created_at, coalesce(t.record->>'data_mark', '') FROM tasks t
			WHERE t.tenant = $1 AND t.type NOT LIKE 'action.%' AND ` + where
		if req.State != "" {
			args = append(args, req.State)
			q += ` AND t.state = $` + strconv.Itoa(len(args))
		}
		if fromID != "" {
			args = append(args, from, fromID)
			q += ` AND (t.created_at, t.id) > ($` + strconv.Itoa(len(args)-1) + `, $` + strconv.Itoa(len(args)) + `)`
		}
		args = append(args, limit+1)
		q += ` ORDER BY t.created_at, t.id LIMIT $` + strconv.Itoa(len(args))
		rows, err := tx.Query(ctx, q, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		var last time.Time
		for rows.Next() {
			var it TaskListItem
			var at time.Time
			var mark string
			if err := rows.Scan(&it.ID, &it.Type, &it.State, &at, &mark); err != nil {
				return err
			}
			// A row of the team only of a type the person may read (PRM-9).
			if req.Op == "team.tasks" && (!hasCap(tv, groups, "task.read:"+it.Type) || !hasData(tv, groups, mark)) {
				continue
			}
			if len(out.Items) == limit {
				out.Next = strconv.FormatInt(last.UnixMicro(), 10) + "." + out.Items[len(out.Items)-1].ID
				break
			}
			it.Title = cat.TaskTypes[it.Type].Title
			it.CreatedAt = fmtTime(at)
			last = at
			out.Items = append(out.Items, it)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}
