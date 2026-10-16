// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
)

// The farm manager's advisory lens (GOV-25, GOV-17): while the manager seat is held, a proposal
// of a tenant that did not opt out gets a review of the farm_manager lens, on a worker of the
// tenant's seat role on behalf of the seat. The lens has weight 1, no veto, is not in min,
// high_risk or untrusted and is not needed for a quorum; it is paid after the other lenses and
// is skipped first when the review line runs short, and the verdict says so.
const (
	managerLens     = "farm_manager"
	managerLensType = "review.farm_manager.v1"
)

// managerLensExecutor is the executor of the farm_manager lens for a proposal, or none: the
// tenant works with the manager, the seat is held, the seat role claims the lens type, and
// the proposal is not a recusal case (GOV-17).
func (c *Core) managerLensExecutor(ctx context.Context, tx pgx.Tx, cat *Catalog, p *proposalRow) (lensExecutor, bool, error) {
	sr := seatRole(cat, p.Tenant)
	if !managerEnabled(cat, p.Tenant) || sr == "" || !slices.Contains(cat.Roles[sr].Claims, managerLensType) {
		return lensExecutor{}, false, nil
	}
	if _, ok := cat.TaskTypes[managerLensType]; !ok {
		return lensExecutor{}, false, nil
	}
	m, err := loadManagerSeat(ctx, tx, false)
	if err != nil || m == nil || m.State != "held" || m.Holder == "" {
		return lensExecutor{}, false, err
	}
	// GOV-17: not for a proposal of the seat holder's own bot.
	if asString(mmapAny(p.Rec["origin"])["acting_bot"]) == m.Holder {
		return lensExecutor{}, false, nil
	}
	if managerRecusal(cat, p.Tenant, p.body().Change) {
		return lensExecutor{}, false, nil
	}
	return lensExecutor{Lens: managerLens, Type: managerLensType, Role: sr, Source: "seat", Seat: managerSeatID}, true, nil
}

// managerRecusal says whether a change touches a target of MGR-16 (the manifest kinds the seat
// holder never changes, the seat role, the budget lines) or a role of profile farm-manager or
// farm-manager-reader (GOV-17).
func managerRecusal(cat *Catalog, tenant string, ch proposalChange) bool {
	if ch.ManifestChange == nil {
		return false
	}
	for _, op := range ch.ManifestChange.Ops {
		switch {
		case slices.Contains(selfChangeKinds, op.Target.Kind):
			return true
		case op.Target.Kind == "Role":
			if pr := cat.Roles[op.Target.Name].Profile; pr == "farm-manager" || pr == "farm-manager-reader" || op.Target.Name == seatRole(cat, tenant) {
				return true
			}
		case op.Target.Kind == "Tenant" && len(op.Field) >= len("spec.budgets") && op.Field[:len("spec.budgets")] == "spec.budgets":
			return true
		}
	}
	return false
}

// withManagerLens adds the farm_manager lens after the other lenses, so the veto lenses are
// enqueued and paid first (GOV-25).
func (c *Core) withManagerLens(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, p *proposalRow, lenses []string, exec map[string]lensExecutor) ([]string, map[string]lensExecutor, bool, error) {
	if p.body().Change.Kind == "none" || len(exec) == 0 {
		return lenses, exec, false, nil
	}
	e, ok, err := c.managerLensExecutor(ctx, tx, cat, p)
	if err != nil || !ok {
		return lenses, exec, false, err
	}
	out := make(map[string]lensExecutor, len(exec)+1)
	for k, v := range exec {
		out[k] = v
	}
	out[managerLens] = e
	return append(slices.Clone(lenses), managerLens), out, true, nil
}

// withoutManagerLens drops the farm_manager lens.
func withoutManagerLens(lenses []string, exec map[string]lensExecutor) ([]string, map[string]lensExecutor) {
	out := make(map[string]lensExecutor, len(exec))
	for k, v := range exec {
		if k != managerLens {
			out[k] = v
		}
	}
	return slices.DeleteFunc(slices.Clone(lenses), func(l string) bool { return l == managerLens }), out
}
