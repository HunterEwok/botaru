// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
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
	Source         string // interactive, schedule, route, root
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
		if slices.Contains(tv.Groups[g].Caps, cap) {
			return true
		}
	}
	return false
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
	if p := c.checkBudget(ctx, tx, now, tv, in.Initiator, groups, estimate, ""); p != nil {
		return reject(p)
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
	record := map[string]any{
		"tenant":         in.Tenant,
		"enqueued_by":    in.EnqueuedBy,
		"depth":          0,
		"root":           id,
		"correlationid":  id,
		"max_tier":       tt.Tier,
		"data_mark":      tt.DataClass,
		"untrusted_seen": false,
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
	if _, err := tx.Exec(ctx, `INSERT INTO tasks (id, tenant, type, state, initiator, priority_class, idempotency_key, body_hash, body, record, deadline_at, created_at, updated_at)
		VALUES ($1, $2, $3, 'submitted', $4, $5, $6, $7, $8, $9, $10, $11, $11)`,
		id, in.Tenant, in.Type, in.Initiator, prio, in.IdempotencyKey, bodyHash, body, rec, deadline, now); err != nil {
		return nil, err
	}
	if estimate > 0 {
		if _, err := tx.Exec(ctx, `INSERT INTO spend (id, tenant, task, root, subject, line, unit, state, eur_micros, time)
			VALUES ($1, $2, $3, $3, $4, 'work', 'task', 'reserved', $5, $6)`, ids.New(ids.Spend), in.Tenant, id, in.Initiator, estimate, now); err != nil {
			return nil, err
		}
	}
	if err := c.appendEvent(ctx, tx, now, in.Tenant, Event{Type: "task.submitted", Subject: id, Data: map[string]any{"task": id, "type": in.Type}}); err != nil {
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

// checkBudget compares the money windows with the limits: the tenant's month, day and hour
// and the person's day limit from their groups (BUD-1). Reserves count until they are released.
// With a reserve of extra micro-euros it refuses when the reserve would pass a limit (BUD-2).
// The task reserve of the tree ownRoot is left out: a model call of that tree draws on it.
// The tenant's budget lock is held until the transaction ends, so two reserves of one tenant
// never both pass on the same old sum.
func (c *Core) checkBudget(ctx context.Context, tx pgx.Tx, now time.Time, tv *TenantView, subject string, groups []string, extra int64, ownRoot string) *Problem {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(7300102, hashtext($1))`, tv.Name); err != nil {
		return refuse("not_ready", "store")
	}
	over := func(used int64, limit float64) bool {
		if extra > 0 {
			return used+extra > money.FromEUR(limit)
		}
		return used >= money.FromEUR(limit)
	}
	b := tv.Spec.Budgets
	type window struct {
		limit *float64
		from  time.Time
	}
	// Days and months are the tenant's calendar ones.
	loc := time.UTC
	if l, err := time.LoadLocation(tv.Spec.Timezone); err == nil && tv.Spec.Timezone != "" {
		loc = l
	}
	local := now.In(loc)
	month := time.Date(local.Year(), local.Month(), 1, 0, 0, 0, 0, loc)
	day := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
	dayLimit := b.DayEUR
	if dayLimit == nil && b.MonthEUR != nil {
		v := *b.MonthEUR / 20
		dayLimit = &v
	}
	hourLimit := b.HourEUR
	if hourLimit == nil && b.MonthEUR != nil {
		v := *b.MonthEUR / 100
		hourLimit = &v
	}
	for _, w := range []window{{b.MonthEUR, month}, {dayLimit, day}, {hourLimit, now.Add(-time.Hour)}} {
		if w.limit == nil {
			continue
		}
		var used int64
		if err := tx.QueryRow(ctx, `SELECT coalesce(sum(eur_micros), 0) FROM spend WHERE tenant = $1 AND time >= $2 AND NOT (unit = 'task' AND root = $3)`, tv.Name, w.from, ownRoot).Scan(&used); err != nil {
			return refuse("not_ready", "store")
		}
		if over(used, *w.limit) {
			return refuse("budget_exhausted", "the tenant limit is used up")
		}
	}
	var personal *float64
	for _, g := range groups {
		if l := tv.Groups[g].DayEUR; l != nil && (personal == nil || *l > *personal) {
			personal = l
		}
	}
	if personal != nil && subject != "system" {
		var used int64
		if err := tx.QueryRow(ctx, `SELECT coalesce(sum(eur_micros), 0) FROM spend WHERE tenant = $1 AND subject = $2 AND time >= $3 AND NOT (unit = 'task' AND root = $4)`, tv.Name, subject, day, ownRoot).Scan(&used); err != nil {
			return refuse("not_ready", "store")
		}
		if over(used, *personal) {
			return refuse("budget_exhausted", "the personal day limit is used up")
		}
	}
	return nil
}
