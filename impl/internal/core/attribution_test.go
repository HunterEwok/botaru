// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"testing"

	"github.com/HunterEwok/botaru/impl/internal/ids"
)

// Tests of the incident attribution: the dependency rule and the P2 operations.

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
	// One candidate per incident and change: a canceled one is not made again.
	b.exec(t, `UPDATE attributions SET state = 'canceled' WHERE id = $1`, v.ID)
	if again, err := b.c.AttributionCreate(ctx, alice, AttributionRequest{Incident: other, Proposal: id}); reasonOf(err) != "conflict" {
		t.Fatalf("create over a canceled candidate %+v %v", again, err)
	}
	if _, err := b.c.AttributionList(ctx, alice, AttributionRequest{State: "open"}); reasonOf(err) != "input_schema_invalid" {
		t.Fatalf("a state outside the enum: %v", err)
	}
}
