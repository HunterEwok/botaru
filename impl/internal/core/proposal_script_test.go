// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"strings"
	"testing"
)

// scriptProposalEdit gives the hello role scripts with propose: in_proposal over its own
// greeting, which gets a goal template.
func scriptProposalEdit(ms []map[string]any) {
	for _, m := range ms {
		s := m["spec"].(map[string]any)
		name, _ := m["metadata"].(map[string]any)["name"].(string)
		switch {
		case m["kind"] == "Role" && name == "hello":
			s["scripts"] = map[string]any{"propose": "in_proposal", "step_types": []any{"hello.greet.v1"}}
		case m["kind"] == "TaskType" && name == "hello.greet.v1":
			s["goal_templates"] = []any{map[string]any{"name": "greet", "goal": "Greet the farm."}}
		}
	}
}

func scriptChange(steps ...map[string]any) map[string]any {
	list := make([]any, len(steps))
	for i, s := range steps {
		list[i] = s
	}
	return map[string]any{"kind": "script", "script": map[string]any{"script": "v1", "steps": list}}
}

// A proposal with a script change (PRP-3): the draft is screened within the author role's
// scripts with propose: in_proposal, a step outside the vocabulary or a goal taken from an
// assignment is a rejection; "Accept" puts the draft through the SCR-5 acceptance on behalf of
// the person and sends the SCR-6 card; the completed run realizes the proposal.
func TestProposalScriptChange(t *testing.T) {
	ctx := context.Background()
	b := newCommissionBench(t, true, presentEdit, scriptProposalEdit)
	b.helloDone(t, "1", "", "hi")
	b.loop(t, "observe")
	greet := map[string]any{"id": "greet", "kind": "task", "type": "hello.greet.v1", "goal": map[string]any{"template": "greet"}}
	outside := map[string]any{"id": "look", "kind": "task", "type": "hello.insight.v1", "goal": map[string]any{"template": "greet"}}
	free := map[string]any{"id": "greet", "kind": "task", "type": "hello.greet.v1", "goal": map[string]any{"assignment": true}}
	bodies := []any{changeBody("slow", scriptChange(outside)), changeBody("slow", scriptChange(free)), changeBody("slow", scriptChange(greet))}
	if _, err := b.analysis(t, "1", false, map[string]any{"proposals": bodies}); err != nil {
		t.Fatal(err)
	}
	if got := rejections(t, b); len(got) != 2 || got[0] != "forbidden_change" || got[1] != "forbidden_change" {
		t.Fatalf("rejections %v", got)
	}
	b.loop(t, "proposal")
	b.reviewAll(t, 4)
	b.loop(t, "proposal")
	if state, rec := proposalState(t, b); state != "presented" {
		t.Fatalf("state %s %v", state, rec["state_reason"])
	}
	card := cardOf(t, b.drain(t), proposalCard, "direct:1001")
	id, nonce := b.proposalNonce(t)
	r, err := b.c.Decide(ctx, b.channel, press("telegram:1001", "direct:1001", card.external), DecideRequest{SubjectKind: "proposal", ID: id, Nonce: nonce, Decision: "accept"})
	if err != nil || r.State != "realizing" {
		t.Fatalf("accept %+v %v", r, err)
	}
	_, rec := proposalState(t, b)
	if err := b.c.spec.Validate("urn:botaru:schema:proposal-record:1", rec); err != nil {
		t.Fatalf("record: %v", err)
	}
	runID := rec["realization"].(map[string]any)["object"].(string)
	if !strings.HasPrefix(runID, "scr_") {
		t.Fatalf("realization %v", rec["realization"])
	}
	run, states := b.run(t, runID)
	if run.State != "proposed" || run.Initiator != b.alice || states != "pending" {
		t.Fatalf("run %s %s %s", run.State, run.Initiator, states)
	}
	// The run card goes to the person who accepted; the run is not started without "Run".
	sc := cardOf(t, b.drain(t), scriptConfirmCard, "direct:1001")
	b.loop(t, "proposal")
	if state, _ := proposalState(t, b); state != "realizing" {
		t.Fatalf("before the run %s", state)
	}
	if res, err := b.c.Decide(ctx, b.channel, press("telegram:1001", "direct:1001", sc.external),
		DecideRequest{SubjectKind: "script", ID: run.ID, Nonce: run.Nonce, Decision: "approve"}); err != nil || res.State != "running" {
		t.Fatalf("run %+v %v", res, err)
	}
	item := b.claim(t, b.agent, "attempt")
	if item == nil {
		t.Fatal("no greeting attempt")
	}
	if _, err := b.c.Complete(ctx, b.agent, header(t, item), CompleteRequest{Result: map[string]any{"answer": "hello"}}); err != nil {
		t.Fatal(err)
	}
	if run, _ = b.run(t, run.ID); run.State != "completed" {
		t.Fatalf("run %s", run.State)
	}
	b.loop(t, "proposal")
	state, rec := proposalState(t, b)
	if state != "realized" || rec["realization"].(map[string]any)["link"] != "object" {
		t.Fatalf("after the run %s %v", state, rec["realization"])
	}
}
