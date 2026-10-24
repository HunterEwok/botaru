// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/mcp"
)

// msgFields is the closed field schema of the test report types.
func msgFields() map[string]any {
	return map[string]any{"type": "object", "additionalProperties": false, "required": []any{"outcome"},
		"properties": map[string]any{
			"outcome": map[string]any{"enum": []any{"done", "failed"}},
			"items":   map[string]any{"type": "integer", "minimum": 0, "maximum": 1000},
		}}
}

// newMsgBench is the hello bench with messages: hello (agent) reports report.work.v1 to the
// role lead and the bot group pool and broadcasts report.bcast.v1 to lead; lead is a job role
// whose lead.review.v1 reads its inbox; state.worker.v1 is the state of worker code.
func newMsgBench(t *testing.T) *bench { t.Helper(); return newMsgBenchWith(t, nil) }

// newMsgBenchWith is newMsgBench with more manifests.
func newMsgBenchWith(t *testing.T, more func(ms []map[string]any) []map[string]any) *bench {
	t.Helper()
	return newBenchWith(t, func(_ string, ms []map[string]any) []map[string]any {

		for _, m := range ms {
			s := m["spec"].(map[string]any)
			switch m["kind"] {
			case "Role":
				s["messages"] = map[string]any{
					"send":    []any{map[string]any{"type": "report.work.v1", "to": []any{"role:lead"}}, map[string]any{"type": "report.bcast.v1", "to": []any{"role:lead"}}},
					"receive": []any{map[string]any{"type": "report.work.v1"}},
				}
			case "TaskType":
				s["inbox"] = map[string]any{"read": []any{"report.work.v1"}}
			case "Group":
				s["caps"] = append(s["caps"].([]any), "msg.send:report.work.v1", "msg.send:report.bcast.v1", "task:lead.review.v1")
			}
		}
		mtype := func(name string, spec map[string]any) map[string]any {
			base := map[string]any{"kind": "report", "fields_schema": msgFields(), "data_class": "summary", "note": map[string]any{"max_chars": 100},
				"delivery": "one", "ttl": "1h", "max_bytes": 512, "producers": []any{"agent", "job", "core"}, "rate": map[string]any{"per_hour_per_sender": 6}}
			for k, v := range spec {
				if v == nil {
					delete(base, k)
				} else {
					base[k] = v
				}
			}
			return map[string]any{"apiVersion": "botaru/v1alpha1", "kind": "MessageType", "metadata": map[string]any{"name": name}, "spec": base}
		}
		ms = append(ms,
			mtype("report.work.v1", nil),
			mtype("report.bcast.v1", map[string]any{"delivery": "all", "note": map[string]any{"max_chars": 0}, "producers": []any{"agent", "job"}}),
			mtype("state.worker.v1", map[string]any{"kind": "state", "delivery": nil, "ttl": nil, "stale_after": "10m", "note": map[string]any{"max_chars": 0},
				"producers": []any{"code"}, "rate": nil, "fields_schema": map[string]any{"type": "object", "additionalProperties": false, "required": []any{"phase"},
					"properties": map[string]any{"phase": map[string]any{"enum": []any{"idle", "busy"}}}}}),
			map[string]any{"apiVersion": "botaru/v1alpha1", "kind": "Role", "metadata": map[string]any{"name": "lead"},
				"spec": map[string]any{"profile": "worker", "worker_kind": "job", "claims": []any{"lead.review.v1"},
					"messages": map[string]any{
						"send":    []any{map[string]any{"type": "report.work.v1", "to": []any{"role:hello"}}},
						"receive": []any{map[string]any{"type": "report.work.v1"}, map[string]any{"type": "report.bcast.v1"}, map[string]any{"type": "state.worker.v1"}},
					}}},
			map[string]any{"apiVersion": "botaru/v1alpha1", "kind": "TaskType", "metadata": map[string]any{"name": "lead.review.v1"},
				"spec": map[string]any{"title": "Review", "worker_kind": "job", "purpose": "work", "tier": 0, "data_class": "summary", "tree_budget_eur": 0.05,
					"params_schema": map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{}},
					"result_schema": map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"ok": map[string]any{"type": "boolean"}}},
					"inbox":         map[string]any{"read": []any{"report.work.v1", "report.bcast.v1", "state.worker.v1"}}}},
			map[string]any{"apiVersion": "botaru/v1alpha1", "kind": "BotGroup", "metadata": map[string]any{"name": "pool", "tenant": "demo"},
				"spec": map[string]any{"select": map[string]any{"roles": []any{"lead"}, "labels": map[string]any{"zone": "a"}}}},
			map[string]any{"apiVersion": "botaru/v1alpha1", "kind": "Group", "metadata": map[string]any{"name": "system-ops", "tenant": "demo"},
				"spec": map[string]any{"caps": []any{"task:lead.review.v1", "msg.send:report.work.v1"}, "data": []any{"summary"}}},
		)
		if more != nil {
			ms = more(ms)
		}
		return ms
	})
}

// coreSend publishes a core report at once, to any address of the tenant (MSG-16 uses this
// path for auto-reports).
func coreSend(t *testing.T, b *bench, to string) {
	t.Helper()
	ctx := context.Background()
	cat, err := b.c.Catalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	err = b.c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		_, p, err := b.c.sendMessage(ctx, tx, now, cat, &msgSender{Tenant: "demo", Role: "core", Producer: "core", Initiator: b.alice, Root: "t_root"},
			MsgSendRequest{Type: "report.work.v1", To: to, Fields: map[string]any{"outcome": "done"}})
		if p != nil {
			t.Fatalf("core send: %v", p)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// msgCall calls send_message or inbox_read and says whether the model got a yes, with the text.
func msgCall(t *testing.T, b *bench, h, name string, args map[string]any) (bool, string) {
	t.Helper()
	r, err := b.c.P3ToolsCall(context.Background(), h, mcp.CallParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	text := ""
	if len(r.Content) > 0 {
		text = r.Content[0].Text
	}
	return !r.IsError, text
}

// enqueueReview enqueues lead.review.v1 from the initiator and returns its id.
func enqueueReview(t *testing.T, b *bench, key, initiator string) string {
	t.Helper()
	ctx := context.Background()
	cat, err := b.c.Catalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var id string
	err = b.c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		res, err := b.c.enqueue(ctx, tx, now, cat, enqueueInput{Tenant: "demo", Type: "lead.review.v1", Body: map[string]any{"type": "lead.review.v1", "goal": "review"},
			IdempotencyKey: key, Initiator: initiator, EnqueuedBy: "core", Source: "route", SourceID: "test"})
		if err == nil && res.Rejected != nil {
			t.Fatalf("rejected %+v", res.Rejected)
		}
		if res != nil {
			id = res.TaskID
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func msgCount(t *testing.T, b *bench, where string, args ...any) int {
	t.Helper()
	var n int
	if err := b.c.st.Pool.QueryRow(context.Background(), `SELECT count(*) FROM messages WHERE `+where, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// claimFor claims attempts of the worker until it gets the task.
func claimFor(t *testing.T, b *bench, w *Worker, task string) *WorkItem {
	t.Helper()
	for range 10 {
		it := b.claim(t, w, "attempt")
		if it == nil {
			break
		}
		if it.Payload["task"].(map[string]any)["id"] == task {
			return it
		}
	}
	t.Fatalf("no attempt of %s", task)
	return nil
}

// An agent stages reports through send_message; they are published, with the core fields and
// without the note in any log, only in the transaction of its complete, a broadcast as a copy
// per worker of the role, and an attempt sends at most three (MSG-1, MSG-3, MSG-4, MSG-5, MSG-6,
// MSG-9, MSG-13, MSG-14, MSG-15, BGR-1).
func TestMessageSendPublish(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newMsgBench(t)
	leadA := b.enroll(t, EnrollRequest{Tenant: "demo", Role: "lead", Kind: "job", Labels: map[string]string{"zone": "a"}})
	leadB := b.enroll(t, EnrollRequest{Tenant: "demo", Role: "lead", Kind: "job", Labels: map[string]string{"zone": "b"}})
	task := b.hello(t, "1")
	att := b.claim(t, b.agent, "attempt")
	h := att.Payload["handles"].(map[string]any)["tools"].(string)
	if names := toolNames(t, b, h); !containsString(names, sendMessageName) || !containsString(names, inboxReadName) {
		t.Fatalf("catalog %v", names)
	}
	work := func(to, note string) map[string]any {
		a := map[string]any{"type": "report.work.v1", "to": to, "fields": map[string]any{"outcome": "done", "items": 3}}
		if note != "" {
			a["note"] = note
		}
		return a
	}
	const note = "the header line looked odd"
	if ok, _ := msgCall(t, b, h, sendMessageName, work("role:hello", "")); ok {
		t.Fatal("an address outside messages.send passed")
	}
	if ok, _ := msgCall(t, b, h, sendMessageName, map[string]any{"type": "state.worker.v1", "fields": map[string]any{"phase": "idle"}}); ok {
		t.Fatal("the model sent a state value")
	}
	if ok, _ := msgCall(t, b, h, sendMessageName, map[string]any{"type": "report.work.v1", "to": "role:lead", "fields": map[string]any{"outcome": "maybe"}}); ok {
		t.Fatal("fields outside the schema passed")
	}
	if ok, _ := msgCall(t, b, h, sendMessageName, work("role:lead", strings.Repeat("x", 101))); ok {
		t.Fatal("a note above max_chars passed")
	}
	if ok, text := msgCall(t, b, h, sendMessageName, work("role:lead", note)); !ok || text != `{"staged":true}` {
		t.Fatalf("send: %v %s", ok, text)
	}
	if ok, _ := msgCall(t, b, h, sendMessageName, work("group:pool", "")); ok {
		t.Fatal("a group address outside messages.send passed")
	}
	if ok, _ := msgCall(t, b, h, sendMessageName, work("role:lead", "")); !ok {
		t.Fatal("second send")
	}
	if ok, _ := msgCall(t, b, h, sendMessageName, map[string]any{"type": "report.bcast.v1", "to": "role:lead", "fields": map[string]any{"outcome": "done"}}); !ok {
		t.Fatal("broadcast")
	}
	if ok, _ := msgCall(t, b, h, sendMessageName, work("role:lead", "")); ok {
		t.Fatal("a fourth send of one attempt passed")
	}
	if n := msgCount(t, b, `state = 'staged' AND sender_task = $1`, task); n != 3 {
		t.Fatalf("staged %d", n)
	}
	if n := msgCount(t, b, `state = 'queued'`); n != 0 {
		t.Fatalf("visible before complete: %d", n)
	}
	if _, err := b.c.Complete(ctx, b.agent, header(t, att), CompleteRequest{Result: map[string]any{"answer": "hi"}}); err != nil {
		t.Fatal(err)
	}
	var raw []byte
	var stored *string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT envelope, note FROM messages WHERE inbox_kind = 'role' AND inbox_name = 'lead' AND state = 'queued'`).Scan(&raw, &stored); err != nil {
		t.Fatal(err)
	}
	var env map[string]any
	_ = json.Unmarshal(raw, &env)
	if env["tenant"] != "demo" || env["msgto"] != "role:lead" || env["fromrole"] != "hello" || env["dataorigin"] != "model" || env["untrusted"] != true ||
		env["hops"] != 1.0 || env["datamark"] != "summary" || env["correlationid"] != task || !strings.Contains(asString(env["untrustedsrc"]), "note") ||
		!strings.Contains(asString(env["idempotencykey"]), "system:msg:") || env["expiresat"] == nil {
		t.Fatalf("envelope %s", raw)
	}
	if inits := asStrings(env["origininitiators"]); len(inits) != 1 || inits[0] != b.alice {
		t.Fatalf("origininitiators %v", env["origininitiators"])
	}
	if strings.Contains(string(raw), note) || stored == nil || *stored != note {
		t.Fatalf("the note is in the envelope or lost: %s %v", raw, stored)
	}
	// No log holds the note (MSG-3, MSG-13).
	for _, tb := range []string{"audit", "events"} {
		var n int
		if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM `+tb+` x WHERE x::text LIKE '%header line%'`).Scan(&n); err != nil || n != 0 {
			t.Fatalf("%s holds the note: %d %v", tb, n, err)
		}
	}
	// The broadcast copied to each worker of the role, under a worker: address only the core
	// writes (MSG-4, MSG-14); the group entry waits in the group inbox (BGR-1).
	for _, w := range []*Worker{leadA, leadB} {
		if n := msgCount(t, b, `inbox_kind = 'worker' AND inbox_name = $1 AND type = 'report.bcast.v1' AND state = 'queued' AND envelope->>'msgto' = $2`,
			w.ID, "worker:"+strings.ToLower(w.ID)); n != 1 {
			t.Fatalf("copies of %s: %d", w.ID, n)
		}
	}
	coreSend(t, b, "group:pool")
	if n := msgCount(t, b, `inbox_kind = 'group' AND inbox_name = 'pool' AND state = 'queued'`); n != 1 {
		t.Fatalf("group entries %d", n)
	}
	if n := countEvents(t, b, "message.published", ""); n != 4 {
		t.Fatalf("published events %d", n)
	}
}

// countEventsType counts the events of a type.
func countEventsType(t *testing.T, b *bench, typ string) int {
	t.Helper()
	var n int
	if err := b.c.st.Pool.QueryRow(context.Background(), `SELECT count(*) FROM events WHERE type = $1`, typ).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// A failed sender publishes nothing; a job attempt reads its role, group and own inbox under
// its lease, a repeat returns the same entries, nack returns one at once, and complete
// acknowledges the rest; reading narrows the reader by the flag, the depth floor and the
// initiators of what it read, and a bot group is a selector, not a permission (MSG-6, MSG-7,
// MSG-9, MSG-11, BGR-1, BGR-2).
func TestMessageReadAck(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newMsgBench(t)
	lead := b.enroll(t, EnrollRequest{Tenant: "demo", Role: "lead", Kind: "job", Labels: map[string]string{"zone": "a"}})
	other := b.enroll(t, EnrollRequest{Tenant: "demo", Role: "lead", Kind: "job", Labels: map[string]string{"zone": "b"}})
	// A failed attempt publishes nothing.
	b.hello(t, "1")
	att := b.claim(t, b.agent, "attempt")
	h := att.Payload["handles"].(map[string]any)["tools"].(string)
	if ok, _ := msgCall(t, b, h, sendMessageName, map[string]any{"type": "report.work.v1", "to": "role:lead", "fields": map[string]any{"outcome": "done"}}); !ok {
		t.Fatal("send")
	}
	if _, err := b.c.Fail(ctx, b.agent, header(t, att), FailRequest{Reason: "runtime_error", Retryable: false}); err != nil {
		t.Fatal(err)
	}
	if n := msgCount(t, b, `state = 'discarded' AND reason = 'attempt_ended'`); n != 1 {
		t.Fatalf("discarded %d", n)
	}
	// A second greeting reports to the role and the group and completes.
	b.hello(t, "2")
	att = b.claim(t, b.agent, "attempt")
	h = att.Payload["handles"].(map[string]any)["tools"].(string)
	if ok, _ := msgCall(t, b, h, sendMessageName, map[string]any{"type": "report.work.v1", "to": "role:lead", "fields": map[string]any{"outcome": "done"}}); !ok {
		t.Fatal("send")
	}
	if _, err := b.c.Complete(ctx, b.agent, header(t, att), CompleteRequest{Result: map[string]any{"answer": "hi"}}); err != nil {
		t.Fatal(err)
	}
	coreSend(t, b, "group:pool")
	// A worker outside the group reads only the role entry.
	review := enqueueReview(t, b, "r1", "system")
	it := claimFor(t, b, other, review)
	got, err := b.c.MsgReceive(ctx, other, header(t, it), MsgReceiveRequest{Max: 20})
	if err != nil || len(got.Records) != 1 {
		t.Fatalf("outside the group: %+v %v", got, err)
	}
	if _, err := b.c.Fail(ctx, other, header(t, it), FailRequest{Reason: "runtime_error", Retryable: false}); err != nil {
		t.Fatal(err)
	}
	if n := msgCount(t, b, `state = 'queued' AND handouts = 1`); n != 1 {
		t.Fatalf("a failed reader returns its entry with a counted hand-out: %d", n)
	}
	review = enqueueReview(t, b, "r2", "system")
	it = claimFor(t, b, lead, review)
	got, err = b.c.MsgReceive(ctx, lead, header(t, it), MsgReceiveRequest{Max: 20})
	if err != nil || len(got.Records) != 2 || got.Hidden != 0 {
		t.Fatalf("read %+v %v", got, err)
	}
	again, err := b.c.MsgReceive(ctx, lead, header(t, it), MsgReceiveRequest{Max: 20})
	if err != nil || len(again.Records) != 2 || again.Records[0]["handle"] != got.Records[0]["handle"] {
		t.Fatalf("a repeat in the attempt: %+v %v", again, err)
	}
	rec := taskTree(t, b, review)
	if !rec.UntrustedSeen || rec.DepthFloor != 1 || len(rec.CoInitiators) != 1 || rec.CoInitiators[0] != b.alice {
		t.Fatalf("reader record %+v", rec)
	}
	// A reply goes to the sender role of the entry read; the reader sends as job.
	reply, err := b.c.MsgSend(ctx, lead, header(t, it), MsgSendRequest{Type: "report.work.v1", ReplyTo: got.Records[0]["handle"].(string), Fields: map[string]any{"outcome": "done"}})
	if err != nil || reply.State != "staged" {
		t.Fatalf("reply %+v %v", reply, err)
	}
	var hops int
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT hops FROM messages WHERE id = $1`, reply.MessageID).Scan(&hops); err != nil || hops != 2 {
		t.Fatalf("reply hops %d %v", hops, err)
	}
	if _, err := b.c.MsgSend(ctx, lead, header(t, it), MsgSendRequest{Type: "report.work.v1", ReplyTo: "notahandle1", Fields: map[string]any{"outcome": "done"}}); err == nil {
		t.Fatal("a reply to an entry this attempt did not read")
	}
	n, err := b.c.MsgAck(ctx, lead, header(t, it), MsgAckRequest{Handles: []string{got.Records[1]["handle"].(string)}}, true)
	if err != nil || n.Count != 1 {
		t.Fatalf("nack %+v %v", n, err)
	}
	if _, err := b.c.Complete(ctx, lead, header(t, it), CompleteRequest{Result: map[string]any{"ok": true}}); err != nil {
		t.Fatal(err)
	}
	if n := msgCount(t, b, `state = 'acked'`); n != 1 {
		t.Fatalf("acked %d", n)
	}
	if n := msgCount(t, b, `state = 'queued' AND handouts >= 1 AND inbox_kind <> 'role' OR state = 'queued' AND inbox_name = 'hello'`); n != 2 {
		t.Fatalf("the nacked entry and the reply: %d", n)
	}
	// A reader that read alice's report can start no work alice may not start (MSG-7).
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE tasks SET record = record || '{"co_initiators": ["human:p_nobody"]}' WHERE id = $1`, review); err != nil {
		t.Fatal(err)
	}
	cat, _ := b.c.Catalog(ctx)
	err = b.c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		res, err := b.c.enqueue(ctx, tx, now, cat, enqueueInput{Tenant: "demo", Type: "lead.review.v1", Body: map[string]any{"type": "lead.review.v1", "goal": "x"},
			IdempotencyKey: "r3", Initiator: "system", EnqueuedBy: "core", CoInitiators: []string{"human:p_nobody"}})
		if err == nil && (res.Rejected == nil || res.Rejected.Reason != "no_group_cap") {
			t.Fatalf("a co-initiator without the capability: %+v", res)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// An entry returned three times is dead; an entry past its expiry is expired, no longer handed
// out and loses its note (MSG-9, MSG-12).
func TestMessageDeadExpired(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newMsgBench(t)
	lead := b.enroll(t, EnrollRequest{Tenant: "demo", Role: "lead", Kind: "job"})
	b.hello(t, "1")
	att := b.claim(t, b.agent, "attempt")
	h := att.Payload["handles"].(map[string]any)["tools"].(string)
	for _, n := range []string{"one", "two"} {
		if ok, _ := msgCall(t, b, h, sendMessageName, map[string]any{"type": "report.work.v1", "to": "role:lead", "fields": map[string]any{"outcome": "done"}, "note": n}); !ok {
			t.Fatal("send")
		}
	}
	if _, err := b.c.Complete(ctx, b.agent, header(t, att), CompleteRequest{Result: map[string]any{"answer": "hi"}}); err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		review := enqueueReview(t, b, "d"+string(rune('0'+i)), "system")
		it := claimFor(t, b, lead, review)
		got, err := b.c.MsgReceive(ctx, lead, header(t, it), MsgReceiveRequest{Max: 1})
		if err != nil || len(got.Records) != 1 {
			t.Fatalf("read %d: %+v %v", i, got, err)
		}
		if _, err := b.c.MsgAck(ctx, lead, header(t, it), MsgAckRequest{Handles: []string{got.Records[0]["handle"].(string)}}, true); err != nil {
			t.Fatal(err)
		}
		if _, err := b.c.Complete(ctx, lead, header(t, it), CompleteRequest{Result: map[string]any{"ok": true}}); err != nil {
			t.Fatal(err)
		}
	}
	if n := msgCount(t, b, `state = 'dead' AND reason = 'delivery_exhausted' AND note IS NULL`); n != 1 {
		t.Fatalf("dead %d", n)
	}
	if countEventsType(t, b, "message.dead") != 1 {
		t.Fatal("no message.dead event")
	}
	b.advance(t, 2*3600)
	b.loop(t, "inbox")
	if n := msgCount(t, b, `state = 'expired' AND note IS NULL`); n != 1 {
		t.Fatalf("expired %d", n)
	}
	review := enqueueReview(t, b, "late", "system")
	it := claimFor(t, b, lead, review)
	if got, err := b.c.MsgReceive(ctx, lead, header(t, it), MsgReceiveRequest{Max: 5}); err != nil || len(got.Records) != 0 {
		t.Fatalf("an expired entry was handed out: %+v %v", got, err)
	}
}

// The limits of a sender: the type's class below the mark, hops past five, a hold with
// no_message, the type's rate, the sender's share of an inbox and a full inbox (MSG-6, MSG-11,
// MSG-15, HLD-3).
func TestMessageLimits(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newMsgBench(t)
	lead := b.enroll(t, EnrollRequest{Tenant: "demo", Role: "lead", Kind: "job"})
	send := func(task string) (*MsgSendResult, error) {
		it := claimFor(t, b, lead, task)
		defer func() {
			_, _ = b.c.Fail(ctx, lead, header(t, it), FailRequest{Reason: "runtime_error", Retryable: false})
		}()
		return b.c.MsgSend(ctx, lead, header(t, it), MsgSendRequest{Type: "report.work.v1", To: "role:hello", Fields: map[string]any{"outcome": "done"}})
	}
	reason := func(err error) string {
		if p, ok := AsProblem(err); ok {
			return p.Reason
		}
		return ""
	}
	task := enqueueReview(t, b, "m1", "system")
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE tasks SET record = record || '{"data_mark": "internal"}' WHERE id = $1`, task); err != nil {
		t.Fatal(err)
	}
	if _, err := send(task); reason(err) != "data_class_exceeded" {
		t.Fatalf("class: %v", err)
	}
	task = enqueueReview(t, b, "m2", "system")
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE tasks SET record = record || '{"depth_floor": 5}' WHERE id = $1`, task); err != nil {
		t.Fatal(err)
	}
	if _, err := send(task); reason(err) != "hops_exceeded" {
		t.Fatalf("hops: %v", err)
	}
	if _, err := b.c.HoldSet(ctx, OperatorActor(b.op), HoldSetRequest{Op: "hold.set", Scope: HoldScope{Kind: "role", Tenant: "demo", Name: "lead"}, Effects: []string{"no_message"}, Reason: "manual"}); err != nil {
		t.Fatal(err)
	}
	if _, err := send(enqueueReview(t, b, "m3", "system")); reason(err) != "held" {
		t.Fatalf("hold: %v", err)
	}
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE holds SET state = 'released'`); err != nil {
		t.Fatal(err)
	}
	// The sender's share: a quarter of the role inbox of 1000.
	if _, err := b.c.st.Pool.Exec(ctx, `INSERT INTO messages (id, tenant, inbox_kind, inbox_name, type, kind, delivery, state, envelope, from_role, idem_key, data_mark,
		untrusted, hops, created_at, updated_at) SELECT 'm_X' || g, 'demo', 'role', 'hello', 'report.work.v1', 'report', 'one', 'queued', '{}', 'lead', 'k' || g, 'summary',
		false, 1, botaru_now() - interval '2 hours', botaru_now() FROM generate_series(1, 250) g`); err != nil {
		t.Fatal(err)
	}
	if _, err := send(enqueueReview(t, b, "m4", "system")); reason(err) != "sender_quota" {
		t.Fatalf("share: %v", err)
	}
	if countEventsType(t, b, "inbox.flood") != 1 {
		t.Fatal("no inbox.flood")
	}
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE messages SET from_role = 'other' WHERE idem_key LIKE 'k%'`); err != nil {
		t.Fatal(err)
	}
	// The type's rate: six an hour per sender role.
	for i := range 6 {
		if res, err := send(enqueueReview(t, b, "r"+string(rune('a'+i)), "system")); err != nil || res.State != "staged" {
			t.Fatalf("send %d: %+v %v", i, res, err)
		}
	}
	if _, err := send(enqueueReview(t, b, "rz", "system")); reason(err) != "service_rate_limited" {
		t.Fatalf("rate: %v", err)
	}
	// A full inbox refuses every sender and pushes nothing out.
	if _, err := b.c.st.Pool.Exec(ctx, `INSERT INTO messages (id, tenant, inbox_kind, inbox_name, type, kind, delivery, state, envelope, from_role, idem_key, data_mark,
		untrusted, hops, created_at, updated_at) SELECT 'm_Y' || g, 'demo', 'role', 'hello', 'report.work.v1', 'report', 'one', 'queued', '{}', 'r' || (g % 10), 'f' || g, 'summary',
		false, 1, botaru_now() - interval '2 hours', botaru_now() FROM generate_series(1, 750) g`); err != nil {
		t.Fatal(err)
	}
	b.advance(t, 3700)
	if _, err := send(enqueueReview(t, b, "full", "system")); reason(err) != "inbox_full" {
		t.Fatalf("full: %v", err)
	}
}

// Worker code sends only state with its own token: the latest value per worker and type
// replaces the previous one, at most once a minute unless the phase changes, and goes stale
// after stale_after; a job attempt cannot send state, code reads current values without a lease
// (STA-1, MSG-12, MSG-14).
func TestStateValues(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newMsgBench(t)
	lead := b.enroll(t, EnrollRequest{Tenant: "demo", Role: "lead", Kind: "job"})
	put := func(phase string) error {
		_, err := b.c.MsgSend(ctx, lead, "", MsgSendRequest{Type: "state.worker.v1", Fields: map[string]any{"phase": phase}})
		return err
	}
	if err := put("idle"); err != nil {
		t.Fatal(err)
	}
	if p, ok := AsProblem(put("idle")); !ok || p.Reason != "service_rate_limited" {
		t.Fatalf("a second value within a minute: %v", p)
	}
	key := "k1"
	first, err := b.c.MsgSend(ctx, lead, "", MsgSendRequest{Type: "state.worker.v1", Fields: map[string]any{"phase": "busy"}, IdempotencyKey: &key})
	if err != nil {
		t.Fatal(err)
	}
	// A repeat with the worker's key returns the value it sent (IDM-1).
	again, err := b.c.MsgSend(ctx, lead, "", MsgSendRequest{Type: "state.worker.v1", Fields: map[string]any{"phase": "busy"}, IdempotencyKey: &key})
	if err != nil || again.MessageID != first.MessageID {
		t.Fatalf("repeat %+v %v", again, err)
	}
	if n := msgCount(t, b, `type = 'state.worker.v1' AND state = 'current'`); n != 1 {
		t.Fatalf("current %d", n)
	}
	if n := msgCount(t, b, `type = 'state.worker.v1' AND state = 'replaced'`); n != 1 {
		t.Fatalf("replaced %d", n)
	}
	if _, err := b.c.MsgSend(ctx, lead, "", MsgSendRequest{Type: "report.work.v1", To: "role:hello", Fields: map[string]any{"outcome": "done"}}); err == nil {
		t.Fatal("code outside an attempt sent a report")
	}
	got, err := b.c.MsgReceive(ctx, lead, "", MsgReceiveRequest{Max: 5})
	if err != nil || len(got.Records) != 1 || got.Records[0]["stale"] != false || got.Records[0]["fields"].(map[string]any)["phase"] != "busy" {
		t.Fatalf("state read %+v %v", got, err)
	}
	b.advance(t, 11*60)
	b.loop(t, "inbox")
	got, err = b.c.MsgReceive(ctx, lead, "", MsgReceiveRequest{Max: 5})
	if err != nil || len(got.Records) != 1 || got.Records[0]["stale"] != true {
		t.Fatalf("stale read %+v %v", got, err)
	}
	review := enqueueReview(t, b, "s1", "system")
	it := claimFor(t, b, lead, review)
	if _, err := b.c.MsgSend(ctx, lead, header(t, it), MsgSendRequest{Type: "state.worker.v1", Fields: map[string]any{"phase": "idle"}}); err == nil {
		t.Fatal("an attempt sent a state value")
	}
	// A revoked worker's values are removed.
	if _, err := b.c.WorkerRevoke(ctx, b.op, WorkerRequest{Worker: lead.ID}); err != nil {
		t.Fatal(err)
	}
	if n := msgCount(t, b, `type = 'state.worker.v1' AND state = 'removed'`); n != 1 {
		t.Fatalf("removed %d", n)
	}
}

// An entry above the reader's mark is not handed out and counts in hidden (MSG-6).
func TestMessageHidden(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newMsgBench(t)
	lead := b.enroll(t, EnrollRequest{Tenant: "demo", Role: "lead", Kind: "job"})
	b.hello(t, "1")
	att := b.claim(t, b.agent, "attempt")
	h := att.Payload["handles"].(map[string]any)["tools"].(string)
	for range 2 {
		if ok, _ := msgCall(t, b, h, sendMessageName, map[string]any{"type": "report.work.v1", "to": "role:lead", "fields": map[string]any{"outcome": "done"}}); !ok {
			t.Fatal("send")
		}
	}
	if _, err := b.c.Complete(ctx, b.agent, header(t, att), CompleteRequest{Result: map[string]any{"answer": "hi"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE messages SET data_mark = 'internal' WHERE seq = (SELECT min(seq) FROM messages)`); err != nil {
		t.Fatal(err)
	}
	it := claimFor(t, b, lead, enqueueReview(t, b, "h1", "system"))
	got, err := b.c.MsgReceive(ctx, lead, header(t, it), MsgReceiveRequest{Max: 5})
	if err != nil || len(got.Records) != 1 || got.Hidden != 1 {
		t.Fatalf("hidden %+v %v", got, err)
	}
}

// An apply that takes an inbox away ends its live entries with recipient_gone, and an ended
// entry leaves the store with its fields after the tenant's content period (90 days in the
// example tenant) (MSG-12, RET-2, RET-9).
func TestMessageGoneRetention(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newMsgBench(t)
	coreSend(t, b, "role:lead")
	coreSend(t, b, "group:pool")
	// The bot group goes; the role inbox stays.
	res, err := b.c.Apply(ctx, farmOf(b.op), ApplyRequest{Commit: "1111111111111111111111111111111111111111", Delete: []ApplyDelete{{Kind: "BotGroup", Tenant: "demo", Name: "pool"}}})
	if err != nil || !res.Applied {
		t.Fatalf("apply %+v %v", res, err)
	}
	if n := msgCount(t, b, `state = 'discarded' AND reason = 'recipient_gone' AND note IS NULL AND inbox_kind = 'group'`); n != 1 {
		t.Fatalf("entries of a gone inbox %d", n)
	}
	if n := msgCount(t, b, `state = 'queued' AND inbox_kind = 'role'`); n != 1 {
		t.Fatalf("entries of a kept inbox %d", n)
	}
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE messages SET state = 'acked' WHERE state = 'queued'`); err != nil {
		t.Fatal(err)
	}
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE messages SET updated_at = updated_at - interval '91 days'`); err != nil {
		t.Fatal(err)
	}
	b.loop(t, "retention")
	if n := msgCount(t, b, `true`); n != 0 {
		t.Fatalf("ended entries kept %d", n)
	}
}
