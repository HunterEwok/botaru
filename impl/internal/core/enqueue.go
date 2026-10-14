// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/ids"
	"github.com/HunterEwok/botaru/impl/internal/jcs"
	"github.com/HunterEwok/botaru/impl/internal/manifest"
	"github.com/HunterEwok/botaru/impl/internal/money"
)

// Origin is where to reply: the channel and the chat, written by the core on intake (DLV-6).
type Origin struct {
	Channel string `json:"channel"`
	Chat    Chat   `json:"chat"`
}

// Chat is a chat of a channel.
type Chat struct {
	Kind       string `json:"kind"`
	ExternalID string `json:"external_id"`
}

// ID is the chat target id of a delivery: <direct|group>:<external id>.
func (c Chat) ID() string { return c.Kind + ":" + c.ExternalID }

type enqueueInput struct {
	Tenant         string
	Type           string
	Body           map[string]any
	IdempotencyKey string
	Initiator      string
	EnqueuedBy     string
	Origin         *Origin
	Priority       int
	// Source is where the task comes from: interactive (a person's or a program's direct
	// request), campaign, route, schedule or script with SourceID, or empty for a task the core
	// creates itself; it sets fairness_key (DSP-2).
	Source   string
	SourceID string
	// FairnessKey is set for a child, a continuation, a redrive or an action task after
	// approval: they keep the key of their source (DSP-2).
	FairnessKey string
	// Cause is the id of the direct cause of task.submitted: the incoming message of a channel
	// task (OBS-2).
	Cause string
}

// fairnessKey writes the dispatcher fairness key <kind>:<id> from the task source (DSP-2,
// D-98). The core writes it on every task from v0.1; it orders dispatch only from v0.2 (DSP-6).
func fairnessKey(in enqueueInput, tt manifest.TaskType) string {
	if in.FairnessKey != "" {
		return in.FairnessKey
	}
	switch in.Source {
	case "interactive":
		return "interactive:" + in.Initiator
	case "campaign", "route", "schedule", "script":
		if in.SourceID != "" {
			return in.Source + ":" + in.SourceID
		}
	}
	purpose := tt.Purpose
	if purpose == "" {
		purpose = "work"
	}
	return "system:" + purpose
}

// newTraceparent starts the W3C trace of a task tree (OBS-2): version 00, a random trace id, a
// random parent span id, sampled.
func newTraceparent() (string, error) {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "00-" + hex.EncodeToString(b[:16]) + "-" + hex.EncodeToString(b[16:]) + "-01", nil
}

// enqueueOutcome is a written task, an existing task with the same key, or a refusal. A refusal
// is not an error of the transaction: its audit record is committed (spec/lifecycles/task.md,
// "rejected").
type enqueueOutcome struct {
	TaskID   string
	Existing bool
	Rejected *Problem
}

// groupClosure returns the groups of a principal with the groups they inherit (PRM-2).
func groupClosure(tv *TenantView, groups []string) []string {
	seen := map[string]bool{}
	var walk func(string)
	walk = func(g string) {
		if seen[g] {
			return
		}
		gr, ok := tv.Groups[g]
		if !ok {
			return
		}
		seen[g] = true
		for _, p := range gr.Inherits {
			walk(p)
		}
	}
	for _, g := range groups {
		walk(g)
	}
	out := make([]string, 0, len(seen))
	for g := range seen {
		out = append(out, g)
	}
	sort.Strings(out)
	return out
}

// hasCap reports whether one of the groups grants the capability. Wildcards are not allowed
// in action capabilities (PRM-7); a task capability matches exactly.
func hasCap(tv *TenantView, groups []string, cap string) bool {
	for _, g := range groups {
		for _, have := range tv.Groups[g].Caps {
			if capCovers(have, cap) {
				return true
			}
		}
	}
	return false
}

// capCovers says whether a granted capability covers the wanted one: the same string, or a
// task capability with a trailing wildcard on a dot boundary (task:report.* covers
// task:report.daily.v1).
func capCovers(have, want string) bool {
	if have == want {
		return true
	}
	prefix, ok := strings.CutSuffix(have, "*")
	return ok && strings.HasPrefix(have, "task:") && strings.HasSuffix(prefix, ".") && strings.HasPrefix(want, prefix)
}

func hasData(tv *TenantView, groups []string, mark string) bool {
	for _, g := range groups {
		if slices.Contains(tv.Groups[g].Data, mark) {
			return true
		}
	}
	return false
}

// groupsWithCap lists the groups of the tenant that grant a capability: the needs field of a
// refusal (PRM-8).
func groupsWithCap(tv *TenantView, cap string) []string {
	var out []string
	for name, g := range tv.Groups {
		if slices.Contains(g.Caps, cap) {
			out = append(out, "group:"+name)
		}
	}
	sort.Strings(out)
	return out
}

func durationOr(s string, def time.Duration) time.Duration {
	if d, ok := parseDuration(s); ok {
		return d
	}
	return def
}

// parseDuration reads the duration form of the schemas: 15m, 60s, 2h, 7d.
func parseDuration(s string) (time.Duration, bool) {
	if len(s) < 2 {
		return 0, false
	}
	unit := map[byte]time.Duration{'s': time.Second, 'm': time.Minute, 'h': time.Hour, 'd': 24 * time.Hour}[s[len(s)-1]]
	if unit == 0 {
		return 0, false
	}
	n := 0
	for _, ch := range s[:len(s)-1] {
		if ch < '0' || ch > '9' {
			return 0, false
		}
		n = n*10 + int(ch-'0')
	}
	return time.Duration(n) * unit, n > 0
}

// enqueue writes a task after the enqueue checks, in order (spec/lifecycles/task.md): the
// body schema, the initiator's rights task:<type> and data:<mark>, the budget, the queue
// ceiling and the key. A refusal writes nothing except the audit record.
func (c *Core) enqueue(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, in enqueueInput) (*enqueueOutcome, error) {
	tv := cat.Tenants[in.Tenant]
	tt, ok := cat.TaskTypes[in.Type]
	if !ok || strings.HasPrefix(in.Type, "action.") {
		return nil, refuse("type_not_allowed", "unknown task type")
	}
	bodyHash, err := jcs.Hash(in.Body)
	if err != nil {
		return nil, err
	}
	// The key: the same initiator and body return the existing task (IDM-1).
	var existingID, existingInit, existingHash string
	err = tx.QueryRow(ctx, `SELECT id, initiator, body_hash FROM tasks WHERE tenant = $1 AND idempotency_key = $2`, in.Tenant, in.IdempotencyKey).
		Scan(&existingID, &existingInit, &existingHash)
	if err == nil {
		if existingInit == in.Initiator && existingHash == bodyHash {
			return &enqueueOutcome{TaskID: existingID, Existing: true}, nil
		}
		return nil, refuse("idempotency_conflict", "the key is taken by another task")
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	reject := func(p *Problem) (*enqueueOutcome, error) {
		_, err := c.appendAudit(ctx, tx, now, Audit{
			Tenant: in.Tenant, Kind: "authorize", Actor: Actor{Principal: in.Initiator, Via: viaOf(in.EnqueuedBy)},
			Command: "task.enqueue", Target: &Target{Kind: "task_type", ID: in.Type}, OK: false, Reason: p.Reason,
			After: map[string]any{"type": in.Type, "decision": "deny"},
		})
		if err != nil {
			return nil, err
		}
		return &enqueueOutcome{Rejected: p}, nil
	}
	if err := c.spec.Validate("task-body", in.Body); err != nil {
		return reject(refuse("input_schema_invalid", "the body does not match the task body schema"))
	}
	if err := c.validateParams(tt, in.Body); err != nil {
		return reject(refuse("input_schema_invalid", "params do not match params_schema"))
	}
	// A hold with no_enqueue on the farm, the tenant or the type is checked before the
	// permission formula, as a ban no permission lifts (HLD-3, PRM-2).
	if held, err := heldBy(ctx, tx, now, []string{"no_enqueue"},
		append(tenantScopes(in.Tenant), scopeRef{kind: "tasktype", tenant: in.Tenant, name: in.Type})...); err != nil {
		return nil, err
	} else if held != "" {
		return reject(refuse("held", "a hold forbids new tasks of this type"))
	}
	// The initiator: an active principal with task:<type> and data:<mark> (PRM-3, PRM-5).
	var groups []string
	if in.Initiator != "system" {
		p, err := loadPrincipal(ctx, tx, now, in.Tenant, in.Initiator)
		if errors.Is(err, pgx.ErrNoRows) {
			return reject(refuse("person_inactive", "unknown initiator"))
		}
		if err != nil {
			return nil, err
		}
		if p.State != "active" {
			return reject(refuse("person_inactive", "the initiator is not active"))
		}
		groups = groupClosure(tv, p.Groups)
	}
	capName := "task:" + in.Type
	if !hasCap(tv, groups, capName) {
		pr := refuse("no_group_cap", "the initiator has no %s", capName)
		pr.Needs = groupsWithCap(tv, capName)
		return reject(pr)
	}
	if !hasData(tv, groups, tt.DataClass) {
		pr := refuse("no_data_class", "the initiator has no data:%s", tt.DataClass)
		pr.Needs = []string{"data:" + tt.DataClass}
		return reject(pr)
	}
	// Money: the estimate of the task is reserved in the tenant windows and the person's day
	// limit, next to usage and open reserves (BUD-1). The reserve is written with the task below.
	estimate, err := taskEstimate(ctx, tx, now, in.Tenant, in.Type, tt)
	if err != nil {
		return nil, err
	}
	line := c.budgetLine(tt.Purpose)
	if p := c.checkBudget(ctx, tx, now, tv, budgetAsk{subject: in.Initiator, groups: groups, extra: estimate, line: line}); p != nil {
		return reject(p)
	}
	// The enqueue ceiling of the sliding hour, under the tenant's budget lock (CAP-1).
	if max := tv.Spec.Limits.MaxEnqueuePerHour; max > 0 {
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE tenant = $1 AND created_at > $2`, in.Tenant, now.Add(-time.Hour)).Scan(&n); err != nil {
			return nil, err
		}
		if n >= max {
			pr := refuse("over_capacity", "the tenant enqueue limit of the hour is reached")
			pr.RetryAfterS = 60
			return reject(pr)
		}
	}
	// The queue ceiling (CAP-1).
	if max := tv.Spec.Limits.MaxQueued; max > 0 {
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE tenant = $1 AND state IN ('submitted', 'working')`, in.Tenant).Scan(&n); err != nil {
			return nil, err
		}
		if n >= max {
			pr := refuse("over_capacity", "the tenant queue is full")
			pr.RetryAfterS = 60
			return reject(pr)
		}
	}

	prio := in.Priority
	if prio == 0 {
		prio = tt.Dispatch.PriorityClass
		if prio == 0 {
			prio = 3
		}
		if in.Source == "interactive" && tt.Interactive {
			prio = 2
		}
	}
	id := ids.New(ids.Task)
	trace, err := newTraceparent()
	if err != nil {
		return nil, err
	}
	record := map[string]any{
		"tenant":         in.Tenant,
		"enqueued_by":    in.EnqueuedBy,
		"depth":          0,
		"root":           id,
		"correlationid":  id,
		"max_tier":       tt.Tier,
		"data_mark":      tt.DataClass,
		"untrusted_seen": false,
		"fairness_key":   fairnessKey(in, tt),
		// The budget line and purpose are fixed at enqueue: an apply that changes the type's
		// purpose later does not move the task's spending to another line (BUD-7).
		"budget_line": line,
		"purpose":     purposeOf(tt),
		"trace":       trace,
	}
	if in.Origin != nil {
		record["origin"] = in.Origin
		record["conversation"] = "c:" + in.Origin.Channel + ":" + in.Origin.Chat.ID()
	}
	rec, err := json.Marshal(record)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(in.Body)
	if err != nil {
		return nil, err
	}
	deadline := now.Add(durationOr(tt.Timeouts.Deadline, time.Hour))
	// A task nobody took by then raises alarm.task.unclaimed: the tenant's setting, else the
	// type's start_by (DSP-8).
	var startBy any
	after := tv.Spec.Dispatcher.UnclaimedAlarmAfter
	if after == "" {
		after = tt.Timeouts.StartBy
	}
	if d, ok := parseDuration(after); ok {
		startBy = now.Add(d)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO tasks (id, tenant, type, state, initiator, priority_class, idempotency_key, body_hash, body, record, deadline_at, start_by_at, created_at, updated_at)
		VALUES ($1, $2, $3, 'submitted', $4, $5, $6, $7, $8, $9, $10, $11, $12, $12)`,
		id, in.Tenant, in.Type, in.Initiator, prio, in.IdempotencyKey, bodyHash, body, rec, deadline, startBy, now); err != nil {
		return nil, err
	}
	if estimate > 0 {
		if _, err := tx.Exec(ctx, `INSERT INTO spend (id, tenant, task, root, subject, line, purpose, unit, state, eur_micros, time)
			VALUES ($1, $2, $3, $3, $4, $5, $6, 'task', 'reserved', $7, $8)`, ids.New(ids.Spend), in.Tenant, id, in.Initiator, line, purposeOf(tt), estimate, now); err != nil {
			return nil, err
		}
	}
	if err := c.appendEvent(ctx, tx, now, in.Tenant, Event{Type: "task.submitted", Subject: id, Data: map[string]any{"task": id, "type": in.Type}, Causation: in.Cause}); err != nil {
		return nil, err
	}
	if _, err := c.appendAudit(ctx, tx, now, Audit{
		Tenant: in.Tenant, Kind: "authorize", Actor: Actor{Principal: in.Initiator, Via: viaOf(in.EnqueuedBy)},
		Command: "task.enqueue", Target: &Target{Kind: "task", ID: id}, OK: true,
		After: map[string]any{"type": in.Type, "decision": "allow"},
	}); err != nil {
		return nil, err
	}
	if err := notifyWork(ctx, tx); err != nil {
		return nil, err
	}
	return &enqueueOutcome{TaskID: id}, nil
}

func viaOf(enqueuedBy string) string {
	switch {
	case strings.HasPrefix(enqueuedBy, "channel:"):
		return "channel"
	case strings.HasPrefix(enqueuedBy, "worker:"):
		return "bot"
	default:
		return "core"
	}
}

func (c *Core) validateParams(tt manifest.TaskType, body map[string]any) error {
	params, ok := body["params"]
	if !ok {
		params = map[string]any{}
	}
	if tt.ParamsSchema == nil {
		return nil
	}
	return c.rs.validate(tt.ParamsSchema, params)
}

// taskEstimate is the enqueue estimate of a task (BUD-14): the 90th percentile of the price of
// the whole tree of the finished tasks of the type in the tenant over the last 30 days, and
// without such tasks the worst case, the type's tree budget (GW-6). A type without a tree
// budget and without history has no estimate; P4 still reserves every call (BUD-2).
func taskEstimate(ctx context.Context, tx pgx.Tx, now time.Time, tenant, typ string, tt manifest.TaskType) (int64, error) {
	var n int
	var p90 float64
	err := tx.QueryRow(ctx, `SELECT count(*), coalesce(percentile_cont(0.9) WITHIN GROUP (ORDER BY cost), 0) FROM (
		SELECT (SELECT coalesce(sum(s.eur_micros), 0) FROM spend s WHERE s.root = t.id AND s.unit <> 'task') AS cost
		FROM tasks t WHERE t.tenant = $1 AND t.type = $2 AND t.state IN ('completed', 'failed') AND t.updated_at >= $3) x`,
		tenant, typ, now.Add(-30*24*time.Hour)).Scan(&n, &p90)
	if err != nil {
		return 0, err
	}
	if n > 0 {
		return int64(math.Ceil(p90)), nil
	}
	if tt.TreeBudgetEUR > 0 {
		return money.FromEUR(tt.TreeBudgetEUR), nil
	}
	return 0, nil
}
