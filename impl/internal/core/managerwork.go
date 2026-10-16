// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/ids"
	"github.com/HunterEwok/botaru/impl/internal/money"
	"github.com/HunterEwok/botaru/impl/internal/store"
)

// The work of the farm manager seat in a tenant besides incidents (MGR-4, MGR-41): directives
// of people with manager.direct, the daily review manager.review.v1 with its numbers to people,
// and P2 manager.cost.

const (
	directiveType = "manager.directive.v1"
	reviewType    = "manager.review.v1"
	// defaultReviewMaxEUR is control_limits.daily_review_max_eur when the farm charter sets
	// none (MGR-41).
	defaultReviewMaxEUR = 0.40
	// maxDirectiveText is the longest text a person types into a directive (MGR-4).
	maxDirectiveText = 1000
)

// directiveScopes are the scope kinds of a directive; directiveOps the operations it may name
// (p2-manager-direct-request, control-ops.yaml).
var (
	directiveScopes = []string{"role", "worker", "resource", "incident"}
	directiveOps    = []string{"hold", "quarantine", "drain", "preempt", "cancel", "pin_task", "request_action", "release", "notify"}
	reDirectiveID   = regexp.MustCompile(`^[a-z0-9:_.-]{1,64}$`)
)

// ManagerDirectRequest is p2-manager-direct-request.
type ManagerDirectRequest struct {
	Scope ControlTarget `json:"scope"`
	Op    string        `json:"op,omitempty"`
	Text  string        `json:"text,omitempty"`
}

// directive is what a directive task keeps in its record for the code: the closed fields the
// code parsed and the person who gave it. Its operations are the seat's rights within the
// scope ∩ the person's rights (MGR-4).
type directive struct {
	Scope ControlTarget `json:"scope"`
	Op    string        `json:"op,omitempty"`
	By    string        `json:"by"`
	Via   string        `json:"via"`
}

func (r ManagerDirectRequest) check() *Problem {
	if !slices.Contains(directiveScopes, r.Scope.Kind) || !reDirectiveID.MatchString(r.Scope.ID) {
		return refuse("input_schema_invalid", "scope")
	}
	if r.Op != "" && !slices.Contains(directiveOps, r.Op) {
		return refuse("input_schema_invalid", "op")
	}
	// A directive names only what this version can do: an operation it serves, and cancel or
	// preempt, which take a task, only within an incident whose involved tasks they reach.
	if r.Op != "" && !slices.Contains(controlOpsServed, r.Op) {
		return refuse("input_schema_invalid", "operation %s is not served by this version", r.Op)
	}
	if (r.Op == "cancel" || r.Op == "preempt") && r.Scope.Kind != "incident" {
		return refuse("input_schema_invalid", "%s takes a task: give an incident scope", r.Op)
	}
	if len([]rune(r.Text)) > maxDirectiveText {
		return refuse("input_schema_invalid", "text")
	}
	return nil
}

// ManagerDirect is P2 manager.direct: the operator of a tenant directs the farm manager there.
// The credential stands for a person with manager.direct in the tenant; a tenant with the
// manager disabled, and tenant farm, which has no seat tasks, refuse with manager_disabled
// (MGR-4, MGR-42).
func (c *Core) ManagerDirect(ctx context.Context, op Operator, req ManagerDirectRequest) (*Done, error) {
	if p := req.check(); p != nil {
		return nil, p
	}
	cat, err := c.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	var out Done
	err = c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		id, p, err := c.enqueueDirective(ctx, tx, now, cat, op.Tenant, directive{Scope: req.Scope, Op: req.Op, By: principalOr(op.Principal), Via: "farmctl"}, req.Text)
		if err != nil {
			return err
		}
		if p != nil {
			return p
		}
		out = Done{OK: true, ID: id, State: "submitted"}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// enqueueDirective puts a directive task in the tenant: the decision role gets the closed fields
// in the input block and the person's own text as the goal; nothing else of the request reaches
// it (MGR-4). A refusal comes back as a Problem.
func (c *Core) enqueueDirective(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, tenant string, d directive, text string) (string, *Problem, error) {
	if !managerEnabled(cat, tenant) {
		return "", refuse("manager_disabled", "the farm manager is disabled in the tenant"), nil
	}
	role := managerRole(cat)
	if _, ok := cat.TaskTypes[directiveType]; !ok || role == "" || !slices.Contains(cat.Roles[role].Claims, directiveType) {
		return "", refuse("type_not_allowed", "the catalog has no directive type of the farm manager"), nil
	}
	goal := strings.TrimSpace(text)
	if goal == "" {
		goal = "Directive."
	}
	input := map[string]any{"directive": map[string]any{"scope": map[string]any{"kind": d.Scope.Kind, "id": d.Scope.ID}}}
	if d.Op != "" {
		input["directive"].(map[string]any)["op"] = d.Op
	}
	body := map[string]any{"type": directiveType, "goal": goal, "params": map[string]any{"input": input}}
	res, err := c.enqueue(ctx, tx, now, cat, enqueueInput{Tenant: tenant, Type: directiveType, Body: body,
		IdempotencyKey: "system:directive:" + newIdemSuffix(), Initiator: "system", EnqueuedBy: "system:directive",
		Source: seatSource, SourceID: d.By, Priority: 2})
	if err != nil {
		return "", nil, err
	}
	if res.Rejected != nil {
		return "", res.Rejected, nil
	}
	extra, err := json.Marshal(map[string]any{"seat": managerSeatID, "executor_role": role, "directive": d})
	if err != nil {
		return "", nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE tasks SET record = record || $2::jsonb WHERE id = $1`, res.TaskID, extra); err != nil {
		return "", nil, err
	}
	_, err = c.appendAudit(ctx, tx, now, Audit{
		Tenant: tenant, Kind: "command", Actor: Actor{Principal: d.By, Via: d.Via}, Command: "manager.direct",
		Target: &Target{Kind: "task", ID: res.TaskID}, OK: true,
		After: map[string]any{"scope": map[string]any{"kind": d.Scope.Kind, "id": d.Scope.ID}, "op": nullable(d.Op), "text_len": len([]rune(text))},
	})
	return res.TaskID, nil, err
}

// newIdemSuffix is a fresh idempotency key part: every directive is a new task.
func newIdemSuffix() string { return strings.ToLower(ids.Secret(12)) }

// taskDirective is the directive of a task record, or nil.
func taskDirective(rec []byte) *directive {
	var r struct {
		Directive *directive `json:"directive"`
	}
	if json.Unmarshal(rec, &r) != nil {
		return nil
	}
	return r.Directive
}

// managerCommand is /manager [OP] KIND ID [TEXT] in a private chat from a person with
// manager.direct: the code parses the closed fields, the rest is the person's own text (MGR-4).
// A forwarded or quoted message never gets here.
func (c *Core) managerCommand(ctx context.Context, tx pgx.Tx, now time.Time, pc personCommand, rest string) (*ActivityResult, error) {
	// A directive is the person's own typed text: no attachments (MGR-4).
	if pc.attachments > 0 {
		return pc.reply(c, "cmd.manager.usage", nil), nil
	}
	f := strings.Fields(rest)
	var req ManagerDirectRequest
	if len(f) > 0 && slices.Contains(directiveOps, f[0]) {
		req.Op, f = f[0], f[1:]
	}
	if len(f) < 2 {
		return pc.reply(c, "cmd.manager.usage", nil), nil
	}
	req.Scope = ControlTarget{Kind: f[0], ID: strings.ToLower(f[1])}
	req.Text = strings.Join(f[2:], " ")
	if p := req.check(); p != nil {
		return pc.reply(c, "cmd.manager.usage", nil), nil
	}
	if !hasCap(pc.tv, pc.groups, "manager.direct") {
		return pc.reply(c, "task.rejected.no_group_cap", nil), nil
	}
	id, p, err := c.enqueueDirective(ctx, tx, now, pc.cat, pc.tv.Name, directive{Scope: req.Scope, Op: req.Op, By: pc.person, Via: "channel"}, req.Text)
	if err != nil {
		return nil, err
	}
	if p != nil {
		if p.Reason == "manager_disabled" {
			return pc.reply(c, "cmd.manager.disabled", nil), nil
		}
		return &ActivityResult{Result: "reply", Text: c.rejectText(pc.locale, p.Reason)}, nil
	}
	return pc.reply(c, "cmd.manager.done", map[string]any{"task": id}), nil
}

// forwardedDirective says whether someone else's text starts with the /manager command.
func forwardedDirective(text string) bool {
	word, _, _ := strings.Cut(strings.TrimSpace(text), " ")
	if i := strings.IndexByte(word, '@'); i >= 0 {
		word = word[:i]
	}
	return word == "/manager"
}

// directiveAllows checks a seat operation of a directive task: the target is the directive's
// scope and the operation the one it names, if any; a person of a channel directive must still
// hold control.apply, and the operator credential of P2 holds it in its tenant (MGR-4).
func (c *Core) directiveAllows(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, tenant string, d *directive, req ControlRequest) (*Problem, error) {
	if d.Op != "" && d.Op != req.Op {
		return refuse("forbidden", "the directive names another operation"), nil
	}
	if p, err := directiveScopeHolds(ctx, tx, tenant, d, req); err != nil || p != nil {
		return p, err
	}
	if d.Via != "channel" {
		return nil, nil
	}
	tv := cat.Tenants[tenant]
	p, err := loadPrincipal(ctx, tx, now, tenant, d.By)
	// The person's own right to the operation: control.apply:<op>, and hold.release for a
	// release, as a person would need to do it themselves (PRM-13).
	want := "control.apply:" + req.Op
	if req.Op == "release" {
		want = "hold.release"
	}
	if errors.Is(err, pgx.ErrNoRows) || err == nil && (p.State != "active" || tv == nil || !hasCap(tv, groupClosure(tv, p.Groups), want)) {
		return refuse("forbidden", "the person of the directive has no %s", want), nil
	}
	return nil, err
}

// directiveScopeHolds checks that the target falls within the directive's scope. An incident
// scope is the incident as the basis, whose code list then bounds the target; a release takes
// a hold whose operation was on the scope's target or within the scope's incident.
func directiveScopeHolds(ctx context.Context, tx pgx.Tx, tenant string, d *directive, req ControlRequest) (*Problem, error) {
	kind, id, incident := req.Target.Kind, req.Target.ID, req.Basis.Incident
	if req.Op == "release" {
		err := tx.QueryRow(ctx, `SELECT target_kind, target_id, coalesce(incident, '') FROM control_ops
			WHERE tenant = $1 AND hold = $2 AND actor_kind = 'seat' AND op <> 'release' ORDER BY created_at LIMIT 1`, tenant, req.Target.ID).Scan(&kind, &id, &incident)
		if errors.Is(err, pgx.ErrNoRows) {
			return refuse("target_not_implicated", "the hold is not within the directive's scope"), nil
		}
		if err != nil {
			return nil, err
		}
	}
	if d.Scope.Kind == "incident" {
		if !strings.EqualFold(incident, d.Scope.ID) || !strings.EqualFold(req.Basis.Incident, d.Scope.ID) {
			return refuse("target_not_implicated", "the operation is not within the directive's incident"), nil
		}
		return nil, nil
	}
	if kind != d.Scope.Kind || !strings.EqualFold(id, d.Scope.ID) {
		return refuse("target_not_implicated", "the target is not the directive's scope"), nil
	}
	return nil, nil
}

// reviewMaxEUR is control_limits.daily_review_max_eur of the farm charter (MGR-41).
func reviewMaxEUR(cat *Catalog) float64 { return charterReviewMaxEUR(managerCharter(cat)) }

// charterReviewMaxEUR is daily_review_max_eur of a farm_manager block of the charter.
func charterReviewMaxEUR(fm map[string]any) float64 {
	if v, ok := mnum(mmap(fm, "control_limits"), "daily_review_max_eur"); ok && v >= 0 {
		return v
	}
	return defaultReviewMaxEUR
}

// tenantDay is the start of the tenant's calendar day of now (TIM-2).
func tenantDay(tv *TenantView, now time.Time) time.Time {
	loc := time.UTC
	if l, err := time.LoadLocation(tv.Spec.Timezone); err == nil && tv.Spec.Timezone != "" {
		loc = l
	}
	l := now.In(loc)
	return time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, loc)
}

// managerDaily runs the farm manager's day in every tenant with the manager enabled, once per
// local day: the review manager.review.v1 when the catalog has it, with a tree budget of
// daily_review_max_eur from the manager line, and the numbers of the last day to the tenant's
// admins; tenant farm's admins get the numbers of the whole farm (MGR-41). A day without any
// manager work sends no numbers.
func (c *Core) managerDaily(ctx context.Context, cat *Catalog) error {
	if managerRole(cat) == "" {
		return nil
	}
	var enabled []string
	for _, t := range slices.Sorted(mapKeys(cat.Tenants)) {
		tv := cat.Tenants[t]
		if !managerEnabled(cat, t) || tv.Spec.Kind == "" {
			continue
		}
		enabled = append(enabled, t)
		err := c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
			day := tenantDay(tv, now)
			tag, err := tx.Exec(ctx, `INSERT INTO manager_days (tenant, day, created_at) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`, t, day.Format("2006-01-02"), now)
			if err != nil || tag.RowsAffected() == 0 {
				return err
			}
			// The numbers of the tenant's previous calendar day, so days never overlap.
			n, err := dayNumbers(ctx, tx, t, day.AddDate(0, 0, -1), day)
			if err != nil {
				return err
			}
			review, err := c.enqueueReview(ctx, tx, now, cat, t, day, n)
			if err != nil {
				return err
			}
			if review != "" {
				if _, err := tx.Exec(ctx, `UPDATE manager_days SET review = $3 WHERE tenant = $1 AND day = $2`, t, day.Format("2006-01-02"), review); err != nil {
					return err
				}
			}
			// From v0.3 the summary is mandatory, with the takeovers and notices (MGR-50).
			key := "notice.manager_daily"
			if claimsV03() {
				key = "notice.manager_daily_v03"
			} else if n.empty() {
				return nil
			}
			return c.noticeAdmins(ctx, tx, now, cat, []string{t}, "manager_daily", key, n.params(true), "admin:people")
		})
		if err != nil {
			return err
		}
	}
	// The farm's numbers have their own day mark and are counted in their own transaction, so a
	// failure in any tenant leaves them for the next run whole.
	if err := c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		day := now.UTC()
		if tv := cat.Tenants[farmTenant]; tv != nil {
			day = tenantDay(tv, now)
		}
		tag, err := tx.Exec(ctx, `INSERT INTO manager_days (tenant, day, created_at) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`, farmTenant, day.Format("2006-01-02"), now)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		var farm managerNumbers
		for _, t := range enabled {
			n, err := dayNumbers(ctx, tx, t, day.AddDate(0, 0, -1), day)
			if err != nil {
				return err
			}
			farm.add(n)
		}
		key, params := "notice.manager_daily_farm", farm.params(false)
		if claimsV03() {
			// Problems across tenants are found by code and shown only here (MGR-51).
			key = "notice.manager_daily_farm_v03"
			if params["recurring"], err = recurringClasses(ctx, tx, day.AddDate(0, 0, -1), day); err != nil {
				return err
			}
		} else if farm.empty() {
			return nil
		}
		return c.noticeAdmins(ctx, tx, now, cat, []string{farmTenant}, "manager_daily", key, params, "admin:governance")
	}); err != nil {
		return err
	}
	return c.holderReportWeek(ctx, cat)
}

// managerNumbers are the numbers of a day of the manager in a tenant, codes and counts only.
type managerNumbers struct {
	Incidents, Ops, Reverts, Reads int
	// Takeovers and Notices count from v0.3 (MGR-50).
	Takeovers, Notices int
	SpentMicros        int64
}

func (n *managerNumbers) add(o managerNumbers) {
	n.Incidents += o.Incidents
	n.Ops += o.Ops
	n.Reverts += o.Reverts
	n.Reads += o.Reads
	n.Takeovers += o.Takeovers
	n.Notices += o.Notices
	n.SpentMicros += o.SpentMicros
}

func (n managerNumbers) empty() bool { return n == managerNumbers{} }

// params are the parameters of the code text; the usage of the manager line goes only to the
// tenant's admins, who are its recipients (MGR-41).
func (n managerNumbers) params(spend bool) map[string]any {
	p := map[string]any{"incidents": n.Incidents, "ops": n.Ops, "reverts": n.Reverts, "reads": n.Reads}
	if claimsV03() {
		p["takeovers"], p["notices"] = n.Takeovers, n.Notices
	}
	if spend {
		p["spent_cents"] = money.CentsUp(n.SpentMicros)
	}
	return p
}

// dayNumbers counts the manager's work in a tenant in the interval [since, until).
func dayNumbers(ctx context.Context, tx pgx.Tx, tenant string, since, until time.Time) (managerNumbers, error) {
	var n managerNumbers
	err := tx.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM tasks WHERE tenant = $1 AND created_at >= $2 AND created_at < $4 AND record->>'seat' = $3 AND record ? 'incident'),
		(SELECT count(*) FROM control_ops WHERE tenant = $1 AND created_at >= $2 AND created_at < $4 AND (actor_kind = 'seat' OR approved_by IS NOT NULL)),
		(SELECT count(*) FROM audit WHERE tenant = $1 AND at >= $2 AND at < $4 AND record->>'command' = 'control.revert'),
		(SELECT count(*) FROM manager_reads WHERE tenant = $1 AND at >= $2 AND at < $4),
		(SELECT coalesce(sum(eur_micros + coalesce(correction_eur_micros, 0)), 0) FROM spend WHERE tenant = $1 AND time >= $2 AND time < $4 AND line = 'manager' AND unit <> 'task'),
		(SELECT count(*) FROM takeovers WHERE tenant = $1 AND created_at >= $2 AND created_at < $4 AND state <> 'staged'),
		(SELECT count(*) FROM manager_feedback WHERE tenant = $1 AND created_at >= $2 AND created_at < $4 AND kind = $5)`,
		tenant, since, managerSeatID, until, attentionNotice).Scan(&n.Incidents, &n.Ops, &n.Reverts, &n.Reads, &n.SpentMicros, &n.Takeovers, &n.Notices)
	return n, err
}

// enqueueReview enqueues the day's review in a tenant; a catalog without the type, or a seat
// role that does not take it, gets none. The review's tree budget is daily_review_max_eur, so it
// spends no more than that of the tenant's manager line (MGR-41).
func (c *Core) enqueueReview(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, tenant string, day time.Time, n managerNumbers) (string, error) {
	role := managerRole(cat)
	if _, ok := cat.TaskTypes[reviewType]; !ok || !slices.Contains(cat.Roles[role].Claims, reviewType) {
		return "", nil
	}
	// A zero ceiling means no review: a tree budget of zero would mean no ceiling at all.
	if money.FromEUR(reviewMaxEUR(cat)) <= 0 {
		return "", nil
	}
	input := map[string]any{"day": day.Format("2006-01-02"), "numbers": n.params(false)}
	// From v0.3 the review reads the two farm guards of the tenant's last day (PRP-26).
	if guards, err := c.farmGuardDay(ctx, tx, cat, tenant, day.AddDate(0, 0, -1), day); err != nil {
		return "", err
	} else if guards != nil {
		input["farm_guards"] = guards
	}
	body := map[string]any{"type": reviewType, "goal": "Review the day of the farm manager in the tenant.", "params": map[string]any{"input": input}}
	res, err := c.enqueue(ctx, tx, now, cat, enqueueInput{Tenant: tenant, Type: reviewType, Body: body,
		IdempotencyKey: "system:daily:" + tenant + ":" + day.Format("2006-01-02"), Initiator: "system", EnqueuedBy: "system:manager_review",
		Source: seatSource, SourceID: day.Format("2006-01-02"), Priority: 5})
	if err != nil {
		return "", err
	}
	if res.Rejected != nil {
		c.log.Warn("the daily review of the farm manager was refused", "tenant", tenant, "reason", res.Rejected.Reason)
		return "", nil
	}
	extra, err := json.Marshal(map[string]any{"seat": managerSeatID, "executor_role": role, "review_day": day.Format("2006-01-02"),
		"tree_budget_micros": money.FromEUR(reviewMaxEUR(cat))})
	if err != nil {
		return "", err
	}
	_, err = tx.Exec(ctx, `UPDATE tasks SET record = record || $2::jsonb WHERE id = $1`, res.TaskID, extra)
	return res.TaskID, err
}

// ManagerCostRequest is p2-tenant-request with the period of P2 manager.cost.
type ManagerCostRequest struct {
	Tenant string `json:"tenant"`
	From   string `json:"from,omitempty"`
	To     string `json:"to,omitempty"`
}

// ManagerCost is P2 manager.cost: the usage of the tenant's manager line over a period, the
// current calendar month of the tenant by default, with the daily review as a separate line
// (MGR-28, MGR-41).
func (c *Core) ManagerCost(ctx context.Context, op Operator, req ManagerCostRequest) (map[string]any, error) {
	tenant, err := viewTenant(op, req.Tenant)
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
	day := tenantDay(tv, now)
	from, to := time.Date(day.Year(), day.Month(), 1, 0, 0, 0, 0, day.Location()), now
	if req.From != "" {
		if from, err = time.Parse(time.RFC3339Nano, req.From); err != nil {
			return nil, refuse("input_schema_invalid", "from")
		}
	}
	if req.To != "" {
		if to, err = time.Parse(time.RFC3339Nano, req.To); err != nil {
			return nil, refuse("input_schema_invalid", "to")
		}
	}
	var spent, review int64
	if err := c.st.Pool.QueryRow(ctx, `SELECT coalesce(sum(s.eur_micros + coalesce(s.correction_eur_micros, 0)), 0),
		coalesce(sum(s.eur_micros + coalesce(s.correction_eur_micros, 0)) FILTER (WHERE t.type = $4), 0)
		FROM spend s LEFT JOIN tasks t ON t.id = s.task WHERE s.tenant = $1 AND s.line = 'manager' AND s.unit <> 'task' AND s.time >= $2 AND s.time <= $3`,
		tenant, from, to, reviewType).Scan(&spent, &review); err != nil {
		return nil, err
	}
	out := map[string]any{"from": fmtTime(from), "to": fmtTime(to), "spent": money.CentsUp(spent),
		"lines": map[string]any{"review": money.CentsUp(review), "other": money.CentsUp(spent) - money.CentsUp(review)}}
	out["tenant"] = tenant
	return out, nil
}
