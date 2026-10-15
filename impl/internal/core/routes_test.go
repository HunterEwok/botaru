// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// newRouteBench is the message bench with three routes of tenant demo: wake-lead wakes the role
// lead by its inbox, tell-people tells the group hello of each report.work.v1, review-done
// reports the end of each lead.review.v1 to the role hello. The tenant allows one wake-up a day;
// a person of hello has a personal day limit of 1 EUR. sysReport grants system the report.
func newRouteBench(t *testing.T) *bench { t.Helper(); return newRouteBenchWith(t, true) }

func newRouteBenchWith(t *testing.T, sysReport bool) *bench {
	t.Helper()
	return newMsgBenchWith(t, func(ms []map[string]any) []map[string]any {
		for _, m := range ms {
			s := m["spec"].(map[string]any)
			name := m["metadata"].(map[string]any)["name"]
			switch {
			case m["kind"] == "Tenant" && name == "demo":
				s["messaging"] = map[string]any{"wakes_per_day": 1}
			case m["kind"] == "Role" && name == "hello":
				msgs := s["messages"].(map[string]any)
				msgs["receive"] = append(msgs["receive"].([]any), map[string]any{"type": "report.done.v1"})
			case m["kind"] == "TaskType" && name == "lead.review.v1":
				s["summary_fields"] = []any{"ok"}
			case m["kind"] == "Group" && name == "system-ops" && sysReport:
				s["caps"] = append(s["caps"].([]any), "msg.send:report.done.v1")
			case m["kind"] == "Group" && name == "hello":
				s["day_eur"] = 1.0
				s["caps"] = append(s["caps"].([]any), "msg.send:report.done.v1")
			}
		}
		route := func(name string, spec map[string]any) map[string]any {
			return map[string]any{"apiVersion": "botaru/v1alpha1", "kind": "Route", "metadata": map[string]any{"name": name, "tenant": "demo"}, "spec": spec}
		}
		return append(ms,
			map[string]any{"apiVersion": "botaru/v1alpha1", "kind": "MessageType", "metadata": map[string]any{"name": "report.done.v1"},
				"spec": map[string]any{"kind": "report", "data_class": "summary", "delivery": "one", "ttl": "1h", "producers": []any{"core"}, "max_bytes": 256,
					"note":          map[string]any{"max_chars": 0},
					"fields_schema": map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"ok": map[string]any{"type": "boolean"}}}}},
			route("wake-lead", map[string]any{"on": map[string]any{"inbox": "role:lead"}, "task": map[string]any{"type": "lead.review.v1"}}),
			route("tell-people", map[string]any{"on": map[string]any{"message": "report.work.v1"}, "mode": "notify", "template": "report-arrived",
				"deliver_to": []any{"group:hello"}}),
			route("review-done", map[string]any{"on": map[string]any{"task_done": "lead.review.v1"}, "mode": "report",
				"report": map[string]any{"type": "report.done.v1", "to": "role:hello"}}),
		)
	})
}

// wakeTasks are the wake-up tasks of the route wake-lead.
func wakeTasks(t *testing.T, b *bench) []string {
	t.Helper()
	rows, err := b.c.st.Pool.Query(context.Background(), `SELECT id FROM tasks WHERE idempotency_key LIKE 'system:wake:wake-lead:%' ORDER BY created_at`)
	if err != nil {
		t.Fatal(err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return ids
}

// An inbox wakes its role through a route from git: one wake-up task of system per window while
// one is open, with a depth not below the hops of its entries and a start_by within their
// expiry; the end of the task publishes the route's auto-report with its closed summary fields
// and the core key; the tenant's daily number of wake-ups skips the next one with wake.skipped
// (MSG-11, MSG-12, MSG-16).
func TestRouteWake(t *testing.T) {
	ctx := context.Background()
	b := newRouteBench(t)
	// The number of wake-ups counts a local day: the test runs at noon in the tenant's zone, so
	// its clock steps never cross midnight.
	b.atNoon(t, "Europe/Berlin")
	lead := b.enroll(t, EnrollRequest{Tenant: "demo", Role: "lead", Kind: "job", Labels: map[string]string{"zone": "a"}})
	b.loop(t, "inbox")
	if n := len(wakeTasks(t, b)); n != 0 {
		t.Fatalf("an empty inbox woke %d", n)
	}
	// The entries come from the tree of t_root, which pins its own tree budget.
	if _, err := b.c.st.Pool.Exec(ctx, `INSERT INTO tasks (id, tenant, type, state, initiator, priority_class, idempotency_key, body_hash, body, record, deadline_at, created_at, updated_at)
		VALUES ('t_root', 'demo', 'lead.review.v1', 'completed', $1, 1, 'root-of-entries', 'h', '{}', '{"root": "t_root", "tree_budget_micros": 4242}', now(), now(), now())`, b.alice); err != nil {
		t.Fatal(err)
	}
	coreSend(t, b, "role:lead")
	b.loop(t, "inbox")
	b.loop(t, "inbox")
	wakes := wakeTasks(t, b)
	if len(wakes) != 1 {
		t.Fatalf("wake-ups %d", len(wakes))
	}
	var initiator string
	var rec []byte
	var startBy, expires *time.Time
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT initiator, record, start_by_at, (SELECT max(expires_at) FROM messages WHERE inbox_name = 'lead')
		FROM tasks WHERE id = $1`, wakes[0]).Scan(&initiator, &rec, &startBy, &expires); err != nil {
		t.Fatal(err)
	}
	var r struct {
		Depth     int      `json:"depth"`
		WakeInbox string   `json:"wake_inbox"`
		Root      string   `json:"correlationid"`
		TreeRoot  string   `json:"root"`
		Budget    int64    `json:"tree_budget_micros"`
		Co        []string `json:"co_initiators"`
	}
	if err := json.Unmarshal(rec, &r); err != nil {
		t.Fatal(err)
	}
	if initiator != "system" || r.Depth != 1 || r.WakeInbox != "role:lead" || r.Root != "t_root" || startBy == nil || expires == nil || startBy.After(*expires) ||
		r.TreeRoot != "t_root" || r.Budget != 4242 || len(r.Co) != 1 || r.Co[0] != b.alice {
		t.Fatalf("wake-up %s %s %v %v", initiator, rec, startBy, expires)
	}
	// The wake-up reads its inbox and completes; its auto-report goes to hello.
	it := claimFor(t, b, lead, wakes[0])
	got, err := b.c.MsgReceive(ctx, lead, header(t, it), MsgReceiveRequest{Max: 20})
	if err != nil || len(got.Records) != 1 {
		t.Fatalf("read %+v %v", got, err)
	}
	if _, err := b.c.Complete(ctx, lead, header(t, it), CompleteRequest{Result: map[string]any{"ok": true}}); err != nil {
		t.Fatal(err)
	}
	var env []byte
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT envelope FROM messages WHERE inbox_kind = 'role' AND inbox_name = 'hello' AND type = 'report.done.v1' AND state = 'queued'
		AND idem_key = $1`, "system:report:"+wakes[0]+":review-done").Scan(&env); err != nil {
		t.Fatalf("auto-report: %v", err)
	}
	var e map[string]any
	if err := json.Unmarshal(env, &e); err != nil {
		t.Fatal(err)
	}
	if e["fromrole"] != "core" || e["dataorigin"] != "code" || e["hops"] != float64(2) || e["data"].(map[string]any)["fields"].(map[string]any)["ok"] != true {
		t.Fatalf("auto-report envelope %v", e)
	}
	// The usage line of a message goes to the sender's tree (BUD-3).
	var usage int
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM spend WHERE unit = 'message' AND task = $1 AND message->>'kind' = 'report.done.v1'`, wakes[0]).Scan(&usage); err != nil || usage != 1 {
		t.Fatalf("usage lines %d %v", usage, err)
	}
	// The second wake-up of the day is past the tenant's number.
	coreSend(t, b, "role:lead")
	b.advance(t, 6*60)
	b.loop(t, "inbox")
	if n := len(wakeTasks(t, b)); n != 1 {
		t.Fatalf("wake-ups past the number %d", n)
	}
	var reason string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT data->>'reason' FROM events WHERE type = 'wake.skipped' AND subject = 'wake-lead'`).Scan(&reason); err != nil || reason != "service_rate_limited" {
		t.Fatalf("wake.skipped %q %v", reason, err)
	}
}

// A route on message tells people only of an entry the code wrote and nobody distrusts, by the
// route's code text (MSG-16, INJ-7).
func TestRouteNotify(t *testing.T) {
	ctx := context.Background()
	b := newRouteBench(t)
	notices := func() int {
		var n int
		if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM deliveries WHERE kind = 'route_notice' AND body->>'text_key' = 'route.report-arrived'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	coreSend(t, b, "role:lead")
	if n := notices(); n != 1 {
		t.Fatalf("notices %d", n)
	}
	cat, err := b.c.Catalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		_, p, err := b.c.sendMessage(ctx, tx, now, cat, &msgSender{Tenant: "demo", Role: "core", Producer: "core", Root: "t_root", Untrusted: true, UntrustedOf: []string{"web"}},
			MsgSendRequest{Type: "report.work.v1", To: "role:lead", Fields: map[string]any{"outcome": "failed"}})
		if p != nil {
			t.Fatalf("send %v", p)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if n := notices(); n != 1 {
		t.Fatalf("an untrusted entry told people: %d", n)
	}
}

// apply refuses a message graph that cannot deliver: a pair without a receiving role, a route
// whose mode does not fit its source (MSG-5, MSG-16).
func TestRouteLint(t *testing.T) {
	ctx := context.Background()
	b := newRouteBench(t)
	route := map[string]any{"apiVersion": "botaru/v1alpha1", "kind": "Route", "metadata": map[string]any{"name": "bad", "tenant": "demo"},
		"spec": map[string]any{"on": map[string]any{"message": "report.work.v1"}, "mode": "task", "task": map[string]any{"type": "lead.review.v1"}}}
	res, err := b.c.Apply(ctx, b.op, ApplyRequest{Commit: "1111111111111111111111111111111111111111", Manifests: []map[string]any{route}, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range res.Errors {
		found = found || e.Code == "route_mode"
	}
	if !found {
		t.Fatalf("errors %+v", res.Errors)
	}
}

// A route's auto-report is refused, as message.rejected, when system has no msg.send for its
// type; the task ends anyway (MSG-5, MSG-16).
func TestRouteReportNeedsSystemCap(t *testing.T) {
	ctx := context.Background()
	b := newRouteBenchWith(t, false)
	lead := b.enroll(t, EnrollRequest{Tenant: "demo", Role: "lead", Kind: "job", Labels: map[string]string{"zone": "a"}})
	b.loop(t, "inbox")
	coreSend(t, b, "role:lead")
	b.loop(t, "inbox")
	b.loop(t, "inbox")
	wakes := wakeTasks(t, b)
	if len(wakes) != 1 {
		t.Fatalf("wake-ups %d", len(wakes))
	}
	it := claimFor(t, b, lead, wakes[0])
	if _, err := b.c.Complete(ctx, lead, header(t, it), CompleteRequest{Result: map[string]any{"ok": true}}); err != nil {
		t.Fatal(err)
	}
	var reason string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT data->>'reason' FROM events WHERE type = 'message.rejected' AND subject = 'report.done.v1'`).Scan(&reason); err != nil || reason != "msg_type_not_allowed" {
		t.Fatalf("message.rejected %q %v", reason, err)
	}
}

// The wake-ups a person's entries caused count in that person's day limit, though they run as
// system (MSG-11, BUD-1).
func TestWakeShare(t *testing.T) {
	ctx := context.Background()
	b := newRouteBench(t)
	if _, err := b.c.st.Pool.Exec(ctx, `INSERT INTO tasks (id, tenant, type, state, initiator, priority_class, idempotency_key, body_hash, body, record, deadline_at, created_at, updated_at)
		VALUES ('t_wake', 'demo', 'lead.review.v1', 'completed', 'system', 1, 'old-wake', 'h', '{}', jsonb_build_object('root', 't_wake', 'wake_subjects', jsonb_build_array($1::text)), now(), now(), now())`, b.alice); err != nil {
		t.Fatal(err)
	}
	cat, err := b.c.Catalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tv := cat.Tenants["demo"]
	share := func() *Problem {
		var p *Problem
		if err := b.c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
			groups, err := principalGroups(ctx, tx, now, tv, b.alice)
			if err != nil {
				return err
			}
			p, err = b.c.wakeShare(ctx, tx, now, tv, b.alice, groups, "work", 0)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return p
	}
	if p := share(); p != nil {
		t.Fatalf("an unspent day refused: %v", p)
	}
	if _, err := b.c.st.Pool.Exec(ctx, `INSERT INTO spend (id, tenant, task, root, subject, line, purpose, unit, state, eur_micros, time)
		VALUES ('sp_wake', 'demo', 't_wake', 't_wake', 'system', 'work', 'work', 'tokens', 'committed', 2000000, now())`); err != nil {
		t.Fatal(err)
	}
	if p := share(); p == nil || p.Reason != "budget_exhausted" {
		t.Fatalf("the wake-ups of the day passed: %v", p)
	}
}

// atNoon sets the bench clock to noon of today in the zone.
func (b *bench) atNoon(t *testing.T, zone string) {
	t.Helper()
	loc, err := time.LoadLocation(zone)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().In(loc)
	noon := time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, loc)
	if _, err := b.c.st.Pool.Exec(context.Background(), `UPDATE farm_meta SET clock_offset_us = $1`, noon.Sub(time.Now()).Microseconds()); err != nil {
		t.Fatal(err)
	}
}
