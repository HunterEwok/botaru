// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"strings"
	"testing"
)

// escalationBench is the v0.2 manager bench with the seat type manager.escalations.v1, the
// necessity type review.escalation.v1 claimed by the reviewer role esc-reviewer, system-ops
// allowed to enqueue it, and review as the month of the tenant's review line.
func escalationBench(t *testing.T, review float64) *bench {
	t.Helper()
	nec := seatType(necessityType)
	asMap(nec["spec"])["purpose"] = "review"
	extra := []map[string]any{nec,
		{"apiVersion": "botaru/v1alpha1", "kind": "Role", "metadata": map[string]any{"name": "esc-reviewer"},
			"spec": map[string]any{"profile": "reviewer", "worker_kind": "agent", "claims": []any{necessityType}, "models": map[string]any{"base": "review-a"}}},
		{"apiVersion": "botaru/v1alpha1", "kind": "Group", "metadata": map[string]any{"name": "system-ops", "tenant": "demo"},
			"spec": map[string]any{"caps": []any{"task:" + necessityType}, "data": []any{"summary", "internal"}}},
	}
	return managerV02Bench(t, []string{escalationsType}, extra, func(ms []map[string]any) {
		for _, m := range ms {
			if m["kind"] == "Tenant" {
				asMap(asMap(asMap(m["spec"])["budgets"])["lines"])["review"] = map[string]any{"month_eur": review}
			}
		}
	})
}

// escalateOnce runs one hello attempt of worker w that calls escalate with args, then completes;
// it returns the tool answer.
func (b *bench) escalateOnce(t *testing.T, w *Worker, update string, args ...map[string]any) []map[string]any {
	t.Helper()
	ctx := context.Background()
	b.hello(t, update)
	att := b.claim(t, w, "attempt")
	if att == nil {
		t.Fatal("no attempt")
	}
	h := att.Payload["handles"].(map[string]any)["tools"].(string)
	var outs []map[string]any
	for _, a := range args {
		outs = append(outs, callTool(t, b, h, escalateToolName, a))
	}
	if _, err := b.c.Complete(ctx, w, header(t, att), CompleteRequest{Result: map[string]any{"answer": "ok"}}); err != nil {
		t.Fatal(err)
	}
	return outs
}

func escalationStates(t *testing.T, b *bench) map[string]string {
	t.Helper()
	rows, err := b.c.st.Pool.Query(context.Background(), `SELECT fields->>'category', state || ':' || coalesce(reason, '') || ':' || dup_count FROM escalations`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			t.Fatal(err)
		}
		out[k] = v
	}
	return out
}

// An escalation is a closed record of a live attempt, once per attempt, with a subject of its
// own (MGR-5). Code screens it: a duplicate raises dup_count, the worker rate and a normal path
// dismiss it, a firing alarm on the subject merges it, and the rest goes to the necessity lens
// without the note; a score at the forward threshold forwards it as manager.escalations.v1 with
// the key system:escalation:<attempt> (MGR-6, COM-13). Without money on the review line it is
// parked.
func TestEscalations(t *testing.T) {
	ctx := context.Background()
	b := escalationBench(t, 5)
	self := map[string]any{"kind": "self"}
	esc := func(category string, extra map[string]any) map[string]any {
		m := map[string]any{"category": category, "subject": self, "urgency": "normal", "note": "secret words"}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	outs := b.escalateOnce(t, b.agent, "u1",
		esc("resource_stuck", map[string]any{"subject": map[string]any{"kind": "resource", "id": "res:db7:data"}}),
		esc("other", nil), esc("dependency_down", nil))
	if outs[0]["code"] != "input_schema_invalid" || outs[1]["staged"] != true || outs[2]["code"] != "escalation_rate_limited" {
		t.Fatalf("escalate %v", outs)
	}
	var id, review, input string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT e.id, e.review_task, t.body::text FROM escalations e JOIN tasks t ON t.id = e.review_task
		WHERE e.state = 'in_review' AND t.idempotency_key LIKE '%system:necessity:' || e.id`).Scan(&id, &review, &input); err != nil {
		t.Fatalf("in review: %v %v", err, escalationStates(t, b))
	}
	if strings.Contains(input, "secret") {
		t.Fatalf("the lens sees the note: %s", input)
	}
	// The same worker within the hour: a duplicate, then the worker rate.
	b.escalateOnce(t, b.agent, "u2", esc("other", nil))
	b.escalateOnce(t, b.agent, "u3", esc("capacity_exhausted", nil))
	w2 := b.enroll(t, EnrollRequest{Tenant: "demo", Role: "hello", Kind: "agent"})
	b.escalateOnce(t, w2, "u4", esc("data_risk", map[string]any{"requested_response": "request_action"}))
	w3 := b.enroll(t, EnrollRequest{Tenant: "demo", Role: "hello", Kind: "agent"})
	// A firing alarm on the worker (an episode as the alarm loop writes it).
	if _, err := b.c.st.Pool.Exec(ctx, `INSERT INTO alarm_episodes (tenant, alarmkey, alarm, state, severity, subject, seq, opened_at, last_seen_at)
		VALUES ('demo', 'worker.stale:' || $1, 'alarm.worker.stale.v1', 'firing', 'warn', jsonb_build_object('kind', 'worker', 'id', $1::text), 1, now(), now())`, strings.ToLower(w3.ID)); err != nil {
		t.Fatal(err)
	}
	b.escalateOnce(t, w3, "u5", esc("suspected_compromise", nil))
	got := escalationStates(t, b)
	want := map[string]string{"other": "in_review::1", "capacity_exhausted": "dismissed:escalation_rate_limited:0",
		"data_risk": "dismissed:use_normal_path:0", "suspected_compromise": "merged::0"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: %s, want %s", k, got[k], v)
		}
	}

	// The lens score at the threshold forwards it to the seat.
	rw := b.enroll(t, EnrollRequest{Tenant: "demo", Role: "esc-reviewer", Kind: "agent"})
	if err := b.review(t, rw, map[string]any{"lens": necessityLens, "score": 2, "reason": "plausible_risk"}); err != nil {
		t.Fatal(err)
	}
	var state string
	var prio int
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT e.state, t.priority_class FROM escalations e JOIN tasks t ON t.id = e.manager_task
		WHERE e.id = $1 AND t.type = $2 AND t.idempotency_key LIKE '%system:escalation:' || e.task || ':' || e.lease_no`, id, escalationsType).Scan(&state, &prio); err != nil || state != "forwarded" || prio != 3 {
		t.Fatalf("forward %s %d %v", state, prio, err)
	}

	// An empty review line parks it.
	p := escalationBench(t, 0)
	p.escalateOnce(t, p.agent, "u1", esc("other", nil))
	if got := escalationStates(t, p)["other"]; got != "parked::0" {
		t.Fatalf("parked %s", got)
	}
}
