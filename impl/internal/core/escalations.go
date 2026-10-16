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
	"github.com/HunterEwok/botaru/impl/internal/mcp"
	"github.com/HunterEwok/botaru/impl/internal/money"
	"github.com/HunterEwok/botaru/impl/internal/toolspec"
)

// Bot escalations to the farm manager (MGR-5, MGR-6, COM-13, spec/lifecycles/escalation.md). A
// bot escalates only with the built-in escalate (a job through P1 escalation.submit), once per
// live attempt, with closed fields; the record is staged and published in the complete
// transaction with the attempt's untrusted_seen. Code screens it: a duplicate raises dup_count,
// the rates dismiss it (escalation_rate_limited), a chain deeper than 5 is chain_too_deep, an
// open incident or firing alarm on the subject merges it, request_action has the normal path
// (use_normal_path). What remains goes to the necessity lens of the tenant's commission as
// review.escalation.v1, paid from the tenant's review line or, for an urgent, compromise or
// data-risk escalation, from farm's; without money it is parked. The verdict by code under the
// commission's escalation section forwards it as manager.escalations.v1 for the seat. The bot
// gets no answer beyond {staged: true}, and nobody but the tenant's people sees the note.

const (
	escalateToolName = "escalate"
	escalationsType  = "manager.escalations.v1"
	necessityType    = "review.escalation.v1"
	necessityLens    = "necessity"
	escalationTTL    = time.Hour
	escalationDupFor = time.Hour
	escPerWorkerHour = 1
	escPerTenantDay  = 20
	escMaxDepth      = 5
	escForwardMin    = 2
	escUntrustedMin  = 3
)

// escalationFieldsSchema is the closed form of an escalation for the model and for P1 (the
// escalation-submit-request schema, with patterns inline).
func escalationFieldsSchema() map[string]any {
	return map[string]any{
		"type": "object", "additionalProperties": false, "required": []any{"category", "subject", "urgency"},
		"properties": map[string]any{
			"category": map[string]any{"enum": []any{"resource_stuck", "capacity_exhausted", "suspected_compromise", "data_risk", "dependency_down", "other"}},
			"subject": map[string]any{"type": "object", "additionalProperties": false, "required": []any{"kind"}, "properties": map[string]any{
				"kind": map[string]any{"enum": []any{"self", "task_tree", "resource"}},
				"id":   map[string]any{"type": "string", "maxLength": 120, "pattern": "^[a-z][a-z0-9_.:-]{0,119}$"},
			}},
			"observed":           map[string]any{"type": "array", "maxItems": 5, "items": map[string]any{"type": "string", "maxLength": 160, "pattern": "^[a-z][A-Za-z0-9_.:-]{0,159}$"}},
			"requested_response": map[string]any{"enum": []any{"look", "quarantine_self", "hold_resource", "request_action", "pin_selfcheck"}},
			"urgency":            map[string]any{"enum": []any{"normal", "high"}},
			"note":               map[string]any{"type": "string", "maxLength": 500},
		},
	}
}

// escalateTool is escalate in a work attempt of a worker or manager profile role, in a tenant
// whose farm manager is enabled and has the escalation type applied (MGR-5).
func escalateTool(cat *Catalog, g *toolsGrant) *catalogTool {
	if p := g.Purpose; p != "" && p != "work" {
		return nil
	}
	if p := profileOf(cat, g.Role); p != "worker" && p != "manager" {
		return nil
	}
	if !managerEnabled(cat, g.Tenant) {
		return nil
	}
	if _, ok := cat.TaskTypes[escalationsType]; !ok {
		return nil
	}
	desc := "Escalate to the farm manager with closed fields: the subject is your own worker (self), your own task tree or a resource of your task's scope. At most once per attempt; the answer is only {staged: true}."
	return &catalogTool{Name: escalateToolName, Builtin: true, Tool: toolspec.Tool{Description: desc}, Schema: escalationFieldsSchema()}
}

// escalateCall answers escalate.
func (c *Core) escalateCall(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, g *toolsGrant, offered bool, args map[string]any) (*mcp.CallResult, error) {
	if !offered {
		return toolAnswer(map[string]any{"code": "forbidden"}, true), nil
	}
	p, err := c.stageEscalation(ctx, tx, now, g, args)
	if err != nil {
		return nil, err
	}
	if p != nil {
		return toolAnswer(map[string]any{"code": p.Reason}, true), nil
	}
	return toolAnswer(map[string]any{"staged": true}, false), nil
}

// EscalationSubmitRequest is escalation-submit-request.
type EscalationSubmitRequest map[string]any

// EscalationSubmit is P1 escalation.submit of a job worker under its attempt lease (MGR-5).
func (c *Core) EscalationSubmit(ctx context.Context, w *Worker, header string, req EscalationSubmitRequest) (map[string]any, error) {
	cat, err := c.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	var refusal *Problem
	err = c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		refusal = nil
		l, _, err := c.heldLease(ctx, tx, w, header, "escalation.submit", nil)
		if err != nil {
			return err
		}
		if l.Kind != "attempt" || !l.Until.After(now) {
			return refuse("fenced", "an escalation comes from a live attempt")
		}
		if w.Kind != "job" {
			return refuse("forbidden", "an agent escalates through its tool gateway")
		}
		g := &toolsGrant{Tenant: l.Tenant, Task: l.WorkID, Worker: w.ID, Role: w.Role, LeaseNo: l.LeaseNo}
		if err := c.loadGrantTask(ctx, tx, g); err != nil {
			return err
		}
		if escalateTool(cat, g) == nil {
			return refuse("forbidden", "the attempt cannot escalate")
		}
		refusal, err = c.stageEscalation(ctx, tx, now, g, req)
		return err
	})
	if err != nil {
		return nil, err
	}
	if refusal != nil {
		return nil, refusal
	}
	return map[string]any{"staged": true}, nil
}

// stageEscalation checks the schema and the subject and stages the record of the attempt; a
// second one in the same attempt gets escalation_rate_limited (MGR-5).
func (c *Core) stageEscalation(ctx context.Context, tx pgx.Tx, now time.Time, g *toolsGrant, args map[string]any) (*Problem, error) {
	if err := c.rs.validate(escalationFieldsSchema(), args); err != nil {
		return refuse("input_schema_invalid", "the escalation fields are closed"), nil
	}
	subject := mmap(args, "subject")
	id := strings.ToLower(mstr(subject, "id"))
	var root string
	var depth int
	if err := tx.QueryRow(ctx, `SELECT coalesce(record->>'root', id), coalesce((record->>'depth')::int, 0) FROM tasks WHERE id = $1`, g.Task).Scan(&root, &depth); err != nil {
		return nil, err
	}
	// The subject is only the own worker, the own task tree or a resource of the task's scope.
	switch mstr(subject, "kind") {
	case "self":
		if id != "" && id != strings.ToLower(g.Worker) {
			return refuse("input_schema_invalid", "the subject self is the own worker"), nil
		}
		id = strings.ToLower(g.Worker)
	case "task_tree":
		if id != "" && id != strings.ToLower(root) {
			return refuse("input_schema_invalid", "the subject task_tree is the own tree"), nil
		}
		id = strings.ToLower(root)
	case "resource":
		if id == "" || !slices.ContainsFunc(g.Scope, func(s string) bool { return strings.EqualFold(s, id) || strings.EqualFold("res:"+s, id) }) {
			return refuse("input_schema_invalid", "the resource is not in the task's scope"), nil
		}
	}
	fields := map[string]any{"category": args["category"], "subject": map[string]any{"kind": mstr(subject, "kind"), "id": id}, "urgency": args["urgency"]}
	if v, ok := args["observed"]; ok {
		fields["observed"] = v
	}
	if v := mstr(args, "requested_response"); v != "" {
		fields["requested_response"] = v
	}
	raw, err := json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	tag, err := tx.Exec(ctx, `INSERT INTO escalations (id, tenant, task, lease_no, worker, role, fields, note, depth, state, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 'staged', $10, $10) ON CONFLICT DO NOTHING`,
		ids.New(ids.Escalation), g.Tenant, g.Task, g.LeaseNo, g.Worker, g.Role, raw, nullable(mstr(args, "note")), depth, now)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return refuse("escalation_rate_limited", "one escalation per attempt"), nil
	}
	return nil, nil
}

// escalationRow is an escalation as the store keeps it.
type escalationRow struct {
	ID, Tenant, Task, Worker, Role, State string
	LeaseNo, Depth                        int
	Fields                                map[string]any
	Untrusted                             bool
}

func (e *escalationRow) category() string { return mstr(e.Fields, "category") }
func (e *escalationRow) subjectID() string {
	return mstr(mmap(e.Fields, "subject"), "id")
}
func (e *escalationRow) urgent() bool { return mstr(e.Fields, "urgency") == "high" }

// farmPaid says whether the farm pays the necessity review: an urgent escalation, or one of
// suspected compromise or data risk (MGR-6).
func (e *escalationRow) farmPaid() bool {
	return e.urgent() || e.category() == "suspected_compromise" || e.category() == "data_risk"
}

const escalationCols = `id, tenant, task, worker, role, state, lease_no, depth, fields, untrusted`

func scanEscalation(row pgx.Row) (*escalationRow, error) {
	var e escalationRow
	var raw []byte
	err := row.Scan(&e.ID, &e.Tenant, &e.Task, &e.Worker, &e.Role, &e.State, &e.LeaseNo, &e.Depth, &raw, &e.Untrusted)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal(raw, &e.Fields)
	return &e, nil
}

// publishEscalations publishes the escalation a completed attempt staged, with the attempt's
// untrusted_seen, and screens it (MGR-5, MGR-6).
func (c *Core) publishEscalations(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, task string, leaseNo int) error {
	e, err := scanEscalation(tx.QueryRow(ctx, `UPDATE escalations e SET state = 'screened', published_at = $3, updated_at = $3,
		untrusted = coalesce((SELECT (t.record->>'untrusted_seen')::boolean FROM tasks t WHERE t.id = e.task), false)
		WHERE task = $1 AND lease_no = $2 AND state = 'staged' RETURNING `+escalationCols, task, leaseNo, now))
	if err != nil || e == nil {
		return err
	}
	return c.screenEscalation(ctx, tx, now, cat, e)
}

// screenEscalation is the code screening of a published escalation (MGR-6).
func (c *Core) screenEscalation(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, e *escalationRow) error {
	// The duplicate and rate counts of a tenant are read and moved one screening at a time.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(7300151, hashtext($1))`, e.Tenant); err != nil {
		return err
	}
	subject, _ := json.Marshal(mmap(e.Fields, "subject"))
	// A duplicate within an hour raises dup_count of the first record and makes none.
	var first string
	err := tx.QueryRow(ctx, `SELECT id FROM escalations WHERE tenant = $1 AND id <> $2 AND state <> 'staged' AND fields->>'category' = $3
		AND fields->'subject' = $4::jsonb AND published_at > $5 ORDER BY published_at, id LIMIT 1`, e.Tenant, e.ID, e.category(), subject, now.Add(-escalationDupFor)).Scan(&first)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if first != "" {
		if _, err := tx.Exec(ctx, `UPDATE escalations SET dup_count = dup_count + 1, updated_at = $2 WHERE id = $1`, first, now); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `DELETE FROM escalations WHERE id = $1`, e.ID)
		return err
	}
	var perWorker, perTenant int
	if err := tx.QueryRow(ctx, `SELECT count(*) FILTER (WHERE worker = $3 AND published_at > $4), count(*) FILTER (WHERE published_at > $5)
		FROM escalations WHERE tenant = $1 AND id <> $2 AND state <> 'staged' AND coalesce(reason, '') <> 'escalation_rate_limited'`,
		e.Tenant, e.ID, e.Worker, now.Add(-time.Hour), now.Add(-24*time.Hour)).Scan(&perWorker, &perTenant); err != nil {
		return err
	}
	switch {
	case perWorker >= escPerWorkerHour || perTenant >= escPerTenantDay:
		return c.moveEscalation(ctx, tx, now, e, "dismissed", "escalation_rate_limited", "")
	case e.Depth+1 > escMaxDepth:
		return c.moveEscalation(ctx, tx, now, e, "dismissed", "chain_too_deep", "")
	}
	if into, err := escalationMerge(ctx, tx, e); err != nil {
		return err
	} else if into != "" {
		return c.moveEscalation(ctx, tx, now, e, "merged", "", into)
	}
	if mstr(e.Fields, "requested_response") == "request_action" {
		return c.moveEscalation(ctx, tx, now, e, "dismissed", "use_normal_path", "")
	}
	return c.reviewEscalation(ctx, tx, now, cat, e)
}

// escalationMerge finds an open incident or a firing alarm of the tenant on the escalation's
// subject: an incident id, or alarm:<alarmkey>.
func escalationMerge(ctx context.Context, tx pgx.Tx, e *escalationRow) (string, error) {
	id := e.subjectID()
	bare := strings.TrimPrefix(id, "res:")
	var inc string
	err := tx.QueryRow(ctx, `SELECT id FROM incidents WHERE tenant = $1 AND state IN ('open', 'contained', 'mitigating', 'reopened')
		AND lower(subject_id) IN ($2, $3) ORDER BY opened_at LIMIT 1`, e.Tenant, id, bare).Scan(&inc)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	if inc != "" {
		return inc, nil
	}
	var key string
	err = tx.QueryRow(ctx, `SELECT alarmkey FROM alarm_episodes WHERE tenant = $1 AND state = 'firing' AND lower(subject->>'id') IN ($2, $3)
		ORDER BY opened_at LIMIT 1`, e.Tenant, id, bare).Scan(&key)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	if key != "" {
		return cut("alarm:"+key, 80), nil
	}
	return "", nil
}

// moveEscalation writes a state change of an escalation with its audit record (metadata only).
func (c *Core) moveEscalation(ctx context.Context, tx pgx.Tx, now time.Time, e *escalationRow, state, reason, into string) error {
	if _, err := tx.Exec(ctx, `UPDATE escalations SET state = $2, reason = $3, merged_into = coalesce($4, merged_into), updated_at = $5 WHERE id = $1`,
		e.ID, state, nullable(reason), nullable(into), now); err != nil {
		return err
	}
	e.State = state
	_, err := c.appendAudit(ctx, tx, now, Audit{
		Tenant: e.Tenant, Kind: "command", Actor: Actor{Principal: "system", Via: "core"}, Command: "escalation." + state,
		Target: &Target{Kind: "escalation", ID: e.ID}, OK: true, Reason: reason,
		After: map[string]any{"state": state, "category": e.category(), "urgency": mstr(e.Fields, "urgency"), "merged_into": nullable(into)},
	})
	return err
}

// necessityExecutor is the role that reviews the necessity lens of a tenant: the fallback role
// of a commission seat of the lens, else a reviewer role that claims the type, independent of
// the escalating role (COM-7, COM-13).
func (c *Core) necessityExecutor(cat *Catalog, tenant, sourceRole string) string {
	com := mmap(tenantSpecOf(cat, "Governance", tenant), "commission")
	for _, s := range mlist(com, "seats") {
		sm := asMap(s)
		if mstr(sm, "lens") == necessityLens {
			if r := mstr(sm, "fallback_role"); c.independentReviewer(cat, r, necessityType, sourceRole) {
				return r
			}
		}
	}
	for _, rn := range slices.Sorted(mapKeys(cat.Roles)) {
		if cat.Roles[rn].Profile == "reviewer" && c.independentReviewer(cat, rn, necessityType, sourceRole) {
			return rn
		}
	}
	return ""
}

// reviewEscalation puts a screened or parked escalation to the necessity lens: the task
// review.escalation.v1 in the source tenant with the key system:necessity:<escalation>, paid
// from the tenant's review line or farm's; without the money, or without a lens executor, the
// escalation is parked (MGR-6, COM-13).
func (c *Core) reviewEscalation(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, e *escalationRow) error {
	tt, ok := cat.TaskTypes[necessityType]
	role := c.necessityExecutor(cat, e.Tenant, e.Role)
	payer := e.Tenant
	if e.farmPaid() {
		payer = farmTenant
	}
	estimate := money.FromEUR(tt.TreeBudgetEUR)
	park := !ok || role == "" || cat.Tenants[payer] == nil
	if !park {
		if p := c.checkBudget(ctx, tx, now, cat.Tenants[payer], budgetAsk{subject: "system", extra: estimate, line: "review"}); p != nil {
			if p.Reason != "budget_exhausted" {
				return p
			}
			park = true
		}
	}
	if park {
		if e.State == "parked" {
			return nil
		}
		return c.moveEscalation(ctx, tx, now, e, "parked", "", "")
	}
	evidence, err := escalationEvidence(ctx, tx, e)
	if err != nil {
		return err
	}
	input := map[string]any{"view": "escalation/v1", "escalation": e.ID, "category": e.category(), "subject": e.Fields["subject"],
		"urgency": e.Fields["urgency"], "untrusted": e.Untrusted, "evidence": evidence}
	if v, ok := e.Fields["requested_response"]; ok {
		input["requested_response"] = v
	}
	body := map[string]any{"type": necessityType, "goal": "Review the necessity of the escalation.", "params": map[string]any{"input": input}}
	res, err := c.enqueue(ctx, tx, now, cat, enqueueInput{Tenant: e.Tenant, Type: necessityType, Body: body,
		IdempotencyKey: "system:necessity:" + e.ID, Initiator: "system", EnqueuedBy: "system:escalation", Source: reviewSource, SourceID: e.ID,
		EstimateMin: estimate, PayTenant: payer})
	if err != nil {
		return err
	}
	if res.Rejected != nil {
		if e.State == "parked" {
			return nil
		}
		return c.moveEscalation(ctx, tx, now, e, "parked", res.Rejected.Reason, "")
	}
	extra, _ := json.Marshal(map[string]any{"lens": necessityLens, "escalation": e.ID, "executor_role": role, "not_worker": e.Worker})
	if _, err := tx.Exec(ctx, `UPDATE tasks SET record = record || $2::jsonb WHERE id = $1`, res.TaskID, extra); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE escalations SET review_task = $2, paid_by = $3 WHERE id = $1`, e.ID, res.TaskID, payer); err != nil {
		return err
	}
	return c.moveEscalation(ctx, tx, now, e, "in_review", "", "")
}

// escalationEvidence are the figures code reads from the observed references: how many name
// an attempt of the source tree and how many a firing alarm of the tenant. The references
// themselves are not handed on.
func escalationEvidence(ctx context.Context, tx pgx.Tx, e *escalationRow) (map[string]any, error) {
	refs := mstrs(e.Fields, "observed")
	attempts, alarms := 0, 0
	for _, r := range refs {
		switch {
		case strings.HasPrefix(r, "alarm:"):
			var n int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM alarm_episodes WHERE tenant = $1 AND state = 'firing' AND (alarmkey = $2 OR alarm = $2)`,
				e.Tenant, strings.TrimPrefix(r, "alarm:")).Scan(&n); err != nil {
				return nil, err
			}
			if n > 0 {
				alarms++
			}
		default:
			task, n, ok := strings.Cut(r, ":")
			if _, err := strconv.Atoi(n); !ok || err != nil {
				continue
			}
			var found bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM tasks t JOIN tasks s ON coalesce(s.record->>'root', s.id) = coalesce(t.record->>'root', t.id)
				WHERE lower(t.id) = lower($1) AND s.id = $2 AND t.tenant = $3)`, task, e.Task, e.Tenant).Scan(&found); err != nil {
				return nil, err
			}
			if found {
				attempts++
			}
		}
	}
	return map[string]any{"observed": len(refs), "attempts_found": attempts, "alarms_firing": alarms}, nil
}

// necessitySchema is the closed output of the necessity lens: a score of 0-3 and a reason from
// the closed list (COM-13).
var necessitySchema = map[string]any{
	"type": "object", "additionalProperties": false, "required": []any{"lens", "score", "reason"},
	"properties": map[string]any{
		"lens":       map[string]any{"const": necessityLens},
		"score":      map[string]any{"type": "integer", "minimum": 0, "maximum": 3},
		"reason":     map[string]any{"enum": []any{"evidence_below_threshold", "covered_by_alarm", "out_of_scope", "plausible_risk", "clear_risk"}},
		"confidence": map[string]any{"enum": []any{"low", "medium", "high"}},
		"shadow":     map[string]any{"const": false},
	},
}

// escalationOfRecord is the escalation a necessity review is about, or "".
func escalationOfRecord(rec []byte) string {
	var r struct {
		Escalation string `json:"escalation"`
	}
	_ = json.Unmarshal(rec, &r)
	return r.Escalation
}

// payTenantOf is the tenant that pays a task when it is not its own.
func payTenantOf(rec []byte) string {
	var r struct {
		Pay string `json:"pay_tenant"`
	}
	_ = json.Unmarshal(rec, &r)
	return r.Pay
}

// escalationReviewed computes the verdict of a completed necessity review under the
// commission's escalation section: forward with a score of at least forward_if_score_gte (2),
// for an untrusted escalation untrusted_forward_if_score_gte (3); else dismissed with the
// lens reason (COM-13). A forwarded escalation becomes the seat task manager.escalations.v1 in
// the tenant with the key system:escalation:<attempt>, class 3, or class 1 when urgent (MGR-6).
func (c *Core) escalationReviewed(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, id string, result map[string]any) error {
	e, err := scanEscalation(tx.QueryRow(ctx, `SELECT `+escalationCols+` FROM escalations WHERE id = $1 FOR UPDATE`, id))
	if err != nil || e == nil || e.State != "in_review" {
		return err
	}
	score := 0
	if v, ok := mnum(result, "score"); ok {
		score = int(v)
	}
	if _, err := tx.Exec(ctx, `UPDATE escalations SET score = $2 WHERE id = $1`, e.ID, score); err != nil {
		return err
	}
	esc := mmap(tenantSpecOf(cat, "Commission", e.Tenant), "escalation")
	if len(esc) == 0 {
		esc = mmap(mmap(tenantSpecOf(cat, "Governance", e.Tenant), "commission"), "escalation")
	}
	threshold := escForwardMin
	if v, ok := mnum(esc, "forward_if_score_gte"); ok && int(v) > threshold {
		threshold = int(v)
	}
	if e.Untrusted {
		threshold = escUntrustedMin
		if v, ok := mnum(esc, "untrusted_forward_if_score_gte"); ok && int(v) > threshold {
			threshold = int(v)
		}
	}
	if score < threshold {
		reason := mstr(result, "reason")
		if reason != "covered_by_alarm" && reason != "out_of_scope" {
			reason = "evidence_below_threshold"
		}
		return c.moveEscalation(ctx, tx, now, e, "dismissed", reason, "")
	}
	role := managerRole(cat)
	if _, ok := cat.TaskTypes[escalationsType]; !ok || role == "" || !slices.Contains(cat.Roles[role].Claims, escalationsType) || !managerEnabled(cat, e.Tenant) {
		return c.moveEscalation(ctx, tx, now, e, "dismissed", "manager_disabled", "")
	}
	view := map[string]any{"view": "escalation/v1", "escalation": e.ID, "category": e.category(), "subject": e.Fields["subject"],
		"urgency": e.Fields["urgency"], "untrusted": e.Untrusted, "score": score}
	if v, ok := e.Fields["requested_response"]; ok {
		view["requested_response"] = v
	}
	priority := 3
	if e.urgent() {
		priority = 1
	}
	attempt := e.Task + ":" + strconv.Itoa(e.LeaseNo)
	body := map[string]any{"type": escalationsType, "goal": "Look at the escalation.", "params": map[string]any{"input": view}}
	res, err := c.enqueue(ctx, tx, now, cat, enqueueInput{Tenant: e.Tenant, Type: escalationsType, Body: body,
		IdempotencyKey: "system:escalation:" + attempt, Initiator: "system", EnqueuedBy: "system:escalation", Source: seatSource, SourceID: e.ID, Priority: priority})
	if err != nil {
		return err
	}
	if res.Rejected != nil {
		return c.moveEscalation(ctx, tx, now, e, "dismissed", res.Rejected.Reason, "")
	}
	// The seat task gets the source depth plus 1 (spec/lifecycles/escalation.md).
	extra, _ := json.Marshal(map[string]any{"seat": managerSeatID, "executor_role": role, "escalation": e.ID, "depth": e.Depth + 1})
	if _, err := tx.Exec(ctx, `UPDATE tasks SET record = record || $2::jsonb WHERE id = $1`, res.TaskID, extra); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE escalations SET manager_task = $2 WHERE id = $1`, e.ID, res.TaskID); err != nil {
		return err
	}
	return c.moveEscalation(ctx, tx, now, e, "forwarded", "", "")
}

// escalationLoop is the escalation part of the incident loop: an escalation unfinished after
// escalation_ttl (1 h) expires, a parked one goes to review once the money is there, and a
// screened one left by a refused enqueue is tried again (MGR-6, MGR-2).
func (c *Core) escalationLoop(ctx context.Context, cat *Catalog) error {
	var tenants []string
	if err := c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		rows, err := tx.Query(ctx, `SELECT DISTINCT tenant FROM escalations WHERE state IN ('screened', 'parked', 'in_review', 'forwarded', 'taken') ORDER BY tenant`)
		if err != nil {
			return err
		}
		tenants, err = pgx.CollectRows(rows, pgx.RowTo[string])
		return err
	}); err != nil {
		return err
	}
	// One transaction per tenant: a failure in one tenant does not hold back the others.
	for _, tenant := range tenants {
		if err := c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
			if _, err := tx.Exec(ctx, `UPDATE escalations SET state = 'expired', updated_at = $1
				WHERE tenant = $3 AND state IN ('screened', 'parked', 'in_review', 'forwarded', 'taken') AND published_at <= $2`, now, now.Add(-escalationTTL), tenant); err != nil {
				return err
			}
			rows, err := tx.Query(ctx, `SELECT `+escalationCols+` FROM escalations WHERE tenant = $1 AND state IN ('screened', 'parked') ORDER BY published_at, id LIMIT 100 FOR UPDATE`, tenant)
			if err != nil {
				return err
			}
			var es []*escalationRow
			for rows.Next() {
				e, err := scanEscalation(rows)
				if err != nil {
					rows.Close()
					return err
				}
				es = append(es, e)
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}
			for _, e := range es {
				if err := c.reviewEscalation(ctx, tx, now, cat, e); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			c.log.Error("the escalation pass of a tenant failed", "tenant", tenant, "error", err)
		}
	}
	return nil
}
