// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/HunterEwok/botaru/impl/internal/manifest"
	"github.com/HunterEwok/botaru/impl/internal/spec"
)

// lintCore is a core with only the specification, enough for the set lint.
func lintCore(t *testing.T, farm ...string) *Core {
	t.Helper()
	root, err := spec.FindRoot("")
	if err != nil {
		t.Fatal(err)
	}
	sp, err := spec.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	return &Core{spec: sp, rs: newResultSchemas(), log: slog.Default(), cfg: Config{FarmHosts: farm}}
}

// lintCodes runs the set lint over YAML manifests and returns the codes it found.
func lintCodes(t *testing.T, c *Core, docs string) []string {
	t.Helper()
	ds, err := manifest.ParseYAML(strings.NewReader(docs), "test")
	if err != nil {
		t.Fatal(err)
	}
	index := make([]int, len(ds))
	for i := range ds {
		index[i] = i
		if ds[i].Kind == "Role" || ds[i].Kind == "TaskType" || ds[i].Kind == "ToolPlugin" {
			ds[i].Tenant = ""
		}
	}
	errs, err := c.lintManifests(ds, index, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range errs {
		if !slices.Contains(out, e.Code) {
			out = append(out, e.Code)
		}
	}
	slices.Sort(out)
	return out
}

// A machine, a monitoring node and a (machine, service) pair belong to one tenant, and a farm
// node is in no catalog (DEP-11, TEN-4, D-75).
func TestLintHostsOneTenant(t *testing.T) {
	c := lintCore(t, "core1", "backup1")
	cat := func(tenant, host string) string {
		return `apiVersion: botaru/v1alpha1
kind: Catalog
metadata: {name: ` + tenant + `, tenant: ` + tenant + `}
spec:
  hosts:
    - {id: ` + host + `, monitor_host: ` + host + `.example.org, services: [{name: nginx, unit: nginx.service, control: [restart], via: host}]}
---
`
	}
	if got := lintCodes(t, c, cat("demo", "web1")+cat("demo2", "web2")); len(got) > 0 {
		t.Fatalf("two tenants on their own machines: %v", got)
	}
	if got := lintCodes(t, c, cat("demo", "web1")+cat("demo2", "web1")); !slices.Equal(got, []string{"host_two_tenants"}) {
		t.Fatalf("one machine in two catalogs: %v", got)
	}
	if got := lintCodes(t, c, cat("demo", "core1")); !slices.Equal(got, []string{"farm_host_in_catalog"}) {
		t.Fatalf("a farm node in a catalog: %v", got)
	}
	if got := lintCodes(t, lintCore(t), cat("demo", "core1")); len(got) > 0 {
		t.Fatalf("without farm_hosts nothing is a farm node: %v", got)
	}
}

// A plugin reaches only machines of a tenant catalog; the monitor plugin also reaches the
// monitoring API of the deployment (INJ-6).
func TestLintEgress(t *testing.T) {
	c := lintCore(t)
	set := func(name, egress string) string {
		return `apiVersion: botaru/v1alpha1
kind: Catalog
metadata: {name: demo, tenant: demo}
spec:
  hosts: [{id: web1}]
---
apiVersion: botaru/v1alpha1
kind: ToolPlugin
metadata: {name: ` + name + `}
spec:
  placement: host
  network: {egress: [` + egress + `]}
  tools: []
`
	}
	for _, tc := range []struct {
		name, egress string
		want         []string
	}{
		{"web", "web1", nil},
		{"web", "", nil},
		{"web", "db9", []string{"egress_unknown"}},
		{"web", "monitor-api", []string{"egress_unknown"}},
		{"monitor", "monitor-api", nil},
	} {
		if got := lintCodes(t, c, set(tc.name, tc.egress)); !slices.Equal(got, tc.want) {
			t.Errorf("%s egress [%s]: %v, want %v", tc.name, tc.egress, got, tc.want)
		}
	}
}

// The tenant dispatch policy only narrows (DSP-3, FEN-3): the cases of fx-dsp-10 that the
// schema does not already refuse.
func TestLintDispatch(t *testing.T) {
	c := lintCore(t)
	base := `apiVersion: botaru/v1alpha1
kind: TaskType
metadata: {name: data.qc.ctd.v1}
spec: {purpose: work, worker_kind: agent, timeouts: {attempt: 5m, deadline: 30m}, retry: {max_attempts: 3}}
---
apiVersion: botaru/v1alpha1
kind: TaskType
metadata: {name: report.render.v1}
spec: {purpose: work, worker_kind: agent}
---
apiVersion: botaru/v1alpha1
kind: Role
metadata: {name: analyst}
spec: {worker_kind: agent, claims: [data.qc.ctd.v1]}
---
apiVersion: botaru/v1alpha1
kind: Role
metadata: {name: reporter}
spec: {worker_kind: agent, claims: [report.render.v1]}
---
apiVersion: botaru/v1alpha1
kind: BotGroup
metadata: {name: science-pool, tenant: demo}
spec: {select: {roles: [analyst], labels: {zone: lab}}}
---
apiVersion: botaru/v1alpha1
kind: BotGroup
metadata: {name: reporters, tenant: demo}
spec: {select: {roles: [reporter]}}
---
apiVersion: botaru/v1alpha1
kind: Tenant
metadata: {name: demo}
spec:
  kind: test
  dispatch:
    rules: [`
	for _, tc := range []struct {
		rule string
		want []string
	}{
		{`{match: {type: data.qc.ctd.v1}, group: science-pool, priority_class: 4, max_running: 4, timeouts: {attempt: 8m}, retry: {max_attempts: 2}}`, nil},
		{`{match: {type: data.qc.ctd.v1}, group: no-such-group}`, []string{"ref_missing"}},
		{`{match: {type: data.qc.ctd.v1}, group: reporters}`, []string{"dispatch_group_disjoint"}},
		{`{match: {type: data.qc.ctd.v1}, timeouts: {attempt: 11m}}`, []string{"dispatch_attempt_bounds"}},
		{`{match: {type: data.qc.ctd.v1}, timeouts: {attempt: 10m}}`, nil},
		{`{match: {type: data.qc.ctd.v1}, retry: {max_attempts: 3}}`, []string{"dispatch_attempt_bounds"}},
		{`{match: {type: data.qc.ctd.v1}, retry: {max_attempts: 0}}`, []string{"dispatch_attempt_bounds"}},
		{`{match: {type_prefix: data.}, timeouts: {attempt: 31m}}`, []string{"dispatch_attempt_bounds"}},
	} {
		got := lintCodes(t, c, base+tc.rule+"]\n")
		// The example set has no manager line or charter; only the dispatch codes count here.
		got = slices.DeleteFunc(got, func(s string) bool { return !strings.HasPrefix(s, "dispatch_") && s != "ref_missing" })
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s: %v, want %v", tc.rule, got, tc.want)
		}
	}
}

// A model profile of a tenant is checked as the runtime catalog resolves it: the tenant's own
// profile first, not one of another tenant with the same name.
func TestLintProfileOfTenant(t *testing.T) {
	c := lintCore(t)
	prof := func(tenant, training string) string {
		return `apiVersion: botaru/v1alpha1
kind: ModelProfile
metadata: {name: base, tenant: ` + tenant + `}
spec:
  provider: provider-a
  provider_training: {value: ` + training + `}
---
`
	}
	ten := `apiVersion: botaru/v1alpha1
kind: Tenant
metadata: {name: demo}
spec:
  models_allowed: [base]
`
	if got := lintCodes(t, c, prof("demo", "true")+prof("demo2", "false")+ten); !slices.Contains(got, "provider_training") {
		t.Fatalf("the training profile of the tenant itself: %v", got)
	}
	if got := lintCodes(t, c, prof("demo2", "true")+prof("demo", "false")+ten); slices.Contains(got, "provider_training") {
		t.Fatalf("a training profile of another tenant: %v", got)
	}
}

// A role that names a whole plugin gets each of its tools checked: a write tool among them is
// refused as when it is named alone.
func TestLintWholePlugin(t *testing.T) {
	c := lintCore(t)
	set := func(access, ref string) string {
		return `apiVersion: botaru/v1alpha1
kind: ToolPlugin
metadata: {name: web}
spec:
  tools: [{name: restart, access: ` + access + `}]
---
apiVersion: botaru/v1alpha1
kind: Role
metadata: {name: r}
spec:
  tools: [` + ref + `]
`
	}
	for _, tc := range []struct {
		access, ref string
		want        bool
	}{
		{"write", "web", true},
		{"write", "web/restart", true},
		{"read", "web", false},
	} {
		if got := lintCodes(t, c, set(tc.access, tc.ref)); slices.Contains(got, "role_write_tool") != tc.want {
			t.Errorf("%s tool by %s: %v", tc.access, tc.ref, got)
		}
	}
	if got := lintCodes(t, c, set("read", "nope")); !slices.Contains(got, "ref_missing") {
		t.Errorf("an unknown plugin: %v", got)
	}
	// A person-memory role may not hold a tool with untrusted output, by name or by plugin.
	for _, ref := range []string{"web", "web/fetch"} {
		got := lintCodes(t, c, `apiVersion: botaru/v1alpha1
kind: ToolPlugin
metadata: {name: web}
spec:
  tools: [{name: fetch, access: read, untrusted_output: true}]
---
apiVersion: botaru/v1alpha1
kind: Role
metadata: {name: r}
spec:
  memory: person
  tools: [`+ref+`]
`)
		if !slices.Contains(got, "memory_person") {
			t.Errorf("person memory with %s: %v", ref, got)
		}
	}
}
