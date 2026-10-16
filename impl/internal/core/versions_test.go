// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"path/filepath"
	"slices"
	"strconv"
	"testing"

	"github.com/HunterEwok/botaru/impl/internal/manifest"
	"github.com/HunterEwok/botaru/impl/internal/spec"
	"github.com/HunterEwok/botaru/impl/internal/testbench"
)

// helloSet is the hello-bot manifest set and the index of each kind in it.
func helloSet(t *testing.T) ([]map[string]any, map[string]int) {
	t.Helper()
	root, err := spec.FindRoot("")
	if err != nil {
		t.Fatal(err)
	}
	ms, err := testbench.HelloBotManifests(root)
	if err != nil {
		t.Fatal(err)
	}
	at := map[string]int{}
	for i, m := range ms {
		at[m["kind"].(string)] = i
	}
	return ms, at
}

// A core that claims v0.1 with [core] refuses a kind and fields of a later version and of the
// feature profiles it does not claim, with the path; nothing of the set is applied, and the
// same set without them passes (MAN-11, blackbox:core/apply-later-version).
func TestApplyLaterVersion(t *testing.T) {
	ctx := context.Background()
	b := newBench(t)
	// The test uses EvalReports as a later kind: this build accepts it ahead, so the list is
	// emptied for the test.
	saved := spec.Ahead
	defer func() { spec.Ahead = saved }()
	spec.Ahead = map[string][]string{}
	ms, at := helloSet(t)
	tt := ms[at["TaskType"]]["spec"].(map[string]any)
	tt["title"] = "Greeting, changed"
	tt["await"] = map[string]any{"event": "ext.monitor.problem.v1", "timeout": "2h"}
	ms[at["Tenant"]]["spec"].(map[string]any)["retention"] = map[string]any{"journals": "30d"}
	ms[at["Tenant"]]["spec"].(map[string]any)["operator_access"] = map[string]any{"debug_journal": "notify"}
	ms[at["Role"]]["spec"].(map[string]any)["skills"] = []any{}
	extra := []map[string]any{
		{"apiVersion": "botaru/v1alpha1", "kind": "EvalReports", "metadata": map[string]any{"name": "reports"},
			"spec": map[string]any{"reports": []any{}}},
		{"apiVersion": "botaru/v1alpha1", "kind": "Catalog", "metadata": map[string]any{"name": "demo", "tenant": "demo"},
			"spec": map[string]any{"resources": []any{
				map[string]any{"id": "manuals", "title": "Manuals"},
				map[string]any{"id": "docs", "title": "Documents", "kind": "corpus"},
			}}},
		{"apiVersion": "botaru/v1alpha1", "kind": "TaskType", "metadata": map[string]any{"name": "data.sandboxed.v1"},
			"spec": map[string]any{"title": "Sandbox", "worker_kind": "agent", "purpose": "work", "tier": 2, "data_class": "internal",
				"instruction": "Run.", "params_schema": map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{}},
				"tree_budget_eur": 0.05}},
	}
	all := append(slices.Clone(ms), extra...)
	n := len(ms)
	res, err := b.c.Apply(ctx, b.op, ApplyRequest{Commit: "4444444444444444444444444444444444444444", Manifests: all})
	if err != nil {
		t.Fatal(err)
	}
	if res.Applied {
		t.Fatal("a set with later fields was applied")
	}
	var got []string
	for _, e := range res.Errors {
		if e.Code != "version_unsupported" {
			t.Fatalf("error %+v", e)
		}
		got = append(got, e.Path)
	}
	want := []string{
		"/manifests/" + itoa(at["TaskType"]) + "/spec/await",
		"/manifests/" + itoa(at["Tenant"]) + "/spec/operator_access/debug_journal",
		"/manifests/" + itoa(at["Tenant"]) + "/spec/retention",
		"/manifests/" + itoa(at["Role"]) + "/spec/skills",
		"/manifests/" + itoa(n) + "/kind",
		"/manifests/" + itoa(n+1) + "/spec/resources/1/kind",
		"/manifests/" + itoa(n+2) + "/spec/tier",
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("paths\n got %v\nwant %v", got, want)
	}
	cat, err := b.c.Catalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if cat.TaskTypes["hello.greet.v1"].Title != "Greeting" {
		t.Fatal("a refused set changed the store")
	}

	// The same set without these fields and objects passes.
	ms, _ = helloSet(t)
	ms[at["TaskType"]]["spec"].(map[string]any)["title"] = "Greeting, changed"
	extra[1]["spec"] = map[string]any{"resources": []any{map[string]any{"id": "manuals", "title": "Manuals"}}}
	res, err = b.c.Apply(ctx, b.op, ApplyRequest{Commit: "5555555555555555555555555555555555555555", Manifests: append(ms, extra[1])})
	if err != nil || !res.Applied {
		t.Fatalf("apply: %+v %v", res, err)
	}
	if cat, _ := b.c.Catalog(ctx); cat.TaskTypes["hello.greet.v1"].Title != "Greeting, changed" {
		t.Fatal("the set was not applied")
	}
}

func itoa(i int) string { return strconv.Itoa(i) }

// The set lint sees the store as it will be after the apply: an object of an earlier apply
// that stays counts in the checks across objects.
func TestApplyLintsStore(t *testing.T) {
	ctx := context.Background()
	b := newBench(t)
	cat := map[string]any{"apiVersion": "botaru/v1alpha1", "kind": "Catalog", "metadata": map[string]any{"name": "demo", "tenant": "demo"},
		"spec": map[string]any{"hosts": []any{map[string]any{"id": "web1"}}}}
	if res, err := b.c.Apply(ctx, b.op, ApplyRequest{Commit: "6666666666666666666666666666666666666666", Manifests: []map[string]any{cat}}); err != nil || !res.Applied {
		t.Fatalf("catalog: %+v %v", res, err)
	}
	root, err := spec.FindRoot("")
	if err != nil {
		t.Fatal(err)
	}
	plugin := func(egress string) map[string]any {
		ds, err := manifest.ReadFiles([]string{filepath.Join(root, "examples/tools/checks/tool.yaml")})
		if err != nil {
			t.Fatal(err)
		}
		v := ds[0].Value
		v["spec"].(map[string]any)["network"] = map[string]any{"egress": []any{egress}}
		return v
	}
	// The catalog of the store is visible: a machine of it is a known egress.
	if res, err := b.c.Apply(ctx, b.op, ApplyRequest{Commit: "7777777777777777777777777777777777777777", Manifests: []map[string]any{plugin("web1")}}); err != nil || !res.Applied {
		t.Fatalf("a plugin to a machine of the stored catalog: %+v %v", res, err)
	}
	res, err := b.c.Apply(ctx, b.op, ApplyRequest{Commit: "7777777777777777777777777777777777777777", Manifests: []map[string]any{plugin("db9")}})
	if err != nil || res.Applied || len(res.Errors) != 1 || res.Errors[0].Code != "egress_unknown" {
		t.Fatalf("a plugin to an unknown machine: %+v %v", res, err)
	}
	// Deleting the catalog in the same apply removes its machines from the check.
	res, err = b.c.Apply(ctx, b.op, ApplyRequest{Commit: "7777777777777777777777777777777777777777", Manifests: []map[string]any{plugin("web1")},
		Delete: []ApplyDelete{{Kind: "Catalog", Tenant: "demo", Name: "demo"}}})
	if err != nil || res.Applied || len(res.Errors) != 1 || res.Errors[0].Code != "egress_unknown" {
		t.Fatalf("a plugin to a machine of a deleted catalog: %+v %v", res, err)
	}
}
