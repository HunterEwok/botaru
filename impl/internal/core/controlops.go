// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/ids"
	"github.com/HunterEwok/botaru/impl/internal/mcp"
	"github.com/HunterEwok/botaru/impl/internal/toolspec"
)

// Control operations (MGR-9, MGR-10, MGR-11, MGR-13, MGR-14, MGR-22, MGR-32, MGR-37). The seat
// holder changes the world only through the operations of control-ops.yaml, by the built-in tool
// control_ops of a decision attempt; people call the same code path through P2 control.apply.
// A seat operation needs the incident it acts on, a target from the involved parties code built
// for it, and passes the exclusions, the protected set, the share of a role under holds, the
// hourly limit and the safeguard. Every applied operation writes a control_ops row, the event
// control.applied, an audit record and an entry in the incident record, and counts as handling
// the incident.

const (
	controlToolName = "control_ops"
	// defaultOpsPerHour, defaultHoldTTLMax, defaultShareHeld and defaultDenials are the seat
	// limits when the farm charter sets none (MGR-14, MGR-37).
	defaultOpsPerHour = 20
	defaultHoldTTLMax = 2 * time.Hour
	defaultShareHeld  = 0.25
	defaultDenials    = 3
	// maxPreempts is the number of preemptions a task takes before the next one cancels it.
	maxPreempts = 2
)

// controlOpsServed are the operations of control-ops.yaml this version applies; pin_task,
// request_action and notify are not served yet.
var controlOpsServed = []string{"hold", "quarantine", "drain", "preempt", "cancel", "release"}

// excludedProfiles are the role profiles whose workers and roles no seat operation targets
// (MGR-9); excludedPurposes the task purposes.
var (
	excludedProfiles = []string{"reporter", "reviewer", "translator", "farm-manager", "farm-manager-reader"}
	excludedPurposes = []string{"governance", "review", "inspect", "manage"}
)

// ControlTarget is the target of a control operation.
type ControlTarget struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// ControlRequest is p2-control-apply-request and the arguments of the control_ops tool.
type ControlRequest struct {
	Op     string        `json:"op"`
	Target ControlTarget `json:"target"`
	Basis  struct {
		Incident string `json:"incident,omitempty"`
	} `json:"basis"`
	Effects  []string `json:"effects,omitempty"`
	TTL      string   `json:"ttl,omitempty"`
	Action   string   `json:"action,omitempty"`
	TaskType string   `json:"tasktype,omitempty"`
	Template string   `json:"template,omitempty"`
	// Finding names the finding a seat operation's target came from (MGR-26).
	Finding string `json:"finding,omitempty"`
}

// controlActor is who applies an operation: the seat holder with the seat epoch, or a person
// with the operator credential of the tenant.
type controlActor struct {
	Seat      bool
	Principal string // the holder's identity, or the person
	Epoch     int
	Untrusted bool // the decision attempt read an untrusted source (MGR-22)
	Operator  Operator
	// Directive is the directive of a seat attempt's task, whose scope bounds its operations
	// (MGR-4).
	Directive *directive
	// Task is the seat attempt's task, for a control card.
	Task string
	// Origin is the target origin of a person's operation approved on a control card,
	// finding:<id>, and ApprovedBy that person (MGR-26).
	Origin, ApprovedBy string
}

// controlLimits are the seat limits of the farm charter (MGR-14, MGR-37).
type controlLimits struct {
	OpsPerHour int
	HoldTTLMax time.Duration
	ShareHeld  float64
	Denials    int
}

func seatLimits(cat *Catalog) controlLimits {
	ch := managerCharter(cat)
	cl := mmap(ch, "control_limits")
	l := controlLimits{OpsPerHour: defaultOpsPerHour, HoldTTLMax: defaultHoldTTLMax, ShareHeld: defaultShareHeld, Denials: defaultDenials}
	if v, ok := mnum(cl, "ops_per_hour"); ok {
		l.OpsPerHour = int(v)
	}
	if d := specDuration(cl, "hold_ttl_max"); d > 0 && d < l.HoldTTLMax {
		l.HoldTTLMax = d
	}
	if v, ok := mnum(cl, "max_share_held"); ok {
		l.ShareHeld = v
	}
	if v, ok := mnum(mmap(ch, "breaker"), "consecutive_denials"); ok && v >= 1 {
		l.Denials = int(v)
	}
	return l
}

// controlEffects are the hold effects of an operation: the request's own for hold, the fixed
// set of quarantine and drain (control-ops.yaml).
func controlEffects(req ControlRequest) []string {
	switch req.Op {
	case "quarantine":
		return []string{"cancel_running", "no_action", "no_claim", "no_tool"}
	case "drain":
		return []string{"no_claim"}
	}
	return req.Effects
}

// controlTargetTenant derives the tenant of an object target: a worker, a bot, a task or a hold
// carries its own; a name (role, type, resource, bot group) is of the caller's tenant (MGR-3).
func controlTargetTenant(ctx context.Context, tx pgx.Tx, t ControlTarget, caller string) (string, error) {
	var q string
	switch t.Kind {
	case "worker":
		q = `SELECT tenant FROM workers WHERE id = $1`
	case "bot":
		q = `SELECT tenant FROM bots WHERE id = $1`
	case "task":
		q = `SELECT tenant FROM tasks WHERE id = $1`
	case "hold":
		q = `SELECT tenant FROM holds WHERE id = $1`
	default:
		return caller, nil
	}
	var tenant string
	err := tx.QueryRow(ctx, q, t.ID).Scan(&tenant)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", refuse("not_found", "no such %s", t.Kind)
	}
	return tenant, err
}

// resolveTargetID turns a worker or task id as the incident view shows it (lowercase) into the
// stored one, so every check and the operation see the same row (MGR-9).
func resolveTargetID(ctx context.Context, tx pgx.Tx, t *ControlTarget) error {
	var q string
	switch t.Kind {
	case "worker":
		q = `SELECT id FROM workers WHERE id = ANY($1) ORDER BY id = $2 DESC LIMIT 1`
	case "task":
		q = `SELECT id FROM tasks WHERE id = ANY($1) ORDER BY id = $2 DESC LIMIT 1`
	default:
		return nil
	}
	// A generated id is a lowercase prefix and an uppercase ULID (ids.New).
	canon := t.ID
	if p, rest, ok := strings.Cut(t.ID, "_"); ok {
		canon = strings.ToLower(p) + "_" + strings.ToUpper(rest)
	}
	var id string
	err := tx.QueryRow(ctx, q, []string{t.ID, canon}, t.ID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err == nil {
		t.ID = id
	}
	return err
}

// controlApply applies one control operation in tenant. A refusal comes back as a Problem in
// the first result, not as an error, so the safeguard's count of a seat refusal commits.
func (c *Core) controlApply(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, tenant string, a controlActor, req ControlRequest) (*Done, *Problem, error) {
	if err := resolveTargetID(ctx, tx, &req.Target); err != nil {
		return nil, nil, err
	}
	if !slices.Contains(controlOpsServed, req.Op) {
		return nil, refuse("input_schema_invalid", "operation %s is not served by this version", req.Op), nil
	}
	if (req.Op == "release") != (req.Target.Kind == "hold") {
		return nil, refuse("input_schema_invalid", "release takes a hold, and only release does"), nil
	}
	var m *managerSeat
	var inc *incidentRow
	if a.Seat {
		var err error
		m, err = loadManagerSeat(ctx, tx, true)
		if err != nil {
			return nil, nil, err
		}
		if m == nil || m.State != "held" || m.Holder != a.Principal || m.Epoch != a.Epoch {
			return nil, refuse("fenced", "the seat epoch changed"), nil
		}
		origin := ""
		p, err := c.seatChecks(ctx, tx, now, cat, tenant, a, req, &inc, &origin)
		if err != nil {
			return nil, nil, err
		}
		// A target that came from a finding goes to a person on a control card (MGR-26).
		if p != nil && p.Reason == "approval_required" && strings.HasPrefix(origin, "finding:") {
			id, err := c.openControlCard(ctx, tx, now, cat, tenant, a, req, inc)
			if err != nil {
				return nil, nil, err
			}
			return &Done{OK: true, ID: id, State: "card"}, nil, nil
		}
		if p != nil {
			// A conversion to a person's decision, or a person's hold, is not a refusal of the
			// safeguard (MGR-32, MGR-37).
			if p.Reason != "protected_target" && p.Reason != "approval_required" && p.Reason != "held" {
				if err := c.seatDenied(ctx, tx, now, cat, m); err != nil {
					return nil, nil, err
				}
			}
			return nil, p, nil
		}
	} else if req.Basis.Incident != "" {
		r, err := loadIncident(ctx, tx, tenant, req.Basis.Incident, true)
		if err != nil {
			return nil, nil, err
		}
		if r == nil {
			return nil, refuse("not_found", "no such incident in the tenant"), nil
		}
		inc = r
	}
	holdID, p, err := c.controlDo(ctx, tx, now, cat, tenant, a, req, inc)
	if err != nil {
		return nil, nil, err
	}
	if p != nil {
		// A refusal of the operation itself counts toward the safeguard like one of the checks
		// (MGR-14).
		if m != nil && p.Reason != "protected_target" && p.Reason != "approval_required" {
			if err := c.seatDenied(ctx, tx, now, cat, m); err != nil {
				return nil, nil, err
			}
		}
		return nil, p, nil
	}
	id := ids.New(ids.Control)
	actorKind, actor := "person", a.Operator.Principal
	var epoch any
	by := any(actor)
	if a.Seat {
		actorKind, actor, epoch = "seat", a.Principal, a.Epoch
		by = map[string]any{"kind": "seat", "epoch": a.Epoch}
	}
	incident := ""
	if inc != nil {
		incident = inc.ID
	}
	origin := "code"
	if a.Origin != "" {
		origin = a.Origin
	}
	if _, err := tx.Exec(ctx, `INSERT INTO control_ops (id, tenant, op, target_kind, target_id, incident, actor_kind, actor, epoch, origin, hold, approved_by, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
		id, tenant, req.Op, req.Target.Kind, req.Target.ID, nullable(incident), actorKind, actor, epoch, origin, nullable(holdID), nullable(a.ApprovedBy), now); err != nil {
		return nil, nil, err
	}
	data := map[string]any{"op": req.Op, "target": map[string]any{"kind": req.Target.Kind, "id": req.Target.ID}, "by": actorKind}
	if incident != "" {
		data["incident"] = incident
	}
	if epoch != nil {
		data["epoch"] = epoch
	}
	if err := c.appendEvent(ctx, tx, now, tenant, Event{Type: "control.applied.v1", Subject: id, Data: data}); err != nil {
		return nil, nil, err
	}
	auditActor := Actor{Principal: principalOr(a.Operator.Principal), Via: "farmctl"}
	if a.Seat {
		auditActor = Actor{Principal: "system", Via: "seat", Role: managerSeatID}
	}
	seq, err := c.appendAudit(ctx, tx, now, Audit{
		Tenant: tenant, Kind: "command", Actor: auditActor, Command: "control.apply",
		Target: &Target{Kind: req.Target.Kind, ID: req.Target.ID}, OK: true,
		After: map[string]any{"op": req.Op, "incident": nullable(incident), "epoch": epoch, "hold": nullable(holdID), "ttl": nullable(req.TTL),
			"origin": origin, "result": "applied"},
	})
	if err != nil {
		return nil, nil, err
	}
	if inc != nil {
		ops, _ := inc.Record["ops"].([]any)
		entry := map[string]any{"op": req.Op, "by": by, "at": fmtTime(now)}
		if holdID != "" {
			entry["id"] = holdID
		}
		// The record keeps the latest 100 operations; control_ops keeps them all.
		ops = append(ops, entry)
		if len(ops) > 100 {
			ops = ops[len(ops)-100:]
		}
		inc.Record["ops"] = ops
		inc.Handled = true
		// The first operation for the incident moves it to mitigating (MGR-8, spec/lifecycles/incident.md).
		first := inc.State == "open" || inc.State == "contained" || inc.State == "reopened"
		if first {
			inc.State = "mitigating"
		}
		if err := inc.save(ctx, tx, now); err != nil {
			return nil, nil, err
		}
		if first {
			if err := c.incidentEvent(ctx, tx, now, inc, "mitigating"); err != nil {
				return nil, nil, err
			}
		}
		if err := c.settleUnhandled(ctx, tx, now, inc); err != nil {
			return nil, nil, err
		}
		// The operation edits the incident's message to people in its own transaction: there
		// is no operation without an entry there (MGR-13, MGR-39).
		if err := c.noticeOpLine(ctx, tx, now, inc.ID, req, a.Seat); err != nil {
			return nil, nil, err
		}
	}
	if m != nil {
		// An applied seat operation ends a run of refusals (MGR-14).
		if _, err := tx.Exec(ctx, `UPDATE seats SET denials = 0 WHERE tenant = $1 AND id = $2`, farmTenant, managerSeatID); err != nil {
			return nil, nil, err
		}
	}
	return &Done{OK: true, ID: id, AuditSeq: seq, State: "applied"}, nil, nil
}

// seatChecks are the checks of a seat operation before it applies: the manager works in the
// tenant, the hourly limit, the basis incident, the target from its involved parties, the
// exclusions, the distrust mode, the protected set and the share of a role under holds.
func (c *Core) seatChecks(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, tenant string, a controlActor, req ControlRequest, incOut **incidentRow, origin *string) (*Problem, error) {
	if !managerEnabled(cat, tenant) {
		return refuse("manager_disabled", "the farm manager is disabled in the tenant"), nil
	}
	// A person's no_control hold on the farm or the tenant stops the seat's operations there
	// (MGR-15).
	if held, err := heldBy(ctx, tx, now, []string{"no_control"}, tenantScopes(tenant)...); err != nil || held != "" {
		if held != "" {
			return refuse("held", "a hold stops control operations in the tenant"), nil
		}
		return nil, err
	}
	if req.Op != "release" && !seatConsents(cat, tenant, req.Op) {
		return refuse("forbidden", "the tenant's farm-manager-ops has no control.apply:%s", req.Op), nil
	}
	lim := seatLimits(cat)
	n, err := seatOpsLastHour(ctx, tx, now)
	if err != nil {
		return nil, err
	}
	if n >= lim.OpsPerHour {
		return refuse("control_rate_limited", "the seat reached ops_per_hour"), nil
	}
	// A directive without an incident bounds the operation by its scope (MGR-4).
	var inc *incidentRow
	if req.Basis.Incident == "" && a.Directive != nil {
		if p, err := c.directiveAllows(ctx, tx, now, cat, tenant, a.Directive, req); err != nil || p != nil {
			return p, err
		}
	} else {
		if req.Basis.Incident == "" {
			return refuse("target_not_implicated", "a seat operation names its incident"), nil
		}
		var err error
		inc, err = loadIncident(ctx, tx, tenant, req.Basis.Incident, true)
		if err != nil {
			return nil, err
		}
		if inc == nil || !slices.Contains(incidentLive, inc.State) {
			return refuse("target_not_implicated", "no live incident of the tenant"), nil
		}
		*incOut = inc
		// The class's responses list what the seat may do for its incidents; release only
		// undoes the seat's own hold (MGR-9).
		if req.Op != "release" && !slices.Contains(mstrs(mmap(classByName(cat, inc.Class), "responses"), "ops"), req.Op) {
			return refuse("forbidden", "the incident class does not allow %s", req.Op), nil
		}
		if a.Directive != nil {
			if p, err := c.directiveAllows(ctx, tx, now, cat, tenant, a.Directive, req); err != nil || p != nil {
				return p, err
			}
		}
	}
	if req.Op == "release" {
		// Only a hold the seat set with the current epoch on the same basis, this incident or
		// none (control-ops.yaml, release).
		basis := ""
		if inc != nil {
			basis = inc.ID
		}
		var author, kind string
		var epoch *int
		err := tx.QueryRow(ctx, `SELECT h.author, h.author_kind, (SELECT o.epoch FROM control_ops o WHERE o.hold = h.id AND o.actor_kind = 'seat'
				AND o.incident IS NOT DISTINCT FROM $3 LIMIT 1)
			FROM holds h WHERE h.id = $1 AND h.tenant = $2`, req.Target.ID, tenant, nullable(basis)).Scan(&author, &kind, &epoch)
		if errors.Is(err, pgx.ErrNoRows) || err == nil && (author != a.Principal || kind != "seat_holder" || epoch == nil || *epoch != a.Epoch) {
			return refuse("forbidden", "only the seat's own holds of the current epoch are released"), nil
		}
		return nil, err
	}
	// A target outside the code list is reachable only through a finding of the tenant on that
	// target, and then only on a person's card (MGR-26).
	fromFinding := false
	if inc != nil && !implicatedTarget(inc, req.Target) {
		ok, err := findingTarget(ctx, tx, tenant, req.Finding, req.Target)
		if err != nil {
			return nil, err
		}
		if !ok {
			return refuse("target_not_implicated", "the target is not among the incident's involved parties"), nil
		}
		fromFinding = true
		*origin = "finding:" + req.Finding
	}
	if excluded, err := c.targetExcluded(ctx, tx, cat, tenant, req.Target); err != nil || excluded {
		if excluded {
			return refuse("target_excluded", "the target is excluded from seat operations"), nil
		}
		return nil, err
	}
	if a.Untrusted {
		// In distrust mode a cancellation goes only by a person's card, and one quarantine per
		// incident at most (MGR-22).
		if req.Op == "cancel" {
			return refuse("approval_required", "a cancellation in distrust mode goes to a person"), nil
		}
		if req.Op == "quarantine" && inc != nil {
			var q int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM control_ops WHERE incident = $1 AND op = 'quarantine' AND actor_kind = 'seat'`, inc.ID).Scan(&q); err != nil {
				return nil, err
			}
			if q >= 1 {
				return refuse("approval_required", "one quarantine per incident in distrust mode"), nil
			}
		}
	}
	if protected, err := c.protectedTarget(ctx, tx, cat, tenant, req.Target); err != nil || protected {
		if protected {
			return refuse("protected_target", "a person decides on a protected target"), nil
		}
		return nil, err
	}
	if fromFinding {
		return refuse("approval_required", "a target from a finding goes to a person"), nil
	}
	if slices.Contains([]string{"hold", "quarantine", "drain"}, req.Op) {
		over, err := c.shareExceeded(ctx, tx, now, cat, tenant, req.Target, lim.ShareHeld)
		if err != nil || over {
			if over {
				return refuse("approval_required", "the share of the role under seat holds would exceed the limit"), nil
			}
			return nil, err
		}
	}
	return nil, nil
}

// implicatedTarget says whether a target is among the involved parties code built for the
// incident: its workers, tasks, identities, roles and types, and its subject (MGR-9).
func implicatedTarget(inc *incidentRow, t ControlTarget) bool {
	if t.Kind == inc.SubjectKind && strings.EqualFold(t.ID, inc.SubjectID) && t.Kind != "tenant" {
		return true
	}
	key := map[string]string{"worker": "worker", "task": "task", "bot": "bot", "role": "role", "tasktype": "tasktype"}[t.Kind]
	if key == "" {
		return false
	}
	list, _ := inc.Record["implicated"].([]any)
	for _, v := range list {
		if strings.EqualFold(mstr(asMap(v), key), t.ID) && t.ID != "" {
			return true
		}
	}
	return false
}

// targetExcluded says whether a target is out of reach of seat operations: workers and roles of
// the excluded profiles, tasks and types of the excluded purposes (MGR-9).
func (c *Core) targetExcluded(ctx context.Context, tx pgx.Tx, cat *Catalog, tenant string, t ControlTarget) (bool, error) {
	role, typ := "", ""
	switch t.Kind {
	case "role":
		role = t.ID
	case "tasktype":
		typ = t.ID
	case "worker":
		if err := tx.QueryRow(ctx, `SELECT role FROM workers WHERE id = $1 AND tenant = $2`, t.ID, tenant).Scan(&role); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return false, err
		}
	case "bot":
		if err := tx.QueryRow(ctx, `SELECT role FROM bots WHERE id = $1 AND tenant = $2`, t.ID, tenant).Scan(&role); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return false, err
		}
	case "task":
		if err := tx.QueryRow(ctx, `SELECT type FROM tasks WHERE id = $1 AND tenant = $2`, t.ID, tenant).Scan(&typ); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return false, err
		}
	}
	if role != "" && slices.Contains(excludedProfiles, profileOf(cat, role)) {
		return true, nil
	}
	if typ != "" && slices.Contains(excludedPurposes, cat.TaskTypes[typ].Purpose) {
		return true, nil
	}
	types, roles := stopWriters(cat)
	return roles[role] || types[typ], nil
}

// stopWriters are the task types that publish the alarm of a class with stops_manager and the
// roles that claim them: the manager may not blind the alarm that stops it (GOV-28, MGR-9).
func stopWriters(cat *Catalog) (types, roles map[string]bool) {
	alarms := map[string]bool{}
	for _, ic := range incidentClasses(cat) {
		if a := mstr(mmap(ic.Spec, "detect"), "alarm"); a != "" && truthy(ic.Spec["stops_manager"]) {
			alarms[a] = true
		}
	}
	types, roles = map[string]bool{}, map[string]bool{}
	for name, tt := range cat.TaskTypes {
		if slices.ContainsFunc(tt.Events, func(e string) bool { return alarms[e] }) {
			types[name] = true
		}
	}
	for name, r := range cat.Roles {
		if slices.ContainsFunc(r.Claims, func(t string) bool { return types[t] }) {
			roles[name] = true
		}
	}
	return types, roles
}

// seatConsents says whether the tenant's consent group farm-manager-ops holds
// control.apply:<op> (MGR-3, PRM-13); release has no row in the consent table.
func seatConsents(cat *Catalog, tenant, op string) bool {
	tv := cat.Tenants[tenant]
	if tv == nil {
		return false
	}
	return hasCap(tv, groupClosure(tv, []string{"farm-manager-ops"}), "control.apply:"+op)
}

// seatOpsLastHour counts what the seat did in the sliding hour: its control operations and the
// granted service actions and scripts of its tasks, which share ops_per_hour (MGR-14).
func seatOpsLastHour(ctx context.Context, tx pgx.Tx, now time.Time) (int, error) {
	// One lock across tenants holds the count and the admission together until commit.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(7300110)`); err != nil {
		return 0, err
	}
	var n int
	err := tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM control_ops WHERE actor_kind = 'seat' AND created_at > $1)
		+ (SELECT count(*) FROM toolcalls c JOIN tasks t ON t.id = c.task
			LEFT JOIN approvals a ON a.id = c.approval_id LEFT JOIN tasks st ON st.id = a.source_task
			WHERE c.tool IN ($2, $3) AND c.access = 'write' AND (c.grant_id IS NOT NULL OR c.approval_id IS NOT NULL) AND c.state <> 'denied'
			AND c.created_at > $1 AND (t.record->>'seat' = $4 OR st.record->>'seat' = $4))`,
		now.Add(-time.Hour), serviceRight, scriptRight, managerSeatID).Scan(&n)
	return n, err
}

// seatToolGate locks the seat for a service action or a script of a seat task (rec is the task
// record) and checks the seat's shared ops_per_hour (MGR-14). It returns whether the task is a
// seat task, and control_rate_limited when the hour is full.
func seatToolGate(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, rec []byte) (bool, string, error) {
	if seatOfRecord(rec) != managerSeatID {
		return false, "", nil
	}
	// The seat row first, as in control.apply, then the count's lock.
	if _, err := loadManagerSeat(ctx, tx, true); err != nil {
		return false, "", err
	}
	n, err := seatOpsLastHour(ctx, tx, now)
	if err != nil || n < seatLimits(cat).OpsPerHour {
		return true, "", err
	}
	return true, "control_rate_limited", nil
}

// seatToolDone counts a seat task's service action or script toward the safeguard: a refusal
// adds to the run of refusals, a granted call ends it (MGR-14).
func (c *Core) seatToolDone(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, refused bool) error {
	if refused {
		return c.seatDenied(ctx, tx, now, cat, nil)
	}
	_, err := tx.Exec(ctx, `UPDATE seats SET denials = 0 WHERE tenant = $1 AND id = $2`, farmTenant, managerSeatID)
	return err
}

// protectedType is a type of the protected set: one that publishes alarms, makes backups or is a
// system type kept out of proposals (MGR-32).
func protectedType(cat *Catalog, typ string) bool {
	tt, ok := cat.TaskTypes[typ]
	if !ok {
		return false
	}
	if classNamed(cat)[typ] || tt.MakesBackup || tt.Proposable != nil && !*tt.Proposable {
		return true
	}
	return slices.ContainsFunc(tt.Events, func(e string) bool { return strings.HasPrefix(e, "alarm.") })
}

// classNamed are the names any class of the incident catalog mentions; the types and roles
// among them are protected targets (MGR-32).
func classNamed(cat *Catalog) map[string]bool {
	out := map[string]bool{}
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case string:
			out[x] = true
		case []any:
			for _, e := range x {
				walk(e)
			}
		case map[string]any:
			for _, e := range x {
				walk(e)
			}
		}
	}
	for _, ic := range incidentClasses(cat) {
		walk(ic.Spec)
	}
	return out
}

// protectedTarget says whether a target is in the protected set of the tenant: a protected type,
// a task of one, or a worker running a task of one now (MGR-32).
func (c *Core) protectedTarget(ctx context.Context, tx pgx.Tx, cat *Catalog, tenant string, t ControlTarget) (bool, error) {
	switch t.Kind {
	case "role":
		return classNamed(cat)[t.ID], nil
	case "tasktype":
		return protectedType(cat, t.ID), nil
	case "task":
		var typ string
		if err := tx.QueryRow(ctx, `SELECT type FROM tasks WHERE id = $1 AND tenant = $2`, t.ID, tenant).Scan(&typ); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return false, err
		}
		return protectedType(cat, typ), nil
	case "worker":
		// The worker's row lock, which a claim takes first, keeps a claim from slipping in
		// between this check and the hold.
		if _, err := tx.Exec(ctx, `SELECT 1 FROM workers WHERE id = $1 FOR UPDATE`, t.ID); err != nil {
			return false, err
		}
		rows, err := tx.Query(ctx, `SELECT t.type FROM leases l JOIN tasks t ON t.id = l.work_id WHERE l.worker_id = $1 AND l.state = 'active' AND l.kind = 'attempt'`, t.ID)
		if err != nil {
			return false, err
		}
		types, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return false, err
		}
		return slices.ContainsFunc(types, func(typ string) bool { return protectedType(cat, typ) }), nil
	}
	return false, nil
}

// shareExceeded says whether a seat hold on the target would put more than
// floor(max_share_held × n) of the workers of a role of the tenant under seat holds (MGR-37).
func (c *Core) shareExceeded(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, tenant string, t ControlTarget, share float64) (bool, error) {
	role := ""
	switch t.Kind {
	case "role":
		role = t.ID
	case "worker":
		if err := tx.QueryRow(ctx, `SELECT role FROM workers WHERE id = $1`, t.ID).Scan(&role); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return false, err
		}
	case "bot":
		if err := tx.QueryRow(ctx, `SELECT role FROM bots WHERE id = $1`, t.ID).Scan(&role); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return false, err
		}
	default:
		return false, nil
	}
	if role == "" {
		return false, nil
	}
	// Workers already under a live seat hold of their own, of their identity or of their role,
	// and those the new hold would add; a worker already held counts once.
	var n, held, after int
	if err := tx.QueryRow(ctx, `WITH w AS (SELECT w.id, EXISTS (
			SELECT 1 FROM holds h WHERE h.state = 'active' AND h.author_kind = 'seat_holder' AND h.scope_tenant = $1 AND (h.until IS NULL OR h.until > $3)
			AND (h.scope_kind = 'worker' AND h.scope_name = lower(w.id) OR h.scope_kind = 'role' AND h.scope_name = w.role
				OR h.scope_kind = 'bot' AND h.scope_name IN (SELECT id FROM bots WHERE worker_id = w.id))) AS held,
			($4 = 'role' OR $4 = 'worker' AND w.id = $5 OR $4 = 'bot' AND w.id IN (SELECT worker_id FROM bots WHERE id = $5)) AS covered
		FROM workers w WHERE w.tenant = $1 AND w.role = $2 AND w.state <> 'revoked')
		SELECT count(*), count(*) FILTER (WHERE held), count(*) FILTER (WHERE held OR covered) FROM w`,
		tenant, role, now, t.Kind, t.ID).Scan(&n, &held, &after); err != nil {
		return false, err
	}
	return after > held && after > int(math.Floor(share*float64(n))), nil
}

// seatDenied counts a seat refusal; the third in a row trips the safeguard: no_control joins
// the stop reasons and alarm.manager.breaker pages people (MGR-14).
func (c *Core) seatDenied(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, m *managerSeat) error {
	var n int
	if err := tx.QueryRow(ctx, `UPDATE seats SET denials = denials + 1 WHERE tenant = $1 AND id = $2 RETURNING denials`, farmTenant, managerSeatID).Scan(&n); err != nil {
		return err
	}
	if n < seatLimits(cat).Denials {
		return nil
	}
	if _, err := tx.Exec(ctx, `UPDATE seats SET denials = 0 WHERE tenant = $1 AND id = $2`, farmTenant, managerSeatID); err != nil {
		return err
	}
	// The safeguard is a seat hold with the effect no_control: the hold adds the stop reason in
	// the same transaction, and its release by a person with admin:farm removes it.
	if _, _, err := c.insertHold(ctx, tx, now, HoldActor{Principal: "system", Rank: "contain_auto", Tenant: farmTenant, Via: "system", Source: "safeguard"},
		HoldScope{Kind: "seat", Tenant: farmTenant, Name: managerSeatID}, []string{"no_control"}, "manager_breaker", nil); err != nil {
		return err
	}
	return c.fireAlarm(ctx, tx, now, Alarm{Tenant: farmTenant, Name: "manager.breaker", Subject: AlarmSubject{Kind: "seat", ID: managerSeatID}, Severity: "page",
		Values: map[string]any{"denials": n}})
}

// controlDo applies the operation itself and returns the hold it set, if any.
func (c *Core) controlDo(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, tenant string, a controlActor, req ControlRequest, inc *incidentRow) (string, *Problem, error) {
	switch req.Op {
	case "hold", "quarantine", "drain":
		effects := controlEffects(req)
		if len(effects) == 0 {
			return "", refuse("input_schema_invalid", "a hold has effects"), nil
		}
		if req.Op != "hold" && req.Target.Kind != "worker" {
			return "", refuse("input_schema_invalid", "%s takes a worker", req.Op), nil
		}
		// A bot group hold is not checked by this version's hold matching, so it is not offered.
		if !slices.Contains([]string{"worker", "bot", "role", "tasktype", "resource"}, req.Target.Kind) {
			return "", refuse("input_schema_invalid", "a hold takes a worker, bot, role, type or resource"), nil
		}
		ttl := defaultHoldTTLMax
		if a.Seat {
			ttl = seatLimits(cat).HoldTTLMax
		}
		if req.TTL != "" {
			d, ok := parseDuration(req.TTL)
			if !ok || d <= 0 {
				return "", refuse("input_schema_invalid", "ttl"), nil
			}
			if a.Seat && d > ttl {
				// A longer hold is refused, not cut short, so the seat does not count on it.
				return "", refuse("input_schema_invalid", "ttl above hold_ttl_max"), nil
			}
			ttl = d
		}
		var until *time.Time
		actor := OperatorActor(a.Operator)
		if a.Seat {
			source := ""
			if inc != nil {
				source = inc.ID
			}
			actor = HoldActor{Principal: a.Principal, Rank: "seat_holder", Tenant: tenant, Via: "seat", Source: source}
		}
		u := now.Add(ttl)
		until = &u
		name := req.Target.ID
		if req.Target.Kind == "worker" {
			name = strings.ToLower(name)
		}
		if a.Seat && req.Target.Kind == "bot" {
			// A seat hold of an identity only delays its tasks (MGR-9).
			effects = []string{"no_claim"}
		}
		id, _, err := c.insertHold(ctx, tx, now, actor, HoldScope{Kind: req.Target.Kind, Tenant: tenant, Name: name}, effects, "by_manager", until)
		if err != nil {
			return "", nil, err
		}
		if req.Op == "drain" {
			// Only a drain moves the worker to draining; hello after the hold ends returns it to
			// active (spec/lifecycles/worker.md).
			if _, err := tx.Exec(ctx, `UPDATE workers SET state = 'draining' WHERE id = $1 AND tenant = $2 AND state = 'active'`, req.Target.ID, tenant); err != nil {
				return "", nil, err
			}
		}
		return id, nil, nil
	case "cancel", "preempt":
		if req.Target.Kind != "task" {
			return "", refuse("input_schema_invalid", "%s takes a task", req.Op), nil
		}
		var state string
		var rec []byte
		err := tx.QueryRow(ctx, `SELECT state, record FROM tasks WHERE id = $1 AND tenant = $2 FOR UPDATE`, req.Target.ID, tenant).Scan(&state, &rec)
		if errors.Is(err, pgx.ErrNoRows) {
			return "", refuse("not_found", "no such task in the tenant"), nil
		}
		if err != nil {
			return "", nil, err
		}
		if !slices.Contains([]string{"submitted", "working"}, state) {
			return "", refuse("conflict", "the task is %s", state), nil
		}
		if req.Op == "cancel" {
			return "", nil, c.cancelTaskIDs(ctx, tx, now, tenant, []string{req.Target.ID}, "by_manager")
		}
		var r struct {
			Preempts int `json:"preempts"`
		}
		_ = json.Unmarshal(rec, &r)
		if state != "working" {
			return "", refuse("conflict", "the task has no running attempt"), nil
		}
		if r.Preempts >= maxPreempts {
			return "", nil, c.cancelTaskIDs(ctx, tx, now, tenant, []string{req.Target.ID}, "preempt_limit")
		}
		if err := c.preemptAttempt(ctx, tx, now, req.Target.ID); err != nil {
			return "", nil, err
		}
		_, err = tx.Exec(ctx, `UPDATE tasks SET record = record || jsonb_build_object('preempts', $2::int) WHERE id = $1`, req.Target.ID, r.Preempts+1)
		return "", nil, err
	case "release":
		h, err := loadHold(ctx, tx, req.Target.ID, true)
		if err != nil {
			return "", nil, err
		}
		if h == nil || h.Tenant != tenant {
			return "", refuse("not_found", "no such hold in the tenant"), nil
		}
		if !a.Seat && h.AuthorKind != "seat_holder" {
			return "", refuse("forbidden", "control.apply releases only seat holds; people use hold.release"), nil
		}
		if h.State != "active" {
			return h.ID, nil, nil
		}
		releasedBy := a.Operator.Principal
		actor := OperatorActor(a.Operator)
		if a.Seat {
			releasedBy, actor = a.Principal, HoldActor{Principal: a.Principal, Rank: "seat_holder", Tenant: tenant, Via: "seat"}
		}
		if _, err := tx.Exec(ctx, `UPDATE holds SET state = 'released', released_by = $2, ended_at = $3 WHERE id = $1`, h.ID, releasedBy, now); err != nil {
			return "", nil, err
		}
		h.State = "released"
		_, err = c.holdChanged(ctx, tx, now, h.Hold, "hold.released.v1", "hold.release", actor, releasedBy)
		return h.ID, nil, err
	}
	return "", refuse("input_schema_invalid", "operation %s", req.Op), nil
}

// preemptAttempt moves the running attempt of a task to preempted without spending it, and the
// task goes back to the queue (MGR-11).
func (c *Core) preemptAttempt(ctx context.Context, tx pgx.Tx, now time.Time, task string) error {
	var l leaseRow
	err := tx.QueryRow(ctx, `SELECT work_id, lease_no, kind, tenant, worker_id, epoch, state, lease_until FROM leases
		WHERE work_id = $1 AND state = 'active' AND kind = 'attempt' FOR UPDATE`, task).
		Scan(&l.WorkID, &l.LeaseNo, &l.Kind, &l.Tenant, &l.WorkerID, &l.Epoch, &l.State, &l.Until)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := closeLease(ctx, tx, now, &l, "preempted", "preempted", "heartbeat", nil, &HeartbeatResult{CancelRequested: true}); err != nil {
		return err
	}
	if err := c.requeue(ctx, tx, now, &l, now); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE tasks SET reason = 'preempted' WHERE id = $1`, task)
	return err
}

// ControlApply is P2 control.apply: a person of the tenant calls an operation by the seat's code
// path; preemption is an operator operation of farmctl (PRM-14).
func (c *Core) ControlApply(ctx context.Context, op Operator, req ControlRequest) (*Done, error) {
	cat, err := c.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	var out *Done
	err = c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		if err := resolveTargetID(ctx, tx, &req.Target); err != nil {
			return err
		}
		tenant, err := controlTargetTenant(ctx, tx, req.Target, op.Tenant)
		if err != nil {
			return err
		}
		if tenant == farmTenant || op.Tenant != farmTenant && tenant != op.Tenant {
			return refuse("forbidden", "the operator credential is for another tenant")
		}
		d, p, err := c.controlApply(ctx, tx, now, cat, tenant, controlActor{Operator: op}, req)
		if err != nil {
			return err
		}
		if p != nil {
			return p
		}
		out = d
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ControlRevertRequest is p2-control-revert-request.
type ControlRevertRequest struct {
	Since    string `json:"since,omitempty"`
	Incident string `json:"incident,omitempty"`
}

// ControlRevert is P2 control.revert: it releases the seat's holds on one incident (an operator
// of its tenant) or all seat holds set after an instant (the operator of farm). Actions already
// executed stay (MGR-15).
func (c *Core) ControlRevert(ctx context.Context, op Operator, req ControlRevertRequest) (*Done, error) {
	if (req.Since == "") == (req.Incident == "") {
		return nil, refuse("input_schema_invalid", "since or incident")
	}
	var since time.Time
	if req.Since != "" {
		if op.Tenant != farmTenant {
			return nil, refuse("operator_only", "a revert for a period is an operation of the operator of farm")
		}
		t, err := time.Parse(time.RFC3339Nano, req.Since)
		if err != nil {
			return nil, refuse("input_schema_invalid", "since")
		}
		since = t
	}
	var out Done
	err := c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		q := `SELECT ` + holdColumns + ` FROM holds WHERE state = 'active' AND author_kind = 'seat_holder' AND created_at >= $1 ORDER BY id FOR UPDATE`
		args := []any{since}
		if req.Incident != "" {
			// The operator of farm reverts an incident of any tenant: the revert command of the
			// farm operators' message names only the incident (MGR-15, MGR-39).
			q = `SELECT ` + holdColumns + ` FROM holds WHERE state = 'active' AND author_kind = 'seat_holder' AND lower(source) = lower($1)
				AND ($2 = 'farm' OR tenant = $2) ORDER BY id FOR UPDATE`
			args = []any{req.Incident, op.Tenant}
		}
		rows, err := tx.Query(ctx, q, args...)
		if err != nil {
			return err
		}
		list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (*holdRow, error) { return scanHold(r) })
		if err != nil {
			return err
		}
		for _, h := range list {
			if _, err := tx.Exec(ctx, `UPDATE holds SET state = 'released', released_by = $2, ended_at = $3 WHERE id = $1`, h.ID, op.Principal, now); err != nil {
				return err
			}
			h.State = "released"
			if _, err := c.holdChanged(ctx, tx, now, h.Hold, "hold.released.v1", "control.revert", OperatorActor(op), op.Principal); err != nil {
				return err
			}
		}
		// A pending approval a seat task asked for would still run the reverted work (MGR-15).
		aq := `SELECT ` + approvalColumns + ` FROM approvals a WHERE a.state = 'pending' AND a.created_at >= $1
			AND EXISTS (SELECT 1 FROM tasks t WHERE t.id = a.source_task AND t.record->>'seat' = $2) ORDER BY a.id FOR UPDATE`
		aargs := []any{since, managerSeatID}
		if req.Incident != "" {
			aq = `SELECT ` + approvalColumns + ` FROM approvals a WHERE a.state = 'pending' AND ($3 = 'farm' OR a.tenant = $3)
				AND EXISTS (SELECT 1 FROM tasks t WHERE t.id = a.source_task AND t.record->>'seat' = $2 AND lower(t.record->>'incident') = lower($1)) ORDER BY a.id FOR UPDATE`
			aargs = []any{req.Incident, managerSeatID, op.Tenant}
		}
		arows, err := tx.Query(ctx, aq, aargs...)
		if err != nil {
			return err
		}
		as, err := pgx.CollectRows(arows, func(r pgx.CollectableRow) (*approvalRow, error) { return scanApproval(r) })
		if err != nil {
			return err
		}
		for _, a := range as {
			if err := c.closeApproval(ctx, tx, now, a, "canceled", "control_reverted"); err != nil {
				return err
			}
		}
		out = Done{OK: true, State: fmt.Sprintf("released %d", len(list))}
		if req.Incident == "" || len(list) == 0 {
			return nil
		}
		// The revert is a line of the incident's message to people too (MGR-39).
		var id string
		err = tx.QueryRow(ctx, `SELECT id FROM incidents WHERE lower(id) = lower($1) AND ($2 = 'farm' OR tenant = $2)`, req.Incident, op.Tenant).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		return c.noticeLog(ctx, tx, now, id, noticeEntry{Kind: "op", Op: "revert", By: "person"}, true)
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// ControlList is P2 control.list: the applied operations of a tenant, newest first.
func (c *Core) ControlList(ctx context.Context, op Operator, req TenantRequest) (map[string]any, error) {
	tenant, err := viewTenant(op, req.Tenant)
	if err != nil {
		return nil, err
	}
	limit := req.Limit
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	rows, err := c.st.Pool.Query(ctx, `SELECT id, op, target_kind, target_id, created_at FROM control_ops WHERE tenant = $1
		ORDER BY created_at DESC, id DESC LIMIT $2`, tenant, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []any{}
	for rows.Next() {
		var id, o, kind, target string
		var at time.Time
		if err := rows.Scan(&id, &o, &kind, &target, &at); err != nil {
			return nil, err
		}
		items = append(items, map[string]any{"id": id, "state": "applied", "created_at": fmtTime(at), "title": o + " " + kind + " " + target})
	}
	return map[string]any{"items": items}, rows.Err()
}

// controlTool offers control_ops to a decision attempt of the farm manager seat (MGR-9, PRF-1).
func (c *Core) controlTool(cat *Catalog, g *toolsGrant) *catalogTool {
	if g.Purpose != "manage" || profileOf(cat, g.Role) != "farm-manager" {
		return nil
	}
	// Only the operations the tenant consented to (MGR-3); release undoes the seat's own hold.
	ops := []any{}
	for _, o := range controlOpsServed {
		if o == "release" || seatConsents(cat, g.Tenant, o) {
			ops = append(ops, o)
		}
	}
	schema := map[string]any{
		"type": "object", "additionalProperties": false, "required": []any{"op", "target"},
		"properties": map[string]any{
			"op": map[string]any{"enum": ops},
			"target": map[string]any{"type": "object", "additionalProperties": false, "required": []any{"kind", "id"}, "properties": map[string]any{
				"kind": map[string]any{"enum": []any{"worker", "bot", "role", "tasktype", "resource", "task", "hold"}},
				"id":   map[string]any{"type": "string", "maxLength": 64, "pattern": "^[A-Za-z0-9:_.-]{1,64}$"},
			}},
			"basis": map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{
				"incident": map[string]any{"type": "string", "maxLength": 64, "pattern": "^inc_[A-Za-z0-9]{1,60}$"},
			}},
			"finding": map[string]any{"type": "string", "maxLength": 64, "pattern": "^fd_[A-Za-z0-9]{1,60}$"},
			"effects": map[string]any{"type": "array", "maxItems": 6, "uniqueItems": true, "items": map[string]any{
				"enum": []any{"no_enqueue", "no_claim", "no_tool", "no_action", "no_delivery", "cancel_running"}}},
			"ttl": map[string]any{"type": "string", "maxLength": 8, "pattern": "^[1-9][0-9]{0,3}(s|m|h)$"},
		},
	}
	desc := "Apply a control operation of the farm manager seat on a target involved in the incident or in the directive's scope: hold, quarantine, drain, preempt, cancel, or release your own hold. A target that only a finding names goes to a person; name the finding."
	return &catalogTool{Name: controlToolName, Builtin: true, Tool: toolspec.Tool{Description: desc}, Schema: schema}
}

// controlToolCall answers control_ops: only a decision attempt on behalf of the seat holder with
// the current epoch applies; a refusal goes back to the model as its code.
func (c *Core) controlToolCall(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, g *toolsGrant, offered bool, args map[string]any) (*mcp.CallResult, error) {
	answer := func(m map[string]any, isErr bool) *mcp.CallResult {
		b, _ := json.Marshal(m)
		return &mcp.CallResult{Content: []mcp.Content{{Type: "text", Text: string(b)}}, IsError: isErr}
	}
	if !offered {
		return answer(map[string]any{"code": "forbidden"}, true), nil
	}
	tool := c.controlTool(cat, g)
	if tool == nil {
		return answer(map[string]any{"code": "forbidden"}, true), nil
	}
	acting, err := leaseActing(ctx, tx, g.Task, g.Worker)
	if err != nil {
		return nil, err
	}
	if acting.Seat != managerSeatID {
		return answer(map[string]any{"code": "forbidden"}, true), nil
	}
	// Arguments outside the schema, an operation without consent included, are a refusal of the
	// holder's call like any other (MGR-14).
	invalid := func() (*mcp.CallResult, error) {
		m, err := loadManagerSeat(ctx, tx, true)
		if err != nil {
			return nil, err
		}
		if m != nil && m.State == "held" && m.Holder == acting.Bot && m.Epoch == acting.SeatEpoch {
			if err := c.seatDenied(ctx, tx, now, cat, m); err != nil {
				return nil, err
			}
		}
		return answer(map[string]any{"code": "input_schema_invalid"}, true), nil
	}
	if err := c.rs.validate(tool.Schema, args); err != nil {
		return invalid()
	}
	raw, _ := json.Marshal(args)
	var req ControlRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return invalid()
	}
	var untrusted bool
	var rec []byte
	if err := tx.QueryRow(ctx, `SELECT coalesce((record->>'untrusted_seen')::boolean, false), record FROM tasks WHERE id = $1`, g.Task).Scan(&untrusted, &rec); err != nil {
		return nil, err
	}
	d, p, err := c.controlApply(ctx, tx, now, cat, g.Tenant, controlActor{Seat: true, Principal: acting.Bot, Epoch: acting.SeatEpoch, Untrusted: untrusted,
		Directive: taskDirective(rec), Task: g.Task}, req)
	if err != nil {
		return nil, err
	}
	if p != nil {
		return answer(map[string]any{"code": p.Reason}, true), nil
	}
	if d.State == "card" {
		return answer(map[string]any{"code": "card", "id": d.ID}, false), nil
	}
	out := map[string]any{"code": "applied", "id": d.ID}
	// A hold's own id is what release takes.
	var hold string
	if err := tx.QueryRow(ctx, `SELECT coalesce(hold, '') FROM control_ops WHERE id = $1`, d.ID).Scan(&hold); err != nil {
		return nil, err
	}
	if hold != "" {
		out["hold"] = hold
	}
	return answer(out, false), nil
}
