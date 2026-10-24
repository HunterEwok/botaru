// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/manifest"
	"github.com/HunterEwok/botaru/impl/internal/store"
)

// Accountability from v0.3 (GOV-36, D-116): the paths of the v0.3 rules run when the core claims
// v0.3. The first apply of a tenant on such a core writes governance.v03_enabled, whose time is
// governance_v03_since; only reviews, verdicts and confirmations after it count. The tenant
// charter's commission.accountability is shadow (code computes and shows to people only) or on,
// and apply accepts on only while every seat has a holder with enough scored forecasts.

const (
	// defaultMinForecasts is commission.accountability_min_forecasts when the charter sets none,
	// and minMinForecasts the lowest value it may take (GOV-36, D-116).
	defaultMinForecasts = 20
	minMinForecasts     = 8
	// readyEvery is how often accountability_ready repeats while it holds (GOV-36).
	readyEvery = 30 * 24 * time.Hour
	// accuracyDays is the window of scored forecasts of the accuracy (COM-17).
	accuracyDays = 180
)

// accountabilityMode is commission.accountability of the tenant charter: shadow or on (GOV-36).
func accountabilityMode(cat *Catalog, tenant string) string {
	if mstr(mmap(tenantSpecOf(cat, "Governance", tenant), "commission"), "accountability") == "on" {
		return "on"
	}
	return "shadow"
}

// minForecasts is commission.accountability_min_forecasts: 20 by default, only lowered and never
// below 8 (GOV-36, D-116).
func minForecasts(cat *Catalog, tenant string) int {
	n, ok := mnum(mmap(tenantSpecOf(cat, "Governance", tenant), "commission"), "accountability_min_forecasts")
	if !ok {
		return defaultMinForecasts
	}
	return min(max(int(n), minMinForecasts), defaultMinForecasts)
}

// v03State is governance_v03_since and accountability_on_since of a tenant, nil when not set.
func v03State(ctx context.Context, q store.Querier, tenant string) (since, onSince *time.Time, err error) {
	err = q.QueryRow(ctx, `SELECT since, accountability_on_since FROM governance_v03 WHERE tenant = $1`, tenant).Scan(&since, &onSince)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, nil
	}
	return since, onSince, err
}

// accountabilityOn says whether departures and cards of accountability run in the tenant: a v0.3
// core, the charter mode on and its first apply recorded (GOV-36). It returns the time from which
// incidents and confirmations lead to departures.
func (c *Core) accountabilityOn(ctx context.Context, q store.Querier, cat *Catalog, tenant string) (*time.Time, error) {
	if accountabilityMode(cat, tenant) != "on" {
		return nil, nil
	}
	_, on, err := v03State(ctx, q, tenant)
	return on, err
}

// applyV03 writes, in the apply transaction of a v0.3 core, governance.v03_enabled at the first
// apply of each tenant and commission.accountability_on at the first apply with on (GOV-36).
func (c *Core) applyV03(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog) error {
	for _, t := range slices.Sorted(mapKeys(cat.Tenants)) {
		tag, err := tx.Exec(ctx, `INSERT INTO governance_v03 (tenant, since, updated_at) VALUES ($1, $2, $2) ON CONFLICT DO NOTHING`, t, now)
		if err != nil {
			return err
		}
		if tag.RowsAffected() > 0 {
			if err := c.govAudit(ctx, tx, now, t, "governance.v03_enabled", "tenant", t, "", map[string]any{"since": fmtTime(now)}); err != nil {
				return err
			}
		}
		if accountabilityMode(cat, t) != "on" {
			continue
		}
		tag, err = tx.Exec(ctx, `UPDATE governance_v03 SET accountability_on_since = $2, updated_at = $2 WHERE tenant = $1 AND accountability_on_since IS NULL`, t, now)
		if err != nil {
			return err
		}
		if tag.RowsAffected() > 0 {
			if err := c.govAudit(ctx, tx, now, t, "commission.accountability_on", "tenant", t, "", map[string]any{"since": fmtTime(now)}); err != nil {
				return err
			}
		}
	}
	return nil
}

// accountabilityLint refuses a charter that switches commission.accountability to on while a
// seat of the charter has no holder or a holder has fewer scored forecasts than
// accountability_min_forecasts (accountability_not_ready, GOV-36). A charter of a tenant that is
// already on is accepted again, so a seat a link freed does not block every later apply; going
// back to shadow is always accepted.
func (c *Core) accountabilityLint(ctx context.Context, docs []manifest.Doc, index []int, current *Catalog) ([]ApplyError, error) {
	var out []ApplyError
	now, err := storeNow(ctx, c.st.Pool)
	if err != nil {
		return nil, err
	}
	for i, d := range docs {
		if d.Kind != "Governance" {
			continue
		}
		sp, _ := d.Value["spec"].(map[string]any)
		com := mmap(sp, "commission")
		if mstr(com, "accountability") != "on" {
			continue
		}
		if current != nil && accountabilityMode(current, d.Tenant) == "on" {
			if _, on, err := v03State(ctx, c.st.Pool, d.Tenant); err != nil {
				return nil, err
			} else if on != nil {
				continue
			}
		}
		need := defaultMinForecasts
		if n, ok := mnum(com, "accountability_min_forecasts"); ok {
			need = min(max(int(n), minMinForecasts), defaultMinForecasts)
		}
		ready, _, err := seatsReady(ctx, c.st.Pool, now, d.Tenant, c.proposalSeats(charterSeatsOf(com)), need)
		if err != nil {
			return nil, err
		}
		if !ready {
			out = append(out, ApplyError{Kind: d.Kind, Tenant: d.Tenant, Name: d.Name, Code: "accountability_not_ready", Path: fmt.Sprintf("/manifests/%d/spec/commission/accountability", index[i])})
		}
	}
	return out, nil
}

// charterSeatsOf are the seat ids of a commission block with their lens.
func charterSeatsOf(com map[string]any) map[string]string {
	out := map[string]string{}
	for _, s := range mlist(com, "seats") {
		sm, _ := s.(map[string]any)
		if n, ok := toFloat(sm["id"]); ok {
			out[fmt.Sprintf("commission:%d", int(n))] = mstr(sm, "lens")
		}
	}
	return out
}

// proposalSeats are the seats whose lens assesses proposals (subject proposal in
// spec/model/lenses.yaml): only they write forecasts, so a seat of the necessity lens, which
// assesses escalations, does not hold accountability back (GOV-36, COM-13).
func (c *Core) proposalSeats(seats map[string]string) map[string]string {
	reg, err := c.spec.Registry("spec/model/lenses.yaml")
	if err != nil {
		return seats
	}
	out := map[string]string{}
	for id, lens := range seats {
		if e, ok := reg.Entry(lens); ok && e["subject"] != "proposal" {
			continue
		}
		out[id] = lens
	}
	return out
}

// seatsReady says whether every seat has a holder with at least need scored forecasts in its
// lens over the accuracy window, and gives the accuracy of every held seat (GOV-36).
func seatsReady(ctx context.Context, q store.Querier, now time.Time, tenant string, seats map[string]string, need int) (bool, map[string]accuracy, error) {
	accs := map[string]accuracy{}
	ready := len(seats) > 0
	for _, id := range slices.Sorted(mapKeys(seats)) {
		var holder string
		err := q.QueryRow(ctx, `SELECT coalesce(holder, '') FROM seats WHERE tenant = $1 AND id = $2 AND state = 'held'`, tenant, id).Scan(&holder)
		if errors.Is(err, pgx.ErrNoRows) || err == nil && holder == "" {
			ready = false
			continue
		}
		if err != nil {
			return false, nil, err
		}
		a, err := lensAccuracy(ctx, q, now, tenant, seats[id], holder)
		if err != nil {
			return false, nil, err
		}
		accs[id] = a
		if a.N < need {
			ready = false
		}
	}
	return ready, accs, nil
}

// readyStep sends the tenant admins with admin:governance the notice accountability_ready while
// the mode is shadow and every seat is held with enough scored forecasts, once a month (GOV-36).
func (c *Core) readyStep(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, tv *TenantView) error {
	if accountabilityMode(cat, tv.Name) != "shadow" {
		return nil
	}
	var sent *time.Time
	err := tx.QueryRow(ctx, `SELECT ready_sent_at FROM governance_v03 WHERE tenant = $1 FOR UPDATE`, tv.Name).Scan(&sent)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil || sent != nil && now.Sub(*sent) < readyEvery {
		return err
	}
	ready, accs, err := seatsReady(ctx, tx, now, tv.Name, c.proposalSeats(charterSeats(cat, tv.Name)), minForecasts(cat, tv.Name))
	if err != nil || !ready {
		return err
	}
	var lines []string
	for _, id := range slices.Sorted(mapKeys(accs)) {
		lines = append(lines, id+" "+accs[id].text())
	}
	if _, err := tx.Exec(ctx, `UPDATE governance_v03 SET ready_sent_at = $2, updated_at = $2 WHERE tenant = $1`, tv.Name, now); err != nil {
		return err
	}
	return c.noticeAdmins(ctx, tx, now, cat, []string{tv.Name}, "accountability_ready", "notice.accountability_ready",
		map[string]any{"tenant": tv.Name, "seats": cut(strings.Join(lines, "; "), 500), "field": "commission.accountability"}, "admin:governance")
}

// storeNow is the store clock outside a transaction.
func storeNow(ctx context.Context, q store.Querier) (time.Time, error) {
	var now time.Time
	err := q.QueryRow(ctx, `SELECT botaru_now()`).Scan(&now)
	return now, err
}
