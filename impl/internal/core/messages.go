// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/manifest"
	"github.com/HunterEwok/botaru/impl/internal/store"
)

// Bot messages (docs/messaging.md, spec/lifecycles/message.md). A message only informs: it is
// staged by an attempt and published in the transaction of its complete, read by an attempt
// under its lease and acknowledged by its complete (MSG-9). Inboxes live in the store, keyed by
// (tenant, role), (tenant, bot group) and (tenant, worker). Only the core writes the envelope
// fields (MSG-1); the note lives only in its own column of class content (MSG-13).
// This file holds the requests, the sender and the inboxes; sending, publishing and reading
// are in messagesend.go, messagepublish.go and messageread.go.

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
	// Key is the core's own idempotency key of a message without an attempt: an auto-report's
	// system:report:<task>:<route> (MSG-16).
	Key string
	// TreeTask is the task whose tree the core's own message belongs to: the usage line goes
	// there (BUD-3).
	TreeTask string
	// ModelFields marks the core's auto-report on an agent task: the model wrote what it
	// carries (MSG-1, MSG-6).
	ModelFields bool
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
// lockInbox serializes the room checks of one inbox until the end of the transaction, so two
// sends never pass its capacity or a sender's share together (MSG-15).
func lockInbox(ctx context.Context, tx pgx.Tx, tenant string, in msgInbox) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(7300123, hashtext($1 || ':' || $2))`, tenant, in.addr())
	return err
}

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
