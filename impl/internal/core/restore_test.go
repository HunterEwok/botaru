// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// A restore from a backup (RET-6, blackbox:core/restore, blackbox:core/restore-spend-pending):
// the epoch grows, a pending approval becomes canceled(restored), an approval whose write call
// was leased becomes exec_unknown with the call, an erasure request of the separate log enters
// the store again, P1 is not ready; restore.complete refuses while the spend gap is open, and
// with spend_pending becomes ready with alarm.spend.unreconciled and halved daily limits, which
// a reconciliation of the gap removes.
func TestRestore(t *testing.T) {
	ctx := context.Background()
	b := newActionBench(t, nil)
	att, h := b.attemptTools(t, "1", false)
	r := b.request(t, h, map[string]any{"action": "fix_web", "args": map[string]any{"host": "web1"}})
	pend := b.request(t, h, map[string]any{"action": "fix_web", "args": map[string]any{"host": "web1", "level": "hard"}})
	b.finish(t, att)
	b.tick(t, "approval-expiry")
	card := cardOf(t, b.drain(t), approvalCard, "direct:1002")
	a := b.approval(t, r.ApprovalID)
	if _, err := b.c.Decide(ctx, b.channel, press("telegram:1002", "direct:1002", card.external), DecideRequest{SubjectKind: "approval", ID: a.ID, Nonce: a.Nonce, Decision: "approve"}); err != nil {
		t.Fatal(err)
	}
	b.claimTool(t, b.th)
	if _, err := b.c.st.Pool.Exec(ctx, `INSERT INTO spend (id, tenant, line, unit, state, eur_micros, provider, time)
		VALUES ('sp_r', 'demo', 'work', 'tokens', 'committed', 1000, 'mock', botaru_now() - interval '2 days')`); err != nil {
		t.Fatal(err)
	}
	if _, err := b.c.st.Pool.Exec(ctx, `INSERT INTO spend_workspaces (tenant, provider, first_at) VALUES ('demo', 'mock', botaru_now() - interval '2 days') ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	log, err := b.c.ErasureLogExport(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// A request the backup does not have: made after the backup moment.
	log = append(log, ErasureLogEntry{ID: "er_after_backup", Tenant: "demo", Reason: "subject_request", Author: json.RawMessage(`{"principal": "system"}`),
		Scope: json.RawMessage(`{"tenant": "demo", "subject": "` + b.alice + `"}`), CreatedAt: time.Now()})
	epoch := b.c.Epoch()
	n, err := b.c.RestoreBegin(ctx, log)
	if err != nil || n != 1 {
		t.Fatalf("restore begin: %d %v", n, err)
	}
	// The backup was taken two days ago: the spend gap runs from then to yesterday.
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE farm_meta SET restore_point = botaru_now() - interval '2 days'`); err != nil {
		t.Fatal(err)
	}
	if err := b.c.settleRestore(ctx); err != nil {
		t.Fatal(err)
	}
	if err := b.c.loadRestoring(ctx); err != nil {
		t.Fatal(err)
	}
	var newEpoch int64
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT epoch FROM farm_meta`).Scan(&newEpoch); err != nil || newEpoch != epoch+1 {
		t.Fatalf("epoch %d -> %d %v", epoch, newEpoch, err)
	}
	if s := b.approval(t, pend.ApprovalID); s.State != "canceled" || s.Reason != "restored" {
		t.Fatalf("pending approval after a restore: %s %s", s.State, s.Reason)
	}
	ex := b.approval(t, a.ID)
	var callState string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT state FROM toolcalls WHERE id = (SELECT toolcall FROM approvals WHERE id = $1)`, a.ID).Scan(&callState); err != nil || ex.State != "exec_unknown" || callState != "exec_unknown" {
		t.Fatalf("executing approval after a restore: %s, call %s %v", ex.State, callState, err)
	}
	var replayed string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT state FROM erasure_requests WHERE id = 'er_after_backup'`).Scan(&replayed); err != nil || replayed != "requested" {
		t.Fatalf("replayed erasure request %q %v", replayed, err)
	}
	if err := b.c.P1Ready("claim"); err == nil {
		t.Fatal("P1 is ready during a restore")
	}
	farm := Operator{Principal: b.op.Principal, Tenant: "farm"}
	if _, err := b.c.RestoreComplete(ctx, farm, RestoreCompleteRequest{}); err == nil {
		t.Fatal("restore.complete passed with an open spend gap")
	}
	if _, err := b.c.RestoreComplete(ctx, farm, RestoreCompleteRequest{SpendPending: true}); err != nil {
		t.Fatal(err)
	}
	if err := b.c.P1Ready("claim"); err != nil {
		t.Fatalf("not ready after restore.complete: %v", err)
	}
	if s := b.alarmState(t, "alarm.spend.unreconciled.v1", "farm"); s != "firing" {
		t.Fatalf("unreconciled alarm %q", s)
	}
	if open := b.gapOpen(t); !open {
		t.Fatal("the spend gap is not open")
	}
	var now time.Time
	_ = b.c.st.Pool.QueryRow(ctx, `SELECT botaru_now()`).Scan(&now)
	for d := now.Add(-2 * 24 * time.Hour); d.Before(now.UTC().Truncate(24 * time.Hour)); d = d.Add(24 * time.Hour) {
		if _, err := b.c.SpendReconcile(ctx, farm, SpendReconcileRequest{Day: d.Format("2006-01-02"), Provider: "mock", Rows: []SpendReconcileItem{{Tenant: "demo", SpentEURMicros: 1000}}}); err != nil {
			t.Fatal(err)
		}
	}
	if open := b.gapOpen(t); open {
		t.Fatal("the spend gap stays open after its reconciliation")
	}
	if s := b.alarmState(t, "alarm.spend.unreconciled.v1", "farm"); s != "resolved" {
		t.Fatalf("unreconciled alarm after the reconciliation %q", s)
	}
}

func (b *bench) gapOpen(t *testing.T) bool {
	t.Helper()
	var open bool
	err := b.c.tx(context.Background(), func(tx pgx.Tx, now time.Time) error {
		var err error
		open, err = spendGapOpen(context.Background(), tx)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return open
}

// Backup reports (RET-8, blackbox:core/backup-report): without profile monitor, no good backup
// of a location for 26 hours raises alarm.backup.overdue and a failed restore check raises
// alarm.backup.restore_check_overdue at once; a fresh good backup resolves the first; an erasure
// log backup older than the store backup fails doctor.
func TestBackupReport(t *testing.T) {
	ctx := context.Background()
	b := newBench(t)
	b.c.cfg.Backup = BackupConfig{Locations: []string{"offsite-1"}, ErasureLogLocation: "offsite-1-erasure"}
	farm := Operator{Principal: b.op.Principal, Tenant: "farm"}
	report := func(kind, loc string, ok bool) {
		t.Helper()
		var now time.Time
		_ = b.c.st.Pool.QueryRow(ctx, `SELECT botaru_now()`).Scan(&now)
		if _, err := b.c.BackupReport(ctx, farm, BackupReportRequest{Kind: kind, LocationID: loc, OK: ok,
			StartedAt: now.Add(-time.Minute).Format(time.RFC3339), FinishedAt: now.Format(time.RFC3339)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := b.c.BackupReport(ctx, b.op, BackupReportRequest{Kind: "backup", LocationID: "offsite-1", StartedAt: "2027-01-01T00:00:00Z", FinishedAt: "2027-01-01T00:01:00Z"}); err == nil {
		t.Fatal("a tenant operator reported a backup")
	}
	report("backup", "offsite-1-erasure", true)
	b.advance(t, 60)
	report("backup", "offsite-1", true)
	report("restore_check", "offsite-1", false)
	if err := b.c.tickLoop(ctx, "health"); err != nil {
		t.Fatal(err)
	}
	if s := b.alarmState(t, "alarm.backup.restore_check_overdue.v1", "farm"); s != "firing" {
		t.Fatalf("a failed restore check: %q", s)
	}
	if s := b.alarmState(t, "alarm.backup.overdue.v1", "farm"); s == "firing" {
		t.Fatal("a fresh backup is overdue")
	}
	d, err := b.c.Doctor(ctx, farm, TenantRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for _, ch := range d.Checks {
		if ch.ID == "backup.erasure_log_age_s" && ch.OK {
			t.Fatal("an erasure log backup older than the store backup passes doctor")
		}
	}
	b.advance(t, 27*3600)
	if err := b.c.tickLoop(ctx, "health"); err != nil {
		t.Fatal(err)
	}
	if s := b.alarmState(t, "alarm.backup.overdue.v1", "farm"); s != "firing" {
		t.Fatalf("no backup for 27 hours: %q", s)
	}
	report("backup", "offsite-1", true)
	if err := b.c.tickLoop(ctx, "health"); err != nil {
		t.Fatal(err)
	}
	if s := b.alarmState(t, "alarm.backup.overdue.v1", "farm"); s != "resolved" {
		t.Fatalf("after a fresh backup: %q", s)
	}
}
