// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// countTasks is the number of task rows of every type, the reporter's included.
func countTasks(t *testing.T, b *bench) int {
	t.Helper()
	var n int
	if err := b.c.st.Pool.QueryRow(context.Background(), `SELECT count(*) FROM tasks`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// REP-12: the channel commands are answered by code with texts of the catalog, without a
// model and without a task of any type; in a group chat only /help and /about answer, every
// other command gets cmd.direct_only.
func TestBackfillCodeCommands(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newGroupBench(t)
	bind := ChatBindRequest{Op: "chat.bind", Tenant: "demo", Channel: "telegram", Chat: Chat{Kind: "group", ExternalID: "-500"}, Ceiling: "summary", Name: "lab"}
	if _, err := b.c.ChatBind(ctx, b.op, bind); err != nil {
		t.Fatal(err)
	}
	before := countTasks(t, b.bench)
	about := b.c.texts.Render("en", "cmd.about.text", map[string]any{"tenant": "demo"})
	direct := b.c.texts.Render("en", "cmd.direct_only", nil)
	update := 100
	next := func() string { update++; return fmt.Sprint(update) }
	for _, cmd := range []string{"/tasks", "/status 1", "/cancel 1", "/team lab", "/lang en"} {
		if r := b.inGroup(t, next(), "telegram:1001", cmd); r.Result != "reply" || r.Text != direct {
			t.Errorf("REP-12: %s in a group: %+v", cmd, r)
		}
	}
	if r := b.inGroup(t, next(), "telegram:1001", "/about"); r.Result != "reply" || r.Text != about {
		t.Errorf("REP-12: /about in a group: %+v", r)
	}
	if r := b.inGroup(t, next(), "telegram:1001", "/help"); r.Result != "reply" || !strings.Contains(r.Text, "Greeting") {
		t.Errorf("REP-12: /help in a group: %+v", r)
	}
	for _, cmd := range []string{"/tasks", "/status 1", "/help", "/about", "/lang en"} {
		r := b.say(t, next(), "telegram:1001", "1001", cmd)
		if r.Result != "reply" || r.Text == "" || r.Text == direct {
			t.Errorf("REP-12: %s in a private chat: %+v", cmd, r)
		}
		if cmd == "/about" && r.Text != about {
			t.Errorf("REP-12: /about is not the catalog text: %q", r.Text)
		}
	}
	if after := countTasks(t, b.bench); after != before {
		t.Fatalf("REP-12: the commands created %d tasks", after-before)
	}
	if w := b.claim(t, b.agent, "attempt"); w != nil {
		t.Fatalf("REP-12: a command gave the model an attempt: %s", w.WorkID)
	}
}

// REP-5: the core delivers principal_left itself, under a stop of the tenant's work and
// without a reporter task, only to private chats of admins with admin:people, never to the
// bound group chat where the person was seen.
func TestBackfillNoticeBypassesReporter(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newGroupBench(t)
	bind := ChatBindRequest{Op: "chat.bind", Tenant: "demo", Channel: "telegram", Chat: Chat{Kind: "group", ExternalID: "-500"}, Ceiling: "summary", Name: "lab"}
	if _, err := b.c.ChatBind(ctx, b.op, bind); err != nil {
		t.Fatal(err)
	}
	// alice is seen in the group chat.
	if r := b.inGroup(t, "1", "telegram:1001", "/help"); r.Result != "reply" {
		t.Fatalf("help %+v", r)
	}
	farmOp := OperatorActor(Operator{Principal: b.op.Principal, Tenant: "farm"})
	if _, err := b.c.HoldSet(ctx, farmOp, HoldSetRequest{Scope: HoldScope{Kind: "tenant", Tenant: "demo"}, Effects: []string{"no_enqueue", "no_claim"}, Reason: "manual"}); err != nil {
		t.Fatal(err)
	}
	before := countTasks(t, b.bench)
	if _, err := b.c.Principal(ctx, b.op, PrincipalRequest{Op: "principal.offboard", Subject: b.alice}); err != nil {
		t.Fatal(err)
	}
	rows, err := b.c.st.Pool.Query(ctx, `SELECT target_id, recipient FROM deliveries WHERE kind = 'principal_left'`)
	if err != nil {
		t.Fatal(err)
	}
	var targets []string
	for rows.Next() {
		var target, recipient string
		if err := rows.Scan(&target, &recipient); err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(target, "direct:") || recipient != b.bob {
			t.Errorf("REP-5: principal_left to %s for %s", target, recipient)
		}
		targets = append(targets, target)
	}
	if rows.Err() != nil || len(targets) != 1 || targets[0] != "direct:1002" {
		t.Fatalf("REP-5: principal_left deliveries %v %v", targets, rows.Err())
	}
	if after := countTasks(t, b.bench); after != before {
		t.Fatalf("REP-5: the notice made %d tasks", after-before)
	}
}

// REP-6: a decision comes only from the channel that delivered the card: a model worker (the
// bench's agent, or a second agent worker of the tenant) that calls decide with the very press
// of the approver is refused and the approval waits; no built-in tool of the reporter profile
// decides, draws a card or picks the cards of a digest, so a reporter has no path to decide.
func TestBackfillReporterDecidesNothing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newActionBench(t, nil)
	att, h := b.attemptTools(t, "1", false)
	r := b.request(t, h, map[string]any{"action": "fix_web", "args": map[string]any{"host": "web1"}})
	if r.Code != "pending" {
		t.Fatalf("request %+v", r)
	}
	b.finish(t, att)
	b.tick(t, "approval-expiry")
	card := cardOf(t, b.drain(t), approvalCard, "direct:1002")
	a := b.approval(t, r.ApprovalID)
	req := DecideRequest{SubjectKind: "approval", ID: a.ID, Nonce: a.Nonce, Decision: "approve"}
	second := b.enroll(t, EnrollRequest{Tenant: "demo", Role: "hello", Kind: "agent"})
	for _, w := range []*Worker{b.agent, second} {
		_, err := b.c.Decide(ctx, w, press("telegram:1002", "direct:1002", card.external), req)
		wantReason(t, err, "forbidden")
	}
	if a := b.approval(t, r.ApprovalID); a.State != "pending" {
		t.Fatalf("REP-6: a model worker decided: %s", a.State)
	}
	if _, err := b.c.Decide(ctx, b.channel, press("telegram:1002", "direct:1002", card.external), req); err != nil {
		t.Fatalf("REP-6: the press from the card's channel: %v", err)
	}
	reg, err := b.c.spec.Registry("spec/model/builtin-tools.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var seen int
	for _, e := range reg.Entries {
		id := asString(e["id"])
		if !slices.Contains(toStrings(e["profiles"]), "reporter") {
			continue
		}
		seen++
		for _, w := range []string{"decide", "approve", "card", "digest"} {
			if strings.Contains(id, w) {
				t.Errorf("REP-6: the built-in tool %s", id)
			}
		}
	}
	if seen == 0 {
		t.Fatal("REP-6: no built-in tool of the reporter profile")
	}
}
