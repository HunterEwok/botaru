// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"testing"
	"time"
)

// newScheduleBench is the hello bench with a daily schedule at 02:30 Berlin time that delivers
// to the group hello and the sink archive, and the group system-ops that may enqueue it.
func newScheduleBench(t *testing.T, cron string, skipIfWaiting *bool) *actionBench {
	t.Helper()
	b := newBenchWith(t, func(root string, ms []map[string]any) []map[string]any {
		for _, m := range ms {
			if m["kind"] == "TaskType" {
				m["spec"].(map[string]any)["timeouts"] = map[string]any{"deadline": "72h", "start_by": "72h"}
			}
		}
		sc := map[string]any{
			"cron": cron, "timezone": "Europe/Berlin",
			"task":       map[string]any{"type": "hello.greet.v1", "goal": "Say good morning."},
			"deliver_to": []any{"group:hello", "sink:archive"},
		}
		if skipIfWaiting != nil {
			sc["skip_if_waiting"] = *skipIfWaiting
		}
		return append(ms, map[string]any{
			"apiVersion": "botaru/v1alpha1", "kind": "Group",
			"metadata": map[string]any{"name": "system-ops", "tenant": "demo"},
			"spec":     map[string]any{"caps": []any{"task:hello.greet.v1"}, "data": []any{"summary"}},
		}, map[string]any{
			"apiVersion": "botaru/v1alpha1", "kind": "Sink",
			"metadata": map[string]any{"name": "archive", "tenant": "demo"},
			"spec":     map[string]any{"driver": "webhook", "ceiling": "summary"},
		}, map[string]any{
			"apiVersion": "botaru/v1alpha1", "kind": "Schedule",
			"metadata": map[string]any{"name": "morning", "tenant": "demo"},
			"spec":     sc,
		})
	})
	return &actionBench{bench: b}
}

func (b *actionBench) at(t *testing.T, at string) {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, at)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.c.TestClockSet(context.Background(), ts); err != nil {
		t.Fatal(err)
	}
}

func (b *actionBench) skips(t *testing.T) []string {
	t.Helper()
	rows, err := b.c.st.Pool.Query(context.Background(), `SELECT data->>'slot', data->>'reason' FROM events WHERE type = 'schedule.skipped' ORDER BY seq`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var slot, reason string
		if err := rows.Scan(&slot, &reason); err != nil {
			t.Fatal(err)
		}
		out = append(out, slot+" "+reason)
	}
	return out
}

func (b *actionBench) scheduled(t *testing.T) []string {
	t.Helper()
	rows, err := b.c.st.Pool.Query(context.Background(), `SELECT idempotency_key FROM tasks WHERE idempotency_key LIKE 'system:schedule:%' ORDER BY created_at`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			t.Fatal(err)
		}
		out = append(out, k)
	}
	return out
}

// A schedule runs its latest due slot once with the key system:schedule:<name>:<slot>, skips a
// local time that does not exist, skips a slot while the previous task waits, and delivers the
// result to its addresses (TIM-2, TIM-3, DLV-6, PRM-10).
func TestSchedule(t *testing.T) {
	b := newScheduleBench(t, "30 2 * * *", nil)
	b.enroll(t, EnrollRequest{Tenant: "demo", Kind: "sink", Target: "archive"})
	b.at(t, "2027-03-27T12:00:00Z")
	b.tick(t, "schedule") // the first sight: slots count from now
	b.at(t, "2027-03-28T02:00:00Z")
	b.tick(t, "schedule")
	if s := b.skips(t); len(s) != 1 || s[0] != "2027-03-28T00:59Z dst_gap" {
		t.Fatalf("skips %v", s)
	}
	b.at(t, "2027-03-29T00:31:00Z")
	b.tick(t, "schedule")
	b.tick(t, "schedule")
	if k := b.scheduled(t); len(k) != 1 || k[0] != "system:schedule:morning:2027-03-29T00:30Z" {
		t.Fatalf("tasks %v", k)
	}
	b.at(t, "2027-03-30T00:31:00Z")
	b.tick(t, "schedule")
	if s := b.skips(t); len(s) != 2 || s[1] != "2027-03-30T00:30Z waiting" {
		t.Fatalf("skips %v", s)
	}
	b.finish(t, b.claim(t, b.agent, "attempt"))
	got := b.drain(t)
	r := cardOf(t, got, "task_result", "direct:1001")
	if r.body["text"] == nil {
		t.Fatalf("result %v", r.body)
	}
	var sink int
	if err := b.c.st.Pool.QueryRow(context.Background(), `SELECT count(*) FROM deliveries WHERE exec_kind = 'sink' AND exec_name = 'archive' AND addr = 'sink:archive'`).Scan(&sink); err != nil || sink != 1 {
		t.Fatalf("sink deliveries %d %v", sink, err)
	}
}

// With skip_if_waiting: false the slot is enqueued while the previous task waits; a hold skips
// the slot; a change of the zone does not run a slot twice nor lose the next one (TIM-3).
func TestScheduleNoSkip(t *testing.T) {
	no := false
	b := newScheduleBench(t, "0 * * * *", &no)
	b.at(t, "2027-04-01T10:30:00Z")
	b.tick(t, "schedule")
	b.at(t, "2027-04-01T11:00:00Z")
	b.tick(t, "schedule")
	b.at(t, "2027-04-01T12:00:00Z")
	b.tick(t, "schedule")
	if k := b.scheduled(t); len(k) != 2 {
		t.Fatalf("tasks %v", k)
	}
	// A hold with no_enqueue on the type skips the slot with held.
	ctx := context.Background()
	set, err := b.c.HoldSet(ctx, OperatorActor(b.op), HoldSetRequest{
		Scope: HoldScope{Kind: "tasktype", Tenant: "demo", Name: "hello.greet.v1"}, Effects: []string{"no_enqueue"}, Reason: "manual"})
	if err != nil {
		t.Fatal(err)
	}
	b.at(t, "2027-04-01T13:00:00Z")
	b.tick(t, "schedule")
	if s := b.skips(t); len(s) != 1 || s[0] != "2027-04-01T13:00Z held" {
		t.Fatalf("skips %v", s)
	}
	if _, err := b.c.HoldRelease(ctx, OperatorActor(b.op), HoldReleaseRequest{ID: set.ID}); err != nil {
		t.Fatal(err)
	}
	// A changed zone takes effect from the next slot: the tick that sees it runs nothing, the
	// next slot runs once.
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE schedule_state SET timezone = 'UTC'`); err != nil {
		t.Fatal(err)
	}
	b.at(t, "2027-04-01T14:00:30Z")
	b.tick(t, "schedule")
	if k := b.scheduled(t); len(k) != 2 {
		t.Fatalf("a changed zone ran a slot at once: %v", k)
	}
	b.at(t, "2027-04-01T15:00:00Z")
	b.tick(t, "schedule")
	b.tick(t, "schedule")
	if k := b.scheduled(t); len(k) != 3 || k[2] != "system:schedule:morning:2027-04-01T15:00Z" {
		t.Fatalf("tasks %v", k)
	}
}
