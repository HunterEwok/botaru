// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"slices"
	"testing"
)

// Tests of the tenant lifecycle (LCY-1, RET-3).

// The tenant lifecycle (LCY-1, RET-3): suspend is an operator hold that only the operator
// releases, with tenant.resumed at its end; offboard revokes the workers, names the operator
// step and opens the export window; delete before its end gets export_window_open, after it an
// erasure request (tenant) and a tombstone that apply refuses; farm never leaves.
func TestTenantLifecycle(t *testing.T) {
	ctx := context.Background()
	b := newBench(t)
	if _, err := b.c.TenantLifecycle(ctx, Operator{Principal: b.op.Principal, Tenant: "farm"}, TenantLifecycleRequest{Op: "tenant.offboard", Tenant: "farm"}); !hasReason(err, "forbidden") {
		t.Fatalf("farm offboarded: %v", err)
	}
	s, err := b.c.TenantLifecycle(ctx, b.op, TenantLifecycleRequest{Op: "tenant.suspend", Tenant: "demo"})
	if err != nil || s.State != "suspended" {
		t.Fatalf("suspend %+v %v", s, err)
	}
	var effects []string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT effects FROM holds WHERE id = $1 AND reason = 'suspended' AND scope_kind = 'tenant' AND until IS NULL`, s.ID).Scan(&effects); err != nil || len(effects) != 5 {
		t.Fatalf("suspension hold %v %v", effects, err)
	}
	// No admin:people holder in this bench: no notice, and the audit record says so (HLD-3).
	var why string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT record->'after'->>'admins_notice' FROM audit WHERE record->>'command' = 'tenant.suspend'`).Scan(&why); err != nil || why != "no_recipient" {
		t.Fatalf("suspension audit %q %v", why, err)
	}
	admin := HoldActor{Principal: b.alice, Rank: "tenant_admin", Tenant: "demo", Via: "channel"}
	if _, err := b.c.HoldRelease(ctx, admin, HoldReleaseRequest{ID: s.ID}); !hasReason(err, "forbidden") {
		t.Fatalf("an admin lifted a suspension: %v", err)
	}
	if _, err := b.c.HoldRelease(ctx, OperatorActor(b.op), HoldReleaseRequest{ID: s.ID}); err != nil {
		t.Fatal(err)
	}
	events := func(typ string) int {
		var n int
		if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE type = $1 AND subject = 'demo'`, typ).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if events("tenant.suspended.v1") != 1 || events("tenant.resumed.v1") != 1 {
		t.Fatal("suspension events")
	}
	if _, err := b.c.TenantLifecycle(ctx, b.op, TenantLifecycleRequest{Op: "tenant.offboard", Tenant: "demo", ExportWindow: "31d"}); !hasReason(err, "input_schema_invalid") {
		t.Fatalf("export window above 30 days: %v", err)
	}
	off, err := b.c.TenantLifecycle(ctx, b.op, TenantLifecycleRequest{Op: "tenant.offboard", Tenant: "demo", ExportWindow: "2d"})
	if err != nil || off.State != "offboarding" || !slices.Equal(off.OperatorSteps, []string{"provider_key_revoke"}) {
		t.Fatalf("offboard %+v %v", off, err)
	}
	var live int
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM workers WHERE tenant = 'demo' AND state <> 'revoked'`).Scan(&live); err != nil || live != 0 {
		t.Fatalf("live workers after offboard %d %v", live, err)
	}
	if _, err := b.c.HoldRelease(ctx, OperatorActor(b.op), HoldReleaseRequest{ID: off.ID}); !hasReason(err, "forbidden") {
		t.Fatalf("the offboarding hold was released: %v", err)
	}
	b.advance(t, 86400)
	if _, err := b.c.TenantLifecycle(ctx, b.op, TenantLifecycleRequest{Op: "tenant.delete", Tenant: "demo"}); !hasReason(err, "export_window_open") {
		t.Fatalf("delete in the window: %v", err)
	}
	b.advance(t, 86400+1)
	if _, err := b.c.st.Pool.Exec(ctx, `INSERT INTO monitor_accounts (tenant, worker, report, checked_at) VALUES ('demo', 'wk_x', '{}', now())`); err != nil {
		t.Fatal(err)
	}
	del, err := b.c.TenantLifecycle(ctx, b.op, TenantLifecycleRequest{Op: "tenant.delete", Tenant: "demo"})
	if err != nil || del.State != "deleted" {
		t.Fatalf("delete %+v %v", del, err)
	}
	// MON-4: a deleted tenant's account reports go with it.
	if n := b.count(t, `SELECT count(*) FROM monitor_accounts WHERE tenant = 'demo'`); n != 0 {
		t.Fatal("account reports of a deleted tenant")
	}
	var reason string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT reason FROM erasure_requests WHERE id = $1 AND tenant = 'demo' AND subject IS NULL`, del.ID).Scan(&reason); err != nil || reason != "tenant_delete" {
		t.Fatalf("erasure request %q %v", reason, err)
	}
	if events("tenant.offboarding.v1") != 1 || events("tenant.deleted.v1") != 1 {
		t.Fatal("offboarding events")
	}
	ms, _ := helloSet(t)
	res, err := b.c.Apply(ctx, b.op, ApplyRequest{Commit: "8888888888888888888888888888888888888888", Manifests: ms})
	if err != nil || res.Applied || !hasCode(res, "gone") {
		t.Fatalf("apply of a deleted tenant: %+v %v", res, err)
	}
}
