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

// subjectCardOf is the card of a request kind: a copy has its own (SEC-10).
func subjectCardOf(kind string) string {
	if kind == "export" {
		return "subject_request_export"
	}
	return subjectCard
}

// maxMatchIDs is the most object ids an erase_match records; the erasure request takes as many.
const maxMatchIDs = 1000

type subjectRequest struct {
	ID, Tenant, Kind, Subject, Author, State, Nonce, Erasure string
	Found                                                    map[string][]string
	Created, Deadline                                        time.Time
}

// loadSubjectRequest reads a request of the tenant; the farm tenant reads a request of any
// tenant, whose card its admins get when the tenant has no second decider (SEC-11).
func loadSubjectRequest(ctx context.Context, q store.Querier, tenant, id string, lock bool) (*subjectRequest, error) {
	sql := `SELECT id, tenant, kind, coalesce(subject, ''), author, state, nonce, coalesce(erasure, ''), found, created_at, deadline_at
		FROM subject_requests WHERE id = $1 AND (tenant = $2 OR $2 = 'farm')`
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
	if r.Kind == "export" {
		title = "Send " + r.Subject + " a copy of their data, encrypted with their key"
	}
	if r.Kind == "erase_match" {
		counts := map[string]any{}
		for k, v := range r.Found {
			counts[k] = len(v)
		}
		ctx["found"] = counts
	}
	return map[string]any{
		"card": subjectCardOf(r.Kind), "subject_kind": "subject_request", "id": r.ID, "nonce": r.Nonce,
		"title": cut(title, 200), "state": r.State, "buttons": buttons, "context": ctx,
		"expires_at": fmtTime(r.Deadline),
		"footer":     map[string]any{"why_you": "admin:people", "until": fmtTime(r.Deadline), "on_silence": "the request is not executed"},
	}
}

// createSubjectRequest records a person's request and plans its card to every active person
// of the tenant with admin:people except the author. An erase_match first asks every sink and
// monitoring adapter of the tenant to match the needles, which it keeps only until the last
// answer; the card goes out then (SEC-11).
func (c *Core) createSubjectRequest(ctx context.Context, tx pgx.Tx, now time.Time, tv *TenantView, kind, subject, author string, found map[string][]string, needles []string) (string, error) {
	// A repeated request about the same person while one is open is that request (IDM-3).
	if kind == "erase" || kind == "export" {
		var id string
		err := tx.QueryRow(ctx, `SELECT id FROM subject_requests WHERE tenant = $1 AND kind = $3 AND subject = $2 AND state = 'pending'`, tv.Name, subject, kind).Scan(&id)
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
	matching := false
	if kind == "erase_match" && len(needles) > 0 {
		var err error
		if matching, err = addMatchSteps(ctx, tx, now, tv.Name, id); err != nil {
			return "", err
		}
		if matching {
			if _, err := tx.Exec(ctx, `UPDATE subject_requests SET needles = $2 WHERE id = $1`, id, needles); err != nil {
				return "", err
			}
			_ = notifyWork(ctx, tx)
		}
	}
	if !matching {
		if err := c.planSubjectCards(ctx, tx, now, tv, id, author, false); err != nil {
			return "", err
		}
	}
	_, err := c.appendAudit(ctx, tx, now, Audit{
		Tenant: tv.Name, Kind: "command", Actor: Actor{Principal: author, Via: "core"},
		Command: "principal.data_request", Target: &Target{Kind: "subject_request", ID: id}, OK: true,
		After: map[string]any{"kind": kind},
	})
	return id, err
}

func (c *Core) planSubjectCards(ctx context.Context, tx pgx.Tx, now time.Time, tv *TenantView, id, author string, reminder bool) error {
	deciders, err := c.adminChats(ctx, tx, now, tv, author, "admin:people")
	if err != nil {
		return err
	}
	var kind string
	if err := tx.QueryRow(ctx, `SELECT kind FROM subject_requests WHERE id = $1`, id).Scan(&kind); err != nil {
		return err
	}
	card := subjectCardOf(kind)
	chats, tenant := deciders, tv.Name
	// A tenant with no second person with admin:people: a farm admin decides and counts as
	// the second person (SEC-11, D-86 (8)).
	if len(deciders) == 0 && tv.Name != "farm" {
		cat, err := c.Catalog(ctx)
		if err != nil {
			return err
		}
		if farm := cat.Tenants["farm"]; farm != nil {
			if chats, err = c.adminChats(ctx, tx, now, farm, "", "admin:people"); err != nil {
				return err
			}
			tenant = "farm"
		}
	}
	seen := map[string]bool{}
	for _, a := range chats {
		if seen[a.Principal] {
			continue
		}
		seen[a.Principal] = true
		// A farm admin who is the author under another principal is not the second person.
		if tenant != tv.Name {
			same, err := sharesIdentity(ctx, tx, tv.Name, author, tenant, a.Principal)
			if err != nil {
				return err
			}
			if same {
				continue
			}
		}
		body := map[string]any{"form": "card", "card": card}
		if reminder {
			body["reminder"] = true
		}
		if _, err := c.planDelivery(ctx, tx, now, plannedDelivery{
			Tenant: tenant, Kind: card, Addr: "admins", TargetKind: "chat",
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
		// The whole identity, as the contract gives it (telegram:1003), and its external part
		// alone, as a store may hold it without the kind (RET-4).
		if !slices.Contains(out, s) {
			out = append(out, s)
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
		press, err := c.checkPress(ctx, tx, now, cat, w, ob, "subject_request", r.ID, []string{subjectCardOf(r.Kind)})
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
		if press.tv.Name != r.Tenant {
			same, err := sharesIdentity(ctx, tx, r.Tenant, r.Author, press.tv.Name, press.principal)
			if err != nil {
				return err
			}
			if same {
				return refuse("forbidden", "the author of a request does not decide it")
			}
		}
		tv := cat.Tenants[r.Tenant]
		if tv == nil {
			return refuse("not_ready", "the tenant is not applied")
		}
		// A farm admin decides another tenant's request only when that tenant has no second
		// person with admin:people (SEC-11).
		if press.tv.Name != r.Tenant {
			deciders, err := c.adminChats(ctx, tx, now, tv, r.Author, "admin:people")
			if err != nil {
				return err
			}
			if len(deciders) > 0 {
				return refuse("forbidden", "the tenant has its own second person with admin:people")
			}
		}
		erasure := ""
		if want == "approved" && r.Kind == "export" {
			// The copy goes only to the export sink with recipient_ref self, encrypted with the
			// person's key (SEC-10).
			sink := tv.Spec.Privacy.ExportSink
			if rec, _, err := exportSink(cat, tv.Name, sink); sink == "" || err != nil || rec != "self" {
				return refuse("not_ready", "the tenant has no export sink with recipient_ref self")
			}
			var key string
			if err := tx.QueryRow(ctx, `SELECT coalesce(recipient_key, '') FROM subject_requests WHERE id = $1`, r.ID).Scan(&key); err != nil {
				return err
			}
			if _, err := c.queueExport(ctx, tx, now, tv.Name, "principal", r.Subject, sink, "self", key, r.ID, press.principal); err != nil {
				return err
			}
		} else if want == "approved" {
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
				// The request records the ids of each store and which stores found nothing,
				// so a replay after a restore erases by these and never searches (RET-11).
				if spec.Found, err = matchRecord(ctx, tx, r); err != nil {
					return err
				}
				if len(spec.Scope.IDs) > maxMatchIDs {
					spec.Scope.IDs = spec.Scope.IDs[:maxMatchIDs]
				}
				// With no id found, an erasure request has nothing to name: a scope of no ids
				// is the whole tenant. A store that could not be searched is then no proof of
				// "nothing to erase", so the request stays open rather than closing as approved
				// with no record of that store (SEC-11); a person rejects it and asks again.
				if len(spec.Scope.IDs) == 0 {
					var failed bool
					if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM subject_match_steps WHERE request = $1 AND state = 'failed')`, r.ID).Scan(&failed); err != nil {
						return err
					}
					if failed {
						return refuse("not_ready", "a store could not be searched and nothing was found; reject the request and ask again")
					}
				}
			}
			if len(spec.Scope.IDs) > 0 || r.Kind == "erase" {
				if erasure, err = c.requestErasure(ctx, tx, now, tv, spec); err != nil {
					return err
				}
				if r.Kind == "erase_match" {
					if err := settleEdgeSteps(ctx, tx, now, r, erasure); err != nil {
						return err
					}
				}
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE subject_requests SET state = $2, decided_by = $3, decided_at = $4, erasure = $5, updated_at = $4 WHERE id = $1`,
			r.ID, want, press.principal, now, nullable(erasure)); err != nil {
			return err
		}
		out.State = want
		if err := c.resolveAlarm(ctx, tx, now, Alarm{Tenant: r.Tenant, Name: "subject_request.overdue",
			Subject: AlarmSubject{Kind: "subject_request", ID: strings.ToLower(r.ID)}, Severity: "warn"}); err != nil {
			return err
		}
		if err := c.appendEvent(ctx, tx, now, r.Tenant, Event{Type: "decision.made", Subject: r.ID, Data: map[string]any{
			"subject": map[string]any{"kind": "subject_request", "id": r.ID}, "card": subjectCardOf(r.Kind), "decision": req.Decision,
			"decided_by": press.principal, "channel": w.Target, "chat_kind": ob.Chat.Kind, "via": "decide",
			"delivered_at": fmtTime(press.deliveredAt), "decided_at": fmtTime(now),
			"response_ms": max(now.Sub(press.deliveredAt).Milliseconds(), 0),
		}}); err != nil {
			return err
		}
		_, err = c.appendAudit(ctx, tx, now, Audit{
			Tenant: r.Tenant, Kind: "decision", Actor: Actor{Principal: press.principal, Via: "channel"},
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
		// A request whose edge stores still match has no card yet, so no reminder either.
		rows, err := tx.Query(ctx, `SELECT id, tenant, author, created_at, deadline_at, reminded_at IS NOT NULL OR EXISTS (SELECT 1 FROM subject_match_steps m
			WHERE m.request = subject_requests.id AND m.state IN ('pending', 'leased')), overdue_at IS NOT NULL
			FROM subject_requests WHERE state = 'pending' ORDER BY created_at LIMIT 200 FOR UPDATE OF subject_requests SKIP LOCKED`)
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
	Kind         string `json:"kind"`
	Subject      string `json:"subject"`
	RecipientKey string `json:"recipient_key"`
}

// PrincipalDataRequest is P2 principal.data_request: a request to erase a principal's data,
// decided on a card by another person with admin:people. A copy (export) is asked only by the
// person (SEC-10).
func (c *Core) PrincipalDataRequest(ctx context.Context, op Operator, req DataRequest) (*Done, error) {
	if req.Kind == "export" {
		// A copy goes only to the person, who asks with /mydata export and their key (SEC-10).
		return nil, refuse("export_recipient_not_subject", "a copy of a person's data is requested only by that person")
	}
	if req.Kind != "erase" {
		return nil, refuse("input_schema_invalid", "unknown kind %s", req.Kind)
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
		id, err := c.createSubjectRequest(ctx, tx, now, tv, "erase", req.Subject, op.Principal, nil, nil)
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
// of the tenant and asks the edge stores to match them (op match); it records only the ids
// found and forgets the strings when the last store answered; another person with
// admin:people confirms the erasure of those objects on a card (SEC-11).
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
		id, err := c.createSubjectRequest(ctx, tx, now, tv, "erase_match", "", op.Principal, found, req.Needles)
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
		spec := erasureSpec{Scope: req.Scope, Reason: req.Reason, Author: op.Principal}
		// An edge store recognizes the person only by the needles (RET-4).
		if req.Scope.Subject != "" {
			var err error
			if spec.Needles, err = needlesOf(ctx, tx, tv.Name, req.Scope.Subject); err != nil {
				return err
			}
		}
		id, err := c.requestErasure(ctx, tx, now, tv, spec)
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

// addMatchSteps records a match step for every sink and monitoring adapter of the tenant, every
// channel that did not declare it stores nothing (a cache or saved attachments) and every agent
// runtime that declared runtime_internal, the edge stores the core does not read (SEC-11,
// RET-4, RET-11, RT-5); it says whether there is one.
func addMatchSteps(ctx context.Context, tx pgx.Tx, now time.Time, tenant, request string) (bool, error) {
	// An agent whose runtime keeps sessions or memory itself is a store the core does not
	// read either: it is searched by its worker id (RT-5).
	rows, err := tx.Query(ctx, `SELECT DISTINCT kind, target FROM workers WHERE tenant = $1 AND state <> 'revoked' AND target IS NOT NULL
		AND (kind IN ('sink', 'monitor') OR kind = 'channel' AND coalesce(hello->'driver'->>'stores_content', '') <> 'none')
		UNION SELECT DISTINCT 'runtime', id FROM workers WHERE tenant = $1 AND state <> 'revoked' AND kind = 'agent' AND `+runtimeInternal+` ORDER BY 1, 2`, tenant)
	if err != nil {
		return false, err
	}
	type store struct{ kind, target string }
	var stores []store
	for rows.Next() {
		var s store
		if err := rows.Scan(&s.kind, &s.target); err != nil {
			rows.Close()
			return false, err
		}
		stores = append(stores, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, err
	}
	for _, s := range stores {
		if _, err := tx.Exec(ctx, `INSERT INTO subject_match_steps (id, request, tenant, store, kind, target, state, updated_at) VALUES ($1, $2, $3, $4, $5, $6, 'pending', $7)
			ON CONFLICT (request, store) DO NOTHING`, ids.New(ids.Step), request, tenant, storeKey(s.kind, s.target), s.kind, s.target, now); err != nil {
			return false, err
		}
	}
	return len(stores) > 0, nil
}

// claimMatch takes the oldest pending match step of this channel, sink, monitoring adapter or
// agent runtime: the work item carries the needles of the request and asks for ids only
// (SEC-11, RET-11, RT-5).
func (c *Core) claimMatch(ctx context.Context, tx pgx.Tx, now time.Time, w *Worker) (*WorkItem, error) {
	var id, request string
	var leaseNo int
	var needles []string
	kind, target := w.Kind, w.Target
	if w.Kind == "agent" {
		kind, target = "runtime", w.ID
	}
	err := tx.QueryRow(ctx, `SELECT m.id, m.request, m.lease_no, coalesce(r.needles, '{}') FROM subject_match_steps m JOIN subject_requests r ON r.id = m.request
		WHERE m.tenant = $1 AND m.kind = $2 AND m.target = $3 AND m.state = 'pending' AND (m.run_at IS NULL OR m.run_at <= $4)
		ORDER BY m.updated_at LIMIT 1 FOR UPDATE OF m SKIP LOCKED`, w.Tenant, kind, target, now).Scan(&id, &request, &leaseNo, &needles)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	leaseNo++
	item, err := c.newLease(ctx, tx, now, w, "erasure", id, leaseNo, c.cfg.LeaseS, "")
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE subject_match_steps SET state = 'leased', lease_no = $2, updated_at = $3 WHERE id = $1`, id, leaseNo, now); err != nil {
		return nil, err
	}
	tgt := map[string]any{"kind": kind, "name": target}
	if kind == "runtime" {
		tgt = map[string]any{"kind": kind, "worker_id": target}
	}
	item.Payload = map[string]any{
		"request_id": request, "op": "match", "reason": "subject_request",
		"target": tgt, "scope": map[string]any{"tenant": w.Tenant},
		"needles": needles,
	}
	return item, nil
}

// isMatchStep says whether the work of an erasure lease is a match step.
func isMatchStep(ctx context.Context, tx pgx.Tx, workID string) (bool, error) {
	var ok bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM subject_match_steps WHERE id = $1)`, workID).Scan(&ok)
	return ok, err
}

// endMatchStep closes or requeues a match step at the end of its lease. The ids an edge store
// found join the request's found ids of that store; when the last step answered the request
// forgets the needles and its card goes out (SEC-11).
func (c *Core) endMatchStep(ctx context.Context, tx pgx.Tx, now time.Time, l *leaseRow, state, reason string, found []string, retry bool) error {
	if retry {
		_, err := tx.Exec(ctx, `UPDATE subject_match_steps SET state = 'pending', run_at = $3, reason = $4, updated_at = $5 WHERE id = $1 AND lease_no = $2 AND state = 'leased'`,
			l.WorkID, l.LeaseNo, now.Add(erasureRetry), nullable(reason), now)
		return err
	}
	var request, tenant, store string
	err := tx.QueryRow(ctx, `UPDATE subject_match_steps SET state = $3, reason = $4, updated_at = $5 WHERE id = $1 AND lease_no = $2 AND state = 'leased'
		RETURNING request, tenant, store`, l.WorkID, l.LeaseNo, state, nullable(reason), now).Scan(&request, &tenant, &store)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	r, err := loadSubjectRequest(ctx, tx, tenant, request, true)
	if err != nil {
		return err
	}
	if state == "done" && len(found) > 0 {
		if r.Found == nil {
			r.Found = map[string][]string{}
		}
		total := 0
		for _, v := range r.Found {
			total += len(v)
		}
		var fresh []string
		for _, id := range found {
			if !slices.Contains(r.Found[store], id) && !slices.Contains(fresh, id) {
				fresh = append(fresh, id)
			}
		}
		// More ids than the request keeps: the store's match fails as a whole, so its erasure
		// fails visibly instead of erasing a part and reporting done (SEC-11).
		if total+len(fresh) > maxMatchIDs {
			_, err := tx.Exec(ctx, `UPDATE subject_match_steps SET state = 'failed', reason = 'match_overflow', updated_at = $2 WHERE id = $1`, l.WorkID, now)
			if err != nil {
				return err
			}
			fresh = nil
		}
		r.Found[store] = append(r.Found[store], fresh...)
		raw, _ := json.Marshal(r.Found)
		if _, err := tx.Exec(ctx, `UPDATE subject_requests SET found = $2, updated_at = $3 WHERE id = $1`, r.ID, raw, now); err != nil {
			return err
		}
	}
	var open bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM subject_match_steps WHERE request = $1 AND state IN ('pending', 'leased'))`, r.ID).Scan(&open); err != nil || open {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE subject_requests SET needles = NULL, updated_at = $2 WHERE id = $1`, r.ID, now); err != nil {
		return err
	}
	if r.State != "pending" {
		return nil
	}
	cat, err := c.Catalog(ctx)
	if err != nil {
		return err
	}
	tv := cat.Tenants[r.Tenant]
	if tv == nil {
		return nil
	}
	return c.planSubjectCards(ctx, tx, now, tv, r.ID, r.Author, false)
}

// settleEdgeSteps closes, at the confirmation of an erase_match, the edge steps of its
// erasure request that have nothing to erase by ids: a store whose match found nothing is
// not_applicable (no_match); a store whose match failed, or that matched nothing because it
// came later, fails, since unmatched is not "not found" (SEC-11). A store with found ids gets
// erase work by its own ids (claimErasure). A runtime that was searched (RT-5) counts the same
// way; one without a match step erases by the request's scope.
func settleEdgeSteps(ctx context.Context, tx pgx.Tx, now time.Time, r *subjectRequest, erasure string) error {
	// A store that answered the match but whose worker was revoked since gets no step from
	// the active workers; it gets one from its match step, so its found ids stay owed and the
	// erasure stays open instead of finishing done without it (SEC-11).
	mrows, err := tx.Query(ctx, `SELECT store, kind, target FROM subject_match_steps WHERE request = $1 ORDER BY store`, r.ID)
	if err != nil {
		return err
	}
	type edge struct{ store, kind, target string }
	var edges []edge
	for mrows.Next() {
		var e edge
		if err := mrows.Scan(&e.store, &e.kind, &e.target); err != nil {
			mrows.Close()
			return err
		}
		edges = append(edges, e)
	}
	mrows.Close()
	if err := mrows.Err(); err != nil {
		return err
	}
	for _, e := range edges {
		if _, err := tx.Exec(ctx, `INSERT INTO erasure_steps (id, request, tenant, store, kind, target, state, updated_at) VALUES ($1, $2, $3, $4, $5, $6, 'pending', $7)
			ON CONFLICT (request, store) DO NOTHING`, ids.New(ids.Step), erasure, r.Tenant, e.store, e.kind, e.target, now); err != nil {
			return err
		}
	}
	// A channel that declared it stores nothing is closed by the erasure loop (declared_none).
	rows, err := tx.Query(ctx, `SELECT s.store, m.state, coalesce(m.reason, '') FROM erasure_steps s
		LEFT JOIN subject_match_steps m ON m.request = $2 AND m.store = s.store
		WHERE s.request = $1 AND s.state = 'pending' AND (s.kind IN ('sink', 'monitor')
			OR s.kind = 'channel' AND NOT EXISTS (SELECT 1 FROM workers w WHERE w.tenant = s.tenant AND w.kind = 'channel' AND w.target = s.target
				AND w.state <> 'revoked' AND w.hello->'driver'->>'stores_content' = 'none')
			OR s.kind = 'runtime' AND m.store IS NOT NULL) ORDER BY s.store`, erasure, r.ID)
	if err != nil {
		return err
	}
	type step struct {
		store, reason string
		match         *string
	}
	var steps []step
	for rows.Next() {
		var s step
		if err := rows.Scan(&s.store, &s.match, &s.reason); err != nil {
			rows.Close()
			return err
		}
		steps = append(steps, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, s := range steps {
		if len(r.Found[s.store]) > 0 {
			continue
		}
		state, basis, reason := "not_applicable", "no_match", ""
		if s.match == nil || *s.match == "failed" {
			state, basis, reason = "failed", "", s.reason
			if reason == "" {
				reason = "not_ready"
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE erasure_steps SET state = $3, basis = $4, reason = $5, updated_at = $6 WHERE request = $1 AND store = $2`,
			erasure, s.store, state, nullable(basis), nullable(reason), now); err != nil {
			return err
		}
	}
	return nil
}

// sharesIdentity says whether two principals of two tenants are one person: they share a
// channel identity (SEC-11).
func sharesIdentity(ctx context.Context, tx pgx.Tx, tenantA, principalA, tenantB, principalB string) (bool, error) {
	var same bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM identities a JOIN identities b ON a.identity = b.identity
		WHERE a.tenant = $1 AND a.principal = $2 AND b.tenant = $3 AND b.principal = $4)`, tenantA, principalA, tenantB, principalB).Scan(&same)
	return same, err
}

// matchRecord is what an erase_match found, as the erasure request keeps it: the ids per store,
// and an empty list for a store whose match answered without ids; a store whose match failed
// or never answered has no key (RET-11).
func matchRecord(ctx context.Context, tx pgx.Tx, r *subjectRequest) (map[string][]string, error) {
	out := map[string][]string{}
	for k, v := range r.Found {
		out[k] = slices.Clone(v)
	}
	rows, err := tx.Query(ctx, `SELECT store FROM subject_match_steps WHERE request = $1 AND state IN ('done', 'not_applicable') ORDER BY store`, r.ID)
	if err != nil {
		return nil, err
	}
	stores, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	for _, st := range stores {
		if out[st] == nil {
			out[st] = []string{}
		}
	}
	return out, nil
}
