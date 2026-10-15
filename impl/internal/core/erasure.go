// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/ids"
	"github.com/HunterEwok/botaru/impl/internal/manifest"
	"github.com/HunterEwok/botaru/impl/internal/store"
)

// Early deletion goes only through an erasure request (RET-7): a recorded object with a step
// per store. The erasure loop erases the core stores itself and closes the channel steps by
// their declaration; a sink or a monitoring adapter gets an erasure work item through claim
// and answers with an outcome (RET-4). The request stays open while a step is neither done
// nor not_applicable, and raises alarm.erasure.overdue at its deadline.

// defaultSubjectRequestDays is the deadline of a request when privacy.subject_request_days
// is not set (SEC-11).
const defaultSubjectRequestDays = 30

// erasureRetry is the pause before an edge step that failed retryably is offered again.
const erasureRetry = time.Minute

// coreStores are the content stores of the core in the order they are erased.
var coreStores = []string{"tasks", "deliveries", "toolcalls", "sessions", "memory", "files"}

// ErasureScope is the scope of an erasure request (deletion-request).
type ErasureScope struct {
	Tenant  string   `json:"tenant"`
	Subject string   `json:"subject,omitempty"`
	Bot     string   `json:"bot,omitempty"`
	IDs     []string `json:"ids,omitempty"`
}

func (s ErasureScope) kind() string {
	switch {
	case s.Subject != "":
		return "tenant_subject"
	case s.Bot != "":
		return "bot"
	case len(s.IDs) > 0:
		return "ids"
	}
	return "tenant"
}

// erasureSpec is a request to record.
type erasureSpec struct {
	Scope   ErasureScope
	Reason  string
	Author  string // a principal, or "" for system
	Needles []string
}

var storeKeyBad = regexp.MustCompile(`[^a-z0-9_]`)

// storeKey is the key of a store in the request results. A name that sanitising changed or
// that is too long gets a hash of the kind and the name, so two stores never share a key and
// a step (unique per request and key) is never lost.
func storeKey(kind, name string) string {
	k := kind + "_" + storeKeyBad.ReplaceAllString(strings.ToLower(name), "_")
	if k == kind+"_"+name && len(k) <= 32 {
		return k
	}
	sum := sha256.Sum256([]byte(kind + "\x00" + name))
	if len(k) > 23 {
		k = k[:23]
	}
	return k + "_" + hex.EncodeToString(sum[:])[:8]
}

func subjectRequestDays(tv *TenantView) int {
	if tv != nil && tv.Spec.Privacy.SubjectRequestDays > 0 {
		return tv.Spec.Privacy.SubjectRequestDays
	}
	return defaultSubjectRequestDays
}

// requestErasure records an erasure request with its steps before a byte is deleted (RET-7).
// forget reaches only memory and sessions (MEM-4); every other reason reaches every store of
// the tenant with content.
func (c *Core) requestErasure(ctx context.Context, tx pgx.Tx, now time.Time, tv *TenantView, s erasureSpec) (string, error) {
	id := ids.New(ids.Erasure)
	author := map[string]any{"kind": "system"}
	if s.Author != "" && s.Author != "system" {
		author = map[string]any{"kind": "principal", "id": s.Author}
	}
	scope, _ := json.Marshal(s.Scope)
	var needles []string
	if len(s.Needles) > 0 {
		needles = s.Needles
	}
	if _, err := tx.Exec(ctx, `INSERT INTO erasure_requests (id, tenant, subject, reason, author, state, created_at, scope, deadline_at, needles, updated_at)
		VALUES ($1, $2, $3, $4, $5, 'requested', $6, $7, $8, $9, $6)`,
		id, tv.Name, nullable(s.Scope.Subject), s.Reason, author, now, scope, now.Add(time.Duration(subjectRequestDays(tv))*24*time.Hour), needles); err != nil {
		return "", err
	}
	if err := addErasureSteps(ctx, tx, now, tv.Name, tv, id, s.Reason); err != nil {
		return "", err
	}
	if err := c.appendEvent(ctx, tx, now, tv.Name, Event{Type: "erasure.requested", Subject: id, Data: map[string]any{
		"request": id, "scope_kind": s.Scope.kind(), "reason": s.Reason,
	}}); err != nil {
		return "", err
	}
	_, err := c.appendAudit(ctx, tx, now, Audit{
		Tenant: tv.Name, Kind: "command", Actor: Actor{Principal: principalOr(s.Author), Via: "core"},
		Command: "erasure.request", Target: &Target{Kind: "erasure", ID: id}, OK: true,
		After: map[string]any{"reason": s.Reason, "scope_kind": s.Scope.kind()},
	})
	_ = notifyWork(ctx, tx)
	return id, err
}

// addErasureSteps records a step per store a request reaches: forget reaches only memory and
// sessions (MEM-4), every other reason every store of the tenant with content. tv is nil for
// a tenant no longer in the catalog; then it has no channels.
func addErasureSteps(ctx context.Context, tx pgx.Tx, now time.Time, tenant string, tv *TenantView, id, reason string) error {
	var channels map[string]manifest.Channel
	if tv != nil {
		channels = tv.Channels
	}
	type step struct{ store, kind, target string }
	var steps []step
	for _, st := range coreStores {
		if reason == "forget" && st != "sessions" && st != "memory" {
			continue
		}
		// A retired identity's sealed data is its memory of the bot scope (MEM-9).
		if reason == "bot_retired" && st != "memory" {
			continue
		}
		steps = append(steps, step{st, "core", st})
	}
	if reason != "forget" && reason != "bot_retired" {
		for _, n := range slices.Sorted(mapKeys(channels)) {
			steps = append(steps, step{storeKey("channel", n), "channel", n})
		}
		rows, err := tx.Query(ctx, `SELECT DISTINCT kind, target FROM workers WHERE tenant = $1 AND kind IN ('sink', 'monitor') AND state <> 'revoked' AND target IS NOT NULL ORDER BY 1, 2`, tenant)
		if err != nil {
			return err
		}
		for rows.Next() {
			var k, t string
			if err := rows.Scan(&k, &t); err != nil {
				rows.Close()
				return err
			}
			steps = append(steps, step{storeKey(k, t), k, t})
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
	}
	for _, st := range steps {
		if _, err := tx.Exec(ctx, `INSERT INTO erasure_steps (id, request, tenant, store, kind, target, state, updated_at) VALUES ($1, $2, $3, $4, $5, $6, 'pending', $7)
			ON CONFLICT (request, store) DO NOTHING`, ids.New(ids.Step), id, tenant, st.store, st.kind, st.target, now); err != nil {
			return err
		}
	}
	return nil
}

func principalOr(p string) string {
	if p == "" {
		return "system"
	}
	return p
}

// erasureRow is a request as the loop reads it.
type erasureRow struct {
	ID, Tenant, Reason, State string
	Scope                     ErasureScope
	Needles                   []string
	Deadline                  time.Time
}

func loadErasure(ctx context.Context, q store.Querier, id string, lock bool) (*erasureRow, error) {
	sql := `SELECT id, tenant, reason, state, scope, coalesce(needles, '{}'), deadline_at FROM erasure_requests WHERE id = $1`
	if lock {
		sql += ` FOR UPDATE`
	}
	var r erasureRow
	var scope []byte
	if err := q.QueryRow(ctx, sql, id).Scan(&r.ID, &r.Tenant, &r.Reason, &r.State, &scope, &r.Needles, &r.Deadline); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(scope, &r.Scope); err != nil {
		return nil, err
	}
	return &r, nil
}

// erasureLoop runs the open requests: core steps and channel steps now, then the outcome of
// each request; an open request past its deadline raises its alarm. It also runs the person's
// requests: the reminder at half of the deadline and the overdue notice (SEC-11).
func (c *Core) erasureLoop(ctx context.Context) error {
	var open []string
	err := c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		open = open[:0]
		// The requests with work left, the least recently touched first, so that requests
		// waiting on an edge store never starve newer ones; a failed request comes back only
		// after erasure.retry.
		rows, err := tx.Query(ctx, `SELECT id FROM erasure_requests r WHERE state IN ('requested', 'running')
			OR (state = 'failed' AND EXISTS (SELECT 1 FROM erasure_steps s WHERE s.request = r.id AND s.state = 'pending'))
			ORDER BY updated_at, created_at LIMIT 100`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			open = append(open, id)
		}
		return rows.Err()
	})
	if err != nil {
		return err
	}
	cat, err := c.Catalog(ctx)
	if err != nil {
		return err
	}
	for _, id := range open {
		if err := c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
			r, err := loadErasure(ctx, tx, id, true)
			if err != nil {
				return err
			}
			if r.State == "done" {
				return nil
			}
			// A request recorded before erasure steps existed (an offboarding on an older
			// build) gets its steps now, so it is never closed with nothing erased.
			var n int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM erasure_steps WHERE request = $1`, r.ID).Scan(&n); err != nil {
				return err
			}
			if n == 0 {
				if err := addErasureSteps(ctx, tx, now, r.Tenant, cat.Tenants[r.Tenant], r.ID, r.Reason); err != nil {
					return err
				}
			}
			if err := c.runCoreSteps(ctx, tx, now, cat, r); err != nil {
				return err
			}
			return c.settleErasure(ctx, tx, now, r)
		}); err != nil {
			return err
		}
	}
	if err := c.erasureOverdue(ctx); err != nil {
		return err
	}
	return c.subjectRequestTimers(ctx, cat)
}

// runCoreSteps erases the core stores of a request and closes its channel steps by the
// driver's declaration: a channel that stores no content is not_applicable with the basis
// declared_none; the erase of a channel cache arrives in v0.2, so any other channel fails
// (SEC-11, RET-11).
func (c *Core) runCoreSteps(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, r *erasureRow) error {
	rows, err := tx.Query(ctx, `SELECT store, kind, target FROM erasure_steps WHERE request = $1 AND state = 'pending' AND kind IN ('core', 'channel') ORDER BY id`, r.ID)
	if err != nil {
		return err
	}
	type step struct{ store, kind, target string }
	var steps []step
	for rows.Next() {
		var s step
		if err := rows.Scan(&s.store, &s.kind, &s.target); err != nil {
			rows.Close()
			return err
		}
		steps = append(steps, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	set := func(store, state, basis, reason string) error {
		_, err := tx.Exec(ctx, `UPDATE erasure_steps SET state = $3, basis = $4, reason = $5, updated_at = $6 WHERE request = $1 AND store = $2`,
			r.ID, store, state, nullable(basis), nullable(reason), now)
		return err
	}
	for _, s := range steps {
		if s.kind == "channel" {
			var none bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM workers WHERE tenant = $1 AND kind = 'channel' AND target = $2 AND state <> 'revoked'
				AND hello->'driver'->>'stores_content' = 'none')`, r.Tenant, s.target).Scan(&none); err != nil {
				return err
			}
			if none {
				err = set(s.store, "not_applicable", "declared_none", "")
			} else {
				err = set(s.store, "failed", "", "not_ready")
			}
			if err != nil {
				return err
			}
			continue
		}
		if err := c.eraseCore(ctx, tx, now, cat, r, s.target); err != nil {
			return err
		}
		if err := set(s.store, "done", "", ""); err != nil {
			return err
		}
	}
	return nil
}

// eraseCore deletes the content of one core store in the scope of a request: a task keeps its
// row as a tombstone without body and result (task.purge, RET-3); a delivery and a tool call
// lose their content; a session and a memory record are deleted (MEM-4). A live task in a
// tenant or person scope is canceled first, so no attempt writes into a purged task.
func (c *Core) eraseCore(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, r *erasureRow, st string) error {
	sc := r.Scope
	if sc.Bot != "" {
		// The bot scope reaches only the identity's own memory, none in the person stores.
		return nil
	}
	// tasksIn selects the tasks of the scope: the person's own and those carrying their data
	// (AUD-5).
	tasksIn := `SELECT id FROM tasks WHERE tenant = $1 AND ($2 = '' OR initiator = $2 OR record->'personal_subjects' ? $2)
		AND (cardinality($3::text[]) = 0 OR id = ANY($3))`
	args := []any{r.Tenant, sc.Subject, idsWith(sc.IDs, "t_")}
	switch st {
	case "tasks":
		if len(sc.IDs) > 0 && len(idsWith(sc.IDs, "t_")) == 0 {
			return nil
		}
		{
			// A live task in the scope is canceled first, by person, tenant or id alike.
			var live []string
			rows, err := tx.Query(ctx, tasksIn+` AND state IN ('submitted', 'working')`, args...)
			if err != nil {
				return err
			}
			for rows.Next() {
				var id string
				if err := rows.Scan(&id); err != nil {
					rows.Close()
					return err
				}
				live = append(live, id)
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}
			if len(live) > 0 {
				if err := c.cancelTaskIDs(ctx, tx, now, r.Tenant, live, r.Reason); err != nil {
					return err
				}
			}
		}
		_, err := tx.Exec(ctx, `UPDATE tasks SET body = NULL, result = NULL WHERE id IN (`+tasksIn+`) AND (body IS NOT NULL OR result IS NOT NULL)`, args...)
		return err
	case "deliveries":
		dargs := []any{r.Tenant, sc.Subject, idsWith(sc.IDs, "t_"), idsWith(sc.IDs, "d_"), r.Reason, now}
		inScope := `tenant = $1 AND (
			(cardinality($3::text[]) = 0 AND cardinality($4::text[]) = 0 AND ($2 = '' OR recipient = $2 OR task IN (` + tasksIn + `)))
			OR task = ANY($3) OR id = ANY($4))`
		if len(sc.IDs) > 0 && len(idsWith(sc.IDs, "t_", "d_")) == 0 {
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE deliveries SET state = 'withheld', reason = $5, updated_at = $6 WHERE state = 'planned' AND `+inScope, dargs...); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE deliveries SET body = NULL WHERE body IS NOT NULL AND `+inScope, dargs[:4]...)
		return err
	case "toolcalls":
		if len(sc.IDs) > 0 && len(idsWith(sc.IDs, "t_", "c_")) == 0 {
			return nil
		}
		_, err := tx.Exec(ctx, `UPDATE toolcalls SET args = '{}', output = NULL WHERE tenant = $1 AND (args <> '{}' OR output IS NOT NULL) AND (
			((cardinality($3::text[]) > 0 OR $5) AND task IN (`+tasksIn+`)) OR id = ANY($4))`,
			r.Tenant, sc.Subject, idsWith(sc.IDs, "t_"), idsWith(sc.IDs, "c_"), len(sc.IDs) == 0)
		return err
	case "sessions":
		if len(sc.IDs) > 0 && len(idsWith(sc.IDs, "s_")) == 0 {
			return nil
		}
		_, err := tx.Exec(ctx, `DELETE FROM sessions WHERE tenant = $1 AND ($2 = '' OR subjects @> ARRAY[$2]) AND (cardinality($3::text[]) = 0 OR id = ANY($3))`,
			r.Tenant, sc.Subject, idsWith(sc.IDs, "s_"))
		return err
	case "files":
		// The bytes go, the record stays as a tombstone so the file_ref is never reused (RET-3).
		if len(sc.IDs) > 0 && len(idsWith(sc.IDs, "t_", "file:")) == 0 {
			return nil
		}
		_, err := tx.Exec(ctx, `UPDATE files SET state = 'purged', content = NULL WHERE tenant = $1 AND state <> 'purged' AND (
			(cardinality($4::text[]) = 0 AND ($2 = '' OR producer = $2) AND cardinality($3::text[]) = 0)
			OR producer IN (`+tasksIn+`) AND (cardinality($3::text[]) > 0 OR ($2 <> '' AND cardinality($4::text[]) = 0))
			OR file_ref = ANY($4))`,
			r.Tenant, sc.Subject, idsWith(sc.IDs, "t_"), idsWith(sc.IDs, "file:"))
		return err
	case "memory":
		if len(sc.IDs) > 0 && len(idsWith(sc.IDs, "m_")) == 0 {
			return nil
		}
		_, err := tx.Exec(ctx, `DELETE FROM memory_items WHERE tenant = $1 AND ($2 = '' OR subject = $2) AND (cardinality($3::text[]) = 0 OR id = ANY($3))`,
			r.Tenant, sc.Subject, idsWith(sc.IDs, "m_"))
		return err
	}
	return nil
}

// idsWith keeps the ids with one of the prefixes.
func idsWith(all []string, prefixes ...string) []string {
	out := []string{}
	for _, id := range all {
		for _, p := range prefixes {
			if strings.HasPrefix(id, p) {
				out = append(out, id)
				break
			}
		}
	}
	return out
}

// settleErasure writes the results and the state of a request: done when every step is done
// or not_applicable (a failed monitoring step on a list of ids does not keep it open, RET-4),
// failed when no step is left to run but one failed, else running. A closed request drops its
// needles and resolves its overdue alarm.
func (c *Core) settleErasure(ctx context.Context, tx pgx.Tx, now time.Time, r *erasureRow) error {
	rows, err := tx.Query(ctx, `SELECT store, kind, state FROM erasure_steps WHERE request = $1 ORDER BY store`, r.ID)
	if err != nil {
		return err
	}
	results := map[string]any{}
	open, failed := false, false
	for rows.Next() {
		var store, kind, state string
		if err := rows.Scan(&store, &kind, &state); err != nil {
			rows.Close()
			return err
		}
		switch state {
		case "pending", "leased":
			open = true
		case "failed":
			results[store] = state
			if !(kind == "monitor" && r.Scope.kind() == "ids") {
				failed = true
			}
		default:
			results[store] = state
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	state := "running"
	switch {
	case len(results) == 0 && !open:
		// No step at all: nothing was erased, so the request is not done.
		state = "failed"
	case !open && !failed:
		state = "done"
	case !open:
		state = "failed"
	}
	raw, _ := json.Marshal(results)
	if state == "done" {
		if _, err := tx.Exec(ctx, `UPDATE erasure_requests SET state = 'done', results = $2, needles = NULL, updated_at = $3, closed_at = $3 WHERE id = $1`, r.ID, raw, now); err != nil {
			return err
		}
	} else if _, err := tx.Exec(ctx, `UPDATE erasure_requests SET state = $2, results = $3, updated_at = $4 WHERE id = $1`, r.ID, state, raw, now); err != nil {
		return err
	}
	alarm := Alarm{Tenant: r.Tenant, Name: "erasure.overdue", Subject: AlarmSubject{Kind: "erasure", ID: strings.ToLower(r.ID)}, Severity: "warn"}
	if state == "done" || (state != r.State && state == "failed") {
		ev := "erasure.done"
		if state == "failed" {
			ev = "erasure.failed"
		}
		if err := c.appendEvent(ctx, tx, now, r.Tenant, Event{Type: ev, Subject: r.ID, Data: map[string]any{
			"request": r.ID, "scope_kind": r.Scope.kind(), "reason": r.Reason, "results": results,
		}}); err != nil {
			return err
		}
	}
	if state == "done" {
		if err := c.resolveAlarm(ctx, tx, now, alarm); err != nil {
			return err
		}
		if r.Scope.Bot != "" {
			if err := botErased(ctx, tx, now, r.Scope.Bot); err != nil {
				return err
			}
		}
		return c.erasureClosed(ctx, tx, now, r, results)
	}
	return nil
}

// erasureOverdue raises the alarm of every open request past its deadline, once (RET-7).
func (c *Core) erasureOverdue(ctx context.Context) error {
	return c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		rows, err := tx.Query(ctx, `UPDATE erasure_requests SET overdue_at = $1 WHERE state <> 'done' AND deadline_at <= $1 AND overdue_at IS NULL RETURNING id, tenant`, now)
		if err != nil {
			return err
		}
		type due struct{ id, tenant string }
		var ds []due
		for rows.Next() {
			var d due
			if err := rows.Scan(&d.id, &d.tenant); err != nil {
				rows.Close()
				return err
			}
			ds = append(ds, d)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, d := range ds {
			if err := c.fireAlarm(ctx, tx, now, Alarm{Tenant: d.tenant, Name: "erasure.overdue", Subject: AlarmSubject{Kind: "erasure", ID: strings.ToLower(d.id)}, Severity: "warn"}); err != nil {
				return err
			}
		}
		return nil
	})
}

// claimErasure takes the oldest pending erasure step of this sink or monitoring adapter
// (RET-4): the work item carries the scope and, until the request closes, the needles.
func (c *Core) claimErasure(ctx context.Context, tx pgx.Tx, now time.Time, w *Worker) (*WorkItem, error) {
	// A match of a person's request comes first: its card waits for it (SEC-11).
	if item, err := c.claimMatch(ctx, tx, now, w); err != nil || item != nil {
		return item, err
	}
	var id, request, store string
	var leaseNo int
	err := tx.QueryRow(ctx, `SELECT id, request, lease_no, store FROM erasure_steps WHERE tenant = $1 AND kind = $2 AND target = $3 AND state = 'pending'
		AND (run_at IS NULL OR run_at <= $4) ORDER BY updated_at LIMIT 1 FOR UPDATE SKIP LOCKED`, w.Tenant, w.Kind, w.Target, now).Scan(&id, &request, &leaseNo, &store)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r, err := loadErasure(ctx, tx, request, false)
	if err != nil {
		return nil, err
	}
	leaseNo++
	item, err := c.newLease(ctx, tx, now, w, "erasure", id, leaseNo, c.cfg.LeaseS, "")
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE erasure_steps SET state = 'leased', lease_no = $2, updated_at = $3 WHERE id = $1`, id, leaseNo, now); err != nil {
		return nil, err
	}
	scope := map[string]any{"tenant": r.Scope.Tenant}
	if r.Scope.Subject != "" {
		scope["subject"] = r.Scope.Subject
	}
	if len(r.Scope.IDs) > 0 {
		scope["ids"] = r.Scope.IDs
		// The erasure of an erase_match gives an edge store the ids its own match found.
		var raw []byte
		err := tx.QueryRow(ctx, `SELECT found FROM subject_requests WHERE erasure = $1 AND kind = 'erase_match'`, r.ID).Scan(&raw)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		var found map[string][]string
		if len(raw) > 0 && json.Unmarshal(raw, &found) == nil && len(found[store]) > 0 {
			scope["ids"] = found[store]
		}
	}
	payload := map[string]any{
		"request_id": r.ID, "op": "erase", "reason": r.Reason,
		"target": map[string]any{"kind": w.Kind, "name": w.Target}, "scope": scope,
	}
	if len(r.Needles) > 0 {
		payload["needles"] = r.Needles
	}
	item.Payload = payload
	return item, nil
}

// erasureOutcome is erasure-outcome.
type erasureOutcome struct {
	Outcome string   `json:"outcome"`
	Reason  string   `json:"reason"`
	Basis   string   `json:"basis"`
	IDs     []string `json:"ids"`
}

// completeErasure writes the outcome of an edge store into its step (RET-4).
func (c *Core) completeErasure(ctx context.Context, tx pgx.Tx, now time.Time, l *leaseRow, out map[string]any) error {
	if err := c.spec.Validate("erasure-outcome", out); err != nil {
		return refuse("input_schema_invalid", "%v", err)
	}
	var o erasureOutcome
	raw, _ := json.Marshal(out)
	if err := json.Unmarshal(raw, &o); err != nil {
		return err
	}
	if match, err := isMatchStep(ctx, tx, l.WorkID); err != nil {
		return err
	} else if match {
		return c.endMatchStep(ctx, tx, now, l, o.Outcome, o.Reason, o.IDs, false)
	}
	return c.endErasureStep(ctx, tx, now, l, o.Outcome, o.Basis, o.Reason, false)
}

// endErasureStep closes or requeues an erasure step at the end of its lease; the loop settles
// the request.
func (c *Core) endErasureStep(ctx context.Context, tx pgx.Tx, now time.Time, l *leaseRow, state, basis, reason string, retry bool) error {
	if match, err := isMatchStep(ctx, tx, l.WorkID); err != nil {
		return err
	} else if match {
		return c.endMatchStep(ctx, tx, now, l, state, reason, nil, retry)
	}
	if retry {
		_, err := tx.Exec(ctx, `UPDATE erasure_steps SET state = 'pending', run_at = $3, reason = $4, updated_at = $5 WHERE id = $1 AND lease_no = $2 AND state = 'leased'`,
			l.WorkID, l.LeaseNo, now.Add(erasureRetry), nullable(reason), now)
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE erasure_steps SET state = $3, basis = $4, reason = $5, updated_at = $6 WHERE id = $1 AND lease_no = $2 AND state = 'leased'`,
		l.WorkID, l.LeaseNo, state, nullable(basis), nullable(reason), now)
	if err == nil {
		_ = notifyWork(ctx, tx)
	}
	return err
}

// erasureClosed tells the person who asked the result of each store as a code text when a
// request of a person's request closes (SEC-11).
func (c *Core) erasureClosed(ctx context.Context, tx pgx.Tx, now time.Time, r *erasureRow, results map[string]any) error {
	if r.Reason != "subject_request" {
		return nil
	}
	var author string
	err := tx.QueryRow(ctx, `SELECT author FROM subject_requests WHERE erasure = $1`, r.ID).Scan(&author)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	cat, err := c.Catalog(ctx)
	if err != nil {
		return err
	}
	tv := cat.Tenants[r.Tenant]
	if tv == nil {
		return nil
	}
	channel, chat, ok, err := cardChat(ctx, tx, tv, author)
	if err != nil || !ok {
		return err
	}
	var lines []string
	keys := make([]string, 0, len(results))
	for k := range results {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		lines = append(lines, k+": "+results[k].(string))
	}
	_, err = c.planDelivery(ctx, tx, now, plannedDelivery{
		Tenant: r.Tenant, Kind: "notice", Addr: "approval_initiator", TargetKind: "chat", TargetID: chat.ID(),
		ExecKind: "channel", ExecName: channel, Recipient: author, DataOrigin: "code",
		Body: map[string]any{"form": "text_code", "text_key": "notice.erasure_done", "params": map[string]any{"request": r.ID, "results": strings.Join(lines, "\n")}},
	})
	return err
}
