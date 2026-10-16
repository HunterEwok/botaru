// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/ids"
)

// The run card marks targets the typed text does not name (TRN-10): a machine named neither by
// id nor by name, a selector the text does not name, and one wider than
// scripts.unnamed_targets_max.
func TestScriptUnnamedTargets(t *testing.T) {
	ctx := context.Background()
	b := newScriptRunBench(t)
	att, h := b.attemptTools(t, "1", false)
	b.exec(t, `UPDATE tasks SET body = jsonb_set(body, '{goal}', '"check web1, please"') WHERE id = $1`, att.WorkID)
	other := map[string]any{"id": "other", "kind": "task", "type": "hello.child.v1", "goal": map[string]any{"template": "check"}, "params": map[string]any{"resource": "web3"}}
	all := map[string]any{"id": "all", "kind": "fanout", "type": "hello.child.v1", "goal": map[string]any{"template": "check"}, "items": map[string]any{"selector": "webs"}}
	r := proposeScriptCall(t, b.bench, h, checkStep, other, all)
	if r.State != "proposed" {
		t.Fatalf("propose %+v", r)
	}
	run, _ := b.run(t, r.RunRef)
	if run.Rec.Unnamed["check"] != nil || containsString(run.Rec.Marks["check"], "targets_not_in_assignment") {
		t.Fatalf("a named machine is marked: %+v", run.Rec.Unnamed)
	}
	if u := run.Rec.Unnamed["other"]; u == nil || len(u.Resources) != 1 || u.Resources[0] != "web3" || !containsString(run.Rec.Marks["other"], "targets_not_in_assignment") {
		t.Fatalf("an unnamed machine: %+v", u)
	}
	if u := run.Rec.Unnamed["all"]; u == nil || len(u.Groups) != 1 || !u.Groups[0].NotNamed || u.Groups[0].Wide || u.Groups[0].Machines != 2 {
		t.Fatalf("an unnamed selector: %+v", u)
	}
	card, err := b.c.buildScriptCard(ctx, b.c.st.Pool, b.catalog(t), run, scriptConfirmCard)
	if err != nil {
		t.Fatal(err)
	}
	validOut(t, b.bench, "card", card)
	// A named selector wider than the tenant's ceiling is still marked.
	tv := *b.catalog(t).Tenants["demo"]
	tv.Spec.Scripts.UnnamedTargetsMax = 1
	sel := scriptStep{ID: "all", Kind: "fanout", Items: map[string]any{"selector": "webs"}}
	if u := unnamedOf(b.catalog(t), &tv, nil, sel, []string{"web1", "web2"}, "run it on webs"); u == nil || u.Groups[0].NotNamed || !u.Groups[0].Wide {
		t.Fatalf("a wide selector: %+v", u)
	}
	if textNames("check web10", "web1") || !textNames("Check WEB1.", "web1") {
		t.Fatal("names are whole words")
	}
}

// Script links and the role hold (TRN-7, HLD-9, blackbox:core/role-incident-hold): an incident
// on a target of a finished run gives a candidate on script_link_card to a person with
// governance.attribute who did not press "Run"; with nobody else the initiator decides and the
// answer is a self-assessment. Three confirmed links of the translator role in 90 days set the
// role hold with no_action and the alarm; a draft with an action is rejected with held, a
// read-only draft is accepted and runs; a dismissed incident disputes its link but lifts
// nothing; a tenant admin with hold.release lifts the hold.
func TestScriptLinkRoleHold(t *testing.T) {
	ctx := context.Background()
	b := newScriptRunBenchWith(t, []map[string]any{{
		"apiVersion": "botaru/v1alpha1", "kind": "Group", "metadata": map[string]any{"name": "governors", "tenant": "demo"},
		"spec": map[string]any{"caps": []any{"governance.attribute", "hold.release"}, "data": []any{"summary", "internal"}},
	}, {
		"apiVersion": "botaru/v1alpha1", "kind": "IncidentClass", "metadata": map[string]any{"name": "web_down"},
		"spec": map[string]any{"title": "Web down", "severity": "sev2", "change_sensitive": true,
			"detect": map[string]any{"alarm": "alarm.disk.free.low.v1"}, "subject": map[string]any{"kind": "resource"}},
	}, storeLowManifest()})
	for _, p := range []string{b.alice, b.bob} {
		b.exec(t, `INSERT INTO memberships (tenant, principal, grp, state, created_at) VALUES ('demo', $1, 'governors', 'active', now())`, p)
	}
	// A read-only run on web1 by alice.
	att, h := b.attemptTools(t, "1", false)
	r := proposeScriptCall(t, b.bench, h, checkStep)
	b.translated(t, att, map[string]any{"outcome": "drafted", "run_ref": r.RunRef})
	card := cardOf(t, b.drain(t), scriptConfirmCard, "direct:1001")
	run, _ := b.run(t, r.RunRef)
	on := press("telegram:1001", "direct:1001", card.external)
	if _, err := b.c.CardDetails(ctx, b.channel, on, CardOp{Op: "card.details", SubjectKind: "script", ID: run.ID, Nonce: run.Nonce}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.c.Decide(ctx, b.channel, on, DecideRequest{SubjectKind: "script", ID: run.ID, Nonce: run.Nonce, Decision: "approve"}); err != nil {
		t.Fatal(err)
	}
	item := b.claim(t, b.checker, "attempt")
	if _, err := b.c.Complete(ctx, b.checker, header(t, item), CompleteRequest{Result: map[string]any{"answer": "fine", "healthy": true}}); err != nil {
		t.Fatal(err)
	}
	b.drain(t)
	// Retention purges the finished run's record; its links still count (HLD-9).
	if err := b.c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		n, err := purgeRuns(ctx, tx, "", "SELECT $1::text", run.ID)
		if err == nil && n != 1 {
			err = fmt.Errorf("purged %d", n)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// One live incident per subject: the later ones are already closed.
	incident := func(subject, state string) string {
		id := ids.New(ids.Incident)
		b.exec(t, `INSERT INTO incidents (id, tenant, class, subject_kind, subject_id, alarmkey, episode, episodes, alarmkeys, state, record, since, implicated_at, opened_at, updated_at)
			VALUES ($1, 'demo', 'web_down', 'resource', $2, $1, 1, ARRAY[1::bigint], ARRAY[$1], $3, '{}', botaru_now(), botaru_now(), botaru_now(), botaru_now())`, id, subject, state)
		return id
	}
	b.advance(t, 3600)
	first := incident("web1", "open")
	incident("web1", "closed")
	incident("web2", "open") // not a target of the run
	b.tick(t, "governance")
	var links int
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM attributions WHERE kind = 'script_run' AND script_run = $1 AND record->>'rule' = 'script_target'`, run.ID).Scan(&links); err != nil || links != 2 {
		t.Fatalf("links %d %v", links, err)
	}
	sent := b.drain(t)
	var cards []map[string]any
	for _, s := range sent {
		if s.kind == scriptLinkCard {
			if s.chat != "direct:1002" {
				t.Fatalf("a script link card to the initiator: %+v", s)
			}
			cd := s.body["card"].(map[string]any)
			validOut(t, b.bench, "card", cd)
			if ctxt := cd["context"].(map[string]any); ctxt["initiator_opened_steps"] != true || ctxt["self_assessment"] != nil {
				t.Fatalf("card context %v", ctxt)
			}
			cards = append(cards, cd)
			on := press("telegram:1002", "direct:1002", s.external)
			if _, err := b.govDecide(t, cd, on, "yes", "", ""); err == nil {
				t.Fatal("yes without Details")
			}
			b.govDetails(t, cd, on)
			if res, err := b.govDecide(t, cd, on, "yes", "", ""); err != nil || res.State != "confirmed" {
				t.Fatalf("yes %+v %v", res, err)
			}
		}
	}
	if len(cards) != 2 {
		t.Fatalf("cards %d", len(cards))
	}
	if n := countEvents(t, b.bench, "hold.set.v1", ""); n != 0 {
		t.Fatalf("a hold after two links: %d", n)
	}
	// With bob gone, alice is the only person who may decide: a self-assessment.
	if _, err := b.c.Grant(ctx, b.op, GrantRequest{Op: "ungrant", Subject: b.bob, Group: "governors"}); err != nil {
		t.Fatal(err)
	}
	incident("web1", "closed")
	b.tick(t, "governance")
	s := cardOf(t, b.drain(t), scriptLinkCard, "direct:1001")
	cd := s.body["card"].(map[string]any)
	if cd["context"].(map[string]any)["self_assessment"] != true {
		t.Fatalf("card %v", cd["context"])
	}
	on = press("telegram:1001", "direct:1001", s.external)
	b.govDetails(t, cd, on)
	if res, err := b.govDecide(t, cd, on, "yes", "", ""); err != nil || res.State != "confirmed" {
		t.Fatalf("self-assessed yes %+v %v", res, err)
	}
	var self int
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM audit WHERE record->>'command' = 'decide' AND record->'target'->>'kind' = 'attribution'
		AND (record->'after'->>'self_assessment')::boolean`).Scan(&self); err != nil || self != 1 {
		t.Fatalf("self-assessment audit %d %v", self, err)
	}
	var hold string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT id FROM holds WHERE state = 'active' AND scope_kind = 'role' AND scope_name = 'hello' AND reason = 'role_incidents'
		AND effects = ARRAY['no_action'] AND author_kind = 'contain_auto' AND until IS NULL`).Scan(&hold); err != nil {
		t.Fatalf("the role hold: %v", err)
	}
	if n := countEvents(t, b.bench, "alarm.hold.auto.v1", "hold.auto:hello"); n != 1 {
		t.Fatalf("alarm %d", n)
	}
	// Under the hold: a draft with an action is rejected with held, a read-only one runs; its
	// read fanout is a campaign, which no_action pauses like every campaign (ORC-11), so it
	// fails closed.
	att, h = b.attemptTools(t, "2", false)
	if r := proposeScriptCall(t, b.bench, h, checkStep, fixStep); r.reason() != "held" {
		t.Fatalf("a draft with an action: %+v", r)
	}
	all := map[string]any{"id": "all", "kind": "fanout", "type": "hello.child.v1", "goal": map[string]any{"template": "check"}, "items": map[string]any{"selector": "webs"}}
	r = proposeScriptCall(t, b.bench, h, all)
	if r.State != "proposed" {
		t.Fatalf("a read-only draft: %+v", r)
	}
	b.translated(t, att, map[string]any{"outcome": "drafted", "run_ref": r.RunRef})
	card = cardOf(t, b.drain(t), scriptConfirmCard, "direct:1001")
	run, _ = b.run(t, r.RunRef)
	if res, err := b.c.Decide(ctx, b.channel, press("telegram:1001", "direct:1001", card.external),
		DecideRequest{SubjectKind: "script", ID: run.ID, Nonce: run.Nonce, Decision: "approve"}); err != nil || res.State != "running" {
		t.Fatalf("a read-only run under the hold %+v %v", res, err)
	}
	var cmpID string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT campaign FROM script_steps WHERE run = $1 AND id = 'all'`, run.ID).Scan(&cmpID); err != nil {
		t.Fatal(err)
	}
	cmp := b.campaign(t, cmpID)
	if cmp.State != "paused" || cmp.PauseReason != "held" {
		t.Fatalf("the read fanout under the hold: %s %s", cmp.State, cmp.PauseReason)
	}
	if eff := campaignHoldEffectsOf(cmp); !containsString(eff, "no_enqueue") || !containsString(eff, "no_action") || containsString(eff, "pause_action") {
		t.Fatalf("read campaign hold effects %v", eff)
	}
	// A dismissed incident disputes its link and lifts nothing.
	if _, err := b.c.IncidentDismiss(ctx, b.op, IncidentRequest{Incident: first, ReasonCode: "maintenance", Details: "planned work"}); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT a.state || '/' || h.state FROM attributions a, holds h WHERE a.incident = $1 AND h.id = $2`, first, hold).Scan(&state); err != nil || state != "disputed/active" {
		t.Fatalf("after the dismissal %s %v", state, err)
	}
	if res, err := b.c.HoldRelease(ctx, HoldActor{Principal: b.alice, Rank: "tenant_admin", Tenant: "demo", Via: "channel"}, HoldReleaseRequest{ID: hold}); err != nil || res.State != "released" {
		t.Fatalf("release %+v %v", res, err)
	}
	// The drafting worker's own hold counts as well.
	if _, err := b.c.HoldSet(ctx, OperatorActor(b.op), HoldSetRequest{Scope: HoldScope{Kind: "worker", Tenant: "demo", Name: strings.ToLower(b.agent.ID)},
		Effects: []string{"no_action"}, Reason: "manual"}); err != nil {
		t.Fatal(err)
	}
	_, h = b.attemptTools(t, "3", false)
	if r := proposeScriptCall(t, b.bench, h, checkStep, fixStep); r.reason() != "held" {
		t.Fatalf("a draft with an action under the worker hold: %+v", r)
	}
}
