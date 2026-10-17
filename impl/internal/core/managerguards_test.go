// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"testing"
)

// Tests of the farm guards the farm manager's daily review reads (PRP-26).

// From v0.3 the farm manager's daily review reads the two farm guards of the tenant's last day,
// as numbers with their samples (PRP-26).
func TestManagerReviewFarmGuards(t *testing.T) {
	ctx := context.Background()
	b := newManagerWorkBench(t)
	b.exec(t, `INSERT INTO obs_points (tenant, series, subject, at, enum, source, data_mark, key, created_at)
		SELECT 'demo', 'farm.task.outcome', 'task_type:hello.greet.v1', botaru_now() - interval '1 day', CASE WHEN g % 4 = 0 THEN 'failed' ELSE 'done' END, 'core', 'internal', 'o' || g, botaru_now()
		FROM generate_series(1, 8) g`)
	b.exec(t, `INSERT INTO obs_points (tenant, series, subject, at, num, source, data_mark, key, created_at)
		SELECT 'demo', 'farm.queue.wait', 'task_type:hello.greet.v1', botaru_now() - interval '1 day', g, 'core', 'internal', 'w' || g, botaru_now() FROM generate_series(1, 20) g`)
	b.loop(t, "identity")
	var input map[string]any
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT body->'params'->'input' FROM tasks WHERE type = $1 AND tenant = 'demo'`, reviewType).Scan(&input); err != nil {
		t.Fatal(err)
	}
	got := map[string]map[string]any{}
	for _, g := range asList(input["farm_guards"]) {
		gm := mmapAny(g)
		got[asString(gm["series"])] = gm
	}
	if o := got["farm.task.outcome"]; o == nil || o["value"] != 0.75 || o["samples"] != float64(8) {
		t.Fatalf("task outcome %v", input["farm_guards"])
	}
	if w := got["farm.queue.wait"]; w == nil || w["value"] != float64(19) || w["subject"] != "tenant:demo" {
		t.Fatalf("queue wait %v", input["farm_guards"])
	}
}
