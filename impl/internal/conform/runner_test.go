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
// passing it (BUD-9 here). A fixture manifest that lacks required fields other than the checked
// one leaves the case unchecked, since its refusal comes from the fixture. A given manifest of an
// object of the seed (the demo tenant) changes only the keys it names; expect.apply is the outcome
// of the given manifests, and given.lint is a label (COM-8 here).
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
      spec: {timezone: UTC}
  expect: {result: ok}
- id: apply-fact
  given:
    lint: budget/types
    manifest:
      kind: Commission
      metadata: {name: default, tenant: demo}
      spec:
        weights: {value: 4}
  expect:
    apply: {result: rejected, field: weights.value}
- id: apply-unnamed
  given:
    manifest:
      kind: Commission
      spec:
        weights: {value: 4}
  expect:
    apply: {result: rejected}
`)
	wantStatus(t, got, map[string]Status{"extra-field": Passed, "missing-field": Passed, "partial": Unchecked, "on-base": Passed,
		"apply-fact": Passed, "apply-unnamed": Unchecked})
	if d := got["partial"].Detail; !strings.Contains(d, "lacks the required spec.title") {
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

// TestLintFragments: a lint fragment completes the example object of its kind and is applied
// with dry_run on top of the seed (conformance/runner.md, verb lint), after the same set without
// the fragment as the control. Rows of INJ-14, APR-5 and INJ-3 here; a fragment in given runs as
// the row's lint, a fragment in prose and the deployment description stay unchecked.
func TestLintFragments(t *testing.T) {
	r := testRunner(t)
	got := runYAML(t, r, `
id: t-lint
rules: [INJ-14]
cases:
- id: shell
  given:
    cmd_yaml: {argv: [/bin/sh, -c, df -h]}
  expect: {result: rejected, stage: apply, error: cmd_forbidden_program}
- id: argv-ok
  given:
    cmd_yaml: {argv: [/usr/bin/df, -h, -x, tmpfs]}
  expect: {result: ok}
- id: action-grant
  steps:
  - lint: {groups_yaml: 'staff: task:action.cleanup_pipeline_tmp.v1'}
  expect: {result: rejected, stage: apply, error: action_type_grant}
- id: untrusted-missing
  steps:
  - lint: {tool_yaml: {access: read, tier: 1, output_class: raw_logs, untrusted_output: null}}
  expect: {result: rejected, stage: apply, field: spec.tools.0.untrusted_output}
- id: prose
  steps:
  - lint: {role_yaml: 'ops-reader with models.base base-alt'}
  expect: {result: ok}
- id: deploy
  given:
    deploy_yaml: {runtime: {secrets: [provider-a]}}
  expect: {result: rejected, stage: lint}
`)
	wantStatus(t, got, map[string]Status{"shell": Passed, "argv-ok": Passed, "action-grant": Passed, "untrusted-missing": Passed,
		"prose": Unchecked, "deploy": Unchecked})
}

func TestMergeFragment(t *testing.T) {
	f, err := fragment("run.approval: none, tier: 3")
	if err != nil {
		t.Fatal(err)
	}
	dst := map[string]any{"tier": 0, "run": map[string]any{"approval": "card", "max": 1}, "drop": true}
	f["drop"] = nil
	mergeInto(dst, f)
	if want := `{"run":{"approval":"none","max":1},"tier":3}`; jsonText(dst) != want {
		t.Fatalf("merged %s, want %s", jsonText(dst), want)
	}
	if g, err := fragment("approver: action.approve:*"); err != nil || g["approver"] != "action.approve:*" {
		t.Fatalf("group fragment: %v %v", g, err)
	}
	if _, err := fragment("hello with a new persona (tier 0 types only)"); err == nil {
		t.Fatal("a fragment in prose was read")
	}
}

// TestPlainRow: a lint or P1 row of the permission table runs as any other case without the
// table context (system, roles, catalog) and without the header people it does not name; a C2
// row goes to authorize.eval and a service row keeps its context.
func TestPlainRow(t *testing.T) {
	r := testRunner(t)
	r.tables = map[string]*tableInfo{"t.yaml": {authorize: true, tenantKind: "production"}}
	given := map[string]any{"tenant": "demo", "system": []any{"system-ops"}, "catalog": map[string]any{"web1": map[string]any{}},
		"people": []any{map[string]any{"principal": "human:alice", "groups": []any{"user", "admin"}}}}
	lint := &Case{File: "t.yaml", Given: given, Steps: []map[string]any{{"lint": map[string]any{"groups_yaml": "staff: data:*"}}}}
	pc := r.plainRow(lint)
	if pc == nil || len(pc.Given) != 1 || pc.Given["tenant"] != "demo" || !pc.narrowed {
		t.Fatalf("lint row: %+v", pc)
	}
	if r.plainRow(pc) != nil {
		t.Fatal("a narrowed row is narrowed again")
	}
	c2 := &Case{File: "t.yaml", Given: given, Steps: []map[string]any{{"call": map[string]any{"port": "C2", "op": "authorize"}}}}
	if r.plainRow(c2) != nil {
		t.Fatal("a C2 row runs as a plain case")
	}
	if row, _ := r.tableRow(c2); row == nil {
		t.Fatal("a C2 row is no table row")
	}
	svc := &Case{File: "t.yaml", Given: map[string]any{"tool": "service/control"}, Steps: lint.Steps}
	if r.plainRow(svc) != nil {
		t.Fatal("a service row lost its context")
	}
}

// TestMonitorProblem: a monitoring problem the fixture lets occur goes through the bench's
// monitoring system and the E5 adapter to the core, and the alarm fact is read from what the
// monitoring system was given. The class table sets the alarm and its severity (MON-5); a
// wrong expectation fails; a shorthand catalog and an unknown event stay unchecked.
func TestMonitorProblem(t *testing.T) {
	r := testRunner(t)
	got := runYAML(t, r, `
id: t-monitor
rules: [MON-5]
given:
  tenant: demo
  catalog:
    hosts:
    - {id: db1, monitor_host: db1.example.org}
    monitor_triggers:
    - {triggerid: '17021', class: disk_free, item: vfs.fs.size.free_pct}
cases:
- id: open
  steps:
  - occur:
      event:
        type: ext.monitor.problem.v1
        data: {eventid: '700', triggerid: '17021', resource: db1, severity: 2, state: open}
  expect:
    alarm: {type: alarm.disk.free.low.v1, severity: page, origin: monitor, alarmkey: 'disk.free.low:db1', state: firing, seq: 1}
- id: unknown-trigger
  steps:
  - occur:
      event:
        type: ext.monitor.problem.v1
        data: {eventid: '701', triggerid: '99999', resource: db1, severity: 5, state: open}
  expect:
    alarm: {type: alarm.monitor.problem.v1, origin: monitor}
- id: closed
  steps:
  - occur:
      event:
        type: ext.monitor.problem.v1
        data: {eventid: '700', triggerid: '17021', resource: db1, severity: 2, state: open}
  - occur:
      event:
        type: ext.monitor.problem.v1
        data: {eventid: '700', triggerid: '17021', resource: db1, severity: 2, state: resolved}
  expect:
    alarm: {type: alarm.disk.free.low.v1, state: resolved, seq: 2}
- id: wrong-severity
  steps:
  - occur:
      event:
        type: ext.monitor.problem.v1
        data: {eventid: '700', triggerid: '17021', resource: db1, severity: 4, state: open}
  expect:
    alarm: {type: alarm.disk.free.low.v1, severity: warn}
- id: shorthand-catalog
  given:
    catalog:
      web1: {monitor_scripts_allowed: [app-api-restart]}
  expect: {result: ok}
- id: other-event
  steps:
  - occur:
      event: {type: ext.monitor.maintenance.v1, data: {resource: db1}}
  expect: {result: ok}
`)
	wantStatus(t, got, map[string]Status{
		"open": Passed, "unknown-trigger": Passed, "closed": Passed, "wrong-severity": Failed,
		"shorthand-catalog": Unchecked, "other-event": Unchecked,
	})
	if d := got["wrong-severity"].Detail; !strings.Contains(d, `alarm.severity "page"`) {
		t.Errorf("wrong-severity: %s", d)
	}
}

// TestVagueRefusal: a refusal checks an expected refusal only when the expectation names what is
// refused and the refusal is at an object the fragment changed. A refusal for an unrelated
// reason is never counted as a pass: without an error code it is unchecked, with the wrong code
// it fails; a given manifest refused for something else as well is unchecked; the argv parameters of an example command keep their schemas, so only what the row
// writes is wrong; a later version of an example type is added under its name.
func TestVagueRefusal(t *testing.T) {
	r := testRunner(t)
	// A fragment of the type ops.insight.v1 in the set of the role ops-reader: a worker kind of
	// job is refused at the role that claims the type (worker_kind_mismatch), an object the
	// fragment did not change, so the expected code is found only outside the changed object.
	fragmentKinds["insight_type_yaml"] = true
	lintObjects["insight_type_yaml"] = lintObject{files: opsReaderFiles, pick: byKindName("TaskType", "ops.insight.v1"), apply: specMerge}
	defer func() {
		delete(fragmentKinds, "insight_type_yaml")
		delete(lintObjects, "insight_type_yaml")
	}()
	got := runYAML(t, r, `
id: t-vague
rules: [INJ-14]
cases:
- id: no-code
  given:
    cmd_yaml: {argv: [/bin/sh, -c, df -h]}
  expect: {result: rejected, stage: apply}
- id: unrelated-code
  given:
    cmd_yaml: {argv: [/bin/sh, -c, df -h]}
  expect: {result: rejected, stage: apply, error: cmd_argv_path}
- id: param-kept
  given:
    cmd_yaml: {argv: [/usr/bin/journalctl, -u, --, '{service.unit}']}
  expect: {result: ok}
- id: unrelated-object
  steps:
  - lint: {insight_type_yaml: {worker_kind: job}}
  expect: {result: rejected, stage: apply, error: worker_kind_mismatch}
- id: manifest-besides
  given:
    manifest:
      kind: Role
      metadata: {name: ops-runner}
      spec:
        profile: worker
        worker_kind: agent
        claims: [ops.insight.v1]
        models: {base: base-std}
        insight: {types: [ops.insight.v1], merge_window: 30m}
  expect: {result: rejected, stage: apply, field: spec.insight.merge_window}
- id: next-version
  steps:
  - lint: {schedule_yaml: 'type: data.sensor_check.v2'}
  expect: {result: rejected, stage: apply, error: system_ops_missing}
`)
	wantStatus(t, got, map[string]Status{"no-code": Unchecked, "unrelated-code": Failed, "param-kept": Passed, "unrelated-object": Unchecked, "manifest-besides": Unchecked,
		"next-version": Passed})
	if d := got["no-code"].Detail; !strings.Contains(d, "names no error code") {
		t.Errorf("no-code: %s", d)
	}
	if d := got["manifest-besides"].Detail; !strings.Contains(d, "besides the expected error") {
		t.Errorf("manifest-besides: %s", d)
	}
	if d := got["unrelated-object"].Detail; !strings.Contains(d, "outside the objects the fragment changes") {
		t.Errorf("unrelated-object: %s", d)
	}
	o := &outcome{Result: "rejected", Error: "cmd_forbidden_program", Path: "/manifests/0/spec/tools/0", Touched: map[int]bool{1: true}}
	if n := vague(map[string]any{"result": "rejected", "error": "cmd_forbidden_program"}, o); !strings.Contains(n, "outside the objects") {
		t.Errorf("a matching refusal at an object the fragment did not change: %q", n)
	}
	o.Touched = map[int]bool{0: true}
	if n := vague(map[string]any{"result": "rejected", "error": "cmd_forbidden_program"}, o); n != "" {
		t.Errorf("a matching refusal at the changed object: %q", n)
	}
}

// TestRemovedPaths: a field a fragment sets to null is matched by its full path.
func TestRemovedPaths(t *testing.T) {
	before := map[string]any{"spec": map[string]any{"tools": []any{map[string]any{"untrusted_output": true, "name": "run"}}, "name": "x"}}
	after := map[string]any{"spec": map[string]any{"tools": []any{map[string]any{"name": "run"}}, "name": "x"}}
	out := map[string]bool{}
	removedPaths(before, after, "", out)
	if len(out) != 1 || !out["spec.tools.0.untrusted_output"] {
		t.Fatalf("removed %v", out)
	}
}

// TestCodePattern: the verification code is read only from the cmd.profile.code line.
func TestCodePattern(t *testing.T) {
	root, err := spec.FindRoot("")
	if err != nil {
		t.Fatal(err)
	}
	re, err := codePattern(root)
	if err != nil {
		t.Fatal(err)
	}
	if m := re.FindStringSubmatch("Your groups: user. Task 12345678.\nYour verification code for the operator: 87654321."); m == nil || m[1] != "87654321" {
		t.Fatalf("code %v", m)
	}
	if re.MatchString("Task 12345678 is done.") {
		t.Fatal("another 8-digit number read as the code")
	}
}
