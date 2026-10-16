// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/HunterEwok/botaru/impl/internal/spec"
)

// recordFields are the task and attempt record fields only the core writes (IDN-1).
var recordFields = []string{"tenant", "initiator", "initiators", "origin", "conversation", "parent", "root", "depth", "correlationid",
	"max_tier", "data_mark", "untrusted_seen", "untrusted_source", "approval_id", "continues", "campaign", "campaign_item", "enqueued_by",
	"seat_epoch", "incident", "grant", "takeover_of", "to_group", "priority_class", "fairness_key", "pin", "script_run", "script_step",
	"acting_bot", "bot_epoch", "trace", "state", "attempt"}

// Only the core writes the record fields: the request, the task body and an enqueue_child call
// with any of them are refused by the schema, and a child gets the record the core built
// (IDN-1, blackbox:model/child-record-fields).
func TestBackfillRecordFieldsCoreOnly(t *testing.T) {
	b := newChainBench(t, 3)
	for _, f := range recordFields {
		// IDN-1: P1 enqueue and the task body refuse the field.
		if err := b.c.spec.Validate("enqueue-request", map[string]any{"body": greet("x"), "idempotency_key": "k", f: "v"}); err == nil {
			t.Errorf("enqueue-request with %s passed the schema", f)
		}
		body := greet("x")
		body[f] = "v"
		if err := b.c.spec.Validate("task-body", body); err == nil {
			t.Errorf("task-body with %s passed the schema", f)
		}
	}
	root := b.hello(t, "1")
	att := b.claim(t, b.agent, "attempt")
	h := att.Payload["handles"].(map[string]any)["tools"].(string)
	for _, f := range []string{"initiator", "data_mark", "tenant", "untrusted_seen", "priority_class"} {
		// IDN-1: the model's enqueue_child call with the field is refused and makes no task.
		if r := childCall(t, b, h, map[string]any{"type": "hello.child.v1", "goal": "look it up", f: "v"}); r.Code == "enqueued" {
			t.Errorf("enqueue_child with %s: %+v", f, r)
		}
	}
	if n := b.count(t, `SELECT count(*) FROM tasks WHERE record->>'parent' = $1`, root); n != 0 {
		t.Fatalf("%d children from refused calls", n)
	}
}

// The initiator is verified and inherited: a child and its continuation copy the initiator,
// origin, conversation, the mark and untrusted_seen of the parent (IDN-2,
// blackbox:model/child-record-fields).
func TestBackfillInitiatorInherited(t *testing.T) {
	ctx := context.Background()
	b := newChainBench(t, 1)
	root := b.hello(t, "1")
	att := b.claim(t, b.agent, "attempt")
	// The root read someone else's text.
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE tasks SET record = record || '{"untrusted_seen": true, "untrusted_source": "forward"}' WHERE id = $1`, root); err != nil {
		t.Fatal(err)
	}
	h := att.Payload["handles"].(map[string]any)["tools"].(string)
	r := childCall(t, b, h, map[string]any{"type": "hello.child.v1", "goal": "look it up"})
	if r.Code != "enqueued" {
		t.Fatalf("child %+v", r)
	}
	type rec struct {
		initiator, origin, conversation, mark string
		untrusted                             bool
	}
	read := func(id string) rec {
		t.Helper()
		var out rec
		var origin []byte
		var conv, mark *string
		var untrusted *bool
		if err := b.c.st.Pool.QueryRow(ctx, `SELECT initiator, record->'origin', record->>'conversation', record->>'data_mark', (record->>'untrusted_seen')::bool FROM tasks WHERE id = $1`, id).
			Scan(&out.initiator, &origin, &conv, &mark, &untrusted); err != nil {
			t.Fatal(err)
		}
		out.origin = string(origin)
		if conv != nil {
			out.conversation = *conv
		}
		if mark != nil {
			out.mark = *mark
		}
		out.untrusted = untrusted != nil && *untrusted
		return out
	}
	want := read(root)
	if !strings.HasPrefix(want.initiator, "human:") || want.origin == "" || want.conversation == "" {
		t.Fatalf("the root record: %+v", want)
	}
	// IDN-2: the child has the root's initiator, origin, conversation, mark and untrusted_seen.
	if got := read(r.ID); got != want {
		t.Fatalf("the child record %+v, the root %+v", got, want)
	}
	if _, err := b.c.Complete(ctx, b.agent, header(t, att), CompleteRequest{Result: map[string]any{"answer": "asked"}}); err != nil {
		t.Fatal(err)
	}
	catt := b.claim(t, b.agent, "attempt")
	if _, err := b.c.Complete(ctx, b.agent, header(t, catt), CompleteRequest{Result: map[string]any{"answer": "found"}}); err != nil {
		t.Fatal(err)
	}
	var cont string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT id FROM tasks WHERE idempotency_key = $1`, "system:cont:reply:"+r.ID).Scan(&cont); err != nil {
		t.Fatal(err)
	}
	// IDN-2: so does the continuation.
	if got := read(cont); got != want {
		t.Fatalf("the continuation record %+v, the root %+v", got, want)
	}
}

// No commands or decisions without a real sender: an unverified sender, an unknown identity
// and a forwarded command create no task and run no command; a channel with weak sender
// verification runs no privileged command and takes no decision, while /help still answers
// (IDN-3, adapter:channel/sender-chat, adapter:channel/unbound-chat, channel/on-behalf).
func TestBackfillRealSender(t *testing.T) {
	ctx := context.Background()
	b := newBench(t)
	submit := func(update, identity string, verified, forwarded bool, text string) *ActivityResult {
		t.Helper()
		a := Activity{Kind: "message", Channel: "telegram", UpdateID: update, Chat: Chat{Kind: "direct", ExternalID: "1001"}, Text: text, Forwarded: forwarded}
		a.Sender.Identity, a.Sender.Verified = identity, verified
		res, err := b.c.SubmitActivity(ctx, b.channel, a)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	tasks := func() int { return b.count(t, `SELECT count(*) FROM tasks WHERE type = 'hello.greet.v1'`) }
	// IDN-3: an unverified sender (a message on behalf of the chat) gets no task.
	if r := submit("1", "telegram:1001", false, false, "/hello hi"); r.Result == "task" || tasks() != 0 {
		t.Fatalf("an unverified sender: %+v", r)
	}
	// IDN-3: an unknown identity creates no task.
	if r := submit("2", "telegram:9999", true, false, "/hello hi"); r.Result == "task" || tasks() != 0 {
		t.Fatalf("an unknown identity: %+v", r)
	}
	// IDN-3: a forwarded command is no command.
	if r := submit("3", "telegram:1001", true, true, "/hello hi"); r.Result == "task" {
		var untrusted bool
		if err := b.c.st.Pool.QueryRow(ctx, `SELECT coalesce((record->>'untrusted_seen')::bool, false) FROM tasks WHERE id = $1`, r.Task).Scan(&untrusted); err != nil || !untrusted {
			t.Fatalf("a forwarded command made a trusted task: %+v", r)
		}
		var goal string
		if err := b.c.st.Pool.QueryRow(ctx, `SELECT body->>'goal' FROM tasks WHERE id = $1`, r.Task).Scan(&goal); err != nil || goal == "hi" {
			t.Fatalf("a forwarded command ran as the command: goal %q", goal)
		}
	}
	// A channel with weak sender verification.
	tv, _, err := b.c.requireTenant(ctx, "demo")
	if err != nil {
		t.Fatal(err)
	}
	ch := tv.Channels["telegram"]
	ch.IdentityAssurance = "weak"
	tv.Channels["telegram"] = ch
	if tv, _, _ = b.c.requireTenant(ctx, "demo"); tv.Channels["telegram"].IdentityAssurance != "weak" {
		t.Fatal("the channel is not weak")
	}
	// IDN-3: no privileged command.
	before := b.count(t, `SELECT count(*) FROM tasks`)
	for i, cmd := range []string{"/cancel all", "/run x", "/service x", "/forget"} {
		r := submit("w"+string(rune('a'+i)), "telegram:1001", true, false, cmd)
		if r.Result != "reply" || r.Text != b.c.texts.Render("en", "activity.not_verified", nil) {
			t.Errorf("%s on a weak channel: %+v", cmd, r)
		}
	}
	if n := b.count(t, `SELECT count(*) FROM tasks`); n != before {
		t.Fatal("a weak channel's command made a task")
	}
	if r := submit("wh", "telegram:1001", true, false, "/help"); r.Result != "reply" || r.Text == b.c.texts.Render("en", "activity.not_verified", nil) {
		t.Fatalf("/help on a weak channel: %+v", r)
	}
	// IDN-3: no decision.
	if _, err := b.c.Decide(ctx, b.channel, press("telegram:1001", "direct:1001", "1"), DecideRequest{SubjectKind: "approval", ID: "ap_x", Nonce: "n", Decision: "approve"}); err == nil {
		t.Fatal("a weak channel took a decision")
	}
}

// A personal assistant is deferred: enrollment has no owner, a chat is not bound to an assistant
// and a type of a role with person memory is no default type, so no task of a person reaches an
// assistant (IDN-5, blackbox:core/assistant-owner-only, D-114).
func TestBackfillAssistantDeferred(t *testing.T) {
	ctx := context.Background()
	b := newBench(t)
	// IDN-5: worker.enroll names no owner.
	if err := b.c.spec.Validate("p2-worker-enroll-request", map[string]any{"tenant": "demo", "role": "hello", "kind": "agent", "owner": b.alice}); err == nil {
		t.Fatal("worker.enroll accepted an owner")
	}
	if _, ok := reflect.TypeOf(EnrollRequest{}).FieldByName("Owner"); ok {
		t.Fatal("the enroll request carries an owner")
	}
	// IDN-5: no chat binds to an assistant.
	if _, err := b.c.ChatBind(ctx, b.op, ChatBindRequest{Tenant: "demo", Channel: "telegram", Chat: Chat{Kind: "direct", ExternalID: "1001"}, Assistant: true}); err == nil {
		t.Fatal("a chat was bound to an assistant")
	} else {
		wantReason(t, err, "version_unsupported")
	}
	// IDN-5: a type of a person-memory role is no default type of the tenant.
	c := lintCore(t)
	codes := lintCodes(t, c, `apiVersion: botaru/v1alpha1
kind: Role
metadata: {name: pa}
spec: {profile: worker, worker_kind: agent, claims: [pa.ask.v1], memory: person, models: {base: base-std}}
---
apiVersion: botaru/v1alpha1
kind: Tenant
metadata: {name: demo}
spec: {kind: test, default_types: [pa.ask.v1]}
`)
	if !slices.Contains(codes, "assistant_in_default_types") {
		t.Fatalf("codes %v", codes)
	}
}

// Code chooses the task type: a command word picks the type from the tenant's commands, the
// text of a message never names a type, and an unknown command makes no task (IDN-6,
// blackbox:channel/type-by-command).
func TestBackfillTypeByCode(t *testing.T) {
	ctx := context.Background()
	b := newChainBench(t, 1)
	submit := func(update, text string) *ActivityResult {
		t.Helper()
		a := Activity{Kind: "message", Channel: "telegram", UpdateID: update, Chat: Chat{Kind: "direct", ExternalID: "1001"}, Text: text}
		a.Sender.Identity, a.Sender.Verified = "telegram:1001", true
		res, err := b.c.SubmitActivity(ctx, b.channel, a)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	typeOf := func(id string) string {
		var typ string
		if err := b.c.st.Pool.QueryRow(ctx, `SELECT type FROM tasks WHERE id = $1`, id).Scan(&typ); err != nil {
			t.Fatal(err)
		}
		return typ
	}
	// IDN-6: the command word picks the type.
	if r := submit("1", "/hello hi"); r.Result != "task" || typeOf(r.Task) != "hello.greet.v1" {
		t.Fatalf("/hello: %+v", r)
	}
	// IDN-6: a type named in the text is not chosen.
	for i, text := range []string{"/hello.child.v1 look", "run hello.child.v1 now", `{"type": "hello.child.v1"}`} {
		r := submit("t"+string(rune('a'+i)), text)
		if r.Result == "task" && typeOf(r.Task) == "hello.child.v1" {
			t.Fatalf("%q chose the type", text)
		}
	}
	// IDN-6: an unknown command makes no task.
	before := b.count(t, `SELECT count(*) FROM tasks`)
	if r := submit("u", "/nosuch hi"); r.Result == "task" {
		t.Fatalf("an unknown command: %+v", r)
	}
	if n := b.count(t, `SELECT count(*) FROM tasks`); n != before {
		t.Fatal("an unknown command made a task")
	}
}

// Identity and position grant no permissions: the permission check takes no identity, position,
// seat, persona, rank or track record, neither in its request schema nor in the core's request;
// a principal is a person, a program or system, never a bot (IDN-9, gov-fx-05).
func TestBackfillNoPermissionFromIdentity(t *testing.T) {
	root, err := spec.FindRoot("")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(root, "spec/model/authz-request.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var s map[string]any
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	var names []string
	var walk func(v any)
	walk = func(v any) {
		switch n := v.(type) {
		case map[string]any:
			if p, ok := n["properties"].(map[string]any); ok {
				for k := range p {
					names = append(names, k)
				}
			}
			for _, x := range n {
				walk(x)
			}
		case []any:
			for _, x := range n {
				walk(x)
			}
		}
	}
	walk(s)
	var fields []string
	var tags func(reflect.Type)
	tags = func(rt reflect.Type) {
		for i := range rt.NumField() {
			f := rt.Field(i)
			fields = append(fields, strings.Split(f.Tag.Get("json"), ",")[0])
			if f.Type.Kind() == reflect.Struct {
				tags(f.Type)
			}
		}
	}
	tags(reflect.TypeOf(authzRequest{}))
	for _, bad := range []string{"bot", "acting_bot", "bot_epoch", "identity", "position", "seat", "seat_epoch", "persona", "rank", "max_rank", "track_record", "score"} {
		// IDN-9: neither the schema nor the core's request has the field.
		if slices.Contains(names, bad) || slices.Contains(fields, bad) {
			t.Errorf("the permission check takes %s", bad)
		}
	}
	kinds, _ := s["properties"].(map[string]any)["principal"].(map[string]any)["properties"].(map[string]any)["kind"].(map[string]any)["enum"].([]any)
	if slices.Contains(kinds, any("bot")) || len(kinds) != 3 {
		t.Fatalf("principal kinds %v", kinds)
	}
}

// A task of an identity without memory or tools runs on any worker of the identity's role in
// its tenant: the seat task of the farm manager runs on whichever worker of the role claims it,
// acting_bot stays the seat holder, and the executing worker is in the lease and the audit
// (IDN-12, gov-fx-07).
func TestBackfillSeatTaskAnyWorker(t *testing.T) {
	ctx := context.Background()
	b := newManagerBench(t)
	w1 := b.enroll(t, EnrollRequest{Tenant: "demo", Kind: "agent", Role: "farm-manager"})
	w2 := b.enroll(t, EnrollRequest{Tenant: "demo", Kind: "agent", Role: "farm-manager"})
	b.diskAlarm(t, true, 3)
	b.advance(t, 6*60)
	b.loop(t, "incident")
	if _, err := b.c.SeatAssign(ctx, b.farmOp(), SeatRequest{Seat: managerSeatID}); err != nil {
		t.Fatal(err)
	}
	m := b.managerSeat(t)
	// IDN-12: the second worker of the role takes it, though neither is the holder's binding.
	item := b.claim(t, w2, "attempt")
	if item == nil {
		t.Fatal("the other worker of the role got no seat task")
	}
	if item.Token.ActingBot != m.Holder || item.Token.ActingBot == w1.Bot || item.Token.ActingBot == w2.Bot {
		t.Fatalf("acting_bot %s, holder %s", item.Token.ActingBot, m.Holder)
	}
	if n := b.count(t, `SELECT count(*) FROM leases WHERE work_id = $1 AND worker_id = $2 AND acting_bot = $3`, item.WorkID, w2.ID, m.Holder); n != 1 {
		t.Fatal("the lease does not name the executing worker and the holder")
	}
	if n := b.count(t, `SELECT count(*) FROM audit WHERE record->>'command' = 'task.claim' AND record->'target'->>'id' = $1 AND record->'after'->>'worker' = $2`, item.WorkID, w2.ID); n != 1 {
		t.Fatal("the audit does not name the executing worker")
	}
}
