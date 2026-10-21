// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"fmt"
	"testing"
)

// The audit records every decision of a task's way and every change: the apply with its commit,
// the admin change of a principal and a membership, the allowed and the refused enqueue, the
// claim, the completion, the delivery and the audit read itself (AUD-1).
func TestBackfillAuditRecordsDecisions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newBench(t)
	task := b.helloDone(t, "1", "", "Hello, alice!")
	for w := b.claim(t, b.channel, "delivery"); w != nil; w = b.claim(t, b.channel, "delivery") {
		if _, err := b.c.Complete(ctx, b.channel, header(t, w), CompleteRequest{Receipt: &Receipt{ExternalID: "r" + w.WorkID}}); err != nil {
			t.Fatal(err)
		}
	}
	// A refusal at enqueue: the tenant day is used up.
	if _, err := b.c.st.Pool.Exec(ctx, `INSERT INTO spend (id, tenant, line, unit, state, eur_micros, time)
		VALUES ('spd_aud', 'demo', 'work', 'tokens', 'committed', 15000000, botaru_now())`); err != nil {
		t.Fatal(err)
	}
	a := Activity{Kind: "message", Channel: "telegram", UpdateID: "2", Chat: Chat{Kind: "direct", ExternalID: "1001"}, Text: "/hello"}
	a.Sender.Identity, a.Sender.Verified = "telegram:1001", true
	if res, err := b.c.SubmitActivity(ctx, b.channel, a); err != nil || res.Task != "" {
		t.Fatalf("an enqueue over the day limit: %+v %v", res, err)
	}
	if _, err := b.c.AuditQuery(ctx, b.op, AuditQueryRequest{Command: "grant"}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		what, sql string
		args      []any
	}{
		{"apply with its commit", `record->>'command' = 'apply' AND record->>'git_rev' = $1 AND tenant = 'farm'`, []any{"0000000000000000000000000000000000000000"}},
		{"principal.add", `record->>'command' = 'principal.add' AND record->'target'->>'id' = $1`, []any{b.alice}},
		{"grant", `record->>'command' = 'grant' AND record->'target'->>'id' = $1`, []any{b.alice + "/hello"}},
		{"enqueue allowed", `record->>'kind' = 'authorize' AND record->>'command' = 'task.enqueue' AND record->'target'->>'id' = $1 AND (record->>'ok')::boolean`, []any{task}},
		{"enqueue refused", `record->>'kind' = 'authorize' AND record->>'command' = 'task.enqueue' AND NOT (record->>'ok')::boolean AND record->>'reason' = $1`, []any{"budget_exhausted"}},
		{"claim", `record->>'command' = 'task.claim' AND record->'target'->>'id' = $1`, []any{task}},
		{"completion", `record->>'command' = 'task.complete' AND record->'target'->>'id' = $1`, []any{task}},
		{"delivery", `record->>'command' = 'delivery.complete' AND record->'target'->>'id' IN (SELECT id FROM deliveries WHERE task = $1)`, []any{task}},
		{"audit read", `record->>'kind' = 'read' AND record->>'command' = $1`, []any{"audit.query"}},
	} {
		// AUD-1: each decision and change has its audit record.
		if n := b.count(t, `SELECT count(*) FROM audit WHERE `+c.sql, c.args...); n == 0 {
			t.Errorf("no audit record of the %s", c.what)
		}
	}
	if n, broken, err := b.c.VerifyAudit(ctx, "demo"); err != nil || broken != 0 || n == 0 {
		t.Fatalf("audit chain: %d %d %v", n, broken, err)
	}
}

// The append-only stores hold metadata only: a canary in a person's request and in the model's
// answer reaches the task body and the delivery, which are content with an expiry, but no audit
// record, event, usage line, observation point or rollup, loop tick or alarm value (AUD-2).
func TestBackfillAuditMetadataOnly(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newBench(t)
	const canary = "CANARY7f3a9q"
	a := Activity{Kind: "message", Channel: "telegram", UpdateID: "1", Chat: Chat{Kind: "direct", ExternalID: "1001"}, Text: "/hello " + canary}
	a.Sender.Identity, a.Sender.Verified = "telegram:1001", true
	res, err := b.c.SubmitActivity(ctx, b.channel, a)
	if err != nil || res.Task == "" {
		t.Fatalf("submit: %+v %v", res, err)
	}
	for w := b.claim(t, b.channel, "delivery"); w != nil; w = b.claim(t, b.channel, "delivery") {
		if _, err := b.c.Complete(ctx, b.channel, header(t, w), CompleteRequest{Receipt: &Receipt{ExternalID: "n" + w.WorkID}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE contacts SET ai_notice_at = now() WHERE ai_notice_at IS NULL`); err != nil {
		t.Fatal(err)
	}
	att := b.claim(t, b.agent, "attempt")
	if att == nil {
		t.Fatal("no attempt")
	}
	if _, err := b.c.Complete(ctx, b.agent, header(t, att), CompleteRequest{Result: map[string]any{"answer": "Hello " + canary}}); err != nil {
		t.Fatal(err)
	}
	b.loop(t, "observe")
	like := "%" + canary + "%"
	// The canary is there, in the content stores with an expiry.
	if b.count(t, `SELECT count(*) FROM tasks x WHERE x::text LIKE $1`, like) == 0 || b.count(t, `SELECT count(*) FROM deliveries x WHERE x::text LIKE $1`, like) == 0 {
		t.Fatal("the canary is not in the task body or the delivery")
	}
	// AUD-2: no append-only store holds it.
	for _, table := range []string{"audit", "events", "spend", "obs_points", "obs_rollups", "loop_ticks", "alarm_values", "activity_outcomes"} {
		if n := b.count(t, fmt.Sprintf(`SELECT count(*) FROM %s x WHERE x::text LIKE $1`, table), like); n != 0 {
			t.Errorf("%s holds the canary in %d rows", table, n)
		}
	}
}
