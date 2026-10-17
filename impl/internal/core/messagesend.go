// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/ids"
	"github.com/HunterEwok/botaru/impl/internal/jcs"
	"github.com/HunterEwok/botaru/impl/internal/manifest"
)

// Sending a bot message: the checks, the inbox room and the staged rows (MSG-1, MSG-13).

// sendMessage checks and stages a report message, or answers a refusal (MSG-1, MSG-4, MSG-5,
// MSG-6, MSG-11, MSG-13, MSG-15). The refusal is audited and the store keeps no row.
func (c *Core) sendMessage(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, s *msgSender, req MsgSendRequest) (*MsgSendResult, *Problem, error) {
	tv := cat.Tenants[s.Tenant]
	deny := func(reason, detail string) (*MsgSendResult, *Problem, error) {
		meta := map[string]any{"type": req.Type}
		if req.To != "" {
			meta["to"] = req.To
		}
		if err := c.msgAudit(ctx, tx, now, s, "msg.send", "", false, reason, meta); err != nil {
			return nil, nil, err
		}
		return nil, refuse(reason, "%s", detail), nil
	}
	mt, ok := cat.MessageTypes[req.Type]
	if !ok || tv == nil || !slices.Contains(mt.Producers, s.Producer) {
		return deny("msg_type_not_allowed", "the type is unknown or not produced by this sender")
	}
	if mt.Kind != "report" {
		// A state value is sent only by worker code with its own token (STA-1).
		return deny("msg_type_not_allowed", "only worker code sends a state value")
	}
	role := cat.Roles[s.Role]
	var allowed []string
	for _, e := range role.Messages.Send {
		if e.Type == req.Type {
			allowed = append(allowed, e.To...)
		}
	}
	if s.Producer != "core" && (len(allowed) == 0 || role.Profile == "reporter") {
		return deny("msg_type_not_allowed", "the role does not send this type")
	}
	if (req.To == "") == (req.ReplyTo == "") {
		return deny("address_invalid", "a report has exactly one of to and reply_to")
	}
	// The address: only role: or group: from the role's messages.send, or a reply to an entry
	// this attempt read, which goes to its sender role; a foreign, missing or non-accepting
	// address gets the same answer (MSG-4).
	addr, replyTo := req.To, ""
	if req.ReplyTo != "" {
		var from, id string
		err := tx.QueryRow(ctx, `SELECT from_role, id FROM messages WHERE handle = $1 AND tenant = $2 AND reader_task = $3 AND reader_lease = $4 AND state = 'leased'`,
			req.ReplyTo, s.Tenant, s.Task, s.Lease).Scan(&from, &id)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, nil, err
		}
		if err != nil || from == "core" {
			return deny("address_invalid", "the address is not valid")
		}
		addr, replyTo = "role:"+from, id
	}
	if !strings.HasPrefix(addr, "role:") && !strings.HasPrefix(addr, "group:") || s.Producer != "core" && !slices.Contains(allowed, addr) {
		return deny("address_invalid", "the address is not valid")
	}
	if len(receivingRoles(cat, tv, addr, req.Type)) == 0 {
		return deny("address_invalid", "the address is not valid")
	}
	if mt.Delivery == "all" && len(slices.DeleteFunc(receivingRoles(cat, tv, addr, req.Type), func(rn string) bool { return !cat.Roles[rn].HasInstanceInbox() })) == 0 {
		return deny("address_invalid", "the address is not valid")
	}
	// The initiator of the attempt and every initiator of what it read have msg.send:<type>;
	// system through system-ops (MSG-5, MSG-7). A route's auto-report is sent by system for the
	// ended task, so system needs it as well as the task's initiators.
	if s.Task != "" || s.TreeTask != "" {
		who := append([]string{s.Initiator}, s.Initiators...)
		if s.Task == "" && s.Initiator != "system" {
			who = append(who, "system")
		}
		ok, err := initiatorsHave(ctx, tx, now, tv, "msg.send:"+req.Type, who...)
		if err != nil {
			return nil, nil, err
		}
		if !ok {
			return deny("msg_type_not_allowed", "an initiator has no msg.send for this type")
		}
	}
	// A hold with no_message on the sender's scope or on a group address (HLD-3).
	scopes := msgSenderScopes(s)
	if kind, name, _ := strings.Cut(addr, ":"); kind == "group" {
		scopes = append(scopes, scopeRef{kind: "bot_group", tenant: s.Tenant, name: name})
	}
	if held, err := heldBy(ctx, tx, now, []string{"no_message"}, scopes...); err != nil {
		return nil, nil, err
	} else if held != "" {
		return deny("held", "a hold forbids messages")
	}
	fields, size, p, err := c.checkMsgFields(cat, s, mt, req.Fields)
	if err != nil {
		return nil, nil, err
	}
	if p != nil {
		return deny(p.Reason, p.Detail)
	}
	if req.Note != "" && (mt.Note.MaxChars <= 0 || utf8.RuneCountInString(req.Note) > min(mt.Note.MaxChars, maxNoteChars)) {
		return deny("note_not_allowed", "the type allows no note of this length")
	}
	// The class is not lowered through a message (MSG-6).
	if to, ok := c.markOrder(mt.DataClass); s.Mark != "" && (!ok || func() bool { o, k := c.markOrder(s.Mark); return !k || o > to }()) {
		return deny("data_class_exceeded", "the type's class is below the attempt's mark")
	}
	hops := s.Depth + 1
	if hops > maxHops {
		return deny("hops_exceeded", "the message would pass more than five hops")
	}
	// Idempotency: the sender's own key, or the core key of the n-th send of the attempt (MSG-9).
	var sent int
	if s.Task != "" {
		// The sends of one attempt are counted one at a time, so parallel calls neither share
		// a key nor pass the limit together.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(7300120, hashtext($1 || ':' || $2))`, s.Task, strconv.Itoa(s.Lease)); err != nil {
			return nil, nil, err
		}
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM messages WHERE sender_task = $1 AND sender_lease = $2`, s.Task, s.Lease).Scan(&sent); err != nil {
			return nil, nil, err
		}
	}
	id := ids.New(ids.Message)
	key, own := fmt.Sprintf("system:msg:%s:%d:%d", s.Task, s.Lease, sent+1), s.Key
	if s.Task == "" {
		// The core's own message without a key of its own is a message of its own.
		key = "system:msg:" + id
	}
	if req.IdempotencyKey != nil && *req.IdempotencyKey != "" {
		own = strings.ToLower(s.Worker) + ":" + *req.IdempotencyKey
	}
	if own != "" {
		key = own
		var id, state string
		var recipients *int
		err := tx.QueryRow(ctx, `SELECT id, state, (envelope->>'recipients')::int FROM messages WHERE tenant = $1 AND idem_key = $2 ORDER BY seq LIMIT 1`,
			s.Tenant, key).Scan(&id, &state, &recipients)
		if err == nil {
			if state != "staged" && state != "discarded" {
				state = "queued"
			}
			return &MsgSendResult{MessageID: id, State: state, Recipients: recipients}, nil, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return nil, nil, err
		}
	}
	if s.Task != "" && sent >= maxSendsPerAttempt {
		return deny("service_rate_limited", "an attempt sends at most three messages")
	}
	// The rate per (tenant, sender role, type): the type's and at most the farm's (MSG-15).
	rate := farmMsgRatePerHour
	if r := mt.Rate.PerHourPerSender; r > 0 {
		rate = min(rate, r)
	}
	// The rate is counted under a lock of (tenant, role, type), so parallel sends of the role do
	// not pass it together.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(7300122, hashtext($1 || ':' || $2 || ':' || $3))`, s.Tenant, s.Role, req.Type); err != nil {
		return nil, nil, err
	}
	var lastHour int
	if err := tx.QueryRow(ctx, `SELECT count(DISTINCT id) FROM messages WHERE tenant = $1 AND from_role = $2 AND type = $3 AND created_at > $4`,
		s.Tenant, s.Role, req.Type, now.Add(-time.Hour)).Scan(&lastHour); err != nil {
		return nil, nil, err
	}
	if lastHour >= rate {
		if due, err := markOnce(ctx, tx, now, s.Tenant, "rate:"+s.Role+":"+req.Type, time.Hour); err != nil {
			return nil, nil, err
		} else if due {
			if err := c.appendEvent(ctx, tx, now, s.Tenant, Event{Type: "message.rejected", Subject: req.Type,
				Data: map[string]any{"type": req.Type, "role": s.Role, "reason": "service_rate_limited"}}); err != nil {
				return nil, nil, err
			}
		}
		return deny("service_rate_limited", "the type's rate is spent")
	}
	in := msgInbox{}
	in.Kind, in.Name, _ = strings.Cut(addr, ":")
	var recipients *int
	if mt.Delivery == "all" {
		rs, err := allRecipients(ctx, tx, cat, tv, addr, req.Type)
		if err != nil {
			return nil, nil, err
		}
		n := len(rs)
		recipients = &n
	} else if p, err := c.inboxRoom(ctx, tx, now, s.Tenant, in, s.Role); err != nil {
		return nil, nil, err
	} else if p != nil {
		return deny(p.Reason, p.Detail)
	}
	// The flag and its sources: the attempt's untrusted_seen, a note, the model writing the
	// fields (MSG-6, MSG-13).
	untrusted, srcs := s.Untrusted, slices.Clone(s.UntrustedOf)
	if req.Note != "" {
		untrusted, srcs = true, append(srcs, "note")
	}
	origin := "code"
	if s.Producer == "agent" || s.ModelFields {
		untrusted, srcs, origin = true, append(srcs, "model"), "model"
	}
	mark := s.Mark
	if mark == "" {
		mark = mt.DataClass
	}
	source := "/core"
	if s.Worker != "" {
		source = "/w/" + strings.ToLower(s.Worker)
	}
	initiators := []string{}
	if s.Initiator != "" {
		initiators = append(initiators, s.Initiator)
	}
	for _, i := range s.Initiators {
		if !slices.Contains(initiators, i) {
			initiators = append(initiators, i)
		}
	}
	ttl := messageTTL(mt, req.TTL)
	env := map[string]any{
		"specversion": "1.0", "id": id, "source": source, "type": "botaru.msg." + req.Type, "time": fmtTime(now),
		"datacontenttype": "application/json", "dataschema": "urn:botaru:schema:msg:" + req.Type,
		"tenant": s.Tenant, "msgkind": "report", "msgto": addr, "msgdelivery": deliveryOf(mt), "fromrole": s.Role,
		"correlationid": s.Root, "idempotencykey": key, "hops": hops, "datamark": mark, "untrusted": untrusted,
		"dataorigin": origin, "origininitiators": initiators, "ttl_s": int(ttl / time.Second),
		"data": map[string]any{"fields": fields},
	}
	if s.Worker != "" {
		env["fromworker"] = s.Worker
	}
	if s.Task != "" {
		env["fromtask"], env["causationid"] = s.Task, s.Task
	}
	if replyTo != "" {
		env["replyto"], env["causationid"] = replyTo, replyTo
	}
	if len(srcs) > 0 {
		env["untrustedsrc"] = strings.Join(dedupe(srcs), ",")
	}
	if s.Trace != "" {
		env["traceparent"] = s.Trace
	}
	if sub := asString(fields[mt.SubjectFrom]); mt.SubjectFrom != "" && sub != "" {
		env["subject"] = sub
	}
	if recipients != nil {
		env["recipients"] = *recipients
	}
	raw, err := json.Marshal(env)
	if err != nil {
		return nil, nil, err
	}
	if len(raw)+len(req.Note) > maxEnvelopeBytes {
		return deny("data_schema_invalid", "the envelope is above 8 KiB")
	}
	state := "staged"
	var seq int64
	if err := tx.QueryRow(ctx, `INSERT INTO messages (id, tenant, inbox_kind, inbox_name, type, kind, delivery, state, envelope, note, from_role, from_worker,
		sender_task, sender_lease, idem_key, data_mark, untrusted, hops, expires_at, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, 'report', $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $19) RETURNING seq`,
		id, s.Tenant, in.Kind, in.Name, req.Type, deliveryOf(mt), state, raw, nullable(req.Note), s.Role, nullable(s.Worker),
		nullable(s.Task), nullableInt(s.Lease), key, mark, untrusted, hops, nil, now).Scan(&seq); err != nil {
		return nil, nil, err
	}
	if s.Task == "" {
		// The core's own message is published at once, as an attempt's is at its complete.
		if err := c.publishRows(ctx, tx, now, cat, s, `seq = $1`, seq); err != nil {
			return nil, nil, err
		}
		// A hold or a full inbox may have discarded it, or refused every copy of an all
		// delivery: the audit says what happened.
		// The copies made are the recipients the message reached.
		var made int
		if err := tx.QueryRow(ctx, `SELECT count(*), coalesce(min(state), 'discarded') FROM messages WHERE id = $1 AND state <> 'discarded'`, id).Scan(&made, &state); err != nil {
			return nil, nil, err
		}
		if recipients != nil && *recipients > made {
			recipients = &made
		}
	}
	hash, _ := jcs.Hash(fields)
	if err := c.msgAudit(ctx, tx, now, s, "msg.send", id, true, "", map[string]any{"type": req.Type, "kind": "report", "to": addr, "bytes": size, "fields_hash": hash, "state": state}); err != nil {
		return nil, nil, err
	}
	return &MsgSendResult{MessageID: id, State: state, Recipients: recipients}, nil, nil
}

// inboxRoom checks the capacity of an inbox and the sending role's share of it; the first
// refusal of a period raises inbox.flood (MSG-15). Old entries are never pushed out.
func (c *Core) inboxRoom(ctx context.Context, tx pgx.Tx, now time.Time, tenant string, in msgInbox, role string) (*Problem, error) {
	if err := lockInbox(ctx, tx, tenant, in); err != nil {
		return nil, err
	}
	all, mine, err := inboxLoad(ctx, tx, tenant, in, role)
	if err != nil {
		return nil, err
	}
	reason := ""
	switch {
	case all >= in.capacity():
		reason = "inbox_full"
	case mine >= in.capacity()/4:
		reason = "sender_quota"
	default:
		return nil, nil
	}
	if due, err := markOnce(ctx, tx, now, tenant, "flood:"+in.addr(), time.Hour); err != nil {
		return nil, err
	} else if due {
		if err := c.appendEvent(ctx, tx, now, tenant, Event{Type: "inbox.flood", Subject: in.addr(),
			Data: map[string]any{"inbox": in.addr(), "reason": reason, "role": role}}); err != nil {
			return nil, err
		}
	}
	return refuse(reason, "the inbox has no room for this sender"), nil
}

// checkMsgFields checks the fields against the type's closed schema with the catalog values of
// the sender's mark, and their size against the type's max_bytes and the 4 KiB ceiling (MSG-2,
// MSG-15).
func (c *Core) checkMsgFields(cat *Catalog, s *msgSender, mt manifest.MessageType, fields map[string]any) (map[string]any, int, *Problem, error) {
	if fields == nil {
		fields = map[string]any{}
	}
	names := map[string]bool{}
	enumFromNames(mt.FieldsSchema, names)
	mark := s.Mark
	if mark == "" {
		mark = mt.DataClass
	}
	full, err := c.fullSchema(mt.FieldsSchema, c.resolveEnumFrom(cat, s.Tenant, mark, names))
	if err != nil {
		return nil, 0, nil, err
	}
	if c.rs.validate(full, fields) != nil {
		return nil, 0, refuse("data_schema_invalid", "the fields are outside the type's schema"), nil
	}
	raw, err := json.Marshal(fields)
	if err != nil {
		return nil, 0, nil, err
	}
	limit := maxFieldsBytes
	if mt.MaxBytes > 0 {
		limit = min(limit, mt.MaxBytes)
	}
	if len(raw) > limit {
		return nil, 0, refuse("data_schema_invalid", "the fields are above the type's size"), nil
	}
	return fields, len(raw), nil, nil
}

func deliveryOf(mt manifest.MessageType) string {
	if mt.Kind == "state" {
		return "state"
	}
	if mt.Delivery == "all" {
		return "all"
	}
	return "one"
}

func dedupe(in []string) []string {
	var out []string
	for _, s := range in {
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

// msgSenderScopes are the hold scopes of a sender: its tenant, role and worker.
func msgSenderScopes(s *msgSender) []scopeRef {
	out := tenantScopes(s.Tenant)
	if s.Role != "" {
		out = append(out, scopeRef{kind: "role", tenant: s.Tenant, name: s.Role})
	}
	if s.Worker != "" {
		out = append(out, scopeRef{kind: "worker", tenant: s.Tenant, name: strings.ToLower(s.Worker)})
	}
	return out
}

// sendState puts a state value of worker code (STA-1, MSG-14): only a type of genus state, the
// sender is the worker of the credentials, no note, untrusted false and dataorigin code, at most
// once a minute per (worker, type) unless the phase changes. The value replaces the worker's
// previous one in each inbox of a role that receives the type, in one transaction.
func (c *Core) sendState(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, w *Worker, req MsgSendRequest) (*MsgSendResult, *Problem, error) {
	s := &msgSender{Tenant: w.Tenant, Role: w.Role, Worker: w.ID, Producer: "code"}
	deny := func(reason, detail string, audit bool) (*MsgSendResult, *Problem, error) {
		if audit {
			if err := c.msgAudit(ctx, tx, now, s, "msg.send", "", false, reason, map[string]any{"type": req.Type}); err != nil {
				return nil, nil, err
			}
		}
		return nil, refuse(reason, "%s", detail), nil
	}
	mt, ok := cat.MessageTypes[req.Type]
	if !ok || mt.Kind != "state" || !slices.Equal(mt.Producers, []string{"code"}) {
		return deny("msg_type_not_allowed", "worker code outside an attempt sends only a state value", true)
	}
	if req.To != "" || req.ReplyTo != "" || req.Note != "" {
		return deny("msg_type_not_allowed", "a state value has no address and no note", true)
	}
	if held, err := heldBy(ctx, tx, now, []string{"no_message"}, msgSenderScopes(s)...); err != nil {
		return nil, nil, err
	} else if held != "" {
		return deny("held", "a hold forbids messages", true)
	}
	fields, _, p, err := c.checkMsgFields(cat, s, mt, req.Fields)
	if err != nil {
		return nil, nil, err
	}
	if p != nil {
		return deny(p.Reason, p.Detail, true)
	}
	// One value of a worker and type at a time, so two sends never leave two current values.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(7300121, hashtext($1 || ':' || $2))`, w.ID, req.Type); err != nil {
		return nil, nil, err
	}
	// A repeat with the worker's own key returns the value it already sent (IDM-1).
	own := ""
	if req.IdempotencyKey != nil && *req.IdempotencyKey != "" {
		own = strings.ToLower(w.ID) + ":" + *req.IdempotencyKey
		var id string
		err := tx.QueryRow(ctx, `SELECT id FROM messages WHERE tenant = $1 AND idem_key = $2 ORDER BY seq LIMIT 1`, w.Tenant, own).Scan(&id)
		if err == nil {
			return &MsgSendResult{MessageID: id, State: "current"}, nil, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return nil, nil, err
		}
	}
	// Once a minute, plus on a change of phase; an extra value is refused without an event.
	var last []byte
	var at time.Time
	err = tx.QueryRow(ctx, `SELECT envelope->'data'->'fields', created_at FROM messages WHERE tenant = $1 AND from_worker = $2 AND type = $3
		ORDER BY seq DESC LIMIT 1`, w.Tenant, w.ID, req.Type).Scan(&last, &at)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, err
	}
	if err == nil && now.Sub(at) < stateMinGap {
		var prev map[string]any
		_ = json.Unmarshal(last, &prev)
		if prev["phase"] == fields["phase"] {
			return deny("service_rate_limited", "a state value at most once a minute", false)
		}
	}
	id := ids.New(ids.Message)
	env := map[string]any{
		"specversion": "1.0", "id": id, "source": "/w/" + strings.ToLower(w.ID), "type": "botaru.msg." + req.Type, "time": fmtTime(now),
		"datacontenttype": "application/json", "dataschema": "urn:botaru:schema:msg:" + req.Type, "tenant": w.Tenant, "msgkind": "state",
		"fromrole": w.Role, "fromworker": w.ID, "correlationid": w.ID, "idempotencykey": strings.ToLower(w.ID) + ":" + id,
		"hops": 1, "datamark": mt.DataClass, "untrusted": false, "dataorigin": "code", "data": map[string]any{"fields": fields},
	}
	if own != "" {
		env["idempotencykey"] = own
	}
	if sub := asString(fields[mt.SubjectFrom]); mt.SubjectFrom != "" && sub != "" {
		env["subject"] = sub
	}
	var roles []string
	for rn, r := range cat.Roles {
		// A reporter sees the state of workers in status.workers, not in its inbox (REP-8).
		if r.Receives(req.Type) && !noMessageProfiles[r.Profile] && r.Profile != "reporter" {
			roles = append(roles, rn)
		}
	}
	sort.Strings(roles)
	for _, rn := range roles {
		if _, err := tx.Exec(ctx, `UPDATE messages SET state = 'replaced', updated_at = $5 WHERE tenant = $1 AND inbox_kind = 'role' AND inbox_name = $2
			AND from_worker = $3 AND type = $4 AND state IN ('current', 'stale')`, w.Tenant, rn, w.ID, req.Type, now); err != nil {
			return nil, nil, err
		}
		env["msgto"] = "role:" + rn
		env["expiresat"] = fmtTime(now.Add(stateStaleAfter(mt)))
		raw, err := json.Marshal(env)
		if err != nil {
			return nil, nil, err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO messages (id, tenant, inbox_kind, inbox_name, type, kind, delivery, state, envelope, from_role, from_worker,
			idem_key, data_mark, untrusted, hops, expires_at, created_at, updated_at)
			VALUES ($1, $2, 'role', $3, $4, 'state', 'state', 'current', $5, $6, $7, $8, $9, false, 1, $10, $11, $11)`,
			id, w.Tenant, rn, req.Type, raw, w.Role, w.ID, env["idempotencykey"], mt.DataClass, now.Add(stateStaleAfter(mt)), now); err != nil {
			return nil, nil, err
		}
	}
	if len(roles) == 0 {
		// No role receives the type: the value is kept as the worker's latest, for its rate.
		raw, err := json.Marshal(env)
		if err != nil {
			return nil, nil, err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO messages (id, tenant, inbox_kind, inbox_name, type, kind, delivery, state, envelope, from_role, from_worker,
			idem_key, data_mark, untrusted, hops, expires_at, created_at, updated_at)
			VALUES ($1, $2, 'worker', $3, $4, 'state', 'state', 'removed', $5, $6, $7, $8, $9, false, 1, NULL, $10, $10)`,
			id, w.Tenant, w.ID, req.Type, raw, w.Role, w.ID, env["idempotencykey"], mt.DataClass, now); err != nil {
			return nil, nil, err
		}
	}
	if err := c.msgAudit(ctx, tx, now, s, "msg.state", id, true, "", map[string]any{"type": req.Type, "kind": "state", "inboxes": len(roles)}); err != nil {
		return nil, nil, err
	}
	return &MsgSendResult{MessageID: id, State: "current"}, nil, nil
}
