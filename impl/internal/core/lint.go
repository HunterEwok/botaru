// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/HunterEwok/botaru/impl/internal/manifest"
)

// The set lint of apply: the checks of ci/check_manifests.py that span objects of the set and
// that the schemas cannot express, with its stable codes (ci/lint_codes.py). The lint runs on
// the whole set after every object passed its schema, so a case gets the same codes from apply
// as from the skeleton lint over the files in git.

// builtinCommands are the channel commands that a tenant's commands cannot take (PRM-17).
var builtinCommands = map[string]bool{"/help": true, "/about": true, "/lang": true, "/mydata": true, "/tasks": true, "/status": true,
	"/cancel": true, "/me": true, "/team": true, "/run": true, "/service": true, "/forget": true, "/grant": true, "/ungrant": true}

// timeoutCeilings are the farm ceilings of type timeouts in seconds (DLV-16).
var timeoutCeilings = map[string]int64{"lease": 120, "heartbeat": 30, "tool_start_by": 30}

// decisionCaps are the decision and management capabilities that system and delegated groups
// never hold (PRM-10, PRM-16).
var decisionCaps = map[string]bool{"action.approve": true, "proposal.decide": true, "control.approve": true, "control.apply": true,
	"hold.set": true, "hold.release": true, "script.run": true, "incident.manage": true, "manager.direct": true, "audit.read": true,
	"governance.confirm": true, "governance.attribute": true, "proposal.override_veto": true, "model_grant.manage": true, "task.takeover": true}

var (
	reTaskWildcard = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z0-9_]+)*\.\*$`)
	reAddrPerson   = regexp.MustCompile(`(^|[^\w.])(user|staff|service|chat|group):[a-z]`)
	reEmail        = regexp.MustCompile(`[\w.+-]+@[\w-]+\.[\w.]+`)
	reCronTZ       = regexp.MustCompile(`(CRON_)?TZ\s*=`)
)

// lintObj is one object of the set with the index of its manifest.
type lintObj struct {
	i                  int
	kind, tenant, name string
	spec               map[string]any
}

// lintSet indexes the objects of a set by kind.
type lintSet struct {
	byKind map[string][]lintObj
}

func newLintSet(docs []manifest.Doc, index []int) *lintSet {
	s := &lintSet{byKind: map[string][]lintObj{}}
	for j, d := range docs {
		spec, _ := d.Value["spec"].(map[string]any)
		if spec == nil {
			spec = map[string]any{}
		}
		tenant := d.Tenant
		if d.Kind == "Tenant" {
			tenant = d.Name
		}
		s.byKind[d.Kind] = append(s.byKind[d.Kind], lintObj{i: index[j], kind: d.Kind, tenant: tenant, name: d.Name, spec: spec})
	}
	return s
}

func (s *lintSet) each(kind string) []lintObj { return s.byKind[kind] }

// get finds an object of a kind by name, in a tenant or global.
func (s *lintSet) get(kind, name, tenant string) map[string]any {
	for _, o := range s.byKind[kind] {
		if o.name == name && (tenant == "" || o.tenant == tenant || o.tenant == "") {
			return o.spec
		}
	}
	return nil
}

// lintFindings collects the errors of the set lint, one per code and object.
type lintFindings struct {
	out  []ApplyError
	seen map[string]bool
}

func (f *lintFindings) add(o lintObj, code, path string) {
	k := fmt.Sprintf("%d/%s/%s", o.i, code, path)
	if o.i < 0 {
		k = fmt.Sprintf("store/%s/%s/%s/%s/%s", o.kind, o.tenant, o.name, code, path)
	}
	if f.seen == nil {
		f.seen = map[string]bool{}
	}
	if f.seen[k] {
		return
	}
	f.seen[k] = true
	e := ApplyError{Kind: o.kind, Name: o.name, Code: code, Path: fmt.Sprintf("/manifests/%d/spec%s", o.i, path)}
	if o.i < 0 {
		// An object the store keeps from an earlier apply: the set does not hold it.
		e.Path = "/store/spec" + path
	}
	if o.kind != "Tenant" {
		e.Tenant = o.tenant
	}
	f.out = append(f.out, e)
}

// Accessors over decoded YAML.

func mstr(m map[string]any, k string) string { s, _ := m[k].(string); return s }
func mmap(m map[string]any, k string) map[string]any {
	v, _ := m[k].(map[string]any)
	if v == nil {
		return map[string]any{}
	}
	return v
}
func mlist(m map[string]any, k string) []any { v, _ := m[k].([]any); return v }
func mstrs(m map[string]any, k string) []string {
	var out []string
	for _, v := range mlist(m, k) {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
func mnum(m map[string]any, k string) (float64, bool) {
	switch v := m[k].(type) {
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case float64:
		return v, true
	case json.Number:
		f, err := v.Float64()
		return f, err == nil
	}
	return 0, false
}
func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	case int:
		return x != 0
	case float64:
		return x != 0
	case json.Number:
		f, _ := x.Float64()
		return f != 0
	}
	return true
}

// durSeconds reads a duration of the schemas (15m) in seconds, or -1.
func durSeconds(v any) int64 {
	s, _ := v.(string)
	d, ok := parseDuration(s)
	if !ok {
		return -1
	}
	return int64(d.Seconds())
}

// walkNodes calls fn for every object node under v.
func walkNodes(v any, fn func(map[string]any)) {
	switch x := v.(type) {
	case map[string]any:
		fn(x)
		for _, e := range x {
			walkNodes(e, fn)
		}
	case []any:
		for _, e := range x {
			walkNodes(e, fn)
		}
	}
}

func hasKey(v any, key string) bool {
	found := false
	walkNodes(v, func(m map[string]any) {
		if _, ok := m[key]; ok {
			found = true
		}
	})
	return found
}

// allStrings calls fn for every string of v, the keys of objects included.
func allStrings(v any, fn func(string)) {
	switch x := v.(type) {
	case string:
		fn(x)
	case map[string]any:
		for k, e := range x {
			fn(k)
			allStrings(e, fn)
		}
	case []any:
		for _, e := range x {
			allStrings(e, fn)
		}
	}
}

// lintRegs are the registries the set lint reads.
type lintRegs struct {
	formats      map[string]*regexp.Regexp
	capForms     map[string]map[string]any
	dataClasses  map[string]map[string]any
	roleProfiles map[string]map[string]any
	purposes     map[string]bool
	forbidden    map[string]bool
	kinds        map[string]map[string]any
	retention    map[string]map[string]any
	driverForbid map[string]bool
}

func (c *Core) lintRegistries() (*lintRegs, error) {
	r := &lintRegs{formats: map[string]*regexp.Regexp{}, capForms: map[string]map[string]any{}, dataClasses: map[string]map[string]any{},
		roleProfiles: map[string]map[string]any{}, purposes: map[string]bool{}, kinds: map[string]map[string]any{},
		retention: map[string]map[string]any{}, driverForbid: map[string]bool{}}
	load := func(path string, fn func(id string, e map[string]any)) error {
		reg, err := c.spec.Registry(path)
		if err != nil {
			return err
		}
		for _, e := range reg.Entries {
			if id, ok := e["id"].(string); ok {
				fn(id, e)
			}
		}
		return nil
	}
	if err := load("spec/model/formats.yaml", func(id string, e map[string]any) {
		if p, ok := e["pattern"].(string); ok {
			// Go regexp has no lookaround; a pattern it cannot compile is left to the schema.
			if re, err := regexp.Compile(p); err == nil {
				r.formats[id] = re
			} else {
				r.formats[id] = nil
			}
		}
	}); err != nil {
		return nil, err
	}
	if err := load("spec/model/capabilities.yaml", func(id string, e map[string]any) {
		switch e["kind"] {
		case "capability":
			r.capForms[id] = e
		case "data_class":
			r.dataClasses[id] = e
		}
	}); err != nil {
		return nil, err
	}
	if err := load("spec/model/role-profiles.yaml", func(id string, e map[string]any) { r.roleProfiles[id] = e }); err != nil {
		return nil, err
	}
	if err := load("spec/model/task-purposes.yaml", func(id string, _ map[string]any) { r.purposes[id] = true }); err != nil {
		return nil, err
	}
	if err := load("spec/manifests/kinds.yaml", func(id string, e map[string]any) { r.kinds[id] = e }); err != nil {
		return nil, err
	}
	if err := load("spec/model/retention-classes.yaml", func(id string, e map[string]any) { r.retention[id] = e }); err != nil {
		return nil, err
	}
	return r, nil
}

// formatOK says whether v passes a registry format; a format the registry has without a
// pattern Go can run passes here and is left to the schema checks.
func (r *lintRegs) formatOK(format, v string) bool {
	re := r.formats[format]
	return re == nil || re.MatchString(v)
}

// parseCap splits a capability into its form and argument; "" when the form is unknown.
func (r *lintRegs) parseCap(cp string) (string, string) {
	if e, ok := r.capForms[cp]; ok && !strings.Contains(fmt.Sprint(e["form"]), "<") {
		return cp, ""
	}
	if base, arg, ok := strings.Cut(cp, ":"); ok {
		if e, ok := r.capForms[base]; ok && strings.Contains(fmt.Sprint(e["form"]), "<") {
			return base, arg
		}
	}
	if _, ok := r.capForms[cp]; ok {
		return cp, ""
	}
	return "", ""
}

// lintManifests runs the set lint (MAN-6: an error fails the whole set).
func (c *Core) lintManifests(docs []manifest.Doc, index []int) ([]ApplyError, error) {
	r, err := c.lintRegistries()
	if err != nil {
		return nil, err
	}
	forbidden, err := c.forbiddenPrograms()
	if err != nil {
		return nil, err
	}
	r.forbidden = forbidden
	S := newLintSet(docs, index)
	f := &lintFindings{}
	farm := map[string]bool{}
	for _, h := range c.cfg.FarmHosts {
		farm[h] = true
	}

	for _, kind := range sortedKinds(S) {
		for _, o := range S.each(kind) {
			if hasKey(o.spec, "max_tokens") {
				f.add(o, "max_tokens_field", "")
			}
			// Leaf formats inside manifests (INJ-1).
			walkNodes(o.spec, func(n map[string]any) {
				if fm, ok := n["x-botaru-format"].(string); ok {
					if _, known := r.formats[fm]; !known {
						f.add(o, "format_unknown", "")
					}
				}
			})
		}
	}
	tenants := map[string]bool{}
	for _, o := range S.each("Tenant") {
		tenants[o.name] = true
	}
	types := map[string]map[string]any{}
	for _, o := range S.each("TaskType") {
		types[o.name] = o.spec
	}
	type toolRef struct {
		plugin string
		ps, t  map[string]any
	}
	tools := map[string]toolRef{}
	for _, o := range S.each("ToolPlugin") {
		for _, t := range mlist(o.spec, "tools") {
			tm, _ := t.(map[string]any)
			if tm != nil {
				tools[o.name+"/"+mstr(tm, "name")] = toolRef{o.name, o.spec, tm}
			}
		}
	}
	// A role names a tool as <plugin>/<tool> or a whole plugin as <plugin>: the bare form
	// grants every enabled tool of the plugin and is checked tool by tool.
	expand := func(ref string) []string {
		if strings.Contains(ref, "/") {
			return []string{ref}
		}
		var out []string
		for _, k := range slices.Sorted(maps.Keys(tools)) {
			if strings.HasPrefix(k, ref+"/") && tools[k].t["enabled"] != false {
				out = append(out, k)
			}
		}
		return out
	}
	plugins := map[string]bool{}
	for _, o := range S.each("ToolPlugin") {
		plugins[o.name] = true
	}
	actions := map[string]map[string]any{}
	for _, o := range S.each("Action") {
		actions[o.name] = o.spec
	}
	// A model profile of either scope resolves as the runtime catalog does: the profile of
	// the tenant first, then the global one of the same name.
	profiles := map[[2]string]map[string]any{}
	for _, o := range S.each("ModelProfile") {
		profiles[[2]string{o.tenant, o.name}] = o.spec
	}
	profile := func(tenant, name string) map[string]any {
		if p, ok := profiles[[2]string{tenant, name}]; ok {
			return p
		}
		return profiles[[2]string{"", name}]
	}
	// A global object (a role, a task type) runs in every tenant, so it is checked against
	// the global profile and against every tenant profile of the same name.
	variants := func(tenant, name string) []map[string]any {
		if tenant != "" {
			if p := profile(tenant, name); p != nil {
				return []map[string]any{p}
			}
			return nil
		}
		var out []map[string]any
		for _, k := range slices.SortedFunc(maps.Keys(profiles), func(a, b [2]string) int { return strings.Compare(a[0]+"\x00"+a[1], b[0]+"\x00"+b[1]) }) {
			if k[1] == name {
				out = append(out, profiles[k])
			}
		}
		return out
	}
	roles := map[string]lintObj{}
	for _, o := range S.each("Role") {
		roles[o.name] = o
	}
	roleNames := slices.Sorted(mapKeys(roles))

	// Catalogs: values, services, farm nodes, machines of one tenant (MAN-10, DEP-11, TEN-4, MON-8).
	catalogs := map[string]map[string][]string{}
	hostOwner := map[string]string{}
	svcOwner := map[string]string{}
	for _, o := range S.each("Catalog") {
		lists := map[string][]string{}
		for _, h := range mlist(o.spec, "hosts") {
			hm, _ := h.(map[string]any)
			lists["hosts"] = append(lists["hosts"], mstr(hm, "id"))
			for _, s := range mlist(hm, "services") {
				sm, _ := s.(map[string]any)
				lists["services"] = append(lists["services"], mstr(sm, "name"))
			}
		}
		for _, tr := range mlist(o.spec, "monitor_triggers") {
			tm, _ := tr.(map[string]any)
			lists["monitor_triggers"] = append(lists["monitor_triggers"], mstr(tm, "triggerid"))
		}
		for j, res := range mlist(o.spec, "resources") {
			rm, _ := res.(map[string]any)
			path := fmt.Sprintf("/resources/%d", j)
			fm := mstr(rm, "format")
			if fm == "" {
				fm = "id"
			}
			if _, ok := r.formats[fm]; !ok {
				f.add(o, "format_unknown", path+"/format")
			} else {
				for k, v := range mlist(rm, "values") {
					if !r.formatOK(fm, fmt.Sprint(v)) {
						f.add(o, "catalog_value_format", fmt.Sprintf("%s/values/%d", path, k))
					}
				}
			}
			if l := mstr(rm, "list"); l != "" {
				for _, v := range mlist(rm, "values") {
					lists[l] = append(lists[l], fmt.Sprint(v))
				}
			}
			if host := mstr(rm, "host"); host != "" {
				if farm[host] {
					f.add(o, "farm_host_in_catalog", path+"/host")
				}
				if !slices.Contains(lists["hosts"], host) {
					f.add(o, "ref_missing", path+"/host")
				}
			}
		}
		catalogs[o.tenant] = lists
		for j, h := range mlist(o.spec, "hosts") {
			hm, _ := h.(map[string]any)
			path := fmt.Sprintf("/hosts/%d", j)
			ids := []string{mstr(hm, "id")}
			if mon := mstr(hm, "monitor_host"); mon != "" {
				short, _, _ := strings.Cut(mon, ".")
				ids = append(ids, mon, short)
			}
			for _, id := range ids {
				if farm[id] {
					f.add(o, "farm_host_in_catalog", path)
				}
			}
			for _, id := range ids {
				if other, ok := hostOwner[id]; ok && other != o.tenant {
					f.add(o, "host_two_tenants", path)
				}
				hostOwner[id] = o.tenant
			}
			scripts := map[string]bool{}
			for _, s := range mlist(hm, "monitor_scripts_allowed") {
				sm, _ := s.(map[string]any)
				scripts[mstr(sm, "name")] = true
			}
			for k, s := range mlist(hm, "services") {
				sm, _ := s.(map[string]any)
				sp := fmt.Sprintf("%s/services/%d", path, k)
				if nc, ok := sm["no_card"].([]any); ok {
					for _, v := range nc {
						if v != "restart" {
							f.add(o, "service_no_card", sp)
						}
					}
				}
				switch mstr(sm, "via") {
				case "host":
					if mstr(sm, "unit") == "" {
						f.add(o, "service_unit_missing", sp)
					}
				case "monitor":
					if mstr(sm, "unit") != "" {
						f.add(o, "service_unit_missing", sp)
					}
					for _, verb := range mstrs(sm, "control") {
						sc := mstr(mmap(sm, "scripts"), verb)
						if sc == "" || !scripts[sc] {
							f.add(o, "service_script_unknown", sp)
						}
					}
				}
				key := mstr(hm, "id") + "\x00" + mstr(sm, "name")
				if other, ok := svcOwner[key]; ok && other != o.tenant {
					f.add(o, "host_two_tenants", sp)
				}
				svcOwner[key] = o.tenant
			}
		}
	}

	// Groups: capabilities (PRM-7, APR-5, PRM-2, PRM-10).
	groups := map[string]map[string]any{}
	for _, o := range S.each("Group") {
		groups[o.tenant+"\x00"+o.name] = o.spec
		for j, cpv := range mstrs(o.spec, "caps") {
			path := fmt.Sprintf("/caps/%d", j)
			base, arg := r.parseCap(cpv)
			if base == "" {
				f.add(o, "cap_unknown", path)
				continue
			}
			if strings.Contains(cpv, "*") {
				if !(base == "task" && reTaskWildcard.MatchString(arg) && !strings.HasPrefix(arg, "action.") && o.name != "system-ops") {
					f.add(o, "cap_star", path)
					continue
				}
			}
			if base == "task" && strings.HasPrefix(arg, "action.") {
				f.add(o, "action_type_grant", path)
			}
			if base == "data" {
				if dc, ok := r.dataClasses[arg]; !ok || truthy(dc["reserved"]) {
					f.add(o, "cap_data_class", path)
				}
			}
			if (base == "action.request" || base == "action.approve") && actions[arg] == nil && arg != "service.control" && arg != "monitor.script_run" {
				f.add(o, "cap_unknown_action", path)
			}
			if o.name == "system-ops" && decisionCaps[base] {
				f.add(o, "system_decision_cap", path)
			}
		}
		for j, d := range mstrs(o.spec, "data") {
			if dc, ok := r.dataClasses[d]; !ok || truthy(dc["reserved"]) {
				f.add(o, "cap_data_class", fmt.Sprintf("/data/%d", j))
			}
		}
		for j, inh := range mstrs(o.spec, "inherits") {
			if S.get("Group", inh, o.tenant) == nil {
				f.add(o, "ref_missing", fmt.Sprintf("/inherits/%d", j))
			}
		}
	}
	var effCaps func(tenant, g string, seen map[string]bool) (map[string]bool, map[string]bool)
	effCaps = func(tenant, g string, seen map[string]bool) (map[string]bool, map[string]bool) {
		caps, data := map[string]bool{}, map[string]bool{}
		gs := groups[tenant+"\x00"+g]
		for _, cp := range mstrs(gs, "caps") {
			caps[cp] = true
		}
		for _, d := range mstrs(gs, "data") {
			data[d] = true
		}
		seen[g] = true
		for _, inh := range mstrs(gs, "inherits") {
			if !seen[inh] {
				c2, d2 := effCaps(tenant, inh, seen)
				for k := range c2 {
					caps[k] = true
				}
				for k := range d2 {
					data[k] = true
				}
			}
		}
		return caps, data
	}
	systemHas := func(tenant, tt string) bool {
		caps, _ := effCaps(tenant, "system-ops", map[string]bool{})
		return caps["task:"+tt]
	}
	order := func(class string) float64 {
		if dc, ok := r.dataClasses[class]; ok {
			n, ok := mnum(dc, "order")
			if ok {
				return n
			}
		}
		return 9
	}

	// Tenants (APR-9, PRM-17, DSP-10, APR-17, PRM-16, MGR-20, MON-1, GW-15, DSP-3).
	var seatProfile string
	for _, o := range S.each("Governance") {
		if o.tenant == "farm" {
			if mp := mstr(mmap(o.spec, "farm_manager"), "model_profile"); mp != "" {
				seatProfile = mp
			}
		}
	}
	for _, o := range S.each("Tenant") {
		ts := o.spec
		kind := mstr(ts, "kind")
		if kind == "" {
			kind = "production"
		}
		if kind == "production" && ts["self_approve"] == true {
			f.add(o, "self_approve_production", "/self_approve")
		}
		for _, cmd := range slices.Sorted(mapKeys(mmap(ts, "commands"))) {
			tt, _ := mmap(ts, "commands")[cmd].(string)
			path := "/commands/" + cmd
			if builtinCommands[cmd] {
				f.add(o, "builtin_command", path)
			}
			if strings.HasPrefix(tt, "action.") {
				f.add(o, "action_type_grant", path)
			} else if len(types) > 0 && types[tt] == nil {
				c.lintWarn(o, "ref_missing", path)
			}
		}
		assistant := map[string]bool{}
		for _, rn := range roleNames {
			if roles[rn].spec["memory"] == "person" {
				for _, t := range mstrs(roles[rn].spec, "claims") {
					assistant[t] = true
				}
			}
		}
		for j, dt := range mlist(ts, "default_types") {
			t, _ := dt.(string)
			if m, ok := dt.(map[string]any); ok {
				t = mstr(m, "type")
			}
			path := fmt.Sprintf("/default_types/%d", j)
			if assistant[t] {
				f.add(o, "assistant_in_default_types", path)
			} else if len(types) > 0 && types[t] == nil {
				c.lintWarn(o, "ref_missing", path)
			}
		}
		for _, rn := range slices.Sorted(mapKeys(mmap(ts, "run"))) {
			rs, _ := mmap(ts, "run")[rn].(map[string]any)
			path := "/run/" + rn
			var props map[string]any
			if tn, ok := rs["type"].(string); ok {
				tt := types[tn]
				if tt == nil {
					c.lintWarn(o, "ref_missing", path)
					continue
				}
				if mstr(mmap(tt, "run"), "approval") != "none" {
					f.add(o, "run_needs_approval_none", path)
				}
				props = mmap(mmap(tt, "params_schema"), "properties")
			} else {
				act := actions[mstr(rs, "action")]
				if act == nil {
					c.lintWarn(o, "ref_missing", path)
					continue
				}
				props = mmap(mmap(act, "args"), "properties")
			}
			for _, a := range mstrs(rs, "args") {
				if _, ok := props[a]; !ok {
					f.add(o, "run_arg_unknown", path)
				}
			}
		}
		for j, g := range mstrs(ts, "delegated_groups") {
			path := fmt.Sprintf("/delegated_groups/%d", j)
			if groups[o.name+"\x00"+g] == nil {
				c.lintWarn(o, "ref_missing", path)
				continue
			}
			caps, data := effCaps(o.name, g, map[string]bool{})
			bad := false
			for cp := range caps {
				base, arg := r.parseCap(cp)
				switch {
				case base == "action.request" || base == "service.control" || base == "monitor.script_run" || decisionCaps[base] ||
					strings.HasPrefix(base, "admin") || strings.HasPrefix(cp, "admin"):
					bad = true
				case (base == "task" || base == "task.read") && strings.Contains(arg, "*"):
					// A wildcard reaches types of any tier (PRM-16).
					bad = true
				case (base == "task" || base == "task.read") && types[arg] != nil:
					tier, _ := mnum(types[arg], "tier")
					if tier >= 3 || order(mstr(types[arg], "data_class")) > 1 {
						bad = true
					}
				}
			}
			for d := range data {
				if order(d) > 1 {
					bad = true
				}
			}
			if bad {
				f.add(o, "delegated_group_privileged", path)
			}
		}
		fm := mmap(ts, "farm_manager")
		if fm["enabled"] != false && c.claimsProfile("manager") {
			switch {
			case seatProfile == "":
				f.add(o, "manager_profile_missing", "/farm_manager")
			case !slices.Contains(mstrs(ts, "models_allowed"), seatProfile):
				f.add(o, "manager_profile_not_allowed", "/models_allowed")
			}
			line := mmap(mmap(mmap(ts, "budgets"), "lines"), "manager")
			m, _ := mnum(line, "month_eur")
			d, _ := mnum(line, "day_eur")
			if m <= 0 && d <= 0 {
				f.add(o, "manager_line_missing", "/budgets/lines")
			}
		}
		priv := mmap(ts, "privacy")
		if rc, ok := priv["recipients"].([]any); ok && !slices.Contains(rc, any("monitoring")) && c.claimsProfile("monitor") {
			f.add(o, "recipients_monitoring", "/privacy/recipients")
		}
		subs := map[string]map[string]any{}
		for _, s := range mlist(priv, "subprocessors") {
			sm, _ := s.(map[string]any)
			subs[mstr(sm, "provider")] = sm
		}
		for j, mp := range mstrs(ts, "models_allowed") {
			path := fmt.Sprintf("/models_allowed/%d", j)
			prof := profile(o.tenant, mp)
			if prof == nil {
				if len(profiles) > 0 {
					c.lintWarn(o, "ref_missing", path)
				}
				continue
			}
			if mmap(prof, "provider_training")["value"] == true {
				f.add(o, "provider_training", path)
			}
			sub := subs[mstr(prof, "provider")]
			if len(subs) > 0 && sub == nil {
				f.add(o, "subprocessor_undeclared", path)
			}
			for _, ps := range mlist(prof, "subprocessors") {
				pm, _ := ps.(map[string]any)
				if len(subs) > 0 && subs[mstr(pm, "provider")] == nil {
					f.add(o, "subprocessor_undeclared", path)
				}
			}
			if sub != nil {
				if mmap(prof, "provider_human_review")["value"] == "yes" {
					if hr := mstr(mmap(sub, "terms"), "human_review"); hr == "none" || hr == "abuse_only" {
						f.add(o, "provider_human_review", path)
					}
				}
				ret := mmap(sub, "retention")
				if mstr(ret, "mode") == "zero" && truthy(prof["requires_retention"]) {
					f.add(o, "provider_retention", path)
				}
				days, hasDays := mnum(mmap(prof, "provider_retention_days"), "value")
				maxDays, _ := mnum(ret, "max_days")
				if mstr(ret, "mode") == "standard" && hasDays && days > maxDays {
					f.add(o, "provider_retention", path)
				}
			}
		}
		c.lintDispatch(f, o, types, roles, S)
	}

	// Task types (APR-5, INJ-7, OBV-3, PRP-10, APR-17, DLV-16).
	for _, o := range S.each("TaskType") {
		ts := o.spec
		if strings.HasPrefix(o.name, "action.") {
			f.add(o, "action_type_grant", "")
		}
		if !r.purposes[mstr(ts, "purpose")] {
			f.add(o, "purpose_unknown", "/purpose")
		}
		for _, k := range slices.Sorted(mapKeys(timeoutCeilings)) {
			if s := durSeconds(mmap(ts, "timeouts")[k]); s > timeoutCeilings[k] {
				f.add(o, "timeout_ceiling", "/timeouts/"+k)
			}
		}
		alarms := false
		for _, e := range mstrs(ts, "events") {
			if strings.HasPrefix(e, "alarm.") {
				alarms = true
			}
		}
		if alarms && mstr(ts, "worker_kind") != "job" {
			f.add(o, "alarm_not_job", "/events")
		}
		if alarms && truthy(mmap(ts, "inbox")["read"]) {
			f.add(o, "alarm_inbox", "/inbox")
		}
		if truthy(ts["observes"]) && mstr(ts, "worker_kind") != "job" {
			f.add(o, "observes_not_job", "/observes")
		}
		mustFalse := mstr(ts, "purpose") != "work" || alarms || truthy(ts["makes_backup"])
		if mustFalse && ts["proposable"] != false {
			f.add(o, "proposable_required_false", "/proposable")
		}
		if mstr(mmap(ts, "run"), "approval") == "none" {
			for _, rn := range roleNames {
				rs := roles[rn].spec
				if !slices.Contains(mstrs(rs, "claims"), o.name) {
					continue
				}
				writes := false
				for _, t := range slices.Concat(mapSlice(mstrs(rs, "tools"), expand)...) {
					if tr, ok := tools[t]; ok && mstr(tr.t, "access") == "write" {
						writes = true
					}
				}
				if truthy(rs["actions_may_request"]) || writes {
					f.add(o, "run_not_readonly", "/run")
				}
			}
		}
	}

	// Plugins: actions behind args_from (MAN-9) and the network list (INJ-6). The checks of
	// one plugin alone ran with its manifest (checkPlugin).
	for _, o := range S.each("ToolPlugin") {
		for j, t := range mlist(o.spec, "tools") {
			tm, _ := t.(map[string]any)
			if tm["enabled"] == false {
				continue
			}
			path := fmt.Sprintf("/tools/%d", j)
			if af := mstr(tm, "args_from"); af != "" {
				_, an, _ := strings.Cut(af, ":")
				act := actions[an]
				if truthy(tm["params"]) {
					f.add(o, "args_from_second_schema", path)
				}
				if act == nil {
					if len(actions) > 0 {
						f.add(o, "ref_missing", path)
					}
				} else if ex := mmap(act, "executor"); mstr(ex, "plugin") != o.name || mstr(ex, "tool") != mstr(tm, "name") {
					f.add(o, "args_from_mismatch", path)
				}
			}
			if mmap(tm, "credential")["per_principal"] == true {
				if n, _ := mnum(tm, "cache_s"); n > 0 {
					f.add(o, "cache_per_principal", path)
				}
			}
		}
		c.lintEgress(f, o, catalogs)
	}

	// Actions (MAN-9).
	for _, o := range S.each("Action") {
		a := o.spec
		routine := []string{"writes_only", "runs_per_hour", "runs_per_day"}
		if mstr(a, "risk") == "routine" {
			missing := a["idempotent"] != true
			for _, k := range routine {
				if _, ok := a[k]; !ok {
					missing = true
				}
			}
			if n, _ := mnum(a, "approvals"); missing || n == 2 {
				f.add(o, "routine_fields", "")
			}
		} else {
			for _, k := range routine {
				if _, ok := a[k]; ok {
					f.add(o, "routine_fields", "/"+k)
				}
			}
			if n, _ := mnum(a, "approvals"); n == 2 {
				// Two approvers arrive in v0.2 (APR-10, APR-16).
				f.add(o, "not_in_version", "/approvals")
			}
		}
		ex := mmap(a, "executor")
		tr, ok := tools[mstr(ex, "plugin")+"/"+mstr(ex, "tool")]
		switch {
		case len(tools) > 0 && !ok:
			f.add(o, "executor_unknown", "/executor")
		case ok && mstr(tr.t, "access") != "write":
			f.add(o, "executor_unknown", "/executor")
		}
	}

	// Roles (PRF-1, GOV-3, MEM-2, APR-1, APR-5).
	for _, rn := range roleNames {
		o := roles[rn]
		rs := o.spec
		pname := mstr(rs, "profile")
		if pname == "" {
			pname = "worker"
		}
		prof, ok := r.roleProfiles[pname]
		if !ok {
			f.add(o, "role_profile_unknown", "/profile")
			continue
		}
		purposes := mstrs(prof, "purposes")
		maxTier, hasMax := mnum(prof, "max_task_tier")
		for j, tn := range mstrs(rs, "claims") {
			ts := types[tn]
			if ts == nil {
				continue
			}
			path := fmt.Sprintf("/claims/%d", j)
			if !slices.Contains(purposes, mstr(ts, "purpose")) {
				f.add(o, "role_profile_purpose", path)
			}
			if tier, _ := mnum(ts, "tier"); hasMax && tier > maxTier {
				f.add(o, "role_profile_tier", path)
			}
			if mstr(ts, "worker_kind") != mstr(rs, "worker_kind") {
				f.add(o, "worker_kind_mismatch", path)
			}
		}
		for _, field := range mstrs(prof, "must_be_empty") {
			head, sub, _ := strings.Cut(field, ".")
			v := rs[head]
			if m, ok := v.(map[string]any); ok && sub != "" {
				v = m[sub]
			}
			if truthy(v) {
				f.add(o, "role_profile_forbidden", "/"+head)
			}
		}
		for _, field := range []string{"service_control", "monitor_scripts", "insight"} {
			if truthy(rs[field]) && !slices.Contains(mstrs(prof, "may_declare"), field) {
				f.add(o, "role_profile_forbidden", "/"+field)
			}
		}
		mem := "none"
		switch m := rs["memory"].(type) {
		case string:
			mem = m
		case map[string]any:
			mem = "read_bot"
		}
		if !slices.Contains(mstrs(prof, "memory"), mem) {
			f.add(o, "role_profile_memory", "/memory")
		}
		for _, slot := range slices.Sorted(mapKeys(mmap(rs, "models"))) {
			if !slices.Contains(mstrs(prof, "slots"), slot) {
				f.add(o, "role_profile_slot", "/models/"+slot)
			}
		}
		for _, sc := range []struct{ slot, key string }{{"base", "max_rank"}, {"analysis", "analysis_max_rank"}} {
			ceil, ok := mnum(prof, sc.key)
			if !ok {
				continue
			}
			mp := mstr(mmap(rs, "models"), sc.slot)
			chain := []string{mp}
			for _, pr := range variants(o.tenant, mp) {
				chain = append(chain, mstrs(pr, "fallback")...)
			}
			for _, p := range chain {
				for _, pr := range variants(o.tenant, p) {
					if rank, _ := mnum(pr, "rank"); rank > ceil {
						f.add(o, "rank_exceeded", "/models/"+sc.slot)
					}
				}
			}
		}
		if cp := mstrs(prof, "campaigns_propose"); len(cp) > 0 && truthy(rs["campaigns"]) && !slices.Contains(cp, mstr(mmap(rs, "campaigns"), "propose")) {
			f.add(o, "role_profile_forbidden", "/campaigns")
		}
		if sp := mstrs(prof, "scripts_propose"); len(sp) > 0 && truthy(rs["scripts"]) && !slices.Contains(sp, mstr(mmap(rs, "scripts"), "propose")) {
			f.add(o, "role_profile_forbidden", "/scripts")
		}
		for j, ref := range mstrs(rs, "tools") {
			path := fmt.Sprintf("/tools/%d", j)
			if !strings.Contains(ref, "/") {
				if !plugins[ref] && len(plugins) > 0 {
					f.add(o, "ref_missing", path)
				}
			}
			for _, tref := range expand(ref) {
				tr, ok := tools[tref]
				switch {
				case !ok:
					if len(tools) > 0 {
						f.add(o, "ref_missing", path)
					}
				case mstr(tr.t, "access") != "read":
					f.add(o, "role_write_tool", path)
				}
			}
		}
		for j, tn := range mstrs(rs, "may_enqueue") {
			if strings.HasPrefix(tn, "action.") {
				f.add(o, "action_type_grant", fmt.Sprintf("/may_enqueue/%d", j))
			}
		}
		for j, an := range mstrs(rs, "actions_may_request") {
			if len(actions) > 0 && actions[an] == nil {
				f.add(o, "ref_missing", fmt.Sprintf("/actions_may_request/%d", j))
			}
		}
		if mem == "person" {
			bad := false
			for _, tn := range mstrs(rs, "claims") {
				if ts := types[tn]; ts != nil && (mstr(ts, "data_class") != "summary" || truthy(mmap(ts, "inbox")["read"])) {
					bad = true
				}
			}
			for _, t := range slices.Concat(mapSlice(mstrs(rs, "tools"), expand)...) {
				if tr, ok := tools[t]; ok && truthy(tr.t["untrusted_output"]) {
					bad = true
				}
			}
			if bad {
				f.add(o, "memory_person", "/memory")
			}
		}
	}
	// One level of aggregators (AGG-3).
	for _, rn := range roleNames {
		o := roles[rn]
		if mstr(o.spec, "profile") != "aggregator" {
			continue
		}
		agg := mmap(o.spec, "aggregate")
		src := mmap(agg, "sources")
		count := 0
		for _, k := range []string{"roles", "bot_groups", "series", "message_kinds"} {
			count += len(mlist(src, k))
		}
		if mx, ok := mnum(agg, "max_sources"); ok && float64(count) > mx {
			f.add(o, "aggregator_sources_over_max", "/aggregate/sources")
		}
		named := mstrs(src, "roles")
		for _, bg := range mstrs(src, "bot_groups") {
			named = append(named, mstrs(mmap(S.get("BotGroup", bg, ""), "select"), "roles")...)
		}
		for _, n := range named {
			if ro, ok := roles[n]; ok && mstr(ro.spec, "profile") == "aggregator" {
				f.add(o, "aggregator_source_aggregator", "/aggregate/sources")
			}
		}
	}

	// Routes and schedules (PRM-10, APR-5, TIM-2, SCR-12, PRP-10).
	checkAddress := func(o lintObj, path string, addr any) {
		if m, ok := addr.(map[string]any); ok {
			an := mstr(mmap(m, "approval_initiator"), "action")
			if len(actions) > 0 && actions[an] == nil {
				f.add(o, "ref_missing", path)
			}
			return
		}
		s, _ := addr.(string)
		kind, name, _ := strings.Cut(s, ":")
		if kind == "group" && S.get("Group", name, o.tenant) == nil && S.get("BotGroup", name, o.tenant) == nil {
			f.add(o, "ref_missing", path)
		}
		if kind == "sink" && S.get("Sink", name, o.tenant) == nil {
			f.add(o, "ref_missing", path)
		}
	}
	for _, o := range S.each("Route") {
		rs := o.spec
		mode := mstr(rs, "mode")
		if mode == "" {
			mode = "task"
		}
		if _, ok := rs[mode]; !ok && (mode == "task" || mode == "report") {
			f.add(o, "route_mode", "/mode")
		}
		task := mmap(rs, "task")
		if tt := mstr(task, "type"); tt != "" {
			switch {
			case strings.HasPrefix(tt, "action."):
				f.add(o, "action_type_grant", "/task/type")
			case len(types) > 0 && types[tt] == nil:
				f.add(o, "ref_missing", "/task/type")
			case len(groups) > 0 && !systemHas(o.tenant, tt):
				f.add(o, "system_ops_missing", "/task/type")
			}
			if p, ok := task["params"]; ok && types[tt] != nil {
				if c.rs.validate(resolveSchema(mmap(types[tt], "params_schema"), catalogs[o.tenant], r), p) != nil {
					f.add(o, "params_invalid", "/task/params")
				}
			}
		}
		for j, a := range mlist(rs, "deliver_to") {
			checkAddress(o, fmt.Sprintf("/deliver_to/%d", j), a)
		}
	}
	for _, o := range S.each("Tenant") {
		if tz := mstr(o.spec, "timezone"); tz != "" && !knownZone(tz) {
			f.add(o, "timezone_unknown", "/timezone")
		}
	}
	for _, o := range S.each("Schedule") {
		ss := o.spec
		cron := mstr(ss, "cron")
		if _, err := parseCron(cron); err != nil || reCronTZ.MatchString(cron) {
			f.add(o, "cron_invalid", "/cron")
		}
		if tz := mstr(ss, "timezone"); tz != "" && !knownZone(tz) {
			f.add(o, "timezone_unknown", "/timezone")
		}
		_, hasTask := ss["task"]
		_, hasScript := ss["script"]
		if hasTask == hasScript {
			f.add(o, "schedule_target", "")
			continue
		}
		for j, a := range mlist(ss, "deliver_to") {
			checkAddress(o, fmt.Sprintf("/deliver_to/%d", j), a)
		}
		if hasScript {
			// Scripts are a kind of v0.2; apply refuses the kind itself.
			continue
		}
		task := mmap(ss, "task")
		tt := mstr(task, "type")
		if strings.HasPrefix(tt, "action.") {
			f.add(o, "action_type_grant", "/task/type")
			continue
		}
		ttype := types[tt]
		if ttype == nil {
			if len(types) > 0 {
				f.add(o, "ref_missing", "/task/type")
			}
			continue
		}
		if mstr(mmap(ttype, "timeouts"), "deadline") == "" {
			f.add(o, "schedule_deadline", "/task/type")
		}
		if len(groups) > 0 && !systemHas(o.tenant, tt) {
			f.add(o, "system_ops_missing", "/task/type")
		}
		if ttype["proposable"] == false && ss["proposable"] != false {
			f.add(o, "proposable_required_false", "/proposable")
		}
		params, ok := task["params"]
		if !ok {
			params = map[string]any{}
		}
		if c.rs.validate(resolveSchema(mmap(ttype, "params_schema"), catalogs[o.tenant], r), params) != nil {
			f.add(o, "params_invalid", "/task/params")
		}
	}

	// Model profile prices older than 120 days refuse the set (GW-17); a test bench, whose
	// example prices age with the calendar, only logs them.
	for _, o := range S.each("ModelProfile") {
		checked, err := time.Parse("2006-01-02", mstr(mmap(o.spec, "price"), "checked"))
		if err != nil || time.Since(checked) <= 120*24*time.Hour {
			continue
		}
		if c.cfg.TestBench {
			c.log.Warn("the price of a model profile is older than 120 days", "profile", o.name, "checked", checked.Format("2006-01-02"))
			continue
		}
		f.add(o, "price_stale", "/price/checked")
	}

	// Message kinds (STA-1, MSG-2).
	for _, o := range S.each("MessageType") {
		ms := o.spec
		kind := mstr(ms, "kind")
		if kind == "state" && !slices.Equal(mstrs(ms, "producers"), []string{"code"}) {
			f.add(o, "state_agent_producer", "/producers")
		}
		if !strings.HasPrefix(o.name, kind+".") {
			f.add(o, "message_name", "")
		}
		need := []string{"stale_after"}
		if kind == "report" {
			need = []string{"delivery", "ttl"}
		}
		for _, k := range need {
			if _, ok := ms[k]; !ok {
				f.add(o, "message_fields", "/"+k)
			}
		}
	}
	// The tree limit of an agent type covers the output of one call (GW-2, BUD-2).
	// No record that code reads from model output names a person or rates one (DLV-14).
	for _, o := range S.each("TaskType") {
		if rs := mmap(o.spec, "result_schema"); readByCode(rs) && namesHuman(rs) {
			f.add(o, "human_subject_in_record", "/result_schema")
		}
		// Only the core writes provenance (PRV-2).
		if _, ok := mmap(mmap(o.spec, "result_schema"), "properties")["provenance"]; ok {
			f.add(o, "provenance_in_result", "/result_schema/properties/provenance")
		}
	}
	for _, o := range S.each("TaskType") {
		ts := o.spec
		budget, ok := mnum(ts, "tree_budget_eur")
		if mstr(ts, "worker_kind") != "agent" || !ok {
			continue
		}
		worst := -1.0
		slot := "base"
		if p := mstr(ts, "purpose"); p == "insight" || p == "aggregate" {
			slot = "analysis"
		}
		for _, rn := range roleNames {
			rs := roles[rn].spec
			if !slices.Contains(mstrs(rs, "claims"), o.name) {
				continue
			}
			mp := mstr(mmap(rs, "models"), slot)
			if mp == "" {
				continue
			}
			rt := roles[rn].tenant
			chain := []string{mp}
			for _, pr := range variants(rt, mp) {
				chain = append(chain, mstrs(pr, "fallback")...)
			}
			for _, p := range chain {
				for _, ps := range variants(rt, p) {
					out, ok1 := mnum(ps, "max_output_tokens")
					price, ok2 := mnum(mmap(ps, "price"), "output")
					if ok1 && ok2 && out*price/1e6 > worst {
						worst = out * price / 1e6
					}
				}
			}
		}
		if worst >= 0 && budget < worst {
			f.add(o, "tree_budget_below_call", "/tree_budget_eur")
		}
	}
	// Commission seats within the reviewer ceiling (GOV-3) and the commission manifest
	// (COM-10, COM-13).
	if ceil, ok := mnum(r.roleProfiles["reviewer"], "max_rank"); ok {
		for _, o := range S.each("Governance") {
			com := mmap(o.spec, "commission")
			ps := []string{mstr(com, "model_profile")}
			for _, s := range mlist(com, "seats") {
				sm, _ := s.(map[string]any)
				ps = append(ps, mstr(sm, "model_profile"))
			}
			for _, p := range ps {
				if pr := profile(o.tenant, p); pr != nil {
					if rank, _ := mnum(pr, "rank"); rank > ceil {
						f.add(o, "rank_exceeded", "/commission")
					}
				}
			}
		}
	}
	for _, o := range S.each("Commission") {
		if sh, ok := o.spec["shadow"].(map[string]any); ok {
			for _, k := range []string{"veto_lenses", "other_lenses"} {
				if n, ok := mnum(sh, k); ok && n == 0 {
					f.add(o, "shadow_zero_without_off", "/shadow/"+k)
				}
			}
		}
		esc := mmap(o.spec, "escalation")
		for k, floor := range map[string]float64{"forward_if_score_gte": 2, "untrusted_forward_if_score_gte": 3} {
			if n, ok := mnum(esc, k); ok && n < floor {
				f.add(o, "escalation_threshold_lowered", "/escalation/"+k)
			}
		}
	}
	// Driver configuration holds no envelope fields (MAN-4).
	if forbid, err := c.driverConfigForbidden(); err == nil {
		for _, kind := range []string{"Channel", "Sink"} {
			for _, o := range S.each(kind) {
				walkNodes(o.spec["config"], func(n map[string]any) {
					for k := range n {
						if forbid[k] {
							f.add(o, "driver_config_forbidden", "/config")
						}
					}
				})
			}
		}
	} else {
		return nil, err
	}
	// Retention periods of a tenant (RET-9).
	for _, o := range S.each("Tenant") {
		ret := mmap(o.spec, "retention")
		periods := map[string]int64{}
		for _, cls := range slices.Sorted(mapKeys(mmap(ret, "classes"))) {
			e := r.retention[cls]
			secs := durSeconds(mmap(ret, "classes")[cls])
			ceilDays, hasCeil := mnum(e, "ceiling_days")
			minDays, ok := mnum(e, "min_days")
			if !ok {
				minDays = 1
			}
			switch {
			case !hasCeil:
				f.add(o, "retention_period", "/retention/classes/"+cls)
			case secs < 0 || float64(secs) < minDays*86400 || float64(secs) > ceilDays*86400:
				f.add(o, "retention_period", "/retention/classes/"+cls)
			default:
				periods[cls] = secs
			}
		}
		content, ok := periods["content"]
		if !ok {
			d, ok := mnum(mmap(r.retention["content"], "default"), "days")
			if !ok {
				d = 30
			}
			content = int64(d * 86400)
		}
		for _, k := range []string{"files", "journals"} {
			if _, ok := ret[k]; ok {
				if s := durSeconds(ret[k]); s >= 0 && s > content {
					f.add(o, "retention_period", "/retention/"+k)
				}
			}
		}
	}
	// Global manifests carry no tenant data (TEN-6).
	for _, kind := range sortedKinds(S) {
		if mstr(r.kinds[kind], "scope") != "global" {
			continue
		}
		for _, o := range S.each(kind) {
			bad := false
			allStrings(o.spec, func(s string) {
				if tenants[s] || reAddrPerson.MatchString(s) || reEmail.MatchString(s) {
					bad = true
				}
			})
			if bad {
				f.add(o, "global_tenant_data", "")
			}
		}
	}
	sort.SliceStable(f.out, func(i, j int) bool { return f.out[i].Path < f.out[j].Path })
	return f.out, nil
}

// lintWarn logs a finding that does not fail the set. A reference of the tenant manifest to a
// type, an action, a group or a model profile outside the set is one: the tenant manifest
// names the whole tenant while a deployment applies it by stages (the hello-bot sequence
// applies the demo tenant with one type), and at run time a missing object is refused where
// it is used.
func (c *Core) lintWarn(o lintObj, code, path string) {
	c.log.Warn("apply: a reference of the tenant manifest to an object outside the set", "code", code, "tenant", o.name, "path", path)
}

func sortedKinds(S *lintSet) []string { return slices.Sorted(mapKeys(S.byKind)) }

func mapKeys[K comparable, V any](m map[K]V) func(func(K) bool) {
	return func(yield func(K) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

// resolveSchema turns a closed manifest schema into a checkable one: enum_from from the
// tenant catalog, the format from the registry, as the skeleton lint does.
func resolveSchema(schema map[string]any, lists map[string][]string, r *lintRegs) map[string]any {
	var res func(v any) any
	res = func(v any) any {
		switch x := v.(type) {
		case []any:
			out := make([]any, len(x))
			for i, e := range x {
				out[i] = res(e)
			}
			return out
		case map[string]any:
			out := map[string]any{}
			for k, e := range x {
				switch {
				case k == "enum_from":
					s, _ := e.(string)
					src, name, _ := strings.Cut(s, ".")
					if vals, ok := lists[name]; src == "catalog" && lists != nil && ok {
						l := make([]any, len(vals))
						for i, v := range vals {
							l[i] = v
						}
						out["enum"] = l
					} else {
						out["type"] = "string"
					}
				case k == "x-botaru-format":
					if s, _ := e.(string); r.formats[s] != nil {
						out["pattern"] = r.formats[s].String()
					}
				case k == "properties" || k == "$defs":
					m, _ := e.(map[string]any)
					pm := map[string]any{}
					for pk, pv := range m {
						pm[pk] = res(pv)
					}
					out[k] = pm
				case k == "items" || k == "additionalProperties" || k == "anyOf" || k == "oneOf" || k == "allOf":
					out[k] = res(e)
				case strings.HasPrefix(k, "x-botaru-") || k == "default":
				default:
					out[k] = e
				}
			}
			return out
		}
		return v
	}
	if schema == nil {
		return map[string]any{}
	}
	return res(schema).(map[string]any)
}

// driverConfigForbidden reads the envelope fields that a driver configuration never holds
// (the forbidden list of the driver-config meta-schema, MAN-4).
func (c *Core) driverConfigForbidden() (map[string]bool, error) {
	out := map[string]bool{}
	raw, err := c.spec.Raw("urn:botaru:schema:driver-config:1")
	if err != nil {
		return out, nil
	}
	defs, _ := raw["$defs"].(map[string]any)
	fb, _ := defs["forbidden"].(map[string]any)
	for _, v := range mlist(fb, "enum") {
		if s, ok := v.(string); ok {
			out[s] = true
		}
	}
	return out, nil
}

// claimsProfile says whether this core claims a conformance profile (core, and monitor when
// the settings turn it on); the checks of the manager seat (MGR-20) and of monitoring as a
// recipient (MON-1, D-88) run only under their profiles.
func (c *Core) claimsProfile(name string) bool { return slices.Contains(c.profiles(), name) }

// lintEgress checks a plugin's network list (INJ-6): every address is a machine or a resource
// of a tenant catalog of the set; the address monitor-api of the monitor plugin comes from
// the deployment description.
func (c *Core) lintEgress(f *lintFindings, o lintObj, catalogs map[string]map[string][]string) {
	known := map[string]bool{}
	for _, lists := range catalogs {
		for _, h := range lists["hosts"] {
			known[h] = true
		}
	}
	for j, a := range mstrs(mmap(o.spec, "network"), "egress") {
		if a == "monitor-api" && o.name == "monitor" {
			continue
		}
		if !known[a] {
			f.add(o, "egress_unknown", fmt.Sprintf("/network/egress/%d", j))
		}
	}
}

// lintDispatch checks the dispatcher rules of a tenant beyond the schema, which already closes
// the match to the type name, the type prefix and the source kind (DSP-3, FEN-3): the rule's
// bot group exists and its selector meets the roles that take the type, and the attempt
// settings stay within the type's bounds (an attempt period of at most twice the type's and not
// beyond its deadline, fewer attempts than the type has and at least one).
func (c *Core) lintDispatch(f *lintFindings, o lintObj, types map[string]map[string]any, roles map[string]lintObj, S *lintSet) {
	for j, rv := range mlist(mmap(o.spec, "dispatch"), "rules") {
		rule, _ := rv.(map[string]any)
		path := fmt.Sprintf("/dispatch/rules/%d", j)
		match := mmap(rule, "match")
		var matched []string
		for _, tn := range slices.Sorted(mapKeys(types)) {
			if tn == mstr(match, "type") || (mstr(match, "type_prefix") != "" && strings.HasPrefix(tn, mstr(match, "type_prefix"))) {
				matched = append(matched, tn)
			}
		}
		takers := map[string]bool{}
		for rn, ro := range roles {
			for _, tn := range matched {
				if slices.Contains(mstrs(ro.spec, "claims"), tn) {
					takers[rn] = true
				}
			}
		}
		if g := mstr(rule, "group"); g != "" {
			bg := S.get("BotGroup", g, o.name)
			if bg == nil {
				f.add(o, "ref_missing", path+"/group")
			} else if sel := mstrs(mmap(bg, "select"), "roles"); len(sel) > 0 && len(matched) > 0 {
				meets := false
				for _, rn := range sel {
					if takers[rn] {
						meets = true
					}
				}
				if !meets {
					f.add(o, "dispatch_group_disjoint", path+"/group")
				}
			}
		}
		for _, tn := range matched {
			tt := types[tn]
			if a := durSeconds(mmap(rule, "timeouts")["attempt"]); a >= 0 {
				ta := durSeconds(mmap(tt, "timeouts")["attempt"])
				dl := durSeconds(mmap(tt, "timeouts")["deadline"])
				if (ta >= 0 && a > 2*ta) || (dl >= 0 && a > dl) {
					f.add(o, "dispatch_attempt_bounds", path+"/timeouts/attempt")
				}
			}
			if n, ok := mnum(mmap(rule, "retry"), "max_attempts"); ok {
				tmax, has := mnum(mmap(tt, "retry"), "max_attempts")
				if !has {
					tmax = 2 // the farm default (README, Defaults)
				}
				if n < 1 || n >= tmax {
					f.add(o, "dispatch_attempt_bounds", path+"/retry/max_attempts")
				}
			}
		}
	}
}

// mapSlice applies fn to every element of in.
func mapSlice[T, U any](in []T, fn func(T) U) []U {
	out := make([]U, 0, len(in))
	for _, v := range in {
		out = append(out, fn(v))
	}
	return out
}

// knownZone says whether an IANA time zone name exists in the zone database (TIM-3).
func knownZone(name string) bool {
	if name == "Local" {
		return false
	}
	_, err := time.LoadLocation(name)
	return err == nil
}

// readByCode says whether a schema has a field marked x-botaru-read-by: code.
func readByCode(v any) bool {
	switch x := v.(type) {
	case map[string]any:
		if x["x-botaru-read-by"] == "code" {
			return true
		}
		for _, e := range x {
			if readByCode(e) {
				return true
			}
		}
	case []any:
		for _, e := range x {
			if readByCode(e) {
				return true
			}
		}
	}
	return false
}

var reHumanValue = regexp.MustCompile(`^human(:|$)`)

// namesHuman says whether a schema fixes a value naming a human principal or the kind human
// in a const or an enum (DLV-14).
func namesHuman(v any) bool {
	switch x := v.(type) {
	case map[string]any:
		if s, ok := x["const"].(string); ok && reHumanValue.MatchString(s) {
			return true
		}
		if e, ok := x["enum"].([]any); ok {
			for _, s := range e {
				if str, ok := s.(string); ok && reHumanValue.MatchString(str) {
					return true
				}
			}
		}
		for _, e := range x {
			if namesHuman(e) {
				return true
			}
		}
	case []any:
		for _, e := range x {
			if namesHuman(e) {
				return true
			}
		}
	}
	return false
}
