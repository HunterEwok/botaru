// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"testing"
	"time"
)

func leaseOf(t *testing.T, b *bench, work string) (state string, until time.Time, reason string) {
	t.Helper()
	var r *string
	if err := b.c.st.Pool.QueryRow(context.Background(), `SELECT state, lease_until, reason FROM leases WHERE work_id = $1 ORDER BY lease_no DESC LIMIT 1`, work).
		Scan(&state, &until, &r); err != nil {
		t.Fatal(err)
	}
	if r != nil {
		reason = *r
	}
	return
}

// After core downtime every lease that ran out during it is extended once from readiness; a
// second core copy leaving downtime later extends nothing again; a holder that renews keeps its
// work and finishes without a spent attempt; a lease not renewed ends with core_unavailable,
// spends no attempt, counts no run and returns the task to submitted without a pause, and a
// delivery to planned without spending its attempt (FEN-5, blackbox:core/core-down-5m,
// blackbox:lease/extend-two-cores).
func TestBackfillCoreDowntime(t *testing.T) {
	ctx := context.Background()
	b := newBench(t)
	kept, lost := b.hello(t, "1"), b.hello(t, "2")
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE contacts SET ai_notice_at = now()`); err != nil {
		t.Fatal(err)
	}
	b.drain(t)
	attKept := claimFor(t, b, b.agent, kept)
	attLost := claimFor(t, b, b.agent, lost)
	// A delivery in flight too: the answer of a third task.
	third := b.helloDone(t, "3", "", "Hello!")
	var delivery *WorkItem
	for w := b.claim(t, b.channel, "delivery"); w != nil; w = b.claim(t, b.channel, "delivery") {
		if w.Payload["delivery"].(map[string]any)["kind"] == "task_result" {
			delivery = w
			break
		}
		if _, err := b.c.Complete(ctx, b.channel, header(t, w), CompleteRequest{Receipt: &Receipt{ExternalID: "x" + w.WorkID}}); err != nil {
			t.Fatal(err)
		}
	}
	if delivery == nil {
		t.Fatalf("no answer delivery of %s", third)
	}
	// The core is down for 5 minutes, then becomes ready.
	b.advance(t, 5*60)
	if err := b.c.benchRestart(ctx); err != nil {
		t.Fatal(err)
	}
	now := b.now(t)
	_, until1, mark := leaseOf(t, b, kept)
	// FEN-5: extended to readiness plus the lease term.
	if !until1.After(now) || until1.After(now.Add(time.Duration(b.c.cfg.LeaseS+1)*time.Second)) || mark != "core_unavailable" {
		t.Fatalf("lease after the downtime: until %v (now %v) mark %q", until1, now, mark)
	}
	// FEN-5: a second core copy leaving downtime a minute later extends nothing again.
	b.advance(t, 30)
	if err := b.c.endLeasesOfDowntime(ctx); err != nil {
		t.Fatal(err)
	}
	if _, until2, _ := leaseOf(t, b, kept); !until2.Equal(until1) {
		t.Fatalf("the lease was extended twice: %v then %v", until1, until2)
	}
	// The holder of one renews and finishes; the other holder is gone.
	if _, err := b.c.Heartbeat(ctx, b.agent, header(t, attKept), HeartbeatRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.c.Complete(ctx, b.agent, header(t, attKept), CompleteRequest{Result: map[string]any{"answer": "Hello!"}}); err != nil {
		t.Fatal(err)
	}
	if n := b.count(t, `SELECT count(*) FROM tasks WHERE id = $1 AND state = 'completed' AND attempts_spent = 0`, kept); n != 1 {
		t.Fatal("the renewed task did not finish without a spent attempt")
	}
	_ = attLost
	b.advance(t, b.c.cfg.LeaseS+1)
	b.loop(t, "reaper")
	// FEN-5: core_unavailable, no attempt spent, no run counted, back to submitted at once.
	if state, _, reason := leaseOf(t, b, lost); state != "expired" || reason != "core_unavailable" {
		t.Fatalf("the abandoned lease: %s %s", state, reason)
	}
	if n := b.count(t, `SELECT count(*) FROM tasks WHERE id = $1 AND state = 'submitted' AND attempts_spent = 0 AND runs_counted = 0 AND run_at <= botaru_now()`, lost); n != 1 {
		var st string
		var spent, runs int
		var runAt *time.Time
		_ = b.c.st.Pool.QueryRow(ctx, `SELECT state, attempts_spent, runs_counted, run_at FROM tasks WHERE id = $1`, lost).Scan(&st, &spent, &runs, &runAt)
		t.Fatalf("the abandoned task: %s spent %d runs %d run_at %v (now %v)", st, spent, runs, runAt, b.now(t))
	}
	// FEN-5: the delivery is planned again without spending its attempt.
	if n := b.count(t, `SELECT count(*) FROM deliveries WHERE id = $1 AND state = 'planned' AND attempts = 0`, delivery.WorkID); n != 1 {
		var st string
		var attempts int
		_ = b.c.st.Pool.QueryRow(ctx, `SELECT state, attempts FROM deliveries WHERE id = $1`, delivery.WorkID).Scan(&st, &attempts)
		t.Fatalf("the delivery: %s attempts %d", st, attempts)
	}
	if w := b.claim(t, b.channel, "delivery"); w == nil || w.WorkID != delivery.WorkID {
		t.Fatalf("the delivery is not handed out at once: %+v", w)
	}
	if w := claimFor(t, b, b.agent, lost); w.LeaseNo != 2 {
		t.Fatalf("the task is not handed out at once: lease %d", w.LeaseNo)
	}
}

// Only the core ends a lease with core_unavailable: a worker's fail with it, or another reason
// the core alone determines, is refused and changes nothing, so a holder cannot requeue a
// delivery forever or skip retry.max_runs (FEN-5, DLV-1).
func TestBackfillDowntimeReasonCoreOnly(t *testing.T) {
	ctx := context.Background()
	b := newBench(t)
	task := b.helloDone(t, "1", "", "Hello!")
	var delivery *WorkItem
	for w := b.claim(t, b.channel, "delivery"); w != nil; w = b.claim(t, b.channel, "delivery") {
		if w.Payload["delivery"].(map[string]any)["kind"] == "task_result" {
			delivery = w
			break
		}
		if _, err := b.c.Complete(ctx, b.channel, header(t, w), CompleteRequest{Receipt: &Receipt{ExternalID: "x" + w.WorkID}}); err != nil {
			t.Fatal(err)
		}
	}
	if delivery == nil {
		t.Fatalf("no answer delivery of %s", task)
	}
	attempt := claimFor(t, b, b.agent, b.hello(t, "2"))
	for _, reason := range []string{"core_unavailable", "lease_expired", "worker_drain", "seat_changed", "preempted", "stalled"} {
		if _, err := b.c.Fail(ctx, b.channel, header(t, delivery), FailRequest{Reason: reason, Retryable: true}); err == nil {
			t.Errorf("a channel failed a delivery with %s", reason)
		} else {
			wantReason(t, err, "input_schema_invalid")
		}
		if _, err := b.c.Fail(ctx, b.agent, header(t, attempt), FailRequest{Reason: reason, Retryable: true}); err == nil {
			t.Errorf("an agent failed an attempt with %s", reason)
		}
	}
	if n := b.count(t, `SELECT count(*) FROM leases WHERE work_id IN ($1, $2) AND state = 'active'`, delivery.WorkID, attempt.WorkID); n != 2 {
		t.Fatalf("active leases after the refused fails: %d", n)
	}
	if n := b.count(t, `SELECT count(*) FROM deliveries WHERE id = $1 AND attempts = 1`, delivery.WorkID); n != 1 {
		t.Fatal("the delivery's attempt was given back")
	}
}
