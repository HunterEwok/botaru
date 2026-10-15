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
// reports the end of each lead.review.v1 to the role hello. The tenant allows one wake-up a day.
func newRouteBench(t *testing.T) *bench {
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
