// SPDX-License-Identifier: AGPL-3.0-only

package telegram

import (
	"encoding/json"
	"testing"
)

func parse(t *testing.T, s string) update {
	t.Helper()
	var u update
	if err := json.Unmarshal([]byte(s), &u); err != nil {
		t.Fatal(err)
	}
	return u
}

func TestActivity(t *testing.T) {
	a := Activity("telegram", parse(t, `{"update_id":5,"message":{"message_id":1,"from":{"id":1001,"is_bot":false},"chat":{"id":1001,"type":"private"},"text":"/hello hi"}}`))
	if a["update_id"] != "5" || a["text"] != "/hello hi" || a["chat"].(map[string]any)["kind"] != "direct" {
		t.Fatalf("activity %v", a)
	}
	if s := a["sender"].(map[string]any); s["identity"] != "telegram:1001" || s["verified"] != true {
		t.Fatalf("sender %v", s)
	}
	fwd := Activity("telegram", parse(t, `{"update_id":6,"message":{"message_id":2,"from":{"id":1001,"is_bot":false},"chat":{"id":-100,"type":"supergroup"},"text":"/hello","forward_origin":{}}}`))
	if fwd["forwarded"] != true || fwd["chat"].(map[string]any)["kind"] != "group" {
		t.Fatalf("forwarded %v", fwd)
	}
	onBehalf := Activity("telegram", parse(t, `{"update_id":7,"message":{"message_id":3,"from":{"id":1087968824,"is_bot":true},"sender_chat":{},"chat":{"id":-100,"type":"group"},"text":"/hello"}}`))
	if onBehalf["sender"].(map[string]any)["verified"] != false {
		t.Fatalf("on behalf of a chat %v", onBehalf)
	}
	if Activity("telegram", parse(t, `{"update_id":8,"message":{"message_id":4,"chat":{"id":-1,"type":"channel"},"text":"x"}}`)) != nil {
		t.Fatal("a channel post was submitted")
	}
}

func TestCutLinks(t *testing.T) {
	c := New(Config{LinkAllowlist: []string{"docs.example.org"}})
	got := c.cutLinks("see https://docs.example.org/a and http://evil.example.com/x or www.other.org")
	want := "see https://docs.example.org/a and [link removed] or [link removed]"
	if got != want {
		t.Fatalf("%q", got)
	}
}
