// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// Tests of the leftovers of an offboarded person in bound chats (LCY-3, IDN-14).

// Offboarding (LCY-3, IDN-14): the principal.offboarded event with only the opaque id, the
// person out of every group, and the bound chat where they were seen named in principal_left;
// the notice repeats once a day for at most 7 days, and binding the chat again clears it.
func TestOffboardLeftovers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newGroupBench(t)
	bind := ChatBindRequest{Op: "chat.bind", Tenant: "demo", Channel: "telegram", Chat: Chat{Kind: "group", ExternalID: "-500"}, Ceiling: "summary", Name: "lab"}
	if _, err := b.c.ChatBind(ctx, b.op, bind); err != nil {
		t.Fatal(err)
	}
	b.inGroup(t, "1", "telegram:1001", "hi")
	if _, err := b.c.Principal(ctx, b.op, PrincipalRequest{Op: "principal.offboard", Subject: b.alice}); err != nil {
		t.Fatal(err)
	}
	var data map[string]any
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT data FROM events WHERE type = 'principal.offboarded.v1' AND subject = $1`, b.alice).Scan(&data); err != nil ||
		data["subject"] != b.alice || data["erasure"] == nil || len(data) != 2 {
		t.Fatalf("event %v %v", data, err)
	}
	if err := b.c.spec.Validate("principal-offboarded", data); err != nil {
		t.Fatalf("event data: %v", err)
	}
	var groups int
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM memberships WHERE tenant = 'demo' AND principal = $1`, b.alice).Scan(&groups); err != nil || groups != 0 {
		t.Fatalf("memberships %d %v", groups, err)
	}
	notices := func() (n int, chats string) {
		t.Helper()
		if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*), max(body->'params'->>'chats') FROM deliveries WHERE kind = 'principal_left' AND body->'params'->>'subject' = $1`, b.alice).Scan(&n, &chats); err != nil {
			t.Fatal(err)
		}
		return
	}
	if n, chats := notices(); n != 1 || chats != "lab" {
		t.Fatalf("principal_left %d %q", n, chats)
	}
	err := b.c.tx(ctx, func(tx pgx.Tx, _ time.Time) error {
		left, err := chatLeftovers(ctx, tx, "demo", "lab")
		if err == nil && (len(left) != 1 || left[0] != b.alice) {
			t.Errorf("leftovers of lab %v", left)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	for day := 1; day <= 8; day++ {
		b.advance(t, 86400)
		if err := b.c.tickLoop(ctx, "deadline"); err != nil {
			t.Fatal(err)
		}
	}
	// One at offboarding and one a day while less than 7 days have passed.
	if n, _ := notices(); n != principalLeftDays {
		t.Fatalf("principal_left after 8 days %d, want %d", n, principalLeftDays)
	}
	if _, err := b.c.ChatBind(ctx, b.op, bind); err != nil {
		t.Fatal(err)
	}
	var rest int
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM chat_leftovers`).Scan(&rest); err != nil || rest != 0 {
		t.Fatalf("leftovers after a new bind %d %v", rest, err)
	}
}

func hasReason(err error, reason string) bool {
	p, ok := AsProblem(err)
	return ok && p.Reason == reason
}
