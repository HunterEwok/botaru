// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/ids"
	"github.com/HunterEwok/botaru/impl/internal/jcs"
	"github.com/HunterEwok/botaru/impl/internal/mcp"
	"github.com/HunterEwok/botaru/impl/internal/redact"
	"github.com/HunterEwok/botaru/impl/internal/store"
	"github.com/HunterEwok/botaru/impl/internal/toolspec"
)

// The monitoring system into the farm (MON-5, MON-10): problems on catalog hosts become farm
// alarms by the class table, the adapter's script lists close the values of monitor/script_run,
// and a script runs once through a monitor_script delivery of the adapter.
const (
	scriptToolName = "monitor_script_run" // the model name of the built-in monitor/script_run
	scriptRight    = "monitor/script_run"
	scriptCap      = "monitor.script_run"
	scriptPlugin   = "monitor"
	// scriptPlacement is the placement of a script call: no toolhost serves it, the adapter
	// runs it through its delivery.
	scriptPlacement = "e5"
	// scriptPerHour is the limit of runs of one script on one host in an hour (MON-10).
	scriptPerHour = 6
	// scriptTimeout is the time limit of a script at the monitoring system; scriptStartBy the
	// time an adapter has to take the run.
	scriptTimeout = 60
	scriptStartBy = 5 * time.Minute
	// scriptOutputMax is the cut of the script output kept in the task journal.
	scriptOutputMax = 4096
)

// monitorClasses is the class table of spec/model/monitor-map.yaml: the class of a catalog
// trigger and the farm alarm it raises. A class outside the table, and a trigger outside the
// catalog, raise alarm.monitor.problem.
var monitorClasses = map[string]string{"disk_free": "disk.free.low", "disk_fill": "disk.fill.rate"}

var eventItem = regexp.MustCompile(`^[A-Za-z0-9_.\[\],-]+$`)

// monitorHost is a host of the tenant catalog bound to a node of the monitoring system, with
// the scripts the catalog allows on it.
type monitorHost struct {
	ID, Node string
	Scripts  []allowedScript
}

type allowedScript struct {
	ScriptID, Name string
	Choices        []string
}

// adapterScript is one script of the adapter's latest list for a host.
type adapterScript struct {
	ScriptID string   `json:"scriptid"`
	Input    string   `json:"input"`
	Choices  []string `json:"choices,omitempty"`
}

// monitorHosts are the hosts of the tenant catalog with a monitor_host, by host id.
func monitorHosts(cat *Catalog, tenant string) map[string]monitorHost {
	spec, _ := tenantCatalog(cat, tenant)
	hosts, _ := spec["hosts"].([]any)
	out := map[string]monitorHost{}
	for _, h := range hosts {
		hm, _ := h.(map[string]any)
		node := asString(hm["monitor_host"])
		if node == "" {
			continue
		}
		mh := monitorHost{ID: asString(hm["id"]), Node: node}
		scripts, _ := hm["monitor_scripts_allowed"].([]any)
		for _, s := range scripts {
			sm, _ := s.(map[string]any)
			// A script is allowed on the nodes its row names; none named means this one.
			if nodes := strList(sm["hosts"]); len(nodes) > 0 && !slices.Contains(nodes, node) {
				continue
			}
			mh.Scripts = append(mh.Scripts, allowedScript{ScriptID: asString(sm["scriptid"]), Name: asString(sm["name"]), Choices: strList(sm["choices"])})
		}
		out[mh.ID] = mh
	}
	return out
}

// monitorTrigger is the class and item of a catalog trigger.
func monitorTrigger(cat *Catalog, tenant, triggerid string) (string, string, bool) {
	spec, _ := tenantCatalog(cat, tenant)
	rows, _ := spec["monitor_triggers"].([]any)
	for _, r := range rows {
		rm, _ := r.(map[string]any)
		if asString(rm["triggerid"]) == triggerid {
			return asString(rm["class"]), asString(rm["item"]), true
		}
	}
	return "", "", false
}

// hostScripts are the latest adapter lists of the tenant by host.
func (c *Core) hostScripts(ctx context.Context, q store.Querier, tenant string) (map[string][]adapterScript, error) {
	if !c.monitorOn() {
		return nil, nil
	}
	rows, err := q.Query(ctx, `SELECT host, scripts FROM monitor_host_scripts WHERE tenant = $1`, tenant)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]adapterScript{}
	for rows.Next() {
		var host string
		var raw []byte
		if err := rows.Scan(&host, &raw); err != nil {
			return nil, err
		}
		var list []adapterScript
		if err := json.Unmarshal(raw, &list); err != nil {
			return nil, err
		}
		out[host] = list
	}
	return out, rows.Err()
}

// runnable is a catalog script the model may name on a host: its scriptid is in the adapter's
// latest list for the host, it takes no free input, and a choice list in the script is the
// catalog's list (MON-10, INJ-16).
func runnable(s allowedScript, list []adapterScript) bool {
	for _, a := range list {
		if a.ScriptID != s.ScriptID {
			continue
		}
		switch a.Input {
		case "none":
			return len(s.Choices) == 0
		case "list":
			x, y := slices.Clone(a.Choices), slices.Clone(s.Choices)
			slices.Sort(x)
			slices.Sort(y)
			return len(y) > 0 && slices.Equal(x, y)
		}
		return false
	}
	return false
}

func roleRunsScript(cat *Catalog, role, script string) bool {
	r, ok := cat.Roles[role]
	return ok && (slices.Contains(r.MonitorScripts, "*") || slices.Contains(r.MonitorScripts, script))
}

// runnableFor are the hosts and scripts the role of a grant may run, by host.
func runnableFor(cat *Catalog, g *toolsGrant) map[string][]allowedScript {
	out := map[string][]allowedScript{}
	for id, h := range monitorHosts(cat, g.Tenant) {
		for _, s := range h.Scripts {
			if roleRunsScript(cat, g.Role, s.Name) && runnable(s, g.HostScripts[id]) {
				out[id] = append(out[id], s)
			}
		}
	}
	return out
}

// scriptRunTool is the built-in monitor/script_run: only with the monitor profile, for a role
// with monitor_scripts, in a work task of tier 1 and above, with the hosts and the names of
// the catalog as an enum (MON-10, APR-18, PRF-1).
func (c *Core) scriptRunTool(cat *Catalog, g *toolsGrant) *catalogTool {
	role, ok := cat.Roles[g.Role]
	if !c.monitorOn() || !ok || len(role.MonitorScripts) == 0 || g.MaxTier < 1 {
		return nil
	}
	if p := role.Profile; p != "" && p != "worker" && p != "manager" && p != "farm-manager" {
		return nil
	}
	if g.Purpose != "" && g.Purpose != "work" && g.Purpose != "manage" {
		return nil
	}
	byHost := runnableFor(cat, g)
	var hosts, names, choices []any
	var desc strings.Builder
	desc.WriteString("Run a script of the monitoring system on a host of the tenant catalog. A listed script runs at once; after reading untrusted text it is refused. The output goes to the task journal, the result is a code. Scripts by host:")
	for _, h := range slices.Sorted(mapKeys(byHost)) {
		hosts = append(hosts, h)
		var line []string
		for _, s := range byHost[h] {
			if !slices.Contains(names, any(s.Name)) {
				names = append(names, s.Name)
			}
			entry := s.Name
			if len(s.Choices) > 0 {
				entry += " (input: " + strings.Join(s.Choices, ", ") + ")"
				for _, ch := range s.Choices {
					if !slices.Contains(choices, any(ch)) {
						choices = append(choices, ch)
					}
				}
			}
			line = append(line, entry)
		}
		desc.WriteString("\n- " + h + ": " + strings.Join(line, ", "))
	}
	if len(hosts) == 0 {
		return nil
	}
	props := map[string]any{
		"resource": map[string]any{"type": "string", "enum": hosts},
		"script":   map[string]any{"type": "string", "enum": names},
	}
	if len(choices) > 0 {
		props["input"] = map[string]any{"type": "string", "enum": choices}
	}
	schema := map[string]any{"type": "object", "additionalProperties": false, "required": []any{"resource", "script"}, "properties": props}
	return &catalogTool{Name: scriptToolName, Right: scriptRight, Builtin: true, Tool: toolspec.Tool{Description: desc.String()}, Schema: schema}
}

// scriptRun is the call of the built-in monitor/script_run from a bot (MON-10, D-73, INJ-15).
// It returns a result at once, or the write call of a grant that the caller waits for.
func (c *Core) scriptRun(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, g *toolsGrant, offered bool, args map[string]any) (*serviceResult, string, string, error) {
	tv := cat.Tenants[g.Tenant]
	actor := Actor{Principal: workerPrincipal(g.Worker), Via: "bot", Role: g.Role, Task: g.Task}
	audit := func(ok bool, reason, id string) error {
		after := map[string]any{"resource": args["resource"], "script": args["script"]}
		if id != "" {
			after["ref"] = id
		}
		_, err := c.appendAudit(ctx, tx, now, Audit{
			Tenant: g.Tenant, Kind: "authorize", Actor: actor, Command: scriptRight, Target: &Target{Kind: "tool", ID: scriptRight}, OK: ok, Reason: reason, After: after,
		})
		return err
	}
	deny := func(code, reason string, needs ...string) (*serviceResult, string, string, error) {
		if err := audit(false, reason, ""); err != nil {
			return nil, "", "", err
		}
		return &serviceResult{Code: code, Needs: needs}, "", "", nil
	}
	if !offered {
		return deny("forbidden", "forbidden")
	}
	hostID, _ := args["resource"].(string)
	name, _ := args["script"].(string)
	input, hasInput := args["input"].(string)
	host, ok := monitorHosts(cat, g.Tenant)[hostID]
	var script *allowedScript
	if ok {
		for _, s := range runnableFor(cat, g)[hostID] {
			if s.Name == name {
				script = &s
				break
			}
		}
	}
	// The input is a value of the script's own list, and only for a script with a list.
	badInput := script != nil && (len(script.Choices) == 0 && (hasInput || args["input"] != nil) ||
		len(script.Choices) > 0 && !slices.Contains(script.Choices, input))
	if script == nil || badInput || len(args) > 3 || len(args) == 3 && !hasInput {
		if err := audit(false, "input_schema_invalid", ""); err != nil {
			return nil, "", "", err
		}
		return nil, "", "", errSchema
	}
	if g.Initiator != "" && !hasCap(tv, groupClosure(tv, g.InitiatorGroups), scriptCap) {
		return deny("forbidden", "forbidden", scriptCap)
	}
	var rec []byte
	if err := tx.QueryRow(ctx, `SELECT record FROM tasks WHERE id = $1`, g.Task).Scan(&rec); err != nil {
		return nil, "", "", err
	}
	var tr struct {
		UntrustedSeen bool `json:"untrusted_seen"`
	}
	if err := json.Unmarshal(rec, &tr); err != nil {
		return nil, "", "", err
	}
	// After untrusted text a script would need a person's decision; v0.1 has no card for a
	// script, so the call is refused (INJ-15).
	if tr.UntrustedSeen {
		return deny("forbidden", "untrusted_seen")
	}
	if code, err := c.scriptGuards(ctx, tx, now, g, hostID, name); err != nil {
		return nil, "", "", err
	} else if code != "" {
		if code == "held" {
			return deny("forbidden", "held")
		}
		return deny(code, code)
	}
	callArgs := map[string]any{"resource": hostID, "script": name}
	if hasInput {
		callArgs["input"] = input
	}
	argsHash, err := jcs.Hash(callArgs)
	if err != nil {
		return nil, "", "", err
	}
	grant, err := c.writeGrant(ctx, tx, now, g.Tenant,
		map[string]any{"kind": "bot", "principal": workerPrincipal(g.Worker), "role": g.Role, "task": g.Task}, actor,
		map[string]any{"resource": hostID, "script": name}, argsHash)
	if err != nil {
		return nil, "", "", err
	}
	callID := ids.New(ids.Call)
	if err := c.insertToolCall(ctx, tx, now, toolCallRow{
		ID: callID, Tenant: g.Tenant, Task: g.Task, Tool: scriptRight, Access: "write", Args: callArgs, Plugin: scriptPlugin, Placement: scriptPlacement,
		Host: hostID, Untrusted: false, State: "queued", StartBy: now.Add(scriptStartBy), ArgsHash: argsHash, GrantID: grant,
	}); err != nil {
		return nil, "", "", err
	}
	body := map[string]any{"resource": hostID, "monitor_host": host.Node, "scriptid": script.ScriptID, "bot_id": g.Worker, "grant_id": grant, "timeout_s": scriptTimeout}
	if hasInput {
		body["input"] = input
	}
	if err := c.spec.Validate("monitor-script", body); err != nil {
		return nil, "", "", err
	}
	delivery, err := c.planDelivery(ctx, tx, now, plannedDelivery{
		Tenant: g.Tenant, Task: g.Task, Kind: "monitor_script", Addr: "monitor", TargetKind: "monitor", TargetID: c.monitorTarget(),
		ExecKind: "monitor", ExecName: c.monitorTarget(), DataOrigin: "code", Body: body,
	})
	if err != nil {
		return nil, "", "", err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO monitor_script_calls (delivery, toolcall, tenant, task, host, script, bot_id, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, delivery, callID, g.Tenant, g.Task, hostID, name, g.Worker, now); err != nil {
		return nil, "", "", err
	}
	if err := audit(true, "", grant); err != nil {
		return nil, "", "", err
	}
	return nil, callID, grant, nil
}

// scriptGuards checks the holds, the rate limits and one script per host at a time (MON-10,
// MON-8, HLD-3). It returns a refusal code, or "".
func (c *Core) scriptGuards(ctx context.Context, tx pgx.Tx, now time.Time, g *toolsGrant, host, script string) (string, error) {
	scopes := append(tenantScopes(g.Tenant), scopeRef{kind: "plugin", tenant: g.Tenant, name: scriptPlugin},
		scopeRef{kind: "action", tenant: g.Tenant, name: scriptCap},
		scopeRef{kind: "role", tenant: g.Tenant, name: g.Role}, scopeRef{kind: "tasktype", tenant: g.Tenant, name: g.Type},
		scopeRef{kind: "worker", tenant: g.Tenant, name: strings.ToLower(g.Worker)})
	scopes = append(scopes, targetScopes(g.Tenant, callTargetRef{host: host})...)
	if held, err := heldBy(ctx, tx, now, []string{"no_tool", "no_action"}, scopes...); err != nil || held != "" {
		if err != nil {
			return "", err
		}
		return "held", nil
	}
	// Service actions and scripts of the tenant share one lock and one tenant limit.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(7300109, hashtext($1))`, g.Tenant); err != nil {
		return "", err
	}
	var perScript, perTenant int
	var busy bool
	if err := tx.QueryRow(ctx, `SELECT count(*) FILTER (WHERE tool = $5 AND target_host = $2 AND args->>'script' = $3 AND created_at > $4),
		count(*) FILTER (WHERE created_at > $4), coalesce(bool_or(tool = $5 AND target_host = $2 AND state IN ('queued', 'leased')), false)
		FROM toolcalls WHERE tenant = $1 AND tool IN ($5, $6) AND access = 'write' AND (grant_id IS NOT NULL OR approval_id IS NOT NULL)
		AND state <> 'denied' AND coalesce(reason, '') NOT IN ('plugin_unavailable', 'no_executor')`,
		g.Tenant, host, script, now.Add(-time.Hour), scriptRight, serviceRight).Scan(&perScript, &perTenant, &busy); err != nil {
		return "", err
	}
	if perScript >= scriptPerHour || perTenant >= tenantPerHour {
		if err := c.fireAlarm(ctx, tx, now, Alarm{Tenant: g.Tenant, Name: "service.control.rate",
			Subject: AlarmSubject{Kind: "script", ID: strings.ToLower(host + ":" + script)}}); err != nil {
			return "", err
		}
		return "service_rate_limited", nil
	}
	if busy {
		return "resource_busy", nil
	}
	return "", nil
}

// scriptHandout checks a script run again when an adapter claims it: the holds of the call
// scopes and the start window. It returns the reason to withhold it, or "" (HLD-3, MON-10).
func (c *Core) scriptHandout(ctx context.Context, tx pgx.Tx, now time.Time, delivery string) (string, error) {
	s, err := loadScriptCall(ctx, tx, delivery)
	if err != nil || s == nil {
		return "", err
	}
	var typ string
	var startBy *time.Time
	if err := tx.QueryRow(ctx, `SELECT t.type, c.start_by FROM tasks t, toolcalls c WHERE t.id = $1 AND c.id = $2`, s.Task, s.Toolcall).Scan(&typ, &startBy); err != nil {
		return "", err
	}
	if startBy != nil && !now.Before(*startBy) {
		return "no_executor", nil
	}
	role, err := deliveryRole(ctx, tx, s.Task)
	if err != nil {
		return "", err
	}
	scopes := append(tenantScopes(s.Tenant), scopeRef{kind: "plugin", tenant: s.Tenant, name: scriptPlugin},
		scopeRef{kind: "action", tenant: s.Tenant, name: scriptCap}, scopeRef{kind: "tasktype", tenant: s.Tenant, name: typ},
		scopeRef{kind: "worker", tenant: s.Tenant, name: strings.ToLower(s.BotID)})
	if role != "" {
		scopes = append(scopes, scopeRef{kind: "role", tenant: s.Tenant, name: role})
	}
	scopes = append(scopes, targetScopes(s.Tenant, callTargetRef{host: s.Host})...)
	held, err := heldBy(ctx, tx, now, []string{"no_tool", "no_action"}, scopes...)
	if err != nil || held == "" {
		return "", err
	}
	return "held", nil
}

// scriptOutcome turns the end of a script call into the closed result for the model: the code
// and the grant number; the output stays in the task journal (MON-10).
func (c *Core) scriptOutcome(ctx context.Context, callID, grant string) (*mcp.CallResult, error) {
	var state string
	var reason *string
	var output []byte
	if err := c.st.Pool.QueryRow(ctx, `SELECT state, reason, output FROM toolcalls WHERE id = $1`, callID).Scan(&state, &reason, &output); err != nil {
		return nil, err
	}
	r := serviceResult{GrantID: grant}
	switch state {
	case "done":
		r.Code = "done"
		var o mcp.CallResult
		if json.Unmarshal(output, &o) == nil && o.StructuredContent["status"] != "success" {
			r.Code = "failed"
		}
	case "exec_unknown":
		r.Code = "exec_unknown"
	case "denied":
		r.Code = "forbidden"
	default:
		switch deref(reason) {
		case "forbidden", "script_unknown":
			r.Code = deref(reason)
		case "held":
			// A hold set after the call was queued, as at the call (HLD-3).
			r.Code = "forbidden"
		default:
			r.Code = "failed"
		}
	}
	return r.call(), nil
}

// scriptCall is the row of a monitor_script delivery.
type scriptCall struct {
	Toolcall, Tenant, Task, Host, Script, BotID string
}

func loadScriptCall(ctx context.Context, tx pgx.Tx, delivery string) (*scriptCall, error) {
	var s scriptCall
	err := tx.QueryRow(ctx, `SELECT toolcall, tenant, task, host, script, bot_id FROM monitor_script_calls WHERE delivery = $1`, delivery).
		Scan(&s.Toolcall, &s.Tenant, &s.Task, &s.Host, &s.Script, &s.BotID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &s, err
}

// scriptDone accepts the outcome of a script run from the adapter: the call ends with the
// status only, the output goes into a task journal file marked untrusted, and the audit and the
// note carry no output (MON-10, INJ-13).
func (c *Core) scriptDone(ctx context.Context, tx pgx.Tx, now time.Time, delivery string, r Receipt) error {
	s, err := loadScriptCall(ctx, tx, delivery)
	if err != nil || s == nil {
		return err
	}
	var state string
	if err := tx.QueryRow(ctx, `SELECT state FROM toolcalls WHERE id = $1 FOR UPDATE`, s.Toolcall).Scan(&state); err != nil {
		return err
	}
	status := "success"
	if r.ExitCode != nil && *r.ExitCode != 0 {
		status = "failed"
	}
	structured := map[string]any{"status": status}
	if r.ExitCode != nil {
		structured["exit_code"] = *r.ExitCode
	}
	if r.Output != "" {
		ref, err := c.scriptJournal(ctx, tx, now, s, r.Output)
		if err != nil {
			return err
		}
		structured["output_file"] = ref
	}
	if state != "queued" && state != "leased" {
		// The call already ended (the wait ran out): the outcome is kept in the journal only.
		return nil
	}
	out, _ := json.Marshal(mcp.CallResult{StructuredContent: structured})
	return c.endToolCall(ctx, tx, now, s.Toolcall, "done", "", out)
}

// scriptJournal stores the output of a script as a produced file of the task: untrusted text,
// so a task that reads it gets untrusted_seen (MON-10, INJ-13).
func (c *Core) scriptJournal(ctx context.Context, tx pgx.Tx, now time.Time, s *scriptCall, output string) (string, error) {
	// The output is masked as tool output is, and cut on a character boundary (INJ-4).
	output, _ = redact.Cut(redact.Secrets(output), scriptOutputMax)
	tv, cat, err := c.requireTenant(ctx, s.Tenant)
	if err != nil {
		return "", err
	}
	var typ string
	var rec []byte
	if err := tx.QueryRow(ctx, `SELECT type, record FROM tasks WHERE id = $1`, s.Task).Scan(&typ, &rec); err != nil {
		return "", err
	}
	var r struct {
		DataMark string `json:"data_mark"`
	}
	_ = json.Unmarshal(rec, &r)
	mark := r.DataMark
	if mark == "" {
		mark = defaultAttachmentClass
	}
	prov, err := c.fileProvenance(ctx, tx, now, s.Task, s.Toolcall)
	if err != nil {
		return "", err
	}
	prov["untrusted"] = true
	provJSON, _ := json.Marshal(prov)
	ref := newFileRef()
	content := []byte(output)
	h := sha256.Sum256(content)
	sum := hex.EncodeToString(h[:])
	if _, err := tx.Exec(ctx, `INSERT INTO files (file_ref, tenant, origin, producer, media_type, size, sha256, data_mark, personal_seen, state, provenance, content, created_at, expires_at)
		VALUES ($1, $2, 'produced', $3, 'text/plain', $4, $5, $6, NULL, 'stored', $7, $8, $9, $10)`,
		ref, s.Tenant, s.Task, len(content), sum, mark, provJSON, content, now,
		fileExpiry(now, tv.Spec.Retention.Files, cat.TaskTypes[typ].ResultTTL)); err != nil {
		return "", err
	}
	return ref, c.appendEvent(ctx, tx, now, s.Tenant, Event{Type: "file.stored", Subject: ref, Data: map[string]any{
		"file_ref": ref, "origin": "produced", "data_mark": mark, "worker": s.BotID,
	}})
}

// scriptFailed ends the call of a monitor_script delivery that will not run again: the adapter
// refused it (forbidden, script_unknown), could not reach the monitoring system before the run
// (provider_unavailable), or its lease ran out, when nobody knows whether it ran (APR-7).
func (c *Core) scriptFailed(ctx context.Context, tx pgx.Tx, now time.Time, delivery, reason string) error {
	s, err := loadScriptCall(ctx, tx, delivery)
	if err != nil || s == nil {
		return err
	}
	var state string
	if err := tx.QueryRow(ctx, `SELECT state FROM toolcalls WHERE id = $1 FOR UPDATE`, s.Toolcall).Scan(&state); err != nil {
		return err
	}
	if state != "queued" && state != "leased" {
		return nil
	}
	switch reason {
	case "forbidden", "script_unknown", "provider_unavailable", "held", "no_executor", "worker_drain":
		return c.endToolCall(ctx, tx, now, s.Toolcall, "error", reason, nil)
	}
	return c.endToolCall(ctx, tx, now, s.Toolcall, "exec_unknown", "exec_unknown", nil)
}

// scriptSweep ends the script calls whose delivery no adapter took in time or that a hold
// withheld, and withdraws such a delivery, so the run never starts late (MON-10, DLV-16).
func (c *Core) scriptSweep(ctx context.Context, tx pgx.Tx, now time.Time) error {
	rows, err := tx.Query(ctx, `SELECT d.id, d.tenant, d.state, coalesce(d.reason, '') FROM monitor_script_calls s JOIN deliveries d ON d.id = s.delivery
		JOIN toolcalls t ON t.id = s.toolcall WHERE t.state IN ('queued', 'leased')
		AND (d.state NOT IN ('planned', 'sending') OR d.state = 'planned' AND t.start_by <= $1) ORDER BY d.id FOR UPDATE OF d`, now)
	if err != nil {
		return err
	}
	type row struct{ id, tenant, state, reason string }
	rs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
		var x row
		return x, r.Scan(&x.id, &x.tenant, &x.state, &x.reason)
	})
	if err != nil {
		return err
	}
	for _, r := range rs {
		reason := r.reason
		if r.state == "planned" {
			if _, err := tx.Exec(ctx, `UPDATE deliveries SET state = 'withheld', reason = 'no_executor', updated_at = $2 WHERE id = $1`, r.id, now); err != nil {
				return err
			}
			if err := c.appendEvent(ctx, tx, now, r.tenant, Event{Type: "delivery.withheld", Subject: r.id,
				Data: map[string]any{"delivery": r.id, "kind": "monitor_script", "reason": "no_executor"}}); err != nil {
				return err
			}
			reason = "no_executor"
		}
		s, err := loadScriptCall(ctx, tx, r.id)
		if err != nil || s == nil {
			return err
		}
		state := "error"
		if reason == "held" {
			state = "denied"
		}
		if reason == "" {
			reason = "no_executor"
		}
		if err := c.endToolCall(ctx, tx, now, s.Toolcall, state, reason, nil); err != nil {
			return err
		}
	}
	return nil
}

// submitSignal accepts an external event of the monitoring adapter of the tenant (MON-5,
// MON-10). It never becomes a task in v0.1: a problem raises or resolves a farm alarm by code,
// a script list closes the values of monitor/script_run.
func (c *Core) submitSignal(ctx context.Context, w *Worker, a Activity) (*ActivityResult, error) {
	if !c.monitorOn() {
		return nil, refuse("not_ready", "the monitor profile is off")
	}
	if a.Kind != "signal" || a.Event == nil || a.Channel != w.Target || a.Sender.Identity != "hook:monitor" {
		return nil, refuse("input_schema_invalid", "a monitoring adapter submits only signals of hook:monitor")
	}
	if w.State != "active" && w.State != "draining" {
		return nil, refuse("blocked", "the adapter has no accepted hello")
	}
	_, cat, err := c.requireTenant(ctx, w.Tenant)
	if err != nil {
		return nil, err
	}
	switch a.Event.Type {
	case "ext.monitor.problem.v1":
		return c.problemSignal(ctx, w, cat, a.Event.Data)
	case "ext.monitor.scripts.v1":
		return c.scriptsSignal(ctx, w, cat, a.Event.Data)
	}
	return nil, refuse("input_schema_invalid", "unknown event type")
}

// SignalEvent is the event of an activity of kind signal.
type SignalEvent struct {
	Type string         `json:"type"`
	Data map[string]any `json:"data"`
}

type problemEvent struct {
	EventID   string             `json:"eventid"`
	TriggerID string             `json:"triggerid"`
	Resource  string             `json:"resource"`
	Class     string             `json:"class,omitempty"`
	Item      string             `json:"item,omitempty"`
	Severity  int                `json:"severity"`
	State     string             `json:"state"`
	Values    map[string]float64 `json:"values,omitempty"`
}

func (c *Core) problemSignal(ctx context.Context, w *Worker, cat *Catalog, data map[string]any) (*ActivityResult, error) {
	var ev problemEvent
	raw, _ := json.Marshal(data)
	if err := json.Unmarshal(raw, &ev); err != nil {
		return nil, refuse("input_schema_invalid", "the problem event does not decode")
	}
	host, ok := monitorHosts(cat, w.Tenant)[ev.Resource]
	if !ok {
		// A node no catalog resource of the tenant binds does not enter the farm (MON-5).
		return &ActivityResult{Result: "ignored", Reason: "not_subject"}, nil
	}
	// The class and the item come only from the catalog; the adapter cannot set them.
	class, item, inCatalog := monitorTrigger(cat, w.Tenant, ev.TriggerID)
	ev.Class, ev.Item = "unknown", ""
	// A catalog class longer than the event allows stays unknown rather than refuse the problem.
	if inCatalog && len(class) <= 32 {
		ev.Class = class
		// An item key outside the closed form of the event (a path, for example) stays in the
		// catalog; the event carries only the class.
		if eventItem.MatchString(item) && len(item) <= 120 {
			ev.Item = item
		}
	}
	m := map[string]any{}
	raw, _ = json.Marshal(ev)
	_ = json.Unmarshal(raw, &m)
	if err := c.spec.Validate("ext-monitor-problem", m); err != nil {
		return nil, refuse("input_schema_invalid", "the problem event is outside its schema")
	}
	alarm := "monitor.problem"
	if name, ok := monitorClasses[ev.Class]; ok {
		alarm = name
	}
	values := map[string]any{"trigger_severity": ev.Severity}
	for k, v := range ev.Values {
		if len(values) < 16 {
			values[k] = v
		}
	}
	key := "system:ingress:monitor:" + ev.EventID + ":" + ev.State
	var out ActivityResult
	err := c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		out = ActivityResult{Result: "ignored"}
		tag, err := tx.Exec(ctx, `INSERT INTO monitor_events (tenant, key, created_at) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`, w.Tenant, key, now)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return nil
		}
		if err := c.appendEvent(ctx, tx, now, w.Tenant, Event{Type: "ext.monitor.problem.v1", Subject: key, Data: m}); err != nil {
			return err
		}
		subject := AlarmSubject{Kind: "resource", ID: strings.ToLower(host.ID)}
		if ev.State == "open" {
			tag, err := tx.Exec(ctx, `INSERT INTO monitor_problems (tenant, eventid, host, triggerid, class, alarm, state, opened_at)
				VALUES ($1, $2, $3, $4, $5, $6, 'open', $7) ON CONFLICT (tenant, eventid) DO NOTHING`,
				w.Tenant, ev.EventID, host.ID, ev.TriggerID, ev.Class, alarm, now)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				// The problem is known already, perhaps resolved before this open came in.
				return nil
			}
			return c.fireAlarm(ctx, tx, now, Alarm{Tenant: w.Tenant, Name: alarm, Subject: subject, Values: values, Origin: "monitor"})
		}
		if err := tx.QueryRow(ctx, `UPDATE monitor_problems SET state = 'resolved', resolved_at = $3 WHERE tenant = $1 AND eventid = $2
			RETURNING alarm`, w.Tenant, ev.EventID, now).Scan(&alarm); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		// The alarm of a host resolves with its last open problem.
		var open int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM monitor_problems WHERE tenant = $1 AND host = $2 AND alarm = $3 AND state = 'open'`,
			w.Tenant, host.ID, alarm).Scan(&open); err != nil {
			return err
		}
		if open > 0 {
			return nil
		}
		return c.resolveAlarm(ctx, tx, now, Alarm{Tenant: w.Tenant, Name: alarm, Subject: subject, Origin: "monitor"})
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

type scriptsEvent struct {
	Resource string          `json:"resource"`
	Hash     string          `json:"hash"`
	Scripts  []adapterScript `json:"scripts"`
}

func (c *Core) scriptsSignal(ctx context.Context, w *Worker, cat *Catalog, data map[string]any) (*ActivityResult, error) {
	if err := c.spec.Validate("ext-monitor-scripts", data); err != nil {
		return nil, refuse("input_schema_invalid", "the script list is outside its schema")
	}
	var ev scriptsEvent
	raw, _ := json.Marshal(data)
	if err := json.Unmarshal(raw, &ev); err != nil {
		return nil, refuse("input_schema_invalid", "the script list does not decode")
	}
	if _, ok := monitorHosts(cat, w.Tenant)[ev.Resource]; !ok {
		return &ActivityResult{Result: "ignored", Reason: "not_subject"}, nil
	}
	sort.Slice(ev.Scripts, func(i, j int) bool { return ev.Scripts[i].ScriptID < ev.Scripts[j].ScriptID })
	list, _ := json.Marshal(ev.Scripts)
	var out ActivityResult
	err := c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		out = ActivityResult{Result: "ignored"}
		tag, err := tx.Exec(ctx, `INSERT INTO monitor_host_scripts (tenant, host, hash, scripts, updated_at) VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (tenant, host) DO UPDATE SET hash = excluded.hash, scripts = excluded.scripts, updated_at = excluded.updated_at
			WHERE monitor_host_scripts.hash <> excluded.hash`, w.Tenant, ev.Resource, ev.Hash, list, now)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		return c.appendEvent(ctx, tx, now, w.Tenant, Event{Type: "ext.monitor.scripts.v1", Subject: ev.Resource, Data: data})
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// monitorNote plans the code note service_control_done after a service action or a script run
// on a host bound to the monitoring system: on the newest open problem of the host; with no
// open problem there is no note (MON-8, MON-10). The note carries closed fields only.
func (c *Core) monitorNote(ctx context.Context, tx pgx.Tx, now time.Time, callID, state string) error {
	if !c.monitorOn() {
		return nil
	}
	var tool, tenant, task string
	var host, grant, approval *string
	var args map[string]any
	err := tx.QueryRow(ctx, `SELECT tool, tenant, task, target_host, grant_id, approval_id, args FROM toolcalls WHERE id = $1 AND access = 'write'`, callID).
		Scan(&tool, &tenant, &task, &host, &grant, &approval, &args)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && tool != scriptRight && tool != serviceRight {
		return nil
	}
	if err != nil {
		return err
	}
	_, cat, err := c.requireTenant(ctx, tenant)
	if err != nil {
		return err
	}
	hosts := monitorHosts(cat, tenant)
	fields := map[string]any{}
	hostID := deref(host)
	if tool == scriptRight {
		fields["script"], fields["verb"] = args["script"], "run"
	} else {
		svc, _ := args["service"].(string)
		fields["service"], fields["verb"] = svc, args["verb"]
		for _, s := range catalogServices(cat, tenant) {
			if s.Name == svc {
				hostID = s.Host
			}
		}
	}
	if _, ok := hosts[hostID]; !ok {
		return nil
	}
	result := "failed"
	if state == "done" {
		result = "success"
		var out []byte
		if err := tx.QueryRow(ctx, `SELECT output FROM toolcalls WHERE id = $1`, callID).Scan(&out); err != nil {
			return err
		}
		var o mcp.CallResult
		if json.Unmarshal(out, &o) == nil && o.StructuredContent["status"] == "failed" {
			result = "failed"
		}
	}
	fields["result"] = result
	if grant != nil {
		fields["grant_id"] = *grant
	} else if approval != nil {
		fields["approval_id"] = *approval
	}
	var eventid string
	err = tx.QueryRow(ctx, `SELECT eventid FROM monitor_problems WHERE tenant = $1 AND host = $2 AND state = 'open' ORDER BY opened_at DESC, eventid DESC LIMIT 1`,
		tenant, hostID).Scan(&eventid)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	bot, err := noteBot(ctx, tx, task)
	if err != nil || bot == "" {
		return err
	}
	body := map[string]any{"eventid": eventid, "bot_id": bot, "template": "service_control_done", "fields": fields}
	if err := c.spec.Validate("monitor-note", body); err != nil {
		return err
	}
	_, err = c.planDelivery(ctx, tx, now, plannedDelivery{
		Tenant: tenant, Task: task, Kind: "monitor_note", Addr: "monitor", TargetKind: "monitor", TargetID: c.monitorTarget(),
		ExecKind: "monitor", ExecName: c.monitorTarget(), DataOrigin: "code", Body: body,
	})
	return err
}

// noteBot is the bot of a note: the worker of the task's last attempt. An action task of a
// person has no attempt and no bot, so it gets no note: the mark would name a bot that did
// nothing (MON-8).
func noteBot(ctx context.Context, tx pgx.Tx, task string) (string, error) {
	var w string
	err := tx.QueryRow(ctx, `SELECT worker_id FROM leases WHERE work_id = $1 AND kind = 'attempt' ORDER BY lease_no DESC LIMIT 1`, task).Scan(&w)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return w, err
}
