// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/ids"
)

// addApprover adds a person with the identity telegram:<n> to the approvers group of the
// action bench.
func (b *actionBench) addApprover(t *testing.T, n string) string {
	t.Helper()
	ctx := context.Background()
	p := ids.PrincipalID()
	if _, err := b.c.Principal(ctx, b.op, PrincipalRequest{Op: "principal.add", Subject: p, Identity: "telegram:" + n}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.c.st.Pool.Exec(ctx, `INSERT INTO memberships (tenant, principal, grp, state, created_at) VALUES ('demo', $1, 'approvers', 'active', now())`, p); err != nil {
		t.Fatal(err)
	}
	return p
}

// pendingCards requests fix_web on web1 from a finished attempt and returns the approval and
// the cards that went out.
func (b *actionBench) pendingCards(t *testing.T, update string) (*approvalRow, []sent) {
	t.Helper()
	att, h := b.attemptTools(t, update, false)
	r := b.request(t, h, map[string]any{"action": "fix_web", "args": map[string]any{"host": "web1"}})
	if r.Code != "pending" {
		t.Fatalf("request %+v", r)
	}
	b.finish(t, att)
	b.tick(t, "approval-expiry")
	return b.approval(t, r.ApprovalID), b.drain(t)
}

// "Details" before the listed decisions (APR-14): the rows with details_required of the
// registry say which decisions need it, and the check refuses such a decision with
// details_not_opened until the person opened "Details" of the subject's card.
func TestDetailsRequired(t *testing.T) {
	ctx := context.Background()
	b := newBench(t)
	for _, x := range []struct {
		card, decision string
		want           bool
	}{
		{"proposal_sample_card", "override_veto", true}, {"proposal_sample_card", "accept", false},
		{"promotion_card", "confirm", true}, {"promotion_card", "reject", false},
		{"attribution_card", "yes", true}, {"attribution_card", "no", true}, {"attribution_card", "unknown", false},
		{approvalCard, "approve", false}, {"no_such_card", "approve", false},
	} {
		if got := b.c.detailsRequired(x.card, x.decision); got != x.want {
			t.Errorf("%s %s: %v", x.card, x.decision, got)
		}
	}
	check := func() error {
		var out error
		err := b.c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
			out = b.c.requireDetails(ctx, tx, now, "demo", "proposal", "prp_1", "proposal_sample_card", "override_veto", b.alice)
			if c, ok := out.(*committed); ok {
				out = c.p
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	wantReason(t, check(), "details_not_opened")
	if _, err := b.c.st.Pool.Exec(ctx, `INSERT INTO card_views (subject_kind, subject_id, principal, opened_at) VALUES ('proposal', 'prp_1', $1, now())`, b.alice); err != nil {
		t.Fatal(err)
	}
	if err := check(); err != nil {
		t.Fatalf("after Details: %v", err)
	}
}

// Two approvers (APR-16): the first "Approve" is a vote that keeps the request pending and
// shows on the card, a repeat changes nothing, the initiator is never an approver, the second
// different person executes it with one write call; one "Reject" closes another request.
func TestTwoApprovers(t *testing.T) {
	ctx := context.Background()
	b := newActionBench(t, func(ms []map[string]any) []map[string]any {
		for _, m := range ms {
			if m["kind"] == "Action" {
				m["spec"].(map[string]any)["approvals"] = 2
			}
		}
		return ms
	})
	carol := b.addApprover(t, "1003")
	a, cards := b.pendingCards(t, "1")
	if a.Approvals != 2 || a.Record()["approvals"] != 2 {
		t.Fatalf("approvals %d", a.Approvals)
	}
	bobCard, carolCard := cardOf(t, cards, approvalCard, "direct:1002"), cardOf(t, cards, approvalCard, "direct:1003")
	req := DecideRequest{SubjectKind: "approval", ID: a.ID, Nonce: a.Nonce, Decision: "approve"}
	res, err := b.c.Decide(ctx, b.channel, press("telegram:1002", "direct:1002", bobCard.external), req)
	if err != nil || res.State != "pending" {
		t.Fatalf("the first approval %+v %v", res, err)
	}
	if again, err := b.c.Decide(ctx, b.channel, press("telegram:1002", "direct:1002", bobCard.external), req); err != nil || again.State != "pending" {
		t.Fatalf("a repeat of the first approval %+v %v", again, err)
	}
	if a = b.approval(t, a.ID); a.State != "pending" || len(a.Votes) != 1 || a.Votes[0].By != b.bob {
		t.Fatalf("after one vote %s %+v", a.State, a.Votes)
	}
	validOut(t, b.bench, "approval", a.Record())
	upd := cardOf(t, b.drain(t), cardUpdate, "direct:1003")
	if by := upd.body["card"].(map[string]any)["approved_by"].([]any); len(by) != 1 || by[0] != b.bob {
		t.Fatalf("the card shows %v", upd.body["card"])
	}
	res, err = b.c.Decide(ctx, b.channel, press("telegram:1003", "direct:1003", carolCard.external), req)
	if err != nil || res.State != "executing" {
		t.Fatalf("the second approval %+v %v", res, err)
	}
	var calls int
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM toolcalls WHERE approval_id = $1`, a.ID).Scan(&calls); err != nil || calls != 1 {
		t.Fatalf("write calls %d %v", calls, err)
	}

	a, cards = b.pendingCards(t, "2")
	req = DecideRequest{SubjectKind: "approval", ID: a.ID, Nonce: a.Nonce, Decision: "approve"}
	if _, err := b.c.Decide(ctx, b.channel, press("telegram:1002", "direct:1002", cardOf(t, cards, approvalCard, "direct:1002").external), req); err != nil {
		t.Fatal(err)
	}
	req.Decision = "reject"
	res, err = b.c.Decide(ctx, b.channel, press("telegram:1003", "direct:1003", cardOf(t, cards, approvalCard, "direct:1003").external), req)
	if err != nil || res.State != "rejected" {
		t.Fatalf("a reject after one approval %+v %v", res, err)
	}
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM toolcalls WHERE approval_id = $1`, a.ID).Scan(&calls); err != nil || calls != 0 {
		t.Fatalf("write calls after reject %d %v", calls, err)
	}
	_ = carol
}

// The apply lint and doctor of two approvers (APR-16): a production tenant whose group may
// request a high risk action with one approval is refused; with approvals: 2 the set passes,
// and doctor warns while fewer than two people other than the initiator may approve.
func TestTwoApproversLintDoctor(t *testing.T) {
	ctx := context.Background()
	b := newActionBench(t, func(ms []map[string]any) []map[string]any {
		for _, m := range ms {
			if m["kind"] == "Action" {
				m["spec"].(map[string]any)["risk"] = "high"
				m["spec"].(map[string]any)["approvals"] = 2
			}
		}
		return ms
	})
	action := map[string]any{"apiVersion": "botaru/v1alpha1", "kind": "Action", "metadata": map[string]any{"name": "db_restore"},
		"spec": map[string]any{"title": "Restore", "description": "Restores the database.", "risk": "high", "approvals": 1,
			"args": map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{}}, "ttl": "60m",
			"executor": map[string]any{"plugin": "db", "tool": "restore", "os_mechanism": map[string]any{
				"oneshot": map[string]any{"unit": "db-restore-{tenant}.service", "started_by": "db-restore-{tenant}.path", "wait": false}}}}}
	set := []map[string]any{
		{"apiVersion": "botaru/v1alpha1", "kind": "Tenant", "metadata": map[string]any{"name": "demo2"},
			"spec": map[string]any{"kind": "production", "farm_manager": map[string]any{"enabled": false}, "budgets": map[string]any{"month_eur": 60}}},
		{"apiVersion": "botaru/v1alpha1", "kind": "Group", "metadata": map[string]any{"name": "ops", "tenant": "demo2"},
			"spec": map[string]any{"caps": []any{"action.request:db_restore"}}},
		action,
	}
	farm := Operator{Principal: b.op.Principal, Tenant: "farm"}
	// The lint is of a production tenant, which a test bench store never holds (APR-9).
	b.c.cfg.TestBench = false
	defer func() { b.c.cfg.TestBench = true }()
	res, err := b.c.Apply(ctx, farm, ApplyRequest{Commit: "2222222222222222222222222222222222222222", Manifests: set, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	codes := func(res *ApplyResult) map[string]bool {
		out := map[string]bool{}
		for _, e := range res.Errors {
			out[e.Code] = true
		}
		return out
	}
	if !codes(res)["high_risk_one_approver"] {
		t.Fatalf("errors %+v", res.Errors)
	}
	action["spec"].(map[string]any)["approvals"] = 2
	if res, err = b.c.Apply(ctx, farm, ApplyRequest{Commit: "3333333333333333333333333333333333333333", Manifests: set, DryRun: true}); err != nil || codes(res)["high_risk_one_approver"] {
		t.Fatalf("approvals 2: %+v %v", res, err)
	}
	// The bench tenant is of kind test; doctor counts as for a production one.
	short := func() any {
		cat, err := b.c.Catalog(ctx)
		if err != nil {
			t.Fatal(err)
		}
		tv := *cat.Tenants["demo"]
		tv.Spec.Kind = "production"
		prod := *cat
		prod.Tenants = map[string]*TenantView{"demo": &tv}
		var got []DoctorCheck
		if err := b.c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
			return b.c.doctorTwoApprovers(ctx, tx, now, &prod, "demo", func(ch DoctorCheck) { got = append(got, ch) })
		}); err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].ID != "approvals.two_approvers_short" {
			t.Fatalf("checks %+v", got)
		}
		return got[0].Value
	}
	if v := short(); v != 1 {
		t.Fatalf("one approver: %v", v)
	}
	b.addApprover(t, "1003")
	if v := short(); v != 0 {
		t.Fatalf("two approvers: %v", v)
	}
}

// A pause of approvals set by a person (HLD-8): a person with hold.set:role sets pause_action
// on the role; the card gets card.paused, a press gets held and changes nothing, the deadline
// does not pass while the hold lasts; when it ends the deadline moves by the time it was
// paused and the card gets the new deadline. no_action wins over pause_action, and a pause
// without until lets an approval wait at most 7 days.
func TestPauseActionByPerson(t *testing.T) {
	ctx := context.Background()
	b := newActionBench(t, func(ms []map[string]any) []map[string]any {
		return append(ms, map[string]any{
			"apiVersion": "botaru/v1alpha1", "kind": "Group", "metadata": map[string]any{"name": "pausers", "tenant": "demo"},
			"spec": map[string]any{"caps": []any{"hold.set:role", "hold.set:host", "hold.release"}, "data": []any{"summary"}},
		})
	})
	carol := ids.PrincipalID()
	if _, err := b.c.Principal(ctx, b.op, PrincipalRequest{Op: "principal.add", Subject: carol, Identity: "telegram:1003"}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.c.st.Pool.Exec(ctx, `INSERT INTO memberships (tenant, principal, grp, state, created_at) VALUES ('demo', $1, 'pausers', 'active', now())`, carol); err != nil {
		t.Fatal(err)
	}
	person := HoldActor{Principal: carol, Rank: "tenant_admin", Tenant: "demo", Via: "channel"}
	a, cards := b.pendingCards(t, "1")
	card := cardOf(t, cards, approvalCard, "direct:1002")
	b.advance(t, 40*60) // 20 minutes of the 60 are left
	until := b.now(t).Add(30 * time.Minute).UTC().Format(time.RFC3339Nano)
	if _, err := b.c.HoldSet(ctx, person, HoldSetRequest{Scope: HoldScope{Kind: "channel", Tenant: "demo", Name: "telegram"},
		Effects: []string{"pause_action"}, Reason: "manual"}); err == nil {
		t.Fatal("pause_action on a scope without approvals")
	}
	if _, err := b.c.HoldSet(ctx, person, HoldSetRequest{Scope: HoldScope{Kind: "role", Tenant: "demo", Name: "hello"},
		Effects: []string{"pause_action"}, Reason: "manual", Until: &until}); err != nil {
		t.Fatal(err)
	}
	b.tick(t, "approval-expiry")
	upd := cardOf(t, b.drain(t), cardUpdate, "direct:1002")
	if st := upd.body["card"].(map[string]any)["state"]; st != "paused" {
		t.Fatalf("the paused card %v", upd.body["card"])
	}
	req := DecideRequest{SubjectKind: "approval", ID: a.ID, Nonce: a.Nonce, Decision: "approve"}
	_, err := b.c.Decide(ctx, b.channel, press("telegram:1002", "direct:1002", card.external), req)
	wantReason(t, err, "held")
	b.advance(t, 25*60) // past the old deadline, inside the hold
	b.tick(t, "approval-expiry")
	if got := b.approval(t, a.ID); got.State != "pending" {
		t.Fatalf("a paused approval expired: %s", got.State)
	}
	b.advance(t, 10*60) // the hold has ended
	b.tick(t, "hold")
	b.tick(t, "approval-expiry")
	got := b.approval(t, a.ID)
	if moved := got.ExpiresAt.Sub(a.ExpiresAt); got.State != "pending" || moved < 29*time.Minute || moved > 31*time.Minute {
		t.Fatalf("after the pause: %s, moved %v", got.State, moved)
	}
	if upd := cardOf(t, b.drain(t), cardUpdate, "direct:1002"); upd.body["card"].(map[string]any)["state"] != "pending" {
		t.Fatalf("the card after the pause %v", upd.body["card"])
	}
	// no_action wins: the pending approval under both holds is canceled.
	if _, err := b.c.HoldSet(ctx, person, HoldSetRequest{Scope: HoldScope{Kind: "role", Tenant: "demo", Name: "hello"},
		Effects: []string{"pause_action"}, Reason: "manual"}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.c.HoldSet(ctx, OperatorActor(b.op), HoldSetRequest{Scope: HoldScope{Kind: "action", Tenant: "demo", Name: "fix_web"},
		Effects: []string{"no_action"}, Reason: "manual"}); err != nil {
		t.Fatal(err)
	}
	b.tick(t, "approval-expiry")
	if got := b.approval(t, a.ID); got.State != "canceled" || got.Reason != "held" {
		t.Fatalf("under no_action %s %s", got.State, got.Reason)
	}
}

// A pause_action hold without until extends an approval by at most 7 days: after 7 days under
// the hold the approval-expiry loop expires it (HLD-8, D-108); a new request under the hold
// is refused (HLD-3).
func TestPauseActionAtMostSevenDays(t *testing.T) {
	ctx := context.Background()
	b := newActionBench(t, nil)
	a, _ := b.pendingCards(t, "1")
	if _, err := b.c.HoldSet(ctx, OperatorActor(b.op), HoldSetRequest{Scope: HoldScope{Kind: "host", Tenant: "demo", Name: "web1"},
		Effects: []string{"pause_action"}, Reason: "manual"}); err != nil {
		t.Fatal(err)
	}
	_, h := b.attemptTools(t, "2", false)
	if r := b.request(t, h, map[string]any{"action": "fix_web", "args": map[string]any{"host": "web1", "level": "hard"}}); r.Code != "forbidden" {
		t.Fatalf("a request under pause_action on its host: %+v", r)
	}
	b.tick(t, "approval-expiry")
	b.advance(t, 6*24*3600)
	b.tick(t, "approval-expiry")
	if got := b.approval(t, a.ID); got.State != "pending" {
		t.Fatalf("after 6 days: %s", got.State)
	}
	b.advance(t, 24*3600+60)
	b.tick(t, "approval-expiry")
	if got := b.approval(t, a.ID); got.State != "expired" {
		t.Fatalf("after 7 days: %s", got.State)
	}
}
