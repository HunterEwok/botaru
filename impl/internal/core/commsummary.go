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
)

// The weekly commission summary (COM-17, GOV-36): once an ISO week the governance loop sends the
// tenant admins with admin:governance the notice commission_weekly, built by code from numbers
// only: the accountability mode, the cases of GOV-38 and GOV-41 found in the week, the departures
// of the week and those that wait, and per seat of a proposal lens the accuracy of its holder in
// that lens (or insufficient) with the scored and missed forecasts. No buttons, no model text;
// in shadow too, since that is where people see the figures (GOV-36).

const commissionWeekly = "commission_weekly"

// weeklyStep sends the summary of the last 7 days once in an ISO week of the store clock, from
// the week governance_v03_since is recorded.
func (c *Core) weeklyStep(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, tv *TenantView) error {
	if !claimsV03() {
		return nil
	}
	since, _, err := v03State(ctx, tx, tv.Name)
	if err != nil || since == nil {
		return err
	}
	y, w := now.UTC().ISOWeek()
	week := fmt.Sprintf("%d-W%02d", y, w)
	first, err := markOnce(ctx, tx, now, tv.Name, "commission_weekly:"+week, 8*24*time.Hour)
	if err != nil || !first {
		return err
	}
	params, err := c.weeklyParams(ctx, tx, now, cat, tv)
	if err != nil {
		return err
	}
	params["week"] = week
	return c.noticeAdmins(ctx, tx, now, cat, []string{tv.Name}, commissionWeekly, "notice.commission_weekly", params, "admin:governance")
}

// weeklyParams are the numbers of the summary over the 7 days before now.
func (c *Core) weeklyParams(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, tv *TenantView) (map[string]any, error) {
	from := now.Add(-7 * 24 * time.Hour)
	var cases, done, waiting int
	// A case is a link to a rejected proposal that code found in the week (GOV-38) or a link
	// confirmed in the week with some holder answering for it (GOV-41); a link that is both
	// counts once.
	if err := tx.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM attributions WHERE tenant = $1 AND (kind = 'rejected_proposal' AND created_at >= $2
			OR state = 'confirmed' AND jsonb_array_length(coalesce(record->'answerable', '[]')) > 0 AND (record->'answer'->>'at')::timestamptz >= $2)),
		(SELECT count(*) FROM link_departures WHERE tenant = $1 AND state = 'done' AND done_at >= $2),
		(SELECT count(*) FROM link_departures WHERE tenant = $1 AND state = 'waiting')`, tv.Name, from).Scan(&cases, &done, &waiting); err != nil {
		return nil, err
	}
	seats := c.proposalSeats(charterSeats(cat, tv.Name))
	locale := c.pickLocale("", tv.Spec.Locale)
	var rows []string
	for _, id := range slices.Sorted(mapKeys(seats)) {
		var holder string
		if err := tx.QueryRow(ctx, `SELECT coalesce(holder, '') FROM seats WHERE tenant = $1 AND id = $2 AND state = 'held'`, tv.Name, id).Scan(&holder); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		row := map[string]any{"seat": id, "lens": seats[id], "accuracy": "vacant", "scored": 0, "missed": 0}
		if holder != "" {
			a, err := lensAccuracy(ctx, tx, now, tv.Name, seats[id], holder)
			if err != nil {
				return nil, err
			}
			row["accuracy"], row["scored"], row["missed"] = fmt.Sprint(a.view()), a.N, a.Missed
		}
		rows = append(rows, c.texts.Render(locale, "notice.commission_weekly.row", row))
	}
	return map[string]any{"tenant": tv.Name, "mode": accountabilityMode(cat, tv.Name), "cases": cases,
		"departures_done": done, "departures_waiting": waiting, "rows": cut(strings.Join(rows, "\n"), 2000)}, nil
}
