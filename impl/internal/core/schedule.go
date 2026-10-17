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
	for _, tenant := range slices.Sorted(mapKeys(cat.Tenants)) {
		tv := cat.Tenants[tenant]
		for _, name := range slices.Sorted(mapKeys(tv.Schedules)) {
			// One failing schedule does not stop the others, nor the loop's tick: it is logged.
			if err := c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
				return c.runSchedule(ctx, tx, now, cat, tv, name, tv.Schedules[name])
			}); err != nil {
				if ctx.Err() != nil {
					return err
				}
				c.log.Warn("schedule failed", "tenant", tenant, "schedule", name, "err", err)
			}
		}
	}
	return nil
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
		// the apply, not after this tick, so the first slot after the apply is not lost; a slot
		// key in UTC never runs an instant twice (TIM-3).
		// The apply that made the change is the schedule's own (its manifest's applied_at),
		// or, for a zone the schedule takes from its tenant, the tenant's: a later unrelated
		// apply does not move the start past a slot that was already due.
		from := now
		// The tenant's apply counts only when the zone the schedule takes from its tenant
		// is what changed, not for any other edit of the tenant; then it alone counts, as a
		// later edit of another field of the schedule moves no slot.
		zoneOnly := err == nil && cron == sc.Cron && sc.Timezone == ""
		var applied *time.Time
		if err := tx.QueryRow(ctx, `SELECT max(applied_at) FROM manifests WHERE tenant = $1
			AND ((kind = 'Schedule' AND name = $2 AND NOT $3) OR (kind = 'Tenant' AND $3))`, tv.Name, name, zoneOnly).Scan(&applied); err != nil {
			return err
		}
		if applied != nil && applied.Before(now) {
			from = *applied
		}
		_, err = tx.Exec(ctx, `INSERT INTO schedule_state (tenant, name, cron, timezone, last_slot, updated_at) VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (tenant, name) DO UPDATE SET cron = $3, timezone = $4, last_slot = $5, updated_at = $6`,
			tv.Name, name, sc.Cron, tz, from.Truncate(time.Minute), now)
		if err != nil || !from.Before(now.Truncate(time.Minute)) {
			return err
		}
		last = from.Truncate(time.Minute)
	} else if err != nil {
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
		c.log.Warn("a schedule slot was found too late and does not run", "tenant", tv.Name, "schedule", name, "slot", fmtSlot(due.At))
		return nil
	}
	slot := fmtSlot(due.At)
	skip := func(reason string) error {
		return c.appendEvent(ctx, tx, now, tv.Name, Event{Type: "schedule.skipped", Subject: name, Data: map[string]any{
			"schedule": name, "slot": slot, "reason": reason,
		}})
	}
	// A paused schedule skips its slots without catch-up, like a hold (TIM-5).
	if sc.Paused {
		return skip("paused")
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
		// The provider circuit of the model slot the type needs is open (TIM-3).
		if open, err := circuitOpenFor(ctx, tx, now, cat, tv.Name, sc.Task.Type); err != nil {
			return err
		} else if open {
			return skip("waiting")
		}
	}
	// A hold on the schedule itself, such as its auto-stop, skips the slot (TIM-3, CAP-4); so
	// does a maintenance window on a machine the slot's task names, without catch-up (MON-9).
	if held, err := heldBy(ctx, tx, now, []string{"no_enqueue"}, scopeRef{kind: "schedule", tenant: tv.Name, name: name}); err != nil {
		return err
	} else if held != "" {
		return skip("held")
	}
	if held, err := c.scheduleHostHeld(ctx, tx, now, cat, tv.Name, sc.Task.Params); err != nil {
		return err
	} else if held {
		return skip("held")
	}
	tt := cat.TaskTypes[sc.Task.Type]
	// Analysis keeps to the run limit and the merge window of its role (INS-4).
	if tt.Purpose == "insight" {
		if over, err := insightOverLimit(ctx, tx, now, cat, tv.Name, sc.Task.Type); err != nil {
			return err
		} else if over {
			return skip("insight_limit")
		}
	}
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

// circuitOpenFor says whether every role that claims the type waits for an open provider
// circuit of its main model slot; a type no role claims does not wait.
func circuitOpenFor(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, tenant, typ string) (bool, error) {
	claimed := false
	for _, name := range slices.Sorted(mapKeys(cat.Roles)) {
		role := cat.Roles[name]
		if !slices.Contains(role.Claims, typ) {
			continue
		}
		claimed = true
		wait, err := pairWait(ctx, tx, now, cat, tenant, role)
		if err != nil {
			return false, err
		}
		if wait != "provider_unavailable" {
			return false, nil
		}
	}
	return claimed, nil
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
// store, a bound chat the closed summary only (DLV-15).
func (c *Core) deliverScheduled(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, tenant, task, name string, tt manifest.TaskType, result map[string]any, mark string) error {
	tv := cat.Tenants[tenant]
	if tv == nil {
		return nil
	}
	sc, ok := tv.Schedules[name]
	if !ok {
		return nil
	}
	return c.deliverResultTo(ctx, tx, now, cat, tv, task, sc.DeliverTo, tt, result, mark)
}

// deliverResultTo sends the result of a task to deliver_to addresses as deliverScheduled does:
// the addresses of a schedule or of a route on an event (DLV-6, DLV-15).
func (c *Core) deliverResultTo(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, tv *TenantView, task string, addrs []any, tt manifest.TaskType, result map[string]any, mark string) error {
	tenant := tv.Name
	text, fields := resultText(result, tt.SummaryFields)
	body := map[string]any{"form": "result", "text": text}
	if len(fields) > 0 {
		body["fields"] = fields
	}
	seen := map[string]bool{}
	for _, a := range addrs {
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
		case "chat":
			// A bound group chat gets only the closed summary within its ceiling; the full
			// result goes only to the other addresses (DLV-15).
			b, err := bindingByName(ctx, tx, tenant, target)
			if err != nil {
				return err
			}
			if b == nil || !c.markWithin(mark, b.Ceiling) {
				continue
			}
			sum, sf := closedSummary(tt, result)
			if sum == "" {
				continue
			}
			if _, err := c.planDelivery(ctx, tx, now, plannedDelivery{
				Tenant: tenant, Task: task, Kind: "task_result", Addr: addr, TargetKind: "chat", TargetID: b.Chat,
				ExecKind: "channel", ExecName: b.Channel, Body: map[string]any{"form": "result", "text": sum, "fields": sf},
				DataMark: mark, DataOrigin: "code",
			}); err != nil {
				return err
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

// insightOverLimit says whether a scheduled analysis of the role that runs the type would pass
// its max_runs or start inside its merge window, which is at least an hour (INS-4).
func insightOverLimit(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, tenant, typ string) (bool, error) {
	var ins *manifest.RoleInsight
	for _, name := range slices.Sorted(mapKeys(cat.Roles)) {
		if r := cat.Roles[name]; r.Insight != nil && slices.Contains(r.Insight.Types, typ) {
			ins = r.Insight
			break
		}
	}
	if ins == nil {
		return true, nil
	}
	window := durationOr(ins.MergeWindow, time.Hour)
	if window < time.Hour {
		window = time.Hour
	}
	since := now.Add(-window)
	per, limit := since, -1
	if ins.MaxRuns != nil {
		limit = ins.MaxRuns.Count
		if ins.MaxRuns.Per == "week" {
			per = now.AddDate(0, 0, -7)
		} else {
			per = now.AddDate(0, 0, -1)
		}
	}
	var recent, runs int
	if err := tx.QueryRow(ctx, `SELECT count(*) FILTER (WHERE created_at > $3), count(*) FILTER (WHERE created_at > $4 AND idempotency_key LIKE 'system:schedule:%')
		FROM tasks WHERE tenant = $1 AND type = ANY($2) AND created_at > least($3, $4)`, tenant, ins.Types, since, per).Scan(&recent, &runs); err != nil {
		return false, err
	}
	return recent > 0 || limit >= 0 && runs >= limit, nil
}
