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
