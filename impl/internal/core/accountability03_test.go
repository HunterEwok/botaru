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

	"github.com/HunterEwok/botaru/impl/internal/ids"
	"github.com/HunterEwok/botaru/impl/internal/manifest"
)

// insertIncident writes an open incident of class about subject kind:id, opened now.
func (b *bench) insertIncident(t *testing.T, class, kind, id string) string {
	t.Helper()
	inc := ids.New(ids.Incident)
	b.exec(t, `INSERT INTO incidents (id, tenant, class, subject_kind, subject_id, alarmkey, episode, episodes, alarmkeys, state, record, since, implicated_at, opened_at, updated_at)
		VALUES ($1, 'demo', $2, $3, $4, $1, 1, ARRAY[1::bigint], ARRAY[$1], 'open', jsonb_build_object('severity', 'sev3', 'alarm', jsonb_build_object('alarmkey', $1::text, 'origin', 'code')), botaru_now(), botaru_now(), botaru_now(), botaru_now())`, inc, class, kind, id)
	return inc
}

// The one dependency step of the catalog (GOV-13 dependency_1): an incident on a worker of the
// role a change targets gets a candidate under dependency_1; an incident on an object that
// depends on nothing changed gets none. P2 attribution.list shows the candidates to people, and
// P2 attribution.create lets a person with governance.attribute link an incident to the change
// while its window is open (rule human, on attribution_card); a link of kind script_run or
// rejected_proposal is found only by code, and a person without the right is refused.
func TestAttributionDependencyAndP2(t *testing.T) {
	ctx := context.Background()
	b, id, _ := realizedBench(t)
	b.exec(t, `UPDATE proposals SET record = jsonb_set(record, '{body,change,manifest_change,ops,0,target}', '{"kind": "Role", "name": "hello"}') WHERE id = $1`, id)
	b.exec(t, `UPDATE incidents SET subject_kind = 'worker', subject_id = lower($1)`, b.agent.ID)
	other := b.insertIncident(t, "tenant_slow", "resource", "web9")
	b.loop(t, "governance")
	alice := Operator{Principal: b.alice, Tenant: "demo"}
	list, err := b.c.AttributionList(ctx, alice, AttributionRequest{})
	if err != nil {
		t.Fatal(err)
	}
	items := list["items"].([]AttributionView)
	if len(items) != 1 || items[0].Rule != "dependency_1" || items[0].Kind != "change" || !items[0].Card || items[0].Proposal != id {
		t.Fatalf("candidates %+v", items)
	}
	b.drain(t)
	if _, err := b.c.AttributionCreate(ctx, b.op, AttributionRequest{Incident: other, Proposal: id}); reasonOf(err) != "forbidden" {
		t.Fatalf("a principal without governance.attribute: %v", err)
	}
	if _, err := b.c.AttributionCreate(ctx, alice, AttributionRequest{Kind: "script_run", Incident: other, ScriptRun: "scr_none"}); reasonOf(err) != "not_found" {
		t.Fatalf("a script link a person makes up: %v", err)
	}
	v, err := b.c.AttributionCreate(ctx, alice, AttributionRequest{Incident: other, Proposal: id})
	if err != nil || !v.Created || v.Rule != "human" || v.CreatedBy != b.alice || v.State != "candidate" || !v.Card {
		t.Fatalf("create %+v %v", v, err)
	}
	cd, _ := b.govCard(t, attributionCard)
	validOut(t, b, "card", cd)
	if cd["id"] != v.ID {
		t.Fatalf("card %v", cd)
	}
	// A second call presents the same candidate and creates nothing.
	if again, err := b.c.AttributionCreate(ctx, alice, AttributionRequest{Incident: other, Proposal: id}); err != nil || again.Created || again.ID != v.ID {
		t.Fatalf("again %+v %v", again, err)
	}
	if n := b.count(t, `SELECT count(*) FROM audit WHERE record->>'command' = 'attribution.create' AND record->'actor'->>'principal' = $1`, b.alice); n != 1 {
		t.Fatalf("create audit %d", n)
	}
}

// linkBench is the v0.3 commission bench whose governors also hold admin:governance and
// incident.manage: they get the weekly summary and record an incident's cause.
func linkBench(t *testing.T) *bench {
	t.Helper()
	setClaimsV03(t, true)
	b := newCommissionBenchWith(t, true, []map[string]any{{
		"apiVersion": "botaru/v1alpha1", "kind": "Group", "metadata": map[string]any{"name": "governors", "tenant": "demo"},
		"spec": map[string]any{"caps": []any{"governance.confirm:commission", "governance.attribute", "admin:governance", "incident.manage"}, "data": []any{"summary", "internal"}},
	}, {
		"apiVersion": "botaru/v1alpha1", "kind": "IncidentClass", "metadata": map[string]any{"name": "tenant_slow"},
		"spec": map[string]any{"title": "Tenant slow", "severity": "sev2", "change_sensitive": true, "problem_codes": []any{"slow"},
			"detect": map[string]any{"alarm": "alarm.disk.free.low.v1"}, "subject": map[string]any{"kind": "tenant"}},
	}, storeLowManifest()}, presentEdit)
	b.exec(t, `INSERT INTO memberships (tenant, principal, grp, state, created_at) VALUES ('demo', $1, 'governors', 'active', now())`, b.alice)
	b.charter(t, 1, nil)
	return b
}

// Links to rejected proposals from v0.3, in shadow (GOV-36, GOV-38, GOV-40, GOV-41, COM-17,
// GOV-46): a change of a task type the commission dismissed, whose problem names the role
// that claims that type, is linked to an incident on the role under rejected_dependency_1; an
// incident a person recorded with an external cause (P2 incident.cause) gets no candidate. Two
// candidates of one incident share one rejection_link_card. In shadow code sends no card, and
// P2 attribution.create presents it to people, who confirm a link for the statistics: the
// record and the audit keep that the farm manager was against the proposal, the holder report
// counts it, and commission.status shows the link under its schema. The weekly commission
// summary goes once a week to the admins with admin:governance. A later external cause cancels
// the open candidates and the departures that wait. The work of a reviewer has no link id and
// no cause (the P4 canary of GOV-46).
func TestRejectionLinksV03(t *testing.T) {
	ctx := context.Background()
	b := linkBench(t)
	alice := Operator{Principal: b.alice, Tenant: "demo"}
	b.helloDone(t, "1", "", "hi")
	b.loop(t, "observe")
	if _, err := b.analysis(t, "1", false, map[string]any{"proposals": []any{changeBody("slow", limitsChange(7))}}); err != nil {
		t.Fatal(err)
	}
	var p1 string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT id FROM proposals`).Scan(&p1); err != nil {
		t.Fatal(err)
	}
	b.advance(t, 60)
	// Dismissed by a veto; the farm manager's review was against it.
	manager := []any{map[string]any{"lens": "farm_manager", "executor": "seat", "state": "done", "result": map[string]any{"lens": "farm_manager", "score": 0, "veto": true}}}
	b.exec(t, `UPDATE proposals SET state = 'dismissed', record = jsonb_set(jsonb_set(record || jsonb_build_object('state', 'dismissed', 'reviews', $2::jsonb, 'verdict',
		jsonb_build_object('outcome', 'dismissed(veto)', 'computed_at', to_char(botaru_now() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'))),
		'{body,change,manifest_change,ops,0,target}', '{"kind": "TaskType", "name": "hello.greet.v1"}'), '{body,problem,subjects}', '["role:hello"]') WHERE id = $1`, p1, manager)
	p2 := ids.New(ids.Proposal)
	b.exec(t, `INSERT INTO proposals SELECT (jsonb_populate_record(NULL::proposals, to_jsonb(p) || jsonb_build_object('id', $2::text, 'record', p.record || jsonb_build_object('id', $2::text)))).*
		FROM proposals p WHERE id = $1`, p1, p2)
	b.advance(t, 86400)
	external := b.insertIncident(t, "tenant_slow", "role", "hello")
	if _, err := b.c.IncidentCause(ctx, b.op, IncidentCauseRequest{Incident: external, Cause: "attack"}); reasonOf(err) != "forbidden" {
		t.Fatalf("a cause from a principal without incident.manage: %v", err)
	}
	if d, err := b.c.IncidentCause(ctx, alice, IncidentCauseRequest{Incident: external, Cause: "external_load"}); err != nil || !d.OK {
		t.Fatalf("cause %+v %v", d, err)
	}
	r, err := scanIncident(b.c.st.Pool.QueryRow(ctx, `SELECT `+incidentCols+` FROM incidents WHERE id = $1`, external))
	if err != nil || r.Record["cause"] != "external_load" || r.Record["cause_by"] != b.alice {
		t.Fatalf("incident %+v %v", r, err)
	}
	validOut(t, b, "urn:botaru:schema:incident:1", r.view())
	b.loop(t, "governance")
	if n := b.count(t, `SELECT count(*) FROM attributions WHERE incident = $1`, external); n != 0 {
		t.Fatalf("candidates of an external cause %d", n)
	}
	b.exec(t, `UPDATE incidents SET state = 'closed' WHERE id = $1`, external)
	inc := b.insertIncident(t, "tenant_slow", "role", "hello")
	b.drain(t)
	b.loop(t, "governance")
	rows, err := b.c.st.Pool.Query(ctx, `SELECT id, proposal, card_of, record->>'rule' FROM attributions WHERE incident = $1 ORDER BY created_at, id`, inc)
	if err != nil {
		t.Fatal(err)
	}
	type link struct{ ID, Proposal, CardOf, Rule string }
	links, err := pgx.CollectRows(rows, pgx.RowToStructByPos[link])
	if err != nil || len(links) != 2 || links[0].CardOf != links[0].ID || links[1].CardOf != links[0].ID || links[0].Rule != "rejected_dependency_1" {
		t.Fatalf("links %+v %v", links, err)
	}
	for _, s := range b.drain(t) {
		if s.kind == rejectionLinkCard {
			t.Fatal("a link card in shadow")
		}
	}
	if n := b.count(t, `SELECT count(*) FROM deliveries WHERE kind = $1`, commissionWeekly); n != 1 {
		t.Fatalf("weekly summaries %d", n)
	}
	// In shadow a person asks for the card through P2 and confirms the second link.
	second := links[1]
	v, err := b.c.AttributionCreate(ctx, alice, AttributionRequest{Kind: "rejected_proposal", Incident: inc, Proposal: second.Proposal})
	if err != nil || v.Created || v.ID != second.ID || !v.Card {
		t.Fatalf("present %+v %v", v, err)
	}
	cd, on := b.govCard(t, rejectionLinkCard)
	validOut(t, b, "card", cd)
	ctxt := cd["context"].(map[string]any)
	if cd["id"] != links[0].ID || len(cd["buttons"].([]any)) != 7 || ctxt["candidate2"] == nil || !strings.Contains(asString(ctxt["candidate1"]), "manager against=true") {
		t.Fatalf("one card for the incident %v", cd)
	}
	var nonce string
	for _, x := range cd["buttons"].([]any) {
		if bm := x.(map[string]any); bm["id"] == second.ID {
			nonce = asString(bm["nonce"])
		}
	}
	yes := DecideRequest{SubjectKind: "attribution", ID: second.ID, Nonce: nonce, Decision: "yes"}
	if _, err := b.c.Decide(ctx, b.channel, on, yes); reasonOf(err) != "details_not_opened" {
		t.Fatalf("yes without details: %v", err)
	}
	b.govDetails(t, cd, on)
	if res, err := b.c.Decide(ctx, b.channel, on, yes); err != nil || res.State != "confirmed" {
		t.Fatalf("yes %+v %v", res, err)
	}
	if n := b.count(t, `SELECT count(*) FROM attributions WHERE id = $1 AND record->>'manager_against' = 'true'`, second.ID); n != 1 {
		t.Fatal("manager against not recorded")
	}
	if n := b.count(t, `SELECT count(*) FROM audit WHERE record->>'command' = 'governance.link.confirmed' AND record->'after'->>'manager_against' = 'true'`); n != 1 {
		t.Fatal("no link audit")
	}
	if n := b.count(t, `SELECT count(*) FROM link_departures`); n != 0 {
		t.Fatalf("departures in shadow %d", n)
	}
	if err := b.c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		n, err := managerAgainstLinks(ctx, tx, now.Add(-time.Hour), now.Add(time.Second))
		if err == nil && n != 1 {
			t.Errorf("manager against %d", n)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	st, err := b.c.CommissionStatus(ctx, alice, TenantRequest{Tenant: "demo"})
	if err != nil || mmapAny(st["accountability"])["rejection_links_confirmed"] != 1 || mmapAny(st["accountability"])["mode"] != "shadow" {
		t.Fatalf("status %v %v", st, err)
	}
	validOut(t, b, "urn:botaru:schema:p2-commission-status-response:1", st)
	// The summary goes once a week.
	b.loop(t, "governance")
	b.advance(t, 7*86400)
	b.loop(t, "governance")
	if n := b.count(t, `SELECT count(*) FROM deliveries WHERE kind = $1`, commissionWeekly); n != 2 {
		t.Fatalf("weekly summaries %d", n)
	}
	// An external cause recorded later cancels the open candidate and ends the departures that
	// wait, since nobody answers for it (GOV-38, GOV-41).
	b.exec(t, `INSERT INTO link_departures (tenant, attribution, seat, bot, state, created_at, updated_at) VALUES ('demo', $1, 'commission:1', 'bot_x', 'waiting', now(), now())`, second.ID)
	if _, err := b.c.IncidentCause(ctx, alice, IncidentCauseRequest{Incident: inc, Cause: "attack"}); err != nil {
		t.Fatal(err)
	}
	if n := b.count(t, `SELECT count(*) FROM attributions WHERE id = $1 AND state = 'canceled'`, links[0].ID); n != 1 {
		t.Fatal("the open candidate of an external cause stays")
	}
	if n := b.count(t, `SELECT count(*) FROM link_departures WHERE state = 'canceled' AND reason = 'external_cause'`); n != 1 {
		t.Fatal("the waiting departure stays")
	}
	// The P4 canary: what a reviewer's model gets is built from its work item, which carries no
	// link id, no cause and no link state.
	b.helloDone(t, "2", "", "hi")
	b.loop(t, "observe")
	if _, err := b.analysis(t, "2", false, map[string]any{"proposals": []any{noneBody("slow", 1)}}); err != nil {
		t.Fatal(err)
	}
	b.loop(t, "proposal")
	ra := b.enroll(t, EnrollRequest{Tenant: "demo", Role: "reviewer-a", Kind: "agent"})
	item := b.claim(t, ra, "attempt")
	if item == nil {
		t.Fatal("no review attempt")
	}
	raw, err := json.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	for _, canary := range []string{links[0].ID, second.ID, "external_load", "attack", "rejected_dependency_1", "manager_against", "rejection_link"} {
		if strings.Contains(string(raw), canary) {
			t.Fatalf("the work item carries %q", canary)
		}
	}
	setClaimsV03(t, false)
	if _, err := b.c.IncidentCause(ctx, alice, IncidentCauseRequest{Incident: inc, Cause: "unknown"}); reasonOf(err) != "not_found" {
		t.Fatalf("incident.cause before v0.3: %v", err)
	}
}

// The dependency hop of script_target (TRN-7) and the stricter tenant threshold of the role
// hold (HLD-9): a resource on a machine a run named depends on it by one step, so an incident on
// the resource is linked to the run; with the tenant catalog's role_incident_hold of one link
// the first confirmed link sets the role hold. A looser catalog value never applies.
func TestScriptLinkHopAndStricterHold(t *testing.T) {
	ctx := context.Background()
	setClaimsV03(t, true)
	b := newBenchWith(t, func(_ string, ms []map[string]any) []map[string]any {
		return append(ms, map[string]any{
			"apiVersion": "botaru/v1alpha1", "kind": "Catalog", "metadata": map[string]any{"name": "demo", "tenant": "demo"},
			"spec": map[string]any{"hosts": []any{map[string]any{"id": "web1"}},
				"resources":          []any{map[string]any{"id": "web1-logs", "title": "Web logs", "host": "web1"}},
				"role_incident_hold": map[string]any{"links": 1, "window": "180d"}},
		}, map[string]any{
			"apiVersion": "botaru/v1alpha1", "kind": "IncidentClass", "metadata": map[string]any{"name": "web_down"},
			"spec": map[string]any{"title": "Web down", "severity": "sev2", "change_sensitive": true,
				"detect": map[string]any{"alarm": "alarm.disk.free.low.v1"}, "subject": map[string]any{"kind": "resource"}},
		}, map[string]any{
			"apiVersion": "botaru/v1alpha1", "kind": "Group", "metadata": map[string]any{"name": "governors", "tenant": "demo"},
			"spec": map[string]any{"caps": []any{"governance.attribute"}, "data": []any{"summary", "internal"}},
		}, storeLowManifest())
	})
	b.exec(t, `INSERT INTO memberships (tenant, principal, grp, state, created_at) VALUES ('demo', $1, 'governors', 'active', now())`, b.alice)
	b.exec(t, `INSERT INTO governance_v03 (tenant, since, updated_at) VALUES ('demo', botaru_now() - interval '2 days', botaru_now())
		ON CONFLICT (tenant) DO UPDATE SET since = excluded.since`)
	run := ids.New(ids.Script)
	b.exec(t, `INSERT INTO script_runs (id, tenant, initiator, state, plan_hash, record, nonce, expires_at, created_at, updated_at, targets, started_at, ended_at, role, data_mark)
		VALUES ($1, 'demo', 'human:other', 'completed', 'x', '{}', 'nonce-of-the-run-01', botaru_now() + interval '1 day', botaru_now() - interval '1 day', botaru_now(),
		ARRAY['web1'], botaru_now() - interval '1 day', botaru_now() - interval '23 hours', 'hello', 'internal')`, run)
	cat := b.catalog(t)
	if err := b.c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		deps, err := subjectDeps(ctx, tx, cat, "demo", "resource", "web1-logs")
		if err == nil && !slices.Equal(deps, []subjectRef{{"resource", "web1"}}) {
			t.Errorf("deps %v", deps)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	b.insertIncident(t, "web_down", "resource", "web1-logs")
	b.insertIncident(t, "web_down", "resource", "db1") // depends on nothing the run named
	b.loop(t, "governance")
	if n := b.count(t, `SELECT count(*) FROM attributions WHERE kind = 'script_run' AND script_run = $1`, run); n != 1 {
		t.Fatalf("script links %d", n)
	}
	cd, on := b.govCard(t, scriptLinkCard)
	b.govDetails(t, cd, on)
	if res, err := b.govDecide(t, cd, on, "yes", "", ""); err != nil || res.State != "confirmed" {
		t.Fatalf("yes %+v %v", res, err)
	}
	if n := b.count(t, `SELECT count(*) FROM holds WHERE state = 'active' AND scope_kind = 'role' AND scope_name = 'hello' AND reason = 'role_incidents'`); n != 1 {
		t.Fatal("no role hold after one link with the stricter threshold")
	}
	loose := &Catalog{RawGlobals: map[manifest.Key]map[string]any{{Kind: "Catalog", Tenant: "demo", Name: "demo"}: {
		"spec": map[string]any{"role_incident_hold": map[string]any{"links": 5, "window": "30d"}}}}}
	if n, w, err := b.c.roleIncidentLimits(loose, "demo"); err != nil || n != 3 || w != 90*24*time.Hour {
		t.Fatalf("a looser catalog value %d %v %v", n, w, err)
	}
}

// From v0.3 a change of the farm manager persona hash changes the seat fingerprint, so the seat
// tasks of a production tenant wait with seat_eval_missing until a new mgr-inj report (GOV-45,
// MGR-23).
func TestManagerPersonaEvalV03(t *testing.T) {
	ctx := context.Background()
	setClaimsV03(t, true)
	b := newManagerBench(t)
	w := b.enroll(t, EnrollRequest{Tenant: "demo", Kind: "agent", Role: "farm-manager"})
	b.exec(t, `UPDATE manifests SET doc = jsonb_set(doc, '{spec,kind}', '"production"') WHERE kind = 'Tenant' AND name = 'demo'`)
	fp, err := seatFingerprint(b.catalog(t), "manager-std")
	if err != nil || fp == "" {
		t.Fatalf("fingerprint %q %v", fp, err)
	}
	b.setStoredDoc(t, "EvalReports", "", "evals", `{"apiVersion": "botaru/v1alpha1", "kind": "EvalReports", "metadata": {"name": "evals"},
		"spec": {"reports": [{"fingerprint": "`+fp+`", "subject": {"kind": "seat", "name": "manager-std"}, "suite": "mgr-inj", "date": "2026-10-02", "cases": 12, "failures": 0, "security_failures": 0}]}}`)
	b.exec(t, `UPDATE manifests SET doc = jsonb_set(doc, '{spec,farm_manager,persona}', jsonb_build_object('ref', 'personas/manager.md', 'sha256', $1::text))
		WHERE kind = 'Governance' AND tenant = 'farm'`, strings.Repeat("a", 64))
	b.exec(t, `UPDATE apply_status SET set_hash = set_hash || 'x'`)
	fp2, err := seatFingerprint(b.catalog(t), "manager-std")
	if err != nil || fp2 == fp {
		t.Fatalf("the persona hash does not move the seat fingerprint: %v", err)
	}
	b.diskAlarm(t, true, 3)
	b.advance(t, 6*60)
	b.loop(t, "incident")
	if _, err := b.c.SeatAssign(ctx, b.farmOp(), SeatRequest{Seat: managerSeatID}); err != nil {
		t.Fatal(err)
	}
	if item := b.claim(t, w, "attempt"); item != nil {
		t.Fatal("a seat task was handed out after the persona changed")
	}
	if n := b.count(t, `SELECT count(*) FROM tasks WHERE tenant = 'demo' AND type = $1 AND reason = 'seat_eval_missing'`, incidentRespondType); n == 0 {
		t.Fatalf("waiting seat tasks %d", n)
	}
}

// The eval gate of the commission persona (GOV-46): on a v0.3 core a reviewer role whose static
// fingerprint changed needs, besides its PRV-3 report, a gov-res-aware report without failures
// for that fingerprint before a production tenant enables it; other roles and a core before v0.3
// do not ask for it.
func TestCommissionUnawareGate(t *testing.T) {
	doc := func(kind, tenant, name string, spec map[string]any) manifest.Doc {
		return manifest.Doc{Kind: kind, Tenant: tenant, Name: name, Value: map[string]any{"apiVersion": "botaru/v1alpha1", "kind": kind,
			"metadata": map[string]any{"name": name}, "spec": spec}}
	}
	base := []manifest.Doc{
		doc("Role", "", "rev", map[string]any{"profile": "reviewer", "claims": []any{"review.security.v1"}}),
		doc("TaskType", "", "review.security.v1", map[string]any{"tier": 1}),
		doc("Tenant", "", "demo", map[string]any{"kind": "production"}),
		doc("Governance", "demo", "charter", map[string]any{"positions": map[string]any{"commission": map[string]any{"persona": map[string]any{"ref": "p.md", "sha256": strings.Repeat("b", 64)}}}}),
	}
	fp, _, err := newEvalIndex(base).staticFingerprint("rev", "demo")
	if err != nil {
		t.Fatal(err)
	}
	reports := func(suites ...string) manifest.Doc {
		var list []any
		for _, s := range suites {
			list = append(list, map[string]any{"fingerprint": fp, "subject": map[string]any{"kind": "role", "name": "rev"}, "suite": s, "date": "2026-10-01",
				"cases": 40, "failures": 0, "security_failures": 0})
		}
		return doc("EvalReports", "", "evals", map[string]any{"reports": list})
	}
	gate := func(docs []manifest.Doc) []ApplyError {
		index := make([]int, len(docs))
		errs, _, err := (&Core{}).evalGate(docs, index, nil)
		if err != nil {
			t.Fatal(err)
		}
		return errs
	}
	setClaimsV03(t, true)
	if errs := gate(append(slices.Clone(base), reports("com"))); len(errs) != 1 || errs[0].Code != "eval_report_missing" {
		t.Fatalf("a commission role without gov-res-aware: %+v", errs)
	}
	if errs := gate(append(slices.Clone(base), reports("com", suiteUnawareness))); len(errs) != 0 {
		t.Fatalf("with gov-res-aware: %+v", errs)
	}
	setClaimsV03(t, false)
	if errs := gate(append(slices.Clone(base), reports("com"))); len(errs) != 0 {
		t.Fatalf("before v0.3: %+v", errs)
	}
}

// From v0.3 the farm manager's daily review reads the two farm guards of the tenant's last day,
// as numbers with their samples (PRP-26).
func TestManagerReviewFarmGuards(t *testing.T) {
	ctx := context.Background()
	setClaimsV03(t, true)
	b := newManagerWorkBench(t)
	b.exec(t, `INSERT INTO obs_points (tenant, series, subject, at, enum, source, data_mark, key, created_at)
		SELECT 'demo', 'farm.task.outcome', 'task_type:hello.greet.v1', botaru_now() - interval '1 day', CASE WHEN g % 4 = 0 THEN 'failed' ELSE 'done' END, 'core', 'internal', 'o' || g, botaru_now()
		FROM generate_series(1, 8) g`)
	b.exec(t, `INSERT INTO obs_points (tenant, series, subject, at, num, source, data_mark, key, created_at)
		SELECT 'demo', 'farm.queue.wait', 'task_type:hello.greet.v1', botaru_now() - interval '1 day', g, 'core', 'internal', 'w' || g, botaru_now() FROM generate_series(1, 20) g`)
	b.loop(t, "identity")
	var input map[string]any
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT body->'params'->'input' FROM tasks WHERE type = $1 AND tenant = 'demo'`, reviewType).Scan(&input); err != nil {
		t.Fatal(err)
	}
	got := map[string]map[string]any{}
	for _, g := range asList(input["farm_guards"]) {
		gm := mmapAny(g)
		got[asString(gm["series"])] = gm
	}
	if o := got["farm.task.outcome"]; o == nil || o["value"] != 0.75 || o["samples"] != float64(8) {
		t.Fatalf("task outcome %v", input["farm_guards"])
	}
	if w := got["farm.queue.wait"]; w == nil || w["value"] != float64(19) || w["subject"] != "tenant:demo" {
		t.Fatalf("queue wait %v", input["farm_guards"])
	}
}
