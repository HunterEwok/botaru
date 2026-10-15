// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/ids"
	"github.com/HunterEwok/botaru/impl/internal/jcs"
	"github.com/HunterEwok/botaru/impl/internal/manifest"
	"github.com/HunterEwok/botaru/impl/internal/mcp"
	"github.com/HunterEwok/botaru/impl/internal/toolspec"
)

// Service control (MON-8, APR-18, INJ-15, PRM-18) and the /run command (APR-17). A restart of a
// catalog service from its no_card list runs without a card under a service_grant record; any
// other verb, and any call in a task that read untrusted text, becomes an ordinary approval of
// the action service.control, decided on a card like any action (APR-3, APR-4, APR-7).
const (
	serviceAction   = "service.control"
	serviceToolName = "service_control" // the model name of the built-in service/control
	serviceRight    = "service/control"
	servicePlugin   = "service"
	// The default guards of MON-8: actions per service and per tenant in an hour.
	servicePerHour = 6
	tenantPerHour  = 30
)

// catalogService is one controllable service of a tenant catalog (MON-8).
type catalogService struct {
	Name, Unit, Via, Host string
	Control, NoCard       []string
}

// catalogServices are the services of the tenant catalog, by name.
func catalogServices(cat *Catalog, tenant string) []catalogService {
	spec, _ := tenantCatalog(cat, tenant)
	hosts, _ := spec["hosts"].([]any)
	var out []catalogService
	for _, h := range hosts {
		hm, _ := h.(map[string]any)
		host, _ := hm["id"].(string)
		svcs, _ := hm["services"].([]any)
		for _, s := range svcs {
			sm, _ := s.(map[string]any)
			cs := catalogService{Name: asString(sm["name"]), Unit: asString(sm["unit"]), Via: asString(sm["via"]), Host: host,
				Control: strList(sm["control"]), NoCard: []string{"restart"}}
			if nc, ok := sm["no_card"]; ok {
				cs.NoCard = strList(nc)
			}
			if cs.Via == "" {
				cs.Via = "host"
			}
			out = append(out, cs)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func strList(v any) []string {
	xs, _ := v.([]any)
	out := make([]string, 0, len(xs))
	for _, x := range xs {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// controllable are the services this build can control: via host with a unit and a control
// list. A via: monitor service runs a monitoring script through E5, which needs the monitor
// profile this build does not claim.
func controllable(cat *Catalog, tenant string) []catalogService {
	var out []catalogService
	for _, s := range catalogServices(cat, tenant) {
		if s.Via == "host" && s.Unit != "" && len(s.Control) > 0 {
			out = append(out, s)
		}
	}
	return out
}

func findControllable(cat *Catalog, tenant, name string) (catalogService, bool) {
	for _, s := range controllable(cat, tenant) {
		if s.Name == name {
			return s, true
		}
	}
	return catalogService{}, false
}

// roleControls says whether the role's service_control names the service, or all of them.
func roleControls(cat *Catalog, role, service string) bool {
	r, ok := cat.Roles[role]
	return ok && (slices.Contains(r.ServiceControl, "*") || slices.Contains(r.ServiceControl, service))
}

// serviceActionDef is the action service.control: the approval of a verb that goes by card and
// the executor of every service write call, the tool control of the plugin service.
func serviceActionDef() manifest.Action {
	a := manifest.Action{
		Title:       "Service control",
		Description: "Start, stop or restart a service of the tenant catalog.",
		Risk:        "low",
		Args: map[string]any{
			"type": "object", "additionalProperties": false, "required": []any{"service", "verb"},
			"properties": map[string]any{
				"service": map[string]any{"type": "string", "enum_from": "catalog.services"},
				"verb":    map[string]any{"type": "string", "enum": []any{"start", "stop", "restart"}},
			},
		},
	}
	a.Executor.Plugin, a.Executor.Tool = servicePlugin, "control"
	return a
}

// actionOf is the action of an approval: a manifest action, or service.control.
func (c *Core) actionOf(cat *Catalog, name string) (manifest.Action, bool) {
	if name == serviceAction {
		return serviceActionDef(), true
	}
	a, ok := cat.Actions[name]
	return a, ok
}

// requestCap is the capability the initiator of an approval needs: action.request:<a>, or
// service.control:<service> for a service action (D-73).
func requestCap(action string, args map[string]any) string {
	if action == serviceAction {
		svc, _ := args["service"].(string)
		return "service.control:" + svc
	}
	return "action.request:" + action
}

// mayApprove says whether a person with these groups decides the approval: action.approve:<a>,
// or for a service action also action.approve:service.control:<service> (APR-18).
func mayApprove(tv *TenantView, groups []string, a *approvalRow) bool {
	if hasCap(tv, groups, "action.approve:"+a.Action) {
		return true
	}
	if a.Action == serviceAction {
		svc, _ := a.Args["service"].(string)
		return hasCap(tv, groups, "action.approve:service.control:"+svc)
	}
	return false
}

// serviceControlTool is the built-in tool service/control of step 7 of the catalog assembly:
// only for a role with service_control, in a work task of tier 1 and above, with the services
// of the role's list in the tenant catalog as an enum (APR-18, PRF-1).
func (c *Core) serviceControlTool(cat *Catalog, g *toolsGrant) *catalogTool {
	role, ok := cat.Roles[g.Role]
	if !ok || len(role.ServiceControl) == 0 || g.MaxTier < 1 {
		return nil
	}
	if p := role.Profile; p != "" && p != "worker" && p != "manager" && p != "farm-manager" {
		return nil
	}
	if g.Purpose != "" && g.Purpose != "work" && g.Purpose != "manage" {
		return nil
	}
	var names, verbs []any
	var desc strings.Builder
	desc.WriteString("Start, stop or restart a service of the tenant catalog; the farm picks the machine. A restart of a listed service runs at once; other verbs, and any call after reading untrusted text, go to a person as a card. Services:")
	for _, s := range controllable(cat, g.Tenant) {
		if !roleControls(cat, g.Role, s.Name) {
			continue
		}
		names = append(names, s.Name)
		for _, v := range s.Control {
			if !slices.Contains(verbs, any(v)) {
				verbs = append(verbs, v)
			}
		}
		fmt.Fprintf(&desc, "\n- %s: %s", s.Name, strings.Join(s.Control, ", "))
	}
	if len(names) == 0 {
		return nil
	}
	slices.SortFunc(verbs, func(a, b any) int { return strings.Compare(a.(string), b.(string)) })
	schema := map[string]any{
		"type": "object", "additionalProperties": false, "required": []any{"service", "verb"},
		"properties": map[string]any{
			"service": map[string]any{"type": "string", "enum": names},
			"verb":    map[string]any{"type": "string", "enum": verbs},
		},
	}
	return &catalogTool{Name: serviceToolName, Right: serviceRight, Builtin: true, Tool: toolspec.Tool{Description: desc.String()}, Schema: schema}
}

// serviceResult is service-control-result.
type serviceResult struct {
	Code       string   `json:"code"`
	State      string   `json:"state,omitempty"`
	GrantID    string   `json:"grant_id,omitempty"`
	ApprovalID string   `json:"approval_id,omitempty"`
	Needs      []string `json:"needs,omitempty"`
}

func (r serviceResult) call() *mcp.CallResult {
	b, _ := json.Marshal(r)
	var sc map[string]any
	_ = json.Unmarshal(b, &sc)
	ok := r.Code == "done" || r.Code == "approval_required"
	return &mcp.CallResult{IsError: !ok, StructuredContent: sc, Content: []mcp.Content{{Type: "text", Text: string(b)}}}
}

// serviceCall is one request to control a service, from a bot or from a person.
type serviceCall struct {
	Tenant  string
	Service catalogService
	Verb    string
	// Initiator is the principal on whose behalf the call runs; Groups its group closure.
	Initiator string
	Groups    []string
	// A call from a bot: its role, task, type and worker, and the task's untrusted flag.
	Role, Task, Type, Worker string
	UntrustedSeen            bool
	UntrustedSource          string
	// A call from a person: the person (the same as the initiator).
	Person string
}

func (s serviceCall) args() map[string]any {
	return map[string]any{"service": s.Service.Name, "verb": s.Verb}
}

// serviceGuards checks the holds, the rate limits and one operation per service at a time
// (MON-8, HLD-3). It returns a refusal code, or "".
func (c *Core) serviceGuards(ctx context.Context, tx pgx.Tx, now time.Time, s serviceCall) (string, error) {
	scopes := append(tenantScopes(s.Tenant), scopeRef{kind: "plugin", tenant: s.Tenant, name: servicePlugin},
		scopeRef{kind: "action", tenant: s.Tenant, name: serviceAction})
	scopes = append(scopes, targetScopes(s.Tenant, callTargetRef{host: s.Service.Host})...)
	if s.Role != "" {
		scopes = append(scopes, scopeRef{kind: "role", tenant: s.Tenant, name: s.Role}, scopeRef{kind: "tasktype", tenant: s.Tenant, name: s.Type})
	}
	if s.Worker != "" {
		scopes = append(scopes, scopeRef{kind: "worker", tenant: s.Tenant, name: strings.ToLower(s.Worker)})
	}
	if held, err := heldBy(ctx, tx, now, []string{"no_tool", "no_action"}, scopes...); err != nil || held != "" {
		if err != nil {
			return "", err
		}
		return "held", nil
	}
	// The service actions of the tenant are counted under its lock.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(7300109, hashtext($1))`, s.Tenant); err != nil {
		return "", err
	}
	var perService, perTenant int
	var busy bool
	if err := tx.QueryRow(ctx, `SELECT count(*) FILTER (WHERE args->>'service' = $2 AND created_at > $3),
		count(*) FILTER (WHERE created_at > $3), coalesce(bool_or(args->>'service' = $2 AND state IN ('queued', 'leased')), false)
		FROM toolcalls WHERE tenant = $1 AND tool = $4 AND access = 'write' AND (grant_id IS NOT NULL OR approval_id IS NOT NULL)
		AND state <> 'denied' AND coalesce(reason, '') <> 'plugin_unavailable'`,
		s.Tenant, s.Service.Name, now.Add(-time.Hour), serviceRight).Scan(&perService, &perTenant, &busy); err != nil {
		return "", err
	}
	if perService >= servicePerHour || perTenant >= tenantPerHour {
		// A stop that hit the limit pages; anything else warns (MON-8).
		if err := c.fireAlarm(ctx, tx, now, Alarm{Tenant: s.Tenant, Name: "service.control.rate",
			Subject: AlarmSubject{Kind: "service", ID: s.Service.Name}, Page: s.Verb == "stop"}); err != nil {
			return "", err
		}
		return "service_rate_limited", nil
	}
	if busy {
		return "resource_busy", nil
	}
	return "", nil
}

// grantAuthor is the author block of a service_grant record.
func (s serviceCall) grantAuthor() (map[string]any, Actor) {
	if s.Person != "" {
		return map[string]any{"kind": "human", "principal": s.Person}, Actor{Principal: s.Person, Via: "channel"}
	}
	return map[string]any{"kind": "bot", "principal": workerPrincipal(s.Worker), "role": s.Role, "task": s.Task},
		Actor{Principal: workerPrincipal(s.Worker), Via: "bot", Role: s.Role, Task: s.Task}
}

// writeGrant writes the service_grant audit record whose id the write call carries in place of
// approval_id (APR-17, APR-18): the author, what runs, the hash of the arguments.
func (c *Core) writeGrant(ctx context.Context, tx pgx.Tx, now time.Time, tenant string, author map[string]any, actor Actor, what map[string]any, argsHash string) (string, error) {
	id := ids.New(ids.Grant)
	block := map[string]any{"author": author, "args_hash": argsHash}
	for k, v := range what {
		block[k] = v
	}
	_, err := c.appendAudit(ctx, tx, now, Audit{
		Tenant: tenant, Kind: "service_grant", Actor: actor, Command: "service_grant", Target: &Target{Kind: "service_grant", ID: id},
		OK: true, ServiceGrant: block,
	})
	return id, err
}

// serviceControl is the call of the built-in service/control from a bot (APR-18, INJ-15). It
// returns a result at once, or the write call of a grant that the caller waits for.
func (c *Core) serviceControl(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, g *toolsGrant, offered bool, args map[string]any) (*serviceResult, string, string, error) {
	tv := cat.Tenants[g.Tenant]
	audit := func(ok bool, reason, id string) error {
		after := map[string]any{"service": args["service"], "verb": args["verb"]}
		if id != "" {
			after["ref"] = id
		}
		_, err := c.appendAudit(ctx, tx, now, Audit{
			Tenant: g.Tenant, Kind: "authorize", Actor: Actor{Principal: workerPrincipal(g.Worker), Via: "bot", Role: g.Role, Task: g.Task},
			Command: serviceRight, Target: &Target{Kind: "tool", ID: serviceRight}, OK: ok, Reason: reason, After: after,
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
	name, _ := args["service"].(string)
	verb, _ := args["verb"].(string)
	svc, ok := findControllable(cat, g.Tenant, name)
	if !ok || !roleControls(cat, g.Role, name) || !slices.Contains(svc.Control, verb) || len(args) != 2 {
		// A value outside the closed lists is a schema error, as for any tool (INJ-1).
		if err := audit(false, "input_schema_invalid", ""); err != nil {
			return nil, "", "", err
		}
		return nil, "", "", errSchema
	}
	// Every initiator needs their own right (D-73): a person, or a service or system principal
	// with an explicit row.
	if g.Initiator != "" && !hasCap(tv, groupClosure(tv, g.InitiatorGroups), "service.control:"+name) {
		return deny("forbidden", "forbidden", "service.control:"+name)
	}
	s := serviceCall{Tenant: g.Tenant, Service: svc, Verb: verb, Initiator: g.Initiator, Role: g.Role, Task: g.Task, Type: g.Type, Worker: g.Worker}
	var rec []byte
	if err := tx.QueryRow(ctx, `SELECT record FROM tasks WHERE id = $1`, g.Task).Scan(&rec); err != nil {
		return nil, "", "", err
	}
	var tr struct {
		UntrustedSeen   bool   `json:"untrusted_seen"`
		UntrustedSource string `json:"untrusted_source"`
	}
	if err := json.Unmarshal(rec, &tr); err != nil {
		return nil, "", "", err
	}
	s.UntrustedSeen, s.UntrustedSource = tr.UntrustedSeen, tr.UntrustedSource
	if code, err := c.serviceGuards(ctx, tx, now, s); err != nil {
		return nil, "", "", err
	} else if code != "" {
		// The model sees a hold as the same short forbidden (PRM-8).
		if code == "held" {
			return deny("forbidden", "held")
		}
		return deny(code, code)
	}
	argsHash, err := jcs.Hash(s.args())
	if err != nil {
		return nil, "", "", err
	}
	if !slices.Contains(svc.NoCard, verb) || s.UntrustedSeen {
		id, code, err := c.openApproval(ctx, tx, now, tv, approvalRow{
			Tenant: g.Tenant, SourceTask: g.Task, Initiator: g.Initiator, Action: serviceAction, ArgsHash: argsHash, Args: s.args(), Risk: "low",
			RequestedBy:   map[string]any{"kind": "bot", "role": g.Role, "task": g.Task},
			UntrustedSeen: s.UntrustedSeen, UntrustedSource: s.UntrustedSource, ExpiresAt: now.Add(time.Hour), Mark: g.Mark,
		})
		if err != nil {
			return nil, "", "", err
		}
		if code != "pending" {
			return deny(code, code)
		}
		if err := audit(true, "", id); err != nil {
			return nil, "", "", err
		}
		return &serviceResult{Code: "approval_required", ApprovalID: id}, "", "", nil
	}
	author, actor := s.grantAuthor()
	grant, err := c.writeGrant(ctx, tx, now, g.Tenant, author, actor, map[string]any{"service": name, "verb": verb}, argsHash)
	if err != nil {
		return nil, "", "", err
	}
	callID, err := c.insertWriteCall(ctx, tx, now, cat, writeCall{
		Tenant: g.Tenant, Task: g.Task, Action: serviceAction, Act: serviceActionDef(), Args: s.args(), ArgsHash: argsHash, GrantID: grant,
	})
	if err != nil {
		return nil, "", "", err
	}
	if err := audit(true, "", grant); err != nil {
		return nil, "", "", err
	}
	return nil, callID, grant, nil
}

// errSchema marks arguments outside the closed schema of a built-in tool.
var errSchema = errors.New("input_schema_invalid")

// serviceOutcome turns the end of a grant's write call into the closed result for the model:
// the code, the unit state the plugin reported and the grant number (APR-18).
func (c *Core) serviceOutcome(ctx context.Context, callID, grant string) (*mcp.CallResult, error) {
	var state string
	var reason *string
	var output []byte
	if err := c.st.Pool.QueryRow(ctx, `SELECT state, reason, output FROM toolcalls WHERE id = $1`, callID).Scan(&state, &reason, &output); err != nil {
		return nil, err
	}
	r := serviceResult{GrantID: grant}
	switch state {
	case "done":
		r.Code, r.State = "done", "unknown"
		var o mcp.CallResult
		if json.Unmarshal(output, &o) == nil {
			if st, _ := o.StructuredContent["state"].(string); slices.Contains([]string{"active", "inactive", "failed"}, st) {
				r.State = st
			}
		}
	case "exec_unknown":
		r.Code = "exec_unknown"
	case "denied":
		r.Code = "forbidden"
	default:
		r.Code = "failed"
	}
	return r.call(), nil
}

// personMark is the highest data mark a person holds, for the closed lists of a command.
func (c *Core) personMark(tv *TenantView, groups []string) string {
	best, bestOrder := "", -1
	for _, g := range groups {
		for _, m := range tv.Groups[g].Data {
			if o, ok := c.markOrder(m); ok && o > bestOrder {
				best, bestOrder = m, o
			}
		}
	}
	return best
}

// capGroups are the groups of the tenant that hold a capability, for the text that names them
// (PRM-8).
func capGroups(tv *TenantView, cap string) string {
	var out []string
	for name, g := range tv.Groups {
		for _, have := range g.Caps {
			if capCovers(have, cap) {
				out = append(out, name)
				break
			}
		}
	}
	sort.Strings(out)
	if len(out) == 0 {
		return "-"
	}
	return strings.Join(out, ", ")
}

// commandWord splits a direct, not forwarded or quoted message into its command and the rest.
func commandWord(a Activity) (string, string, bool) {
	text := strings.TrimSpace(a.Text)
	if a.Chat.Kind != "direct" || a.Forwarded || a.Quoted || !strings.HasPrefix(text, "/") {
		return "", "", false
	}
	word, rest, _ := strings.Cut(text, " ")
	if i := strings.IndexByte(word, '@'); i >= 0 {
		word = word[:i]
	}
	return word, strings.TrimSpace(rest), true
}

// personCommand is a command of a person that the core runs by code, without a model.
type personCommand struct {
	tv      *TenantView
	cat     *Catalog
	person  string
	groups  []string
	locale  string
	channel string
	chat    Chat
	key     string
	// binding is the name of the bound group chat of the command, or "" in a direct chat.
	binding string
}

func (pc personCommand) reply(c *Core, key string, params map[string]any) *ActivityResult {
	return &ActivityResult{Result: "reply", Text: c.texts.Render(pc.locale, key, params)}
}

func (pc personCommand) origin() *Origin { return &Origin{Channel: pc.channel, Chat: pc.chat} }

// serviceCommand is /service <verb> <service> from a person (PRM-18): a verb of no_card runs
// under a service_grant with the person as author, any other verb becomes an approval decided
// by another person.
func (c *Core) serviceCommand(ctx context.Context, tx pgx.Tx, now time.Time, pc personCommand, rest string) (*ActivityResult, error) {
	f := strings.Fields(rest)
	if len(f) != 2 {
		return pc.reply(c, "cmd.service.usage", nil), nil
	}
	verb, name := f[0], f[1]
	svc, ok := findControllable(pc.cat, pc.tv.Name, name)
	if !ok || !slices.Contains(svc.Control, verb) {
		return pc.reply(c, "cmd.service.unknown", nil), nil
	}
	cap := "service.control:" + name
	if !hasCap(pc.tv, pc.groups, cap) {
		if _, err := c.appendAudit(ctx, tx, now, Audit{
			Tenant: pc.tv.Name, Kind: "authorize", Actor: Actor{Principal: pc.person, Via: "channel"}, Command: "/service",
			Target: &Target{Kind: "service", ID: name}, OK: false, Reason: "forbidden",
		}); err != nil {
			return nil, err
		}
		return pc.reply(c, "cmd.forbidden.needs", map[string]any{"group": capGroups(pc.tv, cap)}), nil
	}
	s := serviceCall{Tenant: pc.tv.Name, Service: svc, Verb: verb, Initiator: pc.person, Groups: pc.groups, Person: pc.person}
	if code, err := c.serviceGuards(ctx, tx, now, s); err != nil {
		return nil, err
	} else if code != "" {
		return pc.reply(c, "cmd.refused."+code, nil), nil
	}
	argsHash, err := jcs.Hash(s.args())
	if err != nil {
		return nil, err
	}
	if !slices.Contains(svc.NoCard, verb) {
		return c.commandApproval(ctx, tx, now, pc, approvalRow{
			Tenant: pc.tv.Name, Initiator: pc.person, Action: serviceAction, ArgsHash: argsHash, Args: s.args(), Risk: "low",
			RequestedBy: map[string]any{"kind": "human"}, ExpiresAt: now.Add(time.Hour),
		})
	}
	author, actor := s.grantAuthor()
	grant, err := c.writeGrant(ctx, tx, now, pc.tv.Name, author, actor, map[string]any{"service": name, "verb": verb}, argsHash)
	if err != nil {
		return nil, err
	}
	if _, err := c.grantTask(ctx, tx, now, pc, serviceAction, serviceActionDef(), s.args(), argsHash, grant, ""); err != nil {
		return nil, err
	}
	return pc.reply(c, "cmd.service.accepted", map[string]any{"verb": verb, "service": name}), nil
}

// commandApproval opens an approval on behalf of a person and plans its cards at once, since
// no attempt is running.
func (c *Core) commandApproval(ctx context.Context, tx pgx.Tx, now time.Time, pc personCommand, a approvalRow) (*ActivityResult, error) {
	// The arguments were checked with the person's highest mark; execution uses the same.
	a.Mark = c.personMark(pc.tv, pc.groups)
	id, code, err := c.openApproval(ctx, tx, now, pc.tv, a)
	if err != nil {
		return nil, err
	}
	if code != "pending" {
		return pc.reply(c, "cmd.refused."+code, nil), nil
	}
	ap, err := loadApproval(ctx, tx, pc.tv.Name, id, true)
	if err != nil {
		return nil, err
	}
	if ap.CardsAt == nil {
		if err := c.planCards(ctx, tx, now, pc.tv, ap); err != nil {
			return nil, err
		}
	}
	act, _ := c.actionOf(pc.cat, a.Action)
	title := act.Title
	if title == "" {
		title = a.Action
	}
	return pc.reply(c, "cmd.action.pending", map[string]any{"title": title, "deadline": fmtTime(ap.ExpiresAt)}), nil
}

// grantTask creates the action.<a> task of a grant directly in working with its one write call
// (CNT-7). Its record keeps the origin chat, so the person gets the outcome as code text.
func (c *Core) grantTask(ctx context.Context, tx pgx.Tx, now time.Time, pc personCommand, action string, act manifest.Action, args map[string]any, argsHash, grant, mark string) (string, error) {
	taskID, err := c.insertActionTask(ctx, tx, now, actionTaskInput{
		Tenant: pc.tv.Name, Action: action, Args: args, Initiator: pc.person, Mark: mark, GrantID: grant, Origin: pc.origin(), Key: pc.key,
	})
	if err != nil {
		return "", err
	}
	if _, err := c.insertWriteCall(ctx, tx, now, pc.cat, writeCall{
		Tenant: pc.tv.Name, Task: taskID, Action: action, Act: act, Args: args, ArgsHash: argsHash, GrantID: grant,
	}); err != nil {
		return "", err
	}
	return taskID, nil
}

// runCommand is /run <pipeline> <arguments> (APR-17): a type with run.approval none is enqueued
// as a task, a routine action runs under a service_grant within its limits, any other action
// becomes an approval of the person's request.
func (c *Core) runCommand(ctx context.Context, tx pgx.Tx, now time.Time, pc personCommand, rest string) (*ActivityResult, error) {
	f := strings.Fields(rest)
	if len(f) == 0 {
		return pc.reply(c, "cmd.run.usage", nil), nil
	}
	entry, ok := pc.tv.Spec.Run[f[0]]
	if !ok || len(f)-1 > len(entry.Args) {
		return pc.reply(c, "cmd.run.unknown", nil), nil
	}
	values := map[string]string{}
	for i, v := range f[1:] {
		values[entry.Args[i]] = v
	}
	if entry.Type != "" {
		tt, ok := pc.cat.TaskTypes[entry.Type]
		if !ok || tt.Run == nil || tt.Run.Approval != "none" {
			return pc.reply(c, "cmd.run.unknown", nil), nil
		}
		params, ok := coerceArgs(tt.ParamsSchema, values)
		if !ok {
			return pc.reply(c, "cmd.run.invalid", nil), nil
		}
		res, err := c.enqueue(ctx, tx, now, pc.cat, enqueueInput{
			Tenant: pc.tv.Name, Type: entry.Type, Body: map[string]any{"type": entry.Type, "goal": "/run " + f[0], "params": params},
			IdempotencyKey: pc.key, Initiator: pc.person, EnqueuedBy: "channel:" + pc.channel, Origin: pc.origin(), Source: "interactive", Cause: pc.key,
		})
		if err != nil {
			if p, ok := AsProblem(err); ok {
				return &ActivityResult{Result: "reply", Text: c.rejectText(pc.locale, p.Reason)}, nil
			}
			return nil, err
		}
		if res.Rejected != nil {
			return &ActivityResult{Result: "reply", Text: c.rejectText(pc.locale, res.Rejected.Reason)}, nil
		}
		return &ActivityResult{Result: "task", Task: res.TaskID, Existing: res.Existing}, nil
	}
	act, ok := pc.cat.Actions[entry.Action]
	if !ok {
		return pc.reply(c, "cmd.run.unknown", nil), nil
	}
	cap := "action.request:" + entry.Action
	if !hasCap(pc.tv, pc.groups, cap) {
		if _, err := c.appendAudit(ctx, tx, now, Audit{
			Tenant: pc.tv.Name, Kind: "authorize", Actor: Actor{Principal: pc.person, Via: "channel"}, Command: "/run",
			Target: &Target{Kind: "action", ID: entry.Action}, OK: false, Reason: "forbidden",
		}); err != nil {
			return nil, err
		}
		return pc.reply(c, "cmd.forbidden.needs", map[string]any{"group": capGroups(pc.tv, cap)}), nil
	}
	args, ok := coerceArgs(act.Args, values)
	if !ok {
		return pc.reply(c, "cmd.run.invalid", nil), nil
	}
	args = actionDefaults(act, args)
	mark := c.personMark(pc.tv, pc.groups)
	schema, err := c.actionArgsSchema(pc.cat, pc.tv.Name, mark, act)
	if err != nil {
		return nil, err
	}
	if c.rs.validate(schema, args) != nil {
		return pc.reply(c, "cmd.run.invalid", nil), nil
	}
	if held, err := heldBy(ctx, tx, now, []string{"no_action", "pause_action"}, approvalScopes(pc.tv.Name, entry.Action, "", "")...); err != nil {
		return nil, err
	} else if held != "" {
		return pc.reply(c, "cmd.refused.held", nil), nil
	}
	argsHash, err := jcs.Hash(args)
	if err != nil {
		return nil, err
	}
	if act.Risk != "routine" {
		return c.commandApproval(ctx, tx, now, pc, approvalRow{
			Tenant: pc.tv.Name, Initiator: pc.person, Action: entry.Action, ArgsHash: argsHash, Args: args, Risk: act.Risk,
			RequestedBy: map[string]any{"kind": "human"}, ExpiresAt: now.Add(actionTTL(act)),
		})
	}
	// A routine pipeline runs without a second person within its counters (APR-17, D-78).
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(7300110, hashtext($1))`, pc.tv.Name); err != nil {
		return nil, err
	}
	var hour, day int
	if err := tx.QueryRow(ctx, `SELECT count(*) FILTER (WHERE created_at > $3), count(*) FROM tasks
		WHERE tenant = $1 AND type = $2 AND record ? 'grant_id' AND created_at > $4`,
		pc.tv.Name, "action."+entry.Action, now.Add(-time.Hour), now.Add(-24*time.Hour)).Scan(&hour, &day); err != nil {
		return nil, err
	}
	if act.RunsPerHour > 0 && hour >= act.RunsPerHour || act.RunsPerDay > 0 && day >= act.RunsPerDay {
		return pc.reply(c, "cmd.refused.service_rate_limited", nil), nil
	}
	author := map[string]any{"kind": "human", "principal": pc.person}
	grant, err := c.writeGrant(ctx, tx, now, pc.tv.Name, author, Actor{Principal: pc.person, Via: "channel"}, map[string]any{"action": entry.Action}, argsHash)
	if err != nil {
		return nil, err
	}
	if _, err := c.grantTask(ctx, tx, now, pc, entry.Action, act, args, argsHash, grant, mark); err != nil {
		return nil, err
	}
	title := act.Title
	if title == "" {
		title = entry.Action
	}
	return pc.reply(c, "cmd.run.accepted", map[string]any{"title": title}), nil
}

// coerceArgs turns the positional string values of a command into the types of a closed
// schema: an integer, a number, a boolean, or a comma list for an array; a property the schema
// does not have fails.
func coerceArgs(schema map[string]any, values map[string]string) (map[string]any, bool) {
	props, _ := schema["properties"].(map[string]any)
	out := map[string]any{}
	for k, v := range values {
		p, ok := props[k].(map[string]any)
		if !ok {
			return nil, false
		}
		x, ok := coerce(p, v)
		if !ok {
			return nil, false
		}
		out[k] = x
	}
	return out, true
}

func coerce(p map[string]any, v string) (any, bool) {
	switch p["type"] {
	case "integer":
		n, err := strconv.ParseInt(v, 10, 64)
		return n, err == nil
	case "number":
		n, err := strconv.ParseFloat(v, 64)
		return n, err == nil
	case "boolean":
		b, err := strconv.ParseBool(v)
		return b, err == nil
	case "array":
		items, _ := p["items"].(map[string]any)
		var out []any
		for _, part := range strings.Split(v, ",") {
			x, ok := coerce(items, part)
			if !ok {
				return nil, false
			}
			out = append(out, x)
		}
		return out, true
	}
	return v, true
}
