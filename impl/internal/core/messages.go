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
	"github.com/HunterEwok/botaru/impl/internal/store"
)

// Bot messages (docs/messaging.md, spec/lifecycles/message.md). A message only informs: it is
// staged by an attempt and published in the transaction of its complete, read by an attempt
// under its lease and acknowledged by its complete (MSG-9). Inboxes live in the store, keyed by
// (tenant, role), (tenant, bot group) and (tenant, worker). Only the core writes the envelope
// fields (MSG-1); the note lives only in its own column of class content (MSG-13).

// Farm ceilings of messages (MSG-11, MSG-12, MSG-13, MSG-14, MSG-15).
const (
	maxSendsPerAttempt = 3
	farmMsgRatePerHour = 120
	maxEnvelopeBytes   = 8 << 10
	maxFieldsBytes     = 4 << 10
	maxNoteChars       = 500
	reportTTLDefault   = 24 * time.Hour
	reportTTLMax       = 7 * 24 * time.Hour
	stateStaleMax      = time.Hour
	stateMinGap        = time.Minute
	maxHandouts        = 3
	maxAllRecipients   = 50
	workerInboxCap     = 100
	sharedInboxCap     = 1000
	maxHops            = 5
	maxInboxRead       = 20
)

// noMessageProfiles are the role profiles that never receive messages (MSG-5, PRF-1).
var noMessageProfiles = map[string]bool{"reviewer": true, "aggregator": true, "farm-manager": true, "farm-manager-reader": true, "translator": true}

// MsgSendRequest is msg-send-request: P1 msg.send and the arguments of P3 send_message.
type MsgSendRequest struct {
	Type           string         `json:"type"`
	To             string         `json:"to,omitempty"`
	ReplyTo        string         `json:"reply_to,omitempty"`
	Fields         map[string]any `json:"fields"`
	Note           string         `json:"note,omitempty"`
	IdempotencyKey *string        `json:"idempotency_key,omitempty"`
	TTL            string         `json:"ttl,omitempty"`
}

// MsgSendResult is the P1 answer of msg-send-response.
type MsgSendResult struct {
	MessageID  string `json:"message_id"`
	State      string `json:"state"`
	Recipients *int   `json:"recipients,omitempty"`
}

// MsgReceiveRequest is p1-msg-receive-request and, without wait_s, the arguments of P3
// inbox_read.
type MsgReceiveRequest struct {
	Types []string `json:"types,omitempty"`
	Max   int      `json:"max"`
	WaitS int      `json:"wait_s,omitempty"`
}

// MsgReceiveResult is p1-msg-receive-response and inbox-read-response.
type MsgReceiveResult struct {
	Records []map[string]any `json:"records"`
	Hidden  int              `json:"hidden"`
}

// MsgAckRequest is p1-msg-ack-request.
type MsgAckRequest struct {
	Handles []string `json:"handles"`
}

// MsgAckResult is p1-msg-ack-response.
type MsgAckResult struct {
	Count int `json:"count"`
}

// msgSender is who sends: an attempt (agent through P3, job through P1), worker code outside an
// attempt, or the core. Everything but the request comes from the credentials and the attempt
// record (MSG-1).
type msgSender struct {
	Tenant, Role, Worker string
	// Producer is agent, job, code or core (MSG-5).
	Producer    string
	Task        string
	Lease       int
	Initiator   string
	Initiators  []string // the initiators of what the attempt read (MSG-7)
	Mark        string
	Untrusted   bool
	UntrustedOf []string
	Depth       int // the effective depth: the depth and the floor of what was read (MSG-11)
	Root        string
	Trace       string
}

// msgRecord is the part of a task record that messages read and raise.
type msgRecord struct {
	Root            string   `json:"root"`
	Depth           int      `json:"depth"`
	DepthFloor      int      `json:"depth_floor"`
	DataMark        string   `json:"data_mark"`
	UntrustedSeen   bool     `json:"untrusted_seen"`
	UntrustedSource string   `json:"untrusted_source"`
	UntrustedOf     []string `json:"untrusted_sources"`
	CoInitiators    []string `json:"co_initiators"`
	Trace           string   `json:"trace"`
}

// effDepth is the effective depth of a task: its depth, raised by the hops of what it read.
func (r msgRecord) effDepth() int { return max(r.Depth, r.DepthFloor) }

// attemptSender reads the sender of an attempt from its task record and worker.
func (c *Core) attemptSender(ctx context.Context, tx pgx.Tx, tenant, task string, lease int, worker, producer string) (*msgSender, error) {
	var role, initiator string
	var raw []byte
	if err := tx.QueryRow(ctx, `SELECT t.initiator, t.record, w.role FROM tasks t, workers w WHERE t.id = $1 AND w.id = $2`, task, worker).
		Scan(&initiator, &raw, &role); err != nil {
		return nil, err
	}
	var r msgRecord
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, err
	}
	s := &msgSender{Tenant: tenant, Role: role, Worker: worker, Producer: producer, Task: task, Lease: lease, Initiator: initiator,
		Initiators: r.CoInitiators, Mark: r.DataMark, Untrusted: r.UntrustedSeen, Depth: r.effDepth(), Root: r.Root, Trace: r.Trace}
	if s.Root == "" {
		s.Root = task
	}
	if r.UntrustedSeen {
		src := r.UntrustedSource
		if src == "" {
			src = "attempt"
		}
		s.UntrustedOf = append(s.UntrustedOf, untrustedCode(src))
	}
	return s, nil
}

// untrustedCode turns a source of untrusted_seen into a code of the untrustedsrc list.
func untrustedCode(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := strings.Trim(b.String(), "_")
	if out == "" || out[0] < 'a' || out[0] > 'z' {
		out = "source"
	}
	if len(out) > 32 {
		out = out[:32]
	}
	return out
}

// principalGroups are the groups of an initiator: system acts through system-ops (PRM-10), an
// inactive or unknown principal has none.
func principalGroups(ctx context.Context, q store.Querier, now time.Time, tv *TenantView, id string) ([]string, error) {
	groups, err := rawGroups(ctx, q, now, tv.Name, id)
	return groupClosure(tv, groups), err
}

// rawGroups are the groups a principal is a member of, without inheritance; system has
// system-ops, an inactive or unknown principal none.
func rawGroups(ctx context.Context, q store.Querier, now time.Time, tenant, id string) ([]string, error) {
	if id == "system" {
		return []string{"system-ops"}, nil
	}
	p, err := loadPrincipal(ctx, q, now, tenant, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if p.State != "active" {
		return nil, nil
	}
	return p.Groups, nil
}

// initiatorsHave says whether the initiator and every initiator of what the attempt read have
// the capability: C2 intersects them (MSG-5, MSG-7).
func initiatorsHave(ctx context.Context, tx pgx.Tx, now time.Time, tv *TenantView, cap string, initiators ...string) (bool, error) {
	for _, id := range initiators {
		groups, err := principalGroups(ctx, tx, now, tv, id)
		if err != nil {
			return false, err
		}
		if !hasCap(tv, groups, cap) {
			return false, nil
		}
	}
	return true, nil
}

// botGroupsOf are the bot groups of the tenant whose selector takes a worker of the role with
// the labels recorded at enroll (BGR-1). Membership grants nothing (BGR-2): it only narrows
// dispatch and is an address.
func botGroupsOf(tv *TenantView, role string, labels map[string]string) []string {
	var out []string
	for name, bg := range tv.BotGroups {
		if slices.Contains(bg.Select.Roles, role) && hasLabels(labels, bg.Select.Labels) {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// workerGroups are the bot groups of a worker as it stands in the store: a blocked or revoked
// worker is in none.
func workerGroups(ctx context.Context, tx pgx.Tx, tv *TenantView, worker string) (string, []string, error) {
	var role, state string
	var raw []byte
	if err := tx.QueryRow(ctx, `SELECT role, state, labels FROM workers WHERE id = $1`, worker).Scan(&role, &state, &raw); err != nil {
		return "", nil, err
	}
	if state != "active" && state != "draining" && state != "enrolled" {
		return role, nil, nil
	}
	var labels map[string]string
	_ = json.Unmarshal(raw, &labels)
	return role, botGroupsOf(tv, role, labels), nil
}

// messageTTL is the expiry of a report entry: the type's, 24 hours by default and at most 7
// days, which the sender can only shorten (MSG-12).
func messageTTL(mt manifest.MessageType, asked string) time.Duration {
	ttl := reportTTLDefault
	if d, ok := parseDuration(mt.TTL); ok && d > 0 {
		ttl = min(d, reportTTLMax)
	}
	if d, ok := parseDuration(asked); ok && d > 0 && d < ttl {
		ttl = d
	}
	return ttl
}

// staleAfter is the period of a state value, at most an hour (MSG-12).
func stateStaleAfter(mt manifest.MessageType) time.Duration {
	if d, ok := parseDuration(mt.StaleAfter); ok && d > 0 {
		return min(d, stateStaleMax)
	}
	return stateStaleMax
}

// msgInbox is one inbox: role, group or worker.
type msgInbox struct{ Kind, Name string }

func (i msgInbox) addr() string { return i.Kind + ":" + i.Name }

func (i msgInbox) capacity() int {
	if i.Kind == "worker" {
		return workerInboxCap
	}
	return sharedInboxCap
}

// receivingRoles are the roles an address reaches that accept the type, without the profiles
// that never receive (MSG-4, MSG-5).
func receivingRoles(cat *Catalog, tv *TenantView, addr, typ string) []string {
	kind, name, _ := strings.Cut(addr, ":")
	var roles []string
	switch kind {
	case "role":
		roles = []string{name}
	case "group":
		bg, ok := tv.BotGroups[name]
		if !ok {
			return nil
		}
		roles = bg.Select.Roles
	}
	var out []string
	for _, rn := range roles {
		r, ok := cat.Roles[rn]
		if ok && r.Receives(typ) && !noMessageProfiles[r.Profile] {
			out = append(out, rn)
		}
	}
	return out
}

// allRecipients are the active workers an all delivery copies to: workers of the receiving roles
// with their own inbox, members of the group for a group address, at most 50 in id order
// (MSG-14).
func allRecipients(ctx context.Context, tx pgx.Tx, cat *Catalog, tv *TenantView, addr, typ string) ([]string, error) {
	roles := slices.DeleteFunc(receivingRoles(cat, tv, addr, typ), func(rn string) bool { return !cat.Roles[rn].HasInstanceInbox() })
	if len(roles) == 0 {
		return nil, nil
	}
	rows, err := tx.Query(ctx, `SELECT id, role, labels FROM workers WHERE tenant = $1 AND role = ANY($2) AND state = 'active' ORDER BY id`, tv.Name, roles)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	kind, name, _ := strings.Cut(addr, ":")
	var out []string
	for rows.Next() {
		var id, role string
		var raw []byte
		if err := rows.Scan(&id, &role, &raw); err != nil {
			return nil, err
		}
		if kind == "group" {
			var labels map[string]string
			_ = json.Unmarshal(raw, &labels)
			if !slices.Contains(botGroupsOf(tv, role, labels), name) {
				continue
			}
		}
		if len(out) < maxAllRecipients {
			out = append(out, id)
		}
	}
	return out, rows.Err()
}

// inboxLoad is how many live entries an inbox holds and how many of them came from one role:
// staged, queued and leased entries take space (MSG-15).
func inboxLoad(ctx context.Context, tx pgx.Tx, tenant string, in msgInbox, role string) (int, int, error) {
	var all, mine int
	err := tx.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE from_role = $4) FROM messages
		WHERE tenant = $1 AND inbox_kind = $2 AND inbox_name = $3 AND state IN ('staged', 'queued', 'leased')`, tenant, in.Kind, in.Name, role).Scan(&all, &mine)
	return all, mine, err
}

// markOnce says whether the event of key is due in this period, and takes the period (MSG-15:
// one event per period).
func markOnce(ctx context.Context, tx pgx.Tx, now time.Time, tenant, key string, period time.Duration) (bool, error) {
	tag, err := tx.Exec(ctx, `INSERT INTO message_marks (tenant, key, until) VALUES ($1, $2, $3)
		ON CONFLICT (tenant, key) DO UPDATE SET until = EXCLUDED.until WHERE message_marks.until <= $4`, tenant, key, now.Add(period), now)
	return tag.RowsAffected() > 0, err
}

// msgAudit writes a decision about a message with metadata only: ids, type, address, roles,
// sizes, hashes and the reason, never fields or the note (MSG-3).
func (c *Core) msgAudit(ctx context.Context, tx pgx.Tx, now time.Time, s *msgSender, command, id string, ok bool, reason string, meta map[string]any) error {
	actor := Actor{Principal: "system", Via: "core"}
	if s.Worker != "" {
		actor = Actor{Principal: workerPrincipal(s.Worker), Via: "bot", Role: s.Role, Task: s.Task}
	}
	after := map[string]any{}
	for k, v := range meta {
		after[k] = v
	}
	target := &Target{Kind: "message", ID: id}
	if id == "" {
		target = &Target{Kind: "message_type", ID: asString(meta["type"])}
	}
	_, err := c.appendAudit(ctx, tx, now, Audit{Tenant: s.Tenant, Kind: "command", Actor: actor, Command: command, Target: target, OK: ok, Reason: reason, After: after})
	return err
}

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
	// system through system-ops (MSG-5, MSG-7).
	if s.Task != "" {
		ok, err := initiatorsHave(ctx, tx, now, tv, "msg.send:"+req.Type, append([]string{s.Initiator}, s.Initiators...)...)
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
	key := fmt.Sprintf("system:msg:%s:%d:%d", s.Task, s.Lease, sent+1)
	if s.Task == "" {
		// The core's own message without a key of its own is a message of its own.
		key = "system:msg:" + id
	}
	if req.IdempotencyKey != nil && *req.IdempotencyKey != "" {
		key = strings.ToLower(s.Worker) + ":" + *req.IdempotencyKey
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
	if s.Producer == "agent" {
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
	if s.Task == "" {
		env["expiresat"] = fmtTime(now.Add(ttl))
		delete(env, "ttl_s")
	}
	raw, err := json.Marshal(env)
	if err != nil {
		return nil, nil, err
	}
	if len(raw)+len(req.Note) > maxEnvelopeBytes {
		return deny("data_schema_invalid", "the envelope is above 8 KiB")
	}
	state := "staged"
	var expires any
	if s.Task == "" {
		// The core's own message is published at once, with its expiry.
		state, expires = "queued", now.Add(ttl)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO messages (id, tenant, inbox_kind, inbox_name, type, kind, delivery, state, envelope, note, from_role, from_worker,
		sender_task, sender_lease, idem_key, data_mark, untrusted, hops, expires_at, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, 'report', $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $19)`,
		id, s.Tenant, in.Kind, in.Name, req.Type, deliveryOf(mt), state, raw, nullable(req.Note), s.Role, nullable(s.Worker),
		nullable(s.Task), nullableInt(s.Lease), key, mark, untrusted, hops, expires, now); err != nil {
		return nil, nil, err
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

// publishMessages publishes what an attempt staged and acknowledges what it read, in the
// transaction of its complete (MSG-9, DLV-2). A hold with no_message at this moment discards
// the staged entries (held). An all delivery becomes a copy in the inbox of each recipient
// worker, at most 50; a copy without room is not made and the core writes an event (MSG-14).
func (c *Core) publishMessages(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, tenant, task string, lease int, worker string) error {
	tv := cat.Tenants[tenant]
	var role string
	if err := tx.QueryRow(ctx, `SELECT role FROM workers WHERE id = $1`, worker).Scan(&role); err != nil {
		return err
	}
	s := &msgSender{Tenant: tenant, Role: role, Worker: worker, Task: task, Lease: lease}
	// What the attempt read is acknowledged, entries it returned with msg.nack are already back.
	if err := c.ackRead(ctx, tx, now, s); err != nil {
		return err
	}
	type staged struct {
		seq            int64
		id, typ, inbox string
		delivery       string
		env            map[string]any
		note           *string
	}
	rows, err := tx.Query(ctx, `SELECT seq, id, type, inbox_kind || ':' || inbox_name, delivery, envelope, note FROM messages
		WHERE sender_task = $1 AND sender_lease = $2 AND state = 'staged' ORDER BY seq FOR UPDATE`, task, lease)
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
				all, _, err := inboxLoad(ctx, tx, tenant, in, "")
				if err != nil {
					return err
				}
				if all >= in.capacity() {
					if err := c.appendEvent(ctx, tx, now, tenant, Event{Type: "message.rejected", Subject: st.id,
						Data: map[string]any{"message": st.id, "type": st.typ, "inbox": "worker", "reason": "inbox_full"}}); err != nil {
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
		rows, err := tx.Query(ctx, `SELECT `+cols+` FROM messages m JOIN unnest($2::text[], $3::text[]) AS i(k, n) ON m.inbox_kind = i.k AND m.inbox_name = i.n
			WHERE m.tenant = $1 AND m.state = 'queued' AND m.type = ANY($4) AND (m.expires_at IS NULL OR m.expires_at > $5)
			ORDER BY m.seq LIMIT 500 FOR UPDATE OF m SKIP LOCKED`, r.Tenant, kinds, names, r.Types, now)
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
	return c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
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
	})
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
