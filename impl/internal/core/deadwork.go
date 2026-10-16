// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Dead work at scale (CAP-4): the groups of deadletter.list, and the auto-stop of a schedule
// whose last slots in a row ran out of attempts or runs.

// scheduleFailingSlots is the threshold of the auto-stop schedule_attempts_exhausted of
// spec/model/holds.yaml: K slots in a row.
const scheduleFailingSlots = 3

// DeadGroup is one group of p2-deadletter-list-response (CAP-4).
type DeadGroup struct {
	Tenant  string `json:"tenant"`
	Type    string `json:"type,omitempty"`
	Reason  string `json:"reason"`
	Count   int    `json:"count"`
	FirstAt string `json:"first_at,omitempty"`
	LastAt  string `json:"last_at,omitempty"`
}

// deadGroups groups the dead work of a tenant by (tenant, type, reason) with a count and the
// first and last time; a delivery's type is its kind (CAP-4).
func deadGroups(ctx context.Context, tx pgx.Tx, tenant string) ([]DeadGroup, error) {
	rows, err := tx.Query(ctx, `SELECT type, reason, count(*), min(at), max(at) FROM (
			SELECT type, reason, updated_at AS at FROM tasks WHERE tenant = $1 AND state = 'failed' AND reason = ANY($2)
			UNION ALL
			SELECT kind, coalesce(reason, 'attempts_exhausted'), updated_at FROM deliveries WHERE tenant = $1 AND state = 'dead') d
		GROUP BY 1, 2 ORDER BY 3 DESC, 1, 2 LIMIT 1000`, tenant, deadReasons)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DeadGroup{}
	for rows.Next() {
		g := DeadGroup{Tenant: tenant}
		var first, last time.Time
		if err := rows.Scan(&g.Type, &g.Reason, &g.Count, &first, &last); err != nil {
			return nil, err
		}
		g.FirstAt, g.LastAt = fmtTime(first), fmtTime(last)
		out = append(out, g)
	}
	return out, rows.Err()
}

// scheduleSlotEnded counts the outcome of the root task of a schedule slot (CAP-4): a slot that
// ended with attempts_exhausted or runs_exhausted adds one to the schedule's failing slots,
// any other end resets them. At scheduleFailingSlots in a row the schedule gets the auto-stop:
// a hold on scope schedule with no_enqueue, reason schedule_failing, author contain_auto, no
// until, and the alarm alarm.hold.auto; later slots are skipped with held (TIM-3). A tenant
// admin or the operator releases it (HLD-2); the count starts again from zero.
func (c *Core) scheduleSlotEnded(ctx context.Context, tx pgx.Tx, now time.Time, tenant, id string, rec []byte, reason string) error {
	var r treeRecord
	if err := json.Unmarshal(rec, &r); err != nil {
		return err
	}
	name, ok := strings.CutPrefix(r.EnqueuedBy, "schedule:")
	if !ok || (r.Root != "" && r.Root != id) {
		return nil
	}
	if reason != "attempts_exhausted" && reason != "runs_exhausted" {
		_, err := tx.Exec(ctx, `UPDATE schedule_state SET failed_slots = 0 WHERE tenant = $1 AND name = $2`, tenant, name)
		return err
	}
	var n int
	err := tx.QueryRow(ctx, `UPDATE schedule_state SET failed_slots = failed_slots + 1 WHERE tenant = $1 AND name = $2 RETURNING failed_slots`, tenant, name).Scan(&n)
	if err != nil || n < scheduleFailingSlots {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE schedule_state SET failed_slots = 0 WHERE tenant = $1 AND name = $2`, tenant, name); err != nil {
		return err
	}
	held, err := heldBy(ctx, tx, now, []string{"no_enqueue"}, scopeRef{kind: "schedule", tenant: tenant, name: name})
	if err != nil || held != "" {
		return err
	}
	actor := HoldActor{Principal: "system", Rank: "contain_auto", Tenant: tenant, Via: "system", Source: "schedule_attempts_exhausted"}
	if _, _, err := c.insertHold(ctx, tx, now, actor, HoldScope{Kind: "schedule", Tenant: tenant, Name: name}, []string{"no_enqueue"}, "schedule_failing", nil); err != nil {
		return err
	}
	return c.fireAlarm(ctx, tx, now, Alarm{Tenant: tenant, Name: "hold.auto", Subject: AlarmSubject{Kind: "schedule", ID: strings.ToLower(name)}, Severity: "page",
		Values: map[string]any{"reason": "schedule_failing", "slots": scheduleFailingSlots}})
}
