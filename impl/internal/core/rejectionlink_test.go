// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/ids"
)

// Tests of the rejection links from v0.3 and the weekly commission summary.

// linkBench is the v0.3 commission bench whose governors also hold admin:governance and
// incident.manage: they get the weekly summary and record an incident's cause.
func linkBench(t *testing.T) *bench {
	t.Helper()
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
	clone := func(id string, mark any) {
		b.exec(t, `INSERT INTO proposals SELECT (jsonb_populate_record(NULL::proposals, to_jsonb(p) || jsonb_build_object('id', $2::text,
			'record', p.record || jsonb_build_object('id', $2::text, 'data_mark', coalesce($3::text, p.record->>'data_mark'))))).*
			FROM proposals p WHERE id = $1`, p1, id, mark)
	}
	clone(p2, nil)
	// A third with a mark above the governors' ceiling never shares their card (DIG-2).
	p3 := ids.New(ids.Proposal)
	clone(p3, "restricted")
	// Of another author role, past the cap of 2 cases per role (GOV-38).
	b.exec(t, `UPDATE proposals SET role = 'hello-b' WHERE id = $1`, p3)
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
	all, err := pgx.CollectRows(rows, pgx.RowToStructByPos[link])
	if err != nil || len(all) != 3 {
		t.Fatalf("links %+v %v", all, err)
	}
	var links []link
	var high link
	for _, l := range all {
		if l.Proposal == p3 {
			high = l
		} else {
			links = append(links, l)
		}
	}
	if len(links) != 2 || links[0].CardOf != links[0].ID || links[1].CardOf != links[0].ID || links[0].Rule != "rejected_dependency_1" || high.CardOf != high.ID {
		t.Fatalf("links %+v, the higher mark %+v", links, high)
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
	// A member above the presser's ceiling on the card (as an older grouping could have put it)
	// is neither drawn on the card nor decided by the person.
	b.exec(t, `UPDATE attributions SET card_of = $2 WHERE id = $1`, high.ID, links[0].ID)
	var highNonce string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT nonce FROM attributions WHERE id = $1`, high.ID).Scan(&highNonce); err != nil {
		t.Fatal(err)
	}
	if _, err := b.c.Decide(ctx, b.channel, on, DecideRequest{SubjectKind: "attribution", ID: high.ID, Nonce: highNonce, Decision: "yes"}); reasonOf(err) != "forbidden" {
		t.Fatalf("a member above the ceiling: %v", err)
	}
	if err := b.c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		redrawn, err := b.c.attributionCardFields(ctx, tx, b.catalog(t), links[0].ID)
		if err == nil && (redrawn["context"].(map[string]any)["candidate3"] != nil || strings.Contains(fmt.Sprint(redrawn), p3)) {
			t.Errorf("the card shows a proposal above its ceiling: %v", redrawn)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
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
	// A departure already done returns as after a canceled link (GOV-42): its seat is vacant, so
	// the operators get seat_return_notice.
	var seat string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT id FROM seats WHERE tenant = 'demo' ORDER BY id LIMIT 1`).Scan(&seat); err != nil {
		t.Fatal(err)
	}
	b.exec(t, `UPDATE seats SET state = 'vacant', holder = NULL WHERE tenant = 'demo' AND id = $1`, seat)
	b.exec(t, `INSERT INTO link_departures (tenant, attribution, seat, bot, state, created_at, updated_at) VALUES ('demo', $1, 'commission:9', 'bot_x', 'waiting', now(), now())`, second.ID)
	b.exec(t, `INSERT INTO link_departures (tenant, attribution, seat, bot, state, done_at, created_at, updated_at) VALUES ('demo', $1, $2, 'bot_y', 'done', now(), now(), now())`, second.ID, seat)
	if _, err := b.c.IncidentCause(ctx, alice, IncidentCauseRequest{Incident: inc, Cause: "attack"}); err != nil {
		t.Fatal(err)
	}
	if n := b.count(t, `SELECT count(*) FROM link_departures WHERE bot = 'bot_y' AND state = 'returned' AND reason = 'notice'`); n != 1 {
		t.Fatal("the done departure of an external cause is not returned")
	}
	if n := b.count(t, `SELECT count(*) FROM audit WHERE record->>'command' = 'incident.cause' AND record->'after'->>'cause' = 'attack'
		AND jsonb_array_length(record->'after'->'canceled_attributions') = 2 AND jsonb_array_length(record->'after'->'canceled_departures') = 1
		AND jsonb_array_length(record->'after'->'returned_departures') = 1`); n != 1 {
		t.Fatal("the cause audit does not list what it canceled and returned")
	}
	if n := b.count(t, `SELECT count(*) FROM attributions WHERE id = $1 AND state = 'canceled'`, links[0].ID); n != 1 {
		t.Fatal("the open candidate of an external cause stays")
	}
	if n := b.count(t, `SELECT count(*) FROM link_departures WHERE state = 'canceled' AND reason = 'external_cause'`); n != 1 {
		t.Fatal("the waiting departure stays")
	}
	// The P4 canary: the review of the linked proposal's own resubmission (same fingerprint, after
	// the 30 days of PRP-24) is built from a work item that carries no link id, no cause and no
	// link state.
	b.advance(t, 31*86400)
	b.helloDone(t, "2", "", "hi")
	b.loop(t, "observe")
	if _, err := b.analysis(t, "2", false, map[string]any{"proposals": []any{changeBody("slow", limitsChange(7))}}); err != nil {
		t.Fatal(err)
	}
	var resub string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT id FROM proposals WHERE id NOT IN ($1, $2, $3) AND record->>'fingerprint' = (SELECT record->>'fingerprint' FROM proposals WHERE id = $1)`,
		p1, p2, p3).Scan(&resub); err != nil {
		t.Fatalf("no resubmission of the linked proposal: %v %v", err, rejections(t, b))
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
	if n := b.count(t, `SELECT count(*) FROM tasks WHERE id = $1 AND record->>'proposal' = $2`, item.WorkID, resub); n != 1 {
		t.Fatalf("the review attempt is not of the resubmission %s", raw)
	}
	for _, canary := range []string{links[0].ID, second.ID, high.ID, "external_load", "attack", "rejected_dependency_1", "manager_against", "rejection_link"} {
		if strings.Contains(string(raw), canary) {
			t.Fatalf("the work item carries %q", canary)
		}
	}
	// GOV-41: a due farm-manager review that is missing counts as against; a proposal the
	// manager lens was not due for has no entry, and the manager was not against it.
	if !managerAgainst(&proposalRow{Rec: map[string]any{"reviews": []any{map[string]any{"lens": "farm_manager", "state": "missing"}}}}) {
		t.Fatal("a missing due farm manager review is not against")
	}
	if managerAgainst(&proposalRow{Rec: map[string]any{"reviews": []any{map[string]any{"lens": "security", "state": "done"}}}}) {
		t.Fatal("a proposal the manager was not asked about counts as against")
	}
}
