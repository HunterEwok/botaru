// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// erasureState reads a request's state and results.
func (b *actionBench) erasureState(t *testing.T, id string) (string, map[string]any) {
	t.Helper()
	rec, err := b.c.ErasureStatus(context.Background(), b.op, ErasureIDRequest{Op: "erasure.status", ID: id})
	if err != nil {
		t.Fatal(err)
	}
	res, _ := rec["results"].(map[string]any)
	return rec["state"].(string), res
}

// A channel with a cache matches and erases by ids, the agent runtime that ran an attempt
// purges, and a replay after a restore erases by the recorded ids without a new search (RET-11,
// RET-6, adapter:channel/cache-erase-match, blackbox:core/purge-any-runtime).
func TestEraseChannelCacheReplay(t *testing.T) {
	ctx := context.Background()
	b, _ := newMemoryBench(t)
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE workers SET hello = jsonb_set(hello, '{driver}', '{"stores_content": "cache"}') WHERE id = $1`, b.channel.ID); err != nil {
		t.Fatal(err)
	}
	task := b.hello(t, "1")
	b.finish(t, b.claim(t, b.agent, "attempt"))
	b.drain(t)
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE tasks SET body = body || '{"note": "call Mari Tamm"}' WHERE id = $1`, task); err != nil {
		t.Fatal(err)
	}
	d, err := b.c.ErasureMatch(ctx, b.op, MatchRequest{Subject: "system", Needles: []string{"Mari Tamm"}})
	if err != nil {
		t.Fatal(err)
	}
	item := b.claim(t, b.channel, "erasure")
	if item == nil || item.Payload["op"] != "match" || item.Payload["target"].(map[string]any)["name"] != "telegram" {
		t.Fatalf("match work of the channel %+v", item)
	}
	validOut(t, b.bench, "erasure-work", item.Payload)
	if _, err := b.c.Complete(ctx, b.channel, header(t, item), CompleteRequest{Erasure: map[string]any{"outcome": "done", "ids": []any{"tg:out:17"}}}); err != nil {
		t.Fatal(err)
	}
	card := cardOf(t, b.drain(t), subjectCard, "direct:1002")
	cd := card.body["card"].(map[string]any)
	req := DecideRequest{SubjectKind: "subject_request", ID: d.ID, Nonce: cd["nonce"].(string), Decision: "approve"}
	if _, err := b.c.Decide(ctx, b.channel, press("telegram:1002", "direct:1002", card.external), req); err != nil {
		t.Fatal(err)
	}
	b.tick(t, "erasure")
	eraseByIDs := func() {
		t.Helper()
		item := b.claim(t, b.channel, "erasure")
		if item == nil || item.Payload["op"] != "erase" || fmt.Sprint(item.Payload["scope"].(map[string]any)["ids"]) != "[tg:out:17]" {
			t.Fatalf("erase work of the channel %+v", item)
		}
		if _, err := b.c.Complete(ctx, b.channel, header(t, item), CompleteRequest{Erasure: map[string]any{"outcome": "done"}}); err != nil {
			t.Fatal(err)
		}
	}
	eraseByIDs()
	b.purge(t)
	b.tick(t, "erasure")
	var erasure string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT erasure FROM subject_requests WHERE id = $1`, d.ID).Scan(&erasure); err != nil {
		t.Fatal(err)
	}
	if state, res := b.erasureState(t, erasure); state != "done" || res["channel_telegram"] != "done" || res[storeKey("runtime", b.agent.ID)] != "done" {
		t.Fatalf("request %s %v", state, res)
	}

	// The backup was taken before the person's request: the restore brings the content back
	// and the separate erasure log replays the request with its recorded ids only.
	log, err := b.c.ErasureLogExport(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.c.tx(ctx, func(tx pgx.Tx, _ time.Time) error {
		for _, q := range []string{`DELETE FROM erasure_steps`, `DELETE FROM subject_match_steps`, `DELETE FROM subject_requests`, `DELETE FROM erasure_requests`} {
			if _, err := tx.Exec(ctx, q); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, `UPDATE tasks SET body = '{"type": "hello.greet.v1", "goal": "hi", "note": "call Mari Tamm"}' WHERE id = $1`, task)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if n, err := b.c.RestoreBegin(ctx, log); err != nil || n == 0 {
		t.Fatalf("restore begin %d %v", n, err)
	}
	b.c.epoch++
	if err := b.c.settleRestore(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := b.c.RestoreComplete(ctx, Operator{Principal: b.op.Principal, Tenant: "farm"}, RestoreCompleteRequest{SpendPending: true}); err != nil {
		t.Fatal(err)
	}
	if err := b.c.loadRestoring(ctx); err != nil {
		t.Fatal(err)
	}
	b.tick(t, "erasure")
	if n := b.count(t, `SELECT count(*) FROM tasks WHERE body::text LIKE '%Mari%'`); n != 0 {
		t.Fatal("the replay did not erase the recorded task again")
	}
	if n := b.count(t, `SELECT count(*) FROM subject_match_steps`); n != 0 {
		t.Fatal("the replay searched again")
	}
	eraseByIDs()
	b.purge(t)
	b.tick(t, "erasure")
	if state, res := b.erasureState(t, erasure); state != "done" || res["channel_telegram"] != "done" {
		t.Fatalf("replayed request %s %v", state, res)
	}
}

// A channel with a cache that lacks the erase operation answers failed, and the request stays
// open (RET-11, RET-7); C3 purge takes the scope (tenant) and deletes every session of it, and
// an agent worker that ran no attempt of the tenant gets no purge step (RET-11, LCY-1).
func TestEraseTenantScope(t *testing.T) {
	ctx := context.Background()
	b, bob := newMemoryBench(t)
	idle := b.enroll(t, EnrollRequest{Tenant: "demo", Role: "hello", Kind: "agent"})
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE workers SET hello = jsonb_set(hello, '{driver}', '{"stores_content": "attachments"}') WHERE id = $1`, b.channel.ID); err != nil {
		t.Fatal(err)
	}
	b.hello(t, "1")
	b.finish(t, b.claim(t, b.agent, "attempt"))
	for i, who := range []string{b.alice, bob} {
		if _, err := b.c.st.Pool.Exec(ctx, `INSERT INTO sessions (id, tenant, role, conversation, subjects, created_at, updated_at) VALUES ($1, 'demo', 'hello', $2, ARRAY[$3], now(), now())`,
			fmt.Sprintf("s_t%d", i), fmt.Sprintf("conv%d", i), who); err != nil {
			t.Fatal(err)
		}
	}
	cat, err := b.c.Catalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var id string
	if err := b.c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		var err error
		id, err = b.c.requestErasure(ctx, tx, now, cat.Tenants["demo"], erasureSpec{Scope: ErasureScope{Tenant: "demo"}, Reason: "tenant_delete"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	b.tick(t, "erasure")
	if n := b.count(t, `SELECT count(*) FROM sessions WHERE tenant = 'demo'`); n != 0 {
		t.Fatalf("sessions of the tenant left: %d", n)
	}
	if item := b.claim(t, idle, "erasure"); item != nil {
		t.Fatalf("an agent that ran no attempt got purge work %+v", item)
	}
	item := b.claim(t, b.channel, "erasure")
	if item == nil || item.Payload["scope"].(map[string]any)["subject"] != nil {
		t.Fatalf("erase work of the channel %+v", item)
	}
	if _, err := b.c.Complete(ctx, b.channel, header(t, item), CompleteRequest{Erasure: map[string]any{"outcome": "failed", "reason": "not_ready"}}); err != nil {
		t.Fatal(err)
	}
	b.purge(t)
	b.tick(t, "erasure")
	if state, res := b.erasureState(t, id); state != "failed" || res["channel_telegram"] != "failed" || res["sessions"] != "done" {
		t.Fatalf("request %s %v", state, res)
	}
}
