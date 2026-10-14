// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/HunterEwok/botaru/impl/internal/mcp"
)

// newChainBench is the hello bench with a manager role: hello may enqueue hello.child.v1, whose
// end continues as a hello.greet.v1 continuation; the greeting may ask a question back
// (need_input) and stalls after 60 s without progress.
func newChainBench(t *testing.T, maxChildren int) *bench {
	t.Helper()
	return newBenchWith(t, func(root string, ms []map[string]any) []map[string]any {
		var child map[string]any
		for _, m := range ms {
			s := m["spec"].(map[string]any)
			switch m["kind"] {
			case "TaskType":
				if m["metadata"].(map[string]any)["name"] == "hello.greet.v1" {
					b, _ := json.Marshal(m)
					_ = json.Unmarshal(b, &child)
					s["continuation"] = map[string]any{"type": "hello.greet.v1", "goal": "Tell the person what the child found."}
					s["max_children"] = maxChildren
					s["timeouts"] = map[string]any{"stall_after": "60s"}
					rs := s["result_schema"].(map[string]any)
					rs["properties"].(map[string]any)["need_input"] = map[string]any{"type": "boolean", "x-botaru-read-by": "code"}
				}
			case "Role":
				s["profile"] = "manager"
				s["claims"] = []any{"hello.greet.v1", "hello.child.v1"}
				s["may_enqueue"] = []any{"hello.child.v1"}
			case "Group":
				s["caps"] = append(s["caps"].([]any), "task:hello.child.v1", "task.read:hello.greet.v1")
			}
		}
		child["metadata"].(map[string]any)["name"] = "hello.child.v1"
		child["spec"].(map[string]any)["summary_for_managers"] = "Looks something up"
		return append(ms, child)
	})
}

func childCall(t *testing.T, b *bench, h string, args map[string]any) childResult {
	t.Helper()
	r, err := b.c.P3ToolsCall(context.Background(), h, mcp.CallParams{Name: enqueueChildName, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	var out childResult
	raw, _ := json.Marshal(r.StructuredContent)
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func taskTree(t *testing.T, b *bench, id string) treeRecord {
	t.Helper()
	r, err := loadTree(context.Background(), b.c.st.Pool, id)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// A child of enqueue_child and its reply continuation (PRM-6, CNT-1, CNT-2, CNT-3,
// blackbox:model/child-type-not-allowed, blackbox:model/chain-too-deep).
func TestChildAndContinuation(t *testing.T) {
	ctx := context.Background()
	b := newChainBench(t, 1)
	root := b.hello(t, "1")
	att := b.claim(t, b.agent, "attempt")
	h := att.Payload["handles"].(map[string]any)["tools"].(string)
	if names := toolNames(t, b, h); !containsString(names, enqueueChildName) {
		t.Fatalf("catalog %v", names)
	}
	if r := childCall(t, b, h, map[string]any{"type": "hello.greet.v1", "goal": "x"}); r.Code != "type_not_allowed" {
		t.Fatalf("a type outside may_enqueue: %+v", r)
	}
	r := childCall(t, b, h, map[string]any{"type": "hello.child.v1", "goal": "look it up"})
	if r.Code != "enqueued" || r.ID == "" {
		t.Fatalf("child %+v", r)
	}
	if again := childCall(t, b, h, map[string]any{"type": "hello.child.v1", "goal": "look it up"}); again.ID != r.ID || !again.Existing {
		t.Fatalf("a repeated call made a new child: %+v", again)
	}
	if more := childCall(t, b, h, map[string]any{"type": "hello.child.v1", "goal": "another"}); more.Code != "tree_too_large" {
		t.Fatalf("over max_children: %+v", more)
	}
	ch := taskTree(t, b, r.ID)
	if ch.Parent != root || ch.Root != root || ch.Depth != 1 || ch.Origin == nil || ch.reportsToOrigin() {
		t.Fatalf("child record %+v", ch)
	}
	if _, err := b.c.Complete(ctx, b.agent, header(t, att), CompleteRequest{Result: map[string]any{"answer": "asked"}}); err != nil {
		t.Fatal(err)
	}
	// The child ends: its result goes to no person, the continuation is enqueued once.
	catt := b.claim(t, b.agent, "attempt")
	if catt == nil || catt.Payload["task"].(map[string]any)["id"] != r.ID {
		t.Fatalf("child attempt %v", catt)
	}
	if _, err := b.c.Complete(ctx, b.agent, header(t, catt), CompleteRequest{Result: map[string]any{"answer": "found"}}); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM deliveries WHERE task = $1`, r.ID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("deliveries of the child %d %v", n, err)
	}
	var cont, body string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT id, body::text FROM tasks WHERE idempotency_key = $1`, "system:cont:reply:"+r.ID).Scan(&cont, &body); err != nil {
		t.Fatal(err)
	}
	k := taskTree(t, b, cont)
	if k.Parent != root || k.Root != root || k.Depth != 2 || k.Origin == nil || !k.reportsToOrigin() || !strings.Contains(body, "found") {
		t.Fatalf("continuation %+v %s", k, body)
	}

	// The depth limit: a tree at depth 5 has no deeper child.
	deep := b.hello(t, "2")
	datt := b.claim(t, b.agent, "attempt")
	for datt != nil && datt.Payload["task"].(map[string]any)["id"] != deep {
		datt = b.claim(t, b.agent, "attempt")
	}
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE tasks SET record = record || '{"depth": 5}' WHERE id = $1`, deep); err != nil {
		t.Fatal(err)
	}
	dh := datt.Payload["handles"].(map[string]any)["tools"].(string)
	if r := childCall(t, b, dh, map[string]any{"type": "hello.child.v1", "goal": "deeper"}); r.Code != "depth_exceeded" {
		t.Fatalf("depth: %+v", r)
	}
}

// A person's answer continues the earlier task: by a reply to the bot message, or without a
// reply within the follow-up window that need_input opens; a command closes the window
// (CNT-5, blackbox:channel/followup-window).
func TestFollowupWindow(t *testing.T) {
	ctx := context.Background()
	b := newChainBench(t, 10)
	first := b.hello(t, "1")
	att := b.claim(t, b.agent, "attempt")
	if _, err := b.c.Complete(ctx, b.agent, header(t, att), CompleteRequest{Result: map[string]any{"answer": "Which site?", "need_input": true}}); err != nil {
		t.Fatal(err)
	}
	var text string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT body->>'text' FROM deliveries WHERE task = $1 AND kind = 'task_result'`, first).Scan(&text); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "30 minutes") {
		t.Fatalf("no follow-up hint: %q", text)
	}
	say := func(update, txt, replyTo string) string {
		a := Activity{Kind: "message", Channel: "telegram", UpdateID: update, Chat: Chat{Kind: "direct", ExternalID: "1001"}, Text: txt, ReplyTo: replyTo}
		a.Sender.Identity, a.Sender.Verified = "telegram:1001", true
		res, err := b.c.SubmitActivity(ctx, b.channel, a)
		if err != nil {
			t.Fatal(err)
		}
		return res.Task
	}
	b.advance(t, 300)
	second := say("2", "the shop", "")
	var continues *string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT record->>'continues' FROM tasks WHERE id = $1`, second).Scan(&continues); err != nil || deref(continues) != first {
		t.Fatalf("an answer in the window: %v %v", continues, err)
	}
	// The window closed with that answer.
	// The window closed with that answer: without a default type the next message is no task.
	if third := say("3", "and more", ""); third != "" {
		t.Fatalf("a second answer continued: %s", third)
	}
	// A reply to the bot message always links.
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE deliveries SET state = 'delivered', external_id = '777' WHERE task = $1 AND kind = 'task_result'`, first); err != nil {
		t.Fatal(err)
	}
	b.advance(t, 3000)
	fourth := say("4", "late reply", "777")
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT record->>'continues' FROM tasks WHERE id = $1`, fourth).Scan(&continues); err != nil || deref(continues) != first {
		t.Fatalf("a reply: %v %v", continues, err)
	}
}

// An attempt without progress for longer than stall_after ends with stalled, is spent and
// raises alarm.task.stalled; a running tool call holds it (CNT-4, blackbox:core/stall).
func TestStall(t *testing.T) {
	ctx := context.Background()
	b := newChainBench(t, 10)
	task := b.hello(t, "1")
	att := b.claim(t, b.agent, "attempt")
	p := 0.5
	if _, err := b.c.Heartbeat(ctx, b.agent, header(t, att), HeartbeatRequest{Progress: &p}); err != nil {
		t.Fatal(err)
	}
	b.advance(t, 40)
	// The same progress again is no progress.
	if _, err := b.c.Heartbeat(ctx, b.agent, header(t, att), HeartbeatRequest{Progress: &p}); err != nil {
		t.Fatal(err)
	}
	b.advance(t, 30)
	if err := b.c.tickLoop(ctx, "stall"); err != nil {
		t.Fatal(err)
	}
	var state, reason string
	var spent int
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT l.state, l.reason, t.attempts_spent FROM leases l JOIN tasks t ON t.id = l.work_id WHERE l.work_id = $1 AND l.lease_no = 1`, task).
		Scan(&state, &reason, &spent); err != nil {
		t.Fatal(err)
	}
	if state != "revoked" || reason != "stalled" || spent != 1 {
		t.Fatalf("lease %s %s spent %d", state, reason, spent)
	}
	var firing int
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM alarm_episodes WHERE alarm LIKE 'alarm.task.stalled.%' AND state = 'firing'`).Scan(&firing); err != nil || firing != 1 {
		t.Fatalf("alarm %d %v", firing, err)
	}
	// The next attempt, after the backoff, resolves the alarm.
	b.advance(t, 600)
	if b.claim(t, b.agent, "attempt") == nil {
		t.Fatal("no retry")
	}
	if err := b.c.tickLoop(ctx, "stall"); err != nil {
		t.Fatal(err)
	}
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM alarm_episodes WHERE alarm LIKE 'alarm.task.stalled.%' AND state = 'firing'`).Scan(&firing); err != nil || firing != 0 {
		t.Fatalf("alarm after the retry %d %v", firing, err)
	}
}
