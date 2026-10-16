// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"slices"
	"strings"
	"testing"

	"github.com/HunterEwok/botaru/impl/internal/manifest"
)

// COM-19, COM-8: a seat or the commission manifest names a lens of lenses.yaml or a tenant lens
// of the charter, else lens_unknown; the weight of farm_manager is set by rule, so before v0.2
// the lens is unknown and from v0.2 its weight is lens_weight_fixed.
func TestLintLensNames(t *testing.T) {
	c := lintCore(t)
	com := func(weights string) string {
		return "apiVersion: botaru/v1alpha1\nkind: Commission\nmetadata: {name: default, tenant: demo}\nspec:\n  weights: " + weights + "\n---\n"
	}
	charter := `apiVersion: botaru/v1alpha1
kind: Governance
metadata: {name: demo, tenant: demo}
spec:
  commission:
    seats:
      - {id: 1, lens: x_privacy}
    lenses:
      x_privacy: {meaning: "Personal data in the change.", persona: {ref: p.md}}
---
`
	has := func(got []string, code string) bool { return slices.Contains(got, code) }
	if got := lintCodes(t, c, com("{privacy: 2}")); !has(got, "lens_unknown") {
		t.Fatalf("an unknown lens weight: %v", got)
	}
	if got := lintCodes(t, c, com("{value: 2, x_privacy: 2}")+charter); has(got, "lens_unknown") {
		t.Fatalf("a skeleton lens and a charter lens: %v", got)
	}
	if got := lintCodes(t, c, strings.Replace(charter, "lens: x_privacy", "lens: x_other", 1)); !has(got, "lens_unknown") {
		t.Fatalf("a seat with a lens of no charter: %v", got)
	}
	if got := lintCodes(t, c, com("{farm_manager: 1}")); !has(got, "lens_unknown") {
		t.Fatalf("farm_manager in a v0.1 commission: %v", got)
	}
	setClaimsV03(t, false) // v0.2
	if got := lintCodes(t, c, com("{farm_manager: 1}")); !has(got, "lens_weight_fixed") || has(got, "lens_unknown") {
		t.Fatalf("farm_manager weight from v0.2: %v", got)
	}
}

// APR-17: a pipeline without a card changes nothing on work machines, so run.approval none
// with tier 3 fails apply.
func TestLintRunApprovalTier(t *testing.T) {
	c := lintCore(t)
	typ := func(tier string) string {
		return `apiVersion: botaru/v1alpha1
kind: TaskType
metadata: {name: report.monthly.numbers.v1}
spec: {worker_kind: job, purpose: work, tier: ` + tier + `, data_class: internal, run: {approval: none}, proposable: true}
---
`
	}
	if got := lintCodes(t, c, typ("0")); slices.Contains(got, "run_not_readonly") {
		t.Fatalf("tier 0: %v", got)
	}
	if got := lintCodes(t, c, typ("3")); !slices.Contains(got, "run_not_readonly") {
		t.Fatalf("tier 3 without a card: %v", got)
	}
}

// PRM-5: pii is reserved in types, tools and the catalog, and secret is never an output class.
func TestLintReservedClasses(t *testing.T) {
	c := lintCore(t)
	set := func(typeClass, outClass, resClass string) string {
		return `apiVersion: botaru/v1alpha1
kind: TaskType
metadata: {name: ops.check.v1}
spec: {worker_kind: job, purpose: work, tier: 0, data_class: ` + typeClass + `, proposable: true}
---
apiVersion: botaru/v1alpha1
kind: ToolPlugin
metadata: {name: checks}
spec:
  placement: host
  tools:
    - {name: ping, access: read, tier: 0, output_class: ` + outClass + `, untrusted_output: true, target: any}
---
apiVersion: botaru/v1alpha1
kind: Catalog
metadata: {name: demo, tenant: demo}
spec:
  resources:
    - {id: names, values: [a], data_class: ` + resClass + `}
---
`
	}
	paths := func(docs string) []string {
		ds, err := manifest.ParseYAML(strings.NewReader(docs), "test")
		if err != nil {
			t.Fatal(err)
		}
		index := make([]int, len(ds))
		for i := range ds {
			index[i] = i
			if ds[i].Kind != "Catalog" {
				ds[i].Tenant = ""
			}
		}
		errs, err := c.lintManifests(ds, index, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, e := range errs {
			if e.Code == "cap_data_class" {
				out = append(out, e.Path)
			}
		}
		slices.Sort(out)
		return out
	}
	if got := paths(set("internal", "raw_logs", "internal")); len(got) > 0 {
		t.Fatalf("allowed classes: %v", got)
	}
	want := []string{"/manifests/0/spec/data_class", "/manifests/1/spec/tools/0/output_class", "/manifests/2/spec/resources/0/data_class"}
	if got := paths(set("pii", "pii", "pii")); !slices.Equal(got, want) {
		t.Fatalf("pii: %v", got)
	}
	if got := paths(set("internal", "secret", "internal")); !slices.Equal(got, want[1:2]) {
		t.Fatalf("secret as an output class: %v", got)
	}
}

// PRM-13 (authz-16 of PRM-7): apply rejects a set in which an ordinary group became privileged,
// with members or without; a group new in the set is not a raise.
func TestLintPrivilegeRaised(t *testing.T) {
	c := lintCore(t)
	tenant := "apiVersion: botaru/v1alpha1\nkind: Tenant\nmetadata: {name: demo}\nspec: {kind: test}\n---\n"
	typ := "apiVersion: botaru/v1alpha1\nkind: TaskType\nmetadata: {name: ops.investigate.v1}\nspec: {worker_kind: agent, purpose: work, tier: 1, data_class: internal}\n---\n" +
		"apiVersion: botaru/v1alpha1\nkind: TaskType\nmetadata: {name: ops.health.v1}\nspec: {worker_kind: agent, purpose: work, tier: 0, data_class: internal}\n---\n"
	group := func(name, caps string) string {
		return "apiVersion: botaru/v1alpha1\nkind: Group\nmetadata: {name: " + name + ", tenant: demo}\nspec: {caps: [" + caps + "]}\n---\n"
	}
	parse := func(docs string) []manifest.Doc {
		ds, err := manifest.ParseYAML(strings.NewReader(docs), "test")
		if err != nil {
			t.Fatal(err)
		}
		for i := range ds {
			if ds[i].Kind == "Tenant" || ds[i].Kind == "TaskType" {
				ds[i].Tenant = ""
			}
		}
		return ds
	}
	lint := func(after, prior string) []string {
		ds := parse(after)
		index := make([]int, len(ds))
		for i := range ds {
			index[i] = i
		}
		errs, err := c.lintManifests(ds, index, nil, parse(prior))
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, e := range errs {
			if e.Code == "privileged_group" {
				out = append(out, e.Name)
			}
		}
		return out
	}
	prior := tenant + typ + group("user", "\"task:ops.health.v1\"")
	if got := lint(tenant+typ+group("user", "\"task:ops.health.v1\", \"task:ops.*\""), prior); !slices.Equal(got, []string{"user"}) {
		t.Fatalf("an ordinary group with task:ops.*: %v", got)
	}
	if got := lint(tenant+typ+group("user", "\"task:ops.health.v1\"")+group("leads", "\"task:ops.*\""), prior); len(got) > 0 {
		t.Fatalf("a new privileged group: %v", got)
	}
	// A type that rises to tier 1 makes the group that names it privileged; a type that leaves
	// the set grants nothing.
	raised := strings.Replace(prior, "tier: 0", "tier: 1", 1)
	if got := lint(raised, prior); !slices.Equal(got, []string{"user"}) {
		t.Fatalf("the type of an ordinary group rises to tier 1: %v", got)
	}
	if got := lint(tenant+group("user", "\"task:ops.health.v1\""), prior); len(got) > 0 {
		t.Fatalf("the type of a group leaves the set: %v", got)
	}
}

// GOV-33, GOV-22: a seat declared twice, the farm_manager block in a tenant charter and a
// field against its one direction fail apply.
func TestLintCharterBounds(t *testing.T) {
	c := lintCore(t)
	charter := func(tenant, spec string) string {
		return "apiVersion: botaru/v1alpha1\nkind: Governance\nmetadata: {name: " + tenant + ", tenant: " + tenant + "}\nspec: " + spec + "\n---\n"
	}
	for spec, want := range map[string]string{
		`{commission: {seats: [{id: 1, lens: security}, {id: 1, lens: cost}]}}`: "charter_seat_twice",
		`{farm_manager: {max_rank: 4}}`:                                         "charter_farm_manager",
		`{promotion: {probation: 7d}}`:                                          "charter_direction",
		`{promotion: {min_tenure: 20d}}`:                                        "charter_direction",
		`{removal: {protect_new: 30d}}`:                                         "charter_direction",
		`{memory: {seal_ttl: 10d}}`:                                             "charter_direction",
		`{identity: {unbound_ttl: 10d}}`:                                        "charter_direction",
		`{promotion: {exclude_change_kinds: [script]}}`:                         "charter_direction",
	} {
		if got := lintCodes(t, c, charter("demo", spec)); !slices.Contains(got, want) {
			t.Errorf("%s: %v, want %s", spec, got, want)
		}
	}
	ok := `{promotion: {probation: 21d, swap_cooldown: 45d, exclude_change_kinds: [none, script]}, memory: {seal_ttl: 45d}}`
	if got := lintCodes(t, c, charter("demo", ok)); slices.ContainsFunc(got, func(s string) bool { return strings.HasPrefix(s, "charter_") }) {
		t.Fatalf("raised fields: %v", got)
	}
	long := `{farm_manager: {incident_stop: {decide_within: 10d}}}`
	if got := lintCodes(t, c, charter("farm", long)); !slices.Contains(got, "charter_direction") {
		t.Fatalf("decide_within above 7 days: %v", got)
	}
	if got := lintCodes(t, c, charter("farm", `{farm_manager: {incident_stop: {decide_within: 3d}}}`)); slices.Contains(got, "charter_direction") {
		t.Fatalf("decide_within shortened: %v", got)
	}
}

// MON-5: an alarm of the class unknown is alarm.monitor.problem with severity warn, as the
// class table sets it.
func TestMonitorProblemSeverity(t *testing.T) {
	c := lintCore(t)
	if s, err := c.alarmSeverity(Alarm{Name: "monitor.problem"}); err != nil || s != "warn" {
		t.Fatalf("severity %q %v", s, err)
	}
}
