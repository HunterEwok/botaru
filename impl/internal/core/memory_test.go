// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/HunterEwok/botaru/impl/internal/ids"
)

// newMemoryBench is the hello bench whose role keeps person memory, with bob as a person of
// the group admins (admin:people) and a telegram driver that stores no content.
func newMemoryBench(t *testing.T) (*actionBench, string) {
	t.Helper()
	b := newBenchWith(t, func(root string, ms []map[string]any) []map[string]any {
		for _, m := range ms {
			if m["kind"] == "Role" {
				m["spec"].(map[string]any)["memory"] = "person"
			}
		}
		return append(ms, map[string]any{
			"apiVersion": "botaru/v1alpha1", "kind": "Group",
			"metadata": map[string]any{"name": "admins", "tenant": "demo"},
			"spec":     map[string]any{"caps": []any{"admin:people"}, "data": []any{"summary"}},
		})
	})
	ctx := context.Background()
	bob := ids.PrincipalID()
	if _, err := b.c.Principal(ctx, b.op, PrincipalRequest{Op: "principal.add", Subject: bob, Identity: "telegram:1002"}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.c.st.Pool.Exec(ctx, `INSERT INTO memberships (tenant, principal, grp, state, created_at) VALUES ('demo', $1, 'admins', 'active', now())`, bob); err != nil {
		t.Fatal(err)
	}
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE workers SET hello = jsonb_set(coalesce(hello, '{}'), '{driver}', '{"stores_content": "none"}') WHERE id = $1`, b.channel.ID); err != nil {
		t.Fatal(err)
	}
	return &actionBench{bench: b, bob: bob}, bob
}

func (b *actionBench) say(t *testing.T, update, identity, chat, text string) *ActivityResult {
	t.Helper()
	a := Activity{Kind: "message", Channel: "telegram", UpdateID: update, Chat: Chat{Kind: "direct", ExternalID: chat}, Text: text}
	a.Sender.Identity, a.Sender.Verified = identity, true
	res, err := b.c.SubmitActivity(context.Background(), b.channel, a)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// The session of a conversation and the person's memory through the handles of the lease
// (MEM-1, MEM-3, INJ-13, blackbox:core/memory-space-computed, gov-mem-02): appends are
// idempotent by uuid, a record is active only after its person accepts it on the card, an
// attempt that saw untrusted text proposes nothing and marks the session, and a marked
// session marks the next task of the conversation.
func TestSessionAndMemory(t *testing.T) {
	ctx := context.Background()
	b, _ := newMemoryBench(t)
	b.hello(t, "1")
	att := b.claim(t, b.agent, "attempt")
	hs := att.Payload["handles"].(map[string]any)
	sh, _ := hs["session"].(string)
	mh, _ := hs["memory"].(string)
	if sh == "" || mh == "" {
		t.Fatalf("handles %v", hs)
	}
	entries := []SessionEntry{{UUID: "00000000-0000-4000-8000-000000000001", Content: "hi"}, {UUID: "00000000-0000-4000-8000-000000000002", Content: map[string]any{"role": "bot"}}}
	if r, err := b.c.SessionAppend(ctx, sh, SessionAppendRequest{Entries: entries}); err != nil || r.Appended != 2 {
		t.Fatalf("append %+v %v", r, err)
	}
	if r, err := b.c.SessionAppend(ctx, sh, SessionAppendRequest{Entries: entries}); err != nil || r.Existing != 2 || r.Appended != 0 {
		t.Fatalf("append again %+v %v", r, err)
	}
	if r, err := b.c.SessionLoad(ctx, sh, SessionLoadRequest{After: entries[0].UUID}); err != nil || len(r.Entries) != 1 || r.Entries[0].UUID != entries[1].UUID || r.UntrustedSeen {
		t.Fatalf("load %+v %v", r, err)
	}
	if _, err := b.c.SessionLoad(ctx, mh, SessionLoadRequest{}); err == nil {
		t.Fatal("a memory handle opened the session")
	}
	p, err := b.c.MemoryPropose(ctx, mh, MemoryProposeRequest{Key: "pref.style", Content: "Prefers short answers"})
	if err != nil || p.State != "proposed" || p.Existing {
		t.Fatalf("propose %+v %v", p, err)
	}
	if again, err := b.c.MemoryPropose(ctx, mh, MemoryProposeRequest{Key: "pref.style", Content: "Prefers short answers"}); err != nil || again.ID != p.ID || !again.Existing {
		t.Fatalf("propose again %+v %v", again, err)
	}
	if r, err := b.c.MemorySearch(ctx, mh, MemorySearchRequest{}); err != nil || len(r.Items) != 0 {
		t.Fatalf("a proposed record was found: %+v %v", r, err)
	}
	card := cardOf(t, b.drain(t), memoryCard, "direct:1001")
	cd := card.body["card"].(map[string]any)
	validOut(t, b.bench, "card", cd)
	var nonce string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT nonce FROM memory_items WHERE id = $1`, p.ID).Scan(&nonce); err != nil {
		t.Fatal(err)
	}
	req := DecideRequest{SubjectKind: "memory_record", ID: p.ID, Nonce: nonce, Decision: "accept"}
	if _, err := b.c.Decide(ctx, b.channel, press("telegram:1002", "direct:1002", card.external), req); err == nil {
		t.Fatal("another person decided a memory record")
	}
	if r, err := b.c.Decide(ctx, b.channel, press("telegram:1001", "direct:1001", card.external), req); err != nil || r.State != "active" {
		t.Fatalf("decide %+v %v", r, err)
	}
	if r, err := b.c.MemorySearch(ctx, mh, MemorySearchRequest{Query: "SHORT"}); err != nil || len(r.Items) != 1 {
		t.Fatalf("search %+v %v", r, err)
	} else {
		validOut(t, b.bench, "memory-item", r.Items[0])
	}

	// Untrusted text in the attempt: no proposal, and the session is marked. The attempt
	// also read data of a higher class, which the session carries to the next task.
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE tasks SET record = record || '{"untrusted_seen": true, "data_mark": "internal"}' WHERE id = $1`, att.WorkID); err != nil {
		t.Fatal(err)
	}
	if _, err := b.c.MemoryPropose(ctx, mh, MemoryProposeRequest{Key: "x", Content: "y"}); err == nil || !strings.Contains(err.Error(), "untrusted") {
		t.Fatalf("propose under untrusted: %v", err)
	}
	if _, err := b.c.SessionAppend(ctx, sh, SessionAppendRequest{Entries: []SessionEntry{{UUID: "00000000-0000-4000-8000-000000000003", Content: "z"}}}); err != nil {
		t.Fatal(err)
	}
	b.finish(t, att)
	next := b.hello(t, "2")
	att2 := b.claim(t, b.agent, "attempt")
	if att2.WorkID != next || att2.Payload["task"].(map[string]any)["record"].(map[string]any)["untrusted_seen"] != true {
		t.Fatalf("a task of a marked session is not marked: %v", att2.Payload["task"])
	}
	// The session now carries internal, which alice (data: summary) may not see: her next
	// task does not get the session loaded, and its mark stays.
	var sm string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT data_mark FROM sessions`).Scan(&sm); err != nil || sm != "internal" {
		t.Fatalf("session mark %q %v", sm, err)
	}
	hs2, _ := att2.Payload["handles"].(map[string]any)
	if hs2["session"] != nil || att2.Payload["task"].(map[string]any)["record"].(map[string]any)["data_mark"] != "summary" {
		t.Fatalf("a session above the person's class was loaded: %v %v", hs2, att2.Payload["task"])
	}
}

// /forget erases the person's memory and sessions for real through an erasure request with a
// result per store (MEM-4, RET-7, mem-fx-03).
func TestForget(t *testing.T) {
	ctx := context.Background()
	b, _ := newMemoryBench(t)
	b.hello(t, "1")
	att := b.claim(t, b.agent, "attempt")
	hs := att.Payload["handles"].(map[string]any)
	if _, err := b.c.SessionAppend(ctx, hs["session"].(string), SessionAppendRequest{Entries: []SessionEntry{{UUID: "00000000-0000-4000-8000-000000000001", Content: "hi"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.c.MemoryPropose(ctx, hs["memory"].(string), MemoryProposeRequest{Key: "k", Content: "v"}); err != nil {
		t.Fatal(err)
	}
	b.finish(t, att)
	if r := b.say(t, "2", "telegram:1001", "1001", "/forget"); r.Result != "reply" || !strings.Contains(r.Text, "erased") {
		t.Fatalf("forget %+v", r)
	}
	b.tick(t, "erasure")
	var sessions, records int
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM sessions), (SELECT count(*) FROM memory_items)`).Scan(&sessions, &records); err != nil || sessions+records != 0 {
		t.Fatalf("left %d sessions and %d records %v", sessions, records, err)
	}
	var id string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT id FROM erasure_requests WHERE reason = 'forget'`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	rec, err := b.c.ErasureStatus(ctx, b.op, ErasureIDRequest{Op: "erasure.status", ID: id})
	if err != nil {
		t.Fatal(err)
	}
	validOut(t, b.bench, "deletion-request", rec)
	if rec["state"] != "done" || len(rec["results"].(map[string]any)) != 2 {
		t.Fatalf("request %v", rec)
	}
	if countEvents(t, b.bench, "erasure.done", id) != 1 {
		t.Fatal("no erasure.done")
	}
}

// /mydata erase: the request goes to a person with admin:people who is not its author; the
// approval erases the core stores, the channel that stores nothing is not_applicable, a sink
// gets an erasure work item, and the person gets the result per store (SEC-11, RET-4, RET-7,
// blackbox:core/erasure-v01-ports, blackbox:core/erase-personal-subjects).
func TestSubjectErasure(t *testing.T) {
	ctx := context.Background()
	b, _ := newMemoryBench(t)
	sink := b.enroll(t, EnrollRequest{Tenant: "demo", Kind: "sink", Target: "archive"})
	task := b.hello(t, "1")
	b.finish(t, b.claim(t, b.agent, "attempt"))
	b.drain(t)
	if r := b.say(t, "2", "telegram:1001", "1001", "/mydata erase"); r.Result != "reply" || !strings.Contains(r.Text, "recorded") {
		t.Fatalf("mydata erase %+v", r)
	}
	s := b.drain(t)
	card := cardOf(t, s, subjectCard, "direct:1002")
	cd := card.body["card"].(map[string]any)
	validOut(t, b.bench, "card", cd)
	req := DecideRequest{SubjectKind: "subject_request", ID: cd["id"].(string), Nonce: cd["nonce"].(string), Decision: "approve"}
	r, err := b.c.Decide(ctx, b.channel, press("telegram:1002", "direct:1002", card.external), req)
	if err != nil || r.State != "approved" {
		t.Fatalf("decide %+v %v", r, err)
	}
	b.tick(t, "erasure")
	var body, result *string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT body::text, result::text FROM tasks WHERE id = $1`, task).Scan(&body, &result); err != nil || body != nil || result != nil {
		t.Fatalf("the task kept its content: %v %v %v", body, result, err)
	}
	var erasure string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT erasure FROM subject_requests WHERE id = $1`, req.ID).Scan(&erasure); err != nil {
		t.Fatal(err)
	}
	rec, _ := b.c.ErasureStatus(ctx, b.op, ErasureIDRequest{Op: "erasure.status", ID: erasure})
	if rec["state"] != "running" {
		t.Fatalf("the request closed before the sink answered: %v", rec)
	}
	item := b.claim(t, sink, "erasure")
	if item == nil || item.Payload["scope"].(map[string]any)["subject"] == nil || len(item.Payload["needles"].([]string)) != 1 {
		t.Fatalf("erasure work %+v", item)
	}
	validOut(t, b.bench, "erasure-work", item.Payload)
	if _, err := b.c.Complete(ctx, sink, header(t, item), CompleteRequest{Erasure: map[string]any{"outcome": "not_applicable"}}); err == nil {
		t.Fatal("not_applicable without a basis passed")
	}
	if _, err := b.c.Complete(ctx, sink, header(t, item), CompleteRequest{Erasure: map[string]any{"outcome": "done"}}); err != nil {
		t.Fatal(err)
	}
	b.tick(t, "erasure")
	rec, _ = b.c.ErasureStatus(ctx, b.op, ErasureIDRequest{Op: "erasure.status", ID: erasure})
	res := rec["results"].(map[string]any)
	if rec["state"] != "done" || res["channel_telegram"] != "not_applicable" || res["sink_archive"] != "done" || res["tasks"] != "done" {
		t.Fatalf("request %v", rec)
	}
	var needles *string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT needles::text FROM erasure_requests WHERE id = $1`, erasure).Scan(&needles); err != nil || needles != nil {
		t.Fatalf("a closed request kept its needles: %v %v", needles, err)
	}
	done := b.drain(t)
	if n := cardOf(t, done, "notice", "direct:1001"); !strings.Contains(fmt.Sprint(n.body), "sink_archive: done") {
		t.Fatalf("result notice %v", n.body)
	}
}

// A request an offboarding recorded before erasure steps existed gets its steps at the next
// tick and erases the stores; it is never closed with nothing erased.
func TestErasureWithoutSteps(t *testing.T) {
	ctx := context.Background()
	b, _ := newMemoryBench(t)
	task := b.hello(t, "1")
	b.finish(t, b.claim(t, b.agent, "attempt"))
	b.drain(t)
	var subject string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT initiator FROM tasks WHERE id = $1`, task).Scan(&subject); err != nil {
		t.Fatal(err)
	}
	id := ids.New(ids.Erasure)
	if _, err := b.c.st.Pool.Exec(ctx, `INSERT INTO erasure_requests (id, tenant, subject, reason, author, state, created_at, scope, deadline_at, updated_at)
		VALUES ($1, 'demo', $2, 'offboarding', '{"kind":"system"}', 'requested', now(), jsonb_build_object('tenant', 'demo', 'subject', $2::text), now() + interval '30 days', now())`, id, subject); err != nil {
		t.Fatal(err)
	}
	b.tick(t, "erasure")
	var body *string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT body::text FROM tasks WHERE id = $1`, task).Scan(&body); err != nil || body != nil {
		t.Fatalf("the task kept its content: %v %v", body, err)
	}
	rec, _ := b.c.ErasureStatus(ctx, b.op, ErasureIDRequest{Op: "erasure.status", ID: id})
	res, _ := rec["results"].(map[string]any)
	if rec["state"] != "done" || res["tasks"] != "done" || res["channel_telegram"] != "not_applicable" {
		t.Fatalf("request %v", rec)
	}
}

// erasure.match finds the exact string, records only ids, and needs a second person; an open
// request past its deadline raises its alarm (SEC-11, RET-7).
func TestEraseMatch(t *testing.T) {
	ctx := context.Background()
	b, _ := newMemoryBench(t)
	b.hello(t, "1")
	b.finish(t, b.claim(t, b.agent, "attempt"))
	b.drain(t)
	queued := b.hello(t, "2")
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE tasks SET body = body || '{"note": "call Mari Tamm"}'`); err != nil {
		t.Fatal(err)
	}
	d, err := b.c.ErasureMatch(ctx, b.op, MatchRequest{Subject: "system", Needles: []string{"Mari Tamm"}})
	if err != nil {
		t.Fatal(err)
	}
	var found string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT found::text FROM subject_requests WHERE id = $1`, d.ID).Scan(&found); err != nil || !strings.Contains(found, `"tasks": ["t_`) || strings.Contains(found, "Mari") {
		t.Fatalf("found %s %v", found, err)
	}
	card := cardOf(t, b.drain(t), subjectCard, "direct:1002")
	cd := card.body["card"].(map[string]any)
	if n := fmt.Sprint(cd["context"].(map[string]any)["found"].(map[string]any)["tasks"]); n != "2" {
		t.Fatalf("card %v", cd)
	}
	req := DecideRequest{SubjectKind: "subject_request", ID: d.ID, Nonce: cd["nonce"].(string), Decision: "approve"}
	if _, err := b.c.Decide(ctx, b.channel, press("telegram:1002", "direct:1002", card.external), req); err != nil {
		t.Fatal(err)
	}
	// The channel step fails on a driver that keeps content; the request stays open and its
	// alarm fires at the deadline.
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE workers SET hello = hello - 'driver' WHERE id = $1`, b.channel.ID); err != nil {
		t.Fatal(err)
	}
	b.tick(t, "erasure")
	var n int
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE body::text LIKE '%Mari%'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("the string is still in %d tasks %v", n, err)
	}
	// A queued task among the ids is canceled, so no attempt claims a purged body.
	var state string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT state FROM tasks WHERE id = $1`, queued).Scan(&state); err != nil || state != "canceled" {
		t.Fatalf("queued task %s %v", state, err)
	}
	if item := b.claim(t, b.agent, "attempt"); item != nil {
		t.Fatalf("claimed %+v", item)
	}
	b.advance(t, 31*24*3600)
	b.tick(t, "erasure")
	var firing int
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM alarm_episodes WHERE alarm LIKE 'alarm.erasure.overdue.%' AND state = 'firing'`).Scan(&firing); err != nil || firing != 1 {
		t.Fatalf("overdue alarm %d %v", firing, err)
	}
}
