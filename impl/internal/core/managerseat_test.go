// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/manifest"
	"github.com/HunterEwok/botaru/impl/internal/spec"
	"github.com/HunterEwok/botaru/impl/internal/testbench"
)

// stopClass is an incident class whose subject is the tenant and that stops the farm manager
// seat: no disk conditions apply to it.
func stopClass() map[string]any {
	cl := diskClass()
	cl["metadata"] = map[string]any{"name": "spend_storm"}
	s := cl["spec"].(map[string]any)
	s["subject"] = map[string]any{"kind": "tenant"}
	s["stops_manager"] = true
	delete(s, "contain_auto")
	s["implicate"] = []any{}
	return cl
}

// newManagerBench is the hello bench with the farm manager role, its seat task type and model
// profile, the farm charter, the disk class and a stopping class.
func newManagerBench(t *testing.T, edits ...func([]map[string]any)) *bench {
	t.Helper()
	root, err := spec.FindRoot("")
	if err != nil {
		t.Fatal(err)
	}
	var files []string
	for _, f := range []string{"examples/roles/farm-manager/role.yaml", "examples/tasktypes/manager.incident.respond.v1.yaml",
		"examples/model-profiles/manager-std.yaml", "examples/tenants/farm/governance.yaml"} {
		files = append(files, filepath.Join(root, f))
	}
	docs, err := manifest.ReadFiles(files)
	if err != nil {
		t.Fatal(err)
	}
	var extra []map[string]any
	for _, d := range docs {
		extra = append(extra, d.Value)
	}
	sp, err := spec.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := testbench.DropUnsupported(sp, extra); err != nil {
		t.Fatal(err)
	}
	return newBenchWith(t, func(_ string, ms []map[string]any) []map[string]any {
		// The tenant's consent group for the manager allows the seat task (MGR-3).
		ms = append(append(ms, extra...), diskClass(), stopClass(), map[string]any{
			"apiVersion": "botaru/v1alpha1", "kind": "Group", "metadata": map[string]any{"name": "farm-manager-ops", "tenant": "demo"},
			"spec": map[string]any{"caps": []any{"task:" + incidentRespondType}, "data": []any{"summary", "internal"}}})
		for _, e := range edits {
			e(ms)
		}
		return ms
	})
}

func (b *bench) farmOp() Operator { return Operator{Principal: b.op.Principal, Tenant: farmTenant} }

func (b *bench) managerSeat(t *testing.T) *managerSeat {
	t.Helper()
	var m *managerSeat
	err := b.c.tx(context.Background(), func(tx pgx.Tx, now time.Time) error {
		var err error
		m, err = loadManagerSeat(context.Background(), tx, false)
		return err
	})
	if err != nil || m == nil {
		t.Fatalf("seat %v %v", m, err)
	}
	return m
}

// stopAlarm fires or resolves the alarm the stopping class detects, on a resource of demo.
func (b *bench) stopAlarm(t *testing.T, firing bool) {
	t.Helper()
	a := Alarm{Tenant: "demo", Name: "disk.free.low", Subject: AlarmSubject{Kind: "resource", ID: "db9"}, Origin: "monitor",
		Values: map[string]any{"free_pct": 1}}
	b.alarmTx(t, func(tx pgx.Tx, now time.Time) error {
		if firing {
			return b.c.fireAlarm(context.Background(), tx, now, a)
		}
		return b.c.resolveAlarm(context.Background(), tx, now, a)
	})
}

// Only the operator of farm assigns the farm manager seat; the core creates a new identity in
// farm each time, the previous holder leaves with the next one as successor, and every change
// grows the epoch and tells the admins (MGR-1, MGR-31).
func TestManagerSeatAssign(t *testing.T) {
	ctx := context.Background()
	b := newManagerBench(t)
	if _, err := b.c.SeatAssign(ctx, b.op, SeatRequest{Seat: managerSeatID}); err == nil {
		t.Fatal("a tenant operator assigned the farm manager")
	}
	if _, err := b.c.SeatAssign(ctx, b.farmOp(), SeatRequest{Seat: managerSeatID, Bot: "bot_x"}); err == nil {
		t.Fatal("an existing bot took the farm manager seat")
	}
	d, err := b.c.SeatAssign(ctx, b.farmOp(), SeatRequest{Seat: managerSeatID})
	if err != nil || d.State != "held" {
		t.Fatalf("assign %+v %v", d, err)
	}
	m := b.managerSeat(t)
	first := m.Holder
	if m.State != "held" || m.Epoch != 1 || first == "" || m.ResponsibleSince == nil {
		t.Fatalf("seat %+v", m)
	}
	bot := b.loadBot(t, first)
	if bot.Tenant != farmTenant || bot.Mode != "seat" || bot.Seat != managerSeatID || bot.Role != "farm-manager" {
		t.Fatalf("holder %+v", bot)
	}
	if _, err := b.c.SeatAssign(ctx, b.farmOp(), SeatRequest{Seat: managerSeatID}); err != nil {
		t.Fatal(err)
	}
	m = b.managerSeat(t)
	if m.Holder == first || m.Epoch != 2 {
		t.Fatalf("after the second assign %+v", m)
	}
	if prev := b.loadBot(t, first); prev.State != "retired" || prev.Retired["reason"] != "replaced_by_human" || prev.Retired["successor"] != m.Holder {
		t.Fatalf("previous holder %+v", prev)
	}
	if _, err := b.c.SeatVacate(ctx, b.farmOp(), SeatRequest{Seat: managerSeatID}); err != nil {
		t.Fatal(err)
	}
	if m = b.managerSeat(t); m.State != "vacant" || m.Holder != "" || m.Epoch != 3 || m.UnheldSince == nil {
		t.Fatalf("after vacate %+v", m)
	}
	if n := countEvents(t, b, "seat.assigned.v1", managerSeatID) + countEvents(t, b, "seat.vacated.v1", managerSeatID); n != 3 {
		t.Fatalf("seat events %d", n)
	}
}

// A hold on the scope seat stops the seat with the reason human in the same transaction, and
// its release returns it to held; both grow the epoch (MGR-15).
func TestManagerSeatHumanStop(t *testing.T) {
	ctx := context.Background()
	b := newManagerBench(t)
	if _, err := b.c.SeatAssign(ctx, b.farmOp(), SeatRequest{Seat: managerSeatID}); err != nil {
		t.Fatal(err)
	}
	h, err := b.c.HoldSet(ctx, OperatorActor(b.farmOp()), HoldSetRequest{Scope: HoldScope{Kind: "seat", Tenant: farmTenant, Name: managerSeatID},
		Effects: []string{"no_claim"}, Reason: "manual"})
	if err != nil {
		t.Fatal(err)
	}
	m := b.managerSeat(t)
	if m.State != "stopped" || !m.has("human") || m.Epoch != 2 {
		t.Fatalf("after the hold %+v", m)
	}
	if _, err := b.c.HoldRelease(ctx, OperatorActor(b.farmOp()), HoldReleaseRequest{ID: h.ID}); err != nil {
		t.Fatal(err)
	}
	if m = b.managerSeat(t); m.State != "held" || len(m.Reasons) != 0 || m.Epoch != 3 {
		t.Fatalf("after the release %+v", m)
	}
}

// A stop reason added to or removed from a vacant seat grows the epoch and keeps it vacant.
func TestManagerVacantSeatStop(t *testing.T) {
	ctx := context.Background()
	b := newManagerBench(t)
	if _, err := b.c.SeatAssign(ctx, b.farmOp(), SeatRequest{Seat: managerSeatID}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.c.SeatVacate(ctx, b.farmOp(), SeatRequest{Seat: managerSeatID}); err != nil {
		t.Fatal(err)
	}
	before := b.managerSeat(t).Epoch
	h, err := b.c.HoldSet(ctx, OperatorActor(b.farmOp()), HoldSetRequest{Scope: HoldScope{Kind: "seat", Tenant: farmTenant, Name: managerSeatID},
		Effects: []string{"no_claim"}, Reason: "manual"})
	if err != nil {
		t.Fatal(err)
	}
	if m := b.managerSeat(t); m.State != "vacant" || !m.has("human") || m.Epoch != before+1 {
		t.Fatalf("after the hold %+v", m)
	}
	if _, err := b.c.HoldRelease(ctx, OperatorActor(b.farmOp()), HoldReleaseRequest{ID: h.ID}); err != nil {
		t.Fatal(err)
	}
	if m := b.managerSeat(t); m.State != "vacant" || len(m.Reasons) != 0 || m.Epoch != before+2 {
		t.Fatalf("after the release %+v", m)
	}
}

// A new incident of a stopping class stops a held seat, and its seat task waits in the tenant
// meanwhile; seat.resume ends the stop only after people dismissed the incident; the deadline
// decide_within retires the holder with replaced_after_incident (GOV-22, MGR-2).
func TestManagerIncidentStop(t *testing.T) {
	ctx := context.Background()
	b := newManagerBench(t)
	if _, err := b.c.SeatAssign(ctx, b.farmOp(), SeatRequest{Seat: managerSeatID}); err != nil {
		t.Fatal(err)
	}
	holder := b.managerSeat(t).Holder
	b.stopAlarm(t, true)
	b.advance(t, 6*60)
	b.loop(t, "incident")
	list := incidentsOf(t, b, "spend_storm")
	if len(list) != 1 {
		t.Fatalf("incidents %d", len(list))
	}
	inc := list[0]
	m := b.managerSeat(t)
	if m.State != "stopped" || !m.has("incident") || m.reason("incident").Incident != inc.ID || m.DecideUntil == nil {
		t.Fatalf("seat %+v", m)
	}
	// The disk class of the same alarm does not stop it: no volume quotas (MGR-33).
	if disk := incidentsOf(t, b, "disk_full"); len(disk) != 1 || disk[0].Record["stops_manager_effect"] != nil {
		t.Fatalf("disk incident %+v", disk)
	}
	if _, err := b.c.SeatResume(ctx, b.farmOp(), SeatResumeRequest{Seat: managerSeatID, Incident: inc.ID}); err == nil {
		t.Fatal("resumed before the incident was dismissed")
	}
	if _, err := b.c.IncidentDismiss(ctx, b.op, IncidentRequest{ID: inc.ID, ReasonCode: "false_alarm", Details: "a test alarm"}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.c.SeatResume(ctx, b.farmOp(), SeatResumeRequest{Seat: managerSeatID, Incident: inc.ID}); err != nil {
		t.Fatal(err)
	}
	if m = b.managerSeat(t); m.State != "held" || m.Holder != holder || len(m.Reasons) != 0 {
		t.Fatalf("after resume %+v", m)
	}
	// A second stop that nobody decides: the holder leaves after decide_within.
	b.stopAlarm(t, false)
	b.advance(t, 2*3600)
	b.loop(t, "incident")
	b.stopAlarm(t, true)
	b.advance(t, 6*60)
	b.loop(t, "incident")
	if m = b.managerSeat(t); m.State != "stopped" {
		t.Fatalf("no second stop %+v", m)
	}
	b.advance(t, 7*24*3600+60)
	b.loop(t, "identity")
	if m = b.managerSeat(t); m.State != "vacant" || m.Holder != "" {
		t.Fatalf("after decide_within %+v", m)
	}
	if prev := b.loadBot(t, holder); prev.State != "retired" || prev.Retired["reason"] != "replaced_after_incident" {
		t.Fatalf("holder %+v", prev)
	}
	// The seat has been out of held for longer than 5 minutes.
	if openAlarms(t, b, "alarm.manager.absent.v1") != 1 {
		t.Fatal("no alarm.manager.absent")
	}
}

// The seat task of an incident runs in its tenant on a worker of the seat role, on behalf of the
// holder with the seat epoch; a vacant seat leaves it waiting, and a tenant without the manager
// enrolls no seat worker (MGR-1, MGR-3, MGR-42).
func TestManagerSeatTask(t *testing.T) {
	ctx := context.Background()
	b := newManagerBench(t)
	w := b.enroll(t, EnrollRequest{Tenant: "demo", Kind: "agent", Role: "farm-manager"})
	b.diskAlarm(t, true, 3)
	b.advance(t, 6*60)
	b.loop(t, "incident")
	if item := b.claim(t, w, "attempt"); item != nil {
		t.Fatal("a vacant seat's task was handed out")
	}

	if _, err := b.c.SeatAssign(ctx, b.farmOp(), SeatRequest{Seat: managerSeatID}); err != nil {
		t.Fatal(err)
	}
	m := b.managerSeat(t)
	item := b.claim(t, w, "attempt")
	if item == nil {
		t.Fatal("the holder's task was not handed out")
	}
	if item.Token.ActingBot != m.Holder || item.Token.SeatEpoch != m.Epoch {
		t.Fatalf("acting %+v seat %+v", item.Token, m)
	}
	// The task carries the code-built incident view, not only the incident id (MGR-25).
	task, _ := item.Payload["task"].(map[string]any)
	in := mmap(mmap(mmap(task, "body"), "params"), "input")
	if in["view"] != "incident/v1" || in["class"] != "disk_full" || in["tenant"] != "demo" || mstr(mmap(in, "subject"), "kind") != "resource" ||
		mmap(in, "alarm") == nil || mmap(in, "allowed") == nil || in["implicated"] == nil {
		t.Fatalf("input %v", in)
	}
	// A stop fences the running attempt in the tenant.
	if _, err := b.c.HoldSet(ctx, OperatorActor(b.farmOp()), HoldSetRequest{Scope: HoldScope{Kind: "seat", Tenant: farmTenant, Name: managerSeatID},
		Effects: []string{"no_claim"}, Reason: "manual"}); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT state FROM leases WHERE work_id = $1 AND lease_no = $2`, item.WorkID, item.LeaseNo).Scan(&state); err != nil || state != "revoked" {
		t.Fatalf("lease %s %v", state, err)
	}
}

// A tenant with farm_manager.enabled: false enrolls no worker of a seat role (MGR-42).
func TestManagerDisabledTenant(t *testing.T) {
	ctx := context.Background()
	b := newManagerBench(t, func(ms []map[string]any) {
		for _, m := range ms {
			if m["kind"] == "Tenant" {
				m["spec"].(map[string]any)["farm_manager"] = map[string]any{"enabled": false}
			}
		}
	})
	if _, err := b.c.Enroll(ctx, b.op, EnrollRequest{Tenant: "demo", Kind: "agent", Role: "farm-manager"}); err == nil {
		t.Fatal("a seat worker enrolled in a tenant without the manager")
	}
}

func (b *bench) loadBot(t *testing.T, id string) *botRow {
	t.Helper()
	var r *botRow
	err := b.c.tx(context.Background(), func(tx pgx.Tx, now time.Time) error {
		var err error
		r, err = loadBot(context.Background(), tx, id, false)
		return err
	})
	if err != nil || r == nil {
		t.Fatalf("bot %s: %v", id, err)
	}
	return r
}

// A class threshold on the free share gives a used-bytes threshold only with the volume size
// (MGR-33).
func TestUsedThreshold(t *testing.T) {
	spec := map[string]any{"detect": map[string]any{"when": []any{map[string]any{"atom": "lt", "field": "values.free_pct", "value": 5}}}}
	if _, ok := usedThreshold(spec, map[string]any{}); ok {
		t.Fatal("a threshold without the volume size")
	}
	if got, ok := usedThreshold(spec, map[string]any{"size_bytes": 1000}); !ok || got != 950 {
		t.Fatalf("free share threshold %v %v", got, ok)
	}
	used := map[string]any{"detect": map[string]any{"when": []any{map[string]any{"atom": "ge", "field": "values.used_bytes", "value": 700}}}}
	if got, ok := usedThreshold(used, nil); !ok || got != 700 {
		t.Fatalf("used bytes threshold %v %v", got, ok)
	}
}

// Disk growth compares the numbers logged at the window's start and at its end, not the
// numbers of the incident's opening (MGR-34).
func TestAlarmValuesAt(t *testing.T) {
	ctx := context.Background()
	b := newManagerBench(t)
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i, used := range []float64{100, 200, 200} {
		if _, err := b.c.st.Pool.Exec(ctx, `INSERT INTO alarm_values (episode, at, vals) VALUES ($1, $2, $3)`,
			int64(9001+i/2), t0.Add(time.Duration(i)*time.Hour), map[string]any{"used_bytes": used}); err != nil {
			t.Fatal(err)
		}
	}
	tx, err := b.c.st.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	eps := []int64{9001, 9002}
	before, _ := alarmValuesAt(ctx, tx, eps, t0.Add(-time.Minute))
	start, _ := alarmValuesAt(ctx, tx, eps, t0.Add(90*time.Minute))
	end, err := alarmValuesAt(ctx, tx, eps, t0.Add(3*time.Hour))
	if err != nil || before != nil || start["used_bytes"] != float64(200) || end["used_bytes"] != float64(200) {
		t.Fatalf("before %v start %v end %v %v", before, start, end, err)
	}
	// Growth before the window and a flat window give no stop.
	if diskGrew(start, end) {
		t.Fatal("a flat window counted as growth")
	}
}

// A seat task answers to the tenant's consent group as it stands, not to the system initiator
// (MGR-3).
func TestSeatConsent(t *testing.T) {
	ctx := context.Background()
	b := newManagerBench(t)
	cat, err := b.c.Catalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tv := cat.Tenants["demo"]
	rec := []byte(`{"seat": "farm-manager"}`)
	if p := accessPrincipal("system", rec); p != seatConsent {
		t.Fatalf("principal %s", p)
	}
	if r, err := b.c.accessLost(ctx, b.c.st.Pool, time.Now(), tv, seatConsent, "task:"+incidentRespondType, "internal"); err != nil || r != "" {
		t.Fatalf("allowed task: %s %v", r, err)
	}
	if r, _ := b.c.accessLost(ctx, b.c.st.Pool, time.Now(), tv, seatConsent, "task:ops.summary.v1", ""); r != "no_group_cap" {
		t.Fatalf("a right outside the group: %s", r)
	}
	if r, _ := b.c.accessLost(ctx, b.c.st.Pool, time.Now(), tv, seatConsent, "", "raw_logs"); r != "no_data_class" {
		t.Fatalf("a data class outside the group: %s", r)
	}
}
