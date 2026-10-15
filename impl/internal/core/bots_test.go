// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"testing"
)

// helloAttempt makes a hello task and claims its attempt on the agent.
func (b *bench) helloAttempt(t *testing.T, update string) (string, *WorkItem) {
	t.Helper()
	ctx := context.Background()
	task := b.hello(t, update)
	for {
		w := b.claim(t, b.channel, "delivery")
		if w == nil {
			break
		}
		if _, err := b.c.Complete(ctx, b.channel, header(t, w), CompleteRequest{Receipt: &Receipt{ExternalID: "n" + update}}); err != nil {
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
	return task, att
}

// botOf is the active identity bound to a worker.
func (b *bench) botOf(t *testing.T, worker string) *botRow {
	t.Helper()
	var id string
	if err := b.c.st.Pool.QueryRow(context.Background(), `SELECT id FROM bots WHERE worker_id = $1 AND state IN ('active', 'suspended')`, worker).Scan(&id); err != nil {
		t.Fatal(err)
	}
	r, err := loadBot(context.Background(), b.c.st.Pool, id, false)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// Enroll binds a model role worker to a new identity, and every attempt, its task record and
// the audit of its claim carry the identity and its epoch; a channel worker has none (IDN-8).
func TestBotIdentityOnAttempt(t *testing.T) {
	ctx := context.Background()
	b := newBench(t)
	var none int
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM bots WHERE worker_id = $1`, b.channel.ID).Scan(&none); err != nil || none != 0 {
		t.Fatalf("channel identities %d %v", none, err)
	}
	bot := b.botOf(t, b.agent.ID)
	if bot.Epoch != 1 || bot.Mode != "worker" {
		t.Fatalf("bot %+v", bot)
	}
	if err := b.c.spec.Validate("urn:botaru:schema:bot:1", bot.view()); err != nil {
		t.Fatal(err)
	}
	task, att := b.helloAttempt(t, "1")
	if att.Token.ActingBot != bot.ID || att.Token.BotEpoch != 1 {
		t.Fatalf("token %+v", att.Token)
	}
	var rec map[string]any
	var raw []byte
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT record FROM tasks WHERE id = $1`, task).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatal(err)
	}
	if rec["acting_bot"] != bot.ID || rec["bot_epoch"] != float64(1) {
		t.Fatalf("task record %v %v", rec["acting_bot"], rec["bot_epoch"])
	}
	var actor string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT record->'actor'->>'acting_bot' FROM audit WHERE record->>'command' = 'task.claim' AND record->'target'->>'id' = $1`, task).Scan(&actor); err != nil {
		t.Fatal(err)
	}
	if actor != bot.ID {
		t.Fatalf("audit actor %q", actor)
	}
	if _, err := b.c.Complete(ctx, b.agent, header(t, att), CompleteRequest{Result: map[string]any{"answer": "hi"}}); err != nil {
		t.Fatal(err)
	}
}

// bot.retire fences the running attempt (it goes back unspent) and the live worker goes on
// under a new identity, the successor; the tombstone keeps the reason (IDN-11).
func TestBotRetire(t *testing.T) {
	ctx := context.Background()
	b := newBench(t)
	old := b.botOf(t, b.agent.ID)
	task, att := b.helloAttempt(t, "1")
	if _, err := b.c.BotRetire(ctx, b.op, BotRetireRequest{Bot: old.ID}); err != nil {
		t.Fatal(err)
	}
	_, err := b.c.Complete(ctx, b.agent, header(t, att), CompleteRequest{Result: map[string]any{"answer": "hi"}})
	wantReason(t, err, "fenced")
	gone, err := loadBot(ctx, b.c.st.Pool, old.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	next := b.botOf(t, b.agent.ID)
	if gone.State != "retired" || gone.Retired["reason"] != "human_retire" || gone.Retired["successor"] != next.ID || gone.SealedUntil == nil {
		t.Fatalf("retired %+v", gone)
	}
	var spent int
	var state string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT state, attempts_spent FROM tasks WHERE id = $1`, task).Scan(&state, &spent); err != nil {
		t.Fatal(err)
	}
	if state != "submitted" || spent != 0 {
		t.Fatalf("task %s spent %d", state, spent)
	}
	att = b.claim(t, b.agent, "attempt")
	if att == nil || att.Token.ActingBot != next.ID {
		t.Fatalf("next attempt %+v", att)
	}
	// At the end of the seal the identity gets a bot_retired erasure request and is erased.
	b.advance(t, 31*24*3600)
	b.loop(t, "identity")
	b.loop(t, "erasure")
	gone, err = loadBot(ctx, b.c.st.Pool, old.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if gone.State != "erased" || gone.Retired["reason"] != "human_retire" {
		t.Fatalf("after the seal %+v", gone)
	}
}

// worker.revoke leaves the identity unbound; enroll with the bot parameter binds it again with
// the next epoch, and an unbound identity past unbound_ttl leaves as worker_gone (IDN-8,
// IDN-11).
func TestBotUnbound(t *testing.T) {
	ctx := context.Background()
	b := newBench(t)
	bot := b.botOf(t, b.agent.ID)
	if _, err := b.c.WorkerRevoke(ctx, b.op, WorkerRequest{Op: "worker.revoke", Worker: b.agent.ID}); err != nil {
		t.Fatal(err)
	}
	r, err := loadBot(ctx, b.c.st.Pool, bot.ID, false)
	if err != nil || r.State != "unbound" {
		t.Fatalf("after revoke %+v %v", r, err)
	}
	w := b.enroll(t, EnrollRequest{Tenant: "demo", Role: "hello", Kind: "agent", Bot: bot.ID})
	if r = b.botOf(t, w.ID); r.ID != bot.ID || r.State != "active" || r.Epoch != 3 {
		t.Fatalf("bound again %+v", r)
	}
	if _, err := b.c.WorkerRevoke(ctx, b.op, WorkerRequest{Op: "worker.revoke", Worker: w.ID}); err != nil {
		t.Fatal(err)
	}
	b.advance(t, 31*24*3600)
	b.loop(t, "identity")
	r, err = loadBot(ctx, b.c.st.Pool, bot.ID, false)
	if err != nil || r.State != "retired" || r.Retired["reason"] != "worker_gone" || r.Retired["successor"] != nil {
		t.Fatalf("after unbound_ttl %+v %v", r, err)
	}
}

// seat.assign moves an active functionary identity to a commission seat with the seat epoch,
// its worker gets a new identity, and the lens review then runs on behalf of the holder on a
// worker of the seat role; seat.vacate retires the holder and the seat is vacant (IDN-10).
func TestSeatAssign(t *testing.T) {
	ctx := context.Background()
	b := newCommissionBench(t, true, func(ms []map[string]any) {
		specOf(ms, "Governance", "demo")["commission"].(map[string]any)["seat_role"] = "reviewer-a"
	})
	bot := b.botOf(t, b.agent.ID)
	if _, err := b.c.SeatAssign(ctx, b.op, SeatRequest{Seat: "commission:9", Bot: bot.ID}); err == nil {
		t.Fatal("assigned a seat the charter has not")
	}
	d, err := b.c.SeatAssign(ctx, b.op, SeatRequest{Seat: "commission:1", Bot: bot.ID})
	if err != nil || d.State != "held" {
		t.Fatalf("assign %+v %v", d, err)
	}
	held, err := loadBot(ctx, b.c.st.Pool, bot.ID, false)
	if err != nil || held.Mode != "seat" || held.Seat != "commission:1" || held.Epoch != 2 {
		t.Fatalf("holder %+v %v", held, err)
	}
	if next := b.botOf(t, b.agent.ID); next.ID == bot.ID {
		t.Fatal("the worker kept the identity that took the seat")
	}
	if countEvents(t, b, "seat.assigned.v1", "commission:1") != 1 {
		t.Fatal("no seat event")
	}
	// A repeat changes nothing.
	if _, err := b.c.SeatAssign(ctx, b.op, SeatRequest{Seat: "commission:1", Bot: bot.ID}); err != nil {
		t.Fatal(err)
	}
	proposeNone(t, b)
	b.loop(t, "proposal")
	w := b.enroll(t, EnrollRequest{Tenant: "demo", Role: "reviewer-a", Kind: "agent"})
	var found bool
	for {
		att := b.claim(t, w, "attempt")
		if att == nil {
			break
		}
		if att.Payload["task"].(map[string]any)["type"] == "review.security.v1" {
			found = true
			if att.Token.ActingBot != bot.ID || att.Token.SeatEpoch != 1 {
				t.Fatalf("seat attempt token %+v", att.Token)
			}
			// The holder leaves: the running seat attempt is fenced and goes back unspent.
			if _, err := b.c.SeatVacate(ctx, b.op, SeatRequest{Seat: "commission:1"}); err != nil {
				t.Fatal(err)
			}
			_, err := b.c.Complete(ctx, w, header(t, att), CompleteRequest{Result: map[string]any{"lens": "security", "score": 3, "confidence": "high"}})
			wantReason(t, err, "fenced")
			break
		}
		if _, err := b.c.Complete(ctx, w, header(t, att), CompleteRequest{Result: map[string]any{"lens": "operational", "score": 3, "confidence": "high"}}); err != nil {
			t.Fatal(err)
		}
	}
	if !found {
		rows, _ := b.c.st.Pool.Query(ctx, `SELECT type, state, record::text FROM tasks WHERE type LIKE 'review.%'`)
		for rows.Next() {
			var a, s2, r string
			_ = rows.Scan(&a, &s2, &r)
			t.Log(a, s2, r)
		}
		st, rec := proposalState(t, b)
		t.Fatal("no seat review", st, rec["state_reason"], rec["reviews"], rejections(t, b))
	}
	gone, err := loadBot(ctx, b.c.st.Pool, bot.ID, false)
	if err != nil || gone.State != "retired" || gone.Retired["reason"] != "replaced_by_human" {
		t.Fatalf("after vacate %+v %v", gone, err)
	}
	var state string
	var epoch int
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT state, epoch FROM seats WHERE tenant = 'demo' AND id = 'commission:1'`).Scan(&state, &epoch); err != nil {
		t.Fatal(err)
	}
	if state != "vacant" || epoch != 2 {
		t.Fatalf("seat %s %d", state, epoch)
	}
}
