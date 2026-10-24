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
	"github.com/HunterEwok/botaru/impl/internal/manifest"
	"github.com/HunterEwok/botaru/impl/internal/mcp"
	"github.com/HunterEwok/botaru/impl/internal/money"
	"github.com/HunterEwok/botaru/impl/internal/toolspec"
)

// Model grants (MGR-17, MGR-18, GW-7, spec/lifecycles/grant.md). An executor asks for a stronger
// model for its task with request_model_grant (a job through P1 grant.request); code checks the
// right to ask from signs the model does not control, the evidence, the price and the rate, and
// the request waits as pending while the attempt ends without spending. The grant loop gives the
// farm manager seat one manager.grants.v1 batch per tenant and 10-minute window; the holder
// decides with grants_pending and grant_decide, and a person with model_grant.manage decides,
// revokes and lists through P2. The next attempt of the task runs on the slot granted, and P4
// reserves every call in the rest of the grant's cap_eur.

const (
	grantToolName     = "request_model_grant"
	grantsPendingName = "grants_pending"
	grantDecideName   = "grant_decide"
	grantsType        = "manager.grants.v1"
	grantedSlot       = "granted"
	grantWindow       = 10 * time.Minute
	// grantWaitMax, grantRequestsPerRoleDay and grantFailShareMin are the defaults of
	// farm_manager.grant_limits (MGR-17, MGR-18).
	grantWaitMax            = 15 * time.Minute
	grantRequestsPerRoleDay = 3
	grantFailShareMin       = 0.3
	// grantSeatDayMicros is seat_grants_eur_day when the farm charter sets none: the value of
	// the docs example (docs/farm-manager.md), so the seat's daily ceiling is always on.
	grantSeatDayMicros = 20_000_000
	// grantTTLMax is the longest expiry of a grant, and grantTTLDefault the expiry of a decision
	// that names none.
	grantTTLMax     = 6 * time.Hour
	grantTTLDefault = time.Hour
	grantAttemptMax = 2
)

// grantLimits are the farm charter's farm_manager.grant_limits with their defaults.
type grantLimits struct {
	waitMax       time.Duration
	perRoleDay    int
	failShareMin  float64
	seatDayMicros int64
}

func grantLimitsOf(cat *Catalog) grantLimits {
	m := mmap(managerCharter(cat), "grant_limits")
	l := grantLimits{waitMax: grantWaitMax, perRoleDay: grantRequestsPerRoleDay, failShareMin: grantFailShareMin, seatDayMicros: grantSeatDayMicros}
	if d := specDuration(m, "grant_wait_max"); d > 0 {
		l.waitMax = d
	}
	if v, ok := mnum(m, "requests_per_role_day"); ok {
		l.perRoleDay = int(v)
	}
	if v, ok := mnum(m, "eval_fail_share_min"); ok {
		l.failShareMin = v
	}
	if v, ok := mnum(m, "seat_grants_eur_day"); ok {
		l.seatDayMicros = money.FromEUR(v)
	}
	return l
}

// lineOpen says whether a tenant's budget line has money: its month (or day) amount above 0.
// The grant and takeover lines are 0 by default (MGR-17, MGR-21).
func lineOpen(tv *TenantView, line string) bool {
	if tv == nil {
		return false
	}
	lb := tv.Spec.Budgets.Lines[line]
	if lb.MonthEUR != nil {
		return *lb.MonthEUR > 0 && (lb.DayEUR == nil || *lb.DayEUR > 0)
	}
	return lb.DayEUR != nil && *lb.DayEUR > 0
}

// seatProfiles are the model profiles of seats: the farm manager's and the commission's of the
// tenant. A grant never gives one of them to a worker (MGR-17).
func seatProfiles(cat *Catalog, tenant string) []string {
	out := []string{}
	if p := mstr(managerCharter(cat), "model_profile"); p != "" {
		out = append(out, p)
	}
	if p := mstr(mmap(tenantSpecOf(cat, "Governance", tenant), "commission"), "model_profile"); p != "" {
		out = append(out, p)
	}
	for _, name := range slices.Sorted(mapKeys(cat.Roles)) {
		if profileOf(cat, name) == "farm-manager" {
			for _, p := range cat.Roles[name].Models {
				out = append(out, p)
			}
		}
	}
	return out
}

// grantLadder is the part of a role's ladder the tenant allows and that is no seat profile.
func grantLadder(cat *Catalog, tenant string, role manifest.Role) []string {
	if role.ModelGrants == nil {
		return nil
	}
	tv := cat.Tenants[tenant]
	seats := seatProfiles(cat, tenant)
	var out []string
	for _, p := range role.ModelGrants.Ladder {
		if slices.Contains(seats, p) {
			continue
		}
		if tv != nil && len(tv.Spec.ModelsAllowed) > 0 && !slices.Contains(tv.Spec.ModelsAllowed, p) {
			continue
		}
		if _, ok := cat.Profile(tenant, p); !ok {
			continue
		}
		out = append(out, p)
	}
	return out
}

// grantTool is request_model_grant: in a work attempt of a worker or manager profile role with
// a ladder, while the tenant's grant line is above 0 (MGR-17, builtin-tools.yaml).
func (c *Core) grantTool(cat *Catalog, g *toolsGrant) *catalogTool {
	if p := g.Purpose; p != "" && p != "work" {
		return nil
	}
	if p := profileOf(cat, g.Role); p != "worker" && p != "manager" {
		return nil
	}
	ladder := grantLadder(cat, g.Tenant, cat.Roles[g.Role])
	if len(ladder) == 0 || !lineOpen(cat.Tenants[g.Tenant], "grant") {
		return nil
	}
	schema := map[string]any{
		"type": "object", "additionalProperties": false, "required": []any{"reason", "evidence", "requested_profile", "benefit", "attempts"},
		"properties": map[string]any{
			"reason": map[string]any{"enum": []any{"output_schema_invalid_repeated", "context_too_large", "quality_gate_failed", "tool_plan_failed", "low_confidence_declared"}},
			"evidence": map[string]any{"type": "array", "minItems": 1, "maxItems": 2, "uniqueItems": true,
				"items": map[string]any{"type": "string", "maxLength": 64, "pattern": "^[a-z]{1,8}_[0-9A-HJKMNP-TV-Z]{26}:[1-9][0-9]*$"}},
			"requested_profile": map[string]any{"enum": strAny(ladder)},
			"benefit":           map[string]any{"enum": []any{"pass_schema", "fit_context", "pass_quality_gate", "complete_tool_plan"}},
			"attempts":          map[string]any{"type": "integer", "minimum": 1, "maximum": grantAttemptMax},
			"note":              map[string]any{"type": "string", "maxLength": 500},
		},
	}
	desc := "Ask for a stronger model for this task, with a reason and your own attempts of this task as evidence. The answer is {staged: true}, and this attempt then ends and the task waits for a decision; a request the code refuses answers with its code (grant_not_eligible, grant_rate_limited, grant_over_budget)."
	return &catalogTool{Name: grantToolName, Builtin: true, Tool: toolspec.Tool{Description: desc}, Schema: schema}
}

// toolAnswer is the JSON text answer of a built-in tool.
func toolAnswer(m map[string]any, isErr bool) *mcp.CallResult {
	b, _ := json.Marshal(m)
	return &mcp.CallResult{Content: []mcp.Content{{Type: "text", Text: string(b)}}, IsError: isErr}
}

// grantAsker is the attempt that asks: its task, lease, worker and role.
type grantAsker struct {
	Tenant, Task, Worker, Role string
	LeaseNo                    int
}

// grantToolCall answers request_model_grant.
func (c *Core) grantToolCall(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, g *toolsGrant, offered bool, args map[string]any) (*mcp.CallResult, error) {
	if !offered {
		return toolAnswer(map[string]any{"code": "forbidden"}, true), nil
	}
	if err := c.rs.validate(c.grantTool(cat, g).Schema, args); err != nil {
		return toolAnswer(map[string]any{"code": "input_schema_invalid"}, true), nil
	}
	p, err := c.requestGrant(ctx, tx, now, cat, grantAsker{Tenant: g.Tenant, Task: g.Task, Worker: g.Worker, Role: g.Role, LeaseNo: g.LeaseNo}, args)
	if err != nil {
		return nil, err
	}
	if p != nil {
		return toolAnswer(map[string]any{"code": p.Reason}, true), nil
	}
	return toolAnswer(map[string]any{"staged": true}, false), nil
}

// ModelGrantAsk is p1-grant-request.
type ModelGrantAsk map[string]any

// RequestModelGrant is P1 grant.request of a job worker under its attempt lease (MGR-17).
func (c *Core) RequestModelGrant(ctx context.Context, w *Worker, header string, req ModelGrantAsk) (map[string]any, error) {
	cat, err := c.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	var refusal *Problem
	err = c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		refusal = nil
		l, _, err := c.heldLease(ctx, tx, w, header, "grant.request", nil)
		if err != nil {
			return err
		}
		if l.Kind != "attempt" || !l.Until.After(now) {
			return refuse("fenced", "a grant is asked under an attempt lease")
		}
		if w.Kind != "job" {
			return refuse("forbidden", "an agent asks through its tool gateway")
		}
		g := &toolsGrant{Tenant: l.Tenant, Task: l.WorkID, Worker: w.ID, Role: w.Role, LeaseNo: l.LeaseNo}
		if err := c.loadGrantTask(ctx, tx, g); err != nil {
			return err
		}
		t := c.grantTool(cat, g)
		if t == nil {
			return refuse("forbidden", "the role has no grant ladder or the tenant's grant line is 0")
		}
		if err := c.rs.validate(t.Schema, map[string]any(req)); err != nil {
			return refuse("input_schema_invalid", "%v", err)
		}
		refusal, err = c.requestGrant(ctx, tx, now, cat, grantAsker{Tenant: l.Tenant, Task: l.WorkID, Worker: w.ID, Role: w.Role, LeaseNo: l.LeaseNo}, req)
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

// initialInputTokens is the size of a task's initial input in tokens: its body as written at
// enqueue (with the blocks the core put there), counted from above at one token for four bytes.
// Tool output and what the attempt gathered are not in it (MGR-17).
func initialInputTokens(body []byte) int {
	return (len(body) + 3) / 4
}

// grantEstimate is the worst case of a grant: per attempt one call with the task body and the
// profile's output limit at its prices, as P4 reserves a call (GW-2).
func grantEstimate(p manifest.ModelProfile, bodyBytes, attempts int) int64 {
	tier := p.Price.Tier(int64(bodyBytes + 1024))
	per := float64(bodyBytes+1024)*tier.InputCeiling() + float64(p.MaxOutputTokens)*tier.Output
	return int64(per+0.999999) * int64(max(attempts, 1))
}

// requestGrant takes a request: one per task; a refusal by code is recorded as denied, answered
// with its code and the attempt goes on, an accepted one waits as pending and the attempt ends without spending with
// awaiting_model_grant, the task waiting with model_grant_pending (MGR-17).
func (c *Core) requestGrant(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, a grantAsker, args map[string]any) (*Problem, error) {
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM model_grants WHERE task = $1 AND kind = 'grant')`, a.Task).Scan(&exists); err != nil {
		return nil, err
	}
	if exists {
		return refuse("grant_rate_limited", "one grant request per task"), nil
	}
	tv := cat.Tenants[a.Tenant]
	var typ string
	var body, rec []byte
	if err := tx.QueryRow(ctx, `SELECT type, body, record FROM tasks WHERE id = $1`, a.Task).Scan(&typ, &body, &rec); err != nil {
		return nil, err
	}
	tt := cat.TaskTypes[typ]
	reason, benefit, profile := mstr(args, "reason"), mstr(args, "benefit"), mstr(args, "requested_profile")
	attempts := 1
	if v, ok := mnum(args, "attempts"); ok {
		attempts = min(max(int(v), 1), grantAttemptMax)
	}
	if !slices.Contains(grantLadder(cat, a.Tenant, cat.Roles[a.Role]), profile) {
		return refuse("input_schema_invalid", "the profile is not on the role's ladder"), nil
	}
	var untrusted bool
	var r struct {
		Untrusted bool `json:"untrusted_seen"`
	}
	_ = json.Unmarshal(rec, &r)
	untrusted = r.Untrusted
	pr, _ := cat.Profile(a.Tenant, profile)
	estimate := grantEstimate(pr, len(body), attempts)
	request := map[string]any{"reason": reason, "benefit": benefit, "attempts": attempts, "evidence": args["evidence"], "task_type": typ}
	id := ids.New(ids.ModelGrant)
	deny := ""
	limits := grantLimitsOf(cat)
	// The signs of the right to ask are not the attempts' own failures: the initial input above
	// the type's threshold, the type's history from its eval set on the base model, or a
	// person's mark (MGR-17).
	big := tt.Grant != nil && tt.Grant.InputTokensMin > 0 && initialInputTokens(body) > tt.Grant.InputTokensMin
	history := tt.GrantEligible
	if !history && tt.GrantEvalSet != "" {
		if share := typeBaseFailShare(catalogEvalReports(cat), typ, tt.GrantEvalSet); share != nil && *share >= limits.failShareMin {
			history = true
		}
	}
	switch {
	case reason == "context_too_large" && !big, !big && !history:
		deny = "grant_not_eligible"
	}
	// The evidence names attempts of this very task.
	if deny == "" {
		for _, e := range mstrs(args, "evidence") {
			task, n, ok := strings.Cut(e, ":")
			no, err := strconv.Atoi(n)
			if !ok || err != nil || task != a.Task || no > a.LeaseNo {
				deny = "grant_not_eligible"
				break
			}
		}
	}
	// No more than requests_per_role_day accepted requests of a role in the tenant a day.
	if deny == "" {
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM model_grants WHERE tenant = $1 AND role = $2 AND kind = 'grant' AND created_at > $3
			AND request ? 'accepted'`, a.Tenant, a.Role, utcDayStart(now)).Scan(&n); err != nil {
			return nil, err
		}
		if n >= limits.perRoleDay {
			deny = "grant_rate_limited"
		}
	}
	if deny == "" {
		if p := c.checkBudget(ctx, tx, now, tv, budgetAsk{subject: "system", extra: estimate, line: "grant"}); p != nil {
			if p.Reason != "budget_exhausted" {
				return nil, p
			}
			deny = "grant_over_budget"
		}
	}
	state := "pending"
	if deny != "" {
		state = "denied"
	} else {
		request["accepted"] = true
	}
	raw, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO model_grants (id, tenant, task, lease_no, role, kind, line, profile, request, note, attempts, estimate_micros, untrusted, state, reason, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, 'grant', 'grant', $6, $7, $8, $9, $10, $11, $12, $13, $14, $14)`,
		id, a.Tenant, a.Task, a.LeaseNo, a.Role, profile, raw, nullable(mstr(args, "note")), attempts, estimate, untrusted, state, nullable(deny), now); err != nil {
		return nil, err
	}
	if _, err := c.appendAudit(ctx, tx, now, Audit{
		Tenant: a.Tenant, Kind: "command", Actor: Actor{Principal: workerPrincipal(a.Worker), Via: "bot", Role: a.Role, Task: a.Task},
		Command: "grant.request", Target: &Target{Kind: "grant", ID: id}, OK: deny == "", Reason: deny,
		After: map[string]any{"state": state, "profile": profile, "reason": reason, "attempts": attempts},
	}); err != nil {
		return nil, err
	}
	if deny != "" {
		// A refusal by code answers with its code; the record stays and the attempt goes on.
		return refuse(deny, "the request was refused by code"), nil
	}
	return nil, c.endAttemptUnspent(ctx, tx, now, a.Task, a.LeaseNo, "awaiting_model_grant", "model_grant_pending", infinity)
}

// infinity is a run time that never comes: the task waits until code wakes it.
var infinity = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)

// endAttemptUnspent ends a running attempt without spending it, with the lease reason, and puts
// the task back in the queue from runAt with a waiting reason (MGR-17, MGR-18).
func (c *Core) endAttemptUnspent(ctx context.Context, tx pgx.Tx, now time.Time, task string, leaseNo int, reason, wait string, runAt time.Time) error {
	var l leaseRow
	err := tx.QueryRow(ctx, `SELECT work_id, lease_no, kind, tenant, worker_id, epoch, state, lease_until FROM leases
		WHERE work_id = $1 AND state = 'active' AND kind = 'attempt' AND ($2 = 0 OR lease_no = $2) FOR UPDATE`, task, leaseNo).
		Scan(&l.WorkID, &l.LeaseNo, &l.Kind, &l.Tenant, &l.WorkerID, &l.Epoch, &l.State, &l.Until)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := closeLease(ctx, tx, now, &l, "released", reason, "heartbeat", nil, &HeartbeatResult{CancelRequested: true}); err != nil {
		return err
	}
	if err := c.settleMessages(ctx, tx, now, l.WorkID); err != nil {
		return err
	}
	if err := c.requeue(ctx, tx, now, &l, runAt); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE tasks SET reason = $2 WHERE id = $1 AND state = 'submitted'`, task, nullable(wait))
	return err
}

// grantEnded releases an active grant when its attempt ends: back to approved while attempts are
// left and the task has not ended, else consumed (MGR-18). Every end of an attempt lease calls it: closeLease, the reaper,
// the stall check, a rejected result, the deadline and a cancel.
func grantEnded(ctx context.Context, tx pgx.Tx, now time.Time, task string) error {
	_, err := tx.Exec(ctx, `UPDATE model_grants SET state = CASE WHEN attempts_left > 0 AND (expires_at IS NULL OR expires_at > $2)
		AND NOT EXISTS (SELECT 1 FROM tasks t WHERE t.id = $1 AND t.state IN ('completed', 'failed', 'canceled')) THEN 'approved' ELSE 'consumed' END,
		updated_at = $2 WHERE task = $1 AND state = 'active'`, task, now)
	return err
}

// claimGrant gives an attempt the granted slot when its task has an approved grant with attempts
// left before its expiry; the grant becomes active and spends one attempt (MGR-18, GW-7). A grant
// of kind grant is bound to the role that asked and to that role's ladder as the tenant allows it
// now: another role claiming the task runs on its own slot and the grant stays approved. A
// takeover grant goes to the attempt of the new task whichever role claims its type (MGR-21),
// from v0.3 only to the role its eval gate checked (MGR-48).
func claimGrant(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, tenant, task, role string) (string, bool, error) {
	var id, kind, grantRole, profile string
	err := tx.QueryRow(ctx, `SELECT id, kind, role, profile FROM model_grants WHERE task = $1 AND state = 'approved' AND attempts_left > 0 AND expires_at > $2
		ORDER BY created_at LIMIT 1 FOR UPDATE`, task, now).Scan(&id, &kind, &grantRole, &profile)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if kind == "grant" && (grantRole != role || !slices.Contains(grantLadder(cat, tenant, cat.Roles[role]), profile)) {
		return "", false, nil
	}
	// From v0.3 a takeover grant is bound to the role whose eval report passed (MGR-48).
	if kind == "takeover" && grantRole != role {
		return "", false, nil
	}
	if _, err := tx.Exec(ctx, `UPDATE model_grants SET state = 'active', attempts_left = attempts_left - 1, updated_at = $2 WHERE id = $1`, id, now); err != nil {
		return "", false, err
	}
	return profile, true, nil
}

// grantOfHandle is the active grant of a handle on the granted slot: P4 allows the slot only
// under an active grant of this task with its profile before its expiry (GW-7).
func grantOfHandle(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, task, profile string) (string, string, error) {
	var id, line string
	err := q.QueryRow(ctx, `SELECT id, line FROM model_grants WHERE task = $1 AND profile = $2 AND state = 'active' AND expires_at > botaru_now()`, task, profile).Scan(&id, &line)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", refuse("fenced", "no active grant of this task for the granted slot")
	}
	return id, line, err
}

// grantReserve checks a call's reserve against the rest of the grant's cap_eur before it is
// written. A call that does not fit does not start: the grant is consumed and the attempt ends
// without spending, the task going on with the base model (MGR-18, GW-2).
func (c *Core) grantReserve(ctx context.Context, tx pgx.Tx, now time.Time, g *ModelGrant, micros int64) (*Problem, error) {
	var capMicros, used int64
	var state string
	if err := tx.QueryRow(ctx, `SELECT cap_micros, state FROM model_grants WHERE id = $1 FOR UPDATE`, g.GrantID).Scan(&capMicros, &state); err != nil {
		return nil, err
	}
	if state != "active" {
		return refuse("fenced", "the grant is no longer active"), nil
	}
	if err := tx.QueryRow(ctx, `SELECT coalesce(sum(eur_micros), 0) FROM spend WHERE grant_id = $1`, g.GrantID).Scan(&used); err != nil {
		return nil, err
	}
	if used+micros <= capMicros {
		return nil, nil
	}
	if _, err := tx.Exec(ctx, `UPDATE model_grants SET state = 'consumed', reason = 'budget_exhausted', updated_at = $2 WHERE id = $1`, g.GrantID, now); err != nil {
		return nil, err
	}
	if err := c.endAttemptUnspent(ctx, tx, now, g.Task, g.LeaseNo, "budget_exhausted", "", now); err != nil {
		return nil, err
	}
	return refuse("budget_exhausted", "the call does not fit the rest of the grant"), nil
}

// grantRow is a grant record as the store keeps it.
type grantRow struct {
	ID, Tenant, Task, Role, Kind, Line, Profile, State, Reason string
	Request                                                    map[string]any
	Attempts, AttemptsLeft                                     int
	Estimate, Cap                                              int64
	Expires, Decided                                           *time.Time
	DecidedBy                                                  map[string]any
	Untrusted                                                  bool
	Created                                                    time.Time
}

const grantCols = `id, tenant, task, role, kind, line, profile, state, coalesce(reason, ''), request, attempts, attempts_left, estimate_micros, cap_micros, expires_at, decided_at, decided_by, untrusted, created_at`

func scanGrant(row pgx.Row) (*grantRow, error) {
	var r grantRow
	var req, by []byte
	err := row.Scan(&r.ID, &r.Tenant, &r.Task, &r.Role, &r.Kind, &r.Line, &r.Profile, &r.State, &r.Reason, &req, &r.Attempts, &r.AttemptsLeft,
		&r.Estimate, &r.Cap, &r.Expires, &r.Decided, &by, &r.Untrusted, &r.Created)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal(req, &r.Request)
	if len(by) > 0 {
		_ = json.Unmarshal(by, &r.DecidedBy)
	}
	return &r, nil
}

// view is the grant record of spec/model/grant.schema.json; cap_eur in euro cents (BUD-9).
func (r *grantRow) view() map[string]any {
	v := map[string]any{"id": r.ID, "kind": r.Kind, "tenant": r.Tenant, "task": r.Task, "role": r.Role, "profile": r.Profile,
		"line": r.Line, "state": r.State, "untrusted": r.Untrusted, "attempts_left": r.AttemptsLeft}
	if r.Cap > 0 || r.State != "pending" {
		v["cap_eur"] = money.CentsUp(r.Cap)
	}
	if r.Expires != nil {
		v["expires_at"] = fmtTime(*r.Expires)
	}
	if r.DecidedBy != nil {
		v["decided_by"] = r.DecidedBy
	}
	if r.Reason != "" {
		v["reason"] = r.Reason
	}
	return v
}

// grantDecision is one decision on a pending grant, by the seat or a person.
type grantDecision struct {
	Grant, Decision, Profile, ExpiresIn, Reason string
	Attempts                                    int
	CapEUR                                      *float64
}

// grantDecider is who decides: the seat with its epoch and the distrust of its attempt, or a
// person.
type grantDecider struct {
	Seat      bool
	Epoch     int
	Principal string
	Untrusted bool
}

// decideGrant applies one decision to a pending grant of the tenant. The code ceilings apply to
// every decision: at most 2 attempts, an expiry of at most 6 h, cap_eur within the grant line
// and, for the seat, within seat_grants_eur_day; in distrust only the first step of the ladder
// (MGR-18, MGR-22). A ceiling refusal leaves the grant pending and names the reason.
func (c *Core) decideGrant(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, tenant string, d grantDecision, by grantDecider) (string, string, error) {
	r, err := scanGrant(tx.QueryRow(ctx, `SELECT `+grantCols+` FROM model_grants WHERE id = $1 AND tenant = $2 AND kind = 'grant' FOR UPDATE`, d.Grant, tenant))
	if err != nil {
		return "", "", err
	}
	if r == nil || r.State != "pending" {
		return "", "not_found", nil
	}
	decided, _ := json.Marshal(map[string]any{"kind": "human", "principal": by.Principal})
	if by.Seat {
		decided, _ = json.Marshal(map[string]any{"kind": "seat", "epoch": by.Epoch})
	}
	tv := cat.Tenants[tenant]
	if d.Decision == "deny" {
		reason := d.Reason
		if reason == "" {
			reason = "grant_not_eligible"
		}
		if _, err := tx.Exec(ctx, `UPDATE model_grants SET state = 'denied', reason = $2, decided_by = $3, decided_at = $4, updated_at = $4 WHERE id = $1`,
			r.ID, reason, decided, now); err != nil {
			return "", "", err
		}
		return "denied", "", wakeGrantTask(ctx, tx, now, r.Task)
	}
	profile := d.Profile
	if profile == "" {
		profile = r.Profile
	}
	ladder := grantLadder(cat, tenant, cat.Roles[r.Role])
	if !slices.Contains(ladder, profile) {
		return "pending", "grant_not_eligible", nil
	}
	if (r.Untrusted || by.Untrusted) && profile != ladder[0] {
		return "pending", "grant_not_eligible", nil
	}
	attempts := r.Attempts
	if d.Attempts > 0 {
		attempts = d.Attempts
	}
	if attempts > grantAttemptMax {
		return "pending", "grant_over_budget", nil
	}
	ttl := grantTTLDefault
	if d.ExpiresIn != "" {
		v, ok := parseDuration(d.ExpiresIn)
		if !ok || v > grantTTLMax {
			return "pending", "grant_over_budget", nil
		}
		ttl = v
	}
	capMicros := r.Estimate
	if profile != r.Profile || attempts != r.Attempts {
		pr, _ := cat.Profile(tenant, profile)
		var body []byte
		if err := tx.QueryRow(ctx, `SELECT body FROM tasks WHERE id = $1`, r.Task).Scan(&body); err != nil {
			return "", "", err
		}
		capMicros = grantEstimate(pr, len(body), attempts)
	}
	if d.CapEUR != nil {
		capMicros = money.FromEUR(*d.CapEUR)
	}
	// The open caps of grants count against the line as their reserve, read under the tenant's
	// budget lock.
	if err := lockTenantBudget(ctx, tx, tenant); err != nil {
		return "", "", err
	}
	open, err := openGrantMicros(ctx, tx, tenant, "grant")
	if err != nil {
		return "", "", err
	}
	if p := c.checkBudget(ctx, tx, now, tv, budgetAsk{subject: "system", extra: capMicros + open, line: "grant"}); p != nil {
		if p.Reason != "budget_exhausted" {
			return "", "", p
		}
		return "pending", "grant_over_budget", nil
	}
	if by.Seat {
		// The seat's daily ceiling counts the caps the seat issued since the start of the UTC
		// calendar day, in every tenant, under one farm-wide lock; it is always on (MGR-18).
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(7300152)`); err != nil {
			return "", "", err
		}
		var today int64
		if err := tx.QueryRow(ctx, `SELECT coalesce(sum(cap_micros), 0) FROM model_grants WHERE kind = 'grant' AND decided_by->>'kind' = 'seat' AND state <> 'denied' AND decided_at >= $1`,
			utcDayStart(now)).Scan(&today); err != nil {
			return "", "", err
		}
		if today+capMicros > grantLimitsOf(cat).seatDayMicros {
			return "pending", "grant_over_budget", nil
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE model_grants SET state = 'approved', profile = $2, attempts = $3, attempts_left = $3, cap_micros = $4, expires_at = $5,
		decided_by = $6, decided_at = $7, updated_at = $7 WHERE id = $1`, r.ID, profile, attempts, capMicros, now.Add(ttl), decided, now); err != nil {
		return "", "", err
	}
	return "approved", "", wakeGrantTask(ctx, tx, now, r.Task)
}

// wakeGrantTask puts a task that waits for its grant back in the queue at once.
func wakeGrantTask(ctx context.Context, tx pgx.Tx, now time.Time, task string) error {
	if _, err := tx.Exec(ctx, `UPDATE tasks SET run_at = $2, reason = NULL, updated_at = $2 WHERE id = $1 AND state = 'submitted' AND reason = 'model_grant_pending'`, task, now); err != nil {
		return err
	}
	return notifyWork(ctx, tx)
}

// seatGrantTool says whether the seat tools of the grant batch go to an attempt: a decision
// attempt of the farm manager seat on manager.grants.v1.
func seatGrantTool(cat *Catalog, g *toolsGrant) bool {
	return g.Purpose == "manage" && g.Type == grantsType && profileOf(cat, g.Role) == "farm-manager"
}

// grantSeatTools are grants_pending and grant_decide (MGR-18).
func grantSeatTools(cat *Catalog, g *toolsGrant) []catalogTool {
	if !seatGrantTool(cat, g) {
		return nil
	}
	decide := map[string]any{
		"type": "object", "additionalProperties": false, "required": []any{"decisions"},
		"properties": map[string]any{"decisions": map[string]any{"type": "array", "minItems": 1, "maxItems": 50, "items": map[string]any{
			"type": "object", "additionalProperties": false, "required": []any{"grant", "decision"},
			"properties": map[string]any{
				"grant":      map[string]any{"type": "string", "maxLength": 64, "pattern": "^mg_[0-9A-HJKMNP-TV-Z]{26}$"},
				"decision":   map[string]any{"enum": []any{"issue", "deny"}},
				"profile":    map[string]any{"type": "string", "maxLength": 64, "pattern": "^[a-z][a-z0-9-]{0,62}$"},
				"attempts":   map[string]any{"type": "integer", "minimum": 1, "maximum": grantAttemptMax},
				"cap_eur":    map[string]any{"type": "number", "minimum": 0, "maximum": 1000},
				"expires_in": map[string]any{"type": "string", "pattern": "^[1-9][0-9]{0,5}(s|m|h|d)$"},
				"reason":     map[string]any{"enum": []any{"grant_not_eligible", "grant_over_budget"}},
			}}}},
	}
	empty := map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{}}
	return []catalogTool{
		{Name: grantsPendingName, Builtin: true, Tool: toolspec.Tool{Description: "The pending grant requests of this tenant, closed fields only."}, Schema: empty},
		{Name: grantDecideName, Builtin: true, Tool: toolspec.Tool{Description: "Decide pending grant requests of this tenant in one batch: issue or deny each."}, Schema: decide},
	}
}

// grantSeatToolCall answers grants_pending and grant_decide for the holder with the current
// epoch; both act only on the tenant of the task (MGR-18).
func (c *Core) grantSeatToolCall(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, g *toolsGrant, name string, offered bool, args map[string]any) (*mcp.CallResult, error) {
	if !offered {
		return toolAnswer(map[string]any{"code": "forbidden"}, true), nil
	}
	acting, err := leaseActing(ctx, tx, g.Task, g.Worker)
	if err != nil {
		return nil, err
	}
	m, err := loadManagerSeat(ctx, tx, false)
	if err != nil {
		return nil, err
	}
	if m == nil || m.State != "held" || m.Holder != acting.Bot || m.Epoch != acting.SeatEpoch {
		return toolAnswer(map[string]any{"code": "fenced"}, true), nil
	}
	if name == grantsPendingName {
		list, err := c.pendingGrants(ctx, tx, now, g.Tenant)
		if err != nil {
			return nil, err
		}
		return toolAnswer(map[string]any{"tenant": g.Tenant, "requests": list}, false), nil
	}
	// grant_decide writes; the restricted mode leaves only the reading (MGR-47).
	var rec []byte
	if err := tx.QueryRow(ctx, `SELECT record FROM tasks WHERE id = $1`, g.Task).Scan(&rec); err != nil {
		return nil, err
	}
	if restricted, err := seatTaskRestricted(ctx, tx, g.Tenant, rec); err != nil {
		return nil, err
	} else if restricted {
		return toolAnswer(map[string]any{"code": restrictedReason}, true), nil
	}
	var tool catalogTool
	for _, t := range grantSeatTools(cat, g) {
		if t.Name == grantDecideName {
			tool = t
		}
	}
	if err := c.rs.validate(tool.Schema, args); err != nil {
		return toolAnswer(map[string]any{"code": "input_schema_invalid"}, true), nil
	}
	var untrusted bool
	if err := tx.QueryRow(ctx, `SELECT coalesce((record->>'untrusted_seen')::boolean, false) FROM tasks WHERE id = $1`, g.Task).Scan(&untrusted); err != nil {
		return nil, err
	}
	results := []any{}
	for _, x := range mlist(args, "decisions") {
		dm := asMap(x)
		d := grantDecision{Grant: mstr(dm, "grant"), Decision: mstr(dm, "decision"), Profile: mstr(dm, "profile"), ExpiresIn: mstr(dm, "expires_in"), Reason: mstr(dm, "reason")}
		if v, ok := mnum(dm, "attempts"); ok {
			d.Attempts = int(v)
		}
		if v, ok := mnum(dm, "cap_eur"); ok {
			d.CapEUR = &v
		}
		state, reason, err := c.decideGrant(ctx, tx, now, cat, g.Tenant, d, grantDecider{Seat: true, Epoch: m.Epoch, Principal: acting.Bot, Untrusted: untrusted})
		if err != nil {
			return nil, err
		}
		out := map[string]any{"grant": d.Grant}
		switch state {
		case "approved":
			out["state"] = "active"
		case "denied":
			out["state"] = "denied"
		default:
			out["state"] = "pending"
		}
		if reason != "" {
			out["reason"] = reason
		}
		results = append(results, out)
		if _, err := c.appendAudit(ctx, tx, now, Audit{
			Tenant: g.Tenant, Kind: "decision", Actor: Actor{Principal: acting.Bot, Via: "seat", Role: g.Role, Task: g.Task},
			Command: "grant_decide", Target: &Target{Kind: "grant", ID: d.Grant}, OK: reason == "", Reason: reason,
			After: map[string]any{"decision": d.Decision, "state": out["state"], "epoch": m.Epoch},
		}); err != nil {
			return nil, err
		}
	}
	return toolAnswer(map[string]any{"results": results}, false), nil
}

// pendingGrants are the closed fields of a tenant's pending requests: no note and no task text
// (MGR-18, grants-pending-response).
func (c *Core) pendingGrants(ctx context.Context, tx pgx.Tx, now time.Time, tenant string) ([]any, error) {
	rows, err := tx.Query(ctx, `SELECT `+grantCols+` FROM model_grants WHERE tenant = $1 AND kind = 'grant' AND state = 'pending' ORDER BY created_at, id LIMIT 100`, tenant)
	if err != nil {
		return nil, err
	}
	var gs []*grantRow
	for rows.Next() {
		r, err := scanGrant(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		gs = append(gs, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := []any{}
	for _, r := range gs {
		var granted, consumed int
		if err := tx.QueryRow(ctx, `SELECT count(*) FILTER (WHERE state IN ('approved', 'active', 'consumed', 'expired', 'revoked') AND decided_at IS NOT NULL),
			count(*) FILTER (WHERE state = 'consumed') FROM model_grants WHERE tenant = $1 AND role = $2 AND kind = 'grant' AND created_at > $3`,
			tenant, r.Role, now.Add(-30*24*time.Hour)).Scan(&granted, &consumed); err != nil {
			return nil, err
		}
		item := map[string]any{"grant": r.ID, "task_type": mstr(r.Request, "task_type"), "role": r.Role, "profile": r.Profile,
			"attempts": r.Attempts, "estimate_eur": money.CentsUp(r.Estimate), "untrusted": r.Untrusted,
			"role_grants_30d": granted, "role_consumed_30d": consumed, "pending_since": fmtTime(r.Created)}
		if v := mstr(r.Request, "reason"); v != "" {
			item["reason"] = v
		}
		out = append(out, item)
	}
	return out, nil
}

// grantLoop is the background loop grant: the deadlines of requests and grants, and every 10
// minutes one manager.grants.v1 batch per tenant with a pending request for the held seat
// (MGR-18).
func (c *Core) grantLoop(ctx context.Context) error {
	cat, err := c.Catalog(ctx)
	if err != nil {
		return err
	}
	limits := grantLimitsOf(cat)
	return c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		// A request with no decision within grant_wait_max ends, and its task goes on with the
		// base model.
		rows, err := tx.Query(ctx, `UPDATE model_grants SET state = 'expired', reason = 'grant_expired_unanswered', updated_at = $1
			WHERE kind = 'grant' AND state = 'pending' AND created_at <= $2 RETURNING task`, now, now.Add(-limits.waitMax))
		if err != nil {
			return err
		}
		var wake []string
		for rows.Next() {
			var t string
			if err := rows.Scan(&t); err != nil {
				rows.Close()
				return err
			}
			wake = append(wake, t)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, t := range wake {
			if err := wakeGrantTask(ctx, tx, now, t); err != nil {
				return err
			}
		}
		// A grant past its expiry, or of a task that ended, ends; a running attempt on it
		// ends without spending and the task goes on with the base model.
		rows, err = tx.Query(ctx, `UPDATE model_grants g SET state = CASE WHEN t.state IN ('completed', 'failed', 'canceled') THEN 'consumed' ELSE 'expired' END, updated_at = $1
			FROM tasks t WHERE t.id = g.task AND g.state IN ('approved', 'active') AND (g.expires_at <= $1 OR t.state IN ('completed', 'failed', 'canceled'))
			RETURNING g.task, g.state`, now)
		if err != nil {
			return err
		}
		var ended []string
		for rows.Next() {
			var t, st string
			if err := rows.Scan(&t, &st); err != nil {
				rows.Close()
				return err
			}
			if st == "expired" {
				ended = append(ended, t)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, t := range ended {
			if err := c.endGrantAttempt(ctx, tx, now, t, "expired"); err != nil {
				return err
			}
		}
		if err := c.expireTakeovers(ctx, tx, now, cat); err != nil {
			return err
		}
		return c.enqueueGrantBatches(ctx, tx, now, cat)
	})
}

// endGrantAttempt ends a running attempt of a task that runs on the granted slot, without
// spending it.
func (c *Core) endGrantAttempt(ctx context.Context, tx pgx.Tx, now time.Time, task, reason string) error {
	var granted bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM handles h JOIN leases l ON l.work_id = h.work_id AND l.lease_no = h.lease_no
		WHERE h.work_id = $1 AND h.kind = 'model' AND h.slot = $2 AND l.state = 'active')`, task, grantedSlot).Scan(&granted); err != nil || !granted {
		return err
	}
	return c.endAttemptUnspent(ctx, tx, now, task, 0, reason, "", now)
}

// enqueueGrantBatches puts one manager.grants.v1 task per tenant with a pending request and
// window, key system:grants:<tenant>:<window> in class 3; a tenant without the manager or a
// seat that is not held gets none, and its requests wait for a person (MGR-18).
func (c *Core) enqueueGrantBatches(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog) error {
	role := managerRole(cat)
	if role == "" || !slices.Contains(cat.Roles[role].Claims, grantsType) {
		return nil
	}
	if _, ok := cat.TaskTypes[grantsType]; !ok {
		return nil
	}
	m, err := loadManagerSeat(ctx, tx, false)
	if err != nil || m == nil || m.State != "held" {
		return err
	}
	rows, err := tx.Query(ctx, `SELECT DISTINCT tenant FROM model_grants WHERE kind = 'grant' AND state = 'pending' ORDER BY tenant`)
	if err != nil {
		return err
	}
	var tenants []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			rows.Close()
			return err
		}
		tenants = append(tenants, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	window := fmtTime(now.UTC().Truncate(grantWindow))
	for _, t := range tenants {
		if !managerEnabled(cat, t) {
			continue
		}
		body := map[string]any{"type": grantsType, "goal": "Decide the pending grant requests.", "params": map[string]any{"input": map[string]any{"view": "grants/v1", "window": window}}}
		asMap(asMap(body["params"])["input"])["tenant"] = t
		res, err := c.enqueue(ctx, tx, now, cat, enqueueInput{Tenant: t, Type: grantsType, Body: body,
			IdempotencyKey: "system:grants:" + t + ":" + window, Initiator: "system", EnqueuedBy: "system:grant", Source: seatSource, SourceID: t, Priority: 3})
		if err != nil {
			return err
		}
		if res.Rejected != nil {
			c.log.Warn("the grant batch of a tenant was refused", "tenant", t, "reason", res.Rejected.Reason)
			continue
		}
		if res.Existing {
			continue
		}
		extra, _ := json.Marshal(map[string]any{"seat": managerSeatID, "executor_role": role})
		if _, err := tx.Exec(ctx, `UPDATE tasks SET record = record || $2::jsonb WHERE id = $1`, res.TaskID, extra); err != nil {
			return err
		}
	}
	return nil
}

// ModelGrantRequest is the P2 model_grant.issue, model_grant.revoke and model_grant.list
// request.
type ModelGrantRequest struct {
	Tenant    string   `json:"tenant"`
	Grant     string   `json:"grant"`
	Decision  string   `json:"decision"`
	Profile   string   `json:"profile"`
	Attempts  int      `json:"attempts"`
	CapEUR    *float64 `json:"cap_eur"`
	ExpiresIn string   `json:"expires_in"`
	State     string   `json:"state"`
}

// grantManager checks that the caller is an active person of the tenant with model_grant.manage.
func (c *Core) grantManager(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, op Operator, tenant string) error {
	if tenant == "" || (op.Tenant != tenant && op.Tenant != farmTenant) {
		return refuse("not_found", "no such tenant for this credential")
	}
	tv := cat.Tenants[tenant]
	if tv == nil {
		return refuse("not_found", "no such tenant")
	}
	p, err := loadPrincipal(ctx, tx, now, tenant, op.Principal)
	if errors.Is(err, pgx.ErrNoRows) {
		return refuse("forbidden", "only a person of the tenant with model_grant.manage")
	}
	if err != nil {
		return err
	}
	if p.State != "active" || p.Kind != "human" || !hasCap(tv, groupClosure(tv, p.Groups), "model_grant.manage") {
		return refuse("forbidden", "only a person of the tenant with model_grant.manage")
	}
	return nil
}

// ModelGrantIssue is P2 model_grant.issue: a person decides a pending request within the same
// ceilings as the seat, without the seat's daily ceiling (MGR-18).
func (c *Core) ModelGrantIssue(ctx context.Context, op Operator, req ModelGrantRequest) (map[string]any, error) {
	cat, err := c.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	err = c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		if err := c.grantManager(ctx, tx, now, cat, op, req.Tenant); err != nil {
			return err
		}
		if req.Decision != "issue" && req.Decision != "deny" {
			return refuse("input_schema_invalid", "the decision is issue or deny")
		}
		if req.Attempts > grantAttemptMax {
			return refuse("grant_over_budget", "at most 2 attempts")
		}
		state, reason, err := c.decideGrant(ctx, tx, now, cat, req.Tenant, grantDecision{Grant: req.Grant, Decision: req.Decision, Profile: req.Profile,
			Attempts: req.Attempts, CapEUR: req.CapEUR, ExpiresIn: req.ExpiresIn}, grantDecider{Principal: op.Principal})
		if err != nil {
			return err
		}
		if reason == "not_found" {
			return refuse("not_found", "no pending grant request of this tenant")
		}
		if reason != "" {
			return refuse(reason, "the decision passes a code ceiling")
		}
		r, err := scanGrant(tx.QueryRow(ctx, `SELECT `+grantCols+` FROM model_grants WHERE id = $1`, req.Grant))
		if err != nil {
			return err
		}
		out = map[string]any{"grant": r.view()}
		_, err = c.appendAudit(ctx, tx, now, Audit{
			Tenant: req.Tenant, Kind: "decision", Actor: Actor{Principal: op.Principal, Via: "p2"}, Command: "model_grant.issue",
			Target: &Target{Kind: "grant", ID: req.Grant}, OK: true, After: map[string]any{"decision": req.Decision, "state": state},
		})
		return err
	})
	return out, err
}

// ModelGrantRevoke is P2 model_grant.revoke: an approved or active grant ends; a running attempt
// on it is canceled with grant_revoked without spending, and the task goes on with the base
// model (MGR-18).
func (c *Core) ModelGrantRevoke(ctx context.Context, op Operator, req ModelGrantRequest) (map[string]any, error) {
	cat, err := c.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	err = c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		if err := c.grantManager(ctx, tx, now, cat, op, req.Tenant); err != nil {
			return err
		}
		r, err := scanGrant(tx.QueryRow(ctx, `SELECT `+grantCols+` FROM model_grants WHERE id = $1 AND tenant = $2 FOR UPDATE`, req.Grant, req.Tenant))
		if err != nil {
			return err
		}
		if r == nil || (r.State != "approved" && r.State != "active") {
			return refuse("not_found", "no live grant of this tenant")
		}
		if _, err := tx.Exec(ctx, `UPDATE model_grants SET state = 'revoked', reason = 'grant_revoked', updated_at = $2 WHERE id = $1`, r.ID, now); err != nil {
			return err
		}
		if err := c.endGrantAttempt(ctx, tx, now, r.Task, "grant_revoked"); err != nil {
			return err
		}
		r.State, r.Reason = "revoked", "grant_revoked"
		out = map[string]any{"grant": r.view()}
		_, err = c.appendAudit(ctx, tx, now, Audit{
			Tenant: req.Tenant, Kind: "decision", Actor: Actor{Principal: op.Principal, Via: "p2"}, Command: "model_grant.revoke",
			Target: &Target{Kind: "grant", ID: r.ID}, OK: true, After: map[string]any{"state": "revoked"},
		})
		return err
	})
	return out, err
}

// ModelGrantList is P2 model_grant.list: the grant records of the tenant, newest first, without
// the request note.
func (c *Core) ModelGrantList(ctx context.Context, op Operator, req ModelGrantRequest) (map[string]any, error) {
	cat, err := c.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"grants": []any{}}
	if req.Tenant == "" {
		req.Tenant = op.Tenant
	}
	err = c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		if err := c.grantManager(ctx, tx, now, cat, op, req.Tenant); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT `+grantCols+` FROM model_grants WHERE tenant = $1 AND ($2 = '' OR state = $2) ORDER BY created_at DESC, id LIMIT 200`, req.Tenant, req.State)
		if err != nil {
			return err
		}
		defer rows.Close()
		list := []any{}
		for rows.Next() {
			r, err := scanGrant(rows)
			if err != nil {
				return err
			}
			list = append(list, r.view())
		}
		out["grants"] = list
		return rows.Err()
	})
	return out, err
}

// utcDayStart is the start of the UTC calendar day of t.
func utcDayStart(t time.Time) time.Time {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// lockTenantBudget takes the tenant's budget lock that checkBudget takes, so a read of the open
// grant caps and the budget check see the same spend.
func lockTenantBudget(ctx context.Context, tx pgx.Tx, tenant string) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(7300102, hashtext($1))`, tenant)
	return err
}

// openGrantMicros is the rest of the caps of a tenant's approved and active grants on a line:
// cap_micros less what the spend rows of each grant hold. A running call's reserve is a spend
// row of its grant, so it is counted once, in the line's spend, and not again here (MGR-18,
// MGR-21).
func openGrantMicros(ctx context.Context, tx pgx.Tx, tenant, line string) (int64, error) {
	var open int64
	err := tx.QueryRow(ctx, `SELECT coalesce(sum(greatest(g.cap_micros - coalesce((SELECT sum(s.eur_micros) FROM spend s WHERE s.grant_id = g.id), 0), 0)), 0)
		FROM model_grants g WHERE g.tenant = $1 AND g.line = $2 AND g.state IN ('approved', 'active')`, tenant, line).Scan(&open)
	return open, err
}
