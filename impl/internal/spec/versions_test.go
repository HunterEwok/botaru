// SPDX-License-Identifier: AGPL-3.0-only

package spec

import (
	"slices"
	"testing"
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
// and only in the branch of the manifest's own kind (MAN-11).
func TestUnsupported(t *testing.T) {
	s := load(t)
	saved := Ahead
	defer func() { Ahead = saved }()
	Ahead = nil // the walk of the skeleton itself, without the fields built ahead
	m := map[string]any{
		"apiVersion": "botaru/v1alpha1", "kind": "Schedule",
		"metadata": map[string]any{"name": "nightly", "tenant": "demo", "deprecated": true},
		"spec":     map[string]any{"cron": "0 3 * * *", "script": "cleanup", "locale": "en"},
	}
	got, err := s.Unsupported(m)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/metadata/deprecated", "/spec/locale", "/spec/script"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	m = map[string]any{"apiVersion": "botaru/v1alpha1", "kind": "Script", "metadata": map[string]any{"name": "x", "tenant": "demo"}, "spec": map[string]any{}}
	if got, _ := s.Unsupported(m); !slices.Equal(got, []string{"/kind"}) {
		t.Fatalf("a kind of v0.2: %v", got)
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
}

// Only a whole optional field may be left out of a manifest; a later kind and a value that
// gives a field its meaning may not (MAN-11, farmctl -drop-unsupported).
func TestDroppable(t *testing.T) {
	s := load(t)
	saved := Ahead
	defer func() { Ahead = saved }()
	Ahead = nil // the walk of the skeleton itself, without the fields built ahead
	cases := []struct {
		m    map[string]any
		ptr  string
		drop bool
	}{
		{map[string]any{"apiVersion": "botaru/v1alpha1", "kind": "Tenant", "metadata": map[string]any{"name": "demo"},
			"spec": map[string]any{"operator_access": map[string]any{"debug_journal": "notify"}}}, "/spec/operator_access/debug_journal", true},
		{map[string]any{"apiVersion": "botaru/v1alpha1", "kind": "Schedule", "metadata": map[string]any{"name": "n", "tenant": "demo"},
			"spec": map[string]any{"cron": "0 3 * * *", "locale": "en"}}, "/spec/locale", true},
		{map[string]any{"apiVersion": "botaru/v1alpha1", "kind": "Script", "metadata": map[string]any{"name": "x", "tenant": "demo"}, "spec": map[string]any{}}, "/kind", false},
		{map[string]any{"apiVersion": "botaru/v1alpha1", "kind": "Catalog", "metadata": map[string]any{"name": "demo", "tenant": "demo"},
			"spec": map[string]any{"resources": []any{map[string]any{"id": "docs", "title": "Documents", "kind": "corpus"}}}}, "/spec/resources/0/kind", false},
		{map[string]any{"apiVersion": "botaru/v1alpha1", "kind": "Catalog", "metadata": map[string]any{"name": "demo", "tenant": "demo"},
			"spec": map[string]any{"resources": []any{map[string]any{"id": "rows", "title": "Rows", "kind": "dataset"}}}}, "/spec/resources/0/kind", false},
		{map[string]any{"apiVersion": "botaru/v1alpha1", "kind": "Channel", "metadata": map[string]any{"name": "mail", "tenant": "demo"},
			"spec": map[string]any{"direction": "in"}}, "/spec/direction", false},
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

// A field listed in Ahead is accepted before the claimed version reaches it; the other fields
// of that version stay refused.
func TestAhead(t *testing.T) {
	s := load(t)
	saved := Ahead
	defer func() { Ahead = saved }()
	Ahead = map[string][]string{"Schedule": {"/spec/locale"}, "Catalog": {"/spec/hosts/*/maintenance_hold"}}
	m := map[string]any{
		"apiVersion": "botaru/v1alpha1", "kind": "Schedule",
		"metadata": map[string]any{"name": "nightly", "tenant": "demo"},
		"spec":     map[string]any{"cron": "0 3 * * *", "script": "cleanup", "locale": "en"},
	}
	if got, _ := s.Unsupported(m); !slices.Equal(got, []string{"/spec/script"}) {
		t.Fatalf("schedule: %v", got)
	}
	m = map[string]any{"apiVersion": "botaru/v1alpha1", "kind": "Catalog", "metadata": map[string]any{"name": "c", "tenant": "demo"},
		"spec": map[string]any{"hosts": []any{map[string]any{"name": "web1", "maintenance_hold": true}}}}
	if got, _ := s.Unsupported(m); len(got) != 0 {
		t.Fatalf("catalog: %v", got)
	}
}
