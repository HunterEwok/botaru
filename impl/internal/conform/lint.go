// SPDX-License-Identifier: AGPL-3.0-only

package conform

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/HunterEwok/botaru/impl/internal/manifest"
	"github.com/HunterEwok/botaru/impl/internal/testbench"
	"github.com/HunterEwok/botaru/impl/internal/yamlx"
)

// Lint fragments (conformance/runner.md, verb lint): a row names a piece of one manifest, and
// the runner submits that manifest, completed from the example of its kind, to P2 apply with
// dry_run on top of the seed. A fragment is a mapping or a one-line flow mapping
// ('approver: action.approve:*'); a dotted key (run.approval) names a nested field. Before the
// row the runner applies the same set with the example object unchanged: a context that does not
// apply leaves the row unchecked, so a refusal of the row comes from its fragment.

// fragmentKinds are the fragments the runner builds, by key.
var fragmentKinds = map[string]bool{
	"tenant_yaml": true, "groups_yaml": true, "role_yaml": true, "task_type_yaml": true, "type_yaml": true,
	"tool_yaml": true, "plugin_yaml": true, "cmd_yaml": true, "action_yaml": true, "schedule_yaml": true, "route_yaml": true,
	// adds_type names a type the row's group grants; the runner's set has the example types.
	"adds_type": true,
}

// lintNeeds is what a lint fragment needs that the runner cannot build, or "".
func lintNeeds(l map[string]any) string {
	keys := make([]string, 0, len(l))
	for k := range l {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if k == "deploy_yaml" {
			return "lint deploy_yaml: the deployment description lint of farmctl is not part of the core"
		}
		if !fragmentKinds[k] {
			return "lint " + k
		}
		if k == "adds_type" {
			continue
		}
		if _, err := fragment(l[k]); err != nil {
			return "lint " + k + " in prose"
		}
	}
	if t, ok := l["tenant_yaml"]; ok {
		// A tenant fragment without kind: test lints a production tenant (the example tenant is
		// production), and a test bench refuses every production tenant (APR-9), so an apply
		// would not reach the row's check.
		f, _ := fragment(t)
		if str(f["kind"]) != "test" {
			return "lint of a production tenant on a test bench"
		}
	}
	return ""
}

// fragment reads a fragment: a mapping, or a string that holds a flow mapping without braces.
func fragment(v any) (map[string]any, error) {
	switch t := v.(type) {
	case map[string]any:
		return t, nil
	case string:
		var m map[string]any
		if err := yamlx.Unmarshal([]byte("{"+t+"}"), &m); err != nil {
			return nil, err
		}
		for _, e := range m {
			if e == nil {
				// "ops-reader with models.base base-alt" reads as a key without a value.
				return nil, fmt.Errorf("not a mapping: %q", t)
			}
		}
		return m, nil
	}
	return nil, fmt.Errorf("not a fragment: %T", v)
}

// setPath sets a dotted key in a mapping, creating the mappings on the way.
func setPath(m map[string]any, key string, v any) {
	parts := strings.Split(key, ".")
	for _, p := range parts[:len(parts)-1] {
		next := asMap(m[p])
		if next == nil {
			next = map[string]any{}
			m[p] = next
		}
		m = next
	}
	m[parts[len(parts)-1]] = v
}

// mergeInto merges a fragment into a mapping: mappings merge key by key, null removes the key
// (as in a JSON merge patch), any other value replaces the one there; dotted keys name nested
// fields.
func mergeInto(dst, src map[string]any) {
	for k, v := range src {
		if v == nil {
			delete(dst, k)
			continue
		}
		if strings.Contains(k, ".") {
			setPath(dst, k, v)
			continue
		}
		if sm, ok := v.(map[string]any); ok {
			if dm := asMap(dst[k]); dm != nil {
				mergeInto(dm, sm)
				continue
			}
		}
		dst[k] = v
	}
}

// example reads the example manifests of files under the skeleton root, without the fields of
// later versions.
func (x *exec) example(files ...string) ([]map[string]any, error) {
	paths := make([]string, len(files))
	for i, f := range files {
		paths[i] = filepath.Join(x.r.Root, f)
	}
	docs, err := manifest.ReadFiles(paths)
	if err != nil {
		return nil, err
	}
	var out []map[string]any
	for _, d := range docs {
		if d.Kind == "Tenant" {
			asMap(d.Value["spec"])["kind"] = "test"
		}
		out = append(out, d.Value)
	}
	out = dropLaterResources(x, out)
	if _, err := testbench.DropUnsupported(x.r.spec, out); err != nil {
		return nil, err
	}
	return out, nil
}

// lintObject is one fragment's target: the example object it completes and what makes the
// fragment part of it.
type lintObject struct {
	files []string // example manifests the object needs
	// picks are single objects of example files the object needs: file#Kind/name.
	picks []string
	pick  func(m map[string]any) bool // which of them is the target
	apply func(target map[string]any, f map[string]any)
}

func byKindName(kind, name string) func(map[string]any) bool {
	return func(m map[string]any) bool {
		return str(m["kind"]) == kind && str(asMap(m["metadata"])["name"]) == name
	}
}

func specMerge(target, f map[string]any) { mergeInto(asMap(target["spec"]), f) }

// taskTypeMerge is the merge of a schedule or route fragment, whose type is the type of the
// task it starts.
func taskTypeMerge(target, f map[string]any) {
	f = shallowCopy(f)
	if t, ok := f["type"]; ok {
		delete(f, "type")
		asMap(asMap(target["spec"])["task"])["type"] = t
	}
	specMerge(target, f)
}

// lintObjects are the targets of the fragments. A tool fragment is a tool of the checks plugin
// (a read tool with numbers only; its output_schema is written as its properties), a plugin
// fragment the spec of that plugin, a command fragment the disk_usage command of the cmd plugin
// (its other keys belong to the tool), a role fragment the reference role ops-reader with its
// types and tools (the role of INJ-10 and INJ-16), a type fragment the type ops.investigate.v1 and
// an action fragment the example action cleanup_pipeline_tmp.
var lintObjects = map[string]lintObject{
	"tenant_yaml": {files: []string{"examples/tenants/demo/tenant.yaml"}, pick: byKindName("Tenant", "demo"), apply: specMerge},
	"role_yaml":   {files: opsReaderFiles, pick: byKindName("Role", "ops-reader"), apply: specMerge},
	"task_type_yaml": {files: []string{"examples/tasktypes/ops.investigate.v1.yaml"}, pick: byKindName("TaskType", "ops.investigate.v1"),
		apply: specMerge},
	"type_yaml": {files: []string{"examples/tasktypes/report.monthly.numbers.v1.yaml"}, pick: byKindName("TaskType", "report.monthly.numbers.v1"),
		apply: specMerge},
	"action_yaml": {files: []string{"examples/actions.yaml"}, pick: byKindName("Action", "cleanup_pipeline_tmp"), apply: specMerge},
	"plugin_yaml": {files: []string{"examples/tenants/demo/catalog.yaml", "examples/tools/checks/tool.yaml"},
		pick: byKindName("ToolPlugin", "checks"), apply: specMerge},
	"tool_yaml": {files: []string{"examples/tenants/demo/catalog.yaml", "examples/tools/checks/tool.yaml"},
		pick: byKindName("ToolPlugin", "checks"), apply: func(target, f map[string]any) {
			tool := asMap(asList(asMap(target["spec"])["tools"])[0])
			f = shallowCopy(f)
			if os := asMap(f["output_schema"]); os != nil && os["type"] == nil {
				delete(f, "output_schema")
				asMap(tool["output_schema"])["properties"] = os
				var req []any
				for _, r := range asList(asMap(tool["output_schema"])["required"]) {
					if _, ok := os[str(r)]; ok {
						req = append(req, r)
					}
				}
				if len(req) > 0 {
					asMap(tool["output_schema"])["required"] = req
				} else {
					delete(asMap(tool["output_schema"]), "required")
				}
			}
			mergeInto(tool, f)
		}},
	"schedule_yaml": {files: []string{"examples/tenants/demo/schedules.yaml", "examples/tasktypes/data.sensor_check.v1.yaml"},
		picks: []string{"examples/tenants/demo/groups.yaml#Group/system-ops"},
		pick:  byKindName("Schedule", "sensor-nightly"), apply: taskTypeMerge},
	"route_yaml": {files: []string{"examples/tenants/demo/routes.yaml", "examples/tasktypes/pipeline.status.v1.yaml"},
		picks: []string{"examples/tenants/demo/groups.yaml#Group/system-ops", "examples/tenants/demo/groups.yaml#Group/runs"},
		pick:  byKindName("Route", "lab-run-failed"), apply: taskTypeMerge},
	"cmd_yaml": {files: []string{"examples/tenants/demo/catalog.yaml", "examples/tools/cmd/tool.yaml"},
		pick: byKindName("ToolPlugin", "cmd"), apply: func(target, f map[string]any) {
			tool := asMap(asList(asMap(target["spec"])["tools"])[0])
			cmd := map[string]any{}
			for k, v := range f {
				if k == "argv" || k == "params" {
					cmd[k] = v
				} else {
					tool[k] = v
				}
			}
			asMap(tool["commands"])["disk_usage"] = cmd
		}},
}

// opsReaderFiles are the reference role ops-reader with what it needs: the demo catalog, its
// task types, the plugins of its tools and the profile of its analysis slot.
var opsReaderFiles = []string{
	"examples/tenants/demo/catalog.yaml", "examples/roles/ops-reader/role.yaml",
	"examples/tasktypes/ops.investigate.v1.yaml", "examples/tasktypes/ops.health.v1.yaml", "examples/tasktypes/ops.summary.v1.yaml",
	"examples/tasktypes/ops.insight.v1.yaml", "examples/tools/cmd/tool.yaml", "examples/tools/logs/tool.yaml", "examples/tools/checks/tool.yaml",
	"examples/tools/storage/tool.yaml", "examples/tools/monitor/tool.yaml", "examples/tools/service/tool.yaml",
	"examples/model-profiles/analysis-std.yaml",
}

// lint applies a row's fragments with dry_run on top of the seed, after the same set without the
// fragments applied as the control.
func (x *exec) lint(ctx context.Context, l map[string]any) (*outcome, error) {
	var set []map[string]any
	type change struct {
		key    string
		target map[string]any
		obj    lintObject
	}
	var changes []change
	keys := make([]string, 0, len(l))
	for k := range l {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		switch k {
		case "adds_type":
			continue
		case "groups_yaml":
			continue
		}
		obj := lintObjects[k]
		ms, err := x.example(obj.files...)
		if err != nil {
			return nil, err
		}
		var target map[string]any
		for _, m := range ms {
			if obj.pick(m) {
				target = m
			}
		}
		if target == nil {
			return nil, fmt.Errorf("lint %s: no example object", k)
		}
		// Of the target's kind only the target: an example file holds other objects of it.
		kept := ms[:0]
		for _, m := range ms {
			if m["kind"] != target["kind"] || obj.pick(m) {
				kept = append(kept, m)
			}
		}
		ms = kept
		for _, p := range obj.picks {
			file, ref, _ := strings.Cut(p, "#")
			kind, name, _ := strings.Cut(ref, "/")
			more, err := x.example(file)
			if err != nil {
				return nil, err
			}
			for _, m := range more {
				if byKindName(kind, name)(m) {
					ms = append(ms, m)
				}
			}
		}
		set = withObjects(set, ms)
		changes = append(changes, change{k, target, obj})
	}
	set = x.withExecutors(set)
	var groups []map[string]any
	if g, ok := l["groups_yaml"]; ok {
		f, _ := fragment(g)
		groups = x.groupFragment(f)
	}
	if len(set) > 0 {
		o, err := x.apply(ctx, set, true)
		if err != nil {
			return nil, err
		}
		if o.Unimplemented != "" {
			return o, nil
		}
		if o.Result != "ok" {
			return nil, errUnchecked(fmt.Sprintf("the lint context of %s does not apply: %s", strings.Join(keys, ", "), o.errors()))
		}
	}
	removed := map[string]bool{}
	for _, c := range changes {
		f, _ := fragment(l[c.key])
		nullKeys(f, removed)
		c.obj.apply(c.target, deepCopy(f))
	}
	o, err := x.apply(ctx, withObjects(set, groups), true)
	if err != nil {
		return nil, err
	}
	// A field the fragment removes (a null value) is absent on purpose: its absence is what the
	// case checks, not an incomplete fixture manifest.
	for _, oo := range append([]*outcome{o}, o.Alts...) {
		var keep []string
		for _, m := range oo.Missing {
			if !removed[m[strings.LastIndex(m, ".")+1:]] {
				keep = append(keep, m)
			}
		}
		oo.Missing = keep
	}
	return o, nil
}

// nullKeys adds the names a fragment sets to null, at any depth, to out.
func nullKeys(f map[string]any, out map[string]bool) {
	for k, v := range f {
		if v == nil {
			out[k[strings.LastIndex(k, ".")+1:]] = true
		} else if m, ok := v.(map[string]any); ok {
			nullKeys(m, out)
		}
	}
}

// groupFragment builds the groups of a groups_yaml fragment (name: capability): a seed group
// gets the capability besides its own, another group is new with only it.
func (x *exec) groupFragment(f map[string]any) []map[string]any {
	var out []map[string]any
	names := make([]string, 0, len(f))
	for n := range f {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		var caps []any
		switch v := f[n].(type) {
		case []any:
			caps = v
		default:
			caps = []any{fmt.Sprint(v)}
		}
		g := map[string]any{"apiVersion": "botaru/v1alpha1", "kind": "Group", "metadata": map[string]any{"name": n, "tenant": "demo"},
			"spec": map[string]any{"caps": caps}}
		for _, b := range x.base {
			if byKindName("Group", n)(b) {
				g = deepCopy(b)
				sp := asMap(g["spec"])
				sp["caps"] = append(asList(sp["caps"]), caps...)
			}
		}
		out = append(out, g)
	}
	return out
}

// withObjects adds manifests to a set; one of the same kind, tenant and name replaces the one
// there.
func withObjects(set, more []map[string]any) []map[string]any {
	out := append([]map[string]any{}, set...)
	for _, m := range more {
		md := asMap(m["metadata"])
		replaced := false
		for i, o := range out {
			omd := asMap(o["metadata"])
			if str(o["kind"]) == str(m["kind"]) && str(omd["name"]) == str(md["name"]) && str(omd["tenant"]) == str(md["tenant"]) {
				out[i], replaced = m, true
			}
		}
		if !replaced {
			out = append(out, m)
		}
	}
	return out
}

// dropLaterResources leaves out the catalog resources of a later kind than the claim, which
// DropUnsupported cannot leave out field by field (a resource kind gives the entry its meaning).
func dropLaterResources(x *exec, ms []map[string]any) []map[string]any {
	for _, m := range ms {
		if str(m["kind"]) != "Catalog" {
			continue
		}
		sp := asMap(m["spec"])
		var kept []any
		for _, r := range asList(sp["resources"]) {
			probe := map[string]any{"apiVersion": "botaru/v1alpha1", "kind": "Catalog", "metadata": m["metadata"],
				"spec": map[string]any{"resources": []any{r}}}
			if ptrs, err := x.r.spec.Unsupported(probe); err == nil && len(ptrs) == 0 {
				kept = append(kept, r)
			}
		}
		sp["resources"] = kept
	}
	return ms
}

// withExecutors adds to a set with a tool plugin the example plugins that execute the seed's
// actions: apply checks every executor against the plugins of the set (executor_unknown).
func (x *exec) withExecutors(set []map[string]any) []map[string]any {
	plugins := false
	for _, m := range set {
		plugins = plugins || str(m["kind"]) == "ToolPlugin"
	}
	if !plugins {
		return set
	}
	need := map[string]bool{}
	for _, m := range x.base {
		if str(m["kind"]) == "Action" {
			if p := str(asMap(asMap(m["spec"])["executor"])["plugin"]); p != "" {
				need[p] = true
			}
		}
	}
	var files []string
	for p := range need {
		files = append(files, "examples/tools/"+p+"/tool.yaml")
	}
	sort.Strings(files)
	more, err := x.example(files...)
	if err != nil {
		return set
	}
	var add []map[string]any
	for _, m := range more {
		present := false
		for _, o := range set {
			present = present || byKindName("ToolPlugin", str(asMap(m["metadata"])["name"]))(o)
		}
		if !present {
			add = append(add, m)
		}
	}
	return append(set, add...)
}

// errors lists the errors of a refused apply.
func (o *outcome) errors() string {
	var out []string
	for _, e := range append([]*outcome{o}, o.Alts...) {
		out = append(out, strings.TrimSpace(e.Error+" "+e.Path+" "+e.Reason))
	}
	return strings.Join(out, "; ")
}
