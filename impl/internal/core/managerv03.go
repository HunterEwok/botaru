// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/manifest"
)

// The farm manager from v0.3 (GOV-47, GOV-48, MGR-45, MGR-47, MGR-55): a preventable incident
// does not stop the seat. The core never adds the stop reason incident; a new incident of a
// class with manager_accountable, opened while the seat is held, is preventable when code finds
// the sign of its class and the holder answered for the seat for the whole window. Then the
// incident record says so, alarm.manager.preventable pages people, the farm operators get the
// notice manager_preventable without buttons, and the seat goes into restricted mode in that
// tenant: its decision attempts there only read and send notices, until an operator lifts it
// with seat.resume --tenant, or the seat changes holder or is vacated. The epoch does not change.

const (
	preventableNotice = "manager_preventable"
	// restrictedReason is the refusal of a write in restricted mode (MGR-47); it does not count
	// for the safeguard.
	restrictedReason = "manager_restricted"
)

// managerClass says whether a class answers for the manager under the claimed version: from
// v0.3 manager_accountable (MGR-55), before it stops_manager (GOV-28).
func managerClass(spec map[string]any) bool {
	if claimsV03() {
		return truthy(spec["manager_accountable"])
	}
	return truthy(spec["stops_manager"])
}

// incidentPreventable applies GOV-47 and MGR-45 to a new incident in the transaction of its
// opening: the record keeps true or the reason code why the incident is not preventable.
func (c *Core) incidentPreventable(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, ic incidentClass, r *incidentRow) error {
	if !truthy(ic.Spec["manager_accountable"]) {
		return nil
	}
	m, err := loadManagerSeat(ctx, tx, true)
	if err != nil {
		return err
	}
	why, sign := "", ""
	switch {
	case !managerEnabled(cat, r.Tenant):
		why = "manager_disabled"
	case m == nil || m.Holder == "" || m.State == "vacant":
		why = "seat_vacant"
	case m.State != "held":
		why = "seat_stopped"
	default:
		sign, why, err = c.preventableSign(ctx, tx, now, cat, ic, r, m)
		if err != nil {
			return err
		}
	}
	if why != "" {
		r.Record["manager_preventable"] = why
		return r.save(ctx, tx, now)
	}
	r.Record["manager_preventable"] = true
	if err := r.save(ctx, tx, now); err != nil {
		return err
	}
	// The page goes to the monitoring system, or by the fallback path without one (MON-1, D-88).
	if err := c.fireAlarm(ctx, tx, now, Alarm{Tenant: r.Tenant, Name: "manager.preventable", Subject: AlarmSubject{Kind: "seat", ID: managerSeatID},
		Severity: "page", Values: map[string]any{"class": r.Class, "sign": sign}}); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `INSERT INTO manager_restrictions (tenant, incident, holder, since) VALUES ($1, $2, $3, $4)
		ON CONFLICT (tenant) WHERE until IS NULL DO NOTHING`,
		r.Tenant, r.ID, m.Holder, now)
	if err != nil {
		return err
	}
	if tag.RowsAffected() > 0 {
		if err := c.restrictionChanged(ctx, tx, now, m, r.Tenant, "seat.stopped.v1", preventableNotice, "system"); err != nil {
			return err
		}
	}
	// The people of the tenant see the mark in the incident message (MGR-39).
	if err := c.noticeLog(ctx, tx, now, r.ID, noticeEntry{Kind: "state", State: restrictedReason}, true); err != nil {
		return err
	}
	return c.noticePreventable(ctx, tx, now, cat, r.ID, r.Class, r.Tenant, sign, now, "notice.manager_preventable")
}

// withTenant sets the key tenant of a map of event data or notice params.
func withTenant(m map[string]any, tenant string) map[string]any {
	m["tenant"] = tenant
	return m
}

// restrictionChanged writes the seat event and the audit record of a restriction set or lifted
// in a tenant; the epoch stays (MGR-47).
func (c *Core) restrictionChanged(ctx context.Context, tx pgx.Tx, now time.Time, m *managerSeat, tenant, event, cause, actor string) error {
	var holder any
	if m.Holder != "" {
		holder = m.Holder
	}
	data := withTenant(map[string]any{"seat": managerSeatID, "event": eventName(event), "epoch": max(m.Epoch, 1), "holder": holder,
		"previous": holder, "cause": cause, "at": fmtTime(now)}, farmTenant)
	if err := c.spec.Validate("urn:botaru:schema:seat-event:1", data); err != nil {
		return err
	}
	if err := c.appendEvent(ctx, tx, now, farmTenant, Event{Type: event, Subject: managerSeatID, Data: data}); err != nil {
		return err
	}
	restricted := event == "seat.stopped.v1"
	// A restriction the core set or lifted by itself is the system's; seat.resume is farmctl.
	via := "farmctl"
	if actor == "" || actor == "system" {
		via = "system"
	}
	_, err := c.appendAudit(ctx, tx, now, Audit{
		Tenant: farmTenant, Kind: "command", Actor: Actor{Principal: principalOr(actor), Via: via},
		Command: cause, Target: &Target{Kind: "seat", ID: managerSeatID}, OK: true,
		Before: withTenant(map[string]any{"restricted": !restricted}, tenant), After: withTenant(map[string]any{"restricted": restricted, "epoch": m.Epoch}, tenant),
	})
	return err
}

// endRestrictions lifts the restrictions of the seat in every tenant when the holder changes or
// the seat is vacated (MGR-47).
func (c *Core) endRestrictions(ctx context.Context, tx pgx.Tx, now time.Time, m *managerSeat, cause, actor string) error {
	rows, err := tx.Query(ctx, `UPDATE manager_restrictions SET until = $1 WHERE until IS NULL RETURNING tenant`, now)
	if err != nil {
		return err
	}
	tenants, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	slices.Sort(tenants)
	for _, t := range tenants {
		if err := c.restrictionChanged(ctx, tx, now, m, t, "seat.resumed.v1", cause, actor); err != nil {
			return err
		}
	}
	return nil
}

// seatRestricted says whether a seat attempt in a tenant is in restricted mode: the tenant has a
// restriction, and the attempt's basis is not a sev1 incident of the tenant opened during it
// (MGR-47). Before v0.3 there is no restricted mode.
func seatRestricted(ctx context.Context, tx pgx.Tx, tenant, incident string) (bool, error) {
	if !claimsV03() {
		return false, nil
	}
	var since time.Time
	err := tx.QueryRow(ctx, `SELECT since FROM manager_restrictions WHERE tenant = $1 AND until IS NULL`, tenant).Scan(&since)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if incident == "" {
		return true, nil
	}
	var sev1 bool
	err = tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM incidents WHERE tenant = $1 AND lower(id) = lower($2) AND record->>'severity' = 'sev1' AND opened_at > $3)`,
		tenant, incident, since).Scan(&sev1)
	return !sev1, err
}

// seatTaskRestricted is seatRestricted for a seat task by its record, whose basis is the
// incident the core pinned at enqueue; a task of another seat or none is never restricted.
func seatTaskRestricted(ctx context.Context, tx pgx.Tx, tenant string, rec []byte) (bool, error) {
	if seatOfRecord(rec) != managerSeatID {
		return false, nil
	}
	var r struct {
		Incident string `json:"incident"`
	}
	_ = json.Unmarshal(rec, &r)
	return seatRestricted(ctx, tx, tenant, r.Incident)
}

// preventableSign computes the sign of the incident's class and the holder's responsibility
// over its window from records and code series; it returns the sign code, or the reason code why
// the incident is not preventable (MGR-45).
func (c *Core) preventableSign(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, ic incidentClass, r *incidentRow, m *managerSeat) (string, string, error) {
	responsible := func(window time.Duration) (bool, error) {
		start := now.Add(-window)
		if m.ResponsibleSince == nil || m.ResponsibleSince.After(start) {
			return false, nil
		}
		// A restriction in force at any time of the window, lifted or not, breaks it.
		var restricted bool
		err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM manager_restrictions WHERE tenant = $1 AND (until IS NULL OR until > $2))`, r.Tenant, start).Scan(&restricted)
		return !restricted, err
	}
	if r.SubjectKind != "resource" {
		return "", "class_not_preventable", nil
	}
	// (1) A full disk: the disk_filling incident on the disk for the whole window, the free
	// space falling by the code numbers of its alarm, and an involved party found by the class
	// functions.
	if volumeOf(cat, r.Tenant, r.SubjectID) != nil || strings.HasPrefix(mstr(mmap(ic.Spec, "detect"), "alarm"), "alarm.disk.") {
		start := now.Add(-preventWindow)
		var episodes []int64
		var implicated []byte
		err := tx.QueryRow(ctx, `SELECT episodes, coalesce(record->'implicated', '[]') FROM incidents WHERE tenant = $1 AND class = 'disk_filling'
			AND subject_kind = $2 AND subject_id = $3 AND opened_at <= $4 AND since <= $4 AND state = ANY($5) ORDER BY opened_at LIMIT 1`,
			r.Tenant, r.SubjectKind, r.SubjectID, start, incidentLive).Scan(&episodes, &implicated)
		if errors.Is(err, pgx.ErrNoRows) {
			return "", "no_filling_incident", nil
		}
		if err != nil {
			return "", "", err
		}
		startVals, err := alarmValuesAt(ctx, tx, episodes, start)
		if err != nil {
			return "", "", err
		}
		endVals, err := alarmValuesAt(ctx, tx, episodes, now)
		if err != nil {
			return "", "", err
		}
		if !diskGrew(startVals, endVals) {
			return "", "no_growth", nil
		}
		if string(implicated) == "[]" || string(implicated) == "null" {
			return "", "no_involved", nil
		}
		if ok, err := responsible(preventWindow); err != nil || !ok {
			return "", "not_responsible", err
		}
		return "disk_full", "", nil
	}
	// (2) A catalog service down and not recovered within the set time of its class.
	if svc, ok := serviceOf(cat, r.Tenant, r.SubjectID); ok {
		window := specDuration(mmap(ic.Spec, "detect"), "for")
		if window <= 0 {
			return "", "no_window", nil
		}
		start := now.Add(-window)
		tv := cat.Tenants[r.Tenant]
		listed := slices.Contains(mstrs(mmap(ic.Spec, "responses"), "services"), "restart") && slices.Contains(svc.Control, "restart") &&
			slices.Contains(svc.NoCard, "restart") && hasCap(tv, groupClosure(tv, []string{"farm-manager-ops"}), "service.control:"+svc.Name)
		if !listed {
			return "", "restart_not_listed", nil
		}
		var open, distrust, called bool
		if err := tx.QueryRow(ctx, `SELECT
			EXISTS (SELECT 1 FROM incidents WHERE tenant = $1 AND subject_kind = $2 AND subject_id = $3 AND id <> $4 AND opened_at <= $5 AND since <= $5 AND state = ANY($6)),
			EXISTS (SELECT 1 FROM tasks WHERE tenant = $1 AND record->>'seat' = $7 AND coalesce((record->>'untrusted_seen')::boolean, false) AND updated_at >= $5),
			EXISTS (SELECT 1 FROM toolcalls c JOIN tasks t ON t.id = c.task WHERE c.tenant = $1 AND c.tool = $8 AND c.args->>'service' = $9
				AND c.created_at >= $5 AND t.record->>'seat' = $7)
			OR EXISTS (SELECT 1 FROM approvals a JOIN tasks t ON t.id = a.source_task WHERE a.tenant = $1 AND a.action = $10 AND a.record->'args'->>'service' = $9
				AND a.created_at >= $5 AND t.record->>'seat' = $7)`,
			r.Tenant, r.SubjectKind, r.SubjectID, r.ID, start, incidentLive, managerSeatID, serviceRight, svc.Name, serviceAction).Scan(&open, &distrust, &called); err != nil {
			return "", "", err
		}
		switch {
		case !open:
			return "", "no_open_incident", nil
		case distrust:
			return "", "distrust_mode", nil
		case called:
			return "", "seat_acted", nil
		}
		if ok, err := responsible(window); err != nil || !ok {
			return "", "not_responsible", err
		}
		return "service_down", "", nil
	}
	// (3) to (5) and any other class: backups, an unreachable core and the spending stop are
	// never the manager's to prevent.
	return "", "class_not_preventable", nil
}

// serviceOf is the catalog service a resource subject names: res:<host>:<service> or
// <host>:<service>.
func serviceOf(cat *Catalog, tenant, subject string) (catalogService, bool) {
	for _, s := range catalogServices(cat, tenant) {
		name := strings.ToLower(s.Host + ":" + s.Name)
		if subject == name || subject == "res:"+name {
			return s, true
		}
	}
	return catalogService{}, false
}

// noticePreventable sends manager_preventable to the operators with admin:governance in farm,
// without buttons and built only from codes and numbers, with the ready farmctl commands
// (GOV-47, PRM-14).
func (c *Core) noticePreventable(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, incident, class, tenant, sign string, at time.Time, key string) error {
	return c.noticeAdmins(ctx, tx, now, cat, []string{farmTenant}, preventableNotice, key, withTenant(map[string]any{
		"incident": incident, "class": class, "at": fmtTime(at), "sign": sign}, tenant), "admin:governance")
}

// preventableRepeats are the repeats of manager_preventable after a restriction set at since:
// after 24 h, after 72 h and then once a week (GOV-47).
func preventableRepeats(since, now time.Time) int {
	n := 0
	for _, d := range []time.Duration{24 * time.Hour, 72 * time.Hour} {
		if !since.Add(d).After(now) {
			n++
		}
	}
	if weekly := now.Sub(since.Add(72 * time.Hour)); weekly >= 7*24*time.Hour {
		n += int(weekly / (7 * 24 * time.Hour))
	}
	return n
}

// repeatPreventable repeats the notice of every restriction still in place that no operator
// marked read; a repeat the loop missed is not sent twice (GOV-47).
func (c *Core) repeatPreventable(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog) error {
	rows, err := tx.Query(ctx, `SELECT r.tenant, r.incident, coalesce(i.class, ''), r.since, r.repeats FROM manager_restrictions r
		LEFT JOIN incidents i ON i.id = r.incident WHERE r.until IS NULL AND r.read_at IS NULL ORDER BY r.tenant FOR UPDATE OF r`)
	if err != nil {
		return err
	}
	type due struct {
		tenant, incident, class string
		since                   time.Time
		repeats                 int
	}
	all, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (due, error) {
		var d due
		return d, row.Scan(&d.tenant, &d.incident, &d.class, &d.since, &d.repeats)
	})
	if err != nil {
		return err
	}
	for _, d := range all {
		n := preventableRepeats(d.since, now)
		if n <= d.repeats {
			continue
		}
		if _, err := tx.Exec(ctx, `UPDATE manager_restrictions SET repeats = $2 WHERE tenant = $1 AND until IS NULL`, d.tenant, n); err != nil {
			return err
		}
		if err := c.noticePreventable(ctx, tx, now, cat, d.incident, d.class, d.tenant, "", d.since, "notice.manager_preventable_repeat"); err != nil {
			return err
		}
	}
	return nil
}

// seatResumeTenant is P2 seat.resume with a tenant: an operator of farm lifts the restricted
// mode of the seat in that tenant; the epoch stays (MGR-47).
func (c *Core) seatResumeTenant(ctx context.Context, op Operator, tenant string) (*Done, error) {
	var out Done
	err := c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		m, err := loadManagerSeat(ctx, tx, true)
		if err != nil {
			return err
		}
		if m == nil {
			return refuse("not_found", "no farm manager seat")
		}
		tag, err := tx.Exec(ctx, `UPDATE manager_restrictions SET until = $2 WHERE tenant = $1 AND until IS NULL`, tenant, now)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return refuse("conflict", "the seat is not restricted in the tenant")
		}
		if err := c.restrictionChanged(ctx, tx, now, m, tenant, "seat.resumed.v1", "seat.resume", op.Principal); err != nil {
			return err
		}
		out = Done{OK: true, ID: managerSeatID, State: m.State}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// managerMonthGate decides a seat task of the manager line at claim from v0.3: sev1 work and
// any other line run as before; work outside sev1 after 70% of the month runs on the farm
// charter's fallback_profile when the eval index has a clean mgr-inj report for it, otherwise it
// waits for the next month (MGR-49).
func (c *Core) managerMonthGate(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, tv *TenantView, rec []byte, tt manifest.TaskType) (string, bool, error) {
	if line, _ := c.taskLine(rec, tt); line != "manager" || tv == nil {
		return "", false, nil
	}
	var r struct {
		Severity string `json:"severity"`
	}
	_ = json.Unmarshal(rec, &r)
	if r.Severity == "sev1" {
		return "", false, nil
	}
	spent, err := managerMonthSpent(ctx, tx, now, tv)
	if err != nil || !spent {
		return "", false, err
	}
	fb := mstr(managerCharter(cat), "fallback_profile")
	if fb == "" {
		return "", true, nil
	}
	fp, err := seatFingerprint(cat, fb)
	if err != nil {
		return "", false, err
	}
	if fp == "" || !seatEvalPassed(catalogEvalReports(cat), fb, fp) {
		return "", true, nil
	}
	return fb, false, nil
}
