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

	"github.com/HunterEwok/botaru/impl/internal/ids"
	"github.com/HunterEwok/botaru/impl/internal/store"
)

// The P2 views of bot messages and inboxes (MSG-18): msg.list, msg.show and inbox.status show
// messages as metadata (MSG-3) to the operator of a tenant, and to the operator of farm for any
// tenant. Only fields whose leaves are all enum values, ids or numbers come out; the note only in
// msg.show to a person with audit.read and data:<mark> in the message's tenant, inside an
// untrusted wrapper, and that read is audited. A message or inbox of another tenant and a
// nonexistent one give the same not_found. These are P2 operations: no model or worker reaches
// them.

const (
	// msgListMax is the most rows of a page of msg.list (MSG-18).
	msgListMax = 200
	// inboxStatusMax is the most rows of inbox.status.
	inboxStatusMax = 500
)

// msgStates are the message states a filter may name.
var msgStates = []string{"staged", "queued", "leased", "acked", "discarded", "dead", "expired", "current", "stale", "replaced", "removed"}

// MsgListRequest is p2-msg-list-request.
type MsgListRequest struct {
	Tenant string `json:"tenant,omitempty"`
	Type   string `json:"type,omitempty"`
	Role   string `json:"role,omitempty"`
	State  string `json:"state,omitempty"`
	Since  string `json:"since,omitempty"`
	Until  string `json:"until,omitempty"`
	Limit  int    `json:"limit,omitempty"`
	After  string `json:"after,omitempty"`
}

// MsgShowRequest is p2-msg-show-request.
type MsgShowRequest struct {
	ID string `json:"id"`
}

// InboxStatusRequest is p2-inbox-status-request.
type InboxStatusRequest struct {
	Tenant string `json:"tenant,omitempty"`
	Role   string `json:"role,omitempty"`
}

// msgViewColumns are the columns of a message row for the views.
const msgViewColumns = `seq, id, tenant, type, kind, inbox_kind, inbox_name, from_role, state, coalesce(reason, ''), created_at, expires_at, handouts,
	octet_length(envelope::text) + coalesce(octet_length(note), 0), envelope, hops, untrusted, data_mark, note`

// msgView is one message row with its envelope.
type msgView struct {
	seq                                           int64
	id, tenant, typ, kind, inboxKind, inbox, from string
	state, reason                                 string
	sent                                          time.Time
	expires                                       *time.Time
	handouts                                      int
	size                                          int64
	env                                           []byte
	hops                                          int
	untrusted                                     bool
	mark                                          string
	note                                          *string
}

func scanMsgView(row pgx.Row) (*msgView, error) {
	var m msgView
	err := row.Scan(&m.seq, &m.id, &m.tenant, &m.typ, &m.kind, &m.inboxKind, &m.inbox, &m.from, &m.state, &m.reason, &m.sent, &m.expires,
		&m.handouts, &m.size, &m.env, &m.hops, &m.untrusted, &m.mark, &m.note)
	return &m, err
}

// meta is the metadata form of a message (MSG-3); the tenant only for the operator of farm.
func (m *msgView) meta(farm bool) map[string]any {
	out := map[string]any{"id": m.id, "type": m.typ, "kind": m.kind, "to": m.inboxKind + ":" + m.inbox, "from_role": m.from,
		"state": m.state, "sent_at": fmtTime(m.sent), "deliveries": m.handouts, "size_bytes": m.size}
	if farm {
		out["tenant"] = m.tenant
	}
	if m.reason != "" {
		out["reason"] = m.reason
	}
	if m.expires != nil {
		out["expires_at"] = fmtTime(*m.expires)
	}
	return out
}

// closedSchema says whether every leaf of a fields schema is an enum value, an id or a number:
// enum, const and enum_from (catalog ids), integer, number and boolean, a string with the id
// format, and objects and lists made only of those (MSG-3).
func closedSchema(s map[string]any) bool {
	if s == nil {
		return false
	}
	if _, ok := s["enum"]; ok {
		return true
	}
	if _, ok := s["const"]; ok {
		return true
	}
	if _, ok := s["enum_from"]; ok {
		return true
	}
	switch mstr(s, "type") {
	case "integer", "number", "boolean":
		return true
	case "string":
		f := mstr(s, "x-botaru-format")
		return f == "id" || f == "type_name"
	case "array":
		return closedSchema(asMap(s["items"]))
	case "object":
		props := mmap(s, "properties")
		if len(props) == 0 || s["additionalProperties"] != false {
			return false
		}
		for _, p := range props {
			if !closedSchema(asMap(p)) {
				return false
			}
		}
		return true
	}
	return false
}

// msgClosedFields are the fields of a message whose schema in the message type is closed.
func msgClosedFields(cat *Catalog, typ string, env []byte) map[string]any {
	var e struct {
		Data struct {
			Fields map[string]any `json:"fields"`
		} `json:"data"`
	}
	_ = json.Unmarshal(env, &e)
	props := mmap(cat.MessageTypes[typ].FieldsSchema, "properties")
	out := map[string]any{}
	for k, v := range e.Data.Fields {
		if closedSchema(asMap(props[k])) {
			out[k] = v
		}
	}
	return out
}

// msgTenant is the tenant a message view reads: another tenant than the credential's is the same
// not_found as a nonexistent one (MSG-18).
func msgTenant(op Operator, named string) (string, error) {
	t, err := viewTenant(op, named)
	if err != nil {
		return "", refuse("not_found", "no such inbox")
	}
	return t, nil
}

// msgCursor is the page cursor of msg.list: the message id and the store order of the last row.
func msgCursor(m *msgView) string { return m.id + ":" + strconv.FormatInt(m.seq, 10) }

// MsgList is P2 msg.list: messages of a tenant by closed filters, at most 200 rows a page.
func (c *Core) MsgList(ctx context.Context, op Operator, req MsgListRequest) (map[string]any, error) {
	tenant, err := msgTenant(op, req.Tenant)
	if err != nil {
		return nil, err
	}
	if req.State != "" && !slices.Contains(msgStates, req.State) {
		return nil, refuse("input_schema_invalid", "state")
	}
	since, until := time.Time{}, time.Time{}
	if req.Since != "" {
		if since, err = time.Parse(time.RFC3339Nano, req.Since); err != nil {
			return nil, refuse("input_schema_invalid", "since")
		}
	}
	if req.Until != "" {
		if until, err = time.Parse(time.RFC3339Nano, req.Until); err != nil {
			return nil, refuse("input_schema_invalid", "until")
		}
	}
	limit := req.Limit
	if limit <= 0 || limit > msgListMax {
		limit = msgListMax
	}
	var after int64
	if req.After != "" {
		_, n, _ := strings.Cut(req.After, ":")
		if after, err = strconv.ParseInt(n, 10, 64); err != nil {
			return nil, refuse("input_schema_invalid", "after")
		}
	}
	rows, err := c.st.Pool.Query(ctx, `SELECT `+msgViewColumns+` FROM messages
		WHERE tenant = $1 AND ($2 = '' OR type = $2) AND ($3 = '' OR from_role = $3 OR inbox_kind = 'role' AND inbox_name = $3)
		AND ($4 = '' OR state = $4) AND ($5::timestamptz IS NULL OR created_at >= $5) AND ($6::timestamptz IS NULL OR created_at < $6)
		AND seq > $7 ORDER BY seq LIMIT $8`,
		tenant, req.Type, req.Role, req.State, nullTime(since), nullTime(until), after, limit+1)
	if err != nil {
		return nil, err
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (*msgView, error) { return scanMsgView(r) })
	if err != nil {
		return nil, err
	}
	out := map[string]any{"items": []any{}}
	items := []any{}
	for i, m := range list {
		if i == limit {
			out["next"] = msgCursor(list[i-1])
			break
		}
		items = append(items, m.meta(op.Tenant == farmTenant))
	}
	out["items"] = items
	return out, nil
}

func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

// MsgShow is P2 msg.show: one message as metadata with its closed fields. The note comes only to
// a person of the message's tenant with audit.read and data:<mark>, in an untrusted wrapper, and
// the read writes an audit record; the operator gets no note (MSG-18).
func (c *Core) MsgShow(ctx context.Context, op Operator, req MsgShowRequest) (map[string]any, error) {
	cat, err := c.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	err = c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		// Copies of a message to every worker share its id: the first copy stands for them.
		m, err := scanMsgView(tx.QueryRow(ctx, `SELECT `+msgViewColumns+` FROM messages WHERE id = $1 ORDER BY seq LIMIT 1`, req.ID))
		if errors.Is(err, pgx.ErrNoRows) || err == nil && m.tenant != op.Tenant && op.Tenant != farmTenant {
			return refuse("not_found", "no such message")
		}
		if err != nil {
			return err
		}
		msg := m.meta(op.Tenant == farmTenant)
		msg["fields"], msg["hops"], msg["untrusted"] = msgClosedFields(cat, m.typ, m.env), m.hops, m.untrusted
		out = map[string]any{"message": msg}
		if m.note == nil || *m.note == "" {
			return nil
		}
		tv := cat.Tenants[m.tenant]
		if tv == nil {
			return nil
		}
		p, err := loadPrincipal(ctx, tx, now, m.tenant, op.Principal)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		groups := groupClosure(tv, p.Groups)
		if p.State != "active" || !hasCap(tv, groups, "audit.read") || !hasData(tv, groups, m.mark) {
			return nil
		}
		mark := "untrusted-" + ids.Secret(9)
		msg["note"] = "<" + mark + ">\n" + *m.note + "\n</" + mark + ">"
		_, err = c.appendAudit(ctx, tx, now, Audit{
			Tenant: m.tenant, Kind: "read", Actor: Actor{Principal: op.Principal, Via: "farmctl"}, Command: "msg.show",
			Target: &Target{Kind: "message", ID: m.id}, OK: true, After: map[string]any{"note": true, "data_mark": m.mark},
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// InboxStatus is P2 inbox.status: for each role inbox of the tenant the counts by state, the age
// of the oldest queued entry, the capacity and the tenant's wake-ups left today (MSG-18).
func (c *Core) InboxStatus(ctx context.Context, op Operator, req InboxStatusRequest) (map[string]any, error) {
	tenant, err := msgTenant(op, req.Tenant)
	if err != nil {
		return nil, err
	}
	cat, err := c.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	tv := cat.Tenants[tenant]
	if tv == nil {
		return nil, refuse("not_found", "no such tenant")
	}
	now, err := store.Now(ctx, c.st.Pool)
	if err != nil {
		return nil, err
	}
	roles := map[string]bool{}
	for name, r := range cat.Roles {
		if len(r.Messages.Receive) > 0 {
			roles[name] = true
		}
	}
	type stat struct {
		counts map[string]int
		oldest *time.Time
	}
	stats := map[string]*stat{}
	rows, err := c.st.Pool.Query(ctx, `SELECT inbox_name, state, count(*), min(created_at) FILTER (WHERE state = 'queued') FROM messages
		WHERE tenant = $1 AND inbox_kind = 'role' GROUP BY inbox_name, state`, tenant)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var role, state string
		var n int
		var oldest *time.Time
		if err := rows.Scan(&role, &state, &n, &oldest); err != nil {
			rows.Close()
			return nil, err
		}
		s := stats[role]
		if s == nil {
			s = &stat{counts: map[string]int{}}
			stats[role] = s
		}
		s.counts[state] = n
		if oldest != nil {
			s.oldest = oldest
		}
		roles[role] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	_, perDay := wakeCeilings(tv)
	var wakes int
	if err := c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE tenant = $1 AND created_at >= $2 AND record ? 'wake_inbox'`,
		tenant, tenantDay(tv, now)).Scan(&wakes); err != nil {
		return nil, err
	}
	if req.Role != "" && !roles[req.Role] {
		return nil, refuse("not_found", "no such inbox")
	}
	list := []any{}
	for _, role := range slices.Sorted(mapKeys(roles)) {
		if req.Role != "" && role != req.Role || len(list) >= inboxStatusMax {
			continue
		}
		counts := map[string]any{}
		row := map[string]any{"role": role, "counts": counts, "capacity": msgInbox{Kind: "role", Name: role}.capacity(), "wake_budget_left": max(perDay-wakes, 0)}
		if op.Tenant == farmTenant {
			row["tenant"] = tenant
		}
		if s := stats[role]; s != nil {
			for k, v := range s.counts {
				counts[k] = v
			}
			if s.oldest != nil {
				row["oldest_queued_s"] = max(int64(now.Sub(*s.oldest)/time.Second), 0)
			}
		}
		list = append(list, row)
	}
	return map[string]any{"inboxes": list}, nil
}
