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

// A person's request about their own data (SEC-11): /mydata erase or P2
// principal.data_request make one about the person; P2 erasure.match makes one by exact
// strings for a person who is not a principal. People with admin:people decide it on a card;
// silence neither runs nor closes it: a reminder at half of the deadline and an overdue alarm
// at the deadline. An approval runs an erasure request with the reason subject_request.

const subjectCard = "subject_request_erase"

// maxMatchIDs is the most object ids an erase_match records; the erasure request takes as many.
const maxMatchIDs = 1000

type subjectRequest struct {
	ID, Tenant, Kind, Subject, Author, State, Nonce, Erasure string
	Found                                                    map[string][]string
	Created, Deadline                                        time.Time
}

func loadSubjectRequest(ctx context.Context, q store.Querier, tenant, id string, lock bool) (*subjectRequest, error) {
	sql := `SELECT id, tenant, kind, coalesce(subject, ''), author, state, nonce, coalesce(erasure, ''), found, created_at, deadline_at
		FROM subject_requests WHERE id = $1 AND tenant = $2`
	if lock {
		sql += ` FOR UPDATE`
	}
	var r subjectRequest
	var found []byte
	if err := q.QueryRow(ctx, sql, id, tenant).Scan(&r.ID, &r.Tenant, &r.Kind, &r.Subject, &r.Author, &r.State, &r.Nonce, &r.Erasure, &found, &r.Created, &r.Deadline); err != nil {
		return nil, err
	}
	if len(found) > 0 {
		if err := json.Unmarshal(found, &r.Found); err != nil {
			return nil, err
		}
	}
	return &r, nil
}

// card draws the decision card: who asked about whom and, for a match, how many objects were
// found per store, never their content (SEC-11).
func (r *subjectRequest) card() map[string]any {
	button := func(code string) map[string]any {
		return map[string]any{"code": code, "subject_kind": "subject_request", "id": r.ID, "nonce": r.Nonce}
	}
	buttons := []any{}
	if r.State == "pending" {
		buttons = append(buttons, button("approve"), button("reject"))
	}
	ctx := map[string]any{"kind": r.Kind, "author": r.Author}
	title := "Erase the data of a person who is not registered"
	if r.Subject != "" {
		ctx["subject"] = r.Subject
		title = "Erase the data of " + r.Subject
	}
	if r.Kind == "erase_match" {
		counts := map[string]any{}
		for k, v := range r.Found {
			counts[k] = len(v)
		}
		ctx["found"] = counts
	}
	return map[string]any{
		"card": subjectCard, "subject_kind": "subject_request", "id": r.ID, "nonce": r.Nonce,
		"title": cut(title, 200), "state": r.State, "buttons": buttons, "context": ctx,
		"expires_at": fmtTime(r.Deadline),
		"footer":     map[string]any{"why_you": "admin:people", "until": fmtTime(r.Deadline), "on_silence": "the request is not executed"},
	}
}

// createSubjectRequest records a person's request and plans its card to every active person
// of the tenant with admin:people except the author.
func (c *Core) createSubjectRequest(ctx context.Context, tx pgx.Tx, now time.Time, tv *TenantView, kind, subject, author string, found map[string][]string) (string, error) {
	// A repeated request about the same person while one is open is that request (IDM-3).
	if kind == "erase" {
		var id string
		err := tx.QueryRow(ctx, `SELECT id FROM subject_requests WHERE tenant = $1 AND kind = 'erase' AND subject = $2 AND state = 'pending'`, tv.Name, subject).Scan(&id)
		if err == nil {
			return id, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return "", err
		}
	}
	id := ids.New(ids.Subject)
	var raw []byte
	if found != nil {
		raw, _ = json.Marshal(found)
	}
	deadline := now.Add(time.Duration(subjectRequestDays(tv)) * 24 * time.Hour)
	if _, err := tx.Exec(ctx, `INSERT INTO subject_requests (id, tenant, kind, subject, author, state, nonce, found, deadline_at, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, 'pending', $6, $7, $8, $9, $9)`, id, tv.Name, kind, nullable(subject), author, ids.Secret(16), raw, deadline, now); err != nil {
		return "", err
	}
	if err := c.planSubjectCards(ctx, tx, now, tv, id, author, false); err != nil {
		return "", err
	}
	_, err := c.appendAudit(ctx, tx, now, Audit{
		Tenant: tv.Name, Kind: "command", Actor: Actor{Principal: author, Via: "core"},
		Command: "principal.data_request", Target: &Target{Kind: "subject_request", ID: id}, OK: true,
		After: map[string]any{"kind": kind},
	})
	return id, err
}

func (c *Core) planSubjectCards(ctx context.Context, tx pgx.Tx, now time.Time, tv *TenantView, id, author string, reminder bool) error {
	chats, err := c.adminChats(ctx, tx, now, tv, author, "admin:people")
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, a := range chats {
		if seen[a.Principal] {
			continue
		}
		seen[a.Principal] = true
		body := map[string]any{"form": "card", "card": subjectCard}
		if reminder {
			body["reminder"] = true
		}
		if _, err := c.planDelivery(ctx, tx, now, plannedDelivery{
			Tenant: tv.Name, Kind: subjectCard, Addr: "admins", TargetKind: "chat",
			TargetID: Chat{Kind: "direct", ExternalID: a.ExternalID}.ID(), ExecKind: "channel", ExecName: a.Channel,
			Recipient: a.Principal, Body: body, DataOrigin: "code", SubjectKind: "subject_request", SubjectID: id,
		}); err != nil {
			return err
		}
	}
	return nil
}

// needlesOf are the external identities of a principal, which an edge store matches by
// (RET-4); principals carry no display name in this version.
func needlesOf(ctx context.Context, q store.Querier, tenant, principal string) ([]string, error) {
	rows, err := q.Query(ctx, `SELECT identity FROM identities WHERE tenant = $1 AND principal = $2 ORDER BY identity`, tenant, principal)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		if _, ext, ok := strings.Cut(s, ":"); ok && ext != "" && !slices.Contains(out, ext) {
			out = append(out, ext)
		}
	}
	return out, rows.Err()
}

// decideSubject is P1 decide on a person's request: a person with admin:people who is not
// its author approves or rejects it from the card (decision point subject_request_erase).
func (c *Core) decideSubject(ctx context.Context, w *Worker, ob OnBehalf, req DecideRequest) (*DecideResult, error) {
	if req.Decision != "approve" && req.Decision != "reject" {
		return nil, refuse("input_schema_invalid", "a request is approved or rejected")
	}
	cat, err := c.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	var out *DecideResult
	err = c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		r, err := loadSubjectRequest(ctx, tx, w.Tenant, req.ID, true)
		if errors.Is(err, pgx.ErrNoRows) {
			return refuse("not_found", "no such request")
		}
		if err != nil {
			return err
		}
		if r.Nonce != req.Nonce {
			return refuse("forbidden", "the nonce is not the card's")
		}
		press, err := c.checkPress(ctx, tx, now, cat, w, ob, "subject_request", r.ID, []string{subjectCard})
		if err != nil {
			return err
		}
		want := map[string]string{"approve": "approved", "reject": "rejected"}[req.Decision]
		out = &DecideResult{SubjectKind: "subject_request", ID: r.ID, Decision: req.Decision, State: r.State}
		if r.State != "pending" {
			if r.State == want {
				return nil
			}
			return refuse("already_decided", "the request is decided")
		}
		if !hasCap(press.tv, press.groups, "admin:people") {
			return refuse("forbidden", "the person has no admin:people")
		}
		if press.principal == r.Author {
			return refuse("forbidden", "the author of a request does not decide it")
		}
		erasure := ""
		if want == "approved" {
			spec := erasureSpec{Scope: ErasureScope{Tenant: r.Tenant}, Reason: "subject_request", Author: press.principal}
			if r.Kind == "erase" {
				spec.Scope.Subject = r.Subject
				if spec.Needles, err = needlesOf(ctx, tx, r.Tenant, r.Subject); err != nil {
					return err
				}
			} else {
				for _, k := range slices.Sorted(mapKeys(r.Found)) {
					spec.Scope.IDs = append(spec.Scope.IDs, r.Found[k]...)
				}
				if len(spec.Scope.IDs) > maxMatchIDs {
					spec.Scope.IDs = spec.Scope.IDs[:maxMatchIDs]
				}
			}
			if len(spec.Scope.IDs) > 0 || r.Kind == "erase" {
				if erasure, err = c.requestErasure(ctx, tx, now, press.tv, spec); err != nil {
					return err
				}
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE subject_requests SET state = $2, decided_by = $3, decided_at = $4, erasure = $5, updated_at = $4 WHERE id = $1`,
			r.ID, want, press.principal, now, nullable(erasure)); err != nil {
			return err
		}
		out.State = want
		if err := c.appendEvent(ctx, tx, now, w.Tenant, Event{Type: "decision.made", Subject: r.ID, Data: map[string]any{
			"subject": map[string]any{"kind": "subject_request", "id": r.ID}, "card": subjectCard, "decision": req.Decision,
			"decided_by": press.principal, "channel": w.Target, "chat_kind": ob.Chat.Kind, "via": "decide",
			"delivered_at": fmtTime(press.deliveredAt), "decided_at": fmtTime(now),
			"response_ms": max(now.Sub(press.deliveredAt).Milliseconds(), 0),
		}}); err != nil {
			return err
		}
		_, err = c.appendAudit(ctx, tx, now, Audit{
			Tenant: w.Tenant, Kind: "decision", Actor: Actor{Principal: press.principal, Via: "channel"},
			Command: "decide", Target: &Target{Kind: "subject_request", ID: r.ID}, OK: true,
			After: map[string]any{"decision": req.Decision, "erasure": erasure},
		})
		return err
	})
	return out, err
}

// subjectRequestTimers reminds the deciders at half of the deadline and raises the overdue
// alarm at the deadline; neither closes the request (SEC-11).
func (c *Core) subjectRequestTimers(ctx context.Context, cat *Catalog) error {
	return c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		rows, err := tx.Query(ctx, `SELECT id, tenant, author, created_at, deadline_at, reminded_at IS NOT NULL, overdue_at IS NOT NULL
			FROM subject_requests WHERE state = 'pending' ORDER BY created_at LIMIT 200 FOR UPDATE SKIP LOCKED`)
		if err != nil {
			return err
		}
		type open struct {
			id, tenant, author string
			created, deadline  time.Time
			reminded, overdue  bool
		}
		var rs []open
		for rows.Next() {
			var o open
			if err := rows.Scan(&o.id, &o.tenant, &o.author, &o.created, &o.deadline, &o.reminded, &o.overdue); err != nil {
				rows.Close()
				return err
			}
			rs = append(rs, o)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, o := range rs {
			tv := cat.Tenants[o.tenant]
			if tv == nil {
				continue
			}
			if !o.reminded && !now.Before(o.created.Add(o.deadline.Sub(o.created)/2)) {
				if err := c.planSubjectCards(ctx, tx, now, tv, o.id, o.author, true); err != nil {
					return err
				}
				if _, err := tx.Exec(ctx, `UPDATE subject_requests SET reminded_at = $2 WHERE id = $1`, o.id, now); err != nil {
					return err
				}
			}
			if !o.overdue && !now.Before(o.deadline) {
				if err := c.fireAlarm(ctx, tx, now, Alarm{Tenant: o.tenant, Name: "subject_request.overdue",
					Subject: AlarmSubject{Kind: "subject_request", ID: strings.ToLower(o.id)}, Severity: "warn"}); err != nil {
					return err
				}
				if _, err := tx.Exec(ctx, `UPDATE subject_requests SET overdue_at = $2 WHERE id = $1`, o.id, now); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// DataRequest is p2-data-request-request.
type DataRequest struct {
	Kind    string `json:"kind"`
	Subject string `json:"subject"`
}

// PrincipalDataRequest is P2 principal.data_request: a request to erase a principal's data,
// decided on a card by another person with admin:people. The extract (export) arrives in
// v0.2 (SEC-10).
func (c *Core) PrincipalDataRequest(ctx context.Context, op Operator, req DataRequest) (*Done, error) {
	if req.Kind != "erase" {
		return nil, refuse("version_unsupported", "the data extract arrives in v0.2")
	}
	if req.Subject == "" {
		return nil, refuse("input_schema_invalid", "subject is required")
	}
	tv, _, err := c.requireTenant(ctx, op.Tenant)
	if err != nil {
		return nil, err
	}
	var out Done
	err = c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM principals WHERE tenant = $1 AND id = $2)`, tv.Name, req.Subject).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return refuse("not_found", "no such principal")
		}
		id, err := c.createSubjectRequest(ctx, tx, now, tv, "erase", req.Subject, op.Principal, nil)
		out = Done{OK: true, ID: id, State: "pending"}
		return err
	})
	return &out, err
}

// MatchRequest is p2-erasure-match-request.
type MatchRequest struct {
	Subject string   `json:"subject"`
	Needles []string `json:"needles"`
}

// ErasureMatch is P2 erasure.match: the core finds the exact strings in the content and memory
// of the tenant, records only the ids it found and forgets the strings at once; another person
// with admin:people confirms the erasure of those objects on a card (SEC-11).
func (c *Core) ErasureMatch(ctx context.Context, op Operator, req MatchRequest) (*Done, error) {
	if len(req.Needles) == 0 {
		return nil, refuse("input_schema_invalid", "needles are required")
	}
	tv, _, err := c.requireTenant(ctx, op.Tenant)
	if err != nil {
		return nil, err
	}
	var out Done
	err = c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		found, err := matchContent(ctx, tx, tv.Name, req.Needles)
		if err != nil {
			return err
		}
		id, err := c.createSubjectRequest(ctx, tx, now, tv, "erase_match", "", op.Principal, found)
		out = Done{OK: true, ID: id, State: "pending"}
		return err
	})
	return &out, err
}

// matchContent finds the objects of classes content and memory of a tenant that contain one
// of the strings, by store.
func matchContent(ctx context.Context, tx pgx.Tx, tenant string, needles []string) (map[string][]string, error) {
	queries := []struct{ store, sql string }{
		{"tasks", `SELECT id FROM tasks WHERE tenant = $1 AND EXISTS (SELECT 1 FROM unnest($2::text[]) n WHERE strpos(coalesce(body::text, '') || ' ' || coalesce(result::text, ''), n) > 0)`},
		{"deliveries", `SELECT id FROM deliveries WHERE tenant = $1 AND body IS NOT NULL AND EXISTS (SELECT 1 FROM unnest($2::text[]) n WHERE strpos(body::text, n) > 0)`},
		{"toolcalls", `SELECT tc.id FROM toolcalls tc JOIN tasks t ON t.id = tc.task WHERE t.tenant = $1
			AND EXISTS (SELECT 1 FROM unnest($2::text[]) n WHERE strpos(tc.args::text || ' ' || coalesce(tc.output::text, ''), n) > 0)`},
		{"sessions", `SELECT DISTINCT s.id FROM sessions s JOIN session_entries e ON e.session = s.id WHERE s.tenant = $1
			AND EXISTS (SELECT 1 FROM unnest($2::text[]) n WHERE strpos(e.content::text, n) > 0)`},
		{"memory", `SELECT id FROM memory_items WHERE tenant = $1 AND EXISTS (SELECT 1 FROM unnest($2::text[]) n WHERE strpos(content, n) > 0)`},
	}
	out := map[string][]string{}
	total := 0
	for _, q := range queries {
		rows, err := tx.Query(ctx, q.sql+` ORDER BY 1 LIMIT `+strconv.Itoa(maxMatchIDs-total), tenant, needles)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, err
			}
			out[q.store] = append(out[q.store], id)
			total++
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		if total >= maxMatchIDs {
			break
		}
	}
	return out, nil
}

// ErasureRequestInput is p2-erasure-request.
type ErasureRequestInput struct {
	Scope  ErasureScope `json:"scope"`
	Reason string       `json:"reason"`
}

// ErasureRequest is P2 erasure.request: an operator's early deletion goes through the log too
// (RET-7).
func (c *Core) ErasureRequest(ctx context.Context, op Operator, req ErasureRequestInput) (*Done, error) {
	if req.Scope.Tenant != op.Tenant {
		return nil, refuse("forbidden", "the operator credential is for another tenant")
	}
	tv, _, err := c.requireTenant(ctx, op.Tenant)
	if err != nil {
		return nil, err
	}
	var out Done
	err = c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		id, err := c.requestErasure(ctx, tx, now, tv, erasureSpec{Scope: req.Scope, Reason: req.Reason, Author: op.Principal})
		out = Done{OK: true, ID: id, State: "requested"}
		return err
	})
	return &out, err
}

// ErasureIDRequest is p2-erasure-id-request.
type ErasureIDRequest struct {
	Op string `json:"op"`
	ID string `json:"id"`
}

// erasureRecord is deletion-request.
func erasureRecord(ctx context.Context, q store.Querier, tenant, id string) (map[string]any, error) {
	var reason, state string
	var scope, author, results []byte
	var created time.Time
	err := q.QueryRow(ctx, `SELECT reason, state, scope, author, results, created_at FROM erasure_requests WHERE id = $1 AND tenant = $2`, id, tenant).
		Scan(&reason, &state, &scope, &author, &results, &created)
	if err != nil {
		return nil, err
	}
	rec := map[string]any{"id": id, "reason": reason, "state": state, "created_at": fmtTime(created)}
	var sc, au, res map[string]any
	_ = json.Unmarshal(scope, &sc)
	_ = json.Unmarshal(author, &au)
	rec["scope"], rec["author"] = sc, au
	if len(results) > 0 && json.Unmarshal(results, &res) == nil && len(res) > 0 {
		rec["results"] = res
	}
	return rec, nil
}

// ErasureStatus is P2 erasure.status.
func (c *Core) ErasureStatus(ctx context.Context, op Operator, req ErasureIDRequest) (map[string]any, error) {
	rec, err := erasureRecord(ctx, c.st.Pool, op.Tenant, req.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, refuse("not_found", "no such erasure request")
	}
	return rec, err
}

// ErasureList is P2 erasure.list: the requests of the tenant, newest first.
func (c *Core) ErasureList(ctx context.Context, op Operator, req TenantRequest) (map[string]any, error) {
	if req.Tenant != "" && req.Tenant != op.Tenant {
		return nil, refuse("forbidden", "the operator credential is for another tenant")
	}
	limit := req.Limit
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := c.st.Pool.Query(ctx, `SELECT id FROM erasure_requests WHERE tenant = $1 AND ($2 = '' OR state = $2) ORDER BY created_at DESC LIMIT $3`, op.Tenant, req.State, limit)
	if err != nil {
		return nil, err
	}
	var idl []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		idl = append(idl, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	items := []any{}
	for _, id := range idl {
		rec, err := erasureRecord(ctx, c.st.Pool, op.Tenant, id)
		if err != nil {
			return nil, err
		}
		items = append(items, rec)
	}
	return map[string]any{"items": items}, nil
}

// ErasureRetry is P2 erasure.retry: the failed steps of an open request run again.
func (c *Core) ErasureRetry(ctx context.Context, op Operator, req ErasureIDRequest) (*Done, error) {
	var out Done
	err := c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		r, err := loadErasure(ctx, tx, req.ID, true)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && r.Tenant != op.Tenant) {
			return refuse("not_found", "no such erasure request")
		}
		if err != nil {
			return err
		}
		if r.State == "done" {
			out = Done{OK: true, ID: r.ID, State: r.State}
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE erasure_steps SET state = 'pending', reason = NULL, run_at = NULL, updated_at = $2 WHERE request = $1 AND state = 'failed'`, r.ID, now); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE erasure_requests SET state = 'running', updated_at = $2 WHERE id = $1`, r.ID, now); err != nil {
			return err
		}
		out = Done{OK: true, ID: r.ID, State: "running"}
		_, err = c.appendAudit(ctx, tx, now, Audit{
			Tenant: op.Tenant, Kind: "command", Actor: Actor{Principal: op.Principal, Via: "farmctl"},
			Command: "erasure.retry", Target: &Target{Kind: "erasure", ID: r.ID}, OK: true,
		})
		_ = notifyWork(ctx, tx)
		return err
	})
	return &out, err
}
