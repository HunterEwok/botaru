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

// The farm manager seat (MGR-1, MGR-2, MGR-3, MGR-42, GOV-16, GOV-22, spec/lifecycles/seat.md).
// Tenant farm keeps only the seat: its holder, epoch, state and stop reasons with the time of
// each. A person with the operator credential of farm assigns a holder, and the core always
// creates a new identity for it. Seat tasks live in the tenants: a seat attempt in tenant T
// runs on a worker of a seat role enrolled in T, on behalf of the holder with the seat epoch.
// The seat is in held only while its stop reason set is empty: human (a hold on the scope
// seat), no_control (the safeguard, MGR-14) and incident (a stopping incident).

const (
	managerSeatID = "farm-manager"
	farmTenant    = "farm"
	// maxDecideWithin is the default and the longest time people have to decide on an
	// incident stop before the holder leaves (GOV-22).
	maxDecideWithin = 7 * 24 * time.Hour
	// preventWindow is the time a disk incident's holder must have been responsible (MGR-34).
	preventWindow = 2 * time.Hour
	// absentAfter is how long the seat may be out of held, or a seat task wait for a worker,
	// before alarm.manager.absent (MGR-2).
	absentAfter = 5 * time.Minute
	// holderHoldsCap is how long the holds of a replaced holder still live (MGR-1, MGR-42).
	holderHoldsCap = 15 * time.Minute
	// incidentRespondType is the seat task the core enqueues for a new or reopened incident.
	incidentRespondType = "manager.incident.respond.v1"
	// seatSource is the source of a farm manager seat task.
	seatSource = "seat"
)

// managerProfiles are the role profiles of the seat roles (spec/model/role-profiles.yaml).
var managerProfiles = []string{"farm-manager", "farm-manager-reader"}

// seatTenant is the tenant whose seat registry holds a seat named by a lease or a task: farm for
// the farm manager seat, the task's own tenant for a commission seat.
func seatTenant(tenant, seat string) string {
	if seat == managerSeatID {
		return farmTenant
	}
	return tenant
}

// stopReason is one reason of the seat's stop reason set.
type stopReason struct {
	Reason   string `json:"reason"`
	At       string `json:"at"`
	Incident string `json:"incident,omitempty"`
	// Repeats counts the repeats of manager_stopped sent for the reason incident (MGR-36).
	Repeats int `json:"repeats,omitempty"`
}

// managerSeat is the farm manager seat as the store keeps it.
type managerSeat struct {
	seatRow
	Reasons          []stopReason
	DecideUntil      *time.Time
	ResponsibleSince *time.Time
	UnheldSince      *time.Time
}

func loadManagerSeat(ctx context.Context, tx pgx.Tx, lock bool) (*managerSeat, error) {
	q := `SELECT tenant, id, kind, coalesce(holder, ''), state, epoch, stop_reasons, decide_until, responsible_since, unheld_since
		FROM seats WHERE tenant = $1 AND id = $2`
	if lock {
		q += ` FOR UPDATE`
	}
	var m managerSeat
	var raw []byte
	err := tx.QueryRow(ctx, q, farmTenant, managerSeatID).Scan(&m.Tenant, &m.ID, &m.Kind, &m.Holder, &m.State, &m.Epoch, &raw,
		&m.DecideUntil, &m.ResponsibleSince, &m.UnheldSince)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &m.Reasons); err != nil {
		return nil, err
	}
	return &m, nil
}

func (m *managerSeat) has(reason string) bool {
	return slices.ContainsFunc(m.Reasons, func(r stopReason) bool { return r.Reason == reason })
}

func (m *managerSeat) reason(reason string) *stopReason {
	for i := range m.Reasons {
		if m.Reasons[i].Reason == reason {
			return &m.Reasons[i]
		}
	}
	return nil
}

// without is the reason set without the given reasons.
func (m *managerSeat) without(reasons ...string) []stopReason {
	out := []stopReason{}
	for _, r := range m.Reasons {
		if !slices.Contains(reasons, r.Reason) {
			out = append(out, r)
		}
	}
	return out
}

func (m *managerSeat) save(ctx context.Context, tx pgx.Tx) error {
	if m.Reasons == nil {
		m.Reasons = []stopReason{}
	}
	raw, err := json.Marshal(m.Reasons)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE seats SET stop_reasons = $3, decide_until = $4, responsible_since = $5, unheld_since = $6 WHERE tenant = $1 AND id = $2`,
		farmTenant, managerSeatID, raw, m.DecideUntil, m.ResponsibleSince, m.UnheldSince)
	return err
}

// managerRole is the role of the seat holder's identity: the first role of profile
// farm-manager by name, or "" when the catalog has none.
func managerRole(cat *Catalog) string {
	for _, name := range slices.Sorted(mapKeys(cat.Roles)) {
		if profileOf(cat, name) == "farm-manager" {
			return name
		}
	}
	return ""
}

// managerEnabled says whether the farm manager works with a tenant: Tenant.spec.farm_manager
// .enabled, true by default (MGR-42, D-77). Tenant farm has no seat tasks (MGR-3).
func managerEnabled(cat *Catalog, tenant string) bool {
	tv, ok := cat.Tenants[tenant]
	if !ok || tenant == farmTenant {
		return false
	}
	return mmap(mmap(tv.Raw, "spec"), "farm_manager")["enabled"] != false
}

// managerCharter is the farm_manager block of the farm charter.
func managerCharter(cat *Catalog) map[string]any {
	return mmap(tenantSpecOf(cat, "Governance", farmTenant), "farm_manager")
}

// decideWithin is incident_stop.decide_within of the farm charter, at most 7 days.
func decideWithin(cat *Catalog) time.Duration {
	d := specDuration(mmap(managerCharter(cat), "incident_stop"), "decide_within")
	if d <= 0 || d > maxDecideWithin {
		return maxDecideWithin
	}
	return d
}

// shiftManager moves the seat to a holder and a stop reason set in one step: the state follows
// them (vacant without a holder, held with a holder and no reason, stopped otherwise), the
// epoch grows, the running attempts of seat tasks in every tenant are fenced, and the seat
// event and the audit record are written (MGR-1, GOV-22).
func (c *Core) shiftManager(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, m *managerSeat, holder string, reasons []stopReason, event, cause, actor string) (int64, error) {
	state := "vacant"
	if holder != "" {
		state = "held"
		if len(reasons) > 0 {
			state = "stopped"
		}
	}
	// Responsibility for MGR-34 runs while one holder keeps the seat held, and survives only a
	// stop by the safeguard.
	onlyNoControl := !slices.ContainsFunc(reasons, func(r stopReason) bool { return r.Reason != "no_control" })
	if holder != m.Holder || !onlyNoControl || holder == "" {
		m.ResponsibleSince = nil
	}
	if state == "held" && m.ResponsibleSince == nil {
		m.ResponsibleSince = &now
	}
	if state != "held" && (m.State == "held" || m.UnheldSince == nil) {
		m.UnheldSince = &now
	} else if state == "held" {
		m.UnheldSince = nil
	}
	hadIncident := m.has("incident")
	m.Reasons = reasons
	switch {
	case !m.has("incident"):
		m.DecideUntil = nil
	case !hadIncident || m.DecideUntil == nil:
		until := now.Add(decideWithin(cat))
		m.DecideUntil = &until
	}
	if err := m.save(ctx, tx); err != nil {
		return 0, err
	}
	seq, err := c.moveSeat(ctx, tx, now, &m.seatRow, holder, state, event, cause, actor)
	if err != nil {
		return 0, err
	}
	m.Holder, m.State, m.Epoch = holder, state, m.Epoch+1
	return seq, nil
}

// leaveReason is the retirement trigger of a holder that people replace: after an incident stop
// replaced_after_incident, otherwise replaced_by_human (IDN-11, GOV-22).
func (m *managerSeat) leaveReason() string {
	if m.has("incident") {
		return "replaced_after_incident"
	}
	return "replaced_by_human"
}

// retireHolder retires the holder that leaves the seat: its running attempts are fenced, and the
// holds it set live no longer than 15 minutes (MGR-1).
func (c *Core) retireHolder(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, m *managerSeat, successor, actor string) error {
	if m.Holder == "" {
		return nil
	}
	prev, err := loadBot(ctx, tx, m.Holder, true)
	if err != nil {
		return err
	}
	if prev != nil {
		if _, err := c.retireBot(ctx, tx, now, cat, prev, m.leaveReason(), successor, actor); err != nil {
			return err
		}
	}
	return capHolderHolds(ctx, tx, now, m.Holder, "")
}

// capHolderHolds shortens the active holds a seat holder set, in every tenant or in one, to at
// most 15 minutes from now (MGR-1, MGR-42). A holder's hold carries its identity as author.
func capHolderHolds(ctx context.Context, tx pgx.Tx, now time.Time, holder, tenant string) error {
	_, err := tx.Exec(ctx, `UPDATE holds SET until = $2 WHERE state = 'active' AND author = $1 AND author_kind = 'seat_holder'
		AND ($3 = '' OR tenant = $3) AND (until IS NULL OR until > $2)`, holder, now.Add(holderHoldsCap), tenant)
	return err
}

// managerAssign is P2 seat.assign of the farm manager seat: only the operator of farm assigns,
// the core creates a new identity without origin in farm, and the previous holder leaves with
// the new one as successor. The assignment ends an incident stop; the reasons human and
// no_control stay until a person releases them (MGR-1, GOV-22, PRM-14).
func (c *Core) managerAssign(ctx context.Context, op Operator, req SeatRequest) (*Done, error) {
	if op.Tenant != farmTenant {
		return nil, refuse("forbidden", "the farm manager seat is assigned with the operator credential of farm")
	}
	if req.Bot != "" {
		return nil, refuse("input_schema_invalid", "the farm manager seat takes no bot: the core creates a new identity")
	}
	cat, err := c.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	var out Done
	err = c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		m, err := loadManagerSeat(ctx, tx, true)
		if err != nil {
			return err
		}
		if m == nil {
			return refuse("not_found", "no farm manager seat")
		}
		role := managerRole(cat)
		if role == "" {
			return refuse("forbidden", "the catalog has no role of profile farm-manager")
		}
		id, err := c.insertBot(ctx, tx, now, farmTenant, role, "seat", "", managerSeatID, "seat.assign")
		if err != nil {
			return err
		}
		if err := c.retireHolder(ctx, tx, now, cat, m, id, op.Principal); err != nil {
			return err
		}
		// The sections of a holder that left before come to the new one (MGR-27).
		if err := c.pickUpSeatOffers(ctx, tx, now, cat, id); err != nil {
			return err
		}
		seq, err := c.shiftManager(ctx, tx, now, cat, m, id, m.without("incident"), "seat.assigned.v1", "seat.assign", op.Principal)
		if err != nil {
			return err
		}
		out = Done{OK: true, ID: managerSeatID, State: m.State, AuditSeq: seq}
		return c.noticeManagerChanged(ctx, tx, now, cat, m, "assigned")
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// managerVacate is P2 seat.vacate of the farm manager seat.
func (c *Core) managerVacate(ctx context.Context, op Operator) (*Done, error) {
	if op.Tenant != farmTenant {
		return nil, refuse("forbidden", "the farm manager seat is vacated with the operator credential of farm")
	}
	cat, err := c.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	var out Done
	err = c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		m, err := loadManagerSeat(ctx, tx, true)
		if err != nil {
			return err
		}
		if m == nil {
			return refuse("not_found", "no farm manager seat")
		}
		out = Done{OK: true, ID: managerSeatID, State: "vacant"}
		if m.Holder == "" {
			return nil
		}
		if err := c.retireHolder(ctx, tx, now, cat, m, "", op.Principal); err != nil {
			return err
		}
		seq, err := c.shiftManager(ctx, tx, now, cat, m, "", m.without("incident"), "seat.vacated.v1", "seat.vacate", op.Principal)
		if err != nil {
			return err
		}
		out.AuditSeq = seq
		return c.noticeManagerChanged(ctx, tx, now, cat, m, "vacated")
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// SeatResumeRequest is p2-seat-resume-request.
type SeatResumeRequest struct {
	Seat     string `json:"seat"`
	Incident string `json:"incident"`
}

// SeatResume is P2 seat.resume: it removes only the reason incident, and only after people
// declared the incident that stopped the seat and the stopping incidents of the same minute
// false; the epoch grows (GOV-22).
func (c *Core) SeatResume(ctx context.Context, op Operator, req SeatResumeRequest) (*Done, error) {
	if req.Seat != managerSeatID {
		return nil, refuse("not_found", "only the farm manager seat resumes")
	}
	if op.Tenant != farmTenant {
		return nil, refuse("forbidden", "the farm manager seat is resumed with the operator credential of farm")
	}
	cat, err := c.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	var out Done
	err = c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		m, err := loadManagerSeat(ctx, tx, true)
		if err != nil {
			return err
		}
		if m == nil {
			return refuse("not_found", "no farm manager seat")
		}
		r := m.reason("incident")
		if r == nil {
			return refuse("conflict", "the seat has no incident stop")
		}
		if !strings.EqualFold(r.Incident, req.Incident) {
			return refuse("conflict", "the seat was stopped by another incident")
		}
		var open int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM incidents i JOIN incidents t ON lower(t.id) = lower($1)
			WHERE i.stops_manager AND date_trunc('minute', i.opened_at) = date_trunc('minute', t.opened_at) AND i.state <> 'dismissed'`,
			r.Incident).Scan(&open); err != nil {
			return err
		}
		if open > 0 {
			return refuse("conflict", "the incident and the stopping incidents of its minute are not all dismissed")
		}
		seq, err := c.shiftManager(ctx, tx, now, cat, m, m.Holder, m.without("incident"), "seat.resumed.v1", "seat.resume", op.Principal)
		if err != nil {
			return err
		}
		out = Done{OK: true, ID: managerSeatID, State: m.State, AuditSeq: seq}
		return c.noticeManagerChanged(ctx, tx, now, cat, m, "resumed")
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// stopManager adds a stop reason to the seat with epoch + 1. A vacant seat keeps the reason for
// its next holder; a seat with a holder stops, and the running seat attempts in every tenant end
// with seat_changed (MGR-1, MGR-15, GOV-22).
func (c *Core) stopManager(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, m *managerSeat, reason, incident string) error {
	if m.has(reason) {
		return nil
	}
	reasons := append(slices.Clone(m.Reasons), stopReason{Reason: reason, At: fmtTime(now), Incident: incident})
	// A vacant seat stays vacant, but a new reason still grows the epoch with its event and
	// audit record, so the next holder starts from a visible change (seat.md).
	if _, err := c.shiftManager(ctx, tx, now, cat, m, m.Holder, reasons, "seat.stopped.v1", reason, "system"); err != nil {
		return err
	}
	if reason == "incident" && m.Holder != "" {
		return c.noticeManagerStopped(ctx, tx, now, cat, m, "notice.manager_stopped")
	}
	return nil
}

// syncHumanStop keeps the reasons human and no_control in step with the holds on the scope seat:
// a person's hold adds human and the safeguard hold adds no_control, and the release of the last
// one of each removes its reason (MGR-14, MGR-15).
func (c *Core) syncHumanStop(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog) error {
	m, err := loadManagerSeat(ctx, tx, true)
	if err != nil || m == nil {
		return err
	}
	// The safeguard is the hold code sets with the reason manager_breaker (MGR-14); any other
	// seat hold is a person's kill switch (MGR-15), whatever its effects.
	var held, guard bool
	if err := tx.QueryRow(ctx, `SELECT
		EXISTS (SELECT 1 FROM holds WHERE state = 'active' AND scope_kind = 'seat' AND scope_name = $1 AND (until IS NULL OR until > $2)
			AND NOT (author_kind = 'contain_auto' AND reason = 'manager_breaker')),
		EXISTS (SELECT 1 FROM holds WHERE state = 'active' AND scope_kind = 'seat' AND scope_name = $1 AND (until IS NULL OR until > $2)
			AND author_kind = 'contain_auto' AND reason = 'manager_breaker')`,
		managerSeatID, now).Scan(&held, &guard); err != nil {
		return err
	}
	// A no_control reason without a safeguard hold, from before the hold existed, goes with the
	// release of the last person's hold.
	keepGuard := guard || (m.has("no_control") && held)
	if held == m.has("human") && keepGuard == m.has("no_control") {
		return nil
	}
	reasons := m.without("human", "no_control")
	for _, r := range m.Reasons {
		if (r.Reason == "human" && held) || (r.Reason == "no_control" && keepGuard) {
			reasons = append(reasons, r)
		}
	}
	if held && !m.has("human") {
		reasons = append(reasons, stopReason{Reason: "human", At: fmtTime(now)})
	}
	if keepGuard && !m.has("no_control") {
		reasons = append(reasons, stopReason{Reason: "no_control", At: fmtTime(now)})
	}
	event, cause := "seat.stopped.v1", "hold.release"
	switch {
	case held && !m.has("human"):
		cause = "human"
	case keepGuard && !m.has("no_control"):
		cause = "no_control"
	}
	if len(reasons) == 0 {
		event = "seat.resumed.v1"
	}
	_, err = c.shiftManager(ctx, tx, now, cat, m, m.Holder, reasons, event, cause, "system")
	return err
}

// incidentStop applies GOV-22 to a new incident of a class with stops_manager: the seat stops
// with the reason incident when the manager works with the incident's tenant, the seat is held
// and, for a disk, MGR-33 and MGR-34 hold. Otherwise the record keeps the reason code why there
// is no stop. A seat already stopped by an incident only gets the incident on its card.
func (c *Core) incidentStop(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, ic incidentClass, r *incidentRow) error {
	if !truthy(ic.Spec["stops_manager"]) {
		return nil
	}
	if _, err := tx.Exec(ctx, `UPDATE incidents SET stops_manager = true WHERE id = $1`, r.ID); err != nil {
		return err
	}
	m, err := loadManagerSeat(ctx, tx, true)
	if err != nil {
		return err
	}
	// A resource subject is a disk when it names a catalog volume or the class watches a disk
	// alarm; any other resource stops the seat as any other subject does.
	disk := r.SubjectKind == "resource" && (volumeOf(cat, r.Tenant, r.SubjectID) != nil ||
		strings.HasPrefix(mstr(mmap(ic.Spec, "detect"), "alarm"), "alarm.disk."))
	effect := ""
	switch {
	case !managerEnabled(cat, r.Tenant):
		effect = "manager_disabled"
	case m == nil || m.Holder == "" || m.State == "vacant":
		effect = "seat_vacant"
	case disk:
		effect, err = c.diskStopCondition(ctx, tx, now, cat, ic, r, m)
		if err != nil {
			return err
		}
	}
	if effect == "" {
		onlyNoControl := len(m.without("no_control")) == 0
		switch {
		case m.State == "held", m.State == "stopped" && disk && onlyNoControl:
			// The safeguard inside the window of an incident the holder answers for does not
			// shield it from the stop (GOV-22, MGR-34).
			return c.stopManager(ctx, tx, now, cat, m, "incident", r.ID)
		case m.has("incident"):
			effect = "seat_stopped"
			// An incident of the stop is appended to the card as its own line (MGR-35).
			if err := c.noticeAdmins(ctx, tx, now, cat, []string{farmTenant}, "manager_stopped", "notice.manager_stopped_joined",
				map[string]any{"incident": r.ID, "class": r.Class, "tenant": r.Tenant, "at": fmtTime(now)}, "admin:governance"); err != nil {
				return err
			}
		default:
			effect = "seat_stopped"
		}
	}
	r.Record["stops_manager_effect"] = effect
	return r.save(ctx, tx, now)
}

// diskStopCondition checks MGR-33 and MGR-34 for a disk incident and returns the reason code
// when one of them does not hold, or "".
func (c *Core) diskStopCondition(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, ic incidentClass, r *incidentRow, m *managerSeat) (string, error) {
	// MGR-33: every writer of the disk has a quota, and the quotas add up below the class's
	// alarm threshold on the used bytes.
	vol := volumeOf(cat, r.Tenant, r.SubjectID)
	writers := mlist(vol, "writers")
	if len(writers) == 0 {
		return "quota_missing", nil
	}
	var sum float64
	for _, w := range writers {
		q, ok := mnum(asMap(w), "quota_bytes")
		if !ok {
			return "quota_missing", nil
		}
		sum += q
	}
	limit, ok := usedThreshold(ic.Spec, vol)
	if !ok {
		return "quota_threshold_unknown", nil
	}
	if sum >= limit {
		return "quota_over_threshold", nil
	}
	// MGR-34: a disk_filling incident live on the disk without a break for the whole window
	// (its since moves on a reopen), the used size grew between the window's start and its end
	// by the numbers its alarm logged, and one holder answered for the seat all that time.
	windowStart := now.Add(-preventWindow)
	var episodes []int64
	err := tx.QueryRow(ctx, `SELECT episodes FROM incidents WHERE tenant = $1 AND class = 'disk_filling' AND subject_kind = $2 AND subject_id = $3
		AND opened_at <= $4 AND since <= $4 AND state = ANY($5) ORDER BY opened_at LIMIT 1`, r.Tenant, r.SubjectKind, r.SubjectID, windowStart, incidentLive).Scan(&episodes)
	if errors.Is(err, pgx.ErrNoRows) {
		return "no_filling_incident", nil
	}
	if err != nil {
		return "", err
	}
	startVals, err := alarmValuesAt(ctx, tx, episodes, windowStart)
	if err != nil {
		return "", err
	}
	endVals, err := alarmValuesAt(ctx, tx, episodes, now)
	if err != nil {
		return "", err
	}
	if !diskGrew(startVals, endVals) {
		return "no_growth", nil
	}
	if m.ResponsibleSince == nil || m.ResponsibleSince.After(now.Add(-preventWindow)) {
		return "not_responsible", nil
	}
	return "", nil
}

// alarmValuesAt are the numbers an incident's alarm episodes logged last at or before at, or nil.
func alarmValuesAt(ctx context.Context, tx pgx.Tx, episodes []int64, at time.Time) (map[string]any, error) {
	var raw []byte
	err := tx.QueryRow(ctx, `SELECT vals FROM alarm_values WHERE episode = ANY($1) AND at <= $2 ORDER BY at DESC LIMIT 1`, episodes, at).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var vals map[string]any
	if err := json.Unmarshal(raw, &vals); err != nil {
		return nil, err
	}
	return vals, nil
}

// volumeOf is the volume of a tenant catalog that a disk subject names: res:<host>:<volume> or
// <host>:<volume> (CAP-6).
func volumeOf(cat *Catalog, tenant, subject string) map[string]any {
	for _, v := range mlist(tenantSpecOf(cat, "Catalog", tenant), "volumes") {
		vm := asMap(v)
		name := strings.ToLower(mstr(vm, "host") + ":" + mstr(vm, "volume"))
		if subject == name || subject == "res:"+name {
			return vm
		}
	}
	return nil
}

// usedThreshold is the used-bytes threshold of a class: the value of its detect condition gt or
// ge on values.used_bytes, or, for a condition lt or le on values.free_pct, the used bytes of a
// volume with its size_bytes at that free share.
func usedThreshold(spec, vol map[string]any) (float64, bool) {
	for _, w := range mlist(mmap(spec, "detect"), "when") {
		wm := asMap(w)
		f, ok := toFloat(wm["value"])
		if !ok {
			continue
		}
		switch atom := mstr(wm, "atom"); mstr(wm, "field") {
		case "values.used_bytes":
			if atom == "gt" || atom == "ge" {
				return f, true
			}
		case "values.free_pct":
			if size, ok := mnum(vol, "size_bytes"); ok && size > 0 && (atom == "lt" || atom == "le") && f >= 0 && f <= 100 {
				return size * (1 - f/100), true
			}
		}
	}
	return 0, false
}

// diskGrew compares the code numbers of the disk at the start and at the end of the window:
// more used bytes, or less free share.
func diskGrew(start, end map[string]any) bool {
	if a, ok := toFloat(start["used_bytes"]); ok {
		if b, ok := toFloat(end["used_bytes"]); ok {
			return b > a
		}
	}
	if a, ok := toFloat(start["free_pct"]); ok {
		if b, ok := toFloat(end["free_pct"]); ok {
			return b < a
		}
	}
	return false
}

func asMap(v any) map[string]any { m, _ := v.(map[string]any); return m }

// enqueueIncidentTask puts the seat task of a new or reopened incident in its tenant, to run on
// behalf of the holder; a class of the subject farm, a tenant without the manager or a catalog
// without the type gets none (MGR-3, MGR-42). An enqueue refusal does not stop the incident.
func (c *Core) enqueueIncidentTask(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, ic incidentClass, r *incidentRow) error {
	role := managerRole(cat)
	if r.SubjectKind == "farm" || !managerEnabled(cat, r.Tenant) || role == "" || !slices.Contains(cat.Roles[role].Claims, incidentRespondType) {
		return nil
	}
	if _, ok := cat.TaskTypes[incidentRespondType]; !ok {
		return nil
	}
	body := map[string]any{"type": incidentRespondType, "goal": "Respond to the incident.", "params": map[string]any{"input": incidentView(ic, r)}}
	sev := mstr(r.Record, "severity")
	res, err := c.enqueue(ctx, tx, now, cat, enqueueInput{Tenant: r.Tenant, Type: incidentRespondType, Body: body,
		IdempotencyKey: "incident:" + r.ID + ":" + strconv.FormatInt(r.Episode, 10), Initiator: "system", EnqueuedBy: "system:incident",
		Source: seatSource, SourceID: r.ID, Priority: 1, Sev1: sev == "sev1"})
	if err != nil {
		return err
	}
	if res.Rejected != nil {
		c.log.Warn("the seat task of an incident was refused", "incident", r.ID, "reason", res.Rejected.Reason)
		return nil
	}
	extra, err := json.Marshal(map[string]any{"seat": managerSeatID, "executor_role": role, "incident": r.ID, "severity": nullable(sev)})
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE tasks SET record = record || $2::jsonb WHERE id = $1`, res.TaskID, extra)
	return err
}

// incidentView is the incident/v1 view the seat task gets in its input block: numbers, ids and
// codes of the incident, its alarm and involved parties, and the responses its class allows,
// without tenant text (MGR-25, spec/model/manager-incident-view.schema.json).
func incidentView(ic incidentClass, r *incidentRow) map[string]any {
	subject := map[string]any{"kind": r.SubjectKind}
	if r.SubjectID != "" {
		subject["id"] = r.SubjectID
	}
	allowed := map[string]any{}
	for k, v := range mmap(ic.Spec, "responses") {
		allowed[k] = v
	}
	implicated, _ := r.Record["implicated"].([]any)
	if implicated == nil {
		implicated = []any{}
	}
	view := map[string]any{"view": "incident/v1", "incident": r.ID, "class": r.Class, "state": r.State, "tenant": r.Tenant,
		"subject": subject, "implicated": implicated, "allowed": allowed, "humans_took": r.Record["humans_took"] == true}
	if sev := mstr(r.Record, "severity"); sev != "" {
		view["severity"] = sev
	}
	if a, ok := r.Record["alarm"].(map[string]any); ok {
		view["alarm"] = a
	}
	return view
}

// managerDisabled applies a tenant's farm_manager.enabled: false in the apply transaction: the
// running seat attempts in it end with manager_disabled, its waiting seat tasks are canceled,
// and the holder's holds in it live no longer than 15 minutes (MGR-42).
func (c *Core) managerDisabled(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog) error {
	m, err := loadManagerSeat(ctx, tx, false)
	if err != nil {
		return err
	}
	for _, t := range slices.Sorted(mapKeys(cat.Tenants)) {
		if t == farmTenant || managerEnabled(cat, t) {
			continue
		}
		if err := c.fenceAttempts(ctx, tx, now, "manager_disabled", `seat = $1 AND tenant = $2`, managerSeatID, t); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT id FROM tasks WHERE tenant = $1 AND record->>'seat' = $2 AND state IN ('submitted', 'working') ORDER BY id FOR UPDATE`, t, managerSeatID)
		if err != nil {
			return err
		}
		list, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		if err := c.cancelTaskIDs(ctx, tx, now, t, list, "manager_disabled"); err != nil {
			return err
		}
		if m != nil && m.Holder != "" {
			if err := capHolderHolds(ctx, tx, now, m.Holder, t); err != nil {
				return err
			}
		}
	}
	return nil
}

// managerLoop is the seat part of the identity loop: the reason human follows the seat
// holds as they expire, the decide_within deadline of an incident stop retires the holder, and
// alarm.manager.absent fires while the seat is out of held or a seat task waits for a worker
// for longer than 5 minutes (MGR-2, GOV-22).
func (c *Core) managerLoop(ctx context.Context) error {
	cat, err := c.Catalog(ctx)
	if err != nil {
		return err
	}
	err = c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		if err := c.syncHumanStop(ctx, tx, now, cat); err != nil {
			return err
		}
		m, err := loadManagerSeat(ctx, tx, true)
		if err != nil || m == nil {
			return err
		}
		if m.has("incident") && m.DecideUntil != nil && !now.Before(*m.DecideUntil) && m.Holder != "" {
			if err := c.retireHolder(ctx, tx, now, cat, m, "", "system"); err != nil {
				return err
			}
			if _, err := c.shiftManager(ctx, tx, now, cat, m, "", m.without("incident"), "seat.vacated.v1", "decide_within", "system"); err != nil {
				return err
			}
			if err := c.noticeManagerChanged(ctx, tx, now, cat, m, "vacated"); err != nil {
				return err
			}
		}
		if err := c.repeatManagerStopped(ctx, tx, now, cat, m); err != nil {
			return err
		}
		return c.managerAbsent(ctx, tx, now, cat, m)
	})
	if err != nil {
		return err
	}
	if err := c.expireControlCards(ctx, cat); err != nil {
		return err
	}
	return c.managerDaily(ctx, cat)
}

// managerAbsent raises or resolves alarm.manager.absent. A farm whose catalog has no role of
// profile farm-manager runs without a manager by design and gets no alarm.
func (c *Core) managerAbsent(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, m *managerSeat) error {
	a := Alarm{Tenant: farmTenant, Name: "manager.absent", Subject: AlarmSubject{Kind: "seat", ID: managerSeatID}, Severity: "warn"}
	absent := false
	if managerRole(cat) != "" {
		absent = m.State != "held" && m.UnheldSince != nil && !m.UnheldSince.After(now.Add(-absentAfter))
		if !absent {
			// A reading waits for a worker of the reader role without an alarm: doctor warns
			// of the missing role (MGR-20).
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM tasks WHERE state = 'submitted' AND record->>'seat' = $1 AND created_at <= $2 AND type <> $3)`,
				managerSeatID, now.Add(-absentAfter), inspectType).Scan(&absent); err != nil {
				return err
			}
		}
	}
	if absent {
		return c.fireAlarm(ctx, tx, now, a)
	}
	return c.resolveAlarm(ctx, tx, now, a)
}

// noticeManagerChanged sends manager_holder_changed from a code template to the admins of every
// active tenant: what happened, the new holder identity, the epoch and the time; never an
// incident class, an incident tenant or model text (MGR-31).
func (c *Core) noticeManagerChanged(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, m *managerSeat, what string) error {
	holder := m.Holder
	if holder == "" {
		holder = "-"
	}
	params := map[string]any{"what": what, "holder": holder, "epoch": m.Epoch, "at": fmtTime(now)}
	return c.noticeAdmins(ctx, tx, now, cat, slices.Sorted(mapKeys(cat.Tenants)), "manager_holder_changed", "notice.manager_holder_changed", params,
		"admin:people", "admin:governance")
}

// noticeManagerStopped sends manager_stopped to the operators with admin:governance in farm,
// without buttons: the class, number and tenant of the incident that stopped the seat, the stop
// time, the incidents opened during the stop, the decide_within expiry and the ready farmctl
// commands from code (MGR-35, PRM-14). key is the first card or a repeat (MGR-36).
func (c *Core) noticeManagerStopped(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, m *managerSeat, key string) error {
	r := m.reason("incident")
	if r == nil {
		return nil
	}
	var inc struct{ tenant, class string }
	if err := tx.QueryRow(ctx, `SELECT tenant, class FROM incidents WHERE id = $1`, r.Incident).Scan(&inc.tenant, &inc.class); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	at, _ := time.Parse(time.RFC3339Nano, r.At)
	rows, err := tx.Query(ctx, `SELECT id FROM incidents WHERE stops_manager AND opened_at >= $1 AND id <> $2 ORDER BY opened_at, id LIMIT 20`, at, r.Incident)
	if err != nil {
		return err
	}
	joined, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	list := c.texts.Render("en", "notice.manager_stopped.none", nil)
	if len(joined) > 0 {
		list = strings.Join(joined, ", ")
	}
	until := "-"
	if m.DecideUntil != nil {
		until = fmtTime(*m.DecideUntil)
	}
	return c.noticeAdmins(ctx, tx, now, cat, []string{farmTenant}, "manager_stopped", key, map[string]any{
		"incident": r.Incident, "class": inc.class, "tenant": inc.tenant, "at": r.At, "until": until, "joined": cut(list, 500)}, "admin:governance")
}

// stopRepeats are the times of the manager_stopped repeats: 24 h and 72 h after the stop and
// 24 h before the end of decide_within; a repeat at or after the end, or at the time of an
// earlier one, is not sent (MGR-36).
func stopRepeats(at, until time.Time) []time.Time {
	var out []time.Time
	for _, t := range []time.Time{at.Add(24 * time.Hour), at.Add(72 * time.Hour), until.Add(-24 * time.Hour)} {
		if t.Before(until) && t.After(at) && !slices.ContainsFunc(out, t.Equal) {
			out = append(out, t)
		}
	}
	slices.SortFunc(out, func(a, b time.Time) int { return a.Compare(b) })
	return out
}

// repeatManagerStopped repeats manager_stopped while the seat's reason set holds incident; the
// repeats end with that reason, which assign, vacate and resume remove (MGR-36).
func (c *Core) repeatManagerStopped(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, m *managerSeat) error {
	r := m.reason("incident")
	if r == nil || m.Holder == "" || m.DecideUntil == nil {
		return nil
	}
	at, err := time.Parse(time.RFC3339Nano, r.At)
	if err != nil {
		return nil
	}
	due := 0
	for _, t := range stopRepeats(at, *m.DecideUntil) {
		if !t.After(now) {
			due++
		}
	}
	if due <= r.Repeats {
		return nil
	}
	// Repeats the loop missed are not sent one after another: one card covers them.
	r.Repeats = due
	if err := m.save(ctx, tx); err != nil {
		return err
	}
	return c.noticeManagerStopped(ctx, tx, now, cat, m, "notice.manager_stopped_repeat")
}

// noticeAdmins plans a code notice to the people of the tenants with one of the capabilities, in
// their private chats.
func (c *Core) noticeAdmins(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, tenants []string, kind, textKey string, params map[string]any, caps ...string) error {
	sent := 0
	for _, t := range tenants {
		tv, ok := cat.Tenants[t]
		if !ok || tv.Spec.Kind == "" {
			continue
		}
		chats, err := c.adminChats(ctx, tx, now, tv, "", caps...)
		if err != nil {
			return err
		}
		for _, a := range chats {
			if _, err := c.planDelivery(ctx, tx, now, plannedDelivery{
				Tenant: t, Kind: kind, Addr: "admins", TargetKind: "chat",
				TargetID: Chat{Kind: "direct", ExternalID: a.ExternalID}.ID(), ExecKind: "channel", ExecName: a.Channel,
				Recipient: a.Principal, DataOrigin: "code",
				Body: map[string]any{"form": "text_code", "text_key": textKey, "params": params},
			}); err != nil {
				return err
			}
			sent++
		}
	}
	if sent == 0 {
		// A stop that reaches nobody still shows as alarm.manager.absent after absentAfter.
		c.log.Error("a seat notice has no recipient: no operator of farm or admin has a private chat; declare tenant farm with an operator group and a channel",
			"kind", kind)
	}
	return nil
}
