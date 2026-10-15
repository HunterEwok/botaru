// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Alarms (spec/lifecycles/alarm.md). Only code raises and resolves an alarm (INJ-7): the loops
// of this core call fireAlarm and resolveAlarm in the transaction of the fact they observe. An
// alarm lives in episodes by alarmkey: firing opens one, a repeat only counts, resolved closes
// it (OBS-6). This core does not claim the monitor profile, so it has no export for E5 and
// delivers every episode change to people itself as the code text alarm_notice (MON-1, MON-13).

// AlarmSubject is the subject of an alarm: a kind and an id from the catalog or a farm object.
type AlarmSubject struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// Alarm is one observation of trouble by code.
type Alarm struct {
	Tenant   string
	Name     string // without alarm. and .vN, for example worker.stale
	Subject  AlarmSubject
	Severity string // used when the events catalog gives the alarm no severity
	Values   map[string]any
	// Page raises the severity to page, as for a stop that hit the service limit (MON-8).
	Page bool
	// Origin is monitor for an alarm raised by a monitoring problem (MON-5), else code.
	Origin string
}

// pageRepeat is the pause between the repeated notices of an open page alarm (MON-13).
const pageRepeat = 30 * time.Minute

func (a Alarm) key() string { return a.Name + ":" + a.Subject.ID }

func (a Alarm) eventType() string { return "alarm." + a.Name + ".v1" }

// alarmSeverity takes the severity of the events catalog, where it names one.
func (c *Core) alarmSeverity(a Alarm) (string, error) {
	if a.Page {
		return "page", nil
	}
	cat, err := c.spec.Registry("spec/bindings/events/catalog.yaml")
	if err != nil {
		return "", err
	}
	e, ok := cat.Entry(a.eventType())
	if !ok {
		return "", fmt.Errorf("alarm %s is not in the events catalog", a.eventType())
	}
	if s, _ := e["severity"].(string); s != "" {
		return s, nil
	}
	return a.Severity, nil
}

type episode struct {
	ID       int64
	Tenant   string
	Alarmkey string
	Alarm    string
	State    string
	Severity string
	Subject  AlarmSubject
	Seq      int
	At       time.Time
}

// alarmData builds the event data and checks it against the common schema: a value outside it,
// a text included, is refused (OBS-6).
func (c *Core) alarmData(ep *episode, values map[string]any) (map[string]any, error) {
	data := map[string]any{"alarmkey": ep.Alarmkey, "state": ep.State, "severity": ep.Severity, "subject": ep.Subject}
	if len(values) > 0 {
		data["values"] = values
	}
	if err := c.spec.Validate("alarm", data); err != nil {
		return nil, refuse("input_schema_invalid", "the alarm data does not match the alarm schema")
	}
	return data, nil
}

// lockAlarmkey serializes the changes of one alarmkey, so that two loops never open two
// episodes of it.
func lockAlarmkey(ctx context.Context, tx pgx.Tx, tenant, key string) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 7300102))`, tenant+"/"+key)
	return err
}

// logAlarmValues keeps one change of a firing episode's numbers for incident detection (MGR-8).
func logAlarmValues(ctx context.Context, tx pgx.Tx, episode int64, now time.Time, vals []byte) error {
	_, err := tx.Exec(ctx, `INSERT INTO alarm_values (episode, at, vals) VALUES ($1, $2, $3)
		ON CONFLICT (episode, at) DO UPDATE SET vals = EXCLUDED.vals`, episode, now, vals)
	return err
}

// fireAlarm opens an episode, or counts a repeat of the open one without an event (OBS-6).
func (c *Core) fireAlarm(ctx context.Context, tx pgx.Tx, now time.Time, a Alarm) error {
	sev, err := c.alarmSeverity(a)
	if err != nil {
		return err
	}
	ep := &episode{Tenant: a.Tenant, Alarmkey: a.key(), Alarm: a.eventType(), State: "firing", Severity: sev, Subject: a.Subject, Seq: 1, At: now}
	data, err := c.alarmData(ep, a.Values)
	if err != nil {
		return err
	}
	if err := lockAlarmkey(ctx, tx, a.Tenant, ep.Alarmkey); err != nil {
		return err
	}
	// A repeat keeps the episode and carries the latest numbers, which incident conditions read.
	var latest []byte
	if a.Values != nil {
		latest, _ = json.Marshal(a.Values)
	}
	var repeatID int64
	var changed bool
	err = tx.QueryRow(ctx, `UPDATE alarm_episodes SET repeat_count = repeat_count + 1, last_seen_at = $3, vals = coalesce($4::jsonb, vals),
		vals_at = CASE WHEN $4::jsonb IS NOT NULL AND $4::jsonb IS DISTINCT FROM vals THEN $3 ELSE vals_at END
		WHERE tenant = $1 AND alarmkey = $2 AND state = 'firing'
		RETURNING id, $4::jsonb IS NOT NULL AND vals_at = $3`, a.Tenant, ep.Alarmkey, now, latest).Scan(&repeatID, &changed)
	if err == nil {
		if changed {
			return logAlarmValues(ctx, tx, repeatID, now, latest)
		}
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	subj, _ := json.Marshal(a.Subject)
	vals, _ := json.Marshal(a.Values)
	if a.Values == nil {
		vals = []byte("{}")
	}
	origin := a.Origin
	if origin == "" {
		origin = "code"
	}
	if err := tx.QueryRow(ctx, `INSERT INTO alarm_episodes (tenant, alarmkey, alarm, state, severity, subject, vals, seq, opened_at, last_seen_at, notified_at, origin, vals_at)
		VALUES ($1, $2, $3, 'firing', $4, $5, $6, 1, $7, $7, $7, $8, $7) RETURNING id`,
		a.Tenant, ep.Alarmkey, ep.Alarm, sev, subj, vals, now, origin).Scan(&ep.ID); err != nil {
		return err
	}
	if err := logAlarmValues(ctx, tx, ep.ID, now, vals); err != nil {
		return err
	}
	if err := c.appendEvent(ctx, tx, now, a.Tenant, Event{Type: ep.Alarm, Subject: ep.Alarmkey, Data: data}); err != nil {
		return err
	}
	if err := c.exportEpisode(ctx, tx, now, ep, origin, a.Values); err != nil {
		return err
	}
	return c.noticeAlarm(ctx, tx, now, ep)
}

// resolveAlarm closes the open episode of the alarmkey, if any.
func (c *Core) resolveAlarm(ctx context.Context, tx pgx.Tx, now time.Time, a Alarm) error {
	if err := lockAlarmkey(ctx, tx, a.Tenant, a.key()); err != nil {
		return err
	}
	ep := &episode{Tenant: a.Tenant, Alarmkey: a.key(), State: "resolved", Seq: 2, At: now}
	var subj []byte
	var origin string
	err := tx.QueryRow(ctx, `UPDATE alarm_episodes SET state = 'resolved', seq = 2, resolved_at = $3, notified_at = $3
		WHERE tenant = $1 AND alarmkey = $2 AND state = 'firing' RETURNING id, alarm, severity, subject, origin`,
		a.Tenant, ep.Alarmkey, now).Scan(&ep.ID, &ep.Alarm, &ep.Severity, &subj, &origin)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(subj, &ep.Subject); err != nil {
		return err
	}
	data, err := c.alarmData(ep, nil)
	if err != nil {
		return err
	}
	if err := c.appendEvent(ctx, tx, now, a.Tenant, Event{Type: ep.Alarm, Subject: ep.Alarmkey, Data: data}); err != nil {
		return err
	}
	if err := c.exportEpisode(ctx, tx, now, ep, origin, nil); err != nil {
		return err
	}
	return c.noticeAlarm(ctx, tx, now, ep)
}

// noticeAlarm puts the alarm_notice deliveries of an episode change into the private chats of
// the admins of its tenant, and of the admins of farm when the tenant has none (MON-13). An
// admin is an active person with admin:people or hold.release. The text comes only from the
// code text catalog: the alarm, severity, state, subject, seq and time.
func (c *Core) noticeAlarm(ctx context.Context, tx pgx.Tx, now time.Time, ep *episode) error {
	if c.monitorOn() {
		return nil // the monitoring system is the one path to people (MON-1)
	}
	cat, err := c.Catalog(ctx)
	if err != nil {
		return err
	}
	var chats []adminChat
	tenant := ep.Tenant
	for _, t := range []string{ep.Tenant, "farm"} {
		tv, ok := cat.Tenants[t]
		if !ok || tv.Spec.Kind == "" {
			continue
		}
		if chats, err = c.adminChats(ctx, tx, now, tv, "", "admin:people", "hold.release"); err != nil {
			return err
		}
		if tenant = t; len(chats) > 0 {
			break
		}
	}
	if len(chats) == 0 {
		c.log.Warn("alarm_notice has no recipient: no active admin of the tenant or of farm has a private chat with a channel",
			"tenant", ep.Tenant, "alarm", ep.Alarm, "state", ep.State)
		return nil
	}
	params := map[string]any{
		"alarm": strings.TrimSuffix(strings.TrimPrefix(ep.Alarm, "alarm."), ".v1"), "severity": ep.Severity, "state": ep.State,
		"subject": ep.Subject.Kind + " " + ep.Subject.ID, "seq": ep.Seq, "at": fmtTime(ep.At),
	}
	for _, a := range chats {
		if _, err := c.planDelivery(ctx, tx, now, plannedDelivery{
			Tenant: tenant, Kind: "alarm_notice", Addr: "admins", TargetKind: "chat",
			TargetID: Chat{Kind: "direct", ExternalID: a.ExternalID}.ID(), ExecKind: "channel", ExecName: a.Channel,
			Recipient: a.Principal, DataOrigin: "code", Alarmkey: ep.Tenant + "/" + ep.Alarmkey, Episode: ep.ID,
			Body: map[string]any{"form": "text_code", "text_key": "notice.alarm", "params": params},
		}); err != nil {
			return err
		}
	}
	return nil
}

// alarmLoop repeats the notice of an open page alarm every 30 minutes (MON-13). Without
// incidents in this build there is no incident.ack, so a page alarm repeats until it resolves.
func (c *Core) alarmLoop(ctx context.Context) error {
	for {
		done := true
		err := c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
			ep := &episode{}
			var subj []byte
			err := tx.QueryRow(ctx, `SELECT id, tenant, alarmkey, alarm, severity, subject, opened_at FROM alarm_episodes
				WHERE state = 'firing' AND severity = 'page' AND notified_at <= $1 ORDER BY notified_at, id LIMIT 1 FOR UPDATE SKIP LOCKED`,
				now.Add(-pageRepeat)).Scan(&ep.ID, &ep.Tenant, &ep.Alarmkey, &ep.Alarm, &ep.Severity, &subj, &ep.At)
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			done = false
			if err := json.Unmarshal(subj, &ep.Subject); err != nil {
				return err
			}
			ep.State, ep.Seq = "firing", 1
			if _, err := tx.Exec(ctx, `UPDATE alarm_episodes SET notified_at = $2 WHERE id = $1`, ep.ID, now); err != nil {
				return err
			}
			return c.noticeAlarm(ctx, tx, now, ep)
		})
		if err != nil || done {
			return err
		}
	}
}

// staleAfter is two long-poll periods of claim (OBS-4).
const staleAfter = 2 * 60 * time.Second

// liveness is the worker-liveness loop: a worker not seen by any P1 call for longer than two
// long-poll periods gives the event worker.stale and the alarm alarm.worker.stale; its own
// self-report changes nothing (OBS-4, STA-2). A call afterwards resolves the alarm.
func (c *Core) liveness(ctx context.Context) error {
	for {
		done := true
		err := c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
			var id, tenant, state string
			var seen time.Time
			var since *time.Time
			err := tx.QueryRow(ctx, `SELECT id, tenant, state, last_seen, stale_since FROM workers
				WHERE last_seen IS NOT NULL AND (
					stale_since IS NULL AND state IN ('active', 'draining') AND last_seen < $1
					OR stale_since IS NOT NULL AND (last_seen > stale_since OR state NOT IN ('active', 'draining')))
				ORDER BY id LIMIT 1 FOR UPDATE SKIP LOCKED`, now.Add(-staleAfter)).Scan(&id, &tenant, &state, &seen, &since)
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			done = false
			a := Alarm{Tenant: tenant, Name: "worker.stale", Subject: AlarmSubject{Kind: "worker", ID: strings.ToLower(id)}, Severity: "warn"}
			if since != nil {
				if _, err := tx.Exec(ctx, `UPDATE workers SET stale_since = NULL WHERE id = $1`, id); err != nil {
					return err
				}
				return c.resolveAlarm(ctx, tx, now, a)
			}
			if _, err := tx.Exec(ctx, `UPDATE workers SET stale_since = $2 WHERE id = $1`, id, now); err != nil {
				return err
			}
			if err := c.appendEvent(ctx, tx, now, tenant, Event{Type: "worker.stale.v1", Subject: id, Data: map[string]any{"worker": id, "last_seen": fmtTime(seen)}}); err != nil {
				return err
			}
			a.Values = map[string]any{"silent_s": int64(now.Sub(seen) / time.Second)}
			return c.fireAlarm(ctx, tx, now, a)
		})
		if err != nil || done {
			return err
		}
	}
}

// unclaimed raises alarm.task.unclaimed once for a task that nobody took before the start_by
// of its type; it stays in submitted (DSP-8). The alarm resolves when the task is taken or
// ends.
func (c *Core) unclaimed(ctx context.Context) error {
	for {
		done := true
		err := c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
			var id, tenant, state string
			var leaseNo int
			var flagged bool
			var startBy *time.Time
			err := tx.QueryRow(ctx, `SELECT id, tenant, state, lease_no, unclaimed_alarm, start_by_at FROM tasks
				WHERE state = 'submitted' AND lease_no = 0 AND NOT unclaimed_alarm AND start_by_at <= $1
				OR unclaimed_alarm AND (state <> 'submitted' OR lease_no > 0)
				ORDER BY id LIMIT 1 FOR UPDATE SKIP LOCKED`, now).Scan(&id, &tenant, &state, &leaseNo, &flagged, &startBy)
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			done = false
			a := Alarm{Tenant: tenant, Name: "task.unclaimed", Subject: AlarmSubject{Kind: "task", ID: strings.ToLower(id)}, Severity: "warn"}
			if _, err := tx.Exec(ctx, `UPDATE tasks SET unclaimed_alarm = $2 WHERE id = $1`, id, !flagged); err != nil {
				return err
			}
			if flagged {
				return c.resolveAlarm(ctx, tx, now, a)
			}
			a.Values = map[string]any{"late_s": int64(now.Sub(*startBy) / time.Second)}
			return c.fireAlarm(ctx, tx, now, a)
		})
		if err != nil || done {
			return err
		}
	}
}
