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
	"github.com/HunterEwok/botaru/impl/internal/store"
	"github.com/HunterEwok/botaru/impl/internal/toolspec"
)

// Scripts (spec/lifecycles/script.md, docs/coordination.md). A script is data under the closed
// schema botaru.script/v1, never code (SCR-1). A translator attempt hands in a draft through
// P3 propose_script; the core accepts it synchronously and as a whole or refuses it with the
// step and the reason (SCR-2, SCR-3, SCR-5), and writes the fields only it computes. A run
// starts only on its initiator's "Run" button (SCR-6), which approves no action (SCR-7); then
// deterministic core code releases the steps, rechecks the initiator's rights at every
// release and passes the untrusted flag along the data a step reads (SCR-8, SCR-9), by the
// transition table only (SCR-11).

const (
	proposeScriptName = "propose_script"
	// maxScriptProposals is the ceiling of propose_script calls per attempt (TRN-3).
	maxScriptProposals = 3
	// Script ceilings: the farm, the tenant default, the action steps and the run deadline
	// (SCR-1, SCR-5).
	farmMaxScriptSteps    = 20
	defaultScriptSteps    = 10
	farmMaxActionSteps    = 3
	farmScriptRunDeadline = 6 * time.Hour
	// scriptCardTTL is the deadline of the run card and of a confirm step (decision-points).
	scriptCardTTL     = 30 * time.Minute
	maxConditionAtoms = 8
	scriptConfirmCard = "script_confirm"
	scriptGateCard    = "script_gate_card"
	scriptSummaryKind = "script_summary"
	// scriptProgressEvery is how often step changes edit the run card (ORC-10, DLV-6).
	scriptProgressEvery = 30 * time.Second
)

// scriptLive are the states of a run that still waits or works.
var scriptLive = []string{"proposed", "running", "paused"}

// stepTerminal are the terminal states of a step.
var stepTerminal = []string{"ok", "failed", "unknown", "declined", "skipped", "canceled"}

// scriptStep is one step of an accepted draft.
type scriptStep struct {
	ID        string         `json:"id"`
	Kind      string         `json:"kind"`
	Needs     []string       `json:"needs,omitempty"`
	When      map[string]any `json:"when,omitempty"`
	RunIf     string         `json:"run_if,omitempty"`
	OnFailure string         `json:"on_failure,omitempty"`
	Type      string         `json:"type,omitempty"`
	Action    string         `json:"action,omitempty"`
	Goal      map[string]any `json:"goal,omitempty"`
	Params    map[string]any `json:"params,omitempty"`
	Args      map[string]any `json:"args,omitempty"`
	Items     map[string]any `json:"items,omitempty"`
	Rollout   map[string]any `json:"rollout,omitempty"`
	Who       any            `json:"who,omitempty"`
}

func (s scriptStep) runIf() string {
	if s.RunIf == "" {
		return "succeeded"
	}
	return s.RunIf
}

func (s scriptStep) onFailure() string {
	if s.OnFailure == "" {
		return "stop"
	}
	return s.OnFailure
}

// hasAction says whether the step changes the world through an action (SCR-7).
func (s scriptStep) hasAction() bool {
	return s.Kind == "action" || s.Kind == "fanout" && s.Action != ""
}

// values are the parameters of a type step or the arguments of an action step.
func (s scriptStep) values() map[string]any {
	if s.Action != "" {
		return s.Args
	}
	return s.Params
}

// approversOf is the action whose approvers decide a confirm step, or empty for the initiator.
func (s scriptStep) approversOf() string {
	if m, ok := s.Who.(map[string]any); ok {
		a, _ := m["approvers_of"].(string)
		return a
	}
	return ""
}

func parseSteps(script map[string]any) ([]scriptStep, error) {
	var d struct {
		Steps []scriptStep `json:"steps"`
	}
	raw, err := json.Marshal(script)
	if err != nil {
		return nil, err
	}
	err = json.Unmarshal(raw, &d)
	return d.Steps, err
}

// scriptUntrusted is the untrusted flag with its sources as codes (INJ-13, SCR-9).
type scriptUntrusted struct {
	Value   bool     `json:"value"`
	Sources []string `json:"sources,omitempty"`
}

func (u scriptUntrusted) or(o scriptUntrusted) scriptUntrusted {
	out := scriptUntrusted{Value: u.Value || o.Value, Sources: slices.Clone(u.Sources)}
	for _, s := range o.Sources {
		if !slices.Contains(out.Sources, s) && len(out.Sources) < 16 {
			out.Sources = append(out.Sources, s)
		}
	}
	return out
}

func (u scriptUntrusted) source() string {
	if len(u.Sources) > 0 {
		return u.Sources[0]
	}
	if u.Value {
		return "script_step"
	}
	return ""
}

type scriptEstimate struct {
	EUR       float64 `json:"eur"`
	TreeTasks int     `json:"tree_tasks"`
	Micros    int64   `json:"micros"`
}

type scriptFanout struct {
	Step      string   `json:"step"`
	Resources []string `json:"resources"`
}

// scriptRec is the run record: the accepted draft and what only the core computes (SCR-5), and
// the parts of the anchor's tree the core needs to release the steps.
type scriptRec struct {
	Script      map[string]any  `json:"script"`
	MaxTier     int             `json:"max_tier"`
	DataMark    string          `json:"data_mark"`
	Untrusted   scriptUntrusted `json:"untrusted_seen"`
	Actions     []string        `json:"actions"`
	Estimate    scriptEstimate  `json:"estimate"`
	FanoutLists []scriptFanout  `json:"fanout_lists,omitempty"`
	// Marks are the code marks of each step for the run card (SCR-6, TRN-2); NeedsRun says
	// whether "Run" needs the privileged script.run.
	Marks    map[string][]string `json:"marks,omitempty"`
	NeedsRun bool                `json:"needs_script_run"`
	// Unnamed are, per step, the targets the typed text does not name (TRN-10).
	Unnamed    map[string]*unnamedTargets `json:"unnamed_targets,omitempty"`
	StepMicros map[string]int64           `json:"step_micros,omitempty"`
	StepTree   map[string]int             `json:"step_tree,omitempty"`
	Deadline   string                     `json:"run_deadline"`
	// Assignment is the text the initiator typed, the goal of {assignment: true} (SCR-4).
	Assignment   string  `json:"assignment,omitempty"`
	Role         string  `json:"role,omitempty"`
	Worker       string  `json:"worker,omitempty"`
	Root         string  `json:"root"`
	Depth        int     `json:"depth"`
	Mark         string  `json:"mark"`
	Conversation string  `json:"conversation,omitempty"`
	Trace        string  `json:"trace,omitempty"`
	Origin       *Origin `json:"origin,omitempty"`
	// Purged marks the tombstone of a finished run past the content period or erased: the
	// record keeps only the root, and the steps keep their states and links (RET-2, RET-10).
	Purged bool `json:"purged,omitempty"`
}

func (r scriptRec) fanoutList(step string) []string {
	for _, f := range r.FanoutLists {
		if f.Step == step {
			return f.Resources
		}
	}
	return nil
}

// scriptRun is a run as the core reads it.
type scriptRun struct {
	ID, Tenant, Anchor, Initiator, State, Reason, PlanHash, Nonce string
	Rec                                                           scriptRec
	ExpiresAt                                                     time.Time
	DeadlineAt, ProgressAt                                        *time.Time
	Stopped                                                       bool
	TreeLeft                                                      int
	CreatedAt                                                     time.Time
}

const scriptColumns = `id, tenant, coalesce(anchor_task, ''), initiator, state, coalesce(reason, ''), plan_hash, record, nonce, expires_at,
	deadline_at, stopped, tree_left, progress_at, created_at`

func scanRun(row pgx.Row) (*scriptRun, error) {
	var r scriptRun
	var rec []byte
	if err := row.Scan(&r.ID, &r.Tenant, &r.Anchor, &r.Initiator, &r.State, &r.Reason, &r.PlanHash, &rec, &r.Nonce, &r.ExpiresAt,
		&r.DeadlineAt, &r.Stopped, &r.TreeLeft, &r.ProgressAt, &r.CreatedAt); err != nil {
		return nil, err
	}
	return &r, json.Unmarshal(rec, &r.Rec)
}

func loadRun(ctx context.Context, q store.Querier, tenant, id string, lock bool) (*scriptRun, error) {
	sql := `SELECT ` + scriptColumns + ` FROM script_runs WHERE id = $1 AND tenant = $2`
	if lock {
		sql += ` FOR UPDATE`
	}
	return scanRun(q.QueryRow(ctx, sql, id, tenant))
}

// stepRow is a step as the core reads it.
type stepRow struct {
	N                                                              int
	ID, Kind, State, Reason, Task, Campaign, Approval, Gate, Nonce string
	ExpiresAt                                                      *time.Time
	Untrusted                                                      scriptUntrusted
	Fields                                                         map[string]any
}

const stepColumns = `n, id, kind, state, coalesce(reason, ''), coalesce(task, ''), coalesce(campaign, ''), coalesce(approval, ''),
	coalesce(gate, ''), coalesce(nonce, ''), expires_at, untrusted, fields`

func scanStep(row pgx.Row) (*stepRow, error) {
	var s stepRow
	var unt, fields []byte
	if err := row.Scan(&s.N, &s.ID, &s.Kind, &s.State, &s.Reason, &s.Task, &s.Campaign, &s.Approval, &s.Gate, &s.Nonce, &s.ExpiresAt, &unt, &fields); err != nil {
		return nil, err
	}
	if len(unt) > 0 {
		if err := json.Unmarshal(unt, &s.Untrusted); err != nil {
			return nil, err
		}
	}
	if len(fields) > 0 {
		if err := json.Unmarshal(fields, &s.Fields); err != nil {
			return nil, err
		}
	}
	return &s, nil
}

func loadSteps(ctx context.Context, q store.Querier, run string) ([]*stepRow, error) {
	rows, err := q.Query(ctx, `SELECT `+stepColumns+` FROM script_steps WHERE run = $1 ORDER BY n`, run)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*stepRow
	for rows.Next() {
		s, err := scanStep(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// Record is the run as spec/model/script-run.schema.json describes it.
func (r *scriptRun) Record(ctx context.Context, q store.Querier) (map[string]any, error) {
	steps, err := loadSteps(ctx, q, r.ID)
	if err != nil {
		return nil, err
	}
	unt := map[string]any{"value": r.Rec.Untrusted.Value}
	if len(r.Rec.Untrusted.Sources) > 0 {
		unt["sources"] = r.Rec.Untrusted.Sources
	}
	actions := r.Rec.Actions
	if actions == nil {
		actions = []string{}
	}
	out := map[string]any{
		"id": r.ID, "tenant": r.Tenant, "initiator": r.Initiator, "script": r.Rec.Script, "max_tier": r.Rec.MaxTier,
		"data_mark": r.Rec.DataMark, "untrusted_seen": unt, "actions": actions,
		"estimate": map[string]any{"eur": r.Rec.Estimate.EUR, "tree_tasks": r.Rec.Estimate.TreeTasks}, "plan_hash": r.PlanHash, "state": r.State,
	}
	if r.Anchor != "" {
		out["anchor_task"] = r.Anchor
	}
	if r.Reason != "" {
		out["reason"] = r.Reason
	}
	if r.DeadlineAt != nil {
		out["deadline"] = fmtTime(*r.DeadlineAt)
	}
	if len(r.Rec.FanoutLists) > 0 {
		out["fanout_lists"] = r.Rec.FanoutLists
	}
	list := make([]any, 0, len(steps))
	for _, s := range steps {
		e := map[string]any{"id": s.ID, "state": s.State}
		set := func(k, v string) {
			if v != "" {
				e[k] = v
			}
		}
		set("reason", s.Reason)
		set("task", s.Task)
		set("campaign", s.Campaign)
		set("approval", s.Approval)
		if s.Untrusted.Value {
			u := map[string]any{"value": true}
			if len(s.Untrusted.Sources) > 0 {
				u["sources"] = s.Untrusted.Sources
			}
			e["untrusted_seen"] = u
		}
		if len(s.Fields) > 0 {
			e["fields"] = s.Fields
		}
		list = append(list, e)
	}
	out["steps"] = list
	return out, nil
}

// scriptEvent writes one of the script events with metadata only (AUD-2, DLV-2).
func (c *Core) scriptEvent(ctx context.Context, tx pgx.Tx, now time.Time, r *scriptRun, typ string, step *stepRow, state, reason string) error {
	data := map[string]any{"event": strings.TrimSuffix(typ, ".v1"), "run": r.ID, "state": state}
	if step != nil {
		data["step"], data["kind"] = step.ID, step.Kind
	}
	if reason != "" {
		data["reason"] = reason
	}
	return c.appendEvent(ctx, tx, now, r.Tenant, Event{Type: typ, Subject: r.ID, Data: data, Causation: r.Anchor})
}

// scriptAuthor reports whether an attempt may hand in drafts: a translator role with
// scripts.propose: draft in a task of purpose translate (PRF-1, TRN-3).
func scriptAuthor(cat *Catalog, g *toolsGrant) (manifest.Role, bool) {
	role, ok := cat.Roles[g.Role]
	return role, ok && role.Profile == "translator" && role.Scripts != nil && role.Scripts.Propose == "draft" && g.Purpose == "translate"
}

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
		"eq, ne and in over earlier steps. You get the run number or the refused step with its reason; the person decides. Types:")
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
		if props, _ := tt.ParamsSchema["properties"].(map[string]any); len(props) > 0 {
			fmt.Fprintf(&desc, " Params: %s.", strings.Join(sortedKeys(props), ", "))
		}
	}
	desc.WriteString("\nActions:")
	for _, a := range actions {
		act := cat.Actions[a]
		title := act.Title
		if title == "" {
			title = act.Description
		}
		fmt.Fprintf(&desc, "\n- %s: %s", a, strings.TrimSpace(title))
		if props, _ := act.Args["properties"].(map[string]any); len(props) > 0 {
			fmt.Fprintf(&desc, " Args: %s.", strings.Join(sortedKeys(props), ", "))
		}
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
}

// acceptScript is the acceptance chain of SCR-5 in its order: the schema, the references
// (SCR-2), the graph and the conditions (SCR-3), the vocabulary of the author role, the
// initiator's rights for every step, the limits and TRN-4, then the estimate of all branches
// and the tree size. The first refusal rejects the draft as a whole.
func (c *Core) acceptScript(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, g *toolsGrant, args map[string]any) (*acceptedScript, *scriptProblem, error) {
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
	// SCR-1 and the v0.1 kinds; SCR-3: needs only point back, so the graph has no cycle.
	for i, s := range steps {
		n := i + 1
		raw, _ := rawSteps[i].(map[string]any)
		for _, f := range []string{"for", "event", "match", "timeout"} {
			if _, ok := raw[f]; ok {
				return nil, &scriptProblem{Step: n, Reason: "script_invalid", Detail: "the field " + f + " comes in v0.2"}, nil
			}
		}
		if s.Kind == "wait" || s.Kind == "await" {
			return nil, &scriptProblem{Step: n, Reason: "script_invalid", Detail: "the kind " + s.Kind + " comes in v0.2"}, nil
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
				return nil, &scriptProblem{Step: n, Reason: "script_invalid", Detail: "a parameter reference comes in v0.2"}, nil
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
	assignment = b.Goal
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
	if scriptActs(steps, rec.Marks) {
		held, err := heldBy(ctx, tx, now, []string{"no_action"}, append(tenantScopes(g.Tenant), scopeRef{kind: "role", tenant: g.Tenant, name: g.Role})...)
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
	if _, err := tx.Exec(ctx, `INSERT INTO script_runs (id, tenant, anchor_task, initiator, state, plan_hash, record, idempotency_key, nonce, expires_at, created_at, updated_at)
		VALUES ($1, $2, $3, $4, 'proposed', $5, $6, $7, $8, $9, $10, $10)`,
		id, g.Tenant, g.Task, g.Initiator, acc.planHash, raw, key, ids.Secret(16), now.Add(scriptCardTTL), now); err != nil {
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

// planScriptCard plans one run card delivery to a person's direct chat, drawn from the record
// when a channel takes it; the run card goes to the initiator in private even when the
// assignment came from a group (SCR-6, D-48).
func (c *Core) planScriptCard(ctx context.Context, tx pgx.Tx, now time.Time, tv *TenantView, r *scriptRun, kind, card, person string) error {
	if tv == nil {
		return nil
	}
	channel, chat, ok, err := cardChat(ctx, tx, tv, person)
	if err != nil || !ok {
		return err
	}
	// The run card shows the draft's parameters and arguments, so it carries the run's mark.
	mark := r.Rec.DataMark
	_, err = c.planDelivery(ctx, tx, now, plannedDelivery{
		Tenant: tv.Name, Task: r.Anchor, Kind: kind, Addr: "approval_initiator", TargetKind: "chat", TargetID: chat.ID(),
		ExecKind: "channel", ExecName: channel, Recipient: person, Body: map[string]any{"form": "card", "card": card}, DataOrigin: "code",
		DataMark: mark, SubjectKind: "script", SubjectID: r.ID,
	})
	return err
}

// scriptCardUpdates edits the sent run cards: always on a run state change, and on step changes
// at most once in 30 seconds.
func (c *Core) scriptCardUpdates(ctx context.Context, tx pgx.Tx, now time.Time, r *scriptRun, stateChanged bool) error {
	if !stateChanged && r.ProgressAt != nil && now.Sub(*r.ProgressAt) < scriptProgressEvery {
		return nil
	}
	rows, err := tx.Query(ctx, `SELECT id, exec_name, target_id, coalesce(recipient, '') FROM deliveries
		WHERE subject_kind = 'script' AND subject_id = $1 AND kind = $2 AND state IN ('planned', 'sending', 'delivered') ORDER BY seq`, r.ID, scriptConfirmCard)
	if err != nil {
		return err
	}
	type target struct{ id, channel, chat, recipient string }
	var ts []target
	for rows.Next() {
		var t target
		if err := rows.Scan(&t.id, &t.channel, &t.chat, &t.recipient); err != nil {
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
			Tenant: r.Tenant, Task: r.Anchor, Kind: cardUpdate, Addr: "approval_initiator", TargetKind: "chat", TargetID: t.chat,
			ExecKind: "channel", ExecName: t.channel, Recipient: t.recipient, Body: map[string]any{"form": "card", "card": scriptConfirmCard, "of": t.id},
			DataOrigin: "code", SubjectKind: "script", SubjectID: r.ID,
		}); err != nil {
			return err
		}
	}
	at := now
	r.ProgressAt = &at
	_, err = tx.Exec(ctx, `UPDATE script_runs SET progress_at = $2 WHERE id = $1`, r.ID, now)
	return err
}

// buildScriptCard draws the run card from the record in code words (SCR-6): the step count, all
// steps in order with their targets and marks, the estimate, the untrusted flag with its source,
// plan_hash, the deadline and the bottom line; no model text reaches it.
func (c *Core) buildScriptCard(ctx context.Context, q store.Querier, cat *Catalog, r *scriptRun, kind string) (map[string]any, error) {
	steps, err := parseSteps(r.Rec.Script)
	if err != nil {
		return nil, err
	}
	rows, err := loadSteps(ctx, q, r.ID)
	if err != nil {
		return nil, err
	}
	list := make([]any, 0, len(steps))
	for i, s := range steps {
		e := map[string]any{"n": i + 1, "id": s.ID, "kind": s.Kind}
		switch {
		case s.Type != "":
			e["type"] = s.Type
		case s.Action != "":
			e["action"] = s.Action
		}
		if s.Kind == "fanout" {
			e["targets"] = len(r.Rec.fanoutList(s.ID))
		} else if s.Kind != "confirm" {
			e["targets"] = 1
		}
		// The person sees what each step acts on before Run (SCR-6).
		if s.Kind == "fanout" {
			if l := r.Rec.fanoutList(s.ID); len(l) > 0 {
				e["resources"] = l
			}
		}
		if len(s.Args) > 0 {
			e["args"] = s.Args
		}
		if len(s.Params) > 0 {
			e["params"] = s.Params
		}
		if s.When != nil {
			e["condition"] = true
		}
		if len(s.Needs) > 0 {
			e["needs"] = s.Needs
		}
		if m := r.Rec.Marks[s.ID]; len(m) > 0 {
			e["marks"] = m
		}
		if i < len(rows) {
			e["state"] = rows[i].State
		}
		list = append(list, e)
	}
	ctxLine := map[string]any{
		"steps": len(steps), "estimate_eur": strconv.FormatFloat(r.Rec.Estimate.EUR, 'f', 2, 64), "tree_tasks": r.Rec.Estimate.TreeTasks,
		"plan_hash": r.PlanHash, "script_run_required": r.Rec.NeedsRun,
	}
	if r.Reason != "" {
		ctxLine["reason"] = r.Reason
	}
	card := map[string]any{
		"card": kind, "subject_kind": "script", "id": r.ID, "nonce": r.Nonce, "title": fmt.Sprintf("Script of %d steps", len(steps)),
		"args": map[string]any{"steps": list}, "requested_by": map[string]any{"initiator": r.Initiator, "source": "human", "task": r.Anchor},
		"initiator_display": cut(r.Initiator, 80), "untrusted_seen": r.Rec.Untrusted.Value, "state": r.State, "context": ctxLine,
	}
	if r.Anchor == "" {
		delete(card["requested_by"].(map[string]any), "task")
	}
	if src := r.Rec.Untrusted.source(); r.Rec.Untrusted.Value && src != "" {
		card["untrusted_source"] = cut(src, 120)
	}
	footer := map[string]any{"why_you": "the initiator of the script", "on_silence": "the run is rejected and nothing runs"}
	if r.State == "proposed" {
		card["expires_at"] = fmtTime(r.ExpiresAt)
		footer["until"] = fmtTime(r.ExpiresAt)
	} else {
		footer["on_silence"] = "nothing"
		if r.DeadlineAt != nil {
			footer["until"] = fmtTime(*r.DeadlineAt)
		}
	}
	card["footer"] = footer
	button := func(code string) map[string]any {
		return map[string]any{"code": code, "subject_kind": "script", "id": r.ID, "nonce": r.Nonce}
	}
	buttons := []any{}
	if kind == scriptConfirmCard {
		switch r.State {
		case "proposed":
			buttons = append(buttons, button("approve"), button("reject"))
		case "running":
			buttons = append(buttons, button("pause"), button("cancel"))
		case "paused":
			buttons = append(buttons, button("approve"), button("cancel"))
		}
	}
	card["buttons"] = buttons
	return card, nil
}

// buildGateCard draws the card of a confirm step (subject script_gate).
func buildGateCard(r *scriptRun, s *stepRow, approversOf string) map[string]any {
	why := "the initiator of the script"
	if approversOf != "" {
		why = "action.approve:" + approversOf
	}
	card := map[string]any{
		"card": scriptGateCard, "subject_kind": "script_gate", "id": s.Gate, "nonce": s.Nonce, "title": fmt.Sprintf("Continue the script at step %d?", s.N),
		"args":              map[string]any{"run": r.ID, "step": s.ID, "n": s.N},
		"requested_by":      map[string]any{"initiator": r.Initiator, "source": "script_step", "ref": r.ID + ":" + s.ID},
		"initiator_display": cut(r.Initiator, 80), "untrusted_seen": s.Untrusted.Value, "state": s.State,
		"footer": map[string]any{"why_you": why, "on_silence": "the step is declined and the steps after it are skipped"},
	}
	if src := s.Untrusted.source(); s.Untrusted.Value && src != "" {
		card["untrusted_source"] = cut(src, 120)
	}
	if s.ExpiresAt != nil {
		card["expires_at"] = fmtTime(*s.ExpiresAt)
		card["footer"].(map[string]any)["until"] = fmtTime(*s.ExpiresAt)
	}
	buttons := []any{}
	if s.State == "waiting" && r.State == "running" || s.State == "waiting" && r.State == "paused" {
		for _, code := range []string{"approve", "reject"} {
			buttons = append(buttons, map[string]any{"code": code, "subject_kind": "script_gate", "id": s.Gate, "nonce": s.Nonce})
		}
	}
	card["buttons"] = buttons
	return card
}

// scriptCardBody draws a run card, its update, its summary or a confirm step card when a
// channel takes the delivery.
func (c *Core) scriptCardBody(ctx context.Context, tx pgx.Tx, cat *Catalog, tenant, subjectKind, kind, id, target, recipient, channel string, body map[string]any) (map[string]any, string, error) {
	var card map[string]any
	if subjectKind == "script_gate" {
		var runID string
		var n int
		err := tx.QueryRow(ctx, `SELECT s.run, s.n FROM script_steps s JOIN script_runs r ON r.id = s.run WHERE s.gate = $1 AND r.tenant = $2`, id, tenant).Scan(&runID, &n)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, "", nil
		}
		if err != nil {
			return nil, "", err
		}
		r, err := loadRun(ctx, tx, tenant, runID, false)
		if err != nil {
			return nil, "", err
		}
		steps, err := loadSteps(ctx, tx, runID)
		if err != nil {
			return nil, "", err
		}
		if r.Rec.Purged {
			return nil, "", nil
		}
		defs, err := parseSteps(r.Rec.Script)
		if err != nil {
			return nil, "", err
		}
		card = buildGateCard(r, steps[n-1], defs[n-1].approversOf())
	} else {
		r, err := loadRun(ctx, tx, tenant, id, false)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, "", nil
		}
		if err != nil {
			return nil, "", err
		}
		if r.Rec.Purged {
			return nil, "", nil
		}
		cardKind, _ := body["card"].(string)
		if card, err = c.buildScriptCard(ctx, tx, cat, r, cardKind); err != nil {
			return nil, "", err
		}
		if kind == scriptSummaryKind {
			card["buttons"] = []any{}
		}
	}
	replaces := ""
	if kind == cardUpdate {
		of, _ := body["of"].(string)
		var ext *string
		err := tx.QueryRow(ctx, `SELECT external_id FROM deliveries WHERE id = $5 AND subject_kind = $6 AND subject_id = $1
			AND exec_name = $2 AND target_id = $3 AND recipient IS NOT DISTINCT FROM $4 AND state = 'delivered' AND external_id IS NOT NULL`,
			id, channel, target, nullable(recipient), of, subjectKind).Scan(&ext)
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

// shrinkRunReserve moves a released step's share of the run reserve: the money and the tree
// tasks it was reserved for (SCR-8).
func shrinkRunReserve(ctx context.Context, tx pgx.Tx, run string, micros int64, tasks int) error {
	if micros > 0 {
		if _, err := tx.Exec(ctx, `UPDATE spend SET eur_micros = greatest(eur_micros - $2, 0) WHERE script = $1 AND unit = 'task' AND state = 'reserved'`, run, micros); err != nil {
			return err
		}
	}
	if tasks > 0 {
		if _, err := tx.Exec(ctx, `UPDATE script_runs SET tree_left = greatest(tree_left - $2, 0) WHERE id = $1`, run, tasks); err != nil {
			return err
		}
	}
	return nil
}

// scriptScopes are the hold scopes of a run: the tenant, the step types and actions and the
// author role.
func scriptScopes(r *scriptRun, steps []scriptStep) []scopeRef {
	s := tenantScopes(r.Tenant)
	for _, st := range steps {
		if st.Type != "" {
			s = append(s, scopeRef{kind: "tasktype", tenant: r.Tenant, name: st.Type})
		}
		if st.Action != "" {
			s = append(s, scopeRef{kind: "action", tenant: r.Tenant, name: st.Action})
		}
	}
	if r.Rec.Role != "" {
		s = append(s, scopeRef{kind: "role", tenant: r.Tenant, name: r.Rec.Role})
	}
	return s
}

// setRunState moves a run and writes its event and card updates.
func (c *Core) setRunState(ctx context.Context, tx pgx.Tx, now time.Time, r *scriptRun, state, reason string) error {
	if _, err := tx.Exec(ctx, `UPDATE script_runs SET state = $2, reason = $3, updated_at = $4,
		ended_at = CASE WHEN $2 IN ('completed', 'failed', 'canceled') AND started_at IS NOT NULL THEN coalesce(ended_at, $4) ELSE ended_at END WHERE id = $1`,
		r.ID, state, nullable(reason), now); err != nil {
		return err
	}
	r.State, r.Reason = state, reason
	typ := "script.state.v1"
	if state == "running" && r.DeadlineAt == nil {
		typ = "script.started.v1"
	}
	if err := c.scriptEvent(ctx, tx, now, r, typ, nil, state, reason); err != nil {
		return err
	}
	return c.scriptCardUpdates(ctx, tx, now, r, true)
}

// rejectRun ends a proposed run: the card was rejected or expired, a new draft superseded it,
// its initiator left, or the reserve did not fit. Nothing ran and no reserve was taken.
func (c *Core) rejectRun(ctx context.Context, tx pgx.Tx, now time.Time, r *scriptRun, reason string) error {
	if r.State != "proposed" {
		return nil
	}
	if _, err := tx.Exec(ctx, `UPDATE script_steps SET state = 'canceled', ended_at = $2 WHERE run = $1 AND state = 'pending'`, r.ID, now); err != nil {
		return err
	}
	return c.setRunState(ctx, tx, now, r, "rejected", reason)
}

// startRun is the "Run" press after its checks: the whole reserve and tree size are taken, the
// deadline starts and the first steps are released (SCR-8).
func (c *Core) startRun(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, tv *TenantView, r *scriptRun) (string, error) {
	treeLimit := defaultMaxTreeTasks
	if m := tv.Spec.Campaigns.MaxTreeTasks; m > 0 {
		treeLimit = m
	}
	treeLimit = min(treeLimit, farmMaxTreeTasks)
	if _, err := tx.Exec(ctx, `SELECT 1 FROM tasks WHERE id = $1 FOR UPDATE`, r.Rec.Root); err != nil {
		return "", err
	}
	used, err := treeSize(ctx, tx, r.Tenant, r.Rec.Root)
	if err != nil {
		return "", err
	}
	if used+r.Rec.Estimate.TreeTasks > treeLimit {
		return "tree_too_large", c.rejectRun(ctx, tx, now, r, "tree_too_large")
	}
	// The run is paid by the tenant's month and line work, not by the person's day (SCR-8).
	sysGroups := groupClosure(tv, []string{"system-ops"})
	var crossed []crossing
	if pb := c.checkBudget(ctx, tx, now, tv, budgetAsk{subject: "system", groups: sysGroups, extra: r.Rec.Estimate.Micros, line: "work", crossed: &crossed}); pb != nil {
		return "budget_exhausted", c.rejectRun(ctx, tx, now, r, "budget_exhausted")
	}
	if r.Rec.Estimate.Micros > 0 {
		if _, err := tx.Exec(ctx, `INSERT INTO spend (id, tenant, root, subject, line, purpose, unit, state, eur_micros, time, script)
			VALUES ($1, $2, $3, 'system', 'work', 'work', 'task', 'reserved', $4, $5, $3)`, ids.New(ids.Spend), r.Tenant, r.ID, r.Rec.Estimate.Micros, now); err != nil {
			return "", err
		}
	}
	if err := c.emitCrossed(ctx, tx, now, r.Tenant, crossed); err != nil {
		return "", err
	}
	d, ok := parseDuration(r.Rec.Deadline)
	if !ok {
		d = farmScriptRunDeadline
	}
	at := now.Add(d)
	// The targets fixed at "Run" are what a script link looks at (TRN-7).
	defs, err := parseSteps(r.Rec.Script)
	if err != nil {
		return "", err
	}
	targets := runTargets(c.campaignResources(cat, r.Tenant, r.Rec.Mark), defs, r.Rec)
	if _, err := tx.Exec(ctx, `UPDATE script_runs SET tree_left = $2, deadline_at = $3, targets = $4, started_at = $5 WHERE id = $1`,
		r.ID, r.Rec.Estimate.TreeTasks, at, targets, now); err != nil {
		return "", err
	}
	r.TreeLeft = r.Rec.Estimate.TreeTasks
	if err := c.setRunState(ctx, tx, now, r, "running", ""); err != nil {
		return "", err
	}
	r.DeadlineAt = &at
	return "", c.releaseSteps(ctx, tx, now, cat, r)
}

// releaseSteps is the release of SCR-8 and SCR-11: deterministic code over the record releases
// every pending step whose needs are terminal, by run_if and when, while the run is running;
// a step that cannot run is skipped. It finishes the run once every step is terminal.
func (c *Core) releaseSteps(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, r *scriptRun) error {
	defs, err := parseSteps(r.Rec.Script)
	if err != nil {
		return err
	}
	for range len(defs) + 1 {
		cur, err := loadRun(ctx, tx, r.Tenant, r.ID, true)
		if err != nil {
			return err
		}
		*r = *cur
		if r.State != "running" && r.State != "paused" {
			return nil
		}
		steps, err := loadSteps(ctx, tx, r.ID)
		if err != nil {
			return err
		}
		if done, err := c.finishIfDone(ctx, tx, now, r, steps); err != nil || done {
			return err
		}
		if r.State != "running" {
			return nil
		}
		byID := map[string]*stepRow{}
		for _, s := range steps {
			byID[s.ID] = s
		}
		acted := false
		for i, s := range steps {
			if s.State != "pending" {
				continue
			}
			def := defs[i]
			ready := true
			for _, need := range def.Needs {
				if !slices.Contains(stepTerminal, byID[need].State) {
					ready = false
				}
			}
			if !ready {
				continue
			}
			if !runIfMet(def, byID) || r.Stopped && def.runIf() == "succeeded" || def.When != nil && !evalWhen(def.When, byID) {
				if err := c.endStep(ctx, tx, now, r, s, "skipped", "", nil, scriptUntrusted{}); err != nil {
					return err
				}
			} else if err := c.releaseStep(ctx, tx, now, cat, r, s, def, byID); err != nil {
				return err
			}
			acted = true
			break
		}
		if !acted {
			return nil
		}
	}
	return nil
}

// runIfMet applies run_if over the needs of a step (SCR-11).
func runIfMet(def scriptStep, byID map[string]*stepRow) bool {
	switch def.runIf() {
	case "always":
		return true
	case "failed":
		return slices.ContainsFunc(def.Needs, func(n string) bool { st := byID[n].State; return st == "failed" || st == "unknown" })
	}
	return !slices.ContainsFunc(def.Needs, func(n string) bool { return byID[n].State != "ok" })
}

// evalWhen evaluates a condition over the closed fields and the states of earlier steps; a
// field a step did not return equals nothing (SCR-3).
func evalWhen(w map[string]any, byID map[string]*stepRow) bool {
	if list, ok := w["all"].([]any); ok {
		for _, x := range list {
			if !evalWhen(x.(map[string]any), byID) {
				return false
			}
		}
		return true
	}
	if list, ok := w["any"].([]any); ok {
		for _, x := range list {
			if evalWhen(x.(map[string]any), byID) {
				return true
			}
		}
		return false
	}
	if x, ok := w["not"].(map[string]any); ok {
		return !evalWhen(x, byID)
	}
	ref, _ := w["ref"].(string)
	step, field, _ := strings.Cut(ref, ".")
	s := byID[step]
	var have any
	if s != nil {
		if field == "state" {
			have = s.State
		} else {
			have = s.Fields[field]
		}
	}
	eq := func(a, b any) bool {
		return have != nil && fmt.Sprint(a) == fmt.Sprint(b) && jsonKind(a) == jsonKind(b)
	}
	switch w["op"] {
	case "eq":
		return eq(have, w["value"])
	case "ne":
		return !eq(have, w["value"])
	case "in":
		list, _ := w["value"].([]any)
		return slices.ContainsFunc(list, func(v any) bool { return eq(have, v) })
	}
	return false
}

func jsonKind(v any) string {
	switch v.(type) {
	case float64, int, int64:
		return "number"
	case bool:
		return "boolean"
	case string:
		return "string"
	}
	return "other"
}

// stepReads are the earlier steps whose data a step receives: by a reference in its values or
// in its condition (SCR-9).
func stepReads(def scriptStep) []string {
	var out []string
	add := func(ref string) {
		step, _, _ := strings.Cut(ref, ".")
		if step != "" && !slices.Contains(out, step) {
			out = append(out, step)
		}
	}
	for _, v := range def.values() {
		if m, ok := v.(map[string]any); ok {
			if ref, ok := m["ref"].(string); ok {
				add(ref)
			}
		}
	}
	if def.When != nil {
		for _, a := range conditionAtoms(def.When) {
			if ref, _ := a["ref"].(string); !strings.HasSuffix(ref, ".state") {
				add(ref)
			}
		}
	}
	return out
}

// releaseStep releases one step in this transaction by its kind, after the initiator's rights
// are checked again (SCR-7, SCR-8, SCR-9).
func (c *Core) releaseStep(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, r *scriptRun, s *stepRow, def scriptStep, byID map[string]*stepRow) error {
	tv := cat.Tenants[r.Tenant]
	if tv == nil {
		return nil
	}
	pi, err := loadPrincipal(ctx, tx, now, r.Tenant, r.Initiator)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && pi.State != "active" {
		return c.cancelRun(ctx, tx, now, cat, r, "person_inactive")
	}
	if err != nil {
		return err
	}
	groups, err := principalGroups(ctx, tx, now, tv, r.Initiator)
	if err != nil {
		return err
	}
	unt := r.Rec.Untrusted
	for _, step := range stepReads(def) {
		if p := byID[step]; p != nil && p.Untrusted.Value {
			unt = unt.or(p.Untrusted)
			if !slices.Contains(unt.Sources, "script_step") && len(unt.Sources) < 16 {
				unt.Sources = append(unt.Sources, "script_step")
			}
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE script_steps SET untrusted = $3 WHERE run = $1 AND n = $2`, r.ID, s.N, mustJSON(unt)); err != nil {
		return err
	}
	s.Untrusted = unt
	if reason := c.stepRights(cat, tv, groups, r.Rec.Mark, def); reason != "" {
		return c.endStep(ctx, tx, now, r, s, "failed", "rights_revoked", nil, unt)
	}
	if def.Kind == "confirm" {
		return c.openGate(ctx, tx, now, tv, r, s, def)
	}
	// The values: literals, and references filled from the closed fields of earlier steps,
	// checked again with the schema at the mark (SCR-3).
	values := map[string]any{}
	for k, v := range def.values() {
		if m, ok := v.(map[string]any); ok {
			ref, _ := m["ref"].(string)
			step, field, _ := strings.Cut(ref, ".")
			p := byID[step]
			if p == nil || p.Fields[field] == nil {
				return c.endStep(ctx, tx, now, r, s, "failed", "ref_invalid", nil, unt)
			}
			values[k] = p.Fields[field]
			continue
		}
		values[k] = v
	}
	key := "system:script:" + r.ID + ":" + def.ID
	switch def.Kind {
	case "task":
		tt := cat.TaskTypes[def.Type]
		schema, err := c.stepSchema(cat, r.Tenant, r.Rec.Mark, def)
		if err != nil {
			return err
		}
		if c.rs.validate(schema, values) != nil {
			return c.endStep(ctx, tx, now, r, s, "failed", "input_schema_invalid", nil, unt)
		}
		if err := shrinkRunReserve(ctx, tx, r.ID, r.Rec.StepMicros[def.ID], 1); err != nil {
			return err
		}
		body := map[string]any{"type": def.Type, "goal": scriptGoal(tt, def, r.Rec.Assignment)}
		if len(values) > 0 {
			body["params"] = values
		}
		in := enqueueInput{
			Tenant: r.Tenant, Type: def.Type, Body: body, IdempotencyKey: key, Initiator: r.Initiator,
			EnqueuedBy: "core", Origin: r.Rec.Origin, Source: "script", SourceID: r.ID, Cause: r.ID, PaidByCard: true,
			TreeBudgetExtra: r.Rec.Estimate.Micros,
		}
		if r.Anchor != "" {
			in.Chain = &chainLink{
				Parent: r.Anchor, Root: r.Rec.Root, Depth: r.Rec.Depth + 1, Mark: r.Rec.Mark, MaxTier: r.Rec.MaxTier,
				Untrusted: unt.Value, Conversation: r.Rec.Conversation, Trace: r.Rec.Trace, ParentPriority: priorityOf(ctx, tx, r.Anchor),
			}
		}
		res, err := c.enqueue(ctx, tx, now, cat, in)
		reason := ""
		switch {
		case err != nil:
			p, ok := AsProblem(err)
			if !ok {
				return err
			}
			reason = p.Reason
		case res.Rejected != nil:
			reason = res.Rejected.Reason
		}
		if reason != "" {
			return c.endStep(ctx, tx, now, r, s, "failed", reason, nil, unt)
		}
		if unt.Value && unt.source() != "" {
			if _, err := tx.Exec(ctx, `UPDATE tasks SET record = record || jsonb_build_object('untrusted_source', $2::text) WHERE id = $1 AND NOT record ? 'untrusted_source'`,
				res.TaskID, unt.source()); err != nil {
				return err
			}
		}
		return c.moveStep(ctx, tx, now, r, s, "released", "", map[string]any{"task": res.TaskID})
	case "fanout":
		tmpl := map[string]any{"kind": "task", "type": def.Type, "params": values}
		kind, typ := "task", def.Type
		if def.Action != "" {
			tmpl = map[string]any{"kind": "action", "action": def.Action, "args": values}
			kind, typ = "action", "action."+def.Action
		} else if goal := scriptGoal(cat.TaskTypes[def.Type], def, r.Rec.Assignment); goal != "" {
			tmpl["goal"] = goal
		}
		p := campaignPlan{
			Tenant: r.Tenant, Initiator: r.Initiator, ParentTask: r.Anchor, Role: r.Rec.Role, Template: tmpl, Kind: kind, Type: typ, Action: def.Action,
			Resources: r.Rec.fanoutList(def.ID), Untrusted: unt.Value, UntrustedSource: unt.source(), Mark: r.Rec.Mark, MaxTier: r.Rec.MaxTier,
			EnqueuedBy: map[string]any{"role": r.Rec.Role, "worker": r.Rec.Worker, "attempt": 1}, Root: r.Rec.Root, Depth: r.Rec.Depth,
			Conversation: r.Rec.Conversation, Trace: r.Rec.Trace, Origin: r.Rec.Origin, Key: key, Source: "bot",
			ScriptRun: r.ID, ScriptStep: def.ID,
		}
		if def.Rollout != nil {
			raw, _ := json.Marshal(def.Rollout)
			if err := json.Unmarshal(raw, &p.Rollout); err != nil {
				return c.endStep(ctx, tx, now, r, s, "failed", "input_schema_invalid", nil, unt)
			}
			if _, ok := def.Rollout["max_failures"]; !ok {
				p.Rollout.MaxFailures = nil
			}
		}
		out, err := c.admitCampaign(ctx, tx, now, cat, &p)
		if err != nil {
			return err
		}
		if out.State == "rejected" {
			return c.endStep(ctx, tx, now, r, s, "failed", out.Reason, nil, unt)
		}
		state := "released"
		if out.State == "proposed" {
			state = "waiting"
		}
		return c.moveStep(ctx, tx, now, r, s, state, "", map[string]any{"campaign": out.Campaign})
	case "action":
		return c.releaseActionStep(ctx, tx, now, cat, tv, r, s, def, values, key, byID)
	}
	return nil
}

// scriptGoal is the goal of a step: a template of the type, or the initiator's own text (SCR-4).
func scriptGoal(tt manifest.TaskType, def scriptStep, assignment string) string {
	if name, ok := def.Goal["template"].(string); ok {
		for _, gt := range tt.GoalTemplates {
			if gt.Name == name {
				return gt.Goal
			}
		}
	}
	if def.Goal["assignment"] == true {
		return assignment
	}
	return ""
}

// releaseActionStep releases an action step: a routine action without untrusted data runs under
// a service_grant with the initiator as author within its counters (D-78); any other gets its
// own approval with the exact snapshot, decided by a person with action.approve:<a> (SCR-7).
func (c *Core) releaseActionStep(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, tv *TenantView, r *scriptRun, s *stepRow, def scriptStep, values map[string]any, key string, byID map[string]*stepRow) error {
	act := cat.Actions[def.Action]
	args := actionDefaults(act, values)
	schema, err := c.actionArgsSchema(cat, r.Tenant, r.Rec.Mark, act)
	if err != nil {
		return err
	}
	if c.rs.validate(schema, args) != nil {
		return c.endStep(ctx, tx, now, r, s, "failed", "input_schema_invalid", nil, s.Untrusted)
	}
	if held, err := heldBy(ctx, tx, now, []string{"no_action", "pause_action"}, approvalScopes(r.Tenant, def.Action, r.Rec.Role, "")...); err != nil {
		return err
	} else if held != "" {
		return c.endStep(ctx, tx, now, r, s, "failed", "held", nil, s.Untrusted)
	}
	argsHash, err := jcs.Hash(args)
	if err != nil {
		return err
	}
	if act.Risk == "routine" && !s.Untrusted.Value {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(7300110, hashtext($1))`, r.Tenant); err != nil {
			return err
		}
		var hour, day int
		if err := tx.QueryRow(ctx, `SELECT count(*) FILTER (WHERE created_at > $3), count(*) FROM tasks
			WHERE tenant = $1 AND type = $2 AND record ? 'grant_id' AND created_at > $4`,
			r.Tenant, "action."+def.Action, now.Add(-time.Hour), now.Add(-24*time.Hour)).Scan(&hour, &day); err != nil {
			return err
		}
		if act.RunsPerHour > 0 && hour >= act.RunsPerHour || act.RunsPerDay > 0 && day >= act.RunsPerDay {
			return c.endStep(ctx, tx, now, r, s, "failed", "service_rate_limited", nil, s.Untrusted)
		}
		grant, err := c.writeGrant(ctx, tx, now, r.Tenant, map[string]any{"kind": "human", "principal": r.Initiator},
			Actor{Principal: r.Initiator, Via: "core"}, map[string]any{"action": def.Action, "script": r.ID + ":" + def.ID}, argsHash)
		if err != nil {
			return err
		}
		task, err := c.insertActionTask(ctx, tx, now, actionTaskInput{
			Tenant: r.Tenant, Action: def.Action, Args: args, Initiator: r.Initiator, SourceTask: r.Anchor, Mark: r.Rec.Mark, GrantID: grant, Key: key,
		})
		if err != nil {
			return err
		}
		if err := shrinkRunReserve(ctx, tx, r.ID, 0, 1); err != nil {
			return err
		}
		if err := c.moveStep(ctx, tx, now, r, s, "released", "", map[string]any{"task": task}); err != nil {
			return err
		}
		callID, err := c.insertWriteCall(ctx, tx, now, cat, writeCall{
			Tenant: r.Tenant, Task: task, Action: def.Action, Act: act, Args: args, ArgsHash: argsHash, GrantID: grant,
		})
		if err != nil {
			return err
		}
		var state, reason string
		if err := tx.QueryRow(ctx, `SELECT state, coalesce(reason, '') FROM toolcalls WHERE id = $1`, callID).Scan(&state, &reason); err != nil {
			return err
		}
		if state != "queued" {
			return c.writeCallEnded(ctx, tx, now, callID, state, reason, nil)
		}
		return nil
	}
	// The card names the source when the condition before the action read flagged data (SCR-9).
	id, code, err := c.openApproval(ctx, tx, now, tv, approvalRow{
		Tenant: r.Tenant, SourceTask: r.Anchor, Initiator: r.Initiator, Action: def.Action, ArgsHash: argsHash, Args: args, Risk: act.Risk,
		RequestedBy:   map[string]any{"kind": "script_step", "task": r.Anchor, "ref": r.ID + ":" + def.ID},
		UntrustedSeen: s.Untrusted.Value, UntrustedSource: s.Untrusted.source(), ExpiresAt: now.Add(actionTTL(act)), Mark: r.Rec.Mark,
	})
	if err != nil {
		return err
	}
	if code != "pending" {
		return c.endStep(ctx, tx, now, r, s, "failed", code, nil, s.Untrusted)
	}
	if err := c.moveStep(ctx, tx, now, r, s, "waiting", "", map[string]any{"approval": id}); err != nil {
		return err
	}
	a, err := loadApproval(ctx, tx, r.Tenant, id, true)
	if err != nil {
		return err
	}
	return c.planCards(ctx, tx, now, tv, a)
}

// openGate opens a confirm step: a decision subject script_gate with its own nonce and a deadline
// of 30 minutes within the run deadline, carded to the initiator or to the approvers of an
// action (SCR-11).
func (c *Core) openGate(ctx context.Context, tx pgx.Tx, now time.Time, tv *TenantView, r *scriptRun, s *stepRow, def scriptStep) error {
	gate, nonce := ids.New(ids.ScriptGate), ids.Secret(16)
	expires := now.Add(scriptCardTTL)
	if r.DeadlineAt != nil && r.DeadlineAt.Before(expires) {
		expires = *r.DeadlineAt
	}
	if _, err := tx.Exec(ctx, `UPDATE script_steps SET gate = $3, nonce = $4, expires_at = $5 WHERE run = $1 AND n = $2`, r.ID, s.N, gate, nonce, expires); err != nil {
		return err
	}
	s.Gate, s.Nonce, s.ExpiresAt = gate, nonce, &expires
	if err := c.moveStep(ctx, tx, now, r, s, "waiting", "", nil); err != nil {
		return err
	}
	people := []string{r.Initiator}
	if a := def.approversOf(); a != "" {
		list, err := peopleWithCap(ctx, tx, now, tv, "action.approve:"+a)
		if err != nil {
			return err
		}
		people = list
	}
	for _, p := range people {
		channel, chat, ok, err := cardChat(ctx, tx, tv, p)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		if _, err := c.planDelivery(ctx, tx, now, plannedDelivery{
			Tenant: r.Tenant, Task: r.Anchor, Kind: scriptGateCard, Addr: "approval_initiator", TargetKind: "chat", TargetID: chat.ID(),
			ExecKind: "channel", ExecName: channel, Recipient: p, Body: map[string]any{"form": "card", "card": scriptGateCard}, DataOrigin: "code",
			SubjectKind: "script_gate", SubjectID: gate,
		}); err != nil {
			return err
		}
	}
	return nil
}

// peopleWithCap are the active people of a tenant whose groups hold a capability.
func peopleWithCap(ctx context.Context, q store.Querier, now time.Time, tv *TenantView, cap string) ([]string, error) {
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
		groups, err := principalGroups(ctx, q, now, tv, p)
		if err != nil {
			return nil, err
		}
		if hasCap(tv, groups, cap) {
			out = append(out, p)
		}
	}
	return out, nil
}

// moveStep moves a step to waiting or released with what it created, and writes the event.
func (c *Core) moveStep(ctx context.Context, tx pgx.Tx, now time.Time, r *scriptRun, s *stepRow, state, reason string, links map[string]any) error {
	task, _ := links["task"].(string)
	cmp, _ := links["campaign"].(string)
	ap, _ := links["approval"].(string)
	tag, err := tx.Exec(ctx, `UPDATE script_steps SET state = $3, reason = $4, task = coalesce($5, task), campaign = coalesce($6, campaign),
		approval = coalesce($7, approval), released_at = CASE WHEN $3 = 'released' THEN $8 ELSE released_at END
		WHERE run = $1 AND n = $2 AND state IN ('pending', 'waiting')`,
		r.ID, s.N, state, nullable(reason), nullable(task), nullable(cmp), nullable(ap), now)
	if err != nil || tag.RowsAffected() == 0 {
		return err
	}
	s.State = state
	if err := c.scriptEvent(ctx, tx, now, r, "script.step.v1", s, state, reason); err != nil {
		return err
	}
	return c.scriptCardUpdates(ctx, tx, now, r, false)
}

// endStep moves a step to a terminal state with the closed fields of its result and its
// untrusted flag. A failure with on_failure: stop stops the release: every pending step that
// needs success is skipped, and the run will end failed (SCR-11). Then the next steps go.
func (c *Core) endStep(ctx context.Context, tx pgx.Tx, now time.Time, r *scriptRun, s *stepRow, state, reason string, fields map[string]any, unt scriptUntrusted) error {
	var fraw []byte
	if len(fields) > 0 {
		fraw = mustJSON(fields)
	}
	unt = s.Untrusted.or(unt)
	tag, err := tx.Exec(ctx, `UPDATE script_steps SET state = $3, reason = $4, fields = $5, untrusted = $6, ended_at = $7
		WHERE run = $1 AND n = $2 AND state IN ('pending', 'waiting', 'released')`, r.ID, s.N, state, nullable(reason), fraw, mustJSON(unt), now)
	if err != nil || tag.RowsAffected() == 0 {
		return err
	}
	s.State, s.Reason, s.Fields, s.Untrusted = state, reason, fields, unt
	if err := c.scriptEvent(ctx, tx, now, r, "script.step.v1", s, state, reason); err != nil {
		return err
	}
	if state == "failed" || state == "unknown" {
		defs, err := parseSteps(r.Rec.Script)
		if err != nil {
			return err
		}
		if s.N <= len(defs) && defs[s.N-1].onFailure() == "stop" && !r.Stopped {
			r.Stopped = true
			if _, err := tx.Exec(ctx, `UPDATE script_runs SET stopped = true WHERE id = $1`, r.ID); err != nil {
				return err
			}
			steps, err := loadSteps(ctx, tx, r.ID)
			if err != nil {
				return err
			}
			for i, st := range steps {
				if st.State == "pending" && defs[i].runIf() == "succeeded" {
					if _, err := tx.Exec(ctx, `UPDATE script_steps SET state = 'skipped', ended_at = $3 WHERE run = $1 AND n = $2 AND state = 'pending'`, r.ID, st.N, now); err != nil {
						return err
					}
					st.State = "skipped"
					if err := c.scriptEvent(ctx, tx, now, r, "script.step.v1", st, "skipped", ""); err != nil {
						return err
					}
				}
			}
		}
	}
	if err := c.scriptCardUpdates(ctx, tx, now, r, false); err != nil {
		return err
	}
	cat, err := c.Catalog(ctx)
	if err != nil {
		return err
	}
	return c.releaseSteps(ctx, tx, now, cat, r)
}

// finishIfDone ends a running or paused run whose steps are all terminal: completed, or failed
// after a stop failure; the rest of the reserve goes and the initiator gets the summary.
func (c *Core) finishIfDone(ctx context.Context, tx pgx.Tx, now time.Time, r *scriptRun, steps []*stepRow) (bool, error) {
	if r.State != "running" && r.State != "paused" {
		return false, nil
	}
	for _, s := range steps {
		if !slices.Contains(stepTerminal, s.State) {
			return false, nil
		}
	}
	state := "completed"
	if r.Stopped {
		state = "failed"
	}
	return true, c.closeRun(ctx, tx, now, r, state, "")
}

// closeRun writes the terminal state of a run that ran: the reserve goes, the card shows the
// end and the initiator gets script_summary (SCR-11).
func (c *Core) closeRun(ctx context.Context, tx pgx.Tx, now time.Time, r *scriptRun, state, reason string) error {
	if _, err := tx.Exec(ctx, `DELETE FROM spend WHERE script = $1 AND unit = 'task' AND state = 'reserved'`, r.ID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE script_runs SET tree_left = 0 WHERE id = $1`, r.ID); err != nil {
		return err
	}
	r.TreeLeft = 0
	if err := c.setRunState(ctx, tx, now, r, state, reason); err != nil {
		return err
	}
	cat, err := c.Catalog(ctx)
	if err != nil {
		return err
	}
	return c.planScriptCard(ctx, tx, now, cat.Tenants[r.Tenant], r, scriptSummaryKind, scriptConfirmCard, r.Initiator)
}

// cancelRun ends a running or paused run before its end (SCR-11): in one transaction the
// unreleased and waiting steps are canceled with their approvals and gates, the tasks of
// released steps get a cancellation, their campaigns stop, and released write calls are not
// recalled. A deadline makes a running run failed and a paused one canceled.
func (c *Core) cancelRun(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, r *scriptRun, reason string) error {
	if r.State == "proposed" {
		return c.rejectRun(ctx, tx, now, r, reason)
	}
	if r.State != "running" && r.State != "paused" {
		return nil
	}
	steps, err := loadSteps(ctx, tx, r.ID)
	if err != nil {
		return err
	}
	state := "canceled"
	if reason == "deadline_exceeded" && r.State == "running" {
		state = "failed"
	}
	if _, err := tx.Exec(ctx, `UPDATE script_runs SET state = $2, reason = $3, updated_at = $4 WHERE id = $1`, r.ID, state, reason, now); err != nil {
		return err
	}
	r.State = state
	if _, err := tx.Exec(ctx, `UPDATE script_steps SET state = 'canceled', reason = $2, ended_at = $3 WHERE run = $1 AND state IN ('pending', 'waiting', 'released')`,
		r.ID, reason, now); err != nil {
		return err
	}
	var tasks []string
	for _, s := range steps {
		switch s.State {
		case "waiting":
			if s.Approval != "" {
				a, err := loadApproval(ctx, tx, r.Tenant, s.Approval, true)
				if err != nil {
					return err
				}
				if a.State == "pending" {
					if err := c.closeApproval(ctx, tx, now, a, "canceled", reason); err != nil {
						return err
					}
				}
			}
			if s.Campaign != "" {
				cmp, err := loadCampaign(ctx, tx, r.Tenant, s.Campaign, true)
				if err != nil {
					return err
				}
				// An action fanout an approver already started runs while its step waits.
				if cmp.State == "proposed" {
					if err := c.rejectCampaign(ctx, tx, now, cmp, reason); err != nil {
						return err
					}
				} else if err := c.stopCampaign(ctx, tx, now, cat, cmp, reason); err != nil {
					return err
				}
			}
		case "released":
			if s.Task != "" && s.Kind == "task" {
				tasks = append(tasks, s.Task)
			}
			if s.Campaign != "" {
				cmp, err := loadCampaign(ctx, tx, r.Tenant, s.Campaign, true)
				if err != nil {
					return err
				}
				if err := c.stopCampaign(ctx, tx, now, cat, cmp, reason); err != nil {
					return err
				}
			}
		}
	}
	if len(tasks) > 0 {
		if err := c.cancelTaskIDs(ctx, tx, now, r.Tenant, tasks, reason); err != nil {
			return err
		}
	}
	r.State = "running"
	return c.closeRun(ctx, tx, now, r, state, reason)
}

// scriptTaskEnded closes the released step of a task that reached a terminal state, in the
// transaction of that state: ok with the closed result fields, unknown for an action whose
// outcome is unknown, failed otherwise (SCR-8, SCR-11).
func (c *Core) scriptTaskEnded(ctx context.Context, tx pgx.Tx, now time.Time, task, state, reason string) error {
	var runID, tenant string
	var n int
	err := tx.QueryRow(ctx, `SELECT s.run, r.tenant, s.n FROM script_steps s JOIN script_runs r ON r.id = s.run WHERE s.task = $1 AND s.state = 'released'`, task).Scan(&runID, &tenant, &n)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	r, err := loadRun(ctx, tx, tenant, runID, true)
	if err != nil {
		return err
	}
	steps, err := loadSteps(ctx, tx, runID)
	if err != nil {
		return err
	}
	s := steps[n-1]
	var typ string
	var rec, result []byte
	if err := tx.QueryRow(ctx, `SELECT type, record, result FROM tasks WHERE id = $1`, task).Scan(&typ, &rec, &result); err != nil {
		return err
	}
	var tr struct {
		Untrusted bool   `json:"untrusted_seen"`
		Source    string `json:"untrusted_source"`
	}
	_ = json.Unmarshal(rec, &tr)
	unt := scriptUntrusted{Value: tr.Untrusted}
	if tr.Untrusted {
		src := tr.Source
		if src == "" {
			src = "tool_output"
		}
		unt.Sources = []string{src}
	}
	stepState, stepReason := "failed", reason
	var fields map[string]any
	switch {
	case state == "completed":
		stepState, stepReason = "ok", ""
		var res map[string]any
		if len(result) > 0 && json.Unmarshal(result, &res) == nil {
			cat, err := c.Catalog(ctx)
			if err != nil {
				return err
			}
			fields = scriptFields(cat.TaskTypes[typ], res)
		}
	case reason == "exec_unknown":
		stepState = "unknown"
	}
	return c.endStep(ctx, tx, now, r, s, stepState, stepReason, fields, unt)
}

// scriptFields copies the result fields that code reads: values of closed kinds only (OUT-1,
// SCR-3).
func scriptFields(tt manifest.TaskType, result map[string]any) map[string]any {
	props, _ := tt.ResultSchema["properties"].(map[string]any)
	out := map[string]any{}
	for k, p := range props {
		m, _ := p.(map[string]any)
		if m["x-botaru-read-by"] != "code" {
			continue
		}
		switch v := result[k].(type) {
		case float64, bool:
			out[k] = v
		case string:
			if _, closed := m["enum"]; closed && len(v) <= 120 {
				out[k] = v
			} else if _, closed := m["enum_from"]; closed && len(v) <= 120 {
				out[k] = v
			}
		}
		if len(out) >= 32 {
			break
		}
	}
	return out
}

// scriptCampaignEnded closes the step of a fanout campaign: completed is ok, a failed or stopped
// campaign fails the step, and a campaign card a person rejected or let expire declines it.
func (c *Core) scriptCampaignEnded(ctx context.Context, tx pgx.Tx, now time.Time, cmp *campaignRow, state, reason string) error {
	if cmp.Rec.ScriptRun == "" {
		return nil
	}
	r, err := loadRun(ctx, tx, cmp.Tenant, cmp.Rec.ScriptRun, true)
	if err != nil {
		return err
	}
	steps, err := loadSteps(ctx, tx, r.ID)
	if err != nil {
		return err
	}
	i := slices.IndexFunc(steps, func(s *stepRow) bool { return s.ID == cmp.Rec.ScriptStep })
	if i < 0 {
		return nil
	}
	s := steps[i]
	if _, err := tx.Exec(ctx, `UPDATE script_steps SET campaign = $3 WHERE run = $1 AND n = $2 AND campaign IS NULL`, r.ID, s.N, cmp.ID); err != nil {
		return err
	}
	var untrusted bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM campaign_items WHERE campaign = $1 AND untrusted)`, cmp.ID).Scan(&untrusted); err != nil {
		return err
	}
	unt := scriptUntrusted{Value: untrusted}
	if untrusted {
		unt.Sources = []string{"campaign_item"}
	}
	switch state {
	case "completed":
		return c.endStep(ctx, tx, now, r, s, "ok", "", nil, unt)
	case "rejected":
		if reason == "" || reason == "expired" {
			return c.endStep(ctx, tx, now, r, s, "declined", reason, nil, unt)
		}
		return c.endStep(ctx, tx, now, r, s, "failed", reason, nil, unt)
	default:
		if reason == "" {
			reason = state
		}
		return c.endStep(ctx, tx, now, r, s, "failed", reason, nil, unt)
	}
}

// scriptApprovalMoved follows the approval of an action step: executing releases the step with
// the action task, a refusal or an expired card declines it, a cancellation fails it (SCR-7,
// SCR-11). The outcome of the execution closes the step through its task.
func (c *Core) scriptApprovalMoved(ctx context.Context, tx pgx.Tx, now time.Time, a *approvalRow) error {
	if kind, _ := a.RequestedBy["kind"].(string); kind != "script_step" {
		return nil
	}
	var runID, tenant string
	var n int
	err := tx.QueryRow(ctx, `SELECT s.run, r.tenant, s.n FROM script_steps s JOIN script_runs r ON r.id = s.run
		WHERE s.approval = $1 AND s.state IN ('waiting', 'released')`, a.ID).Scan(&runID, &tenant, &n)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	r, err := loadRun(ctx, tx, tenant, runID, true)
	if err != nil {
		return err
	}
	steps, err := loadSteps(ctx, tx, runID)
	if err != nil {
		return err
	}
	s := steps[n-1]
	switch a.State {
	case "executing":
		if err := shrinkRunReserve(ctx, tx, r.ID, 0, 1); err != nil {
			return err
		}
		return c.moveStep(ctx, tx, now, r, s, "released", "", map[string]any{"task": a.ActionTask})
	case "rejected":
		return c.endStep(ctx, tx, now, r, s, "declined", "", nil, scriptUntrusted{})
	case "expired":
		return c.endStep(ctx, tx, now, r, s, "declined", "expired", nil, scriptUntrusted{})
	case "canceled", "exec_failed":
		if s.State == "waiting" {
			return c.endStep(ctx, tx, now, r, s, "failed", a.Reason, nil, scriptUntrusted{})
		}
	}
	return nil
}

// purgeRuns turns finished runs into tombstones: the record keeps only the root, a step loses
// the closed fields of its result and its untrusted sources, and states, links and times stay
// as metadata (RET-2, RET-10).
// head is empty or a WITH clause the selection of runs reads.
func purgeRuns(ctx context.Context, tx pgx.Tx, head, where string, args ...any) (int64, error) {
	with := "WITH "
	if head != "" {
		with = head + ", "
	}
	var n int64
	err := tx.QueryRow(ctx, with+`r AS (UPDATE script_runs SET record = jsonb_build_object('root', record->'root', 'purged', true)
		WHERE id IN (`+where+`) AND state <> ALL ('{proposed,running,paused}') AND NOT record ? 'purged' RETURNING id),
		s AS (UPDATE script_steps SET fields = NULL, untrusted = NULL WHERE run IN (SELECT id FROM r))
		SELECT count(*) FROM r`, args...).Scan(&n)
	return n, err
}

// cancelRunsOf cancels the live runs anchored at a task that was canceled (SCR-11).
func (c *Core) cancelRunsOf(ctx context.Context, tx pgx.Tx, now time.Time, tenant, task, reason string) error {
	return c.cancelRuns(ctx, tx, now, tenant, `anchor_task = $2`, task, reason)
}

// cancelRunsOfTree cancels the live runs anchored in any task of the tree of root.
func (c *Core) cancelRunsOfTree(ctx context.Context, tx pgx.Tx, now time.Time, tenant, root, reason string) error {
	return c.cancelRuns(ctx, tx, now, tenant, `anchor_task IN (SELECT id FROM tasks WHERE tenant = $1 AND (id = $2 OR record->>'root' = $2))`, root, reason)
}

func (c *Core) cancelRuns(ctx context.Context, tx pgx.Tx, now time.Time, tenant, where string, arg any, reason string) error {
	rows, err := tx.Query(ctx, `SELECT id FROM script_runs WHERE tenant = $1 AND `+where+` AND state = ANY($3) ORDER BY id FOR UPDATE`, tenant, arg, scriptLive)
	if err != nil {
		return err
	}
	var list []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		list = append(list, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(list) == 0 {
		return err
	}
	cat, err := c.Catalog(ctx)
	if err != nil {
		return err
	}
	if reason == "" {
		reason = "initiator"
	}
	for _, id := range list {
		r, err := loadRun(ctx, tx, tenant, id, true)
		if err != nil {
			return err
		}
		if err := c.cancelRun(ctx, tx, now, cat, r, reason); err != nil {
			return err
		}
	}
	return nil
}

// scriptLoop is the background loop script: the run card deadline, the decision deadline of
// confirm steps, the run deadline, a disabled initiator and the steps without predecessors
// (SCR-8, SCR-11).
func (c *Core) scriptLoop(ctx context.Context) error {
	cat, err := c.Catalog(ctx)
	if err != nil {
		return err
	}
	var list []struct{ id, tenant string }
	if err := c.tx(ctx, func(tx pgx.Tx, _ time.Time) error {
		list = list[:0]
		rows, err := tx.Query(ctx, `SELECT id, tenant FROM script_runs WHERE state = ANY($1) ORDER BY created_at, id`, scriptLive)
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
	}); err != nil {
		return err
	}
	for _, x := range list {
		if err := c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
			r, err := loadRun(ctx, tx, x.tenant, x.id, true)
			if err != nil {
				return err
			}
			pi, err := loadPrincipal(ctx, tx, now, r.Tenant, r.Initiator)
			if errors.Is(err, pgx.ErrNoRows) || err == nil && pi.State != "active" {
				return c.cancelRun(ctx, tx, now, cat, r, "person_inactive")
			}
			if err != nil {
				return err
			}
			switch {
			case r.State == "proposed" && !r.ExpiresAt.After(now):
				return c.rejectRun(ctx, tx, now, r, "expired")
			case r.DeadlineAt != nil && !r.DeadlineAt.After(now):
				return c.cancelRun(ctx, tx, now, cat, r, "deadline_exceeded")
			case r.State == "proposed":
				return nil
			}
			steps, err := loadSteps(ctx, tx, r.ID)
			if err != nil {
				return err
			}
			for _, s := range steps {
				if s.State == "waiting" && s.Gate != "" && s.ExpiresAt != nil && !s.ExpiresAt.After(now) {
					if err := c.endStep(ctx, tx, now, r, s, "declined", "expired", nil, scriptUntrusted{}); err != nil {
						return err
					}
				}
			}
			return c.releaseSteps(ctx, tx, now, cat, r)
		}); err != nil {
			return err
		}
	}
	return nil
}

// decideScript is P1 decide on the subject script: only the initiator presses "Run", once,
// before the deadline, with script.run when the script needs it (SCR-6); approve on a paused
// run resumes it, reject on a proposed one rejects it.
func (c *Core) decideScript(ctx context.Context, w *Worker, ob OnBehalf, req DecideRequest) (*DecideResult, error) {
	var command string
	switch req.Decision {
	case "approve":
		command = "approve"
	case "reject":
		command = "reject"
	default:
		return nil, refuse("input_schema_invalid", "a script run is approved or rejected")
	}
	res, err := c.scriptCommand(ctx, w, ob, req.ID, req.Nonce, command)
	if err != nil {
		return nil, err
	}
	return &DecideResult{SubjectKind: "script", ID: req.ID, Decision: req.Decision, State: res.State}, nil
}

// ScriptControlRequest is p1-script-control-request: a button of the run card.
type ScriptControlRequest struct {
	ID      string `json:"id"`
	Command string `json:"command"`
	Nonce   string `json:"nonce"`
}

// ScriptControlResult is p1-script-control-response.
type ScriptControlResult struct {
	ID    string `json:"id"`
	State string `json:"state"`
}

// ScriptControl is P1 script.control: "Pause", "Resume" and "Cancel" of the run card, pressed
// by the initiator (SCR-11).
func (c *Core) ScriptControl(ctx context.Context, w *Worker, ob *OnBehalf, req ScriptControlRequest) (*ScriptControlResult, error) {
	if ob == nil {
		return nil, refuse("forbidden", "script.control needs Botaru-On-Behalf and Botaru-Chat")
	}
	switch req.Command {
	case "pause", "resume", "cancel":
	default:
		return nil, refuse("input_schema_invalid", "unknown command")
	}
	return c.scriptCommand(ctx, w, *ob, req.ID, req.Nonce, req.Command)
}

// scriptCommand applies a press on a run card. Every button of the run is the initiator's.
func (c *Core) scriptCommand(ctx context.Context, w *Worker, ob OnBehalf, id, nonce, command string) (*ScriptControlResult, error) {
	cat, err := c.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	var out *ScriptControlResult
	err = c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		r, err := loadRun(ctx, tx, w.Tenant, id, true)
		if errors.Is(err, pgx.ErrNoRows) {
			return refuse("not_found", "no such script run")
		}
		if err != nil {
			return err
		}
		if r.Nonce != nonce {
			return refuse("forbidden", "the nonce is not the card's")
		}
		if r.Rec.Purged {
			// A tombstone has no script left to act on (RET-10).
			return refuse("already_terminal", "the run has ended")
		}
		kinds := []string{scriptConfirmCard}
		if command != "approve" && command != "reject" {
			kinds = []string{scriptConfirmCard, scriptSummaryKind}
		}
		press, err := c.checkPress(ctx, tx, now, cat, w, ob, "script", r.ID, kinds)
		if err != nil {
			return err
		}
		audit := func(ok bool, reason string) error {
			_, err := c.appendAudit(ctx, tx, now, Audit{
				Tenant: r.Tenant, Kind: "decision", Actor: Actor{Principal: press.principal, Via: "channel"},
				Command: "script.control", Target: &Target{Kind: "script", ID: r.ID}, OK: ok, Reason: reason,
				After: map[string]any{"command": command, "state": r.State},
			})
			return err
		}
		deny := func(reason, detail string) error {
			if err := audit(false, reason); err != nil {
				return err
			}
			return &committed{p: refuse(reason, "%s", detail)}
		}
		if press.principal != r.Initiator {
			return deny("forbidden", "only the initiator decides a script run")
		}
		out = &ScriptControlResult{ID: r.ID}
		done := func() error {
			if err := tx.QueryRow(ctx, `SELECT state FROM script_runs WHERE id = $1`, r.ID).Scan(&r.State); err != nil {
				return err
			}
			out.State = r.State
			return audit(true, "")
		}
		terminal := !slices.Contains(scriptLive, r.State)
		switch command {
		case "approve":
			if r.State == "paused" {
				return c.resumeRun(ctx, tx, now, cat, r, deny, done)
			}
			if r.State != "proposed" {
				if terminal {
					return refuse("already_terminal", "the run has ended")
				}
				return refuse("already_decided", "the run was started")
			}
			if !r.ExpiresAt.After(now) {
				if err := c.rejectRun(ctx, tx, now, r, "expired"); err != nil {
					return err
				}
				return &committed{p: refuse("expired", "the card has expired")}
			}
			if r.Rec.NeedsRun && !hasCap(press.tv, press.groups, "script.run") {
				return deny("forbidden", "this script needs script.run")
			}
			defs, err := parseSteps(r.Rec.Script)
			if err != nil {
				return err
			}
			// A read-only run still runs under no_action (HLD-9).
			effects := []string{"no_enqueue"}
			if scriptActs(defs, r.Rec.Marks) {
				effects = append(effects, "no_action")
			}
			if held, err := heldBy(ctx, tx, now, effects, scriptScopes(r, defs)...); err != nil {
				return err
			} else if held != "" {
				return deny("held", "a hold covers the script")
			}
			if err := c.decisionEvent(ctx, tx, now, w, ob, press, "script", r.ID, scriptConfirmCard, command); err != nil {
				return err
			}
			reason, err := c.startRun(ctx, tx, now, cat, press.tv, r)
			if err != nil {
				return err
			}
			if reason != "" {
				return &committed{p: refuse(reason, "the run did not start")}
			}
			return done()
		case "reject":
			if r.State != "proposed" {
				if terminal {
					return refuse("already_terminal", "the run has ended")
				}
				return refuse("already_decided", "the run was started")
			}
			if err := c.decisionEvent(ctx, tx, now, w, ob, press, "script", r.ID, scriptConfirmCard, command); err != nil {
				return err
			}
			if err := c.rejectRun(ctx, tx, now, r, ""); err != nil {
				return err
			}
			return done()
		case "pause":
			switch r.State {
			case "running":
				if err := c.setRunState(ctx, tx, now, r, "paused", ""); err != nil {
					return err
				}
			case "paused":
			default:
				return refuse("already_terminal", "the run is not running")
			}
			return done()
		case "resume":
			if r.State == "running" {
				return done()
			}
			if r.State != "paused" {
				return refuse("already_terminal", "the run is not paused")
			}
			return c.resumeRun(ctx, tx, now, cat, r, deny, done)
		case "cancel":
			if terminal {
				return refuse("already_terminal", "the run has ended")
			}
			if r.State == "proposed" {
				if err := c.rejectRun(ctx, tx, now, r, ""); err != nil {
					return err
				}
			} else if err := c.cancelRun(ctx, tx, now, cat, r, "initiator"); err != nil {
				return err
			}
			return done()
		}
		return nil
	})
	return out, err
}

// resumeRun continues a paused run: the deadline has not passed and the initiator is active
// (SCR-11).
func (c *Core) resumeRun(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, r *scriptRun, deny func(string, string) error, done func() error) error {
	if r.DeadlineAt != nil && !r.DeadlineAt.After(now) {
		return deny("deadline_exceeded", "the run deadline has passed")
	}
	if err := c.setRunState(ctx, tx, now, r, "running", ""); err != nil {
		return err
	}
	if err := c.releaseSteps(ctx, tx, now, cat, r); err != nil {
		return err
	}
	return done()
}

// decisionEvent writes decision.made for a press on a card (APR-11).
func (c *Core) decisionEvent(ctx context.Context, tx pgx.Tx, now time.Time, w *Worker, ob OnBehalf, press *cardPress, kind, id, card, decision string) error {
	return c.appendEvent(ctx, tx, now, w.Tenant, Event{Type: "decision.made", Subject: id, Data: map[string]any{
		"subject": map[string]any{"kind": kind, "id": id}, "card": card, "decision": decision,
		"decided_by": press.principal, "channel": w.Target, "chat_kind": ob.Chat.Kind, "via": "decide",
		"delivered_at": fmtTime(press.deliveredAt), "decided_at": fmtTime(now), "response_ms": max(now.Sub(press.deliveredAt).Milliseconds(), 0),
		"details_opened": false,
	}})
}

// decideGate is P1 decide on the subject script_gate: "Continue" makes the confirm step ok,
// "Stop" declines it; the initiator decides, or a person with action.approve:<a> when the step
// says so (SCR-11).
func (c *Core) decideGate(ctx context.Context, w *Worker, ob OnBehalf, req DecideRequest) (*DecideResult, error) {
	if req.Decision != "approve" && req.Decision != "reject" {
		return nil, refuse("input_schema_invalid", "a confirm step is approved or rejected")
	}
	cat, err := c.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	var out *DecideResult
	err = c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		var runID string
		var n int
		err := tx.QueryRow(ctx, `SELECT s.run, s.n FROM script_steps s JOIN script_runs r ON r.id = s.run WHERE s.gate = $1 AND r.tenant = $2`,
			req.ID, w.Tenant).Scan(&runID, &n)
		if errors.Is(err, pgx.ErrNoRows) {
			return refuse("not_found", "no such confirm step")
		}
		if err != nil {
			return err
		}
		r, err := loadRun(ctx, tx, w.Tenant, runID, true)
		if err != nil {
			return err
		}
		steps, err := loadSteps(ctx, tx, runID)
		if err != nil {
			return err
		}
		if r.Rec.Purged || n > len(steps) {
			// A tombstone has no script left to read the approvers from (RET-10).
			return refuse("already_terminal", "the run has ended")
		}
		s := steps[n-1]
		if s.Nonce != req.Nonce {
			return refuse("forbidden", "the nonce is not the card's")
		}
		press, err := c.checkPress(ctx, tx, now, cat, w, ob, "script_gate", s.Gate, []string{scriptGateCard})
		if err != nil {
			return err
		}
		defs, err := parseSteps(r.Rec.Script)
		if err != nil {
			return err
		}
		if n > len(defs) {
			return refuse("already_terminal", "the run has ended")
		}
		may := press.principal == r.Initiator
		if a := defs[n-1].approversOf(); a != "" {
			may = hasCap(press.tv, press.groups, "action.approve:"+a)
		}
		if !may {
			if _, err := c.appendAudit(ctx, tx, now, Audit{
				Tenant: r.Tenant, Kind: "decision", Actor: Actor{Principal: press.principal, Via: "channel"}, Command: "decide",
				Target: &Target{Kind: "script_gate", ID: s.Gate}, OK: false, Reason: "forbidden",
			}); err != nil {
				return err
			}
			return &committed{p: refuse("forbidden", "the person does not decide this step")}
		}
		if !slices.Contains(scriptLive, r.State) {
			return refuse("already_terminal", "the run has ended")
		}
		if s.State != "waiting" {
			return refuse("already_decided", "the step is decided")
		}
		if err := c.decisionEvent(ctx, tx, now, w, ob, press, "script_gate", s.Gate, scriptGateCard, req.Decision); err != nil {
			return err
		}
		state := "ok"
		if req.Decision == "reject" {
			state = "declined"
		}
		if err := c.endStep(ctx, tx, now, r, s, state, "", nil, scriptUntrusted{}); err != nil {
			return err
		}
		if err := c.gateCardUpdates(ctx, tx, now, r, s); err != nil {
			return err
		}
		out = &DecideResult{SubjectKind: "script_gate", ID: s.Gate, Decision: req.Decision, State: state}
		return nil
	})
	return out, err
}

// gateCardUpdates edits the sent cards of a confirm step after its decision.
func (c *Core) gateCardUpdates(ctx context.Context, tx pgx.Tx, now time.Time, r *scriptRun, s *stepRow) error {
	rows, err := tx.Query(ctx, `SELECT id, exec_name, target_id, coalesce(recipient, '') FROM deliveries
		WHERE subject_kind = 'script_gate' AND subject_id = $1 AND kind = $2 AND state IN ('planned', 'sending', 'delivered') ORDER BY seq`, s.Gate, scriptGateCard)
	if err != nil {
		return err
	}
	type target struct{ id, channel, chat, recipient string }
	var ts []target
	for rows.Next() {
		var t target
		if err := rows.Scan(&t.id, &t.channel, &t.chat, &t.recipient); err != nil {
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
			Tenant: r.Tenant, Task: r.Anchor, Kind: cardUpdate, Addr: "approval_initiator", TargetKind: "chat", TargetID: t.chat,
			ExecKind: "channel", ExecName: t.channel, Recipient: t.recipient, Body: map[string]any{"form": "card", "card": scriptGateCard, "of": t.id},
			DataOrigin: "code", SubjectKind: "script_gate", SubjectID: s.Gate,
		}); err != nil {
			return err
		}
	}
	return nil
}

// ScriptGetRequest is p1-script-get-request.
type ScriptGetRequest struct {
	ID string `json:"id"`
}

// ScriptGet is P1 script.get: the run record without free text for an attempt of its own tree;
// another tenant's or a nonexistent run gives the same not_found (PRM-9).
func (c *Core) ScriptGet(ctx context.Context, w *Worker, lease string, req ScriptGetRequest) (map[string]any, error) {
	var out map[string]any
	err := c.tx(ctx, func(tx pgx.Tx, _ time.Time) error {
		l, _, err := c.heldLease(ctx, tx, w, lease, "script.get", nil)
		if err != nil {
			return err
		}
		if l.Kind != "attempt" {
			return refuse("fenced", "a script run is read under an attempt lease")
		}
		r, err := loadRun(ctx, tx, w.Tenant, req.ID, false)
		if errors.Is(err, pgx.ErrNoRows) {
			return refuse("not_found", "no such script run")
		}
		if err != nil {
			return err
		}
		tree, err := loadTree(ctx, tx, l.WorkID)
		if err != nil {
			return err
		}
		if r.Rec.Root != tree.Root || r.Rec.Purged {
			return refuse("not_found", "no such script run")
		}
		out, err = r.Record(ctx, tx)
		return err
	})
	return out, err
}
