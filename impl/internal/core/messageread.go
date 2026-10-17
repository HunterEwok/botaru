// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/ids"
)

// Reading an inbox under a lease, the inbox loop and the inboxes of departed workers.

// msgReader is the reading attempt: its worker, the inboxes it reads and the types its task
// type and role allow (MSG-6, MSG-8).
type msgReader struct {
	*msgSender
	Inboxes []msgInbox
	Types   []string
	// NoState keeps state values out, as for a reporter (REP-8).
	NoState bool
}

// attemptReader builds the reader of an attempt: the inbox of its role, of its bot groups and,
// for a role with instance_inbox, its own; the types are the task type's inbox.read that the
// role receives, narrowed by the asked types.
func (c *Core) attemptReader(ctx context.Context, tx pgx.Tx, cat *Catalog, s *msgSender, typ string, asked []string) (*msgReader, error) {
	tv := cat.Tenants[s.Tenant]
	role := cat.Roles[s.Role]
	_, groups, err := workerGroups(ctx, tx, tv, s.Worker)
	if err != nil {
		return nil, err
	}
	r := &msgReader{msgSender: s, Inboxes: []msgInbox{{"role", s.Role}}, NoState: role.Profile == "reporter"}
	for _, g := range groups {
		r.Inboxes = append(r.Inboxes, msgInbox{"group", g})
	}
	if role.HasInstanceInbox() {
		r.Inboxes = append(r.Inboxes, msgInbox{"worker", s.Worker})
	}
	for _, t := range cat.TaskTypes[typ].Inbox.Read {
		if role.Receives(t) && (len(asked) == 0 || slices.Contains(asked, t)) {
			r.Types = append(r.Types, t)
		}
	}
	return r, nil
}

// readInbox hands entries to a reading attempt under its lease (MSG-6, MSG-7, MSG-8, MSG-11):
// first those it already holds, so a repeat returns the same entries, then queued ones in number
// order. An entry above the attempt's mark is not handed out and counts in hidden. Reading an
// untrusted entry sets untrusted_seen on the attempt with its source, the entry's hops raise the
// attempt's depth floor and its initiators join the attempt's initiators; the mark does not
// change. State values come without a lease, marked stale when old.
func (c *Core) readInbox(ctx context.Context, tx pgx.Tx, now time.Time, r *msgReader, limit int) (*MsgReceiveResult, error) {
	out := &MsgReceiveResult{Records: []map[string]any{}}
	if len(r.Types) == 0 || limit <= 0 {
		return out, nil
	}
	limit = min(limit, maxInboxRead)
	kinds := make([]string, len(r.Inboxes))
	names := make([]string, len(r.Inboxes))
	for i, in := range r.Inboxes {
		kinds[i], names[i] = in.Kind, in.Name
	}
	ownOrder, ok := c.markOrder(r.Mark)
	above := func(mark string) bool {
		if r.Mark == "" {
			return false // code outside an attempt reads state values of its role's types
		}
		o, k := c.markOrder(mark)
		return !ok || !k || o > ownOrder
	}
	type entry struct {
		seq                        int64
		id, typ, kind, state, from string
		handle                     *string
		env                        map[string]any
		note                       *string
		mark                       string
		untrusted                  bool
		hops                       int
		created                    time.Time
	}
	scan := func(rows pgx.Rows) ([]entry, error) {
		defer rows.Close()
		var list []entry
		for rows.Next() {
			var e entry
			var raw []byte
			if err := rows.Scan(&e.seq, &e.id, &e.typ, &e.kind, &e.state, &e.from, &e.handle, &raw, &e.note, &e.mark, &e.untrusted, &e.hops, &e.created); err != nil {
				return nil, err
			}
			if err := json.Unmarshal(raw, &e.env); err != nil {
				return nil, err
			}
			list = append(list, e)
		}
		return list, rows.Err()
	}
	const cols = `seq, id, type, kind, state, from_role, handle, envelope, note, data_mark, untrusted, hops, created_at`
	held, err := tx.Query(ctx, `SELECT `+cols+` FROM messages WHERE reader_task = $1 AND reader_lease = $2 AND state = 'leased' ORDER BY seq LIMIT $3`,
		r.Task, r.Lease, limit)
	if err != nil {
		return nil, err
	}
	list, err := scan(held)
	if err != nil {
		return nil, err
	}
	if len(list) < limit {
		// The marks the reader may see are chosen first, so entries above its mark neither
		// fill the read nor hide the ones behind them; they only count in hidden.
		const live = `FROM messages m JOIN unnest($2::text[], $3::text[]) AS i(k, n) ON m.inbox_kind = i.k AND m.inbox_name = i.n
			WHERE m.tenant = $1 AND m.state = 'queued' AND m.type = ANY($4) AND (m.expires_at IS NULL OR m.expires_at > $5)`
		mrows, err := tx.Query(ctx, `SELECT DISTINCT m.data_mark `+live, r.Tenant, kinds, names, r.Types, now)
		if err != nil {
			return nil, err
		}
		present, err := pgx.CollectRows(mrows, pgx.RowTo[string])
		if err != nil {
			return nil, err
		}
		allowed := slices.DeleteFunc(present, above)
		var hidden int
		if err := tx.QueryRow(ctx, `SELECT count(*) `+live+` AND NOT (m.data_mark = ANY($6))`, r.Tenant, kinds, names, r.Types, now, allowed).Scan(&hidden); err != nil {
			return nil, err
		}
		out.Hidden += hidden
		rows, err := tx.Query(ctx, `SELECT `+cols+` `+live+` AND m.data_mark = ANY($6) ORDER BY m.seq LIMIT $7 FOR UPDATE OF m SKIP LOCKED`,
			r.Tenant, kinds, names, r.Types, now, allowed, limit-len(list))
		if err != nil {
			return nil, err
		}
		fresh, err := scan(rows)
		if err != nil {
			return nil, err
		}
		for _, e := range fresh {
			if above(e.mark) {
				out.Hidden++
				continue
			}
			if len(list) >= limit {
				continue
			}
			h := ids.Secret(18)
			e.handle = &h
			if _, err := tx.Exec(ctx, `UPDATE messages SET state = 'leased', reader_task = $2, reader_lease = $3, handle = $4, updated_at = $5 WHERE seq = $1`,
				e.seq, r.Task, r.Lease, h, now); err != nil {
				return nil, err
			}
			if err := c.msgAudit(ctx, tx, now, r.msgSender, "msg.lease", e.id, true, "", map[string]any{"type": e.typ, "from_role": e.from}); err != nil {
				return nil, err
			}
			list = append(list, e)
		}
	}
	if !r.NoState && len(list) < limit {
		rows, err := tx.Query(ctx, `SELECT `+cols+` FROM messages m JOIN unnest($2::text[], $3::text[]) AS i(k, n) ON m.inbox_kind = i.k AND m.inbox_name = i.n
			WHERE m.tenant = $1 AND m.state IN ('current', 'stale') AND m.type = ANY($4) ORDER BY m.seq LIMIT $5`, r.Tenant, kinds, names, r.Types, limit-len(list))
		if err != nil {
			return nil, err
		}
		states, err := scan(rows)
		if err != nil {
			return nil, err
		}
		for _, e := range states {
			if above(e.mark) {
				out.Hidden++
				continue
			}
			h := ids.Secret(18)
			e.handle = &h
			list = append(list, e)
		}
	}
	// What was read narrows the reader: the flag, the depth floor and the initiators (MSG-6,
	// MSG-7, MSG-11).
	var srcs []string
	floor := 0
	var inits []string
	for _, e := range list {
		if e.untrusted {
			srcs = append(srcs, "message of type "+e.typ+" from role "+e.from)
		}
		floor = max(floor, e.hops)
		for _, i := range asStrings(e.env["origininitiators"]) {
			if i != r.Initiator && !slices.Contains(inits, i) {
				inits = append(inits, i)
			}
		}
	}
	if r.Task != "" {
		if err := markTaskRead(ctx, tx, r.Task, srcs, floor, inits); err != nil {
			return nil, err
		}
	}
	for _, e := range list {
		rec := map[string]any{"handle": *e.handle, "type": e.typ, "kind": e.kind, "from_role": e.from, "sent_at": fmtTime(e.created),
			"fields": mmapAny(mmapAny(e.env["data"])["fields"]), "untrusted": e.untrusted}
		if t, err := time.Parse(time.RFC3339Nano, asString(e.env["time"])); err == nil {
			rec["sent_at"] = fmtTime(t)
		}
		if sub := asString(e.env["subject"]); sub != "" {
			rec["subject"] = sub
		}
		if e.note != nil && *e.note != "" {
			rec["note"] = *e.note
		}
		if e.kind == "state" {
			rec["stale"] = e.state == "stale"
		}
		out.Records = append(out.Records, rec)
	}
	return out, nil
}

func asStrings(v any) []string {
	var out []string
	switch l := v.(type) {
	case []any:
		for _, x := range l {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
	case []string:
		out = l
	}
	return out
}

// markTaskRead raises a reading task's record: untrusted_seen with the sources, the depth floor
// and the initiators of what it read (MSG-6, MSG-7, MSG-11, INJ-13).
func markTaskRead(ctx context.Context, tx pgx.Tx, task string, srcs []string, floor int, inits []string) error {
	if len(srcs) == 0 && floor == 0 && len(inits) == 0 {
		return nil
	}
	var raw []byte
	if err := tx.QueryRow(ctx, `SELECT record FROM tasks WHERE id = $1 FOR UPDATE`, task).Scan(&raw); err != nil {
		return err
	}
	var rec map[string]any
	if err := json.Unmarshal(raw, &rec); err != nil {
		return err
	}
	if len(srcs) > 0 {
		rec["untrusted_seen"] = true
		if asString(rec["untrusted_source"]) == "" {
			rec["untrusted_source"] = "message"
		}
		all := asStrings(rec["untrusted_sources"])
		for _, s := range srcs {
			if !slices.Contains(all, s) && len(all) < 32 {
				all = append(all, s)
			}
		}
		rec["untrusted_sources"] = all
	}
	if floor > intOf(rec["depth_floor"]) {
		rec["depth_floor"] = floor
	}
	if len(inits) > 0 {
		all := asStrings(rec["co_initiators"])
		for _, i := range inits {
			if !slices.Contains(all, i) && len(all) < 31 {
				all = append(all, i)
			}
		}
		rec["co_initiators"] = all
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE tasks SET record = $2 WHERE id = $1`, task, b)
	return err
}

// readState returns the current state values that worker code outside an attempt may read:
// types of genus state that its role receives, without a lease (MSG-7, P1 msg.receive).
func (c *Core) readState(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, w *Worker, req MsgReceiveRequest) (*MsgReceiveResult, error) {
	role := cat.Roles[w.Role]
	r := &msgReader{msgSender: &msgSender{Tenant: w.Tenant, Role: w.Role, Worker: w.ID}, Inboxes: []msgInbox{{"role", w.Role}}}
	for _, x := range role.Messages.Receive {
		if mt, ok := cat.MessageTypes[x.Type]; ok && mt.Kind == "state" && (len(req.Types) == 0 || slices.Contains(req.Types, x.Type)) {
			r.Types = append(r.Types, x.Type)
		}
	}
	return c.readInbox(ctx, tx, now, r, req.Max)
}

// nackEntries returns entries the attempt read to their inbox at once, counting the hand-out
// (MSG-9); ackEntries only checks the handles: the acknowledgement happens at complete.
func (c *Core) nackEntries(ctx context.Context, tx pgx.Tx, now time.Time, s *msgSender, handles []string, nack bool) (*MsgAckResult, error) {
	out := &MsgAckResult{}
	for _, h := range handles {
		var seq int64
		var id, typ string
		var handouts int
		var expires *time.Time
		err := tx.QueryRow(ctx, `SELECT seq, id, type, handouts, expires_at FROM messages WHERE handle = $1 AND reader_task = $2 AND reader_lease = $3 AND state = 'leased' FOR UPDATE`,
			h, s.Task, s.Lease).Scan(&seq, &id, &typ, &handouts, &expires)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if nack {
			if err := c.returnEntry(ctx, tx, now, s, seq, id, typ, handouts, expires, true); err != nil {
				return nil, err
			}
		}
		out.Count++
	}
	return out, nil
}

// inboxLoop is the background loop inbox (MSG-9, MSG-12): it settles the entries of attempts
// that ended without complete, expires queued entries past their period and marks state values
// stale. An expired entry is no longer handed out, wakes nobody and loses its note.
func (c *Core) inboxLoop(ctx context.Context) error {
	if err := c.tx(ctx, func(tx pgx.Tx, now time.Time) error { return c.settleMessages(ctx, tx, now, "") }); err != nil {
		return err
	}
	if err := c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		rows, err := tx.Query(ctx, `UPDATE messages SET state = 'expired', note = NULL, updated_at = $1
			WHERE seq IN (SELECT seq FROM messages WHERE state = 'queued' AND expires_at <= $1 ORDER BY expires_at LIMIT 500 FOR UPDATE SKIP LOCKED)
			RETURNING id, tenant, type`, now)
		if err != nil {
			return err
		}
		type ex struct{ id, tenant, typ string }
		var list []ex
		for rows.Next() {
			var e ex
			if err := rows.Scan(&e.id, &e.tenant, &e.typ); err != nil {
				rows.Close()
				return err
			}
			list = append(list, e)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, e := range list {
			if err := c.appendEvent(ctx, tx, now, e.tenant, Event{Type: "message.expired", Subject: e.id, Data: map[string]any{"message": e.id, "type": e.typ}}); err != nil {
				return err
			}
			if err := c.msgAudit(ctx, tx, now, &msgSender{Tenant: e.tenant}, "msg.expired", e.id, true, "expired", map[string]any{"type": e.typ}); err != nil {
				return err
			}
		}
		_, err = tx.Exec(ctx, `UPDATE messages SET state = 'stale', updated_at = $1 WHERE state = 'current' AND expires_at <= $1`, now)
		return err
	}); err != nil {
		return err
	}
	// The wake-up routes see the inboxes as they are after the expiry (MSG-12, MSG-16).
	return c.wakeRoutes(ctx)
}

// workerGone ends what the inbox of a revoked worker holds (recipient_gone) and removes its
// state values (spec/lifecycles/message.md).
func (c *Core) workerGone(ctx context.Context, tx pgx.Tx, now time.Time, tenant, worker string) error {
	rows, err := tx.Query(ctx, `UPDATE messages SET state = 'discarded', reason = 'recipient_gone', note = NULL, updated_at = $3
		WHERE tenant = $1 AND inbox_kind = 'worker' AND inbox_name = $2 AND state = 'queued' RETURNING id, type`, tenant, worker, now)
	if err != nil {
		return err
	}
	type gone struct{ id, typ string }
	var list []gone
	for rows.Next() {
		var g gone
		if err := rows.Scan(&g.id, &g.typ); err != nil {
			rows.Close()
			return err
		}
		list = append(list, g)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, g := range list {
		if err := c.appendEvent(ctx, tx, now, tenant, Event{Type: "message.discarded", Subject: g.id, Data: map[string]any{"message": g.id, "type": g.typ, "reason": "recipient_gone"}}); err != nil {
			return err
		}
	}
	_, err = tx.Exec(ctx, `UPDATE messages SET state = 'removed', updated_at = $3 WHERE tenant = $1 AND from_worker = $2 AND state IN ('current', 'stale')`, tenant, worker, now)
	return err
}

// inboxesGone ends the live entries of inboxes the applied catalog no longer has: a role that
// is gone or no longer receives the type, a bot group that is gone. A report entry is discarded
// and a state value removed, both with recipient_gone (MSG-12, MSG-14).
func (c *Core) inboxesGone(ctx context.Context, tx pgx.Tx, now time.Time, next *Catalog) error {
	rows, err := tx.Query(ctx, `SELECT DISTINCT tenant, inbox_kind, inbox_name, type FROM messages
		WHERE inbox_kind <> 'worker' AND state IN ('staged', 'queued', 'leased', 'current', 'stale')`)
	if err != nil {
		return err
	}
	type inbox struct{ tenant, kind, name, typ string }
	var gone []inbox
	for rows.Next() {
		var in inbox
		if err := rows.Scan(&in.tenant, &in.kind, &in.name, &in.typ); err != nil {
			rows.Close()
			return err
		}
		if tv := next.Tenants[in.tenant]; tv == nil || len(receivingRoles(next, tv, in.kind+":"+in.name, in.typ)) == 0 {
			gone = append(gone, in)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, in := range gone {
		if _, err := tx.Exec(ctx, `UPDATE messages SET state = CASE WHEN kind = 'state' THEN 'removed' ELSE 'discarded' END, reason = 'recipient_gone',
			note = NULL, handle = NULL, updated_at = $5
			WHERE tenant = $1 AND inbox_kind = $2 AND inbox_name = $3 AND type = $4 AND state IN ('staged', 'queued', 'leased', 'current', 'stale')`,
			in.tenant, in.kind, in.name, in.typ, now); err != nil {
			return err
		}
	}
	return nil
}
