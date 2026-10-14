// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/jcs"
)

// The commands of a person answered by code, without a model and without a task (PRM-17,
// REP-12): /tasks, /status, /cancel, /help, /me, /lang, /team and /about. Every text comes from
// the code text catalog; a type is shown by its title, never by its name.

// codeCommands are the built-in commands this file answers.
var codeCommands = map[string]bool{"/tasks": true, "/status": true, "/cancel": true, "/help": true, "/me": true, "/lang": true, "/team": true, "/about": true}

func (c *Core) codeCommand(ctx context.Context, tx pgx.Tx, now time.Time, pc personCommand, word, rest string) (*ActivityResult, error) {
	switch word {
	case "/tasks":
		return c.tasksCommand(ctx, tx, now, pc)
	case "/status", "/cancel":
		return c.statusCommand(ctx, tx, now, pc, word, rest)
	case "/help":
		return c.helpCommand(pc), nil
	case "/me":
		return c.meCommand(pc), nil
	case "/lang":
		return c.langCommand(ctx, tx, pc, rest)
	case "/team":
		return c.teamCommand(ctx, tx, now, pc, rest)
	}
	return pc.reply(c, "cmd.about.text", map[string]any{"tenant": pc.tv.Name}), nil
}

// ownTask is a task of the person in the last day, numbered from 1 in the order of creation.
type ownTask struct {
	n              int
	id, typ, state string
	reason         string
	attempts       int
	deadline       *time.Time
	waiting, title string
}

// ownTasks are the person's root tasks of the last day; an action task belongs to its approval
// and is not listed.
func (c *Core) ownTasks(ctx context.Context, tx pgx.Tx, now time.Time, pc personCommand) ([]ownTask, error) {
	rows, err := tx.Query(ctx, `SELECT id, type, state, coalesce(reason, ''), attempts_spent, deadline_at, coalesce(record->>'waiting_reason', '')
		FROM tasks WHERE tenant = $1 AND initiator = $2 AND created_at > $3 AND type NOT LIKE 'action.%' AND coalesce(record->>'root', id) = id
		ORDER BY created_at, id`, pc.tv.Name, pc.person, now.Add(-24*time.Hour))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ownTask
	for rows.Next() {
		var t ownTask
		if err := rows.Scan(&t.id, &t.typ, &t.state, &t.reason, &t.attempts, &t.deadline, &t.waiting); err != nil {
			return nil, err
		}
		t.n = len(out) + 1
		t.title = pc.cat.TaskTypes[t.typ].Title
		if t.title == "" {
			t.title = "-"
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (t ownTask) waitingText() string {
	w := t.waiting
	if w == "" && t.state == "submitted" {
		w = t.reason
	}
	if w == "" {
		return ""
	}
	return " (" + w + ")"
}

func (c *Core) tasksCommand(ctx context.Context, tx pgx.Tx, now time.Time, pc personCommand) (*ActivityResult, error) {
	ts, err := c.ownTasks(ctx, tx, now, pc)
	if err != nil {
		return nil, err
	}
	if len(ts) == 0 {
		return pc.reply(c, "cmd.tasks.empty", nil), nil
	}
	lines := []string{c.texts.Render(pc.locale, "cmd.tasks.header", nil)}
	for _, t := range ts {
		lines = append(lines, c.texts.Render(pc.locale, "cmd.tasks.row", map[string]any{"n": t.n, "title": t.title, "state": t.state, "waiting": t.waitingText()}))
	}
	return &ActivityResult{Result: "reply", Text: strings.Join(lines, "\n")}, nil
}

// cancelNonce is the confirmation word of /cancel: a short hash of the task and the person, so
// only a repeat of the answer cancels.
func cancelNonce(task, person string) string {
	h, _ := jcs.Hash(map[string]any{"cancel": task, "by": person})
	return strings.ToUpper(strings.TrimPrefix(h, "sha256:")[:6])
}

// statusCommand is /status <n> and /cancel <n> [<nonce>]: a number the person does not have
// answers not_found (PRM-9); /cancel asks to confirm, and the repeat with the nonce cancels the
// tree and its waiting approvals with the reason initiator (APR-6).
func (c *Core) statusCommand(ctx context.Context, tx pgx.Tx, now time.Time, pc personCommand, word, rest string) (*ActivityResult, error) {
	f := strings.Fields(rest)
	ts, err := c.ownTasks(ctx, tx, now, pc)
	if err != nil {
		return nil, err
	}
	n := 0
	if len(f) > 0 {
		n, _ = strconv.Atoi(f[0])
	}
	if n < 1 || n > len(ts) {
		return pc.reply(c, "cmd.status.not_found", map[string]any{"n": strings.Join(f, " ")}), nil
	}
	t := ts[n-1]
	if word == "/status" {
		deadline := "-"
		if t.deadline != nil {
			deadline = fmtTime(*t.deadline)
		}
		return pc.reply(c, "cmd.status.row", map[string]any{"n": n, "title": t.title, "state": t.state, "attempt": t.attempts,
			"waiting": t.waitingText(), "deadline": deadline}), nil
	}
	// A finished tree can still have an approval waiting for a decision: /cancel withdraws it.
	if t.state != "submitted" && t.state != "working" {
		var waiting bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM approvals WHERE tenant = $1 AND state = 'pending' AND source_task IN
			(SELECT id FROM tasks WHERE tenant = $1 AND (id = $2 OR record->>'root' = $2)))`, pc.tv.Name, t.id).Scan(&waiting); err != nil {
			return nil, err
		}
		if !waiting {
			return pc.reply(c, "cmd.cancel.finished", map[string]any{"n": n, "state": t.state}), nil
		}
	}
	nonce := cancelNonce(t.id, pc.person)
	if len(f) < 2 || !strings.EqualFold(f[1], nonce) {
		return pc.reply(c, "cmd.cancel.confirm", map[string]any{"n": n, "title": t.title, "nonce": nonce}), nil
	}
	if err := c.cancelTree(ctx, tx, now, pc.tv.Name, t.id, "initiator"); err != nil {
		return nil, err
	}
	if _, err := c.appendAudit(ctx, tx, now, Audit{
		Tenant: pc.tv.Name, Kind: "command", Actor: Actor{Principal: pc.person, Via: "channel"}, Command: "task.cancel",
		Target: &Target{Kind: "task", ID: t.id}, OK: true, Reason: "initiator",
	}); err != nil {
		return nil, err
	}
	return pc.reply(c, "cmd.cancel.done", map[string]any{"n": n}), nil
}

// cancelTree cancels a task with its tree and the pending approvals the tree asked for; an
// action task and a released write call are not revoked (APR-6, APR-7).
func (c *Core) cancelTree(ctx context.Context, tx pgx.Tx, now time.Time, tenant, root, reason string) error {
	rows, err := tx.Query(ctx, `SELECT id FROM tasks WHERE tenant = $1 AND (id = $2 OR record->>'root' = $2) AND state IN ('submitted', 'working')
		AND type NOT LIKE 'action.%' ORDER BY id FOR UPDATE`, tenant, root)
	if err != nil {
		return err
	}
	var tasks []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		tasks = append(tasks, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	// The approvals asked for by any task of the tree, finished or not: cards go out after the
	// asking attempt ends, so its task is usually finished by then.
	aps, err := tx.Query(ctx, `SELECT `+approvalColumns+` FROM approvals WHERE tenant = $1 AND state = 'pending' AND source_task IN
		(SELECT id FROM tasks WHERE tenant = $1 AND (id = $2 OR record->>'root' = $2)) ORDER BY id FOR UPDATE`, tenant, root)
	if err != nil {
		return err
	}
	var pending []*approvalRow
	for aps.Next() {
		a, err := scanApproval(aps)
		if err != nil {
			aps.Close()
			return err
		}
		pending = append(pending, a)
	}
	aps.Close()
	if err := aps.Err(); err != nil {
		return err
	}
	for _, a := range pending {
		if err := c.closeApproval(ctx, tx, now, a, "canceled", reason); err != nil {
			return err
		}
	}
	return c.cancelTaskIDs(ctx, tx, now, tenant, tasks, reason)
}

// helpCommand lists the tenant's commands and /run pipelines the person may use, by title.
func (c *Core) helpCommand(pc personCommand) *ActivityResult {
	var rows []string
	for _, cmd := range slices.Sorted(mapKeys(pc.tv.Spec.Commands)) {
		typ := pc.tv.Spec.Commands[cmd]
		tt, ok := pc.cat.TaskTypes[typ]
		if !ok || !hasCap(pc.tv, pc.groups, "task:"+typ) {
			continue
		}
		rows = append(rows, c.texts.Render(pc.locale, "cmd.help.row", map[string]any{"command": strings.TrimPrefix(cmd, "/"), "title": tt.Title}))
	}
	for _, name := range slices.Sorted(mapKeys(pc.tv.Spec.Run)) {
		e := pc.tv.Spec.Run[name]
		title := ""
		switch {
		case e.Type != "" && hasCap(pc.tv, pc.groups, "task:"+e.Type):
			title = pc.cat.TaskTypes[e.Type].Title
		case e.Action != "" && hasCap(pc.tv, pc.groups, "action.request:"+e.Action):
			title = pc.cat.Actions[e.Action].Title
		default:
			continue
		}
		rows = append(rows, c.texts.Render(pc.locale, "cmd.help.row", map[string]any{"command": "run " + name, "title": title}))
	}
	if len(rows) == 0 {
		return pc.reply(c, "cmd.help.none", nil)
	}
	return &ActivityResult{Result: "reply", Text: c.texts.Render(pc.locale, "cmd.help.header", nil) + "\n" + strings.Join(rows, "\n")}
}

// meCommand shows the person's groups, the types and actions they may ask for by title, and
// their data classes.
func (c *Core) meCommand(pc personCommand) *ActivityResult {
	var types, actions, data []string
	for _, name := range slices.Sorted(mapKeys(pc.cat.TaskTypes)) {
		if tt := pc.cat.TaskTypes[name]; hasCap(pc.tv, pc.groups, "task:"+name) && tt.Title != "" {
			types = append(types, tt.Title)
		}
	}
	for _, name := range slices.Sorted(mapKeys(pc.cat.Actions)) {
		if a := pc.cat.Actions[name]; hasCap(pc.tv, pc.groups, "action.request:"+name) && a.Title != "" {
			actions = append(actions, a.Title)
		}
	}
	for _, g := range pc.groups {
		for _, d := range pc.tv.Groups[g].Data {
			if !slices.Contains(data, d) {
				data = append(data, d)
			}
		}
	}
	sort.Strings(data)
	list := func(xs []string) string {
		if len(xs) == 0 {
			return "-"
		}
		return strings.Join(xs, ", ")
	}
	lines := []string{
		c.texts.Render(pc.locale, "cmd.profile.groups", map[string]any{"groups": list(pc.groups)}),
		c.texts.Render(pc.locale, "cmd.profile.types", map[string]any{"titles": list(types)}),
		c.texts.Render(pc.locale, "cmd.profile.actions", map[string]any{"titles": list(actions)}),
		c.texts.Render(pc.locale, "cmd.profile.data", map[string]any{"classes": list(data)}),
	}
	return &ActivityResult{Result: "reply", Text: strings.Join(lines, "\n")}
}

// langCommand sets the person's locale from the loaded text catalogs (REP-12, DLV-9).
func (c *Core) langCommand(ctx context.Context, tx pgx.Tx, pc personCommand, rest string) (*ActivityResult, error) {
	want := strings.TrimSpace(rest)
	locales := c.texts.Locales()
	if !slices.Contains(locales, want) {
		return pc.reply(c, "cmd.lang.unknown", map[string]any{"locales": strings.Join(locales, ", ")}), nil
	}
	if _, err := tx.Exec(ctx, `UPDATE principals SET locale = $3 WHERE tenant = $1 AND id = $2`, pc.tv.Name, pc.person, want); err != nil {
		return nil, err
	}
	pc.locale = want
	return pc.reply(c, "cmd.lang.done", map[string]any{"locale": want}), nil
}

// teamCommand is /team [group] [window] for a person with team.view:<group> (DLV-14): the
// tasks of the group's people over the window by type, with done, failed and waiting counts,
// and the waiting approvals they asked for; rows only of types the asker may read, never a row
// per person and never a task text.
func (c *Core) teamCommand(ctx context.Context, tx pgx.Tx, now time.Time, pc personCommand, rest string) (*ActivityResult, error) {
	f := strings.Fields(rest)
	group, window := "", 7*24*time.Hour
	for _, x := range f {
		if d, ok := parseWindow(x); ok {
			window = d
		} else {
			group = x
		}
	}
	if group == "" {
		for _, g := range slices.Sorted(mapKeys(pc.tv.Groups)) {
			if hasCap(pc.tv, pc.groups, "team.view:"+g) {
				group = g
				break
			}
		}
	}
	if group == "" || !hasCap(pc.tv, pc.groups, "team.view:"+group) {
		return pc.reply(c, "cmd.forbidden.needs", map[string]any{"group": capGroups(pc.tv, "team.view:"+group)}), nil
	}
	rows, err := tx.Query(ctx, `SELECT t.type, count(*) FILTER (WHERE t.state = 'completed'), count(*) FILTER (WHERE t.state = 'failed'),
		count(*) FILTER (WHERE t.state IN ('submitted', 'working'))
		FROM tasks t WHERE t.tenant = $1 AND t.created_at > $3 AND t.type NOT LIKE 'action.%' AND t.initiator IN
			(SELECT principal FROM memberships WHERE tenant = $1 AND grp = $2 AND state = 'active')
		GROUP BY t.type ORDER BY t.type`, pc.tv.Name, group, now.Add(-window))
	if err != nil {
		return nil, err
	}
	type row struct {
		typ                   string
		done, failed, waiting int
	}
	var rs []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.typ, &r.done, &r.failed, &r.waiting); err != nil {
			rows.Close()
			return nil, err
		}
		rs = append(rs, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	lines := []string{c.texts.Render(pc.locale, "cmd.team.header", map[string]any{"group": group, "window": fmtWindow(window)})}
	for _, r := range rs {
		tt, ok := pc.cat.TaskTypes[r.typ]
		if !ok || !hasCap(pc.tv, pc.groups, "task.read:"+r.typ) || tt.DataClass != "" && !hasData(pc.tv, pc.groups, tt.DataClass) {
			continue
		}
		lines = append(lines, c.texts.Render(pc.locale, "cmd.team.row", map[string]any{"title": tt.Title, "done": r.done, "failed": r.failed, "waiting": r.waiting}))
	}
	var pending int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM approvals WHERE tenant = $1 AND state = 'pending' AND initiator IN
		(SELECT principal FROM memberships WHERE tenant = $1 AND grp = $2 AND state = 'active')`, pc.tv.Name, group).Scan(&pending); err != nil {
		return nil, err
	}
	lines = append(lines, c.texts.Render(pc.locale, "cmd.team.approvals", map[string]any{"count": pending}))
	return &ActivityResult{Result: "reply", Text: strings.Join(lines, "\n")}, nil
}

// parseWindow reads a window of /team: <n>d or <n>h, at most 31 days.
func parseWindow(s string) (time.Duration, bool) {
	if len(s) < 2 {
		return 0, false
	}
	n, err := strconv.Atoi(s[:len(s)-1])
	if err != nil || n < 1 {
		return 0, false
	}
	var d time.Duration
	switch s[len(s)-1] {
	case 'd':
		d = time.Duration(n) * 24 * time.Hour
	case 'h':
		d = time.Duration(n) * time.Hour
	default:
		return 0, false
	}
	return min(d, 31*24*time.Hour), true
}

func fmtWindow(d time.Duration) string {
	if d%(24*time.Hour) == 0 {
		return strconv.Itoa(int(d/(24*time.Hour))) + "d"
	}
	return strconv.Itoa(int(d/time.Hour)) + "h"
}

// cancelTaskIDs cancels the tasks: a running attempt loses its lease and handles in the same
// transaction, so the worker is fenced at its next call.
func (c *Core) cancelTaskIDs(ctx context.Context, tx pgx.Tx, now time.Time, tenant string, tasks []string, reason string) error {
	for _, id := range tasks {
		if _, err := tx.Exec(ctx, `DELETE FROM handles WHERE work_id = $1`, id); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE leases SET state = 'revoked', reason = $3, closed_at = $2 WHERE work_id = $1 AND state = 'active'`, id, now, reason); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE tasks SET state = 'canceled', reason = $2, run_at = NULL, updated_at = $3 WHERE id = $1`, id, reason, now); err != nil {
			return err
		}
		if err := releaseTaskReserve(ctx, tx, id); err != nil {
			return err
		}
		if err := c.appendEvent(ctx, tx, now, tenant, Event{Type: "task.canceled", Subject: id, Data: map[string]any{"task": id, "reason": reason}}); err != nil {
			return err
		}
	}
	return nil
}
