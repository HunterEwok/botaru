// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"testing"
)

// Tests of attempt leases: one lease per profile and the stages of a heartbeat.

// A runtime with model_attribution per_profile holds one attempt lease at a time: while its
// lease is live it gets no second attempt, so the profile's handle and usage belong to that
// lease (RT-3).
func TestPerProfileOneLease(t *testing.T) {
	ctx := context.Background()
	b := newBench(t)
	r, err := b.c.Enroll(ctx, b.op, EnrollRequest{Tenant: "demo", Role: "hello", Kind: "agent"})
	if err != nil {
		t.Fatal(err)
	}
	w, err := b.c.AuthenticateWorker(ctx, r.Token)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.c.Hello(ctx, w, HelloRequest{Kind: "agent", ProtocolVersions: []string{"v1"}, Capacity: 1,
		Runtime: &RuntimeDecl{Session: "farm", Memory: "farm", ModelAttribution: "per_profile"}}); err != nil {
		t.Fatal(err)
	}
	if w, err = b.c.AuthenticateWorker(ctx, r.Token); err != nil {
		t.Fatal(err)
	}
	b.agent = w
	b.helloAttempt(t, "1")
	b.hello(t, "2")
	b.drain(t)
	if item := b.claim(t, w, "attempt"); item != nil {
		t.Fatalf("a second attempt under a live lease: %+v", item)
	}
}

// DLV-11: progress is only a stage from the closed list of the type's stages: a heartbeat with
// another stage is refused, one from the list extends the lease.
func TestHeartbeatStageFromList(t *testing.T) {
	ctx := context.Background()
	b := newBenchWith(t, func(_ string, ms []map[string]any) []map[string]any {
		for _, m := range ms {
			if m["kind"] == "TaskType" {
				m["spec"].(map[string]any)["stages"] = []any{"reading_input", "writing_answer"}
			}
		}
		return ms
	})
	task := b.hello(t, "1")
	att := claimFor(t, b, b.agent, task)
	h := header(t, att)
	p := 50.0
	if _, err := b.c.Heartbeat(ctx, b.agent, h, HeartbeatRequest{Stage: "writing_answer", Progress: &p}); err != nil {
		t.Fatalf("DLV-11: a stage of the list %v", err)
	}
	_, err := b.c.Heartbeat(ctx, b.agent, h, HeartbeatRequest{Stage: "Here is the first half of the answer", Progress: &p})
	wantReason(t, err, "input_schema_invalid")
	if _, err := b.c.Heartbeat(ctx, b.agent, h, HeartbeatRequest{}); err != nil {
		t.Fatalf("DLV-11: a heartbeat without a stage %v", err)
	}
	// A type without stages has no stage to show: any stage is refused, a percentage passes.
	b2 := newBench(t)
	h2 := header(t, claimFor(t, b2, b2.agent, b2.hello(t, "1")))
	_, err = b2.c.Heartbeat(ctx, b2.agent, h2, HeartbeatRequest{Stage: "writing_answer", Progress: &p})
	wantReason(t, err, "input_schema_invalid")
	if _, err := b2.c.Heartbeat(ctx, b2.agent, h2, HeartbeatRequest{Progress: &p}); err != nil {
		t.Fatalf("DLV-11: a percentage without a stage %v", err)
	}
}
