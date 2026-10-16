// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"math"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/manifest"
)

// changeCheck is what the checks of a proposal's change found: whether it touches a guarded
// field (PRP-12) and the targets that go into the fingerprint (PRP-5).
type changeCheck struct {
	Guarded bool
	Targets []any
	// Times are the local times of a changed schedule that disappear or repeat at the time
	// transitions of the next year, for the card (TIM-5).
	Times map[string]any
}

// checkChange applies the allowlist of the change kinds (PRP-3, PRP-8, PRP-10, PRP-11,
// PRP-12, PRP-13). A refusal is a rejection with the reason of the closed list.
func (c *Core) checkChange(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, a proposalAuthor, ch proposalChange) (changeCheck, error) {
	switch ch.Kind {
	case "none":
		return changeCheck{Targets: []any{}}, nil
	case "action_request":
		return c.checkActionRequest(ctx, tx, cat, a, ch)
	case "human_change":
		return c.checkHumanChange(cat, a, ch)
	case "manifest_change":
		return c.checkManifestChange(now, cat, a, ch)
	case "script":
		return c.checkScriptChange(cat, a, ch)
	}
	return changeCheck{}, rejection("forbidden_change")
}

// checkActionRequest: an action of the author role's vocabulary with arguments by its closed
// schema, or a service or monitoring script call within the role's service_control or
// monitor_scripts, checked as a real call would be (PRP-3, MON-8, MON-10).
func (c *Core) checkActionRequest(ctx context.Context, tx pgx.Tx, cat *Catalog, a proposalAuthor, ch proposalChange) (changeCheck, error) {
	ar := ch.ActionRequest
	if ar == nil {
		return changeCheck{}, rejection("schema_invalid")
	}
	g := &toolsGrant{Tenant: a.Tenant, Role: a.Role, Type: a.Type, MaxTier: 1, Mark: a.Mark, Purpose: "work"}
	switch {
	case ar.Action != "":
		act, ok := cat.Actions[ar.Action]
		if !ok || !slices.Contains(cat.Roles[a.Role].ActionsMayRequest, ar.Action) {
			return changeCheck{}, rejection("forbidden_change")
		}
		args := actionDefaults(act, ar.Args)
		schema, err := c.actionArgsSchema(cat, a.Tenant, a.Mark, act)
		if err != nil {
			return changeCheck{}, err
		}
		if c.rs.validate(schema, args) != nil {
			return changeCheck{}, rejection("schema_invalid")
		}
		return changeCheck{Targets: []any{map[string]any{"action": ar.Action, "args": args}}}, nil
	case ar.Tool == "service/control":
		t := c.serviceControlTool(cat, g)
		if t == nil || c.rs.validate(t.Schema, ar.Args) != nil {
			return changeCheck{}, rejection("forbidden_change")
		}
		s, ok := findControllable(cat, a.Tenant, asString(ar.Args["service"]))
		if !ok || !slices.Contains(s.Control, asString(ar.Args["verb"])) {
			return changeCheck{}, rejection("forbidden_change")
		}
		return changeCheck{Targets: []any{map[string]any{"tool": ar.Tool, "args": ar.Args}}}, nil
	case ar.Tool == "monitor/script_run":
		hs, err := c.hostScripts(ctx, tx, a.Tenant)
		if err != nil {
			return changeCheck{}, err
		}
		g.HostScripts = hs
		t := c.scriptRunTool(cat, g)
		if t == nil || c.rs.validate(t.Schema, ar.Args) != nil {
			return changeCheck{}, rejection("forbidden_change")
		}
		host, name, input := asString(ar.Args["resource"]), asString(ar.Args["script"]), asString(ar.Args["input"])
		ok := false
		for _, s := range runnableFor(cat, g)[host] {
			if s.Name == name && (input == "" && len(s.Choices) == 0 || slices.Contains(s.Choices, input)) {
				ok = true
			}
		}
		if !ok {
			return changeCheck{}, rejection("forbidden_change")
		}
		return changeCheck{Targets: []any{map[string]any{"tool": ar.Tool, "args": ar.Args}}}, nil
	}
	return changeCheck{}, rejection("schema_invalid")
}

// checkHumanChange: a closed description by the registry change-paths.yaml of an object of the
// tenant's catalog (PRP-13).
func (c *Core) checkHumanChange(cat *Catalog, a proposalAuthor, ch proposalChange) (changeCheck, error) {
	hc := ch.HumanChange
	if hc == nil {
		return changeCheck{}, rejection("schema_invalid")
	}
	reg, err := c.spec.Registry("spec/model/change-paths.yaml")
	if err != nil {
		return changeCheck{}, err
	}
	e, ok := reg.Entry(hc.Path)
	if !ok || asString(e["object"]) != hc.Target.Kind || !slices.Contains(strList(e["directions"]), hc.Direction) {
		return changeCheck{}, rejection("forbidden_change")
	}
	switch asString(e["value"]) {
	case "number":
		if _, ok := toFloat(hc.Value); !ok {
			return changeCheck{}, rejection("forbidden_change")
		}
	case "enum":
		if !slices.Contains(strList(e["values"]), asString(hc.Value)) {
			return changeCheck{}, rejection("forbidden_change")
		}
	default:
		return changeCheck{}, rejection("forbidden_change")
	}
	switch hc.Target.Kind {
	case "Host":
		if !slices.Contains(catalogHosts(cat, a.Tenant), hc.Target.Name) {
			return changeCheck{}, rejection("forbidden_change")
		}
	default:
		return changeCheck{}, rejection("forbidden_change")
	}
	return changeCheck{Targets: []any{map[string]any{"kind": hc.Target.Kind, "name": hc.Target.Name, "path": hc.Path}}}, nil
}

// typeProposable: a type is proposable only if it does work, is not a backup and does not say
// proposable: false (PRP-10).
func typeProposable(tt manifest.TaskType) bool {
	return purposeOf(tt) == "work" && !tt.MakesBackup && (tt.Proposable == nil || *tt.Proposable)
}

var tenantDownField = regexp.MustCompile(`^spec\.(limits|campaigns|scripts)\.([a-z_]+)$`)

// checkManifestChange: set operations on the allowlist proposable-fields.yaml for objects of
// the author's tenant only; after untrusted text only fields with a safe direction, in it
// (PRP-3, PRP-8, PRP-10, PRP-11, PRP-12).
func (c *Core) checkManifestChange(now time.Time, cat *Catalog, a proposalAuthor, ch proposalChange) (changeCheck, error) {
	mc := ch.ManifestChange
	if mc == nil || len(mc.Ops) == 0 || len(mc.Ops) > 10 {
		return changeCheck{}, rejection("schema_invalid")
	}
	tv := cat.Tenants[a.Tenant]
	if tv == nil {
		return changeCheck{}, rejection("forbidden_change")
	}
	out := changeCheck{Targets: []any{}}
	seen := map[string]bool{}
	tenantSpec := map[string]any{}
	if raw, ok := tv.Raw["spec"].(map[string]any); ok {
		b, _ := json.Marshal(raw)
		_ = json.Unmarshal(b, &tenantSpec)
	}
	tenantTouched := false
	// The cron and zone ops of each schedule, checked together after the loop (TIM-5).
	schedOps := map[string][]manifestOp{}
	for _, op := range mc.Ops {
		// A template from git is not served by this build.
		if op.Op != "set" {
			return changeCheck{}, rejection("forbidden_change")
		}
		key := op.Target.Kind + "/" + op.Target.Name + "/" + op.Field
		if seen[key] {
			return changeCheck{}, rejection("forbidden_change")
		}
		seen[key] = true
		guarded := false
		switch op.Target.Kind {
		case "Schedule":
			g, err := c.checkScheduleOp(cat, tv, a, op)
			if err != nil {
				return changeCheck{}, err
			}
			guarded = g
			schedOps[op.Target.Name] = append(schedOps[op.Target.Name], op)
		case "Tenant":
			if op.Target.Name != a.Tenant {
				return changeCheck{}, rejection("forbidden_change")
			}
			g, err := checkTenantOp(tenantSpec, a, op)
			if err != nil {
				return changeCheck{}, err
			}
			guarded, tenantTouched = g, true
		default:
			// Catalog fields of the allowlist have no path in the manifest schema yet.
			return changeCheck{}, rejection("forbidden_change")
		}
		out.Guarded = out.Guarded || guarded
		out.Targets = append(out.Targets, map[string]any{"kind": op.Target.Kind, "name": op.Target.Name, "field": op.Field})
	}
	for _, name := range slices.Sorted(maps.Keys(schedOps)) {
		times, err := c.checkScheduleTimes(now, cat, tv, name, schedOps[name])
		if err != nil {
			return changeCheck{}, err
		}
		if times != nil {
			if out.Times == nil {
				out.Times = map[string]any{}
			}
			for k, v := range times {
				if prev, ok := out.Times[k].([]string); ok {
					v = append(prev, v.([]string)...)
				}
				out.Times[k] = v
			}
		}
	}
	// The patched tenant spec still passes its schema: the bounds of every changed field.
	if tenantTouched && c.spec.Validate("urn:botaru:schema:tenant:1", tenantSpec) != nil {
		return changeCheck{}, rejection("forbidden_change")
	}
	return out, nil
}

// checkScheduleOp: spec.cron as a window shift without lowering the frequency by more than two
// times, spec.timezone as a zone name; a backup schedule only shifts its window at the same
// frequency, as a guarded field, and it is the only change of a target with proposable:
// false (PRP-3, PRP-10, PRP-11, PRP-12).
func (c *Core) checkScheduleOp(cat *Catalog, tv *TenantView, a proposalAuthor, op manifestOp) (bool, error) {
	sc, ok := tv.Schedules[op.Target.Name]
	if !ok || sc.Task == nil {
		return false, rejection("forbidden_change")
	}
	tt := cat.TaskTypes[sc.Task.Type]
	proposable := typeProposable(tt) && (sc.Proposable == nil || *sc.Proposable)
	value, isString := op.Value.(string)
	switch op.Field {
	case "spec.cron":
		if !isString {
			return false, rejection("forbidden_change")
		}
		if _, err := time.LoadLocation(scheduleTZ(tv, sc)); err != nil {
			return false, rejection("forbidden_change")
		}
		before, after, ok := cronShift(sc.Cron, value)
		if !ok || after == 0 {
			return false, rejection("forbidden_change")
		}
		if tt.MakesBackup {
			// A backup window moves at the same frequency, with the backup age guard; never
			// after untrusted text (PRP-11, PRP-12).
			if a.Untrusted {
				return false, rejection("untrusted_direction")
			}
			if after != before {
				return false, rejection("forbidden_change")
			}
			return true, nil
		}
		if !proposable || after*2 < before || after > before {
			return false, rejection("forbidden_change")
		}
		if a.Untrusted {
			return false, rejection("untrusted_direction")
		}
		return false, nil
	case "spec.timezone":
		if !proposable || !isString {
			return false, rejection("forbidden_change")
		}
		if _, err := time.LoadLocation(value); err != nil || value == "" || value == "Local" {
			return false, rejection("forbidden_change")
		}
		if a.Untrusted {
			return false, rejection("untrusted_direction")
		}
		return false, nil
	}
	return false, rejection("forbidden_change")
}

// checkScheduleTimes checks the schedule a cron or zone change gives with code (TIM-5): the
// local times of the next year that disappear or repeat at the transitions of its zone go to
// the card, and a schedule of a type with makes_backup: true that loses a slot on any day is
// rejected(schedule_time_invalid), so it gets no "Accept" button. The model takes no part.
func (c *Core) checkScheduleTimes(now time.Time, cat *Catalog, tv *TenantView, name string, ops []manifestOp) (map[string]any, error) {
	sc := tv.Schedules[name]
	cron, zone := sc.Cron, scheduleTZ(tv, sc)
	changed := false
	for _, op := range ops {
		value, _ := op.Value.(string)
		switch op.Field {
		case "spec.cron":
			cron, changed = value, true
		case "spec.timezone":
			// The zone names apply takes (TIM-3): never empty and never the host's Local.
			if value == "" || value == "Local" {
				return nil, rejection("forbidden_change")
			}
			zone, changed = value, true
		}
	}
	if !changed {
		return nil, nil
	}
	return scheduleCheck(cron, zone, sc.Task != nil && cat.TaskTypes[sc.Task.Type].MakesBackup, now)
}

// scheduleCheck is the pure part of the check (TIM-5), which function.eval schedule.check
// calls on a test bench too: an expression or zone that apply refuses (TIM-3) is
// forbidden_change, a lost slot of a makes_backup schedule is schedule_time_invalid, else the
// card fields of the times that disappear or repeat, or nil without any.
func scheduleCheck(cron, zone string, makesBackup bool, now time.Time) (map[string]any, error) {
	if zone == "" || zone == "Local" {
		return nil, rejection("forbidden_change")
	}
	cs, err := parseCron(cron)
	if err != nil {
		return nil, rejection("forbidden_change")
	}
	loc, err := time.LoadLocation(zone)
	if err != nil {
		return nil, rejection("forbidden_change")
	}
	disappear, repeat := scheduleTimes(cs, loc, now)
	if len(disappear) > 0 && makesBackup {
		return nil, rejection("schedule_time_invalid")
	}
	if len(disappear) == 0 && len(repeat) == 0 {
		return nil, nil
	}
	out := map[string]any{}
	if len(disappear) > 0 {
		out["times_disappear"] = disappear
	}
	if len(repeat) > 0 {
		out["times_repeat"] = repeat
	}
	return out, nil
}

// cronShift compares two expressions as a shift of the window (PRP-3, PRP-11): the day,
// month and weekday fields must stay as they are, so only the minutes and hours of a day move,
// and the frequency is the number of slots in a day. A change of the days would let a
// frequency count over a fixed span be gamed (all slots of a monthly expression put inside
// it), so it is refused.
func cronShift(cur, next string) (before, after int, ok bool) {
	a, err1 := parseCron(cur)
	b, err2 := parseCron(next)
	if err1 != nil || err2 != nil || a.dom != b.dom || a.month != b.month || a.dow != b.dow ||
		a.domAny != b.domAny || a.dowAny != b.dowAny {
		return 0, 0, false
	}
	daily := func(c *cronSpec) int {
		m, h := 0, 0
		for i := range 60 {
			if c.min[i] {
				m++
			}
		}
		for i := range 24 {
			if c.hour[i] {
				h++
			}
		}
		return m * h
	}
	return daily(a), daily(b), true
}

// checkTenantOp: a tenant limit, campaign or script ceiling only lower (its safe direction),
// or a guarded field of the tenant's single dispatcher rule within its bounds (PRP-3,
// PRP-11, PRP-12). It writes the new value into spec for the schema check.
func checkTenantOp(spec map[string]any, a proposalAuthor, op manifestOp) (bool, error) {
	if m := tenantDownField.FindStringSubmatch(op.Field); m != nil {
		sec, _ := spec[m[1]].(map[string]any)
		cur, ok := sec[m[2]]
		if !ok || !lower(op.Value, cur) {
			return false, rejection("forbidden_change")
		}
		sec[m[2]] = op.Value
		return false, nil
	}
	rest, ok := strings.CutPrefix(op.Field, "spec.dispatch.rules[].")
	if !ok {
		return false, rejection("forbidden_change")
	}
	// A guarded field never changes after untrusted text (PRP-12).
	if a.Untrusted {
		return false, rejection("untrusted_direction")
	}
	disp, _ := spec["dispatch"].(map[string]any)
	rules, _ := disp["rules"].([]any)
	// The operation names no rule: it applies only where the tenant has exactly one.
	if len(rules) != 1 {
		return false, rejection("forbidden_change")
	}
	rule, _ := rules[0].(map[string]any)
	switch rest {
	case "timeouts.attempt":
		t, _ := rule["timeouts"].(map[string]any)
		cur, ok1 := parseDuration(asString(t["attempt"]))
		nv, ok2 := parseDuration(asString(op.Value))
		if !ok1 || !ok2 || nv <= 0 || nv > 2*cur {
			return false, rejection("forbidden_change")
		}
		t["attempt"] = op.Value
	case "retry.max_attempts":
		r, _ := rule["retry"].(map[string]any)
		cur, ok1 := toFloat(r["max_attempts"])
		nv, ok2 := toFloat(op.Value)
		if !ok1 || !ok2 || nv < 1 || nv >= cur || nv != math.Trunc(nv) {
			return false, rejection("forbidden_change")
		}
		r["max_attempts"] = op.Value
	case "max_running":
		cur, ok1 := toFloat(rule["max_running"])
		nv, ok2 := toFloat(op.Value)
		if !ok1 || !ok2 || nv < 1 || math.Abs(nv-cur) != 1 {
			return false, rejection("forbidden_change")
		}
		rule["max_running"] = op.Value
	default:
		return false, rejection("forbidden_change")
	}
	return true, nil
}

// lower says whether a new value is below the current one: numbers, or durations.
func lower(nv, cur any) bool {
	if a, ok := toFloat(nv); ok {
		b, ok := toFloat(cur)
		return ok && a < b
	}
	a, ok1 := parseDuration(asString(nv))
	b, ok2 := parseDuration(asString(cur))
	return ok1 && ok2 && a < b
}

// cronLookback and cronSpan are the cumulative limit on lowering a schedule's frequency: the
// runs of a new spec.cron per 30 days are at least half of the runs of the value applied 90
// days earlier, by the history of applied sets (PRP-16).
const (
	cronLookback = 90 * 24 * time.Hour
	cronSpanDays = 30
)

// cronHistory refuses a spec.cron below half of the runs of the value that was applied 90 days
// ago, so a run of small steps cannot lower the frequency more than one step may (PRP-16). The
// value applied then is the earliest replaced version after that moment, or the one applied
// now when none was replaced since.
func (c *Core) cronHistory(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, tenant string, ch proposalChange) error {
	if ch.Kind != "manifest_change" || ch.ManifestChange == nil {
		return nil
	}
	tv := cat.Tenants[tenant]
	for _, op := range ch.ManifestChange.Ops {
		if op.Target.Kind != "Schedule" || op.Field != "spec.cron" {
			continue
		}
		sc, ok := tv.Schedules[op.Target.Name]
		next, isString := op.Value.(string)
		if !ok || !isString {
			return rejection("forbidden_change")
		}
		old := sc.Cron
		var doc map[string]any
		err := tx.QueryRow(ctx, `SELECT doc FROM manifest_versions WHERE kind = 'Schedule' AND tenant = $1 AND name = $2 AND replaced_at > $3
			ORDER BY replaced_at LIMIT 1`, tenant, op.Target.Name, now.Add(-cronLookback)).Scan(&doc)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if v := asString(mmapAny(doc["spec"])["cron"]); v != "" {
			old = v
		}
		loc, err := time.LoadLocation(scheduleTZ(tv, sc))
		if err != nil {
			return rejection("forbidden_change")
		}
		was, ok1 := cronRuns(old, now.In(loc))
		will, ok2 := cronRuns(next, now.In(loc))
		if !ok1 || !ok2 || will*2 < was {
			return rejection("forbidden_change")
		}
	}
	return nil
}

// cronRuns counts the slots of an expression over 30 days from the day of from.
func cronRuns(expr string, from time.Time) (int, bool) {
	cs, err := parseCron(expr)
	if err != nil {
		return 0, false
	}
	daily := 0
	for m := range 60 {
		for h := range 24 {
			if cs.min[m] && cs.hour[h] {
				daily++
			}
		}
	}
	day := time.Date(from.Year(), from.Month(), from.Day(), 12, 0, 0, 0, from.Location())
	n := 0
	for i := range cronSpanDays {
		if cs.dayMatches(day.AddDate(0, 0, i)) {
			n += daily
		}
	}
	return n, true
}
