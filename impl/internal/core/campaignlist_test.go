// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/HunterEwok/botaru/impl/internal/ids"
)

// A campaign card shows the first 5 items; with more it carries the "full list" button, and a
// press of a person the card went to plans the list of every item from the record to that
// person, once, deciding nothing; a card of 5 items or fewer has no such button, and a person
// without the card cannot ask for the list (ORC-2).
func TestCampaignFullList(t *testing.T) {
	ctx := context.Background()
	b := newCampaignReadBench(t, map[string]any{"confirm_read_over": map[string]any{"items": 2}})
	bob := ids.PrincipalID()
	if _, err := b.c.Principal(ctx, b.op, PrincipalRequest{Op: "principal.add", Subject: bob, Identity: "telegram:1002"}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.c.st.Pool.Exec(ctx, `INSERT INTO memberships (tenant, principal, grp, state, created_at)
		SELECT tenant, $2, grp, 'active', now() FROM memberships WHERE principal = $1`, b.alice, bob); err != nil {
		t.Fatal(err)
	}
	att, h := b.attemptTools(t, "1", false)
	tmpl := map[string]any{"kind": "task", "type": "hello.child.v1", "goal": "Check the machine."}
	six := proposeCall(t, b, h, map[string]any{"template": tmpl, "items": map[string]any{"resources": []any{"web1", "web2", "web3", "web4", "web5", "web6"}}})
	three := proposeCall(t, b, h, map[string]any{"template": tmpl, "items": map[string]any{"resources": []any{"web1", "web2", "web3"}}})
	if six.State != "proposed" || three.State != "proposed" {
		t.Fatalf("propose %+v %+v", six, three)
	}
	b.finish(t, att)
	s := b.drain(t)
	var big, small sent
	for _, x := range s {
		if x.kind == campaignListKind {
			t.Fatalf("the full list went out before a press: %+v", x)
		}
		if x.kind == campaignApprovalCard && x.chat == "direct:1002" {
			switch x.body["card"].(map[string]any)["id"] {
			case six.Campaign:
				big = x
			case three.Campaign:
				small = x
			}
		}
	}
	codes := func(x sent) string {
		var out []string
		for _, bt := range x.body["card"].(map[string]any)["buttons"].([]any) {
			out = append(out, bt.(map[string]any)["code"].(string))
		}
		return strings.Join(out, ",")
	}
	if codes(big) != "approve,reject,full_list" || codes(small) != "approve,reject" {
		t.Fatalf("buttons %q %q", codes(big), codes(small))
	}
	validOut(t, b, "card", big.body["card"].(map[string]any))
	cmp := b.campaign(t, six.Campaign)
	list := DecideRequest{SubjectKind: "campaign", ID: cmp.ID, Nonce: cmp.Nonce, Decision: "full_list"}
	// The initiator has no card of the campaign in her chat.
	_, err := b.c.Decide(ctx, b.channel, press("telegram:1001", "direct:1001", big.external), list)
	wantReason(t, err, "forbidden")
	for range 2 {
		res, err := b.c.Decide(ctx, b.channel, press("telegram:1002", "direct:1002", big.external), list)
		if err != nil || res.State != "proposed" {
			t.Fatalf("full list %+v %v", res, err)
		}
	}
	s = b.drain(t)
	var lists []sent
	for _, x := range s {
		if x.kind == campaignListKind {
			lists = append(lists, x)
		}
	}
	if len(lists) != 1 || lists[0].chat != "direct:1002" || lists[0].body["text_key"] != "card.campaign.list" {
		t.Fatalf("lists %+v", lists)
	}
	params := lists[0].body["params"].(map[string]any)
	if fmt.Sprint(params["count"]) != "6" || !strings.Contains(params["items"].(string), "web6") {
		t.Fatalf("list params %v", params)
	}
	if cmp = b.campaign(t, cmp.ID); cmp.State != "proposed" || cmp.DecidedBy != "" {
		t.Fatalf("the list decided something: %+v", cmp)
	}
	// A card that shows every item has no list to give.
	small3 := b.campaign(t, three.Campaign)
	_, err = b.c.Decide(ctx, b.channel, press("telegram:1002", "direct:1002", small.external),
		DecideRequest{SubjectKind: "campaign", ID: small3.ID, Nonce: small3.Nonce, Decision: "full_list"})
	wantReason(t, err, "input_schema_invalid")
}
