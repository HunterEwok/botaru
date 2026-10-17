// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Publishing staged messages at complete, and the acknowledgement and return of read ones (MSG-9).

// publishMessages publishes what an attempt staged and acknowledges what it read, in the
// transaction of its complete (MSG-9, DLV-2). A hold with no_message at this moment discards
// the staged entries (held). An all delivery becomes a copy in the inbox of each recipient
// worker, at most 50; a copy without room is not made and the core writes an event (MSG-14).
func (c *Core) publishMessages(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, tenant, task string, lease int, worker string) error {
	var role string
	if err := tx.QueryRow(ctx, `SELECT role FROM workers WHERE id = $1`, worker).Scan(&role); err != nil {
		return err
	}
	s := &msgSender{Tenant: tenant, Role: role, Worker: worker, Task: task, Lease: lease}
	// What the attempt read is acknowledged, entries it returned with msg.nack are already back.
	if err := c.ackRead(ctx, tx, now, s); err != nil {
		return err
	}
	return c.publishRows(ctx, tx, now, cat, s, `sender_task = $1 AND sender_lease = $2`, task, lease)
}

// publishRows publishes the staged entries that cond selects, of an attempt or of the core: a
// held sender or group discards them, an all delivery becomes a copy per recipient worker, the
// rest are queued with their expiry; each publication writes the usage line of the sender's
// tree and runs the notification routes of its type (MSG-9, MSG-11, MSG-14, MSG-16).
func (c *Core) publishRows(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, s *msgSender, cond string, args ...any) error {
	tenant, task := s.Tenant, s.Task
	tv := cat.Tenants[tenant]
	type staged struct {
		seq            int64
		id, typ, inbox string
		delivery       string
		env            map[string]any
		note           *string
	}
	rows, err := tx.Query(ctx, `SELECT seq, id, type, inbox_kind || ':' || inbox_name, delivery, envelope, note FROM messages
		WHERE `+cond+` AND state = 'staged' ORDER BY seq FOR UPDATE`, args...)
	if err != nil {
		return err
	}
	var list []staged
	for rows.Next() {
		var st staged
		var raw []byte
		if err := rows.Scan(&st.seq, &st.id, &st.typ, &st.inbox, &st.delivery, &raw, &st.note); err != nil {
			rows.Close()
			return err
		}
		if err := json.Unmarshal(raw, &st.env); err != nil {
			rows.Close()
			return err
		}
		list = append(list, st)
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(list) == 0 {
		return err
	}
	scopes := msgSenderScopes(s)
	held, err := heldBy(ctx, tx, now, []string{"no_message"}, scopes...)
	if err != nil {
		return err
	}
	for _, st := range list {
		gheld := held
		if kind, name, _ := strings.Cut(st.inbox, ":"); gheld == "" && kind == "group" {
			if gheld, err = heldBy(ctx, tx, now, []string{"no_message"}, scopeRef{kind: "bot_group", tenant: tenant, name: name}); err != nil {
				return err
			}
		}
		if gheld != "" {
			if _, err := tx.Exec(ctx, `UPDATE messages SET state = 'discarded', reason = 'held', note = NULL, updated_at = $2 WHERE seq = $1`, st.seq, now); err != nil {
				return err
			}
			if err := c.msgAudit(ctx, tx, now, s, "msg.discard", st.id, true, "held", map[string]any{"type": st.typ, "to": st.inbox}); err != nil {
				return err
			}
			continue
		}
		ttl := time.Duration(intOf(st.env["ttl_s"])) * time.Second
		if ttl <= 0 {
			ttl = reportTTLDefault
		}
		expires := now.Add(ttl)
		st.env["time"], st.env["expiresat"] = fmtTime(now), fmtTime(expires)
		delete(st.env, "ttl_s")
		recipients := 1
		if st.delivery == "all" {
			ws, err := allRecipients(ctx, tx, cat, tv, st.inbox, st.typ)
			if err != nil {
				return err
			}
			recipients = 0
			for _, wid := range ws {
				in := msgInbox{Kind: "worker", Name: wid}
				if err := lockInbox(ctx, tx, tenant, in); err != nil {
					return err
				}
				all, mine, err := inboxLoad(ctx, tx, tenant, in, s.Role)
				if err != nil {
					return err
				}
				// A copy keeps to the inbox's capacity and the sending role's share of it (MSG-15).
				if reason := ""; all >= in.capacity() || mine >= in.capacity()/4 {
					reason = "inbox_full"
					if all < in.capacity() {
						reason = "sender_quota"
					}
					if err := c.appendEvent(ctx, tx, now, tenant, Event{Type: "message.rejected", Subject: st.id,
						Data: map[string]any{"message": st.id, "type": st.typ, "inbox": map[string]any{"kind": "worker", "name": strings.ToLower(wid)}, "reason": reason}}); err != nil {
						return err
					}
					continue
				}
				// Only the core writes a worker: address (MSG-4, D-32).
				st.env["msgto"] = "worker:" + strings.ToLower(wid)
				raw, err := json.Marshal(st.env)
				if err != nil {
					return err
				}
				if _, err := tx.Exec(ctx, `INSERT INTO messages (id, tenant, inbox_kind, inbox_name, type, kind, delivery, state, envelope, note, from_role, from_worker,
					sender_task, sender_lease, idem_key, data_mark, untrusted, hops, expires_at, created_at, updated_at)
					SELECT id, tenant, 'worker', $2, type, kind, delivery, 'queued', $3, note, from_role, from_worker, sender_task, sender_lease, idem_key, data_mark,
						untrusted, hops, $4, created_at, $5 FROM messages WHERE seq = $1`, st.seq, wid, raw, expires, now); err != nil {
					return err
				}
				recipients++
			}
			// The staged row stood for the address; its copies are the entries now.
			if _, err := tx.Exec(ctx, `DELETE FROM messages WHERE seq = $1`, st.seq); err != nil {
				return err
			}
		} else {
			raw, err := json.Marshal(st.env)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE messages SET state = 'queued', envelope = $2, expires_at = $3, updated_at = $4 WHERE seq = $1`, st.seq, raw, expires, now); err != nil {
				return err
			}
		}
		if err := c.appendEvent(ctx, tx, now, tenant, Event{Type: "message.published", Subject: st.id, Causation: task,
			Data: map[string]any{"message": st.id, "type": st.typ, "to": st.inbox, "recipients": recipients}}); err != nil {
			return err
		}
		if err := c.msgAudit(ctx, tx, now, s, "msg.publish", st.id, true, "", map[string]any{"type": st.typ, "to": st.inbox, "recipients": recipients}); err != nil {
			return err
		}
		if err := c.messageUsage(ctx, tx, now, s, st.id, st.typ, st.env, st.note); err != nil {
			return err
		}
		if err := c.notifyRoutes(ctx, tx, now, cat, tenant, st.id, st.typ, st.env); err != nil {
			return err
		}
	}
	return nil
}

func intOf(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	}
	return 0
}

// ackRead acknowledges the entries an attempt read, in the transaction of its complete (MSG-9).
func (c *Core) ackRead(ctx context.Context, tx pgx.Tx, now time.Time, s *msgSender) error {
	rows, err := tx.Query(ctx, `UPDATE messages SET state = 'acked', note = NULL, handle = NULL, updated_at = $3
		WHERE reader_task = $1 AND reader_lease = $2 AND state = 'leased' RETURNING id, type`, s.Task, s.Lease, now)
	if err != nil {
		return err
	}
	type acked struct{ id, typ string }
	var list []acked
	for rows.Next() {
		var a acked
		if err := rows.Scan(&a.id, &a.typ); err != nil {
			rows.Close()
			return err
		}
		list = append(list, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, a := range list {
		if err := c.msgAudit(ctx, tx, now, s, "msg.ack", a.id, true, "", map[string]any{"type": a.typ}); err != nil {
			return err
		}
	}
	return nil
}

// countedReturn says whether the end of a reader's lease counts a hand-out of what it read: a
// fail, a lost lease and a stall do; a cancellation, a preemption, a hold, a drain, a revoke
// and a lease that ran out while the core was down do not (MSG-9, FEN-5).
func countedReturn(leaseState, reason string) bool {
	switch leaseState {
	case "closed":
		return true
	case "expired":
		return reason != "core_unavailable"
	case "revoked":
		return reason == "stalled"
	}
	return false
}

// settleMessages ends the message part of the attempts whose lease is no longer active, of one
// task or, with task "", of all: what such an attempt staged is discarded, and what it read
// returns to its inbox (MSG-9). A counted return past the third hand-out is dead
// (delivery_exhausted); an entry past its expiry is expired (MSG-12).
func (c *Core) settleMessages(ctx context.Context, tx pgx.Tx, now time.Time, task string) error {
	// What else the ended attempt staged is not published either (MGR-5, MGR-21).
	if err := dropStaged(ctx, tx, now, task); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `SELECT m.seq, m.id, m.tenant, m.type, m.state, coalesce(l.state, 'gone'), coalesce(l.reason, ''), m.handouts, m.expires_at,
			coalesce(m.sender_task, m.reader_task), coalesce(m.sender_lease, m.reader_lease), coalesce(l.worker_id, '')
		FROM messages m LEFT JOIN leases l ON l.work_id = CASE WHEN m.state = 'staged' THEN m.sender_task ELSE m.reader_task END
			AND l.lease_no = CASE WHEN m.state = 'staged' THEN m.sender_lease ELSE m.reader_lease END
		WHERE (m.state = 'staged' AND ($1 = '' OR m.sender_task = $1) OR m.state = 'leased' AND ($1 = '' OR m.reader_task = $1))
			AND coalesce(l.state, 'gone') <> 'active'
		ORDER BY m.seq LIMIT 500 FOR UPDATE OF m SKIP LOCKED`, task)
	if err != nil {
		return err
	}
	type row struct {
		seq                                int64
		id, tenant, typ, state, ls, reason string
		handouts                           int
		expires                            *time.Time
		task                               string
		lease                              int
		worker                             string
	}
	var list []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.seq, &r.id, &r.tenant, &r.typ, &r.state, &r.ls, &r.reason, &r.handouts, &r.expires, &r.task, &r.lease, &r.worker); err != nil {
			rows.Close()
			return err
		}
		list = append(list, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, r := range list {
		s := &msgSender{Tenant: r.tenant, Worker: r.worker, Task: r.task, Lease: r.lease}
		if r.state == "staged" {
			reason := "attempt_ended"
			if r.reason == "held" {
				reason = "held"
			}
			if _, err := tx.Exec(ctx, `UPDATE messages SET state = 'discarded', reason = $2, note = NULL, updated_at = $3 WHERE seq = $1`, r.seq, reason, now); err != nil {
				return err
			}
			if err := c.msgAudit(ctx, tx, now, s, "msg.discard", r.id, true, reason, map[string]any{"type": r.typ}); err != nil {
				return err
			}
			continue
		}
		if err := c.returnEntry(ctx, tx, now, s, r.seq, r.id, r.typ, r.handouts, r.expires, countedReturn(r.ls, r.reason)); err != nil {
			return err
		}
	}
	return nil
}

// returnEntry puts a leased entry back into its inbox: a counted return raises the hand-out
// count and the third makes the entry dead; an entry whose period ended is expired (MSG-9,
// MSG-12).
func (c *Core) returnEntry(ctx context.Context, tx pgx.Tx, now time.Time, s *msgSender, seq int64, id, typ string, handouts int, expires *time.Time, counted bool) error {
	if counted {
		handouts++
	}
	state, reason, event := "queued", "", ""
	switch {
	case expires != nil && !expires.After(now):
		state, event = "expired", "message.expired"
	case handouts >= maxHandouts:
		state, reason, event = "dead", "delivery_exhausted", "message.dead"
	}
	note := "note"
	if state != "queued" {
		note = "NULL"
	}
	if _, err := tx.Exec(ctx, `UPDATE messages SET state = $2, reason = $3, handouts = $4, reader_task = NULL, reader_lease = NULL, handle = NULL,
		nacked = false, note = `+note+`, updated_at = $5 WHERE seq = $1`, seq, state, nullable(reason), handouts, now); err != nil {
		return err
	}
	if event != "" {
		if err := c.appendEvent(ctx, tx, now, s.Tenant, Event{Type: event, Subject: id, Data: map[string]any{"message": id, "type": typ, "reason": reason}}); err != nil {
			return err
		}
	}
	cmd := "msg.return"
	if state != "queued" {
		cmd = "msg." + state
	}
	return c.msgAudit(ctx, tx, now, s, cmd, id, true, reason, map[string]any{"type": typ, "handouts": handouts, "counted": counted})
}
