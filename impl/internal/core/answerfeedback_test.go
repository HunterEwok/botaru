// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"testing"
)

// INS-7: with attention.answer_feedback the answer to a person gets, once it went out, a
// "Useful" / "Not needed" card in the room left of the person's card limit; a press is stored
// once, and an analysis would read the useful share of the type. At the limit no card is
// planned, the answer cards do not count against decision cards, and without the setting no
// card is planned at all.
func TestAnswerFeedback(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newBenchWith(t, func(root string, ms []map[string]any) []map[string]any {
		for _, m := range ms {
			if m["kind"] == "Tenant" {
				m["spec"].(map[string]any)["attention"] = map[string]any{"noncritical_per_day": 2, "answer_feedback": true}
			}
		}
		return ms
	})
	var cardOf func(task string) (id, nonce string, ok bool)
	answer := func(update string) string {
		task := b.hello(t, update)
		att := b.claim(t, b.agent, "attempt")
		if _, err := b.c.Complete(ctx, b.agent, header(t, att), CompleteRequest{Result: map[string]any{"answer": "Hello"}}); err != nil {
			t.Fatal(err)
		}
		if _, _, ok := cardOf(task); ok {
			t.Fatal("INS-7: a card before the answer went out")
		}
		b.drain(t)
		return task
	}
	cardOf = func(task string) (id, nonce string, ok bool) {
		err := b.c.st.Pool.QueryRow(ctx, `SELECT id, nonce FROM manager_feedback WHERE kind = $1 AND task = $2`, taskAnswer, task).Scan(&id, &nonce)
		return id, nonce, err == nil
	}
	first := answer("1")
	id, nonce, ok := cardOf(first)
	if !ok {
		t.Fatal("INS-7: no feedback card under the answer")
	}
	var planned int
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM deliveries WHERE kind = $1 AND task = $2 AND recipient = $3`, taskAnswer, first, b.alice).Scan(&planned); err != nil || planned != 1 {
		t.Fatalf("INS-7: card deliveries %d %v", planned, err)
	}
	ob := &OnBehalf{Identity: "telegram:1001", Chat: Chat{Kind: "direct", ExternalID: "1001"}}
	if _, err := b.c.CardFeedback(ctx, b.channel, ob, CardFeedbackRequest{ID: id, Nonce: nonce, Value: "useful"}); err != nil {
		t.Fatal(err)
	}
	if out, err := b.c.CardFeedback(ctx, b.channel, ob, CardFeedbackRequest{ID: id, Nonce: nonce, Value: "not_needed"}); err != nil || out["value"] != "useful" {
		t.Fatalf("INS-7: the second press %v %v", out, err)
	}
	// The second answer's card reaches the limit of 2; the third answer gets none.
	if _, _, ok := cardOf(answer("2")); !ok {
		t.Fatal("INS-7: no card under the second answer")
	}
	if _, _, ok := cardOf(answer("3")); ok {
		t.Fatal("INS-7: a card above the daily limit")
	}
	// The goal G1 of doctor counts the three answered tasks of a person (PRP-29).
	doc, err := b.c.Doctor(ctx, b.op, TenantRequest{})
	if err != nil {
		t.Fatal(err)
	}
	seen := false
	for _, ch := range doc.Checks {
		if ch.ID == "goals.g1.done_share" && ch.Tenant == "demo" {
			if seen = true; ch.Value != 1.0 {
				t.Fatalf("PRP-29: %+v", ch)
			}
		}
	}
	if !seen {
		t.Fatal("PRP-29: no goals.g1.done_share")
	}
	cat, err := b.c.Catalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := b.c.st.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	in, err := answerFeedbackInput(ctx, tx, b.now(t), cat.Tenants["demo"])
	if err != nil || len(in) != 1 {
		t.Fatalf("INS-7: input %v %v", in, err)
	}
	row := in[0].(map[string]any)
	if row["type"] != "hello.greet.v1" || row["sent"] != 2 || row["useful"] != 1 || row["not_needed"] != 0 {
		t.Fatalf("INS-7: row %v", row)
	}
	// Decision cards still have the whole limit.
	if n, err := b.c.cardsToday(ctx, tx, "demo", b.alice, tenantDay(cat.Tenants["demo"], b.now(t))); err != nil || n != 0 {
		t.Fatalf("INS-7: answer cards in the decision card count: %d %v", n, err)
	}
}

// INS-7: without the setting nothing changes.
func TestAnswerFeedbackOff(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newBench(t)
	task := b.hello(t, "1")
	att := b.claim(t, b.agent, "attempt")
	if _, err := b.c.Complete(ctx, b.agent, header(t, att), CompleteRequest{Result: map[string]any{"answer": "Hello"}}); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM manager_feedback WHERE task = $1`, task).Scan(&n); err != nil || n != 0 {
		t.Fatalf("INS-7: a card without the setting: %d %v", n, err)
	}
}
