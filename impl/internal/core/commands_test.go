// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"strings"
	"testing"
)

// The commands of an initiator answered by code (PRM-17): /tasks, /status, /cancel with its
// confirmation, which cancels the tree and its waiting approval with the reason initiator,
// /help, /me, /lang and /team.
func TestPersonCommands(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newServiceBench(t)
	if res := b.say(t, "1001", "30", "/tasks"); !strings.Contains(res.Text, "no tasks") {
		t.Fatalf("/tasks with none %+v", res)
	}
	att, h := b.attemptTools(t, "31", false)
	r := b.request(t, h, map[string]any{"action": "fix_web", "args": map[string]any{"host": "web1"}})
	if r.Code != "pending" {
		t.Fatalf("request %+v", r)
	}
	if res := b.say(t, "1001", "32", "/tasks"); !strings.Contains(res.Text, "1. Greeting: working") {
		t.Fatalf("/tasks %q", res.Text)
	}
	if res := b.say(t, "1001", "33", "/status 1"); !strings.Contains(res.Text, `Task 1 "Greeting": working`) {
		t.Fatalf("/status %q", res.Text)
	}
	if res := b.say(t, "1001", "34", "/status 7"); !strings.Contains(res.Text, "no task 7") {
		t.Fatalf("/status of another number %q", res.Text)
	}
	// Another person has no task 1.
	if res := b.say(t, "1002", "35", "/cancel 1"); !strings.Contains(res.Text, "no task 1") {
		t.Fatalf("/cancel of a foreign task %q", res.Text)
	}
	res := b.say(t, "1001", "36", "/cancel 1")
	i := strings.Index(res.Text, "/cancel 1 ")
	if i < 0 {
		t.Fatalf("/cancel asks %q", res.Text)
	}
	nonce := strings.TrimSuffix(strings.Fields(res.Text[i:])[2], ".")
	if res := b.say(t, "1001", "37", "/cancel 1 WRONG1"); !strings.Contains(res.Text, "To confirm") {
		t.Fatalf("a wrong nonce %q", res.Text)
	}
	if res := b.say(t, "1001", "38", "/cancel 1 "+nonce); !strings.Contains(res.Text, "canceled") {
		t.Fatalf("/cancel confirmed %q", res.Text)
	}
	var state, reason string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT state, reason FROM tasks WHERE id = $1`, att.WorkID).Scan(&state, &reason); err != nil || state != "canceled" || reason != "initiator" {
		t.Fatalf("task %s %s %v", state, reason, err)
	}
	a := b.approval(t, r.ApprovalID)
	if a.State != "canceled" || a.Reason != "initiator" {
		t.Fatalf("approval %s %s", a.State, a.Reason)
	}
	validOut(t, b.bench, "approval", a.Record())
	if res := b.say(t, "1001", "39", "/cancel 1"); !strings.Contains(res.Text, "already finished") {
		t.Fatalf("/cancel of a finished task %q", res.Text)
	}

	if res := b.say(t, "1001", "40", "/help"); !strings.Contains(res.Text, "/hello: Greeting") || !strings.Contains(res.Text, "/run clean: Clean temporary files") {
		t.Fatalf("/help %q", res.Text)
	}
	if res := b.say(t, "1001", "41", "/me"); !strings.Contains(res.Text, "operators") || !strings.Contains(res.Text, "Greeting") {
		t.Fatalf("/me %q", res.Text)
	}
	if res := b.say(t, "1001", "42", "/lang xx"); !strings.Contains(res.Text, "Available: en") {
		t.Fatalf("/lang xx %q", res.Text)
	}
	if res := b.say(t, "1001", "43", "/lang en"); !strings.Contains(res.Text, "now en") {
		t.Fatalf("/lang en %q", res.Text)
	}

	// /team needs team.view:<group>; the rows are by type, never by person.
	if res := b.say(t, "1002", "44", "/team requesters"); !strings.Contains(res.Text, "needs the group") {
		t.Fatalf("/team without the right %q", res.Text)
	}
	if _, err := b.c.st.Pool.Exec(ctx, `INSERT INTO memberships (tenant, principal, grp, state, created_at) VALUES ('demo', $1, 'leads', 'active', now())`, b.bob); err != nil {
		t.Fatal(err)
	}
	res = b.say(t, "1002", "45", "/team requesters 2d")
	if !strings.Contains(res.Text, "Group requesters, last 2d") || !strings.Contains(res.Text, "Greeting: done 0, failed 0, waiting 0") || strings.Contains(res.Text, b.alice) {
		t.Fatalf("/team %q", res.Text)
	}
}
