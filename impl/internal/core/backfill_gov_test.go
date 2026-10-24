// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"slices"
	"strings"
	"testing"
)

// Governance lives in the tenant: the farm manager's seat task for an incident in demo is a task
// of demo, run by a worker of demo, with the key and the budget of demo; farm holds only the
// registry of the manager seat and its holder identity, and no task, fact, case, offer, memory or
// commission seat (GOV-16, gov-fx-10).
func TestBackfillGovernanceInTenant(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newManagerBench(t)
	w := b.enroll(t, EnrollRequest{Tenant: "demo", Kind: "agent", Role: "farm-manager"})
	b.diskAlarm(t, true, 3)
	b.advance(t, 6*60)
	b.loop(t, "incident")
	// Assigned after the incident opened, so its class does not stop the seat.
	if _, err := b.c.SeatAssign(ctx, b.farmOp(), SeatRequest{Seat: managerSeatID}); err != nil {
		t.Fatal(err)
	}
	item := b.claim(t, w, "attempt")
	if item == nil {
		t.Fatal("the seat task was not handed out")
	}
	var tenant, key, worker string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT t.tenant, t.idempotency_key, l.worker_id FROM tasks t JOIN leases l ON l.work_id = t.id WHERE t.id = $1`, item.WorkID).
		Scan(&tenant, &key, &worker); err != nil {
		t.Fatal(err)
	}
	// GOV-16: the task, its worker and its key are of demo.
	if tenant != "demo" || worker != w.ID || !strings.HasPrefix(key, "system:incident:") {
		t.Fatalf("the seat task: tenant %s worker %s key %s", tenant, worker, key)
	}
	if n := b.count(t, `SELECT count(*) FROM workers WHERE id = $1 AND tenant = 'demo'`, w.ID); n != 1 {
		t.Fatal("the executor is not a worker of demo")
	}
	// GOV-16: farm holds the manager seat and the holder's identity, nothing else of governance.
	m := b.managerSeat(t)
	if n := b.count(t, `SELECT count(*) FROM seats WHERE tenant = 'farm'`); n != 1 {
		t.Fatalf("farm holds %d seats", n)
	}
	if n := b.count(t, `SELECT count(*) FROM seats WHERE tenant = 'farm' AND kind = 'commission'`); n != 0 {
		t.Fatal("farm holds a commission seat")
	}
	if n := b.count(t, `SELECT count(*) FROM bots WHERE tenant = 'farm' AND id <> $1 AND state NOT IN ('retired', 'erased')`, m.Holder); n != 0 {
		t.Fatalf("farm holds %d more live identities", n)
	}
	for _, q := range []string{
		`SELECT count(*) FROM tasks WHERE tenant = 'farm'`,
		`SELECT count(*) FROM facts WHERE tenant = 'farm'`,
		`SELECT count(*) FROM fact_offers WHERE tenant = 'farm'`,
		`SELECT count(*) FROM governance_cases WHERE tenant = 'farm'`,
		`SELECT count(*) FROM attributions WHERE tenant = 'farm'`,
		`SELECT count(*) FROM memory_items WHERE tenant = 'farm'`,
		`SELECT count(*) FROM sessions WHERE tenant = 'farm'`,
	} {
		if n := b.count(t, q); n != 0 {
			t.Fatalf("%s: %d", q, n)
		}
	}
	// GOV-16: a canary of demo appears in no record of another tenant: no task, fact, case,
	// identity or seat outside demo carries it.
	const canary = "CANARY-gov16-7f3a"
	act := Activity{Kind: "message", Channel: "telegram", UpdateID: "canary", Chat: Chat{Kind: "direct", ExternalID: "1001"}, Text: "/hello " + canary}
	act.Sender.Identity, act.Sender.Verified = "telegram:1001", true
	if res, err := b.c.SubmitActivity(ctx, b.channel, act); err != nil || res.Result != "task" {
		t.Fatalf("the canary task: %+v %v", res, err)
	}
	for _, q := range []string{
		`SELECT count(*) FROM tasks WHERE tenant <> 'demo' AND (body::text LIKE $1 OR record::text LIKE $1)`,
		`SELECT count(*) FROM facts f WHERE f.tenant <> 'demo' AND row_to_json(f)::text LIKE $1`,
		`SELECT count(*) FROM governance_cases g WHERE g.tenant <> 'demo' AND row_to_json(g)::text LIKE $1`,
		`SELECT count(*) FROM bots x WHERE x.tenant <> 'demo' AND row_to_json(x)::text LIKE $1`,
		`SELECT count(*) FROM seats x WHERE x.tenant <> 'demo' AND row_to_json(x)::text LIKE $1`,
	} {
		if n := b.count(t, q, "%"+canary+"%"); n != 0 {
			t.Fatalf("%s: the canary of demo in %d rows", q, n)
		}
	}
	if n := b.count(t, `SELECT count(*) FROM tasks WHERE tenant = 'demo' AND body::text LIKE $1`, "%"+canary+"%"); n != 1 {
		t.Fatal("the canary is not in its own tenant")
	}
	// GOV-16: the spend of the seat task, if any, is on the line of demo.
	if n := b.count(t, `SELECT count(*) FROM spend WHERE task = $1 AND tenant <> 'demo'`, item.WorkID); n != 0 {
		t.Fatal("the seat task spends on another tenant")
	}
}

// Only the charter declares seats: apply refuses seats in the commission manifest, a seat
// declared twice, a block outside the closed list, the farm_manager block in a tenant charter,
// and every numeric field moved against its direction; it keeps a seat with a holder, and
// accepts fields moved in their direction (GOV-33, gov-fx-26).
func TestBackfillOnlyCharterDeclaresSeats(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newCommissionBench(t, false)
	try := func(name string, edit func(gov, com map[string]any)) []ApplyError {
		t.Helper()
		ms := cloneSet(t, b.applied)
		edit(specOf(ms, "Governance", "demo"), specOf(ms, "Commission", "demo"))
		res, err := b.c.Apply(ctx, farmOf(b.op), ApplyRequest{Commit: strings.Repeat("5", 40), Manifests: ms, DryRun: true})
		if err != nil {
			return []ApplyError{{Code: "refused", Detail: err.Error()}}
		}
		return res.Errors
	}
	sub := func(m map[string]any, k string) map[string]any {
		v, _ := m[k].(map[string]any)
		if v == nil {
			v = map[string]any{}
			m[k] = v
		}
		return v
	}
	if errs := try("unchanged", func(_, _ map[string]any) {}); len(errs) != 0 {
		t.Fatalf("the example set: %+v", errs)
	}
	for _, tc := range []struct {
		name, code, path string
		edit             func(gov, com map[string]any)
	}{
		{"seats in the commission manifest", "schema_invalid", "", func(_, com map[string]any) { com["seats"] = []any{map[string]any{"id": 5, "lens": "cost"}} }},
		{"a seat declared twice", "charter_seat_twice", "/spec/commission/seats/4/id", func(gov, _ map[string]any) {
			c := sub(gov, "commission")
			c["seats"] = append(c["seats"].([]any), map[string]any{"id": 1, "lens": "cost", "fallback_role": "reviewer-b"})
		}},
		{"a block outside the list", "schema_invalid", "", func(gov, _ map[string]any) { gov["rewards"] = map[string]any{} }},
		{"the farm manager in a tenant charter", "charter_farm_manager", "/spec/farm_manager", func(gov, _ map[string]any) { gov["farm_manager"] = map[string]any{"max_rank": 4} }},
		{"probation shortened", "charter_direction", "/promotion/probation", func(gov, _ map[string]any) { sub(gov, "promotion")["probation"] = "7d" }},
		{"min_tenure shortened", "charter_direction", "/promotion/min_tenure", func(gov, _ map[string]any) { sub(gov, "promotion")["min_tenure"] = "20d" }},
		{"swap_cooldown shortened", "charter_direction", "/promotion/swap_cooldown", func(gov, _ map[string]any) { sub(gov, "promotion")["swap_cooldown"] = "10d" }},
		{"protect_new shortened", "charter_direction", "/removal/protect_new", func(gov, _ map[string]any) { sub(gov, "removal")["protect_new"] = "30d" }},
		{"attribution window shortened", "charter_direction", "/attribution/window", func(gov, _ map[string]any) { sub(gov, "attribution")["window"] = "7d" }},
		{"confirmation ttl shortened", "charter_direction", "/confirmation/ttl", func(gov, _ map[string]any) { sub(gov, "confirmation")["ttl"] = "3d" }},
		{"unbound_ttl shortened", "charter_direction", "/identity/unbound_ttl", func(gov, _ map[string]any) { sub(gov, "identity")["unbound_ttl"] = "10d" }},
		{"seal_ttl shortened", "charter_direction", "/memory/seal_ttl", func(gov, _ map[string]any) { sub(gov, "memory")["seal_ttl"] = "10d" }},
		{"max_active raised", "schema_invalid", "", func(gov, _ map[string]any) { sub(sub(gov, "memory"), "facts")["max_active"] = 200 }},
		{"per_attempt raised", "schema_invalid", "", func(gov, _ map[string]any) { sub(sub(gov, "memory"), "facts")["per_attempt"] = 4 }},
		{"offer max raised", "schema_invalid", "", func(gov, _ map[string]any) { sub(sub(gov, "memory"), "offer")["max"] = 120 }},
		{"none left out of the excluded kinds", "charter_direction", "/promotion/exclude_change_kinds", func(gov, _ map[string]any) {
			sub(gov, "promotion")["exclude_change_kinds"] = []any{"script"}
		}},
		{"audit sample lowered", "schema_invalid", "", func(gov, _ map[string]any) { sub(sub(gov, "commission"), "audit_sample")["min_per_month"] = 2 }},
	} {
		// GOV-33: each is refused with its code, at its path for a lint finding; the schema
		// refuses the counts, the commission seats and an unknown block.
		errs := try(tc.name, tc.edit)
		if !slices.ContainsFunc(errs, func(e ApplyError) bool { return e.Code == tc.code && strings.HasSuffix(e.Path, tc.path) }) {
			t.Errorf("%s: errors %+v, want %s at %s", tc.name, errs, tc.code, tc.path)
		}
	}
	// GOV-33: moves in the allowed direction pass.
	if errs := try("allowed", func(gov, _ map[string]any) {
		p := sub(gov, "promotion")
		p["probation"], p["swap_cooldown"] = "21d", "45d"
		mem := sub(gov, "memory")
		mem["seal_ttl"] = "45d"
		f := sub(mem, "facts")
		f["max_active"], f["per_attempt"] = 50, 2
	}); len(errs) != 0 {
		t.Fatalf("moves in the allowed direction: %+v", errs)
	}
	// GOV-33: a seat with a holder is not removed by an apply without it; seat.vacate first.
	var bot string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT id FROM bots WHERE tenant = 'demo' AND state = 'active' ORDER BY id LIMIT 1`).Scan(&bot); err != nil {
		t.Fatal(err)
	}
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE seats SET holder = $1, state = 'held' WHERE tenant = 'demo' AND id = 'commission:4'`, bot); err != nil {
		t.Fatal(err)
	}
	ms := cloneSet(t, b.applied)
	c := specOf(ms, "Governance", "demo")["commission"].(map[string]any)
	c["seats"] = c["seats"].([]any)[:3]
	if res, err := b.c.Apply(ctx, farmOf(b.op), ApplyRequest{Commit: strings.Repeat("6", 40), Manifests: ms}); err == nil && res.Applied {
		t.Fatal("an apply removed a seat with a holder")
	}
	if n := b.count(t, `SELECT count(*) FROM seats WHERE tenant = 'demo' AND id = 'commission:4' AND state = 'held' AND holder = $1`, bot); n != 1 {
		t.Fatal("the held seat changed")
	}
}

// The farm charter is the Governance of tenant farm, or one named farm without a tenant; a
// charter of another tenant named farm is a tenant charter and may not declare the manager seat
// (GOV-33).
func TestBackfillFarmCharterByTenant(t *testing.T) {
	t.Parallel()
	c := lintCore(t)
	charter := func(meta string) string {
		return "apiVersion: botaru/v1alpha1\nkind: Governance\nmetadata: " + meta + "\nspec:\n  farm_manager: {incident_stop: {decide_within: 3d}}\n"
	}
	for _, tc := range []struct {
		meta    string
		refused bool
	}{
		{"{name: farm, tenant: farm}", false},
		{"{name: farm}", false},
		{"{name: farm, tenant: demo}", true},
		{"{name: demo, tenant: demo}", true},
	} {
		if got := slices.Contains(lintCodes(t, c, charter(tc.meta)), "charter_farm_manager"); got != tc.refused {
			t.Errorf("%s: charter_farm_manager %v, want %v", tc.meta, got, tc.refused)
		}
	}
}
