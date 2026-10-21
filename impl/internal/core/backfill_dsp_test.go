// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HunterEwok/botaru/impl/internal/spec"
)

// A worker gets work only through P1 claim: of the operations of the ports a worker calls, only
// claim answers with a work item, and an enqueued task waits in the queue without a lease until
// a worker claims it (DSP-1).
func TestBackfillWorkOnlyByClaim(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root, err := spec.FindRoot("")
	if err != nil {
		t.Fatal(err)
	}
	b := newBench(t)
	reg, err := b.c.spec.Registry("spec/ports/operations.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var handing []string
	for _, e := range reg.Entries {
		id, _ := e["id"].(string)
		port, _ := e["port"].(string)
		resp, _ := e["response"].(string)
		if resp == "" || (port != "P1" && port != "P3" && port != "P4") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(root, "spec/model", resp+".schema.json"))
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		// DSP-1: a work item, with its lease, is in no other answer.
		if strings.Contains(string(raw), "urn:botaru:schema:work-item:1") {
			handing = append(handing, id)
		}
	}
	if len(handing) != 1 || handing[0] != "P1.claim" {
		t.Fatalf("operations that hand out work: %v", handing)
	}
	task := b.hello(t, "1")
	// DSP-1: the task is in the tenant's queue, with no lease of any worker.
	if n := b.count(t, `SELECT count(*) FROM tasks WHERE id = $1 AND state = 'submitted'`, task); n != 1 {
		t.Fatal("the task is not queued")
	}
	if n := b.count(t, `SELECT count(*) FROM leases WHERE work_id = $1`, task); n != 0 {
		t.Fatal("the task got a lease without a claim")
	}
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE contacts SET ai_notice_at = now()`); err != nil {
		t.Fatal(err)
	}
	b.drain(t)
	att := claimFor(t, b, b.agent, task)
	if n := b.count(t, `SELECT count(*) FROM leases WHERE work_id = $1 AND worker_id = $2 AND state = 'active'`, task, b.agent.ID); n != 1 || att.LeaseNo != 1 {
		t.Fatalf("leases after the claim %d, lease_no %d", n, att.LeaseNo)
	}
	validOut(t, b, "work-item", att)
}

// The priority classes: the type sets the class, a request only lowers it, class 2 is only a
// person's direct request of an interactive type, class 1 comes from no request or type, and a
// higher class is handed out before any lower one (DSP-5).
func TestBackfillPriorityClasses(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newPeopleBench(t, func(ms []map[string]any) {
		for _, m := range ms {
			if m["kind"] == "TaskType" && m["metadata"].(map[string]any)["name"] == "hello.greet.v1" {
				m["spec"].(map[string]any)["interactive"] = true
			}
		}
	})
	prio := func(id string) int {
		t.Helper()
		return b.count(t, `SELECT priority_class FROM tasks WHERE id = $1`, id)
	}
	// DSP-5: class 1 is in no request or type schema.
	if err := b.c.spec.Validate("enqueue-request", map[string]any{"body": greet("x"), "idempotency_key": "k", "priority": 1}); err == nil {
		t.Fatal("a request of class 1 passed the schema")
	}
	if err := b.c.spec.Validate("enqueue-request", map[string]any{"body": greet("x"), "idempotency_key": "k", "priority": 2}); err == nil {
		t.Fatal("a request of class 2 passed the schema")
	}
	low, err := b.c.ProgramEnqueue(ctx, b.prog, EnqueueRequest{Body: greet("later"), IdempotencyKey: "k-low", Priority: 5})
	if err != nil {
		t.Fatal(err)
	}
	prog, err := b.c.ProgramEnqueue(ctx, b.prog, EnqueueRequest{Body: greet("now"), IdempotencyKey: "k-prog"})
	if err != nil {
		t.Fatal(err)
	}
	person := b.aliceTask(t, "1")
	// DSP-5: a person's direct request of an interactive type is class 2; a program's request
	// takes the type's class, and its own priority only lowers it.
	if got := prio(person); got != 2 {
		t.Errorf("a person's direct request: class %d", got)
	}
	if got := prio(prog.ID); got != 3 {
		t.Errorf("a program's request: class %d", got)
	}
	if got := prio(low.ID); got != 5 {
		t.Errorf("a lowered request: class %d", got)
	}
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE contacts SET ai_notice_at = now()`); err != nil {
		t.Fatal(err)
	}
	b.drain(t)
	// DSP-5: strict classes, the oldest of class 5 waits for the newer ones of classes 2 and 3.
	var order []string
	for range 3 {
		w := b.claim(t, b.agent, "attempt")
		if w == nil {
			t.Fatal("no attempt")
		}
		order = append(order, w.WorkID)
	}
	if order[0] != person || order[1] != prog.ID || order[2] != low.ID {
		t.Fatalf("hand-out order %v, want %s %s %s", order, person, prog.ID, low.ID)
	}
}
