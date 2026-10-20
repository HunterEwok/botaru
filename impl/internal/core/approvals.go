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

	"github.com/HunterEwok/botaru/impl/internal/ids"
	"github.com/HunterEwok/botaru/impl/internal/jcs"
	"github.com/HunterEwok/botaru/impl/internal/manifest"
	"github.com/HunterEwok/botaru/impl/internal/mcp"
	"github.com/HunterEwok/botaru/impl/internal/store"
	"github.com/HunterEwok/botaru/impl/internal/toolspec"
)

// Action approvals (spec/lifecycles/approval.md). The model only asks, through the built-in P3
// tool request_action (APR-1, APR-2); only a person decides, through P1 decide from the chat
// the card went to (APR-4); an approved action is executed at most once by one write call from
// the snapshot, inside an action.<a> task the core creates directly in working (APR-5, APR-7,
// CNT-7).
// This file holds the approval records and request_action; the cards, the loop, the decision
// and the execution are in the files approval*.go.

const (
	// requestActionName is the built-in tool of the model (APR-1).
	requestActionName = "request_action"
	// maxActionRequests is the ceiling of approvals per task (APR-2).
	maxActionRequests = 3
	// defaultCardsPerHour is attention.cards_per_tenant_hour when the tenant sets none (APR-2).
	defaultCardsPerHour = 20
	// remindEvery is how often the initiator may remind the approvers (APR-19).
	remindEvery = 5 * time.Minute
	// approvalCard and initiatorCopy are the rows of spec/model/decision-points.yaml.
	approvalCard  = "approval_card"
	initiatorCopy = "approval_initiator_copy"
	cardUpdate    = "card_update"
)

// approvalRow is an approval with the fields the core reads.
type approvalRow struct {
	ID, Tenant, SourceTask, Initiator, Action, ArgsHash, State, Nonce string
	Args                                                              map[string]any
	Risk                                                              string
	RequestedBy                                                       map[string]any
	UntrustedSeen                                                     bool
	UntrustedSource                                                   string
	Context                                                           map[string]any
	ExpiresAt                                                         time.Time
	RemindedAt, RemindAt, CardsAt, DecidedAt                          *time.Time
	DecidedBy, Decision, Reason, ActionTask, Toolcall                 string
	CreatedAt                                                         time.Time
	// Mark is the data mark the arguments were checked with at the request; execution checks
	// them again with the same mark (MAN-10).
	Mark string
	// Approvals is how many different people approve, fixed from the action at the request
	// (APR-16); Votes are the presses so far.
	Approvals int
	Votes     []approvalVote
	// PausedSince is when the current pause under a pause_action hold began; PausedTotalS is
	// how long the approval was pending under such holds before it (HLD-8).
	PausedSince  *time.Time
	PausedTotalS int64
}

// approvalVote is one press of an approval with approvals: 2 (APR-16).
type approvalVote struct {
	By       string `json:"by"`
	Decision string `json:"decision"`
	At       string `json:"at"`
}

// approvalRecordJSON is the immutable part of the approval record kept in approvals.record.
type approvalRecordJSON struct {
	RequestedBy     map[string]any `json:"requested_by"`
	Args            map[string]any `json:"args"`
	Risk            string         `json:"risk"`
	UntrustedSeen   bool           `json:"untrusted_seen"`
	UntrustedSource string         `json:"untrusted_source,omitempty"`
	Context         map[string]any `json:"context,omitempty"`
	DataMark        string         `json:"data_mark,omitempty"`
	Approvals       int            `json:"approvals,omitempty"`
}

const approvalColumns = `id, tenant, coalesce(source_task, ''), initiator, action, args_hash, state, nonce, record, expires_at,
	reminded_at, remind_at, cards_at, decided_at, coalesce(decided_by, ''), coalesce(decision, ''), coalesce(reason, ''),
	coalesce(action_task, ''), coalesce(toolcall, ''), created_at, votes, paused_since, paused_total_s`

func scanApproval(row pgx.Row) (*approvalRow, error) {
	var a approvalRow
	var rec, votes []byte
	if err := row.Scan(&a.ID, &a.Tenant, &a.SourceTask, &a.Initiator, &a.Action, &a.ArgsHash, &a.State, &a.Nonce, &rec, &a.ExpiresAt,
		&a.RemindedAt, &a.RemindAt, &a.CardsAt, &a.DecidedAt, &a.DecidedBy, &a.Decision, &a.Reason, &a.ActionTask, &a.Toolcall, &a.CreatedAt,
		&votes, &a.PausedSince, &a.PausedTotalS); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(votes, &a.Votes); err != nil {
		return nil, err
	}
	var r approvalRecordJSON
	if err := json.Unmarshal(rec, &r); err != nil {
		return nil, err
	}
	a.Args, a.Risk, a.RequestedBy, a.UntrustedSeen, a.UntrustedSource, a.Context = r.Args, r.Risk, r.RequestedBy, r.UntrustedSeen, r.UntrustedSource, r.Context
	a.Mark, a.Approvals = r.DataMark, max(r.Approvals, 1)
	return &a, nil
}

func loadApproval(ctx context.Context, q store.Querier, tenant, id string, lock bool) (*approvalRow, error) {
	sql := `SELECT ` + approvalColumns + ` FROM approvals WHERE id = $1 AND tenant = $2`
	if lock {
		sql += ` FOR UPDATE`
	}
	return scanApproval(q.QueryRow(ctx, sql, id, tenant))
}

// Record is the approval as spec/model/approval.schema.json describes it.
func (a *approvalRow) Record() map[string]any {
	r := map[string]any{
		"id": a.ID, "tenant": a.Tenant, "initiator": a.Initiator, "requested_by": a.RequestedBy, "action": a.Action,
		"args": a.Args, "args_hash": a.ArgsHash, "nonce": a.Nonce, "risk": a.Risk, "approvals": max(a.Approvals, 1),
		"untrusted_seen": a.UntrustedSeen, "expires_at": fmtTime(a.ExpiresAt), "state": a.State,
	}
	set := func(k, v string) {
		if v != "" {
			r[k] = v
		}
	}
	set("source_task", a.SourceTask)
	set("untrusted_source", a.UntrustedSource)
	set("decided_by", a.DecidedBy)
	set("action_task", a.ActionTask)
	set("toolcall", a.Toolcall)
	if len(a.Context) > 0 {
		r["context"] = a.Context
	}
	if a.RemindedAt != nil {
		r["reminded_at"] = fmtTime(*a.RemindedAt)
	}
	if a.DecidedAt != nil {
		r["decided_at"] = fmtTime(*a.DecidedAt)
	}
	if len(a.Votes) > 0 {
		r["votes"] = a.Votes
	}
	switch a.State {
	case "canceled":
		set("cancel_reason", a.Reason)
	case "exec_failed", "exec_unknown", "expired":
		set("reason", a.Reason)
	}
	return r
}

// actionArgsSchema is the closed argument schema of an action for a tenant and a data mark:
// enum_from becomes the catalog values filtered by the mark (MAN-9, MAN-10).
func (c *Core) actionArgsSchema(cat *Catalog, tenant, mark string, a manifest.Action) (map[string]any, error) {
	names := map[string]bool{}
	enumFromNames(a.Args, names)
	return c.fullSchema(a.Args, c.resolveEnumFor(cat, tenant, mark, "", names))
}

// actionDefaults fills the defaults of the action arguments that a request left out, so the
// card shows what will run (MAN-9).
func actionDefaults(a manifest.Action, args map[string]any) map[string]any {
	out := make(map[string]any, len(args))
	for k, v := range args {
		out[k] = v
	}
	props, _ := a.Args["properties"].(map[string]any)
	for k, s := range props {
		sm, _ := s.(map[string]any)
		if d, ok := sm["default"]; ok {
			if _, have := out[k]; !have {
				out[k] = d
			}
		}
	}
	return out
}

// actionTTL is the card deadline: low and routine 60 minutes unless the manifest sets a longer
// ttl, high 30 minutes unless the manifest sets one (APR-19).
func actionTTL(a manifest.Action) time.Duration {
	ttl, ok := parseDuration(a.TTL)
	switch a.Risk {
	case "high":
		if ok {
			return ttl
		}
		return 30 * time.Minute
	default:
		if ok && ttl > time.Hour {
			return ttl
		}
		return time.Hour
	}
}

// requestableActions are the actions the model of an attempt may ask for: in the role's
// actions_may_request, in the catalog, and with action.request:<a> for the initiator (APR-1).
func requestableActions(cat *Catalog, g *toolsGrant) []string {
	role, ok := cat.Roles[g.Role]
	tv := cat.Tenants[g.Tenant]
	if !ok || tv == nil || g.Purpose != "" && g.Purpose != "work" {
		return nil
	}
	if p := role.Profile; p != "" && p != "worker" && p != "manager" {
		return nil
	}
	groups := groupClosure(tv, g.InitiatorGroups)
	var out []string
	for _, a := range role.ActionsMayRequest {
		if _, ok := cat.Actions[a]; ok && hasCap(tv, groups, "action.request:"+a) && g.coHave(tv, "action.request:"+a) {
			out = append(out, a)
		}
	}
	sort.Strings(out)
	return out
}

// requestActionTool is the built-in tool of step 7 of the catalog assembly, or nil when the
// attempt may request no action (APR-1, PRF-1). Its args hold, as branches of anyOf, the closed
// argument schema of each action with the catalog values of the task's mark as enum (APR-2).
// The pairing of action and args is checked by requestAction: the Messages API refuses oneOf,
// allOf and anyOf at the top level of a tool's input schema.
func (c *Core) requestActionTool(cat *Catalog, g *toolsGrant) (*catalogTool, error) {
	actions := requestableActions(cat, g)
	if len(actions) == 0 {
		return nil, nil
	}
	var argsSchemas []any
	var desc strings.Builder
	desc.WriteString("Ask for an action of the farm catalog. A person decides on a card; you get the request number and do not wait for the decision. Actions:")
	names := make([]any, 0, len(actions))
	for _, name := range actions {
		a := cat.Actions[name]
		schema, err := c.actionArgsSchema(cat, g.Tenant, g.Mark, a)
		if err != nil {
			return nil, err
		}
		names = append(names, name)
		argsSchemas = append(argsSchemas, schema)
		fmt.Fprintf(&desc, "\n- %s: %s", name, strings.TrimSpace(a.Description))
	}
	schema := map[string]any{
		"type": "object", "additionalProperties": false, "required": []any{"action", "args"},
		"properties": map[string]any{
			"action": map[string]any{"enum": names},
			"args":   map[string]any{"type": "object", "maxProperties": 32, "anyOf": argsSchemas},
		},
	}
	return &catalogTool{Name: requestActionName, Builtin: true, Tool: toolspec.Tool{Description: desc.String()}, Schema: schema}, nil
}

// requestActionResult is request-action-result.
type requestActionResult struct {
	Code       string   `json:"code"`
	ApprovalID string   `json:"approval_id,omitempty"`
	Needs      []string `json:"needs,omitempty"`
}

func (r requestActionResult) call() *mcp.CallResult {
	b, _ := json.Marshal(r)
	var sc map[string]any
	_ = json.Unmarshal(b, &sc)
	return &mcp.CallResult{IsError: r.Code != "pending", StructuredContent: sc, Content: []mcp.Content{{Type: "text", Text: string(b)}}}
}

// requestAction is P3 request_action: it creates a pending approval or refuses with a code
// (APR-1, APR-2). The audit gets the full reason; the model sees the short code (PRM-8).
func (c *Core) requestAction(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, g *toolsGrant, offered bool, args map[string]any) (requestActionResult, error) {
	tv := cat.Tenants[g.Tenant]
	name, _ := args["action"].(string)
	actArgs, _ := args["args"].(map[string]any)
	audit := func(ok bool, reason, approval string) error {
		after := map[string]any{"action": name}
		if approval != "" {
			after["approval"] = approval
		}
		_, err := c.appendAudit(ctx, tx, now, Audit{
			Tenant: g.Tenant, Kind: "authorize", Actor: Actor{Principal: workerPrincipal(g.Worker), Via: "bot", Role: g.Role, Task: g.Task},
			Command: "action.request", Target: &Target{Kind: "action", ID: name}, OK: ok, Reason: reason, After: after,
		})
		return err
	}
	deny := func(code, reason string, needs ...string) (requestActionResult, error) {
		if err := audit(false, reason, ""); err != nil {
			return requestActionResult{}, err
		}
		return requestActionResult{Code: code, Needs: needs}, nil
	}
	a, known := cat.Actions[name]
	if !offered || !known || !containsString(requestableActions(cat, g), name) {
		var needs []string
		if known && containsString(cat.Roles[g.Role].ActionsMayRequest, name) {
			needs = []string{"action.request:" + name}
		}
		return deny("forbidden", "forbidden", needs...)
	}
	// A hold with no_action or pause_action rejects new requests (HLD-3).
	if held, err := heldBy(ctx, tx, now, []string{"no_action", "pause_action"}, approvalScopes(g.Tenant, name, g.Role, g.Type)...); err != nil {
		return requestActionResult{}, err
	} else if held != "" {
		return deny("forbidden", "held")
	}
	if actArgs == nil {
		return deny("input_schema_invalid", "input_schema_invalid")
	}
	actArgs = actionDefaults(a, actArgs)
	schema, err := c.actionArgsSchema(cat, g.Tenant, g.Mark, a)
	if err != nil {
		return requestActionResult{}, err
	}
	if err := c.rs.validate(schema, actArgs); err != nil {
		return deny("input_schema_invalid", "input_schema_invalid")
	}
	// The machine and resource the arguments name are scopes of the request too: a
	// maintenance window on the host pauses new requests (HLD-3, MON-9).
	scopes, err := c.approvalHoldScopes(ctx, tx, cat, &approvalRow{Tenant: g.Tenant, Action: name, Args: actArgs, SourceTask: g.Task,
		RequestedBy: map[string]any{"role": g.Role}})
	if err != nil {
		return requestActionResult{}, err
	}
	if held, err := heldBy(ctx, tx, now, []string{"no_action", "pause_action"}, scopes...); err != nil {
		return requestActionResult{}, err
	} else if held != "" {
		return deny("forbidden", "held")
	}
	argsHash, err := jcs.Hash(actArgs)
	if err != nil {
		return requestActionResult{}, err
	}
	var rec []byte
	if err := tx.QueryRow(ctx, `SELECT record FROM tasks WHERE id = $1`, g.Task).Scan(&rec); err != nil {
		return requestActionResult{}, err
	}
	var tr struct {
		UntrustedSeen   bool   `json:"untrusted_seen"`
		UntrustedSource string `json:"untrusted_source"`
	}
	if err := json.Unmarshal(rec, &tr); err != nil {
		return requestActionResult{}, err
	}
	id, code, err := c.openApproval(ctx, tx, now, tv, approvalRow{
		Tenant: g.Tenant, SourceTask: g.Task, Initiator: g.Initiator, Action: name, ArgsHash: argsHash, Args: actArgs, Risk: a.Risk,
		RequestedBy:   map[string]any{"kind": "bot", "role": g.Role, "task": g.Task},
		UntrustedSeen: tr.UntrustedSeen, UntrustedSource: tr.UntrustedSource, ExpiresAt: now.Add(actionTTL(a)), Mark: g.Mark,
	})
	if err != nil {
		return requestActionResult{}, err
	}
	if code != "pending" {
		return deny(code, code)
	}
	if err := audit(true, "", id); err != nil {
		return requestActionResult{}, err
	}
	return requestActionResult{Code: "pending", ApprovalID: id}, nil
}

// openApproval writes a pending approval, or returns the identical pending one of the same
// initiator and source (APR-2); another person's request is their own approval. The
// approvals of the tenant are counted under its lock, so two requests at once do not both pass
// a limit: at most 3 per source task and the tenant's cards per hour; above them the code is
// service_rate_limited.
func (c *Core) openApproval(ctx context.Context, tx pgx.Tx, now time.Time, tv *TenantView, a approvalRow) (string, string, error) {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(7300108, hashtext($1))`, a.Tenant); err != nil {
		return "", "", err
	}
	var existing string
	// A waiting approval of the same write counts as the same request (APR-2, ORC-12).
	err := tx.QueryRow(ctx, `SELECT id FROM approvals WHERE tenant = $1 AND action = $2 AND args_hash = $3
		AND (state = 'pending' OR state = 'approved' AND reason = 'resource_busy')
		AND initiator = $4 AND coalesce(source_task, '') = $5`, a.Tenant, a.Action, a.ArgsHash, a.Initiator, a.SourceTask).Scan(&existing)
	if err == nil {
		return existing, "pending", nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", "", err
	}
	var perTask, perHour int
	if err := tx.QueryRow(ctx, `SELECT count(*) FILTER (WHERE $2 <> '' AND source_task = $2), count(*) FILTER (WHERE created_at > $3) FROM approvals WHERE tenant = $1`,
		a.Tenant, a.SourceTask, now.Add(-time.Hour)).Scan(&perTask, &perHour); err != nil {
		return "", "", err
	}
	limit := tv.Spec.Attention.CardsPerTenantHour
	if limit == 0 {
		limit = defaultCardsPerHour
	}
	if perTask >= maxActionRequests || perHour >= limit {
		return "", "service_rate_limited", nil
	}
	id, err := c.insertApproval(ctx, tx, now, a)
	return id, "pending", err
}

// approvalScopes are the hold scopes of an approval: the farm, the tenant, the action and,
// for a request from an attempt, its role and type.
func approvalScopes(tenant, action, role, typ string) []scopeRef {
	s := append(tenantScopes(tenant), scopeRef{kind: "action", tenant: tenant, name: action})
	if role != "" {
		s = append(s, scopeRef{kind: "role", tenant: tenant, name: role})
	}
	if typ != "" {
		s = append(s, scopeRef{kind: "tasktype", tenant: tenant, name: typ})
	}
	return s
}

// insertApproval writes a pending approval with a fresh nonce of 22 base64url characters, the
// length the Telegram binding needs for its buttons (APR-3).
func (c *Core) insertApproval(ctx context.Context, tx pgx.Tx, now time.Time, a approvalRow) (string, error) {
	a.ID, a.Nonce, a.State = ids.New(ids.Approval), ids.Secret(16), "pending"
	// The number of approvers is the action's at the request and does not change (APR-16).
	if a.Approvals == 0 {
		cat, err := c.Catalog(ctx)
		if err != nil {
			return "", err
		}
		if act, ok := c.actionOf(cat, a.Action); ok && act.Approvals == 2 {
			a.Approvals = 2
		}
	}
	rec, err := json.Marshal(approvalRecordJSON{
		RequestedBy: a.RequestedBy, Args: a.Args, Risk: a.Risk, UntrustedSeen: a.UntrustedSeen, UntrustedSource: a.UntrustedSource, Context: a.Context,
		DataMark: a.Mark, Approvals: a.Approvals,
	})
	if err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO approvals (id, tenant, source_task, initiator, action, args_hash, state, record, nonce, expires_at, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, 'pending', $7, $8, $9, $10, $10)`,
		a.ID, a.Tenant, nullable(a.SourceTask), a.Initiator, a.Action, a.ArgsHash, rec, a.Nonce, a.ExpiresAt, now); err != nil {
		return "", err
	}
	if err := c.appendEvent(ctx, tx, now, a.Tenant, Event{Type: "approval.pending", Subject: a.ID, Causation: a.SourceTask,
		Data: map[string]any{"approval": a.ID, "action": a.Action, "risk": a.Risk}}); err != nil {
		return "", err
	}
	return a.ID, notifyWork(ctx, tx)
}
