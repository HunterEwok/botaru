// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
)

// goalsWindow is the period the goal numbers of doctor cover.
const goalsWindow = 7 * 24 * time.Hour

// refusalReasons are the final reasons of a task the farm stopped by a rule (a right lost, the
// person turned off, a money limit): the goal G1 counts them apart, not as failures. A refusal
// at enqueue leaves no task.
var refusalReasons = []string{"rights_revoked", "person_inactive", "budget_exhausted", "provider_spend_cap"}

// moneyReasons are the final reasons of a task a money limit stopped (G3).
var moneyReasons = []string{"budget_exhausted", "provider_spend_cap"}

// doctorGoals shows, for the tenant of the request, the numbers the farm goals G1 to G6 are
// measured by, over the last goalsWindow (PRP-29). The checks only show: each is ok, with
// severity info, and a person compares the value with the goal's threshold. The farm scope
// leaves them out, so the checks of many tenants stay within the response; the farm operator
// names a tenant to see them.
func (c *Core) doctorGoals(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, tenant string, add func(DoctorCheck)) error {
	from := now.Add(-goalsWindow)
	for _, t := range slices.Sorted(mapKeys(cat.Tenants)) {
		if t != tenant {
			continue
		}
		show := func(id string, v any) {
			add(DoctorCheck{ID: "goals." + id, Tenant: t, OK: true, Severity: "info", Value: v})
		}
		// G1: the tasks people asked for themselves that ended, refusals apart, and their queue
		// wait; child tasks, continuations and campaign items are the farm's own steps.
		var done, failed, refused int
		var wait *float64
		if err := tx.QueryRow(ctx, `SELECT
				count(*) FILTER (WHERE state = 'completed'),
				count(*) FILTER (WHERE state = 'failed' AND NOT coalesce(reason, '') = ANY($3)),
				count(*) FILTER (WHERE state IN ('failed', 'canceled') AND coalesce(reason, '') = ANY($3)),
				percentile_cont(0.95) WITHIN GROUP (ORDER BY extract(epoch FROM
					(SELECT min(l.created_at) FROM leases l WHERE l.work_id = t.id AND l.kind = 'attempt') - t.created_at))
			FROM tasks t WHERE tenant = $1 AND updated_at >= $2 AND state IN ('completed', 'failed', 'canceled')
				AND coalesce(t.record->>'parent', '') = '' AND coalesce(t.record->>'campaign', '') = '' AND EXISTS (SELECT 1 FROM principals p WHERE p.tenant = t.tenant AND p.id = t.initiator AND p.kind = 'human')`,
			t, from, refusalReasons).Scan(&done, &failed, &refused, &wait); err != nil {
			return err
		}
		if done+failed > 0 {
			show("g1.done_share", float64(done)/float64(done+failed))
		} else {
			show("g1.done_share", nil)
		}
		show("g1.refused", refused)
		if wait != nil {
			show("g1.queue_wait_p95_s", *wait)
		} else {
			show("g1.queue_wait_p95_s", nil)
		}
		// G3: the tasks a money limit stopped.
		var stops int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE tenant = $1 AND updated_at >= $2 AND state = 'failed' AND reason = ANY($3)`,
			t, from, moneyReasons).Scan(&stops); err != nil {
			return err
		}
		show("g3.limit_stops", stops)
		// G4: the incidents of severity sev1 opened, and those the manager could have prevented.
		var sev1, preventable int
		if err := tx.QueryRow(ctx, `SELECT count(*) FILTER (WHERE record->>'severity' = 'sev1'),
				count(*) FILTER (WHERE record->>'manager_preventable' = 'true')
			FROM incidents WHERE tenant = $1 AND opened_at >= $2 AND state <> 'dismissed'`, t, from).Scan(&sev1, &preventable); err != nil {
			return err
		}
		show("g4.sev1_incidents", sev1)
		show("g4.preventable", preventable)
		// G5: accepted changes by their trial outcome, since the start, and the worse ones in force.
		var better, worse int
		if err := tx.QueryRow(ctx, `SELECT count(*) FILTER (WHERE record->'trial'->>'outcome' = 'better'),
				count(*) FILTER (WHERE record->'trial'->>'outcome' = 'worse')
			FROM proposals WHERE tenant = $1 AND state IN ('realized', 'reverted')`, t).Scan(&better, &worse); err != nil {
			return err
		}
		inForce, err := worseInForce(ctx, tx, t)
		if err != nil {
			return err
		}
		show("g5.better", better)
		show("g5.worse", worse)
		show("g5.worse_in_force", len(inForce))
		// G6: the most cards one person got in one day.
		var most int
		if err := tx.QueryRow(ctx, `SELECT coalesce(max(n), 0) FROM (SELECT count(*) AS n FROM deliveries
			WHERE tenant = $1 AND created_at >= $2 AND recipient IS NOT NULL AND body->>'form' = 'card' AND kind <> $3
			GROUP BY recipient, date_trunc('day', created_at)) d`, t, from, cardUpdate).Scan(&most); err != nil {
			return err
		}
		show("g6.cards_max_per_person_day", most)
	}
	return nil
}
