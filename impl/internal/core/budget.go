// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/ids"
	"github.com/HunterEwok/botaru/impl/internal/manifest"
	"github.com/HunterEwok/botaru/impl/internal/money"
)

// budgetLine is the budget line of a task type purpose from spec/model/task-purposes.yaml
// (BUD-7). The core sets it, never the worker or the model; a type without a purpose is work.
func (c *Core) budgetLine(purpose string) string {
	if purpose == "" {
		return "work"
	}
	if reg, err := c.spec.Registry("spec/model/task-purposes.yaml"); err == nil {
		if e, ok := reg.Entry(purpose); ok {
			if l, _ := e["budget_line"].(string); l != "" {
				return l
			}
		}
	}
	return "work"
}

// purposeOf is the purpose of a type; a type without one is work.
func purposeOf(tt manifest.TaskType) string {
	if tt.Purpose == "" {
		return "work"
	}
	return tt.Purpose
}

// taskLine is the budget line and purpose pinned in a task record at enqueue; a task written
// before they were pinned takes them from its type as applied now.
func (c *Core) taskLine(rec []byte, tt manifest.TaskType) (line, purpose string) {
	var r struct {
		Line    string `json:"budget_line"`
		Purpose string `json:"purpose"`
	}
	_ = json.Unmarshal(rec, &r)
	if r.Line != "" {
		return r.Line, r.Purpose
	}
	return c.budgetLine(tt.Purpose), purposeOf(tt)
}

// taskTreeBudget is the tree budget in micro-euros pinned in a task record at enqueue, so an
// apply that changes the type's tree_budget_eur does not move the limit of a running tree; a
// task written before takes it from its type. 0 means none.
func taskTreeBudget(rec []byte, tt manifest.TaskType) int64 {
	var r struct {
		Tree *int64 `json:"tree_budget_micros"`
	}
	_ = json.Unmarshal(rec, &r)
	if r.Tree != nil {
		return *r.Tree
	}
	return money.FromEUR(tt.TreeBudgetEUR)
}

// spendFilter narrows a window sum to a subject, a budget line and a role; an empty field
// does not narrow.
type spendFilter struct {
	subject, line, role string
}

// windowUsed sums the spend of a window: usage lines and call reserves in full, and of each
// task reserve only the part its tree has not spent yet, because the calls of a tree draw on
// its task reserve; a wake-up that joined the tree draws on its own reserve, not the root's. The task reserve of ownRoot is left out: the caller's call is that draw.
// A task reserve has no role, so a window of one role counts only the calls of that role. The
// reserve of a task that waits for the provider is kept but not counted: the reserve is taken
// again before its call (BUD-4, GW-2). An estimated line counts as zero once a reconciliation
// reached it or it was released by time, and a correction line counts its signed amount
// (BUD-10). A person's window leaves out the items of a campaign a card paid (ORC-8).
func windowUsed(ctx context.Context, tx pgx.Tx, tenant string, f spendFilter, from time.Time, ownRoot string) (int64, error) {
	var used int64
	err := tx.QueryRow(ctx, `WITH w AS (SELECT * FROM spend WHERE tenant = $1 AND time >= $2
			AND ($3 = '' OR subject = $3 AND (task IS NULL OR NOT EXISTS (SELECT 1 FROM tasks t WHERE t.id = spend.task AND t.record ? 'paid_by_card'))) AND ($5 = '' OR line = $5) AND ($6 = '' OR role = $6))
		SELECT coalesce((SELECT sum(CASE WHEN state = 'estimated' AND (reconciled_at IS NOT NULL OR released_at IS NOT NULL) THEN 0
				ELSE eur_micros + coalesce(correction_eur_micros, 0) END) FROM w WHERE unit <> 'task'), 0)
			+ coalesce((SELECT sum(greatest(r.eur_micros - coalesce((SELECT sum(s.eur_micros) FROM spend s LEFT JOIN tasks u ON u.id = s.task
					WHERE s.root = r.root AND s.unit <> 'task' AND s.time >= r.time
					AND (u.record->>'wake_task') IS NOT DISTINCT FROM (SELECT x.record->>'wake_task' FROM tasks x WHERE x.id = r.task)), 0), 0))
				FROM w r WHERE r.unit = 'task' AND r.root <> $4
				AND NOT EXISTS (SELECT 1 FROM tasks t WHERE t.id = r.task AND t.state = 'submitted' AND t.reason IN ('provider_unavailable', 'provider_quota'))), 0)`,
		tenant, from, f.subject, ownRoot, f.line, f.role).Scan(&used)
	return used, err
}

// budgetAsk is one reserve to check: who it is for, its budget line and role, and its size.
type budgetAsk struct {
	subject string
	groups  []string
	// extra is the reserve in micro-euros; 0 checks only that no limit is used up.
	extra int64
	// ownRoot is the tree whose task reserve the reserve draws on.
	ownRoot string
	// line is the budget line (BUD-7); empty is work.
	line string
	// role is the role of a call; empty at enqueue, where the per-role limit is not known.
	role string
	// sev1 marks a seat ask for a sev1 incident, which may use the reserve of the manager line
	// (MGR-20).
	sev1 bool
	// crossed collects the windows whose 80% threshold the ask reaches. The caller gives the
	// events only once the ask is accepted, so a refused ask takes no threshold slot (BUD-4).
	crossed *[]crossing
}

// crossing is a window at its 80% threshold.
type crossing struct {
	scope, subject string
	period         time.Time
}

// emitCrossed gives the threshold events of an accepted ask.
func (c *Core) emitCrossed(ctx context.Context, tx pgx.Tx, now time.Time, tenant string, crossed []crossing) error {
	for _, x := range crossed {
		if err := c.budgetThreshold(ctx, tx, now, tenant, x.scope, x.subject, x.period); err != nil {
			return err
		}
	}
	return nil
}

// checkBudget compares the money windows with the limits (BUD-1, BUD-7): the tenant's month
// and day, the month and day of the budget line and its limit per role, and the person's day
// limit from their groups, which counts only the work line. Reserves count until they are
// released. With a reserve it refuses when the reserve would pass a limit (BUD-2). The hourly
// figure is not a reserve limit: it is the threshold of the runaway_spend hold (BUD-4, HLD-7).
// The tenant's budget lock is held until the transaction ends, so two reserves of one tenant
// never both pass on the same old sum.
func (c *Core) checkBudget(ctx context.Context, tx pgx.Tx, now time.Time, tv *TenantView, ask budgetAsk) *Problem {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(7300102, hashtext($1))`, tv.Name); err != nil {
		return refuse("not_ready", "store")
	}
	over := func(used int64, limit float64) bool {
		if ask.extra > 0 {
			return used+ask.extra > money.FromEUR(limit)
		}
		return used >= money.FromEUR(limit)
	}
	line := ask.line
	if line == "" {
		line = "work"
	}
	b := tv.Spec.Budgets
	// Days and months are the tenant's calendar ones (TIM-2).
	loc := time.UTC
	if l, err := time.LoadLocation(tv.Spec.Timezone); err == nil && tv.Spec.Timezone != "" {
		loc = l
	}
	local := now.In(loc)
	month := time.Date(local.Year(), local.Month(), 1, 0, 0, 0, 0, loc)
	day := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
	dayLimit := b.DayEUR
	if dayLimit == nil && b.MonthEUR != nil {
		v := *b.MonthEUR / 20
		dayLimit = &v
	}
	type window struct {
		limit  *float64
		from   time.Time
		filter spendFilter
		why    string
		// scope names the window for the 80% event; "" has none.
		scope string
	}
	lb := b.Lines[line]
	lineMonth := lb.MonthEUR
	if lineMonth == nil && line != "work" {
		// Analysis, reviews and the other lines are turned on explicitly with money (BUD-7).
		zero := 0.0
		lineMonth = &zero
	}
	windows := []window{
		{b.MonthEUR, month, spendFilter{}, "the tenant limit is used up", "tenant_month"},
		{dayLimit, day, spendFilter{}, "the tenant limit is used up", "tenant_day"},
		{lineMonth, month, spendFilter{line: line}, "the budget line is used up", ""},
		{lb.DayEUR, day, spendFilter{line: line}, "the budget line is used up", ""},
	}
	// 30% of the manager line's day is a reserve for sev1: work outside sev1 stops at 70% (MGR-20).
	if line == "manager" && lb.DayEUR != nil && !ask.sev1 {
		v := *lb.DayEUR * managerNonSev1Share
		windows = append(windows, window{&v, day, spendFilter{line: line}, "the manager line keeps its sev1 reserve", ""})
	}
	if ask.role != "" {
		windows = append(windows, window{lb.PerRoleMonthEUR, month, spendFilter{line: line, role: ask.role}, "the budget line of the role is used up", ""})
	}
	if line == "work" && ask.subject != "system" {
		var personal *float64
		for _, g := range ask.groups {
			if l := tv.Groups[g].DayEUR; l != nil && (personal == nil || *l > *personal) {
				personal = l
			}
		}
		windows = append(windows, window{personal, day, spendFilter{subject: ask.subject, line: "work"}, "the personal day limit is used up", "person_day"})
	}
	// After a restore without the provider report the daily limits are halved until a
	// reconciliation covers the gap (RET-6).
	halve, err := spendGapOpen(ctx, tx)
	if err != nil {
		return refuse("not_ready", "store")
	}
	var crossed []crossing
	for _, w := range windows {
		if w.limit == nil {
			continue
		}
		// Only the day windows: on the first of a month the month windows start at the same time.
		if halve && (w.limit == dayLimit || w.limit == lb.DayEUR || w.scope == "person_day" || w.why == "the manager line keeps its sev1 reserve") {
			half := *w.limit / 2
			w.limit = &half
		}
		used, err := windowUsed(ctx, tx, tv.Name, w.filter, w.from, ask.ownRoot)
		if err != nil {
			return refuse("not_ready", "store")
		}
		if w.scope == "person_day" {
			// The wake-ups the person's entries caused count in the person's day (MSG-11, BUD-1).
			wakes, err := wakeSum(ctx, tx, tv.Name, w.from, "", "subject", ask.subject)
			if err != nil {
				return refuse("not_ready", "store")
			}
			used += wakes
		}
		if over(used, *w.limit) {
			return refuse("budget_exhausted", "%s", w.why)
		}
		if w.scope != "" && *w.limit > 0 && float64(used+ask.extra) >= 0.8*float64(money.FromEUR(*w.limit)) {
			crossed = append(crossed, crossing{w.scope, w.filter.subject, w.from})
		}
	}
	if ask.crossed != nil {
		*ask.crossed = append(*ask.crossed, crossed...)
		return nil
	}
	if err := c.emitCrossed(ctx, tx, now, tv.Name, crossed); err != nil {
		return refuse("not_ready", "store")
	}
	return nil
}

// budgetThreshold gives the event budget.threshold once per period of a window when the
// usage with the reserves of running attempts reaches 80% of its limit (BUD-4). The event has
// no amounts; a person's window names the person as its subject.
func (c *Core) budgetThreshold(ctx context.Context, tx pgx.Tx, now time.Time, tenant, scope, subject string, period time.Time) error {
	key := scope
	if subject != "" {
		key += ":" + subject
	}
	tag, err := tx.Exec(ctx, `INSERT INTO budget_thresholds (tenant, scope, period) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`, tenant, key, period)
	if err != nil || tag.RowsAffected() == 0 {
		return err
	}
	return c.appendEvent(ctx, tx, now, tenant, Event{Type: "budget.threshold", Subject: subject,
		Data: map[string]any{"scope": scope, "threshold_pct": 80, "period_start": fmtTime(period)}})
}

// spendRow is a usage line or a reserve of one call (BUD-3).
type spendRow struct {
	ID, Tenant, Task, Root, Subject, Role, Worker string
	Line, Purpose, Unit, Profile, Fingerprint     string
	Toolcall                                      string
	// Provider is the model provider of a call line: with the tenant it names the workspace a
	// reconciliation compares (BUD-10).
	Provider string
	Micros   int64
	// LeaseNo is the lease of the attempt of a model call line (CNT-4); 0 for none.
	LeaseNo int
}

// reserveSpend writes the reserve of a call after the limits pass (BUD-2): the windows of
// checkBudget and the tree budget. A call of no price passes no limit and still gets its line.
func (c *Core) reserveSpend(ctx context.Context, tx pgx.Tx, now time.Time, tv *TenantView, r spendRow, groups []string, treeBudget int64) error {
	if r.Micros > 0 {
		var crossed []crossing
		sev1, err := taskSev1(ctx, tx, r.Line, r.Task)
		if err != nil {
			return err
		}
		if p := c.checkBudget(ctx, tx, now, tv, budgetAsk{subject: r.Subject, groups: groups, extra: r.Micros, ownRoot: r.Root, line: r.Line, role: r.Role, sev1: sev1, crossed: &crossed}); p != nil {
			return p
		}
		// A call in a wake-up's subtree draws on the wake-up's money too (MSG-11).
		var wake *string
		if r.Task != "" {
			if err := tx.QueryRow(ctx, `SELECT record->>'wake_task' FROM tasks WHERE id = $1`, r.Task).Scan(&wake); err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
		}
		if wake != nil {
			if p, err := c.wakeLimits(ctx, tx, now, tv, *wake, r.Micros, r.Line); err != nil {
				return err
			} else if p != nil {
				return p
			}
		}
		if treeBudget > 0 {
			var used int64
			if err := tx.QueryRow(ctx, `SELECT coalesce(sum(eur_micros), 0) FROM spend WHERE root = $1 AND unit <> 'task'`, r.Root).Scan(&used); err != nil {
				return err
			}
			if used+r.Micros > treeBudget {
				return refuse("budget_exhausted", "the call reserve does not fit the tree budget")
			}
		}
		if err := c.emitCrossed(ctx, tx, now, r.Tenant, crossed); err != nil {
			return err
		}
	}
	if r.Provider != "" {
		if _, err := tx.Exec(ctx, `INSERT INTO spend_workspaces (tenant, provider, first_at) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`, r.Tenant, r.Provider, now); err != nil {
			return err
		}
	}
	// The usage line names the identity of the attempt that made the call (IDN-8).
	var acting actingOf
	if r.Task != "" && r.Worker != "" {
		var err error
		if acting, err = leaseActing(ctx, tx, r.Task, r.Worker); err != nil {
			return err
		}
	}
	var be any
	if acting.Bot != "" {
		be = acting.BotEpoch
	}
	var leaseNo any
	if r.LeaseNo > 0 {
		leaseNo = r.LeaseNo
	}
	_, err := tx.Exec(ctx, `INSERT INTO spend (id, tenant, task, root, subject, role, worker, line, purpose, unit, profile, state, eur_micros, time, fingerprint, toolcall, provider, acting_bot, bot_epoch, attempt_lease_no)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, 'reserved', $12, $13, $14, $15, $16, $17, $18, $19)`,
		r.ID, r.Tenant, r.Task, r.Root, r.Subject, r.Role, r.Worker, r.Line, nullable(r.Purpose), r.Unit, nullable(r.Profile), r.Micros, now,
		nullable(r.Fingerprint), nullable(r.Toolcall), nullable(r.Provider), nullable(acting.Bot), be, leaseNo)
	return err
}

// managerNonSev1Share is the share of the manager line's day that work outside sev1 may use
// (MGR-20).
const managerNonSev1Share = 0.7

// taskSev1 says whether a task of the manager line acts on a sev1 incident: the severity the
// core pinned in its record at enqueue (MGR-20).
func taskSev1(ctx context.Context, tx pgx.Tx, line, task string) (bool, error) {
	if line != "manager" || task == "" {
		return false, nil
	}
	var sev string
	err := tx.QueryRow(ctx, `SELECT coalesce(record->>'severity', '') FROM tasks WHERE id = $1`, task).Scan(&sev)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return sev == "sev1", err
}

// toolPrice is the price of one call in micro-euros: the manifest price of a tool the farm
// pays outside the model provider, else 0 (BUD-5).
func toolPrice(billing string, priceEUR float64) int64 {
	if billing == "external" {
		return money.FromEUR(priceEUR)
	}
	return 0
}

// reserveToolCall reserves the manifest price of a read call before it is queued (BUD-2,
// BUD-5) on the line of the task's purpose (BUD-7). The reserve becomes the usage line of the
// call when a toolhost takes it, and is given back when nobody does (migration 0005).
func (c *Core) reserveToolCall(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, g *toolsGrant, callID string, micros int64) error {
	tv := cat.Tenants[g.Tenant]
	if tv == nil {
		return refuse("not_ready", "tenant is not applied")
	}
	var rec []byte
	var fp *string
	if err := tx.QueryRow(ctx, `SELECT t.record, l.fingerprint FROM tasks t JOIN leases l ON l.work_id = t.id AND l.lease_no = $2 WHERE t.id = $1`,
		g.Task, g.LeaseNo).Scan(&rec, &fp); err != nil {
		return err
	}
	var r struct {
		Root string `json:"root"`
	}
	_ = json.Unmarshal(rec, &r)
	if r.Root == "" {
		r.Root = g.Task
	}
	line, purpose := c.taskLine(rec, cat.TaskTypes[g.Type])
	var groups []string
	if p, err := loadPrincipal(ctx, tx, now, g.Tenant, g.Initiator); err == nil {
		groups = groupClosure(tv, p.Groups)
	}
	tree := taskTreeBudget(rec, cat.TaskTypes[g.Type])
	return c.reserveSpend(ctx, tx, now, tv, spendRow{
		ID: ids.New(ids.Spend), Tenant: g.Tenant, Task: g.Task, Root: r.Root, Subject: g.Initiator, Role: g.Role, Worker: g.Worker,
		Line: line, Purpose: purpose, Unit: "eur_cents", Fingerprint: deref(fp), Toolcall: callID, Micros: micros,
	}, groups, tree)
}

// runawayHold is the hold of HLD-7 from spec/model/holds.yaml (runaway_spend_hour).
const runawayHold = time.Hour

// runawaySpend sets the runaway_spend hold of HLD-7: a tenant whose usage over the sliding hour
// is above hour_eur (by default a hundredth of month_eur) gets a no_claim hold on every agent
// role for an hour, by the author contain_auto, and the alarm alarm.hold.auto. A job role
// keeps working. A role that already has such a hold gets no second one; the alarm resolves
// when no such hold is active in the tenant.
func (c *Core) runawaySpend(ctx context.Context) error {
	cat, err := c.Catalog(ctx)
	if err != nil {
		return err
	}
	var agents []string
	for name, r := range cat.Roles {
		if r.WorkerKind == "agent" {
			agents = append(agents, name)
		}
	}
	slices.Sort(agents)
	names := make([]string, 0, len(cat.Tenants))
	for name := range cat.Tenants {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		tv := cat.Tenants[name]
		b := tv.Spec.Budgets
		limit := b.HourEUR
		if limit == nil && b.MonthEUR != nil {
			v := *b.MonthEUR / 100
			limit = &v
		}
		err := c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
			// The same lock as the reserves: two copies of the loop set one hold.
			if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(7300102, hashtext($1))`, name); err != nil {
				return err
			}
			a := Alarm{Tenant: name, Name: "hold.auto", Subject: AlarmSubject{Kind: "tenant", ID: name}, Severity: "page"}
			var used int64
			if err := tx.QueryRow(ctx, `SELECT coalesce(sum(eur_micros), 0) FROM spend WHERE tenant = $1 AND time > $2 AND unit <> 'task' AND state <> 'reserved'`,
				name, now.Add(-time.Hour)).Scan(&used); err != nil {
				return err
			}
			if limit == nil || used <= money.FromEUR(*limit) {
				var n int
				if err := tx.QueryRow(ctx, `SELECT count(*) FROM holds WHERE tenant = $1 AND state = 'active' AND reason = 'runaway_spend'
					AND (until IS NULL OR until > $2)`, name, now).Scan(&n); err != nil {
					return err
				}
				if n == 0 {
					return c.resolveAlarm(ctx, tx, now, a)
				}
				return nil
			}
			until := now.Add(runawayHold)
			set := 0
			for _, role := range agents {
				var n int
				if err := tx.QueryRow(ctx, `SELECT count(*) FROM holds WHERE tenant = $1 AND state = 'active' AND reason = 'runaway_spend'
					AND scope_kind = 'role' AND scope_tenant = $1 AND scope_name = $2 AND (until IS NULL OR until > $3)`, name, role, now).Scan(&n); err != nil {
					return err
				}
				if n > 0 {
					continue
				}
				actor := HoldActor{Principal: "system", Rank: "contain_auto", Tenant: name, Via: "system", Source: "runaway_spend_hour"}
				if _, _, err := c.insertHold(ctx, tx, now, actor, HoldScope{Kind: "role", Tenant: name, Name: role}, []string{"no_claim"}, "runaway_spend", &until); err != nil {
					return err
				}
				set++
			}
			if set == 0 {
				return nil
			}
			a.Values = map[string]any{"reason": "runaway_spend", "hour_cents": money.CentsUp(used), "limit_cents": money.CentsUp(money.FromEUR(*limit)), "roles": set}
			return c.fireAlarm(ctx, tx, now, a)
		})
		if err != nil {
			return err
		}
	}
	return nil
}
