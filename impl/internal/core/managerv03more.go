// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/jcs"
	"github.com/HunterEwok/botaru/impl/internal/manifest"
)

// The rest of the farm manager rules of v0.3 (GOV-47, MGR-47, MGR-48, MGR-51, MGR-52, MGR-53,
// MGR-55): the read mark of manager_preventable, the end of the v0.2 incident stop, the field
// restricted_in of the seat record, the series farm.takeover.count, the author level of a seat
// proposal, a person's request for an analysis paragraph and the v0.2 fields a v0.3 core does
// not use.

// seriesTakeoverCount counts the takeovers by the functionary role and the task type of the
// original task; input for people, never a trial metric: it has no outcome block (MGR-48).
const seriesTakeoverCount = "farm.takeover.count"

// preventableRead is P2 incident.ack with the operator credential of farm on the incident of a
// restriction still in force: it marks the notice manager_preventable read, so its repeats stop;
// the restriction stays until seat.resume, a new holder or a vacated seat (GOV-47). The incident
// itself, a record of its tenant, is not acked: farm gets no tenant content. ok false means no
// such restriction, and the call goes on as an ordinary incident.ack in farm.
func (c *Core) preventableRead(ctx context.Context, op Operator, req IncidentRequest) (*Done, bool, error) {
	id := req.target()
	if id == "" {
		return nil, false, nil
	}
	var out Done
	found := false
	err := c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		var tenant, incident string
		var read *time.Time
		err := tx.QueryRow(ctx, `SELECT tenant, incident, read_at FROM manager_restrictions
			WHERE lower(incident) = lower($1) AND until IS NULL FOR UPDATE`, id).Scan(&tenant, &incident, &read)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		found = true
		// The answer names the seat and the read mark only: the tenant's incident and its state
		// are not farm's to see.
		out = Done{OK: true, ID: managerSeatID, State: "read"}
		if read != nil {
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE manager_restrictions SET read_at = $2, read_by = $3 WHERE tenant = $1 AND until IS NULL`, tenant, now, op.Principal); err != nil {
			return err
		}
		seq, err := c.appendAudit(ctx, tx, now, Audit{
			Tenant: farmTenant, Kind: "command", Actor: Actor{Principal: principalOr(op.Principal), Via: "farmctl"},
			Command: "incident.ack", Target: &Target{Kind: "seat", ID: managerSeatID}, OK: true,
			After: withTenant(map[string]any{"notice": preventableNotice, "read": true, "incident": incident}, tenant),
		})
		out.AuditSeq = seq
		return err
	})
	if err != nil {
		return nil, false, err
	}
	if !found {
		return nil, false, nil
	}
	return &out, true, nil
}

// dropIncidentStop removes a stop reason incident a v0.2 core left on the seat: from v0.3 the
// seat has only the reasons human and no_control, decide_within no longer retires the holder and
// manager_stopped is not repeated (GOV-47, MGR-55). The change moves the seat like any other.
func (c *Core) dropIncidentStop(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, m *managerSeat) error {
	if !m.has("incident") {
		return nil
	}
	// The seat leaves the stop: resumed, unless another reason still stops it.
	reasons := m.without("incident")
	event := "seat.resumed.v1"
	if len(reasons) > 0 && m.Holder != "" {
		event = "seat.stopped.v1"
	}
	if _, err := c.shiftManager(ctx, tx, now, cat, m, m.Holder, reasons, event, "incident_stop_ignored", "system"); err != nil {
		return err
	}
	// The manager_stopped cards that the earlier core planned (a quiet hour, the card limit) are
	// about a stop that no longer holds: they are withheld with the event delivery.withheld, as
	// every withheld delivery. A card already sending stays: the channel may have sent it, and its
	// outcome comes back on its lease, so withholding it would contradict what was sent.
	rows, err := tx.Query(ctx, `UPDATE deliveries SET state = 'withheld', reason = 'superseded', updated_at = $1
		WHERE kind = 'manager_stopped' AND state = 'planned' RETURNING id, tenant`, now)
	if err != nil {
		return err
	}
	type withheld struct{ id, tenant string }
	ws, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (withheld, error) {
		var w withheld
		return w, r.Scan(&w.id, &w.tenant)
	})
	if err != nil {
		return err
	}
	slices.SortFunc(ws, func(a, b withheld) int { return strings.Compare(a.id, b.id) })
	for _, w := range ws {
		if err := c.appendEvent(ctx, tx, now, w.tenant, Event{Type: "delivery.withheld", Subject: w.id,
			Data: map[string]any{"delivery": w.id, "kind": "manager_stopped", "reason": "superseded"}}); err != nil {
			return err
		}
	}
	// A holder's seat back in held is told to the admins as seat.resume tells them (MGR-31).
	if m.State != "held" {
		return nil
	}
	return c.noticeManagerChanged(ctx, tx, now, cat, m, "resumed")
}

// managerSeatRecord is the record of the farm manager seat by spec/model/seat.schema.json: the
// holder, epoch, state and stop reasons and, from v0.3, restricted_in, the tenants where the
// seat is in restricted mode with the incident and the time (MGR-47).
func (c *Core) managerSeatRecord(ctx context.Context, tx pgx.Tx) (map[string]any, error) {
	m, err := loadManagerSeat(ctx, tx, false)
	if err != nil || m == nil {
		return nil, err
	}
	var holder any
	if m.Holder != "" {
		holder = m.Holder
	}
	reasons := []any{}
	for _, r := range m.Reasons {
		e := map[string]any{"reason": r.Reason, "at": r.At}
		if r.Incident != "" {
			e["incident"] = r.Incident
		}
		reasons = append(reasons, e)
	}
	rec := withTenant(map[string]any{"id": managerSeatID, "kind": "farm_manager", "holder": holder, "epoch": m.Epoch, "state": m.State,
		"stop_reasons": reasons}, farmTenant)
	if m.DecideUntil != nil {
		rec["decide_until"] = fmtTime(*m.DecideUntil)
	}
	rows, err := tx.Query(ctx, `SELECT tenant, incident, since FROM manager_restrictions WHERE until IS NULL ORDER BY tenant LIMIT 1000`)
	if err != nil {
		return nil, err
	}
	list, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (any, error) {
		var tenant, incident string
		var since time.Time
		err := row.Scan(&tenant, &incident, &since)
		return withTenant(map[string]any{"incident": incident, "since": fmtTime(since)}, tenant), err
	})
	if err != nil {
		return nil, err
	}
	if list == nil {
		list = []any{}
	}
	rec["restricted_in"] = list
	if err := c.spec.Validate("urn:botaru:schema:seat:1", rec); err != nil {
		return nil, err
	}
	return rec, nil
}

// doctorRestricted shows from the seat record whether the farm manager seat is in restricted
// mode in the tenant: the time since when, or null (MGR-47).
func (c *Core) doctorRestricted(ctx context.Context, tx pgx.Tx, tenant string, add func(DoctorCheck)) error {
	rec, err := c.managerSeatRecord(ctx, tx)
	if err != nil || rec == nil {
		return err
	}
	ch := DoctorCheck{ID: "manager.restricted_since", OK: true, Severity: "info", Value: nil, Tenant: tenant}
	list, _ := rec["restricted_in"].([]any)
	for _, e := range list {
		if em := asMap(e); em["tenant"] == tenant {
			ch.Value, ch.OK, ch.Severity = em["since"], false, "warn"
		}
	}
	add(ch)
	return nil
}

// takeoverSeries is the definition of farm.takeover.count for a tenant from v0.3 (MGR-48).
func takeoverSeries() seriesDef {
	return seriesDef{Name: seriesTakeoverCount, CatalogKind: "task_type", DataClass: coreSeriesMark, Source: "core",
		RawDays: obsRawDays, RollDays: obsRollupDays, Rollups: []string{"day.max", "day.p95"}}
}

// takeoverPoint counts one takeover in farm.takeover.count on the task type of the original task,
// with the functionary role that runs the new task; the takeover id keys it, so it is written
// once (MGR-48).
func takeoverPoint(ctx context.Context, tx pgx.Tx, now time.Time, tenant, typ, role, takeover string) error {
	return writePoint(ctx, tx, now, obsPoint{Tenant: tenant, Series: seriesTakeoverCount, Subject: "task_type:" + typ, Source: "core",
		Mark: coreSeriesMark, Role: role, At: now, Num: num(1), Key: "takeover:" + takeover})
}

// notTakeoverPoints leaves out of a trial computation every point of an attempt on the seat
// model: the points keyed by the lease, by the task and by a run.finished event of a takeover
// task (MGR-48, PRP-22). $1 is the tenant.
const notTakeoverPoints = ` AND NOT (key LIKE 'lease:%' AND split_part(key, ':', 2) IN (SELECT id FROM tasks WHERE tenant = $1 AND record ? 'takeover_of'))` +
	` AND NOT (key LIKE 'task:%' AND substr(key, 6) IN (SELECT id FROM tasks WHERE tenant = $1 AND record ? 'takeover_of'))` +
	` AND NOT (key LIKE 'event:%' AND substr(key, 7) IN (SELECT e.id FROM tasks t JOIN events e ON e.tenant = t.tenant AND e.data->>'task' = t.id
		AND e.type = '` + runFinishedType + `' WHERE t.tenant = $1 AND t.record ? 'takeover_of'))`

// authorLevel is origin.level of a proposal: 2 for the farm manager seat (MGR-41, MGR-51), so its
// proposals never make anyone answer for a link and give no rejected-proposal candidate (GOV-38,
// GOV-41); 1 for an aggregator; 0 for a bot of a role.
func authorLevel(seat bool, purpose string) int {
	switch {
	case seat:
		return 2
	case purpose == "aggregate":
		return 1
	}
	return 0
}

// quietOnlyKinds are the notices that the person's quiet hours defer although their kind is not
// deferrable in delivery-kinds.yaml: the manager's notice to people is not urgent, and quiet
// hours apply to it (MGR-52). The daily digest does not collect it.
func quietOnlyKinds() []string {
	return []string{attentionNotice}
}

// attentionChecked checks the arguments of attention_notice by the skeleton schema too, whose
// operation list is closed: an argument the core's own narrower schema would let through can
// never reach people (MGR-52).
func (c *Core) attentionChecked(args map[string]any) bool {
	return c.spec.Validate("urn:botaru:schema:attention-notice:1", args) == nil
}

// paragraphRecent says whether an analysis of the same class and object got a model paragraph in
// the last 7 days, other than the task except (MGR-53).
func paragraphRecent(ctx context.Context, tx pgx.Tx, now time.Time, r *incidentRow, except string) (bool, error) {
	var recent bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM tasks t JOIN incidents i ON i.id = t.record->>'postmortem'
		WHERE t.tenant = $1 AND t.type = $2 AND (t.record->>'paragraph')::boolean AND t.created_at > $3 AND i.class = $4 AND i.subject_kind = $5 AND i.subject_id = $6
		AND t.id <> $7)`,
		r.Tenant, postmortemType, now.Add(-paragraphEvery), r.Class, r.SubjectKind, r.SubjectID, except).Scan(&recent)
	return recent, err
}

// paragraphAsked is a person's request for the model paragraph of an incident's analysis, from
// the channel command /incident analyse <number> by a person with incident.manage (MGR-53). Before
// the incident resolves the request is kept in its record and the analysis gets the paragraph;
// an analysis that waits for its worker gets it at once; once it started or ended it is too late.
// With incident_review off, or a paragraph of the same class and object in 7 days, there is none.
// It returns the answer code: requested, reading_off, recent or late.
func (c *Core) paragraphAsked(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, tv *TenantView, person, ref string) (string, string, error) {
	r, err := loadIncident(ctx, tx, tv.Name, ref, true)
	if err != nil {
		return "", "", err
	}
	if r == nil {
		return "", "", refuse("not_found", "no such incident in the tenant")
	}
	// The tenant gets no analysis at all: nothing to ask for (the conditions of enqueuePostmortem).
	if !postmortemOn(cat, tv.Name) {
		return r.ID, "no_analysis", nil
	}
	if !incidentReviewOn(tv) {
		return r.ID, "reading_off", nil
	}
	var task, state string
	var paragraph bool
	err = tx.QueryRow(ctx, `SELECT id, state, coalesce((record->>'paragraph')::boolean, false) FROM tasks WHERE tenant = $1 AND idempotency_key = $2 FOR UPDATE`,
		tv.Name, "system:postmortem:"+r.ID).Scan(&task, &state, &paragraph)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		if r.State == "resolved" || r.State == "closed" || r.State == "dismissed" {
			return r.ID, "late", nil
		}
	case err != nil:
		return "", "", err
	case paragraph:
		return r.ID, "requested", nil
	case state != "submitted":
		return r.ID, "late", nil
	}
	if recent, err := paragraphRecent(ctx, tx, now, r, task); err != nil {
		return "", "", err
	} else if recent {
		return r.ID, "recent", nil
	}
	if task == "" {
		// Beside the record, which has no such field, so no incident view shows it.
		if _, err := tx.Exec(ctx, `UPDATE incidents SET paragraph_requested_at = $2, paragraph_requested_by = $3 WHERE id = $1`, r.ID, now, person); err != nil {
			return "", "", err
		}
	} else if err := setPostmortemParagraph(ctx, tx, task); err != nil {
		return "", "", err
	}
	_, err = c.appendAudit(ctx, tx, now, Audit{
		Tenant: tv.Name, Kind: "command", Actor: Actor{Principal: person, Via: "channel"}, Command: "incident.analyse",
		Target: &Target{Kind: "incident", ID: r.ID}, OK: true, After: map[string]any{"paragraph": true},
	})
	return r.ID, "requested", err
}

// ignoredV03 are the warnings of apply for the fields of v0.1 and v0.2 that a v0.3 core accepts
// and does not use: stops_manager of an IncidentClass and incident_stop of the farm charter
// (MGR-55, GOV-22, GOV-28). They do not fail the set.
func ignoredV03(d manifest.Doc, v map[string]any, i int) []ApplyError {
	spec, _ := v["spec"].(map[string]any)
	var path string
	switch {
	case d.Kind == "IncidentClass" && spec["stops_manager"] != nil:
		path = "/spec/stops_manager"
	case d.Kind == "Governance" && isFarmCharter(d.Tenant, d.Name) && mmap(spec, "farm_manager")["incident_stop"] != nil:
		path = "/spec/farm_manager/incident_stop"
	default:
		return nil
	}
	return []ApplyError{{Kind: d.Kind, Tenant: d.Tenant, Name: d.Name, Code: "version_unsupported", Path: fmt.Sprintf("/manifests/%d%s", i, path),
		Detail: "a field of v0.2 that a v0.3 core does not use (MGR-55)"}}
}

// paragraphRequested says whether a person asked for the analysis paragraph of an incident
// before it resolved (MGR-53).
func paragraphRequested(ctx context.Context, tx pgx.Tx, incident string) (bool, error) {
	var ok bool
	err := tx.QueryRow(ctx, `SELECT paragraph_requested_at IS NOT NULL FROM incidents WHERE id = $1`, incident).Scan(&ok)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return ok, err
}

// setPostmortemParagraph turns the paragraph on in a waiting analysis: in its record and in the
// input of its body, with the body hash computed again over the new body (MGR-53).
func setPostmortemParagraph(ctx context.Context, tx pgx.Tx, task string) error {
	var raw []byte
	if err := tx.QueryRow(ctx, `SELECT body FROM tasks WHERE id = $1`, task).Scan(&raw); err != nil {
		return err
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		return err
	}
	input := mmap(mmap(body, "params"), "input")
	if input == nil {
		return nil
	}
	input["paragraph"] = true
	hash, err := jcs.Hash(body)
	if err != nil {
		return err
	}
	nb, err := json.Marshal(body)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE tasks SET record = record || '{"paragraph": true}', body = $2, body_hash = $3 WHERE id = $1`, task, nb, hash)
	return err
}

// postmortemOn says whether a resolved incident of the tenant gets an analysis: the conditions of
// enqueuePostmortem (MGR-53).
func postmortemOn(cat *Catalog, tenant string) bool {
	role := managerRole(cat)
	if cat.Tenants[tenant] == nil || !managerEnabled(cat, tenant) || managerCharter(cat)["postmortem"] == false || role == "" ||
		!slices.Contains(cat.Roles[role].Claims, postmortemType) {
		return false
	}
	_, ok := cat.TaskTypes[postmortemType]
	return ok
}

// analyseCommand is /incident analyse <number> (MGR-53).
func (c *Core) analyseCommand(ctx context.Context, tx pgx.Tx, now time.Time, pc personCommand, ref string) (*ActivityResult, error) {
	id, code, err := c.paragraphAsked(ctx, tx, now, pc.cat, pc.tv, pc.person, ref)
	if p, ok := AsProblem(err); ok && p.Reason == "not_found" {
		return pc.reply(c, "cmd.incident.not_found", nil), nil
	}
	if err != nil {
		return nil, err
	}
	return pc.reply(c, "cmd.incident.analyse_"+code, map[string]any{"incident": id}), nil
}
