// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
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
}

const approvalColumns = `id, tenant, coalesce(source_task, ''), initiator, action, args_hash, state, nonce, record, expires_at,
	reminded_at, remind_at, cards_at, decided_at, coalesce(decided_by, ''), coalesce(decision, ''), coalesce(reason, ''),
	coalesce(action_task, ''), coalesce(toolcall, ''), created_at`

func scanApproval(row pgx.Row) (*approvalRow, error) {
	var a approvalRow
	var rec []byte
	if err := row.Scan(&a.ID, &a.Tenant, &a.SourceTask, &a.Initiator, &a.Action, &a.ArgsHash, &a.State, &a.Nonce, &rec, &a.ExpiresAt,
		&a.RemindedAt, &a.RemindAt, &a.CardsAt, &a.DecidedAt, &a.DecidedBy, &a.Decision, &a.Reason, &a.ActionTask, &a.Toolcall, &a.CreatedAt); err != nil {
		return nil, err
	}
	var r approvalRecordJSON
	if err := json.Unmarshal(rec, &r); err != nil {
		return nil, err
	}
	a.Args, a.Risk, a.RequestedBy, a.UntrustedSeen, a.UntrustedSource, a.Context = r.Args, r.Risk, r.RequestedBy, r.UntrustedSeen, r.UntrustedSource, r.Context
	a.Mark = r.DataMark
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
		"args": a.Args, "args_hash": a.ArgsHash, "nonce": a.Nonce, "risk": a.Risk, "approvals": 1,
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
		if _, ok := cat.Actions[a]; ok && hasCap(tv, groups, "action.request:"+a) {
			out = append(out, a)
		}
	}
	sort.Strings(out)
	return out
}

// requestActionTool is the built-in tool of step 7 of the catalog assembly, or nil when the
// attempt may request no action (APR-1, PRF-1). Its schema holds, for each action, the closed
// argument schema with the catalog values of the task's mark as enum (APR-2).
func (c *Core) requestActionTool(cat *Catalog, g *toolsGrant) (*catalogTool, error) {
	actions := requestableActions(cat, g)
	if len(actions) == 0 {
		return nil, nil
	}
	var variants []any
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
		variants = append(variants, map[string]any{
			"properties": map[string]any{"action": map[string]any{"const": name}, "args": schema},
		})
		fmt.Fprintf(&desc, "\n- %s: %s", name, strings.TrimSpace(a.Description))
	}
	schema := map[string]any{
		"type": "object", "additionalProperties": false, "required": []any{"action", "args"},
		"properties": map[string]any{
			"action": map[string]any{"enum": names},
			"args":   map[string]any{"type": "object", "maxProperties": 32},
		},
		"oneOf": variants,
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
	err := tx.QueryRow(ctx, `SELECT id FROM approvals WHERE tenant = $1 AND action = $2 AND args_hash = $3 AND state = 'pending'
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
	rec, err := json.Marshal(approvalRecordJSON{
		RequestedBy: a.RequestedBy, Args: a.Args, Risk: a.Risk, UntrustedSeen: a.UntrustedSeen, UntrustedSource: a.UntrustedSource, Context: a.Context,
		DataMark: a.Mark,
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

// cardChat is a direct chat of a person in a channel of the tenant: the first channel, by
// name, whose identity kind the person has. A Telegram private chat has the id of the user.
func cardChat(ctx context.Context, q store.Querier, tv *TenantView, principal string) (string, Chat, bool, error) {
	rows, err := q.Query(ctx, `SELECT identity FROM identities WHERE tenant = $1 AND principal = $2 ORDER BY identity`, tv.Name, principal)
	if err != nil {
		return "", Chat{}, false, err
	}
	var idents []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			rows.Close()
			return "", Chat{}, false, err
		}
		idents = append(idents, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", Chat{}, false, err
	}
	names := make([]string, 0, len(tv.Channels))
	for n := range tv.Channels {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		ch := tv.Channels[n]
		if ch.Direction == "out" || ch.IdentityKind == "" {
			continue
		}
		for _, id := range idents {
			if ext, ok := strings.CutPrefix(id, ch.IdentityKind+":"); ok {
				return n, Chat{Kind: "direct", ExternalID: ext}, true, nil
			}
		}
	}
	return "", Chat{}, false, nil
}

// approvers are the active people of the tenant with action.approve:<a>, without the
// initiator unless the tenant allows self approval (APR-9, decision-points approval_card).
func approvers(ctx context.Context, q store.Querier, now time.Time, tv *TenantView, a *approvalRow) ([]string, error) {
	initiator := a.Initiator
	rows, err := q.Query(ctx, `SELECT DISTINCT m.principal FROM memberships m JOIN principals p ON p.tenant = m.tenant AND p.id = m.principal
		WHERE m.tenant = $1 AND m.state = 'active' AND (m.until IS NULL OR m.until > $2) AND p.state = 'active' AND p.kind = 'human'
		ORDER BY m.principal`, tv.Name, now)
	if err != nil {
		return nil, err
	}
	var all []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			rows.Close()
			return nil, err
		}
		all = append(all, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var out []string
	for _, p := range all {
		if p == initiator && !tv.Spec.SelfApprove {
			continue
		}
		info, err := loadPrincipal(ctx, q, now, tv.Name, p)
		if err != nil {
			return nil, err
		}
		if mayApprove(tv, groupClosure(tv, info.Groups), a) {
			out = append(out, p)
		}
	}
	return out, nil
}

// planCards plans the card to every approver and the copy to the initiator (APR-3, APR-19). A
// person without a direct chat in a channel of the tenant gets no card.
func (c *Core) planCards(ctx context.Context, tx pgx.Tx, now time.Time, tv *TenantView, a *approvalRow) error {
	people, err := approvers(ctx, tx, now, tv, a)
	if err != nil {
		return err
	}
	for _, p := range people {
		if err := c.planCard(ctx, tx, now, tv, a, approvalCard, p, false); err != nil {
			return err
		}
	}
	if strings.HasPrefix(a.Initiator, "human:") {
		if err := c.planCard(ctx, tx, now, tv, a, initiatorCopy, a.Initiator, false); err != nil {
			return err
		}
	}
	_, err = tx.Exec(ctx, `UPDATE approvals SET cards_at = $2, updated_at = $2 WHERE id = $1`, a.ID, now)
	return err
}

// planCard plans one card delivery. The body names only the subject: the card itself is drawn
// from the record when a channel takes the delivery (APR-3).
func (c *Core) planCard(ctx context.Context, tx pgx.Tx, now time.Time, tv *TenantView, a *approvalRow, kind, person string, reminder bool) error {
	channel, chat, ok, err := cardChat(ctx, tx, tv, person)
	if err != nil || !ok {
		return err
	}
	body := map[string]any{"form": "card", "card": kind}
	if reminder {
		body["reminder"] = true
	}
	_, err = c.planDelivery(ctx, tx, now, plannedDelivery{
		Tenant: tv.Name, Task: a.SourceTask, Kind: kind, Addr: "approval_initiator", TargetKind: "chat", TargetID: chat.ID(),
		ExecKind: "channel", ExecName: channel, Recipient: person, Body: body, DataOrigin: "code",
		SubjectKind: "approval", SubjectID: a.ID,
	})
	return err
}

// planCardUpdates plans a card_update for every delivered card of the approval, so each
// recipient sees its new state (spec/lifecycles/approval.md).
func (c *Core) planCardUpdates(ctx context.Context, tx pgx.Tx, now time.Time, a *approvalRow) error {
	// Every card message gets its update: the first card, a reminder, the initiator copy.
	rows, err := tx.Query(ctx, `SELECT id, kind, exec_name, target_id, coalesce(recipient, '') FROM deliveries
		WHERE subject_kind = 'approval' AND subject_id = $1 AND kind IN ('approval_card', 'approval_initiator_copy') AND state IN ('planned', 'sending', 'delivered')
		ORDER BY seq`, a.ID)
	if err != nil {
		return err
	}
	type target struct{ id, kind, channel, chat, recipient string }
	var ts []target
	for rows.Next() {
		var t target
		if err := rows.Scan(&t.id, &t.kind, &t.channel, &t.chat, &t.recipient); err != nil {
			rows.Close()
			return err
		}
		ts = append(ts, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, t := range ts {
		if _, err := c.planDelivery(ctx, tx, now, plannedDelivery{
			Tenant: a.Tenant, Task: a.SourceTask, Kind: cardUpdate, Addr: "approval_initiator", TargetKind: "chat", TargetID: t.chat,
			ExecKind: "channel", ExecName: t.channel, Recipient: t.recipient, Body: map[string]any{"form": "card", "card": t.kind, "of": t.id},
			DataOrigin: "code", SubjectKind: "approval", SubjectID: a.ID,
		}); err != nil {
			return err
		}
	}
	return nil
}

// buildCard draws a card from the approval record by the code template (APR-3, APR-12, APR-19).
func (c *Core) buildCard(ctx context.Context, q store.Querier, cat *Catalog, a *approvalRow, kind string, reminder bool) (map[string]any, error) {
	act, _ := c.actionOf(cat, a.Action)
	title := act.Title
	if title == "" {
		title = a.Action
	}
	req := map[string]any{"initiator": a.Initiator, "source": a.RequestedBy["kind"]}
	for _, k := range []string{"role", "task", "ref"} {
		if v, ok := a.RequestedBy[k]; ok {
			req[k] = v
		}
	}
	// Principals carry no display name in 0.1: the card shows the principal id.
	display := cut(a.Initiator, 80)
	card := map[string]any{
		"card": kind, "subject_kind": "approval", "id": a.ID, "nonce": a.Nonce, "title": cut(title, 200),
		"args": a.Args, "requested_by": req, "initiator_display": display, "untrusted_seen": a.UntrustedSeen,
		"risk": a.Risk, "expires_at": fmtTime(a.ExpiresAt), "state": a.State,
		"footer": map[string]any{"why_you": "action.approve:" + a.Action, "until": fmtTime(a.ExpiresAt), "on_silence": "the action is not executed"},
	}
	if a.UntrustedSource != "" {
		card["untrusted_source"] = cut(a.UntrustedSource, 120)
	}
	if len(a.Context) > 0 {
		card["context"] = a.Context
	}
	if a.State != "pending" && a.Decision == "approve" && a.DecidedBy != "" {
		card["approved_by"] = []any{a.DecidedBy}
	}
	if reminder {
		card["reminder"] = true
	}
	button := func(code string) map[string]any {
		return map[string]any{"code": code, "subject_kind": "approval", "id": a.ID, "nonce": a.Nonce}
	}
	buttons := []any{}
	if a.State == "pending" {
		switch kind {
		case approvalCard:
			buttons = append(buttons, button("approve"), button("reject"), button("details"))
		case initiatorCopy:
			card["footer"] = map[string]any{"why_you": "the initiator of the request", "until": fmtTime(a.ExpiresAt), "on_silence": "the action is not executed"}
			buttons = append(buttons, button("remind"))
		}
	}
	card["buttons"] = buttons
	return card, nil
}

func cut(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// approvalContext is the context line: the closed result fields of the source attempt that
// the type marks x-botaru-read-by: code, without model text (APR-19).
func approvalContext(tt manifest.TaskType, result map[string]any) map[string]any {
	props, _ := tt.ResultSchema["properties"].(map[string]any)
	out := map[string]any{}
	keys := make([]string, 0, len(props))
	for k := range props {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		s, _ := props[k].(map[string]any)
		if s["x-botaru-read-by"] != "code" || len(out) >= 16 {
			continue
		}
		switch v := result[k].(type) {
		case string:
			if len([]rune(v)) <= 120 {
				out[k] = v
			}
		case float64, int, int64, bool:
			out[k] = v
		}
	}
	return out
}

// approvalLoop is the approval-expiry loop: the cards of an approval go out once the attempt
// that asked has ended (the attempt does not wait for the decision, D-41), with the context
// line of its result; a pending approval under a no_action hold is canceled, an overdue one
// expires, and at half of the deadline the approvers get one reminder (APR-12, APR-19, HLD-3).
func (c *Core) approvalLoop(ctx context.Context) error {
	cat, err := c.Catalog(ctx)
	if err != nil {
		return err
	}
	// A memory record card that expired without a decision discards its record (MEM-3).
	if err := c.tx(ctx, func(tx pgx.Tx, now time.Time) error { return c.expireMemory(ctx, tx, now) }); err != nil {
		return err
	}
	var list []struct{ id, tenant string }
	err = c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		list = list[:0]
		rows, err := tx.Query(ctx, `SELECT id, tenant FROM approvals WHERE state = 'pending' ORDER BY created_at, id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var x struct{ id, tenant string }
			if err := rows.Scan(&x.id, &x.tenant); err != nil {
				return err
			}
			list = append(list, x)
		}
		return rows.Err()
	})
	if err != nil {
		return err
	}
	for _, x := range list {
		if err := c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
			a, err := loadApproval(ctx, tx, x.tenant, x.id, true)
			if err != nil {
				return err
			}
			if a.State != "pending" {
				return nil
			}
			tv := cat.Tenants[a.Tenant]
			if tv == nil {
				return nil
			}
			typ := ""
			if a.SourceTask != "" {
				_ = tx.QueryRow(ctx, `SELECT type FROM tasks WHERE id = $1`, a.SourceTask).Scan(&typ)
			}
			role, _ := a.RequestedBy["role"].(string)
			if held, err := heldBy(ctx, tx, now, []string{"no_action"}, approvalScopes(a.Tenant, a.Action, role, typ)...); err != nil {
				return err
			} else if held != "" {
				return c.closeApproval(ctx, tx, now, a, "canceled", "held")
			}
			if !a.ExpiresAt.After(now) {
				return c.closeApproval(ctx, tx, now, a, "expired", "expired")
			}
			if a.CardsAt == nil {
				return c.cardsWhenAttemptEnds(ctx, tx, now, cat, tv, a, typ)
			}
			half := a.CardsAt.Add(a.ExpiresAt.Sub(*a.CardsAt) / 2)
			if a.RemindedAt == nil && !now.Before(half) {
				if _, err := tx.Exec(ctx, `UPDATE approvals SET reminded_at = $2, updated_at = $2 WHERE id = $1`, a.ID, now); err != nil {
					return err
				}
				_, err := c.remindApprovers(ctx, tx, now, tv, a)
				return err
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}

// cardsWhenAttemptEnds plans the cards once the source attempt has no active lease, with the
// context line of its result when it completed.
func (c *Core) cardsWhenAttemptEnds(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, tv *TenantView, a *approvalRow, typ string) error {
	if a.SourceTask != "" {
		var active bool
		var state string
		var result []byte
		if err := tx.QueryRow(ctx, `SELECT state, result, EXISTS (SELECT 1 FROM leases WHERE work_id = t.id AND kind = 'attempt' AND state = 'active')
			FROM tasks t WHERE id = $1`, a.SourceTask).Scan(&state, &result, &active); err != nil {
			return err
		}
		if active {
			return nil
		}
		if state == "completed" && len(result) > 0 {
			var res map[string]any
			if json.Unmarshal(result, &res) == nil {
				if ctxLine := approvalContext(cat.TaskTypes[typ], res); len(ctxLine) > 0 {
					a.Context = ctxLine
					if _, err := tx.Exec(ctx, `UPDATE approvals SET record = jsonb_set(record, '{context}', $2) WHERE id = $1`, a.ID, mustJSON(ctxLine)); err != nil {
						return err
					}
				}
			}
		}
	}
	return c.planCards(ctx, tx, now, tv, a)
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

// remindApprovers sends the approvers who have not decided a new card with the reminder line
// (APR-19); it returns how many got one.
func (c *Core) remindApprovers(ctx context.Context, tx pgx.Tx, now time.Time, tv *TenantView, a *approvalRow) (int, error) {
	people, err := approvers(ctx, tx, now, tv, a)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, p := range people {
		var had bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM deliveries WHERE subject_kind = 'approval' AND subject_id = $1 AND kind = 'approval_card' AND recipient = $2)`,
			a.ID, p).Scan(&had); err != nil {
			return 0, err
		}
		if !had {
			continue
		}
		if err := c.planCard(ctx, tx, now, tv, a, approvalCard, p, true); err != nil {
			return 0, err
		}
		n++
	}
	return n, nil
}

// closeApproval ends a pending approval without execution: rejected, expired or canceled.
func (c *Core) closeApproval(ctx context.Context, tx pgx.Tx, now time.Time, a *approvalRow, state, reason string) error {
	if _, err := tx.Exec(ctx, `UPDATE approvals SET state = $2, reason = $3, updated_at = $4 WHERE id = $1`, a.ID, state, nullable(reason), now); err != nil {
		return err
	}
	a.State, a.Reason = state, reason
	if err := c.appendEvent(ctx, tx, now, a.Tenant, Event{Type: "approval." + state, Subject: a.ID,
		Data: map[string]any{"approval": a.ID, "action": a.Action, "reason": reason}}); err != nil {
		return err
	}
	return c.planCardUpdates(ctx, tx, now, a)
}

// cancelApprovalsOf cancels the pending approvals of an initiator, as P2 principal.off or a
// lost right does (APR-6).
func (c *Core) cancelApprovalsOf(ctx context.Context, tx pgx.Tx, now time.Time, tenant, initiator, reason string) error {
	rows, err := tx.Query(ctx, `SELECT `+approvalColumns+` FROM approvals WHERE tenant = $1 AND initiator = $2 AND state = 'pending' ORDER BY id FOR UPDATE`, tenant, initiator)
	if err != nil {
		return err
	}
	var as []*approvalRow
	for rows.Next() {
		a, err := scanApproval(rows)
		if err != nil {
			rows.Close()
			return err
		}
		as = append(as, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, a := range as {
		if err := c.closeApproval(ctx, tx, now, a, "canceled", reason); err != nil {
			return err
		}
	}
	return nil
}

// recheckApprovalsOf cancels the pending approvals of an initiator who is no longer active
// (person_inactive) or lost action.request for the action (rights_revoked), in the
// transaction of the change (APR-6).
func (c *Core) recheckApprovalsOf(ctx context.Context, tx pgx.Tx, now time.Time, tv *TenantView, principal string) error {
	rows, err := tx.Query(ctx, `SELECT `+approvalColumns+` FROM approvals WHERE tenant = $1 AND initiator = $2 AND state = 'pending' ORDER BY id FOR UPDATE`, tv.Name, principal)
	if err != nil {
		return err
	}
	var as []*approvalRow
	for rows.Next() {
		a, err := scanApproval(rows)
		if err != nil {
			rows.Close()
			return err
		}
		as = append(as, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(as) == 0 {
		return err
	}
	p, err := loadPrincipal(ctx, tx, now, tv.Name, principal)
	if errors.Is(err, pgx.ErrNoRows) {
		p, err = nil, nil
	}
	if err != nil {
		return err
	}
	for _, a := range as {
		switch {
		case p == nil || p.State != "active":
			err = c.closeApproval(ctx, tx, now, a, "canceled", "person_inactive")
		case !hasCap(tv, groupClosure(tv, p.Groups), requestCap(a.Action, a.Args)):
			err = c.closeApproval(ctx, tx, now, a, "canceled", "rights_revoked")
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// OnBehalf is who pressed a card button and from which chat (on-behalf schema): the channel
// passes it in the headers Botaru-On-Behalf, Botaru-Chat and Botaru-Message.
type OnBehalf struct {
	Identity string
	Chat     Chat
	Message  string
}

// ParseOnBehalf reads the on-behalf headers; it reports false when they are missing or malformed.
func ParseOnBehalf(identity, chat, message string) (OnBehalf, bool) {
	kind, ext, ok := strings.Cut(chat, ":")
	if identity == "" || !ok || ext == "" || (kind != "direct" && kind != "group") || len(identity) > 200 || len(ext) > 128 || len(message) > 128 {
		return OnBehalf{}, false
	}
	if !strings.Contains(identity, ":") {
		return OnBehalf{}, false
	}
	return OnBehalf{Identity: identity, Chat: Chat{Kind: kind, ExternalID: ext}, Message: message}, true
}

// DecideRequest is p1-decide-request.
type DecideRequest struct {
	SubjectKind string `json:"subject_kind"`
	ID          string `json:"id"`
	Nonce       string `json:"nonce"`
	Decision    string `json:"decision"`
	ReasonCode  string `json:"reason_code,omitempty"`
}

// DecideResult is p1-decide-response.
type DecideResult struct {
	SubjectKind string `json:"subject_kind"`
	ID          string `json:"id"`
	Decision    string `json:"decision"`
	State       string `json:"state,omitempty"`
}

// cardPress is a checked press of a card button: the person and the card delivery to them.
type cardPress struct {
	tv          *TenantView
	principal   string
	groups      []string
	deliveredAt time.Time
}

// checkPress checks a press on behalf of a person (spec/bindings/http/README.md, APR-4): the
// identity kind of the channel's driver, an active human with strong identity in the channel's
// tenant, and a card of this subject delivered by this channel to this person in this chat, on
// this message.
func (c *Core) checkPress(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, w *Worker, ob OnBehalf, subjectKind, id string, kinds []string) (*cardPress, error) {
	if w.Kind != "channel" {
		return nil, refuse("forbidden", "only a channel presses a card")
	}
	tv := cat.Tenants[w.Tenant]
	if tv == nil {
		return nil, refuse("not_ready", "the tenant is not applied")
	}
	ch, ok := tv.Channels[w.Target]
	if !ok || ch.IdentityKind == "" || !strings.HasPrefix(ob.Identity, ch.IdentityKind+":") {
		if _, err := c.appendAudit(ctx, tx, now, Audit{
			Tenant: w.Tenant, Kind: "authorize", Actor: Actor{Principal: workerPrincipal(w.ID), Via: "channel"},
			Command: "decide", Target: &Target{Kind: subjectKind, ID: id}, OK: false, Reason: "identity_kind",
		}); err != nil {
			return nil, err
		}
		return nil, refuse("forbidden", "the identity kind does not match the channel driver")
	}
	if ch.IdentityAssurance != "strong" {
		return nil, refuse("forbidden", "a decision needs a channel with strong identity")
	}
	if ob.Chat.Kind != "direct" && ch.MembershipAssurance != "enforced" {
		return nil, refuse("forbidden", "a group chat without enforced membership takes no decisions")
	}
	principal, err := principalByIdentity(ctx, tx, w.Tenant, ob.Identity)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, refuse("forbidden", "the person is not a subject of the tenant")
	}
	if err != nil {
		return nil, err
	}
	p, err := loadPrincipal(ctx, tx, now, w.Tenant, principal)
	if err != nil {
		return nil, err
	}
	if p.State != "active" || p.Kind != "human" {
		return nil, refuse("forbidden", "only an active person decides")
	}
	var at time.Time
	err = tx.QueryRow(ctx, `SELECT updated_at FROM deliveries WHERE subject_kind = $1 AND subject_id = $2 AND kind = ANY($3) AND exec_kind = 'channel'
		AND exec_name = $4 AND recipient = $5 AND target_id = $6 AND state = 'delivered' AND ($7 = '' OR external_id = $7)
		ORDER BY seq LIMIT 1`, subjectKind, id, kinds, w.Target, principal, ob.Chat.ID(), ob.Message).Scan(&at)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, refuse("forbidden", "no card of this subject was delivered to this person in this chat")
	}
	if err != nil {
		return nil, err
	}
	return &cardPress{tv: tv, principal: principal, groups: groupClosure(tv, p.Groups), deliveredAt: at}, nil
}

// Decide is P1 decide on the subject approval (APR-4): once, by a person with the right at the
// moment of the decision, from the chat of the card. An approval executes in the same
// transaction (APR-7).
func (c *Core) Decide(ctx context.Context, w *Worker, ob *OnBehalf, req DecideRequest) (*DecideResult, error) {
	if ob == nil {
		return nil, refuse("forbidden", "decide needs Botaru-On-Behalf and Botaru-Chat")
	}
	switch req.SubjectKind {
	case "approval":
	case "memory_record":
		return c.decideMemory(ctx, w, *ob, req)
	case "subject_request":
		return c.decideSubject(ctx, w, *ob, req)
	default:
		return nil, refuse("not_found", "this build decides approvals, memory records and a person's requests")
	}
	if req.Decision != "approve" && req.Decision != "reject" {
		return nil, refuse("input_schema_invalid", "an approval is approved or rejected")
	}
	cat, err := c.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	var out *DecideResult
	err = c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		a, err := loadApproval(ctx, tx, w.Tenant, req.ID, true)
		if errors.Is(err, pgx.ErrNoRows) {
			return refuse("not_found", "no such approval")
		}
		if err != nil {
			return err
		}
		if a.Nonce != req.Nonce {
			return refuse("forbidden", "the nonce is not the card's")
		}
		press, err := c.checkPress(ctx, tx, now, cat, w, *ob, "approval", a.ID, []string{approvalCard})
		if err != nil {
			return err
		}
		out = &DecideResult{SubjectKind: "approval", ID: a.ID, Decision: req.Decision}
		if a.State != "pending" {
			// A repeat of the same decision by the same person gets the same answer (IDM-3).
			if a.DecidedBy == press.principal && a.Decision == req.Decision {
				out.State = a.State
				return nil
			}
			if a.State == "expired" {
				return refuse("expired", "the card has expired")
			}
			return refuse("already_decided", "the approval is decided")
		}
		deny := func(reason, detail string) error {
			if _, err := c.appendAudit(ctx, tx, now, Audit{
				Tenant: a.Tenant, Kind: "decision", Actor: Actor{Principal: press.principal, Via: "channel"},
				Command: "decide", Target: &Target{Kind: "approval", ID: a.ID}, OK: false, Reason: reason,
				After: map[string]any{"decision": req.Decision},
			}); err != nil {
				return err
			}
			return &committed{p: refuse(reason, "%s", detail)}
		}
		if !mayApprove(press.tv, press.groups, a) {
			return deny("forbidden", "the person has no action.approve for this action")
		}
		if press.principal == a.Initiator && !press.tv.Spec.SelfApprove {
			return deny("forbidden", "the initiator does not approve their own request")
		}
		if !a.ExpiresAt.After(now) {
			if err := c.closeApproval(ctx, tx, now, a, "expired", "expired"); err != nil {
				return err
			}
			return &committed{p: refuse("expired", "the card has expired")}
		}
		typ := ""
		if a.SourceTask != "" {
			_ = tx.QueryRow(ctx, `SELECT type FROM tasks WHERE id = $1`, a.SourceTask).Scan(&typ)
		}
		role, _ := a.RequestedBy["role"].(string)
		if held, err := heldBy(ctx, tx, now, []string{"no_action"}, approvalScopes(a.Tenant, a.Action, role, typ)...); err != nil {
			return err
		} else if held != "" {
			if err := c.closeApproval(ctx, tx, now, a, "canceled", "held"); err != nil {
				return err
			}
			return &committed{p: refuse("held", "a hold cancels pending approvals")}
		}
		if held, err := heldBy(ctx, tx, now, []string{"pause_action"}, approvalScopes(a.Tenant, a.Action, role, typ)...); err != nil {
			return err
		} else if held != "" {
			return refuse("held", "a hold pauses decisions on this approval")
		}
		var views int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM card_views WHERE subject_kind = 'approval' AND subject_id = $1 AND principal = $2`, a.ID, press.principal).Scan(&views); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE approvals SET decided_by = $2, decided_at = $3, decision = $4, updated_at = $3 WHERE id = $1`,
			a.ID, press.principal, now, req.Decision); err != nil {
			return err
		}
		a.DecidedBy, a.Decision, a.DecidedAt = press.principal, req.Decision, &now
		ev := map[string]any{
			"subject": map[string]any{"kind": "approval", "id": a.ID}, "card": approvalCard, "decision": req.Decision,
			"decided_by": press.principal, "channel": w.Target, "chat_kind": ob.Chat.Kind, "via": "decide",
			"delivered_at": fmtTime(press.deliveredAt), "decided_at": fmtTime(now),
			"response_ms": max(now.Sub(press.deliveredAt).Milliseconds(), 0), "details_opened": views > 0,
		}
		if req.ReasonCode != "" {
			ev["reason_code"] = req.ReasonCode
		}
		if err := c.appendEvent(ctx, tx, now, a.Tenant, Event{Type: "decision.made", Subject: a.ID, Data: ev}); err != nil {
			return err
		}
		if _, err := c.appendAudit(ctx, tx, now, Audit{
			Tenant: a.Tenant, Kind: "decision", Actor: Actor{Principal: press.principal, Via: "channel"},
			Command: "decide", Target: &Target{Kind: "approval", ID: a.ID}, OK: true,
			After: map[string]any{"decision": req.Decision, "action": a.Action},
		}); err != nil {
			return err
		}
		if req.Decision == "reject" {
			if err := c.closeApproval(ctx, tx, now, a, "rejected", ""); err != nil {
				return err
			}
			out.State = "rejected"
			return nil
		}
		if err := c.executeApproval(ctx, tx, now, cat, press.tv, a); err != nil {
			return err
		}
		out.State = a.State
		return nil
	})
	var cm *committed
	if errors.As(err, &cm) {
		return nil, cm.p
	}
	if err != nil {
		return nil, err
	}
	return out, nil
}

// executeApproval moves an approved approval to executing: the action.<a> task directly in
// working and its one write call from the snapshot (APR-5, APR-7, CNT-7). Before that the core
// checks again that the initiator is active with action.request:<a> and that the arguments
// still pass the catalog (APR-6, MAN-10).
func (c *Core) executeApproval(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, tv *TenantView, a *approvalRow) error {
	act, ok := c.actionOf(cat, a.Action)
	if !ok {
		return c.closeApproval(ctx, tx, now, a, "canceled", "ref_invalid")
	}
	if strings.HasPrefix(a.Initiator, "human:") {
		p, err := loadPrincipal(ctx, tx, now, a.Tenant, a.Initiator)
		if errors.Is(err, pgx.ErrNoRows) || err == nil && p.State != "active" {
			return c.closeApproval(ctx, tx, now, a, "canceled", "person_inactive")
		}
		if err != nil {
			return err
		}
		if !hasCap(tv, groupClosure(tv, p.Groups), requestCap(a.Action, a.Args)) {
			return c.closeApproval(ctx, tx, now, a, "canceled", "rights_revoked")
		}
	}
	// A service action from a bot needs the role's permission at the decision too (APR-18),
	// and every service action passes the guards of MON-8 again: the verb is still allowed,
	// one operation per service, the rate limits and the holds.
	if a.Action == serviceAction {
		role, _ := a.RequestedBy["role"].(string)
		name, _ := a.Args["service"].(string)
		verb, _ := a.Args["verb"].(string)
		if role != "" && !roleControls(cat, role, name) {
			return c.closeApproval(ctx, tx, now, a, "canceled", "rights_revoked")
		}
		svc, ok := findControllable(cat, a.Tenant, name)
		if !ok || !slices.Contains(svc.Control, verb) {
			return c.closeApproval(ctx, tx, now, a, "canceled", "ref_invalid")
		}
		task, _ := a.RequestedBy["task"].(string)
		var typ string
		if task != "" {
			_ = tx.QueryRow(ctx, `SELECT type FROM tasks WHERE id = $1`, task).Scan(&typ)
		}
		code, err := c.serviceGuards(ctx, tx, now, serviceCall{Tenant: a.Tenant, Service: svc, Verb: verb, Initiator: a.Initiator, Role: role, Task: task, Type: typ})
		if err != nil {
			return err
		}
		switch code {
		case "":
		case "held":
			return c.closeApproval(ctx, tx, now, a, "canceled", "held")
		default:
			return c.closeApproval(ctx, tx, now, a, "exec_failed", code)
		}
	}
	mark := a.Mark
	if mark == "" && a.SourceTask != "" {
		var rec []byte
		if err := tx.QueryRow(ctx, `SELECT record FROM tasks WHERE id = $1`, a.SourceTask).Scan(&rec); err == nil {
			var r struct {
				DataMark string `json:"data_mark"`
			}
			_ = json.Unmarshal(rec, &r)
			mark = r.DataMark
		}
	}
	schema, err := c.actionArgsSchema(cat, a.Tenant, mark, act)
	if err != nil {
		return err
	}
	if c.rs.validate(schema, a.Args) != nil {
		return c.closeApproval(ctx, tx, now, a, "canceled", "ref_invalid")
	}
	taskID, err := c.insertActionTask(ctx, tx, now, actionTaskInput{
		Tenant: a.Tenant, Action: a.Action, Args: a.Args, Initiator: a.Initiator, SourceTask: a.SourceTask, Mark: mark, ApprovalID: a.ID,
	})
	if err != nil {
		return err
	}
	callID, err := c.insertWriteCall(ctx, tx, now, cat, writeCall{
		Tenant: a.Tenant, Task: taskID, Action: a.Action, Act: act, Args: a.Args, ArgsHash: a.ArgsHash, ApprovalID: a.ID,
	})
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE approvals SET state = 'executing', action_task = $2, toolcall = $3, updated_at = $4 WHERE id = $1 AND state = 'pending'`,
		a.ID, taskID, callID, now); err != nil {
		return err
	}
	a.State, a.ActionTask, a.Toolcall = "executing", taskID, callID
	if err := c.appendEvent(ctx, tx, now, a.Tenant, Event{Type: "approval.executing", Subject: a.ID,
		Data: map[string]any{"approval": a.ID, "action": a.Action, "task": taskID}}); err != nil {
		return err
	}
	if err := c.planCardUpdates(ctx, tx, now, a); err != nil {
		return err
	}
	// A call that ended at once (no toolhost, a hold) closes the task and the approval now.
	var state, reason string
	if err := tx.QueryRow(ctx, `SELECT state, coalesce(reason, '') FROM toolcalls WHERE id = $1`, callID).Scan(&state, &reason); err != nil {
		return err
	}
	if state != "queued" {
		if err := c.writeCallEnded(ctx, tx, now, callID, state, reason, nil); err != nil {
			return err
		}
		a.State = map[string]string{"done": "executed", "exec_unknown": "exec_unknown"}[state]
		if a.State == "" {
			a.State = "exec_failed"
		}
	}
	return nil
}

// actionTaskInput is the action.<a> task of an approval or a grant.
type actionTaskInput struct {
	Tenant, Action, Initiator, SourceTask, Mark string
	Args                                        map[string]any
	ApprovalID, GrantID                         string
	// Origin is the chat of a person's command, which gets the outcome as code text; Key is
	// then the key of the activity, so a repeat of the same update finds the task (DLV-5).
	Origin *Origin
	Key    string
}

// insertActionTask creates the action.<a> task directly in working, without an attempt and a
// lease, with the key system:approval:<id> or system:grant:<id> (CNT-7). It has no deadline of
// its own: the card's deadline and the call's timeout hold it.
func (c *Core) insertActionTask(ctx context.Context, tx pgx.Tx, now time.Time, in actionTaskInput) (string, error) {
	id := ids.New(ids.Task)
	typ := "action." + in.Action
	body := map[string]any{"type": typ, "params": in.Args}
	bodyHash, err := jcs.Hash(body)
	if err != nil {
		return "", err
	}
	root, fairness := id, "action:"+in.Action
	if in.SourceTask != "" {
		var rec []byte
		if err := tx.QueryRow(ctx, `SELECT record FROM tasks WHERE id = $1`, in.SourceTask).Scan(&rec); err == nil {
			var r struct {
				Root     string `json:"root"`
				Fairness string `json:"fairness_key"`
			}
			_ = json.Unmarshal(rec, &r)
			if r.Root != "" {
				root = r.Root
			}
			if r.Fairness != "" {
				fairness = r.Fairness
			}
		}
	}
	record := map[string]any{
		"tenant": in.Tenant, "enqueued_by": "core", "depth": 0, "root": root, "correlationid": root, "max_tier": 3,
		"untrusted_seen": false, "fairness_key": fairness, "budget_line": "work", "purpose": "work",
	}
	key, cause := "system:approval:"+in.ApprovalID, in.ApprovalID
	if in.ApprovalID != "" {
		record["approval_id"] = in.ApprovalID
	} else {
		record["grant_id"] = in.GrantID
		key, cause = "system:grant:"+in.GrantID, in.GrantID
	}
	if in.Key != "" {
		key = in.Key
	}
	if in.Mark != "" {
		record["data_mark"] = in.Mark
	}
	if in.Origin != nil {
		record["origin"] = in.Origin
	}
	rec, err := json.Marshal(record)
	if err != nil {
		return "", err
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO tasks (id, tenant, type, state, initiator, priority_class, idempotency_key, body_hash, body, record, created_at, updated_at)
		VALUES ($1, $2, $3, 'working', $4, 2, $5, $6, $7, $8, $9, $9)`,
		id, in.Tenant, typ, in.Initiator, key, bodyHash, raw, rec, now); err != nil {
		return "", err
	}
	if err := c.appendEvent(ctx, tx, now, in.Tenant, Event{Type: "task.submitted", Subject: id, Data: map[string]any{"task": id, "type": typ}, Causation: cause}); err != nil {
		return "", err
	}
	return id, nil
}

// writeCall is a write call of an action.
type writeCall struct {
	Tenant, Task, Action string
	Act                  manifest.Action
	Args                 map[string]any
	ArgsHash             string
	ApprovalID, GrantID  string
}

// insertWriteCall queues the one write call of an action task; a call that a hold or the lack
// of a toolhost stops ends at once, before it is handed out (APR-7, DLV-16).
func (c *Core) insertWriteCall(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, wc writeCall) (string, error) {
	plugin, toolName := wc.Act.Executor.Plugin, wc.Act.Executor.Tool
	right := toolspec.RightName(plugin, toolName)
	pl := cat.Plugins[plugin]
	t, ok := pl.Tool(toolName)
	id := ids.New(ids.Call)
	row := toolCallRow{
		ID: id, Tenant: wc.Tenant, Task: wc.Task, Tool: right, Access: "write", Args: wc.Args, Plugin: plugin, Placement: pl.Placement,
		Untrusted: !ok || !t.TrustedOutput(), State: "queued", ArgsHash: wc.ArgsHash, ApprovalID: wc.ApprovalID, GrantID: wc.GrantID,
	}
	end := func(state, reason string) (string, error) {
		row.State, row.Reason = state, reason
		if err := c.insertToolCall(ctx, tx, now, row); err != nil {
			return "", err
		}
		return id, nil
	}
	if !ok || !t.On() || t.Access != "write" {
		return end("error", "plugin_unavailable")
	}
	row.DefHash = toolDefHash(pl.Placement, t)
	// The machine of the call comes from the action argument named in target_from, by the
	// catalog lists of the action schema (TEN-3a).
	params := map[string]map[string]any{}
	props, _ := wc.Act.Args["properties"].(map[string]any)
	for k, v := range props {
		if m, ok := v.(map[string]any); ok {
			params[k] = m
		}
	}
	target, resolved, err := c.writeTarget(cat, wc.Tenant, t, params, wc.Args)
	if err != nil {
		return end("error", "input_schema_invalid")
	}
	row.Host, row.Resource, row.Resolved = target.host, target.resource, resolved
	scopes := append(tenantScopes(wc.Tenant), scopeRef{kind: "plugin", tenant: wc.Tenant, name: plugin},
		scopeRef{kind: "action", tenant: wc.Tenant, name: wc.Action}, scopeRef{kind: "tasktype", tenant: wc.Tenant, name: "action." + wc.Action})
	scopes = append(scopes, targetScopes(wc.Tenant, target)...)
	if held, err := heldBy(ctx, tx, now, []string{"no_tool", "no_action"}, scopes...); err != nil {
		return "", err
	} else if held != "" {
		return end("denied", "held")
	}
	live, err := c.liveToolhost(ctx, tx, now, cat, wc.Tenant, plugin, pl.Placement, target.host)
	if err != nil {
		return "", err
	}
	if !live {
		return end("error", "plugin_unavailable")
	}
	row.StartBy = now.Add(maxToolStartBy)
	if err := c.insertToolCall(ctx, tx, now, row); err != nil {
		return "", err
	}
	return id, notifyWork(ctx, tx)
}

// writeTarget computes the machine of a write call: the argument of target_from may be one
// value or a list of values, and all of them must lie on one machine.
func (c *Core) writeTarget(cat *Catalog, tenant string, t toolspec.Tool, params map[string]map[string]any, args map[string]any) (callTargetRef, map[string]string, error) {
	if t.Target == "any" {
		return callTargetRef{}, map[string]string{}, nil
	}
	p := params[t.TargetFrom]
	if p == nil {
		return callTargetRef{}, nil, errors.New("no target parameter")
	}
	var values []string
	switch v := args[t.TargetFrom].(type) {
	case string:
		values = []string{v}
	case []any:
		for _, x := range v {
			s, ok := x.(string)
			if !ok {
				return callTargetRef{}, nil, errors.New("a target value is not a string")
			}
			values = append(values, s)
		}
		if items, ok := p["items"].(map[string]any); ok {
			p = items
		}
	}
	if len(values) == 0 {
		return callTargetRef{}, nil, errors.New("no target value")
	}
	tool := t
	tool.Params = map[string]map[string]any{t.TargetFrom: p}
	tool.Commands = nil
	var first callTargetRef
	resolved := map[string]string{}
	for i, v := range values {
		ref, res, err := c.callTargetAny(cat, tenant, tool, map[string]any{t.TargetFrom: v})
		if err != nil {
			return callTargetRef{}, nil, err
		}
		if i == 0 {
			first, resolved = ref, res
			continue
		}
		if ref.host != first.host {
			return callTargetRef{}, nil, errors.New("the target values lie on several machines")
		}
		if ref.resource != first.resource {
			first.resource = ""
		}
	}
	return first, resolved, nil
}

// writeCallEnded closes the action task and the approval of a write call that ended (CNT-7,
// APR-7): done completes them, exec_unknown leaves the outcome unknown, anything else is
// exec_failed.
func (c *Core) writeCallEnded(ctx context.Context, tx pgx.Tx, now time.Time, callID, state, reason string, output []byte) error {
	var task, tenant string
	var approval, grant *string
	err := tx.QueryRow(ctx, `SELECT task, tenant, approval_id, grant_id FROM toolcalls WHERE id = $1 AND access = 'write'`, callID).Scan(&task, &tenant, &approval, &grant)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	taskState, taskReason, apState := "completed", "", "executed"
	switch state {
	case "done":
	case "exec_unknown":
		taskState, taskReason, apState = "failed", "exec_unknown", "exec_unknown"
	default:
		taskState, taskReason, apState = "failed", "exec_failed", "exec_failed"
	}
	var result []byte
	if state == "done" {
		var o mcp.CallResult
		_ = json.Unmarshal(output, &o)
		sc := o.StructuredContent
		if sc == nil {
			sc = map[string]any{}
		}
		result = mustJSON(sc)
	}
	// Only an action task lives and ends with its write call (CNT-7); a grant call of a bot's
	// own task ends only the call.
	var rec []byte
	var typ, initiator string
	err = tx.QueryRow(ctx, `UPDATE tasks SET state = $2, reason = $3, result = $4, updated_at = $5 WHERE id = $1 AND state = 'working' AND type LIKE 'action.%'
		RETURNING record, type, initiator`, task, taskState, nullable(taskReason), result, now).Scan(&rec, &typ, &initiator)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if err == nil {
		data := map[string]any{"task": task}
		if taskReason != "" {
			data["reason"] = taskReason
		}
		if err := c.appendEvent(ctx, tx, now, tenant, Event{Type: "task." + taskState, Subject: task, Data: data}); err != nil {
			return err
		}
		if err := c.grantOutcomeNotice(ctx, tx, now, tenant, task, typ, initiator, rec, taskState, taskReason); err != nil {
			return err
		}
	}
	if approval == nil {
		return nil
	}
	a, err := loadApproval(ctx, tx, tenant, *approval, true)
	if err != nil {
		return err
	}
	if a.State != "executing" {
		return nil
	}
	apReason := ""
	if apState != "executed" {
		apReason = reason
		if apReason == "" {
			apReason = apState
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE approvals SET state = $2, reason = $3, updated_at = $4 WHERE id = $1`, a.ID, apState, nullable(apReason), now); err != nil {
		return err
	}
	a.State, a.Reason = apState, apReason
	if err := c.appendEvent(ctx, tx, now, tenant, Event{Type: "approval." + apState, Subject: a.ID,
		Data: map[string]any{"approval": a.ID, "action": a.Action, "task": task, "reason": apReason}}); err != nil {
		return err
	}
	return c.planCardUpdates(ctx, tx, now, a)
}

// grantOutcomeNotice tells the person who gave /service or /run the outcome of the grant's
// action as code text in the chat of the command (PRM-18).
func (c *Core) grantOutcomeNotice(ctx context.Context, tx pgx.Tx, now time.Time, tenant, task, typ, initiator string, rec []byte, state, reason string) error {
	var r struct {
		Origin *Origin `json:"origin"`
	}
	if json.Unmarshal(rec, &r) != nil || r.Origin == nil {
		return nil
	}
	body := map[string]any{"form": "text_code", "text_key": "cmd.action.done", "params": map[string]any{"action": strings.TrimPrefix(typ, "action.")}}
	if state != "completed" {
		body = map[string]any{"form": "text_code", "text_key": "task.failed", "params": map[string]any{"reason": reason}}
	}
	_, err := c.planDelivery(ctx, tx, now, plannedDelivery{
		Tenant: tenant, Task: task, Kind: "task_result", Addr: "origin", TargetKind: "chat", TargetID: r.Origin.Chat.ID(),
		ExecKind: "channel", ExecName: r.Origin.Channel, Recipient: initiator, DataOrigin: "code", Body: body,
	})
	return err
}

// CardOp is p1-card-request: a press of an operation button of a card: Details or Remind approvers.
type CardOp struct {
	Op          string `json:"op"`
	SubjectKind string `json:"subject_kind"`
	ID          string `json:"id"`
	Nonce       string `json:"nonce"`
}

// CardDetails is P1 card.details: the card fields from the code template; it records the
// opening for details_opened (APR-11). There is no model text in an approval record.
func (c *Core) CardDetails(ctx context.Context, w *Worker, ob *OnBehalf, req CardOp) (map[string]any, error) {
	if ob == nil {
		return nil, refuse("forbidden", "card.details needs Botaru-On-Behalf and Botaru-Chat")
	}
	if req.SubjectKind != "approval" {
		return nil, refuse("not_found", "this build has cards only for approvals")
	}
	cat, err := c.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	err = c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		a, err := loadApproval(ctx, tx, w.Tenant, req.ID, false)
		if errors.Is(err, pgx.ErrNoRows) {
			return refuse("not_found", "no such approval")
		}
		if err != nil {
			return err
		}
		if a.Nonce != req.Nonce {
			return refuse("forbidden", "the nonce is not the card's")
		}
		press, err := c.checkPress(ctx, tx, now, cat, w, *ob, "approval", a.ID, []string{approvalCard, initiatorCopy})
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO card_views (subject_kind, subject_id, principal, opened_at) VALUES ('approval', $1, $2, $3) ON CONFLICT DO NOTHING`,
			a.ID, press.principal, now); err != nil {
			return err
		}
		act, _ := c.actionOf(cat, a.Action)
		fields := map[string]any{
			"action": a.Action, "title": act.Title, "args": a.Args, "risk": a.Risk, "state": a.State,
			"requested_by": a.RequestedBy, "untrusted_seen": a.UntrustedSeen, "expires_at": fmtTime(a.ExpiresAt),
		}
		if len(a.Context) > 0 {
			fields["context"] = a.Context
		}
		if a.UntrustedSource != "" {
			fields["untrusted_source"] = a.UntrustedSource
		}
		out = map[string]any{"template": "card.details.approval", "fields": fields, "deliver_to": "same_chat"}
		return nil
	})
	return out, err
}

// CardRemind is P1 card.remind from the initiator copy: the approvers who have not decided get
// a reminder, at most once in 5 minutes per approval (APR-19).
func (c *Core) CardRemind(ctx context.Context, w *Worker, ob *OnBehalf, req CardOp) (map[string]any, error) {
	if ob == nil {
		return nil, refuse("forbidden", "card.remind needs Botaru-On-Behalf and Botaru-Chat")
	}
	if req.SubjectKind != "approval" {
		return nil, refuse("not_found", "this build has cards only for approvals")
	}
	cat, err := c.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	err = c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		a, err := loadApproval(ctx, tx, w.Tenant, req.ID, true)
		if errors.Is(err, pgx.ErrNoRows) {
			return refuse("not_found", "no such approval")
		}
		if err != nil {
			return err
		}
		if a.Nonce != req.Nonce {
			return refuse("forbidden", "the nonce is not the card's")
		}
		press, err := c.checkPress(ctx, tx, now, cat, w, *ob, "approval", a.ID, []string{initiatorCopy})
		if err != nil {
			return err
		}
		if press.principal != a.Initiator {
			return refuse("forbidden", "only the initiator reminds the approvers")
		}
		if a.State != "pending" {
			return refuse("already_decided", "the approval is decided")
		}
		if a.RemindAt != nil && now.Sub(*a.RemindAt) < remindEvery {
			p := refuse("service_rate_limited", "a reminder was sent less than 5 minutes ago")
			p.RetryAfterS = int((remindEvery - now.Sub(*a.RemindAt)).Seconds()) + 1
			return p
		}
		if _, err := tx.Exec(ctx, `UPDATE approvals SET remind_at = $2, updated_at = $2 WHERE id = $1`, a.ID, now); err != nil {
			return err
		}
		n, err := c.remindApprovers(ctx, tx, now, press.tv, a)
		if err != nil {
			return err
		}
		out = map[string]any{"reminded": n}
		return nil
	})
	return out, err
}

// approvalCardBody draws the body of a card delivery from the approval record when a channel
// takes it; a card update carries the external id of the message it edits (FEN-7).
func (c *Core) approvalCardBody(ctx context.Context, tx pgx.Tx, cat *Catalog, tenant, kind, id, target, recipient, channel string, body map[string]any) (map[string]any, string, error) {
	a, err := loadApproval(ctx, tx, tenant, id, false)
	if err != nil {
		return nil, "", err
	}
	cardKind, _ := body["card"].(string)
	reminder, _ := body["reminder"].(bool)
	card, err := c.buildCard(ctx, tx, cat, a, cardKind, reminder)
	if err != nil {
		return nil, "", err
	}
	replaces := ""
	if kind == cardUpdate {
		// The update edits the message of the card delivery it names.
		of, _ := body["of"].(string)
		var ext *string
		err := tx.QueryRow(ctx, `SELECT external_id FROM deliveries WHERE id = $6 AND subject_kind = 'approval' AND subject_id = $1 AND kind = $2
			AND exec_name = $3 AND target_id = $4 AND recipient IS NOT DISTINCT FROM $5 AND state = 'delivered' AND external_id IS NOT NULL`,
			id, cardKind, channel, target, nullable(recipient), of).Scan(&ext)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, "", err
		}
		if ext == nil {
			return nil, "", nil
		}
		replaces = *ext
	}
	return map[string]any{"form": "card", "card": card}, replaces, nil
}
