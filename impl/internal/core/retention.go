// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// The default periods of the retention classes that the v0.1 loop deletes
// (spec/model/retention-classes.yaml, RET-1, RET-2).
const (
	sessionsNoMemoryDays    = 7
	retentionBatch          = 500
	retentionMaxBatchesTick = 20
)

// retentionLoop deletes content past the default period of class content without an erasure
// request (RET-2): the body and result of a finished task (its row stays as a tombstone that
// keeps the idempotency key, RET-3), the body of a finished delivery, the arguments and output
// of a finished tool call, a session that has not changed for the period (7 days for a role
// without memory), and the bytes of a file past its expiry or the period. Metadata (audit,
// events, usage lines, decisions) stays. The class observation has no store in this build.
// Each pass deletes in bounded batches, so a long backlog does not hold one transaction.
func (c *Core) retentionLoop(ctx context.Context) error {
	cat, err := c.Catalog(ctx)
	if err != nil {
		return err
	}
	var pass bool
	if err := c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		pass = c.slowPass(&c.retentionAt, now)
		return nil
	}); err != nil || !pass {
		return err
	}
	var noMemory []string
	for name, r := range cat.Roles {
		if roleMemory(r) == "none" {
			noMemory = append(noMemory, name)
		}
	}
	steps := []func(tx pgx.Tx, now time.Time) (int64, error){
		func(tx pgx.Tx, now time.Time) (int64, error) {
			tag, err := tx.Exec(ctx, `DELETE FROM activity_outcomes WHERE (tenant, key) IN (SELECT tenant, key FROM activity_outcomes
				WHERE created_at <= $1 LIMIT $2 FOR UPDATE SKIP LOCKED)`, now.AddDate(0, 0, -contentDays), retentionBatch)
			return tag.RowsAffected(), err
		},
		func(tx pgx.Tx, now time.Time) (int64, error) {
			tag, err := tx.Exec(ctx, `UPDATE tasks SET body = NULL, result = NULL WHERE id IN (SELECT id FROM tasks
				WHERE (body IS NOT NULL OR result IS NOT NULL) AND state IN ('completed', 'failed', 'canceled') AND updated_at <= $1
				ORDER BY updated_at LIMIT $2 FOR UPDATE SKIP LOCKED)`, now.AddDate(0, 0, -contentDays), retentionBatch)
			return tag.RowsAffected(), err
		},
		func(tx pgx.Tx, now time.Time) (int64, error) {
			tag, err := tx.Exec(ctx, `UPDATE deliveries SET body = NULL WHERE id IN (SELECT id FROM deliveries
				WHERE body IS NOT NULL AND state IN ('delivered', 'dead', 'withheld') AND updated_at <= $1
				LIMIT $2 FOR UPDATE SKIP LOCKED)`, now.AddDate(0, 0, -contentDays), retentionBatch)
			return tag.RowsAffected(), err
		},
		func(tx pgx.Tx, now time.Time) (int64, error) {
			tag, err := tx.Exec(ctx, `UPDATE toolcalls SET args = '{}', output = NULL WHERE id IN (SELECT id FROM toolcalls
				WHERE (args <> '{}' OR output IS NOT NULL) AND state NOT IN ('requested', 'queued', 'leased') AND updated_at <= $1
				LIMIT $2 FOR UPDATE SKIP LOCKED)`, now.AddDate(0, 0, -contentDays), retentionBatch)
			return tag.RowsAffected(), err
		},
		func(tx pgx.Tx, now time.Time) (int64, error) {
			tag, err := tx.Exec(ctx, `DELETE FROM sessions WHERE id IN (SELECT id FROM sessions
				WHERE updated_at <= $1 OR (role = ANY($2) AND updated_at <= $3) LIMIT $4 FOR UPDATE SKIP LOCKED)`,
				now.AddDate(0, 0, -contentDays), noMemory, now.AddDate(0, 0, -sessionsNoMemoryDays), retentionBatch)
			return tag.RowsAffected(), err
		},
		func(tx pgx.Tx, now time.Time) (int64, error) {
			tag, err := tx.Exec(ctx, `UPDATE files SET state = 'expired', content = NULL WHERE file_ref IN (SELECT file_ref FROM files
				WHERE state = 'stored' AND (expires_at <= $1 OR (expires_at IS NULL AND created_at <= $2)) LIMIT $3 FOR UPDATE SKIP LOCKED)`,
				now, now.AddDate(0, 0, -contentDays), retentionBatch)
			return tag.RowsAffected(), err
		},
		// An expired file has no bytes left; the next step records expired -> purged and the
		// record stays as the tombstone (spec/lifecycles/file.md, RET-2).
		func(tx pgx.Tx, now time.Time) (int64, error) {
			tag, err := tx.Exec(ctx, `UPDATE files SET state = 'purged', content = NULL WHERE file_ref IN (SELECT file_ref FROM files
				WHERE state = 'expired' LIMIT $1 FOR UPDATE SKIP LOCKED)`, retentionBatch)
			return tag.RowsAffected(), err
		},
	}
	for _, step := range steps {
		for range retentionMaxBatchesTick {
			var n int64
			if err := c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
				var err error
				n, err = step(tx, now)
				return err
			}); err != nil {
				return err
			}
			if n < retentionBatch {
				break
			}
		}
	}
	return nil
}
