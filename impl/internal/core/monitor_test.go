// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func (b *bench) monitorOn(t *testing.T) *Worker {
	t.Helper()
	b.c.cfg.Monitor = MonitorConfig{Enabled: true}
	return b.enroll(t, EnrollRequest{Tenant: "demo", Kind: "monitor", Target: "zabbix"})
}

func countKind(t *testing.T, b *bench, kind, state string) int {
	t.Helper()
	var n int
	if err := b.c.st.Pool.QueryRow(context.Background(), `SELECT count(*) FROM deliveries WHERE kind = $1 AND ($2 = '' OR state = $2)`, kind, state).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// With profile monitor an episode change is one monitor_alarm delivery with tags and no
// alarm_notice; a repeated firing gives none; resolved waits for firing; the export does not
// die by attempts; the first success after a failure exports the open episodes again
// (MON-11, MON-12, blackbox:monitor/export-retry, blackbox:monitor/export-order,
// blackbox:core/alarm-path-monitor).
func TestMonitorExport(t *testing.T) {
	ctx := context.Background()
	b := newBench(t)
	b.withAdmins(t)
	mon := b.monitorOn(t)
	a := Alarm{Tenant: "demo", Name: "worker.stale", Subject: AlarmSubject{Kind: "worker", ID: "wk_1"}, Severity: "warn"}
	for range 60 {
		b.alarmTx(t, func(tx pgx.Tx, now time.Time) error { return b.c.fireAlarm(ctx, tx, now, a) })
	}
	if n := countKind(t, b, "monitor_alarm", ""); n != 1 || countNotices(t, b) != 0 {
		t.Fatalf("monitor_alarm %d, alarm_notice %d", n, countNotices(t, b))
	}
	item := b.claim(t, mon, "delivery")
	if item == nil {
		t.Fatal("no monitor_alarm for the adapter")
	}
	b.alarmTx(t, func(tx pgx.Tx, now time.Time) error { return b.c.resolveAlarm(ctx, tx, now, a) })
	body := dbody(item)
	if body["state"] != "firing" || body["key"] != "demo:worker.stale:wk_1" || body["scope"] != "tenant" || body["origin"] != "code" || body["severity"] != "warn" {
		t.Fatalf("export body %v", body)
	}
	// resolved does not go out while firing is being exported (MON-11).
	if next := b.claim(t, mon, "delivery"); next != nil {
		t.Fatalf("resolved went out before firing: %v", dbody(next))
	}
	// The monitoring system is down for 30 minutes: the delivery is retried, never dead.
	for range 8 {
		if _, err := b.c.Fail(ctx, mon, header(t, item), FailRequest{Reason: "provider_unavailable", Retryable: true}); err != nil {
			t.Fatal(err)
		}
		b.advance(t, 600)
		if item = b.claim(t, mon, "delivery"); item == nil {
			t.Fatal("the export was not retried")
		}
	}
	if countKind(t, b, "monitor_alarm", "dead") != 0 {
		t.Fatal("an export died by attempts")
	}
	// A second episode opens while the system is down; the first success exports it again.
	other := Alarm{Tenant: "demo", Name: "worker.stale", Subject: AlarmSubject{Kind: "worker", ID: "wk_2"}, Severity: "warn"}
	b.alarmTx(t, func(tx pgx.Tx, now time.Time) error { return b.c.fireAlarm(ctx, tx, now, other) })
	if _, err := b.c.Complete(ctx, mon, header(t, item), CompleteRequest{Receipt: &Receipt{ExternalID: "z1"}}); err != nil {
		t.Fatal(err)
	}
	var exported, heads int
	b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM deliveries d JOIN alarm_episodes e ON e.id = d.episode WHERE d.kind = 'monitor_alarm' AND e.alarmkey = 'worker.stale:wk_2'`).Scan(&exported)
	b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM deliveries WHERE kind = 'monitor_audit_head' AND state = 'planned'`).Scan(&heads)
	if exported != 1 || heads != 1 {
		t.Fatalf("after recovery: the planned export of the second episode %d (want 1, not a second copy), heads %d", exported, heads)
	}
	r := b.c.Health(ctx)
	if age, ok := r.MonitorExportAgeS["demo"]; !ok || age != 0 {
		t.Fatalf("export age %v", r.MonitorExportAgeS)
	}
}

// Export fails for 15 minutes: one alarm.monitor.export_stalled and one code message to the
// admins; after 20 more minutes no second message; the export coming back resolves the alarm
// (MON-11, MON-12, blackbox:monitor/export-stalled).
func TestMonitorExportStalled(t *testing.T) {
	ctx := context.Background()
	b := newBench(t)
	b.withAdmins(t)
	b.monitorOn(t)
	a := Alarm{Tenant: "demo", Name: "worker.stale", Subject: AlarmSubject{Kind: "worker", ID: "wk_1"}, Severity: "warn"}
	b.alarmTx(t, func(tx pgx.Tx, now time.Time) error { return b.c.fireAlarm(ctx, tx, now, a) })
	b.advance(t, 16*60)
	b.loop(t, "monitor")
	if s := b.alarmState(t, "alarm.monitor.export_stalled.v1", "demo"); s != "firing" {
		t.Fatalf("export_stalled %q", s)
	}
	if n := countKind(t, b, "monitor_export_stalled", ""); n == 0 {
		t.Fatal("no monitor_export_stalled message")
	}
	n := countKind(t, b, "monitor_export_stalled", "")
	b.advance(t, 20*60)
	b.loop(t, "monitor")
	if countKind(t, b, "monitor_export_stalled", "") != n {
		t.Fatal("a second message within 6 hours")
	}
	d, err := b.c.Doctor(ctx, b.op, TenantRequest{})
	if err != nil {
		t.Fatal(err)
	}
	failed := map[string]bool{}
	for _, ch := range d.Checks {
		if !ch.OK && ch.Severity == "fail" {
			failed[ch.ID] = true
		}
	}
	if !failed["monitor.export_age_s"] || !failed["monitor.planned_old"] {
		t.Fatalf("doctor checks %v", failed)
	}
}

// The audit-head loop puts a signed head once a day; verify accepts the witness history and
// finds a head with a changed hash, a foreign signature, seq falling within an epoch and an
// edit of the store (MON-3, blackbox:core/audit-head-signed).
func TestAuditHead(t *testing.T) {
	ctx := context.Background()
	b := newBench(t)
	mon := b.monitorOn(t)
	var heads []AuditHead
	take := func() {
		t.Helper()
		b.loop(t, "audit-head")
		for {
			item := b.claim(t, mon, "delivery")
			if item == nil {
				break
			}
			body := dbody(item)
			if body["tenant"] == "demo" {
				heads = append(heads, AuditHead{Tenant: "demo", Epoch: int64(body["epoch"].(float64)), Seq: int64(body["seq"].(float64)),
					Hash: body["hash"].(string), Sig: body["sig"].(string), KeyID: body["key_id"].(string)})
			}
			if _, err := b.c.Complete(ctx, mon, header(t, item), CompleteRequest{Receipt: &Receipt{ExternalID: "h"}}); err != nil {
				t.Fatal(err)
			}
		}
	}
	take()
	b.loop(t, "audit-head")
	var again int
	b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM deliveries WHERE kind = 'monitor_audit_head' AND tenant = 'demo' AND state = 'planned'`).Scan(&again)
	if again != 0 {
		t.Fatal("a second head within a day")
	}
	if _, err := b.c.Grant(ctx, b.op, GrantRequest{Op: "grant", Subject: b.alice, Group: "hello"}); err != nil {
		t.Fatal(err)
	}
	b.advance(t, 25*3600)
	take()
	if len(heads) != 2 || heads[1].Seq <= heads[0].Seq {
		t.Fatalf("heads %v", heads)
	}
	verify := func(hs []AuditHead) *AuditVerifyResult {
		t.Helper()
		r, err := b.c.AuditVerify(ctx, "demo", hs)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	if r := verify(heads); !r.OK {
		t.Fatalf("a good witness: %+v", r)
	}
	// The token key rotates away: the heads it signed still verify by the kept public key.
	rotated, err := New(ctx, Config{TokenKey: []byte("fedcba9876543210fedcba9876543210"), KeyID: "k2", TestBench: true}, b.c.st, b.c.spec, b.c.texts)
	if err != nil {
		t.Fatal(err)
	}
	if r, err := rotated.AuditVerify(ctx, "demo", heads); err != nil || !r.OK {
		t.Fatalf("a good witness after the key rotated away: %+v %v", r, err)
	}
	bad := append([]AuditHead{}, heads...)
	bad[1].Hash = heads[0].Hash
	if r := verify(bad); r.OK || r.Reason != "head_signature" {
		t.Fatalf("a changed hash: %+v", r)
	}
	if r := verify([]AuditHead{heads[1], heads[0]}); r.OK || r.Reason != "head_seq" {
		t.Fatalf("seq falling within an epoch: %+v", r)
	}
	foreign := heads[1]
	foreign.KeyID = "k9"
	if r := verify([]AuditHead{foreign}); r.OK || r.Reason != "head_signature" {
		t.Fatalf("an unknown key: %+v", r)
	}
	// An edit of a record in the store, with the chain rebuilt around it, no longer matches
	// the signed head.
	// Whoever owns the store can lift the append-only trigger.
	for _, q := range []string{`ALTER TABLE audit DISABLE TRIGGER audit_no_update`,
		`UPDATE audit SET hash = 'sha256:0000000000000000000000000000000000000000000000000000000000000000' WHERE tenant = 'demo' AND seq = ` + strconv.FormatInt(heads[0].Seq, 10),
		`ALTER TABLE audit ENABLE TRIGGER audit_no_update`} {
		if _, err := b.c.st.Pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if r := verify(heads[:1]); r.OK {
		t.Fatalf("an edited store: %+v", r)
	}
	d, err := b.c.Doctor(ctx, Operator{Principal: b.op.Principal, Tenant: "farm"}, TenantRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for _, ch := range d.Checks {
		if ch.ID == "monitor.audit_head_age_s" && ch.Tenant == "demo" && !ch.OK {
			t.Fatalf("head age %+v", ch)
		}
	}
}

// A hold with no_delivery does not withhold the export; a newer change of the same alarm
// replaces one not handed out yet (MON-1, MON-11).
func TestMonitorExportHoldAndSupersede(t *testing.T) {
	ctx := context.Background()
	b := newBench(t)
	b.withAdmins(t)
	mon := b.monitorOn(t)
	if _, err := b.c.HoldSet(ctx, OperatorActor(b.op), HoldSetRequest{
		Scope: HoldScope{Kind: "tenant", Tenant: "demo"}, Effects: []string{"no_delivery"}, Reason: "manual"}); err != nil {
		t.Fatal(err)
	}
	a := Alarm{Tenant: "demo", Name: "worker.stale", Subject: AlarmSubject{Kind: "worker", ID: "wk_1"}, Severity: "warn"}
	for range 3 {
		b.alarmTx(t, func(tx pgx.Tx, now time.Time) error { return b.c.fireAlarm(ctx, tx, now, a) })
		b.alarmTx(t, func(tx pgx.Tx, now time.Time) error { return b.c.resolveAlarm(ctx, tx, now, a) })
	}
	if n := countKind(t, b, "monitor_alarm", "planned"); n != 1 {
		t.Fatalf("planned exports of one alarm %d, want 1", n)
	}
	item := b.claim(t, mon, "delivery")
	if item == nil || dbody(item)["state"] != "resolved" {
		t.Fatalf("the hold withheld the export, or the newest change is not the one left: %v", item)
	}
}

func dbody(w *WorkItem) map[string]any {
	d, _ := w.Payload["delivery"].(map[string]any)
	b, _ := d["body"].(map[string]any)
	return b
}

func (b *bench) loop(t *testing.T, loop string) {
	t.Helper()
	if err := b.c.tickLoop(context.Background(), loop); err != nil {
		t.Fatal(err)
	}
}
