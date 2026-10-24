// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"testing"
	"time"
)

// The tenant's dispatch rule sets the attempt deadline and lowers the attempts of its type
// (DSP-3): the deadline is fixed at claim and handed to the runtime, and when it passes the stall
// loop ends the attempt with attempt_timeout, which spends it; with one attempt left the task
// fails (FEN-1, FEN-3).
func TestDispatchRuleAttemptDeadline(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newBenchWith(t, func(root string, ms []map[string]any) []map[string]any {
		for _, m := range ms {
			switch {
			case m["kind"] == "Tenant":
				m["spec"].(map[string]any)["dispatch"] = map[string]any{"rules": []any{
					map[string]any{"match": map[string]any{"type": "hello.greet.v1"},
						"timeouts": map[string]any{"attempt": "2m"}, "retry": map[string]any{"max_attempts": 1}}}}
			case m["kind"] == "TaskType" && m["metadata"].(map[string]any)["name"] == "hello.greet.v1":
				spec := m["spec"].(map[string]any)
				to, _ := spec["timeouts"].(map[string]any)
				if to == nil {
					to = map[string]any{}
					spec["timeouts"] = to
				}
				to["attempt"] = "10m"
			}
		}
		return ms
	})
	task := b.hello(t, "1")
	att := b.claim(t, b.agent, "attempt")
	if att == nil || att.WorkID != task {
		t.Fatalf("attempt %+v", att)
	}
	until, _ := att.Payload["attempt_until"].(string)
	at, err := time.Parse(time.RFC3339, until)
	if err != nil {
		t.Fatalf("attempt_until %q: %v", until, err)
	}
	var created time.Time
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT created_at FROM leases WHERE work_id = $1 AND lease_no = $2`, task, att.LeaseNo).Scan(&created); err != nil {
		t.Fatal(err)
	}
	if d := at.Sub(created); d < 119*time.Second || d > 121*time.Second {
		t.Fatalf("the attempt deadline is %s after the claim, want the rule's 2m", d)
	}
	b.loop(t, "stall")
	var lease string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT state FROM leases WHERE work_id = $1 AND lease_no = $2`, task, att.LeaseNo).Scan(&lease); err != nil || lease != "active" {
		t.Fatalf("lease %s before the deadline %v", lease, err)
	}
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE leases SET attempt_until = attempt_until - interval '3 minutes', lease_until = lease_until + interval '1 hour'
		WHERE work_id = $1 AND lease_no = $2`, task, att.LeaseNo); err != nil {
		t.Fatal(err)
	}
	b.loop(t, "stall")
	var reason, state, treason string
	var spent int
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT l.state || ':' || l.reason, t.state, coalesce(t.reason, ''), t.attempts_spent FROM leases l JOIN tasks t ON t.id = l.work_id
		WHERE l.work_id = $1 AND l.lease_no = $2`, task, att.LeaseNo).Scan(&reason, &state, &treason, &spent); err != nil {
		t.Fatal(err)
	}
	if reason != "revoked:attempt_timeout" || state != "failed" || treason != "attempts_exhausted" || spent != 1 {
		t.Fatalf("lease %s, task %s(%s), spent %d", reason, state, treason, spent)
	}
}
