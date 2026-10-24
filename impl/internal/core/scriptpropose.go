// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/ids"
	"github.com/HunterEwok/botaru/impl/internal/jcs"
	"github.com/HunterEwok/botaru/impl/internal/manifest"
	"github.com/HunterEwok/botaru/impl/internal/mcp"
	"github.com/HunterEwok/botaru/impl/internal/money"
	"github.com/HunterEwok/botaru/impl/internal/toolspec"
)

// Script proposals: the tool propose_script, the checks of a draft and the accepted run (SCR-2, SCR-3, SCR-5).

// scriptVocabulary is the part of the author role's vocabulary the initiator may use: the
// types with the initiator's task:<type> and data class, the actions with action.request:<a>
// (SCR-5, TRN-2).
func (c *Core) scriptVocabulary(cat *Catalog, g *toolsGrant, role manifest.Role) ([]string, []string) {
	tv := cat.Tenants[g.Tenant]
	if tv == nil {
		return nil, nil
	}
	var types, actions []string
	for _, t := range role.Scripts.StepTypes {
		tt, ok := cat.TaskTypes[t]
		if ok && hasCap(tv, g.InitiatorGroups, "task:"+t) && hasData(tv, g.InitiatorGroups, c.higherMark(tt.DataClass, g.Mark)) {
			types = append(types, t)
		}
	}
	for _, a := range role.Scripts.Actions {
		if _, ok := cat.Actions[a]; ok && hasCap(tv, g.InitiatorGroups, "action.request:"+a) {
			actions = append(actions, a)
		}
	}
	return types, actions
}

// proposeScriptTool is the built-in propose_script of a translator (TRN-3). Its description is
// the vocabulary the core built from applied manifests, filtered by the initiator's rights and
// the mark (TRN-2); the core checks the draft itself against the full schema.
func (c *Core) proposeScriptTool(cat *Catalog, g *toolsGrant) *catalogTool {
	role, ok := scriptAuthor(cat, g)
	if !ok {
		return nil
	}
	types, actions := c.scriptVocabulary(cat, g, role)
	var desc strings.Builder
	desc.WriteString("Hand in a script draft botaru.script/v1: {script: \"v1\", steps: [...]}. Step kinds: task (type, goal, params), " +
		"fanout (type and goal, or action and args, with items {resources} or {selector}), action (action, args), confirm (who). " +
		"A goal is {template: <name>} or {assignment: true}; a parameter is a literal or {ref: \"<step>.<field>\"}; conditions use " +
		"eq, ne and in over earlier steps. Every step has an id, unique in the script (a lowercase letter, then lowercase letters, digits or -, such as check-run), " +
		"and its kind. You get the run number or the refused step with its reason, at most 3 drafts per attempt; the person decides. Types:")
	for _, t := range types {
		tt := cat.TaskTypes[t]
		summary := tt.SummaryForManagers
		if summary == "" {
			summary = tt.Title
		}
		fmt.Fprintf(&desc, "\n- %s: %s", t, strings.TrimSpace(summary))
		var names []string
		for _, gt := range tt.GoalTemplates {
			names = append(names, gt.Name)
		}
		if len(names) > 0 {
			fmt.Fprintf(&desc, " Goal templates: %s.", strings.Join(names, ", "))
		}
		desc.WriteString(fieldsPart("Params", tt.ParamsSchema))
	}
	desc.WriteString("\nActions:")
	for _, a := range actions {
		act := cat.Actions[a]
		title := act.Title
		if title == "" {
			title = act.Description
		}
		fmt.Fprintf(&desc, "\n- %s: %s", a, strings.TrimSpace(title))
		desc.WriteString(fieldsPart("Args", act.Args))
	}
	if sel := catalogSelectors(cat, g.Tenant); len(sel) > 0 {
		fmt.Fprintf(&desc, "\nSelectors: %s.", strings.Join(sel, ", "))
	}
	if res := c.campaignResources(cat, g.Tenant, g.Mark); len(res) > 0 {
		fmt.Fprintf(&desc, "\nResources: %s.", strings.Join(res, ", "))
	}
	schema := map[string]any{"type": "object", "additionalProperties": false, "required": []any{"script"},
		"properties": map[string]any{"script": map[string]any{"type": "object"}}}
	return &catalogTool{Name: proposeScriptName, Builtin: true, Tool: toolspec.Tool{Description: desc.String()}, Schema: schema}
}

// fieldSchemaMax is how many bytes of one field's schema the description shows.
const fieldSchemaMax = 300

// fieldsPart is the sentence of a step's params or args: its fields, or none, so a draft does
// not invent a field the closed schema refuses.
func fieldsPart(label string, schema map[string]any) string {
	if props, _ := schema["properties"].(map[string]any); len(props) > 0 {
		return " " + label + ": " + fieldsLine(schema) + "."
	}
	return " " + label + ": none."
}

// fieldsLine is the properties of a closed schema with their schemas, the required ones
// marked, so a draft can fill them in: older_than_days (required) {"maximum":30,...}.
func fieldsLine(schema map[string]any) string {
	props, _ := schema["properties"].(map[string]any)
	required := map[string]bool{}
	for _, r := range mlist(schema, "required") {
		if n, ok := r.(string); ok {
			required[n] = true
		}
	}
	var parts []string
	for _, k := range sortedKeys(props) {
		part := k
		if required[k] {
			part += " (required)"
		}
		if raw, err := json.Marshal(props[k]); err == nil {
			// A long enum or description is cut, so the description stays short.
			if len(raw) > fieldSchemaMax {
				raw = append(raw[:fieldSchemaMax], "..."...)
			}
			part += " " + string(raw)
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, ", ")
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// scriptProblem is the first refusal of an acceptance: the reason, the step number and, for the
// audit only, the full detail (PRM-8).
type scriptProblem struct {
	Step   int
	Reason string
	Detail string
}

func (p *scriptProblem) answer() map[string]any {
	e := map[string]any{"reason": p.Reason}
	if p.Step > 0 {
		e["step"] = p.Step
	}
	return map[string]any{"state": "rejected", "problems": []any{e}}
}

func scriptAnswer(v map[string]any) *mcp.CallResult {
	b, _ := json.Marshal(v)
	return &mcp.CallResult{IsError: v["state"] == "rejected", StructuredContent: v, Content: []mcp.Content{{Type: "text", Text: string(b)}}}
}

// proposeScript is P3 propose_script: at most 3 calls per attempt, acceptance as a whole, and on
// success a proposed run with the script_confirm card to the initiator (TRN-3, SCR-5, SCR-6).
// A refusal writes only the audit and takes no key.
func (c *Core) proposeScript(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, g *toolsGrant, offered bool, args map[string]any) (*mcp.CallResult, error) {
	attempt := max(g.LeaseNo, 1)
	actor := Actor{Principal: workerPrincipal(g.Worker), Via: "bot", Role: g.Role, Task: g.Task}
	audit := func(ok bool, reason, id string, after map[string]any) error {
		after["attempt"] = attempt
		_, err := c.appendAudit(ctx, tx, now, Audit{
			Tenant: g.Tenant, Kind: "authorize", Actor: actor, Command: "script.propose", Target: &Target{Kind: "script", ID: id},
			OK: ok, Reason: reason, After: after,
		})
		return err
	}
	refuse := func(p *scriptProblem) (*mcp.CallResult, error) {
		after := map[string]any{}
		if p.Step > 0 {
			after["step"] = p.Step
		}
		if p.Detail != "" {
			after["detail"] = cut(p.Detail, 200)
		}
		if err := audit(false, p.Reason, "-", after); err != nil {
			return nil, err
		}
		return scriptAnswer(p.answer()), nil
	}
	if _, ok := scriptAuthor(cat, g); !offered || !ok {
		return refuse(&scriptProblem{Reason: "forbidden"})
	}
	var calls int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM audit WHERE tenant = $1 AND record->>'command' = 'script.propose'
		AND record->'actor'->>'task' = $2 AND record->'after'->>'attempt' = $3`, g.Tenant, g.Task, strconv.Itoa(attempt)).Scan(&calls); err != nil {
		return nil, err
	}
	if calls >= maxScriptProposals {
		return refuse(&scriptProblem{Reason: "service_rate_limited", Detail: "at most 3 drafts per attempt"})
	}
	acc, prob, err := c.acceptScript(ctx, tx, now, cat, g, args)
	if err != nil {
		return nil, err
	}
	if prob != nil {
		return refuse(prob)
	}
	id, existing, err := c.insertRun(ctx, tx, now, cat, g, acc)
	if err != nil {
		return nil, err
	}
	if id == "" {
		return refuse(&scriptProblem{Reason: "idempotency_conflict"})
	}
	if err := audit(true, "", id, map[string]any{"steps": len(acc.steps), "plan_hash": acc.planHash, "existing": existing}); err != nil {
		return nil, err
	}
	return scriptAnswer(map[string]any{"run_ref": id, "state": "proposed"}), nil
}

// acceptedScript is a draft that passed acceptance with what the core computed.
type acceptedScript struct {
	script   map[string]any
	steps    []scriptStep
	rec      scriptRec
	planHash string
	// key is the idempotency key of the run when not that of a translator's draft.
	key string
}

// acceptScript is the acceptance chain of SCR-5 in its order: the schema, the references
// (SCR-2), the graph and the conditions (SCR-3), the vocabulary of the author role, the
// initiator's rights for every step, the limits and TRN-4, then the estimate of all branches
// and the tree size. The first refusal rejects the draft as a whole.
func (c *Core) acceptScript(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, g *toolsGrant, args map[string]any) (*acceptedScript, *scriptProblem, error) {
	return c.acceptScriptAs(ctx, tx, now, cat, g, args, false)
}

// acceptScriptAs is acceptScript; noAssignment takes no assignment from the anchor task, as
// for the draft of a proposal, whose analysis goal is no person's assignment (PRP-3).
func (c *Core) acceptScriptAs(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, g *toolsGrant, args map[string]any, noAssignment bool) (*acceptedScript, *scriptProblem, error) {
	tv := cat.Tenants[g.Tenant]
	if tv == nil {
		return nil, &scriptProblem{Reason: "not_ready"}, nil
	}
	if err := c.spec.Validate("script-propose-request", args); err != nil {
		return nil, &scriptProblem{Reason: "script_invalid", Detail: err.Error()}, nil
	}
	script, _ := args["script"].(map[string]any)
	if _, ok := script["params"]; ok {
		return nil, &scriptProblem{Reason: "script_free_goal", Detail: "a draft has no parameters (SCR-14)"}, nil
	}
	if _, ok := script["goals"]; ok {
		return nil, &scriptProblem{Reason: "script_free_goal", Detail: "a draft has no free goals (SCR-4)"}, nil
	}
	steps, err := parseSteps(script)
	if err != nil {
		return nil, &scriptProblem{Reason: "script_invalid", Detail: err.Error()}, nil
	}
	rawSteps, _ := script["steps"].([]any)
	tree, err := loadTree(ctx, tx, g.Task)
	if err != nil {
		return nil, nil, err
	}
	mark := tree.DataMark
	role := cat.Roles[g.Role]
	index := map[string]int{}
	ancestors := make([]map[string]bool, len(steps))
	// SCR-1 and the kinds this core offers (wait and await are of SCR-13, deferred); SCR-3: needs only point back, so the graph has no cycle.
	for i, s := range steps {
		n := i + 1
		raw, _ := rawSteps[i].(map[string]any)
		for _, f := range []string{"for", "event", "match", "timeout"} {
			if _, ok := raw[f]; ok {
				return nil, &scriptProblem{Step: n, Reason: "script_invalid", Detail: "the field " + f + " is of the deferred rule SCR-13"}, nil
			}
		}
		if s.Kind == "wait" || s.Kind == "await" {
			return nil, &scriptProblem{Step: n, Reason: "script_invalid", Detail: "the kind " + s.Kind + " is of the deferred rule SCR-13"}, nil
		}
		if _, dup := index[s.ID]; dup {
			return nil, &scriptProblem{Step: n, Reason: "script_invalid", Detail: "a repeated step id"}, nil
		}
		anc := map[string]bool{}
		for _, need := range s.Needs {
			j, ok := index[need]
			if !ok {
				return nil, &scriptProblem{Step: n, Reason: "script_invalid", Detail: "needs names a step that is not earlier"}, nil
			}
			anc[need] = true
			for a := range ancestors[j] {
				anc[a] = true
			}
		}
		ancestors[i] = anc
		index[s.ID] = i
		for _, v := range s.values() {
			if m, ok := v.(map[string]any); ok && m["param"] != nil {
				return nil, &scriptProblem{Step: n, Reason: "script_invalid", Detail: "a parameter reference is of the deferred rule SCR-14"}, nil
			}
		}
	}
	// SCR-2: references to applied manifests of the tenant only.
	resources := c.campaignResources(cat, g.Tenant, mark)
	var lists []scriptFanout
	for i, s := range steps {
		n := i + 1
		unknown := func(detail string) (*acceptedScript, *scriptProblem, error) {
			return nil, &scriptProblem{Step: n, Reason: "script_ref_unknown", Detail: detail}, nil
		}
		if s.Type != "" {
			tt, ok := cat.TaskTypes[s.Type]
			if !ok || strings.HasPrefix(s.Type, "action.") || tt.Purpose != "work" && tt.Purpose != "report" ||
				slices.ContainsFunc(tt.Events, func(e string) bool { return strings.HasPrefix(e, "alarm.") }) {
				return unknown("the type is not a work or report type of the tenant")
			}
			if s.Goal != nil {
				if name, ok := s.Goal["template"].(string); ok && !slices.ContainsFunc(tt.GoalTemplates, func(gt manifest.GoalTemplate) bool { return gt.Name == name }) {
					return unknown("the type has no such goal template")
				}
			}
		}
		if s.Action != "" {
			act, ok := cat.Actions[s.Action]
			if !ok || act.Script == nil || !act.Script.Allowed {
				return unknown("the action is not allowed in a script")
			}
			// A fanout runs as a campaign, whose card is one press: an action that needs two
			// approvers is not fanned out. An action step opens its own approval with the two
			// votes (APR-16).
			if act.Approvals >= 2 && s.Kind == "fanout" {
				return nil, &scriptProblem{Step: n, Reason: "script_invalid", Detail: "the action needs two approvers"}, nil
			}
		}
		if a := s.approversOf(); a != "" {
			if _, ok := cat.Actions[a]; !ok {
				return unknown("approvers_of names no action")
			}
		}
		if s.Kind == "fanout" {
			var list []string
			if sel, ok := s.Items["selector"].(string); ok {
				l, ok := expandSelector(cat, g.Tenant, sel)
				if !ok || len(l) == 0 {
					return unknown("no such selector")
				}
				list = l
			} else {
				for _, r := range s.Items["resources"].([]any) {
					list = append(list, r.(string))
				}
			}
			for _, r := range list {
				if !slices.Contains(resources, r) {
					return unknown("a resource is not in the catalog at the mark")
				}
			}
			lists = append(lists, scriptFanout{Step: s.ID, Resources: list})
		}
	}
	// SCR-3: conditions and references over closed fields of earlier steps of one result.
	for i, s := range steps {
		n := i + 1
		invalid := func(detail string) (*acceptedScript, *scriptProblem, error) {
			return nil, &scriptProblem{Step: n, Reason: "script_invalid", Detail: detail}, nil
		}
		if s.When != nil {
			atoms := conditionAtoms(s.When)
			if len(atoms) > maxConditionAtoms {
				return invalid("more than 8 atoms")
			}
			for _, a := range atoms {
				ref, _ := a["ref"].(string)
				step, field, _ := strings.Cut(ref, ".")
				if !ancestors[i][step] {
					return invalid("a condition reads a step this one does not wait for")
				}
				if field == "state" {
					continue
				}
				fs, ok := closedField(cat, steps[index[step]], field)
				if !ok {
					return invalid("a condition reads a field that is not closed")
				}
				vals := []any{a["value"]}
				if a["op"] == "in" {
					list, ok := a["value"].([]any)
					if !ok {
						return invalid("in takes a list")
					}
					vals = list
				}
				for _, v := range vals {
					if c.rs.validate(fs, v) != nil {
						return invalid("a literal does not pass the field schema")
					}
				}
			}
		}
		if s.Kind == "confirm" {
			continue
		}
		schema, err := c.stepSchema(cat, g.Tenant, mark, s)
		if err != nil {
			return nil, nil, err
		}
		props, _ := schema["properties"].(map[string]any)
		for k, v := range s.values() {
			p, ok := props[k].(map[string]any)
			if !ok {
				return invalid("an unknown parameter " + k)
			}
			if m, ok := v.(map[string]any); ok {
				ref, _ := m["ref"].(string)
				step, field, _ := strings.Cut(ref, ".")
				if !ancestors[i][step] || field == "state" {
					return invalid("a reference reads a step this one does not wait for")
				}
				fs, ok := closedField(cat, steps[index[step]], field)
				if !ok || !compatible(p, fs) {
					return invalid("a reference is not of a compatible closed kind")
				}
				continue
			}
			if c.rs.validate(p, v) != nil {
				return invalid("the parameter " + k + " does not pass its schema")
			}
		}
		if s.Kind != "fanout" {
			req, _ := schema["required"].([]any)
			for _, r := range req {
				if _, ok := s.values()[r.(string)]; !ok {
					return invalid("a required parameter is missing")
				}
			}
		}
	}
	// The vocabulary of the author role (SCR-5).
	for i, s := range steps {
		if s.Type != "" && !slices.Contains(role.Scripts.StepTypes, s.Type) || s.Action != "" && !slices.Contains(role.Scripts.Actions, s.Action) {
			return nil, &scriptProblem{Step: i + 1, Reason: "no_role_cap"}, nil
		}
	}
	// The initiator's rights for every step, as if they enqueued it themselves (SCR-5).
	pi, err := loadPrincipal(ctx, tx, now, g.Tenant, g.Initiator)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && (pi.State != "active" || pi.Kind != "human") {
		return nil, &scriptProblem{Reason: "forbidden", Detail: "a run needs an active person as its initiator"}, nil
	}
	if err != nil {
		return nil, nil, err
	}
	groups, err := principalGroups(ctx, tx, now, tv, g.Initiator)
	if err != nil {
		return nil, nil, err
	}
	for i, s := range steps {
		if reason := c.stepRights(cat, tv, groups, mark, s); reason != "" {
			return nil, &scriptProblem{Step: i + 1, Reason: reason}, nil
		}
	}
	// Limits (SCR-5) and TRN-4.
	maxSteps := defaultScriptSteps
	if m := tv.Spec.Scripts.MaxSteps; m > 0 {
		maxSteps = m
	}
	if m := role.Scripts.MaxSteps; m > 0 {
		maxSteps = min(maxSteps, m)
	}
	maxSteps = min(maxSteps, farmMaxScriptSteps)
	if len(steps) > maxSteps {
		return nil, &scriptProblem{Reason: "script_too_large", Detail: fmt.Sprintf("more than %d steps", maxSteps)}, nil
	}
	maxActions := farmMaxActionSteps
	if m := tv.Spec.Scripts.MaxActionSteps; m != nil {
		maxActions = min(maxActions, *m)
	}
	itemLimit := defaultCampaignItems
	if m := tv.Spec.Campaigns.MaxItems; m > 0 {
		itemLimit = m
	}
	itemLimit = min(itemLimit, farmMaxCampaignItems)
	untrusted := tree.UntrustedSeen
	actionSteps := 0
	for i, s := range steps {
		n := i + 1
		if s.hasAction() {
			actionSteps++
		}
		if actionSteps > maxActions {
			return nil, &scriptProblem{Step: n, Reason: "script_too_large", Detail: "too many steps with an action"}, nil
		}
		if s.Kind == "fanout" {
			limit := itemLimit
			if act, ok := cat.Actions[s.Action]; ok && act.Campaign != nil && act.Campaign.MaxItems > 0 {
				limit = min(limit, act.Campaign.MaxItems)
			}
			if s.Action != "" && (cat.Actions[s.Action].Campaign == nil || !cat.Actions[s.Action].Campaign.Allowed) {
				return nil, &scriptProblem{Step: n, Reason: "script_too_large", Detail: "the action has no campaign"}, nil
			}
			if len(lists[slices.IndexFunc(lists, func(f scriptFanout) bool { return f.Step == s.ID })].Resources) > limit {
				return nil, &scriptProblem{Step: n, Reason: "script_too_large", Detail: "more items than the campaign ceilings"}, nil
			}
		}
		if untrusted {
			_, selector := s.Items["selector"]
			if s.Kind == "fanout" && s.Action != "" || s.Kind == "action" && actionSteps > 1 || s.hasAction() && selector {
				return nil, &scriptProblem{Step: n, Reason: "script_untrusted_limit"}, nil
			}
		}
	}
	deadline := farmScriptRunDeadline
	if d, ok := parseDuration(tv.Spec.Scripts.RunDeadline); ok && d < deadline {
		deadline = d
	}
	// The estimate of all branches at once and the tree size (SCR-5, BUD-14).
	rec := scriptRec{
		Script: script, DataMark: mark, Actions: []string{}, Marks: map[string][]string{}, StepMicros: map[string]int64{}, StepTree: map[string]int{},
		Deadline: deadline.String(), Role: g.Role, Worker: g.Worker, Root: tree.Root, Depth: max(tree.Depth, tree.DepthFloor),
		Mark: mark, Conversation: tree.Conversation, Trace: tree.Trace, Origin: tree.Origin, FanoutLists: lists,
	}
	var total int64
	var assignment string
	var body []byte
	if err := tx.QueryRow(ctx, `SELECT coalesce(body, 'null'::jsonb), record FROM tasks WHERE id = $1`, g.Task).Scan(&body, new([]byte)); err != nil {
		return nil, nil, err
	}
	var b struct {
		Goal string `json:"goal"`
	}
	_ = json.Unmarshal(body, &b)
	if !noAssignment {
		assignment = b.Goal
	}
	rec.Assignment = assignment
	for _, s := range steps {
		var micros int64
		tasks := 0
		switch {
		case s.Type != "":
			tt := cat.TaskTypes[s.Type]
			est, err := taskEstimate(ctx, tx, now, g.Tenant, s.Type, tt)
			if err != nil {
				return nil, nil, err
			}
			rec.MaxTier = max(rec.MaxTier, tt.Tier)
			rec.DataMark = c.higherMark(tt.DataClass, rec.DataMark)
			tasks = 1
			if s.Kind == "fanout" {
				tasks = len(rec.fanoutList(s.ID))
			}
			micros = est * int64(tasks)
		case s.Action != "":
			rec.MaxTier = 3
			tasks = 1
			if s.Kind == "fanout" {
				tasks = len(rec.fanoutList(s.ID))
			}
			if !slices.Contains(rec.Actions, s.Action) {
				rec.Actions = append(rec.Actions, s.Action)
			}
		}
		rec.StepMicros[s.ID], rec.StepTree[s.ID] = micros, tasks
		total += micros
		rec.Estimate.TreeTasks += tasks
		rec.Marks[s.ID] = c.stepMarks(cat, s, assignment)
		if s.hasAction() || s.Kind == "confirm" || slices.Contains(rec.Marks[s.ID], "may_control_services") {
			rec.NeedsRun = true
		}
		if u := unnamedOf(cat, tv, resources, s, rec.fanoutList(s.ID), assignment); u != nil {
			if rec.Unnamed == nil {
				rec.Unnamed = map[string]*unnamedTargets{}
			}
			rec.Unnamed[s.ID] = u
			rec.Marks[s.ID] = append(rec.Marks[s.ID], "targets_not_in_assignment")
		}
	}
	// HLD-9, HLD-3: under a no_action hold on the author role, a draft that acts (an action, or
	// a step that may control services or run monitoring scripts) is rejected; read-only
	// drafts still pass.
	if c.scriptActs(cat, steps) {
		held, err := heldBy(ctx, tx, now, []string{"no_action"}, append(tenantScopes(g.Tenant), scopeRef{kind: "role", tenant: g.Tenant, name: g.Role},
			scopeRef{kind: "worker", tenant: g.Tenant, name: strings.ToLower(g.Worker)})...)
		if err != nil {
			return nil, nil, err
		}
		if held != "" {
			return nil, &scriptProblem{Reason: "held", Detail: "a hold stops the actions of the role"}, nil
		}
	}
	rec.Estimate.Micros = total
	rec.Estimate.EUR = float64(money.CentsUp(total)) / 100
	treeLimit := defaultMaxTreeTasks
	if m := tv.Spec.Campaigns.MaxTreeTasks; m > 0 {
		treeLimit = m
	}
	treeLimit = min(treeLimit, farmMaxTreeTasks)
	used, err := treeSize(ctx, tx, g.Tenant, tree.Root)
	if err != nil {
		return nil, nil, err
	}
	if used+rec.Estimate.TreeTasks > treeLimit {
		return nil, &scriptProblem{Reason: "tree_too_large"}, nil
	}
	if tree.UntrustedSeen {
		var tr struct {
			Source string `json:"untrusted_source"`
		}
		var raw []byte
		if err := tx.QueryRow(ctx, `SELECT record FROM tasks WHERE id = $1`, g.Task).Scan(&raw); err != nil {
			return nil, nil, err
		}
		_ = json.Unmarshal(raw, &tr)
		src := tr.Source
		if src == "" {
			src = "parent"
		}
		rec.Untrusted = scriptUntrusted{Value: true, Sources: []string{src}}
	}
	planHash, err := jcs.Hash(map[string]any{"script": script, "fanout_lists": lists})
	if err != nil {
		return nil, nil, err
	}
	return &acceptedScript{script: script, steps: steps, rec: rec, planHash: planHash}, nil, nil
}

// conditionAtoms lists the atoms of a when condition.
func conditionAtoms(w map[string]any) []map[string]any {
	if _, ok := w["ref"]; ok {
		return []map[string]any{w}
	}
	var out []map[string]any
	for _, k := range []string{"all", "any"} {
		if list, ok := w[k].([]any); ok {
			for _, x := range list {
				if m, ok := x.(map[string]any); ok {
					out = append(out, conditionAtoms(m)...)
				}
			}
		}
	}
	if m, ok := w["not"].(map[string]any); ok {
		out = append(out, conditionAtoms(m)...)
	}
	return out
}

// closedField is the schema of a result field that code reads: a field of a step with one
// result (a task step), marked x-botaru-read-by: code (OUT-1, SCR-3).
func closedField(cat *Catalog, s scriptStep, field string) (map[string]any, bool) {
	if s.Kind != "task" {
		return nil, false
	}
	props, _ := cat.TaskTypes[s.Type].ResultSchema["properties"].(map[string]any)
	f, ok := props[field].(map[string]any)
	if !ok || f["x-botaru-read-by"] != "code" {
		return nil, false
	}
	return f, true
}

// compatible says whether a closed result field may fill a parameter: the same catalog list, an
// enum no wider, a number within the bounds, or a boolean (SCR-3).
func compatible(param, field map[string]any) bool {
	if ef, ok := param["enum_from"].(string); ok {
		return field["enum_from"] == ef
	}
	if pe, ok := param["enum"].([]any); ok {
		fe, ok := field["enum"].([]any)
		if !ok {
			return false
		}
		for _, v := range fe {
			if !slices.Contains(pe, v) {
				return false
			}
		}
		return true
	}
	num := func(m map[string]any) bool { return m["type"] == "number" || m["type"] == "integer" }
	if num(param) && num(field) {
		if param["type"] == "integer" && field["type"] != "integer" {
			return false
		}
		pmin, okp := param["minimum"].(float64)
		fmin, okf := field["minimum"].(float64)
		if okp && (!okf || fmin < pmin) {
			return false
		}
		pmax, okp := param["maximum"].(float64)
		fmax, okf := field["maximum"].(float64)
		return !okp || okf && fmax <= pmax
	}
	return param["type"] == "boolean" && field["type"] == "boolean"
}

// stepSchema is the closed schema of a step's values at a mark: the type's params or the
// action's arguments with the catalog lists filtered by the mark (MAN-9, MAN-10).
func (c *Core) stepSchema(cat *Catalog, tenant, mark string, s scriptStep) (map[string]any, error) {
	if s.Action != "" {
		return c.actionArgsSchema(cat, tenant, mark, cat.Actions[s.Action])
	}
	tt := cat.TaskTypes[s.Type]
	m := c.higherMark(tt.DataClass, mark)
	if tt.ParamsSchema == nil {
		return map[string]any{"type": "object", "properties": map[string]any{}}, nil
	}
	names := map[string]bool{}
	enumFromNames(tt.ParamsSchema, names)
	return c.fullSchema(tt.ParamsSchema, c.resolveEnumFor(cat, tenant, m, "", names))
}

// stepRights is the initiator's right for one step: task:<type> and the data class, or
// action.request:<a> (SCR-5, SCR-8); empty when the right holds.
func (c *Core) stepRights(cat *Catalog, tv *TenantView, groups []string, mark string, s scriptStep) string {
	if s.Type != "" {
		if !hasCap(tv, groups, "task:"+s.Type) {
			return "no_group_cap"
		}
		if !hasData(tv, groups, c.higherMark(cat.TaskTypes[s.Type].DataClass, mark)) {
			return "no_data_class"
		}
	}
	if s.Action != "" && !hasCap(tv, groups, "action.request:"+s.Action) {
		return "no_group_cap"
	}
	return ""
}

// stepMarks are the code marks of a step on the run card (SCR-6, TRN-2): an action that needs
// its own approval or runs as routine, a type whose bot may control services, and an action the
// typed text does not name.
func (c *Core) stepMarks(cat *Catalog, s scriptStep, assignment string) []string {
	var out []string
	if s.hasAction() {
		act := cat.Actions[s.Action]
		if s.Kind == "action" && act.Risk == "routine" {
			out = append(out, "routine_no_card")
		} else {
			out = append(out, "needs_separate_approval")
		}
		text := strings.ToLower(assignment)
		if !strings.Contains(text, strings.ToLower(s.Action)) && (act.Title == "" || !strings.Contains(text, strings.ToLower(act.Title))) {
			out = append(out, "not_in_assignment")
		}
	}
	if s.Type != "" {
		for _, r := range cat.Roles {
			if slices.Contains(r.Claims, s.Type) && (len(r.ServiceControl) > 0 || len(r.MonitorScripts) > 0) {
				out = append(out, "may_control_services")
				break
			}
		}
	}
	return out
}

// insertRun writes the proposed run with its steps, supersedes a waiting draft of the same
// person in the same conversation and plans the script_confirm card (SCR-5, SCR-6). A repeat of
// the same plan from the same attempt is the same run; the empty id is an idempotency conflict.
func (c *Core) insertRun(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, g *toolsGrant, acc *acceptedScript) (string, bool, error) {
	key := "system:script:propose:" + g.Task + ":" + acc.planHash
	if acc.key != "" {
		key = acc.key
	}
	var id string
	err := tx.QueryRow(ctx, `SELECT id FROM script_runs WHERE tenant = $1 AND idempotency_key = $2`, g.Tenant, key).Scan(&id)
	if err == nil {
		return id, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", false, err
	}
	tv := cat.Tenants[g.Tenant]
	// A new draft in the same conversation supersedes a waiting one (SCR-6).
	if acc.rec.Conversation != "" {
		rows, err := tx.Query(ctx, `SELECT `+scriptColumns+` FROM script_runs WHERE tenant = $1 AND initiator = $2 AND state = 'proposed'
			AND record->>'conversation' = $3 ORDER BY id FOR UPDATE`, g.Tenant, g.Initiator, acc.rec.Conversation)
		if err != nil {
			return "", false, err
		}
		var old []*scriptRun
		for rows.Next() {
			r, err := scanRun(rows)
			if err != nil {
				rows.Close()
				return "", false, err
			}
			old = append(old, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return "", false, err
		}
		for _, r := range old {
			if err := c.rejectRun(ctx, tx, now, r, "script_superseded"); err != nil {
				return "", false, err
			}
		}
	}
	id = ids.New(ids.Script)
	raw, err := json.Marshal(acc.rec)
	if err != nil {
		return "", false, err
	}
	// The role and the mark stay in columns past the purge of the record (TRN-7, HLD-9).
	if _, err := tx.Exec(ctx, `INSERT INTO script_runs (id, tenant, anchor_task, initiator, state, plan_hash, record, idempotency_key, nonce, expires_at, created_at, updated_at,
		role, data_mark)
		VALUES ($1, $2, $3, $4, 'proposed', $5, $6, $7, $8, $9, $10, $10, $11, $12)`,
		id, g.Tenant, g.Task, g.Initiator, acc.planHash, raw, key, ids.Secret(16), now.Add(scriptCardTTL), now, nullable(acc.rec.Role), nullable(acc.rec.DataMark)); err != nil {
		return "", false, err
	}
	for i, s := range acc.steps {
		if _, err := tx.Exec(ctx, `INSERT INTO script_steps (run, n, id, kind, state) VALUES ($1, $2, $3, $4, 'pending')`, id, i+1, s.ID, s.Kind); err != nil {
			return "", false, err
		}
	}
	run, err := loadRun(ctx, tx, g.Tenant, id, true)
	if err != nil {
		return "", false, err
	}
	if err := c.scriptEvent(ctx, tx, now, run, "script.drafted.v1", nil, "proposed", ""); err != nil {
		return "", false, err
	}
	if err := c.planScriptCard(ctx, tx, now, tv, run, scriptConfirmCard, scriptConfirmCard, run.Initiator); err != nil {
		return "", false, err
	}
	return id, false, notifyWork(ctx, tx)
}
