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
// over the ceiling waits for the next free slot (marked paced), and a newer digest to the same
// chat replaces the waiting one, which ends withheld(superseded); waiting for the pace is no
// failure (CAP-3). The ceiling applies again at hand-out, so digests that a person's
// preferences held back do not all go at once; only digests are ever replaced.

const (
	defaultGroupDigestsPerHour  = 1
	defaultDirectDigestsPerHour = 4
)

// digestLimit is the digest ceiling per hour of a chat of a channel.
func digestLimit(ctx context.Context, tx pgx.Tx, tenant, channel, target string) (int, error) {
	if !strings.HasPrefix(target, "group:") {
		return defaultDirectDigestsPerHour, nil
	}
	var n *int
	err := tx.QueryRow(ctx, `SELECT digest_max_per_hour FROM chat_bindings WHERE tenant = $1 AND channel = $2 AND chat = $3`, tenant, channel, target).Scan(&n)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return 0, err
	}
	if n != nil && *n > 0 {
		return *n, nil
	}
	return defaultGroupDigestsPerHour, nil
}

// digestsLastHour are the times of the digests to a chat in the last hour, latest first: the
// handed-out ones, and with planned also the ones about to go, at now; exclude leaves one out.
func digestsLastHour(ctx context.Context, tx pgx.Tx, now time.Time, tenant, channel, target string, planned bool, exclude string) ([]time.Time, error) {
	rows, err := tx.Query(ctx, `SELECT coalesce(handed_at, $5) FROM deliveries WHERE tenant = $1 AND kind = 'digest' AND exec_name = $2 AND target_id = $3
		AND (handed_at > $4 OR ($6 AND handed_at IS NULL AND (state = 'sending' OR state = 'planned' AND NOT paced AND (run_at IS NULL OR run_at <= $5))))
		AND id <> $7 ORDER BY 1 DESC`, tenant, channel, target, now.Add(-time.Hour), now, planned, exclude)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var sent []time.Time
	for rows.Next() {
		var t time.Time
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		sent = append(sent, t)
	}
	return sent, rows.Err()
}

// digestPace is the pace of a new digest to a chat at planning: the run_at it waits for, or
// nil when it may go now, and the waiting paced digest to the same chat that the new one
// replaces, which the caller withholds once the new one is planned.
func (c *Core) digestPace(ctx context.Context, tx pgx.Tx, now time.Time, tenant, channel, target string) (*time.Time, string, error) {
	var waiting string
	var slot time.Time
	err := tx.QueryRow(ctx, `SELECT id, run_at FROM deliveries WHERE tenant = $1 AND kind = 'digest' AND exec_name = $2 AND target_id = $3
		AND state = 'planned' AND paced AND run_at > $4 ORDER BY run_at LIMIT 1 FOR UPDATE`, tenant, channel, target, now).Scan(&waiting, &slot)
	if err == nil {
		return &slot, waiting, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, "", err
	}
	limit, err := digestLimit(ctx, tx, tenant, channel, target)
	if err != nil {
		return nil, "", err
	}
	sent, err := digestsLastHour(ctx, tx, now, tenant, channel, target, true, "")
	if err != nil || len(sent) < limit {
		return nil, "", err
	}
	// The next free slot: an hour after the limit-th latest digest.
	next := sent[limit-1].Add(time.Hour)
	return &next, "", nil
}

// supersedeDigest ends a waiting digest that a newer one replaced.
func (c *Core) supersedeDigest(ctx context.Context, tx pgx.Tx, now time.Time, tenant, id string) error {
	if _, err := tx.Exec(ctx, `UPDATE deliveries SET state = 'withheld', reason = 'superseded', updated_at = $2 WHERE id = $1`, id, now); err != nil {
		return err
	}
	return c.appendEvent(ctx, tx, now, tenant, Event{Type: "delivery.withheld", Subject: id,
		Data: map[string]any{"delivery": id, "kind": "digest", "reason": "superseded"}})
}

// digestHandout applies the ceiling again when a digest is handed out. Over it the digest
// waits for the next free slot as a paced digest, and of the paced digests to the chat only
// the newest is kept. It reports whether the digest waits or was replaced.
func (c *Core) digestHandout(ctx context.Context, tx pgx.Tx, now time.Time, tenant, id, channel, target string) (bool, error) {
	limit, err := digestLimit(ctx, tx, tenant, channel, target)
	if err != nil {
		return false, err
	}
	// A requeued digest keeps its first handed_at; it does not count against itself.
	sent, err := digestsLastHour(ctx, tx, now, tenant, channel, target, false, id)
	if err != nil || len(sent) < limit {
		return false, err
	}
	var newer bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM deliveries o, deliveries d WHERE d.id = $1 AND o.id <> d.id AND o.tenant = d.tenant
		AND o.kind = 'digest' AND o.exec_name = d.exec_name AND o.target_id = d.target_id AND o.state = 'planned' AND o.seq > d.seq)`, id).Scan(&newer); err != nil {
		return false, err
	}
	if newer {
		return true, c.supersedeDigest(ctx, tx, now, tenant, id)
	}
	rows, err := tx.Query(ctx, `SELECT o.id FROM deliveries o, deliveries d WHERE d.id = $1 AND o.id <> d.id AND o.tenant = d.tenant AND o.kind = 'digest'
		AND o.exec_name = d.exec_name AND o.target_id = d.target_id AND o.state = 'planned' AND o.paced AND o.seq < d.seq FOR UPDATE OF o`, id)
	if err != nil {
		return false, err
	}
	older, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return false, err
	}
	for _, o := range older {
		if err := c.supersedeDigest(ctx, tx, now, tenant, o); err != nil {
			return false, err
		}
	}
	next := sent[limit-1].Add(time.Hour)
	_, err = tx.Exec(ctx, `UPDATE deliveries SET run_at = $2, paced = true, updated_at = $3 WHERE id = $1`, id, next, now)
	return true, err
}
