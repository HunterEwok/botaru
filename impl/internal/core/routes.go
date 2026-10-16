// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/ids"
	"github.com/HunterEwok/botaru/impl/internal/manifest"
	"github.com/HunterEwok/botaru/impl/internal/money"
)

// Routes from messages (MSG-16): a wake-up of a role or bot group by its inbox (on: inbox), a
// notice to people from a code template (on: message) and an auto-report at the end of a task
// (on: task_done). Only routes from git do this; the model, a worker and an external program
// never create one.

const (
	// wakeWindowMin is the shortest wake-up window of a route: a wake-up per window at most.
	wakeWindowMin = 5 * time.Minute
	// wakesPerDayMax is the daily number of wake-ups of a tenant, which it can only lower.
	wakesPerDayMax = 200
)

// messageUsage writes the usage line of a published message into the sender's tree: unit
// message with its size and type, at no price (BUD-3, MSG-11). The core's own message without a
// tree writes none.
func (c *Core) messageUsage(ctx context.Context, tx pgx.Tx, now time.Time, s *msgSender, id, typ string, env map[string]any, note *string) error {
	task := s.Task
	if task == "" {
		task = s.TreeTask
	}
	if task == "" {
		return nil
	}
	var initiator string
	var rec []byte
	if err := tx.QueryRow(ctx, `SELECT initiator, record FROM tasks WHERE id = $1`, task).Scan(&initiator, &rec); err != nil {
		return err
	}
	var r struct {
		Root       string `json:"root"`
		BudgetLine string `json:"budget_line"`
		Purpose    string `json:"purpose"`
	}
	if err := json.Unmarshal(rec, &r); err != nil {
		return err
	}
	if r.BudgetLine == "" {
		r.BudgetLine = "work"
	}
	raw, err := json.Marshal(env)
	if err != nil {
		return err
	}
	size := len(raw)
	if note != nil {
		size += len(*note)
	}
	msg, err := json.Marshal(map[string]any{"size": size, "kind": typ})
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO spend (id, tenant, task, root, subject, role, worker, line, purpose, unit, state, eur_micros, message, time)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 'message', 'committed', 0, $10, $11)`,
		ids.New(ids.Spend), s.Tenant, task, r.Root, initiator, nullable(s.Role), nullable(s.Worker), r.BudgetLine, nullable(r.Purpose), msg, now)
	return err
}

// notifyRoutes runs the on: message routes of a published message: a code text to the route's
// people, only from an entry the code wrote and nobody distrusts, never an alarm (MSG-16, INJ-7,
// MON-1). The text is the route's template with the closed fields of the message.
func (c *Core) notifyRoutes(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, tenant, id, typ string, env map[string]any) error {
	tv := cat.Tenants[tenant]
	if tv == nil || env["dataorigin"] != "code" || env["untrusted"] != false {
		return nil
	}
	subject, _ := env["subject"].(string)
	mark, _ := env["datamark"].(string)
	data, _ := env["data"].(map[string]any)
	fields, _ := data["fields"].(map[string]any)
	// The parameters of a code text are strings up to 500 characters and integers.
	params := map[string]any{"type": typ, "subject": subject}
	for k, v := range fields {
		switch x := v.(type) {
		case string:
			params[k] = cut500(x)
		case bool:
			params[k] = strconv.FormatBool(x)
		case float64:
			if x == math.Trunc(x) && math.Abs(x) < 1<<53 {
				params[k] = int64(x)
			} else {
				params[k] = strconv.FormatFloat(x, 'g', -1, 64)
			}
		}
	}
	for _, name := range slices.Sorted(mapKeys(tv.Routes)) {
		rt := tv.Routes[name]
		if rt.On.Message != typ || rt.RouteMode() != "notify" || rt.Template == "" {
			continue
		}
		if rt.On.SubjectPrefix != "" && !strings.HasPrefix(subject, rt.On.SubjectPrefix) {
			continue
		}
		body := map[string]any{"form": "text_code", "text_key": "route." + rt.Template, "params": params}
		if err := c.deliverNotice(ctx, tx, now, tv, "route_notice", rt.DeliverTo, body, mark); err != nil {
			return err
		}
	}
	return nil
}

// deliverNotice plans a code text to the people of the addresses as the catalog has them now: a
// group is a direct message to each active member with data:<mark>, a bound chat gets it within
// its ceiling (DLV-6, DLV-15). A sink takes no notices.
func (c *Core) deliverNotice(ctx context.Context, tx pgx.Tx, now time.Time, tv *TenantView, kind string, addrs []any, body map[string]any, mark string) error {
	seen := map[string]bool{}
	for _, a := range addrs {
		addr, _ := a.(string)
		k, target, _ := strings.Cut(addr, ":")
		switch k {
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
				if err := c.firstContact(ctx, tx, now, tv, ch.Channel, ch.Principal, Chat{Kind: "direct", ExternalID: ch.ExternalID}); err != nil {
					return err
				}
				if _, err := c.planDelivery(ctx, tx, now, plannedDelivery{
					Tenant: tv.Name, Kind: kind, Addr: addr, TargetKind: "chat", TargetID: to,
					ExecKind: "channel", ExecName: ch.Channel, Recipient: ch.Principal, Body: body, DataMark: mark, DataOrigin: "code",
				}); err != nil {
					return err
				}
			}
		case "chat":
			b, err := bindingByName(ctx, tx, tv.Name, target)
			if err != nil {
				return err
			}
			if b == nil || !c.markWithin(mark, b.Ceiling) {
				continue
			}
			if _, err := c.planDelivery(ctx, tx, now, plannedDelivery{
				Tenant: tv.Name, Kind: kind, Addr: addr, TargetKind: "chat", TargetID: b.Chat,
				ExecKind: "channel", ExecName: b.Channel, Body: body, DataMark: mark, DataOrigin: "code",
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

// closedFields are the summary_fields of a result of closed kinds only, with their values
// (INJ-1): what an auto-report carries.
func closedFields(tt manifest.TaskType, result map[string]any) map[string]any {
	_, sf := closedSummary(tt, result)
	out := map[string]any{}
	for k := range sf {
		out[k] = result[k]
	}
	return out
}

// autoReports publishes the on: task_done reports of a task that reached a terminal state, in
// the transaction of that transition: the summary_fields of closed kinds, the task's mark and
// flag, the key system:report:<task>:<route>. A report that is refused is not created: the core
// writes message.rejected and the transition goes through (MSG-4, MSG-16).
func (c *Core) autoReports(ctx context.Context, tx pgx.Tx, now time.Time, tenant, task string) error {
	cat, err := c.Catalog(ctx)
	if err != nil {
		return err
	}
	tv := cat.Tenants[tenant]
	if tv == nil || len(tv.Routes) == 0 {
		return nil
	}
	var typ, initiator string
	var rec, res []byte
	if err := tx.QueryRow(ctx, `SELECT type, initiator, record, result FROM tasks WHERE id = $1`, task).Scan(&typ, &initiator, &rec, &res); err != nil {
		return err
	}
	var names []string
	for _, name := range slices.Sorted(mapKeys(tv.Routes)) {
		if rt := tv.Routes[name]; rt.On.TaskDone == typ && rt.RouteMode() == "report" && rt.Report != nil {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return nil
	}
	var r msgRecord
	if err := json.Unmarshal(rec, &r); err != nil {
		return err
	}
	result := map[string]any{}
	if len(res) > 0 {
		if err := json.Unmarshal(res, &result); err != nil {
			return err
		}
	}
	fields := closedFields(cat.TaskTypes[typ], result)
	for _, name := range names {
		rt := tv.Routes[name]
		s := &msgSender{Tenant: tenant, Role: "core", Producer: "core", Initiator: initiator, Initiators: r.CoInitiators,
			Mark: r.DataMark, Untrusted: r.UntrustedSeen, UntrustedOf: r.UntrustedOf, Depth: r.effDepth(), Root: r.Root, Trace: r.Trace,
			Key: "system:report:" + task + ":" + name, TreeTask: task, ModelFields: cat.TaskTypes[typ].WorkerKind == "agent"}
		if r.UntrustedSeen && len(s.UntrustedOf) == 0 && r.UntrustedSource != "" {
			s.UntrustedOf = []string{r.UntrustedSource}
		}
		_, p, err := c.sendMessage(ctx, tx, now, cat, s, MsgSendRequest{Type: rt.Report.Type, To: rt.Report.To, Fields: fields})
		if err != nil {
			return err
		}
		if p != nil {
			if err := c.appendEvent(ctx, tx, now, tenant, Event{Type: "message.rejected", Subject: rt.Report.Type, Causation: task,
				Data: map[string]any{"type": rt.Report.Type, "to": rt.Report.To, "from_role": "core", "reason": p.Reason}}); err != nil {
				return err
			}
		}
	}
	return nil
}

// wakeCeilings are the tenant's daily wake-up money per inbox and number of wake-ups: the
// values of messaging, which can only lower the defaults, a tenth of the day budget and 200
// (MSG-11). A nil money ceiling is no ceiling: the tenant has no day budget.
func wakeCeilings(tv *TenantView) (*float64, int) {
	b := tv.Spec.Budgets
	day := b.DayEUR
	if day == nil && b.MonthEUR != nil {
		v := *b.MonthEUR / 20
		day = &v
	}
	var eur *float64
	if day != nil {
		v := *day / 10
		eur = &v
	}
	if w := tv.Spec.Messaging.WakeDayEUR; w != nil && (eur == nil || *w < *eur) {
		v := *w
		eur = &v
	}
	n := wakesPerDayMax
	if w := tv.Spec.Messaging.WakesPerDay; w != nil && *w < n {
		n = *w
	}
	return eur, n
}

// wakeRoutes runs the on: inbox routes of every tenant once (MSG-16).
func (c *Core) wakeRoutes(ctx context.Context) error {
	cat, err := c.Catalog(ctx)
	if err != nil {
		return err
	}
	for _, tenant := range slices.Sorted(mapKeys(cat.Tenants)) {
		tv := cat.Tenants[tenant]
		for _, name := range slices.Sorted(mapKeys(tv.Routes)) {
			rt := tv.Routes[name]
			if rt.On.Inbox == "" || rt.RouteMode() != "task" || rt.Task == nil {
				continue
			}
			if err := c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
				return c.runWake(ctx, tx, now, cat, tv, name, rt)
			}); err != nil {
				if ctx.Err() != nil {
					return err
				}
				c.log.Warn("wake-up route failed", "tenant", tenant, "route", name, "err", err)
			}
		}
	}
	return nil
}

// runWake enqueues one wake-up of a route when its inbox holds at least min live entries and
// no wake-up of the route is still open: from system, a type of system-ops, with the key
// system:wake:<route>:<window>, a depth not below the highest hops of the entries, the
// correlation of the oldest entry, a start_by no later than their latest expiry. The money of
// the inbox's day and the tenant's number of wake-ups are checked first; a skip writes
// wake.skipped once per window and the entries wait until their expiry (MSG-11, MSG-12).
func (c *Core) runWake(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, tv *TenantView, name string, rt manifest.Route) error {
	kind, inbox, _ := strings.Cut(rt.On.Inbox, ":")
	kinds := rt.On.Kinds
	if len(kinds) == 0 {
		kinds = []string{"report"}
	}
	var states []string
	if slices.Contains(kinds, "report") {
		states = append(states, "queued")
	}
	if slices.Contains(kinds, "state") {
		states = append(states, "current")
	}
	var n, hops int
	var latest *time.Time
	var root *string
	var inits []string
	if err := tx.QueryRow(ctx, `SELECT count(*), coalesce(max(hops), 0), max(expires_at),
			(SELECT envelope->>'correlationid' FROM messages WHERE tenant = $1 AND inbox_kind = $2 AND inbox_name = $3 AND state = ANY($4)
				AND (expires_at IS NULL OR expires_at > $5) ORDER BY seq LIMIT 1),
			coalesce((SELECT array_agg(DISTINCT i) FROM messages m2, jsonb_array_elements_text(m2.envelope->'origininitiators') i
				WHERE m2.tenant = $1 AND m2.inbox_kind = $2 AND m2.inbox_name = $3 AND m2.state = ANY($4) AND (m2.expires_at IS NULL OR m2.expires_at > $5)), '{}')
		FROM messages WHERE tenant = $1 AND inbox_kind = $2 AND inbox_name = $3 AND state = ANY($4) AND (expires_at IS NULL OR expires_at > $5)`,
		tv.Name, kind, inbox, states, now).Scan(&n, &hops, &latest, &root, &inits); err != nil {
		return err
	}
	if n < max(rt.On.Min, 1) {
		return nil
	}
	prefix := "system:wake:" + name + ":"
	var open bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM tasks WHERE tenant = $1 AND idempotency_key LIKE $2 AND state IN ('submitted', 'working'))`,
		tv.Name, prefix+"%").Scan(&open); err != nil || open {
		return err
	}
	window := wakeWindowMin
	if d, ok := parseDuration(rt.On.Debounce); ok && d > window {
		window = d
	}
	slot := now.Truncate(window)
	key := prefix + fmtSlot(slot)
	var taken bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM tasks WHERE tenant = $1 AND idempotency_key = $2)`, tv.Name, key).Scan(&taken); err != nil || taken {
		return err
	}
	skip := func(reason string) error {
		due, err := markOnce(ctx, tx, now, tv.Name, "wake:"+name+":"+reason, window)
		if err != nil || !due {
			return err
		}
		return c.appendEvent(ctx, tx, now, tv.Name, Event{Type: "wake.skipped", Subject: name, Data: map[string]any{
			"route": name, "inbox": map[string]any{"kind": kind, "name": inbox}, "reason": reason, "period_start": fmtTime(slot)}})
	}
	tt, ok := cat.TaskTypes[rt.Task.Type]
	if !ok {
		return nil
	}
	// A hold with no_message on the tenant or on the woken role or group sets no wake-ups
	// (HLD-3).
	scopes := tenantScopes(tv.Name)
	switch kind {
	case "role":
		scopes = append(scopes, scopeRef{kind: "role", tenant: tv.Name, name: inbox})
	case "group":
		scopes = append(scopes, scopeRef{kind: "bot_group", tenant: tv.Name, name: inbox})
	}
	if held, err := heldBy(ctx, tx, now, []string{"no_message"}, scopes...); err != nil {
		return err
	} else if held != "" {
		return skip("held")
	}
	// A wake-up refused in this window is not tried again before the next one: a refusal
	// writes its audit once per window, not once per pass.
	var refused bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM message_marks WHERE tenant = $1 AND key = $2 AND until > $3)`,
		tv.Name, "wake:"+name+":refused", now).Scan(&refused); err != nil || refused {
		return err
	}
	// The wake-up ceilings are checked under the tenant's budget lock, as the enqueue's own.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(7300102, hashtext($1))`, tv.Name); err != nil {
		return err
	}
	day := tenantDay(tv, now)
	eur, perDay := wakeCeilings(tv)
	var wakes int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE tenant = $1 AND created_at >= $2 AND record ? 'wake_inbox'`, tv.Name, day).Scan(&wakes); err != nil {
		return err
	}
	if wakes >= perDay {
		return skip("service_rate_limited")
	}
	estimate, err := taskEstimate(ctx, tx, now, tv.Name, rt.Task.Type, tt)
	if err != nil {
		return err
	}
	if eur != nil {
		used, err := wakeInboxUsed(ctx, tx, tv.Name, rt.On.Inbox, day, "")
		if err != nil {
			return err
		}
		if used+estimate > money.FromEUR(*eur) {
			return skip("budget_exhausted")
		}
	}
	goal := rt.Task.Goal
	if goal == "" {
		goal = tt.Title
	}
	if goal == "" {
		goal = rt.Task.Type
	}
	body := map[string]any{"type": rt.Task.Type, "goal": goal}
	if rt.Task.Params != nil {
		body["params"] = rt.Task.Params
	}
	res, err := c.enqueue(ctx, tx, now, cat, enqueueInput{
		Tenant: tv.Name, Type: rt.Task.Type, Body: body, IdempotencyKey: key, Initiator: "system",
		EnqueuedBy: "route:" + name, Source: "route", SourceID: name, CoInitiators: inits,
		Wake: &wakeInput{Inbox: rt.On.Inbox, Depth: hops, Correlation: deref(root), StartBy: latest, Subjects: inits},
	})
	reason := ""
	if p, ok := AsProblem(err); ok {
		reason = p.Reason
	} else if err != nil {
		return err
	} else if res.Rejected != nil {
		reason = res.Rejected.Reason
	}
	if reason == "" {
		return nil
	}
	if reason != "budget_exhausted" && reason != "service_rate_limited" {
		c.log.Warn("a wake-up was refused", "tenant", tv.Name, "route", name, "reason", reason)
	}
	if _, err := markOnce(ctx, tx, now, tv.Name, "wake:"+name+":refused", window); err != nil {
		return err
	}
	return skip(reason)
}

// wakeInput is what a wake-up task carries from the entries it wakes for (MSG-11, MSG-12).
type wakeInput struct {
	Inbox       string
	Depth       int
	Correlation string
	StartBy     *time.Time
	// Subjects are the origin initiators of the entries: the wake-up reserves in each one's
	// day limit too.
	Subjects []string
}

// wakeInboxUsed is the money of the day of the wake-ups of an inbox: the spend of their whole
// subtrees this day, and of their open reserves the part not spent yet, a wake-up's own reserve
// being the estimate of its subtree. The reserves of ownWake are left out: the caller's reserve
// is a draw on them (MSG-11).
func wakeInboxUsed(ctx context.Context, tx pgx.Tx, tenant, inbox string, day time.Time, ownWake string) (int64, error) {
	var used int64
	err := tx.QueryRow(ctx, `SELECT coalesce(sum(CASE WHEN s.unit <> 'task' THEN CASE WHEN s.time >= $2 THEN s.eur_micros ELSE 0 END
			WHEN t.state IN ('submitted', 'working') AND w.id <> $4 THEN greatest(s.eur_micros - coalesce((SELECT sum(x.eur_micros) FROM spend x JOIN tasks xt ON xt.id = x.task
				WHERE x.root = s.root AND x.unit <> 'task' AND (xt.id = s.task OR (s.task = w.id AND xt.record->>'wake_task' = w.id))), 0), 0)
			ELSE 0 END), 0)
		FROM spend s JOIN tasks t ON t.id = s.task JOIN tasks w ON w.id = t.record->>'wake_task'
		WHERE s.tenant = $1 AND w.tenant = $1 AND (s.time >= $2 OR t.state IN ('submitted', 'working')) AND w.record->>'wake_inbox' = $3`,
		tenant, day, inbox, ownWake).Scan(&used)
	return used, err
}

// wakeLimits checks a reserve in a wake-up's subtree, a task's estimate or a call's, against
// the wake-up's money: the day ceiling of its inbox and the day limits of the principals its
// entries came from. The wake-up's admission checked them once; its children and calls are
// checked again, so the subtree does not spend past them while it runs (MSG-11, BUD-2).
func (c *Core) wakeLimits(ctx context.Context, tx pgx.Tx, now time.Time, tv *TenantView, wake string, extra int64, line string) (*Problem, error) {
	var inbox *string
	var subjects []string
	err := tx.QueryRow(ctx, `SELECT record->>'wake_inbox', coalesce((SELECT array_agg(x) FROM jsonb_array_elements_text(coalesce(record->'wake_subjects', '[]')) x), '{}')
		FROM tasks WHERE tenant = $1 AND id = $2`, tv.Name, wake).Scan(&inbox, &subjects)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && inbox == nil) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	day := tenantDay(tv, now)
	if eur, _ := wakeCeilings(tv); eur != nil {
		used, err := wakeInboxUsed(ctx, tx, tv.Name, *inbox, day, wake)
		if err != nil {
			return nil, err
		}
		if used+extra > money.FromEUR(*eur) {
			return refuse("budget_exhausted", "the wake-ups of the inbox used up their day"), nil
		}
	}
	for _, sub := range subjects {
		sg, err := principalGroups(ctx, tx, now, tv, sub)
		if err != nil {
			return nil, err
		}
		if p := c.checkBudget(ctx, tx, now, tv, budgetAsk{subject: sub, groups: sg, extra: extra, line: line}); p != nil {
			return p, nil
		}
		if p, err := c.wakeShare(ctx, tx, now, tv, sub, sg, line, extra, wake); err != nil || p != nil {
			return p, err
		}
	}
	return nil, nil
}
