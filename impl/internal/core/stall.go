// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// stalls ends the attempts that made no progress for longer than their type's stall_after
// (CNT-4): the latest of the attempt start, the last finished tool or model call and the last
// change of the heartbeat progress. A tool call still queued or leased does not count as
// progress, but while it runs the attempt is not stalled: the call's own deadlines hold it.
// A stalled attempt is spent and retried, and the core raises alarm.task.stalled, which
// resolves when the task is no longer stalled.
func (c *Core) stalls(ctx context.Context) error {
	cat, err := c.Catalog(ctx)
	if err != nil {
		return err
	}
	var types, afters []string
	for name, tt := range cat.TaskTypes {
		if d, ok := parseDuration(tt.Timeouts.StallAfter); ok {
			types, afters = append(types, name), append(afters, fmtTime(time.Unix(0, 0).Add(d)))
		}
	}
	for {
		done := true
		err := c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
			var l leaseRow
			err := tx.QueryRow(ctx, `SELECT l.work_id, l.lease_no, l.kind, l.tenant, l.worker_id, l.epoch, l.state, l.lease_until
				FROM leases l JOIN tasks t ON t.id = l.work_id
				JOIN unnest($1::text[], $2::timestamptz[]) AS s(type, after) ON s.type = t.type
				WHERE l.state = 'active' AND l.kind = 'attempt'
				AND NOT EXISTS (SELECT 1 FROM toolcalls tc WHERE tc.task = l.work_id AND tc.state IN ('queued', 'leased'))
				AND greatest(l.created_at, coalesce(l.progress_at, l.created_at),
					coalesce((SELECT max(tc.ended_at) FROM toolcalls tc WHERE tc.task = l.work_id AND tc.attempt_lease_no = l.lease_no AND tc.state IN ('done', 'error')), l.created_at),
					coalesce((SELECT max(sp.time) FROM spend sp WHERE sp.task = l.work_id AND sp.unit <> 'task' AND sp.state = 'charged' AND sp.time >= l.created_at), l.created_at))
					+ (s.after - 'epoch'::timestamptz) <= $3
				ORDER BY l.created_at LIMIT 1 FOR UPDATE OF l SKIP LOCKED`, types, afters, now).
				Scan(&l.WorkID, &l.LeaseNo, &l.Kind, &l.Tenant, &l.WorkerID, &l.Epoch, &l.State, &l.Until)
			if err == pgx.ErrNoRows {
				return nil
			}
			if err != nil {
				return err
			}
			done = false
			if _, err := tx.Exec(ctx, `UPDATE leases SET state = 'revoked', reason = 'stalled', closed_at = $3 WHERE work_id = $1 AND lease_no = $2`,
				l.WorkID, l.LeaseNo, now); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `DELETE FROM handles WHERE work_id = $1 AND lease_no = $2`, l.WorkID, l.LeaseNo); err != nil {
				return err
			}
			if err := c.fireAlarm(ctx, tx, now, Alarm{Tenant: l.Tenant, Name: "task.stalled", Subject: AlarmSubject{Kind: "task", ID: strings.ToLower(l.WorkID)}, Severity: "warn"}); err != nil {
				return err
			}
			_, err = c.endAttempt(ctx, tx, now, l.Tenant, &l, "stalled", true, true, 0, "")
			return err
		})
		if err != nil {
			return err
		}
		if done {
			return c.resolveStalls(ctx)
		}
	}
}

// resolveStalls resolves alarm.task.stalled of a task that ended or runs a new attempt.
func (c *Core) resolveStalls(ctx context.Context) error {
	return c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		rows, err := tx.Query(ctx, `SELECT e.tenant, e.subject->>'id' FROM alarm_episodes e
			WHERE e.state = 'firing' AND e.alarm LIKE 'alarm.task.stalled.%' AND NOT EXISTS (
				SELECT 1 FROM tasks t WHERE lower(t.id) = e.subject->>'id' AND t.tenant = e.tenant AND t.state = 'submitted'
				AND NOT EXISTS (SELECT 1 FROM leases l WHERE l.work_id = t.id AND l.state = 'active'))`)
		if err != nil {
			return err
		}
		type stalled struct{ tenant, id string }
		var ss []stalled
		for rows.Next() {
			var s stalled
			if err := rows.Scan(&s.tenant, &s.id); err != nil {
				rows.Close()
				return err
			}
			ss = append(ss, s)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, s := range ss {
			if err := c.resolveAlarm(ctx, tx, now, Alarm{Tenant: s.tenant, Name: "task.stalled", Subject: AlarmSubject{Kind: "task", ID: s.id}}); err != nil {
				return err
			}
		}
		return nil
	})
}
