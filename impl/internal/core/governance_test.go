// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/ids"
)

// govBench is the commission bench with the digest, a change-sensitive incident class about
// the tenant, and alice in a group that confirms cases and answers attributions.
func govBench(t *testing.T) *bench {
	t.Helper()
	b := newCommissionBenchWith(t, true, []map[string]any{{
		"apiVersion": "botaru/v1alpha1", "kind": "Group", "metadata": map[string]any{"name": "governors", "tenant": "demo"},
		"spec": map[string]any{"caps": []any{"governance.confirm:commission", "governance.attribute"}, "data": []any{"summary", "internal"}},
	}, {
		"apiVersion": "botaru/v1alpha1", "kind": "IncidentClass", "metadata": map[string]any{"name": "tenant_slow"},
		"spec": map[string]any{"title": "Tenant slow", "severity": "sev2", "change_sensitive": true,
			"detect": map[string]any{"alarm": "alarm.disk.free.low.v1"}, "subject": map[string]any{"kind": "tenant"}},
	}, storeLowManifest()}, presentEdit)
	b.exec(t, `INSERT INTO memberships (tenant, principal, grp, state, created_at) VALUES ('demo', $1, 'governors', 'active', now())`, b.alice)
	return b
}

// govCard is the latest card of a kind to alice with its press.
func (b *bench) govCard(t *testing.T, kind string) (map[string]any, *OnBehalf) {
	t.Helper()
	card := cardOf(t, b.drain(t), kind, "direct:1001")
	return card.body["card"].(map[string]any), press("telegram:1001", "direct:1001", card.external)
}

func (b *bench) govDecide(t *testing.T, cd map[string]any, on *OnBehalf, decision, reason, choice string) (*DecideResult, error) {
	t.Helper()
	return b.c.Decide(context.Background(), b.channel, on, DecideRequest{SubjectKind: asString(cd["subject_kind"]), ID: asString(cd["id"]), Nonce: asString(cd["nonce"]),
		Decision: decision, ReasonCode: reason, Choice: choice})
}

func (b *bench) govDetails(t *testing.T, cd map[string]any, on *OnBehalf) {
	t.Helper()
	b.advance(t, 1)
	if _, err := b.c.CardDetails(context.Background(), b.channel, on, CardOp{Op: "card.details", SubjectKind: asString(cd["subject_kind"]), ID: asString(cd["id"]), Nonce: asString(cd["nonce"])}); err != nil {
		t.Fatal(err)
	}
}

func (b *bench) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := b.c.st.Pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatal(err)
	}
}

func (b *bench) entry(t *testing.T, bot string) (state string, count int) {
	t.Helper()
	if err := b.c.st.Pool.QueryRow(context.Background(), `SELECT state, wrong_candidate_count FROM promotion_entries WHERE bot = $1`, bot).Scan(&state, &count); err != nil {
		t.Fatal(err)
	}
	return state, count
}

// An executed, accepted change of a functionary gives a promotion trigger only once its
// attribution candidate is answered "No" (GOV-13, GOV-30, GOV-14); "No" needs "Details" first
// (APR-14). The head of the queue takes a vacant seat on "Confirm" after "Details" (GOV-9), and
// its entry leaves as seated; every step writes its governance audit record (GOV-21).
func TestPromotionVacancy(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b, _ := promotedBench(t)
	var n int
	for _, cmd := range []string{"governance.entry.queued", "governance.case.opened", "governance.decided", "governance.effective", "attribution.create"} {
		if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM audit WHERE record->>'command' = $1`, cmd).Scan(&n); err != nil || n == 0 {
			t.Errorf("no audit %s: %v", cmd, err)
		}
	}
	st, err := b.c.CommissionStatus(ctx, Operator{Tenant: "demo"}, TenantRequest{Tenant: "demo"})
	if err != nil || st["governance_since"] == nil {
		t.Fatalf("status %v %v", st, err)
	}
}

// promotedBench runs the vacancy case of TestPromotionVacancy and returns the bench and the
// promoted bot.
func promotedBench(t *testing.T) (*bench, string) {
	t.Helper()
	ctx := context.Background()
	b, _, author := realizedBench(t)
	b.loop(t, "governance")
	var n int
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM promotion_entries`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("an open attribution gave a trigger: %d %v", n, err)
	}
	cd, on := b.govCard(t, attributionCard)
	validOut(t, b, "card", cd)
	if _, err := b.govDecide(t, cd, on, "no", "", ""); reasonOf(err) != "details_not_opened" {
		t.Fatalf("no without details: %v", err)
	}
	b.govDetails(t, cd, on)
	if r, err := b.govDecide(t, cd, on, "no", "", ""); err != nil || r.State != "rejected" {
		t.Fatalf("no %+v %v", r, err)
	}
	b.loop(t, "governance")
	if st, _ := b.entry(t, author.ID); st != "in_case" {
		t.Fatalf("entry %s", st)
	}
	cd, on = b.govCard(t, promotionCard)
	validOut(t, b, "card", cd)
	if ctxt := cd["context"].(map[string]any); ctxt["mode"] != "vacancy" || ctxt["no1"] == nil {
		t.Fatalf("card %v", cd)
	}
	if _, err := b.govDecide(t, cd, on, "confirm", "", ""); reasonOf(err) != "details_not_opened" {
		t.Fatalf("confirm without details: %v", err)
	}
	b.govDetails(t, cd, on)
	if r, err := b.govDecide(t, cd, on, "confirm", "", ""); err != nil || r.State != "effective" {
		t.Fatalf("confirm %+v %v", r, err)
	}
	var holder string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT holder FROM seats WHERE tenant = 'demo' AND id = 'commission:1'`).Scan(&holder); err != nil || holder != author.ID {
		t.Fatalf("seat holder %q %v", holder, err)
	}
	if st, _ := b.entry(t, author.ID); st != "removed" {
		t.Fatalf("entry %s", st)
	}
	return b, author.ID
}

// With every seat held, a case lists the seats whose holders sat for protect_new (GOV-12).
// "Reject" needs a reason code; the first "Wrong candidate" returns the entry with its own
// cooldown (GOV-31). "Confirm" names a seat of L in choice, or leaver_not_offered; the leaver
// retires removed_by_vote and the case carries evicts_upheld_veto for a leaver whose veto
// people upheld (GOV-9, GOV-19).
func TestPromotionRemoval(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b, _ := promotedBench(t)
	for _, seat := range []string{"commission:2", "commission:3", "commission:4"} {
		w := b.enroll(t, EnrollRequest{Tenant: "demo", Role: "hello", Kind: "agent"})
		if _, err := b.c.SeatAssign(ctx, b.op, SeatRequest{Seat: seat, Bot: b.botOf(t, w.ID).ID}); err != nil {
			t.Fatal(err)
		}
	}
	cand := b.botOf(t, b.enroll(t, EnrollRequest{Tenant: "demo", Role: "hello", Kind: "agent"}).ID).ID
	var trigger string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT proposal FROM promotion_track`).Scan(&trigger); err != nil {
		t.Fatal(err)
	}
	b.advance(t, 61*86400)
	b.exec(t, `INSERT INTO promotion_entries (tenant, bot, state, trigger_proposal, trigger_at, updated_at) VALUES ('demo', $1, 'queued', $2, botaru_now(), botaru_now())`, cand, trigger)
	b.loop(t, "governance")
	cd, on := b.govCard(t, promotionCard)
	if ctxt := cd["context"].(map[string]any); ctxt["mode"] != "removal" || ctxt["seat4"] == nil {
		t.Fatalf("card %v", cd)
	}
	if _, err := b.govDecide(t, cd, on, "reject", "", ""); reasonOf(err) != "reason_code_required" {
		t.Fatalf("reject without a reason: %v", err)
	}
	if r, err := b.govDecide(t, cd, on, "reject", "wrong_candidate", ""); err != nil || r.State != "rejected" {
		t.Fatalf("reject %+v %v", r, err)
	}
	if st, n := b.entry(t, cand); st != "queued" || n != 1 {
		t.Fatalf("entry %s %d", st, n)
	}
	b.loop(t, "governance")
	var open int
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM governance_cases WHERE state = 'confirming'`).Scan(&open); err != nil || open != 0 {
		t.Fatalf("a case opened within the entry's cooldown: %d %v", open, err)
	}
	b.advance(t, 31*86400)
	b.loop(t, "governance")
	cd, on = b.govCard(t, promotionCard)
	b.exec(t, `UPDATE governance_cases SET upheld = '{"commission:2": 1}' WHERE state = 'confirming'`)
	b.govDetails(t, cd, on)
	if _, err := b.govDecide(t, cd, on, "confirm", "", "commission:9"); reasonOf(err) != "leaver_not_offered" {
		t.Fatalf("a seat outside L: %v", err)
	}
	leaver := ""
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT holder FROM seats WHERE tenant = 'demo' AND id = 'commission:2'`).Scan(&leaver); err != nil {
		t.Fatal(err)
	}
	if r, err := b.govDecide(t, cd, on, "confirm", "", "commission:2"); err != nil || r.State != "effective" {
		t.Fatalf("confirm %+v %v", r, err)
	}
	if l := b.loadBot(t, leaver); l.State != "retired" || l.Retired["reason"] != "removed_by_vote" {
		t.Fatalf("leaver %+v", l)
	}
	if n := b.loadBot(t, cand); n.Seat != "commission:2" {
		t.Fatalf("candidate %+v", n)
	}
	var flags string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT record->>'flags' FROM governance_cases WHERE state = 'effective' AND record->'subject'->>'mode' = 'removal'`).Scan(&flags); err != nil || flags != `["evicts_upheld_veto"]` {
		t.Fatalf("flags %s %v", flags, err)
	}
}

// realizedBench is the governance bench with an accepted change of the hello bot executed 20
// days ago with the trial outcome better, a bot of 40 days, governance since 30 days, and an
// incident of a change-sensitive class about the changed tenant opened after it.
func realizedBench(t *testing.T) (*bench, string, *botRow) {
	t.Helper()
	b := govBench(t)
	id, _ := acceptLimits(t, b)
	author := b.botOf(t, b.agent.ID)
	b.drain(t)
	b.exec(t, `UPDATE proposals SET state = 'realized', record = jsonb_set(jsonb_set(jsonb_set(record, '{state}', '"realized"'),
		'{realization,realized_at}', to_jsonb(to_char((botaru_now() - interval '20 days') AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'))),
		'{trial}', coalesce(record->'trial', '{}') || '{"outcome": "better", "state": "done"}') || jsonb_build_object('origin', record->'origin' || jsonb_build_object('acting_bot', $2::text))
		WHERE id = $1`, id, author.ID)
	// The governance rules hold since before the probation ended (GOV-14).
	b.exec(t, `INSERT INTO governance_tenants (tenant, since, updated_at) VALUES ('demo', botaru_now() - interval '30 days', botaru_now())`)
	b.exec(t, `UPDATE bots SET created_at = botaru_now() - interval '40 days' WHERE id = $1`, author.ID)
	b.exec(t, `INSERT INTO incidents (id, tenant, class, subject_kind, subject_id, alarmkey, episode, episodes, alarmkeys, state, record, since, implicated_at, opened_at, updated_at)
		VALUES ($1, 'demo', 'tenant_slow', 'tenant', 'demo', 'k', 1, ARRAY[1::bigint], ARRAY['k'], 'open', '{}', botaru_now(), botaru_now(), botaru_now() - interval '19 days', botaru_now())`, ids.New(ids.Incident))
	return b, id, author
}

// The trigger conditions (GOV-14, GOV-30, GOV-32): an undecided attribution keeps the trigger
// back and turns unknown on silence with weekly reminders; the outcome worse, a lifted veto or a
// probation that ended before governance_since give only a track row; a self-measured outcome
// waits for "Count it" on self_measured_card.
func TestPromotionTriggers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b, id, _ := realizedBench(t)
	b.loop(t, "governance")
	var state string
	var reminders int
	check := func(want string, wantReminders int) {
		t.Helper()
		if err := b.c.st.Pool.QueryRow(ctx, `SELECT state, (record->>'reminders')::int FROM attributions`).Scan(&state, &reminders); err != nil || state != want || reminders != wantReminders {
			t.Fatalf("attribution %s %d %v, want %s %d", state, reminders, err, want, wantReminders)
		}
	}
	check("candidate", 0)
	b.advance(t, 8*86400)
	b.loop(t, "governance")
	check("unknown", 1)
	b.advance(t, 7*86400)
	b.loop(t, "governance")
	check("unknown", 2)
	cat := b.catalog(t)
	success := func(edit string) string {
		t.Helper()
		var v string
		err := b.c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
			if _, err := tx.Exec(ctx, `UPDATE attributions SET state = 'rejected'`); err != nil {
				return err
			}
			if edit != "" {
				if _, err := tx.Exec(ctx, `UPDATE proposals SET record = record || $2::jsonb WHERE id = $1`, id, edit); err != nil {
					return err
				}
			}
			p, err := loadProposal(ctx, tx, id)
			if err != nil {
				return err
			}
			if _, v, err = b.c.successOf(ctx, tx, now, cat, cat.Tenants["demo"], p, now.AddDate(0, 0, -60)); err != nil {
				return err
			}
			return errors.New("rollback")
		})
		if err != nil && err.Error() != "rollback" {
			t.Fatal(err)
		}
		return v
	}
	if v := success(""); v != "trigger" {
		t.Fatalf("plain %s", v)
	}
	if v := success(`{"veto_overridden": true}`); v != "veto_overridden" {
		t.Fatalf("lifted veto %s", v)
	}
	var trial string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT record->>'trial' FROM proposals WHERE id = $1`, id).Scan(&trial); err != nil {
		t.Fatal(err)
	}
	worse := strings.Replace(trial, `"outcome": "better"`, `"outcome": "worse"`, 1)
	if v := success(`{"trial": ` + worse + `}`); v != "trial_worse" {
		t.Fatalf("worse %s", v)
	}
	self := strings.Replace(trial, `"self_measured": false`, `"self_measured": true`, 1)
	if v := success(`{"trial": ` + self + `}`); v != "wait" {
		t.Fatalf("self measured %s", v)
	}
	// A probation that ended before governance_since gives only a track row.
	err := b.c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		if _, err := tx.Exec(ctx, `UPDATE attributions SET state = 'rejected'`); err != nil {
			return err
		}
		p, err := loadProposal(ctx, tx, id)
		if err != nil {
			return err
		}
		if _, v, err := b.c.successOf(ctx, tx, now, cat, cat.Tenants["demo"], p, now); err != nil || v != "before_governance" {
			t.Errorf("before governance %s %v", v, err)
		}
		return errors.New("rollback")
	})
	if err == nil || err.Error() != "rollback" {
		t.Fatal(err)
	}
	// Self-measured: the card goes out, and "Count it" gives the trigger.
	b.exec(t, `UPDATE attributions SET state = 'rejected'`)
	b.exec(t, `UPDATE proposals SET record = record || jsonb_build_object('trial', $2::jsonb) WHERE id = $1`, id, self)
	b.loop(t, "governance")
	cd, on := b.govCard(t, selfMeasuredCard)
	validOut(t, b, "card", cd)
	if _, err := b.govDecide(t, cd, on, "count", "", ""); err != nil {
		t.Fatal(err)
	}
	b.loop(t, "governance")
	var trig bool
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT trigger FROM promotion_track WHERE proposal = $1`, id).Scan(&trig); err != nil || !trig {
		t.Fatalf("track %v %v", trig, err)
	}
}

// openVacancyCase answers the attribution "No" in the store and opens the vacancy case.
func openVacancyCase(t *testing.T) (*bench, *botRow) {
	t.Helper()
	b, _, author := realizedBench(t)
	b.loop(t, "governance")
	b.exec(t, `UPDATE attributions SET state = 'rejected', record = record || '{"state": "rejected"}'`)
	b.loop(t, "governance")
	if st, _ := b.entry(t, author.ID); st != "in_case" {
		t.Fatalf("entry %s", st)
	}
	return b, author
}

func (b *bench) caseState(t *testing.T) (state, reason string) {
	t.Helper()
	if err := b.c.st.Pool.QueryRow(context.Background(), `SELECT state, coalesce(record->>'cancel_reason', '') FROM governance_cases ORDER BY created_at DESC LIMIT 1`).Scan(&state, &reason); err != nil {
		t.Fatal(err)
	}
	return state, reason
}

// P2 seat.assign of the vacant seat of an open case cancels the case in the same transaction
// (canceled(seat_changed)), and the entry goes back to the queue (GOV-12).
func TestPromotionSeatChanged(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b, author := openVacancyCase(t)
	other := b.botOf(t, b.enroll(t, EnrollRequest{Tenant: "demo", Role: "hello", Kind: "agent"}).ID)
	if _, err := b.c.SeatAssign(ctx, b.op, SeatRequest{Seat: "commission:1", Bot: other.ID}); err != nil {
		t.Fatal(err)
	}
	if st, reason := b.caseState(t); st != "canceled" || reason != "seat_changed" {
		t.Fatalf("case %s %s", st, reason)
	}
	if st, _ := b.entry(t, author.ID); st != "queued" {
		t.Fatalf("entry %s", st)
	}
}

// A hold on the scope governance pauses an open case: it does not expire while held, and its
// expiry moves on by the held time (GOV-14).
func TestPromotionHold(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b, _ := openVacancyCase(t)
	h, err := b.c.HoldSet(ctx, OperatorActor(b.op), HoldSetRequest{Scope: HoldScope{Kind: "governance", Tenant: "demo", Name: "cases"}, Effects: []string{"no_enqueue"}, Reason: "maintenance"})
	if err != nil {
		t.Fatal(err)
	}
	b.loop(t, "governance")
	b.advance(t, 8*86400)
	b.loop(t, "governance")
	if st, _ := b.caseState(t); st != "confirming" {
		t.Fatalf("a held case moved: %s", st)
	}
	if _, err := b.c.HoldRelease(ctx, OperatorActor(b.op), HoldReleaseRequest{ID: h.ID}); err != nil {
		t.Fatal(err)
	}
	b.loop(t, "governance")
	b.advance(t, 86400)
	b.loop(t, "governance")
	if st, _ := b.caseState(t); st != "confirming" {
		t.Fatalf("the expiry did not move on: %s", st)
	}
	b.advance(t, 7*86400)
	b.loop(t, "governance")
	if st, _ := b.caseState(t); st != "expired" {
		t.Fatalf("case %s", st)
	}
}

// Dismissing the incident cancels its attribution candidate in the same transaction, and the
// change then gives its trigger (GOV-13, GOV-14).
func TestPromotionIncidentDismissed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b, id, _ := realizedBench(t)
	b.loop(t, "governance")
	var inc string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT incident FROM attributions`).Scan(&inc); err != nil {
		t.Fatal(err)
	}
	if _, err := b.c.IncidentDismiss(ctx, b.op, IncidentRequest{Incident: inc, ReasonCode: "maintenance", Details: "planned load test"}); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT state FROM attributions`).Scan(&state); err != nil || state != "canceled" {
		t.Fatalf("attribution %s %v", state, err)
	}
	b.loop(t, "governance")
	var trig bool
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT trigger FROM promotion_track WHERE proposal = $1`, id).Scan(&trig); err != nil || !trig {
		t.Fatalf("track %v %v", trig, err)
	}
}
