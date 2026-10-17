// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/manifest"
)

// Tests of the preventable incidents of the farm manager and the restricted mode of the seat.

// accountableEdit marks the disk class manager_accountable and adds disk_filling on the same
// alarm (MGR-55, MGR-45).
func accountableEdit(ms []map[string]any) {
	for _, m := range ms {
		if m["kind"] == "IncidentClass" && m["metadata"].(map[string]any)["name"] == "disk_full" {
			m["spec"].(map[string]any)["manager_accountable"] = true
		}
	}
}

func fillingClass() map[string]any {
	return map[string]any{
		"apiVersion": "botaru/v1alpha1", "kind": "IncidentClass", "metadata": map[string]any{"name": "disk_filling"},
		"spec": map[string]any{"title": "Disk filling", "severity": "sev2",
			"detect": map[string]any{"alarm": "alarm.disk.free.low.v1", "for": "5m",
				"when": []any{map[string]any{"atom": "lt", "field": "values.free_pct", "value": 15}}},
			"subject":   map[string]any{"kind": "resource"},
			"implicate": []any{map[string]any{"fn": "lease_holders_on", "args": map[string]any{"resource": "$subject"}}},
			"resolve":   map[string]any{"alarm_state": "resolved", "for": "10m"},
		},
	}
}

func restricted(t *testing.T, b *bench, tenant, incident string) bool {
	t.Helper()
	var r bool
	err := b.c.tx(context.Background(), func(tx pgx.Tx, _ time.Time) error {
		var err error
		r, err = seatRestricted(context.Background(), tx, tenant, incident)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// A preventable incident from v0.3 does not stop the seat: the record says so, the alarm
// manager.preventable pages, the farm operators get manager_preventable, and the seat goes into
// restricted mode in the tenant with the same epoch, lifted by seat.resume --tenant; a class
// that stops_manager stops nothing any more (GOV-47, MGR-45, MGR-47, MGR-55).
func TestManagerPreventableV03(t *testing.T) {
	ctx := context.Background()
	// manager_accountable is a v0.3 field, which apply accepts only under the v0.3 claim (MAN-11).
	b := newManagerBenchWith(t, nil, []map[string]any{fillingClass()}, accountableEdit)
	op := b.farmOperator(t)
	if _, err := b.c.SeatAssign(ctx, b.farmOp(), SeatRequest{Seat: managerSeatID}); err != nil {
		t.Fatal(err)
	}
	epoch := b.managerSeat(t).Epoch
	b.diskAlarm(t, true, 12)
	b.advance(t, 6*60)
	b.loop(t, "incident")
	filling := incidentsOf(t, b, "disk_filling")
	if len(filling) != 1 {
		t.Fatalf("disk_filling %d", len(filling))
	}
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE incidents SET record = record || '{"implicated": [{"kind": "worker", "id": "wk_x"}]}' WHERE id = $1`, filling[0].ID); err != nil {
		t.Fatal(err)
	}
	b.advance(t, 2*3600)
	b.diskAlarm(t, true, 3)
	b.advance(t, 6*60)
	b.loop(t, "incident")
	full := incidentsOf(t, b, "disk_full")
	if len(full) != 1 || full[0].Record["manager_preventable"] != true {
		t.Fatalf("disk_full %+v", full)
	}
	if m := b.managerSeat(t); m.State != "held" || m.Epoch != epoch || len(m.Reasons) != 0 {
		t.Fatalf("seat %+v", m)
	}
	if openAlarms(t, b, "alarm.manager.preventable.v1") != 1 {
		t.Fatal("no alarm.manager.preventable")
	}
	countNotices := func() int {
		var n int
		if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM deliveries WHERE kind = $1 AND recipient = $2`, preventableNotice, op).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if countNotices() != 1 {
		t.Fatal("no manager_preventable to the farm operator")
	}
	// Restricted mode: any basis but a sev1 incident opened during it.
	if !restricted(t, b, "demo", full[0].ID) || !restricted(t, b, "demo", "") || restricted(t, b, "other", "") {
		t.Fatal("restricted mode")
	}
	// The spending class stops_manager stops nothing from v0.3; its disk incident on db9 has no
	// filling incident, and as a sev1 opened during the restriction it is a basis that may act.
	b.stopAlarm(t, true)
	b.advance(t, 6*60)
	b.loop(t, "incident")
	if m := b.managerSeat(t); m.State != "held" {
		t.Fatalf("stopped by stops_manager %+v", m)
	}
	var db9 *incidentRow
	for _, r := range incidentsOf(t, b, "disk_full") {
		if r.SubjectID == "db9" {
			db9 = r
		}
	}
	if db9 == nil || db9.Record["manager_preventable"] != "no_filling_incident" || restricted(t, b, "demo", db9.ID) {
		t.Fatalf("db9 %+v", db9)
	}
	// The notice repeats after 24 h, once.
	b.advance(t, 24*3600)
	b.loop(t, "identity")
	b.loop(t, "identity")
	if n := countNotices(); n != 2 {
		t.Fatalf("notices after 24 h: %d", n)
	}
	_, err := b.c.SeatResume(ctx, b.farmOp(), SeatResumeRequest{Seat: managerSeatID, Tenant: "demo"})
	if err != nil {
		t.Fatal(err)
	}
	if restricted(t, b, "demo", "") || b.managerSeat(t).Epoch != epoch {
		t.Fatal("seat.resume --tenant")
	}
	_, err = b.c.SeatResume(ctx, b.farmOp(), SeatResumeRequest{Seat: managerSeatID, Tenant: "demo"})
	wantReason(t, err, "conflict")
	var lifted int
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM manager_restrictions WHERE tenant = 'demo' AND until IS NOT NULL`).Scan(&lifted); err != nil || lifted != 1 {
		t.Fatalf("lifted restriction kept %d %v", lifted, err)
	}
	if countEvents(t, b, "seat.stopped.v1", managerSeatID) < 1 || countEvents(t, b, "seat.resumed.v1", managerSeatID) < 1 {
		t.Fatal("no seat events of the restriction")
	}
}

// The v0.1 forms of the manager are gone at the v0.3 claim: seat.resume without a tenant is
// refused, stops_manager does not answer for the manager while manager_accountable does, and a
// takeover is paid from the manager line (GOV-47, MGR-55, MGR-48).
func TestManagerV01FormsGone(t *testing.T) {
	b := newManagerBench(t)
	_, err := b.c.SeatResume(context.Background(), b.farmOp(), SeatResumeRequest{Seat: managerSeatID, Incident: "inc_x"})
	wantReason(t, err, "input_schema_invalid")
	if managerClass(map[string]any{"stops_manager": true}) || !managerClass(map[string]any{"manager_accountable": true}) {
		t.Fatal("managerClass at v0.3")
	}
	if takeoverLine() != "manager" {
		t.Fatal("the takeover line at v0.3")
	}
}

// The repeats of manager_preventable: 24 h, 72 h, then weekly (GOV-47).
func TestPreventableRepeats(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for h, want := range map[int]int{1: 0, 24: 1, 71: 1, 72: 2, 72 + 167: 2, 72 + 168: 3, 72 + 336: 4} {
		if got := preventableRepeats(t0, t0.Add(time.Duration(h)*time.Hour)); got != want {
			t.Errorf("after %d h: %d, want %d", h, got, want)
		}
	}
}

// manager_accountable needs a class whose alarm no agent-role type publishes (GOV-48).
func TestAccountableLint(t *testing.T) {
	c := lintCore(t)
	class := `apiVersion: botaru/v1alpha1
kind: IncidentClass
metadata: {name: disk_full}
spec:
  title: Disk full
  severity: sev1
  manager_accountable: true
  detect: {alarm: alarm.disk.free.low.v1}
  subject: {kind: resource}
`
	if codes := lintCodes(t, c, class); slices.Contains(codes, "incident_class_ref") {
		t.Fatalf("an accountable class alone %v", codes)
	}
	publisher := class + `---
apiVersion: botaru/v1alpha1
kind: TaskType
metadata: {name: disk.watch.v1}
spec:
  purpose: manage
  events: [alarm.disk.free.low.v1]
`
	if codes := lintCodes(t, c, publisher); !slices.Contains(codes, "incident_class_ref") {
		t.Fatalf("an alarm a manage type publishes %v", codes)
	}
}

// kindTo is how many deliveries of a kind went to a person.
func kindTo(t *testing.T, b *bench, kind, recipient string) int {
	t.Helper()
	var n int
	if err := b.c.st.Pool.QueryRow(context.Background(), `SELECT count(*) FROM deliveries WHERE kind = $1 AND recipient = $2`, kind, recipient).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// From v0.3 an operator of farm marks manager_preventable read with incident.ack on the incident
// of the restriction, and the repeats stop while the restriction stays; seat.resume needs the
// tenant; a stop reason incident left by v0.2 goes without a manager_stopped repeat; the seat
// record and doctor show restricted_in (GOV-47, MGR-47, MGR-55).
func TestPreventableReadV03(t *testing.T) {
	ctx := context.Background()
	b := newManagerBench(t)
	op := b.farmOperator(t)
	if _, err := b.c.SeatAssign(ctx, b.farmOp(), SeatRequest{Seat: managerSeatID}); err != nil {
		t.Fatal(err)
	}
	holder := b.managerSeat(t).Holder
	b.exec(t, `INSERT INTO manager_restrictions (tenant, incident, holder, since) VALUES ('demo', 'inc_01J9Z3M2Q8YB6V4N7T5K0C2D1E', $1, botaru_now() - interval '25 hours')`, holder)
	b.loop(t, "identity")
	if n := kindTo(t, b, preventableNotice, op); n != 1 {
		t.Fatalf("the 24 h repeat: %d", n)
	}
	// The seat record carries restricted_in, and doctor shows it for the tenant (MGR-47).
	var rec map[string]any
	if err := b.c.tx(ctx, func(tx pgx.Tx, _ time.Time) error {
		var err error
		rec, err = b.c.managerSeatRecord(ctx, tx)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	list, _ := rec["restricted_in"].([]any)
	if len(list) != 1 || asMap(list[0])["tenant"] != "demo" || rec["state"] != "held" {
		t.Fatalf("seat record %v", rec)
	}
	doc, err := b.c.Doctor(ctx, b.op, TenantRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if i := slices.IndexFunc(doc.Checks, func(c DoctorCheck) bool { return c.ID == "manager.restricted_since" }); i < 0 || doc.Checks[i].OK || doc.Checks[i].Value == nil {
		t.Fatalf("doctor %+v", doc.Checks)
	}
	// A tenant operator's incident.ack is an ordinary one: the incident is not of its tenant.
	if _, err := b.c.IncidentAck(ctx, b.op, IncidentRequest{ID: "inc_01J9Z3M2Q8YB6V4N7T5K0C2D1E"}); err == nil {
		t.Fatal("a tenant operator marked the farm notice read")
	}
	d, err := b.c.IncidentAck(ctx, b.farmOp(), IncidentRequest{ID: "inc_01j9z3m2q8yb6v4n7t5k0c2d1e"})
	if err != nil || !d.OK || d.AuditSeq == 0 || d.ID != managerSeatID || d.State != "read" {
		t.Fatalf("marked read %+v %v", d, err)
	}
	if again, err := b.c.IncidentAck(ctx, b.farmOp(), IncidentRequest{ID: "inc_01J9Z3M2Q8YB6V4N7T5K0C2D1E"}); err != nil || again.AuditSeq != 0 {
		t.Fatalf("marked read twice %+v %v", again, err)
	}
	b.advance(t, 72*3600)
	b.loop(t, "identity")
	if n := kindTo(t, b, preventableNotice, op); n != 1 {
		t.Fatalf("a repeat after the read mark: %d", n)
	}
	if !restricted(t, b, "demo", "") {
		t.Fatal("the read mark lifted the restriction")
	}
	// A v0.3 core requires the tenant on seat.resume.
	_, err = b.c.SeatResume(ctx, b.farmOp(), SeatResumeRequest{Seat: managerSeatID, Incident: "inc_01J9Z3M2Q8YB6V4N7T5K0C2D1E"})
	wantReason(t, err, "input_schema_invalid")
	// A stop reason incident from a v0.2 core goes with the next loop pass, without a
	// manager_stopped repeat and without retiring the holder.
	epoch, changed := b.managerSeat(t).Epoch, kindTo(t, b, "manager_holder_changed", op)
	b.exec(t, `UPDATE seats SET state = 'stopped', decide_until = botaru_now() - interval '1 hour',
		stop_reasons = jsonb_build_array(jsonb_build_object('reason', 'incident', 'at', to_char((botaru_now() - interval '8 days') AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'), 'incident', 'inc_01J9Z3M2Q8YB6V4N7T5K0C2D1F'))
		WHERE tenant = 'farm' AND id = $1`, managerSeatID)
	b.loop(t, "identity")
	m := b.managerSeat(t)
	if m.State != "held" || m.Holder != holder || m.has("incident") || m.Epoch != epoch+1 {
		t.Fatalf("seat after the v0.2 reason %+v", m)
	}
	if kindTo(t, b, "manager_holder_changed", op) != changed+1 || countEvents(t, b, "seat.resumed.v1", managerSeatID) < 1 {
		t.Fatal("the seat back in held was not told as resumed")
	}
	if n := kindTo(t, b, "manager_stopped", op); n != 0 {
		t.Fatalf("manager_stopped from v0.3: %d", n)
	}
}

// A v0.3 core accepts stops_manager and incident_stop and does not use them: apply warns, the
// lint of store_low no longer refuses stops_manager, and stops_manager does not answer for the
// manager (MGR-55, GOV-48).
func TestIgnoredV02FieldsV03(t *testing.T) {
	class := manifest.Doc{Kind: "IncidentClass", Name: "spend_storm"}
	charter := manifest.Doc{Kind: "Governance", Tenant: farmTenant, Name: "farm"}
	cv := map[string]any{"spec": map[string]any{"stops_manager": true}}
	gv := map[string]any{"spec": map[string]any{"farm_manager": map[string]any{"incident_stop": map[string]any{"decide_within": "3d"}}}}
	c := lintCore(t)
	storeLow := `apiVersion: botaru/v1alpha1
kind: IncidentClass
metadata: {name: store_low}
spec:
  title: Low space
  severity: sev1
  stops_manager: true
  detect: {alarm: alarm.store.free.low.v1}
  subject: {kind: farm}
`
	w := ignoredV03(class, cv, 3)
	if len(w) != 1 || w[0].Code != "version_unsupported" || w[0].Path != "/manifests/3/spec/stops_manager" {
		t.Fatalf("class warning %+v", w)
	}
	if w := ignoredV03(charter, gv, 1); len(w) != 1 || w[0].Path != "/manifests/1/spec/farm_manager/incident_stop" {
		t.Fatalf("charter warning %+v", w)
	}
	if w := ignoredV03(manifest.Doc{Kind: "Governance", Tenant: "demo"}, gv, 1); w != nil {
		t.Fatalf("a tenant charter %+v", w)
	}
	// The farm charter by name without a tenant is the farm charter too, as for the charter
	// bounds lint (isFarmCharter).
	if w := ignoredV03(manifest.Doc{Kind: "Governance", Name: "farm"}, gv, 2); len(w) != 1 || w[0].Path != "/manifests/2/spec/farm_manager/incident_stop" {
		t.Fatalf("farm charter without a tenant %+v", w)
	}
	if codes := lintCodes(t, c, storeLow); slices.Contains(codes, "incident_store_low") {
		t.Fatalf("v0.3 lint %v", codes)
	}
	if managerClass(map[string]any{"stops_manager": true}) || !managerClass(map[string]any{"manager_accountable": true}) {
		t.Fatal("managerClass v0.3")
	}
}
