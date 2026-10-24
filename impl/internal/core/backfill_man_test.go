// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/HunterEwok/botaru/impl/internal/manifest"
	"github.com/HunterEwok/botaru/impl/internal/spec"
	"github.com/HunterEwok/botaru/impl/internal/testbench"
)

// helloManifests is the manifest set the bench applied, read again.
func helloManifests(t *testing.T) []map[string]any {
	t.Helper()
	ms, _ := helloSet(t)
	return ms
}

// storedHash is the hash of a stored manifest, "" when the store has none.
func storedHash(t *testing.T, b *bench, kind, tenant, name string) string {
	t.Helper()
	var h string
	err := b.c.st.Pool.QueryRow(context.Background(), `SELECT coalesce((SELECT hash FROM manifests WHERE kind = $1 AND tenant = $2 AND name = $3), '')`, kind, tenant, name).Scan(&h)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func specMap(ms []map[string]any, kind, name string) map[string]any {
	for _, m := range ms {
		if m["kind"] == kind && m["metadata"].(map[string]any)["name"] == name {
			return m["spec"].(map[string]any)
		}
	}
	return nil
}

func changeOps(res *ApplyResult) map[string]string {
	out := map[string]string{}
	for _, ch := range res.Changes {
		out[ch.Kind+"/"+ch.Name] = ch.Op
	}
	return out
}

// MAN-2: apply brings the store to the set; the same set again changes nothing, dry_run shows
// the plan without writing, and an object missing from the set stays, reported as orphaned,
// until an explicit delete.
func TestBackfillApplyIdempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newBench(t)
	commit := "1111111111111111111111111111111111111111"
	res, err := b.c.Apply(ctx, farmOf(b.op), ApplyRequest{Commit: commit, Manifests: helloManifests(t)})
	if err != nil || !res.Applied {
		t.Fatalf("apply again: %+v %v", res, err)
	}
	for _, ch := range res.Changes {
		if ch.Op != "unchanged" {
			t.Fatalf("MAN-2: the same set changed %+v", ch)
		}
	}
	before := storedHash(t, b, "TaskType", "", "hello.greet.v1")
	if before == "" {
		t.Fatal("no stored task type")
	}
	ms := helloManifests(t)
	specMap(ms, "TaskType", "hello.greet.v1")["title"] = "Greeting, changed"
	res, err = b.c.Apply(ctx, farmOf(b.op), ApplyRequest{Commit: commit, Manifests: ms, DryRun: true})
	if err != nil || len(res.Errors) > 0 {
		t.Fatalf("dry run: %+v %v", res, err)
	}
	if changeOps(res)["TaskType/hello.greet.v1"] != "update" {
		t.Fatalf("MAN-2: the plan does not show the update: %+v", res.Changes)
	}
	if storedHash(t, b, "TaskType", "", "hello.greet.v1") != before {
		t.Fatal("MAN-2: dry_run wrote the store")
	}
	// A group leaves the set: it stays, shown as orphaned, until the explicit delete.
	extra := map[string]any{"apiVersion": "botaru/v1alpha1", "kind": "Group",
		"metadata": map[string]any{"name": "fx-extra", "tenant": "demo"},
		"spec":     map[string]any{"caps": []any{"task:hello.greet.v1"}, "data": []any{"summary"}}}
	res, err = b.c.Apply(ctx, farmOf(b.op), ApplyRequest{Commit: commit, Manifests: append(helloManifests(t), extra)})
	if err != nil || !res.Applied || changeOps(res)["Group/fx-extra"] != "create" {
		t.Fatalf("apply with a second group: %+v %v", res, err)
	}
	res, err = b.c.Apply(ctx, farmOf(b.op), ApplyRequest{Commit: commit, Manifests: helloManifests(t)})
	if err != nil || !res.Applied {
		t.Fatalf("apply without the group: %+v %v", res, err)
	}
	if changeOps(res)["Group/fx-extra"] != "orphaned" || storedHash(t, b, "Group", "demo", "fx-extra") == "" {
		t.Fatalf("MAN-2: a group missing from git was not kept as orphaned: %+v", res.Changes)
	}
	res, err = b.c.Apply(ctx, farmOf(b.op), ApplyRequest{Commit: commit, Manifests: helloManifests(t), Delete: []ApplyDelete{{Kind: "Group", Tenant: "demo", Name: "fx-extra"}}})
	if err != nil || !res.Applied || changeOps(res)["Group/fx-extra"] != "delete" || storedHash(t, b, "Group", "demo", "fx-extra") != "" {
		t.Fatalf("MAN-2: the explicit delete: %+v %v", res, err)
	}
}

// MAN-6: in a set of a role, a task type and a group the third object has an error: apply
// refuses the whole set and all three objects keep their previous state; the answer lists the
// errors of the rejected set.
func TestBackfillApplyAtomic(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newBench(t)
	role, typ, group := storedHash(t, b, "Role", "", "hello"), storedHash(t, b, "TaskType", "", "hello.greet.v1"), storedHash(t, b, "Group", "demo", "hello")
	if role == "" || typ == "" || group == "" {
		t.Fatalf("stored hashes %q %q %q", role, typ, group)
	}
	ms := helloManifests(t)
	specMap(ms, "Role", "hello")["views"] = []any{"status.tasks"}
	specMap(ms, "TaskType", "hello.greet.v1")["title"] = "Greeting, changed"
	specMap(ms, "Group", "hello")["caps"] = []any{"task:hello.greet.v1", "action.approve:*"}
	res, err := b.c.Apply(ctx, farmOf(b.op), ApplyRequest{Commit: "2222222222222222222222222222222222222222", Manifests: ms})
	if err != nil {
		t.Fatal(err)
	}
	// Only the third object is wrong; the first two would pass on their own. The group with
	// members also becomes privileged (PRM-13).
	if res.Applied || !slices.ContainsFunc(res.Errors, func(e ApplyError) bool { return e.Code == "cap_star" }) ||
		slices.ContainsFunc(res.Errors, func(e ApplyError) bool {
			return e.Code != "cap_star" && e.Code != "privileged_group" || e.Kind != "Group"
		}) {
		t.Fatalf("MAN-6: the set with a bad group: %+v", res)
	}
	if storedHash(t, b, "Role", "", "hello") != role || storedHash(t, b, "TaskType", "", "hello.greet.v1") != typ || storedHash(t, b, "Group", "demo", "hello") != group {
		t.Fatal("MAN-6: a rejected set changed an object")
	}
}

// INJ-1: the closed-schema meta-schema refuses the terminal send_keys(text) of the fixture, a
// free string with any pattern, and passes the closed leaves.
func TestBackfillClosedSchema(t *testing.T) {
	t.Parallel()
	root, err := spec.FindRoot("")
	if err != nil {
		t.Fatal(err)
	}
	sp, err := testbench.SharedSpec(root)
	if err != nil {
		t.Fatal(err)
	}
	fx, err := spec.ReadYAMLFile(filepath.Join(root, "conformance/fixtures/schemas/invalid/terminal-send-keys.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	m := fx.(map[string]any)
	if err := sp.Validate(m["schema"].(string), m["data"]); err == nil {
		t.Fatal("INJ-1: a free string with a pattern passed the closed schema")
	}
	for name, leaf := range map[string]any{
		"enum":     map[string]any{"enum": []any{"a", "b"}},
		"const":    map[string]any{"const": "a"},
		"enumfrom": map[string]any{"enum_from": "catalog.hosts"},
		"integer":  map[string]any{"type": "integer", "minimum": 0, "maximum": 10},
		"boolean":  map[string]any{"type": "boolean"},
		"format":   map[string]any{"type": "string", "x-botaru-format": "id"},
	} {
		if err := sp.Validate(m["schema"].(string), leaf); err != nil {
			t.Errorf("INJ-1: the closed leaf %s does not pass: %v", name, err)
		}
	}
	for name, leaf := range map[string]any{
		"integer without bounds": map[string]any{"type": "integer"},
		"string with a pattern":  map[string]any{"type": "string", "pattern": "^[a-z]+$", "maxLength": 10},
	} {
		if err := sp.Validate(m["schema"].(string), leaf); err == nil {
			t.Errorf("INJ-1: %s passed the closed schema", name)
		}
	}
}

// INJ-3: untrusted_output is required in the manifest of a tool.
func TestBackfillUntrustedOutputRequired(t *testing.T) {
	t.Parallel()
	c := lintCore(t)
	doc := `apiVersion: botaru/v1alpha1
kind: ToolPlugin
metadata: {name: fx-logs}
spec:
  placement: host
  network: {egress: []}
  tools:
    - {name: tail, access: read, tier: 1, output_class: raw_logs, redact: [ip, email, secret], target: any}
`
	ds, err := manifest.ParseYAML(strings.NewReader(doc), "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.spec.Validate("manifest:tool-plugin", ds[0].Value["spec"]); err == nil {
		t.Fatal("INJ-3: a tool without untrusted_output passed the kind schema")
	}
}
