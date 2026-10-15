// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// newFactBench is the hello bench with bot memory: the role keeps facts and the type proposes
// and reads them, and the attempts of the type have a duration and an outcome history.
func newFactBench(t *testing.T) *bench {
	t.Helper()
	b := newBenchWith(t, func(_ string, ms []map[string]any) []map[string]any {
		specOf(ms, "Role", "hello")["memory"] = "bot"
		specOf(ms, "TaskType", "hello.greet.v1")["memory"] = map[string]any{"facts": "propose", "read": "bot"}
		return ms
	})
	ctx := context.Background()
	now := b.now(t)
	for i := 1; i <= 15; i++ {
		at := now.Add(-time.Duration(i) * time.Hour)
		outcome := "done"
		if i > 10 {
			outcome = "failed"
		}
		if _, err := b.c.st.Pool.Exec(ctx, `INSERT INTO obs_points (tenant, series, subject, at, num, enum, source, data_mark, key, created_at) VALUES
			('demo', 'farm.task.outcome', 'task_type:hello.greet.v1', $1, NULL, $2, 'core', 'internal', $3, $1)`, at, outcome, fmt.Sprintf("o%d", i)); err != nil {
			t.Fatal(err)
		}
		if i <= 10 {
			if _, err := b.c.st.Pool.Exec(ctx, `INSERT INTO obs_points (tenant, series, subject, at, num, enum, source, data_mark, key, created_at) VALUES
				('demo', 'farm.attempt.duration', 'task_type:hello.greet.v1', $1, $2, NULL, 'core', 'internal', $3, $1)`, at, float64(60*i), fmt.Sprintf("d%d", i)); err != nil {
				t.Fatal(err)
			}
		}
	}
	return b
}

func (b *bench) now(t *testing.T) time.Time {
	t.Helper()
	var now time.Time
	if err := b.c.st.Pool.QueryRow(context.Background(), `SELECT botaru_now()`).Scan(&now); err != nil {
		t.Fatal(err)
	}
	return now
}

func factCount(t *testing.T, b *bench, bot, state string) int {
	t.Helper()
	var n int
	if err := b.c.st.Pool.QueryRow(context.Background(), `SELECT count(*) FROM facts WHERE bot = $1 AND state = $2`, bot, state).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

var factProposals = []any{
	map[string]any{"kind": "how_long", "predicate": "task.duration.p95_min", "subject": map[string]any{"type": "tasktype", "id": "hello.greet.v1"}},
	map[string]any{"kind": "what_broke", "predicate": "task.fails_with", "subject": map[string]any{"type": "tasktype", "id": "hello.greet.v1"}},
	map[string]any{"kind": "where", "predicate": "dataset.stored_on", "subject": map[string]any{"type": "dataset", "id": "ctd"}},
}

// The model names the kind, the predicate and the subject; the core computes the value over
// the window, refuses a proposal without enough evidence with an event, and the next attempt
// of the identity reads the facts without their author (MEM-6).
func TestFactsProposeAndRead(t *testing.T) {
	ctx := context.Background()
	b := newFactBench(t)
	bot := b.botOf(t, b.agent.ID)
	_, att := b.helloAttempt(t, "1")
	if _, err := b.c.Complete(ctx, b.agent, header(t, att), CompleteRequest{Result: map[string]any{"answer": "hi", "facts": factProposals}}); err != nil {
		t.Fatal(err)
	}
	var p95 float64
	var code string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT (SELECT value::text::float FROM facts WHERE bot = $1 AND predicate = 'task.duration.p95_min'),
		(SELECT value #>> '{}' FROM facts WHERE bot = $1 AND predicate = 'task.fails_with')`, bot.ID).Scan(&p95, &code); err != nil {
		t.Fatal(err)
	}
	if p95 != 10 || code != "failed" {
		t.Fatalf("values %v %q", p95, code)
	}
	if n := countEvents(t, b, "fact.rejected.v1", att.Token.WorkID); n != 1 {
		t.Fatalf("rejected events %d", n)
	}
	var result string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT result::text FROM tasks WHERE id = $1`, att.Token.WorkID).Scan(&result); err != nil || result != `{"answer": "hi"}` {
		t.Fatalf("stored result %s %v", result, err)
	}
	// The core series are internal: a fact gets only into a task whose mark covers it.
	_, att = b.helloAttempt(t, "2")
	if facts, _ := att.Payload["facts"].([]any); len(facts) != 0 {
		t.Fatalf("facts in a task of a lower mark %v", facts)
	}
	if _, err := b.c.Complete(ctx, b.agent, header(t, att), CompleteRequest{Result: map[string]any{"answer": "hi"}}); err != nil {
		t.Fatal(err)
	}
	_, att = b.helloAttempt(t, "3", "internal")
	facts, _ := att.Payload["facts"].([]any)
	if len(facts) != 2 {
		t.Fatalf("facts read %v", att.Payload["facts"])
	}
	if f := facts[0].(map[string]any); f["from_bot"] != nil || f["acting_bot"] != nil || f["window"] == nil {
		t.Fatalf("fact view %v", f)
	}
	// Four proposals are over the per-attempt limit and the schema refuses them all.
	if _, err := b.c.Complete(ctx, b.agent, header(t, att), CompleteRequest{Result: map[string]any{"answer": "hi",
		"facts": append(append([]any{}, factProposals...), factProposals[0])}}); err != nil {
		t.Fatal(err)
	}
	if n := factCount(t, b, bot.ID, "active"); n != 2 {
		t.Fatalf("active facts %d", n)
	}
}

// A retired identity's facts are rechecked and copied to its successor with the mark
// inherited, and sealed; without a successor the offer waits for the first identity enrolled
// in the role (MEM-7).
func TestFactsOffer(t *testing.T) {
	ctx := context.Background()
	b := newFactBench(t)
	old := b.botOf(t, b.agent.ID)
	_, att := b.helloAttempt(t, "1")
	if _, err := b.c.Complete(ctx, b.agent, header(t, att), CompleteRequest{Result: map[string]any{"answer": "hi", "facts": factProposals[:2]}}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.c.BotRetire(ctx, b.op, BotRetireRequest{Bot: old.ID}); err != nil {
		t.Fatal(err)
	}
	next := b.botOf(t, b.agent.ID)
	if factCount(t, b, old.ID, "sealed") != 2 || factCount(t, b, next.ID, "active") != 2 {
		t.Fatalf("sealed %d copied %d", factCount(t, b, old.ID, "sealed"), factCount(t, b, next.ID, "active"))
	}
	var inherited int
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM facts WHERE bot = $1 AND inherited->>'from_bot' = $2`, next.ID, old.ID).Scan(&inherited); err != nil || inherited != 2 {
		t.Fatalf("inherited %d %v", inherited, err)
	}
	// The worker goes: its identity leaves after unbound_ttl and its offer waits.
	if _, err := b.c.WorkerRevoke(ctx, b.op, WorkerRequest{Op: "worker.revoke", Worker: b.agent.ID}); err != nil {
		t.Fatal(err)
	}
	b.advance(t, 31*24*3600)
	b.loop(t, "identity")
	var waiting int
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM fact_offers WHERE from_bot = $1 AND recipient_state = 'waiting'`, next.ID).Scan(&waiting); err != nil || waiting != 1 {
		t.Fatalf("waiting offers %d %v", waiting, err)
	}
	w := b.enroll(t, EnrollRequest{Tenant: "demo", Role: "hello", Kind: "agent"})
	if n := factCount(t, b, b.botOf(t, w.ID).ID, "active"); n != 2 {
		t.Fatalf("facts of the first enrolled identity %d", n)
	}
	// At the end of the seal the facts of the retired identities are wiped with it.
	b.advance(t, 31*24*3600)
	b.loop(t, "identity")
	b.loop(t, "erasure")
	b.loop(t, "identity")
	b.loop(t, "erasure")
	if factCount(t, b, old.ID, "sealed") != 0 || factCount(t, b, next.ID, "sealed") != 0 {
		t.Fatal("sealed facts outlived the seal")
	}
}
