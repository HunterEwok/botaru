// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"strings"
	"testing"

	"github.com/HunterEwok/botaru/impl/internal/ids"
)

// addPerson adds a principal to the demo tenant with active memberships of groups, written
// straight to the store as the verified grant flow would leave them.
func (b *bench) addPerson(t *testing.T, identity string, groups ...string) string {
	t.Helper()
	ctx := context.Background()
	p := ids.PrincipalID()
	if _, err := b.c.Principal(ctx, b.op, PrincipalRequest{Op: "principal.add", Subject: p, Identity: identity}); err != nil {
		t.Fatal(err)
	}
	for _, g := range groups {
		if _, err := b.c.st.Pool.Exec(ctx, `INSERT INTO memberships (tenant, principal, grp, state, created_at) VALUES ('demo', $1, $2, 'active', now())`, p, g); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

// auditCount counts the audit records of a command on a target with an outcome.
func (b *bench) auditCount(t *testing.T, command, target string, ok bool) int {
	t.Helper()
	var n int
	if err := b.c.st.Pool.QueryRow(context.Background(), `SELECT count(*) FROM audit WHERE record->>'command' = $1
		AND record->'target'->>'id' = $2 AND (record->>'ok')::boolean = $3`, command, target, ok).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func listIDs(out map[string]any) []string {
	var ids []string
	for _, it := range out["items"].([]any) {
		ids = append(ids, it.(map[string]any)["id"].(string))
	}
	return ids
}

// P2 campaign.list, campaign.show, campaign.control and campaign.retry_failed (ORC-1, PRM-9,
// IDM-3, AUD-1): the initiator sees its campaign and its frozen record; a person without a
// right to it, another tenant and a farm credential get not_found or forbidden; "Pause" by the
// initiator pauses it with the reason human; "Cancel" by a person who decides it stops it as
// canceled(admin), a repeat answers the same and the commands are audited; "Retry failed" is
// the initiator's only (a refusal of another person is audited) and proposes a new campaign
// of the unfinished items with a new card, the same one on a repeat.
func TestP2Campaigns(t *testing.T) {
	ctx := context.Background()
	b := newCampaignReadBench(t)
	att, h := b.attemptTools(t, "1", false)
	r := proposeCall(t, b, h, map[string]any{
		"template": map[string]any{"kind": "task", "type": "hello.child.v1", "goal": "Check the machine."},
		"items":    map[string]any{"resources": []any{"web1", "web2", "web3"}},
		"rollout":  map[string]any{"max_in_flight": 2},
	})
	if r.State != "running" {
		t.Fatalf("propose %+v", r)
	}
	b.finish(t, att)
	alice := Operator{Principal: b.alice, Tenant: "demo"}
	list, err := b.c.CampaignList(ctx, alice, TenantRequest{})
	if err != nil || len(list["items"].([]any)) != 1 || listIDs(list)[0] != r.Campaign {
		t.Fatalf("list %v %v", list, err)
	}
	validOut(t, b, "p2-list-response", list)
	if list["items"].([]any)[0].(map[string]any)["title"] == "" {
		t.Fatalf("row %v", list)
	}
	rec, err := b.c.CampaignShow(ctx, alice, ObjectRequest{Op: "campaign.show", ID: r.Campaign})
	if err != nil || rec["state"] != "running" || rec["plan_hash"] == "" {
		t.Fatalf("show %v %v", rec, err)
	}
	validOut(t, b, "campaign", rec)
	if b.auditCount(t, "campaign.show", r.Campaign, true) != 1 {
		t.Fatal("the read is not audited")
	}
	// Nobody else sees it: a person without rights, another tenant, farm.
	carol := Operator{Principal: b.addPerson(t, "telegram:1003"), Tenant: "demo"}
	if l, err := b.c.CampaignList(ctx, carol, TenantRequest{}); err != nil || len(l["items"].([]any)) != 0 {
		t.Fatalf("carol's list %v %v", l, err)
	}
	for _, op := range []Operator{carol, {Principal: b.alice, Tenant: "farm"}, {Principal: b.alice, Tenant: "demo2"}} {
		if _, err := b.c.CampaignShow(ctx, op, ObjectRequest{ID: r.Campaign}); reasonOf(err) != "not_found" {
			t.Fatalf("%+v shows: %v", op, err)
		}
		if _, err := b.c.CampaignControlP2(ctx, op, P2CampaignControlRequest{ID: r.Campaign, Command: "cancel"}); reasonOf(err) != "not_found" {
			t.Fatalf("%+v cancels: %v", op, err)
		}
	}
	if _, err := b.c.CampaignList(ctx, alice, TenantRequest{Tenant: "demo2"}); reasonOf(err) != "forbidden" {
		t.Fatalf("another tenant's list: %v", err)
	}
	// Pause by the initiator, twice.
	for range 2 {
		if res, err := b.c.CampaignControlP2(ctx, alice, P2CampaignControlRequest{ID: r.Campaign, Command: "pause"}); err != nil || res.State != "paused" {
			t.Fatalf("pause %+v %v", res, err)
		}
	}
	if cmp := b.campaign(t, r.Campaign); cmp.PauseReason != "human" {
		t.Fatalf("pause reason %s", cmp.PauseReason)
	}
	// A person who decides the campaign (task:<type> of a read campaign) cancels it.
	bob := Operator{Principal: b.addPerson(t, "telegram:1002", "hello"), Tenant: "demo"}
	first, err := b.c.CampaignControlP2(ctx, bob, P2CampaignControlRequest{ID: r.Campaign, Command: "cancel"})
	if err != nil {
		t.Fatal(err)
	}
	cmp := b.campaign(t, r.Campaign)
	if cmp.StopReason != "admin" || first.State != cmp.State || cmp.State != "canceled" && cmp.State != "stopping" {
		t.Fatalf("cancel %+v %s %s", first, cmp.State, cmp.StopReason)
	}
	validOut(t, b, "p1-campaign-control-response", first)
	if again, err := b.c.CampaignControlP2(ctx, bob, P2CampaignControlRequest{ID: r.Campaign, Command: "cancel"}); err != nil || *again != *first {
		t.Fatalf("repeat %+v %v", again, err)
	}
	if _, err := b.c.CampaignControlP2(ctx, alice, P2CampaignControlRequest{ID: r.Campaign, Command: "pause"}); reasonOf(err) != "already_terminal" {
		t.Fatalf("pause of a canceled campaign: %v", err)
	}
	if b.auditCount(t, "campaign.control", r.Campaign, false) != 1 {
		t.Fatal("the refused pause is not audited")
	}
	if b.auditCount(t, "campaign.control", r.Campaign, true) != 3 {
		t.Fatalf("control audit %d", b.auditCount(t, "campaign.control", r.Campaign, true))
	}
	// The released items end canceled and the pending one skipped; "Retry failed" takes the
	// skipped one (failed, unknown and skipped, not canceled).
	for _, w := range []*Worker{b.agent} {
		for item := b.claim(t, w, "attempt"); item != nil; item = b.claim(t, w, "attempt") {
			t.Fatalf("a canceled item was handed out: %v", item.Payload["task"])
		}
	}
	if cmp = b.campaign(t, r.Campaign); cmp.State != "canceled" {
		t.Fatalf("state %s items %s", cmp.State, b.itemStates(t, cmp.ID))
	}
	if _, err := b.c.CampaignRetryFailed(ctx, bob, ObjectRequest{Op: "campaign.retry_failed", ID: r.Campaign}); reasonOf(err) != "forbidden" {
		t.Fatalf("retry by another person: %v", err)
	}
	if b.auditCount(t, "campaign.retry_failed", r.Campaign, false) != 1 { // bob's
		t.Fatal("the refused retry is not audited")
	}
	// A campaign of a script step is its run's: no retry by a person (SCR-6).
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE campaigns SET record = record || '{"script_run": "sr_01JB0000000000000000000099"}' WHERE id = $1`, r.Campaign); err != nil {
		t.Fatal(err)
	}
	if _, err := b.c.CampaignRetryFailed(ctx, alice, ObjectRequest{ID: r.Campaign}); reasonOf(err) != "forbidden" {
		t.Fatalf("retry of a script step's campaign: %v", err)
	}
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE campaigns SET record = record - 'script_run' WHERE id = $1`, r.Campaign); err != nil {
		t.Fatal(err)
	}
	d, err := b.c.CampaignRetryFailed(ctx, alice, ObjectRequest{Op: "campaign.retry_failed", ID: r.Campaign})
	if err != nil || d.ID == r.Campaign || d.State != "proposed" {
		t.Fatalf("retry %+v %v", d, err)
	}
	validOut(t, b, "done-response", d)
	retry := b.campaign(t, d.ID)
	if retry.Rec.Refs != r.Campaign || !retry.Card || retry.Rec.Then != nil || b.itemStates(t, retry.ID) != "pending" {
		t.Fatalf("the retry %+v %s", retry.Rec, b.itemStates(t, retry.ID))
	}
	if again, err := b.c.CampaignRetryFailed(ctx, alice, ObjectRequest{Op: "campaign.retry_failed", ID: r.Campaign}); err != nil || again.ID != d.ID {
		t.Fatalf("repeat retry %+v %v", again, err)
	}
}

// A read campaign whose released item no worker takes by its start_by pauses with
// no_executor and releases nothing more; the initiator's "Continue" resumes it with a new
// start_by window, so the next pass does not pause it again at once (ORC-1, ORC-4).
func TestCampaignNoExecutor(t *testing.T) {
	ctx := context.Background()
	b := newCampaignReadBench(t)
	att, h := b.attemptTools(t, "1", false)
	r := proposeCall(t, b, h, map[string]any{
		"template": map[string]any{"kind": "task", "type": "hello.child.v1", "goal": "Check the machine."},
		"items":    map[string]any{"resources": []any{"web1", "web2", "web3"}},
		"rollout":  map[string]any{"max_in_flight": 1},
	})
	b.finish(t, att)
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE tasks t SET start_by_at = i.released_at + interval '60 seconds'
		FROM campaign_items i WHERE i.task = t.id AND i.campaign = $1`, r.Campaign); err != nil {
		t.Fatal(err)
	}
	b.loop(t, "campaign")
	if cmp := b.campaign(t, r.Campaign); cmp.State != "running" {
		t.Fatalf("before start_by %s", cmp.State)
	}
	b.advance(t, 61)
	// A task that waits for another reason (a waiting_reason) does not count (ORC-4).
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE tasks t SET record = t.record || '{"waiting_reason": "type_quota"}'
		FROM campaign_items i WHERE i.task = t.id AND i.campaign = $1`, r.Campaign); err != nil {
		t.Fatal(err)
	}
	b.loop(t, "campaign")
	if cmp := b.campaign(t, r.Campaign); cmp.State != "running" {
		t.Fatalf("with a waiting_reason %s", cmp.State)
	}
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE tasks t SET record = t.record - 'waiting_reason'
		FROM campaign_items i WHERE i.task = t.id AND i.campaign = $1`, r.Campaign); err != nil {
		t.Fatal(err)
	}
	b.loop(t, "campaign")
	cmp := b.campaign(t, r.Campaign)
	if cmp.State != "paused" || cmp.PauseReason != "no_executor" || b.itemStates(t, cmp.ID) != "released,pending,pending" {
		t.Fatalf("after start_by %s %s %s", cmp.State, cmp.PauseReason, b.itemStates(t, cmp.ID))
	}
	progress := cardOf(t, b.drain(t), campaignProgressCard, "direct:1001")
	res, err := b.c.CampaignControl(ctx, b.channel, press("telegram:1001", "direct:1001", progress.external),
		CampaignControlRequest{ID: cmp.ID, Command: "resume", Nonce: cmp.Nonce})
	if err != nil || res.State != "running" {
		t.Fatalf("resume %+v %v", res, err)
	}
	b.loop(t, "campaign")
	if cmp = b.campaign(t, cmp.ID); cmp.State != "running" || cmp.Rec.ExecutorFrom == nil {
		t.Fatalf("after the resume %s %v", cmp.State, cmp.Rec.ExecutorFrom)
	}
	b.advance(t, 61)
	b.loop(t, "campaign")
	if cmp = b.campaign(t, cmp.ID); cmp.State != "paused" || cmp.PauseReason != "no_executor" {
		t.Fatalf("the new window %s %s", cmp.State, cmp.PauseReason)
	}
}

// P2 script.list, script.show and script.cancel (SCR-5, SCR-11, PRM-9, IDM-3, AUD-1): the
// initiator sees its run with the accepted draft; a person without task.read of the anchor
// type and another tenant get not_found; a reader who is no admin gets forbidden for the
// cancel, which is audited; a person with admin:people cancels a running run in one
// transaction with its waiting approval (canceled(admin)), a repeat answers the same; the
// initiator's cancel of a proposed run rejects it with the reason declined, as its card's button.
func TestP2Scripts(t *testing.T) {
	ctx := context.Background()
	b := newScriptRunBenchWith(t, []map[string]any{
		{"apiVersion": "botaru/v1alpha1", "kind": "Group", "metadata": map[string]any{"name": "admins", "tenant": "demo"},
			"spec": map[string]any{"caps": []any{"admin:people"}, "data": []any{"summary"}}},
		{"apiVersion": "botaru/v1alpha1", "kind": "Group", "metadata": map[string]any{"name": "readers", "tenant": "demo"},
			"spec": map[string]any{"caps": []any{"task.read:hello.greet.v1"}, "data": []any{"summary"}}},
	})
	att, h := b.attemptTools(t, "1", false)
	r := proposeScriptCall(t, b.bench, h, checkStep, fixStep)
	b.translated(t, att, map[string]any{"outcome": "drafted", "run_ref": r.RunRef})
	card := cardOf(t, b.drain(t), scriptConfirmCard, "direct:1001")
	run, _ := b.run(t, r.RunRef)
	if _, err := b.c.Decide(ctx, b.channel, press("telegram:1001", "direct:1001", card.external),
		DecideRequest{SubjectKind: "script", ID: run.ID, Nonce: run.Nonce, Decision: "approve"}); err != nil {
		t.Fatal(err)
	}
	item := b.claim(t, b.checker, "attempt")
	if _, err := b.c.Complete(ctx, b.checker, header(t, item), CompleteRequest{Result: map[string]any{"answer": "down", "healthy": false}}); err != nil {
		t.Fatal(err)
	}
	var approval string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT approval FROM script_steps WHERE run = $1 AND id = 'fix'`, run.ID).Scan(&approval); err != nil {
		t.Fatal(err)
	}
	alice := Operator{Principal: b.alice, Tenant: "demo"}
	list, err := b.c.ScriptList(ctx, alice, TenantRequest{})
	if err != nil || len(listIDs(list)) != 1 || listIDs(list)[0] != run.ID {
		t.Fatalf("list %v %v", list, err)
	}
	validOut(t, b.bench, "p2-list-response", list)
	rec, err := b.c.ScriptShow(ctx, alice, ObjectRequest{Op: "script.show", ID: run.ID})
	if err != nil || rec["state"] != "running" || rec["script"] == nil {
		t.Fatalf("show %v %v", rec, err)
	}
	validOut(t, b.bench, "script-run", rec)
	// bob approves fix_web but may not read the run; another tenant and farm see nothing.
	bob := Operator{Principal: b.bob, Tenant: "demo"}
	if l, err := b.c.ScriptList(ctx, bob, TenantRequest{}); err != nil || len(l["items"].([]any)) != 0 {
		t.Fatalf("bob's list %v %v", l, err)
	}
	for _, op := range []Operator{bob, {Principal: b.alice, Tenant: "farm"}, {Principal: b.alice, Tenant: "demo2"}} {
		if _, err := b.c.ScriptShow(ctx, op, ObjectRequest{ID: run.ID}); reasonOf(err) != "not_found" {
			t.Fatalf("%+v shows: %v", op, err)
		}
		if _, err := b.c.ScriptCancel(ctx, op, ObjectRequest{ID: run.ID}); reasonOf(err) != "not_found" {
			t.Fatalf("%+v cancels: %v", op, err)
		}
	}
	reader := Operator{Principal: b.addPerson(t, "telegram:1004", "readers"), Tenant: "demo"}
	if _, err := b.c.ScriptShow(ctx, reader, ObjectRequest{ID: run.ID}); err != nil {
		t.Fatalf("a reader with task.read: %v", err)
	}
	if _, err := b.c.ScriptCancel(ctx, reader, ObjectRequest{ID: run.ID}); reasonOf(err) != "forbidden" {
		t.Fatalf("a reader cancels: %v", err)
	}
	if b.auditCount(t, "script.cancel", run.ID, false) != 1 {
		t.Fatal("the refused cancel is not audited")
	}
	admin := Operator{Principal: b.addPerson(t, "telegram:1005", "admins"), Tenant: "demo"}
	d, err := b.c.ScriptCancel(ctx, admin, ObjectRequest{Op: "script.cancel", ID: run.ID})
	if err != nil || d.State != "canceled" || d.AuditSeq == 0 {
		t.Fatalf("cancel %+v %v", d, err)
	}
	validOut(t, b.bench, "done-response", d)
	run, states := b.run(t, run.ID)
	if run.Reason != "admin" || states != "ok,canceled" || b.approval(t, approval).State != "canceled" {
		t.Fatalf("after the cancel %s %s %s", run.Reason, states, b.approval(t, approval).State)
	}
	if again, err := b.c.ScriptCancel(ctx, admin, ObjectRequest{ID: run.ID}); err != nil || again.State != "canceled" {
		t.Fatalf("repeat %+v %v", again, err)
	}
	if _, err := b.c.ScriptCancel(ctx, alice, ObjectRequest{ID: run.ID}); reasonOf(err) != "already_terminal" {
		t.Fatalf("the initiator after the admin: %v", err)
	}
	if b.auditCount(t, "script.cancel", run.ID, false) != 2 {
		t.Fatal("the refused cancel of an ended run is not audited")
	}
	// A proposed run: the initiator's cancel rejects it.
	att, h = b.attemptTools(t, "2", false)
	r2 := proposeScriptCall(t, b.bench, h, checkStep)
	b.translated(t, att, map[string]any{"outcome": "drafted", "run_ref": r2.RunRef})
	if d, err := b.c.ScriptCancel(ctx, alice, ObjectRequest{ID: r2.RunRef}); err != nil || d.State != "rejected" {
		t.Fatalf("cancel of a proposed run %+v %v", d, err)
	}
	if run2, _ := b.run(t, r2.RunRef); run2.Reason != "declined" {
		t.Fatalf("reason %s", run2.Reason)
	}
}

// P2 proposal.list and proposal.show (PRP-4, DIG-1, PRP-20, PRM-9): a person with
// proposal.decide sees only proposals shown to people: not while they are screened or in
// review, then the presented one with its record; the view creates no task, approval, run
// or delivery; a blind sample item, also one closed without "Would accept", shows no reason,
// reviews or verdict; a proposal above the person's ceiling is neither listed nor shown; a person without proposal.decide is
// forbidden, and another tenant gets nothing.
func TestP2Proposals(t *testing.T) {
	ctx := context.Background()
	b := newCommissionBench(t, true, presentEdit)
	proposeNone(t, b)
	alice := Operator{Principal: b.alice, Tenant: "demo"}
	id, _ := b.proposalNonce(t)
	if l, err := b.c.ProposalList(ctx, alice, ProposalListRequest{}); err != nil || len(l["items"].([]any)) != 0 {
		t.Fatalf("a proposal in review is listed: %v %v", l, err)
	}
	if _, err := b.c.ProposalShow(ctx, alice, ProposalShowRequest{Proposal: id}); reasonOf(err) != "not_found" {
		t.Fatalf("a proposal in review is shown: %v", err)
	}
	b.reviewAll(t, 3)
	b.loop(t, "proposal")
	if state, _ := proposalState(t, b); state != "presented" {
		t.Fatalf("state %s", state)
	}
	b.drain(t)
	count := func() int {
		var n int
		if err := b.c.st.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM tasks) + (SELECT count(*) FROM approvals) + (SELECT count(*) FROM script_runs)
			+ (SELECT count(*) FROM deliveries)`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := count()
	l, err := b.c.ProposalList(ctx, alice, ProposalListRequest{})
	if err != nil || len(listIDs(l)) != 1 || listIDs(l)[0] != id {
		t.Fatalf("list %v %v", l, err)
	}
	validOut(t, b, "p2-proposal-list-response", l)
	if l, err := b.c.ProposalList(ctx, alice, ProposalListRequest{Category: []string{"cost"}}); err != nil || len(l["items"].([]any)) != 0 {
		t.Fatalf("a category filter %v %v", l, err)
	}
	rec, err := b.c.ProposalShow(ctx, alice, ProposalShowRequest{Proposal: id})
	if err != nil || rec["state"] != "presented" {
		t.Fatalf("show %v %v", rec, err)
	}
	validOut(t, b, "p2-proposal-show-response", rec)
	if after := count(); after != before {
		t.Fatalf("a view created objects: %d -> %d", before, after)
	}
	if b.auditCount(t, "proposal.show", id, true) != 1 {
		t.Fatal("the read is not audited")
	}
	// Without proposal.decide, and from another tenant.
	carol := Operator{Principal: b.addPerson(t, "telegram:1003"), Tenant: "demo"}
	if _, err := b.c.ProposalList(ctx, carol, ProposalListRequest{}); reasonOf(err) != "forbidden" {
		t.Fatalf("carol lists: %v", err)
	}
	if _, err := b.c.ProposalShow(ctx, carol, ProposalShowRequest{Proposal: id}); reasonOf(err) != "forbidden" {
		t.Fatalf("carol shows: %v", err)
	}
	for _, op := range []Operator{{Principal: b.alice, Tenant: "farm"}, {Principal: b.alice, Tenant: "demo2"}} {
		if _, err := b.c.ProposalShow(ctx, op, ProposalShowRequest{Proposal: id}); reasonOf(err) != "not_found" {
			t.Fatalf("%+v shows: %v", op, err)
		}
	}
	// A blind sample item (DIG-6).
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE proposals SET state = 'sample_presented',
		record = record || '{"state": "sample_presented", "state_reason": "veto"}' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	l, err = b.c.ProposalList(ctx, alice, ProposalListRequest{State: []string{"sample_presented"}})
	if err != nil || len(l["items"].([]any)) != 1 || l["items"].([]any)[0].(map[string]any)["state_reason"] != nil {
		t.Fatalf("a blind row %v %v", l, err)
	}
	rec, err = b.c.ProposalShow(ctx, alice, ProposalShowRequest{Proposal: id})
	if err != nil || len(rec["reviews"].([]any)) != 0 || rec["verdict"] != nil || rec["state_reason"] != nil {
		t.Fatalf("a blind record %v %v", rec, err)
	}
	validOut(t, b, "p2-proposal-show-response", rec)
	// Closed without "Would accept": still blind.
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE proposals SET state = 'sample_closed', record = record || '{"state": "sample_closed"}' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if rec, err = b.c.ProposalShow(ctx, alice, ProposalShowRequest{Proposal: id}); err != nil || rec["verdict"] != nil || rec["state_reason"] != nil {
		t.Fatalf("a closed blind record %v %v", rec, err)
	}
	// Above the person's ceiling (internal): neither a row nor a record.
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE proposals SET record = record || '{"data_mark": "raw_logs"}' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if l, err := b.c.ProposalList(ctx, alice, ProposalListRequest{}); err != nil || len(l["items"].([]any)) != 0 {
		t.Fatalf("the row above the ceiling %v %v", l, err)
	}
	if _, err := b.c.ProposalShow(ctx, alice, ProposalShowRequest{Proposal: id}); reasonOf(err) != "not_found" {
		t.Fatalf("a record above the ceiling: %v", err)
	}
}

// P2 escalation.list and escalation.show (MGR-5, TEN-2): an active person of the tenant who
// holds the source task's mark sees the record by the escalation schema without the note; only
// one with audit.read gets the note, inside the untrusted wrapper and audited, and not once the
// source task is gone; a credential without a person, farm and another tenant see nothing, and
// naming the tenant from elsewhere is forbidden.
func TestP2Escalations(t *testing.T) {
	ctx := context.Background()
	nec := seatType(necessityType)
	asMap(nec["spec"])["purpose"] = "review"
	b := managerV02Bench(t, []string{escalationsType}, []map[string]any{nec,
		{"apiVersion": "botaru/v1alpha1", "kind": "Role", "metadata": map[string]any{"name": "esc-reviewer"},
			"spec": map[string]any{"profile": "reviewer", "worker_kind": "agent", "claims": []any{necessityType}, "models": map[string]any{"base": "review-a"}}},
		{"apiVersion": "botaru/v1alpha1", "kind": "Group", "metadata": map[string]any{"name": "system-ops", "tenant": "demo"},
			"spec": map[string]any{"caps": []any{"task:" + necessityType}, "data": []any{"summary", "internal"}}},
		{"apiVersion": "botaru/v1alpha1", "kind": "Group", "metadata": map[string]any{"name": "auditors", "tenant": "demo"},
			"spec": map[string]any{"caps": []any{"audit.read"}, "data": []any{"summary", "internal"}}},
	}, func(ms []map[string]any) {
		for _, m := range ms {
			if m["kind"] == "Tenant" {
				asMap(asMap(asMap(m["spec"])["budgets"])["lines"])["review"] = map[string]any{"month_eur": 5.0}
			}
		}
	})
	b.escalateOnce(t, b.agent, "u1", map[string]any{"category": "other", "subject": map[string]any{"kind": "self"}, "urgency": "normal", "note": "secret words"})
	alice := Operator{Principal: b.alice, Tenant: "demo"}
	list, err := b.c.EscalationList(ctx, alice, TenantRequest{})
	if err != nil || len(listIDs(list)) != 1 {
		t.Fatalf("list %v %v", list, err)
	}
	validOut(t, b, "p2-list-response", list)
	id := listIDs(list)[0]
	out, err := b.c.EscalationShow(ctx, alice, ObjectRequest{Op: "escalation.show", ID: id})
	if err != nil || out["note"] != nil {
		t.Fatalf("alice: %v %v", out, err)
	}
	esc := out["escalation"].(map[string]any)
	if esc["note_ref"] != id || esc["category"] != "other" {
		t.Fatalf("record %v", esc)
	}
	if err := b.c.spec.Validate("urn:botaru:schema:escalation:1", esc); err != nil {
		t.Fatalf("record: %v", err)
	}
	// A credential without an active person, farm and another tenant see nothing; a tenant
	// named otherwise is forbidden, also for farm.
	if l, err := b.c.EscalationList(ctx, b.op, TenantRequest{}); err != nil || len(l["items"].([]any)) != 0 {
		t.Fatalf("a credential without a person lists %v %v", l, err)
	}
	for _, op := range []Operator{b.op, {Principal: b.op.Principal, Tenant: "farm"}, {Principal: b.alice, Tenant: "demo2"}} {
		if _, err := b.c.EscalationShow(ctx, op, ObjectRequest{ID: id}); reasonOf(err) != "not_found" {
			t.Fatalf("%+v shows: %v", op, err)
		}
	}
	for _, op := range []Operator{{Principal: b.op.Principal, Tenant: "farm"}, {Principal: b.alice, Tenant: "demo2"}} {
		if _, err := b.c.EscalationList(ctx, op, TenantRequest{Tenant: "demo"}); reasonOf(err) != "forbidden" {
			t.Fatalf("%+v lists demo: %v", op, err)
		}
	}
	auditor := Operator{Principal: b.addPerson(t, "telegram:1006", "auditors"), Tenant: "demo"}
	out, err = b.c.EscalationShow(ctx, auditor, ObjectRequest{ID: id})
	if err != nil {
		t.Fatal(err)
	}
	if note := asString(out["note"]); !strings.HasPrefix(note, "<untrusted-") || !strings.Contains(note, "secret words") {
		t.Fatalf("note %q", note)
	}
	if b.auditCount(t, "escalation.show", id, true) != 1 {
		t.Fatal("the note read is not audited")
	}
	// Without its source task the note is withheld.
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE escalations SET task = 't_01JB0000000000000000000099' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if out, err = b.c.EscalationShow(ctx, auditor, ObjectRequest{ID: id}); err != nil || out["note"] != nil {
		t.Fatalf("without the source task %v %v", out, err)
	}
}

// "Cancel" of a proposed campaign rejects it; the same cancel again answers the same, while a
// cancel by another person of the ended campaign is already_terminal (ORC-1, IDM-3).
func TestP2CampaignCancelProposed(t *testing.T) {
	ctx := context.Background()
	b := newCampaignReadBench(t, map[string]any{"confirm_read_over": map[string]any{"items": 2}})
	att, h := b.attemptTools(t, "1", false)
	r := proposeCall(t, b, h, map[string]any{
		"template": map[string]any{"kind": "task", "type": "hello.child.v1", "goal": "Check the machine."},
		"items":    map[string]any{"resources": []any{"web1", "web2", "web3"}},
	})
	if r.State != "proposed" {
		t.Fatalf("propose %+v", r)
	}
	b.finish(t, att)
	alice := Operator{Principal: b.alice, Tenant: "demo"}
	for range 2 {
		if res, err := b.c.CampaignControlP2(ctx, alice, P2CampaignControlRequest{ID: r.Campaign, Command: "cancel"}); err != nil || res.State != "rejected" {
			t.Fatalf("cancel %+v %v", res, err)
		}
	}
	bob := Operator{Principal: b.addPerson(t, "telegram:1002", "hello"), Tenant: "demo"}
	if _, err := b.c.CampaignControlP2(ctx, bob, P2CampaignControlRequest{ID: r.Campaign, Command: "cancel"}); reasonOf(err) != "already_terminal" {
		t.Fatalf("another person's cancel: %v", err)
	}
}
