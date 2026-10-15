// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/HunterEwok/botaru/impl/internal/ids"
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

// Points go after the raw period and rollups after the rollup period, in whole days (OBV-4).
func TestObserveRetention(t *testing.T) {
	ctx := context.Background()
	b := newObsBench(t)
	now := time.Now()
	for _, days := range []int{20, 5} {
		if _, err := b.c.st.Pool.Exec(ctx, `INSERT INTO obs_points (tenant, series, subject, at, num, source, data_mark, key, created_at)
			VALUES ('demo', $1, 'task_type:hello.greet.v1', $2, 1, 'core', 'internal', $3, now())`, seriesTaskAttempts, now.AddDate(0, 0, -days), "t:"+time.Duration(days).String()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := b.c.st.Pool.Exec(ctx, `INSERT INTO obs_rollups (tenant, series, subject, rollup, period_start, value, samples, data_mark, updated_at)
		VALUES ('demo', $1, 'task_type:hello.greet.v1', 'day.p95', $2, 1, 1, 'internal', now())`, seriesTaskAttempts, now.AddDate(0, 0, -100).Truncate(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	b.loop(t, "observe")
	if n := b.obsCount(t, seriesTaskAttempts); n != 1 {
		t.Fatalf("points after retention %d", n)
	}
	var n int
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM obs_rollups WHERE period_start < now() - interval '93 days'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("old rollups %d %v", n, err)
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
