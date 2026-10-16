// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/ids"
	"github.com/HunterEwok/botaru/impl/internal/mcp"
)

// seatType is a farm manager task type of purpose manage, as the core enqueues it.
func seatType(name string) map[string]any {
	return map[string]any{"apiVersion": "botaru/v1alpha1", "kind": "TaskType", "metadata": map[string]any{"name": name},
		"spec": map[string]any{"title": "Farm manager " + name, "worker_kind": "agent", "purpose": "manage", "tier": 1, "data_class": "internal",
			"params_schema": map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{}},
			"timeouts":      map[string]any{"lease": "60s", "deadline": "2h"}, "tree_budget_eur": 0.2, "proposable": false}}
}

// managerV02Bench is the manager bench with the grant and takeover lines, a grant ladder on the
// hello role, a grant-eligible hello type and the seat types of the given names claimed by the
// seat role and allowed to its consent group.
func managerV02Bench(t *testing.T, types []string, extra []map[string]any, edits ...func([]map[string]any)) *bench {
	t.Helper()
	add := extra
	for _, n := range types {
		add = append(add, seatType(n))
	}
	return newManagerBenchWith(t, []string{"examples/model-profiles/review-a.yaml"}, add, append([]func([]map[string]any){func(ms []map[string]any) {
		for _, m := range ms {
			spec := asMap(m["spec"])
			name := mstr(asMap(m["metadata"]), "name")
			switch {
			case m["kind"] == "Tenant":
				lines := asMap(asMap(spec["budgets"])["lines"])
				lines["grant"] = map[string]any{"month_eur": 5}
				lines["takeover"] = map[string]any{"month_eur": 5}
			case m["kind"] == "Role" && name == "hello":
				spec["model_grants"] = map[string]any{"ladder": []any{"review-a"}}
			case m["kind"] == "Role" && name == "farm-manager":
				claims, _ := spec["claims"].([]any)
				for _, n := range types {
					claims = append(claims, n)
				}
				spec["claims"] = claims
			case m["kind"] == "TaskType" && name == "hello.greet.v1":
				spec["grant_eligible"] = true
			case m["kind"] == "Group" && name == "farm-manager-ops":
				caps, _ := spec["caps"].([]any)
				for _, n := range types {
					caps = append(caps, "task:"+n)
				}
				spec["caps"] = caps
			}
		}
	}}, edits...)...)
}

func callTool(t *testing.T, b *bench, handle, name string, args map[string]any) map[string]any {
	t.Helper()
	r, err := b.c.P3ToolsCall(context.Background(), handle, mcp.CallParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(callText(r)), &out); err != nil {
		t.Fatalf("%s: %s", name, callText(r))
	}
	return out
}

// An executor asks for a stronger model with request_model_grant: the answer is {staged: true},
// the attempt ends without spending and the task waits; evidence not of the task is refused:
// the answer is grant_not_eligible and the attempt goes on (MGR-17). The grant loop gives the held seat
// one manager.grants.v1 batch for the tenant's window; grant_decide issues it, the next attempt
// runs on the granted slot, a call over the rest of cap_eur does not start and consumes the
// grant without spending the attempt, and an unanswered request expires (MGR-18, GW-7).
func TestModelGrant(t *testing.T) {
	ctx := context.Background()
	b := managerV02Bench(t, []string{grantsType}, nil)
	att, h := b.attemptTools(t, "u1", false)
	if !slices.Contains(toolNames(t, b, h), grantToolName) {
		t.Fatalf("tools %v", toolNames(t, b, h))
	}
	ask := func(h, evidence string) map[string]any {
		return callTool(t, b, h, grantToolName, map[string]any{"reason": "low_confidence_declared", "evidence": []any{evidence},
			"requested_profile": "review-a", "benefit": "pass_schema", "attempts": 1, "note": "please"})
	}
	if out := ask(h, att.WorkID+":1"); out["staged"] != true {
		t.Fatalf("request %v", out)
	}
	var lease, reason, state string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT l.state, coalesce(t.reason, ''), (SELECT state FROM model_grants WHERE task = t.id) FROM tasks t
		JOIN leases l ON l.work_id = t.id WHERE t.id = $1`, att.WorkID).Scan(&lease, &reason, &state); err != nil || lease != "released" || reason != "model_grant_pending" || state != "pending" {
		t.Fatalf("lease %s task %s grant %s %v", lease, reason, state, err)
	}
	other, h2 := b.attemptTools(t, "u2", false)
	if out := ask(h2, att.WorkID+":1"); out["code"] != "grant_not_eligible" {
		t.Fatalf("request %v", out)
	}
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT l.state, g.state || ':' || g.reason FROM leases l JOIN model_grants g ON g.task = l.work_id WHERE l.work_id = $1`,
		other.WorkID).Scan(&lease, &state); err != nil || lease != "active" || state != "denied:grant_not_eligible" {
		t.Fatalf("foreign evidence: lease %s grant %s %v", lease, state, err)
	}

	// No batch while the seat is vacant; then one per tenant and window.
	b.loop(t, "grant")
	var batches int
	count := func() int {
		if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE type = $1 AND tenant = 'demo' AND idempotency_key LIKE 'system:grants:demo:%'`, grantsType).Scan(&batches); err != nil {
			t.Fatal(err)
		}
		return batches
	}
	if count() != 0 {
		t.Fatal("a batch for a vacant seat")
	}
	mw := b.enroll(t, EnrollRequest{Tenant: "demo", Kind: "agent", Role: "farm-manager"})
	if _, err := b.c.SeatAssign(ctx, b.farmOp(), SeatRequest{Seat: managerSeatID}); err != nil {
		t.Fatal(err)
	}
	b.loop(t, "grant")
	b.loop(t, "grant")
	if count() != 1 {
		t.Fatalf("batches %d", batches)
	}
	item := b.claim(t, mw, "attempt")
	if item == nil {
		t.Fatal("the batch was not handed out")
	}
	sh := item.Payload["handles"].(map[string]any)["tools"].(string)
	pending := callTool(t, b, sh, grantsPendingName, map[string]any{})
	reqs, _ := pending["requests"].([]any)
	if len(reqs) != 1 || strings.Contains(callText(mustCall(t, b, sh, grantsPendingName)), "please") {
		t.Fatalf("pending %v", pending)
	}
	gid := mstr(asMap(reqs[0]), "grant")
	res := callTool(t, b, sh, grantDecideName, map[string]any{"decisions": []any{
		map[string]any{"grant": gid, "decision": "issue", "cap_eur": 0.001, "expires_in": "7h"},
		map[string]any{"grant": gid, "decision": "issue", "cap_eur": 0.001},
		map[string]any{"grant": "mg_01JB0000000000000000000000", "decision": "issue"},
	}})
	rs, _ := res["results"].([]any)
	if len(rs) != 3 || asMap(rs[0])["reason"] != "grant_over_budget" || asMap(rs[1])["state"] != "active" || asMap(rs[2])["reason"] != "not_found" {
		t.Fatalf("decide %v", res)
	}

	// The next attempt runs on the granted slot; a call over the cap does not start.
	again := b.claim(t, b.agent, "attempt")
	if again == nil || again.WorkID != att.WorkID {
		t.Fatalf("granted attempt %+v", again)
	}
	mg, err := b.c.ResolveModelHandle(ctx, again.Payload["handles"].(map[string]any)["model"].(string))
	if err != nil || mg.Slot != grantedSlot || mg.ProfileName != "review-a" || mg.Line != "grant" || mg.GrantID != gid {
		t.Fatalf("handle %+v %v", mg, err)
	}
	if _, err := b.c.Reserve(ctx, mg, 5000); err == nil || !strings.Contains(err.Error(), "budget_exhausted") {
		t.Fatalf("reserve over the cap: %v", err)
	}
	var spent int
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT g.state, l.state, t.attempts_spent FROM model_grants g JOIN leases l ON l.work_id = g.task AND l.lease_no = $2
		JOIN tasks t ON t.id = g.task WHERE g.id = $1`, gid, again.LeaseNo).Scan(&state, &lease, &spent); err != nil || state != "consumed" || lease != "released" || spent != 0 {
		t.Fatalf("grant %s lease %s spent %d %v", state, lease, spent, err)
	}

	// A request nobody decides within grant_wait_max expires, and its task goes on.
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE model_grants SET state = 'pending', reason = NULL, created_at = created_at - interval '1 hour' WHERE task = $1`, other.WorkID); err != nil {
		t.Fatal(err)
	}
	b.loop(t, "grant")
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT state || ':' || reason FROM model_grants WHERE task = $1`, other.WorkID).Scan(&state); err != nil || state != "expired:grant_expired_unanswered" {
		t.Fatalf("expiry %s %v", state, err)
	}
}

func mustCall(t *testing.T, b *bench, handle, name string) *mcp.CallResult {
	t.Helper()
	r, err := b.c.P3ToolsCall(context.Background(), handle, mcp.CallParams{Name: name, Arguments: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// A person with model_grant.manage issues, lists and revokes a grant through P2; a revoke ends
// the running attempt on the granted slot with grant_revoked without spending; an operator
// without the right is refused (MGR-18).
func TestModelGrantP2(t *testing.T) {
	ctx := context.Background()
	b := managerV02Bench(t, nil, nil)
	if _, err := b.c.Apply(ctx, b.op, ApplyRequest{Commit: strings.Repeat("1", 40), Manifests: []map[string]any{{"apiVersion": "botaru/v1alpha1", "kind": "Group",
		"metadata": map[string]any{"name": "grant-managers", "tenant": "demo"}, "spec": map[string]any{"caps": []any{"model_grant.manage"}}}}}); err != nil {
		t.Fatal(err)
	}
	b.member(t, "demo", b.alice, "grant-managers")
	att, h := b.attemptTools(t, "u1", false)
	callTool(t, b, h, grantToolName, map[string]any{"reason": "low_confidence_declared", "evidence": []any{att.WorkID + ":1"},
		"requested_profile": "review-a", "benefit": "pass_schema", "attempts": 2})
	var gid string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT id FROM model_grants WHERE task = $1`, att.WorkID).Scan(&gid); err != nil {
		t.Fatal(err)
	}
	if _, err := b.c.ModelGrantIssue(ctx, b.op, ModelGrantRequest{Tenant: "demo", Grant: gid, Decision: "issue"}); err == nil {
		t.Fatal("an operator without model_grant.manage issued a grant")
	}
	alice := Operator{Principal: b.alice, Tenant: "demo"}
	out, err := b.c.ModelGrantIssue(ctx, alice, ModelGrantRequest{Tenant: "demo", Grant: gid, Decision: "issue", ExpiresIn: "2h"})
	if err != nil || asMap(out["grant"])["state"] != "approved" || asMap(out["grant"])["attempts_left"] != 2 {
		t.Fatalf("issue %v %v", out, err)
	}
	if list, err := b.c.ModelGrantList(ctx, alice, ModelGrantRequest{}); err != nil || len(list["grants"].([]any)) != 1 {
		t.Fatalf("list %v %v", list, err)
	}
	again := b.claim(t, b.agent, "attempt")
	if again == nil {
		t.Fatal("no granted attempt")
	}
	if _, err := b.c.ModelGrantRevoke(ctx, alice, ModelGrantRequest{Tenant: "demo", Grant: gid}); err != nil {
		t.Fatal(err)
	}
	var lease, reason string
	var spent int
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT l.state, l.reason, t.attempts_spent FROM leases l JOIN tasks t ON t.id = l.work_id WHERE l.work_id = $1 AND l.lease_no = $2`,
		att.WorkID, again.LeaseNo).Scan(&lease, &reason, &spent); err != nil || lease != "released" || reason != "grant_revoked" || spent != 0 {
		t.Fatalf("lease %s %s spent %d %v", lease, reason, spent, err)
	}
}

// An approved grant is bound to the role that asked and to its ladder: an attempt of another
// role, or a profile the role's ladder no longer has, gets no granted slot and leaves the grant
// approved. The seat's daily grant ceiling is on without seat_grants_eur_day, per UTC calendar
// day (MGR-18, GW-7).
func TestGrantBoundToRole(t *testing.T) {
	ctx := context.Background()
	b := managerV02Bench(t, nil, nil)
	task := b.hello(t, "u1")
	cat, err := b.c.Catalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	grant := func(profile string) {
		if _, err := b.c.st.Pool.Exec(ctx, `DELETE FROM model_grants WHERE task = $1`, task); err != nil {
			t.Fatal(err)
		}
		if _, err := b.c.st.Pool.Exec(ctx, `INSERT INTO model_grants (id, tenant, task, role, kind, line, profile, attempts, attempts_left, estimate_micros, cap_micros, expires_at, state, created_at, updated_at)
			VALUES ($1, 'demo', $2, 'hello', 'grant', 'grant', $3, 1, 1, 1000, 1000, now() + interval '1 hour', 'approved', now(), now())`, ids.New(ids.ModelGrant), task, profile); err != nil {
			t.Fatal(err)
		}
	}
	try := func(role string) bool {
		var ok bool
		if err := b.c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
			_, ok, err = claimGrant(ctx, tx, now, cat, "demo", task, role)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return ok
	}
	grant("review-a")
	if try("other-role") || !try("hello") {
		t.Fatal("the grant went to the wrong role")
	}
	grant("base-a")
	if try("hello") {
		t.Fatal("a profile off the ladder was granted")
	}
	if l := grantLimitsOf(newCatalog()); l.seatDayMicros != 20_000_000 {
		t.Fatalf("seat day ceiling %d", l.seatDayMicros)
	}
	if d := utcDayStart(time.Date(2026, 10, 16, 23, 59, 0, 0, time.FixedZone("x", -3*3600))); !d.Equal(time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("day start %v", d)
	}
}
