// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func (b *bench) spendLine(t *testing.T, id, state string, micros int64, at time.Time) {
	t.Helper()
	if _, err := b.c.st.Pool.Exec(context.Background(), `INSERT INTO spend (id, tenant, line, unit, state, eur_micros, provider, time)
		VALUES ($1, 'demo', 'work', 'tokens', $2, $3, 'mock', $4)`, id, state, micros, at); err != nil {
		t.Fatal(err)
	}
	if _, err := b.c.st.Pool.Exec(context.Background(), `INSERT INTO spend_workspaces (tenant, provider, first_at) VALUES ('demo', 'mock', $1) ON CONFLICT DO NOTHING`, at); err != nil {
		t.Fatal(err)
	}
}

func (b *bench) alarmState(t *testing.T, alarm string) string {
	t.Helper()
	var state string
	err := b.c.st.Pool.QueryRow(context.Background(), `SELECT state FROM alarm_episodes WHERE tenant = 'demo' AND alarm = $1 ORDER BY id DESC LIMIT 1`, alarm).Scan(&state)
	if err != nil && err != pgx.ErrNoRows {
		t.Fatal(err)
	}
	return state
}

func (b *bench) used(t *testing.T) int64 {
	t.Helper()
	var used int64
	err := b.c.tx(context.Background(), func(tx pgx.Tx, now time.Time) error {
		var err error
		used, err = windowUsed(context.Background(), tx, "demo", spendFilter{}, now.Add(-90*24*time.Hour), "")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return used
}

// Reconciliation with the provider report (BUD-10, blackbox:gateway/estimate-reconciled,
// blackbox:core/reconcile-overdue): a provider day below the farm lines gives one correction
// line of the difference, the line prices stay, the estimated line of the day counts zero; a
// second request for the day replaces the correction; a workspace without a reconciliation for
// 35 days raises the overdue alarm, which only a reconciliation resolves; an estimated line
// past seven days without a reconciliation stops counting.
func TestReconcile(t *testing.T) {
	ctx := context.Background()
	b := newBench(t)
	if _, err := b.c.TestClockSet(ctx, time.Date(2027, 3, 2, 12, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	day := time.Date(2027, 3, 1, 10, 0, 0, 0, time.UTC)
	b.spendLine(t, "sp_a", "committed", 3_000_000, day)
	b.spendLine(t, "sp_b", "committed", 2_000_000, day)
	b.spendLine(t, "sp_c", "estimated", 4_000_000, day)
	if u := b.used(t); u != 9_000_000 {
		t.Fatalf("used before %d", u)
	}
	farm := Operator{Principal: b.op.Principal, Tenant: "farm"}
	req := SpendReconcileRequest{Day: "2027-03-01", Provider: "mock", Rows: []SpendReconcileItem{{Tenant: "demo", SpentEURMicros: 6_000_000}}}
	if _, err := b.c.SpendReconcile(ctx, farm, req); err != nil {
		t.Fatal(err)
	}
	var corr int64
	var n int
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*), coalesce(sum(correction_eur_micros), 0) FROM spend WHERE unit = 'correction'`).Scan(&n, &corr); err != nil || n != 1 || corr != 1_000_000 {
		t.Fatalf("correction %d lines, %d %v", n, corr, err)
	}
	if u := b.used(t); u != 6_000_000 {
		t.Fatalf("used after the reconciliation %d, want the provider's 6000000", u)
	}
	var price int64
	var reconciled bool
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT eur_micros, reconciled_at IS NOT NULL FROM spend WHERE id = 'sp_c'`).Scan(&price, &reconciled); err != nil || price != 4_000_000 || !reconciled {
		t.Fatalf("estimated line %d %v %v", price, reconciled, err)
	}
	// The same day again, now within the tolerance of a 0.5 euro difference: one correction.
	req.Rows[0].SpentEURMicros = 5_400_000
	if _, err := b.c.SpendReconcile(ctx, farm, req); err != nil {
		t.Fatal(err)
	}
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*), coalesce(sum(correction_eur_micros), 0) FROM spend WHERE unit = 'correction'`).Scan(&n, &corr); err != nil || n != 1 || corr != 400_000 {
		t.Fatalf("correction again %d lines, %d %v", n, corr, err)
	}
	if b.alarmState(t, "alarm.spend.estimate_drift.v1") == "firing" {
		t.Fatal("a difference within the tolerance keeps the drift alarm")
	}
	// A tenant operator, today and a day with an open reserve are refused.
	if _, err := b.c.SpendReconcile(ctx, b.op, SpendReconcileRequest{Day: "2027-03-01", Provider: "mock", Rows: []SpendReconcileItem{{Tenant: "demo", SpentEURMicros: 1}}}); err == nil {
		t.Fatal("a tenant operator reconciled its own spend")
	}
	b.spendLine(t, "sp_open", "reserved", 1_000, day.Add(-24*time.Hour))
	if _, err := b.c.SpendReconcile(ctx, farm, SpendReconcileRequest{Day: "2027-02-28", Provider: "mock", Rows: req.Rows}); err == nil {
		t.Fatal("a day with an open reserve was reconciled")
	}
	// Two rows of one tenant add up.
	if _, err := b.c.SpendReconcile(ctx, farm, SpendReconcileRequest{Day: "2027-03-01", Provider: "MOCK", Rows: []SpendReconcileItem{
		{Tenant: "demo", SpentEURMicros: 5_000_000}, {Tenant: "demo", SpentEURMicros: 400_000}}}); err != nil {
		t.Fatal(err)
	}
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*), coalesce(sum(correction_eur_micros), 0) FROM spend WHERE unit = 'correction'`).Scan(&n, &corr); err != nil || n != 1 || corr != 400_000 {
		t.Fatalf("correction of two rows %d lines, %d %v", n, corr, err)
	}
	if _, err := b.c.st.Pool.Exec(ctx, `DELETE FROM spend WHERE id = 'sp_open'`); err != nil {
		t.Fatal(err)
	}
	if _, err := b.c.SpendReconcile(ctx, farm, SpendReconcileRequest{Day: "2027-03-02", Provider: "mock", Rows: req.Rows}); err == nil {
		t.Fatal("today was reconciled")
	}

	// 36 days later without a reconciliation: overdue; a new estimated line is released after 7 days.
	b.advance(t, 36*24*3600)
	if err := b.c.tickLoop(ctx, "budget"); err != nil {
		t.Fatal(err)
	}
	if s := b.alarmState(t, "alarm.spend.reconcile_overdue.v1"); s != "firing" {
		t.Fatalf("overdue alarm %q", s)
	}
	var now time.Time
	_ = b.c.st.Pool.QueryRow(ctx, `SELECT botaru_now()`).Scan(&now)
	b.spendLine(t, "sp_d", "estimated", 7_000_000, now.Add(-8*24*time.Hour))
	before := b.used(t)
	b.advance(t, 61)
	if err := b.c.tickLoop(ctx, "budget"); err != nil {
		t.Fatal(err)
	}
	if after := b.used(t); before-after != 7_000_000 {
		t.Fatalf("the released estimate still counts: %d -> %d", before, after)
	}
	day2 := now.Add(-24 * time.Hour).Format("2006-01-02")
	if _, err := b.c.SpendReconcile(ctx, farm, SpendReconcileRequest{Day: day2, Provider: "mock", Rows: []SpendReconcileItem{{Tenant: "demo", SpentEURMicros: 0}}}); err != nil {
		t.Fatal(err)
	}
	if s := b.alarmState(t, "alarm.spend.reconcile_overdue.v1"); s != "resolved" {
		t.Fatalf("overdue alarm after a reconciliation %q", s)
	}
	d, err := b.c.Doctor(ctx, b.op, TenantRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var seen []string
	for _, ch := range d.Checks {
		if strings.HasPrefix(ch.ID, "spend.") || strings.HasPrefix(ch.ID, "budget.") || strings.HasPrefix(ch.ID, "model.price") {
			seen = append(seen, ch.ID)
		}
	}
	if !strings.Contains(strings.Join(seen, " "), "spend.reconcile_age_s.mock") || !strings.Contains(strings.Join(seen, " "), "budget.used_30d_eur_micros.work") {
		t.Fatalf("doctor money checks %v", seen)
	}
}

// Retention (RET-2, RET-3, blackbox:core/retention-minimal, blackbox:core/tombstone): the
// content of a task finished more than 30 days ago is deleted by the loop while its metadata
// stays, a younger one keeps its content, and the key of a deleted task answers gone.
func TestRetention(t *testing.T) {
	ctx := context.Background()
	b := newBench(t)
	old := b.helloDone(t, "1", "", "old answer")
	b.advance(t, 20*24*3600)
	young := b.helloDone(t, "2", "", "young answer")
	b.advance(t, 11*24*3600)
	if err := b.c.tickLoop(ctx, "retention"); err != nil {
		t.Fatal(err)
	}
	var oldGone, youngKept bool
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT (SELECT body IS NULL AND result IS NULL FROM tasks WHERE id = $1), (SELECT body IS NOT NULL FROM tasks WHERE id = $2)`, old, young).Scan(&oldGone, &youngKept); err != nil || !oldGone || !youngKept {
		t.Fatalf("old deleted %v, young kept %v, %v", oldGone, youngKept, err)
	}
	var key string
	var events int
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT idempotency_key, (SELECT count(*) FROM events WHERE subject = $1) FROM tasks WHERE id = $1`, old).Scan(&key, &events); err != nil || events == 0 {
		t.Fatalf("the metadata of the deleted task: %d events %v", events, err)
	}
	cat, err := b.c.Catalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	err = b.c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		_, err := b.c.enqueue(ctx, tx, now, cat, enqueueInput{Tenant: "demo", Type: "hello.greet.v1", IdempotencyKey: key, Initiator: b.alice,
			Body: map[string]any{"type": "hello.greet.v1", "goal": "hi"}, EnqueuedBy: "channel:telegram", Source: "channel"})
		return err
	})
	if p, ok := AsProblem(err); !ok || p.Reason != "gone" {
		t.Fatalf("the key of a deleted task: %v", err)
	}
}

// The core drain (LCY-6, blackbox:core/health-draining): claim answers not_ready, health
// answers draining with the seconds to the deadline, a tenant operator cannot drain, and the
// next core start hands out work again.
func TestDrain(t *testing.T) {
	ctx := context.Background()
	b := newBench(t)
	if _, err := b.c.CoreDrain(ctx, b.op, CoreDrainRequest{Until: "10m"}); err == nil {
		t.Fatal("a tenant operator drained the core")
	}
	if _, err := b.c.CoreDrain(ctx, Operator{Principal: b.op.Principal, Tenant: "farm"}, CoreDrainRequest{Until: "10m"}); err != nil {
		t.Fatal(err)
	}
	b.hello(t, "1")
	wait := 0
	_, err := b.c.Claim(ctx, b.agent, ClaimRequest{Kinds: []string{"attempt"}, WaitS: &wait})
	if p, ok := AsProblem(err); !ok || p.Reason != "not_ready" || p.RetryAfterS == 0 {
		t.Fatalf("claim while draining: %v", err)
	}
	if _, err := b.c.Claim(ctx, b.channel, ClaimRequest{Kinds: []string{"delivery"}, WaitS: &wait}); err != nil {
		t.Fatalf("a delivery claim while draining: %v", err)
	}
	h := b.c.Health(ctx)
	if h.Alive || h.Reason != "draining" || h.DrainUntilS == nil || *h.DrainUntilS < 590 {
		t.Fatalf("health while draining: %+v", h)
	}
	if err := b.c.ClearDrain(ctx); err != nil {
		t.Fatal(err)
	}
	if w := b.claim(t, b.agent, "attempt"); w == nil {
		t.Fatal("no work after the next start")
	}
}
