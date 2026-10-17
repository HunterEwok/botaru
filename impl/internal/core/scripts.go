// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/manifest"
	"github.com/HunterEwok/botaru/impl/internal/store"
)

// Scripts (spec/lifecycles/script.md, docs/coordination.md). A script is data under the closed
// schema botaru.script/v1, never code (SCR-1). A translator attempt hands in a draft through
// P3 propose_script; the core accepts it synchronously and as a whole or refuses it with the
// step and the reason (SCR-2, SCR-3, SCR-5), and writes the fields only it computes. A run
// starts only on its initiator's "Run" button (SCR-6), which approves no action (SCR-7); then
// deterministic core code releases the steps, rechecks the initiator's rights at every
// release and passes the untrusted flag along the data a step reads (SCR-8, SCR-9), by the
// transition table only (SCR-11).
// This file holds the constants and the records of scripts, runs and steps with their stored
// rows; the proposal, cards, runs, their end and the decisions are in the files script*.go.

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
