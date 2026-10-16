// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/ids"
	"github.com/HunterEwok/botaru/impl/internal/spec"
	"github.com/HunterEwok/botaru/impl/internal/texts"
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
	// A reminder goes only to the approver who has not voted.
	b.alarmTx(t, func(tx pgx.Tx, now time.Time) error {
		cat, err := b.c.Catalog(ctx)
		if err != nil {
			return err
		}
		if n, err := b.c.remindApprovers(ctx, tx, now, cat.Tenants["demo"], a); err != nil || n != 1 {
			t.Fatalf("reminded %d %v", n, err)
		}
		return nil
	})
	if got := b.drain(t); slices.ContainsFunc(got, func(s sent) bool { return s.chat == "direct:1002" && s.kind == approvalCard }) {
		t.Fatalf("a voter was reminded %+v", got)
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

// The card limit per person per day (APR-15): with noncritical_per_day 2 and an urgent card
// the person already got today, one memory record card goes out and the others wait in
// planned until the next day, counted as excess without names; the card that went out counts
// its deadline from its first delivery; the next day the waiting cards go in order of expiry.
func TestCardLimitPerDay(t *testing.T) {
	ctx := context.Background()
	b := newBenchWith(t, func(root string, ms []map[string]any) []map[string]any {
		for _, m := range ms {
			if m["kind"] == "Tenant" {
				m["spec"].(map[string]any)["attention"] = map[string]any{"noncritical_per_day": 2}
			}
		}
		return ms
	})
	storedPersonMemory(t, b)
	b.hello(t, "1")
	att := b.claim(t, b.agent, "attempt")
	mh := att.Payload["handles"].(map[string]any)["memory"].(string)
	b.drain(t) // the notices of the first contact
	// An urgent card alice got today counts toward the limit.
	if _, err := b.c.st.Pool.Exec(ctx, `INSERT INTO deliveries (id, tenant, kind, addr, target_kind, target_id, exec_kind, exec_name, recipient, state, dataorigin,
		created_at, updated_at, handed_at) VALUES ('dl_urgent', 'demo', 'approval_card', 'approval_initiator', 'chat', 'direct:1001', 'channel', 'telegram', $1,
		'delivered', 'code', botaru_now(), botaru_now(), botaru_now())`, b.alice); err != nil {
		t.Fatal(err)
	}
	var first string
	for i, k := range []string{"pref.a", "pref.b", "pref.c"} {
		p, err := b.c.MemoryPropose(ctx, mh, MemoryProposeRequest{Key: k, Content: "Prefers " + k})
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = p.ID
		}
		b.advance(t, 1)
	}
	if got := b.drain(t); len(got) != 1 || got[0].kind != memoryCard {
		t.Fatalf("cards today %+v", got)
	}
	var waiting, excess int
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM deliveries WHERE kind = $1 AND state = 'planned' AND run_at IS NOT NULL`, memoryCard).Scan(&waiting); err != nil || waiting != 2 {
		t.Fatalf("waiting %d %v", waiting, err)
	}
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT sum(count) FROM card_excess WHERE tenant = 'demo'`).Scan(&excess); err != nil || excess != 2 {
		t.Fatalf("excess %d %v", excess, err)
	}
	var created, decideBy, handed time.Time
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT m.created_at, m.decide_by, f.delivered_at FROM memory_items m JOIN card_first_deliveries f
		ON f.subject_kind = 'memory_record' AND f.subject_id = m.id WHERE m.id = $1`, first).Scan(&created, &decideBy, &handed); err != nil {
		t.Fatal(err)
	}
	if got := decideBy.Sub(handed); got != memoryCardTTL {
		t.Fatalf("the deadline counts %v from the first delivery", got)
	}
	// A record whose card waits does not expire before its card may go.
	var early int
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM memory_items m WHERE m.id <> $1 AND m.decide_by IS NOT NULL
		AND m.decide_by - m.created_at <= $2::interval`, first, memoryCardTTL).Scan(&early); err != nil || early != 0 {
		t.Fatalf("waiting records with the old deadline %d %v", early, err)
	}
	b.advance(t, 24*3600)
	if got := b.drain(t); len(got) != 2 {
		t.Fatalf("cards the next day %+v", got)
	}
	_ = created
}

// A person's quiet hours and daily digest defer only deliveries of a deferrable kind; an admin
// without admin:people cannot set another person's (DLV-10).
func TestPrefsQuietHours(t *testing.T) {
	ctx := context.Background()
	b := newActionBench(t, nil)
	b.drain(t)
	cat, err := b.c.Catalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	loc := tenantLocation(cat, "demo")
	now := b.now(t).In(loc)
	from, to := now.Add(-time.Hour).Format("15:04"), now.Add(time.Hour).Format("15:04")
	if res := b.say(t, "p1", "telegram:1001", "1001", "/prefs quiet "+from+"-"+to); !strings.Contains(res.Text, "Quiet hours: "+from+"-"+to) {
		t.Fatalf("/prefs %q", res.Text)
	}
	if res := b.say(t, "p2", "telegram:1001", "1001", "/prefs telegram:1002 digest daily"); !strings.Contains(res.Text, "group") {
		t.Fatalf("/prefs of another person %q", res.Text)
	}
	for _, k := range []string{"digest", "alarm_notice"} {
		if _, err := b.c.st.Pool.Exec(ctx, `INSERT INTO deliveries (id, tenant, kind, addr, target_kind, target_id, exec_kind, exec_name, recipient, state, dataorigin,
			body, created_at, updated_at) VALUES ('dl_'||$2, 'demo', $2, 'x', 'chat', 'direct:1001', 'channel', 'telegram', $1, 'planned', 'code', '{}', botaru_now(), botaru_now())`,
			b.alice, k); err != nil {
			t.Fatal(err)
		}
	}
	if got := b.drain(t); len(got) == 0 || got[len(got)-1].kind != "alarm_notice" || slices.ContainsFunc(got, func(s sent) bool { return s.kind == "digest" }) {
		t.Fatalf("inside quiet hours %+v", got)
	}
	b.advance(t, 3700)
	if got := b.drain(t); len(got) != 1 || got[0].kind != "digest" {
		t.Fatalf("after quiet hours %+v", got)
	}
}

// The language of a delivery and the reply_locale of an attempt follow the order of DLV-18:
// the recipient's own locale with a catalog, then the tenant's, then en.
func TestLocaleOrder(t *testing.T) {
	ctx := context.Background()
	b := newBenchWith(t, func(root string, ms []map[string]any) []map[string]any {
		for _, m := range ms {
			if m["kind"] == "Tenant" {
				m["spec"].(map[string]any)["locale"] = "et"
			}
		}
		return ms
	})
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "et.yaml"), []byte("locale: et\ntexts:\n  notice.ai: Tere.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, _ := spec.FindRoot("")
	tx, err := texts.Load(root, dir)
	if err != nil {
		t.Fatal(err)
	}
	b.c.texts = tx
	// A locale without a catalog is passed over.
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE principals SET locale = 'ru' WHERE id = $1`, b.alice); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"et", "en"} {
		b.hello(t, "lo-"+want)
		if want == "et" {
			// The notices of the first contact.
			d := b.claim(t, b.channel, "delivery")
			if d == nil || d.Payload["locale"] != want {
				t.Fatalf("delivery locale %v, want %s", d, want)
			}
			b.drain(t)
		}
		att := b.claim(t, b.agent, "attempt")
		if att == nil || att.Payload["reply_locale"] != want {
			t.Fatalf("reply_locale %v, want %s", att, want)
		}
		if _, err := b.c.st.Pool.Exec(ctx, `UPDATE principals SET locale = 'en' WHERE id = $1`, b.alice); err != nil {
			t.Fatal(err)
		}
	}
}

// Digests to one chat go out at most max_per_hour times an hour, 4 for a private chat by
// default; one over the ceiling waits for the next free slot, and a newer one replaces the
// waiting one (REP-10).
func TestDigestPace(t *testing.T) {
	ctx := context.Background()
	b := newBench(t)
	ins := func(id string, handed, runAt any, state string) {
		t.Helper()
		if _, err := b.c.st.Pool.Exec(ctx, `INSERT INTO deliveries (id, tenant, kind, addr, target_kind, target_id, exec_kind, exec_name, state, dataorigin,
			body, created_at, updated_at, handed_at, run_at) VALUES ($1, 'demo', 'digest', 'origin', 'chat', 'direct:1001', 'channel', 'telegram', $4, 'code', '{}',
			botaru_now(), botaru_now(), $2, $3)`, id, handed, runAt, state); err != nil {
			t.Fatal(err)
		}
	}
	now := b.now(t)
	for i := range 4 {
		ins(fmt.Sprintf("dl_d%d", i), now.Add(-time.Duration(50-i*10)*time.Minute), nil, "delivered")
	}
	pace := func() (*time.Time, string) {
		t.Helper()
		var out *time.Time
		var replaced string
		if err := b.c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
			var err error
			out, replaced, err = b.c.digestPace(ctx, tx, now, "demo", "telegram", "direct:1001")
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return out, replaced
	}
	slot, _ := pace()
	if slot == nil || !slot.Equal(now.Add(10*time.Minute)) {
		t.Fatalf("next free slot %v", slot)
	}
	ins("dl_wait", nil, *slot, "planned")
	// One the person's preferences deferred is never replaced at planning (DLV-10).
	ins("dl_quiet", nil, slot.Add(time.Minute), "planned")
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE deliveries SET paced = true WHERE id = 'dl_wait'`); err != nil {
		t.Fatal(err)
	}
	if again, replaced := pace(); again == nil || !again.Equal(*slot) || replaced != "dl_wait" {
		t.Fatalf("the newer digest takes the slot %v of %q", again, replaced)
	}
	// At hand-out the ceiling holds again: of two due digests the older is replaced and the
	// newer waits for the next free slot (REP-10).
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE deliveries SET state = 'withheld' WHERE id IN ('dl_wait', 'dl_quiet')`); err != nil {
		t.Fatal(err)
	}
	ins("dl_due1", nil, nil, "planned")
	ins("dl_due2", nil, nil, "planned")
	b.drain(t)
	got := map[string]string{}
	rows, err := b.c.st.Pool.Query(ctx, `SELECT id, state || coalesce('/' || reason, '') || CASE WHEN paced THEN '/paced' ELSE '' END FROM deliveries WHERE id LIKE 'dl_due%'`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id, st string
		if err := rows.Scan(&id, &st); err != nil {
			t.Fatal(err)
		}
		got[id] = st
	}
	rows.Close()
	if got["dl_due1"] != "withheld/superseded" || got["dl_due2"] != "planned/paced" {
		t.Fatalf("hand-out over the ceiling %v", got)
	}
	// A requeued digest does not count its own earlier hand-out against the ceiling.
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE deliveries SET state = CASE WHEN id = 'dl_d3' THEN 'planned' ELSE 'withheld' END
		WHERE id IN ('dl_d3', 'dl_due2')`); err != nil {
		t.Fatal(err)
	}
	if out := b.drain(t); len(out) != 1 {
		t.Fatalf("the requeued digest %+v", out)
	}
}

// Within a class the next task comes from the fairness key with the fewest running attempts
// per weight; a key of weight 0 waits while another key has work (DSP-6).
func TestFairnessWeights(t *testing.T) {
	ctx := context.Background()
	b := newBenchWith(t, func(root string, ms []map[string]any) []map[string]any {
		for _, m := range ms {
			if m["kind"] == "Tenant" {
				d, _ := m["spec"].(map[string]any)["dispatch"].(map[string]any)
				if d == nil {
					d = map[string]any{}
					m["spec"].(map[string]any)["dispatch"] = d
				}
				d["fairness"] = map[string]any{"weights": map[string]any{"campaign": 0}}
			}
		}
		return ms
	})
	b.hello(t, "f1")
	b.advance(t, 1)
	a2 := b.hello(t, "f2")
	b.advance(t, 1)
	other := b.hello(t, "f3")
	b.advance(t, 1)
	idle := b.hello(t, "f4")
	for id, key := range map[string]string{other: "system:work", idle: "campaign:c1"} {
		if _, err := b.c.st.Pool.Exec(ctx, `UPDATE tasks SET record = record || jsonb_build_object('fairness_key', $2::text) WHERE id = $1`, id, key); err != nil {
			t.Fatal(err)
		}
	}
	b.drain(t)
	var got []string
	for range 4 {
		att := b.claim(t, b.agent, "attempt")
		if att == nil {
			t.Fatal("no attempt")
		}
		got = append(got, att.Payload["task"].(map[string]any)["id"].(string))
	}
	// alice's first, then the key without a running attempt, then alice's second, the weight 0 key last.
	if got[1] != other || got[2] != a2 || got[3] != idle {
		t.Fatalf("order %v, want [_ %s %s %s]", got, other, a2, idle)
	}
}

// Dead work comes in groups by (tenant, type, reason); a redrive of a task whose body its
// period deleted answers gone; three slots of a schedule in a row out of attempts stop the
// schedule with a hold, and the next slot is skipped as held (CAP-4).
func TestDeadGroupsScheduleAutoStop(t *testing.T) {
	ctx := context.Background()
	b := newScheduleBench(t, "30 2 * * *", nil)
	t1, t2 := b.hello(t, "g1"), b.hello(t, "g2")
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE tasks SET state = 'failed', reason = 'attempts_exhausted' WHERE id = ANY($1)`, []string{t1, t2}); err != nil {
		t.Fatal(err)
	}
	list, err := b.c.DeadletterList(ctx, b.op, TenantRequest{})
	if err != nil {
		t.Fatal(err)
	}
	validOut(t, b.bench, "p2-deadletter-list-response", list)
	if len(list.Groups) != 1 || list.Groups[0].Count != 2 || list.Groups[0].Type != "hello.greet.v1" || list.Groups[0].Reason != "attempts_exhausted" {
		t.Fatalf("groups %+v", list.Groups)
	}
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE tasks SET body = NULL WHERE id = $1`, t1); err != nil {
		t.Fatal(err)
	}
	_, err = b.c.Redrive(ctx, b.op, RedriveRequest{Op: "task.redrive", ID: t1})
	wantReason(t, err, "gone")

	b.at(t, "2027-03-27T12:00:00Z")
	b.tick(t, "schedule") // the first sight writes the schedule's state
	rec := []byte(`{"enqueued_by": "schedule:morning"}`)
	for i, reason := range []string{"attempts_exhausted", "completed", "runs_exhausted", "attempts_exhausted", "attempts_exhausted"} {
		b.alarmTx(t, func(tx pgx.Tx, now time.Time) error {
			return b.c.scheduleSlotEnded(ctx, tx, now, "demo", fmt.Sprintf("t_slot%d", i), rec, reason)
		})
	}
	var n int
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM holds WHERE scope_kind = 'schedule' AND scope_name = 'morning' AND reason = 'schedule_failing'
		AND state = 'active' AND until IS NULL AND 'no_enqueue' = ANY(effects)`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("auto-stop holds %d %v", n, err)
	}
	b.at(t, "2027-03-29T00:31:00Z")
	b.tick(t, "schedule")
	if s := b.skips(t); len(s) == 0 || !strings.HasSuffix(s[len(s)-1], " held") {
		t.Fatalf("skips %v", s)
	}
}

// Before "Accept" of a schedule change code finds the local times that disappear or repeat at
// the transitions of the zone within a year, and a backup schedule that would lose a slot is
// rejected(schedule_time_invalid) (TIM-5).
func TestScheduleChangeTimes(t *testing.T) {
	ctx := context.Background()
	b := newScheduleBench(t, "0 4 * * *", nil)
	cat, err := b.c.Catalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tv := cat.Tenants["demo"]
	now := time.Date(2027, 1, 10, 12, 0, 0, 0, time.UTC)
	op := manifestOp{Op: "set", Field: "spec.cron", Value: "30 2 * * *"}
	op.Target.Kind, op.Target.Name = "Schedule", "morning"
	times, err := b.c.checkScheduleTimes(now, cat, tv, "morning", []manifestOp{op})
	if err != nil {
		t.Fatal(err)
	}
	if d, r := times["times_disappear"], times["times_repeat"]; fmt.Sprint(d) != "[2027-03-28 02:30]" || fmt.Sprint(r) != "[2027-10-31 02:30]" {
		t.Fatalf("times %v", times)
	}
	clear := op
	clear.Value = "0 5 * * *"
	if times, _ := b.c.checkScheduleTimes(now, cat, tv, "morning", []manifestOp{clear}); times != nil {
		t.Fatalf("a time clear of the transitions %v", times)
	}
	// A zone apply does not take, and both ops of one proposal checked together.
	zone := op
	zone.Field, zone.Value = "spec.timezone", "Local"
	if _, err := b.c.checkScheduleTimes(now, cat, tv, "morning", []manifestOp{zone}); err != rejection("forbidden_change") {
		t.Fatalf("zone Local: %v", err)
	}
	zone.Value = "UTC"
	if times, _ := b.c.checkScheduleTimes(now, cat, tv, "morning", []manifestOp{op, zone}); times != nil {
		t.Fatalf("a cron in UTC has no transitions %v", times)
	}
	backup := *cat
	backup.TaskTypes = maps.Clone(cat.TaskTypes)
	tt := backup.TaskTypes["hello.greet.v1"]
	tt.MakesBackup = true
	backup.TaskTypes["hello.greet.v1"] = tt
	if _, err = b.c.checkScheduleTimes(now, &backup, tv, "morning", []manifestOp{op}); err != rejection("schedule_time_invalid") {
		t.Fatalf("a backup schedule that loses a slot: %v", err)
	}
}

// A campaign approval is one press, so an action with approvals: 2 is not run as a campaign
// (APR-16).
func TestTwoApproversNoCampaign(t *testing.T) {
	b := newActionBench(t, func(ms []map[string]any) []map[string]any {
		campaignHosts(ms)
		for _, m := range ms {
			s := m["spec"].(map[string]any)
			switch {
			case m["kind"] == "Role":
				s["profile"] = "manager"
				s["campaigns"] = map[string]any{"propose": "action"}
			case m["kind"] == "Action" && m["metadata"].(map[string]any)["name"] == "fix_web":
				s["approvals"] = 2
				s["campaign"] = map[string]any{"allowed": true, "max_items": 3, "max_in_flight": 1, "canary_min": 1, "after_canary": "manual", "max_failures_max": 0}
			}
		}
		return ms
	})
	_, h := b.attemptTools(t, "1", false)
	if !containsString(toolNames(t, b.bench, h), proposeCampaignName) {
		return
	}
	tmpl := map[string]any{"kind": "action", "action": "fix_web", "args": map[string]any{}}
	if r := proposeCall(t, b.bench, h, map[string]any{"template": tmpl, "items": map[string]any{"resources": []any{"web1"}}}); r.State != "rejected" {
		t.Fatalf("a campaign of a two approver action %+v", r)
	}
}

// A pause_action hold on a bot group of the worker that asked pauses its approvals (HLD-8).
func TestPauseActionBotGroup(t *testing.T) {
	ctx := context.Background()
	b := newActionBench(t, func(ms []map[string]any) []map[string]any {
		return append(ms, map[string]any{"apiVersion": "botaru/v1alpha1", "kind": "BotGroup", "metadata": map[string]any{"name": "pool", "tenant": "demo"},
			"spec": map[string]any{"select": map[string]any{"roles": []any{b0Role(ms)}}}})
	})
	a, _ := b.pendingCards(t, "1")
	if _, err := b.c.HoldSet(ctx, OperatorActor(b.op), HoldSetRequest{Scope: HoldScope{Kind: "bot_group", Tenant: "demo", Name: "pool"},
		Effects: []string{"pause_action"}, Reason: "manual"}); err != nil {
		t.Fatal(err)
	}
	b.tick(t, "approval-expiry")
	if got := b.approval(t, a.ID); got.PausedSince == nil {
		t.Fatalf("the approval under a bot group hold is not paused: %+v", got)
	}
}

// b0Role is the name of the first Role of a manifest set.
func b0Role(ms []map[string]any) string {
	for _, m := range ms {
		if m["kind"] == "Role" {
			return m["metadata"].(map[string]any)["name"].(string)
		}
	}
	return ""
}

// The expiry of a promotion case and of an attribution counts from the first delivery of
// their card (APR-15): the presentation (and the case's expiry with it) moves once, and
// self_measured_card, a later card on a presented proposal, has a mark of its own.
func TestFirstDeliveryGovernance(t *testing.T) {
	ctx := context.Background()
	b := newBench(t)
	t0 := time.Date(2026, 1, 5, 10, 0, 0, 0, time.UTC)
	if _, err := b.c.st.Pool.Exec(ctx, `INSERT INTO governance_cases (id, tenant, state, record, presented_at, expires_at, created_at, updated_at)
		VALUES ('gc_fd', 'demo', 'confirming', '{}', $1, $2, $1, $1)`, t0, t0.Add(7*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := b.c.st.Pool.Exec(ctx, `INSERT INTO attributions (id, tenant, incident, proposal, state, record, presented_at, created_at, updated_at)
		VALUES ('at_fd', 'demo', 'inc_fd', 'prp_fd', 'candidate', '{}', $1, $1, $1)`, t0); err != nil {
		t.Fatal(err)
	}
	at := t0.Add(30 * time.Hour)
	run := func(when time.Time, kind, sk, id string) {
		t.Helper()
		err := b.c.tx(ctx, func(tx pgx.Tx, _ time.Time) error { return b.c.firstCardDelivery(ctx, tx, when, kind, sk, id) })
		if err != nil {
			t.Fatal(err)
		}
	}
	run(at, promotionCard, "governance_case", "gc_fd")
	run(at, attributionCard, "attribution", "at_fd")
	// A second delivery changes nothing.
	run(at.Add(time.Hour), promotionCard, "governance_case", "gc_fd")
	run(at.Add(time.Hour), attributionCard, "attribution", "at_fd")
	var pres, exp, apres time.Time
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT presented_at, expires_at FROM governance_cases WHERE id = 'gc_fd'`).Scan(&pres, &exp); err != nil {
		t.Fatal(err)
	}
	if !pres.Equal(at) || !exp.Equal(at.Add(7*24*time.Hour)) {
		t.Fatalf("case presented %v expires %v", pres, exp)
	}
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT presented_at FROM attributions WHERE id = 'at_fd'`).Scan(&apres); err != nil {
		t.Fatal(err)
	}
	if !apres.Equal(at) {
		t.Fatalf("attribution presented %v", apres)
	}
	run(at, selfMeasuredCard, "proposal", "prp_fd")
	if n := b.count(t, `SELECT count(*) FROM card_first_deliveries WHERE subject_kind = 'proposal' AND subject_id = 'prp_fd'`); n != 0 {
		t.Fatalf("self_measured_card took the proposal's first delivery")
	}
	if n := b.count(t, `SELECT count(*) FROM card_first_deliveries WHERE subject_kind = 'self_measured_card' AND subject_id = 'prp_fd'`); n != 1 {
		t.Fatalf("self_measured_card has no mark of its own")
	}
}

// A promotion case with two recipients (APR-15, APR-14): the card of the one over the card
// limit is claimed first and waits for the next day, the other's goes now, and that person
// opens "Details" and confirms today: the first delivery sets the presentation to its time.
func TestCaseDeferredRecipient(t *testing.T) {
	ctx := context.Background()
	b, _, _ := realizedBench(t)
	bob := ids.PrincipalID()
	if _, err := b.c.Principal(ctx, b.op, PrincipalRequest{Op: "principal.add", Subject: bob, Identity: "telegram:1002"}); err != nil {
		t.Fatal(err)
	}
	b.exec(t, `INSERT INTO memberships (tenant, principal, grp, state, created_at) VALUES ('demo', $1, 'governors', 'active', now())`, bob)
	for i := range defaultNoncriticalPerDay {
		b.exec(t, `INSERT INTO deliveries (id, tenant, kind, addr, target_kind, target_id, exec_kind, exec_name, recipient, state, dataorigin,
			created_at, updated_at, handed_at) VALUES ($2, 'demo', 'approval_card', 'approval_initiator', 'chat', 'direct:1002', 'channel', 'telegram', $1,
			'delivered', 'code', botaru_now(), botaru_now(), botaru_now())`, bob, fmt.Sprintf("dl_bob%d", i))
	}
	b.loop(t, "governance")
	b.exec(t, `UPDATE attributions SET state = 'rejected', record = record || '{"state": "rejected"}'`)
	b.loop(t, "governance")
	// bob's card comes first in the queue.
	b.exec(t, `UPDATE deliveries SET seq = -seq WHERE recipient = $1 AND kind = 'promotion_card'`, bob)
	cd, on := b.govCard(t, promotionCard)
	if n := b.count(t, `SELECT count(*) FROM deliveries WHERE recipient = $1 AND kind = 'promotion_card' AND state = 'planned' AND run_at > botaru_now()`, bob); n != 1 {
		t.Fatalf("the card over the limit did not wait: %d", n)
	}
	b.govDetails(t, cd, on)
	if r, err := b.govDecide(t, cd, on, "confirm", "", ""); err != nil || r.State != "effective" {
		t.Fatalf("confirm today %+v %v", r, err)
	}
}
