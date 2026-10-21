// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/money"
)

// Tests of the farm manager's reach to people and its reports from v0.3 (MGR-50 to MGR-54).

// attention_notice from v0.3 goes from closed fields to the people with rights on the object,
// within the daily limit per person, and the seat learns only how many it reached; a person
// answers "Useful" once (MGR-52).
func TestManagerAttentionV03(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

// attention_notice from v0.3 passes the closed skeleton schema as well as the core's tool schema,
// and quiet hours defer manager_attention (never the digest), as they never defer a decision
// card (MGR-52, DLV-10).
func TestAttentionClosedQuietV03(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newManagerBench(t)
	ok := map[string]any{"sees": "disk_growth", "object": "t_01J9Z3M2Q8YB6V4N7T5K0C2D1E", "threat": "disk_full_soon", "proposal": map[string]any{"op": "hold"}}
	if !b.c.attentionChecked(ok) {
		t.Fatal("a valid notice")
	}
	for _, op := range []string{"notify", "call the admin", "rm -rf"} {
		bad := map[string]any{"sees": "disk_growth", "object": "t_01J9Z3M2Q8YB6V4N7T5K0C2D1E", "threat": "disk_full_soon", "proposal": map[string]any{"op": op}}
		if b.c.attentionChecked(bad) {
			t.Fatalf("op %q passed the closed list", op)
		}
	}
	cat, err := b.c.Catalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	loc := tenantLocation(cat, "demo")
	now := b.now(t).In(loc)
	quiet := map[string]any{"from": now.Add(-time.Hour).Format("15:04"), "to": now.Add(time.Hour).Format("15:04")}
	seq := 0
	defers := func(kind string, prefs map[string]any) bool {
		raw, _ := json.Marshal(prefs)
		b.exec(t, `UPDATE principals SET prefs = $2 WHERE id = $1`, b.alice, raw)
		seq++
		id := "dl_quiet" + string(rune('a'+seq))
		b.exec(t, `INSERT INTO deliveries (id, tenant, kind, addr, target_kind, target_id, exec_kind, exec_name, recipient, state, dataorigin, created_at, updated_at)
			VALUES ($1, 'demo', $2, 'admins', 'chat', 'direct:1001', 'channel', 'telegram', $3, 'planned', 'code', botaru_now(), botaru_now())`, id, kind, b.alice)
		var wait bool
		if err := b.c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
			var err error
			wait, err = b.c.prefsDefer(ctx, tx, now, cat, "demo", id, kind, b.alice)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return wait
	}
	if !defers(attentionNotice, map[string]any{"quiet_hours": quiet}) {
		t.Fatal("manager_attention in quiet hours went out")
	}
	if defers(attentionNotice, map[string]any{"digest": "daily", "quiet_hours": map[string]any{"from": now.Add(2 * time.Hour).Format("15:04"), "to": now.Add(3 * time.Hour).Format("15:04")}}) {
		t.Fatal("manager_attention collected into the digest")
	}
	if defers("approval_card", map[string]any{"quiet_hours": quiet}) {
		t.Fatal("quiet hours deferred a decision card")
	}
}

// The weekly holder report goes once a week to the farm operators with numbers only, and the
// daily summary is sent even for an empty day (MGR-50).
func TestHolderReportV03(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newManagerBench(t)
	op := b.farmOperator(t)
	b.loop(t, "identity")
	b.loop(t, "identity")
	if n := kindTo(t, b, holderReport, op); n != 1 {
		t.Fatalf("holder reports %d", n)
	}
	var body []byte
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT body FROM deliveries WHERE kind = $1 AND recipient = $2`, holderReport, op).Scan(&body); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "demo") || !strings.Contains(string(body), "notice.holder_report_v03") || !strings.Contains(string(body), "manager_against") {
		t.Fatalf("holder report %s", body)
	}
	var daily int
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM deliveries WHERE kind = 'manager_daily' AND body->>'text_key' IN ('notice.manager_daily_v03', 'notice.manager_daily_farm_v03')`).
		Scan(&daily); err != nil || daily == 0 {
		t.Fatalf("daily summaries of an empty day %d %v", daily, err)
	}
}

// The manager's proposals carry origin level 2, so they never make anyone answer for a link and
// give no rejected-proposal candidate; the reviewer input has no author level; at most 2 a month
// per tenant (MGR-51, GOV-38, GOV-41, COM-15).
func TestManagerProposalsV03(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	if authorLevel(true, "manage") != 2 || authorLevel(false, "aggregate") != 1 || authorLevel(false, "insight") != 0 {
		t.Fatal("author levels")
	}
	p := &proposalRow{Rec: map[string]any{"origin": map[string]any{"level": 2, "seat": managerSeatID}, "body": map[string]any{"category": "schedule"}}}
	if in := reviewInput(p); in["author_level"] != nil || in["seat"] != nil || in["origin"] != nil {
		t.Fatalf("reviewer input %v", in)
	}
	b := newManagerBench(t)
	if !managerProposer(reviewType) || !managerProposer(postmortemType) || managerProposer(incidentRespondType) {
		t.Fatal("which seat tasks propose")
	}
	cat, err := b.c.Catalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	limited := func() bool {
		var l bool
		if err := b.c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
			var err error
			l, err = managerProposalLimited(ctx, tx, now, cat, "demo")
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return l
	}
	for i, id := range []string{"prp_m1", "prp_m2"} {
		if limited() {
			t.Fatalf("limited after %d", i)
		}
		b.exec(t, `INSERT INTO proposals (id, tenant, role, worker, task, fingerprint, kind, state, record, created_at, updated_at, state_at)
			VALUES ($1, 'demo', 'farm-manager', 'wk_x', 't_x', $1, 'manifest', 'screened', '{"origin": {"seat": "farm-manager", "level": 2}}', botaru_now(), botaru_now(), botaru_now())`, id)
	}
	if !limited() {
		t.Fatal("a third proposal in the month")
	}
}
