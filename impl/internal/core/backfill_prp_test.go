// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/HunterEwok/botaru/impl/internal/manifest"
	"github.com/jackc/pgx/v5"
)

// changeOf decodes a proposal change from JSON.
func changeOf(t *testing.T, raw string) proposalChange {
	t.Helper()
	var ch proposalChange
	if err := json.Unmarshal([]byte(raw), &ch); err != nil {
		t.Fatal(err)
	}
	return ch
}

// rejectedWith is the reason of a rejection, "" when the check passed.
func rejectedWith(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		return ""
	}
	var rj rejection
	if !errors.As(err, &rj) {
		t.Fatal(err)
	}
	return string(rj)
}

// PRP-10: a type is proposable only when it does work, is no backup and does not say
// proposable: false; a manifest change of any target outside the allowlist, such as a task
// type, is rejected with forbidden_change.
func TestBackfillProposable(t *testing.T) {
	t.Parallel()
	no := false
	for _, tc := range []struct {
		tt   manifest.TaskType
		want bool
	}{
		{manifest.TaskType{Purpose: "work"}, true},
		{manifest.TaskType{}, true},
		{manifest.TaskType{Purpose: "report"}, false},
		{manifest.TaskType{Purpose: "work", MakesBackup: true}, false},
		{manifest.TaskType{Purpose: "work", Proposable: &no}, false},
	} {
		if got := typeProposable(tc.tt); got != tc.want {
			t.Errorf("PRP-10: %+v proposable %v, want %v", tc.tt, got, tc.want)
		}
	}
	b := newBench(t)
	cat, err := b.c.Catalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	a := proposalAuthor{Tenant: "demo", Role: "hello", Mark: "summary"}
	ch := changeOf(t, `{"kind": "manifest_change", "manifest_change": {"ops": [{"op": "set", "target": {"kind": "TaskType", "name": "hello.greet.v1"}, "field": "spec.tree_budget_eur", "value": 1}]}}`)
	_, err = b.c.checkManifestChange(timeNow(t, b), cat, a, ch)
	if r := rejectedWith(t, err); r != "forbidden_change" {
		t.Fatalf("PRP-10: a change of a task type: %q", r)
	}
	// A schedule window shift passes for a proposable type and is forbidden_change when the
	// type or the schedule says proposable: false.
	sb := newScheduleBench(t, "0 4 * * *", nil)
	scat, err := sb.c.Catalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	op := manifestOp{Op: "set", Field: "spec.cron", Value: "0 5 * * *"}
	op.Target.Kind, op.Target.Name = "Schedule", "morning"
	tv := scat.Tenants["demo"]
	if _, err := sb.c.checkScheduleOp(scat, tv, a, op); err != nil {
		t.Fatalf("PRP-10: a window shift of a proposable schedule: %v", err)
	}
	typeOff := *scat
	typeOff.TaskTypes = maps.Clone(scat.TaskTypes)
	tt := typeOff.TaskTypes["hello.greet.v1"]
	tt.Proposable = &no
	typeOff.TaskTypes["hello.greet.v1"] = tt
	if _, err := sb.c.checkScheduleOp(&typeOff, tv, a, op); rejectedWith(t, err) != "forbidden_change" {
		t.Fatalf("PRP-10: a schedule of a type with proposable: false: %v", err)
	}
	schedOff := *tv
	schedOff.Schedules = maps.Clone(tv.Schedules)
	sc := schedOff.Schedules["morning"]
	sc.Proposable = &no
	schedOff.Schedules["morning"] = sc
	if _, err := sb.c.checkScheduleOp(scat, &schedOff, a, op); rejectedWith(t, err) != "forbidden_change" {
		t.Fatalf("PRP-10: a schedule with proposable: false: %v", err)
	}
}

// PRP-13: a human_change is a closed description: an object of the tenant's catalog, a path of
// change-paths.yaml for that kind, a direction and a value of the path's kind; the registry
// holds no target the allowlist forbids.
func TestBackfillHumanChange(t *testing.T) {
	t.Parallel()
	b := newToolBench(t)
	cat, err := b.c.Catalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	a := proposalAuthor{Tenant: "demo", Role: "hello", Mark: "summary"}
	hc := func(kind, name, path, dir string, value any) proposalChange {
		raw, _ := json.Marshal(map[string]any{"kind": "human_change", "human_change": map[string]any{
			"target": map[string]any{"kind": kind, "name": name}, "path": path, "direction": dir, "value": value}})
		return changeOf(t, string(raw))
	}
	ok, err := b.c.checkHumanChange(cat, a, hc("Host", "web1", "catalog.host.disk_gb", "up", 200))
	if err != nil || len(ok.Targets) != 1 {
		t.Fatalf("PRP-13: a valid description: %+v %v", ok, err)
	}
	for name, ch := range map[string]proposalChange{
		"a path outside the registry":      hc("Host", "web1", "spec.free_text", "set", 1),
		"a string for a number":            hc("Host", "web1", "catalog.host.disk_gb", "up", "a lot"),
		"a direction outside the list":     hc("Host", "web1", "catalog.host.disk_gb", "sideways", 1),
		"a host outside the catalog":       hc("Host", "db9", "catalog.host.disk_gb", "up", 1),
		"a path of another kind of object": hc("Role", "hello", "catalog.host.disk_gb", "up", 1),
	} {
		_, err := b.c.checkHumanChange(cat, a, ch)
		if r := rejectedWith(t, err); r != "forbidden_change" {
			t.Errorf("PRP-13: %s: %q", name, r)
		}
	}
	reg, err := b.c.spec.Registry("spec/model/change-paths.yaml")
	if err != nil {
		t.Fatal(err)
	}
	forbidden := []string{"Group", "Role", "ToolPlugin", "ModelProfile", "Governance", "Commission", "Seat", "IncidentClass", "ObservationSeries", "Action"}
	for _, e := range reg.Entries {
		if slices.Contains(forbidden, asString(e["object"])) {
			t.Errorf("PRP-13: the path %v targets a forbidden kind", e["id"])
		}
		if v := asString(e["value"]); v != "number" && v != "enum" {
			t.Errorf("PRP-13: the path %v has a value of kind %q", e["id"], v)
		}
	}
}

// PRP-23: a trial with outcome worse at the end of the window gives the executor the "Revert"
// card; the farm itself reverts nothing: the realized value stays in the store.
func TestBackfillTrialRevertCard(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newCommissionBench(t, true, presentEdit)
	id, _ := acceptLimits(t, b)
	if res, err := b.c.Apply(ctx, b.op, ApplyRequest{Commit: "a000000000000000000000000000000000000002", Message: "Botaru-Proposal: " + id, Manifests: withLimit(t, b, 7)}); err != nil || !res.Applied {
		t.Fatalf("apply %+v %v", res, err)
	}
	tenantHash := storedHash(t, b, "Tenant", "demo", "demo")
	if tenantHash == "" {
		t.Fatal("no stored tenant")
	}
	// A guard broke in the window; the metric is gone, so only the guard decides the outcome.
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE proposals SET record = jsonb_set(jsonb_set(record, '{trial,metric,series}', '"gone.series"'),
		'{trial,guards}', (SELECT coalesce(jsonb_agg(g || '{"broken_at": "2026-01-01T00:00:00Z"}'), '[{"series": "gone.guard", "broken_at": "2026-01-01T00:00:00Z"}]') FROM jsonb_array_elements(record->'trial'->'guards') g))
		WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	b.advance(t, 15*24*3600)
	b.loop(t, "proposal")
	rec := recordOf(t, b, id)
	if tr := rec["trial"].(map[string]any); tr["outcome"] != "worse" {
		t.Fatalf("trial %v", tr)
	}
	var cards int
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM deliveries WHERE kind = $1 AND subject_id = $2`, rollbackCard, id).Scan(&cards); err != nil || cards != 1 {
		t.Fatalf("PRP-23: rollback cards %d %v", cards, err)
	}
	if storedHash(t, b, "Tenant", "demo", "demo") != tenantHash {
		t.Fatal("PRP-23: the farm reverted the change itself")
	}
}

// PRP-27: a change that came out worse and stays in force keeps its fields locked, shows in
// doctor, and after three days the people who decide on the category get the "Revert" card once.
func TestWorseInForce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newCommissionBench(t, true, presentEdit)
	id, _ := acceptLimits(t, b)
	if res, err := b.c.Apply(ctx, b.op, ApplyRequest{Commit: "a000000000000000000000000000000000000002", Message: "Botaru-Proposal: " + id, Manifests: withLimit(t, b, 7)}); err != nil || !res.Applied {
		t.Fatalf("apply %+v %v", res, err)
	}
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE proposals SET record = jsonb_set(jsonb_set(record, '{trial,metric,series}', '"gone.series"'),
		'{trial,guards}', '[{"series": "gone.guard", "broken_at": "2026-01-01T00:00:00Z"}]') WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	b.advance(t, 15*24*3600)
	b.loop(t, "proposal")
	cards := func() int {
		var n int
		if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM deliveries WHERE kind = $1 AND subject_id = $2`, rollbackCard, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := cards(); n != 1 {
		t.Fatalf("rollback cards %d", n)
	}
	var keys []string
	var held []any
	if err := b.c.tx(ctx, func(tx pgx.Tx, _ time.Time) error {
		p, err := readProposal(ctx, tx, id)
		if err != nil {
			return err
		}
		keys = lockKeys(p.body())
		held, err = lockHolders(ctx, tx, "demo", "", keys)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(keys) == 0 || !slices.Contains(held, any(id)) {
		t.Fatalf("PRP-27: the worse change holds no lock: keys %v held %v", keys, held)
	}
	doc, err := b.c.Doctor(ctx, b.op, TenantRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(doc.Checks, func(ch DoctorCheck) bool {
		return ch.ID == "insights.worse_in_force" && ch.Tenant == "demo" && !ch.OK && ch.Value == id
	}) {
		t.Fatal("PRP-27: doctor does not show the change")
	}
	if !slices.ContainsFunc(doc.Checks, func(ch DoctorCheck) bool {
		return ch.ID == "goals.g5.worse_in_force" && ch.Tenant == "demo" && ch.Value == 1
	}) {
		t.Fatal("PRP-29: the goal G5 does not count the change")
	}
	// Under three days nothing more; after them one card to each decider, once.
	b.advance(t, 24*3600)
	b.loop(t, "proposal")
	if n := cards(); n != 1 {
		t.Fatalf("early reminder: %d cards", n)
	}
	b.advance(t, 3*24*3600)
	b.loop(t, "proposal")
	after := cards()
	if after < 2 {
		t.Fatalf("PRP-27: no second card, %d", after)
	}
	b.advance(t, 3*24*3600)
	b.loop(t, "proposal")
	if n := cards(); n != after {
		t.Fatalf("PRP-27: the second card repeats: %d then %d", after, n)
	}
	if r := recordOf(t, b, id)["realization"].(map[string]any); r["worse_reminded"] == nil {
		t.Fatalf("realization %v", r)
	}
	inForceNow := func() []string {
		var out []string
		if err := b.c.tx(ctx, func(tx pgx.Tx, _ time.Time) error {
			var err error
			out, err = worseInForce(ctx, tx, "demo")
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return out
	}
	// A person edits the field by hand to another value: the change is no longer in force, and
	// its follow-up and lock end.
	if res, err := b.c.Apply(ctx, b.op, ApplyRequest{Commit: "a000000000000000000000000000000000000003", Message: "edit by hand", Manifests: withLimit(t, b, 9)}); err != nil || !res.Applied {
		t.Fatalf("apply %+v %v", res, err)
	}
	b.loop(t, "proposal")
	if r := recordOf(t, b, id)["realization"].(map[string]any); r["worse_released"] == nil || len(inForceNow()) != 0 {
		t.Fatalf("PRP-27: a change edited away stays followed: %v", r)
	}
	// A change that came out worse before PRP-27 carries no mark and is not followed.
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE proposals SET record = record #- '{realization,worse_watch}' #- '{realization,worse_released}' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if n := inForceNow(); len(n) != 0 {
		t.Fatalf("PRP-27: an older worse change is followed: %v", n)
	}
}

// PRP-28: a valid human_change is still refused at creation, since nothing can link it.
func TestHumanChangeRefused(t *testing.T) {
	t.Parallel()
	b := newToolBench(t)
	ctx := context.Background()
	cat, err := b.c.Catalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	a := proposalAuthor{Tenant: "demo", Role: "hello", Mark: "summary"}
	ch := changeOf(t, `{"kind": "human_change", "human_change": {"target": {"kind": "Host", "name": "web1"}, "path": "catalog.host.disk_gb", "direction": "up", "value": 200}}`)
	err = b.c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		_, err := b.c.checkChange(ctx, tx, now, cat, a, ch)
		return err
	})
	if r := rejectedWith(t, err); r != "forbidden_change" {
		t.Fatalf("PRP-28: %q", r)
	}
}
