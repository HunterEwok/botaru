// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"strings"
	"testing"
)

// The P2 views of messages show metadata and closed fields only; the note comes only to a person
// with audit.read and data:<mark> in the message's tenant, wrapped and audited; another tenant's
// message is the same not_found as a nonexistent one (MSG-18).
func TestMessageViews(t *testing.T) {
	ctx := context.Background()
	b := newMsgBenchWith(t, func(ms []map[string]any) []map[string]any {
		return append(ms, map[string]any{"apiVersion": "botaru/v1alpha1", "kind": "Group", "metadata": map[string]any{"name": "auditors", "tenant": "demo"},
			"spec": map[string]any{"caps": []any{"audit.read"}, "data": []any{"summary"}}})
	})
	b.enroll(t, EnrollRequest{Tenant: "demo", Role: "lead", Kind: "job", Labels: map[string]string{"zone": "a"}})
	b.hello(t, "1")
	att := b.claim(t, b.agent, "attempt")
	h := att.Payload["handles"].(map[string]any)["tools"].(string)
	const note = "the header line looked odd"
	if ok, _ := msgCall(t, b, h, sendMessageName, map[string]any{"type": "report.work.v1", "to": "role:lead", "note": note,
		"fields": map[string]any{"outcome": "done", "items": 3}}); !ok {
		t.Fatal("send")
	}
	if _, err := b.c.Complete(ctx, b.agent, header(t, att), CompleteRequest{Result: map[string]any{"answer": "hi"}}); err != nil {
		t.Fatal(err)
	}
	list, err := b.c.MsgList(ctx, b.op, MsgListRequest{Role: "lead", State: "queued"})
	if err != nil {
		t.Fatal(err)
	}
	items := list["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("list %v", list)
	}
	row := items[0].(map[string]any)
	if row["to"] != "role:lead" || row["from_role"] != "hello" || row["state"] != "queued" || row["note"] != nil || row["fields"] != nil {
		t.Fatalf("row %v", row)
	}
	id := row["id"].(string)
	if l, _ := b.c.MsgList(ctx, b.op, MsgListRequest{State: "acked"}); len(l["items"].([]any)) != 0 {
		t.Fatalf("a filter by state %v", l)
	}
	_, err = b.c.MsgList(ctx, b.op, MsgListRequest{Tenant: "other"})
	wantReason(t, err, "not_found")
	show, err := b.c.MsgShow(ctx, b.op, MsgShowRequest{ID: id})
	if err != nil {
		t.Fatal(err)
	}
	msg := show["message"].(map[string]any)
	if msg["note"] != nil || msg["fields"].(map[string]any)["outcome"] != "done" || msg["fields"].(map[string]any)["items"] != float64(3) {
		t.Fatalf("operator view %v", msg)
	}
	_, err = b.c.MsgShow(ctx, Operator{Principal: b.op.Principal, Tenant: "other"}, MsgShowRequest{ID: id})
	wantReason(t, err, "not_found")
	_, err = b.c.MsgShow(ctx, b.op, MsgShowRequest{ID: "msg_none"})
	wantReason(t, err, "not_found")
	// audit.read is privileged: the membership goes into the store directly.
	if _, err := b.c.st.Pool.Exec(ctx, `INSERT INTO memberships (tenant, principal, grp, state, created_at) VALUES ('demo', $1, 'auditors', 'active', now())`, b.alice); err != nil {
		t.Fatal(err)
	}
	show, err = b.c.MsgShow(ctx, Operator{Principal: b.alice, Tenant: "demo"}, MsgShowRequest{ID: id})
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := show["message"].(map[string]any)["note"].(string); !strings.HasPrefix(n, "<untrusted-") || !strings.Contains(n, note) {
		t.Fatalf("the auditor's view %v", show)
	}
	var reads int
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM audit WHERE record->>'command' = 'msg.show'`).Scan(&reads); err != nil || reads != 1 {
		t.Fatalf("audit of the read %d %v", reads, err)
	}
	st, err := b.c.InboxStatus(ctx, b.op, InboxStatusRequest{Role: "lead"})
	if err != nil {
		t.Fatal(err)
	}
	in := st["inboxes"].([]any)
	if len(in) != 1 || in[0].(map[string]any)["counts"].(map[string]any)["queued"] != 1 || in[0].(map[string]any)["capacity"] == nil {
		t.Fatalf("inbox status %v", st)
	}
	_, err = b.c.InboxStatus(ctx, b.op, InboxStatusRequest{Role: "nobody"})
	wantReason(t, err, "not_found")
}
