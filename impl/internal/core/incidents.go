// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/ids"
)

// Incidents (spec/lifecycles/incident.md). Only the core opens an incident, from a firing code
// alarm episode and an IncidentClass of the farm git (MGR-7, MGR-8); only code sets resolved,
// from the alarm state; a person or the seat holder closes it after that, and only a person
// declares it false. The incident loop opens, contains, resolves and reopens incidents,
// recomputes the involved parties once a minute and raises alarm.incident.unhandled when
// nobody took up an incident in time. The seat tasks of an incident arrive with the farm
// manager seat.

// incidentLive are the states in which an incident is not resolved and not finished.
var incidentLive = []string{"open", "contained", "mitigating", "reopened"}

// reopenWindow is how long after resolved an alarm of the same key returns the same incident.
const reopenWindow = time.Hour

// implicateEvery is how often code recomputes the involved parties of a live incident.
const implicateEvery = time.Minute

// implicatedMax is the most involved targets an incident keeps.
const implicatedMax = 20

// implicateFns are the involvement functions an IncidentClass may name (MGR-7).
var implicateFns = []string{"lease_holders_on", "recent_targets", "worker_of_subject", "role_failing", "spend_top"}

// detectAtoms are the condition atoms of detect.when: a closed alarm field against a literal.
var detectAtoms = []string{"eq", "ne", "lt", "le", "gt", "ge", "in"}

// incidentClass is an applied IncidentClass.
type incidentClass struct {
	Name string
	Spec map[string]any
}

// incidentClasses are the applied classes by name.
func incidentClasses(cat *Catalog) []incidentClass {
	var out []incidentClass
	for k, v := range cat.RawGlobals {
		if k.Kind == "IncidentClass" {
			spec, _ := v["spec"].(map[string]any)
			out = append(out, incidentClass{Name: k.Name, Spec: spec})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func classByName(cat *Catalog, name string) map[string]any {
	for _, ic := range incidentClasses(cat) {
		if ic.Name == name {
			return ic.Spec
		}
	}
	return nil
}

// specDuration reads a duration field of a class; a missing one is zero.
func specDuration(m map[string]any, k string) time.Duration {
	d, _ := parseDuration(mstr(m, k))
	return d
}

// alarmEpisode is a firing or resolved alarm episode as the incident loop reads it.
type alarmEpisode struct {
	ID       int64
	Tenant   string
	Alarmkey string
	Alarm    string
	State    string
	Subject  AlarmSubject
	Values   map[string]any
	Origin   string
	Opened   time.Time
	Resolved *time.Time
}

// incidentSubject maps an alarm episode to the subject of a class: the farm, the tenant of the
// alarm, or the alarm's own subject when it is of the class's kind; ok is false when the class
// does not fit the alarm.
func incidentSubject(class map[string]any, ep alarmEpisode) (kind, id string, ok bool) {
	kind = mstr(mmap(class, "subject"), "kind")
	switch kind {
	case "farm":
		return kind, "", true
	case "tenant":
		return kind, ep.Tenant, ep.Tenant != "farm"
	case "":
		return "", "", false
	}
	if ep.Subject.Kind != kind {
		return "", "", false
	}
	return kind, ep.Subject.ID, ep.Subject.ID != ""
}

// detectHolds checks the detect.when atoms of a class on an alarm: each atom reads a closed
// field (values.<name>, subject.kind, subject.id) and compares it with the literal; a field
// the alarm does not carry fails the atom.
func detectHolds(class map[string]any, ep alarmEpisode) bool {
	for _, a := range mlist(mmap(class, "detect"), "when") {
		atom, _ := a.(map[string]any)
		var v any
		switch field := mstr(atom, "field"); {
		case strings.HasPrefix(field, "values."):
			v = ep.Values[strings.TrimPrefix(field, "values.")]
		case field == "subject.kind":
			v = ep.Subject.Kind
		case field == "subject.id":
			v = ep.Subject.ID
		}
		if v == nil || !atomHolds(mstr(atom, "atom"), v, atom["value"]) {
			return false
		}
	}
	return true
}

func atomHolds(op string, v, lit any) bool {
	if op == "in" {
		for _, x := range strings.Split(fmt.Sprint(lit), ",") {
			if strings.TrimSpace(x) == fmt.Sprint(v) {
				return true
			}
		}
		return false
	}
	fv, vok := toFloat(v)
	fl, lok := toFloat(lit)
	if vok && lok {
		switch op {
		case "eq":
			return fv == fl
		case "ne":
			return fv != fl
		case "lt":
			return fv < fl
		case "le":
			return fv <= fl
		case "gt":
			return fv > fl
		case "ge":
			return fv >= fl
		}
		return false
	}
	switch op {
	case "eq":
		return fmt.Sprint(v) == fmt.Sprint(lit)
	case "ne":
		return fmt.Sprint(v) != fmt.Sprint(lit)
	}
	return false
}

// incidentRow is a stored incident.
type incidentRow struct {
	ID          string
	Tenant      string
	Class       string
	SubjectKind string
	SubjectID   string
	Alarmkey    string
	Episode     int64
	State       string
	Record      map[string]any
	Since       time.Time
	Handled     bool
	Unhandled   bool
	OpenedAt    time.Time
	ResolvedAt  *time.Time
	EndedAt     *time.Time
}

const incidentCols = `id, tenant, class, subject_kind, subject_id, alarmkey, episode, state, record, since, handled, unhandled, opened_at, resolved_at, ended_at`

func scanIncident(row pgx.Row) (*incidentRow, error) {
	r := &incidentRow{}
	var rec []byte
	if err := row.Scan(&r.ID, &r.Tenant, &r.Class, &r.SubjectKind, &r.SubjectID, &r.Alarmkey, &r.Episode, &r.State, &rec,
		&r.Since, &r.Handled, &r.Unhandled, &r.OpenedAt, &r.ResolvedAt, &r.EndedAt); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(rec, &r.Record); err != nil {
		return nil, err
	}
	return r, nil
}

func loadIncident(ctx context.Context, tx pgx.Tx, tenant, id string, lock bool) (*incidentRow, error) {
	// People type ids in any case.
	q := `SELECT ` + incidentCols + ` FROM incidents WHERE tenant = $1 AND lower(id) = lower($2)`
	if lock {
		q += ` FOR UPDATE`
	}
	r, err := scanIncident(tx.QueryRow(ctx, q, tenant, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return r, err
}

// view is the incident record (spec/model/incident.schema.json).
func (r *incidentRow) view() map[string]any {
	out := map[string]any{}
	for k, v := range r.Record {
		out[k] = v
	}
	subject := map[string]any{"kind": r.SubjectKind}
	if r.SubjectID != "" {
		subject["id"] = r.SubjectID
	}
	out["id"], out["tenant"], out["class"], out["state"], out["subject"] = r.ID, r.Tenant, r.Class, r.State, subject
	out["opened_at"] = fmtTime(r.OpenedAt)
	if r.ResolvedAt != nil {
		out["resolved_at"] = fmtTime(*r.ResolvedAt)
	}
	return out
}

// save writes the state and the record of an incident.
func (r *incidentRow) save(ctx context.Context, tx pgx.Tx, now time.Time) error {
	rec, err := json.Marshal(r.Record)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE incidents SET state = $2, record = $3, episode = $4, since = $5, handled = $6, unhandled = $7,
		resolved_at = $8, ended_at = $9, updated_at = $10 WHERE id = $1`,
		r.ID, r.State, rec, r.Episode, r.Since, r.Handled, r.Unhandled, r.ResolvedAt, r.EndedAt, now)
	return err
}

// incidentEvent writes the event of an incident transition; it carries only codes and ids, never
// the dismissal text (MGR-8).
func (c *Core) incidentEvent(ctx context.Context, tx pgx.Tx, now time.Time, r *incidentRow, what string) error {
	subject := map[string]any{"kind": r.SubjectKind}
	if r.SubjectID != "" {
		subject["id"] = r.SubjectID
	}
	return c.appendEvent(ctx, tx, now, r.Tenant, Event{Type: "incident." + what + ".v1", Subject: r.ID, Data: map[string]any{
		"incident": r.ID, "class": r.Class, "state": r.State, "severity": r.Record["severity"], "subject": subject}})
}

// incidentLoop is the incident loop (spec/ports/background-loops.md).
func (c *Core) incidentLoop(ctx context.Context) error {
	cat, err := c.Catalog(ctx)
	if err != nil {
		return err
	}
	// Every step runs even when another failed: a failed opening still lets incidents resolve and
	// page.
	classes := incidentClasses(cat)
	return errors.Join(c.openIncidents(ctx, cat, classes), c.resolveIncidents(ctx, cat),
		c.refreshImplicated(ctx, cat), c.unhandledIncidents(ctx, cat))
}

// openIncidents opens or reopens an incident for every firing episode that a class detects and
// that held the class conditions for detect.for (MGR-8).
func (c *Core) openIncidents(ctx context.Context, cat *Catalog, classes []incidentClass) error {
	if len(classes) == 0 {
		return nil
	}
	alarms := map[string]bool{}
	for _, ic := range classes {
		alarms[mstr(mmap(ic.Spec, "detect"), "alarm")] = true
	}
	rows, err := c.st.Pool.Query(ctx, `SELECT id, tenant, alarmkey, alarm, subject, vals, origin, opened_at FROM alarm_episodes
		WHERE state = 'firing' AND alarm = ANY($1) ORDER BY id`, slices.Collect(mapKeys(alarms)))
	if err != nil {
		return err
	}
	var eps []alarmEpisode
	for rows.Next() {
		var ep alarmEpisode
		var subj, vals []byte
		if err := rows.Scan(&ep.ID, &ep.Tenant, &ep.Alarmkey, &ep.Alarm, &subj, &vals, &ep.Origin, &ep.Opened); err != nil {
			rows.Close()
			return err
		}
		_ = json.Unmarshal(subj, &ep.Subject)
		_ = json.Unmarshal(vals, &ep.Values)
		eps = append(eps, ep)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	var errs []error
	for _, ep := range eps {
		for _, ic := range classes {
			if mstr(mmap(ic.Spec, "detect"), "alarm") != ep.Alarm || !detectHolds(ic.Spec, ep) {
				continue
			}
			kind, id, ok := incidentSubject(ic.Spec, ep)
			if !ok {
				continue
			}
			if err := c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
				if ep.Opened.After(now.Add(-specDuration(mmap(ic.Spec, "detect"), "for"))) {
					return nil
				}
				return c.openIncident(ctx, tx, now, cat, ic, ep, kind, id)
			}); err != nil {
				// One episode that fails does not stop the others; the loop still reports it.
				errs = append(errs, c.incidentFailed(ctx, err, "open", ic.Name, ep.Alarmkey))
			}
		}
	}
	return errors.Join(errs...)
}

// incidentFailed logs a failure of one incident or episode, so the loop goes on with the rest.
func (c *Core) incidentFailed(ctx context.Context, err error, what, class, key string) error {
	if ctx.Err() != nil {
		return err
	}
	c.log.Warn("incident loop: one item failed", "step", what, "class", class, "key", key, "err", err)
	return fmt.Errorf("incident %s %s %s: %w", what, class, key, err)
}

// openIncident opens, reopens or keeps the incident of one class and subject for an episode, in
// one transaction serialized by the class and subject.
func (c *Core) openIncident(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, ic incidentClass, ep alarmEpisode, kind, id string) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 7300103))`,
		ep.Tenant+"/"+ic.Name+"/"+kind+"/"+id); err != nil {
		return err
	}
	var seen bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM incidents WHERE class = $1 AND episodes @> ARRAY[$2::bigint])`, ic.Name, ep.ID).Scan(&seen); err != nil {
		return err
	}
	if seen {
		return nil
	}
	// A live incident of the class and subject keeps its number: an alarm that came back
	// before the incident resolved, or another alarm key of the same subject, joins it.
	live, err := scanIncident(tx.QueryRow(ctx, `SELECT `+incidentCols+` FROM incidents
		WHERE tenant = $1 AND class = $2 AND subject_kind = $3 AND subject_id = $4 AND state = ANY($5) FOR UPDATE`,
		ep.Tenant, ic.Name, kind, id, incidentLive))
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if live != nil {
		live.Episode = ep.ID
		live.Record["alarm"] = incidentAlarm(ep)
		if err := live.save(ctx, tx, now); err != nil {
			return err
		}
		return adoptEpisode(ctx, tx, live.ID, ep)
	}
	// The class and subject back within an hour after resolved reopen the incident (MGR-8).
	r, err := scanIncident(tx.QueryRow(ctx, `SELECT `+incidentCols+` FROM incidents
		WHERE tenant = $1 AND class = $2 AND subject_kind = $3 AND subject_id = $4 AND state = 'resolved' AND resolved_at >= $5
		ORDER BY resolved_at DESC LIMIT 1 FOR UPDATE`, ep.Tenant, ic.Name, kind, id, now.Add(-reopenWindow)))
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if r != nil {
		r.State, r.Episode, r.Since, r.Handled, r.ResolvedAt = "reopened", ep.ID, now, false, nil
		r.Record["alarm"] = incidentAlarm(ep)
		// The new round is not taken up yet: the acks of the previous one do not carry over.
		delete(r.Record, "resolved_at")
		delete(r.Record, "taken_by")
		delete(r.Record, "humans_took")
		if err := r.save(ctx, tx, now); err != nil {
			return err
		}
		if err := adoptEpisode(ctx, tx, r.ID, ep); err != nil {
			return err
		}
		if err := c.incidentEvent(ctx, tx, now, r, "reopened"); err != nil {
			return err
		}
	} else {
		r = &incidentRow{ID: ids.New(ids.Incident), Tenant: ep.Tenant, Class: ic.Name, SubjectKind: kind, SubjectID: id,
			Alarmkey: ep.Alarmkey, Episode: ep.ID, State: "open", Since: now, OpenedAt: now,
			Record: map[string]any{"severity": mstr(ic.Spec, "severity"), "alarm": incidentAlarm(ep), "implicated": []any{}, "ops": []any{}}}
		rec, err := json.Marshal(r.Record)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO incidents (id, tenant, class, subject_kind, subject_id, alarmkey, episode, episodes, alarmkeys, state, record,
			since, implicated_at, opened_at, updated_at) VALUES ($1, $2, $3, $4, $5, $6, $7, ARRAY[$7::bigint], ARRAY[$6::text], 'open', $8, $9, $9, $9, $9)`,
			r.ID, r.Tenant, r.Class, kind, id, ep.Alarmkey, ep.ID, rec, now); err != nil {
			return err
		}
		if err := c.incidentEvent(ctx, tx, now, r, "opened"); err != nil {
			return err
		}
	}
	if err := c.implicate(ctx, tx, now, cat, ic.Spec, r); err != nil {
		return err
	}
	return c.containAuto(ctx, tx, now, cat, ic, r)
}

// adoptEpisode adds an episode and its alarm key to an incident.
func adoptEpisode(ctx context.Context, tx pgx.Tx, id string, ep alarmEpisode) error {
	_, err := tx.Exec(ctx, `UPDATE incidents SET episodes = array_append(episodes, $2::bigint),
		alarmkeys = CASE WHEN $3::text = ANY(alarmkeys) THEN alarmkeys ELSE array_append(alarmkeys, $3::text) END WHERE id = $1`, id, ep.ID, ep.Alarmkey)
	return err
}

// incidentAlarm is the alarm block of the record: only the key, the numbers and the origin, never
// a text of a monitoring problem (MON-5).
func incidentAlarm(ep alarmEpisode) map[string]any {
	vals := map[string]any{}
	keys := slices.Sorted(mapKeys(ep.Values))
	for _, k := range keys {
		if f, ok := toFloat(ep.Values[k]); ok && len(vals) < 10 {
			vals[k] = f
		}
	}
	origin := ep.Origin
	if origin != "monitor" {
		origin = "code"
	}
	out := map[string]any{"alarmkey": ep.Alarmkey, "origin": origin}
	if len(vals) > 0 {
		out["values"] = vals
	}
	return out
}

// containAuto applies the first measure of the class: holds with an expiry by the author
// contain_auto, which only a tenant admin or the operator releases earlier; the core does not
// release them when the incident ends (MGR-7, HLD-2).
func (c *Core) containAuto(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, ic incidentClass, r *incidentRow) error {
	measures := mlist(ic.Spec, "contain_auto")
	if len(measures) == 0 {
		return nil
	}
	applied := false
	for _, m := range measures {
		mm, _ := m.(map[string]any)
		if mstr(mm, "op") != "hold" {
			continue
		}
		scope, ok := containScope(mmap(mm, "scope"), r)
		if !ok {
			continue
		}
		ttl := specDuration(mm, "ttl")
		if ttl <= 0 {
			continue
		}
		// A scope that a first measure of the class still holds gets no second hold: the
		// incident counts as contained by it.
		k := scope.key()
		var held bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM holds WHERE state = 'active' AND author_kind = 'contain_auto' AND source = $1
			AND scope_kind = $2 AND scope_tenant = $3 AND scope_name = $4 AND (until IS NULL OR until > $5))`,
			ic.Name, k.kind, k.tenant, k.name, now).Scan(&held); err != nil {
			return err
		}
		if held {
			applied = true
			continue
		}
		until := now.Add(ttl)
		actor := HoldActor{Principal: "system", Rank: "contain_auto", Tenant: r.Tenant, Via: "system", Source: ic.Name}
		if _, _, err := c.insertHold(ctx, tx, now, actor, scope, mstrs(mm, "effects"), "contain_auto", &until); err != nil {
			return err
		}
		applied = true
	}
	if !applied || r.State == "contained" {
		return nil
	}
	r.State = "contained"
	if err := r.save(ctx, tx, now); err != nil {
		return err
	}
	return c.incidentEvent(ctx, tx, now, r, "contained")
}

// containScope reads the one-key scope of a first measure: the scope kind and the name, where
// $subject names the incident subject. A farm incident has no first measure by a hold.
func containScope(m map[string]any, r *incidentRow) (HoldScope, bool) {
	if len(m) != 1 || r.Tenant == "farm" {
		return HoldScope{}, false
	}
	for kind, v := range m {
		name, _ := v.(string)
		if name == "$subject" {
			name = r.SubjectID
		}
		if name == "" || farmScope(kind) || kind == "tenant" {
			return HoldScope{}, false
		}
		return HoldScope{Kind: kind, Tenant: r.Tenant, Name: name}, true
	}
	return HoldScope{}, false
}

// resolveIncidents moves a live incident to resolved when its alarm episode has been resolved
// for the class's resolve.for (MGR-8).
func (c *Core) resolveIncidents(ctx context.Context, cat *Catalog) error {
	// An incident resolves when none of its episodes fires; the wait counts from the last one.
	rows, err := c.st.Pool.Query(ctx, `SELECT i.id, i.tenant, i.class,
		coalesce((SELECT max(e.resolved_at) FROM alarm_episodes e WHERE e.id = ANY(i.episodes)), i.since) FROM incidents i
		WHERE i.state = ANY($1) AND NOT EXISTS (SELECT 1 FROM alarm_episodes e WHERE e.id = ANY(i.episodes) AND e.state <> 'resolved')
		ORDER BY i.id`, incidentLive)
	if err != nil {
		return err
	}
	type due struct {
		id, tenant, class string
		at                time.Time
	}
	var all []due
	for rows.Next() {
		var d due
		if err := rows.Scan(&d.id, &d.tenant, &d.class, &d.at); err != nil {
			rows.Close()
			return err
		}
		all = append(all, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	var errs []error
	for _, d := range all {
		wait := specDuration(mmap(classByName(cat, d.class), "resolve"), "for")
		if err := c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
			if d.at.After(now.Add(-wait)) {
				return nil
			}
			r, err := loadIncident(ctx, tx, d.tenant, d.id, true)
			if err != nil || r == nil || !slices.Contains(incidentLive, r.State) {
				return err
			}
			var firing bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM alarm_episodes WHERE id = ANY(SELECT unnest(episodes) FROM incidents WHERE id = $1)
				AND state <> 'resolved')`, r.ID).Scan(&firing); err != nil {
				return err
			}
			if firing {
				return nil
			}
			r.State, r.ResolvedAt = "resolved", &now
			if err := r.save(ctx, tx, now); err != nil {
				return err
			}
			if err := c.incidentEvent(ctx, tx, now, r, "resolved"); err != nil {
				return err
			}
			return c.settleUnhandled(ctx, tx, now, r)
		}); err != nil {
			errs = append(errs, c.incidentFailed(ctx, err, "resolve", d.class, d.id))
		}
	}
	return errors.Join(errs...)
}

// refreshImplicated recomputes the involved parties of each live incident once a minute.
func (c *Core) refreshImplicated(ctx context.Context, cat *Catalog) error {
	var errs []error
	for {
		done := true
		var id, class string
		err := c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
			r, err := scanIncident(tx.QueryRow(ctx, `SELECT `+incidentCols+` FROM incidents WHERE state = ANY($1) AND implicated_at <= $2
				ORDER BY implicated_at, id LIMIT 1 FOR UPDATE SKIP LOCKED`, incidentLive, now.Add(-implicateEvery)))
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			done, id, class = false, r.ID, r.Class
			return c.implicate(ctx, tx, now, cat, classByName(cat, r.Class), r)
		})
		if err != nil && id == "" {
			return errors.Join(append(errs, err)...)
		}
		if err != nil {
			// The failed incident waits for the next minute, so the others are recomputed.
			errs = append(errs, c.incidentFailed(ctx, err, "implicate", class, id))
			if err := c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
				_, err := tx.Exec(ctx, `UPDATE incidents SET implicated_at = $2 WHERE id = $1`, id, now)
				return err
			}); err != nil {
				return errors.Join(append(errs, err)...)
			}
		}
		if done {
			return errors.Join(errs...)
		}
	}
}

// implicated is one involved target: a worker or a task, with the identity, the role and the
// type code adds to it (MGR-9).
type implicated struct {
	Worker, Task, Bot, Role, TaskType string
}

func (i implicated) view() map[string]any {
	out := map[string]any{}
	for k, v := range map[string]string{"worker": strings.ToLower(i.Worker), "task": strings.ToLower(i.Task), "bot": i.Bot, "role": i.Role, "tasktype": i.TaskType} {
		if v != "" {
			out[k] = v
		}
	}
	return out
}

// implicate builds the involved parties with the class functions, at most 20, and writes them
// into the record.
func (c *Core) implicate(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, class map[string]any, r *incidentRow) error {
	var found []implicated
	seen := map[string]bool{}
	for _, f := range mlist(class, "implicate") {
		fm, _ := f.(map[string]any)
		got, err := implicateFn(ctx, tx, now, mstr(fm, "fn"), mmap(fm, "args"), r)
		if err != nil {
			return err
		}
		for _, g := range got {
			k := g.Worker + "/" + g.Task
			if !seen[k] && len(found) < implicatedMax {
				seen[k] = true
				found = append(found, g)
			}
		}
	}
	items := []any{}
	for _, g := range found {
		if g.Worker != "" {
			if err := tx.QueryRow(ctx, `SELECT w.role, coalesce((SELECT b.id FROM bots b WHERE b.worker_id = w.id AND b.state IN ('active', 'suspended')), '')
				FROM workers w WHERE w.id = $1`, g.Worker).Scan(&g.Role, &g.Bot); err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
		}
		if g.Task != "" {
			if err := tx.QueryRow(ctx, `SELECT type FROM tasks WHERE id = $1`, g.Task).Scan(&g.TaskType); err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
		}
		items = append(items, g.view())
	}
	r.Record["implicated"] = items
	if err := r.save(ctx, tx, now); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE incidents SET implicated_at = $2 WHERE id = $1`, r.ID, now)
	return err
}

// implicateFn runs one involvement function of the closed list (MGR-7) in the incident's tenant;
// $subject in an argument names the incident subject.
func implicateFn(ctx context.Context, tx pgx.Tx, now time.Time, fn string, args map[string]any, r *incidentRow) ([]implicated, error) {
	arg := func(k string) string {
		v := mstr(args, k)
		if v == "$subject" {
			return r.SubjectID
		}
		return v
	}
	window := func(def time.Duration) time.Duration {
		if d, ok := parseDuration(mstr(args, "window")); ok {
			return d
		}
		return def
	}
	num := func(k string, def int) int {
		if n, ok := mnum(args, k); ok && n >= 1 {
			return int(n)
		}
		return def
	}
	var q string
	var qargs []any
	switch fn {
	case "lease_holders_on":
		// Live attempts that call tools on the resource, and the toolhosts that run those calls.
		q = `SELECT l.worker_id, l.work_id FROM leases l WHERE l.state = 'active' AND l.kind = 'attempt' AND l.tenant = $1
				AND EXISTS (SELECT 1 FROM toolcalls tc WHERE tc.task = l.work_id AND tc.attempt_lease_no = l.lease_no AND tc.target_resource = $2)
			UNION SELECT l.worker_id, tc.task FROM leases l JOIN toolcalls tc ON tc.id = l.work_id
			WHERE l.state = 'active' AND l.kind = 'toolcall' AND l.tenant = $1 AND tc.target_resource = $2`
		qargs = []any{r.Tenant, arg("resource")}
	case "recent_targets":
		q = `SELECT l.worker_id, '' FROM toolcalls tc JOIN leases l ON l.work_id = tc.task AND l.lease_no = tc.attempt_lease_no
			WHERE tc.tenant = $1 AND tc.target_resource = $2 AND tc.created_at >= $3
			GROUP BY l.worker_id HAVING count(*) >= $4`
		qargs = []any{r.Tenant, arg("resource"), now.Add(-window(15 * time.Minute)), num("min_calls", 10)}
	case "worker_of_subject":
		if r.SubjectKind != "worker" {
			return nil, nil
		}
		q = `SELECT id, '' FROM workers WHERE tenant = $1 AND lower(id) = $2`
		qargs = []any{r.Tenant, r.SubjectID}
	case "role_failing":
		role := arg("role")
		if role == "" && r.SubjectKind == "role" {
			role = r.SubjectID
		}
		q = `SELECT l.worker_id, t.id FROM tasks t JOIN leases l ON l.work_id = t.id AND l.lease_no = t.lease_no AND l.kind = 'attempt'
			JOIN workers w ON w.id = l.worker_id
			WHERE t.tenant = $1 AND t.state = 'failed' AND t.updated_at >= $2 AND w.role = $3`
		qargs = []any{r.Tenant, now.Add(-window(15 * time.Minute)), role}
	case "spend_top":
		q = `SELECT worker, '' FROM spend WHERE tenant = $1 AND time >= $2 AND worker IS NOT NULL
			GROUP BY worker ORDER BY sum(eur_micros) DESC, worker LIMIT least($3, 50)`
		qargs = []any{r.Tenant, now.Add(-window(time.Hour)), num("n", 3)}
	default:
		return nil, nil
	}
	if fn != "spend_top" {
		q += ` LIMIT 50`
	}
	rows, err := tx.Query(ctx, q, qargs...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []implicated
	for rows.Next() {
		var g implicated
		if err := rows.Scan(&g.Worker, &g.Task); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// unhandledIncidents raises alarm.incident.unhandled for an incident without an operation and
// without a person's incident.ack within the class's humans.page_if_unhandled_after, and
// resolves it when either appears or the incident is resolved or finished (MGR-8, MON-1).
func (c *Core) unhandledIncidents(ctx context.Context, cat *Catalog) error {
	rows, err := c.st.Pool.Query(ctx, `SELECT id, tenant, class, since FROM incidents
		WHERE NOT unhandled AND NOT handled AND state = ANY($1)
		UNION ALL SELECT id, tenant, class, since FROM incidents WHERE unhandled AND (handled OR NOT state = ANY($1))`, incidentLive)
	if err != nil {
		return err
	}
	type cand struct {
		id, tenant, class string
		since             time.Time
	}
	var all []cand
	for rows.Next() {
		var x cand
		if err := rows.Scan(&x.id, &x.tenant, &x.class, &x.since); err != nil {
			rows.Close()
			return err
		}
		all = append(all, x)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	var errs []error
	for _, x := range all {
		after := specDuration(mmap(classByName(cat, x.class), "humans"), "page_if_unhandled_after")
		if err := c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
			r, err := loadIncident(ctx, tx, x.tenant, x.id, true)
			if err != nil || r == nil {
				return err
			}
			if r.Unhandled {
				return c.settleUnhandled(ctx, tx, now, r)
			}
			if after <= 0 || r.Handled || !slices.Contains(incidentLive, r.State) || r.Since.After(now.Add(-after)) {
				return nil
			}
			r.Unhandled = true
			if err := r.save(ctx, tx, now); err != nil {
				return err
			}
			return c.fireAlarm(ctx, tx, now, unhandledAlarm(r, now))
		}); err != nil {
			errs = append(errs, c.incidentFailed(ctx, err, "unhandled", x.class, x.id))
		}
	}
	return errors.Join(errs...)
}

func unhandledAlarm(r *incidentRow, now time.Time) Alarm {
	return Alarm{Tenant: r.Tenant, Name: "incident.unhandled", Subject: AlarmSubject{Kind: "incident", ID: strings.ToLower(r.ID)}, Severity: "page",
		Values: map[string]any{"unhandled_s": int64(now.Sub(r.Since) / time.Second)}}
}

// settleUnhandled resolves the unhandled alarm of an incident that was handled, resolved or
// finished.
func (c *Core) settleUnhandled(ctx context.Context, tx pgx.Tx, now time.Time, r *incidentRow) error {
	if !r.Unhandled || (!r.Handled && slices.Contains(incidentLive, r.State)) {
		return nil
	}
	r.Unhandled = false
	if err := r.save(ctx, tx, now); err != nil {
		return err
	}
	return c.resolveAlarm(ctx, tx, now, unhandledAlarm(r, now))
}

// IncidentRequest is the body of incident.show, incident.ack, incident.close and
// incident.dismiss.
type IncidentRequest struct {
	Op         string `json:"op"`
	ID         string `json:"id"`
	Incident   string `json:"incident"`
	ReasonCode string `json:"reason_code"`
	Details    string `json:"details"`
}

func (q IncidentRequest) target() string {
	if q.Incident != "" {
		return q.Incident
	}
	return q.ID
}

// IncidentList is P2 incident.list: the incidents of the operator's tenant, newest first.
func (c *Core) IncidentList(ctx context.Context, op Operator, req TenantRequest) (map[string]any, error) {
	cat, err := c.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	limit := req.Limit
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	rows, err := c.st.Pool.Query(ctx, `SELECT id, state, opened_at, class FROM incidents WHERE tenant = $1 AND ($2 = '' OR state = $2)
		ORDER BY opened_at DESC, id DESC LIMIT $3`, op.Tenant, req.State, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []any{}
	for rows.Next() {
		var id, state, class string
		var at time.Time
		if err := rows.Scan(&id, &state, &at, &class); err != nil {
			return nil, err
		}
		title := mstr(classByName(cat, class), "title")
		if title == "" {
			title = class
		}
		items = append(items, map[string]any{"id": id, "state": state, "created_at": fmtTime(at), "title": title})
	}
	return map[string]any{"items": items}, rows.Err()
}

// IncidentShow is P2 incident.show: the incident record for people of its tenant.
func (c *Core) IncidentShow(ctx context.Context, op Operator, req IncidentRequest) (map[string]any, error) {
	var out map[string]any
	err := c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		r, err := loadIncident(ctx, tx, op.Tenant, req.target(), false)
		if err != nil {
			return err
		}
		if r == nil {
			return refuse("not_found", "no such incident in the tenant")
		}
		out = r.view()
		return nil
	})
	return out, err
}

// incidentActor is a person who acts on an incident: an operator of its tenant through P2, or a
// person of the tenant with incident.manage through a channel command.
type incidentActor struct {
	Principal string
	Via       string
}

// IncidentAck is P2 incident.ack: a person took up the incident; the unhandled alarm is not
// raised, and a raised one resolves (MGR-8).
func (c *Core) IncidentAck(ctx context.Context, op Operator, req IncidentRequest) (*Done, error) {
	return c.incidentAct(ctx, op.Tenant, incidentActor{Principal: op.Principal, Via: "farmctl"}, "ack", req)
}

// IncidentClose is P2 incident.close: only after resolved (MGR-8).
func (c *Core) IncidentClose(ctx context.Context, op Operator, req IncidentRequest) (*Done, error) {
	return c.incidentAct(ctx, op.Tenant, incidentActor{Principal: op.Principal, Via: "farmctl"}, "close", req)
}

// IncidentDismiss is P2 incident.dismiss: a person declares the incident false with a reason code
// and the "Details" text, which only people see (MGR-8, GOV-13).
func (c *Core) IncidentDismiss(ctx context.Context, op Operator, req IncidentRequest) (*Done, error) {
	return c.incidentAct(ctx, op.Tenant, incidentActor{Principal: op.Principal, Via: "farmctl"}, "dismiss", req)
}

func (c *Core) incidentAct(ctx context.Context, tenant string, a incidentActor, what string, req IncidentRequest) (*Done, error) {
	var out Done
	err := c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		d, err := c.incidentActTx(ctx, tx, now, tenant, a, what, req)
		if d != nil {
			out = *d
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// dismissReasons are the reason codes of a dismissal (spec/model/incident-dismiss-reasons.yaml).
var dismissReasons = []string{"false_alarm", "threshold_misconfigured", "maintenance", "duplicate", "test", "other"}

func (c *Core) incidentActTx(ctx context.Context, tx pgx.Tx, now time.Time, tenant string, a incidentActor, what string, req IncidentRequest) (*Done, error) {
	if what == "dismiss" {
		if !strings.HasPrefix(a.Principal, "human:") {
			return nil, refuse("forbidden", "only a person declares an incident false")
		}
		if !slices.Contains(dismissReasons, req.ReasonCode) || strings.TrimSpace(req.Details) == "" || len([]rune(req.Details)) > 500 {
			return nil, refuse("dismiss_reason_required", "a dismissal needs a reason code and the Details text of up to 500 characters")
		}
	}
	r, err := loadIncident(ctx, tx, tenant, req.target(), true)
	if err != nil {
		return nil, err
	}
	if r == nil {
		return nil, refuse("not_found", "no such incident in the tenant")
	}
	done := &Done{OK: true, ID: r.ID, State: r.State}
	terminal := r.State == "closed" || r.State == "dismissed"
	audit := map[string]any{}
	switch what {
	case "ack":
		if terminal {
			return done, nil
		}
		taken, _ := r.Record["taken_by"].([]any)
		if slices.ContainsFunc(taken, func(v any) bool { return v == a.Principal }) {
			return done, nil
		}
		if len(taken) < 20 {
			taken = append(taken, a.Principal)
		}
		r.Record["taken_by"], r.Record["humans_took"], r.Handled = taken, true, true
		if err := r.save(ctx, tx, now); err != nil {
			return nil, err
		}
		if err := c.incidentEvent(ctx, tx, now, r, "acked"); err != nil {
			return nil, err
		}
		if err := c.settleUnhandled(ctx, tx, now, r); err != nil {
			return nil, err
		}
	case "close":
		if r.State == "closed" {
			return done, nil
		}
		if r.State != "resolved" {
			return nil, refuse("conflict", "an incident is closed only after resolved")
		}
		r.State, r.EndedAt = "closed", &now
		r.Record["closed_at"], r.Record["closed_by"] = fmtTime(now), a.Principal
		if err := r.save(ctx, tx, now); err != nil {
			return nil, err
		}
		if err := c.incidentEvent(ctx, tx, now, r, "closed"); err != nil {
			return nil, err
		}
		if err := c.settleUnhandled(ctx, tx, now, r); err != nil {
			return nil, err
		}
	case "dismiss":
		if r.State == "dismissed" {
			return done, nil
		}
		if terminal {
			return nil, refuse("conflict", "the incident is closed")
		}
		r.State, r.EndedAt = "dismissed", &now
		r.Record["dismiss"] = map[string]any{"reason_code": req.ReasonCode, "details": req.Details, "by": a.Principal, "at": fmtTime(now)}
		if err := r.save(ctx, tx, now); err != nil {
			return nil, err
		}
		if err := c.incidentEvent(ctx, tx, now, r, "dismissed"); err != nil {
			return nil, err
		}
		if err := c.settleUnhandled(ctx, tx, now, r); err != nil {
			return nil, err
		}
		// The audit keeps the reason code and the Details text for people; no model reads the
		// audit (MGR-8).
		audit["reason_code"], audit["details"] = req.ReasonCode, req.Details
	}
	audit["state"] = r.State
	seq, err := c.appendAudit(ctx, tx, now, Audit{
		Tenant: tenant, Kind: "command", Actor: Actor{Principal: a.Principal, Via: a.Via},
		Command: "incident." + what, Target: &Target{Kind: "incident", ID: r.ID}, OK: true, After: audit,
	})
	if err != nil {
		return nil, err
	}
	done.State, done.AuditSeq = r.State, seq
	return done, nil
}

// afterFields is the text after the first n words.
func afterFields(s string, n int) string {
	s = strings.TrimSpace(s)
	for range n {
		i := strings.IndexFunc(s, unicode.IsSpace)
		if i < 0 {
			return ""
		}
		s = strings.TrimSpace(s[i:])
	}
	return s
}

// incidentCommand is /incident ack|close|dismiss <id> [<reason_code> <details>] from a person of
// the tenant with incident.manage (MGR-8).
func (c *Core) incidentCommand(ctx context.Context, tx pgx.Tx, now time.Time, pc personCommand, rest string) (*ActivityResult, error) {
	f := strings.Fields(rest)
	if len(f) < 2 || !slices.Contains([]string{"ack", "close", "dismiss"}, f[0]) {
		return pc.reply(c, "cmd.incident.usage", nil), nil
	}
	if !hasCap(pc.tv, pc.groups, "incident.manage") {
		return pc.reply(c, "task.rejected.no_group_cap", nil), nil
	}
	req := IncidentRequest{ID: f[1]}
	if f[0] == "dismiss" {
		if len(f) < 4 {
			return pc.reply(c, "cmd.incident.dismiss_reason", nil), nil
		}
		req.ReasonCode = f[2]
		req.Details = afterFields(rest, 3)
	}
	d, err := c.incidentActTx(ctx, tx, now, pc.tv.Name, incidentActor{Principal: pc.person, Via: "channel"}, f[0], req)
	var p *Problem
	if errors.As(err, &p) {
		switch p.Reason {
		case "not_found":
			return pc.reply(c, "cmd.incident.not_found", nil), nil
		case "dismiss_reason_required":
			return pc.reply(c, "cmd.incident.dismiss_reason", nil), nil
		case "conflict":
			return pc.reply(c, "cmd.incident.conflict", nil), nil
		}
	}
	if err != nil {
		return nil, err
	}
	return pc.reply(c, "cmd.incident.done", map[string]any{"incident": d.ID, "state": d.State}), nil
}

// incidentSubjectScopes are the hold scopes a first measure may name in a tenant.
var incidentSubjectScopes = []string{"worker", "bot", "bot_group", "role", "tasktype", "resource"}

// lintIncidentClasses checks the references of an IncidentClass (MGR-7): detect names a code
// alarm of the events catalog and closed condition atoms; implicate only the involvement
// functions; responses only operations of control-ops.yaml, applied actions, pinnable types
// and the service verbs; a class of the farm has no services and no monitoring scripts; and
// the first measure is only a hold with an expiry.
func (c *Core) lintIncidentClasses(S *lintSet, f *lintFindings) error {
	events, err := c.spec.Registry("spec/bindings/events/catalog.yaml")
	if err != nil {
		return err
	}
	ops, err := c.spec.Registry("spec/model/control-ops.yaml")
	if err != nil {
		return err
	}
	holds, err := c.spec.Registry("spec/model/holds.yaml")
	if err != nil {
		return err
	}
	for _, o := range S.each("IncidentClass") {
		if o.i < 0 {
			continue
		}
		d := mmap(o.spec, "detect")
		if a := mstr(d, "alarm"); !strings.HasPrefix(a, "alarm.") || !events.Has(a) || a == "alarm.incident.unhandled.v1" {
			f.add(o, "incident_class_ref", "/detect/alarm")
		}
		for j, w := range mlist(d, "when") {
			wm, _ := w.(map[string]any)
			field := mstr(wm, "field")
			if !slices.Contains(detectAtoms, mstr(wm, "atom")) ||
				!(strings.HasPrefix(field, "values.") || field == "subject.kind" || field == "subject.id") {
				f.add(o, "incident_class_ref", fmt.Sprintf("/detect/when/%d", j))
			}
		}
		for j, fn := range mlist(o.spec, "implicate") {
			fm, _ := fn.(map[string]any)
			if !slices.Contains(implicateFns, mstr(fm, "fn")) {
				f.add(o, "incident_class_ref", fmt.Sprintf("/implicate/%d/fn", j))
			}
		}
		r := mmap(o.spec, "responses")
		for j, op := range mstrs(r, "ops") {
			if !ops.Has(op) {
				f.add(o, "incident_class_ref", fmt.Sprintf("/responses/ops/%d", j))
			}
		}
		for j, a := range mstrs(r, "actions") {
			if S.get("Action", a, "") == nil {
				f.add(o, "incident_class_ref", fmt.Sprintf("/responses/actions/%d", j))
			}
		}
		for j, t := range mstrs(r, "pin_types") {
			if tt := S.get("TaskType", t, ""); tt == nil || !truthy(tt["pinnable"]) {
				f.add(o, "incident_class_ref", fmt.Sprintf("/responses/pin_types/%d", j))
			}
		}
		if mstr(mmap(o.spec, "subject"), "kind") == "farm" && (r["services"] != nil || r["monitor_scripts"] != nil) {
			f.add(o, "incident_class_ref", "/responses")
		}
		for j, m := range mlist(o.spec, "contain_auto") {
			mm, _ := m.(map[string]any)
			scope := mmap(mm, "scope")
			ok := mstr(mm, "op") == "hold" && len(scope) == 1 && specDuration(mm, "ttl") > 0 && len(mstrs(mm, "effects")) > 0
			for k, v := range scope {
				ok = ok && slices.Contains(incidentSubjectScopes, k)
				// $subject names the incident subject, so the scope kind is the subject kind.
				ok = ok && (v != "$subject" || k == mstr(mmap(o.spec, "subject"), "kind"))
			}
			for _, e := range mstrs(mm, "effects") {
				he, known := holds.Entry(e)
				ok = ok && known && he["kind"] == "effect"
			}
			if !ok {
				f.add(o, "incident_contain_auto", fmt.Sprintf("/contain_auto/%d", j))
			}
		}
	}
	return nil
}
