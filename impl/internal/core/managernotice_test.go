// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/ids"
	"github.com/HunterEwok/botaru/impl/internal/manifest"
)

// withIncidentTeam gives the demo bench a group with incident.manage, so its members are the
// people of the tenant route of manager_notice (MGR-39).
func withIncidentTeam(ms []map[string]any) []map[string]any {
	return append(ms, map[string]any{
		"apiVersion": "botaru/v1alpha1", "kind": "Group", "metadata": map[string]any{"name": "incident-team", "tenant": "demo"},
		"spec": map[string]any{"caps": []any{"incident.manage"}, "data": []any{"summary"}}})
}

// member writes a membership into the store: a privileged group comes only through farmctl
// with a verification code (PRM-13), which this build lacks.
func (b *bench) member(t *testing.T, tenant, principal, group string) {
	t.Helper()
	if _, err := b.c.st.Pool.Exec(context.Background(), `INSERT INTO memberships (tenant, principal, grp, state, created_at) VALUES ($1, $2, $3, 'active', now())`,
		tenant, principal, group); err != nil {
		t.Fatal(err)
	}
}

// newNoticeBench is the manager bench where alice is in the incident team.
func newNoticeBench(t *testing.T, edits ...func([]map[string]any)) *bench {
	t.Helper()
	b := newManagerBenchWith(t, nil, withIncidentTeam(nil), edits...)
	b.member(t, "demo", b.alice, "incident-team")
	return b
}

// withoutEdit turns off edit of the demo channel driver.
func withoutEdit(ms []map[string]any) {
	for _, m := range ms {
		if m["kind"] == "Channel" {
			m["spec"].(map[string]any)["capabilities"].(map[string]any)["edit"] = false
		}
	}
}

// notices are the manager_notice deliveries of an incident the channel took, in order.
func notices(s []sent, incident string) []sent {
	var out []sent
	for _, x := range s {
		if b, _ := json.Marshal(x.body); x.kind == managerNoticeKind && strings.Contains(string(b), incident) {
			out = append(out, x)
		}
	}
	return out
}

func noticeText(t *testing.T, b *bench, s sent) string {
	t.Helper()
	params, _ := s.body["params"].(map[string]any)
	key, _ := s.body["text_key"].(string)
	if s.body["form"] != "text_code" || key == "" {
		t.Fatalf("body %+v", s.body)
	}
	return b.c.texts.Render("en", key, params)
}

// The incident's message is created at opening and edited through the driver's edit on each
// operation; several changes before the channel takes the update share one edit; the message
// has the one revert command; the farm operators' text has only the number, the class, the
// operation codes and the numbers (MGR-39, MGR-13).
func TestManagerNoticeEdit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newNoticeBench(t)
	b.diskAlarm(t, true, 3)
	b.advance(t, 6*60)
	b.loop(t, "incident")
	inc := incidentsOf(t, b, "disk_full")[0]
	first := notices(b.drain(t), inc.ID)
	if len(first) != 1 || first[0].chat != "direct:1001" || first[0].replaces != "" {
		t.Fatalf("first message %+v", first)
	}
	text := noticeText(t, b, first[0])
	for _, want := range []string{inc.ID, "disk_full", "resource db1", ": contained", "farmctl control revert incident=" + inc.ID} {
		if !strings.Contains(text, want) {
			t.Fatalf("first message %q lacks %q", text, want)
		}
	}
	apply := func(op string, effects ...string) {
		t.Helper()
		req := ControlRequest{Op: op, Target: ControlTarget{Kind: "resource", ID: "db1"}, Effects: effects, TTL: "1h"}
		req.Basis.Incident = inc.ID
		if _, err := b.c.ControlApply(ctx, b.op, req); err != nil {
			t.Fatal(err)
		}
	}
	apply("hold", "no_tool")
	edit := notices(b.drain(t), inc.ID)
	if len(edit) != 1 || edit[0].replaces != first[0].external {
		t.Fatalf("edit %+v, first %s", edit, first[0].external)
	}
	if text := noticeText(t, b, edit[0]); !strings.Contains(text, "hold resource db1 by person") || strings.Count(text, "farmctl control revert") != 1 {
		t.Fatalf("edit %q", text)
	}
	// Two operations before the channel comes: one edit shows both.
	apply("hold", "no_enqueue")
	apply("hold", "no_action")
	edit = notices(b.drain(t), inc.ID)
	if len(edit) != 1 || strings.Count(noticeText(t, b, edit[0]), "hold resource db1") != 3 {
		t.Fatalf("coalesced edit %+v", edit)
	}
	// The farm route: codes and numbers only.
	var raw []byte
	var r *incidentRow
	if err := b.c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		var err error
		r, err = scanIncidentLog(tx.QueryRow(ctx, `SELECT `+incidentCols+`, notice_log FROM incidents WHERE id = $1`, inc.ID), &raw)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var log []noticeEntry
	if err := json.Unmarshal(raw, &log); err != nil {
		t.Fatal(err)
	}
	farm := b.c.noticeWhole("en", "farm", r, log)
	ftext := b.c.texts.Render("en", "notice.manager_notice", farm["params"].(map[string]any))
	if strings.Contains(ftext, "db1") || strings.Contains(ftext, "resource") || strings.Contains(ftext, "demo") ||
		!strings.Contains(ftext, inc.ID) || !strings.Contains(ftext, "disk_full") || strings.Count(ftext, ": hold") != 3 || !strings.Contains(ftext, "operations 3") {
		t.Fatalf("farm text %q", ftext)
	}
	// A closing edits it too.
	b.diskAlarm(t, false, 30)
	b.advance(t, 11*60)
	b.loop(t, "incident")
	if _, err := b.c.IncidentClose(ctx, b.op, IncidentRequest{ID: inc.ID}); err != nil {
		t.Fatal(err)
	}
	edit = notices(b.drain(t), inc.ID)
	if len(edit) != 1 || !strings.Contains(noticeText(t, b, edit[0]), ": closed") {
		t.Fatalf("close edit %+v", edit)
	}
	// A long log keeps its latest lines, their numbers and the operation count.
	if err := b.c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		for range noticeLogMax + 5 {
			if err := b.c.noticeLog(ctx, tx, now, inc.ID, noticeEntry{Kind: "op", Op: "hold"}, false); err != nil {
				return err
			}
		}
		if err := tx.QueryRow(ctx, `SELECT notice_log FROM incidents WHERE id = $1`, inc.ID).Scan(&raw); err != nil {
			return err
		}
		return json.Unmarshal(raw, &log)
	}); err != nil {
		t.Fatal(err)
	}
	if last := log[len(log)-1]; len(log) != noticeLogMax || last.N != log[0].N+noticeLogMax-1 || last.Ops != noticeLogMax+5+3 {
		t.Fatalf("trimmed log of %d, first %+v, last %+v", len(log), log[0], last)
	}
}

// A driver without edit gets the new lines in a batch at most once in 15 minutes (MGR-39).
func TestManagerNoticeBatch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newNoticeBench(t, withoutEdit)
	b.diskAlarm(t, true, 3)
	b.advance(t, 6*60)
	b.loop(t, "incident")
	inc := incidentsOf(t, b, "disk_full")[0]
	if first := notices(b.drain(t), inc.ID); len(first) != 1 {
		t.Fatalf("first message %+v", first)
	}
	req := ControlRequest{Op: "hold", Target: ControlTarget{Kind: "resource", ID: "db1"}, Effects: []string{"no_tool"}, TTL: "1h"}
	req.Basis.Incident = inc.ID
	if _, err := b.c.ControlApply(ctx, b.op, req); err != nil {
		t.Fatal(err)
	}
	if got := notices(b.drain(t), inc.ID); len(got) != 0 {
		t.Fatalf("a batch inside the 15 minutes %+v", got)
	}
	b.advance(t, 15*60)
	got := notices(b.drain(t), inc.ID)
	if len(got) != 1 || got[0].replaces != "" || got[0].body["text_key"] != "notice.manager_notice.batch" {
		t.Fatalf("batch %+v", got)
	}
	if text := noticeText(t, b, got[0]); !strings.Contains(text, "hold resource db1") || strings.Contains(text, "contained") {
		t.Fatalf("batch %q", text)
	}
}

// A failure while writing the message to people leaves no operation, audit or control.applied:
// it is one transaction (MGR-13, blackbox:manager/immediate-report).
func TestManagerNoticeOneTransaction(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newNoticeBench(t)
	b.diskAlarm(t, true, 3)
	b.advance(t, 6*60)
	b.loop(t, "incident")
	inc := incidentsOf(t, b, "disk_full")[0]
	b.drain(t)
	if _, err := b.c.st.Pool.Exec(ctx, `CREATE FUNCTION fail_notice() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'notice write failed'; END $$;
		CREATE TRIGGER fail_notice BEFORE INSERT ON deliveries FOR EACH ROW WHEN (NEW.kind = 'manager_notice') EXECUTE FUNCTION fail_notice()`); err != nil {
		t.Fatal(err)
	}
	req := ControlRequest{Op: "hold", Target: ControlTarget{Kind: "resource", ID: "db1"}, Effects: []string{"no_tool"}, TTL: "1h"}
	req.Basis.Incident = inc.ID
	if _, err := b.c.ControlApply(ctx, b.op, req); err == nil {
		t.Fatal("the operation applied without its message")
	}
	var ops, events int
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM control_ops), (SELECT count(*) FROM events WHERE type = 'control.applied.v1')`).Scan(&ops, &events); err != nil {
		t.Fatal(err)
	}
	if ops != 0 || events != 0 {
		t.Fatalf("ops %d events %d", ops, events)
	}
}

// farmOperator puts tenant farm into the applied catalog, with the demo channel and a group
// governors with admin:governance, and adds a person of farm with telegram:2002 to it. A
// Tenant manifest named farm would clash in the lint with the farm subject of the global
// incident classes; the deployment sets up the farm operators.
func (b *bench) farmOperator(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	cat, err := b.c.Catalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	demo := cat.Tenants["demo"]
	cat.Tenants[farmTenant] = &TenantView{Name: farmTenant, Spec: demo.Spec, Raw: demo.Raw, Channels: demo.Channels,
		Groups: map[string]manifest.Group{"governors": {Caps: []string{"admin:governance"}}}}
	p := ids.PrincipalID()
	if _, err := b.c.Principal(ctx, b.farmOp(), PrincipalRequest{Op: "principal.add", Subject: p, Identity: "telegram:2002"}); err != nil {
		t.Fatal(err)
	}
	b.member(t, farmTenant, p, "governors")
	return p
}

// stoppedNotices are the manager_stopped deliveries to a person: their text keys and params.
func stoppedNotices(t *testing.T, b *bench, person string) []map[string]any {
	t.Helper()
	rows, err := b.c.st.Pool.Query(context.Background(), `SELECT body FROM deliveries WHERE kind = 'manager_stopped' AND recipient = $1 ORDER BY seq`, person)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var raw []byte
		var body map[string]any
		if err := rows.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatal(err)
		}
		out = append(out, body)
	}
	return out
}

// At the v0.3 claim an incident of a class with stops_manager sends no manager_stopped notice
// and nothing repeats one: the seat is not stopped by incidents (GOV-47, MGR-55); a
// preventable incident sends manager_preventable instead (TestPreventableRepeats).
func TestManagerStoppedCard(t *testing.T) {
	t.Parallel()
	b := newManagerBench(t)
	op := b.farmOperator(t)
	if _, err := b.c.SeatAssign(context.Background(), b.farmOp(), SeatRequest{Seat: managerSeatID}); err != nil {
		t.Fatal(err)
	}
	b.stopAlarm(t, true)
	b.advance(t, 6*60)
	b.loop(t, "incident")
	if len(incidentsOf(t, b, "spend_storm")) != 1 {
		t.Fatal("no incident")
	}
	if cards := stoppedNotices(t, b, op); len(cards) != 0 {
		t.Fatalf("cards %+v", cards)
	}
	b.advance(t, 72*3600)
	b.loop(t, "identity")
	if cards := stoppedNotices(t, b, op); len(cards) != 0 {
		t.Fatalf("after 72 h %+v", cards)
	}
	if n := b.count(t, `SELECT count(*) FROM deliveries WHERE kind = 'manager_stopped'`); n != 0 {
		t.Fatalf("manager_stopped deliveries %d", n)
	}
}

// The repeats of manager_stopped (MGR-36) end with v0.2: a stop reason incident and an expired
// decide_within that an earlier core left on the seat are dropped by the manager loop, the
// holder stays, and no repeat is sent (GOV-47, MGR-55).
func TestStopRepeats(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newManagerBench(t)
	op := b.farmOperator(t)
	if _, err := b.c.SeatAssign(ctx, b.farmOp(), SeatRequest{Seat: managerSeatID}); err != nil {
		t.Fatal(err)
	}
	holder := b.managerSeat(t).Holder
	b.exec(t, `UPDATE seats SET state = 'stopped', stop_reasons = '[{"reason": "incident", "incident": "inc_x", "repeats": 1}]',
		decide_until = botaru_now() - interval '1 hour' WHERE id = $1`, managerSeatID)
	// A manager_stopped card the earlier core planned for a quiet hour is withheld, not sent.
	b.exec(t, `INSERT INTO deliveries (id, tenant, kind, addr, target_kind, target_id, exec_kind, exec_name, state, dataorigin,
		created_at, updated_at) VALUES ('dl_stopped', 'farm', 'manager_stopped', 'admins', 'chat', 'direct:1001', 'channel', 'telegram',
		'planned', 'code', botaru_now(), botaru_now())`)
	b.advance(t, 72*3600)
	b.loop(t, "identity")
	var state, reason string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT state, coalesce(reason, '') FROM deliveries WHERE id = 'dl_stopped'`).Scan(&state, &reason); err != nil || state != "withheld" || reason != "superseded" {
		t.Fatalf("planned manager_stopped %s %s %v", state, reason, err)
	}
	if n := b.count(t, `SELECT count(*) FROM events WHERE type = 'delivery.withheld' AND subject = 'dl_stopped' AND data->>'reason' = 'superseded'`); n != 1 {
		t.Fatalf("delivery.withheld events %d", n)
	}
	if m := b.managerSeat(t); m.State != "held" || m.Holder != holder || len(m.Reasons) != 0 || m.DecideUntil != nil {
		t.Fatalf("seat %+v", m)
	}
	if cards := stoppedNotices(t, b, op); len(cards) != 0 {
		t.Fatalf("repeats %+v", cards)
	}
}
