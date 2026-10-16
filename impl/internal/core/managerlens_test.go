// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"maps"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/manifest"
)

// While the manager seat is held, a proposal of a tenant gets the farm_manager lens after the
// other lenses, on the tenant's seat role on behalf of the seat (GOV-25); not for a change
// that touches the seat role or for a proposal of the holder's own bot (GOV-17). The lens
// weighs 1 and its review is not needed for the verdict.
func TestManagerLens(t *testing.T) {
	ctx := context.Background()
	b := newManagerBench(t)
	if _, err := b.c.SeatAssign(ctx, b.farmOp(), SeatRequest{Seat: managerSeatID}); err != nil {
		t.Fatal(err)
	}
	holder := b.managerSeat(t).Holder
	live := b.catalog(t)
	cat := *live
	cat.Roles = maps.Clone(live.Roles)
	cat.Roles["rev"] = manifest.Role{Profile: "reviewer", Claims: []string{managerLensType}}
	cat.TaskTypes = maps.Clone(live.TaskTypes)
	cat.TaskTypes[managerLensType] = manifest.TaskType{Purpose: "review"}
	cat.RawGlobals = maps.Clone(live.RawGlobals)
	cat.RawGlobals[manifest.Key{Kind: "Governance", Tenant: "demo", Name: "demo"}] = map[string]any{"spec": map[string]any{"commission": map[string]any{"seat_role": "rev"}}}
	prop := func(target, bot string) *proposalRow {
		return &proposalRow{Tenant: "demo", Rec: map[string]any{"origin": map[string]any{"acting_bot": bot}, "body": map[string]any{"change": map[string]any{
			"kind": "manifest_change", "manifest_change": map[string]any{"ops": []any{map[string]any{"op": "set", "target": map[string]any{"kind": "Role", "name": target}, "field": "spec.limits.max_parallel", "value": 2}}}}}}}
	}
	exec := map[string]lensExecutor{"security": {Lens: "security", Role: "reviewer-a"}}
	run := func(p *proposalRow) ([]string, map[string]lensExecutor) {
		var lenses []string
		var out map[string]lensExecutor
		if err := b.c.tx(ctx, func(tx pgx.Tx, now time.Time) (err error) {
			lenses, out, _, err = b.c.withManagerLens(ctx, tx, now, &cat, p, []string{"security"}, exec)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return lenses, out
	}
	lenses, out := run(prop("hello", "bot_other"))
	if len(lenses) != 2 || lenses[1] != managerLens || out[managerLens].Seat != managerSeatID || out[managerLens].Role != "rev" {
		t.Fatalf("lenses %v %v", lenses, out)
	}
	if lenses, _ := run(prop("rev", "bot_other")); len(lenses) != 1 {
		t.Fatalf("a change of the seat role got the manager lens: %v", lenses)
	}
	if lenses, _ := run(prop("hello", holder)); len(lenses) != 1 {
		t.Fatalf("the holder's own proposal got the manager lens: %v", lenses)
	}
	in := verdictInput{Risk: "low", Lenses: []string{"operational", "security", "value"}, VetoLens: map[string]bool{"operational": true, "security": true},
		Reviews: []lensReview{{Lens: "operational", State: "done", Score: 4}, {Lens: "security", State: "done", Score: 4}, {Lens: "value", State: "done", Score: 4}}}
	without := verdict(defaultPolicy(), in)
	in.Lenses = append(in.Lenses, managerLens)
	if v := verdict(defaultPolicy(), in); v["outcome"] != without["outcome"] || v["outcome"] != "recommended" {
		t.Fatalf("verdict without the manager review %v", v)
	}
	in.Reviews = append(in.Reviews, lensReview{Lens: managerLens, State: "done", Score: 1})
	if v := verdict(defaultPolicy(), in); v["weighted_mean"] != 3.25 {
		t.Fatalf("the manager lens weighs 1: %v", v)
	}
}
