// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/ids"
	"github.com/HunterEwok/botaru/impl/internal/manifest"
	"github.com/HunterEwok/botaru/impl/internal/mcp"
)

// newObsBench is the tool bench whose hello type publishes run.finished.v1 and makes backups.
func newObsBench(t *testing.T) *bench {
	t.Helper()
	return newToolBenchWith(t, nil, func(ms []map[string]any) []map[string]any {
		for _, m := range ms {
			if m["kind"] == "TaskType" && m["metadata"].(map[string]any)["name"] == "hello.greet.v1" {
				s := m["spec"].(map[string]any)
				s["events"] = []any{"run.finished.v1"}
				s["makes_backup"] = true
				s["proposable"] = false
			}
		}
		return ms
	})
}

func (b *bench) obsCount(t *testing.T, series string) int {
	t.Helper()
	var n int
	if err := b.c.st.Pool.QueryRow(context.Background(), `SELECT count(*) FROM obs_points WHERE series = $1`, series).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// obsPoints gives the points of the single group of a query answer.
func obsPoints(t *testing.T, out map[string]any) []map[string]any {
	t.Helper()
	b, _ := json.Marshal(out)
	var r struct {
		Groups []struct {
			Points []map[string]any `json:"points"`
		} `json:"groups"`
	}
	if err := json.Unmarshal(b, &r); err != nil || len(r.Groups) != 1 {
		t.Fatalf("answer %s", b)
	}
	return r.Groups[0].Points
}

// The observe loop writes the core series of ended tasks once, however often it runs; the
// operator reads them through obs.series, and another tenant sees neither the series subject
// nor the points (OBV-3, OBV-5, OBV-6, TEN-4).
func TestObserveCoreSeries(t *testing.T) {
	ctx := context.Background()
	b := newObsBench(t)
	b.helloDone(t, "1", "", "hi")
	b.helloDone(t, "2", "", "hi")
	b.loop(t, "observe")
	n := b.obsCount(t, seriesTaskOutcome)
	if n != 2 || b.obsCount(t, seriesTaskAttempts) != 2 || b.obsCount(t, seriesAttemptTime) != 2 {
		t.Fatalf("points: outcome %d attempts %d duration %d", n, b.obsCount(t, seriesTaskAttempts), b.obsCount(t, seriesAttemptTime))
	}
	b.loop(t, "observe")
	if b.obsCount(t, seriesTaskOutcome) != n {
		t.Fatal("a second run wrote the points again")
	}
	q := ObsQueryRequest{Series: seriesTaskOutcome, Subject: "task_type:hello.greet.v1", Window: "1d", Rollup: "window.share"}
	out, err := b.c.ObsSeries(ctx, b.op, q)
	if err != nil {
		t.Fatal(err)
	}
	if p := obsPoints(t, out); len(p) != 1 || p[0]["value"] != 1.0 || p[0]["samples"] != 2.0 {
		t.Fatalf("share %v", p)
	}
	if out, err := b.c.ObsSeries(ctx, b.op, ObsQueryRequest{Series: seriesTaskOutcome, Subject: "task_type:hello.greet.v1", Window: "1d", Rollup: "hour.max"}); err == nil {
		t.Fatalf("a max of an enum series: %v", out)
	}
	other := Operator{Principal: ids.PrincipalID(), Tenant: "demo2"}
	if out, err := b.c.ObsSeries(ctx, other, q); err == nil {
		t.Fatalf("another tenant read the series: %v", out)
	}
	if _, err := b.c.ObsSeries(ctx, b.op, ObsQueryRequest{Series: seriesTaskOutcome, Subject: "task_type:other.v1", Window: "1d", Rollup: "window.share"}); err == nil {
		t.Fatal("a subject outside the tenant was read")
	}
}

// A job attempt publishes run.finished.v1 only of its type's events, with data by the schema and
// a subject of its catalog; the observe loop turns it into farm.external.* and the backup age,
// and the job reads only its type's observes (OBV-3, OBV-7, IDM-4).
func TestEmitRunFinished(t *testing.T) {
	ctx := context.Background()
	b := newObsBench(t)
	b.hello(t, "1")
	att := b.claim(t, b.agent, "attempt")
	if att == nil {
		t.Fatal("no attempt")
	}
	h := header(t, att)
	data := map[string]any{"run_id": "r1", "subject": "web1-logs", "duration_s": 30, "retries": 0, "exit_code": 0, "outcome": "done"}
	if _, err := b.c.Emit(ctx, b.agent, h, EmitRequest{Type: runFinishedType, Data: data}); err == nil {
		t.Fatal("an agent published an event")
	}
	job := *b.agent
	job.Kind = "job"
	for _, bad := range []EmitRequest{
		{Type: "schedule.skipped", Data: data},
		{Type: runFinishedType, Data: map[string]any{"run_id": "r1", "subject": "db9", "duration_s": 1, "retries": 0, "exit_code": 0, "outcome": "done"}},
		{Type: runFinishedType, Data: map[string]any{"run_id": "r1", "subject": "web1-logs", "duration_s": 1, "retries": 0, "exit_code": 0, "outcome": "done", "note": "free text"}},
	} {
		if _, err := b.c.Emit(ctx, &job, h, bad); err == nil {
			t.Fatalf("published %v", bad)
		}
	}
	r, err := b.c.Emit(ctx, &job, h, EmitRequest{Type: runFinishedType, Data: data})
	if err != nil {
		t.Fatal(err)
	}
	var source, typ string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT source, data->>'task_type' FROM events WHERE id = $1`, r.ID).Scan(&source, &typ); err != nil {
		t.Fatal(err)
	}
	if source != "botaru:core/demo" || typ != "hello.greet.v1" {
		t.Fatalf("event source %s type %s", source, typ)
	}
	b.loop(t, "observe")
	// The type observes nothing, so its job reads no series; a reader of the series reads the point.
	if _, err := b.c.ObsQuery(ctx, &job, h, ObsQueryRequest{Series: seriesExtDuration, Subject: "task_type:hello.greet.v1", Window: "1d", Rollup: "window.mean"}); err == nil {
		t.Fatal("a job read a series outside its type's observes")
	}
	cat, err := b.c.Catalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	out, err := b.c.queryObservations(ctx, b.c.st.Pool, time.Now(), cat, "demo", []string{seriesExtDuration}, "summary",
		obsQuery{Series: seriesExtDuration, Subject: "task_type:hello.greet.v1", Window: "1d", Rollup: "window.mean"})
	if err == nil {
		t.Fatalf("an internal series read under the mark summary: %v", out)
	}
	out, err = b.c.queryObservations(ctx, b.c.st.Pool, time.Now(), cat, "demo", []string{seriesExtDuration}, "internal",
		obsQuery{Series: seriesExtDuration, Subject: "task_type:hello.greet.v1", Window: "1d", Rollup: "window.mean"})
	if err != nil {
		t.Fatal(err)
	}
	if p := obsPoints(t, out); len(p) != 1 || p[0]["value"] != 30.0 {
		t.Fatalf("external duration %v", p)
	}
	if n := b.obsCount(t, seriesBackupAge); n != 1 {
		t.Fatalf("backup age points %d", n)
	}
}

// Points go after the raw period and rollups after the rollup period, only in whole calendar
// months: a month goes once its last day is past the period (OBV-4, RET-10).
func TestObserveRetention(t *testing.T) {
	ctx := context.Background()
	b := newObsBench(t)
	// The clock goes to the 21st day of a month, so the months below are known.
	now := b.storeNow(t)
	day21 := monthStart(now).AddDate(0, 2, 20).Add(12 * time.Hour)
	b.advance(t, int(day21.Sub(now)/time.Second))
	m := monthStart(day21)
	// The last day of the previous month is 21 days past: its month goes. The first day of this
	// month is 20 days past the 14 days, but its month has not ended: it stays, as does 5 days.
	for i, at := range []time.Time{m.Add(-12 * time.Hour), m.Add(time.Hour), day21.AddDate(0, 0, -5)} {
		if _, err := b.c.st.Pool.Exec(ctx, `INSERT INTO obs_points (tenant, series, subject, at, num, source, data_mark, key, created_at)
			VALUES ('demo', $1, 'task_type:hello.greet.v1', $2, 1, 'core', 'internal', $3, now())`, seriesTaskAttempts, at, fmt.Sprint("t:", i)); err != nil {
			t.Fatal(err)
		}
	}
	// A rollup of the last day of the month that ended more than 93 days ago goes; one of the
	// first day of the next month, 113 days old, stays until that month is past.
	old := m.AddDate(0, -3, 0)
	for _, at := range []time.Time{old.AddDate(0, 0, -1), old} {
		if _, err := b.c.st.Pool.Exec(ctx, `INSERT INTO obs_rollups (tenant, series, subject, rollup, period_start, value, samples, data_mark, updated_at)
			VALUES ('demo', $1, 'task_type:hello.greet.v1', 'day.p95', $2, 1, 1, 'internal', now())`, seriesTaskAttempts, at); err != nil {
			t.Fatal(err)
		}
	}
	b.loop(t, "observe")
	if n := b.obsCount(t, seriesTaskAttempts); n != 2 {
		t.Fatalf("points after retention %d", n)
	}
	if n := b.count(t, `SELECT count(*) FROM obs_points WHERE at < $1`, m); n != 0 {
		t.Fatalf("a point of the past month stayed: %d", n)
	}
	if n := b.count(t, `SELECT count(*) FROM obs_rollups WHERE period_start < $1`, old); n != 0 {
		t.Fatalf("old rollups %d", n)
	}
	if n := b.count(t, `SELECT count(*) FROM obs_rollups WHERE period_start = $1`, old); n != 1 {
		t.Fatalf("a rollup of a month not yet past went: %d", n)
	}
}

// A done call of a trusted tool writes the series that map its closed field, on the catalog host
// of the call; a value outside the series bounds and an untrusted or failed call write nothing
// (OBV-1, OBV-2).
func TestToolSeries(t *testing.T) {
	ctx := context.Background()
	b := newToolBenchWith(t, nil, func(ms []map[string]any) []map[string]any {
		return append(ms, map[string]any{
			"apiVersion": "botaru/v1alpha1", "kind": "ObservationSeries", "metadata": map[string]any{"name": "site.ms.v1", "tenant": "demo"},
			"spec": map[string]any{"catalog_kind": "host", "value": map[string]any{"type": "integer", "minimum": 0, "maximum": 30000},
				"data_class": "summary", "source": map[string]any{"kind": "tool", "tool": "checks/latest", "field": "ms_max"}, "rollups": []any{"hour.max"}},
		})
	})
	_, h := b.attemptTools(t, "1", false)
	th := b.toolhost(t, "checks")
	for _, ms := range []int{120, 50000} {
		done := callAsync(b, h, mcp.CallParams{Name: "checks_latest", Arguments: map[string]any{"check": "www-health"}})
		item := b.claimTool(t, th)
		out := map[string]any{"content": []any{}, "structuredContent": map[string]any{"check": "www-health", "ok": 3, "total": 3, "ms_max": ms}}
		if _, err := b.c.Complete(ctx, th, header(t, item), CompleteRequest{Output: out}); err != nil {
			t.Fatal(err)
		}
		if r := <-done; r.err != nil || r.r.IsError {
			t.Fatalf("call %+v %v", r.r, r.err)
		}
	}
	var subject string
	var v float64
	var n int
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT min(subject), min(num), count(*) FROM obs_points WHERE series = 'site.ms.v1'`).Scan(&subject, &v, &n); err != nil {
		t.Fatal(err)
	}
	if n != 1 || subject != "host:web1" || v != 120 {
		t.Fatalf("points %d %s %v", n, subject, v)
	}
	b.loop(t, "observe")
	out, err := b.c.ObsSeries(ctx, b.op, ObsQueryRequest{Series: "site.ms.v1", Subject: "host:web1", Window: "1d", Rollup: "hour.max"})
	if err != nil {
		t.Fatal(err)
	}
	if p := obsPoints(t, out); len(p) != 1 || p[0]["value"] != 120.0 {
		t.Fatalf("hour.max %v", p)
	}
}

// A batch full of rows with one time does not hold the cursor: the next pass reads after the
// last row of the batch, and a task that ends later is observed.
func TestObserveCursorFullBatch(t *testing.T) {
	ctx := context.Background()
	b := newObsBench(t)
	id := b.helloDone(t, "1", "", "hi")
	if _, err := b.c.st.Pool.Exec(ctx, `INSERT INTO tasks SELECT (jsonb_populate_record(null::tasks, to_jsonb(t) ||
		jsonb_build_object('id', t.id || '-' || g, 'idempotency_key', coalesce(t.idempotency_key, 'k') || '-' || g))).*
		FROM tasks t, generate_series(1, $2::int) g WHERE t.id = $1`, id, obsBatch+100); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		b.loop(t, "observe")
	}
	b.helloDone(t, "2", "", "hi")
	for range 3 {
		b.loop(t, "observe")
	}
	if n := b.obsCount(t, seriesTaskOutcome); n != obsBatch+102 {
		t.Fatalf("outcome points %d", n)
	}
}

// A window longer than the raw points kept is refused for a rollup the series does not store.
func TestObsWindowBeyondRaw(t *testing.T) {
	b := newObsBench(t)
	q := ObsQueryRequest{Series: seriesTaskOutcome, Subject: "task_type:hello.greet.v1", Window: "93d", Rollup: "window.share"}
	if out, err := b.c.ObsSeries(context.Background(), b.op, q); err == nil {
		t.Fatalf("a 93d share over 14 days of points: %v", out)
	}
	q = ObsQueryRequest{Series: seriesAttemptTime, Subject: "task_type:hello.greet.v1", Window: "93d", Rollup: "day.p95"}
	if _, err := b.c.ObsSeries(context.Background(), b.op, q); err != nil {
		t.Fatal(err)
	}
}

// A tenant series of source job takes its field from run.finished.v1 on the event's catalog
// subject (OBV-1, OBV-7).
func TestEmitJobSeries(t *testing.T) {
	ctx := context.Background()
	b := newToolBenchWith(t, nil, func(ms []map[string]any) []map[string]any {
		for _, m := range ms {
			if m["kind"] == "TaskType" && m["metadata"].(map[string]any)["name"] == "hello.greet.v1" {
				m["spec"].(map[string]any)["events"] = []any{"run.finished.v1"}
			}
		}
		return append(ms, map[string]any{
			"apiVersion": "botaru/v1alpha1", "kind": "ObservationSeries", "metadata": map[string]any{"name": "logs_qc.duration", "tenant": "demo"},
			"spec": map[string]any{"catalog_kind": "resource", "value": map[string]any{"type": "integer", "minimum": 0, "maximum": 604800},
				"data_class": "internal", "source": map[string]any{"kind": "job", "field": "duration_s"}},
		})
	})
	b.hello(t, "1")
	att := b.claim(t, b.agent, "attempt")
	if att == nil {
		t.Fatal("no attempt")
	}
	job := *b.agent
	job.Kind = "job"
	data := map[string]any{"run_id": "r1", "subject": "web1-logs", "duration_s": 42, "retries": 0, "exit_code": 0, "outcome": "done"}
	if _, err := b.c.Emit(ctx, &job, header(t, att), EmitRequest{Type: runFinishedType, Data: data}); err != nil {
		t.Fatal(err)
	}
	b.loop(t, "observe")
	var subject, source string
	var v float64
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT subject, source, num FROM obs_points WHERE series = 'logs_qc.duration'`).Scan(&subject, &source, &v); err != nil {
		t.Fatal(err)
	}
	if subject != "resource:web1-logs" || source != "job" || v != 42 {
		t.Fatalf("job point %s %s %v", subject, source, v)
	}
}

// Emit takes the data schema of the events catalog for any type a job produces; a type the
// core produces is never published by a job (IDM-4).
func TestJobEventSchema(t *testing.T) {
	b := newObsBench(t)
	for typ, want := range map[string]string{runFinishedType: "run-finished", "alarm.disk.free.low.v1": "alarm", "alarm.worker.stale.v1": "", "schedule.skipped": "", "no.such.v1": ""} {
		got, ok, err := b.c.jobEventSchema(typ)
		if err != nil || got != want || ok != (want != "") {
			t.Fatalf("%s: %q %v %v", typ, got, ok, err)
		}
	}
}

// A job's alarm goes the one alarm path: an episode opens once, its alarmkey must be its name
// and subject, and a resolution closes it (MON-1, OBS-6).
func TestEmitAlarm(t *testing.T) {
	b := newObsBench(t)
	ctx := context.Background()
	alarm := func(state, key string) (string, error) {
		var id string
		err := b.c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
			var err error
			id, err = b.c.emitAlarm(ctx, tx, now, "demo", EmitRequest{Type: "alarm.disk.free.low.v1", Data: map[string]any{
				"alarmkey": key, "state": state, "severity": "warn", "subject": map[string]any{"kind": "resource", "id": "web1-logs"},
				"values": map[string]any{"free_pct": 4}}})
			return err
		})
		return id, err
	}
	if _, err := alarm("firing", "disk.free.low:db1"); err == nil {
		t.Fatal("an alarmkey of another subject was taken")
	}
	if _, err := alarm("resolved", "disk.free.low:web1-logs"); err == nil {
		t.Fatal("a resolution without an episode answered an event")
	}
	first, err := alarm("firing", "disk.free.low:web1-logs")
	if err != nil {
		t.Fatal(err)
	}
	again, err := alarm("firing", "disk.free.low:web1-logs")
	if err != nil || again != first {
		t.Fatalf("a repeat wrote an event: %s %s %v", first, again, err)
	}
	if _, err := alarm("resolved", "disk.free.low:web1-logs"); err != nil {
		t.Fatal(err)
	}
	var states string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT string_agg(data->>'state', ',' ORDER BY time, id) FROM events WHERE type = 'alarm.disk.free.low.v1'`).Scan(&states); err != nil {
		t.Fatal(err)
	}
	if states != "firing,resolved" {
		t.Fatalf("alarm events %s", states)
	}
	// The job's numbers and the catalog's origin reach the episode (MGR-8, MON-5).
	var origin, vals string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT origin, vals::text FROM alarm_episodes WHERE alarm = 'alarm.disk.free.low.v1'`).Scan(&origin, &vals); err != nil {
		t.Fatal(err)
	}
	if origin != "monitor" || vals != `{"free_pct": 4}` {
		t.Fatalf("episode origin %s values %s", origin, vals)
	}
}

// Rollups cover periods older than two days that no rollup has yet, within the raw points
// kept, and a window longer than the rollups kept is refused (OBV-4).
func TestObserveRollupBackfill(t *testing.T) {
	ctx := context.Background()
	b := newToolBenchWith(t, nil, func(ms []map[string]any) []map[string]any {
		return append(ms, map[string]any{
			"apiVersion": "botaru/v1alpha1", "kind": "ObservationSeries", "metadata": map[string]any{"name": "site.ms.v1", "tenant": "demo"},
			"spec": map[string]any{"catalog_kind": "host", "value": map[string]any{"type": "integer", "minimum": 0, "maximum": 30000},
				"data_class": "summary", "source": map[string]any{"kind": "tool", "tool": "checks/latest", "field": "ms_max"}, "rollups": []any{"day.max"},
				"retention": map[string]any{"raw": "14d", "rollup": "30d"}},
		})
	})
	if _, err := b.c.st.Pool.Exec(ctx, `INSERT INTO obs_points (tenant, series, subject, at, num, source, data_mark, key, created_at)
		VALUES ('demo', 'site.ms.v1', 'host:web1', botaru_now() - interval '5 days', 77, 'tool', 'summary', 'old', botaru_now()),
		('demo', 'site.ms.v1', 'host:web1', botaru_now() - interval '20 days', 99, 'tool', 'summary', 'gone', botaru_now())`); err != nil {
		t.Fatal(err)
	}
	b.loop(t, "observe")
	var n int
	var v float64
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*), coalesce(max(value), 0) FROM obs_rollups WHERE series = 'site.ms.v1'`).Scan(&n, &v); err != nil {
		t.Fatal(err)
	}
	if n != 1 || v != 77 {
		t.Fatalf("rollups %d max %v", n, v)
	}
	q := ObsQueryRequest{Series: "site.ms.v1", Subject: "host:web1", Window: "93d", Rollup: "day.max"}
	if out, err := b.c.ObsSeries(ctx, b.op, q); err == nil {
		t.Fatalf("a 93d window over 30 days of rollups: %v", out)
	}
}

// An aggregator reads only the series of its sources, and only its own tenant's points: a
// canary point of another tenant in the same series and subject is not in the answer (AGG-3,
// AGG-5).
func TestAggregatorReads(t *testing.T) {
	ctx := context.Background()
	b := newBench(t)
	cat := &Catalog{Tenants: map[string]*TenantView{}, Roles: map[string]manifest.Role{"agg": {Profile: "aggregator",
		Aggregate: &manifest.RoleAggregate{}}}, TaskTypes: map[string]manifest.TaskType{"agg.v1": {Purpose: "aggregate", Observes: []string{seriesTaskOutcome, seriesTaskAttempts}}}}
	cat.Roles["agg"].Aggregate.Sources.Series = []string{seriesTaskAttempts}
	if got := b.c.readableSeries(cat, "demo", "agg", "agg.v1"); len(got) != 1 || got[0] != seriesTaskAttempts {
		t.Fatalf("readable %v", got)
	}
	live, err := b.c.Catalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.c.st.Pool.Exec(ctx, `INSERT INTO obs_points (tenant, series, subject, at, num, source, data_mark, key, created_at)
		VALUES ('other', $1, 'task_type:hello.greet.v1', botaru_now(), 42, 'core', 'internal', 'canary', botaru_now())`, seriesTaskAttempts); err != nil {
		t.Fatal(err)
	}
	err = b.c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		out, err := b.c.queryObservations(ctx, tx, now, live, "demo", []string{seriesTaskAttempts}, "internal",
			obsQuery{Series: seriesTaskAttempts, Subject: "task_type:hello.greet.v1", Window: "1d", Rollup: "window.mean"})
		if err != nil {
			return err
		}
		for _, g := range asList(out["groups"]) {
			if len(asList(mmapAny(g)["points"])) != 0 {
				t.Errorf("another tenant's point is visible: %v", out)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
