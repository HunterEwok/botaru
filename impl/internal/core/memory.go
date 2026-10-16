// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/ids"
	"github.com/HunterEwok/botaru/impl/internal/manifest"
	"github.com/HunterEwok/botaru/impl/internal/store"
)

// Conversation sessions (C3) and person memory (C4) live in the farm store. The core computes
// the space and the session key from the lease (MEM-1): a handle names them, and nothing the
// model sends sets them.

const (
	// memoryCardTTL is the expiry of a memory record card; on silence the record is discarded
	// (MEM-3, decision point memory_record_card).
	memoryCardTTL  = 7 * 24 * time.Hour
	memoryCard     = "memory_record_card"
	sessionLoadMax = 100
)

// roleMemory is the memory of a role: none, person, bot or read_bot (PRF-1, MEM-2).
func roleMemory(r manifest.Role) string {
	switch m := r.Memory.(type) {
	case string:
		return m
	case map[string]any:
		return "read_bot"
	}
	return "none"
}

// leaseHandles issues the session handle of a task with a conversation and the memory handle
// of a role with person memory working for a person (MEM-1). A marked session marks the task
// (INJ-13). It updates record in place.
func (c *Core) leaseHandles(ctx context.Context, tx pgx.Tx, now time.Time, w *Worker, role manifest.Role, task string, leaseNo int, initiator string, record map[string]any, handles map[string]any) error {
	issue := func(kind, ref string) error {
		h := "bh_" + ids.Secret(32)
		if _, err := tx.Exec(ctx, `INSERT INTO handles (hash, kind, tenant, work_id, lease_no, epoch, worker_id, ref, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
			c.hashSecret(h), kind, w.Tenant, task, leaseNo, c.epoch, w.ID, ref, now); err != nil {
			return err
		}
		handles[kind] = h
		return nil
	}
	if conv, _ := record["conversation"].(string); conv != "" {
		var sid string
		var marked bool
		var sessionMark *string
		subjects := []string{}
		if strings.HasPrefix(initiator, "human:") {
			subjects = append(subjects, initiator)
		}
		err := tx.QueryRow(ctx, `INSERT INTO sessions (id, tenant, role, conversation, subjects, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $6)
			ON CONFLICT (tenant, role, conversation) DO UPDATE SET
				subjects = CASE WHEN sessions.subjects @> EXCLUDED.subjects THEN sessions.subjects ELSE sessions.subjects || EXCLUDED.subjects END,
				updated_at = EXCLUDED.updated_at
			RETURNING id, untrusted_seen, data_mark`, ids.New(ids.Session), w.Tenant, w.Role, conv, subjects, now).Scan(&sid, &marked, &sessionMark)
		if err != nil {
			return err
		}
		// A session carries the highest mark of the tasks that wrote to it: a task that loads
		// it is not lower (OUT-1, ART-2).
		own, _ := record["data_mark"].(string)
		raise := sessionMark != nil && c.higherMark(own, *sessionMark) != own
		if raise && strings.HasPrefix(initiator, "human:") {
			// A person who may no longer see the session's class does not get it loaded.
			ok, err := c.mayRead(ctx, tx, now, w.Tenant, initiator, *sessionMark)
			if err != nil {
				return err
			}
			if !ok {
				raise, conv = false, ""
			}
		}
		if raise {
			record["data_mark"] = *sessionMark
			if _, err := tx.Exec(ctx, `UPDATE tasks SET record = jsonb_set(record, '{data_mark}', to_jsonb($2::text)) WHERE id = $1`, task, *sessionMark); err != nil {
				return err
			}
		}
		if marked && record["untrusted_seen"] != true {
			record["untrusted_seen"] = true
			if _, err := tx.Exec(ctx, `UPDATE tasks SET record = record || '{"untrusted_seen": true, "untrusted_source": "session"}' WHERE id = $1`, task); err != nil {
				return err
			}
		}
		if conv != "" {
			if err := issue("session", sid); err != nil {
				return err
			}
		}
	}
	if roleMemory(role) == "person" && strings.HasPrefix(initiator, "human:") {
		return issue("memory", initiator)
	}
	return nil
}

// leaseHandle is the lease a session or memory handle is bound to.
type leaseHandle struct {
	Tenant, Task, Worker, Role, Ref string
	LeaseNo                         int
	Untrusted                       bool
	// Mark is the data mark of the task.
	Mark string
}

// resolveLeaseHandle finds a live handle of a kind; a handle of a dead lease or of another
// store epoch is fenced (SEC-3, FEN-4).
func (c *Core) resolveLeaseHandle(ctx context.Context, q store.Querier, handle, kind string) (*leaseHandle, error) {
	if handle == "" {
		return nil, refuse("handle_invalid", "no %s handle", kind)
	}
	var h leaseHandle
	var epoch int64
	var state string
	var ref *string
	var until, now time.Time
	var rec []byte
	err := q.QueryRow(ctx, `SELECT h.tenant, h.work_id, h.lease_no, h.epoch, h.worker_id, h.ref, l.state, l.lease_until, botaru_now(), t.record, w.role
		FROM handles h JOIN leases l ON l.work_id = h.work_id AND l.lease_no = h.lease_no
		JOIN tasks t ON t.id = h.work_id JOIN workers w ON w.id = h.worker_id
		WHERE h.hash = ANY($1) AND h.kind = $2`, c.secretHashes(handle), kind).
		Scan(&h.Tenant, &h.Task, &h.LeaseNo, &epoch, &h.Worker, &ref, &state, &until, &now, &rec, &h.Role)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, refuse("handle_invalid", "unknown %s handle", kind)
	}
	if err != nil {
		return nil, err
	}
	if epoch != c.epoch || state != "active" || !until.After(now) {
		return nil, refuse("fenced", "the lease of the handle is not alive")
	}
	h.Ref = deref(ref)
	var r struct {
		Untrusted bool   `json:"untrusted_seen"`
		Mark      string `json:"data_mark"`
	}
	_ = json.Unmarshal(rec, &r)
	h.Untrusted, h.Mark = r.Untrusted, r.Mark
	return &h, nil
}

// SessionEntry is session-entry.
type SessionEntry struct {
	UUID    string `json:"uuid"`
	Summary bool   `json:"summary,omitempty"`
	Content any    `json:"content"`
}

// SessionLoadRequest is p1-session-load-request.
type SessionLoadRequest struct {
	After string `json:"after,omitempty"`
	Limit int    `json:"limit,omitempty"`
}

// SessionLoadResult is p1-session-load-response.
type SessionLoadResult struct {
	Entries       []SessionEntry `json:"entries"`
	UntrustedSeen bool           `json:"untrusted_seen"`
}

// SessionLoad reads the session of the handle in order, after an entry (MEM-1).
func (c *Core) SessionLoad(ctx context.Context, handle string, req SessionLoadRequest) (*SessionLoadResult, error) {
	h, err := c.resolveLeaseHandle(ctx, c.st.Pool, handle, "session")
	if err != nil {
		return nil, err
	}
	limit := req.Limit
	if limit <= 0 || limit > sessionLoadMax {
		limit = sessionLoadMax
	}
	out := &SessionLoadResult{Entries: []SessionEntry{}}
	if err := c.st.Pool.QueryRow(ctx, `SELECT untrusted_seen FROM sessions WHERE id = $1`, h.Ref).Scan(&out.UntrustedSeen); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, refuse("handle_invalid", "the session was erased")
		}
		return nil, err
	}
	rows, err := c.st.Pool.Query(ctx, `SELECT uuid, summary, content FROM session_entries WHERE session = $1
		AND seq > coalesce((SELECT seq FROM session_entries WHERE session = $1 AND uuid = $2), 0) ORDER BY seq LIMIT $3`, h.Ref, req.After, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var e SessionEntry
		var raw []byte
		if err := rows.Scan(&e.UUID, &e.Summary, &raw); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &e.Content); err != nil {
			return nil, err
		}
		out.Entries = append(out.Entries, e)
	}
	return out, rows.Err()
}

// SessionAppendRequest is p1-session-append-request.
type SessionAppendRequest struct {
	Entries []SessionEntry `json:"entries"`
}

// SessionAppendResult is p1-session-append-response.
type SessionAppendResult struct {
	Appended int `json:"appended"`
	Existing int `json:"existing"`
}

// SessionAppend adds entries, idempotent by the entry uuid; a task that saw untrusted text
// marks the session (INJ-13).
func (c *Core) SessionAppend(ctx context.Context, handle string, req SessionAppendRequest) (*SessionAppendResult, error) {
	out := &SessionAppendResult{}
	err := c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		*out = SessionAppendResult{}
		h, err := c.resolveLeaseHandle(ctx, tx, handle, "session")
		if err != nil {
			return err
		}
		var sessionMark *string
		err = tx.QueryRow(ctx, `SELECT data_mark FROM sessions WHERE id = $1 FOR UPDATE`, h.Ref).Scan(&sessionMark)
		if errors.Is(err, pgx.ErrNoRows) {
			return refuse("handle_invalid", "the session was erased")
		}
		if err != nil {
			return err
		}
		mark := c.higherMark(deref(sessionMark), h.Mark)
		tag, err := tx.Exec(ctx, `UPDATE sessions SET updated_at = $2, untrusted_seen = untrusted_seen OR $3, data_mark = nullif($4, '') WHERE id = $1`, h.Ref, now, h.Untrusted, mark)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return refuse("handle_invalid", "the session was erased")
		}
		for _, e := range req.Entries {
			raw, err := json.Marshal(e.Content)
			if err != nil {
				return err
			}
			tag, err := tx.Exec(ctx, `INSERT INTO session_entries (session, uuid, summary, content, created_at) VALUES ($1, $2, $3, $4, $5)
				ON CONFLICT DO NOTHING`, h.Ref, e.UUID, e.Summary, raw, now)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 1 {
				out.Appended++
			} else {
				out.Existing++
			}
		}
		return nil
	})
	return out, err
}

// MemoryItem is memory-item.
type MemoryItem struct {
	ID           string `json:"id"`
	Tenant       string `json:"tenant"`
	Scope        string `json:"scope"`
	Subject      string `json:"subject"`
	Role         string `json:"role"`
	Key          string `json:"key"`
	Content      string `json:"content,omitempty"`
	State        string `json:"state"`
	PersonalSeen bool   `json:"personal_seen"`
	CreatedAt    string `json:"created_at"`
	DecideBy     string `json:"decide_by,omitempty"`
	DecidedAt    string `json:"decided_at,omitempty"`
}

// MemorySearchRequest is p1-memory-search-request.
type MemorySearchRequest struct {
	Query     string `json:"query,omitempty"`
	KeyPrefix string `json:"key_prefix,omitempty"`
	Limit     int    `json:"limit,omitempty"`
}

// MemorySearchResult is p1-memory-search-response.
type MemorySearchResult struct {
	Items  []MemoryItem `json:"items"`
	Hidden int          `json:"hidden"`
}

const memoryCols = `id, tenant, scope, subject, role, key, content, state, personal_seen, created_at, decide_by, decided_at`

func scanMemory(row pgx.Row) (MemoryItem, error) {
	var m MemoryItem
	var created time.Time
	var decideBy, decidedAt *time.Time
	if err := row.Scan(&m.ID, &m.Tenant, &m.Scope, &m.Subject, &m.Role, &m.Key, &m.Content, &m.State, &m.PersonalSeen, &created, &decideBy, &decidedAt); err != nil {
		return m, err
	}
	m.CreatedAt = fmtTime(created)
	if decideBy != nil {
		m.DecideBy = fmtTime(*decideBy)
	}
	if decidedAt != nil {
		m.DecidedAt = fmtTime(*decidedAt)
	}
	return m, nil
}

// MemorySearch reads the active records of the space (tenant, person, subject) of the role;
// hidden counts the matches beyond the limit.
func (c *Core) MemorySearch(ctx context.Context, handle string, req MemorySearchRequest) (*MemorySearchResult, error) {
	h, err := c.resolveLeaseHandle(ctx, c.st.Pool, handle, "memory")
	if err != nil {
		return nil, err
	}
	limit := req.Limit
	if limit <= 0 || limit > 50 {
		limit = 50
	}
	out := &MemorySearchResult{Items: []MemoryItem{}}
	cond := `tenant = $1 AND subject = $2 AND role = $3 AND state = 'active' AND key LIKE $4 || '%'
		AND ($5 = '' OR strpos(lower(content), lower($5)) > 0 OR strpos(key, lower($5)) > 0)`
	args := []any{h.Tenant, h.Ref, h.Role, likeEscape(req.KeyPrefix), req.Query}
	var total int
	if err := c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM memory_items WHERE `+cond, args...).Scan(&total); err != nil {
		return nil, err
	}
	rows, err := c.st.Pool.Query(ctx, `SELECT `+memoryCols+` FROM memory_items WHERE `+cond+` ORDER BY key, created_at LIMIT `+strconv.Itoa(limit), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		m, err := scanMemory(rows)
		if err != nil {
			return nil, err
		}
		out.Items = append(out.Items, m)
	}
	out.Hidden = max(total-len(out.Items), 0)
	return out, rows.Err()
}

func likeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// MemoryProposeRequest is p1-memory-propose-request.
type MemoryProposeRequest struct {
	Key     string `json:"key"`
	Content string `json:"content"`
}

// MemoryProposeResult is p1-memory-propose-response.
type MemoryProposeResult struct {
	ID       string `json:"id"`
	State    string `json:"state"`
	Existing bool   `json:"existing"`
}

// MemoryPropose records a proposed record of the person's memory and sends them the card: it
// becomes active only by their decision (MEM-3). An attempt that saw untrusted text proposes
// nothing (memory_untrusted); the same content again returns the same record.
func (c *Core) MemoryPropose(ctx context.Context, handle string, req MemoryProposeRequest) (*MemoryProposeResult, error) {
	cat, err := c.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(req.Key + "\x00" + req.Content))
	hash := hex.EncodeToString(sum[:])
	var out *MemoryProposeResult
	err = c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		h, err := c.resolveLeaseHandle(ctx, tx, handle, "memory")
		if err != nil {
			return err
		}
		if h.Untrusted {
			return refuse("memory_untrusted", "an attempt that saw untrusted text proposes no memory")
		}
		var id, state string
		err = tx.QueryRow(ctx, `SELECT id, state FROM memory_items WHERE tenant = $1 AND subject = $2 AND role = $3 AND content_hash = $4
			AND state IN ('proposed', 'active')`, h.Tenant, h.Ref, h.Role, hash).Scan(&id, &state)
		if err == nil {
			out = &MemoryProposeResult{ID: id, State: state, Existing: true}
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		var rec []byte
		if err := tx.QueryRow(ctx, `SELECT record FROM tasks WHERE id = $1`, h.Task).Scan(&rec); err != nil {
			return err
		}
		id = ids.New(ids.Memory)
		if _, err := tx.Exec(ctx, `INSERT INTO memory_items (id, tenant, scope, subject, role, key, content, content_hash, state, personal_seen, nonce, source_task, created_at, decide_by, updated_at)
			VALUES ($1, $2, 'person', $3, $4, $5, $6, $7, 'proposed', $8, $9, $10, $11, $12, $11)`,
			id, h.Tenant, h.Ref, h.Role, req.Key, req.Content, hash, len(personalSeen(rec)) > 0, ids.Secret(16), h.Task, now, now.Add(memoryCardTTL)); err != nil {
			return err
		}
		out = &MemoryProposeResult{ID: id, State: "proposed"}
		if tv := cat.Tenants[h.Tenant]; tv != nil {
			if err := c.planSubjectCard(ctx, tx, now, tv, "memory_record", id, memoryCard, h.Ref, h.Task, false); err != nil {
				return err
			}
		}
		_, err = c.appendAudit(ctx, tx, now, Audit{
			Tenant: h.Tenant, Kind: "command", Actor: Actor{Principal: workerPrincipal(h.Worker), Via: "bot", Role: h.Role, Task: h.Task},
			Command: "memory.propose", Target: &Target{Kind: "memory_record", ID: id}, OK: true,
		})
		return err
	})
	return out, err
}

// planSubjectCard plans a card of a memory record or a person's request to one person, in
// their private chat; the card is drawn from the record when a channel takes it (APR-3).
func (c *Core) planSubjectCard(ctx context.Context, tx pgx.Tx, now time.Time, tv *TenantView, subjectKind, id, kind, person, task string, reminder bool) error {
	channel, chat, ok, err := cardChat(ctx, tx, tv, person)
	if err != nil || !ok {
		return err
	}
	body := map[string]any{"form": "card", "card": kind}
	if reminder {
		body["reminder"] = true
	}
	_, err = c.planDelivery(ctx, tx, now, plannedDelivery{
		Tenant: tv.Name, Task: task, Kind: kind, Addr: "approval_initiator", TargetKind: "chat", TargetID: chat.ID(),
		ExecKind: "channel", ExecName: channel, Recipient: person, Body: body, DataOrigin: "code",
		SubjectKind: subjectKind, SubjectID: id,
	})
	return err
}

// memoryCardBody draws the card of a memory record: the key and the content the person decides
// on, Accept and Reject while it is proposed.
func memoryCardBody(m MemoryItem, nonce string) map[string]any {
	button := func(code string) map[string]any {
		return map[string]any{"code": code, "subject_kind": "memory_record", "id": m.ID, "nonce": nonce}
	}
	buttons := []any{}
	if m.State == "proposed" {
		buttons = append(buttons, button("accept"), button("reject"))
	}
	card := map[string]any{
		"card": memoryCard, "subject_kind": "memory_record", "id": m.ID, "nonce": nonce,
		"title": cut("Remember: "+m.Key, 200), "state": m.State, "buttons": buttons,
		"context": map[string]any{"key": m.Key, "content": cut(m.Content, 2000)},
		"footer":  map[string]any{"why_you": "the record is about you", "until": m.DecideBy, "on_silence": "the record is not saved"},
	}
	if m.DecideBy != "" {
		card["expires_at"] = m.DecideBy
	}
	return card
}

// decideMemory is P1 decide on a memory record: only the person whose memory it is, from the
// card in their chat; accept makes it active, reject or expiry discards it (MEM-3).
func (c *Core) decideMemory(ctx context.Context, w *Worker, ob OnBehalf, req DecideRequest) (*DecideResult, error) {
	if req.Decision != "accept" && req.Decision != "reject" {
		return nil, refuse("input_schema_invalid", "a memory record is accepted or rejected")
	}
	cat, err := c.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	var out *DecideResult
	err = c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		var nonce string
		m, err := scanMemory(tx.QueryRow(ctx, `SELECT `+memoryCols+` FROM memory_items WHERE id = $1 AND tenant = $2 FOR UPDATE`, req.ID, w.Tenant))
		if errors.Is(err, pgx.ErrNoRows) {
			return refuse("not_found", "no such memory record")
		}
		if err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT nonce FROM memory_items WHERE id = $1`, m.ID).Scan(&nonce); err != nil {
			return err
		}
		if nonce != req.Nonce {
			return refuse("forbidden", "the nonce is not the card's")
		}
		press, err := c.checkPress(ctx, tx, now, cat, w, ob, "memory_record", m.ID, []string{memoryCard})
		if err != nil {
			return err
		}
		if press.principal != m.Subject {
			return refuse("forbidden", "only the person whose memory it is decides")
		}
		want := map[string]string{"accept": "active", "reject": "discarded"}[req.Decision]
		out = &DecideResult{SubjectKind: "memory_record", ID: m.ID, Decision: req.Decision, State: m.State}
		if m.State != "proposed" {
			if m.State == want {
				return nil
			}
			return refuse("already_decided", "the record is decided")
		}
		if t, err := time.Parse(time.RFC3339Nano, m.DecideBy); err == nil && !t.After(now) {
			if _, err := tx.Exec(ctx, `UPDATE memory_items SET state = 'discarded', updated_at = $2 WHERE id = $1`, m.ID, now); err != nil {
				return err
			}
			return &committed{p: refuse("expired", "the card has expired")}
		}
		if _, err := tx.Exec(ctx, `UPDATE memory_items SET state = $2, decided_at = $3, updated_at = $3 WHERE id = $1`, m.ID, want, now); err != nil {
			return err
		}
		out.State = want
		if err := c.appendEvent(ctx, tx, now, w.Tenant, Event{Type: "decision.made", Subject: m.ID, Data: map[string]any{
			"subject": map[string]any{"kind": "memory_record", "id": m.ID}, "card": memoryCard, "decision": req.Decision,
			"decided_by": press.principal, "channel": w.Target, "chat_kind": ob.Chat.Kind, "via": "decide",
			"delivered_at": fmtTime(press.deliveredAt), "decided_at": fmtTime(now),
			"response_ms": max(now.Sub(press.deliveredAt).Milliseconds(), 0),
		}}); err != nil {
			return err
		}
		_, err = c.appendAudit(ctx, tx, now, Audit{
			Tenant: w.Tenant, Kind: "decision", Actor: Actor{Principal: press.principal, Via: "channel"},
			Command: "decide", Target: &Target{Kind: "memory_record", ID: m.ID}, OK: true,
			After: map[string]any{"decision": req.Decision},
		})
		return err
	})
	return out, err
}

// expireMemory discards the proposed records whose card expired without a decision (MEM-3).
func (c *Core) expireMemory(ctx context.Context, tx pgx.Tx, now time.Time) error {
	_, err := tx.Exec(ctx, `UPDATE memory_items SET state = 'discarded', updated_at = $1 WHERE state = 'proposed' AND decide_by <= $1`, now)
	return err
}

// subjectCardBody draws the card of a memory record or a person's request when a channel takes
// its delivery; nil when the subject is gone (erased).
func (c *Core) subjectCardBody(ctx context.Context, tx pgx.Tx, tenant, subjectKind, id string, body map[string]any) (map[string]any, error) {
	reminder, _ := body["reminder"].(bool)
	switch subjectKind {
	case "memory_record":
		m, err := scanMemory(tx.QueryRow(ctx, `SELECT `+memoryCols+` FROM memory_items WHERE id = $1 AND tenant = $2`, id, tenant))
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		var nonce string
		if err := tx.QueryRow(ctx, `SELECT nonce FROM memory_items WHERE id = $1`, id).Scan(&nonce); err != nil {
			return nil, err
		}
		return memoryCardBody(m, nonce), nil
	case "subject_request":
		r, err := loadSubjectRequest(ctx, tx, tenant, id, false)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		card := r.card()
		if reminder {
			card["reminder"] = true
		}
		return card, nil
	}
	return nil, nil
}

// mayRead says whether a person may see data of a class now (hasData of their groups).
func (c *Core) mayRead(ctx context.Context, tx pgx.Tx, now time.Time, tenant, person, mark string) (bool, error) {
	cat, err := c.Catalog(ctx)
	if err != nil {
		return false, err
	}
	tv := cat.Tenants[tenant]
	p, err := loadPrincipal(ctx, tx, now, tenant, person)
	if errors.Is(err, pgx.ErrNoRows) || tv == nil {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return hasData(tv, groupClosure(tv, p.Groups), mark), nil
}
