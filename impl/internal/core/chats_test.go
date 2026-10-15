// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// newGroupBench is the memory bench whose hello type answers free text in group chats and
// has a closed summary field, with the delegated group researcher and the privileged group
// holders.
func newGroupBench(t *testing.T) *actionBench {
	t.Helper()
	b := newBenchWith(t, func(root string, ms []map[string]any) []map[string]any {
		for _, m := range ms {
			spec := m["spec"].(map[string]any)
			switch m["kind"] {
			case "Tenant":
				spec["default_types"] = []any{map[string]any{"type": "hello.greet.v1", "chats": []any{"group"}}}
				spec["delegated_groups"] = []any{"researcher"}
			case "TaskType":
				rs := spec["result_schema"].(map[string]any)
				rs["properties"].(map[string]any)["count"] = map[string]any{"type": "integer", "minimum": 0, "maximum": 100000}
				spec["summary_fields"] = []any{"count", "answer"}
			}
		}
		return append(ms, map[string]any{
			"apiVersion": "botaru/v1alpha1", "kind": "Group",
			"metadata": map[string]any{"name": "admins", "tenant": "demo"},
			"spec":     map[string]any{"caps": []any{"admin:people", "task:hello.greet.v1"}, "data": []any{"summary"}},
		}, map[string]any{
			"apiVersion": "botaru/v1alpha1", "kind": "Group",
			"metadata": map[string]any{"name": "researcher", "tenant": "demo"},
			"spec":     map[string]any{"caps": []any{"task:hello.greet.v1"}, "data": []any{"summary"}},
		}, map[string]any{
			"apiVersion": "botaru/v1alpha1", "kind": "Group",
			"metadata": map[string]any{"name": "holders", "tenant": "demo"},
			"spec":     map[string]any{"caps": []any{"hold.set"}},
		})
	})
	ctx := context.Background()
	ab := &actionBench{bench: b}
	ab.bob = "p_bob"
	if _, err := b.c.Principal(ctx, b.op, PrincipalRequest{Op: "principal.add", Subject: ab.bob, Identity: "telegram:1002"}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.c.st.Pool.Exec(ctx, `INSERT INTO memberships (tenant, principal, grp, state, created_at) VALUES ('demo', $1, 'admins', 'active', now())`, ab.bob); err != nil {
		t.Fatal(err)
	}
	return ab
}

func (b *actionBench) inGroup(t *testing.T, update, identity, text string) *ActivityResult {
	t.Helper()
	a := Activity{Kind: "message", Channel: "telegram", UpdateID: update, Chat: Chat{Kind: "group", ExternalID: "-500"}, Text: text}
	a.Sender.Identity, a.Sender.Verified = identity, true
	res, err := b.c.SubmitActivity(context.Background(), b.channel, a)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// A group chat works only after chat.bind; a ceiling above summary needs enforced membership;
// only /help and /about run there; the answer is split into a closed summary for the group
// and the full answer by direct message; a person outside the tenant stops the deliveries
// until the chat is bound again (DLV-7, DLV-15, PRM-15).
func TestGroupChat(t *testing.T) {
	ctx := context.Background()
	b := newGroupBench(t)
	if r := b.inGroup(t, "1", "telegram:1001", "hi"); r.Result != "ignored" {
		t.Fatalf("unbound group %+v", r)
	}
	bind := ChatBindRequest{Op: "chat.bind", Tenant: "demo", Channel: "telegram", Chat: Chat{Kind: "group", ExternalID: "-500"}, Ceiling: "internal", Name: "lab"}
	if _, err := b.c.ChatBind(ctx, b.op, bind); err == nil || !strings.Contains(err.Error(), "enforced") {
		t.Fatalf("a ceiling above summary without enforced membership: %v", err)
	}
	bind.Ceiling = "summary"
	if r, err := b.c.ChatBind(ctx, b.op, bind); err != nil || r.ID != "lab" {
		t.Fatalf("bind %+v %v", r, err)
	}
	if r := b.inGroup(t, "2", "telegram:1001", "/tasks"); r.Result != "reply" || r.Text != b.c.texts.Render("en", "cmd.direct_only", nil) {
		t.Fatalf("a command in a group %+v", r)
	}
	if r := b.inGroup(t, "3", "telegram:1001", "/help"); r.Result != "reply" || !strings.Contains(r.Text, "Greeting") {
		t.Fatalf("help in a group %+v", r)
	}
	r := b.inGroup(t, "4", "telegram:1001", "how many profiles?")
	if r.Result != "task" {
		t.Fatalf("free text in a group %+v", r)
	}
	att := b.claim(t, b.agent, "attempt")
	if _, err := b.c.Complete(ctx, b.agent, header(t, att), CompleteRequest{Result: map[string]any{"answer": "Seven profiles have remarks.", "count": 7}}); err != nil {
		t.Fatal(err)
	}
	got := b.drain(t)
	var group []string
	for _, s := range got {
		if s.chat == "group:-500" && s.kind == "task_result" {
			group = append(group, fmt.Sprint(s.body))
		}
	}
	if len(group) != 2 || !strings.Contains(group[0], "count: 7") || strings.Contains(strings.Join(group, " "), "Seven") || !strings.Contains(group[1], "delivery.details_private") {
		t.Fatalf("group deliveries %v", group)
	}
	if dm := cardOf(t, got, "task_result", "direct:1001"); !strings.Contains(fmt.Sprint(dm.body), "Seven") {
		t.Fatalf("direct answer %v", dm.body)
	}
	// A person outside the tenant writes: ignored, and the chat gets nothing until it is bound
	// again.
	if r := b.inGroup(t, "5", "telegram:9999", "hello all"); r.Result != "ignored" {
		t.Fatalf("a stranger %+v", r)
	}
	b.inGroup(t, "6", "telegram:1001", "/tasks")
	b.inGroup(t, "7", "telegram:1001", "and now?")
	b.finish(t, b.claim(t, b.agent, "attempt"))
	for _, s := range b.drain(t) {
		if s.chat == "group:-500" {
			t.Fatalf("a delivery reached a chat with an unknown member: %+v", s)
		}
	}
	var withheld int
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM deliveries WHERE target_id = 'group:-500' AND reason = 'unknown_member'`).Scan(&withheld); err != nil || withheld == 0 {
		t.Fatalf("withheld %d %v", withheld, err)
	}
	if _, err := b.c.ChatBind(ctx, b.op, bind); err != nil {
		t.Fatal(err)
	}
	if bd, _ := bindingByName(ctx, b.c.st.Pool, "demo", "lab"); bd == nil || bd.MembersUnknown {
		t.Fatalf("binding again did not clear the mark: %+v", bd)
	}
}

// An admin with admin:people grants a delegated group from a private chat, and the admins hear
// of it; a privileged group outside delegated_groups is for the operator only (PRM-16, PRM-7).
func TestDelegatedGrant(t *testing.T) {
	ctx := context.Background()
	b := newGroupBench(t)
	if r := b.say(t, "1", "telegram:1002", "1002", "/grant telegram:1001 researcher"); r.Result != "reply" || !strings.Contains(r.Text, "is now in group researcher") {
		t.Fatalf("grant %+v", r)
	}
	var n int
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM memberships WHERE principal = $1 AND grp = 'researcher' AND state = 'active'`, b.alice).Scan(&n); err != nil || n != 1 {
		t.Fatalf("membership %d %v", n, err)
	}
	if note := cardOf(t, b.drain(t), "notice", "direct:1002"); !strings.Contains(fmt.Sprint(note.body), "notice.delegated_grant") {
		t.Fatalf("admin notice %v", note.body)
	}
	if r := b.say(t, "2", "telegram:1002", "1002", "/grant telegram:1001 holders"); r.Text != b.c.texts.Render("en", "cmd.grant.privileged", map[string]any{"group": "holders"}) {
		t.Fatalf("privileged %+v", r)
	}
	if r := b.say(t, "3", "telegram:1001", "1001", "/grant telegram:1002 researcher"); r.Text != b.c.texts.Render("en", "task.rejected.no_group_cap", nil) {
		t.Fatalf("not an admin %+v", r)
	}
	if r := b.say(t, "4", "telegram:1002", "1002", "/ungrant telegram:1001 researcher"); !strings.Contains(r.Text, "no longer") {
		t.Fatalf("ungrant %+v", r)
	}
}

// The direct message of a split answer dies: the group gets the request to open a private
// chat, never the content (DLV-15).
func TestGroupAnswerNoDirect(t *testing.T) {
	ctx := context.Background()
	b := newGroupBench(t)
	if _, err := b.c.ChatBind(ctx, b.op, ChatBindRequest{Op: "chat.bind", Tenant: "demo", Channel: "telegram", Chat: Chat{Kind: "group", ExternalID: "-500"}, Ceiling: "summary"}); err != nil {
		t.Fatal(err)
	}
	b.inGroup(t, "1", "telegram:1001", "question")
	b.finish(t, b.claim(t, b.agent, "attempt"))
	var texts []string
	for range 20 {
		item := b.claim(t, b.channel, "delivery")
		if item == nil {
			break
		}
		d := item.Payload["delivery"].(map[string]any)
		chat := d["recipient"].(map[string]any)["chat"].(map[string]any)
		if chat["kind"] == "direct" && d["kind"] == "task_result" {
			if _, err := b.c.Fail(ctx, b.channel, header(t, item), FailRequest{Reason: "recipient_inactive"}); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if chat["kind"] == "group" {
			texts = append(texts, fmt.Sprint(d["body"]))
		}
		if _, err := b.c.Complete(ctx, b.channel, header(t, item), CompleteRequest{Receipt: &Receipt{ExternalID: fmt.Sprint(len(texts) + 100)}}); err != nil {
			t.Fatal(err)
		}
	}
	if !strings.Contains(strings.Join(texts, " "), "delivery.open_private_chat") {
		t.Fatalf("group texts %v", texts)
	}
}
