// SPDX-License-Identifier: AGPL-3.0-only

package manifest

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const couplingSet = `
apiVersion: botaru/v1alpha1
kind: Tenant
metadata: {name: lab}
spec:
  dispatch:
    rules:
      - match: {type_prefix: ops.}
        group: night
---
apiVersion: botaru/v1alpha1
kind: TaskType
metadata: {name: ops.check.v1}
spec: {purpose: work, params_schema: {type: object, properties: {host: {type: string, enum_from: hosts}}}}
---
apiVersion: botaru/v1alpha1
kind: TaskType
metadata: {name: ops.report.v1}
spec: {purpose: work}
---
apiVersion: botaru/v1alpha1
kind: ToolPlugin
metadata: {name: logs}
spec: {}
---
apiVersion: botaru/v1alpha1
kind: Catalog
metadata: {name: lab, tenant: lab}
spec: {}
---
apiVersion: botaru/v1alpha1
kind: Role
metadata: {name: ops-reader}
spec:
  claims: [ops.check.v1]
  tools: [logs/tail]
  skills: [{name: triage, description: x, sha256: "00"}]
---
apiVersion: botaru/v1alpha1
kind: BotGroup
metadata: {name: night, tenant: lab}
spec: {select: {roles: [ops-reader]}}
---
apiVersion: botaru/v1alpha1
kind: Schedule
metadata: {name: nightly, tenant: lab}
spec: {cron: "0 3 * * *", task: {type: ops.check.v1}}
---
apiVersion: botaru/v1alpha1
kind: Schedule
metadata: {name: weekly, tenant: lab}
spec: {cron: "0 4 * * 1", task: {type: ops.gone.v1}}
`

func couplingDocs(t *testing.T) []Doc {
	t.Helper()
	docs, err := ParseYAML(strings.NewReader(couplingSet), "set.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return docs
}

func names(ns []Node) []string {
	var out []string
	for _, n := range ns {
		out = append(out, n.String())
	}
	return out
}

// The edges of the graph run from the callee to the caller (EVO-1).
func TestCouplingEdges(t *testing.T) {
	g := Coupling(couplingDocs(t))
	tt := Node{Kind: "TaskType", Name: "ops.check.v1"}
	want := []string{"Catalog/lab/lab", "DispatchRule/lab/0", "Role/ops-reader", "Schedule/lab/nightly"}
	got := names(g.Neighbours(tt))
	if !slices.Equal(got, want) {
		t.Fatalf("neighbours of %s = %v, want %v", tt, got, want)
	}
	if c := names(g.Callers(Node{Kind: "ToolPlugin", Name: "logs"})); !slices.Equal(c, []string{"Role/ops-reader"}) {
		t.Fatalf("callers of logs = %v", c)
	}
	if c := names(g.Callers(Node{Kind: "Skill", Name: "triage"})); !slices.Equal(c, []string{"Role/ops-reader"}) {
		t.Fatalf("callers of the skill = %v", c)
	}
	// The prefix rule reaches both ops types; the group feeds the rule; the role feeds the group.
	rule := Node{Kind: "DispatchRule", Tenant: "lab", Name: "0"}
	if c := names(g.Callees(rule)); !slices.Equal(c, []string{"BotGroup/lab/night", "TaskType/ops.check.v1", "TaskType/ops.report.v1"}) {
		t.Fatalf("callees of the rule = %v", c)
	}
	miss := g.Missing()
	if len(miss) != 1 || miss[0].Callee.String() != "TaskType/ops.gone.v1" || miss[0].Caller.String() != "Schedule/lab/weekly" {
		t.Fatalf("missing = %+v", miss)
	}
	if near := g.MissingNear([]Node{{Kind: "Schedule", Tenant: "lab", Name: "nightly"}}); len(near) != 0 {
		t.Fatalf("a change of nightly broke nothing, got %+v", near)
	}
	if near := g.MissingNear([]Node{{Kind: "Schedule", Tenant: "lab", Name: "weekly"}}); len(near) != 1 {
		t.Fatalf("a change of weekly sees its missing type, got %+v", near)
	}
}

// The roles a change reaches and the node of a tenant field (EVO-1).
func TestCouplingAffectedAndFields(t *testing.T) {
	g := Coupling(couplingDocs(t))
	sched := FieldNode("Schedule", "lab", "nightly", "spec.cron")
	if sched.String() != "Schedule/lab/nightly" {
		t.Fatalf("field node = %s", sched)
	}
	if a := g.Affected([]Node{sched}); !slices.Equal(a, []string{"ops-reader"}) {
		t.Fatalf("affected roles = %v", a)
	}
	rule := FieldNode("Tenant", "", "lab", "spec.dispatch.rules[].timeouts.attempt")
	if rule.String() != "DispatchRule/lab/0" {
		t.Fatalf("dispatch field node = %s", rule)
	}
	if n := FieldNode("Tenant", "", "lab", "spec.limits.max_queued"); n.String() != "Tenant/lab/lab" {
		t.Fatalf("tenant field node = %s", n)
	}
	if n := FieldNode("Role", "lab", "ops-reader", "spec.claims"); n.String() != "Role/ops-reader" {
		t.Fatalf("a global kind carries no tenant: %s", n)
	}
}

// A batch of changes goes from callee to caller; a cycle is an error (EVO-1).
func TestCouplingTopoOrder(t *testing.T) {
	g := Coupling(couplingDocs(t))
	in := []Node{
		{Kind: "BotGroup", Tenant: "lab", Name: "night"},
		{Kind: "Schedule", Tenant: "lab", Name: "nightly"},
		{Kind: "Role", Name: "ops-reader"},
		{Kind: "TaskType", Name: "ops.check.v1"},
		{Kind: "ToolPlugin", Name: "logs"},
	}
	out, err := g.TopoOrder(in)
	if err != nil {
		t.Fatal(err)
	}
	pos := map[string]int{}
	for i, n := range out {
		pos[n.String()] = i
	}
	for _, e := range [][2]string{
		{"TaskType/ops.check.v1", "Role/ops-reader"},
		{"TaskType/ops.check.v1", "Schedule/lab/nightly"},
		{"ToolPlugin/logs", "Role/ops-reader"},
		{"Role/ops-reader", "BotGroup/lab/night"},
	} {
		if pos[e[0]] > pos[e[1]] {
			t.Fatalf("%s must come before %s: %v", e[0], e[1], names(out))
		}
	}
	// A role, its bot group and the rule that names both order without a cycle.
	cyc, err := ParseYAML(strings.NewReader(`
apiVersion: botaru/v1alpha1
kind: Role
metadata: {name: a}
spec: {claims: [t.v1]}
---
apiVersion: botaru/v1alpha1
kind: BotGroup
metadata: {name: g, tenant: lab}
spec: {select: {roles: [a]}}
---
apiVersion: botaru/v1alpha1
kind: Tenant
metadata: {name: lab}
spec: {dispatch: {rules: [{match: {type: t.v1}, group: g}]}}
`), "c.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cg := Coupling(cyc)
	if _, err := cg.TopoOrder([]Node{{Kind: "Role", Name: "a"}, {Kind: "BotGroup", Tenant: "lab", Name: "g"}}); err != nil {
		t.Fatalf("no cycle between the role and its group: %v", err)
	}
}

// TestCouplingExamples builds the graph of the example set: every schedule's task type and
// every claimed type of the example roles resolve.
func TestCouplingExamples(t *testing.T) {
	root := filepath.Join("..", "..", "..", "examples")
	if _, err := os.Stat(root); err != nil {
		t.Skip("no examples folder")
	}
	docs, err := ReadFiles([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	g := Coupling(docs)
	for _, m := range g.Missing() {
		if m.Caller.Kind == "Schedule" && m.Callee.Kind == "TaskType" {
			t.Errorf("example schedule %s names a missing type %s", m.Caller, m.Callee)
		}
	}
}
