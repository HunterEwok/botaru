// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"testing"
	"time"
)

// Tests of worker.drain with a deadline (LCY-4).

// worker.drain with a deadline: before it the heartbeat answer asks nothing, from it the answer
// asks for release, and release does not spend the attempt; the expired lease of a draining
// worker that stays silent past its deadline is not spent either (LCY-4).
func TestWorkerDrainDeadline(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newBench(t)
	task := b.hello(t, "1")
	att := b.claim(t, b.agent, "attempt")
	h := header(t, att)
	at := func(s int) string {
		var now time.Time
		if err := b.c.st.Pool.QueryRow(ctx, `SELECT botaru_now()`).Scan(&now); err != nil {
			t.Fatal(err)
		}
		return fmtTime(now.Add(time.Duration(s) * time.Second))
	}
	if _, err := b.c.WorkerDrain(ctx, b.op, WorkerRequest{Op: "worker.drain", Worker: b.agent.ID, Deadline: at(120)}); err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct {
		advance int
		release bool
	}{{50, false}, {50, false}, {30, true}} {
		b.advance(t, step.advance)
		hb, err := b.c.Heartbeat(ctx, b.agent, h, HeartbeatRequest{})
		if err != nil || hb.ReleaseRequested != step.release {
			t.Fatalf("heartbeat %+v %v", hb, err)
		}
	}
	if _, err := b.c.Release(ctx, b.agent, h); err != nil {
		t.Fatal(err)
	}
	spent := func(id string) int {
		var n int
		if err := b.c.st.Pool.QueryRow(ctx, `SELECT attempts_spent FROM tasks WHERE id = $1`, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if spent(task) != 0 {
		t.Fatal("the released attempt was spent")
	}
	// Another worker takes the task; it drains with a deadline and stays silent.
	other := b.enroll(t, EnrollRequest{Tenant: "demo", Role: "hello", Kind: "agent"})
	if att := b.claim(t, other, "attempt"); att == nil || att.WorkID != task {
		t.Fatalf("the other worker got %+v", att)
	}
	if _, err := b.c.WorkerDrain(ctx, b.op, WorkerRequest{Op: "worker.drain", Worker: other.ID, Deadline: at(10)}); err != nil {
		t.Fatal(err)
	}
	b.advance(t, 200)
	if err := b.c.tickLoop(ctx, "reaper"); err != nil {
		t.Fatal(err)
	}
	var reason string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT reason FROM leases WHERE work_id = $1 ORDER BY lease_no DESC LIMIT 1`, task).Scan(&reason); err != nil || reason != "worker_drain" {
		t.Fatalf("lease reason %q %v", reason, err)
	}
	if spent(task) != 0 {
		t.Fatal("the expired lease of a drained worker was spent")
	}
}
