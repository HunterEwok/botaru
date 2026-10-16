// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/manifest"
)

// kindTo is how many deliveries of a kind went to a person.
func kindTo(t *testing.T, b *bench, kind, recipient string) int {
	t.Helper()
	var n int
	if err := b.c.st.Pool.QueryRow(context.Background(), `SELECT count(*) FROM deliveries WHERE kind = $1 AND recipient = $2`, kind, recipient).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// From v0.3 an operator of farm marks manager_preventable read with incident.ack on the incident
// of the restriction, and the repeats stop while the restriction stays; seat.resume needs the
// tenant; a stop reason incident left by v0.2 goes without a manager_stopped repeat; the seat
// record and doctor show restricted_in (GOV-47, MGR-47, MGR-55).
func TestPreventableReadV03(t *testing.T) {
	ctx := context.Background()
	b := newManagerBench(t)
	withV03(t)
	op := b.farmOperator(t)
	if _, err := b.c.SeatAssign(ctx, b.farmOp(), SeatRequest{Seat: managerSeatID}); err != nil {
		t.Fatal(err)
	}
	holder := b.managerSeat(t).Holder
	b.exec(t, `INSERT INTO manager_restrictions (tenant, incident, holder, since) VALUES ('demo', 'inc_01J9Z3M2Q8YB6V4N7T5K0C2D1E', $1, botaru_now() - interval '25 hours')`, holder)
	b.loop(t, "identity")
	if n := kindTo(t, b, preventableNotice, op); n != 1 {
		t.Fatalf("the 24 h repeat: %d", n)
	}
	// The seat record carries restricted_in, and doctor shows it for the tenant (MGR-47).
	var rec map[string]any
	if err := b.c.tx(ctx, func(tx pgx.Tx, _ time.Time) error {
		var err error
		rec, err = b.c.managerSeatRecord(ctx, tx)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	list, _ := rec["restricted_in"].([]any)
	if len(list) != 1 || asMap(list[0])["tenant"] != "demo" || rec["state"] != "held" {
		t.Fatalf("seat record %v", rec)
	}
	doc, err := b.c.Doctor(ctx, b.op, TenantRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if i := slices.IndexFunc(doc.Checks, func(c DoctorCheck) bool { return c.ID == "manager.restricted_since" }); i < 0 || doc.Checks[i].OK || doc.Checks[i].Value == nil {
		t.Fatalf("doctor %+v", doc.Checks)
	}
	// A tenant operator's incident.ack is an ordinary one: the incident is not of its tenant.
	if _, err := b.c.IncidentAck(ctx, b.op, IncidentRequest{ID: "inc_01J9Z3M2Q8YB6V4N7T5K0C2D1E"}); err == nil {
		t.Fatal("a tenant operator marked the farm notice read")
	}
	d, err := b.c.IncidentAck(ctx, b.farmOp(), IncidentRequest{ID: "inc_01j9z3m2q8yb6v4n7t5k0c2d1e"})
	if err != nil || !d.OK || d.AuditSeq == 0 {
		t.Fatalf("marked read %+v %v", d, err)
	}
	if again, err := b.c.IncidentAck(ctx, b.farmOp(), IncidentRequest{ID: "inc_01J9Z3M2Q8YB6V4N7T5K0C2D1E"}); err != nil || again.AuditSeq != 0 {
		t.Fatalf("marked read twice %+v %v", again, err)
	}
	b.advance(t, 72*3600)
	b.loop(t, "identity")
	if n := kindTo(t, b, preventableNotice, op); n != 1 {
		t.Fatalf("a repeat after the read mark: %d", n)
	}
	if !restricted(t, b, "demo", "") {
		t.Fatal("the read mark lifted the restriction")
	}
	// A v0.3 core requires the tenant on seat.resume.
	_, err = b.c.SeatResume(ctx, b.farmOp(), SeatResumeRequest{Seat: managerSeatID, Incident: "inc_01J9Z3M2Q8YB6V4N7T5K0C2D1E"})
	wantReason(t, err, "input_schema_invalid")
	// A stop reason incident from a v0.2 core goes with the next loop pass, without a
	// manager_stopped repeat and without retiring the holder.
	epoch := b.managerSeat(t).Epoch
	b.exec(t, `UPDATE seats SET state = 'stopped', decide_until = botaru_now() - interval '1 hour',
		stop_reasons = jsonb_build_array(jsonb_build_object('reason', 'incident', 'at', to_char((botaru_now() - interval '8 days') AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'), 'incident', 'inc_01J9Z3M2Q8YB6V4N7T5K0C2D1F'))
		WHERE tenant = 'farm' AND id = $1`, managerSeatID)
	b.loop(t, "identity")
	m := b.managerSeat(t)
	if m.State != "held" || m.Holder != holder || m.has("incident") || m.Epoch != epoch+1 {
		t.Fatalf("seat after the v0.2 reason %+v", m)
	}
	if n := kindTo(t, b, "manager_stopped", op); n != 0 {
		t.Fatalf("manager_stopped from v0.3: %d", n)
	}
}

// From v0.3 every takeover counts in farm.takeover.count, a series without an outcome block, and
// the lease, task and event points of a takeover task stay out of the trial computations of the
// role (MGR-48, PRP-22).
func TestTakeoverSeriesV03(t *testing.T) {
	ctx := context.Background()
	b := newManagerBench(t)
	cat, err := b.c.Catalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := b.c.tenantSeries(cat, "demo")[seriesTakeoverCount]; ok {
		t.Fatal("farm.takeover.count before v0.3")
	}
	withV03(t)
	d, ok := b.c.tenantSeries(cat, "demo")[seriesTakeoverCount]
	if !ok || d.Outcome != nil || d.CatalogKind != "task_type" {
		t.Fatalf("series %+v %v", d, ok)
	}
	if err := b.c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		for range 2 {
			if err := takeoverPoint(ctx, tx, now, "demo", "hello.greet.v1", "hello", "tko_x"); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM obs_points WHERE series = $1 AND subject = 'task_type:hello.greet.v1' AND role = 'hello' AND num = 1`,
		seriesTakeoverCount).Scan(&n); err != nil || n != 1 {
		t.Fatalf("takeover points %d %v", n, err)
	}
	took, other := b.hello(t, "u1"), b.hello(t, "u2")
	b.exec(t, `UPDATE tasks SET record = record || jsonb_build_object('takeover_of', $2::text) WHERE id = $1`, took, other)
	b.exec(t, `INSERT INTO events (id, tenant, type, source, time, data) VALUES ('evt_take', 'demo', $1, 'core', botaru_now(), jsonb_build_object('task', $2::text))`,
		runFinishedType, took)
	for _, key := range []string{"task:" + took, "lease:" + took + ":1", "event:evt_take", "task:" + other} {
		b.exec(t, `INSERT INTO obs_points (tenant, series, subject, at, num, source, data_mark, key, created_at)
			VALUES ('demo', 'farm.task.attempts', 'task_type:hello.greet.v1', botaru_now() - interval '1 minute', 2, 'core', 'internal', $1, botaru_now())`, key)
	}
	s := seriesDef{Name: "farm.task.attempts", Outcome: &seriesOutcome{Compare: "window.mean"}}
	if err := b.c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		v, err := trialValue(ctx, tx, "demo", s, "task_type:hello.greet.v1", now.Add(-time.Hour), now.Add(time.Second), "", "")
		if err == nil && v["samples"] != 1 {
			t.Errorf("trial samples %v, want only the point of the other task", v)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// attention_notice from v0.3 passes the closed skeleton schema as well as the core's tool schema,
// and quiet hours defer manager_attention (never the digest), as they never defer a decision
// card (MGR-52, DLV-10).
func TestAttentionClosedQuietV03(t *testing.T) {
	ctx := context.Background()
	b := newManagerBench(t)
	ok := map[string]any{"sees": "disk_growth", "object": "t_01J9Z3M2Q8YB6V4N7T5K0C2D1E", "threat": "disk_full_soon", "proposal": map[string]any{"op": "hold"}}
	if !b.c.attentionChecked(ok) {
		t.Fatal("a valid notice")
	}
	for _, op := range []string{"notify", "call the admin", "rm -rf"} {
		bad := map[string]any{"sees": "disk_growth", "object": "t_01J9Z3M2Q8YB6V4N7T5K0C2D1E", "threat": "disk_full_soon", "proposal": map[string]any{"op": op}}
		if b.c.attentionChecked(bad) {
			t.Fatalf("op %q passed the closed list", op)
		}
	}
	if quietOnlyKinds() != nil {
		t.Fatal("quiet hours for manager_attention before v0.3")
	}
	withV03(t)
	cat, err := b.c.Catalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	loc := tenantLocation(cat, "demo")
	now := b.now(t).In(loc)
	quiet := map[string]any{"from": now.Add(-time.Hour).Format("15:04"), "to": now.Add(time.Hour).Format("15:04")}
	seq := 0
	defers := func(kind string, prefs map[string]any) bool {
		raw, _ := json.Marshal(prefs)
		b.exec(t, `UPDATE principals SET prefs = $2 WHERE id = $1`, b.alice, raw)
		seq++
		id := "dl_quiet" + string(rune('a'+seq))
		b.exec(t, `INSERT INTO deliveries (id, tenant, kind, addr, target_kind, target_id, exec_kind, exec_name, recipient, state, dataorigin, created_at, updated_at)
			VALUES ($1, 'demo', $2, 'admins', 'chat', 'direct:1001', 'channel', 'telegram', $3, 'planned', 'code', botaru_now(), botaru_now())`, id, kind, b.alice)
		var wait bool
		if err := b.c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
			var err error
			wait, err = b.c.prefsDefer(ctx, tx, now, cat, "demo", id, kind, b.alice)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return wait
	}
	if !defers(attentionNotice, map[string]any{"quiet_hours": quiet}) {
		t.Fatal("manager_attention in quiet hours went out")
	}
	if defers(attentionNotice, map[string]any{"digest": "daily", "quiet_hours": map[string]any{"from": now.Add(2 * time.Hour).Format("15:04"), "to": now.Add(3 * time.Hour).Format("15:04")}}) {
		t.Fatal("manager_attention collected into the digest")
	}
	if defers("approval_card", map[string]any{"quiet_hours": quiet}) {
		t.Fatal("quiet hours deferred a decision card")
	}
}

// sev2Postmortem makes disk_full a sev2 class, so the analysis gets a model paragraph only for
// link candidates or a person's request (MGR-53).
func sev2Postmortem(ms []map[string]any) {
	for _, m := range ms {
		if m["kind"] == "IncidentClass" && asMap(m["metadata"])["name"] == "disk_full" {
			asMap(m["spec"])["severity"] = "sev2"
		}
	}
}

// A person asks for the model paragraph of an analysis before the incident resolves, or while the
// analysis waits; afterwards it is too late, and with reading off there is none. Link candidates
// of every rule (GOV-13, GOV-38, TRN-7) count for a paragraph, and the analysis input never has
// them (MGR-53, MGR-54).
func TestPostmortemRequestV03(t *testing.T) {
	ctx := context.Background()
	b := newManagerBenchWith(t, nil, []map[string]any{postmortemDoc()}, claimPostmortem, sev2Postmortem)
	withV03(t)
	b.diskAlarm(t, true, 3)
	b.advance(t, 6*60)
	b.loop(t, "incident")
	list := incidentsOf(t, b, "disk_full")
	if len(list) != 1 {
		t.Fatalf("incidents %+v", list)
	}
	inc := list[0]
	cat, err := b.c.Catalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tv := cat.Tenants["demo"]
	ask := func(tv *TenantView) string {
		var code string
		if err := b.c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
			var err error
			_, code, err = b.c.paragraphAsked(ctx, tx, now, tv, b.alice, inc.ID)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return code
	}
	off := *tv
	off.Spec.Privacy.Purposes = nil
	if code := ask(&off); code != "reading_off" {
		t.Fatalf("reading off: %s", code)
	}
	if code := ask(tv); code != "requested" {
		t.Fatalf("before resolved: %s", code)
	}
	// A candidate of the rejected proposal rule, which the input must never show.
	b.exec(t, `INSERT INTO attributions (id, tenant, incident, proposal, state, record, created_at, updated_at, kind)
		VALUES ('att_canary01', 'demo', $1, 'prp_x', 'candidate', '{}', botaru_now(), botaru_now(), 'rejected_proposal')`, inc.ID)
	b.diskAlarm(t, false, 50)
	b.advance(t, 11*60)
	b.loop(t, "incident")
	b.advance(t, 11*60)
	b.loop(t, "incident")
	var task string
	var body, rec []byte
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT id, body, record FROM tasks WHERE type = $1 AND idempotency_key = $2`, postmortemType, "system:postmortem:"+inc.ID).
		Scan(&task, &body, &rec); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rec), `"paragraph": true`) || strings.Contains(string(body), "att_canary01") || strings.Contains(string(body), "candidate") {
		t.Fatalf("analysis record %s body %s", rec, body)
	}
	// While the analysis waits a request sets the paragraph; once it runs it is late.
	b.exec(t, `UPDATE tasks SET record = record || '{"paragraph": false}', body = jsonb_set(body, '{params,input,paragraph}', 'false') WHERE id = $1`, task)
	if code := ask(tv); code != "requested" {
		t.Fatalf("while waiting: %s", code)
	}
	var param bool
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT (body->'params'->'input'->>'paragraph')::boolean FROM tasks WHERE id = $1`, task).Scan(&param); err != nil || !param {
		t.Fatalf("input paragraph %v %v", param, err)
	}
	b.exec(t, `UPDATE tasks SET state = 'working', record = record || '{"paragraph": false}' WHERE id = $1`, task)
	if code := ask(tv); code != "late" {
		t.Fatalf("after the start: %s", code)
	}
	// Every rule's candidates count (MGR-54).
	b.exec(t, `INSERT INTO attributions (id, tenant, incident, script_run, state, record, created_at, updated_at, kind)
		VALUES ('att_script01', 'demo', $1, 'scr_x', 'candidate', '{}', botaru_now(), botaru_now(), 'script_run')`, inc.ID)
	if err := b.c.tx(ctx, func(tx pgx.Tx, _ time.Time) error {
		links, err := linkCandidates(ctx, tx, "demo", inc.ID)
		if err == nil && !slices.Equal(links, []string{"att_canary01", "att_script01"}) {
			t.Errorf("candidates %v", links)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// The weekly holder report goes once a week to the farm operators with numbers only, and the
// daily summary is sent even for an empty day (MGR-50).
func TestHolderReportV03(t *testing.T) {
	ctx := context.Background()
	b := newManagerBench(t)
	withV03(t)
	op := b.farmOperator(t)
	b.loop(t, "identity")
	b.loop(t, "identity")
	if n := kindTo(t, b, holderReport, op); n != 1 {
		t.Fatalf("holder reports %d", n)
	}
	var body []byte
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT body FROM deliveries WHERE kind = $1 AND recipient = $2`, holderReport, op).Scan(&body); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "demo") || !strings.Contains(string(body), "notice.holder_report_v03") || !strings.Contains(string(body), "manager_against") {
		t.Fatalf("holder report %s", body)
	}
	var daily int
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM deliveries WHERE kind = 'manager_daily' AND body->>'text_key' IN ('notice.manager_daily_v03', 'notice.manager_daily_farm_v03')`).
		Scan(&daily); err != nil || daily == 0 {
		t.Fatalf("daily summaries of an empty day %d %v", daily, err)
	}
}

// The manager's proposals carry origin level 2, so they never make anyone answer for a link and
// give no rejected-proposal candidate; the reviewer input has no author level; at most 2 a month
// per tenant (MGR-51, GOV-38, GOV-41, COM-15).
func TestManagerProposalsV03(t *testing.T) {
	ctx := context.Background()
	if authorLevel(true, "manage") != 2 || authorLevel(false, "aggregate") != 1 || authorLevel(false, "insight") != 0 {
		t.Fatal("author levels")
	}
	p := &proposalRow{Rec: map[string]any{"origin": map[string]any{"level": 2, "seat": managerSeatID}, "body": map[string]any{"category": "schedule"}}}
	if in := reviewInput(p, true); in["author_level"] != nil || in["seat"] != nil || in["origin"] != nil {
		t.Fatalf("reviewer input %v", in)
	}
	b := newManagerBench(t)
	withV03(t)
	if !managerProposer(reviewType) || !managerProposer(postmortemType) || managerProposer(incidentRespondType) {
		t.Fatal("which seat tasks propose")
	}
	cat, err := b.c.Catalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	limited := func() bool {
		var l bool
		if err := b.c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
			var err error
			l, err = managerProposalLimited(ctx, tx, now, cat, "demo")
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return l
	}
	for i, id := range []string{"prp_m1", "prp_m2"} {
		if limited() {
			t.Fatalf("limited after %d", i)
		}
		b.exec(t, `INSERT INTO proposals (id, tenant, role, worker, task, fingerprint, kind, state, record, created_at, updated_at, state_at)
			VALUES ($1, 'demo', 'farm-manager', 'wk_x', 't_x', $1, 'manifest', 'screened', '{"origin": {"seat": "farm-manager", "level": 2}}', botaru_now(), botaru_now(), botaru_now())`, id)
	}
	if !limited() {
		t.Fatal("a third proposal in the month")
	}
}

// A v0.3 core accepts stops_manager and incident_stop and does not use them: apply warns, the
// lint of store_low no longer refuses stops_manager, and stops_manager does not answer for the
// manager (MGR-55, GOV-48).
func TestIgnoredV02FieldsV03(t *testing.T) {
	class := manifest.Doc{Kind: "IncidentClass", Name: "spend_storm"}
	charter := manifest.Doc{Kind: "Governance", Tenant: farmTenant, Name: "farm"}
	cv := map[string]any{"spec": map[string]any{"stops_manager": true}}
	gv := map[string]any{"spec": map[string]any{"farm_manager": map[string]any{"incident_stop": map[string]any{"decide_within": "3d"}}}}
	setClaimsV03(t, false)
	if ignoredV03(class, cv, 0) != nil {
		t.Fatal("a warning before v0.3")
	}
	c := lintCore(t)
	storeLow := `apiVersion: botaru/v1alpha1
kind: IncidentClass
metadata: {name: store_low}
spec:
  title: Low space
  severity: sev1
  stops_manager: true
  detect: {alarm: alarm.store.free.low.v1}
  subject: {kind: farm}
`
	if codes := lintCodes(t, c, storeLow); !slices.Contains(codes, "incident_store_low") {
		t.Fatalf("v0.1 lint %v", codes)
	}
	setClaimsV03(t, true)
	w := ignoredV03(class, cv, 3)
	if len(w) != 1 || w[0].Code != "version_unsupported" || w[0].Path != "/manifests/3/spec/stops_manager" {
		t.Fatalf("class warning %+v", w)
	}
	if w := ignoredV03(charter, gv, 1); len(w) != 1 || w[0].Path != "/manifests/1/spec/farm_manager/incident_stop" {
		t.Fatalf("charter warning %+v", w)
	}
	if w := ignoredV03(manifest.Doc{Kind: "Governance", Tenant: "demo"}, gv, 1); w != nil {
		t.Fatalf("a tenant charter %+v", w)
	}
	if codes := lintCodes(t, c, storeLow); slices.Contains(codes, "incident_store_low") {
		t.Fatalf("v0.3 lint %v", codes)
	}
	if managerClass(map[string]any{"stops_manager": true}) || !managerClass(map[string]any{"manager_accountable": true}) {
		t.Fatal("managerClass v0.3")
	}
}
