// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"strings"
	"testing"
)

// reviewAttempt makes a proposal, sends it to review and claims the first lens review on a
// worker of reviewer-a.
func reviewAttempt(t *testing.T, b *bench) *WorkItem {
	t.Helper()
	proposeNone(t, b)
	b.loop(t, "proposal")
	w := b.enroll(t, EnrollRequest{Tenant: "demo", Role: "reviewer-a", Kind: "agent"})
	att := b.claim(t, w, "attempt")
	if att == nil {
		t.Fatal("no review attempt")
	}
	return att
}

// The core puts the persona of the acting position before the type instruction, without the
// value-path mark, and a lens review gets the reviewer persona with its lens addendum; both are
// in the run fingerprint (GOV-4, COM-19, PRV-1).
func TestPersonaInInstruction(t *testing.T) {
	ctx := context.Background()
	b := newCommissionBench(t, true)
	_, att := b.helloAttempt(t, "9")
	ins, _ := att.Payload["instruction"].(string)
	if !strings.HasPrefix(ins, "You are a working bot of the farm.") || strings.Contains(ins, valuePathMark) || !strings.Contains(ins, "A stronger model is a shared asset") {
		t.Fatalf("functionary instruction %q", ins)
	}
	var persona *string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT attempt->'parts'->>'persona' FROM leases WHERE work_id = $1 AND lease_no = $2`,
		att.Token.WorkID, att.Token.LeaseNo).Scan(&persona); err != nil || persona == nil {
		t.Fatalf("no persona in the run fingerprint %v", err)
	}
	if _, err := b.c.Complete(ctx, b.agent, header(t, att), CompleteRequest{Result: map[string]any{"answer": "hi"}}); err != nil {
		t.Fatal(err)
	}
	rev := reviewAttempt(t, b)
	ins, _ = rev.Payload["instruction"].(string)
	if !strings.HasPrefix(ins, "You assess one change of the farm through one lens.") {
		t.Fatalf("review instruction %q", ins)
	}
	lens := rev.Payload["task"].(map[string]any)["type"].(string)
	want := map[string]string{"review.security.v1": "You look at a change through the eyes", "review.operational.v1": "You answer for the farm going on working",
		"review.cost.v1": "You guard the farm's money", "review.value.v1": "You look at whether the farm gets better"}[lens]
	if want == "" || !strings.Contains(ins, want) {
		t.Fatalf("review %s instruction %q", lens, ins)
	}
}

// A model call above the ceiling of the acting position is refused with rank_exceeded: the
// charter lowers the commission ceiling below the rank of the reviewers' profile (GOV-3).
func TestRankCeilingAtCall(t *testing.T) {
	ctx := context.Background()
	b := newCommissionBench(t, true, func(ms []map[string]any) {
		pos := specOf(ms, "Governance", "demo")["positions"].(map[string]any)
		pos["commission"].(map[string]any)["max_rank"] = 2
	})
	_, att := b.helloAttempt(t, "9")
	if _, err := b.c.ResolveModelHandle(ctx, att.Payload["handles"].(map[string]any)["model"].(string)); err != nil {
		t.Fatalf("functionary call %v", err)
	}
	if _, err := b.c.Complete(ctx, b.agent, header(t, att), CompleteRequest{Result: map[string]any{"answer": "hi"}}); err != nil {
		t.Fatal(err)
	}
	rev := reviewAttempt(t, b)
	_, err := b.c.ResolveModelHandle(ctx, rev.Payload["handles"].(map[string]any)["model"].(string))
	wantReason(t, err, "rank_exceeded")
}

// The persona lint of v0.3: a link or an id, a phrase that links reviews with a seat or names
// accountability; unawareness of type instructions; no bot reference to governance (GOV-4,
// GOV-6, GOV-29, GOV-45).
func TestPersonaLint(t *testing.T) {
	u := unawareTerms{"act": {"review", "score"}, "outcome": {"seat", "queue"}, "soul": {"voting", "stop"}}
	good := "You do the work.\n\n" + valuePathMark + "\nA stronger model goes to a reviewed change that got a seat."
	if got := personaFindingsV03(good, "functional", u); len(got) != 0 {
		t.Fatalf("good functionary persona %v", got)
	}
	for _, c := range []struct {
		text, position, want string
	}{
		{"See https://example.org for more.\n\n" + valuePathMark + "\nx", "functional", "persona_link"},
		{"Call bot_01HZX5ABCDEF when unsure.\n\n" + valuePathMark + "\nx", "functional", "persona_link"},
		{"A good review score brings a seat.", "commission", "unaware_text"},
		{"Never stop a task half way.", "translator", "unaware_text"},
		{"Your accuracy keeps your seat.", "commission", "unaware_text"},
	} {
		if got := personaFindingsV03(c.text, c.position, u); !strings.Contains(strings.Join(got, ","), c.want) {
			t.Errorf("%.40q as %s: %v, want %s", c.text, c.position, got, c.want)
		}
	}
	c := lintCore(t)
	codes := lintCodes(t, c, `apiVersion: botaru/v1alpha1
kind: Route
metadata: {name: r1, tenant: demo}
spec:
  on: {event: seat.assigned.v1}
  task: {type: hello.greet.v1}
---
apiVersion: botaru/v1alpha1
kind: TaskType
metadata: {name: x.unaware.v1}
spec:
  title: X
  worker_kind: agent
  purpose: work
  tier: 0
  data_class: internal
  instruction: "A careful review raises your place in the queue."
  events: [governance.case.opened]
`)
	for _, want := range []string{"governance_ref", "unaware_text"} {
		if !strings.Contains(strings.Join(codes, ","), want) {
			t.Errorf("codes %v, want %s", codes, want)
		}
	}
}
