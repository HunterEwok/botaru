// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"fmt"
	"math"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/ids"
)

// v03Bench is the commission bench with the digest, an incident class about the tenant that a
// rejected proposal with the problem code slow can be linked to, and alice in the group that
// answers attributions; the v0.3 paths run and the charter is applied again on them, which
// records governance_v03_since (GOV-36).
func v03Bench(t *testing.T) *bench {
	t.Helper()
	b := newCommissionBenchWith(t, true, []map[string]any{{
		"apiVersion": "botaru/v1alpha1", "kind": "Group", "metadata": map[string]any{"name": "governors", "tenant": "demo"},
		"spec": map[string]any{"caps": []any{"governance.confirm:commission", "governance.attribute"}, "data": []any{"summary", "internal"}},
	}, {
		"apiVersion": "botaru/v1alpha1", "kind": "IncidentClass", "metadata": map[string]any{"name": "tenant_slow"},
		"spec": map[string]any{"title": "Tenant slow", "severity": "sev2", "change_sensitive": true, "problem_codes": []any{"slow"},
			"detect": map[string]any{"alarm": "alarm.disk.free.low.v1"}, "subject": map[string]any{"kind": "tenant"}},
	}, storeLowManifest()}, presentEdit)
	b.exec(t, `INSERT INTO memberships (tenant, principal, grp, state, created_at) VALUES ('demo', $1, 'governors', 'active', now())`, b.alice)
	b.c.v03.Store(true)
	b.charter(t, 1, nil)
	return b
}

// charter applies the bench set again with edit on the commission block of the demo charter.
func (b *bench) charter(t *testing.T, n int, edit func(com map[string]any)) *ApplyResult {
	t.Helper()
	ms := cloneSet(t, appliedSets[b])
	if edit != nil {
		edit(specOf(ms, "Governance", "demo")["commission"].(map[string]any))
	}
	res, err := b.c.Apply(context.Background(), b.op, ApplyRequest{Commit: fmt.Sprintf("c%039x", n), Message: "Charter", Manifests: ms})
	if err != nil && res == nil {
		if p, ok := AsProblem(err); !ok || p.Reason != "apply_lint_failed" {
			t.Fatalf("apply: %v", err)
		}
	}
	return res
}

// The Brier score, the checks of a forecast and the tie (COM-16, COM-17); the alarm series of
// the author's share (GOV-38).
func TestForecastRules(t *testing.T) {
	f := map[string]float64{"bad": 0.2, "ok": 0.8}
	if b := brierOf(lensClasses("security", true), f, "bad"); math.Abs(b-1.28) > 1e-9 {
		t.Fatalf("brier %v", b)
	}
	if b := brierOf(lensClasses("value", false), map[string]float64{"better": 1}, "worse"); b != worstBrier {
		t.Fatalf("worst brier %v", b)
	}
	for _, c := range []struct {
		res  map[string]any
		want bool
	}{
		{map[string]any{"score": 3, "forecast": map[string]any{"bad": 0.2, "ok": 0.8}}, true},
		{map[string]any{"score": 3, "forecast": map[string]any{"bad": 0.2, "ok": 0.7}}, false},
		{map[string]any{"score": 3, "forecast": map[string]any{"bad": 0.5, "ok": 0.5}}, false},
		{map[string]any{"score": 1, "forecast": map[string]any{"bad": 0.5, "ok": 0.5}}, true},
	} {
		if got := forecastValid("security", true, c.res); got != c.want {
			t.Errorf("%v: valid %v", c.res, got)
		}
	}
	if !forecastDisagrees("security", true, 4, true, f) || forecastDisagrees("security", true, 4, false, f) {
		t.Fatal("disagreement")
	}
	if !tied(accuracy{Value: 0.5, SE: 0.1}, accuracy{Value: 0.4, SE: 0.1}) || tied(accuracy{Value: 0.5}, accuracy{Value: 0.4}) {
		t.Fatal("tie")
	}
	// GOV-38: the author's share is read on the series named like the alarm.
	if alarmSeriesName("alarm.disk.free.low.v1") != "disk.free.low" || alarmSeriesName("farm.task.outcome") != "" {
		t.Fatal("alarm series")
	}
}

// From v0.3 a persona of the farm manager names its goal in the purpose paragraph, and no persona
// speaks of accuracy or parking (GOV-45, GOV-46).
func TestPersonaV03(t *testing.T) {
	u := unawareTerms{}
	if got := personaFindingsV03("You check the farm.", "farm_manager", u); !slices.Contains(got, "persona_purpose") {
		t.Fatalf("no purpose: %v", got)
	}
	ok := purposeMark + " Your goal is to prevent critical incidents from the approved catalog.\n\nYou check the farm."
	if got := personaFindingsV03(ok, "farm_manager", u); len(got) != 0 {
		t.Fatalf("purpose: %v", got)
	}
	if got := personaFindingsV03("Your accuracy decides your seat.", "commission", u); !slices.Contains(got, "unaware_text") {
		t.Fatalf("accuracy words: %v", got)
	}
}

// A v0.3 core never retires the farm manager as replaced_after_incident (IDN-15, IDN-16).
func TestLeaveReasonV03(t *testing.T) {
	m := &managerSeat{Reasons: []stopReason{{Reason: "incident"}}}
	if m.leaveReason(false) != "replaced_after_incident" || m.leaveReason(true) != "replaced_by_human" {
		t.Fatal("leave reason")
	}
}

// On a v0.3 core the first apply records governance.v03_enabled; a review hands in
// review-result.v2, whose input has no author level and carries cost_eur; a forecast that
// contradicts its score makes the review invalid; code keeps a forecast row of each review
// (COM-15, COM-16, GOV-36, GOV-46). A resubmission of a dismissed fingerprint before 30 days is
// rejected, after them it is marked (PRP-24).
func TestAccountabilityReview(t *testing.T) {
	ctx := context.Background()
	b := v03Bench(t)
	if n := b.count(t, `SELECT count(*) FROM audit WHERE record->>'command' = 'governance.v03_enabled'`); n != 1 {
		t.Fatalf("v03_enabled audit %d", n)
	}
	proposeNone(t, b)
	rows, err := b.c.st.Pool.Query(ctx, `SELECT record->>'review_schema', body->'params'->'input' FROM tasks WHERE type LIKE 'review.%'`)
	if err != nil {
		t.Fatal(err)
	}
	type task struct {
		Schema string
		Input  map[string]any
	}
	tasks, err := pgx.CollectRows(rows, pgx.RowToStructByPos[task])
	if err != nil || len(tasks) != 2 {
		t.Fatalf("review tasks %v %v", tasks, err)
	}
	for _, tk := range tasks {
		if _, ok := tk.Input["author_level"]; tk.Schema != "v2" || ok || tk.Input["cost_eur"] == nil {
			t.Fatalf("review task %+v", tk)
		}
	}
	ra := b.enroll(t, EnrollRequest{Tenant: "demo", Role: "reviewer-a", Kind: "agent"})
	rb := b.enroll(t, EnrollRequest{Tenant: "demo", Role: "reviewer-b", Kind: "agent"})
	if err := b.review(t, ra, map[string]any{"lens": "security", "score": 3, "confidence": "high", "forecast": map[string]any{"bad": 0.1, "ok": 0.9}}); err != nil {
		t.Fatal(err)
	}
	if err := b.review(t, rb, map[string]any{"lens": "value", "score": 4, "confidence": "high", "forecast": map[string]any{"better": 0.2, "unchanged": 0.2, "worse": 0.6}}); err != nil {
		t.Fatal(err)
	}
	b.loop(t, "proposal")
	_, rec := proposalState(t, b)
	if err := b.c.spec.Validate("urn:botaru:schema:proposal-record:1", rec); err != nil {
		t.Fatalf("record: %v", err)
	}
	states := map[string]string{}
	for _, r := range asList(rec["reviews"]) {
		rm := mmapAny(r)
		states[asString(rm["lens"])] = asString(rm["state"])
	}
	if states["security"] != "done" || states["value"] != "schema_invalid" {
		t.Fatalf("reviews %v", states)
	}
	if n := b.count(t, `SELECT count(*) FROM forecasts WHERE (lens = 'security' AND state = 'done' AND forecast->>'bad' = '0.1') OR (lens = 'value' AND state = 'invalid')`); n != 2 {
		t.Fatalf("forecasts %d", n)
	}
	// PRP-24: the commission dismissed the fingerprint.
	b.exec(t, `UPDATE proposals SET state = 'dismissed', record = record || jsonb_build_object('state', 'dismissed', 'verdict',
		record->'verdict' || jsonb_build_object('outcome', 'dismissed(low_score)', 'computed_at', to_char(botaru_now() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')))`)
	if _, err := b.analysis(t, "2", false, map[string]any{"proposals": []any{noneBody("slow", 1)}}); err != nil {
		t.Fatal(err)
	}
	if r := rejections(t, b); !slices.Contains(r, "resubmit_too_early") {
		t.Fatalf("rejections %v", r)
	}
	b.advance(t, 31*86400)
	b.helloDone(t, "2", "", "hi")
	b.loop(t, "observe")
	if _, err := b.analysis(t, "3", false, map[string]any{"proposals": []any{noneBody("slow", 1)}}); err != nil {
		t.Fatal(err)
	}
	recs := proposals(t, b)
	if len(recs) != 2 || mmapAny(recs[1]["resubmission"])["same_values"] != true {
		t.Fatalf("records %d rejections %v", len(recs), rejections(t, b))
	}
}

// reviewForecast is a review-result.v2 forecast that agrees with score (COM-16).
func reviewForecast(lens string, score int) map[string]any {
	bad := 0.1
	if score <= 1 {
		bad = 0.9
	}
	switch lens {
	case "security", "operational":
		return map[string]any{"bad": bad, "ok": math.Round((1-bad)*10) / 10}
	case "cost":
		return map[string]any{"over": bad, "within": math.Round((1-bad)*10) / 10}
	}
	return map[string]any{"better": math.Round((0.9-bad)*10) / 10, "unchanged": 0.1, "worse": bad}
}

// seatScores writes n scored seat forecasts of the holder of seat in lens, each with brier,
// against outcomes a and b in turn: a reference forecast of 0.5 each scores 0.5.
func (b *bench) seatScores(t *testing.T, seat, lens, holder string, n int, brier float64) {
	t.Helper()
	b.exec(t, `INSERT INTO forecasts (task, tenant, proposal, lens, executor, bot, seat, seat_epoch, state, forecast, written_at)
		SELECT $1 || g, 'demo', $1 || 'p' || g, $2, 'seat', $3, $1, 1, 'done', '{}', botaru_now() FROM generate_series(1, $4) g`, seat, lens, holder, n)
	b.exec(t, `INSERT INTO forecast_scores (task, source, tenant, proposal, lens, outcome, brier, scored_at)
		SELECT $1 || g, 'trial', 'demo', $1 || 'p' || g, $2, CASE WHEN g % 2 = 0 THEN 'a' ELSE 'b' END, $4, botaru_now() FROM generate_series(1, $3) g`, seat, lens, n, brier)
}

// The accuracy of a seat holder against the base rates, with shadow reviews and unconfirmed
// attributions left out and fewer than 10 scores insufficient (COM-17); apply refuses
// accountability on until every seat has enough scored forecasts and code says when they have
// (GOV-36); code proposes the worst holder as the leaver (GOV-43). A rejected proposal of the
// problem code of an incident class whose subject has an incident after the verdict becomes a
// link candidate on rejection_link_card (GOV-38, GOV-40); "Yes" after "Details" makes the holders
// whose review was against it without a veto answer (GOV-41); one of them leaves its seat and is
// parked with the seat epoch + 1, the other waits for the pace (GOV-42, IDN-16), and a parked bot
// retires as parked_expired after 180 days (IDN-15).
func TestAccountabilityOn(t *testing.T) {
	ctx := context.Background()
	b := v03Bench(t)
	if res := b.charter(t, 2, func(com map[string]any) { com["accountability"] = "on" }); res == nil || res.Applied ||
		!slices.ContainsFunc(res.Errors, func(e ApplyError) bool { return e.Code == "accountability_not_ready" }) {
		t.Fatalf("apply on without seats: %+v", res)
	}
	lenses := map[string]string{"commission:1": "security", "commission:2": "operational", "commission:3": "cost", "commission:4": "value"}
	briers := map[string]float64{"commission:1": 0.1, "commission:2": 0.2, "commission:3": 0.3, "commission:4": 0.9}
	holders := map[string]string{}
	for _, seat := range slices.Sorted(mapKeys(lenses)) {
		w := b.enroll(t, EnrollRequest{Tenant: "demo", Role: "hello", Kind: "agent"})
		holders[seat] = b.botOf(t, w.ID).ID
		if _, err := b.c.SeatAssign(ctx, b.op, SeatRequest{Seat: seat, Bot: holders[seat]}); err != nil {
			t.Fatal(err)
		}
		n := 10
		if seat == "commission:2" {
			n = 9
		}
		b.seatScores(t, seat, lenses[seat], holders[seat], n, briers[seat])
	}
	// A shadow review and a score of an attribution nobody confirmed do not count.
	b.exec(t, `INSERT INTO forecasts (task, tenant, proposal, lens, executor, bot, state, written_at) VALUES ('sh1', 'demo', 'shp', 'security', 'shadow', $1, 'done', botaru_now()),
		('sh2', 'demo', 'shp2', 'security', 'seat', $1, 'done', botaru_now())`, holders["commission:1"])
	b.exec(t, `INSERT INTO forecast_scores (task, source, tenant, proposal, lens, outcome, brier, scored_at) VALUES ('sh1', 'trial', 'demo', 'shp', 'security', 'a', 2, botaru_now())`)
	b.exec(t, `INSERT INTO forecast_scores (task, source, ref, tenant, proposal, lens, outcome, brier, attribution, scored_at)
		VALUES ('sh2', 'attribution', 'x', 'demo', 'shp2', 'security', 'a', 2, 'att_none', botaru_now())`)
	var a1, a2 accuracy
	err := b.c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		var err error
		if a1, err = lensAccuracy(ctx, tx, now, "demo", "security", holders["commission:1"]); err != nil {
			return err
		}
		a2, err = lensAccuracy(ctx, tx, now, "demo", "operational", holders["commission:2"])
		return err
	})
	// The shadow case enters the base rates of the tenant, not the holder's scores.
	if err != nil || a1.N != 10 || !a1.OK || a1.Value < 0.79 || a1.Value > 0.81 || a2.OK || a2.view() != "insufficient" {
		t.Fatalf("accuracy %+v %+v %v", a1, a2, err)
	}
	// In shadow with the minimum lowered to 8 code says the tenant is ready, once.
	if res := b.charter(t, 3, func(com map[string]any) { com["accountability_min_forecasts"] = 8 }); res == nil || !res.Applied {
		t.Fatalf("apply min: %+v", res)
	}
	b.loop(t, "governance")
	if n := b.count(t, `SELECT count(*) FROM governance_v03 WHERE ready_sent_at IS NOT NULL`); n != 1 {
		t.Fatal("no accountability_ready")
	}
	if res := b.charter(t, 4, func(com map[string]any) { com["accountability_min_forecasts"] = 8; com["accountability"] = "on" }); res == nil || !res.Applied {
		t.Fatalf("apply on: %+v", res)
	}
	if n := b.count(t, `SELECT count(*) FROM audit WHERE record->>'command' = 'commission.accountability_on'`); n != 1 {
		t.Fatal("no accountability_on audit")
	}
	cat, err := b.c.Catalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var lp leaverProposal
	if err := b.c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		var list []caseSeat
		for _, s := range slices.Sorted(mapKeys(holders)) {
			list = append(list, caseSeat{id: s, holder: holders[s]})
		}
		lp, err = b.c.proposeLeaver(ctx, tx, now, cat, "demo", list)
		return err
	}); err != nil || lp.proposed != "commission:4" {
		t.Fatalf("leaver %+v %v", lp, err)
	}

	// GOV-38: a manifest change about the tenant that the commission dismissed by a veto.
	body := changeBody("slow", limitsChange(7))
	body["problem"].(map[string]any)["subjects"] = []any{"tenant:demo"}
	b.helloDone(t, "1", "", "hi")
	b.loop(t, "observe")
	if _, err := b.analysis(t, "1", false, map[string]any{"proposals": []any{body}}); err != nil {
		t.Fatal(err)
	}
	review := func(seat string, score int, veto bool) map[string]any {
		var epoch int
		if err := b.c.st.Pool.QueryRow(ctx, `SELECT epoch FROM seats WHERE tenant = 'demo' AND id = $1`, seat).Scan(&epoch); err != nil {
			t.Fatal(err)
		}
		return map[string]any{"lens": lenses[seat], "executor": "seat", "seat": seat, "holder": holders[seat], "seat_epoch": epoch, "state": "done",
			"result": map[string]any{"lens": lenses[seat], "score": score, "veto": veto, "confidence": "high"}}
	}
	b.advance(t, 60)
	reviews := []any{review("commission:1", 0, true), review("commission:3", 1, false), review("commission:4", 1, false)}
	b.exec(t, `UPDATE proposals SET state = 'dismissed', record = record || jsonb_build_object('state', 'dismissed', 'reviews', $1::jsonb, 'verdict',
		jsonb_build_object('outcome', 'dismissed(veto)', 'computed_at', to_char(botaru_now() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')))`, reviews)
	b.advance(t, 86400)
	inc := ids.New(ids.Incident)
	b.exec(t, `INSERT INTO incidents (id, tenant, class, subject_kind, subject_id, alarmkey, episode, episodes, alarmkeys, state, record, since, implicated_at, opened_at, updated_at)
		VALUES ($1, 'demo', 'tenant_slow', 'tenant', 'demo', 'k', 1, ARRAY[1::bigint], ARRAY['k'], 'open', '{}', botaru_now(), botaru_now(), botaru_now(), botaru_now())`, inc)
	b.drain(t)
	b.loop(t, "governance")
	if n := b.count(t, `SELECT count(*) FROM attributions WHERE kind = 'rejected_proposal' AND incident = $1`, inc); n != 1 {
		t.Fatalf("link candidates %d", n)
	}
	cd, on := b.govCard(t, rejectionLinkCard)
	validOut(t, b, "card", cd)
	if _, err := b.govDecide(t, cd, on, "yes", "", ""); reasonOf(err) != "details_not_opened" {
		t.Fatalf("yes without details: %v", err)
	}
	b.govDetails(t, cd, on)
	if r, err := b.govDecide(t, cd, on, "yes", "", ""); err != nil || r.State != "confirmed" {
		t.Fatalf("yes %+v %v", r, err)
	}
	var answerable string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT record->>'answerable' FROM attributions WHERE kind = 'rejected_proposal'`).Scan(&answerable); err != nil || answerable != "[3, 4]" {
		t.Fatalf("answerable %s %v", answerable, err)
	}
	if n := b.count(t, `SELECT count(*) FROM link_departures WHERE (seat = 'commission:3' AND state = 'done') OR (seat = 'commission:4' AND state = 'waiting')`); n != 2 {
		t.Fatalf("departures %d", n)
	}
	var state string
	var epoch int
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT state, epoch FROM seats WHERE tenant = 'demo' AND id = 'commission:3'`).Scan(&state, &epoch); err != nil || state != "vacant" || epoch < 2 {
		t.Fatalf("seat %s %d %v", state, epoch, err)
	}
	parked := b.loadBot(t, holders["commission:3"])
	if parked.State != "unbound" || mmapAny(parked.Record["parked"])["seat"] != "commission:3" {
		t.Fatalf("parked %+v", parked)
	}
	if n := b.count(t, `SELECT count(*) FROM audit WHERE record->>'command' IN ('governance.seat.parked', 'governance.link.confirmed')`); n != 2 {
		t.Fatalf("audit %d", n)
	}
	if n := b.count(t, `SELECT count(*) FROM events WHERE type = 'seat.vacated.v1' AND subject = 'commission:3'`); n != 1 {
		t.Fatalf("seat events %d", n)
	}
	// People see the mode and the departures in commission.status (GOV-36, GOV-42).
	st, err := b.c.CommissionStatus(ctx, Operator{Tenant: "demo"}, TenantRequest{Tenant: "demo"})
	if acc := mmapAny(st["accountability"]); err != nil || acc["mode"] != "on" || acc["departures_done"] != 1 || acc["departures_waiting"] != 1 || st["governance_v03_since"] == nil {
		t.Fatalf("status %v %v", st, err)
	}
	// Back to shadow the waiting departure ends (GOV-36, GOV-42).
	if res := b.charter(t, 5, func(com map[string]any) { com["accountability_min_forecasts"] = 8 }); res == nil || !res.Applied {
		t.Fatalf("apply shadow: %+v", res)
	}
	b.loop(t, "governance")
	if n := b.count(t, `SELECT count(*) FROM link_departures WHERE seat = 'commission:4' AND state = 'canceled' AND reason = 'accountability_shadow'`); n != 1 {
		t.Fatalf("waiting departure in shadow %d", n)
	}
	if err := b.c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		at, err := unseatedRecently(ctx, tx, now, holders["commission:3"])
		if err == nil && at == nil {
			t.Error("no unseat cooldown")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// A false incident disputes the "Yes"; once the dispute ends the link is canceled and the
	// departed member returns to its still vacant seat by a notice (GOV-40, GOV-42).
	b.exec(t, `UPDATE incidents SET state = 'dismissed' WHERE id = $1`, inc)
	b.loop(t, "governance")
	b.advance(t, 8*86400)
	b.loop(t, "governance")
	if n := b.count(t, `SELECT count(*) FROM link_departures WHERE (seat = 'commission:3' AND state = 'returned' AND reason = 'notice') OR (seat = 'commission:4' AND state = 'canceled')`); n != 2 {
		t.Fatalf("returned departures %d", n)
	}
	if n := b.count(t, `SELECT count(*) FROM audit WHERE record->>'command' = 'governance.seat.returned'`); n != 1 {
		t.Fatal("no return audit")
	}
	b.advance(t, 181*86400)
	b.loop(t, "identity")
	if l := b.loadBot(t, holders["commission:3"]); l.State != "retired" || l.Retired["reason"] != "parked_expired" {
		t.Fatalf("parked bot %+v", l)
	}
}

// From v0.3 a trial also watches the farm guards of the tenant (PRP-26).
func TestTrialFarmGuards(t *testing.T) {
	ctx := context.Background()
	b := newCommissionBench(t, true, presentEdit)
	b.c.v03.Store(true)
	id, _ := acceptLimits(t, b)
	if res, err := b.c.Apply(ctx, b.op, ApplyRequest{Commit: "a000000000000000000000000000000000000002", Message: "Raise the queue\n\nBotaru-Proposal: " + id, Manifests: withLimit(t, b, 7)}); err != nil || !res.Applied {
		t.Fatalf("apply %+v %v", res, err)
	}
	trial := mmapAny(recordOf(t, b, id)["trial"])
	var series []string
	for _, g := range asList(trial["farm_guards"]) {
		series = append(series, asString(mmapAny(g)["series"]))
	}
	if !slices.Contains(series, "farm.task.outcome") || !slices.Contains(series, "farm.queue.wait") {
		t.Fatalf("farm guards %v", trial["farm_guards"])
	}
}
