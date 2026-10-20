// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// helloGrant is the model grant of the hello role's main slot, as P4 resolves it.
func helloGrant(t *testing.T, b *bench) *ModelGrant {
	t.Helper()
	cat, err := b.c.Catalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, name := mainSlot(cat.Roles["hello"])
	p, ok := cat.Profile("demo", name)
	if !ok {
		t.Fatalf("no profile %s", name)
	}
	return &ModelGrant{Tenant: "demo", Role: "hello", ProfileName: name, Profile: p}
}

func taskReason(t *testing.T, b *bench, id string) string {
	t.Helper()
	var r *string
	if err := b.c.st.Pool.QueryRow(context.Background(), `SELECT reason FROM tasks WHERE id = $1`, id).Scan(&r); err != nil {
		t.Fatal(err)
	}
	if r == nil {
		return ""
	}
	return *r
}

// A series of provider failures opens the circuit of the pair: P4 answers provider_unavailable
// at once and the dispatcher hands out no attempt; after the pause one probe goes, a failed
// probe opens the circuit for twice the pause, a good one closes it (GW-11, D18,
// blackbox:gateway/outage-30m).
func TestProviderCircuit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newBench(t)
	g := helloGrant(t, b)
	for range breakerFailures {
		if err := b.c.ProviderGate(ctx, g); err != nil {
			t.Fatalf("gate before the circuit opened: %v", err)
		}
		if err := b.c.ProviderOutcome(ctx, g, "provider_unavailable", 0); err != nil {
			t.Fatal(err)
		}
	}
	wantReason(t, b.c.ProviderGate(ctx, g), "provider_unavailable")
	task := b.hello(t, "1")
	if w := b.claim(t, b.agent, "attempt"); w != nil {
		t.Fatal("an attempt of a pair with an open circuit was handed out")
	}
	if r := taskReason(t, b, task); r != "provider_unavailable" {
		t.Fatalf("waiting reason %q", r)
	}
	b.advance(t, 31)
	if err := b.c.ProviderGate(ctx, g); err != nil {
		t.Fatalf("the probe after the pause: %v", err)
	}
	wantReason(t, b.c.ProviderGate(ctx, g), "provider_unavailable")
	if err := b.c.ProviderOutcome(ctx, g, "provider_unavailable", 0); err != nil {
		t.Fatal(err)
	}
	b.advance(t, 31)
	wantReason(t, b.c.ProviderGate(ctx, g), "provider_unavailable")
	b.advance(t, 30)
	if err := b.c.ProviderGate(ctx, g); err != nil {
		t.Fatalf("the probe after the doubled pause: %v", err)
	}
	if err := b.c.ProviderOutcome(ctx, g, "", 0); err != nil {
		t.Fatal(err)
	}
	if err := b.c.ProviderGate(ctx, g); err != nil {
		t.Fatalf("gate after a good probe: %v", err)
	}
	if w := b.claim(t, b.agent, "attempt"); w == nil {
		t.Fatal("no attempt after the circuit closed")
	}
}

// A provider rate limit makes the pair wait for the time the provider gave, with the waiting
// reason provider_quota (GW-10, D19); a configuration error closes the pair until an apply and
// raises an alarm (GW-13).
func TestProviderRateAndConfig(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newBench(t)
	g := helloGrant(t, b)
	if err := b.c.ProviderOutcome(ctx, g, "provider_rate_limited", 20); err != nil {
		t.Fatal(err)
	}
	err := b.c.ProviderGate(ctx, g)
	wantReason(t, err, "provider_rate_limited")
	if p, _ := AsProblem(err); p.RetryAfterS < 19 || p.RetryAfterS > 21 {
		t.Fatalf("retry_after_s %d", p.RetryAfterS)
	}
	task := b.hello(t, "1")
	if w := b.claim(t, b.agent, "attempt"); w != nil || taskReason(t, b, task) != "provider_quota" {
		t.Fatalf("a pair waiting for the rate limit: %+v %q", w, taskReason(t, b, task))
	}
	b.advance(t, 21)
	if err := b.c.ProviderGate(ctx, g); err != nil {
		t.Fatalf("after the wait: %v", err)
	}
	if err := b.c.ProviderOutcome(ctx, g, "provider_config", 0); err != nil {
		t.Fatal(err)
	}
	wantReason(t, b.c.ProviderGate(ctx, g), "provider_config")
	if n := countEvents(t, b, "alarm.provider.config.v1", ""); n != 1 {
		t.Fatalf("config alarm events %d", n)
	}
	b.advance(t, 3600)
	wantReason(t, b.c.ProviderGate(ctx, g), "provider_config")
	if w := b.claim(t, b.agent, "attempt"); w != nil {
		t.Fatal("an attempt of a pair with a configuration error was handed out")
	}
	if _, err := b.c.st.Pool.Exec(ctx, `DELETE FROM provider_pairs WHERE state = 'config'`); err != nil {
		t.Fatal(err)
	}
	if err := b.c.ProviderGate(ctx, g); err != nil {
		t.Fatalf("after the fix: %v", err)
	}
	if err := b.c.ProviderOutcome(ctx, g, "", 0); err != nil {
		t.Fatal(err)
	}
	if countEvents(t, b, "alarm.provider.config.v1", "") != 2 {
		t.Fatal("a good call did not resolve the config alarm")
	}
	if err := b.c.ProviderOutcome(ctx, g, "provider_spend_cap", 0); err != nil {
		t.Fatal(err)
	}
	if countEvents(t, b, "alarm.provider.spend_cap.v1", "") != 1 {
		t.Fatal("no spend cap alarm")
	}
}

// A dispatch rule with max_running leaves the task above the ceiling in submitted with the
// waiting reason type_quota and no attempt spent; when one ends, it is handed out (DSP-7,
// fx-dsp-05).
func TestTypeQuota(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newBenchWith(t, func(root string, ms []map[string]any) []map[string]any {
		for _, m := range ms {
			if m["kind"] == "Tenant" {
				m["spec"].(map[string]any)["dispatch"] = map[string]any{"rules": []any{
					map[string]any{"match": map[string]any{"type": "hello.greet.v1"}, "max_running": 1}}}
			}
		}
		return ms
	})
	b.hello(t, "1")
	second := b.hello(t, "2")
	first := b.claim(t, b.agent, "attempt")
	if first == nil {
		t.Fatal("no first attempt")
	}
	if w := b.claim(t, b.agent, "attempt"); w != nil {
		t.Fatal("a task above max_running was handed out")
	}
	var spent int
	b.c.st.Pool.QueryRow(ctx, `SELECT attempts_spent FROM tasks WHERE id = $1`, second).Scan(&spent)
	if r := taskReason(t, b, second); r != "type_quota" || spent != 0 {
		t.Fatalf("waiting %q spent %d", r, spent)
	}
	if _, err := b.c.Complete(ctx, b.agent, header(t, first), CompleteRequest{Result: map[string]any{"answer": "hi"}}); err != nil {
		t.Fatal(err)
	}
	if w := b.claim(t, b.agent, "attempt"); w == nil || w.WorkID != second {
		t.Fatalf("the waiting task after the first ended: %+v", w)
	}
}

// The reserve of a task that waits for the provider does not count toward the limits, and
// 80% of a limit gives one budget.threshold event per period (BUD-4,
// fixture:budget/waiting-reserve-not-threshold).
func TestBudgetThresholdAndWaitingReserve(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newBench(t)
	task := b.hello(t, "1")
	month := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	used := func() int64 {
		t.Helper()
		var n int64
		b.alarmTx(t, func(tx pgx.Tx, now time.Time) error {
			var err error
			n, err = windowUsed(ctx, tx, "demo", spendFilter{}, month, "")
			return err
		})
		return n
	}
	before := used()
	if before == 0 {
		t.Fatal("the task has no reserve")
	}
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE tasks SET reason = 'provider_unavailable' WHERE id = $1`, task); err != nil {
		t.Fatal(err)
	}
	if after := used(); after != 0 {
		t.Fatalf("a waiting reserve counted: %d", after)
	}
	cat, err := b.c.Catalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tv := *cat.Tenants["demo"]
	limit := 1.0
	tv.Spec.Budgets.MonthEUR = &limit
	for range 2 {
		b.alarmTx(t, func(tx pgx.Tx, now time.Time) error {
			if p := b.c.checkBudget(ctx, tx, now, &tv, budgetAsk{subject: "system", extra: 850_000}); p != nil {
				return p
			}
			return nil
		})
	}
	if n := countEvents(t, b, "budget.threshold", ""); n != 1 {
		t.Fatalf("threshold events %d", n)
	}
}
