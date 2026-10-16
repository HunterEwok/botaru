// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"testing"
)

// worker.rotate (IDN-4): a new token works at once, the old one until old_valid_until and not
// after; the store keeps only hashes; a revoked worker is not rotated, a foreign one is
// not_found.
func TestWorkerRotate(t *testing.T) {
	ctx := context.Background()
	b := newBench(t)
	r, err := b.c.Enroll(ctx, b.op, EnrollRequest{Tenant: "demo", Role: "hello", Kind: "agent"})
	if err != nil {
		t.Fatal(err)
	}
	rot, err := b.c.WorkerRotate(ctx, b.op, WorkerRequest{Op: "worker.rotate", Worker: r.Worker.ID})
	if err != nil {
		t.Fatal(err)
	}
	validOut(t, b, "p2-worker-rotate-response", rot)
	for _, tok := range []string{rot.Token, r.Token} {
		if w, err := b.c.AuthenticateWorker(ctx, tok); err != nil || w.ID != r.Worker.ID {
			t.Fatalf("token in the window: %v", err)
		}
	}
	var n int
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM workers WHERE workers::text LIKE '%' || $1 || '%' OR workers::text LIKE '%' || $2 || '%'`,
		rot.Token, r.Token).Scan(&n); err != nil || n != 0 {
		t.Fatalf("a token in the clear %d %v", n, err)
	}
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE workers SET prev_valid_until = botaru_now() - interval '1 second' WHERE id = $1`, r.Worker.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := b.c.AuthenticateWorker(ctx, r.Token); reasonOf(err) != "unauthenticated" {
		t.Fatalf("the old token after the window: %v", err)
	}
	if _, err := b.c.WorkerRotate(ctx, Operator{Principal: b.op.Principal, Tenant: "demo2"}, WorkerRequest{Op: "worker.rotate", Worker: r.Worker.ID}); reasonOf(err) != "not_found" {
		t.Fatalf("another tenant: %v", err)
	}
	// The farm operator reaches a worker of any tenant (PRM-14).
	if _, err := b.c.WorkerRotate(ctx, Operator{Principal: b.op.Principal, Tenant: "farm"}, WorkerRequest{Op: "worker.rotate", Worker: r.Worker.ID}); err != nil {
		t.Fatalf("the farm operator: %v", err)
	}
	if _, err := b.c.WorkerRevoke(ctx, b.op, WorkerRequest{Op: "worker.revoke", Worker: r.Worker.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.c.WorkerRotate(ctx, b.op, WorkerRequest{Op: "worker.rotate", Worker: r.Worker.ID}); reasonOf(err) != "subject_closed" {
		t.Fatalf("a revoked worker: %v", err)
	}
	if _, err := b.c.AuthenticateWorker(ctx, rot.Token); reasonOf(err) != "unauthenticated" {
		t.Fatalf("a revoked token: %v", err)
	}
}

// worker.unblock (DEP-2, INJ-2): a worker blocked at hello for a mismatch with its enroll record
// goes back to enrolled, and its next hello is checked again from the start: the same mismatch
// blocks it again, a matching hello makes it active. A worker that is not blocked is left as it is.
func TestWorkerUnblock(t *testing.T) {
	ctx := context.Background()
	b := newBench(t)
	r, err := b.c.Enroll(ctx, b.op, EnrollRequest{Tenant: "demo", Role: "hello", Kind: "agent", Labels: map[string]string{"zone": "a"}})
	if err != nil {
		t.Fatal(err)
	}
	w, err := b.c.AuthenticateWorker(ctx, r.Token)
	if err != nil {
		t.Fatal(err)
	}
	bad := HelloRequest{Kind: "agent", ProtocolVersions: []string{"v1"}, Labels: map[string]string{"zone": "b"}}
	if _, err := b.c.Hello(ctx, w, bad); reasonOf(err) != "blocked" {
		t.Fatalf("mismatch: %v", err)
	}
	d, err := b.c.WorkerUnblock(ctx, b.op, WorkerRequest{Op: "worker.unblock", Worker: w.ID})
	if err != nil || d.State != "enrolled" || d.AuditSeq == 0 {
		t.Fatalf("unblock %+v %v", d, err)
	}
	validOut(t, b, "done-response", d)
	if w, err = b.c.AuthenticateWorker(ctx, r.Token); err != nil {
		t.Fatal(err)
	}
	if _, err := b.c.Hello(ctx, w, bad); reasonOf(err) != "blocked" {
		t.Fatalf("the same mismatch after unblock: %v", err)
	}
	if _, err := b.c.WorkerUnblock(ctx, b.op, WorkerRequest{Op: "worker.unblock", Worker: w.ID}); err != nil {
		t.Fatal(err)
	}
	if w, err = b.c.AuthenticateWorker(ctx, r.Token); err != nil {
		t.Fatal(err)
	}
	if _, err := b.c.Hello(ctx, w, HelloRequest{Kind: "agent", ProtocolVersions: []string{"v1"}, Labels: map[string]string{"zone": "a"}}); err != nil {
		t.Fatalf("a matching hello: %v", err)
	}
	again, err := b.c.WorkerUnblock(ctx, b.op, WorkerRequest{Op: "worker.unblock", Worker: w.ID})
	if err != nil || again.State != "active" || again.AuditSeq != 0 {
		t.Fatalf("an active worker %+v %v", again, err)
	}
}

// P2 service.control (APR-18, PRM-18): a person with service.control:nginx restarts it under a
// service_grant with the person as author and one write call, without a card; stop opens an
// approval; a person without the capability gets forbidden with needs; a verb outside the
// service's control is refused; a service principal does not give the command.
func TestServiceControlP2(t *testing.T) {
	ctx := context.Background()
	b := newServiceBench(t)
	alice := Operator{Principal: b.alice, Tenant: "demo"}
	// A stop goes through a card first, a start past the card limit is service_rate_limited; the
	// restart then runs, one operation at a time.
	stop, err := b.c.ServiceControl(ctx, alice, ServiceControlRequest{Service: "nginx", Verb: "stop"})
	if err != nil || stop.Code != "approval_required" || stop.ApprovalID == "" {
		t.Fatalf("stop %+v %v", stop, err)
	}
	validOut(t, b.bench, "service-control-result", stop)
	// PRM-18: past the card limit the answer is service_rate_limited.
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE manifests SET doc = jsonb_set(doc, '{spec,attention}', '{"cards_per_tenant_hour": 1}') WHERE kind = 'Tenant' AND name = 'demo'`); err != nil {
		t.Fatal(err)
	}
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE apply_status SET set_hash = set_hash || 'x'`); err != nil {
		t.Fatal(err)
	}
	if lim, err := b.c.ServiceControl(ctx, alice, ServiceControlRequest{Service: "nginx", Verb: "start"}); err != nil || lim.Code != "service_rate_limited" {
		t.Fatalf("past the card limit %+v %v", lim, err)
	}
	r, err := b.c.ServiceControl(ctx, alice, ServiceControlRequest{Service: "nginx", Verb: "restart"})
	if err != nil || r.Code != "done" || r.GrantID == "" {
		t.Fatalf("restart %+v %v", r, err)
	}
	validOut(t, b.bench, "service-control-result", r)
	gs := b.grants(t)
	if len(gs) != 1 || gs[0]["service_grant"].(map[string]any)["author"].(map[string]any)["principal"] != b.alice {
		t.Fatalf("grants %v", gs)
	}
	var calls int
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM toolcalls WHERE grant_id = $1`, r.GrantID).Scan(&calls); err != nil || calls != 1 {
		t.Fatalf("write calls %d %v", calls, err)
	}
	no, err := b.c.ServiceControl(ctx, Operator{Principal: b.carol, Tenant: "demo"}, ServiceControlRequest{Service: "nginx", Verb: "restart"})
	if err != nil || no.Code != "forbidden" || len(no.Needs) != 1 || no.Needs[0] != "service.control:nginx" {
		t.Fatalf("without the right %+v %v", no, err)
	}
	validOut(t, b.bench, "service-control-result", no)
	if _, err := b.c.ServiceControl(ctx, alice, ServiceControlRequest{Service: "api", Verb: "stop"}); reasonOf(err) != "input_schema_invalid" {
		t.Fatalf("a verb outside control: %v", err)
	}
	if _, err := b.c.Principal(ctx, b.op, PrincipalRequest{Op: "principal.add", Subject: "service:ops", Kind: "service"}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.c.ServiceControl(ctx, Operator{Principal: "service:ops", Tenant: "demo"}, ServiceControlRequest{Service: "nginx", Verb: "restart"}); reasonOf(err) != "forbidden" {
		t.Fatalf("a service principal: %v", err)
	}
	var denied int
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM audit WHERE record->>'command' = 'service.control' AND record->'actor'->>'principal' = 'service:ops'
		AND record->>'ok' = 'false'`).Scan(&denied); err != nil || denied != 1 {
		t.Fatalf("the refusal of a service principal in the audit %d %v", denied, err)
	}
}
