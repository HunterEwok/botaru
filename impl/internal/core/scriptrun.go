// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/ids"
	"github.com/HunterEwok/botaru/impl/internal/jcs"
	"github.com/HunterEwok/botaru/impl/internal/manifest"
	"github.com/HunterEwok/botaru/impl/internal/store"
)

// Script runs: holds, the start and the release of steps by the transition table (SCR-8, SCR-9, SCR-11).

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
	return append(s, runWorkerScope(r)...)
}

// runWorkerScope is the scope of the worker that drafted a run, so its own hold counts too.
func runWorkerScope(r *scriptRun) []scopeRef {
	if r.Rec.Worker == "" {
		return nil
	}
	return []scopeRef{{kind: "worker", tenant: r.Tenant, name: strings.ToLower(r.Rec.Worker)}}
}

// runHoldEffects are the hold effects that forbid the release or the execution of a step, and so
// pause a running run (SCR-10, ORC-11).
var runHoldEffects = []string{"no_enqueue", "no_claim", "no_action", "cancel_running"}

// runHoldScopes are the scopes that cover a run: those of scriptScopes, and the machines and
// resources its steps name (the host and resource values of their arguments and the fanout
// lists). A value is taken for both kinds, so a hold on either covers it (SCR-10).
func runHoldScopes(r *scriptRun, defs []scriptStep) []scopeRef {
	s := scriptScopes(r, defs)
	add := func(v string) {
		v = strings.ToLower(v)
		s = append(s, scopeRef{kind: "host", tenant: r.Tenant, name: v}, scopeRef{kind: "resource", tenant: r.Tenant, name: v})
	}
	for _, st := range defs {
		for _, m := range []map[string]any{st.Args, st.Params} {
			for _, k := range []string{"host", "resource"} {
				if v, ok := m[k].(string); ok && v != "" {
					add(v)
				}
			}
		}
	}
	for _, f := range r.Rec.FanoutLists {
		for _, res := range f.Resources {
			add(res)
		}
	}
	return s
}

// runHeld is the hold that covers a run with an effect of runHoldEffects, or "" (SCR-10); a
// read fanout's campaign still pauses by itself (ORC-11).
func (c *Core) runHeld(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, r *scriptRun) (string, error) {
	defs, err := parseSteps(r.Rec.Script)
	if err != nil {
		return "", err
	}
	scopes := runHoldScopes(r, defs)
	// The campaigns of its fanout steps: a hold on a campaign's item type, action or role covers
	// the run as well (SCR-10).
	rows, err := tx.Query(ctx, `SELECT `+campaignColumns+` FROM campaigns WHERE id IN (SELECT campaign FROM script_steps WHERE run = $1 AND campaign IS NOT NULL)
		AND state IN ('proposed', 'running', 'paused', 'stopping') ORDER BY id`, r.ID)
	if err != nil {
		return "", err
	}
	for rows.Next() {
		cmp, err := scanCampaign(rows)
		if err != nil {
			rows.Close()
			return "", err
		}
		scopes = append(scopes, campaignScopes(cmp)...)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", err
	}
	return runHeldBy(ctx, tx, now, r, c.scriptActs(cat, defs), runHoldEffects, scopes)
}

// runHeldBy is heldBy for a run, with the exception of HLD-9: a no_action hold on the scope of
// the role that wrote the draft does not cover a run that does not act by the catalog at the
// check, so read-only scripts of a held role still run. no_action on every other scope (the farm,
// the tenant, the worker, a type, an action, a machine or resource) covers it as SCR-10 says.
func runHeldBy(ctx context.Context, tx pgx.Tx, now time.Time, r *scriptRun, acts bool, effects []string, scopes []scopeRef) (string, error) {
	if acts || r.Rec.Role == "" || !slices.Contains(effects, "no_action") {
		return heldBy(ctx, tx, now, effects, scopes...)
	}
	other := slices.DeleteFunc(slices.Clone(effects), func(e string) bool { return e == "no_action" })
	if len(other) > 0 {
		if held, err := heldBy(ctx, tx, now, other, scopes...); err != nil || held != "" {
			return held, err
		}
	}
	role := scopeRef{kind: "role", tenant: r.Tenant, name: r.Rec.Role}
	rest := slices.DeleteFunc(slices.Clone(scopes), func(s scopeRef) bool { return s == role })
	return heldBy(ctx, tx, now, []string{"no_action"}, rest...)
}

// pauseHeldRuns moves the running runs of a tenant (every tenant for "") that a hold covers to
// paused with the reason held, in the transaction of the hold; releasing the hold does not resume
// them (SCR-10).
func (c *Core) pauseHeldRuns(ctx context.Context, tx pgx.Tx, now time.Time, tenant string) error {
	rows, err := tx.Query(ctx, `SELECT id, tenant FROM script_runs WHERE state = 'running' AND ($1 = '' OR tenant = $1) ORDER BY id`, tenant)
	if err != nil {
		return err
	}
	type key struct{ id, tenant string }
	list, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (key, error) {
		var k key
		return k, row.Scan(&k.id, &k.tenant)
	})
	if err != nil || len(list) == 0 {
		return err
	}
	cat, err := c.Catalog(ctx)
	if err != nil {
		return err
	}
	for _, k := range list {
		r, err := loadRun(ctx, tx, k.tenant, k.id, true)
		if err != nil {
			return err
		}
		if r.State != "running" {
			continue
		}
		held, err := c.runHeld(ctx, tx, now, cat, r)
		if err != nil {
			return err
		}
		if held != "" {
			if err := c.setRunState(ctx, tx, now, r, "paused", "held"); err != nil {
				return err
			}
		}
	}
	return nil
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
		// While a hold covers the run, nothing of it is released: it pauses (SCR-10).
		if held, err := c.runHeld(ctx, tx, now, cat, r); err != nil {
			return err
		} else if held != "" {
			return c.setRunState(ctx, tx, now, r, "paused", "held")
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
	if held, err := heldBy(ctx, tx, now, []string{"no_action", "pause_action"}, append(approvalScopes(r.Tenant, def.Action, r.Rec.Role, ""), runWorkerScope(r)...)...); err != nil {
		return err
	} else if held != "" {
		return c.endStep(ctx, tx, now, r, s, "failed", "held", nil, s.Untrusted)
	}
	argsHash, err := jcs.Hash(args)
	if err != nil {
		return err
	}
	// A routine action runs without a card, but never one that needs two approvers (APR-16).
	if act.Risk == "routine" && !s.Untrusted.Value && act.Approvals < 2 {
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
