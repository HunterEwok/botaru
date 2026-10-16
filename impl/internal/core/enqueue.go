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
	// Forwarded and Quoted mark a body that came as a forward or a quote: someone else's text
	// (INJ-13, AUD-5).
	Forwarded, Quoted bool
	// Subjects are the other persons whose data the body carries, for personal_subjects
	// (AUD-5): the authors of forwarded or quoted text.
	Subjects []string
	// Refs link the task to earlier work: the original of a redrive (DLV-4).
	Refs []string
	// Chain makes the task a child or a continuation in an existing tree (PRM-6, CNT-3).
	Chain *chainLink
	// CoInitiators are the initiators of what the enqueuing tree read, beside the initiator
	// (MSG-7); a chain carries its own.
	CoInitiators []string
	// Continues is the task this one continues: a reply or the follow-up window (CNT-5).
	Continues string
	// Untrusted carries untrusted_seen over from the continued task (CNT-5).
	Untrusted bool
	// ContinuesMark is the data mark of the continued task, whose outcome the new one carries.
	ContinuesMark string
	// EstimateMin is the least reserve of the task: a review reserves its type's tree budget,
	// the sum the review line was checked for (COM-6).
	EstimateMin int64
	// Wake makes the task a wake-up of a route's inbox (MSG-11).
	Wake *wakeInput
	// Sev1 marks a seat task of a sev1 incident: it may use the reserve of the manager line
	// (MGR-20).
	Sev1 bool
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

// coInitiatorsOf are the initiators of what the enqueuing tree read, other than the initiator.
func coInitiatorsOf(in enqueueInput) []string {
	var out []string
	all := slices.Clone(in.CoInitiators)
	if in.Chain != nil {
		all = append(all, in.Chain.CoInitiators...)
	}
	for _, id := range all {
		if id != in.Initiator && !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out
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
	// Analysis starts only from a schedule or a route, as system; an agent or a person does
	// not start it (INS-1).
	if p := tt.Purpose; (p == "insight" || p == "aggregate") && (in.Initiator != "system" || in.Source != "schedule" && in.Source != "route" || in.Chain != nil) {
		return nil, refuse("type_not_allowed", "analysis starts only from a schedule or a route")
	}
	// The analysis mark stays within insight.max_data of its role (INS-2): the lint refuses such
	// a set, and the core does not start one that got past it.
	if tt.Purpose == "insight" {
		for _, r := range cat.Roles {
			if r.Insight == nil || r.Insight.MaxData == "" || !slices.Contains(r.Insight.Types, in.Type) {
				continue
			}
			to, ok1 := c.markOrder(tt.DataClass)
			mo, ok2 := c.markOrder(r.Insight.MaxData)
			if !ok1 || !ok2 || to > mo {
				return nil, refuse("type_not_allowed", "the analysis class is above insight.max_data")
			}
		}
	}
	// A lens review is enqueued only by the proposal loop, as system (COM-2).
	if tt.Purpose == "review" && (in.Initiator != "system" || in.Source != reviewSource || in.Chain != nil) {
		return nil, refuse("type_not_allowed", "a review is enqueued only by the proposal loop")
	}
	// A farm manager seat task is enqueued only by the core, for its seat (MGR-3, MGR-42).
	if (tt.Purpose == "manage" || tt.Purpose == "inspect") && (in.Initiator != "system" || in.Source != seatSource || in.Chain != nil) {
		return nil, refuse("type_not_allowed", "a farm manager seat task is enqueued only by the core")
	}
	bodyHash, err := jcs.Hash(in.Body)
	if err != nil {
		return nil, err
	}
	// The key: the same initiator and body return the existing task (IDM-1).
	var existingID, existingInit, existingHash string
	var purged bool
	err = tx.QueryRow(ctx, `SELECT id, initiator, body_hash, body IS NULL FROM tasks WHERE tenant = $1 AND idempotency_key = $2`, in.Tenant, in.IdempotencyKey).
		Scan(&existingID, &existingInit, &existingHash, &purged)
	if err == nil {
		// A deleted task keeps its key as a tombstone: the key is never a new task (RET-3).
		if purged {
			return nil, refuse("gone", "the task of this key is deleted")
		}
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
	} else {
		// system enqueues with its own group only: the types of its schedules and routes
		// (PRM-10); a farm manager seat task with the tenant's consent group for the manager
		// (MGR-3).
		groups = groupClosure(tv, []string{"system-ops"})
		if in.Source == seatSource {
			groups = groupClosure(tv, []string{"farm-manager-ops"})
		}
	}
	capName := "task:" + in.Type
	if !hasCap(tv, groups, capName) {
		pr := refuse("no_group_cap", "the initiator has no %s", capName)
		pr.Needs = groupsWithCap(tv, capName)
		return reject(pr)
	}
	// In a tree the mark is not lower than the participants' marks (PRM-6, CNT-3).
	mark := tt.DataClass
	if in.Chain != nil {
		mark = c.higherMark(mark, in.Chain.Mark)
	}
	if in.ContinuesMark != "" {
		mark = c.higherMark(mark, in.ContinuesMark)
	}
	// An attached file must be a stored file of the tenant; the task takes its mark and is
	// untrusted from the start (ART-2, ART-3, TEN-7).
	files, err := attachedFiles(in.Body)
	if err != nil {
		return reject(refuse("input_schema_invalid", "attachments"))
	}
	for _, ref := range files {
		f, err := loadFile(ctx, tx, in.Tenant, ref)
		if err != nil {
			return nil, err
		}
		if f == nil || f.State != "stored" || (f.Expires != nil && !f.Expires.After(now)) {
			return reject(refuse("ref_invalid", "no such file"))
		}
		mark = c.higherMark(mark, f.Mark)
	}
	if !hasData(tv, groups, mark) {
		pr := refuse("no_data_class", "the initiator has no data:%s", mark)
		pr.Needs = []string{"data:" + mark}
		return reject(pr)
	}
	// The initiators of what the tree read narrow it as the initiator does: C2 intersects with
	// each of them (MSG-7).
	for _, id := range coInitiatorsOf(in) {
		cg, err := principalGroups(ctx, tx, now, tv, id)
		if err != nil {
			return nil, err
		}
		if !hasCap(tv, cg, capName) {
			pr := refuse("no_group_cap", "an initiator of what the tree read has no %s", capName)
			pr.Needs = groupsWithCap(tv, capName)
			return reject(pr)
		}
		if !hasData(tv, cg, mark) {
			pr := refuse("no_data_class", "an initiator of what the tree read has no data:%s", mark)
			pr.Needs = []string{"data:" + mark}
			return reject(pr)
		}
	}
	if in.Chain != nil {
		if p, err := c.checkChain(ctx, tx, tv, tt, in.Chain); err != nil {
			return nil, err
		} else if p != nil {
			return reject(p)
		}
	}
	// Money: the estimate of the task is reserved in the tenant windows and the person's day
	// limit, next to usage and open reserves (BUD-1). The reserve is written with the task below.
	estimate, err := taskEstimate(ctx, tx, now, in.Tenant, in.Type, tt)
	if err != nil {
		return nil, err
	}
	estimate = max(estimate, in.EstimateMin)
	line := c.budgetLine(tt.Purpose)
	var crossed []crossing
	if p := c.checkBudget(ctx, tx, now, tv, budgetAsk{subject: in.Initiator, groups: groups, extra: estimate, line: line, sev1: in.Sev1, crossed: &crossed}); p != nil {
		return reject(p)
	}
	// A wake-up reserves in the day limit of each principal the entries came from too (MSG-11).
	if in.Wake != nil {
		for _, sub := range in.Wake.Subjects {
			if sub == "system" || sub == in.Initiator {
				continue
			}
			sg, err := principalGroups(ctx, tx, now, tv, sub)
			if err != nil {
				return nil, err
			}
			if p := c.checkBudget(ctx, tx, now, tv, budgetAsk{subject: sub, groups: sg, extra: estimate, line: line}); p != nil {
				return reject(p)
			}
			if p, err := c.wakeShare(ctx, tx, now, tv, sub, sg, line, estimate, ""); err != nil {
				return nil, err
			} else if p != nil {
				return reject(p)
			}
		}
	}
	// A task in a wake-up's subtree draws on the wake-up's money: the inbox's day and the day
	// limits of the principals its entries came from (MSG-11, BUD-2).
	if in.Chain != nil && in.Chain.WakeTask != "" {
		if p, err := c.wakeLimits(ctx, tx, now, tv, in.Chain.WakeTask, estimate, line); err != nil {
			return nil, err
		} else if p != nil {
			return reject(p)
		}
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
	// A child, a continuation or a follow-up in a tree is not more urgent than its parent.
	if in.Chain != nil {
		prio = max(prio, in.Chain.ParentPriority)
	}
	// Below the soft threshold of the store volume only classes 1 and 2 enter (CAP-5).
	if prio >= 3 && c.capacity.Load() != capacityOK {
		pr := refuse("over_capacity", "the store volume is below its soft threshold")
		pr.RetryAfterS = 60
		return reject(pr)
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
		// So is the tree budget of the root type (GW-6).
		"tree_budget_micros": money.FromEUR(tt.TreeBudgetEUR),
		"trace":              trace,
	}
	// Someone else's text in the body: the task starts untrusted and with personal data seen
	// (INJ-13, AUD-5). A forward has no principal of its author, so no personal_subjects.
	var personal []string
	switch {
	case in.Forwarded:
		record["untrusted_seen"], record["untrusted_source"] = true, "forward"
		personal = append(personal, "forward")
	case in.Quoted:
		record["untrusted_seen"], record["untrusted_source"] = true, "quote"
	}
	if in.Quoted {
		personal = append(personal, "quote")
	}
	if len(personal) > 0 {
		record["personal_seen"] = personal
	}
	if len(in.Subjects) > 0 {
		record["personal_subjects"] = in.Subjects
	}
	if len(in.Refs) > 0 {
		record["refs"] = in.Refs
	}
	if in.Origin != nil {
		record["origin"] = in.Origin
		record["conversation"] = "c:" + in.Origin.Channel + ":" + in.Origin.Chat.ID()
		if in.Origin.Chat.Kind == "group" {
			// In a group each person has a conversation of their own, so a session never
			// carries one member's answers to another (PRM-9, DLV-15).
			record["conversation"] = "c:" + in.Origin.Channel + ":" + in.Origin.Chat.ID() + ":" + in.Initiator
		}
	}
	root := id
	if ch := in.Chain; ch != nil {
		root = ch.Root
		record["parent"], record["root"], record["correlationid"], record["depth"] = ch.Parent, ch.Root, ch.Root, ch.Depth
		if len(ch.CoInitiators) > 0 {
			record["co_initiators"] = ch.CoInitiators
		}
		record["data_mark"], record["max_tier"] = mark, min(tt.Tier, ch.MaxTier)
		if ch.Untrusted {
			record["untrusted_seen"] = true
		}
		if ch.Conversation != "" {
			record["conversation"] = ch.Conversation
		}
		if ch.Trace != "" {
			record["trace"] = ch.Trace
		}
		if ch.WakeTask != "" {
			record["wake_task"] = ch.WakeTask
		}
		// The tree draws on the one limit pinned in its root (PRM-6, BUD-1).
		var rootType string
		var rootRec []byte
		if err := tx.QueryRow(ctx, `SELECT type, record FROM tasks WHERE id = $1`, ch.Root).Scan(&rootType, &rootRec); err != nil {
			return nil, err
		}
		record["tree_budget_micros"] = taskTreeBudget(rootRec, cat.TaskTypes[rootType])
	}
	if w := in.Wake; w != nil {
		record["wake_inbox"], record["depth"] = w.Inbox, w.Depth
		// The wake-up heads a subtree of its own: the spend of its children counts in its
		// money (MSG-11).
		record["wake_task"] = id
		// The wake-up joins the tree its oldest entry came from: the tree's limits and budget
		// stay the ones of its root (MSG-12, BUD-1, PRM-6).
		if w.Correlation != "" {
			record["correlationid"] = w.Correlation
			var rootType string
			var rootRec []byte
			err := tx.QueryRow(ctx, `SELECT type, record FROM tasks WHERE id = $1 AND tenant = $2`, w.Correlation, in.Tenant).Scan(&rootType, &rootRec)
			switch {
			case err == nil:
				// It is one more task of that tree, within the tree's task limit (PRM-6).
				limit := defaultMaxTreeTasks
				if m := tv.Spec.Campaigns.MaxTreeTasks; m > 0 {
					limit = m
				}
				var n int
				if err := tx.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE tenant = $1 AND (id = $2 OR record->>'root' = $2)`, in.Tenant, w.Correlation).Scan(&n); err != nil {
					return nil, err
				}
				if n >= min(limit, farmMaxTreeTasks) {
					return reject(refuse("tree_too_large", "the tree of this root has %d tasks", n))
				}
				root = w.Correlation
				record["root"] = root
				record["tree_budget_micros"] = taskTreeBudget(rootRec, cat.TaskTypes[rootType])
			case !errors.Is(err, pgx.ErrNoRows):
				return nil, err
			}
		}
		// The initiators the entries came from narrow what the wake-up's tree may enqueue, as
		// the co-initiators of a tree that read them (MSG-7).
		var co []string
		for _, sub := range w.Subjects {
			if sub != "system" && sub != in.Initiator && !slices.Contains(co, sub) {
				co = append(co, sub)
			}
		}
		if len(co) > 0 {
			record["co_initiators"], record["wake_subjects"] = co, co
		}
	}
	if in.Continues != "" {
		record["data_mark"] = mark
		record["continues"] = in.Continues
		record["refs"] = append(slices.Clone(in.Refs), in.Continues)
	}
	if in.ContinuesMark != "" {
		record["data_mark"] = mark
	}
	if len(files) > 0 {
		record["data_mark"] = mark
		record["untrusted_seen"] = true
		if record["untrusted_source"] == nil {
			record["untrusted_source"] = "attachment"
		}
	}
	if in.Untrusted {
		// The continued task saw untrusted text: so does its follow-up (CNT-5).
		record["untrusted_seen"] = true
		if record["untrusted_source"] == nil {
			record["untrusted_source"] = "continues"
		}
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
	// A wake-up starts no later than the latest expiry of its entries (MSG-12).
	if w := in.Wake; w != nil && w.StartBy != nil {
		if t, ok := startBy.(time.Time); !ok || w.StartBy.Before(t) {
			startBy = *w.StartBy
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO tasks (id, tenant, type, state, initiator, priority_class, idempotency_key, body_hash, body, record, deadline_at, start_by_at, created_at, updated_at)
		VALUES ($1, $2, $3, 'submitted', $4, $5, $6, $7, $8, $9, $10, $11, $12, $12)`,
		id, in.Tenant, in.Type, in.Initiator, prio, in.IdempotencyKey, bodyHash, body, rec, deadline, startBy, now); err != nil {
		return nil, err
	}
	if estimate > 0 {
		if _, err := tx.Exec(ctx, `INSERT INTO spend (id, tenant, task, root, subject, line, purpose, unit, state, eur_micros, time)
			VALUES ($1, $2, $3, $9, $4, $5, $6, 'task', 'reserved', $7, $8)`, ids.New(ids.Spend), in.Tenant, id, in.Initiator, line, purposeOf(tt), estimate, now, root); err != nil {
			return nil, err
		}
	}
	submitted := map[string]any{"task": id, "type": in.Type}
	if in.Chain != nil {
		submitted["parent"] = in.Chain.Parent
	}
	if err := c.appendEvent(ctx, tx, now, in.Tenant, Event{Type: "task.submitted", Subject: id, Data: submitted, Causation: in.Cause}); err != nil {
		return nil, err
	}
	if _, err := c.appendAudit(ctx, tx, now, Audit{
		Tenant: in.Tenant, Kind: "authorize", Actor: Actor{Principal: in.Initiator, Via: viaOf(in.EnqueuedBy)},
		Command: "task.enqueue", Target: &Target{Kind: "task", ID: id}, OK: true,
		After: map[string]any{"type": in.Type, "decision": "allow"},
	}); err != nil {
		return nil, err
	}
	// The thresholds the accepted task reaches (BUD-4).
	if err := c.emitCrossed(ctx, tx, now, in.Tenant, crossed); err != nil {
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
	// A lens review carries only the input block the core builds (COM-3); its type declares
	// no parameters of its own.
	if tt.Purpose == "review" || tt.Purpose == "manage" || tt.Purpose == "inspect" {
		m, _ := params.(map[string]any)
		if _, ok := m["input"].(map[string]any); !ok || len(m) != 1 {
			return errors.New("a review or a seat task carries only the core's input block")
		}
		return nil
	}
	if tt.ParamsSchema == nil {
		return nil
	}
	return c.rs.validate(tt.ParamsSchema, params)
}

// taskEstimate is the enqueue estimate of a task (BUD-14): the 90th percentile of the price of
// the whole tree of the finished tasks of the type in the tenant over the last 30 days (of a
// wake-up its own subtree, and of a root its tree without the wake-ups that joined it), and
// without such tasks the worst case, the type's tree budget (GW-6). A type without a tree
// budget and without history has no estimate; P4 still reserves every call (BUD-2).
func taskEstimate(ctx context.Context, tx pgx.Tx, now time.Time, tenant, typ string, tt manifest.TaskType) (int64, error) {
	var n int
	var p90 float64
	err := tx.QueryRow(ctx, `SELECT count(*), coalesce(percentile_cont(0.9) WITHIN GROUP (ORDER BY cost), 0) FROM (
		SELECT (SELECT coalesce(sum(s.eur_micros), 0) FROM spend s LEFT JOIN tasks u ON u.id = s.task
			WHERE s.unit <> 'task' AND CASE WHEN t.record ? 'wake_inbox' THEN s.root = coalesce(t.record->>'root', t.id) AND u.record->>'wake_task' = t.id
				ELSE s.root = t.id AND (u.record->>'wake_task') IS NULL END) AS cost
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

// attachedFiles are the file_refs of a task body's attachments.
func attachedFiles(body map[string]any) ([]string, error) {
	raw, ok := body["attachments"]
	if !ok {
		return nil, nil
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, errors.New("attachments is not a list")
	}
	var out []string
	for _, a := range list {
		m, _ := a.(map[string]any)
		if ref, ok := m["file_ref"].(string); ok {
			out = append(out, ref)
		}
	}
	return out, nil
}

// wakeShare refuses a wake-up, or a reserve in its subtree, past the personal day limit of a principal its entries came from:
// the wake-ups their entries caused this day, spent and still reserved, count in that limit
// beside their own spending, though the wake-ups run as system (MSG-11, BUD-1).
func (c *Core) wakeShare(ctx context.Context, tx pgx.Tx, now time.Time, tv *TenantView, sub string, groups []string, line string, estimate int64, ownWake string) (*Problem, error) {
	if line != "" && line != "work" {
		return nil, nil
	}
	var personal *float64
	for _, g := range groups {
		if l := tv.Groups[g].DayEUR; l != nil && (personal == nil || *l > *personal) {
			personal = l
		}
	}
	if personal == nil {
		return nil, nil
	}
	day := tenantDay(tv, now)
	own, err := windowUsed(ctx, tx, tv.Name, spendFilter{subject: sub, line: "work"}, day, "")
	if err != nil {
		return nil, err
	}
	wakes, err := wakeSum(ctx, tx, tv.Name, day, ownWake, "subject", sub)
	if err != nil {
		return nil, err
	}
	if own+wakes+estimate > money.FromEUR(*personal) {
		return refuse("budget_exhausted", "the personal day limit is used up"), nil
	}
	return nil, nil
}
