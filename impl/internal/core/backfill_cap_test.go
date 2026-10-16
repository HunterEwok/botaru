// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// capTaskRow reads the state, reason, spent attempts and the kept provider reason of a task.
func capTaskRow(t *testing.T, b *bench, id string) (state, reason string, spent int, provider string) {
	t.Helper()
	var r, p *string
	if err := b.c.st.Pool.QueryRow(context.Background(), `SELECT state, reason, attempts_spent, record->>'provider_reason' FROM tasks WHERE id = $1`, id).
		Scan(&state, &r, &spent, &p); err != nil {
		t.Fatal(err)
	}
	if r != nil {
		reason = *r
	}
	if p != nil {
		provider = *p
	}
	return
}

// A failure of the model provider does not spend an attempt: the task waits and is retried
// with a pause until its deadline, ends failed(deadline_exceeded) rather than
// attempts_exhausted, and keeps the last provider reason; provider_spend_cap is not retried
// (CAP-2).
func TestBackfillProviderFailureSpendsNoAttempt(t *testing.T) {
	ctx := context.Background()
	b := newBench(t)
	task := b.hello(t, "1")
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE contacts SET ai_notice_at = now()`); err != nil {
		t.Fatal(err)
	}
	b.drain(t)
	// More provider failures than the type's attempts (2 by default).
	for i, reason := range []string{"provider_unavailable", "provider_rate_limited", "provider_quota", "provider_unavailable"} {
		att := claimFor(t, b, b.agent, task)
		r, err := b.c.Fail(ctx, b.agent, header(t, att), FailRequest{Reason: reason, Retryable: true})
		if err != nil || r.Final {
			t.Fatalf("failure %d (%s): %+v %v", i, reason, r, err)
		}
		state, _, spent, kept := capTaskRow(t, b, task)
		// CAP-2: no attempt is spent, and the last provider reason is kept.
		if state != "submitted" || spent != 0 || kept != reason {
			t.Fatalf("after %s: state %s spent %d provider_reason %s", reason, state, spent, kept)
		}
		// The pause passes, and the pair of the provider is closed again.
		if _, err := b.c.st.Pool.Exec(ctx, `UPDATE tasks SET run_at = botaru_now() WHERE id = $1`, task); err != nil {
			t.Fatal(err)
		}
		if _, err := b.c.st.Pool.Exec(ctx, `DELETE FROM provider_pairs`); err != nil {
			t.Fatal(err)
		}
	}
	b.advance(t, 7*24*3600)
	if err := b.c.deadlines(ctx); err != nil {
		t.Fatal(err)
	}
	// CAP-2: the task ends at its deadline, not with attempts_exhausted.
	if state, reason, _, kept := capTaskRow(t, b, task); state != "failed" || reason != "deadline_exceeded" || kept != "provider_unavailable" {
		t.Fatalf("at the deadline: %s %s provider_reason %s", state, reason, kept)
	}

	// CAP-2: provider_spend_cap is not retried and raises an alarm by code.
	capped := b.hello(t, "2")
	att := claimFor(t, b, b.agent, capped)
	if r, err := b.c.Fail(ctx, b.agent, header(t, att), FailRequest{Reason: "provider_spend_cap", Retryable: true}); err != nil || !r.Final {
		t.Fatalf("spend cap: %+v %v", r, err)
	}
	if state, reason, _, _ := capTaskRow(t, b, capped); state != "failed" || reason != "provider_spend_cap" {
		t.Fatalf("spend cap: %s %s", state, reason)
	}
	// The gateway reports the spend cap of the pair, which raises the alarm.
	if err := b.c.ProviderOutcome(ctx, helloGrant(t, b), "provider_spend_cap", 0); err != nil {
		t.Fatal(err)
	}
	if n := b.count(t, `SELECT count(*) FROM alarm_episodes WHERE alarm LIKE 'alarm.provider.spend_cap.%' AND state = 'firing'`); n == 0 {
		t.Fatal("no alarm of the spend cap")
	}
}

// The core neither enforces a volume's quota nor raises a disk alarm of its own: a volume of the
// tenant catalog with its writers and quotas is found by its disk subject, and its used space
// above every quota opens no alarm and no incident of the core (CAP-6).
func TestBackfillVolumeQuotasNotEnforced(t *testing.T) {
	ctx := context.Background()
	b := newToolBenchWith(t, nil, func(ms []map[string]any) []map[string]any {
		for _, m := range ms {
			if m["kind"] == "Catalog" {
				m["spec"].(map[string]any)["volumes"] = []any{map[string]any{"host": "web1", "volume": "data", "size_bytes": 4000,
					"writers": []any{map[string]any{"tenant": "demo", "quota_bytes": 2000}}}}
			}
		}
		return ms
	})
	cat, err := b.c.Catalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// CAP-6: the volume is named res:<host>:<volume> or <host>:<volume>, with its quotas.
	for _, subject := range []string{"res:web1:data", "web1:data"} {
		v := volumeOf(cat, "demo", subject)
		if v == nil || len(mlist(v, "writers")) != 1 {
			t.Fatalf("%s: volume %v", subject, v)
		}
	}
	if volumeOf(cat, "demo", "res:web1:logs") != nil {
		t.Fatal("an undeclared volume was found")
	}
	// The used space, far above the quota, as a code series from the monitoring history.
	err = b.c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		_, err := tx.Exec(ctx, `INSERT INTO obs_points (tenant, series, subject, at, num, source, data_mark, key, created_at)
			VALUES ('demo', 'farm.volume.used_bytes', 'res:web1:data', $1, 3900, 'core', 'internal', 'vol1', $1)`, now)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, loop := range []string{"observe", "alarm", "incident", "budget", "health"} {
		b.loop(t, loop)
	}
	// CAP-6: no alarm and no incident of the core for the volume.
	if n := b.count(t, `SELECT count(*) FROM alarm_episodes WHERE tenant = 'demo'`); n != 0 {
		t.Fatalf("the core raised %d alarms", n)
	}
	if n := b.count(t, `SELECT count(*) FROM incidents WHERE tenant = 'demo'`); n != 0 {
		t.Fatalf("the core opened %d incidents", n)
	}
}
