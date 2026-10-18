// SPDX-License-Identifier: AGPL-3.0-only

package manifest

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// Node is one object of the output coupling graph (EVO-1): a manifest object, a dispatcher rule
// of a tenant (`DispatchRule`, Name is the rule index) or a skill folder (`Skill`, global).
type Node struct {
	Kind   string
	Tenant string
	Name   string
}

func (n Node) String() string {
	if n.Tenant == "" {
		return n.Kind + "/" + n.Name
	}
	return n.Kind + "/" + n.Tenant + "/" + n.Name
}

// Missing is a reference of a caller to a callee that the set does not hold.
type Missing struct {
	Caller Node
	Callee Node
}

// Graph is the directed graph of the references between the manifests of a set, from the
// callee to the caller: ToolPlugin feeds Role and Action; Skill and Action feed Role; Catalog
// feeds TaskType; TaskType feeds Role, Schedule, Route and DispatchRule; Role feeds BotGroup;
// BotGroup feeds DispatchRule; Script feeds Schedule (EVO-1). References are by name, as manifests write them.
type Graph struct {
	nodes   map[Node]bool
	callers map[Node][]Node
	callees map[Node][]Node
	missing []Missing
}

// Coupling builds the graph of a set of manifests.
func Coupling(docs []Doc) *Graph {
	g := &Graph{nodes: map[Node]bool{}, callers: map[Node][]Node{}, callees: map[Node][]Node{}}
	global := func(kind, name string) Node { return Node{Kind: kind, Name: name} }
	var roles, schedules, routes, groups, actions, types, catalogs []Doc
	var tenants []Doc
	for _, d := range docs {
		switch d.Kind {
		case "Role":
			roles = append(roles, d)
		case "Schedule":
			schedules = append(schedules, d)
		case "Route":
			routes = append(routes, d)
		case "BotGroup":
			groups = append(groups, d)
		case "Action":
			actions = append(actions, d)
		case "TaskType":
			types = append(types, d)
		case "Catalog":
			catalogs = append(catalogs, d)
		case "Tenant":
			tenants = append(tenants, d)
		}
		g.nodes[nodeOf(d)] = true
	}
	typeNames := map[string]bool{}
	for _, d := range types {
		typeNames[d.Name] = true
	}
	for _, d := range roles {
		r := nodeOf(d)
		spec := specOf(d)
		for _, t := range uniq(strs(spec["claims"]), strs(spec["may_enqueue"]), strs(dig(spec, "insight", "types"))) {
			g.ref(global("TaskType", t), r)
		}
		tools, _ := spec["tools"].([]any)
		for _, t := range tools {
			if p := toolPlugin(t); p != "" {
				g.ref(global("ToolPlugin", p), r)
			}
		}
		for _, a := range strs(spec["actions_may_request"]) {
			g.ref(global("Action", a), r)
		}
		if sk, ok := spec["skills"].([]any); ok {
			for _, s := range sk {
				if m, ok := s.(map[string]any); ok {
					if n, _ := m["name"].(string); n != "" {
						g.nodes[global("Skill", n)] = true
						g.ref(global("Skill", n), r)
					}
				}
			}
		}
	}
	for _, d := range actions {
		if p, _ := dig(specOf(d), "executor", "plugin").(string); p != "" {
			g.ref(global("ToolPlugin", p), nodeOf(d))
		}
	}
	for _, d := range schedules {
		s := nodeOf(d)
		if t, _ := dig(specOf(d), "task", "type").(string); t != "" {
			g.ref(global("TaskType", t), s)
		}
		if sc, _ := specOf(d)["script"].(string); sc != "" {
			g.ref(Node{Kind: "Script", Tenant: d.Tenant, Name: sc}, s)
		}
	}
	for _, d := range routes {
		if t, _ := dig(specOf(d), "task", "type").(string); t != "" {
			g.ref(global("TaskType", t), nodeOf(d))
		}
	}
	for _, d := range groups {
		for _, r := range strs(dig(specOf(d), "select", "roles")) {
			g.ref(global("Role", r), nodeOf(d))
		}
	}
	for _, d := range tenants {
		rules, _ := dig(specOf(d), "dispatch", "rules").([]any)
		for i, raw := range rules {
			rule, _ := raw.(map[string]any)
			n := Node{Kind: "DispatchRule", Tenant: d.Name, Name: strconv.Itoa(i)}
			g.nodes[n] = true
			match, _ := rule["match"].(map[string]any)
			if t, _ := match["type"].(string); t != "" {
				g.ref(global("TaskType", t), n)
			}
			if p, _ := match["type_prefix"].(string); p != "" {
				for _, t := range sortedKeys(typeNames) {
					if strings.HasPrefix(t, p) {
						g.ref(global("TaskType", t), n)
					}
				}
			}
			if bg, _ := rule["group"].(string); bg != "" {
				g.ref(Node{Kind: "BotGroup", Tenant: d.Name, Name: bg}, n)
			}
		}
	}
	// A catalog list feeds every task type whose schemas take values from a catalog
	// (enum_from): a task type is global, so every tenant's catalogs feed it.
	for _, t := range types {
		if !usesCatalog(t.Value) {
			continue
		}
		for _, c := range catalogs {
			g.ref(nodeOf(c), nodeOf(t))
		}
	}
	for n := range g.callers {
		slices.SortFunc(g.callers[n], cmpNode)
	}
	for n := range g.callees {
		slices.SortFunc(g.callees[n], cmpNode)
	}
	slices.SortFunc(g.missing, func(a, b Missing) int {
		if c := cmpNode(a.Caller, b.Caller); c != 0 {
			return c
		}
		return cmpNode(a.Callee, b.Callee)
	})
	return g
}

// ref adds the edge callee -> caller once, and records a callee the set does not hold.
func (g *Graph) ref(callee, caller Node) {
	if slices.Contains(g.callers[callee], caller) {
		return
	}
	g.callers[callee] = append(g.callers[callee], caller)
	g.callees[caller] = append(g.callees[caller], callee)
	if !g.nodes[callee] {
		g.missing = append(g.missing, Missing{Caller: caller, Callee: callee})
	}
}

// Has says whether the set holds the node.
func (g *Graph) Has(n Node) bool { return g.nodes[n] }

// Callers are the nodes that reference n.
func (g *Graph) Callers(n Node) []Node { return g.callers[n] }

// Callees are the nodes that n references.
func (g *Graph) Callees(n Node) []Node { return g.callees[n] }

// Neighbours are the direct callers and callees of n, sorted.
func (g *Graph) Neighbours(n Node) []Node {
	out := slices.Concat(g.callers[n], g.callees[n])
	slices.SortFunc(out, cmpNode)
	return slices.Compact(out)
}

// Missing are the references to callees the set does not hold, sorted by caller.
func (g *Graph) Missing() []Missing { return g.missing }

// MissingNear are the missing references of the nodes within one step of the changed nodes:
// the references a change may have broken (EVO-1).
func (g *Graph) MissingNear(changed []Node) []Missing {
	near := map[Node]bool{}
	for _, n := range changed {
		near[n] = true
		for _, m := range g.Neighbours(n) {
			near[m] = true
		}
	}
	var out []Missing
	for _, m := range g.missing {
		if near[m.Caller] || near[m.Callee] {
			out = append(out, m)
		}
	}
	return out
}

// Affected are the roles whose work a change of the nodes reaches: a changed role, the roles
// that reference a changed node, and the roles that reference a callee of a changed node.
func (g *Graph) Affected(changed []Node) []string {
	set := map[string]bool{}
	add := func(n Node) {
		if n.Kind == "Role" {
			set[n.Name] = true
		}
	}
	for _, n := range changed {
		add(n)
		for _, c := range g.callers[n] {
			add(c)
		}
		for _, callee := range g.callees[n] {
			for _, c := range g.callers[callee] {
				add(c)
			}
		}
	}
	return sortedKeys(set)
}

// TopoOrder orders the given nodes from callee to caller by the paths of the whole graph, ties
// by name; a cycle among them is an error.
func (g *Graph) TopoOrder(nodes []Node) ([]Node, error) {
	in := map[Node]bool{}
	for _, n := range nodes {
		in[n] = true
	}
	// before[a][b]: a must come before b (a reaches b along callee -> caller edges).
	reach := func(from Node) map[Node]bool {
		seen := map[Node]bool{}
		stack := []Node{from}
		for len(stack) > 0 {
			n := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			for _, c := range g.callers[n] {
				if !seen[c] {
					seen[c] = true
					stack = append(stack, c)
				}
			}
		}
		return seen
	}
	list := slices.Clone(nodes)
	slices.SortFunc(list, cmpNode)
	list = slices.Compact(list)
	after := map[Node][]Node{}
	indeg := map[Node]int{}
	for _, a := range list {
		r := reach(a)
		for _, b := range list {
			if a != b && r[b] {
				after[a] = append(after[a], b)
				indeg[b]++
			}
		}
	}
	var out []Node
	done := map[Node]bool{}
	for len(out) < len(list) {
		progressed := false
		for _, n := range list {
			if done[n] || indeg[n] > 0 {
				continue
			}
			done[n] = true
			out = append(out, n)
			for _, b := range after[n] {
				indeg[b]--
			}
			progressed = true
			break
		}
		if !progressed {
			var rest []string
			for _, n := range list {
				if !done[n] {
					rest = append(rest, n.String())
				}
			}
			return nil, fmt.Errorf("coupling cycle among %s", strings.Join(rest, ", "))
		}
	}
	return out, nil
}

// FieldNode is the node a change of a manifest field touches: a field under
// spec.dispatch.rules of a tenant touches the dispatcher rule (the form `rules[]` names the
// tenant's single rule, `rules[i]` the rule i); any other field touches its object.
func FieldNode(kind, tenant, name, field string) Node {
	if kind == "Tenant" {
		if rest, ok := strings.CutPrefix(field, "spec.dispatch.rules["); ok {
			idx, _, _ := strings.Cut(rest, "]")
			if idx == "" {
				idx = "0"
			}
			return Node{Kind: "DispatchRule", Tenant: name, Name: idx}
		}
		return Node{Kind: "Tenant", Tenant: name, Name: name}
	}
	if tenant == "" || globalKinds[kind] {
		return Node{Kind: kind, Name: name}
	}
	return Node{Kind: kind, Tenant: tenant, Name: name}
}

// globalKinds are the kinds whose objects carry no tenant (spec/manifests/kinds.yaml).
var globalKinds = map[string]bool{"Role": true, "TaskType": true, "ToolPlugin": true, "Action": true,
	"MessageType": true, "IncidentClass": true, "EvalReports": true}

func nodeOf(d Doc) Node {
	if d.Kind == "Tenant" {
		return Node{Kind: "Tenant", Tenant: d.Name, Name: d.Name}
	}
	if globalKinds[d.Kind] {
		return Node{Kind: d.Kind, Name: d.Name}
	}
	return Node{Kind: d.Kind, Tenant: d.Tenant, Name: d.Name}
}

func cmpNode(a, b Node) int { return strings.Compare(a.String(), b.String()) }

func specOf(d Doc) map[string]any {
	s, _ := d.Value["spec"].(map[string]any)
	return s
}

func dig(m map[string]any, path ...string) any {
	var cur any = m
	for _, p := range path {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = mm[p]
	}
	return cur
}

func strs(v any) []string {
	list, _ := v.([]any)
	var out []string
	for _, x := range list {
		if s, ok := x.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}

func uniq(lists ...[]string) []string {
	out := slices.Concat(lists...)
	slices.Sort(out)
	return slices.Compact(out)
}

// toolPlugin is the plugin of a role tool entry: `<plugin>/<tool>`, a bare `<plugin>`, or an
// object with `plugin`.
func toolPlugin(t any) string {
	switch v := t.(type) {
	case string:
		p, _, _ := strings.Cut(v, "/")
		return p
	case map[string]any:
		if p, _ := v["plugin"].(string); p != "" {
			return p
		}
		if n, _ := v["name"].(string); n != "" {
			p, _, _ := strings.Cut(n, "/")
			return p
		}
	}
	return ""
}

// usesCatalog says whether a value holds an `enum_from` keyword anywhere.
func usesCatalog(v any) bool {
	switch x := v.(type) {
	case map[string]any:
		if _, ok := x["enum_from"]; ok {
			return true
		}
		for _, e := range x {
			if usesCatalog(e) {
				return true
			}
		}
	case []any:
		for _, e := range x {
			if usesCatalog(e) {
				return true
			}
		}
	}
	return false
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
