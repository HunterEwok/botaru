// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
)

// The decision statistics of people (DIG-4). Every "Accept" or "Reject" of a person writes a
// point of farm.proposals.accept_share (accepted or declined), and every "Reject" also a point
// of farm.proposals.decline_reason_share (the reason code of decline-reasons.yaml), on the
// subject role:<author role> with the proposal category as the second dimension. The points
// are input for the analysis and for people: the core changes nothing because of them, and no
// threshold, max_per_week, instruction, skill or manifest follows them (INJ-10).
const (
	seriesAcceptShare  = "farm.proposals.accept_share"
	seriesDeclineShare = "farm.proposals.decline_reason_share"
	// decisionStatsDays is the window of the statistics in P2 commission.status.
	decisionStatsDays = 90
)

// decisionSeries is the definition of a decision statistics series: a series of a closed list
// with its raw points kept for the longest read window; accept_share has accepted as its good
// value, so window.share reads the accept share of a role. Neither is a trial metric.
func (c *Core) decisionSeries(name string) (seriesDef, bool) {
	d := seriesDef{Name: name, CatalogKind: "role", DataClass: coreSeriesMark, Source: "core", RawDays: obsRollupDays, RollDays: obsRollupDays}
	switch name {
	case seriesAcceptShare:
		d.Enum = []string{"accepted", "declined"}
		d.Outcome = &seriesOutcome{Compare: "window.share", Good: "accepted", Better: "up"}
	case seriesDeclineShare:
		reg, err := c.spec.Registry("spec/model/decline-reasons.yaml")
		if err != nil {
			return d, false
		}
		for _, e := range reg.Entries {
			if e["since"] == "v0.1" {
				d.Enum = append(d.Enum, asString(e["id"]))
			}
		}
	default:
		return d, false
	}
	return d, true
}

// decisionPoints writes the statistics points of one decision of a person on a proposal.
func decisionPoints(ctx context.Context, tx pgx.Tx, now time.Time, p *proposalRow, decision, reason string) error {
	var outcome string
	switch decision {
	case "accept", "override_veto":
		outcome = "accepted"
	case "reject":
		outcome = "declined"
	default:
		return nil
	}
	cat := p.body().Category
	pt := obsPoint{Tenant: p.Tenant, Series: seriesAcceptShare, Subject: "role:" + p.Role, Source: "core", Mark: coreSeriesMark,
		At: now, Enum: outcome, Key: "decision:" + p.ID}
	if err := writeDimPoint(ctx, tx, now, pt, cat); err != nil {
		return err
	}
	if outcome != "declined" {
		return nil
	}
	pt.Series, pt.Enum = seriesDeclineShare, reason
	return writeDimPoint(ctx, tx, now, pt, cat)
}

// writeDimPoint writes a point once, with its second dimension.
func writeDimPoint(ctx context.Context, tx pgx.Tx, now time.Time, p obsPoint, dim string) error {
	_, err := tx.Exec(ctx, `INSERT INTO obs_points (tenant, series, subject, at, enum, source, data_mark, key, created_at, dim)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10) ON CONFLICT DO NOTHING`,
		p.Tenant, p.Series, p.Subject, p.At, p.Enum, p.Source, p.Mark, p.Key, now, nullable(dim))
	return err
}

// decisionStats computes the statistics by role and category from the points of the window:
// for each pair the number of decisions, the accept share and the share of every decline
// reason among the declined ones (DIG-4).
func decisionStats(ctx context.Context, q pgx.Tx, now time.Time, tenant string) ([]any, error) {
	rows, err := q.Query(ctx, `SELECT series, subject, coalesce(dim, ''), enum, count(*) FROM obs_points
		WHERE tenant = $1 AND series = ANY($2) AND at >= $3 GROUP BY 1, 2, 3, 4 ORDER BY 2, 3, 1, 4`,
		tenant, []string{seriesAcceptShare, seriesDeclineShare}, now.AddDate(0, 0, -decisionStatsDays))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type key struct{ role, category string }
	type agg struct {
		accepted, declined int
		reasons            map[string]int
	}
	stats := map[key]*agg{}
	var order []key
	for rows.Next() {
		var series, subject, dim, enum string
		var n int
		if err := rows.Scan(&series, &subject, &dim, &enum, &n); err != nil {
			return nil, err
		}
		k := key{role: subject[len("role:"):], category: dim}
		a := stats[k]
		if a == nil {
			a = &agg{reasons: map[string]int{}}
			stats[k] = a
			order = append(order, k)
		}
		switch {
		case series == seriesAcceptShare && enum == "accepted":
			a.accepted += n
		case series == seriesAcceptShare:
			a.declined += n
		default:
			a.reasons[enum] += n
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := []any{}
	for _, k := range order {
		a := stats[k]
		decided := a.accepted + a.declined
		row := map[string]any{"role": k.role, "category": k.category, "decided": decided, "accept_share": nil, "decline_reason_share": map[string]any{}}
		if decided > 0 {
			row["accept_share"] = float64(a.accepted) / float64(decided)
		}
		shares := row["decline_reason_share"].(map[string]any)
		for _, r := range slices.Sorted(mapKeys(a.reasons)) {
			if a.declined > 0 {
				shares[r] = float64(a.reasons[r]) / float64(a.declined)
			}
		}
		out = append(out, row)
	}
	return out, nil
}
