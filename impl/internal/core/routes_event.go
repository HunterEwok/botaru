// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Routes on events (on: event, MSG-16): a route from git enqueues a task of system in the
// transaction that wrote the event, with the key system:event:<route>:<source>:<id>, so two
// routes on one event give two tasks and a repeat of the event gives none (IDM-2). The routes run
// on the events a source channel or the monitoring adapter submits (ext.*, ING-4) and on the
// events a job publishes through P1 emit (IDM-4); the core's own events and alarms do not start
// routes, since alarms reach people only along the one alarm path (MON-1).

// eventHopsMax is the longest chain of route tasks through the events their jobs publish: a job
// task that a route on an event enqueued after this many such steps starts no route with its
// events, so two routes cannot enqueue each other without end.
const eventHopsMax = 3

// routedEvent is an event the routes of its tenant look at.
type routedEvent struct {
	Type, ID string
	// Source is the CloudEvents source the core gave the event (IDM-4).
	Source  string
	Subject string
	// Data holds only closed fields: the mapping of a source (ING-2) or the data schema of a
	// job event.
	Data map[string]any
	// File is the body of an external delivery: untrusted, it reaches a task only as an
	// attachment (ING-2).
	File string
	// TokenSource marks an event of a tls_token source, which has no replay protection (ING-1).
	TokenSource bool
	// Hops counts the route steps before this event: those of the publishing task.
	Hops int
}

// eventRoutes enqueues the tasks of the tenant's routes on an event. A route enqueues only a type
// of system-ops (PRM-10), explicitly listed when the event is external (ING-4), never action.*
// (APR-5), only of tier 0 from a tls_token source (ING-1). A type with alarm.* in its events gets
// the closed fields of the event and never the untrusted body (INJ-7). The goal comes from the
// route or the type, never from the event (ING-2). A refusal of the enqueue writes its own audit
// and leaves the event as it is.
func (c *Core) eventRoutes(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, tenant string, ev routedEvent) error {
	tv := cat.Tenants[tenant]
	if tv == nil || ev.Hops >= eventHopsMax {
		return nil
	}
	external := strings.HasPrefix(ev.Type, "ext.")
	for _, name := range slices.Sorted(mapKeys(tv.Routes)) {
		rt := tv.Routes[name]
		if rt.On.Event != ev.Type || rt.RouteMode() != "task" || rt.Task == nil {
			continue
		}
		if rt.On.Subject != "" && rt.On.Subject != ev.Subject || !matchEvent(rt.On.Match, ev.Data) {
			continue
		}
		typ := rt.Task.Type
		tt, ok := cat.TaskTypes[typ]
		why := ""
		switch {
		case !ok:
			why = "the type is not in the catalog"
		case strings.HasPrefix(typ, "action."):
			why = "a route does not enqueue action.* (APR-5)"
		case external && !systemOpsLists(tv, typ):
			why = "system-ops does not list the type (PRM-10, ING-4)"
		case ev.TokenSource && tt.Tier > 0:
			why = "a tls_token source routes only to tier 0 (ING-1)"
		}
		if why != "" {
			c.log.Warn("a route on an event was not run", "tenant", tenant, "route", name, "event", ev.ID, "why", why)
			continue
		}
		goal := rt.Task.Goal
		if goal == "" {
			goal = tt.Title
		}
		if goal == "" {
			goal = typ
		}
		// The closed fields of the event reach the task as the parameters its params_schema
		// names, the route's own params first; the body of an external delivery only as an
		// attachment, which makes the task untrusted, and never to a type that raises alarms
		// (ING-2, INJ-7).
		params := map[string]any{}
		props := mmap(tt.ParamsSchema, "properties")
		for k, v := range ev.Data {
			if _, ok := props[k]; ok {
				params[k] = v
			}
		}
		for k, v := range rt.Task.Params {
			params[k] = v
		}
		body := map[string]any{"type": typ, "goal": goal}
		if len(params) > 0 {
			body["params"] = params
		}
		if ev.File != "" && !alarmType(tt.Events) {
			body["attachments"] = []any{map[string]any{"file_ref": ev.File}}
		}
		res, err := c.enqueue(ctx, tx, now, cat, enqueueInput{
			Tenant: tenant, Type: typ, Body: body, IdempotencyKey: "system:event:" + name + ":" + ev.Source + ":" + ev.ID,
			Initiator: "system", EnqueuedBy: "route:" + name, Source: "route", SourceID: name, Cause: ev.ID,
		})
		if p, ok := AsProblem(err); ok {
			c.log.Warn("a route on an event was refused", "tenant", tenant, "route", name, "event", ev.ID, "reason", p.Reason)
			continue
		} else if err != nil {
			return err
		}
		if res.Rejected != nil {
			c.log.Warn("a route on an event was refused", "tenant", tenant, "route", name, "event", ev.ID, "reason", res.Rejected.Reason)
			continue
		}
		if !res.Existing {
			if _, err := tx.Exec(ctx, `UPDATE tasks SET record = record || jsonb_build_object('event_hops', $2::int) WHERE id = $1`, res.TaskID, ev.Hops+1); err != nil {
				return err
			}
		}
	}
	return nil
}

// alarmType says whether a type's events hold an alarm (INJ-7).
func alarmType(events []string) bool {
	return slices.ContainsFunc(events, func(e string) bool { return strings.HasPrefix(e, "alarm.") })
}

// matchEvent checks the match filter of a route: each named field of the event data, a dotted
// path into its objects, equals the value or one of the list (MSG-16, OUT-1).
func matchEvent(match map[string]any, data map[string]any) bool {
	for k, want := range match {
		var got any = data
		for _, seg := range strings.Split(k, ".") {
			m, ok := got.(map[string]any)
			if !ok {
				return false
			}
			if got, ok = m[seg]; !ok {
				return false
			}
		}
		if list, ok := want.([]any); ok {
			if !slices.ContainsFunc(list, func(w any) bool { return scalarEqual(w, got) }) {
				return false
			}
		} else if !scalarEqual(want, got) {
			return false
		}
	}
	return true
}

// scalarEqual compares two scalars of decoded JSON or YAML: numbers by value.
func scalarEqual(a, b any) bool {
	fa, okA := scalarNumber(a)
	fb, okB := scalarNumber(b)
	if okA || okB {
		return okA && okB && fa == fb
	}
	switch a.(type) {
	case string, bool:
		return a == b
	}
	return false
}

func scalarNumber(v any) (float64, bool) {
	switch x := v.(type) {
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case float64:
		return x, true
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	case interface{ String() string }:
		f, err := strconv.ParseFloat(x.String(), 64)
		return f, err == nil
	}
	return 0, false
}

// eventHops is the number of route steps of a task's tree: what a route on an event wrote in the
// record of its root, so a child of a route task counts as its root does.
func eventHops(ctx context.Context, tx pgx.Tx, task string) (int, error) {
	var n int
	err := tx.QueryRow(ctx, `SELECT coalesce((SELECT (r.record->>'event_hops')::int FROM tasks r WHERE r.id = coalesce(t.record->>'root', t.id)), 0)
		FROM tasks t WHERE t.id = $1`, task).Scan(&n)
	return n, err
}

// eventRouteAddrs are the deliver_to addresses of the route on an event whose task started the
// tree of a task, or nil: the root's enqueued_by names the route (DLV-6). A route on an inbox
// keeps its wake-up's answer to the inbox it read.
func eventRouteAddrs(ctx context.Context, tx pgx.Tx, cat *Catalog, tenant string, r treeRecord, id string) ([]any, error) {
	by := r.EnqueuedBy
	if r.Root != "" && r.Root != id {
		var root *string
		if err := tx.QueryRow(ctx, `SELECT record->>'enqueued_by' FROM tasks WHERE id = $1`, r.Root).Scan(&root); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		by = deref(root)
	}
	name, ok := strings.CutPrefix(by, "route:")
	tv := cat.Tenants[tenant]
	if !ok || tv == nil {
		return nil, nil
	}
	rt, ok := tv.Routes[name]
	if !ok || rt.On.Event == "" || len(rt.DeliverTo) == 0 {
		return nil, nil
	}
	return rt.DeliverTo, nil
}
