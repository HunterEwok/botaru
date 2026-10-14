// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"testing"
)

func openAlarms(t *testing.T, b *bench, alarm string) int {
	t.Helper()
	var n int
	if err := b.c.st.Pool.QueryRow(context.Background(), `SELECT count(*) FROM alarm_episodes WHERE alarm = $1 AND state = 'firing'`, alarm).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Each loop marks its own tick in the store and health reports the age of each; a loop whose
// tick is older than the threshold makes health not alive with loop_stale and gives
// alarm.health.loop_tick for that loop (OBS-4, OBS-9).
func TestLoopTicks(t *testing.T) {
	ctx := context.Background()
	b := newBench(t)
	if err := b.c.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	h := b.c.Health(ctx)
	if !h.Alive || len(h.LoopTickAgeS) != len(loopOrder) {
		t.Fatalf("health %+v", h)
	}
	b.advance(t, 40)
	if _, err := b.c.TestLoopTick(ctx, "reaper", 1); err != nil {
		t.Fatal(err)
	}
	h = b.c.Health(ctx)
	if h.Alive || h.Reason != "loop_stale" || h.LoopTickAgeS["reaper"] != 0 || h.LoopTickAgeS["deadline"] < 40 {
		t.Fatalf("health %+v", h)
	}
	if err := b.c.healthLoop(ctx); err != nil {
		t.Fatal(err)
	}
	// reaper ticked, the four others are 40 s old; the health loop does not judge itself.
	if n := openAlarms(t, b, "alarm.health.loop_tick.v1"); n != len(loopOrder)-2 {
		t.Fatalf("%d loop tick alarms", n)
	}
	if err := b.c.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if n := openAlarms(t, b, "alarm.health.loop_tick.v1"); n != 0 {
		t.Fatalf("%d loop tick alarms after a full tick", n)
	}
	if h := b.c.Health(ctx); !h.Alive {
		t.Fatalf("health %+v", h)
	}
	// A bench that ticks by loop.tick only raises no loop tick alarms.
	b.c.manual.Store(true)
	b.advance(t, 40)
	if err := b.c.healthLoop(ctx); err != nil {
		t.Fatal(err)
	}
	if n := openAlarms(t, b, "alarm.health.loop_tick.v1"); n != 0 {
		t.Fatalf("%d loop tick alarms on a manual bench", n)
	}
}

// A class 2 task aged beyond the threshold gives alarm.health.urgent_age and the value in the
// health answer; the share of deliveries delivered within 10 minutes is counted over the
// deliveries that had their 10 minutes (OBS-9, blackbox:core/health-indicators).
func TestHealthIndicators(t *testing.T) {
	ctx := context.Background()
	b := newBench(t)
	task := b.hello(t, "1")
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE tasks SET priority_class = 2 WHERE id = $1`, task); err != nil {
		t.Fatal(err)
	}
	b.advance(t, 601)
	if err := b.c.healthLoop(ctx); err != nil {
		t.Fatal(err)
	}
	if n := openAlarms(t, b, "alarm.health.urgent_age.v1"); n != 1 {
		t.Fatalf("%d urgent age alarms", n)
	}
	h := b.c.Health(ctx)
	if h.OldestUrgentAgeS == nil || *h.OldestUrgentAgeS < 601 {
		t.Fatalf("health %+v", h)
	}
	// The ai_notice of the first contact was planned with the task and never delivered.
	if h.Delivered10mRatio == nil || *h.Delivered10mRatio != 0 {
		t.Fatalf("delivered share %v", h.Delivered10mRatio)
	}
	if n := openAlarms(t, b, "alarm.health.delivery_share.v1"); n != 1 {
		t.Fatalf("%d delivery share alarms", n)
	}
	// The task is taken: the alarm resolves.
	b.claim(t, b.agent, "attempt")
	if err := b.c.healthLoop(ctx); err != nil {
		t.Fatal(err)
	}
	if n := openAlarms(t, b, "alarm.health.urgent_age.v1"); n != 0 {
		t.Fatal("the urgent age alarm stayed open")
	}
	if h := b.c.Health(ctx); h.OldestUrgentAgeS != nil {
		t.Fatalf("oldest urgent age %d with no class 1-2 task in submitted", *h.OldestUrgentAgeS)
	}
}

// Below the soft threshold of the store volume a class 3 task is refused with over_capacity
// and alarm.store.free.low fires; below the hard one P1 answers not_ready to claim but lets
// complete pass and health is not alive; above both the alarm resolves (CAP-5).
func TestStoreFreeSpace(t *testing.T) {
	ctx := context.Background()
	b := newBench(t)
	free := uint64(50)
	b.c.cfg.FreeSpace = func() (uint64, uint64, error) { return free, 100, nil }
	if err := b.c.healthLoop(ctx); err != nil {
		t.Fatal(err)
	}
	if h := b.c.Health(ctx); !h.Alive || h.StoreFreeBytes == nil || *h.StoreFreeBytes != 50 || *h.StoreFreeRatio != 0.5 {
		t.Fatalf("health %+v", h)
	}
	free = 10
	if err := b.c.healthLoop(ctx); err != nil {
		t.Fatal(err)
	}
	a := Activity{Kind: "message", Channel: "telegram", UpdateID: "1", Chat: Chat{Kind: "direct", ExternalID: "1001"}, Text: "/hello hi"}
	a.Sender.Identity, a.Sender.Verified = "telegram:1001", true
	res, err := b.c.SubmitActivity(ctx, b.channel, a)
	var tasks int
	b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM tasks`).Scan(&tasks)
	if err != nil || res.Result != "reply" || tasks != 0 {
		t.Fatalf("submit_activity below the soft threshold: %+v %v", res, err)
	}
	if n := openAlarms(t, b, "alarm.store.free.low.v1"); n != 1 {
		t.Fatalf("%d store free alarms", n)
	}
	if err := b.c.P1Ready("claim"); err != nil {
		t.Fatalf("claim below the soft threshold: %v", err)
	}
	free = 50
	b.c.healthLoop(ctx)
	task := b.hello(t, "2")
	w := b.claim(t, b.agent, "attempt")
	if w == nil || w.WorkID == "" || task == "" {
		t.Fatal("no attempt")
	}
	free = 3
	if h := b.c.Health(ctx); h.Alive || h.Reason != "not_ready" {
		t.Fatalf("health below the hard threshold %+v", h)
	}
	wantReason(t, b.c.P1Ready("claim"), "not_ready")
	wantReason(t, b.c.P1Ready("submit_activity"), "not_ready")
	for _, op := range []string{"heartbeat", "complete", "fail", "release"} {
		if err := b.c.P1Ready(op); err != nil {
			t.Fatalf("%s below the hard threshold: %v", op, err)
		}
	}
	free = 60
	if err := b.c.healthLoop(ctx); err != nil {
		t.Fatal(err)
	}
	if n := openAlarms(t, b, "alarm.store.free.low.v1"); n != 0 || b.c.P1Ready("claim") != nil {
		t.Fatal("the store free alarm or the refusal stayed after the space returned")
	}
}

// While the store was unavailable P1 answers not_ready with retry_after_s, and health is not
// alive until every loop ticked again (OBS-8).
func TestStoreDownGate(t *testing.T) {
	ctx := context.Background()
	b := newBench(t)
	b.c.storeDown.Store(true)
	err := b.c.P1Ready("heartbeat")
	wantReason(t, err, "not_ready")
	if p, _ := AsProblem(err); p.RetryAfterS == 0 {
		t.Fatal("no retry_after_s")
	}
	if h := b.c.Health(ctx); h.Alive || h.Reason != "not_ready" {
		t.Fatalf("health %+v", h)
	}
}
