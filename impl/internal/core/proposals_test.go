// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/mcp"
)

// newInsightBench is the tool bench whose hello role also runs the analysis hello.insight.v1,
// which system may start; the hello tasks give core series points to cite.
func newInsightBench(t *testing.T, insight map[string]any) *bench {
	t.Helper()
	return newToolBenchWith(t, nil, insightEdit(insight))
}

// insightEdit adds the analysis hello.insight.v1 to the hello role of a tool bench.
func insightEdit(insight map[string]any) func(ms []map[string]any) []map[string]any {
	return func(ms []map[string]any) []map[string]any {
		for _, m := range ms {
			if m["kind"] == "Role" && m["metadata"].(map[string]any)["name"] == "hello" {
				s := m["spec"].(map[string]any)
				s["claims"] = append(s["claims"].([]any), "hello.insight.v1")
				s["models"].(map[string]any)["analysis"] = "base-std"
				ins := map[string]any{"types": []any{"hello.insight.v1"}, "observes": []any{"farm.task.attempts", "farm.task.outcome"},
					"tools": []any{"checks/latest"}, "max_data": "internal", "merge_window": "1h"}
				for k, v := range insight {
					ins[k] = v
				}
				s["insight"] = ins
			}
		}
		return append(ms, map[string]any{
			"apiVersion": "botaru/v1alpha1", "kind": "TaskType", "metadata": map[string]any{"name": "hello.insight.v1"},
			"spec": map[string]any{"title": "Look at the results", "worker_kind": "agent", "purpose": "insight", "tier": 1, "data_class": "internal",
				"instruction": "Propose or answer nothing_found.", "params_schema": map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{}},
				"timeouts": map[string]any{"start_by": "1h", "lease": "60s", "heartbeat": "20s", "attempt": "5m", "deadline": "2h"},
				"retry":    map[string]any{"max_attempts": 1, "max_runs": 2}, "tree_budget_eur": 0.1, "max_children": 0, "proposable": false},
		}, map[string]any{
			"apiVersion": "botaru/v1alpha1", "kind": "Group", "metadata": map[string]any{"name": "system-ops", "tenant": "demo"},
			"spec": map[string]any{"caps": []any{"task:hello.insight.v1"}, "data": []any{"summary", "internal"}},
		})
	}
}

// analysis enqueues the analysis as a schedule would, claims its attempt and hands in result;
// untrusted marks the attempt as one that read untrusted text.
func (b *bench) analysis(t *testing.T, key string, untrusted bool, result map[string]any) (string, error) {
	t.Helper()
	ctx := context.Background()
	task, att := b.analysisAttempt(t, key)
	if untrusted {
		if _, err := b.c.st.Pool.Exec(ctx, `UPDATE tasks SET record = record || '{"untrusted_seen": true, "untrusted_source": "logs/tail"}' WHERE id = $1`, task); err != nil {
			t.Fatal(err)
		}
	}
	_, err := b.c.Complete(ctx, b.agent, header(t, att), CompleteRequest{Result: result})
	return task, err
}

// analysisAttempt enqueues the analysis as a schedule would and claims its attempt.
func (b *bench) analysisAttempt(t *testing.T, key string) (string, *WorkItem) {
	t.Helper()
	ctx := context.Background()
	var task string
	err := b.c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		cat, err := b.c.Catalog(ctx)
		if err != nil {
			return err
		}
		res, err := b.c.enqueue(ctx, tx, now, cat, enqueueInput{Tenant: "demo", Type: "hello.insight.v1", Body: map[string]any{"type": "hello.insight.v1", "goal": "look"},
			IdempotencyKey: "system:schedule:insight:" + key, Initiator: "system", EnqueuedBy: "schedule:insight", Source: "schedule", SourceID: "insight"})
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
	att := b.claim(t, b.agent, "attempt")
	if att == nil {
		t.Fatal("no analysis attempt")
	}
	return task, att
}

func evidence(value any) []any {
	return []any{map[string]any{"kind": "observation", "series": "farm.task.attempts", "subject": "task_type:hello.greet.v1",
		"window": "1d", "rollup": "window.mean", "value": value}}
}

func noneBody(code string, value any) map[string]any {
	return map[string]any{"category": "reliability", "problem": map[string]any{"code": code, "subjects": []any{"task_type:hello.greet.v1"}, "severity": "low"},
		"evidence": evidence(value), "change": map[string]any{"kind": "none"}}
}

func changeBody(code string, change map[string]any) map[string]any {
	b := noneBody(code, 1)
	b["change"] = change
	b["benefit"] = map[string]any{"metric": "farm.task.outcome", "subject": "task_type:hello.greet.v1", "direction": "up", "expected_change_pp": 5, "confidence": "low"}
	b["cost"] = map[string]any{"one_time_eur": 0, "monthly_eur": 0, "effort": "minutes"}
	b["risk"] = map[string]any{"level": "low"}
	return b
}

func limitsChange(v int) map[string]any {
	return map[string]any{"kind": "manifest_change", "manifest_change": map[string]any{"ops": []any{
		map[string]any{"op": "set", "target": map[string]any{"kind": "Tenant", "name": "demo"}, "field": "spec.limits.max_queued", "value": v}}}}
}

func proposals(t *testing.T, b *bench) []map[string]any {
	t.Helper()
	rows, err := b.c.st.Pool.Query(context.Background(), `SELECT record FROM proposals ORDER BY created_at`)
	if err != nil {
		t.Fatal(err)
	}
	recs, err := pgx.CollectRows(rows, pgx.RowTo[map[string]any])
	if err != nil {
		t.Fatal(err)
	}
	return recs
}

func rejections(t *testing.T, b *bench) []string {
	t.Helper()
	rows, err := b.c.st.Pool.Query(context.Background(), `SELECT data->>'reason' FROM events WHERE type = $1 ORDER BY time, id`, proposalRejectedType)
	if err != nil {
		t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// An analysis result creates screened proposals with the author and the attempt's marks, by the
// record schema; a duplicate counts once more instead of a second record; the core's reread
// value goes with the record (INS-3, PRP-1, PRP-2, PRP-5, PRP-14).
func TestProposalCreated(t *testing.T) {
	b := newInsightBench(t, nil)
	b.helloDone(t, "1", "", "hi")
	b.loop(t, "observe")
	task, err := b.analysis(t, "1", false, map[string]any{"proposals": []any{noneBody("slow_greeting", 1)}})
	if err != nil {
		t.Fatal(err)
	}
	recs := proposals(t, b)
	if len(recs) != 1 {
		t.Fatalf("proposals %d, rejections %v", len(recs), rejections(t, b))
	}
	r := recs[0]
	if err := b.c.spec.Validate("proposal-record", r); err != nil {
		t.Fatalf("record: %v", err)
	}
	origin := r["origin"].(map[string]any)
	if r["state"] != "screened" || origin["task"] != task || origin["role"] != "hello" || origin["worker"] != b.agent.ID || r["data_mark"] == "" {
		t.Fatalf("record %v", r)
	}
	if _, err := b.analysis(t, "2", false, map[string]any{"proposals": []any{noneBody("slow_greeting", 1.05)}}); err != nil {
		t.Fatal(err)
	}
	recs = proposals(t, b)
	if len(recs) != 1 || recs[0]["seen_count"] != 2.0 {
		t.Fatalf("duplicate: %d records, seen %v", len(recs), recs[0]["seen_count"])
	}
	if _, err := b.analysis(t, "3", false, map[string]any{"code": "nothing_found"}); err != nil {
		t.Fatal(err)
	}
}

// A body that fails a check gives proposal.rejected.v1 with the reason and no record; a result
// outside insight-result is refused as a whole (PRP-1, PRP-2, PRP-3, PRP-21, INS-1).
func TestProposalRejected(t *testing.T) {
	b := newInsightBench(t, nil)
	b.helloDone(t, "1", "", "hi")
	b.loop(t, "observe")
	noProblem := noneBody("x", 1)
	delete(noProblem, "problem")
	security := changeBody("open_port", limitsChange(10))
	security["category"] = "security"
	badMetric := changeBody("slow", limitsChange(10))
	badMetric["benefit"].(map[string]any)["metric"] = "site.unknown.v1"
	raise := changeBody("too_low", limitsChange(1000000))
	shared := changeBody("role", map[string]any{"kind": "manifest_change", "manifest_change": map[string]any{"ops": []any{
		map[string]any{"op": "set", "target": map[string]any{"kind": "Role", "name": "hello"}, "field": "spec.memory", "value": "none"}}}})
	if _, err := b.analysis(t, "1", false, map[string]any{"proposals": []any{noneBody("slow", 50), noProblem, security}}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.analysis(t, "2", false, map[string]any{"proposals": []any{badMetric, raise, shared}}); err != nil {
		t.Fatal(err)
	}
	want := []string{"evidence_mismatch", "schema_invalid", "forbidden_change", "metric_unknown", "forbidden_change", "schema_invalid"}
	got := rejections(t, b)
	if len(proposals(t, b)) != 0 || len(got) != len(want) {
		t.Fatalf("rejections %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("rejections %v, want %v", got, want)
		}
	}
	if _, err := b.analysis(t, "3", false, map[string]any{"proposals": []any{noneBody("a", 1)}, "note": "free text"}); err == nil {
		t.Fatal("a result outside insight-result was taken")
	}
}

// A tenant limit goes only down; after untrusted text only such a safe change or kind none is
// taken, and a service call is not (PRP-3, PRP-7, PRP-11).
func TestProposalChanges(t *testing.T) {
	b := newInsightBench(t, map[string]any{"max_open_proposals": 5})
	b.helloDone(t, "1", "", "hi")
	b.loop(t, "observe")
	service := changeBody("restart", map[string]any{"kind": "action_request", "action_request": map[string]any{"tool": "service/control",
		"args": map[string]any{"service": "nginx", "verb": "restart"}}})
	if _, err := b.analysis(t, "1", true, map[string]any{"proposals": []any{changeBody("queue_long", limitsChange(10)), service}}); err != nil {
		t.Fatal(err)
	}
	recs := proposals(t, b)
	if len(recs) != 1 {
		t.Fatalf("records %d, rejections %v", len(recs), rejections(t, b))
	}
	us := recs[0]["untrusted_seen"].(map[string]any)
	if us["value"] != true || len(us["sources"].([]any)) != 1 {
		t.Fatalf("untrusted %v", us)
	}
	if got := rejections(t, b); len(got) != 1 || got[0] != "untrusted_direction" {
		t.Fatalf("rejections %v", got)
	}
}

// The open limit of the role refuses a new proposal with an event (INS-4, PRP-5).
func TestProposalRateLimited(t *testing.T) {
	b := newInsightBench(t, map[string]any{"max_open_proposals": 1})
	b.helloDone(t, "1", "", "hi")
	b.loop(t, "observe")
	if _, err := b.analysis(t, "1", false, map[string]any{"proposals": []any{noneBody("one", 1), noneBody("two", 1)}}); err != nil {
		t.Fatal(err)
	}
	if n := len(proposals(t, b)); n != 1 {
		t.Fatalf("records %d", n)
	}
	if got := rejections(t, b); len(got) != 1 || got[0] != "proposal_rate_limited" {
		t.Fatalf("rejections %v", got)
	}
}

// Analysis starts only from a schedule or a route as system, and a second scheduled analysis
// of the role inside its merge window is skipped (INS-1, INS-4).
func TestAnalysisStart(t *testing.T) {
	ctx := context.Background()
	b := newInsightBench(t, nil)
	err := b.c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		cat, err := b.c.Catalog(ctx)
		if err != nil {
			return err
		}
		if _, err := b.c.enqueue(ctx, tx, now, cat, enqueueInput{Tenant: "demo", Type: "hello.insight.v1", Body: map[string]any{"type": "hello.insight.v1", "goal": "look"},
			IdempotencyKey: "k1", Initiator: b.alice, Source: "interactive"}); err == nil {
			t.Error("a person started an analysis")
		}
		over, err := insightOverLimit(ctx, tx, now, cat, "demo", "hello.insight.v1")
		if err != nil || over {
			t.Errorf("the first analysis is over the limit: %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.analysis(t, "1", false, map[string]any{"code": "nothing_found"}); err != nil {
		t.Fatal(err)
	}
	err = b.c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		cat, err := b.c.Catalog(ctx)
		if err != nil {
			return err
		}
		over, err := insightOverLimit(ctx, tx, now, cat, "demo", "hello.insight.v1")
		if err != nil || !over {
			t.Errorf("a second analysis inside the merge window: %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Doctor names the reasons the improvement cycle is off; here no person may decide (INS-6).
func TestLoopDisabled(t *testing.T) {
	b := newInsightBench(t, nil)
	doc, err := b.c.Doctor(context.Background(), b.op, TenantRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for _, ch := range doc.Checks {
		if ch.ID == "insights.loop_disabled" && ch.Tenant == "demo" {
			if ch.OK || ch.Value != "no_decider" {
				t.Fatalf("loop %+v", ch)
			}
			return
		}
	}
	t.Fatal("no insights.loop_disabled check")
}

// Analysis calls only the tools its catalog offers, even when the role's main work holds more
// (INS-2).
func TestAnalysisToolCalls(t *testing.T) {
	ctx := context.Background()
	b := newInsightBench(t, map[string]any{"tools": []any{}})
	_, att := b.analysisAttempt(t, "1")
	h, _ := att.Payload["handles"].(map[string]any)["tools"].(string)
	if got := toolNames(t, b, h); !slices.Equal(got, []string{obsToolName}) {
		t.Fatalf("analysis catalog %v", got)
	}
	r, err := b.c.P3ToolsCall(ctx, h, mcp.CallParams{Name: "checks_latest", Arguments: map[string]any{"check": "www-health"}})
	if err != nil || !r.IsError || callText(r) != noPermission {
		t.Fatalf("checks_latest from an analysis: %+v %v", r, err)
	}
}

// A schedule proposal only shifts the window: the days stay, and the slots of a day neither
// grow nor fall below half (PRP-3, PRP-11).
func TestCronShift(t *testing.T) {
	for _, c := range []struct {
		cur, next     string
		before, after int
		ok            bool
	}{
		{"0 3 * * *", "30 4 * * *", 1, 1, true},
		{"0 3 * * 1", "0 5 * * 1", 1, 1, true},
		{"0 */2 * * *", "0 */4 * * *", 12, 6, true},
		{"0 3 * * *", "0 1-7 5-8 1 *", 0, 0, false},
		{"0 3 * * 1", "0 3 5,12,19,26 1 *", 0, 0, false},
		{"0 3 * * 1", "0 3 * * 1,3", 0, 0, false},
	} {
		before, after, ok := cronShift(c.cur, c.next)
		if ok != c.ok || ok && (before != c.before || after != c.after) {
			t.Errorf("%q -> %q: %d %d %v", c.cur, c.next, before, after, ok)
		}
	}
}

// An untrusted arrival does not join a trusted proposal that changes something, and a joined
// arrival raises the record's mark (PRP-5, PRP-12, INJ-4).
func TestProposalMerge(t *testing.T) {
	ctx := context.Background()
	b := newInsightBench(t, nil)
	b.helloDone(t, "1", "", "hi")
	b.loop(t, "observe")
	if _, err := b.analysis(t, "1", false, map[string]any{"proposals": []any{changeBody("queue_long", limitsChange(10)), noneBody("seen", 1)}}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE proposals SET record = jsonb_set(record, '{data_mark}', '"summary"')`); err != nil {
		t.Fatal(err)
	}
	if _, err := b.analysis(t, "2", true, map[string]any{"proposals": []any{changeBody("queue_long", limitsChange(10)), noneBody("seen", 1)}}); err != nil {
		t.Fatal(err)
	}
	if got := rejections(t, b); len(got) != 1 || got[0] != "untrusted_direction" {
		t.Fatalf("rejections %v", got)
	}
	for _, r := range proposals(t, b) {
		ch := r["body"].(map[string]any)["change"].(map[string]any)
		us := r["untrusted_seen"].(map[string]any)
		if ch["kind"] == "none" && (r["data_mark"] != "internal" || us["value"] != true || r["seen_count"] != float64(2)) {
			t.Fatalf("merged %v %v %v", r["data_mark"], us, r["seen_count"])
		}
	}
}

// An analysis whose class is above insight.max_data of its role does not start, even when a
// catalog got past the lint (INS-2).
func TestAnalysisAboveMaxData(t *testing.T) {
	ctx := context.Background()
	b := newInsightBench(t, nil)
	err := b.c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		cat, err := b.c.Catalog(ctx)
		if err != nil {
			return err
		}
		ins := *cat.Roles["hello"].Insight
		ins.MaxData = "summary"
		role := cat.Roles["hello"]
		role.Insight = &ins
		roles := maps.Clone(cat.Roles)
		roles["hello"] = role
		narrow := *cat
		narrow.Roles = roles
		cat = &narrow
		res, err := b.c.enqueue(ctx, tx, now, cat, enqueueInput{Tenant: "demo", Type: "hello.insight.v1", Body: map[string]any{"type": "hello.insight.v1", "goal": "look"},
			IdempotencyKey: "system:schedule:insight:1", Initiator: "system", EnqueuedBy: "schedule:insight", Source: "schedule", SourceID: "insight"})
		if err == nil && res.Rejected == nil {
			t.Error("an analysis above max_data started")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
