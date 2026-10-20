// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"errors"
	"testing"
	"time"
)

// peopleBench is the hello bench with a privileged group staff (audit.read), a lead group
// leads (team.view:hello, task.read:hello.greet.v1) and the external program report-client
// (service:report-client, identity api:report-client) in group hello.
type peopleBench struct {
	*bench
	prog *Program
}

func peopleGroups(ms []map[string]any) []map[string]any {
	group := func(name string, caps []any) map[string]any {
		return map[string]any{"apiVersion": "botaru/v1alpha1", "kind": "Group", "metadata": map[string]any{"name": name, "tenant": "demo"},
			"spec": map[string]any{"caps": caps, "data": []any{"summary"}}}
	}
	return append(ms, group("staff", []any{"audit.read"}), group("leads", []any{"team.view:hello", "task.read:hello.greet.v1"}))
}

func newPeopleBench(t *testing.T, edit func(ms []map[string]any)) *peopleBench {
	t.Helper()
	ctx := context.Background()
	b := newBenchWith(t, func(_ string, ms []map[string]any) []map[string]any {
		ms = peopleGroups(ms)
		if edit != nil {
			edit(ms)
		}
		return ms
	})
	if _, err := b.c.Principal(ctx, b.op, PrincipalRequest{Op: "principal.add", Subject: "service:report-client", Kind: "service", Identity: "api:report-client"}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.c.Grant(ctx, b.op, GrantRequest{Op: "grant", Subject: "service:report-client", Group: "hello"}); err != nil {
		t.Fatal(err)
	}
	p, err := b.c.AuthenticateProgram(ctx, "demo", "report-client")
	if err != nil {
		t.Fatal(err)
	}
	return &peopleBench{bench: b, prog: p}
}

// aliceTask is a task of alice from her private chat.
func (b *peopleBench) aliceTask(t *testing.T, update string) string {
	t.Helper()
	a := Activity{Kind: "message", Channel: "telegram", UpdateID: update, Chat: Chat{Kind: "direct", ExternalID: "1001"}, Text: "/hello hi"}
	a.Sender.Identity, a.Sender.Verified = "telegram:1001", true
	res, err := b.c.SubmitActivity(context.Background(), b.channel, a)
	if err != nil || res.Task == "" {
		t.Fatalf("submit %+v %v", res, err)
	}
	return res.Task
}

func greet(goal string) map[string]any { return map[string]any{"type": "hello.greet.v1", "goal": goal} }

// P1 enqueue of an external program (TEN-2, IDM-1, TEN-7, BUD-1, APR-5, PRM-11): the tenant and
// the initiator come from the key, a repeat with the same body returns the task, another body
// is idempotency_conflict, a foreign ref is ref_invalid and takes no key, the estimate is
// reserved, action types and the deferred dry run are refused.
func TestProgramEnqueue(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newPeopleBench(t, nil)
	// TEN-2: a body or request with a tenant is refused by the schema.
	if err := b.c.spec.Validate("enqueue-request", map[string]any{"body": greet("x"), "idempotency_key": "k", "tenant": "demo2"}); err == nil {
		t.Fatal("a request with a tenant passed the schema")
	}
	if err := b.c.spec.Validate("enqueue-request", map[string]any{"body": map[string]any{"type": "hello.greet.v1", "goal": "x", "tenant": "demo2"}, "idempotency_key": "k"}); err == nil {
		t.Fatal("a body with a tenant passed the schema")
	}
	if _, err := b.c.AuthenticateProgram(ctx, "demo", "nobody"); reasonOf(err) != "unauthenticated" {
		t.Fatalf("unknown key id: %v", err)
	}
	ref, err := b.c.ProgramEnqueue(ctx, b.prog, EnqueueRequest{Body: greet("nightly report"), IdempotencyKey: "nightly-2026-10-05"})
	if err != nil || ref.Existing {
		t.Fatalf("enqueue %+v %v", ref, err)
	}
	validOut(t, b.bench, "p1-enqueue-response", ref)
	var initiator, tenant, fair string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT initiator, tenant, record->>'fairness_key' FROM tasks WHERE id = $1`, ref.ID).Scan(&initiator, &tenant, &fair); err != nil {
		t.Fatal(err)
	}
	if initiator != "service:report-client" || tenant != "demo" || fair != "interactive:service:report-client" {
		t.Fatalf("record %s %s %s", initiator, tenant, fair)
	}
	if got := taskReserve(t, b.bench, ref.ID); got <= 0 {
		t.Fatalf("no reserve at enqueue (BUD-1): %d", got)
	}
	again, err := b.c.ProgramEnqueue(ctx, b.prog, EnqueueRequest{Body: greet("nightly report"), IdempotencyKey: "nightly-2026-10-05"})
	if err != nil || again.ID != ref.ID || !again.Existing {
		t.Fatalf("repeat %+v %v", again, err)
	}
	if _, err := b.c.ProgramEnqueue(ctx, b.prog, EnqueueRequest{Body: greet("other"), IdempotencyKey: "nightly-2026-10-05"}); reasonOf(err) != "idempotency_conflict" {
		t.Fatalf("another body: %v", err)
	}
	// TEN-7: alice's task is not a reference the program may carry; the key stays free.
	alice := b.aliceTask(t, "1")
	if _, err := b.c.ProgramEnqueue(ctx, b.prog, EnqueueRequest{Body: greet("r"), IdempotencyKey: "k-ref", Refs: []string{alice}}); reasonOf(err) != "ref_invalid" {
		t.Fatalf("foreign ref: %v", err)
	}
	if _, err := b.c.ProgramEnqueue(ctx, b.prog, EnqueueRequest{Body: greet("r"), IdempotencyKey: "k-ref", Refs: []string{"t_01JB0000000000000000000099"}}); reasonOf(err) != "ref_invalid" {
		t.Fatalf("missing ref: %v", err)
	}
	var denied int
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM audit WHERE tenant = 'demo' AND record->>'command' = 'task.enqueue'
		AND record->>'reason' = 'ref_invalid'`).Scan(&denied); err != nil || denied != 2 {
		t.Fatalf("refusals in the audit: %d %v", denied, err)
	}
	own, err := b.c.ProgramEnqueue(ctx, b.prog, EnqueueRequest{Body: greet("r"), IdempotencyKey: "k-ref", Refs: []string{ref.ID}})
	if err != nil || own.Existing {
		t.Fatalf("own ref with the free key: %+v %v", own, err)
	}
	// APR-5: an action type is never enqueued by a sender.
	if _, err := b.c.ProgramEnqueue(ctx, b.prog, EnqueueRequest{Body: map[string]any{"type": "action.service.control", "goal": "x"}, IdempotencyKey: "k-act"}); reasonOf(err) != "type_not_allowed" {
		t.Fatalf("action type: %v", err)
	}
	// PRM-11 is deferred: the dry run is refused, never served.
	if _, err := b.c.ProgramEnqueue(ctx, b.prog, EnqueueRequest{Body: greet("x"), IdempotencyKey: "k-dry", Mode: "validate"}); reasonOf(err) != "version_unsupported" {
		t.Fatalf("dry run: %v", err)
	}
	// DSP-4: a program addresses no bot group.
	req := EnqueueRequest{Body: greet("x"), IdempotencyKey: "k-to"}
	req.To = &struct {
		Group string `json:"group"`
	}{Group: "hello"}
	if _, err := b.c.ProgramEnqueue(ctx, b.prog, req); reasonOf(err) != "group_invalid" {
		t.Fatalf("group address: %v", err)
	}
	// An inactive program is person_inactive.
	if _, err := b.c.Principal(ctx, b.op, PrincipalRequest{Op: "principal.off", Subject: "service:report-client"}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.c.ProgramEnqueue(ctx, b.prog, EnqueueRequest{Body: greet("x"), IdempotencyKey: "k-off"}); reasonOf(err) != "person_inactive" {
		t.Fatalf("inactive program: %v", err)
	}
	// Its key no longer opens P1, so it reads nothing either.
	if _, err := b.c.AuthenticateProgram(ctx, "demo", "report-client"); reasonOf(err) != "person_inactive" {
		t.Fatalf("an inactive program's key: %v", err)
	}
}

// DSP-7 and MAN-5: at the queue ceiling a program gets over_capacity with retry_after_s and the
// key is not taken; a budget.set below the git limit refuses the enqueue with budget_exhausted
// until its expiry, a value above the git limit or without a future expiry is refused, and the
// operation is audited (BUD-1, PRM-14).
func TestProgramEnqueuePressure(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newPeopleBench(t, func(ms []map[string]any) {
		for _, m := range ms {
			if m["kind"] == "Tenant" {
				m["spec"].(map[string]any)["limits"].(map[string]any)["max_queued"] = 1
			}
		}
	})
	if _, err := b.c.ProgramEnqueue(ctx, b.prog, EnqueueRequest{Body: greet("a"), IdempotencyKey: "a"}); err != nil {
		t.Fatal(err)
	}
	_, err := b.c.ProgramEnqueue(ctx, b.prog, EnqueueRequest{Body: greet("b"), IdempotencyKey: "b"})
	var p *Problem
	if !errors.As(err, &p) || p.Reason != "over_capacity" || p.RetryAfterS <= 0 {
		t.Fatalf("over the ceiling: %v", err)
	}
	var n int
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE idempotency_key = 'b'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("the key was taken: %d %v", n, err)
	}

	until := fmtTime(time.Now().Add(time.Hour))
	if _, err := b.c.BudgetSet(ctx, b.op, BudgetSetRequest{Tenant: "demo", Line: "month", Limit: 40000, Until: until}); reasonOf(err) != "forbidden" {
		t.Fatalf("above git: %v", err)
	}
	if _, err := b.c.BudgetSet(ctx, b.op, BudgetSetRequest{Tenant: "demo", Line: "day", Limit: 0, Until: "2020-01-01T00:00:00Z"}); reasonOf(err) != "input_schema_invalid" {
		t.Fatalf("past expiry: %v", err)
	}
	if _, err := b.c.BudgetSet(ctx, b.op, BudgetSetRequest{Tenant: "demo", Line: "nope", Limit: 0, Until: until}); reasonOf(err) != "not_found" {
		t.Fatalf("unknown line: %v", err)
	}
	if _, err := b.c.BudgetSet(ctx, Operator{Principal: b.op.Principal, Tenant: "demo2"}, BudgetSetRequest{Tenant: "demo", Line: "day", Limit: 0, Until: until}); reasonOf(err) != "forbidden" {
		t.Fatalf("another tenant's operator: %v", err)
	}
	d, err := b.c.BudgetSet(ctx, b.op, BudgetSetRequest{Tenant: "demo", Line: "day", Limit: 0, Until: until})
	if err != nil || d.AuditSeq == 0 {
		t.Fatalf("budget.set %+v %v", d, err)
	}
	validOut(t, b.bench, "done-response", d)
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE tasks SET state = 'completed'`); err != nil {
		t.Fatal(err)
	}
	if _, err := b.c.ProgramEnqueue(ctx, b.prog, EnqueueRequest{Body: greet("c"), IdempotencyKey: "c"}); reasonOf(err) != "budget_exhausted" {
		t.Fatalf("under budget.set: %v", err)
	}
	// After the expiry the git limit applies again.
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE budget_overrides SET until = botaru_now() - interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	if _, err := b.c.ProgramEnqueue(ctx, b.prog, EnqueueRequest{Body: greet("c"), IdempotencyKey: "c"}); err != nil {
		t.Fatalf("after the expiry: %v", err)
	}
}
