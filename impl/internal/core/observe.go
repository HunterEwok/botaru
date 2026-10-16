// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/ids"
	"github.com/HunterEwok/botaru/impl/internal/mcp"
	"github.com/HunterEwok/botaru/impl/internal/store"
	"github.com/HunterEwok/botaru/impl/internal/toolspec"
)

// The core series of spec/model/core-series.yaml that the observe loop writes (OBV-5, OBV-6).
const (
	seriesTaskOutcome   = "farm.task.outcome"
	seriesTaskAttempts  = "farm.task.attempts"
	seriesTaskCost      = "farm.task.cost"
	seriesQueueWait     = "farm.queue.wait"
	seriesAttemptTime   = "farm.attempt.duration"
	seriesScheduleSlot  = "farm.schedule.slot"
	seriesBackupAge     = "farm.backup.age"
	seriesExtDuration   = "farm.external.duration"
	seriesExtRetries    = "farm.external.retries"
	seriesExtOutcome    = "farm.external.outcome"
	coreSeriesMark      = "internal"
	obsRawDays          = 14
	obsRollupDays       = 93
	obsLookback         = 10 * time.Minute
	obsBatch            = 500
	obsMaxPoints        = 2232
	obsMaxGroups        = 50
	runFinishedType     = "run.finished.v1"
	scheduleSkippedType = "schedule.skipped"
)

// seriesDef is one series a tenant can read: a core series of the registry or a tenant
// ObservationSeries (OBV-2, OBV-6).
type seriesDef struct {
	Name, CatalogKind, DataClass, Source string
	// Tool and Field map a closed field of a tool's output to the series (OBV-1).
	Tool, Field string
	// Enum lists the values of a series of a closed list; empty for a numeric one.
	Enum     []string
	Min, Max *float64
	Rollups  []string
	RawDays  int
	RollDays int
	Outcome  *seriesOutcome
}

// seriesOutcome is the outcome block of a series; Complete says whether it can be a trial metric
// or guard (OBV-6).
type seriesOutcome struct {
	Compare, Better, Good     string
	TolerancePct, TolerancePP float64
	MinSamples                int
	Complete                  bool
}

// tenantSeries are the series of a tenant by name: the core series of v0.1 and the tenant's own.
func (c *Core) tenantSeries(cat *Catalog, tenant string) map[string]seriesDef {
	out := map[string]seriesDef{}
	if reg, err := c.spec.Registry("spec/model/core-series.yaml"); err == nil {
		for _, e := range reg.Entries {
			if e["since"] != "v0.1" {
				// The decision statistics of v0.2 are core series of this build (DIG-4).
				if d, ok := c.decisionSeries(asString(e["id"])); ok {
					out[d.Name] = d
				} else if e["id"] == seriesTakeoverCount && claimsV03() {
					out[seriesTakeoverCount] = takeoverSeries()
				}
				continue
			}
			d := seriesDef{Name: asString(e["id"]), CatalogKind: asString(e["catalog_kind"]), DataClass: coreSeriesMark, Source: "core",
				Enum: strList(e["values"]), RawDays: obsRawDays, RollDays: obsRollupDays}
			// A numeric core series keeps its day rollups for the rollup period; a series of a
			// closed list has only the window share over its raw points.
			if len(d.Enum) == 0 {
				d.Rollups = []string{"day.max", "day.p95"}
			}
			if o, ok := e["outcome"].(map[string]any); ok {
				d.Outcome = decodeOutcome(o)
			}
			out[d.Name] = d
		}
	}
	for k, raw := range cat.RawGlobals {
		// A tenant series never takes a core series name (apply refuses it, OBV-6).
		if _, core := out[k.Name]; k.Kind != "ObservationSeries" || k.Tenant != tenant || core || strings.HasPrefix(k.Name, "farm.") {
			continue
		}
		spec, _ := raw["spec"].(map[string]any)
		d := seriesDef{Name: k.Name, CatalogKind: asString(spec["catalog_kind"]), DataClass: asString(spec["data_class"]),
			Rollups: strList(spec["rollups"]), RawDays: obsRawDays, RollDays: obsRollupDays}
		src, _ := spec["source"].(map[string]any)
		d.Source, d.Tool, d.Field = asString(src["kind"]), asString(src["tool"]), asString(src["field"])
		val, _ := spec["value"].(map[string]any)
		d.Enum = strList(val["enum"])
		if n, ok := toFloat(val["minimum"]); ok {
			d.Min = &n
		}
		if n, ok := toFloat(val["maximum"]); ok {
			d.Max = &n
		}
		if ret, ok := spec["retention"].(map[string]any); ok {
			if days := durationDays(asString(ret["raw"])); days > 0 {
				d.RawDays = days
			}
			if days := durationDays(asString(ret["rollup"])); days > 0 {
				d.RollDays = days
			}
		}
		if o, ok := spec["outcome"].(map[string]any); ok {
			d.Outcome = decodeOutcome(o)
		}
		out[d.Name] = d
	}
	return out
}

func decodeOutcome(o map[string]any) *seriesOutcome {
	r := &seriesOutcome{Compare: asString(o["compare"]), Better: asString(o["better"]), Good: asString(o["good"])}
	r.TolerancePct, _ = toFloat(o["tolerance_pct"])
	r.TolerancePP, _ = toFloat(o["tolerance_pp"])
	n, _ := toFloat(o["min_samples"])
	r.MinSamples = int(n)
	_, pct := o["tolerance_pct"]
	_, pp := o["tolerance_pp"]
	r.Complete = r.Compare != "" && r.Better != "" && (pct || pp) && r.MinSamples > 0
	return r
}

func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	}
	return 0, false
}

// durationDays turns a duration of the manifest into whole days (at least 1 for anything
// under a day).
func durationDays(s string) int {
	d, ok := parseDuration(s)
	if !ok || d <= 0 {
		return 0
	}
	return max(1, int(math.Ceil(d.Hours()/24)))
}

// obsPoint is one point to write.
type obsPoint struct {
	Tenant, Series, Subject, Source, Mark, Worker, Role, Key string
	At                                                       time.Time
	Num                                                      *float64
	Enum                                                     string
}

func num(v float64) *float64 { return &v }

// writePoint writes a point once: the same key writes nothing again (OBV-5).
func writePoint(ctx context.Context, tx pgx.Tx, now time.Time, p obsPoint) error {
	var enum *string
	if p.Num == nil {
		enum = &p.Enum
	}
	_, err := tx.Exec(ctx, `INSERT INTO obs_points (tenant, series, subject, at, num, enum, source, data_mark, worker, role, key, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12) ON CONFLICT DO NOTHING`,
		p.Tenant, p.Series, p.Subject, p.At, p.Num, enum, p.Source, p.Mark, nullable(p.Worker), nullable(p.Role), p.Key, now)
	return err
}

// observe is the observe loop: the core series from the records of each tenant, the backup
// age, the rollups of closed periods and the deletion of expired points and rollups (OBV-4,
// OBV-5, OBV-7).
func (c *Core) observe(ctx context.Context) error {
	cat, err := c.Catalog(ctx)
	if err != nil {
		return err
	}
	for _, step := range []func(context.Context, *Catalog) error{c.observeTasks, c.observeAttempts, c.observeEvents, c.observeBackupAge, c.observeRollups, c.observeRetention} {
		if err := step(ctx, cat); err != nil {
			return err
		}
	}
	return nil
}

// obsCursor gives where a pass starts: strictly after the keyset position (at, id) of a full
// batch, so a run of rows with one time cannot hold the cursor; otherwise from at minus the
// look-back with no id, so a late commit is read again.
func obsCursor(ctx context.Context, q store.Querier, name string, now time.Time) (time.Time, string, error) {
	var at time.Time
	var id string
	var full bool
	err := q.QueryRow(ctx, `SELECT at, last_id, full_batch FROM obs_cursor WHERE name = $1`, name).Scan(&at, &id, &full)
	if errors.Is(err, pgx.ErrNoRows) {
		return now.AddDate(0, 0, -obsRawDays), "", nil
	}
	if err != nil || full {
		return at, id, err
	}
	return at.Add(-obsLookback), "", nil
}

// setObsCursor stores the end of a pass: the keyset position of a full batch, or the later of
// the stored time, the last row and the look-back edge.
func setObsCursor(ctx context.Context, tx pgx.Tx, name string, n int, at time.Time, id string, now time.Time) error {
	if n == obsBatch {
		_, err := tx.Exec(ctx, `INSERT INTO obs_cursor (name, at, last_id, full_batch) VALUES ($1, $2, $3, true)
			ON CONFLICT (name) DO UPDATE SET at = $2, last_id = $3, full_batch = true`, name, at, id)
		return err
	}
	_, err := tx.Exec(ctx, `INSERT INTO obs_cursor (name, at, last_id, full_batch) VALUES ($1, $2, '', false)
		ON CONFLICT (name) DO UPDATE SET at = greatest(obs_cursor.at, $2), last_id = '', full_batch = false`, name, maxTime(at, now.Add(-obsLookback)))
	return err
}

// taskOutcome is the value of farm.task.outcome for a task that ended.
func taskOutcome(state, reason string) string {
	switch {
	case state == "completed":
		return "done"
	case state == "canceled":
		return "canceled"
	case reason == "deadline_exceeded":
		return "deadline_exceeded"
	}
	return "failed"
}

// observeTasks writes the task series of the tasks that ended: outcome, attempts, cost and
// queue wait, and the slot of a schedule's task (OBV-6). A record is read again within the
// look-back, and its points are written once by key, so a late commit is not missed.
func (c *Core) observeTasks(ctx context.Context, cat *Catalog) error {
	return c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		from, after, err := obsCursor(ctx, tx, "tasks", now)
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT t.id, t.tenant, t.type, t.state, coalesce(t.reason, ''), t.created_at, t.updated_at, t.idempotency_key,
			(SELECT count(*) FROM leases l WHERE l.work_id = t.id AND l.kind = 'attempt'),
			(SELECT min(l.created_at) FROM leases l WHERE l.work_id = t.id AND l.kind = 'attempt'),
			(SELECT l.worker_id FROM leases l WHERE l.work_id = t.id AND l.kind = 'attempt' ORDER BY l.lease_no DESC LIMIT 1),
			(SELECT w.role FROM leases l JOIN workers w ON w.id = l.worker_id WHERE l.work_id = t.id AND l.kind = 'attempt' ORDER BY l.lease_no DESC LIMIT 1),
			coalesce((SELECT sum(s.eur_micros) FROM spend s WHERE s.task = t.id AND s.state <> 'reserved'), 0)
			FROM tasks t WHERE t.state IN ('completed', 'failed', 'canceled') AND (t.updated_at, t.id) > ($1, $3) ORDER BY t.updated_at, t.id LIMIT $2`, from, obsBatch, after)
		if err != nil {
			return err
		}
		type row struct {
			id, tenant, typ, state, reason, key string
			created, updated                    time.Time
			attempts                            int
			first                               *time.Time
			worker, role                        *string
			micros                              int64
		}
		rs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
			var x row
			return x, r.Scan(&x.id, &x.tenant, &x.typ, &x.state, &x.reason, &x.created, &x.updated, &x.key, &x.attempts, &x.first, &x.worker, &x.role, &x.micros)
		})
		if err != nil {
			return err
		}
		last, lastID := from, ""
		for _, r := range rs {
			last, lastID = r.updated, r.id
			subject := "task_type:" + r.typ
			base := obsPoint{Tenant: r.tenant, Subject: subject, Source: "core", Mark: coreSeriesMark, Worker: deref(r.worker), Role: deref(r.role), At: r.updated}
			pts := []obsPoint{}
			p := base
			p.Series, p.Enum, p.Key = seriesTaskOutcome, taskOutcome(r.state, r.reason), "task:"+r.id
			pts = append(pts, p)
			if r.attempts > 0 {
				p = base
				p.Series, p.Num, p.Key = seriesTaskAttempts, num(float64(r.attempts)), "task:"+r.id
				pts = append(pts, p)
				p = base
				p.Series, p.Num, p.Key = seriesTaskCost, num(float64(r.micros)/1e4), "task:"+r.id
				pts = append(pts, p)
			}
			if r.first != nil {
				p = base
				p.Series, p.Num, p.Key, p.At = seriesQueueWait, num(r.first.Sub(r.created).Seconds()), "task:"+r.id, *r.first
				pts = append(pts, p)
			}
			if slot, ok := c.slotPoint(cat, r.tenant, r.key, r.state, r.created, r.first, base); ok {
				pts = append(pts, slot)
			}
			for _, p := range pts {
				if err := writePoint(ctx, tx, now, p); err != nil {
					return err
				}
			}
		}
		return setObsCursor(ctx, tx, "tasks", len(rs), last, lastID, now)
	})
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// slotPoint gives the farm.schedule.slot point of a schedule's task: on_time when the task
// was taken within the start window of its type and completed, late otherwise (TIM-3).
func (c *Core) slotPoint(cat *Catalog, tenant, key, state string, created time.Time, first *time.Time, base obsPoint) (obsPoint, bool) {
	rest, ok := strings.CutPrefix(key, "system:schedule:")
	if !ok {
		return obsPoint{}, false
	}
	name, slot, ok := strings.Cut(rest, ":")
	if !ok {
		return obsPoint{}, false
	}
	tv := cat.Tenants[tenant]
	if tv == nil {
		return obsPoint{}, false
	}
	value := "late"
	if sc, ok := tv.Schedules[name]; ok && sc.Task != nil && state == "completed" && first != nil {
		startBy, ok := parseDuration(cat.TaskTypes[sc.Task.Type].Timeouts.StartBy)
		if !ok || startBy <= 0 {
			startBy = 15 * time.Minute
		}
		if first.Sub(created) <= startBy {
			value = "on_time"
		}
	}
	p := base
	p.Series, p.Subject, p.Enum, p.Num, p.Key = seriesScheduleSlot, "schedule:"+name, value, nil, "slot:"+name+":"+slot
	return p, true
}

// observeAttempts writes farm.attempt.duration for each attempt that ended.
func (c *Core) observeAttempts(ctx context.Context, _ *Catalog) error {
	return c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		from, after, err := obsCursor(ctx, tx, "attempts", now)
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT l.work_id, l.lease_no, l.tenant, t.type, l.worker_id, w.role, l.created_at, l.closed_at,
			l.work_id || '#' || lpad(l.lease_no::text, 8, '0') AS k
			FROM leases l JOIN tasks t ON t.id = l.work_id JOIN workers w ON w.id = l.worker_id
			WHERE l.kind = 'attempt' AND l.closed_at IS NOT NULL AND (l.closed_at, l.work_id || '#' || lpad(l.lease_no::text, 8, '0')) > ($1, $3)
			ORDER BY l.closed_at, k LIMIT $2`, from, obsBatch, after)
		if err != nil {
			return err
		}
		type row struct {
			work, tenant, typ, worker, role, k string
			no                                 int
			created, closed                    time.Time
		}
		rs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
			var x row
			return x, r.Scan(&x.work, &x.no, &x.tenant, &x.typ, &x.worker, &x.role, &x.created, &x.closed, &x.k)
		})
		if err != nil {
			return err
		}
		last, lastID := from, ""
		for _, r := range rs {
			last, lastID = r.closed, r.k
			if err := writePoint(ctx, tx, now, obsPoint{Tenant: r.tenant, Series: seriesAttemptTime, Subject: "task_type:" + r.typ, Source: "core",
				Mark: coreSeriesMark, Worker: r.worker, Role: r.role, At: r.closed, Num: num(r.closed.Sub(r.created).Seconds()),
				Key: fmt.Sprintf("lease:%s:%d", r.work, r.no)}); err != nil {
				return err
			}
		}
		return setObsCursor(ctx, tx, "attempts", len(rs), last, lastID, now)
	})
}

// observeEvents writes the points of run.finished.v1 (the farm.external.* series and the last
// good backup) and of skipped schedule slots (OBV-7, TIM-3).
func (c *Core) observeEvents(ctx context.Context, cat *Catalog) error {
	return c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		from, after, err := obsCursor(ctx, tx, "events", now)
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT id, tenant, type, coalesce(subject, ''), time, data FROM events
			WHERE type IN ($3, $4) AND (time, id) > ($1, $5) ORDER BY time, id LIMIT $2`, from, obsBatch, runFinishedType, scheduleSkippedType, after)
		if err != nil {
			return err
		}
		type row struct {
			id, tenant, typ, subject string
			at                       time.Time
			data                     map[string]any
		}
		rs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
			var x row
			return x, r.Scan(&x.id, &x.tenant, &x.typ, &x.subject, &x.at, &x.data)
		})
		if err != nil {
			return err
		}
		last, lastID := from, ""
		for _, r := range rs {
			last, lastID = r.at, r.id
			if r.typ == scheduleSkippedType {
				name, slot := asString(r.data["schedule"]), asString(r.data["slot"])
				if err := writePoint(ctx, tx, now, obsPoint{Tenant: r.tenant, Series: seriesScheduleSlot, Subject: "schedule:" + name, Source: "core",
					Mark: coreSeriesMark, At: r.at, Enum: "skipped", Key: "slot:" + name + ":" + slot}); err != nil {
					return err
				}
				continue
			}
			if err := c.observeRunFinished(ctx, tx, now, cat, r.tenant, r.id, r.at, r.data); err != nil {
				return err
			}
		}
		return setObsCursor(ctx, tx, "events", len(rs), last, lastID, now)
	})
}

// observeRunFinished writes the farm.external.* points of the task type that emitted the event
// and, for a backup type that ended done, the last good backup of the subject (OBV-7).
func (c *Core) observeRunFinished(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, tenant, id string, at time.Time, data map[string]any) error {
	typ, task := asString(data["task_type"]), asString(data["task"])
	if typ == "" {
		return nil
	}
	worker, role := "", ""
	if task != "" {
		_ = tx.QueryRow(ctx, `SELECT l.worker_id, w.role FROM leases l JOIN workers w ON w.id = l.worker_id
			WHERE l.work_id = $1 AND l.kind = 'attempt' ORDER BY l.lease_no DESC LIMIT 1`, task).Scan(&worker, &role)
	}
	base := obsPoint{Tenant: tenant, Subject: "task_type:" + typ, Source: "core", Mark: coreSeriesMark, Worker: worker, Role: role, At: at, Key: "event:" + id}
	pts := []obsPoint{}
	if d, ok := toFloat(data["duration_s"]); ok {
		p := base
		p.Series, p.Num = seriesExtDuration, num(d)
		pts = append(pts, p)
	}
	if n, ok := toFloat(data["retries"]); ok {
		p := base
		p.Series, p.Num = seriesExtRetries, num(n)
		pts = append(pts, p)
	}
	outcome := asString(data["outcome"])
	if outcome != "" {
		p := base
		p.Series, p.Enum = seriesExtOutcome, outcome
		pts = append(pts, p)
	}
	// A tenant series of source job takes its closed field of the event on the event's
	// catalog subject; a value outside the series writes nothing (OBV-1, OBV-7).
	for _, d := range c.tenantSeries(cat, tenant) {
		if d.Source != "job" || d.Field == "" {
			continue
		}
		subject := d.CatalogKind + ":" + asString(data["subject"])
		v, ok := lookupField(data, d.Field)
		if !ok || !c.subjectOfTenant(cat, tenant, d.CatalogKind, subject) {
			continue
		}
		p := obsPoint{Tenant: tenant, Series: d.Name, Subject: subject, Source: "job", Mark: d.DataClass, Worker: worker, Role: role, At: at, Key: "event:" + id}
		if d.setValue(&p, v) {
			pts = append(pts, p)
		}
	}
	for _, p := range pts {
		if err := writePoint(ctx, tx, now, p); err != nil {
			return err
		}
	}
	if outcome == "done" && cat.TaskTypes[typ].MakesBackup {
		_, err := tx.Exec(ctx, `INSERT INTO obs_backups (tenant, subject, last_good) VALUES ($1, $2, $3)
			ON CONFLICT (tenant, subject) DO UPDATE SET last_good = greatest(obs_backups.last_good, $3)`, tenant, asString(data["subject"]), at)
		return err
	}
	return nil
}

// observeBackupAge writes farm.backup.age once an hour for each subject with a good backup.
func (c *Core) observeBackupAge(ctx context.Context, _ *Catalog) error {
	return c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		hour := now.Truncate(time.Hour)
		rows, err := tx.Query(ctx, `SELECT tenant, subject, last_good FROM obs_backups ORDER BY tenant, subject`)
		if err != nil {
			return err
		}
		type row struct {
			tenant, subject string
			last            time.Time
		}
		rs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
			var x row
			return x, r.Scan(&x.tenant, &x.subject, &x.last)
		})
		if err != nil {
			return err
		}
		for _, r := range rs {
			if err := writePoint(ctx, tx, now, obsPoint{Tenant: r.tenant, Series: seriesBackupAge, Subject: "dataset:" + r.subject, Source: "core",
				Mark: coreSeriesMark, At: hour, Num: num(math.Round(hour.Sub(r.last).Hours()*100) / 100), Key: "age:" + r.subject + ":" + hour.Format(time.RFC3339)}); err != nil {
				return err
			}
		}
		return nil
	})
}

// rollupDef is an entry of spec/model/rollups.yaml.
type rollupDef struct{ Period, Function string }

func (c *Core) rollups() map[string]rollupDef {
	out := map[string]rollupDef{}
	reg, err := c.spec.Registry("spec/model/rollups.yaml")
	if err != nil {
		return out
	}
	for _, e := range reg.Entries {
		if e["since"] == "v0.1" {
			out[asString(e["id"])] = rollupDef{Period: asString(e["period"]), Function: asString(e["function"])}
		}
	}
	return out
}

// periodStart is the start of the hour or day of t in UTC.
func periodStart(period string, t time.Time) time.Time {
	t = t.UTC()
	if period == "day" {
		return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	}
	return t.Truncate(time.Hour)
}

// rollupValue applies a function to the values of a period; share needs the good value.
func rollupValue(function, good string, nums []float64, enums []string) (float64, bool) {
	switch function {
	case "share":
		if good == "" || len(enums) == 0 {
			return 0, false
		}
		n := 0
		for _, e := range enums {
			if e == good {
				n++
			}
		}
		return math.Round(float64(n)/float64(len(enums))*1e4) / 1e4, true
	case "max", "mean", "p95":
		if len(nums) == 0 {
			return 0, false
		}
	default:
		return 0, false
	}
	switch function {
	case "max":
		return slices.Max(nums), true
	case "mean":
		s := 0.0
		for _, v := range nums {
			s += v
		}
		return s / float64(len(nums)), true
	}
	sorted := slices.Clone(nums)
	sort.Float64s(sorted)
	// The nearest rank of the 95th percentile.
	i := int(math.Ceil(0.95*float64(len(sorted)))) - 1
	return sorted[max(i, 0)], true
}

// observeRollups recomputes the hour and day rollups of the periods since the last rollup
// written (at least the last two days) for the rollups a tenant series declares, and day.max of
// the backup age (OBV-4). A rollup is kept longer than the points, so a window past the points
// still has numbers.
func (c *Core) observeRollups(ctx context.Context, cat *Catalog) error {
	defs := c.rollups()
	return c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		for _, tenant := range slices.Sorted(mapKeys(cat.Tenants)) {
			series := c.tenantSeries(cat, tenant)
			for _, name := range slices.Sorted(mapKeys(series)) {
				s := series[name]
				wanted := s.Rollups
				for _, id := range wanted {
					d, ok := defs[id]
					if !ok || d.Period == "window" {
						continue
					}
					good := ""
					if s.Outcome != nil {
						good = s.Outcome.Good
					}
					from, err := rollupFrom(ctx, tx, tenant, name, id, s.RawDays, now)
					if err != nil {
						return err
					}
					groups, err := readPoints(ctx, tx, tenant, name, "", periodStart(d.Period, from), now)
					if err != nil {
						return err
					}
					for subject, pts := range groups {
						for start, vals := range bucket(pts, d.Period) {
							if !start.Add(periodLen(d.Period)).Before(now) && !start.Add(periodLen(d.Period)).Equal(now) {
								continue // the period is still open
							}
							v, ok := rollupValue(d.Function, good, vals.nums, vals.enums)
							if !ok {
								continue
							}
							if _, err := tx.Exec(ctx, `INSERT INTO obs_rollups (tenant, series, subject, rollup, period_start, value, samples, data_mark, updated_at)
								VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) ON CONFLICT (tenant, series, subject, rollup, period_start)
								DO UPDATE SET value = $6, samples = $7, updated_at = $9`,
								tenant, name, subject, id, start, v, vals.n(), s.DataClass, now); err != nil {
								return err
							}
						}
					}
				}
			}
		}
		return nil
	})
}

// rollupFrom is where a rollup reads points from: the start of its last period written, so
// periods missed during an outage or before the first run are rolled up too, and never later
// than two days back for late points; bounded by the raw retention, past which no point is
// left (OBV-4).
func rollupFrom(ctx context.Context, tx pgx.Tx, tenant, series, rollup string, rawDays int, now time.Time) (time.Time, error) {
	from := now.Add(-48 * time.Hour)
	var last *time.Time
	if err := tx.QueryRow(ctx, `SELECT max(period_start) FROM obs_rollups WHERE tenant = $1 AND series = $2 AND rollup = $3`,
		tenant, series, rollup).Scan(&last); err != nil {
		return from, err
	}
	floor := now.AddDate(0, 0, -max(rawDays, 2))
	switch {
	case last == nil:
		from = floor
	case last.Before(from):
		from = *last
	}
	if from.Before(floor) {
		from = floor
	}
	return from, nil
}

func periodLen(period string) time.Duration {
	if period == "day" {
		return 24 * time.Hour
	}
	return time.Hour
}

// obsRead is a point read back.
type obsRead struct {
	At     time.Time
	Num    *float64
	Enum   string
	Worker string
	Role   string
}

type obsValues struct {
	nums  []float64
	enums []string
}

func (v obsValues) n() int { return len(v.nums) + len(v.enums) }

// readPoints reads the points of a series by subject in [from, to); an empty subject reads all.
func readPoints(ctx context.Context, q store.Querier, tenant, series, subject string, from, to time.Time) (map[string][]obsRead, error) {
	rows, err := q.Query(ctx, `SELECT subject, at, num, coalesce(enum, ''), coalesce(worker, ''), coalesce(role, '') FROM obs_points
		WHERE tenant = $1 AND series = $2 AND ($3 = '' OR subject = $3) AND at >= $4 AND at < $5 ORDER BY subject, at`, tenant, series, subject, from, to)
	if err != nil {
		return nil, err
	}
	out := map[string][]obsRead{}
	for rows.Next() {
		var s string
		var p obsRead
		if err := rows.Scan(&s, &p.At, &p.Num, &p.Enum, &p.Worker, &p.Role); err != nil {
			rows.Close()
			return nil, err
		}
		out[s] = append(out[s], p)
	}
	return out, rows.Err()
}

// bucket groups points by the start of their period.
func bucket(pts []obsRead, period string) map[time.Time]obsValues {
	out := map[time.Time]obsValues{}
	for _, p := range pts {
		k := periodStart(period, p.At)
		v := out[k]
		if p.Num != nil {
			v.nums = append(v.nums, *p.Num)
		} else {
			v.enums = append(v.enums, p.Enum)
		}
		out[k] = v
	}
	return out
}

// observeRetention deletes points and rollups past the periods of class observation (the
// series' own raw and rollup periods, 14 and 93 days by default, OBV-4) only in whole calendar
// months: a month goes once its last day is past the period, never a single point or rollup
// (RET-2, RET-10). obsCut gives the first instant that stays.
func (c *Core) observeRetention(ctx context.Context, cat *Catalog) error {
	return c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		rawMax, rollMax := obsRollupDays, 366
		for _, tenant := range slices.Sorted(mapKeys(cat.Tenants)) {
			for name, s := range c.tenantSeries(cat, tenant) {
				rawMax, rollMax = max(rawMax, s.RawDays), max(rollMax, s.RollDays)
				if _, err := tx.Exec(ctx, `DELETE FROM obs_points WHERE tenant = $1 AND series = $2 AND at < $3`, tenant, name, obsCut(now, s.RawDays)); err != nil {
					return err
				}
				if _, err := tx.Exec(ctx, `DELETE FROM obs_rollups WHERE tenant = $1 AND series = $2 AND period_start < $3`, tenant, name, obsCut(now, s.RollDays)); err != nil {
					return err
				}
			}
		}
		// Points of a series that is gone, or of a tenant that is gone, go after the longest periods.
		if _, err := tx.Exec(ctx, `DELETE FROM obs_points WHERE at < $1`, obsCut(now, rawMax)); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `DELETE FROM obs_rollups WHERE period_start < $1`, obsCut(now, rollMax))
		return err
	})
}

// obsCut is the start of the oldest calendar month not yet wholly past days: everything before
// it is older than the period, whole months only (RET-10).
func obsCut(now time.Time, days int) time.Time {
	return monthStart(now.AddDate(0, 0, -days))
}

// obsQuery is a closed observation query (OBV-3).
type obsQuery struct {
	Series, Subject, Window, Rollup string
}

var obsWindows = map[string]int{"1d": 1, "7d": 7, "14d": 14, "93d": 93}

// queryObservations answers a closed query within the series a reader may read and the mark
// ceiling: only numbers and enum values, from points, and from stored rollups for periods
// whose points are gone (OBV-3, OBV-4). It never sets untrusted_seen.
func (c *Core) queryObservations(ctx context.Context, q store.Querier, now time.Time, cat *Catalog, tenant string, allowed []string, ceiling string, req obsQuery) (map[string]any, error) {
	if !slices.Contains(allowed, req.Series) {
		return nil, refuse("type_not_allowed", "the series is not among those this reader may read")
	}
	s, ok := c.tenantSeries(cat, tenant)[req.Series]
	if !ok {
		return nil, refuse("not_found", "no series %s in the tenant", req.Series)
	}
	if ceiling != "" {
		so, ok1 := c.markOrder(s.DataClass)
		co, ok2 := c.markOrder(ceiling)
		if !ok1 || !ok2 || so > co {
			return nil, refuse("forbidden", "the series class is above the reader's mark")
		}
	}
	days, ok := obsWindows[req.Window]
	if !ok {
		return nil, refuse("input_schema_invalid", "unknown window")
	}
	d, ok := c.rollups()[req.Rollup]
	if !ok {
		return nil, refuse("input_schema_invalid", "unknown rollup")
	}
	if !c.subjectOfTenant(cat, tenant, s.CatalogKind, req.Subject) {
		return nil, refuse("not_found", "the subject is not in the tenant")
	}
	good := ""
	if s.Outcome != nil {
		good = s.Outcome.Good
	}
	if d.Function == "share" && good == "" {
		return nil, refuse("input_schema_invalid", "the series has no good value for a share")
	}
	if d.Function != "share" && len(s.Enum) > 0 {
		return nil, refuse("input_schema_invalid", "a series of a closed list has only a share")
	}
	// A window longer than the raw points kept is answered only from stored rollups; otherwise
	// the answer would cover less than the window it names.
	stored := d.Period != "window" && slices.Contains(s.Rollups, req.Rollup)
	if !stored && days > s.RawDays {
		return nil, refuse("input_schema_invalid", "the series keeps raw points for %d days; this window needs a stored rollup", s.RawDays)
	}
	if stored && days > s.RollDays {
		return nil, refuse("input_schema_invalid", "the series keeps rollups for %d days; the window is longer", s.RollDays)
	}
	from := now.AddDate(0, 0, -days)
	readFrom := from
	if d.Period != "window" {
		readFrom = periodStart(d.Period, from)
	}
	groups, err := readPoints(ctx, q, tenant, req.Series, req.Subject, readFrom, now.Add(time.Second))
	if err != nil {
		return nil, err
	}
	pts := groups[req.Subject]
	points := []any{}
	if d.Period == "window" {
		var vals obsValues
		for _, p := range pts {
			if p.Num != nil {
				vals.nums = append(vals.nums, *p.Num)
			} else {
				vals.enums = append(vals.enums, p.Enum)
			}
		}
		if v, ok := rollupValue(d.Function, good, vals.nums, vals.enums); ok {
			points = append(points, map[string]any{"at": fmtTime(from), "value": v, "samples": vals.n()})
		}
	} else {
		byPeriod := map[time.Time]map[string]any{}
		rows, err := q.Query(ctx, `SELECT period_start, value, samples FROM obs_rollups WHERE tenant = $1 AND series = $2 AND subject = $3 AND rollup = $4
			AND period_start >= $5`, tenant, req.Series, req.Subject, req.Rollup, periodStart(d.Period, from))
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var at time.Time
			var v float64
			var n int
			if err := rows.Scan(&at, &v, &n); err != nil {
				rows.Close()
				return nil, err
			}
			byPeriod[at.UTC()] = map[string]any{"at": fmtTime(at), "value": v, "samples": n}
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
		// The points give the periods with no stored rollup, the open one included; a period
		// that began before the raw points kept would be partial and is left out.
		rawEdge := now.AddDate(0, 0, -s.RawDays)
		for start, vals := range bucket(pts, d.Period) {
			if _, has := byPeriod[start]; has || start.Before(rawEdge) {
				continue
			}
			if v, ok := rollupValue(d.Function, good, vals.nums, vals.enums); ok {
				byPeriod[start] = map[string]any{"at": fmtTime(start), "value": v, "samples": vals.n()}
			}
		}
		for _, k := range slices.SortedFunc(mapKeys(byPeriod), func(a, b time.Time) int { return a.Compare(b) }) {
			points = append(points, byPeriod[k])
		}
		if len(points) > obsMaxPoints {
			points = points[len(points)-obsMaxPoints:]
		}
	}
	return map[string]any{"series": req.Series, "window": req.Window, "rollup": req.Rollup, "data_mark": s.DataClass,
		"groups": []any{map[string]any{"subject": req.Subject, "points": points}}}, nil
}

// subjectOfTenant says whether a subject belongs to the tenant and fits the series kind: a task
// type of the set, a schedule, a catalog host or resource of the tenant (OBV-3, TEN-4).
func (c *Core) subjectOfTenant(cat *Catalog, tenant, kind, subject string) bool {
	k, id, ok := strings.Cut(subject, ":")
	if !ok || k != kind {
		return false
	}
	tv := cat.Tenants[tenant]
	if tv == nil {
		return false
	}
	switch k {
	case "task_type":
		_, ok := cat.TaskTypes[id]
		return ok
	case "schedule":
		_, ok := tv.Schedules[id]
		return ok
	case "role":
		_, ok := cat.Roles[id]
		return ok
	case "host":
		return slices.Contains(catalogHosts(cat, tenant), id)
	}
	return slices.Contains(catalogResources(cat, tenant), id)
}

// catalogResources are the resource ids of a tenant catalog.
func catalogResources(cat *Catalog, tenant string) []string {
	spec, _ := tenantCatalog(cat, tenant)
	res, _ := spec["resources"].([]any)
	var out []string
	for _, r := range res {
		if id := asString(r.(map[string]any)["id"]); id != "" {
			out = append(out, id)
		}
	}
	return out
}

// toolSeries writes the points a done tool call gives by the series mapping of its tenant: only
// a closed field of a structured output that the tool's output schema checked (OBV-1). The
// subject is the catalog host of the call; a value outside the series bounds writes nothing.
func (c *Core) toolSeries(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, callID, tool string, trusted bool, structured map[string]any) error {
	if !trusted || structured == nil {
		return nil
	}
	var tenant, task string
	var host *string
	if err := tx.QueryRow(ctx, `SELECT tenant, task, target_host FROM toolcalls WHERE id = $1`, callID).Scan(&tenant, &task, &host); err != nil {
		return err
	}
	worker, role := "", ""
	_ = tx.QueryRow(ctx, `SELECT l.worker_id, w.role FROM leases l JOIN workers w ON w.id = l.worker_id
		WHERE l.work_id = $1 AND l.kind = 'attempt' ORDER BY l.lease_no DESC LIMIT 1`, task).Scan(&worker, &role)
	for _, s := range c.tenantSeries(cat, tenant) {
		if s.Source != "tool" || s.Tool != tool || s.Field == "" {
			continue
		}
		subject := ""
		switch {
		case s.CatalogKind == "host" && deref(host) != "":
			subject = "host:" + *host
		default:
			if h, ok := structured["host"].(string); ok && s.CatalogKind == "host" {
				subject = "host:" + h
			}
		}
		if subject == "" || !c.subjectOfTenant(cat, tenant, s.CatalogKind, subject) {
			continue
		}
		base := obsPoint{Tenant: tenant, Series: s.Name, Subject: subject, Source: "tool", Mark: s.DataClass, Worker: worker, Role: role}
		var pts []obsPoint
		if v, ok := lookupField(structured, s.Field); ok {
			p := base
			p.At, p.Key = now, "call:"+callID
			if s.setValue(&p, v) {
				pts = append(pts, p)
			}
		} else if structured["item"] == s.Field {
			// A history of the field: a point per time of the output (monitor/item_history).
			list, _ := structured["points"].([]any)
			for _, x := range list {
				m, _ := x.(map[string]any)
				at, err := time.Parse(time.RFC3339, asString(m["t"]))
				if err != nil {
					continue
				}
				p := base
				p.At, p.Key = at, "hist:"+subject+":"+at.UTC().Format(time.RFC3339)
				if s.setValue(&p, m["v"]) {
					pts = append(pts, p)
				}
			}
		}
		for _, p := range pts {
			if err := writePoint(ctx, tx, now, p); err != nil {
				return err
			}
		}
	}
	return nil
}

// setValue puts a value in a point when it fits the series: a number within its bounds, or a
// value of its closed list.
func (s seriesDef) setValue(p *obsPoint, v any) bool {
	if len(s.Enum) > 0 {
		e, ok := v.(string)
		if !ok || !slices.Contains(s.Enum, e) {
			return false
		}
		p.Enum = e
		return true
	}
	n, ok := toFloat(v)
	if !ok || math.IsNaN(n) || math.IsInf(n, 0) || s.Min != nil && n < *s.Min || s.Max != nil && n > *s.Max {
		return false
	}
	p.Num = &n
	return true
}

// lookupField finds a dotted field in an object.
func lookupField(m map[string]any, path string) (any, bool) {
	var cur any = m
	for _, part := range strings.Split(path, ".") {
		o, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		if cur, ok = o[part]; !ok {
			return nil, false
		}
	}
	switch cur.(type) {
	case map[string]any, []any:
		return nil, false
	}
	return cur, true
}

// readableSeries are the series a task may read: for an insight task the role's
// insight.observes, for a review any series of the tenant, for a job its type's observes (OBV-3,
// COM-3).
func (c *Core) readableSeries(cat *Catalog, tenant, role, typ string) []string {
	tt := cat.TaskTypes[typ]
	switch tt.Purpose {
	case "insight":
		if r, ok := cat.Roles[role]; ok && r.Insight != nil {
			return r.Insight.Observes
		}
		return nil
	case "review":
		// A reviewer reads any series of the tenant within the task's mark (COM-3).
		return slices.Sorted(mapKeys(c.tenantSeries(cat, tenant)))
	case "aggregate":
		// An aggregator reads only the series its sources list (OBV-3, AGG-3).
		r, ok := cat.Roles[role]
		if !ok || r.Aggregate == nil {
			return nil
		}
		return r.Aggregate.Sources.Series
	}
	return tt.Observes
}

// EmitRequest is p1-emit-request.
type EmitRequest struct {
	Type string         `json:"type"`
	Data map[string]any `json:"data"`
}

// EmitResult is p1-emit-response.
type EmitResult struct {
	ID string `json:"id"`
}

// Emit publishes an event of a job attempt: only a type from the events of its task type, data
// by the event schema, a subject from its own tenant's catalog; the core sets the source and
// the id (IDM-4, OBV-7). The observe loop writes the points of run.finished.v1.
func (c *Core) Emit(ctx context.Context, w *Worker, header string, req EmitRequest) (*EmitResult, error) {
	if w.Kind != "job" {
		return nil, refuse("type_not_allowed", "only a job worker publishes events")
	}
	cat, err := c.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	var out EmitResult
	err = c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		l, _, err := c.heldLease(ctx, tx, w, header, "emit", nil)
		if err != nil {
			return err
		}
		if l.Kind != "attempt" {
			return refuse("fenced", "events are published under an attempt lease")
		}
		var typ string
		if err := tx.QueryRow(ctx, `SELECT type FROM tasks WHERE id = $1`, l.WorkID).Scan(&typ); err != nil {
			return err
		}
		if !slices.Contains(cat.TaskTypes[typ].Events, req.Type) {
			return refuse("type_not_allowed", "the task type does not publish %s", req.Type)
		}
		// Any declared type of the events catalog that a job produces, checked by its own data
		// schema (IDM-4).
		schema, ok, err := c.jobEventSchema(req.Type)
		if err != nil {
			return err
		}
		if !ok {
			return refuse("type_not_allowed", "%s is not an event a job publishes", req.Type)
		}
		if err := c.spec.Validate(schema, req.Data); err != nil {
			return refuse("input_schema_invalid", "the event data is outside its schema")
		}
		subject := asString(req.Data["subject"])
		known := slices.Contains(catalogResources(cat, l.Tenant), subject) || slices.Contains(catalogHosts(cat, l.Tenant), subject)
		if m, ok := req.Data["subject"].(map[string]any); ok {
			// An alarm's subject names its kind, which must be the kind of that catalog entry
			// (MON-1, MGR-8).
			subject = asString(m["id"])
			known = c.subjectOfTenant(cat, l.Tenant, asString(m["kind"]), asString(m["kind"])+":"+subject)
		}
		if !known {
			return refuse("ref_invalid", "the subject is not in the catalog of the tenant")
		}
		if strings.HasPrefix(req.Type, "alarm.") {
			// A job's alarm takes the one alarm path, with its episodes (INJ-7, MON-1, OBS-6).
			id, err := c.emitAlarm(ctx, tx, now, l.Tenant, req)
			out.ID = id
			return err
		}
		data := map[string]any{}
		for k, v := range req.Data {
			data[k] = v
		}
		// The core adds the producing type and task, for the series of the type (OBV-7).
		data["task_type"], data["task"] = typ, l.WorkID
		id := ""
		if err := tx.QueryRow(ctx, `INSERT INTO events (id, tenant, type, source, subject, time, data, correlationid, causationid)
			VALUES ($1, $2, $3, $4, $5, $6, $7, (SELECT coalesce(record->>'root', id) FROM tasks WHERE id = $8), $8) RETURNING id`,
			ids.New(ids.Event), l.Tenant, req.Type, "botaru:core/"+l.Tenant, subject, now, data, l.WorkID).Scan(&id); err != nil {
			return err
		}
		out.ID = id
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// emitAlarm fires or resolves the alarm a job published and answers with the id of the latest
// event of its alarmkey: a repeated firing writes no event of its own (OBS-6).
func (c *Core) emitAlarm(ctx context.Context, tx pgx.Tx, now time.Time, tenant string, req EmitRequest) (string, error) {
	subj, _ := req.Data["subject"].(map[string]any)
	a := Alarm{Tenant: tenant, Name: strings.TrimSuffix(strings.TrimPrefix(req.Type, "alarm."), ".v1"),
		Subject: AlarmSubject{Kind: asString(subj["kind"]), ID: asString(subj["id"])}, Severity: asString(req.Data["severity"])}
	// The numbers of the job's alarm reach the episode, which incident conditions read (MGR-8).
	if v, ok := req.Data["values"].(map[string]any); ok {
		a.Values = v
	}
	if a.eventType() != req.Type || asString(req.Data["alarmkey"]) != a.key() {
		return "", refuse("input_schema_invalid", "the alarmkey is not the alarm name and its subject")
	}
	var err error
	if req.Data["state"] == "resolved" {
		err = c.resolveAlarm(ctx, tx, now, a)
	} else {
		err = c.fireAlarm(ctx, tx, now, a)
	}
	if err != nil {
		return "", err
	}
	var id string
	err = tx.QueryRow(ctx, `SELECT id FROM events WHERE tenant = $1 AND type = $2 AND subject = $3 ORDER BY time DESC, id DESC LIMIT 1`,
		tenant, req.Type, a.key()).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", refuse("not_found", "the alarm has no open episode to resolve")
	}
	return id, err
}

// jobEventSchema is the short name of the data schema of an event type of the events catalog
// that a job worker produces; ok is false for any other type.
func (c *Core) jobEventSchema(typ string) (string, bool, error) {
	reg, err := c.spec.Registry("spec/bindings/events/catalog.yaml")
	if err != nil {
		return "", false, err
	}
	e, ok := reg.Entry(typ)
	if !ok {
		return "", false, nil
	}
	prod, _ := e["producer"].([]any)
	if !slices.Contains(prod, any("job")) {
		return "", false, nil
	}
	path, _ := e["data_schema"].(string)
	name := strings.TrimSuffix(filepath.Base(path), ".schema.json")
	if _, ok := c.spec.ID(name); !ok || path == "" {
		return "", false, nil
	}
	return name, true, nil
}

// ObsQueryRequest is p1-obs-query-request and p2-obs-series-request.
type ObsQueryRequest struct {
	Series  string `json:"series"`
	Subject string `json:"subject"`
	Window  string `json:"window"`
	Rollup  string `json:"rollup"`
}

// ObsQuery is P1 obs.query: a job attempt reads the series of its task type's observes within
// the task's mark (OBV-3).
func (c *Core) ObsQuery(ctx context.Context, w *Worker, header string, req ObsQueryRequest) (map[string]any, error) {
	if w.Kind != "job" {
		return nil, refuse("type_not_allowed", "only a job worker queries observations through P1")
	}
	cat, err := c.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	err = c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		l, _, err := c.heldLease(ctx, tx, w, header, "obs.query", nil)
		if err != nil {
			return err
		}
		if l.Kind != "attempt" {
			return refuse("fenced", "observations are read under an attempt lease")
		}
		var typ, mark string
		if err := tx.QueryRow(ctx, `SELECT type, coalesce(record->>'data_mark', '') FROM tasks WHERE id = $1`, l.WorkID).Scan(&typ, &mark); err != nil {
			return err
		}
		if mark == "" {
			return refuse("forbidden", "the task has no data mark to bound the read")
		}
		out, err = c.queryObservations(ctx, tx, now, cat, l.Tenant, cat.TaskTypes[typ].Observes, mark,
			obsQuery(req))
		return err
	})
	return out, err
}

// ObsSeries is P2 obs.series: the operator of the credential's tenant reads any series of it
// (OBV-3).
func (c *Core) ObsSeries(ctx context.Context, op Operator, req ObsQueryRequest) (map[string]any, error) {
	tenant := op.Tenant
	cat, err := c.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	err = c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		out, err = c.queryObservations(ctx, tx, now, cat, tenant, slices.Collect(mapKeys(c.tenantSeries(cat, tenant))), "",
			obsQuery(req))
		return err
	})
	return out, err
}

// obsToolName is the built-in tool of a closed observation query (OBV-3).
const obsToolName = "observations_query"

// obsTool offers observations_query in an insight, review or aggregate task of a role that may
// read series: the series enum within what the task may read, the windows and rollups of the
// closed lists (OBV-3, PRF-1). It answers only rollups of the closed list, so an aggregator,
// whose profile has no other tools, reads only aggregated inputs (AGG-2).
func (c *Core) obsTool(cat *Catalog, g *toolsGrant) *catalogTool {
	switch g.Purpose {
	case "insight", "review", "aggregate":
	default:
		return nil
	}
	series := c.readableSeries(cat, g.Tenant, g.Role, g.Type)
	known := c.tenantSeries(cat, g.Tenant)
	enum := []any{}
	for _, s := range series {
		if _, ok := known[s]; ok {
			enum = append(enum, s)
		}
	}
	if len(enum) == 0 {
		return nil
	}
	rollups := []any{}
	for _, id := range slices.Sorted(mapKeys(c.rollups())) {
		rollups = append(rollups, id)
	}
	schema := map[string]any{
		"type": "object", "additionalProperties": false, "required": []any{"series", "subject", "window", "rollup"},
		"properties": map[string]any{
			"series":  map[string]any{"enum": enum},
			"subject": map[string]any{"type": "string", "maxLength": 152, "pattern": "^[a-z][a-z_]{0,31}:[a-z0-9][a-z0-9_.@-]{0,119}$"},
			"window":  map[string]any{"enum": []any{"1d", "7d", "14d", "93d"}},
			"rollup":  map[string]any{"enum": rollups},
		},
	}
	desc := "Read the rollups of an observation series of this tenant: only numbers and values of closed lists. The subject is kind:id, for example task_type:demo.db.backup.v1, schedule:report-nightly or host:db1."
	return &catalogTool{Name: obsToolName, Builtin: true, Tool: toolspec.Tool{Description: desc}, Schema: schema}
}

// obsToolCall answers observations_query; reading sets no untrusted_seen (OBV-3).
func (c *Core) obsToolCall(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, g *toolsGrant, offered bool, args map[string]any) (*mcp.CallResult, error) {
	fail := func(code string) *mcp.CallResult {
		b, _ := json.Marshal(map[string]any{"code": code})
		return &mcp.CallResult{IsError: true, Content: []mcp.Content{{Type: "text", Text: string(b)}}}
	}
	if !offered || g.Mark == "" {
		return fail("forbidden"), nil
	}
	if err := c.spec.Validate("observation-query", args); err != nil || args["group_by"] != nil {
		return fail("input_schema_invalid"), nil
	}
	out, err := c.queryObservations(ctx, tx, now, cat, g.Tenant, c.readableSeries(cat, g.Tenant, g.Role, g.Type), g.Mark,
		obsQuery{Series: asString(args["series"]), Subject: asString(args["subject"]), Window: asString(args["window"]), Rollup: asString(args["rollup"])})
	if p, ok := AsProblem(err); ok {
		return fail(p.Reason), nil
	}
	if err != nil {
		return nil, err
	}
	b, _ := json.Marshal(out)
	return &mcp.CallResult{StructuredContent: out, Content: []mcp.Content{{Type: "text", Text: string(b)}}}, nil
}
