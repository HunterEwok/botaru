// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"strings"
	"testing"
)

// say sends a direct message of a verified telegram person to the bot.
func (b *peopleBench) say(t *testing.T, ext, update, text string) *ActivityResult {
	t.Helper()
	a := Activity{Kind: "message", Channel: "telegram", UpdateID: update, Chat: Chat{Kind: "direct", ExternalID: ext}, Text: text}
	a.Sender.Identity, a.Sender.Verified = "telegram:"+ext, true
	res, err := b.c.SubmitActivity(context.Background(), b.channel, a)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func (b *peopleBench) invite(t *testing.T, op Operator, groups ...string) *InviteResult {
	t.Helper()
	r, err := b.c.Invite(context.Background(), op, InviteRequest{Op: "invite.create", Groups: groups})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func (b *peopleBench) membership(t *testing.T, subject, group string) string {
	t.Helper()
	var state string
	_ = b.c.st.Pool.QueryRow(context.Background(), `SELECT state FROM memberships WHERE tenant = 'demo' AND principal = $1 AND grp = $2`, subject, group).Scan(&state)
	return state
}

func (b *peopleBench) inviteState(t *testing.T, id string) string {
	t.Helper()
	var state string
	if err := b.c.st.Pool.QueryRow(context.Background(), `SELECT state FROM invitations WHERE id = $1`, id).Scan(&state); err != nil {
		t.Fatal(err)
	}
	return state
}

// Invitations (PRM-7, PRM-13, IDN-14): single-use and with an expiry; the link from a private
// chat makes a new person with the ordinary group active at once; a retried update repeats its
// reply; a used, revoked or expired invitation and one of an inactive creator answer the same
// text; a privileged group stays pending until invite.confirm checks the person's verification
// code, a wrong code changes nothing and is audited; offboarding revokes the unused invitations
// the person issued.
func TestInvitations(t *testing.T) {
	ctx := context.Background()
	b := newPeopleBench(t, nil)
	inv := b.invite(t, b.op, "hello")
	validOut(t, b.bench, "p2-invite-response", inv)
	if !strings.HasPrefix(inv.Code, inviteCodePrefix) || inv.State != "open" {
		t.Fatalf("invite %+v", inv)
	}
	var stored int
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM invitations WHERE invitations::text LIKE '%' || $1 || '%'`, inv.Code).Scan(&stored); err != nil || stored != 0 {
		t.Fatalf("the link code in the clear %d %v", stored, err)
	}
	invalid := b.c.texts.Render("en", "cmd.invite.invalid", nil)
	res := b.say(t, "1009", "u1", "/start "+inv.Code)
	if res.Result != "reply" || !strings.Contains(res.Text, "Welcome") {
		t.Fatalf("use %+v", res)
	}
	newcomer, err := principalByIdentity(ctx, b.c.st.Pool, "demo", "telegram:1009")
	if err != nil || b.membership(t, newcomer, "hello") != "active" {
		t.Fatalf("newcomer %s %v", newcomer, err)
	}
	if again := b.say(t, "1009", "u1", "/start "+inv.Code); again.Text != res.Text {
		t.Fatalf("a retried update %+v", again)
	}
	if reuse := b.say(t, "1012", "u2", "/start "+inv.Code); reuse.Text != invalid {
		t.Fatalf("a second use %+v", reuse)
	}
	if _, err := b.c.Invite(ctx, b.op, InviteRequest{Op: "invite.revoke", Invite: inv.Invite}); reasonOf(err) != "already_terminal" {
		t.Fatalf("revoke a used invitation: %v", err)
	}

	// Revoked and expired invitations.
	rev := b.invite(t, b.op, "hello")
	for range 2 {
		r, err := b.c.Invite(ctx, b.op, InviteRequest{Op: "invite.revoke", Invite: rev.Invite})
		if err != nil || r.State != "revoked" {
			t.Fatalf("revoke %+v %v", r, err)
		}
		validOut(t, b.bench, "p2-invite-response", r)
	}
	exp := b.invite(t, b.op, "hello")
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE invitations SET expires_at = botaru_now() - interval '1 second' WHERE id = $1`, exp.Invite); err != nil {
		t.Fatal(err)
	}
	for i, code := range []string{rev.Code, exp.Code, inviteCodePrefix + "nothing"} {
		if r := b.say(t, "1012", "u3"+string(rune('a'+i)), "/start "+code); r.Text != invalid {
			t.Fatalf("invitation %d: %+v", i, r)
		}
	}
	// Each failure is audited with its reason, though the text is the same.
	var reasons []string
	rows, err := b.c.st.Pool.Query(ctx, `SELECT record->>'reason' FROM audit WHERE tenant = 'demo' AND record->>'command' = 'invite.use' AND record->>'ok' = 'false' ORDER BY seq`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			t.Fatal(err)
		}
		reasons = append(reasons, r)
	}
	rows.Close()
	if strings.Join(reasons, ",") != "already_terminal,already_terminal,expired,not_found" {
		t.Fatalf("audited failures %v", reasons)
	}

	// A privileged group waits for the verification code (PRM-13).
	priv := b.invite(t, b.op, "staff", "hello")
	res = b.say(t, "1010", "u4", "/start "+priv.Code)
	carol, err := principalByIdentity(ctx, b.c.st.Pool, "demo", "telegram:1010")
	if err != nil {
		t.Fatal(err)
	}
	code := b.c.verificationCode("demo", carol)
	if !strings.Contains(res.Text, code) || b.membership(t, carol, "staff") != "pending" || b.membership(t, carol, "hello") != "active" {
		t.Fatalf("privileged use %+v", res)
	}
	// The stored outcome holds no copy of the code; a retried update shows it again.
	var copies int
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM activity_outcomes WHERE reply::text LIKE '%' || $1 || '%'`, code).Scan(&copies); err != nil || copies != 0 {
		t.Fatalf("the code in a stored outcome %d %v", copies, err)
	}
	if again := b.say(t, "1010", "u4", "/start "+priv.Code); again.Text != res.Text {
		t.Fatalf("a retried privileged use %+v", again)
	}
	if p, err := loadPrincipal(ctx, b.c.st.Pool, timeNow(t, b.bench), "demo", carol); err != nil || containsString(p.Groups, "staff") {
		t.Fatalf("a pending group grants: %+v %v", p, err)
	}
	wrong := "00000000"
	if wrong == code {
		wrong = "11111111"
	}
	if _, err := b.c.Invite(ctx, b.op, InviteRequest{Op: "invite.confirm", Invite: priv.Invite, Code: wrong}); reasonOf(err) != "forbidden" {
		t.Fatalf("a wrong code: %v", err)
	}
	var failed int
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM audit WHERE tenant = 'demo' AND record->>'command' = 'invite.confirm' AND record->>'ok' = 'false'`).Scan(&failed); err != nil || failed != 1 {
		t.Fatalf("the wrong code in the audit %d %v", failed, err)
	}
	if b.membership(t, carol, "staff") != "pending" {
		t.Fatal("a wrong code changed the membership")
	}
	for range 2 {
		r, err := b.c.Invite(ctx, b.op, InviteRequest{Op: "invite.confirm", Invite: priv.Invite, Code: code})
		if err != nil || r.State != "confirmed" {
			t.Fatalf("confirm %+v %v", r, err)
		}
	}
	if b.membership(t, carol, "staff") != "active" {
		t.Fatal("the confirmed group is not active")
	}
	// A pending group lapses when nobody checks the code in time.
	late := b.invite(t, b.op, "staff")
	b.say(t, "1014", "u6", "/start "+late.Code)
	dave, err := principalByIdentity(ctx, b.c.st.Pool, "demo", "telegram:1014")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE memberships SET pending_until = botaru_now() - interval '1 second' WHERE principal = $1`, dave); err != nil {
		t.Fatal(err)
	}
	if _, err := b.c.Invite(ctx, b.op, InviteRequest{Op: "invite.confirm", Invite: late.Invite, Code: b.c.verificationCode("demo", dave)}); err != nil {
		t.Fatal(err)
	}
	if b.membership(t, dave, "staff") != "pending" {
		t.Fatal("a lapsed pending group became active")
	}
	// grant --code activates a group pending from an invitation and confirms the invitation.
	viaGrant := b.invite(t, b.op, "staff")
	b.say(t, "1015", "u7", "/start "+viaGrant.Code)
	erin, err := principalByIdentity(ctx, b.c.st.Pool, "demo", "telegram:1015")
	if err != nil {
		t.Fatal(err)
	}
	if d, err := b.c.Grant(ctx, b.op, GrantRequest{Op: "grant", Subject: erin, Group: "staff", Code: b.c.verificationCode("demo", erin)}); err != nil || d.State != "active" {
		t.Fatalf("grant --code %+v %v", d, err)
	}
	if st := b.inviteState(t, viaGrant.Invite); st != "confirmed" {
		t.Fatalf("the invitation after grant --code is %s", st)
	}

	// The creator is checked again at use: an inactive creator's invitation does not work.
	creator := Operator{Principal: "human:p_01JB0000000000000000000C0D", Tenant: "demo"}
	if _, err := b.c.Principal(ctx, b.op, PrincipalRequest{Op: "principal.add", Subject: creator.Principal}); err != nil {
		t.Fatal(err)
	}
	off := b.invite(t, creator, "hello")
	if _, err := b.c.Principal(ctx, b.op, PrincipalRequest{Op: "principal.off", Subject: creator.Principal}); err != nil {
		t.Fatal(err)
	}
	if r := b.say(t, "1013", "u5", "/start "+off.Code); r.Text != invalid {
		t.Fatalf("an inactive creator %+v", r)
	}
	// IDN-14: offboarding revokes the unused invitations the person issued.
	if _, err := b.c.Principal(ctx, b.op, PrincipalRequest{Op: "principal.offboard", Subject: creator.Principal}); err != nil {
		t.Fatal(err)
	}
	if st := b.inviteState(t, off.Invite); st != "revoked" {
		t.Fatalf("after offboarding the invitation is %s", st)
	}
}

// P2 grant of a privileged group (PRM-13): pending without the code, a wrong code is forbidden,
// the person's code from /me makes it active; /me marks a group of delegated_groups (PRM-16).
func TestPrivilegedGrantCode(t *testing.T) {
	ctx := context.Background()
	b := newPeopleBench(t, nil)
	d, err := b.c.Grant(ctx, b.op, GrantRequest{Op: "grant", Subject: b.alice, Group: "staff"})
	if err != nil || d.State != "pending" {
		t.Fatalf("without the code %+v %v", d, err)
	}
	validOut(t, b.bench, "done-response", d)
	me := b.say(t, "1001", "m1", "/me")
	code := b.c.verificationCode("demo", b.alice)
	if !strings.Contains(me.Text, code) {
		t.Fatalf("/me without the code: %s", me.Text)
	}
	wrong := string('0'+('1'-'0'+code[0]-'0')%10) + code[1:] // another first digit, never the code itself
	if _, err := b.c.Grant(ctx, b.op, GrantRequest{Op: "grant", Subject: b.alice, Group: "staff", Code: wrong}); reasonOf(err) != "forbidden" {
		t.Fatalf("a wrong code: %v", err)
	}
	if b.membership(t, b.alice, "staff") != "pending" {
		t.Fatal("a wrong code changed the membership")
	}
	if d, err := b.c.Grant(ctx, b.op, GrantRequest{Op: "grant", Subject: b.alice, Group: "staff", Code: code}); err != nil || d.State != "active" {
		t.Fatalf("with the code %+v %v", d, err)
	}
	// An ordinary group is active at once.
	if d, err := b.c.Grant(ctx, b.op, GrantRequest{Op: "grant", Subject: b.alice, Group: "leads"}); err != nil || d.State != "active" {
		t.Fatalf("an ordinary group %+v %v", d, err)
	}
}

// PRM-16: /me shows a group of delegated_groups with the attribute delegated, and every admin
// hears of a delegated grant, removal or invitation made on P2.
func TestMeDelegatedMark(t *testing.T) {
	ctx := context.Background()
	b := newPeopleBench(t, func(ms []map[string]any) {
		for _, m := range ms {
			if m["kind"] == "Group" && m["metadata"].(map[string]any)["name"] == "leads" {
				m["metadata"].(map[string]any)["name"] = "researcher"
				m["spec"].(map[string]any)["caps"] = []any{"task.read:hello.greet.v1"}
			}
			if m["kind"] == "Group" && m["metadata"].(map[string]any)["name"] == "staff" {
				m["spec"].(map[string]any)["caps"] = []any{"admin:people"}
			}
		}
	})
	if _, err := b.c.Grant(ctx, b.op, GrantRequest{Op: "grant", Subject: b.alice, Group: "researcher"}); err != nil {
		t.Fatal(err)
	}
	me := b.say(t, "1001", "m1", "/me")
	if !strings.Contains(me.Text, "researcher (delegated)") || strings.Contains(me.Text, "hello (delegated)") {
		t.Fatalf("/me %s", me.Text)
	}
	// Every admin hears privately of a delegated group granted, removed or invited on P2
	// (PRM-16), as from a chat.
	if _, err := b.c.Grant(ctx, b.op, GrantRequest{Op: "grant", Subject: b.alice, Group: "staff", Code: b.c.verificationCode("demo", b.alice)}); err != nil {
		t.Fatal(err)
	}
	bob := "human:p_01JB0000000000000000000B0B"
	if _, err := b.c.Principal(ctx, b.op, PrincipalRequest{Op: "principal.add", Subject: bob, Identity: "telegram:1002"}); err != nil {
		t.Fatal(err)
	}
	for _, op := range []string{"grant", "ungrant"} {
		if _, err := b.c.Grant(ctx, b.op, GrantRequest{Op: op, Subject: bob, Group: "researcher"}); err != nil {
			t.Fatal(err)
		}
	}
	inv := b.invite(t, b.op, "researcher")
	b.say(t, "1016", "d1", "/start "+inv.Code)
	notices := func(key string) int {
		var n int
		if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM deliveries WHERE tenant = 'demo' AND recipient = $1 AND body->>'text_key' = $2`, b.alice, key).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if g, u := notices("notice.delegated_grant"), notices("notice.delegated_ungrant"); g != 2 || u != 1 {
		t.Fatalf("delegated notices: %d grants, %d removals", g, u)
	}
}

// On a production tenant a privileged grant needs the written request of another admin
// (PRM-13); the first admin of a tenant without one is invited without it, and the code check
// still holds.
func TestPrivilegedProduction(t *testing.T) {
	ctx := context.Background()
	b := newPeopleBench(t, func(ms []map[string]any) {
		for _, m := range ms {
			if m["kind"] == "Group" && m["metadata"].(map[string]any)["name"] == "leads" {
				m["metadata"].(map[string]any)["name"] = "admins"
				m["spec"].(map[string]any)["caps"] = []any{"admin:people"}
			}
		}
	})
	// A test bench applies only kind: test (APR-9); the stored tenant becomes production.
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE manifests SET doc = jsonb_set(doc, '{spec,kind}', '"production"') WHERE kind = 'Tenant' AND name = 'demo'`); err != nil {
		t.Fatal(err)
	}
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE apply_status SET set_hash = set_hash || 'x'`); err != nil {
		t.Fatal(err)
	}
	first := b.invite(t, b.op, "admins")
	b.say(t, "1011", "p1", "/start "+first.Code)
	admin, err := principalByIdentity(ctx, b.c.st.Pool, "demo", "telegram:1011")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.c.Invite(ctx, b.op, InviteRequest{Op: "invite.confirm", Invite: first.Invite, Code: b.c.verificationCode("demo", admin)}); err != nil {
		t.Fatal(err)
	}
	if b.membership(t, admin, "admins") != "active" {
		t.Fatal("the first admin is not active")
	}
	if _, err := b.c.Invite(ctx, b.op, InviteRequest{Op: "invite.create", Groups: []string{"staff"}}); reasonOf(err) != "forbidden" {
		t.Fatalf("an invitation without requested_by: %v", err)
	}
	if _, err := b.c.Grant(ctx, b.op, GrantRequest{Op: "grant", Subject: b.alice, Group: "staff"}); reasonOf(err) != "forbidden" {
		t.Fatalf("a grant without requested_by: %v", err)
	}
	if _, err := b.c.Invite(ctx, b.op, InviteRequest{Op: "invite.create", Groups: []string{"staff"}, RequestedBy: admin}); err != nil {
		t.Fatalf("with requested_by: %v", err)
	}
	if d, err := b.c.Grant(ctx, b.op, GrantRequest{Op: "grant", Subject: b.alice, Group: "staff", RequestedBy: admin}); err != nil || d.State != "pending" {
		t.Fatalf("grant with requested_by %+v %v", d, err)
	}
	// The admin hears of the active grant privately (PRM-13).
	if _, err := b.c.Grant(ctx, b.op, GrantRequest{Op: "grant", Subject: b.alice, Group: "staff", RequestedBy: admin, Code: b.c.verificationCode("demo", b.alice)}); err != nil {
		t.Fatal(err)
	}
	var notices int
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM deliveries WHERE tenant = 'demo' AND recipient = $1 AND body->>'text_key' = 'notice.privileged_grant'`, admin).Scan(&notices); err != nil || notices != 2 {
		t.Fatalf("admin notices %d %v", notices, err)
	}
}
