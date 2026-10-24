// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func (b *bench) alarmTx(t *testing.T, fn func(tx pgx.Tx, now time.Time) error) {
	t.Helper()
	if err := b.c.tx(context.Background(), fn); err != nil {
		t.Fatal(err)
	}
}

func countNotices(t *testing.T, b *bench) int {
	t.Helper()
	var n int
	if err := b.c.st.Pool.QueryRow(context.Background(), `SELECT count(*) FROM deliveries WHERE kind = 'alarm_notice'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// 60 identical firing in an hour give one episode with repeat_count 59, one event and one
// alarm_notice to the admin; resolved gives a second notice (OBS-6, MON-13,
// blackbox:core/alarm-repeat-count, blackbox:core/alarm-notice-no-monitor).
func TestAlarmEpisode(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newBench(t)
	b.withAdmins(t)
	a := Alarm{Tenant: "demo", Name: "worker.stale", Subject: AlarmSubject{Kind: "worker", ID: "wk_1"}, Severity: "warn"}
	for range 60 {
		b.alarmTx(t, func(tx pgx.Tx, now time.Time) error { return b.c.fireAlarm(ctx, tx, now, a) })
		if _, err := b.c.TestClockAdvance(ctx, 60); err != nil {
			t.Fatal(err)
		}
	}
	var episodes, repeats int
	b.c.st.Pool.QueryRow(ctx, `SELECT count(*), max(repeat_count) FROM alarm_episodes WHERE alarmkey = 'worker.stale:wk_1'`).Scan(&episodes, &repeats)
	if episodes != 1 || repeats != 59 || countEvents(t, b, "alarm.worker.stale.v1", "") != 1 || countNotices(t, b) != 1 {
		t.Fatalf("episodes %d, repeats %d, events %d, notices %d", episodes, repeats, countEvents(t, b, "alarm.worker.stale.v1", ""), countNotices(t, b))
	}
	b.alarmTx(t, func(tx pgx.Tx, now time.Time) error { return b.c.resolveAlarm(ctx, tx, now, a) })
	if countEvents(t, b, "alarm.worker.stale.v1", "") != 2 || countNotices(t, b) != 2 {
		t.Fatal("resolved gave no event or notice")
	}
	var body map[string]any
	b.c.st.Pool.QueryRow(ctx, `SELECT body FROM deliveries WHERE kind = 'alarm_notice' ORDER BY seq DESC LIMIT 1`).Scan(&body)
	params, _ := body["params"].(map[string]any)
	if body["text_key"] != "notice.alarm" || params["state"] != "resolved" || params["seq"] != float64(2) || params["alarm"] != "worker.stale" {
		t.Fatalf("notice body %v", body)
	}
	// A new firing opens a new episode.
	b.alarmTx(t, func(tx pgx.Tx, now time.Time) error { return b.c.fireAlarm(ctx, tx, now, a) })
	if countNotices(t, b) != 3 {
		t.Fatal("a firing after resolved opened no episode")
	}
	// Text outside the schema is refused (OBS-6).
	bad := a
	bad.Values = map[string]any{"note": "Disk Is Full"}
	err := b.c.tx(ctx, func(tx pgx.Tx, now time.Time) error { return b.c.fireAlarm(ctx, tx, now, bad) })
	wantReason(t, err, "input_schema_invalid")
}

// A page alarm repeats its notice every 30 minutes while open: 70 minutes give two reminders,
// and an incident.ack on the incident that adopted the episode stops them (MON-13). A hold does not withhold an alarm notice (HLD-3, MON-1).
func TestAlarmPageRepeats(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newBench(t)
	b.withAdmins(t)
	if _, err := b.c.HoldSet(ctx, OperatorActor(b.op), HoldSetRequest{
		Scope: HoldScope{Kind: "tenant", Tenant: "demo"}, Effects: []string{"no_delivery"}, Reason: "manual"}); err != nil {
		t.Fatal(err)
	}
	a := Alarm{Tenant: "demo", Name: "health.loop_tick", Subject: AlarmSubject{Kind: "loop", ID: "reaper"}}
	b.alarmTx(t, func(tx pgx.Tx, now time.Time) error { return b.c.fireAlarm(ctx, tx, now, a) })
	for range 7 {
		if _, err := b.c.TestClockAdvance(ctx, 600); err != nil {
			t.Fatal(err)
		}
		if err := b.c.alarmLoop(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if n := countNotices(t, b); n != 3 {
		t.Fatalf("%d notices in 70 minutes, want the first and two reminders", n)
	}
	// A person took up the incident that adopted the episode: the reminders stop.
	b.exec(t, `INSERT INTO incidents (id, tenant, class, subject_kind, alarmkey, episode, episodes, alarmkeys, state, record, since, implicated_at, opened_at, updated_at)
		SELECT 'inc_page', tenant, 'loop_stuck', 'loop', alarmkey, id, ARRAY[id], ARRAY[alarmkey], 'open', '{"humans_took": true}', now(), now(), now(), now()
		FROM alarm_episodes`)
	for range 7 {
		if _, err := b.c.TestClockAdvance(ctx, 600); err != nil {
			t.Fatal(err)
		}
		if err := b.c.alarmLoop(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if n := countNotices(t, b); n != 3 {
		t.Fatalf("%d notices after the incident.ack, want no more reminders", n)
	}
	var sev string
	b.c.st.Pool.QueryRow(ctx, `SELECT severity FROM alarm_episodes`).Scan(&sev)
	if sev != "page" {
		t.Fatalf("severity %s, want page from the events catalog", sev)
	}
	// The channel gets the notice although the tenant holds its deliveries.
	if w := b.claim(t, b.channel, "delivery"); w == nil {
		t.Fatal("no delivery")
	}
	var withheldNotices int
	b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM deliveries WHERE kind = 'alarm_notice' AND state = 'withheld'`).Scan(&withheldNotices)
	if withheldNotices != 0 {
		t.Fatal("a hold withheld an alarm notice")
	}
}

// A worker silent for longer than two long-poll periods gives worker.stale and the alarm; its
// next call resolves it (OBS-4, blackbox:core/worker-stale).
func TestWorkerStale(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newBench(t)
	if _, err := b.c.TestClockAdvance(ctx, 121); err != nil {
		t.Fatal(err)
	}
	if err := b.c.liveness(ctx); err != nil {
		t.Fatal(err)
	}
	if n := countEvents(t, b, "worker.stale.v1", ""); n != 2 {
		t.Fatalf("%d worker.stale events, want the agent and the channel", n)
	}
	if n := countEvents(t, b, "alarm.worker.stale.v1", ""); n != 2 {
		t.Fatalf("%d alarms", n)
	}
	if err := b.c.liveness(ctx); err != nil {
		t.Fatal(err)
	}
	if n := countEvents(t, b, "worker.stale.v1", ""); n != 2 {
		t.Fatal("a second pass wrote worker.stale again")
	}
	b.claim(t, b.agent, "attempt")
	if err := b.c.liveness(ctx); err != nil {
		t.Fatal(err)
	}
	var open int
	b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM alarm_episodes WHERE state = 'firing'`).Scan(&open)
	if open != 1 || countEvents(t, b, "alarm.worker.stale.v1", "") != 3 {
		t.Fatalf("after the agent's call: %d open, want only the channel's", open)
	}
}

// A task nobody took before start_by stays submitted and raises alarm.task.unclaimed once; the
// alarm resolves when the task is taken (DSP-8).
func TestTaskUnclaimed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newBench(t)
	task := b.hello(t, "1")
	b.c.st.Pool.Exec(ctx, `UPDATE contacts SET ai_notice_at = now()`)
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE tasks SET start_by_at = botaru_now() + interval '10 minutes' WHERE id = $1`, task); err != nil {
		t.Fatal(err)
	}
	if err := b.c.deadlines(ctx); err != nil {
		t.Fatal(err)
	}
	if n := countEvents(t, b, "alarm.task.unclaimed.v1", ""); n != 0 {
		t.Fatal("an alarm before start_by")
	}
	if _, err := b.c.TestClockAdvance(ctx, 601); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := b.c.deadlines(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if n := countEvents(t, b, "alarm.task.unclaimed.v1", ""); n != 1 {
		t.Fatalf("%d alarms after start_by", n)
	}
	if w := b.claim(t, b.agent, "attempt"); w == nil || w.WorkID != task {
		t.Fatal("the task was not handed out")
	}
	if err := b.c.deadlines(ctx); err != nil {
		t.Fatal(err)
	}
	var state string
	b.c.st.Pool.QueryRow(ctx, `SELECT state FROM alarm_episodes`).Scan(&state)
	if state != "resolved" {
		t.Fatalf("episode %s after claim", state)
	}
}
