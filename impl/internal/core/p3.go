// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/ids"
	"github.com/HunterEwok/botaru/impl/internal/jcs"
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
}

// catalogTool is one entry of the P3 catalog of an attempt.
type catalogTool struct {
	Name   string // for the model: <plugin>_<tool>
	Right  string // in rights: <plugin>/<tool>
	Plugin toolspec.Plugin
	Tool   toolspec.Tool
	Schema map[string]any // the input schema with enum_from turned into the catalog values
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
		WHERE h.hash = $1 AND h.kind = 'tools'`, c.hashSecret(handle)).
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
		MaxTier  int    `json:"max_tier"`
		DataMark string `json:"data_mark"`
	}
	if err := json.Unmarshal(rec, &r); err != nil {
		return err
	}
	g.MaxTier, g.Mark = r.MaxTier, r.DataMark
	return q.QueryRow(ctx, `SELECT role FROM workers WHERE id = $1`, g.Worker).Scan(&g.Role)
}

// toolCatalog builds the P3 catalog of an attempt by the assembly steps of the P3 sheet: the
// role's tools (1), read only (2), tier not above the task's max_tier (3), output class not
// above the task's mark (4), description and schema from the manifest (5), "which resource"
// parameters as an enum of the tenant's catalog filtered by the mark (6), in a stable order
// (8). The built-in tools of step 7 are not served by this build.
func (c *Core) toolCatalog(cat *Catalog, g *toolsGrant) ([]catalogTool, error) {
	role, ok := cat.Roles[g.Role]
	if !ok {
		return nil, nil
	}
	maxOrder, markOK := c.markOrder(g.Mark)
	var out []catalogTool
	for _, right := range roleTools(cat, role) {
		plugin, tool, _ := cutTool(right)
		p := cat.Plugins[plugin]
		t, _ := p.Tool(tool)
		if !t.On() || t.Access != "read" || t.Tier > g.MaxTier {
			continue
		}
		if t.OutputClass != "" {
			o, ok := c.markOrder(t.OutputClass)
			if !ok || !markOK || o > maxOrder {
				continue
			}
		}
		schema, err := c.modelSchema(cat, g, t)
		if err != nil {
			return nil, err
		}
		out = append(out, catalogTool{Name: toolspec.ModelName(plugin, tool), Right: right, Plugin: p, Tool: t, Schema: schema})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// modelSchema is the input schema of a tool for the model: enum_from becomes the catalog
// values filtered by the task's mark, x-botaru-format an assertion (RT-4, TEN-7).
func (c *Core) modelSchema(cat *Catalog, g *toolsGrant, t toolspec.Tool) (map[string]any, error) {
	schema := t.InputSchema()
	names := map[string]bool{}
	enumFromNames(schema, names)
	return c.fullSchema(schema, c.resolveEnumFrom(cat, g.Tenant, g.Mark, names))
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

// CatalogHashes returns the P4 tool hashes of the P3 catalog of a model handle's attempt by
// tool name (GW-1).
func (c *Core) CatalogHashes(ctx context.Context, g *ModelGrant) (map[string]string, error) {
	cat, err := c.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	tools, err := c.attemptCatalog(ctx, c.st.Pool, cat, g.Tenant, g.Task, g.Worker)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, t := range tools {
		h, err := t.P4Hash()
		if err != nil {
			return nil, err
		}
		out[t.Name] = h
	}
	return out, nil
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
	err = c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		callID, refusal = "", nil
		g, err := c.resolveToolsHandle(ctx, tx, handle)
		if err != nil {
			return err
		}
		// The attempt's lease row is locked, so a lease that ends meanwhile ends after this call
		// is recorded, and the sweep then abandons it (FEN-4).
		if _, err := tx.Exec(ctx, `SELECT 1 FROM leases WHERE work_id = $1 AND lease_no = $2 FOR SHARE`, g.Task, g.LeaseNo); err != nil {
			return err
		}
		callID, refusal, err = c.recordToolCall(ctx, tx, now, cat, g, p)
		return err
	})
	if err != nil {
		return nil, err
	}
	if refusal != nil {
		return toolError(refusal), nil
	}
	return c.awaitToolCall(ctx, cat, callID)
}

// recordToolCall checks a call and writes its record: queued, or denied or error at once.
func (c *Core) recordToolCall(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, g *toolsGrant, p mcp.CallParams) (string, *toolRefusal, error) {
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
			Plugin: plugin, Placement: pl.Placement, State: state, Reason: reason, Untrusted: haveTool && t.Untrusted(),
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
	schema, err := c.modelSchema(cat, g, t)
	if err != nil {
		return "", nil, err
	}
	args = withDefaults(t, args)
	if err := c.rs.validate(schema, args); err != nil {
		return deny("denied", "input_schema_invalid")
	}
	host, resolved, err := c.callTarget(cat, g.Tenant, t, args)
	if err != nil {
		return deny("denied", "input_schema_invalid")
	}
	row := toolCallRow{
		ID: id, Tenant: g.Tenant, Task: g.Task, LeaseNo: g.LeaseNo, Tool: right, Access: "read", Args: args,
		Plugin: plugin, Placement: pl.Placement, Host: host, Resolved: resolved, Untrusted: t.Untrusted(), State: "queued",
	}
	// A target without a live toolhost ends at once (DLV-16, OBS-10).
	live, err := c.liveToolhost(ctx, tx, now, g.Tenant, plugin, pl.Placement, host)
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

// callTarget computes the machine of a call from the argument named in target_from: a machine
// of catalog.hosts, the machine of the service of catalog.services, or the machine of the
// catalog resource whose list holds the value; a tool with target: any has no machine
// (TEN-3a). It also resolves the catalog fields a plugin needs, such as the unit of a service.
func (c *Core) callTarget(cat *Catalog, tenant string, t toolspec.Tool, args map[string]any) (string, map[string]string, error) {
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
		return "", resolved, nil
	}
	v, ok := args[t.TargetFrom].(string)
	if !ok {
		return "", nil, errors.New("no target argument")
	}
	switch params[t.TargetFrom]["enum_from"] {
	case "catalog.hosts":
		return v, resolved, nil
	case "catalog.services":
		if _, host := findService(spec, v); host != "" {
			return host, resolved, nil
		}
		return "", nil, errors.New("unknown service")
	}
	list, _ := strings.CutPrefix(asString(params[t.TargetFrom]["enum_from"]), "catalog.")
	res, _ := spec["resources"].([]any)
	for _, r := range res {
		rm, _ := r.(map[string]any)
		if rm["list"] != list {
			continue
		}
		vals, _ := rm["values"].([]any)
		for _, x := range vals {
			if x == v {
				if h, ok := rm["host"].(string); ok {
					return h, resolved, nil
				}
			}
		}
	}
	return "", nil, errors.New("the target value has no machine")
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

// liveToolhost says whether an active toolhost of the target was seen within staleAfter.
func (c *Core) liveToolhost(ctx context.Context, q store.Querier, now time.Time, tenant, plugin, placement, host string) (bool, error) {
	var n int
	err := q.QueryRow(ctx, `SELECT count(*) FROM workers WHERE tenant = $1 AND kind = 'toolhost' AND state = 'active'
		AND plugins ? $2 AND placement = $3 AND ($4 = '' OR host = $4) AND last_seen > $5`,
		tenant, plugin, placement, host, now.Add(-staleAfter)).Scan(&n)
	return n > 0, err
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
	res := &mcp.CallResult{StructuredContent: o.StructuredContent}
	var text strings.Builder
	for _, b := range o.Content {
		if b.Type == "text" {
			text.WriteString(b.Text)
		}
	}
	body := text.String()
	if body == "" && o.StructuredContent != nil {
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
