// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"math"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	// rollbackCard is the "Revert" card to the executor (PRP-23).
	rollbackCard = "rollback_card"
	// guardEvery is how often the core checks the guards of a running trial (PRP-23).
	guardEvery = 24 * time.Hour
)

// change says how a value moved against its value before, by the series tolerance: 1 better,
// -1 worse, 0 within the tolerance; ok is false when either side has fewer than min_samples
// points (PRP-22).
func change(s seriesDef, before, after map[string]any) (int, bool) {
	bv, ok1 := toFloat(before["value"])
	av, ok2 := toFloat(after["value"])
	bn, _ := toFloat(before["samples"])
	an, _ := toFloat(after["samples"])
	if !ok1 || !ok2 || s.Outcome == nil || int(bn) < s.Outcome.MinSamples || int(an) < s.Outcome.MinSamples {
		return 0, false
	}
	d := av - bv
	if s.Outcome.Better == "down" {
		d = -d
	}
	var tol float64
	switch {
	case s.Outcome.Compare == "window.share":
		d, tol = d*100, s.Outcome.TolerancePP
	case s.Outcome.TolerancePct > 0 && bv != 0:
		d, tol = d/math.Abs(bv)*100, s.Outcome.TolerancePct
	default:
		tol = s.Outcome.TolerancePP
	}
	switch {
	case d > tol:
		return 1, true
	case d < -tol:
		return -1, true
	}
	return 0, true
}

// trialStep runs a change trial (PRP-22, PRP-23): once a day the guards over the window so far,
// a guard beyond its tolerance in the bad direction marks it broken and sends the executor the
// "Revert" card; at the end of the window code computes the outcome.
func (c *Core) trialStep(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, tv *TenantView, p *proposalRow) error {
	t := mmapAny(p.Rec["trial"])
	w := mmapAny(t["window"])
	if t == nil || t["outcome"] != nil || w["starts_at"] == nil {
		return nil
	}
	start := parseTime(asString(w["starts_at"]))
	end := start.AddDate(0, 0, trialDays)
	checked := parseTime(asString(t["checked_at"]))
	if now.Before(end) && now.Sub(checked) < guardEvery {
		return nil
	}
	series := c.tenantSeries(cat, p.Tenant)
	origin := mmapAny(p.Rec["origin"])
	author := asString(origin["worker"])
	to := minTime(now, end)
	broke := false
	for _, g := range asList(t["guards"]) {
		gm := mmapAny(g)
		if gm["broken_at"] != nil {
			continue
		}
		s, ok := series[asString(gm["series"])]
		if !ok || s.Outcome == nil {
			continue
		}
		v, err := trialValue(ctx, tx, p.Tenant, s, asString(gm["subject"]), start, to, author, p.Role)
		if err != nil {
			return err
		}
		// A guard without its baseline points (a trial from before they were kept) has 0 and
		// is never broken (PRP-22).
		bn, _ := toFloat(gm["before_samples"])
		before := map[string]any{"value": gm["before"], "samples": bn}
		if d, ok := change(s, before, v); ok && d < 0 {
			gm["broken_at"], broke = fmtTime(now), true
		}
	}
	t["checked_at"] = fmtTime(now)
	if broke && t["state"] == "running" {
		t["state"] = "guard_broken"
	}
	if !now.Before(end) {
		if err := c.trialOutcome(ctx, tx, cat, p, t, series, start, end, author); err != nil {
			return err
		}
	}
	if err := c.saveProposal(ctx, tx, now, p); err != nil {
		return err
	}
	if broke || t["outcome"] == "worse" {
		r := mmapAny(p.Rec["realization"])
		if realizer := asString(r["realizer"]); realizer != "" && r["rollback_sent"] == nil {
			r["rollback_sent"] = fmtTime(now)
			if err := c.saveProposal(ctx, tx, now, p); err != nil {
				return err
			}
			return c.planProposalCard(ctx, tx, now, tv, p, rollbackCard, "human:"+realizer, nil)
		}
	}
	return nil
}

// trialOutcome computes the outcome at the end of the window (PRP-22): a broken guard gives
// worse; another change realized on the same subjects within the window gives mixed; else the
// metric over all points, without the author and without its role, by the tolerance.
func (c *Core) trialOutcome(ctx context.Context, tx pgx.Tx, cat *Catalog, p *proposalRow, t map[string]any, series map[string]seriesDef, start, end time.Time, author string) error {
	m := mmapAny(t["metric"])
	s, ok := series[asString(m["series"])]
	if !ok || s.Outcome == nil {
		// The metric left the catalog or lost its outcome block: only a broken guard still
		// gives an outcome.
		out := "undetermined"
		if guardBroken(t) {
			out = "worse"
		}
		t["state"], t["outcome"] = out, out
		return nil
	}
	after, err := trialValue(ctx, tx, p.Tenant, s, asString(m["subject"]), start, end, author, p.Role)
	if err != nil {
		return err
	}
	t["after"] = after
	out := ""
	switch {
	case guardBroken(t):
		out = "worse"
	default:
		mixed, err := otherChange(ctx, tx, p, t, start, end)
		if err != nil {
			return err
		}
		if mixed {
			out = "mixed"
		}
	}
	if out == "" {
		var selfOnly bool
		out, selfOnly = metricOutcome(s, mmapAny(t["before"]), after)
		t["self_measured"] = selfOnly
	}
	t["state"], t["outcome"] = out, out
	return nil
}

// metricOutcome is the outcome of the metric by the tolerance over all points, without the
// author and without its role (PRP-22); selfOnly says the points are only the author's or its
// role's own and the outcome comes from the rest. A filtered side with too few points but not
// none leaves the outcome undetermined.
func metricOutcome(s seriesDef, before, after map[string]any) (string, bool) {
	all, okAll := change(s, before, after)
	var results []int
	var undetermined, selfOnly bool
	if okAll {
		results = append(results, all)
	} else {
		undetermined = true
	}
	for _, k := range []string{"without_author", "without_role"} {
		d, ok := change(s, mmapAny(before[k]), mmapAny(after[k]))
		bn, _ := toFloat(mmapAny(before[k])["samples"])
		an, _ := toFloat(mmapAny(after[k])["samples"])
		switch {
		case ok:
			results = append(results, d)
		case okAll && bn == 0 && an == 0:
			// The points are the author's or its role's own: the outcome comes from the rest.
			selfOnly = true
		default:
			undetermined = true
		}
	}
	switch {
	case slices.Contains(results, -1):
		return "worse", selfOnly
	case undetermined:
		return "undetermined", selfOnly
	case len(results) > 0 && !slices.ContainsFunc(results, func(d int) bool { return d != 1 }):
		return "better", selfOnly
	}
	return "unchanged", selfOnly
}

// otherChange says whether another proposal was realized within the window on one of the
// trial's subjects.
func otherChange(ctx context.Context, tx pgx.Tx, p *proposalRow, t map[string]any, start, end time.Time) (bool, error) {
	var subjects []string
	for _, s := range asList(t["subjects"]) {
		subjects = append(subjects, asString(s))
	}
	var n int
	err := tx.QueryRow(ctx, `SELECT count(*) FROM proposals WHERE tenant = $1 AND id <> $2 AND state IN ('realized', 'reverted')
		AND (record->'realization'->>'realized_at')::timestamptz >= $3 AND (record->'realization'->>'realized_at')::timestamptz < $4
		AND EXISTS (SELECT 1 FROM jsonb_array_elements_text(coalesce(record->'trial'->'subjects', '[]'::jsonb)) s WHERE s = ANY($5))`,
		p.Tenant, p.ID, start, end, subjects).Scan(&n)
	return n > 0, err
}

func parseTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339, s)
	return t
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
