// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/manifest"
)

// diskClass is an incident class of the hello bench: the disk alarm of a monitoring problem
// below 5% for 5 minutes, a first measure on the resource and a page after 10 minutes.
func diskClass() map[string]any {
	return map[string]any{
		"apiVersion": "botaru/v1alpha1", "kind": "IncidentClass", "metadata": map[string]any{"name": "disk_full"},
		"spec": map[string]any{
			"title": "Disk full", "severity": "sev1",
			"detect": map[string]any{"alarm": "alarm.disk.free.low.v1", "for": "5m",
				"when": []any{map[string]any{"atom": "lt", "field": "values.free_pct", "value": 5}}},
			"subject":      map[string]any{"kind": "resource"},
			"implicate":    []any{map[string]any{"fn": "lease_holders_on", "args": map[string]any{"resource": "$subject"}}},
			"contain_auto": []any{map[string]any{"op": "hold", "scope": map[string]any{"resource": "$subject"}, "effects": []any{"no_claim"}, "ttl": "1h"}},
			"humans":       map[string]any{"notify_on_open": true, "page_if_unhandled_after": "10m"},
			"resolve":      map[string]any{"alarm_state": "resolved", "for": "10m"},
			"responses":    map[string]any{"ops": []any{"hold", "quarantine", "drain", "preempt", "cancel", "release"}},
		},
	}
}

// storeLowManifest is the required class store_low of every incident catalog (MGR-38).
func storeLowManifest() map[string]any {
	return map[string]any{
		"apiVersion": "botaru/v1alpha1", "kind": "IncidentClass", "metadata": map[string]any{"name": storeLowClass},
		"spec": map[string]any{"title": "Low space for the database", "severity": "sev1",
			"detect": map[string]any{"alarm": "alarm.store.free.low.v1"}, "subject": map[string]any{"kind": "farm"}},
	}
}

func newIncidentBench(t *testing.T) *bench {
	t.Helper()
	return newBenchWith(t, func(_ string, ms []map[string]any) []map[string]any {
		return append(ms, diskClass(), storeLowManifest())
	})
}

// diskAlarm fires or resolves the disk alarm of db1 in demo as a monitoring problem would.
func (b *bench) diskAlarm(t *testing.T, firing bool, free float64) {
	t.Helper()
	a := Alarm{Tenant: "demo", Name: "disk.free.low", Subject: AlarmSubject{Kind: "resource", ID: "db1"}, Origin: "monitor",
		Values: map[string]any{"free_pct": free, "trigger_severity": "high"}}
	err := b.c.tx(context.Background(), func(tx pgx.Tx, now time.Time) error {
		if firing {
			return b.c.fireAlarm(context.Background(), tx, now, a)
		}
		return b.c.resolveAlarm(context.Background(), tx, now, a)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func incidentsOf(t *testing.T, b *bench, class string) []*incidentRow {
	t.Helper()
	rows, err := b.c.st.Pool.Query(context.Background(), `SELECT `+incidentCols+` FROM incidents WHERE class = $1 ORDER BY opened_at, id`, class)
	if err != nil {
		t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (*incidentRow, error) { return scanIncident(r) })
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// The core opens an incident from a code alarm and a class only after detect.for, applies the
// first measure as a hold with an expiry, pages people through the unhandled alarm until a
// person takes it up, resolves it from the alarm state after resolve.for, reopens the same
// number when the alarm comes back within an hour, and closes it only after resolved (MGR-7,
// MGR-8).
func TestIncidentLifecycle(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newIncidentBench(t)
	b.diskAlarm(t, true, 3)
	b.loop(t, "incident")
	if n := len(incidentsOf(t, b, "disk_full")); n != 0 {
		t.Fatalf("opened before detect.for: %d", n)
	}
	b.advance(t, 6*60)
	b.loop(t, "incident")
	b.loop(t, "incident")
	list := incidentsOf(t, b, "disk_full")
	if len(list) != 1 {
		t.Fatalf("incidents %d", len(list))
	}
	r := list[0]
	if r.State != "contained" || r.SubjectKind != "resource" || r.SubjectID != "db1" || r.Tenant != "demo" {
		t.Fatalf("incident %+v", r)
	}
	if err := b.c.spec.Validate("urn:botaru:schema:incident:1", r.view()); err != nil {
		t.Fatalf("record: %v %v", err, r.view())
	}
	if vals := r.Record["alarm"].(map[string]any)["values"].(map[string]any); vals["free_pct"] != float64(3) || vals["trigger_severity"] != nil {
		t.Fatalf("alarm values %v", vals)
	}
	var source, scope string
	var until *time.Time
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT source, scope_kind || ':' || scope_name, until FROM holds WHERE author_kind = 'contain_auto'`).Scan(&source, &scope, &until); err != nil {
		t.Fatal(err)
	}
	if source != "disk_full" || scope != "resource:db1" || until == nil {
		t.Fatalf("first measure %s %s %v", source, scope, until)
	}
	if countEvents(t, b, "incident.opened.v1", r.ID) != 1 || countEvents(t, b, "incident.contained.v1", r.ID) != 1 {
		t.Fatal("no opening events")
	}
	// Nobody took it up within 10 minutes: the unhandled alarm pages people.
	b.advance(t, 11*60)
	b.loop(t, "incident")
	if openAlarms(t, b, "alarm.incident.unhandled.v1") != 1 {
		t.Fatal("no unhandled alarm")
	}
	if _, err := b.c.IncidentClose(ctx, b.op, IncidentRequest{ID: r.ID}); err == nil {
		t.Fatal("closed before resolved")
	}
	if _, err := b.c.IncidentAck(ctx, b.op, IncidentRequest{ID: r.ID}); err != nil {
		t.Fatal(err)
	}
	if openAlarms(t, b, "alarm.incident.unhandled.v1") != 0 {
		t.Fatal("the unhandled alarm outlived the ack")
	}
	// The alarm resolves; the incident follows after resolve.for.
	b.diskAlarm(t, false, 0)
	b.loop(t, "incident")
	if r = incidentsOf(t, b, "disk_full")[0]; r.State != "contained" {
		t.Fatalf("resolved before resolve.for: %s", r.State)
	}
	b.advance(t, 11*60)
	b.loop(t, "incident")
	if r = incidentsOf(t, b, "disk_full")[0]; r.State != "resolved" {
		t.Fatalf("after resolve.for: %s", r.State)
	}
	// Back within an hour: the same number, reopened without the acks of the last round; the
	// first measure still holds, so it contains the incident without a second hold.
	b.diskAlarm(t, true, 2)
	b.advance(t, 6*60)
	b.loop(t, "incident")
	list = incidentsOf(t, b, "disk_full")
	if len(list) != 1 || list[0].State != "contained" || list[0].Handled || list[0].Record["taken_by"] != nil {
		t.Fatalf("after the return %d %+v", len(list), list[0])
	}
	if countEvents(t, b, "incident.reopened.v1", r.ID) != 1 {
		t.Fatal("no reopen event")
	}
	b.diskAlarm(t, false, 0)
	b.advance(t, 11*60)
	b.loop(t, "incident")
	d, err := b.c.IncidentClose(ctx, b.op, IncidentRequest{ID: r.ID})
	if err != nil || d.State != "closed" {
		t.Fatalf("close %+v %v", d, err)
	}
	// Closing does not release the holds of the first measure (HLD-2).
	var active int
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM holds WHERE author_kind = 'contain_auto' AND state = 'active'`).Scan(&active); err != nil || active != 1 {
		t.Fatalf("active first measures %d %v", active, err)
	}
	// An alarm below the class condition opens nothing.
	b.advance(t, 2*3600)
	b.diskAlarm(t, true, 40)
	b.advance(t, 6*60)
	b.loop(t, "incident")
	if n := len(incidentsOf(t, b, "disk_full")); n != 1 {
		t.Fatalf("an alarm outside detect.when opened an incident: %d", n)
	}
}

// Only a person declares an incident false, with a reason code and the Details text; the text
// stays in the record for people and in no event (MGR-8).
func TestIncidentDismiss(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newIncidentBench(t)
	b.diskAlarm(t, true, 1)
	b.advance(t, 6*60)
	b.loop(t, "incident")
	r := incidentsOf(t, b, "disk_full")[0]
	_, err := b.c.IncidentDismiss(ctx, b.op, IncidentRequest{Incident: r.ID, ReasonCode: "false_alarm"})
	wantReason(t, err, "dismiss_reason_required")
	_, err = b.c.IncidentDismiss(ctx, Operator{Principal: "system", Tenant: "demo"}, IncidentRequest{Incident: r.ID, ReasonCode: "test", Details: "drill"})
	wantReason(t, err, "forbidden")
	d, err := b.c.IncidentDismiss(ctx, b.op, IncidentRequest{Incident: r.ID, ReasonCode: "maintenance", Details: "disk swap in progress"})
	if err != nil || d.State != "dismissed" {
		t.Fatalf("dismiss %+v %v", d, err)
	}
	r = incidentsOf(t, b, "disk_full")[0]
	if dm, _ := r.Record["dismiss"].(map[string]any); dm["reason_code"] != "maintenance" || dm["details"] != "disk swap in progress" {
		t.Fatalf("record %v", r.Record["dismiss"])
	}
	var data []byte
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT data FROM events WHERE type = 'incident.dismissed.v1' AND subject = $1`, r.ID).Scan(&data); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "disk swap") {
		t.Fatalf("the Details text is in the event %s", data)
	}
	var ev map[string]any
	_ = json.Unmarshal(data, &ev)
	if err := b.c.spec.Validate("urn:botaru:schema:incident-event:1", ev); err != nil {
		t.Fatal(err)
	}
	// A dismissed incident is finished: the still firing alarm does not open it again.
	b.loop(t, "incident")
	if n := len(incidentsOf(t, b, "disk_full")); n != 1 {
		t.Fatalf("incidents %d", n)
	}
	if _, err := b.c.IncidentAck(ctx, Operator{Principal: b.op.Principal, Tenant: "demo2"}, IncidentRequest{ID: r.ID}); err == nil {
		t.Fatal("acked from another tenant")
	}
}

// The store class of the initial catalog opens from the core's own store alarm with the
// subject farm in the service tenant (MGR-38).
func TestIncidentStoreLow(t *testing.T) {
	t.Parallel()
	b := newBenchWith(t, func(root string, ms []map[string]any) []map[string]any {
		docs, err := manifest.ReadFiles([]string{filepath.Join(root, "examples/incidents/store-low.yaml")})
		if err != nil {
			t.Fatal(err)
		}
		return append(ms, docs[0].Value)
	})
	err := b.c.tx(context.Background(), func(tx pgx.Tx, now time.Time) error {
		return b.c.fireAlarm(context.Background(), tx, now, Alarm{Tenant: "farm", Name: "store.free.low", Subject: AlarmSubject{Kind: "store", ID: "c1"}, Severity: "page"})
	})
	if err != nil {
		t.Fatal(err)
	}
	b.loop(t, "incident")
	list := incidentsOf(t, b, "store_low")
	if len(list) != 1 || list[0].Tenant != "farm" || list[0].SubjectKind != "farm" || list[0].State != "open" {
		t.Fatalf("store incident %+v", list)
	}
	if err := b.c.spec.Validate("urn:botaru:schema:incident:1", list[0].view()); err != nil {
		t.Fatal(err)
	}
}

// The lint keeps an incident class to the closed lists (MGR-7).
func TestIncidentClassLint(t *testing.T) {
	t.Parallel()
	c := lintCore(t)
	codes := lintCodes(t, c, `apiVersion: botaru/v1alpha1
kind: IncidentClass
metadata: {name: bad}
spec:
  title: Bad
  severity: sev2
  detect:
    alarm: task.done.v1
    when: [{atom: regex, field: values.x, value: 1}]
  subject: {kind: farm}
  implicate: [{fn: whoever}]
  contain_auto: [{op: quarantine, scope: {worker: $subject}}]
  responses: {ops: [shell], services: [restart]}
`)
	for _, want := range []string{"incident_class_ref", "incident_contain_auto"} {
		if !slices.Contains(codes, want) {
			t.Errorf("codes %v, want %s", codes, want)
		}
	}
	if codes := lintCodes(t, c, `apiVersion: botaru/v1alpha1
kind: IncidentClass
metadata: {name: store_low}
spec:
  title: Low space for the database
  severity: sev1
  detect: {alarm: alarm.store.free.low.v1}
  subject: {kind: farm}
`); len(codes) != 0 {
		t.Fatalf("good class %v", codes)
	}
}

// A tenant-subject class joins the alarms of several resources into one incident that resolves
// only when none of them fires, and every involvement function runs (MGR-7, MGR-8).
func TestIncidentTenantSubject(t *testing.T) {
	t.Parallel()
	b := newBenchWith(t, func(_ string, ms []map[string]any) []map[string]any {
		cl := diskClass()
		spec := cl["spec"].(map[string]any)
		spec["subject"] = map[string]any{"kind": "tenant"}
		delete(spec, "contain_auto")
		var fns []any
		for _, fn := range implicateFns {
			fns = append(fns, map[string]any{"fn": fn, "args": map[string]any{"resource": "db1", "role": "assistant"}})
		}
		spec["implicate"] = fns
		return append(ms, cl, storeLowManifest())
	})
	fire := func(id string, firing bool) {
		t.Helper()
		a := Alarm{Tenant: "demo", Name: "disk.free.low", Subject: AlarmSubject{Kind: "resource", ID: id}, Origin: "monitor",
			Values: map[string]any{"free_pct": 1}}
		if err := b.c.tx(context.Background(), func(tx pgx.Tx, now time.Time) error {
			if firing {
				return b.c.fireAlarm(context.Background(), tx, now, a)
			}
			return b.c.resolveAlarm(context.Background(), tx, now, a)
		}); err != nil {
			t.Fatal(err)
		}
	}
	fire("db1", true)
	fire("db2", true)
	b.advance(t, 6*60)
	b.loop(t, "incident")
	b.advance(t, 2*60)
	b.loop(t, "incident")
	list := incidentsOf(t, b, "disk_full")
	if len(list) != 1 || list[0].SubjectKind != "tenant" || list[0].SubjectID != "demo" || list[0].State != "open" {
		t.Fatalf("incidents %d %+v", len(list), list)
	}
	ep := list[0].Episode
	b.loop(t, "incident")
	if r := incidentsOf(t, b, "disk_full")[0]; r.Episode != ep {
		t.Fatalf("the incident flipped between episodes: %d %d", ep, r.Episode)
	}
	fire("db1", false)
	b.advance(t, 11*60)
	b.loop(t, "incident")
	if r := incidentsOf(t, b, "disk_full")[0]; r.State != "open" {
		t.Fatalf("resolved while db2 fires: %s", r.State)
	}
	fire("db2", false)
	b.advance(t, 11*60)
	b.loop(t, "incident")
	if r := incidentsOf(t, b, "disk_full")[0]; r.State != "resolved" {
		t.Fatalf("after both resolved: %s", r.State)
	}
	// The other resource coming back within the hour reopens the same incident.
	fire("db1", true)
	b.advance(t, 6*60)
	b.loop(t, "incident")
	if list := incidentsOf(t, b, "disk_full"); len(list) != 1 || list[0].State != "reopened" {
		t.Fatalf("after the return %d %+v", len(list), list)
	}
}

// detect.for counts from when the class conditions last started to hold on the episode's latest
// numbers: values that stop holding before it reset the count, and values that start holding
// later start it (MGR-8).
func TestIncidentDetectFor(t *testing.T) {
	t.Parallel()
	b := newIncidentBench(t)
	b.diskAlarm(t, true, 3)
	b.loop(t, "incident")
	b.advance(t, 3*60)
	b.diskAlarm(t, true, 10)
	b.loop(t, "incident")
	b.advance(t, 3*60)
	b.loop(t, "incident")
	if n := len(incidentsOf(t, b, "disk_full")); n != 0 {
		t.Fatalf("opened although the values stopped holding: %d", n)
	}
	b.diskAlarm(t, true, 2)
	b.loop(t, "incident")
	b.advance(t, 4*60)
	b.loop(t, "incident")
	if n := len(incidentsOf(t, b, "disk_full")); n != 0 {
		t.Fatalf("opened before detect.for of the new start: %d", n)
	}
	b.advance(t, 2*60)
	b.loop(t, "incident")
	if n := len(incidentsOf(t, b, "disk_full")); n != 1 {
		t.Fatalf("not opened after detect.for: %d", n)
	}
}

// A break in the conditions between two loop passes still restarts detect.for (MGR-8).
func TestIncidentDetectBreakBetweenPasses(t *testing.T) {
	t.Parallel()
	b := newIncidentBench(t)
	b.diskAlarm(t, true, 3)
	b.loop(t, "incident")
	b.advance(t, 4*60)
	b.diskAlarm(t, true, 10)
	b.advance(t, 30)
	b.diskAlarm(t, true, 2)
	b.advance(t, 2*60)
	b.loop(t, "incident")
	if n := len(incidentsOf(t, b, "disk_full")); n != 0 {
		t.Fatalf("opened on time from before the break: %d", n)
	}
	b.advance(t, 3*60)
	b.loop(t, "incident")
	if n := len(incidentsOf(t, b, "disk_full")); n != 1 {
		t.Fatalf("not opened after detect.for: %d", n)
	}
}

// incident.list pages newest first by its cursor.
func TestIncidentListPages(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newIncidentBench(t)
	for _, id := range []string{"db1", "db2"} {
		a := Alarm{Tenant: "demo", Name: "disk.free.low", Subject: AlarmSubject{Kind: "resource", ID: id}, Origin: "monitor",
			Values: map[string]any{"free_pct": 1}}
		b.alarmTx(t, func(tx pgx.Tx, now time.Time) error { return b.c.fireAlarm(ctx, tx, now, a) })
		b.advance(t, 6*60)
		b.loop(t, "incident")
	}
	first, err := b.c.IncidentList(ctx, b.op, TenantRequest{Limit: 1})
	if err != nil || len(first["items"].([]any)) != 1 || first["next"] == nil {
		t.Fatalf("first page %v %v", first, err)
	}
	second, err := b.c.IncidentList(ctx, b.op, TenantRequest{Limit: 1, After: first["next"].(string)})
	if err != nil || len(second["items"].([]any)) != 1 || second["next"] != nil {
		t.Fatalf("second page %v %v", second, err)
	}
	if first["items"].([]any)[0].(map[string]any)["id"] == second["items"].([]any)[0].(map[string]any)["id"] {
		t.Fatal("the cursor repeated the page")
	}
}

// The number log keeps a run of matching numbers as long as the longest detect.for, however
// often the numbers change (MGR-8).
func TestIncidentDetectLongRun(t *testing.T) {
	t.Parallel()
	b := newIncidentBench(t)
	for i := range 8 {
		b.diskAlarm(t, true, float64(1+i%3))
		b.loop(t, "incident")
		if i < 4 && len(incidentsOf(t, b, "disk_full")) != 0 {
			t.Fatalf("opened before detect.for at step %d", i)
		}
		b.advance(t, 60)
	}
	if n := len(incidentsOf(t, b, "disk_full")); n != 1 {
		t.Fatalf("a long run of changing matching numbers did not open: %d", n)
	}
}

// A repeat without numbers breaks conditions on the missing field (MGR-8).
func TestIncidentRepeatWithoutValues(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newIncidentBench(t)
	b.diskAlarm(t, true, 3)
	b.advance(t, 3*60)
	a := Alarm{Tenant: "demo", Name: "disk.free.low", Subject: AlarmSubject{Kind: "resource", ID: "db1"}, Origin: "monitor"}
	b.alarmTx(t, func(tx pgx.Tx, now time.Time) error { return b.c.fireAlarm(ctx, tx, now, a) })
	b.advance(t, 3*60)
	b.loop(t, "incident")
	if n := len(incidentsOf(t, b, "disk_full")); n != 0 {
		t.Fatalf("opened on numbers a repeat no longer carried: %d", n)
	}
}
