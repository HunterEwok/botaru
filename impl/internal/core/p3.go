// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"errors"
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

// P3Profile is the profile version of the gateway profile of the MCP binding.
const P3Profile = "botaru-tool-gateway/1"

// maxToolStartBy is the ceiling of the handout deadline of a tool call (DLV-16).
const maxToolStartBy = 30 * time.Second

// toolsGrant is what a tools handle stands for: the attempt and its task.
type toolsGrant struct {
	Tenant    string
	Task      string
	Type      string
	LeaseNo   int
	Worker    string
	Role      string
	Initiator string
	MaxTier   int
	Mark      string
	Purpose   string
	// InitiatorGroups are the groups of an active initiator, for the built-in tools.
	InitiatorGroups []string
	// HostScripts are the latest script lists of the adapter by catalog host (MON-10).
	HostScripts map[string][]adapterScript
	// CoGroups are the groups of each initiator of what the attempt read: C2 intersects the
	// attempt's permissions with theirs (MSG-7).
	CoGroups [][]string
}

// coHave says whether every initiator of what the attempt read has the capability (MSG-7).
func (g *toolsGrant) coHave(tv *TenantView, cap string) bool {
	for _, gs := range g.CoGroups {
		if !hasCap(tv, groupClosure(tv, gs), cap) {
			return false
		}
	}
	return true
}

// catalogTool is one entry of the P3 catalog of an attempt.
type catalogTool struct {
	Name   string // for the model: <plugin>_<tool>
	Right  string // in rights: <plugin>/<tool>
	Plugin toolspec.Plugin
	Tool   toolspec.Tool
	Schema map[string]any // the input schema with enum_from turned into the catalog values
	// Builtin marks a built-in tool of the core (spec/model/builtin-tools.yaml), which has no
	// plugin.
	Builtin bool
}

// MCP is the tool as the runtime sees it in tools/list.
func (t catalogTool) MCP() map[string]any {
	m := map[string]any{"name": t.Name, "inputSchema": t.Schema}
	if t.Tool.Description != "" {
		m["description"] = t.Tool.Description
	}
	return m
}

// P4Hash is the P4 tool hash of spec/hashing.md: {name, description, input_schema}.
func (t catalogTool) P4Hash() (string, error) {
	m := map[string]any{"name": t.Name, "input_schema": t.Schema}
	if t.Tool.Description != "" {
		m["description"] = t.Tool.Description
	}
	return jcs.Hash(m)
}

// resolveToolsHandle finds the attempt of a tools handle (SEC-3). A handle of a dead lease or
// of another store epoch is fenced (FEN-4).
func (c *Core) resolveToolsHandle(ctx context.Context, q store.Querier, handle string) (*toolsGrant, error) {
	if handle == "" {
		return nil, refuse("handle_invalid", "no tools handle")
	}
	var g toolsGrant
	var epoch int64
	var state string
	var until, now time.Time
	err := q.QueryRow(ctx, `SELECT h.tenant, h.work_id, h.lease_no, h.epoch, h.worker_id, l.state, l.lease_until, botaru_now()
		FROM handles h JOIN leases l ON l.work_id = h.work_id AND l.lease_no = h.lease_no
		WHERE h.hash = ANY($1) AND h.kind = 'tools'`, c.secretHashes(handle)).
		Scan(&g.Tenant, &g.Task, &g.LeaseNo, &epoch, &g.Worker, &state, &until, &now)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, refuse("handle_invalid", "unknown tools handle")
	}
	if err != nil {
		return nil, err
	}
	if epoch != c.epoch || state != "active" || !until.After(now) {
		return nil, refuse("fenced", "the lease of the handle is not alive")
	}
	if err := c.loadGrantTask(ctx, q, &g); err != nil {
		return nil, err
	}
	return &g, nil
}

func (c *Core) loadGrantTask(ctx context.Context, q store.Querier, g *toolsGrant) error {
	var rec []byte
	if err := q.QueryRow(ctx, `SELECT type, initiator, record FROM tasks WHERE id = $1`, g.Task).Scan(&g.Type, &g.Initiator, &rec); err != nil {
		return err
	}
	var r struct {
		MaxTier      int      `json:"max_tier"`
		DataMark     string   `json:"data_mark"`
		Purpose      string   `json:"purpose"`
		CoInitiators []string `json:"co_initiators"`
	}
	if err := json.Unmarshal(rec, &r); err != nil {
		return err
	}
	g.MaxTier, g.Mark, g.Purpose = r.MaxTier, r.DataMark, r.Purpose
	g.CoGroups = nil
	if len(r.CoInitiators) > 0 {
		var now time.Time
		if err := q.QueryRow(ctx, `SELECT botaru_now()`).Scan(&now); err != nil {
			return err
		}
		for _, id := range r.CoInitiators {
			gs, err := rawGroups(ctx, q, now, g.Tenant, id)
			if err != nil {
				return err
			}
			g.CoGroups = append(g.CoGroups, append([]string{}, gs...))
		}
	}
	// The groups of an active initiator: a person, or a service or system principal whose
	// rights to a built-in write tool come only from explicit rows (APR-18).
	if g.Initiator != "" {
		var state string
		err := q.QueryRow(ctx, `SELECT state FROM principals WHERE tenant = $1 AND id = $2`, g.Tenant, g.Initiator).Scan(&state)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if state == "active" {
			rows, err := q.Query(ctx, `SELECT grp FROM memberships WHERE tenant = $1 AND principal = $2 AND state = 'active' AND (until IS NULL OR until > botaru_now()) ORDER BY grp`,
				g.Tenant, g.Initiator)
			if err != nil {
				return err
			}
			for rows.Next() {
				var grp string
				if err := rows.Scan(&grp); err != nil {
					rows.Close()
					return err
				}
				g.InitiatorGroups = append(g.InitiatorGroups, grp)
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}
		}
	}
	if err := q.QueryRow(ctx, `SELECT role FROM workers WHERE id = $1`, g.Worker).Scan(&g.Role); err != nil {
		return err
	}
	var err error
	g.HostScripts, err = c.hostScripts(ctx, q, g.Tenant)
	return err
}

// toolCatalog builds the P3 catalog of an attempt by the assembly steps of the P3 sheet: the
// role's tools (1), read only (2), tier not above the task's max_tier (3), output class not
// above the task's mark (4), description and schema from the manifest (5), "which resource"
// parameters as an enum of the tenant's catalog filtered by the mark (6), in a stable order
// (8). Of the built-in tools of step 7 this build serves request_action (APR-1).
func (c *Core) toolCatalog(cat *Catalog, g *toolsGrant) ([]catalogTool, error) {
	role, ok := cat.Roles[g.Role]
	if !ok {
		return nil, nil
	}
	maxOrder, markOK := c.markOrder(g.Mark)
	rights, maxTier := roleTools(cat, role), g.MaxTier
	// Analysis reads only insight.tools, at tier 1 at most and up to insight.max_data (INS-2).
	insightMax, insightOK := -1, true
	if g.Purpose == "insight" {
		var only []string
		if role.Insight != nil {
			only = roleTools(cat, manifest.Role{Tools: strAny(role.Insight.Tools)})
			if role.Insight.MaxData != "" {
				insightMax, insightOK = c.markOrder(role.Insight.MaxData)
			}
		}
		rights = slices.DeleteFunc(rights, func(r string) bool { return !slices.Contains(only, r) })
		maxTier = min(maxTier, 1)
	}
	var out []catalogTool
	for _, right := range rights {
		plugin, tool, _ := cutTool(right)
		p := cat.Plugins[plugin]
		t, _ := p.Tool(tool)
		if !t.On() || t.Access != "read" || t.Tier > maxTier {
			continue
		}
		if t.OutputClass != "" {
			o, ok := c.markOrder(t.OutputClass)
			if !ok || !markOK || o > maxOrder || insightMax >= 0 && o > insightMax || !insightOK {
				continue
			}
		}
		schema, err := c.modelSchema(cat, g, right, t)
		if err != nil {
			return nil, err
		}
		out = append(out, catalogTool{Name: toolspec.ModelName(plugin, tool), Right: right, Plugin: p, Tool: t, Schema: schema})
	}
	if rt, err := c.requestActionTool(cat, g); err != nil {
		return nil, err
	} else if rt != nil {
		out = append(out, *rt)
	}
	if st := c.serviceControlTool(cat, g); st != nil {
		out = append(out, *st)
	}
	if st := c.scriptRunTool(cat, g); st != nil {
		out = append(out, *st)
	}
	if ct := c.enqueueChildTool(cat, g); ct != nil {
		out = append(out, *ct)
	}
	if ot := c.obsTool(cat, g); ot != nil {
		out = append(out, *ot)
	}
	if ct := c.controlTool(cat, g); ct != nil {
		out = append(out, *ct)
	}
	if it := c.inspectTool(cat, g); it != nil {
		out = append(out, *it)
	}
	if mt, err := c.sendMessageTool(cat, g); err != nil {
		return nil, err
	} else if mt != nil {
		out = append(out, *mt)
	}
	if it := c.inboxReadTool(cat, g); it != nil {
		out = append(out, *it)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// modelSchema is the input schema of a tool for the model: enum_from becomes the catalog
// values filtered by the task's mark, x-botaru-format an assertion (RT-4, TEN-7).
func (c *Core) modelSchema(cat *Catalog, g *toolsGrant, right string, t toolspec.Tool) (map[string]any, error) {
	schema := t.InputSchema()
	names := map[string]bool{}
	enumFromNames(schema, names)
	return c.fullSchema(schema, c.resolveEnumFor(cat, g.Tenant, g.Mark, right, names))
}

// attemptCatalog builds the P3 catalog of the attempt of a task held by a worker, for the P4
// check of the request tools (GW-1) and the run fingerprint (PRV-1).
func (c *Core) attemptCatalog(ctx context.Context, q store.Querier, cat *Catalog, tenant, task, worker string) ([]catalogTool, error) {
	g := &toolsGrant{Tenant: tenant, Task: task, Worker: worker}
	if err := c.loadGrantTask(ctx, q, g); err != nil {
		return nil, err
	}
	return c.toolCatalog(cat, g)
}

// ToolHash is one tool of the P3 catalog with its P4 tool hash.
type ToolHash struct{ Name, Hash string }

// CatalogHashes returns the P4 tool hashes of the P3 catalog of a model handle's attempt in
// catalog order (GW-1).
func (c *Core) CatalogHashes(ctx context.Context, g *ModelGrant) ([]ToolHash, error) {
	cat, err := c.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	tools, err := c.attemptCatalog(ctx, c.st.Pool, cat, g.Tenant, g.Task, g.Worker)
	if err != nil {
		return nil, err
	}
	if err := pinnedCatalog(ctx, c.st.Pool, g.Task, tools); err != nil {
		return nil, err
	}
	out := make([]ToolHash, 0, len(tools))
	for _, t := range tools {
		h, err := t.P4Hash()
		if err != nil {
			return nil, err
		}
		out = append(out, ToolHash{t.Name, h})
	}
	return out, nil
}

// pinnedCatalog refuses with fenced once an apply has changed the P3 catalog of an attempt:
// the run fingerprint fixed at claim would no longer describe its tools (PRV-1). The runtime
// returns the work without spending the attempt, and the next attempt starts with the new
// catalog.
func pinnedCatalog(ctx context.Context, q store.Querier, task string, tools []catalogTool) error {
	var raw []byte
	err := q.QueryRow(ctx, `SELECT attempt FROM leases WHERE work_id = $1 AND state = 'active' ORDER BY lease_no DESC LIMIT 1`, task).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && len(raw) == 0 {
		return nil // no active attempt: the handle check refuses on its own
	}
	if err != nil {
		return err
	}
	var rec attemptRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return err
	}
	var want any
	if len(tools) > 0 {
		h, err := catalogPart(tools)
		if err != nil {
			return err
		}
		want = h
	}
	if rec.Parts["catalog"] != want {
		return refuse("fenced", "the tool catalog changed after the attempt began")
	}
	return nil
}

// catalogPart is the catalog part of the run fingerprint: the P4 tool hashes in ascending
// order of name (spec/hashing.md, PRV-1).
func catalogPart(tools []catalogTool) (string, error) {
	hashes := make([]any, 0, len(tools))
	for _, t := range tools {
		h, err := t.P4Hash()
		if err != nil {
			return "", err
		}
		hashes = append(hashes, h)
	}
	return jcs.Hash(hashes)
}

// P3Discover is server/discover of the gateway profile.
func (c *Core) P3Discover(ctx context.Context, handle string) (*mcp.DiscoverResult, error) {
	if _, err := c.resolveToolsHandle(ctx, c.st.Pool, handle); err != nil {
		return nil, err
	}
	return &mcp.DiscoverResult{
		ProtocolVersion: mcp.Revision, Profile: P3Profile,
		Capabilities: map[string]any{"tools": map[string]any{}},
		ServerInfo:   map[string]any{"name": "botaru-core"},
	}, nil
}

// P3ToolsList is tools/list: the catalog computed by the handle on every request (P3).
func (c *Core) P3ToolsList(ctx context.Context, handle string) (*mcp.ListResult, error) {
	g, err := c.resolveToolsHandle(ctx, c.st.Pool, handle)
	if err != nil {
		return nil, err
	}
	cat, err := c.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	tools, err := c.toolCatalog(cat, g)
	if err != nil {
		return nil, err
	}
	if err := pinnedCatalog(ctx, c.st.Pool, g.Task, tools); err != nil {
		return nil, err
	}
	res := &mcp.ListResult{Tools: []any{}}
	for _, t := range tools {
		res.Tools = append(res.Tools, t.MCP())
	}
	return res, nil
}

// noPermission is the short answer the model gets on a hold and on any refusal by rights; the
// full reason goes to the audit (HLD-1, PRM-8).
const noPermission = "no permission"

// toolRefusal ends a call before it reaches a toolhost.
type toolRefusal struct {
	state  string // denied or error
	reason string
}

// P3ToolsCall is tools/call of a plugin tool: the core checks holds, rights, tier, output class
// and the argument schema in one transaction, computes the target, queues the call and waits
// for its outcome (P3, TEN-3a, DLV-16).
func (c *Core) P3ToolsCall(ctx context.Context, handle string, p mcp.CallParams) (*mcp.CallResult, error) {
	cat, err := c.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	var callID string
	var refusal *toolRefusal
	var builtin *mcp.CallResult
	var grantID string
	var script bool
	err = c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		callID, refusal, builtin, grantID, script = "", nil, nil, "", false
		g, err := c.resolveToolsHandle(ctx, tx, handle)
		if err != nil {
			return err
		}
		// The attempt's lease row is locked, so a lease that ends meanwhile ends after this call
		// is recorded, and the sweep then abandons it (FEN-4).
		if _, err := tx.Exec(ctx, `SELECT 1 FROM leases WHERE work_id = $1 AND lease_no = $2 FOR SHARE`, g.Task, g.LeaseNo); err != nil {
			return err
		}
		tools, err := c.toolCatalog(cat, g)
		if err != nil {
			return err
		}
		if err := pinnedCatalog(ctx, tx, g.Task, tools); err != nil {
			return err
		}
		if p.Name == requestActionName {
			offered := slices.ContainsFunc(tools, func(t catalogTool) bool { return t.Builtin && t.Name == requestActionName })
			args := p.Arguments
			if args == nil {
				args = map[string]any{}
			}
			res, err := c.requestAction(ctx, tx, now, cat, g, offered, args)
			if err != nil {
				return err
			}
			builtin = res.call()
			return nil
		}
		if p.Name == enqueueChildName {
			offered := slices.ContainsFunc(tools, func(t catalogTool) bool { return t.Builtin && t.Name == enqueueChildName })
			args := p.Arguments
			if args == nil {
				args = map[string]any{}
			}
			res, err := c.enqueueChild(ctx, tx, now, cat, g, offered, args)
			if err != nil {
				return err
			}
			builtin = res.call()
			return nil
		}
		if p.Name == obsToolName {
			offered := slices.ContainsFunc(tools, func(t catalogTool) bool { return t.Builtin && t.Name == obsToolName })
			args := p.Arguments
			if args == nil {
				args = map[string]any{}
			}
			builtin, err = c.obsToolCall(ctx, tx, now, cat, g, offered, args)
			return err
		}
		if p.Name == controlToolName {
			offered := slices.ContainsFunc(tools, func(t catalogTool) bool { return t.Builtin && t.Name == controlToolName })
			args := p.Arguments
			if args == nil {
				args = map[string]any{}
			}
			builtin, err = c.controlToolCall(ctx, tx, now, cat, g, offered, args)
			return err
		}
		if msgToolName(p.Name) {
			args := p.Arguments
			if args == nil {
				args = map[string]any{}
			}
			builtin, err = c.msgToolCall(ctx, tx, now, cat, g, p.Name, offeredTool(tools, p.Name), args)
			return err
		}
		if p.Name == inspectToolName {
			offered := slices.ContainsFunc(tools, func(t catalogTool) bool { return t.Builtin && t.Name == inspectToolName })
			args := p.Arguments
			if args == nil {
				args = map[string]any{}
			}
			builtin, err = c.inspectToolCall(ctx, tx, now, cat, g, offered, args)
			return err
		}
		if p.Name == scriptToolName {
			offered := slices.ContainsFunc(tools, func(t catalogTool) bool { return t.Builtin && t.Name == scriptToolName })
			args := p.Arguments
			if args == nil {
				args = map[string]any{}
			}
			res, id, grant, err := c.scriptRun(ctx, tx, now, cat, g, offered, args)
			if errors.Is(err, errSchema) {
				refusal = &toolRefusal{state: "error", reason: "input_schema_invalid"}
				return nil
			}
			if err != nil {
				return err
			}
			if res != nil {
				builtin = res.call()
			}
			callID, grantID, script = id, grant, true
			return nil
		}
		if p.Name == serviceToolName {
			offered := slices.ContainsFunc(tools, func(t catalogTool) bool { return t.Builtin && t.Name == serviceToolName })
			args := p.Arguments
			if args == nil {
				args = map[string]any{}
			}
			res, id, grant, err := c.serviceControl(ctx, tx, now, cat, g, offered, args)
			if errors.Is(err, errSchema) {
				refusal = &toolRefusal{state: "error", reason: "input_schema_invalid"}
				return nil
			}
			if err != nil {
				return err
			}
			if res != nil {
				builtin = res.call()
			}
			callID, grantID = id, grant
			return nil
		}
		callID, refusal, err = c.recordToolCall(ctx, tx, now, cat, g, tools, p)
		if err != nil || refusal != nil || callID == "" {
			return err
		}
		// A seat attempt's call goes into the read log (MGR-28).
		plugin, tool, _ := strings.Cut(p.Name, "_")
		return c.seatToolRead(ctx, tx, now, g, toolspec.RightName(plugin, tool), p.Arguments)
	})
	if err != nil {
		return nil, err
	}
	if builtin != nil {
		return builtin, nil
	}
	if refusal != nil && refusal.reason == "budget_exhausted" {
		return nil, refuse("budget_exhausted", "the tool call does not fit the budget")
	}
	if refusal != nil {
		return toolError(refusal), nil
	}
	if grantID != "" {
		// A grant call is a write call: the model gets only the closed outcome (APR-18).
		if _, err := c.awaitToolCall(ctx, cat, callID); err != nil {
			return nil, err
		}
		if script {
			return c.scriptOutcome(ctx, callID, grantID)
		}
		return c.serviceOutcome(ctx, callID, grantID)
	}
	return c.awaitToolCall(ctx, cat, callID)
}

// recordToolCall checks a call and writes its record: queued, or denied or error at once.
func (c *Core) recordToolCall(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, g *toolsGrant, tools []catalogTool, p mcp.CallParams) (string, *toolRefusal, error) {
	role := cat.Roles[g.Role]
	plugin, tool, _ := strings.Cut(p.Name, "_")
	// A name of the model maps back to <plugin>/<tool>; plugin names have no underscore
	// (tool_ref), so the first underscore separates them.
	right := toolspec.RightName(plugin, tool)
	pl, havePlugin := cat.Plugins[plugin]
	t, haveTool := pl.Tool(tool)
	id := ids.New(ids.Call)
	args := p.Arguments
	if args == nil {
		args = map[string]any{}
	}
	deny := func(state, reason string) (string, *toolRefusal, error) {
		access := "read"
		if haveTool && t.Access == "write" {
			access = "write"
		}
		if err := c.insertToolCall(ctx, tx, now, toolCallRow{
			ID: id, Tenant: g.Tenant, Task: g.Task, LeaseNo: g.LeaseNo, Tool: right, Access: access, Args: args,
			Plugin: plugin, Placement: pl.Placement, State: state, Reason: reason, Untrusted: haveTool && !t.TrustedOutput(),
		}); err != nil {
			return "", nil, err
		}
		if _, err := c.appendAudit(ctx, tx, now, toolAudit(g, id, right, false, reason)); err != nil {
			return "", nil, err
		}
		return id, &toolRefusal{state: state, reason: reason}, nil
	}
	if !havePlugin || !haveTool || !containsString(roleTools(cat, role), right) || !t.On() {
		return deny("denied", "forbidden")
	}
	// Analysis calls only what its catalog offers: insight.tools, tier 1 at most and up to
	// insight.max_data (INS-2).
	if g.Purpose == "insight" && !slices.ContainsFunc(tools, func(ct catalogTool) bool { return !ct.Builtin && ct.Right == right }) {
		return deny("denied", "forbidden")
	}
	// Holds come before rights (HLD-1): no_tool on the farm, the tenant, the role, the type, the
	// plugin or the agent worker.
	held, err := heldBy(ctx, tx, now, []string{"no_tool"}, append(tenantScopes(g.Tenant),
		scopeRef{kind: "role", tenant: g.Tenant, name: g.Role}, scopeRef{kind: "tasktype", tenant: g.Tenant, name: g.Type},
		scopeRef{kind: "plugin", tenant: g.Tenant, name: plugin}, scopeRef{kind: "worker", tenant: g.Tenant, name: strings.ToLower(g.Worker)})...)
	if err != nil {
		return "", nil, err
	}
	if held != "" {
		return deny("denied", "held")
	}
	if t.Access != "read" {
		return deny("denied", "forbidden") // write tools are never called by the model (APR-1)
	}
	if t.Tier > g.MaxTier {
		return deny("denied", "tier_exceeded")
	}
	if t.OutputClass != "" {
		maxOrder, markOK := c.markOrder(g.Mark)
		if o, ok := c.markOrder(t.OutputClass); !ok || !markOK || o > maxOrder {
			return deny("denied", "data_class_exceeded")
		}
	}
	// The arguments pass the closed schema with the catalog values of the task's mark (INJ-1,
	// TEN-7); a default of the manifest is filled in before the check.
	schema, err := c.modelSchema(cat, g, right, t)
	if err != nil {
		return "", nil, err
	}
	args = withDefaults(t, args)
	if err := c.rs.validate(schema, args); err != nil {
		return deny("denied", "input_schema_invalid")
	}
	target, resolved, err := c.callTarget(cat, g.Tenant, right, t, args)
	if err != nil {
		return deny("denied", "input_schema_invalid")
	}
	// A no_tool hold on the target machine or resource, known only now (HLD-1).
	if held, err := heldBy(ctx, tx, now, []string{"no_tool"}, targetScopes(g.Tenant, target)...); err != nil {
		return "", nil, err
	} else if held != "" {
		return deny("denied", "held")
	}
	host := target.host
	row := toolCallRow{
		ID: id, Tenant: g.Tenant, Task: g.Task, LeaseNo: g.LeaseNo, Tool: right, Access: "read", Args: args,
		Plugin: plugin, Placement: pl.Placement, Host: host, Resource: target.resource, Resolved: resolved, Untrusted: !t.TrustedOutput(), State: "queued",
		DefHash: toolDefHash(pl.Placement, t),
	}
	// A target without a live toolhost ends at once (DLV-16, OBS-10).
	live, err := c.liveToolhost(ctx, tx, now, cat, g.Tenant, plugin, pl.Placement, host)
	if err != nil {
		return "", nil, err
	}
	if !live {
		row.State, row.Reason = "error", "plugin_unavailable"
		if err := c.insertToolCall(ctx, tx, now, row); err != nil {
			return "", nil, err
		}
		if _, err := c.appendAudit(ctx, tx, now, toolAudit(g, id, right, true, "plugin_unavailable")); err != nil {
			return "", nil, err
		}
		return id, &toolRefusal{state: "error", reason: "plugin_unavailable"}, nil
	}
	// The call goes only after a reserve at its manifest price (BUD-2, BUD-5). A call that does
	// not fit a limit ends the attempt with budget_exhausted, like a model call that does not.
	if err := c.reserveToolCall(ctx, tx, now, cat, g, id, toolPrice(t.Billing, t.PriceEUR)); err != nil {
		var p *Problem
		if !errors.As(err, &p) || p.Reason != "budget_exhausted" {
			return "", nil, err
		}
		row.State, row.Reason = "error", "budget_exhausted"
		if err := c.insertToolCall(ctx, tx, now, row); err != nil {
			return "", nil, err
		}
		if _, err := c.appendAudit(ctx, tx, now, toolAudit(g, id, right, true, "budget_exhausted")); err != nil {
			return "", nil, err
		}
		return id, &toolRefusal{state: "error", reason: "budget_exhausted"}, nil
	}
	startBy := maxToolStartBy
	if d, ok := parseDuration(cat.TaskTypes[g.Type].Timeouts.ToolStartBy); ok && d < startBy {
		startBy = d
	}
	row.StartBy = now.Add(startBy)
	if err := c.insertToolCall(ctx, tx, now, row); err != nil {
		return "", nil, err
	}
	if _, err := c.appendAudit(ctx, tx, now, toolAudit(g, id, right, true, "")); err != nil {
		return "", nil, err
	}
	return id, nil, notifyWork(ctx, tx)
}

// targetScopes are the hold scopes of a call's target: its machine and its catalog resource.
func targetScopes(tenant string, t callTargetRef) []scopeRef {
	var out []scopeRef
	if t.host != "" {
		out = append(out, scopeRef{kind: "host", tenant: tenant, name: t.host})
	}
	if t.resource != "" {
		out = append(out, scopeRef{kind: "resource", tenant: tenant, name: t.resource})
	}
	return out
}

func toolAudit(g *toolsGrant, id, right string, ok bool, reason string) Audit {
	return Audit{
		Tenant: g.Tenant, Kind: "authorize", Actor: Actor{Principal: workerPrincipal(g.Worker), Via: "bot", Role: g.Role, Task: g.Task},
		Command: "tool.call", Target: &Target{Kind: "toolcall", ID: id}, OK: ok, Reason: reason,
		After: map[string]any{"tool": right},
	}
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// withDefaults fills the defaults of the manifest parameters that the call left out.
func withDefaults(t toolspec.Tool, args map[string]any) map[string]any {
	out := make(map[string]any, len(args))
	for k, v := range args {
		out[k] = v
	}
	for k, s := range t.Params {
		if _, ok := out[k]; !ok {
			if d, ok := s["default"]; ok {
				out[k] = d
			}
		}
	}
	return out
}

// callTargetRef is the machine of a call and the catalog resource of its target value.
type callTargetRef struct{ host, resource string }

// callTarget computes the machine of a call from the argument named in target_from: a machine
// of catalog.hosts, the machine of the service of catalog.services, or the machine of the
// catalog resource whose list holds the value; a tool with target: any has no machine
// (TEN-3a); only a resource whose read_by holds the tool counts. It also resolves the catalog fields a plugin needs, such as the unit of a service.
func (c *Core) callTarget(cat *Catalog, tenant, right string, t toolspec.Tool, args map[string]any) (callTargetRef, map[string]string, error) {
	spec, _ := tenantCatalog(cat, tenant)
	resolved := map[string]string{}
	params := t.AllParams()
	for name, s := range params {
		if s["enum_from"] != "catalog.services" {
			continue
		}
		v, ok := args[name].(string)
		if !ok {
			continue
		}
		if svc, _ := findService(spec, v); svc != nil {
			if u, ok := svc["unit"].(string); ok {
				resolved[name+".unit"] = u
			}
		}
	}
	if t.Target == "any" {
		return callTargetRef{}, resolved, nil
	}
	v, ok := args[t.TargetFrom].(string)
	if !ok {
		return callTargetRef{}, nil, errors.New("no target argument")
	}
	switch params[t.TargetFrom]["enum_from"] {
	case "catalog.hosts":
		return callTargetRef{host: v}, resolved, nil
	case "catalog.services":
		if _, host := findService(spec, v); host != "" {
			return callTargetRef{host: host}, resolved, nil
		}
		return callTargetRef{}, nil, errors.New("unknown service")
	}
	list, _ := strings.CutPrefix(asString(params[t.TargetFrom]["enum_from"]), "catalog.")
	res, _ := spec["resources"].([]any)
	var found []callTargetRef
	for _, r := range res {
		rm, _ := r.(map[string]any)
		if rm["list"] != list || right != "" && !readBy(rm, right) {
			continue
		}
		vals, _ := rm["values"].([]any)
		for _, x := range vals {
			if x == v {
				h, _ := rm["host"].(string)
				id, _ := rm["id"].(string)
				found = append(found, callTargetRef{host: h, resource: id})
				break
			}
		}
	}
	// A value in two resources of the list does not say which one is meant, so it has no
	// target rather than the first one found.
	if len(found) != 1 || found[0].host == "" {
		return callTargetRef{}, nil, errors.New("the target value has no single machine")
	}
	return found[0], resolved, nil
}

// callTargetAny is callTarget for a write call of an action: its target values come from the
// action's catalog lists, whatever tools read them.
func (c *Core) callTargetAny(cat *Catalog, tenant string, t toolspec.Tool, args map[string]any) (callTargetRef, map[string]string, error) {
	return c.callTarget(cat, tenant, "", t, args)
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}

// findService returns a service of the catalog and its machine.
func findService(spec map[string]any, name string) (map[string]any, string) {
	hosts, _ := spec["hosts"].([]any)
	for _, h := range hosts {
		hm, _ := h.(map[string]any)
		svcs, _ := hm["services"].([]any)
		for _, s := range svcs {
			sm, _ := s.(map[string]any)
			if sm["name"] == name {
				host, _ := hm["id"].(string)
				return sm, host
			}
		}
	}
	return nil, ""
}

// liveToolhost says whether an active toolhost of the target was seen within staleAfter and
// still matches the plugin manifests: a toolhost whose hello fingerprint no longer matches
// after an apply takes no calls until it says hello again (INJ-2).
func (c *Core) liveToolhost(ctx context.Context, q store.Querier, now time.Time, cat *Catalog, tenant, plugin, placement, host string) (bool, error) {
	rows, err := q.Query(ctx, `SELECT plugins, hello FROM workers WHERE tenant = $1 AND kind = 'toolhost' AND state = 'active'
		AND plugins ? $2 AND NOT plugins_down ? $2 AND placement = $3 AND ($4 = '' OR host = $4) AND last_seen > $5`,
		tenant, plugin, placement, host, now.Add(-staleAfter))
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var plugins, hello []byte
		if err := rows.Scan(&plugins, &hello); err != nil {
			return false, err
		}
		w := Worker{Placement: placement}
		var h HelloRequest
		if json.Unmarshal(plugins, &w.Plugins) != nil || json.Unmarshal(hello, &h) != nil {
			continue
		}
		if toolhostMismatch(cat, &w, h) == "" {
			return true, nil
		}
	}
	return false, rows.Err()
}

func toolError(r *toolRefusal) *mcp.CallResult {
	text := "error: " + r.reason
	if r.state == "denied" {
		text = noPermission
	}
	return &mcp.CallResult{IsError: true, Content: []mcp.Content{{Type: "text", Text: text}}}
}

// awaitToolCall waits until the call ends and builds the answer for the model: the output as
// a separate content block, untrusted text wrapped in a mark with a random value per call
// (INJ-4). The wait is bounded by start_by, the tool deadline and the lease; the background
// loops end a call that overstays them.
func (c *Core) awaitToolCall(ctx context.Context, cat *Catalog, id string) (*mcp.CallResult, error) {
	for {
		woken := c.wake.wait()
		var state string
		var reason *string
		var output []byte
		var untrusted bool
		err := c.st.Pool.QueryRow(ctx, `SELECT state, reason, output, untrusted_output FROM toolcalls WHERE id = $1`, id).Scan(&state, &reason, &output, &untrusted)
		if err != nil {
			return nil, err
		}
		switch state {
		case "done":
			return toolOutput(output, untrusted), nil
		case "error", "denied", "exec_unknown":
			return toolError(&toolRefusal{state: state, reason: deref(reason)}), nil
		case "abandoned":
			return nil, refuse("fenced", "the attempt lost its lease")
		}
		t := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, ctx.Err()
		case <-woken:
		case <-t.C:
		}
		t.Stop()
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// toolOutput turns a stored outcome into the tool result for the model.
func toolOutput(raw []byte, untrusted bool) *mcp.CallResult {
	var o mcp.CallResult
	_ = json.Unmarshal(raw, &o)
	res := &mcp.CallResult{}
	if !untrusted {
		// Structured content goes back only when the output schema checked it; untrusted
		// output reaches the runtime only inside the mark (INJ-4).
		res.StructuredContent = o.StructuredContent
	}
	var text strings.Builder
	for _, b := range o.Content {
		if b.Type == "text" {
			text.WriteString(b.Text)
		}
	}
	body := text.String()
	// Trusted output is only the structured content the output schema checked; the plugin's
	// text is not shown (INJ-3).
	if (body == "" || !untrusted) && o.StructuredContent != nil {
		b, _ := json.Marshal(o.StructuredContent)
		body = string(b)
	}
	if untrusted {
		mark := "untrusted-" + ids.Secret(9)
		body = "<" + mark + ">\n" + body + "\n</" + mark + ">"
	}
	res.Content = []mcp.Content{{Type: "text", Text: body}}
	return res
}
