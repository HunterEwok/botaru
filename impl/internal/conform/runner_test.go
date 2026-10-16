// SPDX-License-Identifier: AGPL-3.0-only

package conform

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/HunterEwok/botaru/impl/internal/spec"
	"github.com/HunterEwok/botaru/impl/internal/texts"
	"github.com/HunterEwok/botaru/impl/internal/yamlx"
)

// testRunner is a runner of the v0.1 claim over in-process benches.
func testRunner(t *testing.T) *Runner {
	t.Helper()
	root, err := spec.FindRoot("")
	if err != nil {
		t.Fatal(err)
	}
	sp, err := spec.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := texts.Load(root, "")
	if err != nil {
		t.Fatal(err)
	}
	r, err := NewRunner(root, claim, &inProcess{t: t, root: root, sp: sp, tx: tx})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// runYAML runs the cases of one fixture written as YAML, each on a fresh bench, and returns
// the results by case id. With table set the file is a decision table of that kind.
func runYAML(t *testing.T, r *Runner, doc string, table ...*tableInfo) map[string]Result {
	t.Helper()
	if r.tables == nil {
		r.tables = map[string]*tableInfo{}
	}
	r.tables["test.yaml"] = nil
	if len(table) > 0 {
		r.tables["test.yaml"] = table[0]
	}
	var m map[string]any
	if err := yamlx.Unmarshal([]byte(doc), &m); err != nil {
		t.Fatal(err)
	}
	var cases []*Case
	flatten(&cases, "test.yaml", str(m["id"]), nil, m, &Case{})
	out := map[string]Result{}
	for _, c := range cases {
		out[c.ID] = r.runCase(context.Background(), c)
	}
	return out
}

func wantStatus(t *testing.T, got map[string]Result, want map[string]Status) {
	t.Helper()
	for id, st := range want {
		if res, ok := got[id]; !ok || res.Status != st {
			t.Errorf("%s: %s %s, want %s", id, res.Status, res.Detail, st)
		}
	}
}

// TestSeedPrivilegedGroup: a privileged seed group becomes active only through the person's
// verification code (PRM-13). The runner plays /me in the person's direct chat through its own
// channel worker, reads the code from the reply and repeats the grant with it; the permission
// of the group then holds in C2 (authorize.eval), and the code of another person does not
// activate the grant.
func TestSeedPrivilegedGroup(t *testing.T) {
	r := testRunner(t)
	got := runYAML(t, r, `
id: t-prm13
rules: [PRM-2]
cases:
- id: staff-active
  given:
    tenant: demo
    people:
    - {principal: human:bob, groups: [staff]}
  steps:
  - call:
      port: C2
      op: authorize
      request:
        principal: {kind: human, id: 'human:p_01JB3Q7N0M2V8XK4DGT5RZP9WA', tenant: demo}
        action: task:ops.investigate.v1
        resource: {tenant: demo, kind: task_type, id: ops.investigate.v1}
  expect: {result: ok}
`, &tableInfo{authorize: true, tenantKind: "test"})
	wantStatus(t, got, map[string]Status{"staff-active": Passed})

	// The code is the person's own: alice's code does not activate bob's grant.
	ctx := context.Background()
	ep, stop, err := r.Bench.Fresh(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	x := newExec(r, ep, &Case{Given: map[string]any{}})
	if err := x.seedBase(ctx); err != nil {
		t.Fatal(err)
	}
	bob := "human:p_01JB3Q7N0M2V8XK4DGT5RZP9WA"
	if err := x.addPerson(ctx, bob, "telegram:1002"); err != nil {
		t.Fatal(err)
	}
	if err := x.addPerson(ctx, "human:p_01JB3Q7N0M2V8XK4DGT5RZP9WB", "telegram:1001"); err != nil {
		t.Fatal(err)
	}
	code, err := x.verificationCode(ctx, "telegram:1001")
	if err != nil {
		t.Fatal(err)
	}
	status, resp, err := x.post(ctx, ep.P2+"/v1/p2/grant", ep.Operators["demo"], map[string]any{"op": "grant", "subject": bob, "group": "staff", "code": code})
	if err != nil {
		t.Fatal(err)
	}
	if status == 200 && str(resp["state"]) == "active" {
		t.Fatalf("another person's code activated the grant: %v", resp)
	}
	if o, err := x.seedGrant(ctx, bob, "telegram:1002", "staff"); err != nil || o.Result != "ok" {
		t.Fatalf("seed grant with bob's code: %v %v", o, err)
	}
}

// TestHoldScope: a fixture hold scope gets the tenant of the case, and a tenant scope written
// with the tenant as its name names it in tenant (hold schema).
func TestHoldScope(t *testing.T) {
	x := &exec{c: &Case{Given: map[string]any{"tenant": "demo2"}}}
	for _, tc := range []struct {
		in, want map[string]any
	}{
		{map[string]any{"kind": "tenant", "name": "demo"}, map[string]any{"kind": "tenant", "tenant": "demo"}},
		{map[string]any{"kind": "tasktype", "name": "ops.investigate.v1"}, map[string]any{"kind": "tasktype", "name": "ops.investigate.v1", "tenant": "demo2"}},
		{map[string]any{"kind": "farm"}, map[string]any{"kind": "farm"}},
		{map[string]any{"kind": "role", "tenant": "demo", "name": "hello"}, map[string]any{"kind": "role", "tenant": "demo", "name": "hello"}},
	} {
		if got := x.holdScope(tc.in); !sameJSON(got, tc.want) {
			t.Errorf("%v: %v, want %v", tc.in, got, tc.want)
		}
	}
}

func sameJSON(a, b map[string]any) bool {
	return len(a) == len(b) && strings.TrimSpace(jsonText(a)) == strings.TrimSpace(jsonText(b))
}

// TestManifestSchemaRefusal: a manifest that breaks the schema of its kind is refused at the
// request schema of P2 apply; the runner counts the refusal as the lint error schema_invalid of
// MAN-1 for the stages schema, lint and apply, and compares the field with every place the
// manifest breaks its schema, so a manifest broken elsewhere as well fails the case instead of
// passing it (BUD-9 here). A given manifest of an object of the seed (the demo tenant) changes
// only the keys it names.
func TestManifestSchemaRefusal(t *testing.T) {
	r := testRunner(t)
	got := runYAML(t, r, `
id: t-schema
rules: [BUD-9]
cases:
- id: extra-field
  given:
    manifest:
      kind: TaskType
      metadata: {name: ops.investigate.v1}
      spec:
        title: Investigation
        worker_kind: agent
        purpose: work
        tier: 1
        data_class: internal
        params_schema: {type: object, additionalProperties: false, properties: {}}
        tree_budget_eur: 0.4
        max_tokens: 4000
  expect: {result: rejected, stage: apply, field: max_tokens}
- id: missing-field
  given:
    manifest:
      kind: TaskType
      metadata: {name: review.risk.v1}
      spec:
        title: Risk review
        worker_kind: agent
        purpose: review
        tier: 0
        data_class: internal
        params_schema: {type: object, additionalProperties: false, properties: {}}
  expect: {result: rejected, stage: schema, field: spec.tree_budget_eur}
- id: partial
  given:
    manifest:
      kind: TaskType
      metadata: {name: ops.other.v1}
      spec: {purpose: work, tree_budget_eur: 0.4, max_tokens: 4000}
  expect: {result: rejected, stage: apply, field: max_tokens}
- id: on-base
  given:
    manifest:
      kind: Tenant
      metadata: {name: demo}
      spec: {timezone: Europe/Berlin}
  expect: {result: ok}
`)
	wantStatus(t, got, map[string]Status{"extra-field": Passed, "missing-field": Passed, "partial": Failed, "on-base": Passed})
	if d := got["partial"].Detail; !strings.Contains(d, "besides the field max_tokens") {
		t.Errorf("partial: %s", d)
	}
}

func TestFieldAt(t *testing.T) {
	for _, tc := range []struct {
		path, msg, f string
		want         bool
	}{
		{"/manifests/0/spec/cron", "", "cron", true},
		{"/manifests/0/spec/insight/merge_window", "", "spec.insight.merge_window", true},
		{"/manifests/0/spec/insights/extra_guards/0", "", "spec.insights.extra_guards", true},
		{"/manifests/0/spec", "&{[max_tokens]}", "max_tokens", true},
		{"/manifests/0/spec", "&{[title]}", "max_tokens", false},
		{"/manifests/0/spec/retry", "&{[max_tokens]}", "max_tokens", false},
		{"/until", "", "until", true},
		{"/manifests/0/spec", "", "timezone", false},
	} {
		if got := fieldAt(tc.path, tc.msg, tc.f); got != tc.want {
			t.Errorf("fieldAt(%q, %q, %q) = %v", tc.path, tc.msg, tc.f, got)
		}
	}
}

func jsonText(m map[string]any) string {
	b, _ := json.Marshal(m)
	return string(b)
}
