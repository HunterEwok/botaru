// SPDX-License-Identifier: AGPL-3.0-only

package spec

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/HunterEwok/botaru/impl/internal/yamlx"
)

func TestVersionLater(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{{"v0.2", "v0.1", true}, {"v0.1", "v0.1", false}, {"v0.1", "v0.2", false}, {"v1.0", "v0.9", true}, {"v0.10", "v0.9", true}, {"later", "v0.1", true}} {
		if got := VersionLater(c.a, c.b); got != c.want {
			t.Errorf("VersionLater(%s, %s) = %v", c.a, c.b, got)
		}
	}
}

// The walk finds a field of a later version through $ref and the oneOf of the wrapper by kind,
// and only in the branch of the manifest's own kind (MAN-11). Below the claimed version it is
// the walk of the skeleton itself, without the fields built ahead.
func TestLaterFields(t *testing.T) {
	s := load(t)
	m := map[string]any{
		"apiVersion": "botaru/v1alpha1", "kind": "Schedule",
		"metadata": map[string]any{"name": "nightly", "tenant": "demo", "deprecated": true},
		"spec":     map[string]any{"cron": "0 3 * * *", "script": "cleanup", "locale": "en"},
	}
	if got := s.LaterFields("manifest:manifest", m, "v0.1"); !slices.Equal(got, []string{"/metadata/deprecated", "/spec/locale", "/spec/script"}) {
		t.Fatalf("v0.1: %v", got)
	}
	if got := s.LaterFields("manifest:manifest", m, Version); len(got) != 0 {
		t.Fatalf("%s: %v", Version, got)
	}
}

// At the v0.3 claim apply refuses the kinds and fields of deferred rules (D-114, MAN-11) and the
// fields of the profiles it does not claim; the v0.2 and v0.3 fields it serves pass.
func TestUnsupported(t *testing.T) {
	s := load(t)
	m := map[string]any{
		"apiVersion": "botaru/v1alpha1", "kind": "Schedule",
		"metadata": map[string]any{"name": "nightly", "tenant": "demo", "deprecated": true},
		"spec":     map[string]any{"cron": "0 3 * * *", "script": "cleanup", "locale": "en"},
	}
	got, err := s.Unsupported(m)
	if err != nil {
		t.Fatal(err)
	}
	// MAN-7 deprecated and SCR-12 Schedule.spec.script are deferred; locale is of v0.2.
	if want := []string{"/metadata/deprecated", "/spec/script"}; !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	m = map[string]any{"apiVersion": "botaru/v1alpha1", "kind": "Script", "metadata": map[string]any{"name": "x", "tenant": "demo"}, "spec": map[string]any{}}
	if got, _ := s.Unsupported(m); !slices.Equal(got, []string{"/kind"}) {
		t.Fatalf("a kind of a deferred rule: %v", got)
	}
	m = map[string]any{"apiVersion": "botaru/v1alpha1", "kind": "ToolPlugin", "metadata": map[string]any{"name": "sbx"},
		"spec": map[string]any{"placement": "sandbox", "tools": []any{map[string]any{"name": "run", "tier": 2}, map[string]any{"name": "list", "tier": 1}}}}
	if got, _ := s.Unsupported(m); !slices.Equal(got, []string{"/spec/placement", "/spec/tools/0/tier"}) {
		t.Fatalf("sandbox plugin: %v", got)
	}
	m = map[string]any{"apiVersion": "botaru/v1alpha1", "kind": "Group", "metadata": map[string]any{"name": "g", "tenant": "demo"}, "spec": map[string]any{"caps": []any{}}}
	if got, _ := s.Unsupported(m); len(got) != 0 {
		t.Fatalf("a v0.1 manifest: %v", got)
	}
	// The v0.3 fields of the incident class pass; the COM-18 reward fields of the charter do not.
	m = map[string]any{"apiVersion": "botaru/v1alpha1", "kind": "IncidentClass", "metadata": map[string]any{"name": "disk_full"},
		"spec": map[string]any{"title": "Disk full", "severity": "sev1", "problem_codes": []any{"disk_growth"}, "manager_accountable": true,
			"detect": map[string]any{"alarm": "alarm.disk.free.low.v1"}, "subject": map[string]any{"kind": "resource"}}}
	if got, _ := s.Unsupported(m); len(got) != 0 {
		t.Fatalf("v0.3 incident class: %v", got)
	}
	m = map[string]any{"apiVersion": "botaru/v1alpha1", "kind": "Governance", "metadata": map[string]any{"name": "charter", "tenant": "demo"},
		"spec": map[string]any{"commission": map[string]any{"accountability": "shadow", "reward_cap_eur_month": 5,
			"seats": []any{map[string]any{"lens": "security", "reward_profile": "strong"}}}}}
	if got, _ := s.Unsupported(m); !slices.Equal(got, []string{"/spec/commission/reward_cap_eur_month", "/spec/commission/seats/0/reward_profile"}) {
		t.Fatalf("reward fields: %v", got)
	}
	// A value surface: a webhook sink is refused and cannot be left out; shadow: off passes.
	m = map[string]any{"apiVersion": "botaru/v1alpha1", "kind": "Sink", "metadata": map[string]any{"name": "hook", "tenant": "demo"},
		"spec": map[string]any{"driver": "webhook", "base": "https://example.org/hook"}}
	if got, _ := s.Unsupported(m); !slices.Equal(got, []string{"/spec/driver"}) {
		t.Fatalf("webhook sink: %v", got)
	}
	if err := s.Droppable(m, "/spec/driver"); err == nil {
		t.Fatal("a webhook driver was droppable")
	}
	m = map[string]any{"apiVersion": "botaru/v1alpha1", "kind": "Commission", "metadata": map[string]any{"name": "c", "tenant": "demo"},
		"spec": map[string]any{"shadow": "off"}}
	if got, _ := s.Unsupported(m); len(got) != 0 {
		t.Fatalf("shadow off: %v", got)
	}
}

// Only a whole optional field may be left out of a manifest; a deferred kind and a value that
// gives a field its meaning may not (MAN-11, farmctl -drop-unsupported).
func TestDroppable(t *testing.T) {
	s := load(t)
	cases := []struct {
		m    map[string]any
		ptr  string
		drop bool
	}{
		{map[string]any{"apiVersion": "botaru/v1alpha1", "kind": "Tenant", "metadata": map[string]any{"name": "demo"},
			"spec": map[string]any{"operator_access": map[string]any{"debug_journal": "notify"}}}, "/spec/operator_access/debug_journal", true},
		{map[string]any{"apiVersion": "botaru/v1alpha1", "kind": "Schedule", "metadata": map[string]any{"name": "n", "tenant": "demo"},
			"spec": map[string]any{"cron": "0 3 * * *", "script": "cleanup"}}, "/spec/script", true},
		{map[string]any{"apiVersion": "botaru/v1alpha1", "kind": "Script", "metadata": map[string]any{"name": "x", "tenant": "demo"}, "spec": map[string]any{}}, "/kind", false},
		{map[string]any{"apiVersion": "botaru/v1alpha1", "kind": "ToolPlugin", "metadata": map[string]any{"name": "sbx"},
			"spec": map[string]any{"placement": "sandbox", "tools": []any{map[string]any{"name": "run", "tier": 2}}}}, "/spec/tools/0/tier", false},
	}
	for i, c := range cases {
		err := s.Droppable(c.m, c.ptr)
		if (err == nil) != c.drop {
			t.Errorf("case %d %s: droppable %v, err %v", i, c.ptr, c.drop, err)
		}
	}
	if err := s.Droppable(cases[0].m, "/spec/name"); err == nil {
		t.Error("a pointer the skeleton does not refuse was droppable")
	}
}

// A field listed in Ahead is accepted below the version that marks it; the other fields of
// that version stay refused. The claim is v0.3 and Ahead is empty, so the walk is run at v0.1.
func TestAhead(t *testing.T) {
	s := load(t)
	saved := Ahead
	defer func() { Ahead = saved }()
	Ahead = map[string][]AheadField{"Schedule": {{Version: "v0.2", Ptr: "/spec/locale"}}, "Catalog": {{Version: "v0.2", Ptr: "/spec/hosts/*/maintenance_hold"}}}
	m := map[string]any{
		"apiVersion": "botaru/v1alpha1", "kind": "Schedule",
		"metadata": map[string]any{"name": "nightly", "tenant": "demo"},
		"spec":     map[string]any{"cron": "0 3 * * *", "script": "cleanup", "locale": "en"},
	}
	if got := s.LaterFields("manifest:manifest", m, "v0.1"); !slices.Equal(got, []string{"/spec/script"}) {
		t.Fatalf("schedule: %v", got)
	}
	m = map[string]any{"apiVersion": "botaru/v1alpha1", "kind": "Catalog", "metadata": map[string]any{"name": "c", "tenant": "demo"},
		"spec": map[string]any{"hosts": []any{map[string]any{"name": "web1", "maintenance_hold": true}}}}
	if got := s.LaterFields("manifest:manifest", m, "v0.1"); len(got) != 0 {
		t.Fatalf("catalog: %v", got)
	}
}

// Every deferred rule of conformance/requirements.yaml has a surface in Deferred, or an entry
// of SurfaceNone with a note, and Deferred names no rule that is not deferred (D-114, D3).
// Every operation surface is a row of the operation map and every tool surface a built-in
// tool, so an entry cannot silently miss the name it guards.
func TestDeferredCoversRegistry(t *testing.T) {
	s := load(t)
	raw, err := os.ReadFile(filepath.Join(s.Root, "conformance", "requirements.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	if err := yamlx.Unmarshal(raw, &rows); err != nil {
		t.Fatal(err)
	}
	deferred := map[string]bool{}
	for _, r := range rows {
		if r["status"] == "deferred" {
			deferred[r["id"].(string)] = true
		}
	}
	if len(deferred) == 0 {
		t.Fatal("no deferred rules read")
	}
	ops, err := s.Registry("spec/ports/operations.yaml")
	if err != nil {
		t.Fatal(err)
	}
	opSet := map[string]bool{}
	for _, e := range ops.Entries {
		opSet[fmt.Sprint(e["port"], ".", e["op"])] = true
	}
	tools, err := s.Registry("spec/model/builtin-tools.yaml")
	if err != nil {
		t.Fatal(err)
	}
	toolSet := map[string]bool{}
	for _, e := range tools.Entries {
		toolSet[fmt.Sprint(e["id"])] = true
	}
	covered := map[string]bool{}
	for _, e := range Deferred {
		if !deferred[e.Rule] {
			t.Errorf("%s is in Deferred but not deferred in the registry", e.Rule)
		}
		covered[e.Rule] = true
		switch e.Surface {
		case SurfaceNone:
			if e.Note == "" {
				t.Errorf("%s has no surface and no note", e.Rule)
			}
		case SurfaceOp:
			if !opSet[e.Where] {
				t.Errorf("%s: %s is not an operation of the map", e.Rule, e.Where)
			}
		case SurfaceTool:
			if !toolSet[e.Where] {
				t.Errorf("%s: %s is not a built-in tool", e.Rule, e.Where)
			}
		case SurfaceManifest:
			if e.Kind == "" || !strings.HasPrefix(e.Where, "/") {
				t.Errorf("%s: a manifest surface without a kind or pointer", e.Rule)
			}
		case SurfaceHello:
			if e.Field == "" {
				t.Errorf("%s: a hello surface without a field", e.Rule)
			}
		default:
			t.Errorf("%s: unknown surface %q", e.Rule, e.Surface)
		}
	}
	for id := range deferred {
		if !covered[id] {
			t.Errorf("the deferred rule %s has no entry in Deferred", id)
		}
		if Offered(id) {
			t.Errorf("the deferred rule %s is offered", id)
		}
	}
}

// A call reaches a deferred surface by its operation, a field of its body or a value of it.
func TestDeferredCall(t *testing.T) {
	for _, c := range []struct {
		surface, where string
		body           map[string]any
		rule, ptr      string
	}{
		{SurfaceOp, "P1.obs.append", map[string]any{}, "OBV-8", ""},
		{SurfaceOp, "P1.enqueue", map[string]any{"mode": "validate"}, "PRM-11", "/mode"},
		{SurfaceOp, "P1.enqueue", map[string]any{"type": "x"}, "", ""},
		{SurfaceOp, "P2.chat.bind", map[string]any{"assistant": false}, "", ""},
		{SurfaceOp, "P2.chat.bind", map[string]any{"assistant": true}, "DSP-10", "/assistant"},
		{SurfaceTool, "control_ops", map[string]any{"op": "pin_task"}, "MGR-12", "/op"},
		{SurfaceTool, "control_ops", map[string]any{"op": "hold"}, "", ""},
		{SurfaceTool, "skill_read", nil, "SKL-4", ""},
		{SurfaceHello, "", map[string]any{"runtime": map[string]any{"skills": "farm"}}, "SKL-4", "/runtime/skills"},
		{SurfaceHello, "", map[string]any{"runtime": map[string]any{"skills": "runtime_files"}}, "", ""},
	} {
		h, ok := DeferredCall(c.surface, c.where, c.body)
		if ok != (c.rule != "") || h.Rule != c.rule || h.Ptr != c.ptr {
			t.Errorf("%s %s %v: %v %v", c.surface, c.where, c.body, h, ok)
		}
	}
}
