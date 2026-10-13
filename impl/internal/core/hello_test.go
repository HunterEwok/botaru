// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/ids"
	"github.com/HunterEwok/botaru/impl/internal/pgtest"
	"github.com/HunterEwok/botaru/impl/internal/spec"
	"github.com/HunterEwok/botaru/impl/internal/store"
	"github.com/HunterEwok/botaru/impl/internal/testbench"
	"github.com/HunterEwok/botaru/impl/internal/texts"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

// bench is a test bench core with the hello-bot manifests applied, alice enrolled with
// telegram:1001, an agent of role hello and the telegram channel.
type bench struct {
	c       *Core
	op      Operator
	alice   string
	agent   *Worker
	channel *Worker
}

func newBench(t *testing.T) *bench {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, pgtest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	root, err := spec.FindRoot("")
	if err != nil {
		t.Fatal(err)
	}
	sp, err := spec.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := texts.Load("")
	if err != nil {
		t.Fatal(err)
	}
	c, err := New(ctx, Config{TokenKey: []byte("0123456789abcdef0123456789abcdef"), TestBench: true}, st, sp, tx)
	if err != nil {
		t.Fatal(err)
	}
	ms, err := testbench.HelloBotManifests(root)
	if err != nil {
		t.Fatal(err)
	}
	op := Operator{Principal: ids.PrincipalID(), Tenant: "demo"}
	res, err := c.Apply(ctx, op, ApplyRequest{Commit: "0000000000000000000000000000000000000000", Manifests: ms})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Applied {
		t.Fatalf("apply: %+v", res.Errors)
	}
	b := &bench{c: c, op: op, alice: ids.PrincipalID()}
	if _, err := c.Principal(ctx, op, PrincipalRequest{Op: "principal.add", Subject: b.alice, Identity: "telegram:1001"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Grant(ctx, op, GrantRequest{Op: "grant", Subject: b.alice, Group: "hello"}); err != nil {
		t.Fatal(err)
	}
	b.agent = b.enroll(t, EnrollRequest{Tenant: "demo", Role: "hello", Kind: "agent"})
	b.channel = b.enroll(t, EnrollRequest{Tenant: "demo", Kind: "channel", Target: "telegram"})
	return b
}

func (b *bench) enroll(t *testing.T, req EnrollRequest) *Worker {
	t.Helper()
	ctx := context.Background()
	r, err := b.c.Enroll(ctx, b.op, req)
	if err != nil {
		t.Fatal(err)
	}
	w, err := b.c.AuthenticateWorker(ctx, r.Token)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.c.Hello(ctx, w, HelloRequest{Kind: req.Kind, ProtocolVersions: []string{"v1"}}); err != nil {
		t.Fatal(err)
	}
	w.State = "active"
	return w
}

func zero() *int { z := 0; return &z }

func (b *bench) claim(t *testing.T, w *Worker, kind string) *WorkItem {
	t.Helper()
	r, err := b.c.Claim(context.Background(), w, ClaimRequest{Kinds: []string{kind}, WaitS: zero()})
	if err != nil {
		t.Fatal(err)
	}
	return r.Work
}

func header(t *testing.T, w *WorkItem) string {
	t.Helper()
	h, err := EncodeToken(w.Token)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestHelloSequence(t *testing.T) {
	ctx := context.Background()
	b := newBench(t)
	a := Activity{Kind: "message", Channel: "telegram", UpdateID: "1", Chat: Chat{Kind: "direct", ExternalID: "1001"}, Text: "/hello hi"}
	a.Sender.Identity, a.Sender.Verified = "telegram:1001", true
	res, err := b.c.SubmitActivity(ctx, b.channel, a)
	if err != nil {
		t.Fatal(err)
	}
	if res.Result != "task" || res.Task == "" {
		t.Fatalf("submit_activity: %+v", res)
	}
	if got := taskReserve(t, b, res.Task); got != 50000 {
		t.Fatalf("enqueue reserve %d, want the tree budget 50000", got)
	}
	again, err := b.c.SubmitActivity(ctx, b.channel, a)
	if err != nil || again.Task != res.Task || !again.Existing {
		t.Fatalf("repeat: %+v %v", again, err)
	}

	for i, kind := range []string{"ai_notice", "privacy_notice"} {
		w := b.claim(t, b.channel, "delivery")
		if w == nil {
			t.Fatalf("no %s delivery", kind)
		}
		d := w.Payload["delivery"].(map[string]any)
		if d["kind"] != kind {
			t.Fatalf("delivery %d: %v", i, d["kind"])
		}
		if _, err := b.c.Complete(ctx, b.channel, header(t, w), CompleteRequest{Receipt: &Receipt{ExternalID: "47" + string(rune('0'+i))}}); err != nil {
			t.Fatal(err)
		}
	}
	if w := b.claim(t, b.channel, "delivery"); w != nil {
		t.Fatalf("unexpected delivery %v", w.Payload)
	}

	att := b.claim(t, b.agent, "attempt")
	if att == nil || att.WorkID != res.Task || att.LeaseNo != 1 {
		t.Fatalf("attempt: %+v", att)
	}
	if got := att.Payload["instruction"]; got != "Greet the person in one sentence and say that you are a farm bot." {
		t.Fatalf("instruction %q", got)
	}
	body := att.Payload["task"].(map[string]any)["body"].(map[string]any)
	if body["goal"] != "hi" || body["type"] != "hello.greet.v1" {
		t.Fatalf("body %v", body)
	}
	handle, _ := att.Payload["handles"].(map[string]any)["model"].(string)
	g, err := b.c.ResolveModelHandle(ctx, handle)
	if err != nil {
		t.Fatal(err)
	}
	if g.ProfileName != "base-std" || g.Slot != "base" || g.TreeBudget != 50000 {
		t.Fatalf("grant %+v", g)
	}
	spend, err := b.c.Reserve(ctx, g, 33000)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.c.Reserve(ctx, g, 33000); err == nil {
		t.Fatal("a second reserve above the tree budget passed")
	}
	if err := b.c.Charge(ctx, spend, 260, &Tokens{Input: 180, Output: 20}, "req_1", false); err != nil {
		t.Fatal(err)
	}

	h := header(t, att)
	hb, err := b.c.Heartbeat(ctx, b.agent, h, HeartbeatRequest{})
	if err != nil || hb.CancelRequested {
		t.Fatalf("heartbeat %+v %v", hb, err)
	}
	if _, err := b.c.Complete(ctx, b.agent, h, CompleteRequest{Result: map[string]any{"answer": 5}}); err == nil {
		t.Fatal("a result outside result_schema passed")
	}
	done := CompleteRequest{Result: map[string]any{"answer": "Hello, alice! I am the farm bot @demo_bot."}}
	if _, err := b.c.Complete(ctx, b.agent, h, done); err != nil {
		t.Fatal(err)
	}
	if got := taskReserve(t, b, res.Task); got != 0 {
		t.Fatalf("enqueue reserve %d after completion", got)
	}
	if r, err := b.c.Complete(ctx, b.agent, h, done); err != nil || r.LeaseState != "closed" {
		t.Fatalf("repeat of complete: %+v %v", r, err)
	}
	if _, err := b.c.Heartbeat(ctx, b.agent, h, HeartbeatRequest{}); err == nil {
		t.Fatal("heartbeat on a closed lease passed")
	} else if p, _ := AsProblem(err); p == nil || p.Reason != "fenced" {
		t.Fatalf("heartbeat on a closed lease: %v", err)
	}
	if _, err := b.c.ResolveModelHandle(ctx, handle); err == nil {
		t.Fatal("the handle outlived its lease")
	}

	w := b.claim(t, b.channel, "delivery")
	if w == nil {
		t.Fatal("no task_result delivery")
	}
	d := w.Payload["delivery"].(map[string]any)
	if d["kind"] != "task_result" || d["body"].(map[string]any)["text"] != "Hello, alice! I am the farm bot @demo_bot." {
		t.Fatalf("task_result %v", d)
	}
	if _, err := b.c.Complete(ctx, b.channel, header(t, w), CompleteRequest{Receipt: &Receipt{ExternalID: "4711"}}); err != nil {
		t.Fatal(err)
	}

	var events []string
	rows, err := b.c.st.Pool.Query(ctx, `SELECT type FROM events WHERE subject = $1 ORDER BY seq`, res.Task)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		events = append(events, s)
	}
	rows.Close()
	want := []string{"task.submitted", "task.working", "task.completed"}
	if len(events) != 3 || events[0] != want[0] || events[1] != want[1] || events[2] != want[2] {
		t.Fatalf("events %v", events)
	}
	if n, broken, err := b.c.VerifyAudit(ctx, "demo"); err != nil || broken != 0 || n == 0 {
		t.Fatalf("audit chain: %d %d %v", n, broken, err)
	}
}

func taskReserve(t *testing.T, b *bench, task string) int64 {
	t.Helper()
	var n int64
	if err := b.c.st.Pool.QueryRow(context.Background(), `SELECT coalesce(sum(eur_micros), 0) FROM spend WHERE task = $1 AND unit = 'task'`, task).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestEnqueueReservesEstimate: a task whose estimate does not fit the hour limit is refused at
// enqueue, not accepted and failed later (BUD-1).
func TestEnqueueReservesEstimate(t *testing.T) {
	ctx := context.Background()
	b := newBench(t)
	// 2.98 of the 3 EUR hour limit is used; the estimate of hello.greet.v1 is 0.05 EUR.
	if _, err := b.c.st.Pool.Exec(ctx, `INSERT INTO spend (id, tenant, line, unit, state, eur_micros, time)
		VALUES ('spd_test', 'demo', 'work', 'tokens', 'committed', 2980000, botaru_now())`); err != nil {
		t.Fatal(err)
	}
	a := Activity{Kind: "message", Channel: "telegram", UpdateID: "9", Chat: Chat{Kind: "direct", ExternalID: "1001"}, Text: "/hello"}
	a.Sender.Identity, a.Sender.Verified = "telegram:1001", true
	res, err := b.c.SubmitActivity(ctx, b.channel, a)
	if err != nil {
		t.Fatal(err)
	}
	if res.Result != "reply" || res.Task != "" || res.Text != b.c.texts.Render("en", "task.rejected.budget_exhausted", nil) {
		t.Fatalf("submit_activity near the limit: %+v", res)
	}
}

// TestOnFixWaitsForApply: a provider_config failure is not retried on a timer; the next apply
// wakes the task.
func TestOnFixWaitsForApply(t *testing.T) {
	ctx := context.Background()
	b := newBench(t)
	a := Activity{Kind: "message", Channel: "telegram", UpdateID: "11", Chat: Chat{Kind: "direct", ExternalID: "1001"}, Text: "/hello"}
	a.Sender.Identity, a.Sender.Verified = "telegram:1001", true
	res, err := b.c.SubmitActivity(ctx, b.channel, a)
	if err != nil {
		t.Fatal(err)
	}
	att := b.claim(t, b.agent, "attempt")
	if att == nil || att.WorkID != res.Task {
		t.Fatalf("attempt: %+v", att)
	}
	r, err := b.c.Fail(ctx, b.agent, header(t, att), FailRequest{Reason: "provider_config", Retryable: true})
	if err != nil || r.Final || r.RetryAt != "" {
		t.Fatalf("fail: %+v %v", r, err)
	}
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE tasks SET run_at = 'infinity' WHERE id = $1 AND reason = 'waiting_on_fix'`, res.Task); err != nil {
		t.Fatal(err)
	}
	if w := b.claim(t, b.agent, "attempt"); w != nil {
		t.Fatalf("an on_fix task was claimed before a fix: %v", w.WorkID)
	}
	root, err := spec.FindRoot("")
	if err != nil {
		t.Fatal(err)
	}
	ms, err := testbench.HelloBotManifests(root)
	if err != nil {
		t.Fatal(err)
	}
	if res, err := b.c.Apply(ctx, b.op, ApplyRequest{Commit: "1111111111111111111111111111111111111111", Manifests: ms}); err != nil || !res.Applied {
		t.Fatalf("apply: %+v %v", res, err)
	}
	if w := b.claim(t, b.agent, "attempt"); w == nil || w.WorkID != res.Task {
		t.Fatalf("the task did not wake after apply: %+v", w)
	}
}

// TestEnrollWorkerKind: an agent role does not take a job worker.
func TestEnrollWorkerKind(t *testing.T) {
	b := newBench(t)
	if _, err := b.c.Enroll(context.Background(), b.op, EnrollRequest{Tenant: "demo", Role: "hello", Kind: "job"}); err == nil {
		t.Fatal("a job worker was enrolled under an agent role")
	}
}

// TestTaskReserveDrawDown: the calls of a running tree draw on its task reserve, so another
// task sees the larger of the two, not their sum.
func TestTaskReserveDrawDown(t *testing.T) {
	ctx := context.Background()
	b := newBench(t)
	_, err := b.c.st.Pool.Exec(ctx, `INSERT INTO spend (id, tenant, task, root, subject, line, unit, state, eur_micros, time) VALUES
		('spd_t', 'demo', 'tsk_a', 'tsk_a', 'x', 'work', 'task', 'reserved', 80, botaru_now()),
		('spd_c', 'demo', 'tsk_a', 'tsk_a', 'x', 'work', 'tokens', 'committed', 30, botaru_now()),
		('spd_o', 'demo', 'tsk_b', 'tsk_b', 'x', 'work', 'tokens', 'committed', 100, botaru_now())`)
	if err != nil {
		t.Fatal(err)
	}
	err = b.c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		for root, want := range map[string]int64{"": 180, "tsk_a": 130} {
			got, err := windowUsed(ctx, tx, "demo", "", now.Add(-time.Hour), root)
			if err != nil {
				return err
			}
			if got != want {
				t.Errorf("own root %q: used %d, want %d", root, got, want)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestModelTextWaitsForAINotice(t *testing.T) {
	ctx := context.Background()
	b := newBench(t)
	a := Activity{Kind: "message", Channel: "telegram", UpdateID: "7", Chat: Chat{Kind: "direct", ExternalID: "1001"}, Text: "/hello"}
	a.Sender.Identity, a.Sender.Verified = "telegram:1001", true
	if _, err := b.c.SubmitActivity(ctx, b.channel, a); err != nil {
		t.Fatal(err)
	}
	att := b.claim(t, b.agent, "attempt")
	if att == nil {
		t.Fatal("no attempt")
	}
	if _, err := b.c.Complete(ctx, b.agent, header(t, att), CompleteRequest{Result: map[string]any{"answer": "hi"}}); err != nil {
		t.Fatal(err)
	}
	// ai_notice is claimed but not delivered: privacy_notice (code) may go, task_result may not.
	notice := b.claim(t, b.channel, "delivery")
	privacy := b.claim(t, b.channel, "delivery")
	if privacy == nil || privacy.Payload["delivery"].(map[string]any)["kind"] != "privacy_notice" {
		t.Fatalf("second delivery %v", privacy)
	}
	if w := b.claim(t, b.channel, "delivery"); w != nil {
		t.Fatalf("model text before ai_notice: %v", w.Payload)
	}
	if _, err := b.c.Complete(ctx, b.channel, header(t, notice), CompleteRequest{Receipt: &Receipt{ExternalID: "1"}}); err != nil {
		t.Fatal(err)
	}
	if w := b.claim(t, b.channel, "delivery"); w == nil || w.Payload["delivery"].(map[string]any)["kind"] != "task_result" {
		t.Fatalf("task_result after ai_notice: %v", w)
	}
}

func TestActivityRefusals(t *testing.T) {
	ctx := context.Background()
	b := newBench(t)
	mk := func(id, identity, text string, verified bool) Activity {
		a := Activity{Kind: "message", Channel: "telegram", UpdateID: id, Chat: Chat{Kind: "direct", ExternalID: "1001"}, Text: text}
		a.Sender.Identity, a.Sender.Verified = identity, verified
		return a
	}
	cases := []struct {
		a      Activity
		result string
		reason string
	}{
		{mk("10", "telegram:2002", "/hello", true), "ignored", "not_subject"},
		{mk("11", "telegram:1001", "/hello", false), "reply", ""},
		{mk("12", "telegram:1001", "/nope", true), "reply", ""},
		{mk("13", "telegram:1001", "/health", true), "reply", ""},
	}
	for _, tc := range cases {
		r, err := b.c.SubmitActivity(ctx, b.channel, tc.a)
		if err != nil {
			t.Fatal(err)
		}
		if r.Result != tc.result || r.Reason != tc.reason {
			t.Errorf("%s %q: %+v", tc.a.Sender.Identity, tc.a.Text, r)
		}
	}
	other := mk("14", "email:a@example.org", "/hello", true)
	if _, err := b.c.SubmitActivity(ctx, b.channel, other); err == nil {
		t.Error("an identity of another kind passed")
	}
}

func TestFailRetriesThenExhausts(t *testing.T) {
	ctx := context.Background()
	b := newBench(t)
	a := Activity{Kind: "message", Channel: "telegram", UpdateID: "20", Chat: Chat{Kind: "direct", ExternalID: "1001"}, Text: "/hello"}
	a.Sender.Identity, a.Sender.Verified = "telegram:1001", true
	if _, err := b.c.SubmitActivity(ctx, b.channel, a); err != nil {
		t.Fatal(err)
	}
	att := b.claim(t, b.agent, "attempt")
	r, err := b.c.Fail(ctx, b.agent, header(t, att), FailRequest{Reason: "runtime_error", Retryable: true})
	if err != nil || r.Final || r.RetryAt == "" {
		t.Fatalf("first fail: %+v %v", r, err)
	}
	if w := b.claim(t, b.agent, "attempt"); w != nil {
		t.Fatal("handed out before the backoff")
	}
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE tasks SET run_at = NULL`); err != nil {
		t.Fatal(err)
	}
	att2 := b.claim(t, b.agent, "attempt")
	if att2 == nil || att2.LeaseNo != 2 {
		t.Fatalf("second attempt %+v", att2)
	}
	// The old token is fenced.
	if _, err := b.c.Heartbeat(ctx, b.agent, header(t, att), HeartbeatRequest{}); err == nil {
		t.Fatal("old token passed")
	}
	// A token of another worker is a mismatch.
	if _, err := b.c.Heartbeat(ctx, b.channel, header(t, att2), HeartbeatRequest{}); err == nil {
		t.Fatal("foreign token passed")
	} else if p, _ := AsProblem(err); p.Reason != "lease_mismatch" {
		t.Fatalf("foreign token: %v", err)
	}
	r, err = b.c.Fail(ctx, b.agent, header(t, att2), FailRequest{Reason: "runtime_error", Retryable: true})
	if err != nil || !r.Final || r.Reason != "attempts_exhausted" {
		t.Fatalf("second fail: %+v %v", r, err)
	}
}
