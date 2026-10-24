// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// A cancel that picked a task before its Complete committed does not overwrite the ended task:
// a terminal state is final, and no task.canceled event follows task.completed.
func TestCancelKeepsEndedTask(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newBench(t)
	a := Activity{Kind: "message", Channel: "telegram", UpdateID: "1", Chat: Chat{Kind: "direct", ExternalID: "1001"}, Text: "/hello hi"}
	a.Sender.Identity, a.Sender.Verified = "telegram:1001", true
	res, err := b.c.SubmitActivity(ctx, b.channel, a)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		w := b.claim(t, b.channel, "delivery")
		if _, err := b.c.Complete(ctx, b.channel, header(t, w), CompleteRequest{Receipt: &Receipt{ExternalID: "1"}}); err != nil {
			t.Fatal(err)
		}
	}
	att := b.claim(t, b.agent, "attempt")
	if att == nil || att.WorkID != res.Task {
		t.Fatalf("attempt %+v", att)
	}
	if _, err := b.c.Complete(ctx, b.agent, header(t, att), CompleteRequest{Result: map[string]any{"answer": "Hello!"}}); err != nil {
		t.Fatal(err)
	}
	if err := b.c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		return b.c.cancelTaskIDs(ctx, tx, now, "demo", []string{res.Task}, "campaign_stopped")
	}); err != nil {
		t.Fatal(err)
	}
	var state string
	var canceled int
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT state, (SELECT count(*) FROM events WHERE subject = $1 AND type = 'task.canceled') FROM tasks WHERE id = $1`,
		res.Task).Scan(&state, &canceled); err != nil {
		t.Fatal(err)
	}
	if state != "completed" || canceled != 0 {
		t.Fatalf("state %s, task.canceled events %d", state, canceled)
	}
}

// The reaper releases the grant of the attempt whose lease it ends, as closeLease does: the
// retry may claim the granted slot again (MGR-18).
func TestReaperReleasesGrant(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newBench(t)
	a := Activity{Kind: "message", Channel: "telegram", UpdateID: "1", Chat: Chat{Kind: "direct", ExternalID: "1001"}, Text: "/hello hi"}
	a.Sender.Identity, a.Sender.Verified = "telegram:1001", true
	res, err := b.c.SubmitActivity(ctx, b.channel, a)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		w := b.claim(t, b.channel, "delivery")
		if _, err := b.c.Complete(ctx, b.channel, header(t, w), CompleteRequest{Receipt: &Receipt{ExternalID: "1"}}); err != nil {
			t.Fatal(err)
		}
	}
	att := b.claim(t, b.agent, "attempt")
	if att == nil || att.WorkID != res.Task {
		t.Fatalf("attempt %+v", att)
	}
	if _, err := b.c.st.Pool.Exec(ctx, `INSERT INTO model_grants (id, tenant, task, lease_no, role, kind, line, profile, attempts, attempts_left, expires_at, state, created_at, updated_at)
		VALUES ('mg_01JB0000000000000000000001', 'demo', $1, $2, 'greeter', 'grant', 'grant', 'base-std', 2, 1, now() + interval '1 hour', 'active', now(), now())`, res.Task, att.LeaseNo); err != nil {
		t.Fatal(err)
	}
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE leases SET lease_until = now() - interval '1 minute' WHERE work_id = $1 AND state = 'active'`, res.Task); err != nil {
		t.Fatal(err)
	}
	b.loop(t, "reaper")
	var lease, grant string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT (SELECT state FROM leases WHERE work_id = $1 AND lease_no = $2), (SELECT state FROM model_grants WHERE task = $1)`,
		res.Task, att.LeaseNo).Scan(&lease, &grant); err != nil {
		t.Fatal(err)
	}
	if lease != "expired" || grant != "approved" {
		t.Fatalf("lease %s grant %s", lease, grant)
	}
}

// A store outage while the core runs does not spend the attempts of the running tasks: on the
// way out of not_ready the leases that ran out are extended before the reaper runs (FEN-5).
func TestStoreOutageKeepsAttempts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newBench(t)
	task := b.hello(t, "1")
	att := b.claim(t, b.agent, "attempt")
	if att == nil || att.WorkID != task {
		t.Fatalf("attempt %+v", att)
	}
	// The outage: the lease ran out while the store did not answer.
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE leases SET lease_until = now() - interval '2 minutes' WHERE work_id = $1 AND state = 'active'`, task); err != nil {
		t.Fatal(err)
	}
	b.c.storeDown.Store(true)
	// The fast loops wait while the store is down.
	if err := b.c.tickFast(ctx); err != nil {
		t.Fatal(err)
	}
	// The way out extends the leases, then runs every loop, the reaper included, and is ready.
	b.c.leaveStoreDown(ctx)
	if b.c.storeDown.Load() {
		t.Fatal("the store answers and the loops ticked, yet the core stays not_ready")
	}
	var lease, reason string
	var spent int
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT l.state, coalesce(l.reason, ''), t.attempts_spent FROM leases l JOIN tasks t ON t.id = l.work_id
		WHERE l.work_id = $1 AND l.lease_no = $2`, task, att.LeaseNo).Scan(&lease, &reason, &spent); err != nil {
		t.Fatal(err)
	}
	if lease != "active" || reason != "core_unavailable" || spent != 0 {
		t.Fatalf("lease %s(%s), spent %d", lease, reason, spent)
	}
}

// A drain loop skips a row it failed on for skipFor, and only that loop does.
func TestSkipList(t *testing.T) {
	var s skipList
	now := time.Now()
	s.add("reaper", "t1", now.Add(time.Minute))
	if ids := s.ids("reaper", now); len(ids) != 1 || ids[0] != "t1" {
		t.Fatalf("reaper skips %v", ids)
	}
	if ids := s.ids("deadline", now); len(ids) != 0 {
		t.Fatalf("deadline skips %v", ids)
	}
	if ids := s.ids("reaper", now.Add(2*time.Minute)); len(ids) != 0 {
		t.Fatalf("after the skip time %v", ids)
	}
}

// A row the reaper keeps failing on while the store answers is skipped: the other leases are
// still reaped, and the loop leaves its tick unmarked while the row stays skipped (OBS-9).
func TestReaperSkipsPoisonRow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newBench(t)
	bad := b.hello(t, "1")
	if att := b.claim(t, b.agent, "attempt"); att == nil || att.WorkID != bad {
		t.Fatalf("attempt %+v", att)
	}
	good := b.hello(t, "2")
	w2 := b.enroll(t, EnrollRequest{Tenant: "demo", Role: "hello", Kind: "agent"})
	if att := b.claim(t, w2, "attempt"); att == nil || att.WorkID != good {
		t.Fatalf("second attempt %+v", att)
	}
	// The bad lease ran out first, so the reaper meets it first.
	b.exec(t, `UPDATE leases SET lease_until = now() - interval '3 minutes' WHERE work_id = $1 AND state = 'active'`, bad)
	b.exec(t, `UPDATE leases SET lease_until = now() - interval '2 minutes' WHERE work_id = $1 AND state = 'active'`, good)
	b.exec(t, `CREATE FUNCTION poison_lease() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN IF NEW.work_id = '`+bad+`' THEN RAISE EXCEPTION 'poison row'; END IF; RETURN NEW; END $$`)
	b.exec(t, `CREATE TRIGGER poison_lease BEFORE UPDATE ON leases FOR EACH ROW EXECUTE FUNCTION poison_lease()`)
	if err := b.c.reap(ctx); err == nil {
		t.Fatal("a pass that skipped a row marks its tick")
	}
	var state string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT state FROM leases WHERE work_id = $1 ORDER BY lease_no DESC LIMIT 1`, good).Scan(&state); err != nil || state == "active" {
		t.Fatalf("the lease behind the poison row is %s %v", state, err)
	}
	if err := b.c.reap(ctx); err == nil {
		t.Fatal("the next pass marks its tick while the row stays skipped")
	}
}
