// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/manifest"
	"github.com/HunterEwok/botaru/impl/internal/money"
)

// accountableEdit marks the disk class manager_accountable and adds disk_filling on the same
// alarm (MGR-55, MGR-45).
func accountableEdit(ms []map[string]any) {
	for _, m := range ms {
		if m["kind"] == "IncidentClass" && m["metadata"].(map[string]any)["name"] == "disk_full" {
			m["spec"].(map[string]any)["manager_accountable"] = true
		}
	}
}

func fillingClass() map[string]any {
	return map[string]any{
		"apiVersion": "botaru/v1alpha1", "kind": "IncidentClass", "metadata": map[string]any{"name": "disk_filling"},
		"spec": map[string]any{"title": "Disk filling", "severity": "sev2",
			"detect": map[string]any{"alarm": "alarm.disk.free.low.v1", "for": "5m",
				"when": []any{map[string]any{"atom": "lt", "field": "values.free_pct", "value": 15}}},
			"subject":   map[string]any{"kind": "resource"},
			"implicate": []any{map[string]any{"fn": "lease_holders_on", "args": map[string]any{"resource": "$subject"}}},
			"resolve":   map[string]any{"alarm_state": "resolved", "for": "10m"},
		},
	}
}

func restricted(t *testing.T, b *bench, tenant, incident string) bool {
	t.Helper()
	var r bool
	err := b.c.tx(context.Background(), func(tx pgx.Tx, _ time.Time) error {
		var err error
		r, err = seatRestricted(context.Background(), tx, tenant, incident)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// A preventable incident from v0.3 does not stop the seat: the record says so, the alarm
// manager.preventable pages, the farm operators get manager_preventable, and the seat goes into
// restricted mode in the tenant with the same epoch, lifted by seat.resume --tenant; a class
// that stops_manager stops nothing any more (GOV-47, MGR-45, MGR-47, MGR-55).
func TestManagerPreventableV03(t *testing.T) {
	ctx := context.Background()
	// manager_accountable is a v0.3 field, which apply accepts only under the v0.3 claim (MAN-11).
	b := newManagerBenchWith(t, nil, []map[string]any{fillingClass()}, accountableEdit)
	op := b.farmOperator(t)
	if _, err := b.c.SeatAssign(ctx, b.farmOp(), SeatRequest{Seat: managerSeatID}); err != nil {
		t.Fatal(err)
	}
	epoch := b.managerSeat(t).Epoch
	b.diskAlarm(t, true, 12)
	b.advance(t, 6*60)
	b.loop(t, "incident")
	filling := incidentsOf(t, b, "disk_filling")
	if len(filling) != 1 {
		t.Fatalf("disk_filling %d", len(filling))
	}
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE incidents SET record = record || '{"implicated": [{"kind": "worker", "id": "wk_x"}]}' WHERE id = $1`, filling[0].ID); err != nil {
		t.Fatal(err)
	}
	b.advance(t, 2*3600)
	b.diskAlarm(t, true, 3)
	b.advance(t, 6*60)
	b.loop(t, "incident")
	full := incidentsOf(t, b, "disk_full")
	if len(full) != 1 || full[0].Record["manager_preventable"] != true {
		t.Fatalf("disk_full %+v", full)
	}
	if m := b.managerSeat(t); m.State != "held" || m.Epoch != epoch || len(m.Reasons) != 0 {
		t.Fatalf("seat %+v", m)
	}
	if openAlarms(t, b, "alarm.manager.preventable.v1") != 1 {
		t.Fatal("no alarm.manager.preventable")
	}
	countNotices := func() int {
		var n int
		if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM deliveries WHERE kind = $1 AND recipient = $2`, preventableNotice, op).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if countNotices() != 1 {
		t.Fatal("no manager_preventable to the farm operator")
	}
	// Restricted mode: any basis but a sev1 incident opened during it.
	if !restricted(t, b, "demo", full[0].ID) || !restricted(t, b, "demo", "") || restricted(t, b, "other", "") {
		t.Fatal("restricted mode")
	}
	// The spending class stops_manager stops nothing from v0.3; its disk incident on db9 has no
	// filling incident, and as a sev1 opened during the restriction it is a basis that may act.
	b.stopAlarm(t, true)
	b.advance(t, 6*60)
	b.loop(t, "incident")
	if m := b.managerSeat(t); m.State != "held" {
		t.Fatalf("stopped by stops_manager %+v", m)
	}
	var db9 *incidentRow
	for _, r := range incidentsOf(t, b, "disk_full") {
		if r.SubjectID == "db9" {
			db9 = r
		}
	}
	if db9 == nil || db9.Record["manager_preventable"] != "no_filling_incident" || restricted(t, b, "demo", db9.ID) {
		t.Fatalf("db9 %+v", db9)
	}
	// The notice repeats after 24 h, once.
	b.advance(t, 24*3600)
	b.loop(t, "identity")
	b.loop(t, "identity")
	if n := countNotices(); n != 2 {
		t.Fatalf("notices after 24 h: %d", n)
	}
	_, err := b.c.SeatResume(ctx, b.farmOp(), SeatResumeRequest{Seat: managerSeatID, Tenant: "demo"})
	if err != nil {
		t.Fatal(err)
	}
	if restricted(t, b, "demo", "") || b.managerSeat(t).Epoch != epoch {
		t.Fatal("seat.resume --tenant")
	}
	_, err = b.c.SeatResume(ctx, b.farmOp(), SeatResumeRequest{Seat: managerSeatID, Tenant: "demo"})
	wantReason(t, err, "conflict")
	var lifted int
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM manager_restrictions WHERE tenant = 'demo' AND until IS NOT NULL`).Scan(&lifted); err != nil || lifted != 1 {
		t.Fatalf("lifted restriction kept %d %v", lifted, err)
	}
	if countEvents(t, b, "seat.stopped.v1", managerSeatID) < 1 || countEvents(t, b, "seat.resumed.v1", managerSeatID) < 1 {
		t.Fatal("no seat events of the restriction")
	}
}

// The v0.1 forms of the manager are gone at the v0.3 claim: seat.resume without a tenant is
// refused, stops_manager does not answer for the manager while manager_accountable does, and a
// takeover is paid from the manager line (GOV-47, MGR-55, MGR-48).
func TestManagerV01FormsGone(t *testing.T) {
	b := newManagerBench(t)
	_, err := b.c.SeatResume(context.Background(), b.farmOp(), SeatResumeRequest{Seat: managerSeatID, Incident: "inc_x"})
	wantReason(t, err, "input_schema_invalid")
	if managerClass(map[string]any{"stops_manager": true}) || !managerClass(map[string]any{"manager_accountable": true}) {
		t.Fatal("managerClass at v0.3")
	}
	if takeoverLine() != "manager" {
		t.Fatal("the takeover line at v0.3")
	}
}

// The repeats of manager_preventable: 24 h, 72 h, then weekly (GOV-47).
func TestPreventableRepeats(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for h, want := range map[int]int{1: 0, 24: 1, 71: 1, 72: 2, 72 + 167: 2, 72 + 168: 3, 72 + 336: 4} {
		if got := preventableRepeats(t0, t0.Add(time.Duration(h)*time.Hour)); got != want {
			t.Errorf("after %d h: %d, want %d", h, got, want)
		}
	}
}

// A takeover from v0.3 is paid from the manager line, at most takeover.max_eur each and
// takeover.manager_month_eur a month with the others staged, pending or approved (MGR-48).
func TestTakeoverCeilingsV03(t *testing.T) {
	ctx := context.Background()
	b := newManagerBench(t)
	if takeoverLine() != "manager" {
		t.Fatal("line")
	}
	tv := &TenantView{Name: "demo", Raw: map[string]any{"spec": map[string]any{"takeover": map[string]any{"manager_month_eur": 10.0, "max_eur": 5.0}}}}
	fits := func(eur float64, self ...string) bool {
		var ok bool
		err := b.c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
			var err error
			ok, err = takeoverFits(ctx, tx, now, tv, money.FromEUR(eur), self...)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if fits(5.5) || !fits(4) {
		t.Fatal("max_eur")
	}
	for i, id := range []string{"tko_a", "tko_b"} {
		if _, err := b.c.st.Pool.Exec(ctx, `INSERT INTO takeovers (id, tenant, task, type, seat_task, lease_no, profile, state, estimate_micros, nonce, created_at, updated_at)
			VALUES ($1, 'demo', $2, 'hello.v1', 't_seat', $4, 'manager-std', $3, 4000000, 'n', botaru_now(), botaru_now())`, id, "t_"+id, []string{"approved", "pending"}[i], i); err != nil {
			t.Fatal(err)
		}
	}
	if fits(4) || !fits(2) || !fits(4, "tko_b") {
		t.Fatal("manager_month_eur")
	}
	cat, err := b.c.Catalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := takeoverEvalPassed(cat, "demo", "hello", "manager-std"); err != nil || ok {
		t.Fatalf("eval gate without a report %v %v", ok, err)
	}
}

// From v0.3 work outside sev1 stops at 70% of the manager line's month; sev1 and the fallback
// profile run on (MGR-49).
func TestManagerMonthLineV03(t *testing.T) {
	ctx := context.Background()
	b := newManagerBench(t)
	day, month := 100.0, 10.0
	tv := &TenantView{Name: "demo"}
	tv.Spec.Budgets.Lines = map[string]manifest.LineBudget{"manager": {DayEUR: &day, MonthEUR: &month}}
	if _, err := b.c.st.Pool.Exec(ctx, `INSERT INTO spend (id, tenant, line, unit, state, eur_micros, time)
		VALUES ('spd_mgr', 'demo', 'manager', 'tokens', 'committed', 7500000, botaru_now())`); err != nil {
		t.Fatal(err)
	}
	check := func(ask budgetAsk) *Problem {
		var p *Problem
		err := b.c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
			p = b.c.checkBudget(ctx, tx, now, tv, ask)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	if p := check(budgetAsk{subject: "system", line: "manager", extra: 1000}); p == nil || p.Reason != "budget_exhausted" {
		t.Fatalf("outside sev1 over 70%% of the month: %v", p)
	}
	if p := check(budgetAsk{subject: "system", line: "manager", extra: 1000, sev1: true}); p != nil {
		t.Fatalf("sev1: %v", p)
	}
	if p := check(budgetAsk{subject: "system", line: "manager", extra: 1000, fallback: true}); p != nil {
		t.Fatalf("fallback: %v", p)
	}
	// Once the month is spent, sev1 runs only from the day's 30% reserve above today's other
	// work: 10 + 30 EUR.
	if _, err := b.c.st.Pool.Exec(ctx, `INSERT INTO spend (id, tenant, line, unit, state, eur_micros, time)
		VALUES ('spd_mgr2', 'demo', 'manager', 'tokens', 'committed', 2500000, botaru_now())`); err != nil {
		t.Fatal(err)
	}
	if p := check(budgetAsk{subject: "system", line: "manager", extra: money.FromEUR(29), sev1: true}); p != nil {
		t.Fatalf("sev1 within the reserve: %v", p)
	}
	if p := check(budgetAsk{subject: "system", line: "manager", extra: money.FromEUR(31), sev1: true}); p == nil {
		t.Fatal("sev1 past the reserve with the month spent")
	}
	err := b.c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		spent, err := managerMonthSpent(ctx, tx, now, tv)
		if err != nil || !spent {
			t.Fatalf("month spent %v %v", spent, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// The tenant lint of the manager line from v0.3: the day covers the review, the analyses and
// three reads, the month covers the takeover ceiling (MGR-49).
func TestManagerLineShortV03(t *testing.T) {
	profile := map[string]any{"price": map[string]any{"input": 1.0, "output": 1.0}, "max_output_tokens": 1000.0}
	fm := map[string]any{"daily_review_max_eur": 0.5}
	ts := func(day, month float64) map[string]any {
		return map[string]any{"budgets": map[string]any{"lines": map[string]any{"manager": map[string]any{"day_eur": day, "month_eur": month}}}}
	}
	if got := managerLineShort(ts(0.5, 5), fm, profile); !slices.Contains(got, "/budgets/lines/manager/day_eur") || !slices.Contains(got, "/budgets/lines/manager/month_eur") {
		t.Fatalf("v0.3 short %v", got)
	}
	if got := managerLineShort(ts(5, 20), fm, profile); len(got) != 0 {
		t.Fatalf("v0.3 enough %v", got)
	}
}

// manager_accountable needs a class whose alarm no agent-role type publishes (GOV-48).
func TestAccountableLint(t *testing.T) {
	c := lintCore(t)
	class := `apiVersion: botaru/v1alpha1
kind: IncidentClass
metadata: {name: disk_full}
spec:
  title: Disk full
  severity: sev1
  manager_accountable: true
  detect: {alarm: alarm.disk.free.low.v1}
  subject: {kind: resource}
`
	if codes := lintCodes(t, c, class); slices.Contains(codes, "incident_class_ref") {
		t.Fatalf("an accountable class alone %v", codes)
	}
	publisher := class + `---
apiVersion: botaru/v1alpha1
kind: TaskType
metadata: {name: disk.watch.v1}
spec:
  purpose: manage
  events: [alarm.disk.free.low.v1]
`
	if codes := lintCodes(t, c, publisher); !slices.Contains(codes, "incident_class_ref") {
		t.Fatalf("an alarm a manage type publishes %v", codes)
	}
}

// attention_notice from v0.3 goes from closed fields to the people with rights on the object,
// within the daily limit per person, and the seat learns only how many it reached; a person
// answers "Useful" once (MGR-52).
func TestManagerAttentionV03(t *testing.T) {
	ctx := context.Background()
	b := newManagerBenchWith(t, managerWorkFiles, append(withIncidentTeam(nil), directors), workEdit)
	b.member(t, "demo", b.alice, "incident-team")
	d, err := b.c.ManagerDirect(ctx, b.op, ManagerDirectRequest{Scope: ControlTarget{Kind: "resource", ID: "db1"}, Op: "hold", Text: "Look at db1."})
	if err != nil {
		t.Fatal(err)
	}
	w := b.enroll(t, EnrollRequest{Tenant: "demo", Kind: "agent", Role: "farm-manager"})
	if _, err := b.c.SeatAssign(ctx, b.farmOp(), SeatRequest{Seat: managerSeatID}); err != nil {
		t.Fatal(err)
	}
	item := b.claim(t, w, "attempt")
	if item == nil {
		t.Fatal("no attempt")
	}
	handle := item.Payload["handles"].(map[string]any)["tools"].(string)
	args := map[string]any{"sees": "disk_growth", "object": d.ID, "threat": "disk_full_soon", "proposal": map[string]any{"op": "hold"}}
	if out := seatCall(t, b, handle, attentionToolName, map[string]any{"sees": "free text", "object": d.ID, "threat": "x", "proposal": map[string]any{}}); out["code"] != "input_schema_invalid" {
		t.Fatalf("free text %v", out)
	}
	// The operation is a closed code, never free words, and the core list is a subset of the
	// schema enum.
	raw, err := b.c.spec.Raw("urn:botaru:schema:attention-notice:1")
	if err != nil {
		t.Fatal(err)
	}
	enum, _ := mmapAny(mmapAny(mmapAny(mmapAny(raw["properties"])["proposal"])["properties"])["op"])["enum"].([]any)
	for _, op := range attentionOps() {
		if !slices.Contains(enum, op) {
			t.Fatalf("op %v is not in the schema enum %v", op, enum)
		}
	}
	if out := seatCall(t, b, handle, attentionToolName, map[string]any{"sees": "disk_growth", "object": d.ID, "threat": "disk_full_soon",
		"proposal": map[string]any{"op": "call_the_admin_now"}}); out["code"] != "input_schema_invalid" {
		t.Fatalf("a free operation %v", out)
	}
	first := seatCall(t, b, handle, attentionToolName, args)
	if n, _ := first["people"].(float64); n < 1 {
		t.Fatalf("nobody reached %v", first)
	}
	for range 3 {
		seatCall(t, b, handle, attentionToolName, args)
	}
	var most int
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT coalesce(max(n), 0) FROM (SELECT count(*) AS n FROM manager_feedback WHERE kind = $1 GROUP BY recipient) x`,
		attentionNotice).Scan(&most); err != nil || most != defaultNoticesPerPersonDay {
		t.Fatalf("per person %d %v", most, err)
	}
	var id, nonce string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT id, nonce FROM manager_feedback WHERE recipient = $1 ORDER BY created_at LIMIT 1`, b.alice).Scan(&id, &nonce); err != nil {
		t.Fatal(err)
	}
	ch := &Worker{Tenant: "demo", Kind: "channel"}
	ob := &OnBehalf{Identity: "telegram:1001"}
	if _, err := b.c.CardFeedback(ctx, ch, ob, CardFeedbackRequest{ID: id, Nonce: "wrong", Value: "useful"}); err == nil {
		t.Fatal("a wrong nonce was taken")
	}
	if _, err := b.c.CardFeedback(ctx, ch, ob, CardFeedbackRequest{ID: id, Nonce: nonce, Value: "useful"}); err != nil {
		t.Fatal(err)
	}
	if out, err := b.c.CardFeedback(ctx, ch, ob, CardFeedbackRequest{ID: id, Nonce: nonce, Value: "not_needed"}); err != nil || out["value"] != "useful" {
		t.Fatalf("a second answer was taken %v %v", out, err)
	}
	// In restricted mode the seat's writes in the tenant are refused (MGR-47).
	if _, err := b.c.st.Pool.Exec(ctx, `INSERT INTO manager_restrictions (tenant, incident, holder, since) VALUES ('demo', 'inc_x', 'bot_x', botaru_now())`); err != nil {
		t.Fatal(err)
	}
	out := seatCall(t, b, handle, controlToolName, map[string]any{"op": "hold", "target": map[string]any{"kind": "resource", "id": "db1"},
		"effects": []any{"no_claim"}, "ttl": "30m"})
	if out["code"] != restrictedReason {
		t.Fatalf("a write in restricted mode: %v", out)
	}
	if out := seatCall(t, b, handle, takeoverToolName, map[string]any{"task": d.ID}); out["code"] != restrictedReason {
		t.Fatalf("a takeover in restricted mode: %v", out)
	}
}

// The manager's notices count in the person's daily card limit together with decision cards
// (MGR-52, APR-15): with 9 decision cards handed out today alice gets one notice, which makes
// the tenth card of her day; the next notice no longer reaches her though the 3 notices a day
// are not used up, and a decision card for her would wait.
func TestManagerAttentionCardLimitV03(t *testing.T) {
	ctx := context.Background()
	b := newManagerBenchWith(t, managerWorkFiles, append(withIncidentTeam(nil), directors), workEdit)
	b.member(t, "demo", b.alice, "incident-team")
	d, err := b.c.ManagerDirect(ctx, b.op, ManagerDirectRequest{Scope: ControlTarget{Kind: "resource", ID: "db1"}, Op: "hold", Text: "Look at db1."})
	if err != nil {
		t.Fatal(err)
	}
	w := b.enroll(t, EnrollRequest{Tenant: "demo", Kind: "agent", Role: "farm-manager"})
	if _, err := b.c.SeatAssign(ctx, b.farmOp(), SeatRequest{Seat: managerSeatID}); err != nil {
		t.Fatal(err)
	}
	item := b.claim(t, w, "attempt")
	if item == nil {
		t.Fatal("no attempt")
	}
	for i := range defaultNoncriticalPerDay - 1 {
		b.exec(t, `INSERT INTO deliveries (id, tenant, kind, addr, target_kind, target_id, exec_kind, exec_name, recipient, state, dataorigin,
			created_at, updated_at, handed_at) VALUES ($2, 'demo', 'approval_card', 'approval_initiator', 'chat', 'direct:1001', 'channel', 'telegram', $1,
			'delivered', 'code', botaru_now(), botaru_now(), botaru_now())`, b.alice, fmt.Sprintf("dl_alice%d", i))
	}
	handle := item.Payload["handles"].(map[string]any)["tools"].(string)
	args := map[string]any{"sees": "disk_growth", "object": d.ID, "threat": "disk_full_soon", "proposal": map[string]any{"op": "hold"}}
	notices := func() int {
		var n int
		if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM manager_feedback WHERE kind = $1 AND recipient = $2`, attentionNotice, b.alice).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	seatCall(t, b, handle, attentionToolName, args)
	if n := notices(); n != 1 {
		t.Fatalf("notices before the limit %d", n)
	}
	seatCall(t, b, handle, attentionToolName, args)
	if n := notices(); n != 1 {
		t.Fatalf("a notice over the card limit %d", n)
	}
	cat, err := b.c.Catalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tv := cat.Tenants["demo"]
	if err := b.c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		n, err := b.c.cardsToday(ctx, tx, "demo", b.alice, tenantDay(tv, now))
		if err == nil && n < noncriticalPerDay(tv) {
			err = fmt.Errorf("cards today %d: a decision card would not wait", n)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// A model paragraph may only use the numbers of the table (MGR-53).
func TestParagraphChecked(t *testing.T) {
	table := map[string]any{"alarm_values": []any{map[string]any{"free_pct": 3}}, "ops": 2}
	table["incident"] = "inc_01J9Z8"
	if !paragraphChecked("Free space fell to 3 percent after 2 operations.", []string{"alarms", "inc_01J9Z8"}, table) ||
		paragraphChecked("Free space fell to 4 percent.", nil, table) || paragraphChecked("Free space fell to 3.", []string{"wk_unknown"}, table) ||
		paragraphChecked("Incident 9 was bad.", nil, table) {
		t.Fatal("paragraph check")
	}
}

// postmortemDoc is a manager.postmortem.v1 type for the tests, as examples/tasktypes declares it.
func postmortemDoc() map[string]any {
	return map[string]any{"apiVersion": "botaru/v1alpha1", "kind": "TaskType", "metadata": map[string]any{"name": postmortemType},
		"spec": map[string]any{"title": "Analysis after an incident", "worker_kind": "agent", "purpose": "manage", "tier": 1, "data_class": "internal",
			"instruction": "Analyse the table in the input block.", "params_schema": map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{}},
			"timeouts": map[string]any{"start_by": "6h", "lease": "60s", "heartbeat": "20s", "attempt": "10m", "deadline": "48h"},
			"retry":    map[string]any{"max_attempts": 1, "max_runs": 1}, "tree_budget_eur": 0.40, "max_children": 0, "proposable": false}}
}

func claimPostmortem(ms []map[string]any) {
	for _, m := range ms {
		md, _ := m["metadata"].(map[string]any)
		spec, _ := m["spec"].(map[string]any)
		switch {
		case m["kind"] == "Role" && md["name"] == "farm-manager":
			spec["claims"] = append(spec["claims"].([]any), postmortemType)
		case m["kind"] == "Group" && md["name"] == "farm-manager-ops":
			spec["caps"] = append(spec["caps"].([]any), "task:"+postmortemType)
		}
	}
}

// A resolved incident gets one analysis; reopened and resolved again it gets none and the
// resolve goes through. Analyses share the day's ceiling at claim (MGR-53).
func TestPostmortemOnceV03(t *testing.T) {
	ctx := context.Background()
	b := newManagerBenchWith(t, nil, []map[string]any{postmortemDoc()}, claimPostmortem)
	cycle := func(free float64) {
		b.diskAlarm(t, true, free)
		b.advance(t, 6*60)
		b.loop(t, "incident")
		b.diskAlarm(t, false, 50)
		b.advance(t, 11*60)
		b.loop(t, "incident")
		b.advance(t, 11*60)
		b.loop(t, "incident")
	}
	cycle(3)
	list := incidentsOf(t, b, "disk_full")
	if len(list) != 1 || list[0].State != "resolved" {
		t.Fatalf("incidents %+v", list)
	}
	count := func() int {
		var n int
		if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE type = $1 AND idempotency_key = $2`, postmortemType, "system:postmortem:"+list[0].ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if count() != 1 {
		t.Fatal("no analysis")
	}
	cycle(2)
	if again := incidentsOf(t, b, "disk_full"); len(again) != 1 || again[0].State != "resolved" || count() != 1 {
		t.Fatalf("after the reopen %+v %d", again, count())
	}
	// The ceiling at claim: 0.40 EUR minus the whole tree budget of a running analysis.
	cat, err := b.c.Catalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	budget := func(task string) (int64, bool) {
		var v int64
		var ok bool
		err := b.c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
			var err error
			v, ok, err = postmortemClaimBudget(ctx, tx, now, cat, cat.Tenants["demo"], task)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return v, ok
	}
	if v, ok := budget("t_none"); !ok || v != money.FromEUR(0.40) {
		t.Fatalf("the whole ceiling %d %v", v, ok)
	}
	// The two incidents of the bench (disk_full and spend_storm on the same alarm) have one
	// analysis each; the first runs with 0.30 EUR, the second gets the rest.
	var first, second string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT min(id), max(id) FROM tasks WHERE type = $1`, postmortemType).Scan(&first, &second); err != nil || first == second {
		t.Fatalf("two analyses %s %s %v", first, second, err)
	}
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE tasks SET state = 'working', record = record || '{"tree_budget_micros": 300000}' WHERE id = $1`, first); err != nil {
		t.Fatal(err)
	}
	if v, ok := budget(second); !ok || v != 100000 {
		t.Fatalf("beside a running analysis %d %v", v, ok)
	}
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE tasks SET record = record || '{"tree_budget_micros": 400000}' WHERE id = $1`, first); err != nil {
		t.Fatal(err)
	}
	if _, ok := budget(second); ok {
		t.Fatal("past the ceiling")
	}
}

// From v0.3 a takeover grant goes only to the role its eval gate checked (MGR-48).
func TestTakeoverGrantRoleV03(t *testing.T) {
	ctx := context.Background()
	b := newManagerBench(t)
	if _, err := b.c.st.Pool.Exec(ctx, `INSERT INTO model_grants (id, tenant, task, role, kind, line, profile, attempts, attempts_left, estimate_micros, cap_micros, expires_at,
		decided_at, state, created_at, updated_at) VALUES ('mg_x', 'demo', 't_x', 'hello', 'takeover', 'manager', 'manager-std', 1, 1, 1000, 1000, botaru_now() + interval '1 hour',
		botaru_now(), 'approved', botaru_now(), botaru_now())`); err != nil {
		t.Fatal(err)
	}
	cat, err := b.c.Catalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	claim := func(role string) bool {
		var ok bool
		err := b.c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
			var err error
			_, ok, err = claimGrant(ctx, tx, now, cat, "demo", "t_x", role)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if claim("other") || !claim("hello") {
		t.Fatal("takeover grant role")
	}
}
