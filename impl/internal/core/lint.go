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
	"github.com/HunterEwok/botaru/impl/internal/money"
	"github.com/HunterEwok/botaru/impl/internal/spec"
)

// The set lint of apply: the checks of ci/check_manifests.py that span objects of the set and
// that the schemas cannot express, with its stable codes (ci/lint_codes.py). The lint runs on
// the whole set after every object passed its schema, so a case gets the same codes from apply
// as from the skeleton lint over the files in git.

// builtinCommands are the channel commands that a tenant's commands cannot take (PRM-17).
var builtinCommands = map[string]bool{"/help": true, "/about": true, "/lang": true, "/mydata": true, "/tasks": true, "/status": true,
	"/cancel": true, "/me": true, "/team": true, "/run": true, "/service": true, "/forget": true, "/grant": true, "/ungrant": true, "/incident": true, "/manager": true, "/prefs": true}

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

// refuses says whether the findings already hold an error of the object.
func (f *lintFindings) refuses(o lintObj) bool {
	for _, e := range f.out {
		if e.Kind == o.kind && e.Name == o.name && (o.kind == "Tenant" || e.Tenant == o.tenant) {
			return true
		}
	}
	return false
}

// addDetail is add with a detail text on the error.
func (f *lintFindings) addDetail(o lintObj, code, path, detail string) {
	n := len(f.out)
	f.add(o, code, path)
	if len(f.out) > n {
		f.out[len(f.out)-1].Detail = detail
	}
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
	lenses       map[string]map[string]any
	purposes     map[string]bool
	forbidden    map[string]bool
	kinds        map[string]map[string]any
	retention    map[string]map[string]any
	driverForbid map[string]bool
}

func (c *Core) lintRegistries() (*lintRegs, error) {
	r := &lintRegs{formats: map[string]*regexp.Regexp{}, capForms: map[string]map[string]any{}, dataClasses: map[string]map[string]any{},
		roleProfiles: map[string]map[string]any{}, lenses: map[string]map[string]any{}, purposes: map[string]bool{}, kinds: map[string]map[string]any{},
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
	if err := load("spec/model/lenses.yaml", func(id string, e map[string]any) {
		if e["since"] == "v0.1" && e["subject"] == "proposal" {
			r.lenses[id] = e
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

// classRefused says whether a data class is refused where a manifest sets one: unknown or
// reserved (pii), and for an output class one that is never an output (secret) (PRM-5).
func (r *lintRegs) classRefused(cls string, output bool) bool {
	dc, ok := r.dataClasses[cls]
	return !ok || truthy(dc["reserved"]) || output && dc["output"] == false
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
		// A capability whose form has a fixed argument, such as governance.confirm:commission.
		if e, ok := r.capForms[base]; ok && fmt.Sprint(e["form"]) == cp {
			return base, arg
		}
	}
	if _, ok := r.capForms[cp]; ok {
		return cp, ""
	}
	return "", ""
}

// lintManifests runs the set lint (MAN-6: an error fails the whole set).
// personaLookup gives the text of a persona of a tenant by its hash, when apply has it.
type personaLookup func(tenant, sha string) (string, bool)

func (c *Core) lintManifests(docs []manifest.Doc, index []int, personas personaLookup, prior []manifest.Doc) ([]ApplyError, error) {
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
			lists["monitor_items"] = append(lists["monitor_items"], mstr(tm, "item"))
		}
		for j, res := range mlist(o.spec, "resources") {
			rm, _ := res.(map[string]any)
			path := fmt.Sprintf("/resources/%d", j)
			// Class pii is reserved in the catalog (PRM-5).
			if dc := mstr(rm, "data_class"); dc != "" && r.classRefused(dc, false) {
				f.add(o, "cap_data_class", path+"/data_class")
			}
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
	var farmManager map[string]any
	for _, o := range S.each("Governance") {
		if o.tenant == "farm" {
			farmManager = mmap(o.spec, "farm_manager")
			if mp := mstr(farmManager, "model_profile"); mp != "" {
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
		// The takeover card is never lifted: confirm_above_eur is reserved (MGR-21).
		if v, ok := mnum(mmap(ts, "takeover"), "confirm_above_eur"); ok && v > 0 {
			f.add(o, "takeover_confirm_threshold", "/takeover/confirm_above_eur")
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
			} else if types[t] != nil && len(mlist(types[t], "summary_fields")) == 0 && groupDefault(dt) {
				// A type for group chats carries a code table in its summary (REP-11, DLV-15).
				f.add(o, "summary_fields_required", path)
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
			// The day of the line holds three readings of one worst-case incident at the seat
			// profile's prices (MGR-20).
			for _, ptr := range managerLineShort(ts, farmManager, profile(o.name, seatProfile)) {
				f.add(o, "manager_line_too_small", ptr)
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
		// No continuation starts a farm manager task (MGR-4).
		for _, path := range [][]string{{"continuation"}, {"await", "then"}, {"await", "on_timeout"}} {
			m := ts
			for _, k := range path {
				m = mmap(m, k)
			}
			if tt := S.get("TaskType", mstr(m, "type"), ""); tt != nil && mstr(tt, "purpose") == "manage" {
				f.add(o, "manage_enqueued", "/"+strings.Join(path, "/")+"/type")
			}
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
		// Class pii is reserved in types (PRM-5).
		if dc := mstr(ts, "data_class"); dc != "" && r.classRefused(dc, false) {
			f.add(o, "cap_data_class", "/data_class")
		}
		if mstr(mmap(ts, "run"), "approval") == "none" {
			// Without a card a pipeline changes nothing on work machines: level 3 runs only
			// with a card, under APR-18 or as a routine action (APR-17).
			if n, _ := mnum(ts, "tier"); n >= 3 {
				f.add(o, "run_not_readonly", "/tier")
			}
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
			// secret is never an output class, and pii is reserved in tools (PRM-5).
			if oc := mstr(tm, "output_class"); oc != "" && r.classRefused(oc, true) {
				f.add(o, "cap_data_class", path+"/output_class")
			}
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

	c.lintTwoApprovers(f, S)

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
			// An insight type is claimed only with the role's insight section naming it (INS-1).
			if mstr(ts, "purpose") == "insight" && !slices.Contains(mstrs(mmap(rs, "insight"), "types"), tn) {
				f.add(o, "insight_type_invalid", path)
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
		// The insight section: insight types of tier 1 at most that the role claims, without
		// their own result_schema; tools among the role's own; a merge window of at least an
		// hour (INS-1, INS-2, INS-4).
		ins := mmap(rs, "insight")
		for j, tn := range mstrs(ins, "types") {
			ts := types[tn]
			path := fmt.Sprintf("/insight/types/%d", j)
			if ts == nil {
				if len(types) > 0 {
					f.add(o, "ref_missing", path)
				}
				continue
			}
			tier, _ := mnum(ts, "tier")
			// The type's class is not above max_data, so the attempt's mark stays within it (INS-2).
			wider := ins["max_data"] != nil && order(mstr(ts, "data_class")) > order(mstr(ins, "max_data"))
			if mstr(ts, "purpose") != "insight" || !slices.Contains(mstrs(rs, "claims"), tn) || tier > 1 || ts["result_schema"] != nil || wider {
				f.add(o, "insight_type_invalid", path)
			}
		}
		own := mstrs(rs, "tools")
		for j, t := range mstrs(ins, "tools") {
			plugin, _, _ := strings.Cut(t, "/")
			if !slices.Contains(own, t) && !slices.Contains(own, plugin) {
				f.add(o, "insight_wider", fmt.Sprintf("/insight/tools/%d", j))
			}
		}
		if mw, ok := ins["merge_window"]; ok {
			if d, ok := parseDuration(asString(mw)); !ok || d < time.Hour {
				f.add(o, "insight_merge_window", "/insight/merge_window")
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
			// Only code, the review schedule and people direct the farm manager (MGR-4).
			if tt := S.get("TaskType", tn, ""); tt != nil && mstr(tt, "purpose") == "manage" {
				f.add(o, "manage_enqueued", fmt.Sprintf("/may_enqueue/%d", j))
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
	// The message graph (MSG-5, MSG-14, MSG-16, PRF-1, INJ-7, BGR-1).
	msgTypes := map[string]map[string]any{}
	for _, o := range S.each("MessageType") {
		msgTypes[o.name] = o.spec
		if d, ok := parseDuration(mstr(o.spec, "ttl")); ok && d > reportTTLMax {
			f.add(o, "message_ceiling", "/ttl")
		}
		if d, ok := parseDuration(mstr(o.spec, "stale_after")); ok && d > stateStaleMax {
			f.add(o, "message_ceiling", "/stale_after")
		}
		if n, ok := mnum(o.spec, "max_bytes"); ok && n > maxFieldsBytes {
			f.add(o, "message_ceiling", "/max_bytes")
		}
		if n, ok := mnum(mmap(o.spec, "note"), "max_chars"); ok && n > maxNoteChars {
			f.add(o, "message_ceiling", "/note/max_chars")
		}
	}
	receives := func(ro lintObj, typ string) bool {
		for _, e := range mlist(mmap(ro.spec, "messages"), "receive") {
			if em, _ := e.(map[string]any); mstr(em, "type") == typ {
				return true
			}
		}
		return false
	}
	instanceInbox := func(ro lintObj) bool {
		if v, ok := ro.spec["instance_inbox"].(bool); ok {
			return v
		}
		wk := mstr(ro.spec, "worker_kind")
		return wk != "" && wk != "agent"
	}
	// addrRoles are the roles an inbox address names: the role, or the roles of a bot group of
	// the tenant (any tenant for a global object).
	addrRoles := func(tenant, addr string) ([]string, bool) {
		kind, name, _ := strings.Cut(addr, ":")
		switch kind {
		case "role":
			_, ok := roles[name]
			return []string{name}, ok
		case "group":
			bg := S.get("BotGroup", name, tenant)
			if bg == nil {
				return nil, false
			}
			return mstrs(mmap(bg, "select"), "roles"), true
		}
		return nil, false
	}
	// checkPair checks a type sent to an address: a receiving role, no receiver of a profile
	// that takes no messages, an instance inbox for an all delivery (MSG-5, MSG-14).
	checkPair := func(o lintObj, path, typ, addr string) {
		mt := msgTypes[typ]
		if mt == nil {
			if len(msgTypes) > 0 {
				f.add(o, "ref_missing", path)
			}
			return
		}
		named, ok := addrRoles(o.tenant, addr)
		if !ok {
			if len(roles) > 0 {
				f.add(o, "ref_missing", path)
			}
			return
		}
		var takers []lintObj
		for _, rn := range named {
			ro, ok := roles[rn]
			if !ok {
				continue
			}
			if noMessageProfiles[mstr(ro.spec, "profile")] {
				f.add(o, "message_receiver_profile", path)
			}
			if receives(ro, typ) {
				takers = append(takers, ro)
			}
		}
		if len(takers) == 0 && len(roles) > 0 {
			f.add(o, "message_no_receiver", path)
		}
		if mstr(mt, "delivery") == "all" && len(takers) > 0 && !slices.ContainsFunc(takers, instanceInbox) {
			f.add(o, "message_all_instance_inbox", path)
		}
	}
	for _, rn := range roleNames {
		o := roles[rn]
		msgs := mmap(o.spec, "messages")
		profile := mstr(o.spec, "profile")
		if noMessageProfiles[profile] && len(mlist(msgs, "receive")) > 0 {
			f.add(o, "message_receiver_profile", "/messages/receive")
		}
		if profile == "reporter" {
			continue // its sends are refused by its profile (PRF-1)
		}
		// The kind of sender the role is: the model of an agent role, the code of a job (MSG-5).
		producer := "job"
		if mstr(o.spec, "worker_kind") == "agent" {
			producer = "agent"
		}
		for i, e := range mlist(msgs, "send") {
			em, _ := e.(map[string]any)
			if mt := msgTypes[mstr(em, "type")]; mt != nil && !slices.Contains(mstrs(mt, "producers"), producer) {
				f.add(o, "message_producer", fmt.Sprintf("/messages/send/%d/type", i))
			}
			for j, to := range mstrs(em, "to") {
				checkPair(o, fmt.Sprintf("/messages/send/%d/to/%d", i, j), mstr(em, "type"), to)
			}
		}
	}
	// A type reads only kinds that every role taking it receives (MSG-8).
	for _, rn := range roleNames {
		o := roles[rn]
		for _, tn := range mstrs(o.spec, "claims") {
			for _, typ := range mstrs(mmap(types[tn], "inbox"), "read") {
				if !receives(o, typ) {
					f.add(o, "inbox_read_not_received", "/claims")
				}
			}
		}
	}
	// An alarm type takes nothing from inboxes, so no route hands it model text (INJ-7).
	for _, o := range S.each("TaskType") {
		if strings.HasPrefix(o.name, "alarm.") && len(mlist(mmap(o.spec, "inbox"), "read")) > 0 {
			f.add(o, "alarm_inbox_read", "/inbox/read")
		}
	}
	for _, o := range S.each("Route") {
		on := mmap(o.spec, "on")
		mode := mstr(o.spec, "mode")
		if mode == "" {
			mode = "task"
		}
		// inbox and event only with task, message only with notify, task_done only with
		// report (MSG-16).
		want := "task"
		switch {
		case mstr(on, "message") != "":
			want = "notify"
		case mstr(on, "task_done") != "":
			want = "report"
		}
		if mode != want {
			f.add(o, "route_mode", "/mode")
		}
		if in := mstr(on, "inbox"); in != "" {
			if _, ok := addrRoles(o.tenant, in); !ok && len(roles) > 0 {
				f.add(o, "ref_missing", "/on/inbox")
			}
		}
		if typ := mstr(on, "message"); typ != "" {
			mt := msgTypes[typ]
			switch {
			case mt == nil && len(msgTypes) > 0:
				f.add(o, "ref_missing", "/on/message")
			case mt != nil && !slices.ContainsFunc(mstrs(mt, "producers"), func(p string) bool { return p != "agent" }):
				// A notice goes only from entries the code wrote, never from the model's.
				f.add(o, "route_untrusted_source", "/on/message")
			}
			if tpl := mstr(o.spec, "template"); tpl == "" || c.texts != nil && !c.texts.Has("route."+tpl) {
				f.add(o, "ref_missing", "/template")
			}
		}
		if tt := mstr(on, "task_done"); tt != "" {
			if types[tt] == nil && len(types) > 0 {
				f.add(o, "ref_missing", "/on/task_done")
			}
			rep := mmap(o.spec, "report")
			if mt := msgTypes[mstr(rep, "type")]; mt != nil && (mstr(mt, "kind") != "report" || !slices.Contains(mstrs(mt, "producers"), "core")) {
				f.add(o, "route_report_producer", "/report/type")
			}
			if rep != nil {
				checkPair(o, "/report", mstr(rep, "type"), mstr(rep, "to"))
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
	// Every veto lens has an executor: a seat of the lens whose fallback role claims the lens
	// review type (COM-7). A lens review answers in at most 2000 output tokens (COM-6).
	for _, o := range S.each("Governance") {
		if _, ok := o.spec["commission"]; !ok || len(roles) == 0 {
			continue
		}
		seats := mlist(mmap(o.spec, "commission"), "seats")
		// The analysis profiles of the roles that can write proposals: they claim a type of
		// purpose insight or aggregate (COM-2).
		analysis := map[string]bool{}
		for _, ro := range roles {
			author := slices.ContainsFunc(mstrs(ro.spec, "claims"), func(t string) bool {
				p := mstr(types[t], "purpose")
				return p == "insight" || p == "aggregate"
			})
			if a := mstr(mmap(ro.spec, "models"), "analysis"); a != "" && author {
				analysis[a] = true
			}
		}
		for _, id := range slices.Sorted(mapKeys(r.lenses)) {
			l := r.lenses[id]
			if l["veto"] != true {
				continue
			}
			seated := slices.ContainsFunc(seats, func(s any) bool {
				sm, _ := s.(map[string]any)
				return mstr(sm, "lens") == id
			})
			covered := slices.ContainsFunc(seats, func(s any) bool {
				sm, _ := s.(map[string]any)
				fr, ok := roles[mstr(sm, "fallback_role")]
				return mstr(sm, "lens") == id && ok && slices.Contains(mstrs(fr.spec, "claims"), mstr(l, "review_type"))
			})
			// A lens without a seat goes to a role of profile reviewer that claims its type and
			// does not run on an author's analysis profile (COM-2, COM-8).
			if !seated {
				for _, ro := range roles {
					if mstr(ro.spec, "profile") == "reviewer" && slices.Contains(mstrs(ro.spec, "claims"), mstr(l, "review_type")) &&
						!analysis[mstr(mmap(ro.spec, "models"), "base")] {
						covered = true
					}
				}
			}
			if !covered {
				f.add(o, "veto_lens_no_executor", "/commission/seats")
			}
		}
		// A seat's fallback role does not run on the analysis profile of a role that writes
		// proposals (COM-2).
		for _, s := range seats {
			sm, _ := s.(map[string]any)
			if fr, ok := roles[mstr(sm, "fallback_role")]; ok && analysis[mstr(mmap(fr.spec, "models"), "base")] {
				f.add(o, "reviewer_not_independent", "/commission/seats")
			}
		}
	}
	for _, rn := range slices.Sorted(mapKeys(roles)) {
		ro := roles[rn]
		review := slices.ContainsFunc(mstrs(ro.spec, "claims"), func(tn string) bool { return types[tn] != nil && mstr(types[tn], "purpose") == "review" })
		if !review {
			continue
		}
		for _, ps := range variants(ro.tenant, mstr(mmap(ro.spec, "models"), "base")) {
			if out, ok := mnum(ps, "max_output_tokens"); ok && out > 2000 {
				f.add(ro, "lens_profile_output", "/models/base")
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
	// Global manifests carry no tenant data (TEN-6). The service tenant farm is also a word of
	// the language (a subject kind, a hold scope).
	for _, kind := range sortedKinds(S) {
		if mstr(r.kinds[kind], "scope") != "global" {
			continue
		}
		for _, o := range S.each(kind) {
			bad := false
			allStrings(o.spec, func(s string) {
				if (tenants[s] && s != farmTenant) || reAddrPerson.MatchString(s) || reEmail.MatchString(s) {
					bad = true
				}
			})
			if bad {
				f.add(o, "global_tenant_data", "")
			}
		}
	}
	// Each insight type is in the insight section of exactly one role (INS-1, INS-4).
	for _, o := range S.each("TaskType") {
		if mstr(o.spec, "purpose") != "insight" {
			continue
		}
		n := 0
		for _, rn := range roleNames {
			if slices.Contains(mstrs(mmap(roles[rn].spec, "insight"), "types"), o.name) {
				n++
			}
		}
		if n != 1 {
			f.add(o, "insight_type_invalid", "/purpose")
		}
	}
	// Observation series: the source fits its kind, the data class is not below the source
	// tool's output_class, rollups come from spec/model/rollups.yaml, and a trial guard of a
	// tenant names only a series with a complete outcome block (OBV-2, OBV-4, OBV-6, OBV-7).
	rollups := map[string]bool{}
	coreOutcome := map[string]map[string]any{}
	if reg, err := c.spec.Registry("spec/model/rollups.yaml"); err == nil {
		for _, e := range reg.Entries {
			rollups[asString(e["id"])] = true
		}
	}
	if reg, err := c.spec.Registry("spec/model/core-series.yaml"); err == nil {
		for _, e := range reg.Entries {
			o, _ := e["outcome"].(map[string]any)
			coreOutcome[asString(e["id"])] = o
		}
	}
	tenantSeriesOutcome := map[[2]string]map[string]any{}
	for _, o := range S.each("ObservationSeries") {
		tenantSeriesOutcome[[2]string{o.tenant, o.name}] = mmap(o.spec, "outcome")
		if _, core := coreOutcome[o.name]; core || strings.HasPrefix(o.name, "farm.") {
			f.add(o, "series_name_reserved", "/metadata/name")
		}
		if dc, ok := r.dataClasses[mstr(o.spec, "data_class")]; !ok || truthy(dc["reserved"]) {
			f.add(o, "cap_data_class", "/data_class")
		}
		src := mmap(o.spec, "source")
		switch kind, tool, field := mstr(src, "kind"), mstr(src, "tool"), mstr(src, "field"); {
		case kind == "tool" && (tool == "" || field == ""):
			f.add(o, "series_source_invalid", "/source")
		case kind == "tool":
			if tr, ok := tools[tool]; !ok {
				if len(S.each("ToolPlugin")) > 0 {
					f.add(o, "ref_missing", "/source/tool")
				}
			} else if order(mstr(o.spec, "data_class")) < order(mstr(tr.t, "output_class")) {
				f.add(o, "series_class_below_source", "/data_class")
			}
		case tool != "", kind == "job" && field == "":
			f.add(o, "series_source_invalid", "/source")
		}
		for j, r := range mstrs(o.spec, "rollups") {
			if !rollups[r] {
				f.add(o, "rollup_unknown", fmt.Sprintf("/rollups/%d", j))
			}
		}
		if r := mstr(mmap(o.spec, "outcome"), "compare"); r != "" && !rollups[r] {
			f.add(o, "rollup_unknown", "/outcome/compare")
		}
	}
	for _, o := range S.each("Tenant") {
		for j, lv := range mlist(mmap(o.spec, "insights"), "extra_guards") {
			line, _ := lv.(map[string]any)
			for _, n := range append([]string{mstr(line, "metric")}, mstrs(line, "guards")...) {
				out, isCore := coreOutcome[n]
				if !isCore {
					out = tenantSeriesOutcome[[2]string{o.name, n}]
				}
				if out == nil || !decodeOutcome(out).Complete {
					f.add(o, "guard_not_metric", fmt.Sprintf("/insights/extra_guards/%d", j))
				}
			}
		}
	}
	// Source channels and routes on events (ING-1, ING-3, ING-4, INJ-7).
	c.lintIngress(S, f)
	lintPrivilegeRaised(S, f, docs, prior)
	if err := c.lintLensNames(S, f); err != nil {
		return nil, err
	}
	if err := c.lintGovernance(S, f, personas); err != nil {
		return nil, err
	}
	if err := c.lintIncidentClasses(S, f); err != nil {
		return nil, err
	}
	if err := c.lintSeatProfiles(S, f); err != nil {
		return nil, err
	}
	c.lintData(S, f)
	c.lintKnowledge(S, f)
	// The translator gate of a production tenant (TRN-6).
	if err := lintTranslatorEvals(docs, S, prior, f); err != nil {
		return nil, err
	}
	// The provider workspace ceilings and the review line (GW-9, BUD-13).
	c.lintWorkspaceCeilings(S, f)
	c.lintReviewLine(docs, S, f)
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

// governanceName says whether a name is an event or operation of governance, attribution or
// seats, which no bot reaches (GOV-6, GOV-29); with incidents, also of incident records, which
// no bot creates or changes (GOV-13).
func governanceName(n string, incidents bool) bool {
	for _, p := range []string{"governance.", "attribution.", "seat."} {
		if strings.HasPrefix(n, p) {
			return true
		}
	}
	return incidents && strings.HasPrefix(n, "incident.")
}

// charterFloors are the durations of a tenant charter that only rise from their default, and
// charterCeilings the counts that only fall from it (GOV-33). The schema bounds the counts and
// the audit sample; the durations are strings it cannot compare.
var charterFloors = []struct {
	block, field string
	def          time.Duration
}{
	{"promotion", "probation", govProbation}, {"promotion", "min_tenure", govMinTenure}, {"promotion", "swap_cooldown", govSwapCooldown},
	{"removal", "protect_new", 60 * 24 * time.Hour}, {"attribution", "window", defaultAttributionWindow},
	{"confirmation", "ttl", 7 * 24 * time.Hour}, {"identity", "unbound_ttl", defaultUnboundTTL}, {"memory", "seal_ttl", defaultSealTTL},
}

// lintCharterBounds checks what the schema of a charter cannot (GOV-33): a seat id declared
// twice, the farm_manager block outside the farm charter, a duration below its default, the
// excluded change kinds without none, and in the farm charter a decide_within above 7 days.
// A core that claims v0.3 does not use incident_stop (MGR-55), so it does not check it.
func lintCharterBounds(o lintObj, f *lintFindings) {
	seen := map[string]bool{}
	for i, sv := range mlist(mmap(o.spec, "commission"), "seats") {
		sm, _ := sv.(map[string]any)
		id := fmt.Sprint(sm["id"])
		if seen[id] {
			f.add(o, "charter_seat_twice", fmt.Sprintf("/commission/seats/%d/id", i))
		}
		seen[id] = true
	}
	if o.tenant != farmTenant && o.spec["farm_manager"] != nil {
		f.add(o, "charter_farm_manager", "/farm_manager")
	}
	for _, fl := range charterFloors {
		v := mstr(mmap(o.spec, fl.block), fl.field)
		if d, ok := parseDuration(v); v != "" && (!ok || d < fl.def) {
			f.add(o, "charter_direction", "/"+fl.block+"/"+fl.field)
		}
	}
	if p := mmap(o.spec, "promotion"); p["exclude_change_kinds"] != nil && !slices.Contains(strList(p["exclude_change_kinds"]), "none") {
		f.add(o, "charter_direction", "/promotion/exclude_change_kinds")
	}
	v := mstr(mmap(mmap(o.spec, "farm_manager"), "incident_stop"), "decide_within")
	if d, ok := parseDuration(v); !claimsV03() && v != "" && (!ok || d > 7*24*time.Hour) {
		f.add(o, "charter_direction", "/farm_manager/incident_stop/decide_within")
	}
}

// lintPrivilegeRaised refuses a set in which an ordinary group became privileged (PRM-13): a
// group of the store that was not privileged is privileged after the set, by its own
// capabilities or by a type it names, whether or not it has members. A group new in the set
// is privileged or not from the start and is granted under PRM-13.
func lintPrivilegeRaised(S *lintSet, f *lintFindings, docs, prior []manifest.Doc) {
	if len(prior) == 0 {
		return
	}
	before, err := buildCatalog(prior)
	if err != nil {
		return
	}
	after, err := buildCatalog(docs)
	if err != nil {
		return
	}
	for _, o := range S.each("Group") {
		tb, ta := before.Tenants[o.tenant], after.Tenants[o.tenant]
		if tb == nil || ta == nil {
			continue
		}
		if _, ok := tb.Groups[o.name]; !ok {
			continue
		}
		if !grantsPrivilege(tb, before, o.name, map[string]bool{}) && grantsPrivilege(ta, after, o.name, map[string]bool{}) {
			f.add(o, "privileged_group", "/caps")
		}
	}
}

// grantsPrivilege applies the PRM-13 rule to what a group grants in one state of the set: a
// task capability counts by the types it covers there (a wildcard by prefix, capCovers), so a
// capability of a type that is not in the set grants nothing, unlike privilegedGroup, which
// counts anything unknown as privileged for a grant.
func grantsPrivilege(tv *TenantView, cat *Catalog, name string, seen map[string]bool) bool {
	if seen[name] {
		return false
	}
	seen[name] = true
	g, ok := tv.Groups[name]
	if !ok {
		return false
	}
	for _, cp := range g.Caps {
		if unprivilegedCap(cp) {
			continue
		}
		if !strings.HasPrefix(cp, "task:") {
			return true
		}
		for tn, tt := range cat.TaskTypes {
			if tt.Tier >= 1 && capCovers(cp, "task:"+tn) {
				return true
			}
		}
	}
	for _, d := range g.Data {
		if d != "summary" && d != "internal" {
			return true
		}
	}
	for _, in := range g.Inherits {
		if grantsPrivilege(tv, cat, in, seen) {
			return true
		}
	}
	return false
}

// lintLensNames checks the lenses that a charter seat and the commission manifest name: a lens
// of spec/model/lenses.yaml of the claimed version or a tenant lens x_<name> declared with its
// meaning in the tenant's charter, else lens_unknown (COM-19). The weight of a lens that has
// one by rule (farm_manager, from v0.2) is not set by the manifest (COM-8).
func (c *Core) lintLensNames(S *lintSet, f *lintFindings) error {
	reg, err := c.spec.Registry("spec/model/lenses.yaml")
	if err != nil {
		return err
	}
	skeleton := map[string]map[string]any{}
	for _, e := range reg.Entries {
		if !spec.VersionLater(asString(e["since"]), spec.Effective()) {
			skeleton[asString(e["id"])] = e
		}
	}
	known := func(tenant, lens string) bool {
		if _, ok := skeleton[lens]; ok {
			return true
		}
		if !strings.HasPrefix(lens, "x_") {
			return false
		}
		for _, ch := range S.each("Governance") {
			if ch.tenant == tenant && mstr(mmap(mmap(mmap(ch.spec, "commission"), "lenses"), lens), "meaning") != "" {
				return true
			}
		}
		return false
	}
	for _, o := range S.each("Governance") {
		for i, sv := range mlist(mmap(o.spec, "commission"), "seats") {
			sm, _ := sv.(map[string]any)
			if l := mstr(sm, "lens"); l != "" && !known(o.tenant, l) {
				f.add(o, "lens_unknown", fmt.Sprintf("/commission/seats/%d/lens", i))
			}
		}
	}
	for _, o := range S.each("Commission") {
		for _, k := range []string{"weights", "min"} {
			for _, l := range slices.Sorted(mapKeys(mmap(o.spec, k))) {
				switch {
				case !known(o.tenant, l):
					f.add(o, "lens_unknown", "/"+k+"/"+l)
				case k == "weights" && skeleton[l]["weight"] != nil:
					f.add(o, "lens_weight_fixed", "/weights/"+l)
				}
			}
		}
	}
	return nil
}

// lintGovernance checks the personas of the charters (GOV-4, COM-19), the unawareness of the
// texts bots read (GOV-29) and that nothing of a bot refers to governance (GOV-6, GOV-13).
func (c *Core) lintGovernance(S *lintSet, f *lintFindings, personas personaLookup) error {
	u, err := c.unawarenessTerms()
	if err != nil {
		return err
	}
	for _, o := range S.each("Governance") {
		com := mmap(o.spec, "commission")
		for i, sv := range mlist(com, "seats") {
			sm, _ := sv.(map[string]any)
			if l := mstr(sm, "lens"); l != "" && mstr(mmap(mmap(mmap(com, "lenses"), l), "persona"), "ref") == "" {
				f.add(o, "lens_persona_missing", fmt.Sprintf("/commission/seats/%d/lens", i))
			}
		}
		lintCharterBounds(o, f)
		// From v0.3 the size of a persona counts with its addenda (GOV-45).
		v03 := claimsV03()
		refs := charterPersonas(o.spec, v03)
		sizes := map[string]int{}
		for _, path := range slices.Sorted(mapKeys(refs)) {
			r := refs[path]
			if !r.pinned() {
				continue
			}
			text, ok := "", false
			if personas != nil {
				text, ok = personas(o.tenant, r.SHA256)
			}
			if !ok {
				// A charter of an earlier apply whose text the store lacks (pinned before the
				// core kept persona texts) runs without its persona until it is applied again
				// with the file; it does not block an apply of other objects.
				if o.i >= 0 {
					f.add(o, "persona_missing", path)
				}
				continue
			}
			position := "lens"
			if path == "/farm_manager/persona" {
				position = "farm_manager"
			} else if strings.HasPrefix(path, "/positions/") {
				position = strings.Split(path, "/")[2]
			}
			if !v03 {
				for _, code := range personaFindings(text, position, u) {
					f.add(o, code, path)
				}
				continue
			}
			kind := position
			if strings.Contains(path, "/addenda/") {
				kind = "addendum"
			}
			for _, code := range personaFindingsV03(text, kind, u) {
				f.add(o, code, path)
			}
			key := path
			if strings.HasPrefix(path, "/positions/") {
				key = "/positions/" + position
			}
			if sizes[key] += len(text); sizes[key] > 4096 {
				f.add(o, "persona_size", path)
			}
		}
	}
	for _, o := range S.each("TaskType") {
		if u.linked(mstr(o.spec, "instruction"), false) {
			f.add(o, "unaware_text", "/instruction")
		}
		for j, e := range mstrs(o.spec, "events") {
			if governanceName(e, true) {
				f.add(o, "governance_ref", fmt.Sprintf("/events/%d", j))
			}
		}
	}
	for _, o := range S.each("ToolPlugin") {
		for j, tv := range mlist(o.spec, "tools") {
			if tm, ok := tv.(map[string]any); ok && u.linked(mstr(tm, "description"), false) {
				f.add(o, "unaware_text", fmt.Sprintf("/tools/%d/description", j))
			}
		}
	}
	for _, o := range S.each("Role") {
		for j, a := range mstrs(o.spec, "actions_may_request") {
			if governanceName(a, true) {
				f.add(o, "governance_ref", fmt.Sprintf("/actions_may_request/%d", j))
			}
		}
		for j, t := range mstrs(o.spec, "may_enqueue") {
			if governanceName(t, true) {
				f.add(o, "governance_ref", fmt.Sprintf("/may_enqueue/%d", j))
			}
		}
	}
	for _, kind := range []string{"Action", "MessageType", "TaskType"} {
		for _, o := range S.each(kind) {
			if governanceName(o.name, true) {
				f.add(o, "governance_ref", "/metadata/name")
			}
		}
	}
	for _, o := range S.each("Route") {
		if governanceName(mstr(mmap(o.spec, "on"), "event"), false) {
			f.add(o, "governance_ref", "/on/event")
		}
	}
	return nil
}

// groupDefault says whether a default_types entry also serves group chats: a bare type name or
// an entry without chats serves every chat (REP-11).
func groupDefault(dt any) bool {
	m, ok := dt.(map[string]any)
	if !ok {
		return true
	}
	chats := mstrs(m, "chats")
	return len(chats) == 0 || slices.ContainsFunc(chats, func(ch string) bool { return ch == "group" || strings.HasPrefix(ch, "chat:") })
}

// lintTwoApprovers checks the actions of risk high in a production tenant (APR-16): one that a
// group of the tenant grants action.request for, or that a role lists in actions_may_request,
// needs approvals: 2 (high_risk_one_approver); with approvals: 2 and no group of the tenant
// granting action.approve the apply only notes approvers_fewer_than_two in its log, since the
// response has no notes, and doctor counts the people.
func (c *Core) lintTwoApprovers(f *lintFindings, S *lintSet) {
	for _, t := range S.each("Tenant") {
		if k := mstr(t.spec, "kind"); k != "" && k != "production" {
			continue
		}
		grants := func(cp string) bool {
			for _, g := range S.each("Group") {
				if g.tenant == t.name && slices.Contains(mstrs(g.spec, "caps"), cp) {
					return true
				}
			}
			return false
		}
		for _, a := range S.each("Action") {
			if mstr(a.spec, "risk") != "high" {
				continue
			}
			n, ok := mnum(a.spec, "approvals")
			if !ok {
				n = 1
			}
			requested := grants("action.request:" + a.name)
			if n == 1 {
				// A role of the tenant, or one without a tenant, that lists the action.
				for _, r := range S.each("Role") {
					if (r.tenant == "" || r.tenant == t.name) && slices.Contains(mstrs(r.spec, "actions_may_request"), a.name) {
						requested = true
					}
				}
				if requested {
					f.add(a, "high_risk_one_approver", "/approvals")
				}
			} else if !grants("action.approve:" + a.name) {
				// The note does not depend on who may request the action (APR-16).
				c.log.Warn("apply: no group grants action.approve for a high risk action with two approvers", "code", "approvers_fewer_than_two",
					"tenant", t.name, "action", a.name)
			}
		}
	}
}

// managerLineShort are the fields of a tenant's manager line that do not cover what the seat may
// spend: the day holds three worst-case readings of one incident at the seat profile's prices
// (MGR-20), from v0.3 also the daily review and the daily ceiling of the analyses, and the month
// the takeover ceiling (MGR-49).
func managerLineShort(ts, farmManager, seatProfile map[string]any) []string {
	var out []string
	line := mmap(mmap(mmap(ts, "budgets"), "lines"), "manager")
	if d, hasDay := mnum(line, "day_eur"); seatProfile != nil && hasDay {
		in, _ := mnum(mmap(seatProfile, "price"), "input")
		outPrice, _ := mnum(mmap(seatProfile, "price"), "output")
		maxOut, _ := mnum(seatProfile, "max_output_tokens")
		need := seatWorstReadMicros(in, outPrice, int(maxOut))
		if claimsV03() {
			need += float64(money.FromEUR(charterReviewMaxEUR(farmManager)) + money.FromEUR(postmortemDayEUR(farmManager)))
		}
		if d*1e6 < need {
			out = append(out, "/budgets/lines/manager/day_eur")
		}
	}
	if claimsV03() {
		tm := defaultTakeoverMonthEUR
		if v, ok := mnum(mmap(ts, "takeover"), "manager_month_eur"); ok {
			tm = v
		}
		if mm, ok := mnum(line, "month_eur"); ok && mm < tm {
			out = append(out, "/budgets/lines/manager/month_eur")
		}
	}
	return out
}
