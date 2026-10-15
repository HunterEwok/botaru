// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/manifest"
)

// scheduleLookback bounds the search for the latest due slot after a long stop of the core: a
// skipped slot is not caught up, so only the latest one matters (TIM-3).
const scheduleLookback = 48 * time.Hour

// scheduleLateness is how late a due slot still runs; a later one is dropped.
const scheduleLateness = 15 * time.Minute

// scheduleTZ is the time zone of a schedule: its own, else the tenant's, else UTC (TIM-3).
func scheduleTZ(tv *TenantView, sc manifest.Schedule) string {
	switch {
	case sc.Timezone != "":
		return sc.Timezone
	case tv.Spec.Timezone != "":
		return tv.Spec.Timezone
	}
	return "UTC"
}

// scheduleLoop runs the schedules of every tenant: for each, the latest due slot after the
// last one handled is enqueued once or skipped with one schedule.skipped event (TIM-3).
func (c *Core) scheduleLoop(ctx context.Context) error {
	cat, err := c.Catalog(ctx)
	if err != nil {
		return err
	}
	var first error
	for _, tenant := range slices.Sorted(mapKeys(cat.Tenants)) {
		tv := cat.Tenants[tenant]
		for _, name := range slices.Sorted(mapKeys(tv.Schedules)) {
			// One failing schedule does not stop the others; the loop reports the first error
			// after it ran them all.
			if err := c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
				return c.runSchedule(ctx, tx, now, cat, tv, name, tv.Schedules[name])
			}); err != nil {
				if ctx.Err() != nil {
					return err
				}
				c.log.Warn("schedule failed", "tenant", tenant, "schedule", name, "err", err)
				if first == nil {
					first = err
				}
			}
		}
	}
	return first
}

func (c *Core) runSchedule(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, tv *TenantView, name string, sc manifest.Schedule) error {
	if sc.Task == nil {
		return nil
	}
	tz := scheduleTZ(tv, sc)
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil // apply does not let an unknown zone in (TIM-3)
	}
	spec, err := parseCron(sc.Cron)
	if err != nil {
		return nil
	}
	var cron, zone string
	var last time.Time
	err = tx.QueryRow(ctx, `SELECT cron, timezone, last_slot FROM schedule_state WHERE tenant = $1 AND name = $2 FOR UPDATE`, tv.Name, name).Scan(&cron, &zone, &last)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && (cron != sc.Cron || zone != tz)) {
		// A new schedule, or a changed cron or zone, takes effect from the next slot after
		// the apply; a slot key in UTC never runs an instant twice (TIM-3).
		_, err = tx.Exec(ctx, `INSERT INTO schedule_state (tenant, name, cron, timezone, last_slot, updated_at) VALUES ($1, $2, $3, $4, $5, $5)
			ON CONFLICT (tenant, name) DO UPDATE SET cron = $3, timezone = $4, last_slot = $5, updated_at = $5`,
			tv.Name, name, sc.Cron, tz, now.Truncate(time.Minute))
		return err
	}
	if err != nil {
		return err
	}
	from := last
	if floor := now.Add(-scheduleLookback); from.Before(floor) {
		from = floor
	}
	var due *slotTime
	for s, ok := spec.nextSlot(from, loc); ok && !s.At.After(now); s, ok = spec.nextSlot(s.At, loc) {
		due = &s
	}
	if due == nil {
		return nil
	}
	if _, err := tx.Exec(ctx, `UPDATE schedule_state SET last_slot = $3, updated_at = $4 WHERE tenant = $1 AND name = $2`, tv.Name, name, due.At, now); err != nil {
		return err
	}
	// A slot found long after its time, after a stop of the core, is not run late: a skipped
	// slot is not caught up (TIM-3).
	if now.Sub(due.At) > scheduleLateness {
		return nil
	}
	slot := fmtSlot(due.At)
	skip := func(reason string) error {
		return c.appendEvent(ctx, tx, now, tv.Name, Event{Type: "schedule.skipped", Subject: name, Data: map[string]any{
			"schedule": name, "slot": slot, "reason": reason,
		}})
	}
	if due.Gap {
		return skip("dst_gap")
	}
	keyPrefix := "system:schedule:" + name + ":"
	if sc.SkipIfWaiting == nil || *sc.SkipIfWaiting {
		var waiting bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM tasks WHERE tenant = $1 AND idempotency_key LIKE $2 AND idempotency_key <> $3 AND state = 'submitted')`,
			tv.Name, keyPrefix+"%", keyPrefix+slot).Scan(&waiting); err != nil {
			return err
		}
		if waiting {
			return skip("waiting")
		}
	}
	tt := cat.TaskTypes[sc.Task.Type]
	goal := sc.Task.Goal
	if goal == "" {
		goal = tt.Title
	}
	if goal == "" {
		goal = sc.Task.Type
	}
	body := map[string]any{"type": sc.Task.Type, "goal": goal}
	if sc.Task.Params != nil {
		body["params"] = sc.Task.Params
	}
	res, err := c.enqueue(ctx, tx, now, cat, enqueueInput{
		Tenant: tv.Name, Type: sc.Task.Type, Body: body, IdempotencyKey: keyPrefix + slot, Initiator: "system",
		EnqueuedBy: "schedule:" + name, Source: "schedule", SourceID: name,
	})
	if err != nil {
		if p, ok := AsProblem(err); ok {
			return c.scheduleRefused(ctx, tx, now, tv, name, slot, p, skip)
		}
		return err
	}
	if res.Rejected != nil {
		return c.scheduleRefused(ctx, tx, now, tv, name, slot, res.Rejected, skip)
	}
	return c.resolveAlarm(ctx, tx, now, scheduleAlarm(tv, name))
}

func scheduleAlarm(tv *TenantView, name string) Alarm {
	return Alarm{Tenant: tv.Name, Name: "schedule.refused", Subject: AlarmSubject{Kind: "schedule", ID: strings.ToLower(name)}, Severity: "warn"}
}

// scheduleRefused records a slot the enqueue refused: a hold or a capacity limit is a skip from
// the closed list (TIM-3); any other refusal (a permission of system-ops, money) stays in the
// audit the enqueue wrote and raises the schedule's alarm for the tenant admins.
func (c *Core) scheduleRefused(ctx context.Context, tx pgx.Tx, now time.Time, tv *TenantView, name, slot string, p *Problem, skip func(string) error) error {
	switch p.Reason {
	case "held", "over_capacity":
		return skip(p.Reason)
	}
	return c.fireAlarm(ctx, tx, now, scheduleAlarm(tv, name))
}

// deliverScheduled sends the result of a schedule's task to the schedule's deliver_to as the
// catalog has it now, since an address is checked at every run (DLV-6, PRM-10): a group is a
// direct message to each active member with data:<mark>, a sink gets the result for its
// store. An address the version does not serve yet stays without a delivery.
func (c *Core) deliverScheduled(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, tenant, task, name string, tt manifest.TaskType, result map[string]any, mark string) error {
	tv := cat.Tenants[tenant]
	if tv == nil {
		return nil
	}
	sc, ok := tv.Schedules[name]
	if !ok {
		return nil
	}
	text, fields := resultText(result, tt.SummaryFields)
	body := map[string]any{"form": "result", "text": text}
	if len(fields) > 0 {
		body["fields"] = fields
	}
	seen := map[string]bool{}
	for _, a := range sc.DeliverTo {
		addr, _ := a.(string)
		kind, target, _ := strings.Cut(addr, ":")
		switch kind {
		case "group":
			if _, ok := tv.Groups[target]; !ok {
				continue
			}
			chats, err := c.principalChats(ctx, tx, now, tv, "", func(groups []string) bool {
				return slices.Contains(groups, target) && hasData(tv, groups, mark)
			})
			if err != nil {
				return err
			}
			for _, ch := range chats {
				to := Chat{Kind: "direct", ExternalID: ch.ExternalID}.ID()
				if seen[ch.Channel+"/"+to] {
					continue
				}
				seen[ch.Channel+"/"+to] = true
				// A person the bot never wrote to gets the notices of the first contact first
				// (DLV-12, DLV-13).
				if err := c.firstContact(ctx, tx, now, tv, ch.Channel, ch.Principal, Chat{Kind: "direct", ExternalID: ch.ExternalID}); err != nil {
					return err
				}
				if _, err := c.planDelivery(ctx, tx, now, plannedDelivery{
					Tenant: tenant, Task: task, Kind: "task_result", Addr: addr, TargetKind: "chat", TargetID: to,
					ExecKind: "channel", ExecName: ch.Channel, Recipient: ch.Principal, Body: body,
					DataMark: mark, DataOrigin: "model",
				}); err != nil {
					return err
				}
			}
		case "sink":
			// The sink takes nothing above its ceiling (SEC-9).
			raw := cat.RawGlobals[manifest.Key{Kind: "Sink", Tenant: tenant, Name: target}]
			spec, _ := raw["spec"].(map[string]any)
			ceiling, _ := spec["ceiling"].(string)
			if raw == nil || (ceiling != "" && !c.markWithin(mark, ceiling)) {
				continue
			}
			if _, err := c.planDelivery(ctx, tx, now, plannedDelivery{
				Tenant: tenant, Task: task, Kind: "task_result", Addr: addr, TargetKind: "sink", TargetID: target,
				ExecKind: "sink", ExecName: target, Body: body, DataMark: mark, DataOrigin: "model",
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

// scheduleOf is the schedule whose slot started the tree of a task, or "": the root's
// enqueued_by names it.
func scheduleOf(ctx context.Context, tx pgx.Tx, r treeRecord, id string) (string, error) {
	by := r.EnqueuedBy
	if r.Root != "" && r.Root != id {
		var root *string
		if err := tx.QueryRow(ctx, `SELECT record->>'enqueued_by' FROM tasks WHERE id = $1`, r.Root).Scan(&root); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return "", err
		}
		by = deref(root)
	}
	name, _ := strings.CutPrefix(by, "schedule:")
	if name == by {
		return "", nil
	}
	return name, nil
}
