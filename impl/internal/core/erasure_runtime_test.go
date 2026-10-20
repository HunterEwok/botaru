// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"slices"
	"testing"
	"time"
)

// Tests of the erasure steps and purges of a runtime that keeps sessions itself.

// A runtime that keeps sessions itself gets match work for a person's erase_match, by its
// worker id, with the needles and the request (RT-5, RET-11).
func TestRuntimeInternalMatch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newBench(t)
	b.exec(t, `UPDATE workers SET hello = jsonb_set(hello, '{runtime}', '{"session": "runtime_internal", "erasure": ["erase", "match"]}') WHERE id = $1`, b.agent.ID)
	if _, err := b.c.ErasureMatch(ctx, b.op, MatchRequest{Subject: "system", Needles: []string{"Mari Tamm"}}); err != nil {
		t.Fatal(err)
	}
	item := b.claim(t, b.agent, "erasure")
	if item == nil || item.Payload["op"] != "match" {
		t.Fatalf("match work of the runtime %+v", item)
	}
	tgt := item.Payload["target"].(map[string]any)
	if tgt["kind"] != "runtime" || tgt["worker_id"] != b.agent.ID || !slices.Equal(item.Payload["needles"].([]string), []string{"Mari Tamm"}) {
		t.Fatalf("target %+v", item.Payload)
	}
	validOut(t, b, "erasure-work", item.Payload)
}

// A runtime that keeps sessions itself and gave no match answer fails its erasure step
// at the person's confirmation, as a channel does: unmatched is not "not found" (RT-5,
// SEC-11).
func TestRuntimeInternalUnmatchedFails(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b, _ := newMemoryBench(t)
	b.exec(t, `UPDATE workers SET hello = jsonb_set(hello, '{runtime}', '{"session": "runtime_internal", "erasure": ["erase", "match"]}') WHERE id = $1`, b.agent.ID)
	b.exec(t, `UPDATE workers SET hello = jsonb_set(hello, '{driver}', '{"stores_content": "cache"}') WHERE id = $1`, b.channel.ID)
	b.hello(t, "1")
	b.finish(t, b.claim(t, b.agent, "attempt"))
	b.drain(t)
	d, err := b.c.ErasureMatch(ctx, b.op, MatchRequest{Subject: "system", Needles: []string{"Mari Tamm"}})
	if err != nil {
		t.Fatal(err)
	}
	// The runtime's match step is gone (as for a runtime that came after the request); the
	// channel answers its match with an id.
	b.exec(t, `DELETE FROM subject_match_steps WHERE kind = 'runtime'`)
	item := b.claim(t, b.channel, "erasure")
	if item == nil || item.Payload["op"] != "match" {
		t.Fatalf("match work of the channel %+v", item)
	}
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
	var state string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT s.state FROM erasure_steps s JOIN subject_requests r ON r.erasure = s.request WHERE r.id = $1 AND s.kind = 'runtime'`, d.ID).Scan(&state); err != nil || state != "failed" {
		t.Fatalf("runtime step %s %v", state, err)
	}
}

// MEM-9: the core cannot seal a runtime_internal session, so retiring the identity enqueues in
// the same transaction an erasure request with reason bot_retired for its worker, whose one
// step is the E4 purge of that runtime; the runtime claims it and closes the request, and the
// sealed identity stays sealed until its own request at the end of seal_ttl.
func TestRetireRuntimeInternalPurge(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newBench(t)
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE workers SET hello = jsonb_set(coalesce(hello, '{}'), '{runtime}', '{"session": "runtime_internal", "memory": "core"}') WHERE id = $1`, b.agent.ID); err != nil {
		t.Fatal(err)
	}
	old := b.botOf(t, b.agent.ID)
	now := b.storeNow(t)
	if _, err := b.c.st.Pool.Exec(ctx, `INSERT INTO tool_cache (tenant, key, plugin, tool, output, source_call, created_at, expires_at)
		VALUES ('demo', 'k', 'checks', 'disk', '{"v": 1}', 'c_x', $1, $2)`, now, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := b.c.BotRetire(ctx, b.op, BotRetireRequest{Bot: old.ID}); err != nil {
		t.Fatal(err)
	}
	var req, state string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT id, state FROM erasure_requests WHERE reason = 'bot_retired' AND runtime_worker = $1 AND scope->>'bot' = $2 AND NOT scope ? 'worker'`, b.agent.ID, old.ID).Scan(&req, &state); err != nil {
		t.Fatalf("MEM-9: no bot_retired request for the retired identity on the runtime: %v", err)
	}
	rec, err := erasureRecord(ctx, b.c.st.Pool, "demo", req)
	if err != nil {
		t.Fatal(err)
	}
	validOut(t, b, "deletion-request", rec)
	// The successor bound to the same worker is not the scope of the purge.
	if succ := b.botOf(t, b.agent.ID); succ.ID == old.ID {
		t.Fatal("MEM-9: no successor identity on the worker")
	}
	// The runtime purge reaches only the runtime: the tenant's tool cache stays.
	if n := b.count(t, `SELECT count(*) FROM tool_cache WHERE tenant = 'demo'`); n != 1 {
		t.Fatal("MEM-9: the runtime purge of a retired identity dropped the tenant's tool cache")
	}
	if n := b.count(t, `SELECT count(*) FROM erasure_steps WHERE request = $1`, req); n != 1 {
		t.Fatalf("MEM-9: %d steps", n)
	}
	if n := b.count(t, `SELECT count(*) FROM erasure_steps WHERE request = $1 AND kind = 'runtime' AND target = $2`, req, b.agent.ID); n != 1 {
		t.Fatal("MEM-9: the step is not the purge of the worker's runtime")
	}
	b.loop(t, "erasure")
	w := b.claim(t, b.agent, "erasure")
	if w == nil {
		t.Fatal("MEM-9: the runtime got no purge")
	}
	if w.Payload["op"] != "erase" || w.Payload["reason"] != "bot_retired" || w.Payload["target"].(map[string]any)["worker_id"] != b.agent.ID ||
		w.Payload["scope"].(map[string]any)["bot"] != old.ID {
		t.Fatalf("MEM-9: purge payload %v", w.Payload)
	}
	validOut(t, b, "erasure-work", w.Payload)
	if _, err := b.c.Complete(ctx, b.agent, header(t, w), CompleteRequest{Erasure: map[string]any{"outcome": "done"}}); err != nil {
		t.Fatal(err)
	}
	b.loop(t, "erasure")
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT state FROM erasure_requests WHERE id = $1`, req).Scan(&state); err != nil || state != "done" {
		t.Fatalf("MEM-9: request %s %v", state, err)
	}
	var botState string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT state FROM bots WHERE id = $1`, old.ID).Scan(&botState); err != nil || botState != "retired" {
		t.Fatalf("MEM-9: the purge erased the sealed identity: %s %v", botState, err)
	}
	// A runtime without runtime_internal gets no such request.
	b2 := newBench(t)
	o2 := b2.botOf(t, b2.agent.ID)
	if _, err := b2.c.BotRetire(ctx, b2.op, BotRetireRequest{Bot: o2.ID}); err != nil {
		t.Fatal(err)
	}
	if n := b2.count(t, `SELECT count(*) FROM erasure_requests WHERE reason = 'bot_retired'`); n != 0 {
		t.Fatalf("MEM-9: %d requests for a runtime without its own store", n)
	}
}

// MEM-9, RET-6: a restore that replays the runtime purge of a retired identity from the
// separate erasure log keeps it a purge of the runtime only: the replay has the worker, its one
// step is the runtime's, and closing it leaves the sealed identity and its facts alone.
func TestRestoreKeepsRuntimePurge(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newBench(t)
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE workers SET hello = jsonb_set(coalesce(hello, '{}'), '{runtime}', '{"session": "runtime_internal", "memory": "core"}') WHERE id = $1`, b.agent.ID); err != nil {
		t.Fatal(err)
	}
	old := b.botOf(t, b.agent.ID)
	if _, err := b.c.BotRetire(ctx, b.op, BotRetireRequest{Bot: old.ID}); err != nil {
		t.Fatal(err)
	}
	var req string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT id FROM erasure_requests WHERE reason = 'bot_retired' AND runtime_worker = $1`, b.agent.ID).Scan(&req); err != nil {
		t.Fatal(err)
	}
	log, err := b.c.ErasureLogExport(ctx)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(log, func(e ErasureLogEntry) bool { return e.ID == req })
	if i < 0 || log[i].RuntimeWorker != b.agent.ID {
		t.Fatalf("MEM-9: the erasure log lost the runtime worker: %+v", log)
	}
	// The backup was taken before the request: the store does not have it.
	if _, err := b.c.st.Pool.Exec(ctx, `DELETE FROM erasure_steps WHERE request = $1`, req); err != nil {
		t.Fatal(err)
	}
	if _, err := b.c.st.Pool.Exec(ctx, `DELETE FROM erasure_requests WHERE id = $1`, req); err != nil {
		t.Fatal(err)
	}
	if n, err := b.c.RestoreBegin(ctx, log); err != nil || n != 1 {
		t.Fatalf("restore begin: %d %v", n, err)
	}
	if err := b.c.settleRestore(ctx); err != nil {
		t.Fatal(err)
	}
	if err := b.c.loadRestoring(ctx); err != nil {
		t.Fatal(err)
	}
	b.loop(t, "erasure")
	if n := b.count(t, `SELECT count(*) FROM erasure_steps WHERE request = $1`, req); n != 1 {
		t.Fatalf("MEM-9: the replay has %d steps", n)
	}
	if n := b.count(t, `SELECT count(*) FROM erasure_steps WHERE request = $1 AND kind = 'runtime' AND target = $2`, req, b.agent.ID); n != 1 {
		t.Fatal("MEM-9: the replayed step is not the runtime's purge")
	}
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE erasure_steps SET state = 'done' WHERE request = $1`, req); err != nil {
		t.Fatal(err)
	}
	b.loop(t, "erasure")
	var state, botState string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT state FROM erasure_requests WHERE id = $1`, req).Scan(&state); err != nil || state != "done" {
		t.Fatalf("MEM-9: replayed request %s %v", state, err)
	}
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT state FROM bots WHERE id = $1`, old.ID).Scan(&botState); err != nil || botState != "retired" {
		t.Fatalf("MEM-9: the replayed purge ended the sealed identity: %s %v", botState, err)
	}
}

// worker.revoke of a worker whose runtime declared session: runtime_internal records an erasure
// request with the worker as its scope and the reason worker_revoke; the runtime never ran an
// attempt here, so its step closes not_applicable (LCY-4).
func TestWorkerRevokeRuntimePurge(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newBench(t)
	r, err := b.c.Enroll(ctx, b.op, EnrollRequest{Tenant: "demo", Role: "hello", Kind: "agent"})
	if err != nil {
		t.Fatal(err)
	}
	w, err := b.c.AuthenticateWorker(ctx, r.Token)
	if err != nil {
		t.Fatal(err)
	}
	// A runtime_internal runtime that answers erase and match passes hello (RT-5).
	if _, err := b.c.Hello(ctx, w, HelloRequest{Kind: "agent", ProtocolVersions: []string{"v1"},
		Runtime: &RuntimeDecl{Session: "runtime_internal", Erasure: []string{"erase", "match"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.c.WorkerRevoke(ctx, b.op, WorkerRequest{Op: "worker.revoke", Worker: w.ID}); err != nil {
		t.Fatal(err)
	}
	var id, scope, reason string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT id, scope->>'worker', reason FROM erasure_requests WHERE tenant = 'demo'`).Scan(&id, &scope, &reason); err != nil {
		t.Fatal(err)
	}
	if scope != w.ID || reason != "worker_revoke" {
		t.Fatalf("erasure %s %s", scope, reason)
	}
	if err := b.c.tickLoop(ctx, "erasure"); err != nil {
		t.Fatal(err)
	}
	var state, basis string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT state, coalesce(basis, '') FROM erasure_steps WHERE request = $1 AND kind = 'runtime'`, id).Scan(&state, &basis); err != nil || state != "not_applicable" || basis != "tenant_absent" {
		t.Fatalf("runtime step %s %s %v", state, basis, err)
	}
	// A worker without such a runtime gets no erasure request at revoke.
	if _, err := b.c.WorkerRevoke(ctx, b.op, WorkerRequest{Op: "worker.revoke", Worker: b.agent.ID}); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM erasure_requests`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("requests %d %v", n, err)
	}
}
