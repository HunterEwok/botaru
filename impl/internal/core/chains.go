// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/jcs"
	"github.com/HunterEwok/botaru/impl/internal/manifest"
	"github.com/HunterEwok/botaru/impl/internal/mcp"
	"github.com/HunterEwok/botaru/impl/internal/toolspec"
)

const (
	enqueueChildName = "enqueue_child"
	// maxDepth is the depth limit of a task tree (PRM-6).
	maxDepth = 5
	// defaultMaxChildren is the limit of direct children of a task without max_children.
	defaultMaxChildren = 10
	// Tasks from one root: the tenant default and the farm ceiling (PRM-6, ORC-8).
	defaultMaxTreeTasks = 300
	farmMaxTreeTasks    = 1000
)

// chainLink ties a child or a continuation to its tree (PRM-6, CNT-3). The core fills it from
// the records of the tree; the model sets only the type, the goal and the params.
type chainLink struct {
	Parent, Root string
	Depth        int
	// Mark is the floor of the data mark: the highest mark of the participants.
	Mark string
	// MaxTier is the ceiling of the tier: a child is not above its parent.
	MaxTier      int
	Untrusted    bool
	Conversation string
	Trace        string
}

// treeRecord is the part of a task record a chain reads.
type treeRecord struct {
	Parent        string  `json:"parent"`
	Root          string  `json:"root"`
	Depth         int     `json:"depth"`
	MaxTier       int     `json:"max_tier"`
	DataMark      string  `json:"data_mark"`
	UntrustedSeen bool    `json:"untrusted_seen"`
	Conversation  string  `json:"conversation"`
	Origin        *Origin `json:"origin"`
	EnqueuedBy    string  `json:"enqueued_by"`
	Trace         string  `json:"trace"`
}

// child says whether the task is a child of enqueue_child: a task with a parent that a worker
// enqueued. A continuation has a parent too, but the core enqueues it.
func (r treeRecord) child() bool { return r.Parent != "" && strings.HasPrefix(r.EnqueuedBy, "worker:") }

// reportsToOrigin says whether the outcome of a task goes to the person in origin: a child
// answers its parent, through a continuation, and never the person directly (CNT-1).
func (r treeRecord) reportsToOrigin() bool { return r.Origin != nil && !r.child() }

// higherMark is the higher of two data marks by the order of the data classes.
func (c *Core) higherMark(a, b string) string {
	oa, okA := c.markOrder(a)
	ob, okB := c.markOrder(b)
	switch {
	case !okA:
		return b
	case !okB:
		return a
	case ob > oa:
		return b
	}
	return a
}

// childTypes are the types the attempt may enqueue as children: the role's may_enqueue,
// without action types, that every initiator may enqueue (PRM-6, ORC-13).
func childTypes(cat *Catalog, g *toolsGrant) []string {
	role, ok := cat.Roles[g.Role]
	tv := cat.Tenants[g.Tenant]
	if !ok || tv == nil || role.Profile != "manager" || g.Purpose != "" && g.Purpose != "work" {
		return nil
	}
	groups := groupClosure(tv, g.InitiatorGroups)
	var out []string
	for _, t := range role.MayEnqueue {
		if _, ok := cat.TaskTypes[t]; ok && !strings.HasPrefix(t, "action.") && hasCap(tv, groups, "task:"+t) {
			out = append(out, t)
		}
	}
	sort.Strings(out)
	return out
}

// enqueueChildTool is the built-in enqueue_child of a manager role, or nil when the attempt
// may enqueue no child (PRF-1). The type is an enum of what the role and the initiator allow;
// the description of each type is the summary a person wrote for managers (ORC-13).
func (c *Core) enqueueChildTool(cat *Catalog, g *toolsGrant) *catalogTool {
	types := childTypes(cat, g)
	if len(types) == 0 {
		return nil
	}
	var desc strings.Builder
	desc.WriteString("Enqueue a child task. The child works on its own; its result comes back as a continuation task if this type declares one. Types:")
	enum := make([]any, 0, len(types))
	for _, t := range types {
		enum = append(enum, t)
		tt := cat.TaskTypes[t]
		summary := tt.SummaryForManagers
		if summary == "" {
			summary = tt.Title
		}
		fmt.Fprintf(&desc, "\n- %s: %s", t, strings.TrimSpace(summary))
	}
	schema := map[string]any{
		"type": "object", "additionalProperties": false, "required": []any{"type", "goal"},
		"properties": map[string]any{
			"type":   map[string]any{"enum": enum},
			"goal":   map[string]any{"type": "string", "minLength": 1, "maxLength": 4000},
			"params": map[string]any{"type": "object", "maxProperties": 32},
		},
	}
	return &catalogTool{Name: enqueueChildName, Builtin: true, Tool: toolspec.Tool{Description: desc.String()}, Schema: schema}
}

// childResult is the answer of enqueue_child: TaskRef or a refusal code.
type childResult struct {
	Code     string   `json:"code"`
	ID       string   `json:"id,omitempty"`
	Existing bool     `json:"existing,omitempty"`
	Needs    []string `json:"needs,omitempty"`
}

func (r childResult) call() *mcp.CallResult {
	b, _ := json.Marshal(r)
	var sc map[string]any
	_ = json.Unmarshal(b, &sc)
	return &mcp.CallResult{IsError: r.Code != "enqueued", StructuredContent: sc, Content: []mcp.Content{{Type: "text", Text: string(b)}}}
}

// loadTree reads the tree part of a task record.
func loadTree(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, id string) (treeRecord, error) {
	var raw []byte
	var r treeRecord
	if err := q.QueryRow(ctx, `SELECT record FROM tasks WHERE id = $1`, id).Scan(&raw); err != nil {
		return r, err
	}
	err := json.Unmarshal(raw, &r)
	if r.Root == "" {
		r.Root = id
	}
	return r, err
}

// enqueueChild is P3 enqueue_child (PRM-6): a child of a type from may_enqueue, recorded at the
// call with the key system:child:<parent>:<body hash>, so a retried attempt does not
// duplicate it (IDM-2). Everything but the type, the goal and the params comes from the
// parent; the enqueue checks of the initiator, the holds and the budget apply as for any task.
func (c *Core) enqueueChild(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, g *toolsGrant, offered bool, args map[string]any) (childResult, error) {
	typ, _ := args["type"].(string)
	goal, _ := args["goal"].(string)
	audit := func(ok bool, reason, child string) error {
		after := map[string]any{"type": typ}
		if child != "" {
			after["task"] = child
		}
		_, err := c.appendAudit(ctx, tx, now, Audit{
			Tenant: g.Tenant, Kind: "authorize", Actor: Actor{Principal: workerPrincipal(g.Worker), Via: "bot", Role: g.Role, Task: g.Task},
			Command: "task.enqueue_child", Target: &Target{Kind: "task_type", ID: typ}, OK: ok, Reason: reason, After: after,
		})
		return err
	}
	deny := func(code string, needs ...string) (childResult, error) {
		if err := audit(false, code, ""); err != nil {
			return childResult{}, err
		}
		return childResult{Code: code, Needs: needs}, nil
	}
	if !offered || !containsString(childTypes(cat, g), typ) {
		return deny("type_not_allowed")
	}
	if goal == "" || len(goal) > 4000 {
		return deny("input_schema_invalid")
	}
	body := map[string]any{"type": typ, "goal": goal}
	if p, ok := args["params"].(map[string]any); ok {
		body["params"] = p
	} else if args["params"] != nil {
		return deny("input_schema_invalid")
	}
	parent, err := loadTree(ctx, tx, g.Task)
	if err != nil {
		return childResult{}, err
	}
	parentType := cat.TaskTypes[g.Type]
	maxChildren := defaultMaxChildren
	if parentType.MaxChildren != nil {
		maxChildren = *parentType.MaxChildren
	}
	hash, err := jcs.Hash(body)
	if err != nil {
		return childResult{}, err
	}
	key := "system:child:" + g.Task + ":" + hash
	// A repeat of the same call is the same child, even at the limit.
	var existing string
	err = tx.QueryRow(ctx, `SELECT id FROM tasks WHERE tenant = $1 AND idempotency_key = $2`, g.Tenant, key).Scan(&existing)
	if err == nil {
		return childResult{Code: "enqueued", ID: existing, Existing: true}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return childResult{}, err
	}
	// The children of one task are counted under its row lock.
	if _, err := tx.Exec(ctx, `SELECT 1 FROM tasks WHERE id = $1 FOR UPDATE`, g.Task); err != nil {
		return childResult{}, err
	}
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE tenant = $1 AND record->>'parent' = $2 AND record->>'enqueued_by' LIKE 'worker:%'`, g.Tenant, g.Task).Scan(&n); err != nil {
		return childResult{}, err
	}
	if n >= maxChildren {
		return deny("tree_too_large")
	}
	res, err := c.enqueue(ctx, tx, now, cat, enqueueInput{
		Tenant: g.Tenant, Type: typ, Body: body, IdempotencyKey: key, Initiator: g.Initiator,
		EnqueuedBy: "worker:" + g.Worker, Origin: parent.Origin, FairnessKey: fairnessOf(ctx, tx, g.Task),
		Chain: &chainLink{
			Parent: g.Task, Root: parent.Root, Depth: parent.Depth + 1, Mark: parent.DataMark, MaxTier: parent.MaxTier,
			Untrusted: parent.UntrustedSeen, Conversation: parent.Conversation, Trace: parent.Trace,
		},
	})
	if err != nil {
		if p, ok := AsProblem(err); ok {
			return deny(p.Reason, p.Needs...)
		}
		return childResult{}, err
	}
	if res.Rejected != nil {
		return childResult{Code: res.Rejected.Reason, Needs: res.Rejected.Needs}, nil
	}
	if err := audit(true, "", res.TaskID); err != nil {
		return childResult{}, err
	}
	return childResult{Code: "enqueued", ID: res.TaskID, Existing: res.Existing}, nil
}

// fairnessOf is the fairness key of a task, which its children and continuations keep (DSP-2).
func fairnessOf(ctx context.Context, tx pgx.Tx, task string) string {
	var k *string
	_ = tx.QueryRow(ctx, `SELECT record->>'fairness_key' FROM tasks WHERE id = $1`, task).Scan(&k)
	return deref(k)
}

// checkChain applies the tree limits of PRM-6 to a child or a continuation before it is
// written: depth, tier, mark and the number of tasks of the root.
func (c *Core) checkChain(ctx context.Context, tx pgx.Tx, tv *TenantView, tt manifest.TaskType, ch *chainLink) (*Problem, error) {
	if ch.Depth > maxDepth {
		return refuse("depth_exceeded", "the task tree is deeper than %d", maxDepth), nil
	}
	if tt.Tier > ch.MaxTier {
		return refuse("tier_exceeded", "the child's tier is above its parent's"), nil
	}
	limit := defaultMaxTreeTasks
	if m := tv.Spec.Campaigns.MaxTreeTasks; m > 0 {
		limit = m
	}
	limit = min(limit, farmMaxTreeTasks)
	// The tasks of one root are counted under the root's row lock.
	if _, err := tx.Exec(ctx, `SELECT 1 FROM tasks WHERE id = $1 FOR UPDATE`, ch.Root); err != nil {
		return nil, err
	}
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE tenant = $1 AND (id = $2 OR record->>'root' = $2)`, tv.Name, ch.Root).Scan(&n); err != nil {
		return nil, err
	}
	if n >= limit {
		return refuse("tree_too_large", "the tree of this root has %d tasks", n), nil
	}
	return nil, nil
}

// replyContinuation enqueues the reply continuation of a child that ended, in the transaction
// of its end (CNT-1, CNT-2, CNT-3): the type and the goal come from the parent type's
// continuation in git, the key is system:cont:reply:<child>, and the continuation copies the
// initiator, origin and conversation, is the parent's child at the child's depth plus 1, with
// the highest mark and untrusted_seen of the participants. The child's result is an attachment.
// If it cannot be enqueued, an event says why and the person in origin gets a code text.
func (c *Core) replyContinuation(ctx context.Context, tx pgx.Tx, now time.Time, tenant, child, state string) error {
	cat, err := c.Catalog(ctx)
	if err != nil {
		return err
	}
	src, err := loadTree(ctx, tx, child)
	if err != nil {
		return err
	}
	if !src.child() {
		return nil
	}
	var parentType, initiator string
	var parentState string
	if err := tx.QueryRow(ctx, `SELECT type, initiator, state FROM tasks WHERE id = $1`, src.Parent).Scan(&parentType, &initiator, &parentState); err != nil {
		return err
	}
	cont := cat.TaskTypes[parentType].Continuation
	if cont == nil || cont.Type == "" {
		return nil
	}
	parent, err := loadTree(ctx, tx, src.Parent)
	if err != nil {
		return err
	}
	tv := cat.Tenants[tenant]
	refused := func(reason string) error {
		if err := c.appendEvent(ctx, tx, now, tenant, Event{Type: "continuation.refused", Subject: child,
			Data: map[string]any{"task": child, "trigger": "reply", "type": cont.Type, "reason": reason}}); err != nil {
			return err
		}
		o := src.Origin
		if o == nil {
			return nil
		}
		_, err := c.planDelivery(ctx, tx, now, plannedDelivery{
			Tenant: tenant, Task: child, Kind: "task_result", Addr: "origin", TargetKind: "chat", TargetID: o.Chat.ID(),
			ExecKind: "channel", ExecName: o.Channel, Recipient: initiator, DataOrigin: "code",
			Body: map[string]any{"form": "text_code", "text_key": "task.continuation_refused", "params": map[string]any{"reason": reason}},
		})
		return err
	}
	// A canceled tree has no continuation.
	if parentState == "canceled" {
		return refused("already_terminal")
	}
	if tv == nil {
		return nil
	}
	goal := cont.Goal
	if goal == "" {
		goal = "Continue with the outcome of the child task."
	}
	var result []byte
	if err := tx.QueryRow(ctx, `SELECT result FROM tasks WHERE id = $1`, child).Scan(&result); err != nil {
		return err
	}
	data := map[string]any{"task": child, "state": state}
	if len(result) > 0 && state == "completed" {
		var r map[string]any
		if json.Unmarshal(result, &r) == nil && len(r) > 0 && len(r) <= 60 {
			data["result"] = r
		}
	}
	body := map[string]any{"type": cont.Type, "goal": goal, "attachments": []any{map[string]any{"data": data}}}
	res, err := c.enqueue(ctx, tx, now, cat, enqueueInput{
		Tenant: tenant, Type: cont.Type, Body: body, IdempotencyKey: "system:cont:reply:" + child, Initiator: initiator,
		EnqueuedBy: "core", Origin: src.Origin, FairnessKey: fairnessOf(ctx, tx, child), Cause: child,
		Chain: &chainLink{
			Parent: src.Parent, Root: src.Root, Depth: src.Depth + 1, Mark: c.higherMark(parent.DataMark, src.DataMark),
			MaxTier: parent.MaxTier, Untrusted: parent.UntrustedSeen || src.UntrustedSeen, Conversation: src.Conversation, Trace: src.Trace,
		},
	})
	if err != nil {
		if p, ok := AsProblem(err); ok {
			return refused(p.Reason)
		}
		return err
	}
	if res.Rejected != nil {
		return refused(res.Rejected.Reason)
	}
	return nil
}
