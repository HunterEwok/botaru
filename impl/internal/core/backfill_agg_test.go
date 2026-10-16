// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// aggEdit adds the aggregator role agg, which claims the aggregate type agg.rollup.v1 and reads
// the series farm.task.attempts, and lets system start the type.
func aggEdit(ms []map[string]any) []map[string]any {
	return append(ms, map[string]any{
		"apiVersion": "botaru/v1alpha1", "kind": "Role", "metadata": map[string]any{"name": "agg"},
		"spec": map[string]any{"profile": "aggregator", "worker_kind": "agent", "claims": []any{"agg.rollup.v1"},
			"models": map[string]any{"base": "base-std", "analysis": "base-std"}, "memory": "none",
			"aggregate": map[string]any{"sources": map[string]any{"series": []any{"farm.task.attempts"}}}},
	}, map[string]any{
		"apiVersion": "botaru/v1alpha1", "kind": "TaskType", "metadata": map[string]any{"name": "agg.rollup.v1"},
		"spec": map[string]any{"title": "Roll up the results", "worker_kind": "agent", "purpose": "aggregate", "tier": 1, "data_class": "internal",
			"instruction": "Propose or answer nothing_found.", "params_schema": map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{}},
			"timeouts": map[string]any{"start_by": "1h", "lease": "60s", "heartbeat": "20s", "attempt": "5m", "deadline": "2h"},
			"retry":    map[string]any{"max_attempts": 1, "max_runs": 2}, "tree_budget_eur": 0.1, "max_children": 0, "proposable": false},
	}, map[string]any{
		"apiVersion": "botaru/v1alpha1", "kind": "Group", "metadata": map[string]any{"name": "system-ops", "tenant": "demo"},
		"spec": map[string]any{"caps": []any{"task:agg.rollup.v1"}, "data": []any{"summary", "internal"}},
	})
}

// The built-in tools give an aggregator only the rollups of observations_query: no inbox, no
// message, memory, corpus or plugin tool reaches an aggregate task, and the profile refuses
// tools and messages of its own at apply (AGG-2).
func TestBackfillAggregatorInputsAggregated(t *testing.T) {
	ctx := context.Background()
	b := newToolBenchWith(t, nil, aggEdit)
	cat, err := b.c.Catalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	g := &toolsGrant{Tenant: "demo", Type: "agg.rollup.v1", Role: "agg", Purpose: "aggregate", Mark: "internal", MaxTier: 1, Initiator: "system"}
	tools, err := b.c.toolCatalog(cat, g)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, x := range tools {
		names = append(names, x.Name)
	}
	// AGG-2: the only input tool of the aggregator is the closed rollup query.
	if !slices.Equal(names, []string{obsToolName}) {
		t.Fatalf("aggregator catalog %v, want only %s", names, obsToolName)
	}
	// AGG-2: its query takes a rollup of the closed list only; a raw read of points is outside
	// the schema.
	schema := tools[0].Schema
	if req := asList(schema["required"]); !slices.Contains(req, any("rollup")) || schema["additionalProperties"] != false {
		t.Fatalf("observations_query schema %v", schema)
	}
	rollups := asList(mmapAny(mmapAny(schema["properties"])["rollup"])["enum"])
	if len(rollups) == 0 || !slices.Equal(rollups, func() []any {
		var out []any
		for _, id := range slices.Sorted(maps.Keys(b.c.rollups())) {
			out = append(out, id)
		}
		return out
	}()) {
		t.Fatalf("rollup enum %v", rollups)
	}
	// AGG-2: the profile aggregator refuses tools and messages of its own at apply.
	c := lintCore(t)
	got := lintCodes(t, c, `apiVersion: botaru/v1alpha1
kind: Role
metadata: {name: agg}
spec: {profile: aggregator, worker_kind: agent, claims: [], models: {base: base-std}, memory: none, tools: [logs/tail]}
`)
	if !slices.Contains(got, "role_profile_forbidden") {
		t.Fatalf("an aggregator with tools: %v", got)
	}
}

// An aggregator's proposal carries the mark and the untrusted flag with its sources from the task
// record that the inputs it read raised: the test writes the record as reading a raw_logs input
// with an untrusted source would, and the proposal follows it (AGG-4). Raising the record while
// reading is the job of the read tools and is not exercised here.
func TestBackfillAggregatorMarks(t *testing.T) {
	ctx := context.Background()
	b := newToolBenchWith(t, nil, aggEdit)
	w := b.enroll(t, EnrollRequest{Tenant: "demo", Role: "agg", Kind: "agent"})
	b.helloDone(t, "1", "", "hi")
	b.loop(t, "observe")
	run := func(key string, mark string, untrusted bool) string {
		t.Helper()
		var task string
		err := b.c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
			cat, err := b.c.Catalog(ctx)
			if err != nil {
				return err
			}
			res, err := b.c.enqueue(ctx, tx, now, cat, enqueueInput{Tenant: "demo", Type: "agg.rollup.v1", Body: map[string]any{"type": "agg.rollup.v1", "goal": "roll up"},
				IdempotencyKey: "system:schedule:agg:" + key, Initiator: "system", EnqueuedBy: "schedule:agg", Source: "schedule", SourceID: "agg"})
			if err != nil {
				return err
			}
			if res.Rejected != nil {
				return res.Rejected
			}
			task = res.TaskID
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		// The inputs read so far: the highest mark and the OR of the flags with the source.
		upd := `UPDATE tasks SET record = record || jsonb_build_object('data_mark', $2::text) WHERE id = $1`
		if _, err := b.c.st.Pool.Exec(ctx, upd, task, mark); err != nil {
			t.Fatal(err)
		}
		if untrusted {
			if _, err := b.c.st.Pool.Exec(ctx, `UPDATE tasks SET record = record || '{"untrusted_seen": true, "untrusted_source": "logs/tail"}' WHERE id = $1`, task); err != nil {
				t.Fatal(err)
			}
		}
		att := claimFor(t, b, w, task)
		if _, err := b.c.Complete(ctx, w, header(t, att), CompleteRequest{Result: map[string]any{"proposals": []any{noneBody("slow_greeting_"+key, 1)}}}); err != nil {
			t.Fatal(err)
		}
		return task
	}
	t1 := run("a", "internal", false)
	b.asOtherAuthor(t)
	t2 := run("b", "raw_logs", true)
	byTask := map[string]map[string]any{}
	for _, r := range proposals(t, b) {
		byTask[asString(mmapAny(r["origin"])["task"])] = r
	}
	if len(byTask) != 2 {
		t.Fatalf("proposals %v, rejections %v", byTask, rejections(t, b))
	}
	// AGG-4: the proposal's mark is the highest of the inputs, untrusted_seen their OR.
	p1, p2 := byTask[t1], byTask[t2]
	if p1["data_mark"] != "internal" || mmapAny(p1["untrusted_seen"])["value"] != false {
		t.Fatalf("trusted aggregate: mark %v untrusted %v", p1["data_mark"], p1["untrusted_seen"])
	}
	u2 := mmapAny(p2["untrusted_seen"])
	if p2["data_mark"] != "raw_logs" || u2["value"] != true || !slices.Contains(asList(u2["sources"]), any("logs/tail")) {
		t.Fatalf("aggregate over an untrusted input: mark %v untrusted %v", p2["data_mark"], p2["untrusted_seen"])
	}
}
