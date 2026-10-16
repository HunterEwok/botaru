// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
)

// The health of what a channel and P4 depend on (OBS-10). A channel reports in hello and
// heartbeat a health report of component channel_upstream: its time is the time of the last
// successful poll of the external system, and the core keeps the last report of each channel
// worker. P4 keeps the state of each pair (workspace, model pool) of the provider in
// provider_pairs (GW-11). doctor shows both. A report never widens a hand-out (STA-2).

// channelUpstreamStale is the age of the last successful poll above which doctor warns.
const channelUpstreamStale = 10 * time.Minute

// channelUpstream keeps the last channel_upstream report of a channel worker; a report of
// another component or one whose time does not parse is ignored.
func channelUpstream(ctx context.Context, tx pgx.Tx, w *Worker, reports []ComponentHealth) error {
	for _, r := range slices.Backward(reports) {
		if r.Component != "channel_upstream" {
			continue
		}
		if _, err := time.Parse(time.RFC3339Nano, r.Time); err != nil {
			return nil
		}
		raw, err := json.Marshal(r)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE workers SET upstream = $2 WHERE id = $1`, w.ID, raw)
		return err
	}
	return nil
}

// doctorUpstream adds for each live channel worker of the tenant the age of its last
// successful poll (warn when older than 10 minutes, when it is not ok, or when it never
// reported), and for each provider pair of the tenant its state (warn when not closed).
func doctorUpstream(ctx context.Context, tx pgx.Tx, now time.Time, tenant string, add func(DoctorCheck)) error {
	rows, err := tx.Query(ctx, `SELECT coalesce(target, id), upstream FROM workers WHERE tenant = $1 AND kind = 'channel' AND state <> 'revoked' ORDER BY 1, id`, tenant)
	if err != nil {
		return err
	}
	type up struct {
		target string
		raw    []byte
	}
	var ups []up
	for rows.Next() {
		var u up
		if err := rows.Scan(&u.target, &u.raw); err != nil {
			rows.Close()
			return err
		}
		ups = append(ups, u)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, u := range ups {
		ch := DoctorCheck{ID: checkID("channel.last_poll_age_s." + u.target), OK: false, Severity: "warn", Value: nil, Tenant: tenant}
		var r ComponentHealth
		if u.raw != nil && json.Unmarshal(u.raw, &r) == nil {
			if at, err := time.Parse(time.RFC3339Nano, r.Time); err == nil {
				ch.Value = int64(now.Sub(at).Seconds())
				if r.State == "ok" && now.Sub(at) <= channelUpstreamStale {
					ch.OK, ch.Severity = true, "info"
				}
			}
		}
		add(ch)
	}
	prow, err := tx.Query(ctx, `SELECT pool, state FROM provider_pairs WHERE tenant = $1 ORDER BY pool`, tenant)
	if err != nil {
		return err
	}
	defer prow.Close()
	for prow.Next() {
		var pool, state string
		if err := prow.Scan(&pool, &state); err != nil {
			return err
		}
		ch := DoctorCheck{ID: checkID("provider.state." + pool), OK: state == "closed", Severity: "info", Value: state, Tenant: tenant}
		if state != "closed" {
			ch.Severity = "warn"
		}
		add(ch)
	}
	return prow.Err()
}
