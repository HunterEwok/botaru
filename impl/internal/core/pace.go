// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// The digest pace of a chat (REP-10): digests to one chat binding go out no more often than
// its digest.max_per_hour, by default 1 per hour for a group and 4 for a private chat. A digest
// over the ceiling waits for the next free slot, and a newer digest to the same chat replaces
// the waiting one, which ends withheld(superseded); waiting for the pace is no failure (CAP-3).

const (
	defaultGroupDigestsPerHour  = 1
	defaultDirectDigestsPerHour = 4
)

// digestPace plans the run_at of a new digest to a chat of a channel, and withholds a digest
// to the same chat still waiting for the pace. It returns nil when the digest may go now.
func (c *Core) digestPace(ctx context.Context, tx pgx.Tx, now time.Time, tenant, channel, target string) (*time.Time, error) {
	limit := defaultDirectDigestsPerHour
	if strings.HasPrefix(target, "group:") {
		limit = defaultGroupDigestsPerHour
		var n *int
		err := tx.QueryRow(ctx, `SELECT digest_max_per_hour FROM chat_bindings WHERE tenant = $1 AND channel = $2 AND chat = $3`, tenant, channel, target).Scan(&n)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		if n != nil && *n > 0 {
			limit = *n
		}
	}
	// A digest still waiting for the pace is replaced by the newer one, which takes its slot.
	var waiting string
	var slot time.Time
	err := tx.QueryRow(ctx, `SELECT id, run_at FROM deliveries WHERE tenant = $1 AND kind = 'digest' AND exec_name = $2 AND target_id = $3
		AND state = 'planned' AND paced AND run_at > $4 ORDER BY run_at LIMIT 1 FOR UPDATE`, tenant, channel, target, now).Scan(&waiting, &slot)
	if err == nil {
		if _, err := tx.Exec(ctx, `UPDATE deliveries SET state = 'withheld', reason = 'superseded', updated_at = $2 WHERE id = $1`, waiting, now); err != nil {
			return nil, err
		}
		if err := c.appendEvent(ctx, tx, now, tenant, Event{Type: "delivery.withheld", Subject: waiting,
			Data: map[string]any{"delivery": waiting, "kind": "digest", "reason": "superseded"}}); err != nil {
			return nil, err
		}
		return &slot, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	// The digests of the last hour, the ones about to go included, by the time they went.
	rows, err := tx.Query(ctx, `SELECT coalesce(handed_at, $5) FROM deliveries WHERE tenant = $1 AND kind = 'digest' AND exec_name = $2 AND target_id = $3
		AND (handed_at > $4 OR (handed_at IS NULL AND state IN ('planned', 'sending'))) ORDER BY 1 DESC`, tenant, channel, target, now.Add(-time.Hour), now)
	if err != nil {
		return nil, err
	}
	var sent []time.Time
	for rows.Next() {
		var t time.Time
		if err := rows.Scan(&t); err != nil {
			rows.Close()
			return nil, err
		}
		sent = append(sent, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(sent) < limit {
		return nil, nil
	}
	// The next free slot: an hour after the limit-th latest digest.
	next := sent[limit-1].Add(time.Hour)
	return &next, nil
}
