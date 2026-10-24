// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/HunterEwok/botaru/impl/internal/ids"
	"github.com/HunterEwok/botaru/impl/internal/store"
)

// Offboarding is final (LCY-8, IDN-13, blackbox:core/principal-offboard-final): the state
// offboarded, the identity links deleted and the tasks canceled in one transaction, the
// principal_left notice to the direct chat of an admin with admin:people, subject_closed for
// every people operation on the principal, and the identity free for a new principal without
// the old groups.
func TestPrincipalOffboardFinal(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newBench(t)
	ms, _ := helloSet(t)
	ms = append(ms, map[string]any{"apiVersion": "botaru/v1alpha1", "kind": "Group", "metadata": map[string]any{"name": "people-admins", "tenant": "demo"},
		"spec": map[string]any{"caps": []any{"admin:people"}, "data": []any{"summary"}}})
	if res, err := b.c.Apply(ctx, farmOf(b.op), ApplyRequest{Commit: "7777777777777777777777777777777777777777", Manifests: ms}); err != nil || !res.Applied {
		t.Fatalf("apply: %+v %v", res, err)
	}
	admin := ids.PrincipalID()
	if _, err := b.c.Principal(ctx, b.op, PrincipalRequest{Op: "principal.add", Subject: admin, Identity: "telegram:1002"}); err != nil {
		t.Fatal(err)
	}
	// A privileged group is granted only through the verification flow, which this version
	// lacks; the test seeds the membership directly.
	if _, err := b.c.st.Pool.Exec(ctx, `INSERT INTO memberships (tenant, principal, grp, state, created_at) VALUES ('demo', $1, 'people-admins', 'active', botaru_now())`, admin); err != nil {
		t.Fatal(err)
	}
	task := b.hello(t, "41")

	done, err := b.c.Principal(ctx, b.op, PrincipalRequest{Op: "principal.offboard", Subject: b.alice})
	if err != nil || done.State != "offboarded" {
		t.Fatalf("offboard: %+v %v", done, err)
	}
	var state, reason string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT state, reason FROM tasks WHERE id = $1`, task).Scan(&state, &reason); err != nil || state != "canceled" || reason != "person_inactive" {
		t.Fatalf("task %s %s %v", state, reason, err)
	}
	var links int
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM identities WHERE tenant = 'demo' AND principal = $1`, b.alice).Scan(&links); err != nil || links != 0 {
		t.Fatalf("identity links %d %v", links, err)
	}
	var body []byte
	var target, recipient, addr string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT body, target_id, recipient, addr FROM deliveries WHERE kind = 'principal_left'`).Scan(&body, &target, &recipient, &addr); err != nil {
		t.Fatalf("principal_left: %v", err)
	}
	if addr != "admins" {
		t.Fatalf("principal_left address %q, want admins (DLV-6)", addr)
	}
	var nb struct {
		TextKey string         `json:"text_key"`
		Params  map[string]any `json:"params"`
	}
	_ = json.Unmarshal(body, &nb)
	if target != "direct:1002" || recipient != admin || nb.TextKey != "notice.principal_left" ||
		nb.Params["subject"] != b.alice || nb.Params["reason"] != "offboarding" || nb.Params["chats"] != "none" {
		t.Fatalf("principal_left to %s for %s: %s", target, recipient, body)
	}

	for _, op := range []string{"principal.on", "principal.off", "principal.set_identity"} {
		_, err := b.c.Principal(ctx, b.op, PrincipalRequest{Op: op, Subject: b.alice, Identity: "telegram:1009"})
		if p, _ := AsProblem(err); p == nil || p.Reason != "subject_closed" {
			t.Fatalf("%s: %v", op, err)
		}
	}
	_, err = b.c.Grant(ctx, b.op, GrantRequest{Op: "grant", Subject: b.alice, Group: "hello"})
	if p, _ := AsProblem(err); p == nil || p.Reason != "subject_closed" {
		t.Fatalf("grant: %v", err)
	}
	_, err = b.c.Principal(ctx, b.op, PrincipalRequest{Op: "principal.add", Subject: b.alice, Identity: "telegram:1009"})
	if p, _ := AsProblem(err); p == nil || p.Reason != "subject_closed" {
		t.Fatalf("principal.add of an offboarded id: %v", err)
	}
	if again, err := b.c.Principal(ctx, b.op, PrincipalRequest{Op: "principal.offboard", Subject: b.alice}); err != nil || again.State != "offboarded" {
		t.Fatalf("repeat: %+v %v", again, err)
	}
	// One erasure request by the scope (tenant, subject), waiting for the erasure work; the
	// repeat adds none.
	var requests int
	var eState, eReason, author string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) OVER (), state, reason, author->>'id' FROM erasure_requests WHERE tenant = 'demo' AND subject = $1`, b.alice).
		Scan(&requests, &eState, &eReason, &author); err != nil || requests != 1 || eState != "requested" || eReason != "offboarding" || author != b.op.Principal {
		t.Fatalf("erasure request: %d %s %s %s %v", requests, eState, eReason, author, err)
	}
	var lefts int
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM deliveries WHERE kind = 'principal_left' AND body->'params'->>'subject' = $1`, b.alice).Scan(&lefts); err != nil || lefts != 1 {
		t.Fatalf("principal_left notices after a repeated offboard: %d %v", lefts, err)
	}

	// The identity is free: a new principal gets it, without the old groups, and a message from
	// it reaches the new principal (refused for the missing group, not ignored).
	next := ids.PrincipalID()
	if _, err := b.c.Principal(ctx, b.op, PrincipalRequest{Op: "principal.add", Subject: next, Identity: "telegram:1001"}); err != nil {
		t.Fatal(err)
	}
	p, err := loadPrincipal(ctx, b.c.st.Pool, timeNow(t, b), "demo", next)
	if err != nil || len(p.Groups) != 0 || p.State != "active" {
		t.Fatalf("new principal %+v %v", p, err)
	}
	a := Activity{Kind: "message", Channel: "telegram", UpdateID: "42", Chat: Chat{Kind: "direct", ExternalID: "1001"}, Text: "/hello hi"}
	a.Sender.Identity, a.Sender.Verified = "telegram:1001", true
	res, err := b.c.SubmitActivity(ctx, b.channel, a)
	if err != nil || res.Result != "reply" || res.Text != b.c.texts.Render("en", "task.rejected.no_group_cap", nil) {
		t.Fatalf("message from the freed identity: %+v %v", res, err)
	}
	// Disabling a person sends the same notice with its own reason (IDN-13).
	if _, err := b.c.Principal(ctx, b.op, PrincipalRequest{Op: "principal.off", Subject: next}); err != nil {
		t.Fatal(err)
	}
	// A repeated off changes no state and sends no second notice.
	if _, err := b.c.Principal(ctx, b.op, PrincipalRequest{Op: "principal.off", Subject: next}); err != nil {
		t.Fatal(err)
	}
	var offReason string
	var offs int
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) OVER (), body->'params'->>'reason' FROM deliveries WHERE kind = 'principal_left' AND body->'params'->>'subject' = $1`, next).
		Scan(&offs, &offReason); err != nil || offs != 1 || offReason != "person_inactive" {
		t.Fatalf("principal_left of off: %d %q %v", offs, offReason, err)
	}
}

// With no admin with admin:people reachable, the departure still goes through; the audit
// record of the command shows that no notice was planned (IDN-13).
func TestPrincipalLeftWithoutAdmin(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newBench(t)
	done, err := b.c.Principal(ctx, b.op, PrincipalRequest{Op: "principal.offboard", Subject: b.alice})
	if err != nil || done.State != "offboarded" {
		t.Fatalf("offboard: %+v %v", done, err)
	}
	var after map[string]any
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT record->'after' FROM audit WHERE tenant = 'demo' AND seq = $1`, done.AuditSeq).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after["principal_left"] != 0.0 || after["state"] != "offboarded" || after["erasure_request"] == nil {
		t.Fatalf("audit after: %v", after)
	}
	var notices int
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM deliveries WHERE kind = 'principal_left'`).Scan(&notices); err != nil || notices != 0 {
		t.Fatalf("notices %d %v", notices, err)
	}
}

func timeNow(t *testing.T, b *bench) time.Time {
	t.Helper()
	now, err := store.Now(context.Background(), b.c.st.Pool)
	if err != nil {
		t.Fatal(err)
	}
	return now
}

// firstContact makes alice write to the bot, which plans the first-contact notices for her.
func firstContact(t *testing.T, b *bench) {
	t.Helper()
	a := Activity{Kind: "message", Channel: "telegram", UpdateID: "77", Chat: Chat{Kind: "direct", ExternalID: "1001"}, Text: "/hello hi"}
	a.Sender.Identity, a.Sender.Verified = "telegram:1001", true
	if _, err := b.c.SubmitActivity(context.Background(), b.channel, a); err != nil {
		t.Fatal(err)
	}
}

func deliveriesTo(t *testing.T, b *bench, principal, state string) int {
	t.Helper()
	var n int
	if err := b.c.st.Pool.QueryRow(context.Background(), `SELECT count(*) FROM deliveries WHERE recipient = $1 AND state = $2`, principal, state).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestOffboardWithholdsDeliveries: deliveries still planned for an offboarded person end
// withheld in the same transaction, code notices included, because the chat id they carry
// may later belong to a new principal of the freed identity (DLV-6, LCY-8).
func TestOffboardWithholdsDeliveries(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newBench(t)
	firstContact(t, b)
	if deliveriesTo(t, b, b.alice, "planned") == 0 {
		t.Fatal("no planned delivery for alice after first contact")
	}
	if _, err := b.c.Principal(ctx, b.op, PrincipalRequest{Op: "principal.offboard", Subject: b.alice}); err != nil {
		t.Fatal(err)
	}
	if n := deliveriesTo(t, b, b.alice, "planned"); n != 0 {
		t.Fatalf("%d deliveries for the offboarded person are still planned", n)
	}
	var reason string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT reason FROM deliveries WHERE recipient = $1 AND state = 'withheld' LIMIT 1`, b.alice).Scan(&reason); err != nil || reason != "person_inactive" {
		t.Fatalf("withheld reason %q %v", reason, err)
	}
	if w := b.claim(t, b.channel, "delivery"); w != nil {
		t.Fatalf("a delivery was handed out: %+v", w)
	}
}

// TestHandoffChecksRecipient: a code notice without a data mark is not handed to a channel
// once its recipient is disabled; it ends withheld (DLV-6).
func TestHandoffChecksRecipient(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newBench(t)
	firstContact(t, b)
	if _, err := b.c.Principal(ctx, b.op, PrincipalRequest{Op: "principal.off", Subject: b.alice}); err != nil {
		t.Fatal(err)
	}
	if deliveriesTo(t, b, b.alice, "planned") == 0 {
		t.Fatal("principal.off should leave the planned notices to the handoff check")
	}
	if w := b.claim(t, b.channel, "delivery"); w != nil {
		t.Fatalf("a delivery to a disabled person was handed out: %+v", w)
	}
	if n := deliveriesTo(t, b, b.alice, "planned"); n != 0 {
		t.Fatalf("%d deliveries still planned after the handoff check", n)
	}
	// Re-enabled, she gets the notices again at her next message.
	if _, err := b.c.Principal(ctx, b.op, PrincipalRequest{Op: "principal.on", Subject: b.alice}); err != nil {
		t.Fatal(err)
	}
	a := Activity{Kind: "message", Channel: "telegram", UpdateID: "78", Chat: Chat{Kind: "direct", ExternalID: "1001"}, Text: "/hello again"}
	a.Sender.Identity, a.Sender.Verified = "telegram:1001", true
	if _, err := b.c.SubmitActivity(ctx, b.channel, a); err != nil {
		t.Fatal(err)
	}
	var notices int
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM deliveries WHERE recipient = $1 AND state = 'planned' AND kind IN ('ai_notice', 'privacy_notice')`, b.alice).Scan(&notices); err != nil || notices != 2 {
		t.Fatalf("planned notices after principal.on: %d %v", notices, err)
	}
}
