// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// The manager_notice of an incident (MGR-39, MGR-13). The core keeps one message to people per
// incident and recipient chat, on two routes: tenant (the people of the incident's tenant with
// incident.manage, everything) and farm (the farm operators with admin:governance: only the
// number, the class, the operation codes and the numbers). The message is created when the
// incident opens; each state change, operation and closing appends a line to the incident's
// notice log and plans an update in the same transaction. The text is drawn from the incident
// when a channel takes the delivery, so a waiting update always shows the latest state and
// several changes share one update. A channel with edit edits the first message; one without
// gets the new lines in a batch at most once in 15 minutes. The message is not an alarm: people
// are paged with alarm.incident.unhandled (MON-1).

const (
	managerNoticeKind = "manager_notice"
	// noticeBatchEvery is how often a driver without edit gets the new lines (MGR-39).
	noticeBatchEvery = 15 * time.Minute
	// noticeLogMax is the most lines the notice log of an incident keeps.
	noticeLogMax = 200
	// noticeLinesMax is the room for the lines in one text parameter (work-item schema: 500).
	noticeLinesMax = 480
)

// noticeEntry is one line of the notice log: a state of the incident or an applied operation,
// with codes and ids only.
type noticeEntry struct {
	At     string         `json:"at"`
	Kind   string         `json:"kind"` // state or op
	State  string         `json:"state,omitempty"`
	Op     string         `json:"op,omitempty"`
	Target *ControlTarget `json:"target,omitempty"`
	By     string         `json:"by,omitempty"` // seat or person
}

// noticeLog appends a line to the notice log of an incident and plans the update of its
// messages (MGR-13: in the transaction of the change).
func (c *Core) noticeLog(ctx context.Context, tx pgx.Tx, now time.Time, incident string, e noticeEntry, update bool) error {
	e.At = fmtTime(now)
	raw, err := json.Marshal([]noticeEntry{e})
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE incidents SET notice_log = notice_log || $2::jsonb WHERE id = $1 AND jsonb_array_length(notice_log) < $3`,
		incident, raw, noticeLogMax); err != nil {
		return err
	}
	if !update {
		return nil
	}
	return c.noticeUpdate(ctx, tx, now, incident)
}

// noticeStateEntry is the log line of an incident event; an ack changes no state and gets none.
func noticeStateEntry(r *incidentRow, what string) (noticeEntry, bool) {
	if what == "acked" {
		return noticeEntry{}, false
	}
	return noticeEntry{Kind: "state", State: r.State}, true
}

// noticeOpen creates the messages of a new incident: one per recipient chat of each route.
// An incident of tenant farm has only the tenant route, which is the farm's own people.
func (c *Core) noticeOpen(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, r *incidentRow) error {
	type route struct {
		name, tenant string
		caps         []string
	}
	routes := []route{{"tenant", r.Tenant, []string{"incident.manage"}}, {"farm", farmTenant, []string{"admin:governance"}}}
	if r.Tenant == farmTenant {
		routes = []route{{"tenant", farmTenant, []string{"incident.manage", "admin:governance"}}}
	}
	for _, rt := range routes {
		tv, ok := cat.Tenants[rt.tenant]
		if !ok {
			continue
		}
		chats, err := c.adminChats(ctx, tx, now, tv, "", rt.caps...)
		if err != nil {
			return err
		}
		for _, a := range chats {
			target := Chat{Kind: "direct", ExternalID: a.ExternalID}.ID()
			id, err := c.planDelivery(ctx, tx, now, plannedDelivery{
				Tenant: rt.tenant, Kind: managerNoticeKind, Addr: "admins", TargetKind: "chat", TargetID: target,
				ExecKind: "channel", ExecName: a.Channel, Recipient: a.Principal, DataOrigin: "code",
				Body: map[string]any{"form": "text_code", "text_key": "notice.manager_notice", "route": rt.name}, SubjectKind: "incident", SubjectID: r.ID,
			})
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO manager_notices (delivery, tenant, basis_kind, basis, route, recipient, channel, target_id, edit, created_at)
				VALUES ($1, $2, 'incident', $3, $4, $5, $6, $7, $8, $9)`,
				id, rt.tenant, r.ID, rt.name, a.Principal, a.Channel, target, tv.Channels[a.Channel].Capabilities.Edit, now); err != nil {
				return err
			}
		}
	}
	return nil
}

// noticeUpdate plans one update of every message of an incident. A first message or an update
// that still waits draws the latest text anyway, so none is planned then; on a driver without edit it waits
// for the end of the 15-minute batch window.
func (c *Core) noticeUpdate(ctx context.Context, tx pgx.Tx, now time.Time, incident string) error {
	rows, err := tx.Query(ctx, `SELECT n.delivery, n.tenant, n.route, n.recipient, n.channel, n.target_id, n.edit, n.batch_at FROM manager_notices n
		WHERE n.basis_kind = 'incident' AND n.basis = $1 AND NOT EXISTS (SELECT 1 FROM deliveries d WHERE d.kind = $2 AND d.state = 'planned'
			AND d.subject_kind = 'incident' AND d.subject_id = n.basis AND (d.id = n.delivery OR d.body->>'of' = n.delivery))
		ORDER BY n.delivery FOR UPDATE OF n`, incident, managerNoticeKind)
	if err != nil {
		return err
	}
	type msg struct {
		delivery, tenant, route, recipient, channel, target string
		edit                                                bool
		batchAt                                             *time.Time
	}
	var all []msg
	for rows.Next() {
		var m msg
		if err := rows.Scan(&m.delivery, &m.tenant, &m.route, &m.recipient, &m.channel, &m.target, &m.edit, &m.batchAt); err != nil {
			rows.Close()
			return err
		}
		all = append(all, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, m := range all {
		id, err := c.planDelivery(ctx, tx, now, plannedDelivery{
			Tenant: m.tenant, Kind: managerNoticeKind, Addr: "admins", TargetKind: "chat", TargetID: m.target,
			ExecKind: "channel", ExecName: m.channel, Recipient: m.recipient, DataOrigin: "code",
			Body: map[string]any{"form": "text_code", "text_key": "notice.manager_notice", "route": m.route, "of": m.delivery}, SubjectKind: "incident", SubjectID: incident,
		})
		if err != nil {
			return err
		}
		if !m.edit && m.batchAt != nil && m.batchAt.Add(noticeBatchEvery).After(now) {
			if _, err := tx.Exec(ctx, `UPDATE deliveries SET run_at = $2 WHERE id = $1`, id, m.batchAt.Add(noticeBatchEvery)); err != nil {
				return err
			}
		}
	}
	return nil
}

// managerNoticeBody draws a manager_notice delivery when a channel takes it: the whole message
// for the first delivery and for an edit (with the external id of the first message), the new
// lines for a driver without edit. wait asks to try again shortly, while the first message is
// being sent; reason withholds the delivery.
func (c *Core) managerNoticeBody(ctx context.Context, tx pgx.Tx, now time.Time, id, incident, recipient string, body map[string]any) (out map[string]any, replaces, reason string, wait bool, err error) {
	of, _ := body["of"].(string)
	first := id
	if of != "" {
		first = of
	}
	var tenant, route string
	var edit bool
	var sentLines int
	err = tx.QueryRow(ctx, `SELECT tenant, route, edit, sent_lines FROM manager_notices WHERE delivery = $1 FOR UPDATE`, first).Scan(&tenant, &route, &edit, &sentLines)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, "", "ref_invalid", false, nil
	}
	if err != nil {
		return nil, "", "", false, err
	}
	var raw []byte
	r, err := scanIncidentLog(tx.QueryRow(ctx, `SELECT `+incidentCols+`, notice_log FROM incidents WHERE id = $1`, incident), &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, "", "ref_invalid", false, nil
	}
	if err != nil {
		return nil, "", "", false, err
	}
	var log []noticeEntry
	if err := json.Unmarshal(raw, &log); err != nil {
		return nil, "", "", false, err
	}
	var locale *string
	_ = tx.QueryRow(ctx, `SELECT locale FROM principals WHERE tenant = $1 AND id = $2`, tenant, recipient).Scan(&locale)
	loc := "en"
	if locale != nil && *locale != "" {
		loc = *locale
	}
	if of != "" {
		var state string
		var ext *string
		if err := tx.QueryRow(ctx, `SELECT state, external_id FROM deliveries WHERE id = $1`, of).Scan(&state, &ext); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, "", "", false, err
		}
		switch {
		case state == "planned" || state == "sending":
			return nil, "", "", true, nil
		case edit && (state != "delivered" || ext == nil):
			// The first message never arrived: there is nothing to edit.
			return nil, "", "ref_invalid", false, nil
		case edit:
			replaces = *ext
		}
	}
	if of == "" || edit {
		out = c.noticeWhole(loc, route, r, log)
	} else {
		if sentLines >= len(log) {
			return nil, "", "no_op", false, nil
		}
		out = c.noticeBatch(loc, route, r, log[sentLines:])
	}
	if _, err := tx.Exec(ctx, `UPDATE manager_notices SET sent_lines = $2, batch_at = $3 WHERE delivery = $1`, first, len(log), now); err != nil {
		return nil, "", "", false, err
	}
	return out, replaces, "", false, nil
}

func scanIncidentLog(row pgx.Row, raw *[]byte) (*incidentRow, error) {
	r := &incidentRow{}
	var rec []byte
	if err := row.Scan(&r.ID, &r.Tenant, &r.Class, &r.SubjectKind, &r.SubjectID, &r.Alarmkey, &r.Episode, &r.State, &rec,
		&r.Since, &r.Handled, &r.Unhandled, &r.OpenedAt, &r.ResolvedAt, &r.EndedAt, raw); err != nil {
		return nil, err
	}
	return r, json.Unmarshal(rec, &r.Record)
}

// noticeWhole is the whole message: the header, the latest lines that fit and the one revert
// command (MGR-15).
func (c *Core) noticeWhole(locale, route string, r *incidentRow, log []noticeEntry) map[string]any {
	ops := 0
	for _, e := range log {
		if e.Kind == "op" {
			ops++
		}
	}
	params := map[string]any{"incident": r.ID, "class": r.Class, "state": r.State, "opened": fmtTime(r.OpenedAt), "ops": ops}
	key := "notice.manager_notice.header_farm"
	if route == "tenant" {
		key = "notice.manager_notice.header"
		params["subject"] = r.SubjectKind
		if r.SubjectID != "" {
			params["subject"] = r.SubjectKind + " " + r.SubjectID
		}
	}
	return map[string]any{"form": "text_code", "text_key": "notice.manager_notice", "params": map[string]any{
		"header": cut(c.texts.Render(locale, key, params), 500),
		"lines":  c.noticeLines(locale, route, log),
		"revert": c.texts.Render(locale, "notice.manager_notice.revert", map[string]any{"incident": r.ID}),
	}}
}

// noticeBatch is the batch of new lines for a driver without edit.
func (c *Core) noticeBatch(locale, route string, r *incidentRow, log []noticeEntry) map[string]any {
	return map[string]any{"form": "text_code", "text_key": "notice.manager_notice.batch", "params": map[string]any{
		"incident": r.ID, "lines": c.noticeLines(locale, route, log)}}
}

// noticeLines renders the lines of the log, the latest that fit, after a count of the earlier
// ones. The farm route sees only the codes and the times, never a target or the subject.
func (c *Core) noticeLines(locale, route string, log []noticeEntry) string {
	var lines []string
	for _, e := range log {
		switch {
		case e.Kind == "op" && route == "tenant" && e.Target != nil:
			lines = append(lines, c.texts.Render(locale, "notice.manager_notice.op", map[string]any{
				"at": e.At, "op": e.Op, "target_kind": e.Target.Kind, "target": e.Target.ID, "by": e.By}))
		case e.Kind == "op":
			lines = append(lines, c.texts.Render(locale, "notice.manager_notice.op_farm", map[string]any{"at": e.At, "op": e.Op}))
		default:
			lines = append(lines, c.texts.Render(locale, "notice.manager_notice.state", map[string]any{"at": e.At, "state": e.State}))
		}
	}
	size, from := 0, len(lines)
	for from > 0 && size+len(lines[from-1])+1 <= noticeLinesMax-40 {
		size += len(lines[from-1]) + 1
		from--
	}
	out := lines[from:]
	if from > 0 {
		out = append([]string{c.texts.Render(locale, "notice.manager_notice.earlier", map[string]any{"n": from})}, out...)
	}
	return cut(strings.Join(out, "\n"), 500)
}

// noticeOpLine logs an applied operation in its basis incident (MGR-13).
func (c *Core) noticeOpLine(ctx context.Context, tx pgx.Tx, now time.Time, incident string, req ControlRequest, seat bool) error {
	by := "person"
	if seat {
		by = "seat"
	}
	t := req.Target
	return c.noticeLog(ctx, tx, now, incident, noticeEntry{Kind: "op", Op: req.Op, Target: &t, By: by}, true)
}
