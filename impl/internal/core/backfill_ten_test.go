// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/HunterEwok/botaru/impl/internal/ids"
	"github.com/HunterEwok/botaru/impl/internal/mcp"
)

// TEN-1: every record of the listed kinds belongs to exactly one tenant: its table has a
// tenant column that is never empty, and no statement of the core changes the tenant of a
// record. A hold of scope farm is a record of the service tenant farm, a hold of another scope
// a record of the tenant of its scope.
func TestBackfillRecordTenant(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newBench(t)
	for _, table := range []string{"tasks", "leases", "toolcalls", "approvals", "deliveries", "principals", "identities", "memberships",
		"workers", "files", "memory_items", "facts", "sessions", "spend", "events", "audit", "messages", "proposals", "holds",
		"campaigns", "script_runs", "incidents", "control_ops", "bots", "seats", "governance_cases", "attributions", "erasure_requests", "obs_points"} {
		var nullable string
		err := b.c.st.Pool.QueryRow(ctx, `SELECT is_nullable FROM information_schema.columns WHERE table_schema = current_schema() AND table_name = $1 AND column_name = 'tenant'`, table).Scan(&nullable)
		if err != nil || nullable != "NO" {
			t.Errorf("TEN-1: table %s: tenant column %q %v", table, nullable, err)
		}
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	change := regexp.MustCompile(`(?i)SET\s+(\w+\s*=\s*[^,]+,\s*)*tenant\s*=|tenant\s*=\s*EXCLUDED\.tenant`)
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if loc := change.FindIndex(src); loc != nil {
			t.Errorf("TEN-1: %s changes the tenant of a record: %q", f, src[loc[0]:loc[1]])
		}
	}
	farmOp := OperatorActor(Operator{Principal: ids.PrincipalID(), Tenant: "farm"})
	for _, tc := range []struct {
		scope HoldScope
		want  string
	}{
		{HoldScope{Kind: "tasktype", Tenant: "demo", Name: "hello.greet.v1"}, "demo"},
		{HoldScope{Kind: "farm"}, "farm"},
	} {
		set, err := b.c.HoldSet(ctx, farmOp, HoldSetRequest{Scope: tc.scope, Effects: []string{"no_enqueue"}, Reason: "manual"})
		if err != nil {
			t.Fatalf("hold %s: %v", tc.scope.Kind, err)
		}
		var tenant string
		if err := b.c.st.Pool.QueryRow(ctx, `SELECT tenant FROM holds WHERE id = $1`, set.ID).Scan(&tenant); err != nil || tenant != tc.want {
			t.Fatalf("TEN-1: a hold of scope %s is a record of %q, want %q (%v)", tc.scope.Kind, tenant, tc.want, err)
		}
	}
}

// TIM-1: leases run on the store clock only: the answers carry the remaining seconds, a
// worker clock shifted by 10 minutes in its report changes nothing, and the lease ends when the
// store clock passes it, not the worker's.
func TestBackfillStoreClock(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newBench(t)
	b.hello(t, "1")
	att := b.claim(t, b.agent, "attempt")
	if att == nil || att.LeaseRemainingS <= 0 {
		t.Fatalf("TIM-1: the claim answer carries no remaining seconds: %+v", att)
	}
	shifted := time.Now().Add(10 * time.Minute).UTC().Format(time.RFC3339)
	hb, err := b.c.Heartbeat(ctx, b.agent, header(t, att), HeartbeatRequest{Health: []ComponentHealth{{Component: "runtime", State: "ok", Time: shifted}}})
	if err != nil || hb.LeaseRemainingS <= 0 || hb.LeaseRemainingS > att.LeaseRemainingS+1 {
		t.Fatalf("TIM-1: a heartbeat with a shifted clock: %+v %v", hb, err)
	}
	b.advance(t, hb.LeaseRemainingS+1)
	b.loop(t, "reaper")
	if _, err := b.c.Heartbeat(ctx, b.agent, header(t, att), HeartbeatRequest{}); reasonOf(err) != "fenced" {
		t.Fatalf("TIM-1: the lease outlived the store clock: %v", err)
	}
}

// STA-2: a state self-report decides nothing: a worker that reports itself idle but is not
// seen goes stale by the core's own observation, and another worker still gets the queued task.
func TestBackfillStateInformational(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newMsgBench(t)
	lead := b.enroll(t, EnrollRequest{Tenant: "demo", Role: "lead", Kind: "job"})
	if _, err := b.c.MsgSend(ctx, lead, "", MsgSendRequest{Type: "state.worker.v1", Fields: map[string]any{"phase": "idle"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE workers SET last_seen = botaru_now() - interval '10 minutes' WHERE id = $1`, lead.ID); err != nil {
		t.Fatal(err)
	}
	b.loop(t, "worker-liveness")
	var stale bool
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT stale_since IS NOT NULL FROM workers WHERE id = $1`, lead.ID).Scan(&stale); err != nil || !stale {
		t.Fatalf("STA-2: a worker reporting idle but unseen is not stale: %v %v", stale, err)
	}
	task := b.hello(t, "1")
	if att := b.claim(t, b.agent, "attempt"); att == nil || att.WorkID != task {
		t.Fatalf("STA-2: the claim of another worker changed: %+v", att)
	}
}

// MSG-8: the model sees a message only as the output of inbox_read it called itself: the tool
// is in the attempt's catalog only when the type declares inbox.read, and the message text is
// not in the instruction, the goal or the attempt payload.
func TestBackfillMessageOnlyThroughTool(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	canary := "CANARY-" + ids.Secret(6)
	for _, reads := range []bool{true, false} {
		b := newMsgBenchWith(t, func(ms []map[string]any) []map[string]any {
			for _, m := range ms {
				if m["kind"] == "Role" && m["metadata"].(map[string]any)["name"] == "lead" {
					m["spec"].(map[string]any)["messages"].(map[string]any)["send"] = []any{map[string]any{"type": "report.work.v1", "to": []any{"role:hello"}}}
				}
				if m["kind"] == "Role" && m["metadata"].(map[string]any)["name"] == "hello" {
					m["spec"].(map[string]any)["messages"].(map[string]any)["receive"] = []any{map[string]any{"type": "report.work.v1"}}
				}
				if m["kind"] == "TaskType" && m["metadata"].(map[string]any)["name"] == "hello.greet.v1" && !reads {
					delete(m["spec"].(map[string]any), "inbox")
				}
			}
			return ms
		})
		lead := b.enroll(t, EnrollRequest{Tenant: "demo", Role: "lead", Kind: "job"})
		review := enqueueReview(t, b, "r1", "system")
		it := claimFor(t, b, lead, review)
		if _, err := b.c.MsgSend(ctx, lead, header(t, it), MsgSendRequest{Type: "report.work.v1", To: "role:hello", Fields: map[string]any{"outcome": "done"}, Note: canary}); err != nil {
			t.Fatal(err)
		}
		if _, err := b.c.Complete(ctx, lead, header(t, it), CompleteRequest{Result: map[string]any{"ok": true}}); err != nil {
			t.Fatal(err)
		}
		b.hello(t, "1")
		att := b.claim(t, b.agent, "attempt")
		raw, _ := json.Marshal(att.Payload)
		if strings.Contains(string(raw), canary) {
			t.Fatalf("MSG-8: the message is in the attempt payload: %s", raw)
		}
		h, _ := att.Payload["handles"].(map[string]any)["tools"].(string)
		names := toolNames(t, b, h)
		if slices.Contains(names, inboxReadName) != reads {
			t.Fatalf("MSG-8: inbox.read %v, tools %v", reads, names)
		}
		if !reads {
			continue
		}
		r, err := b.c.P3ToolsCall(ctx, h, mcp.CallParams{Name: inboxReadName, Arguments: map[string]any{"max": 5}})
		if err != nil || r.IsError || !strings.Contains(callText(r), canary) {
			t.Fatalf("MSG-8: inbox_read does not give the message: %+v %v", r, err)
		}
	}
}

// ORC-13: the worker directory of a manager is built from manifests: the role's may_enqueue
// filtered by the initiator's rights, each described by summary_for_managers from git; a type
// the initiator may not enqueue is not in it.
func TestBackfillWorkerDirectory(t *testing.T) {
	t.Parallel()
	b := newBenchWith(t, func(_ string, ms []map[string]any) []map[string]any {
		var types []map[string]any
		for _, m := range ms {
			s := m["spec"].(map[string]any)
			switch m["kind"] {
			case "TaskType":
				for name, summary := range map[string]string{"hello.child.v1": "Looks something up", "hello.other.v1": "Not for this initiator"} {
					ty := deepCopy(m)
					ty["metadata"].(map[string]any)["name"] = name
					ty["spec"].(map[string]any)["summary_for_managers"] = summary
					types = append(types, ty)
				}
			case "Role":
				s["profile"] = "manager"
				s["claims"] = []any{"hello.greet.v1", "hello.child.v1", "hello.other.v1"}
				s["may_enqueue"] = []any{"hello.child.v1", "hello.other.v1"}
			case "Group":
				s["caps"] = append(s["caps"].([]any), "task:hello.child.v1")
			}
		}
		return append(ms, types...)
	})
	b.hello(t, "1")
	att := b.claim(t, b.agent, "attempt")
	h, _ := att.Payload["handles"].(map[string]any)["tools"].(string)
	l, err := b.c.P3ToolsList(context.Background(), h)
	if err != nil {
		t.Fatal(err)
	}
	var tool map[string]any
	for _, x := range l.Tools {
		if m := x.(map[string]any); m["name"] == enqueueChildName {
			tool = m
		}
	}
	if tool == nil {
		t.Fatal("ORC-13: no enqueue_child")
	}
	desc, _ := tool["description"].(string)
	enum := tool["inputSchema"].(map[string]any)["properties"].(map[string]any)["type"].(map[string]any)["enum"]
	if !strings.Contains(desc, "hello.child.v1: Looks something up") || strings.Contains(desc, "hello.other.v1") || !slices.Equal(toStrings(enum), []string{"hello.child.v1"}) {
		t.Fatalf("ORC-13: the directory %q %v", desc, enum)
	}
}

// TEN-5: one attempt sees data of exactly one tenant: a canary in the goal of a task of tenant
// demo is not in the attempt of tenant demo2, nor in any row of demo2's tasks, sessions,
// facts, memory, messages, deliveries or files, and the worker of demo2 gets no work of demo.
func TestBackfillTenantsDoNotMix(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newBenchWith(t, func(_ string, ms []map[string]any) []map[string]any {
		var add []map[string]any
		for _, m := range ms {
			md := m["metadata"].(map[string]any)
			switch {
			case m["kind"] == "Tenant":
				c := deepCopy(m)
				c["metadata"] = map[string]any{"name": "demo2"}
				add = append(add, c)
			case md["tenant"] == "demo" && (m["kind"] == "Channel" || m["kind"] == "Group"):
				c := deepCopy(m)
				c["metadata"].(map[string]any)["tenant"] = "demo2"
				add = append(add, c)
			}
		}
		return append(ms, add...)
	})
	op2 := Operator{Principal: ids.PrincipalID(), Tenant: "demo2"}
	bob := ids.PrincipalID()
	if _, err := b.c.Principal(ctx, op2, PrincipalRequest{Op: "principal.add", Subject: bob, Identity: "telegram:2001"}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.c.Grant(ctx, op2, GrantRequest{Op: "grant", Subject: bob, Group: "hello"}); err != nil {
		t.Fatal(err)
	}
	b2 := &bench{c: b.c, op: op2, alice: bob}
	b2.agent = b2.enroll(t, EnrollRequest{Tenant: "demo2", Role: "hello", Kind: "agent"})
	b2.channel = b2.enroll(t, EnrollRequest{Tenant: "demo2", Kind: "channel", Target: "telegram"})
	canary := "CANARY-" + ids.Secret(6)
	submit := func(w *Worker, identity, chat, update, text string) {
		t.Helper()
		a := Activity{Kind: "message", Channel: "telegram", UpdateID: update, Chat: Chat{Kind: "direct", ExternalID: chat}, Text: text}
		a.Sender.Identity, a.Sender.Verified = identity, true
		if res, err := b.c.SubmitActivity(ctx, w, a); err != nil || res.Result != "task" {
			t.Fatalf("submit_activity: %+v %v", res, err)
		}
	}
	submit(b.channel, "telegram:1001", "1001", "1", "/hello "+canary)
	if w := b2.claim(t, b2.agent, "attempt"); w != nil {
		t.Fatalf("TEN-5: a worker of demo2 got work of demo: %s", w.WorkID)
	}
	own := b.claim(t, b.agent, "attempt")
	if raw, _ := json.Marshal(own.Payload); !strings.Contains(string(raw), canary) {
		t.Fatalf("the demo attempt lost its goal: %s", raw)
	}
	completeHello(t, b, own)
	submit(b2.channel, "telegram:2001", "2001", "1", "/hello hi")
	other := b2.claim(t, b2.agent, "attempt")
	if other == nil || other.Tenant != "demo2" {
		t.Fatalf("no demo2 attempt: %+v", other)
	}
	if raw, _ := json.Marshal(other.Payload); strings.Contains(string(raw), canary) {
		t.Fatalf("TEN-5: the demo2 attempt carries the canary of demo: %s", raw)
	}
	for _, table := range []string{"tasks", "sessions", "facts", "memory_items", "messages", "deliveries", "files"} {
		var n int
		if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM `+table+` x WHERE tenant = 'demo2' AND to_jsonb(x)::text LIKE '%' || $1 || '%'`, canary).Scan(&n); err != nil || n != 0 {
			t.Errorf("TEN-5: %d rows of %s in demo2 hold the canary of demo (%v)", n, table, err)
		}
	}
}
