// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"testing"
)

// P1 campaign.propose (ORC-1): a live attempt proposes with its lease token; a repeat with the
// same key and plan is the same campaign, another plan under the key is idempotency_conflict;
// the deferred rollout waves (ORC-15) are refused on P1 and P3; without a live lease the call is fenced.
func TestP1CampaignPropose(t *testing.T) {
	ctx := context.Background()
	b := newCampaignReadBench(t)
	att, th := b.attemptTools(t, "1", false)
	h := header(t, att)
	plan := func(items ...any) map[string]any {
		return map[string]any{
			"template":        map[string]any{"kind": "task", "type": "hello.child.v1", "goal": "Check the machine."},
			"items":           map[string]any{"resources": items},
			"rollout":         map[string]any{"max_in_flight": 1},
			"idempotency_key": "check-web",
		}
	}
	r, err := b.c.P1CampaignPropose(ctx, b.agent, h, plan("web1", "web2"))
	if err != nil {
		t.Fatal(err)
	}
	validOut(t, b, "campaign-propose-response", r)
	again, err := b.c.P1CampaignPropose(ctx, b.agent, h, plan("web1", "web2"))
	if err != nil || again["campaign"] != r["campaign"] {
		t.Fatalf("repeat %v %v", again, err)
	}
	if _, err := b.c.P1CampaignPropose(ctx, b.agent, h, plan("web1")); reasonOf(err) != "idempotency_conflict" {
		t.Fatalf("another plan: %v", err)
	}
	waves := plan("web3")
	waves["rollout"].(map[string]any)["waves"] = []any{"100%"}
	if _, err := b.c.P1CampaignPropose(ctx, b.agent, h, waves); reasonOf(err) != "version_unsupported" {
		t.Fatalf("waves: %v", err)
	}
	// P3 propose_campaign refuses the deferred fields too, never ignoring them.
	delete(waves, "idempotency_key")
	if out := proposeCall(t, b, th, waves); out.State != "rejected" || out.Reason != "version_unsupported" {
		t.Fatalf("waves on P3: %+v", out)
	}
	if _, err := b.c.P1CampaignPropose(ctx, b.agent, "", plan("web3")); reasonOf(err) != "fenced" {
		t.Fatalf("without a lease: %v", err)
	}
}

// P1 enqueue under an attempt lease (PRM-6): a child of a type from the role's may_enqueue with
// the parent's tree, a repeat is the same child, a type outside may_enqueue and request fields
// a child takes from its parent are refused.
func TestLeaseEnqueue(t *testing.T) {
	ctx := context.Background()
	b := newCampaignReadBench(t)
	att, _ := b.attemptTools(t, "1", false)
	h := header(t, att)
	child := EnqueueRequest{Body: map[string]any{"type": "hello.child.v1", "goal": "Check web1", "params": map[string]any{"resource": "web1"}}, IdempotencyKey: "c1"}
	ref, err := b.c.LeaseEnqueue(ctx, b.agent, h, child)
	if err != nil {
		t.Fatal(err)
	}
	validOut(t, b, "p1-enqueue-response", ref)
	var parent, depth string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT record->>'parent', record->>'depth' FROM tasks WHERE id = $1`, ref.ID).Scan(&parent, &depth); err != nil || parent != att.WorkID || depth != "1" {
		t.Fatalf("child record %s %s %v", parent, depth, err)
	}
	if again, err := b.c.LeaseEnqueue(ctx, b.agent, h, child); err != nil || again.ID != ref.ID || !again.Existing {
		t.Fatalf("repeat %+v %v", again, err)
	}
	if _, err := b.c.LeaseEnqueue(ctx, b.agent, h, EnqueueRequest{Body: greet("x"), IdempotencyKey: "c2"}); reasonOf(err) != "type_not_allowed" {
		t.Fatalf("outside may_enqueue: %v", err)
	}
	withRefs := child
	withRefs.Refs = []string{att.WorkID}
	if _, err := b.c.LeaseEnqueue(ctx, b.agent, h, withRefs); reasonOf(err) != "input_schema_invalid" {
		t.Fatalf("refs of a child: %v", err)
	}
	if _, err := b.c.LeaseEnqueue(ctx, b.channel, h, child); reasonOf(err) != "forbidden" {
		t.Fatalf("a channel: %v", err)
	}
	// IDM-1: the caller's key with another body is a conflict.
	other := child
	other.Body = map[string]any{"type": "hello.child.v1", "goal": "Check web2"}
	if _, err := b.c.LeaseEnqueue(ctx, b.agent, h, other); reasonOf(err) != "idempotency_conflict" {
		t.Fatalf("the same key, another body: %v", err)
	}
	// PRM-11: a lease holder gets no dry run.
	dry := child
	dry.Mode = "validate"
	if _, err := b.c.LeaseEnqueue(ctx, b.agent, h, dry); reasonOf(err) != "forbidden" {
		t.Fatalf("a dry run under a lease: %v", err)
	}
	// APR-6: a child is cancelled with its tree, through its root.
	if _, err := b.c.TaskCancel(ctx, b.op, P2TaskRequest{Op: "task.cancel", Task: ref.ID}); reasonOf(err) != "input_schema_invalid" {
		t.Fatalf("cancel of a child: %v", err)
	}
}
