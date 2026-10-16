// SPDX-License-Identifier: AGPL-3.0-only

package telegram

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/HunterEwok/botaru/impl/internal/spec"
	"github.com/HunterEwok/botaru/impl/internal/texts"
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

// The author of forwarded or quoted text is passed when the messenger shows a person (AUD-5).
func TestActivityAuthors(t *testing.T) {
	for _, tc := range []struct {
		msg, fwd, quote string
	}{
		{`"forward_origin":{"type":"user","sender_user":{"id":2002,"is_bot":false}}`, "telegram:2002", ""},
		{`"forward_origin":{"type":"hidden_user","sender_user_name":"X"}`, "", ""},
		{`"forward_origin":{"type":"user","sender_user":{"id":9,"is_bot":true}}`, "", ""},
		{`"quote":{"text":"x"},"reply_to_message":{"message_id":7,"from":{"id":3003,"is_bot":false}}`, "", "telegram:3003"},
		{`"reply_to_message":{"message_id":7,"from":{"id":3003,"is_bot":false}}`, "", ""},
		{`"external_reply":{"origin":{"type":"user","sender_user":{"id":4004,"is_bot":false}}}`, "", "telegram:4004"},
		{`"external_reply":{"origin":{"type":"channel"}}`, "", ""},
	} {
		a := Activity("telegram", parse(t, `{"update_id":9,"message":{"message_id":5,"from":{"id":1001,"is_bot":false},"chat":{"id":1001,"type":"private"},"text":"x",`+tc.msg+`}}`))
		if f, _ := a["forwarded_from"].(string); f != tc.fwd {
			t.Errorf("%s: forwarded_from %q", tc.msg, f)
		}
		if q, _ := a["quoted_from"].(string); q != tc.quote {
			t.Errorf("%s: quoted_from %q", tc.msg, q)
		}
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

// A digest is drawn from the catalog texts: the table, the hidden count and the model paragraph
// under its sources line as a separate part (REP-2).
func TestRenderDigest(t *testing.T) {
	root, err := spec.FindRoot("")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := texts.Load(root, "")
	if err != nil {
		t.Fatal(err)
	}
	c := New(Config{Texts: tx})
	got := c.renderDigest("en", "24h", []digestRow{{Title: "Greeting", State: "completed", Count: 2}}, 1, "All quiet.", []string{"web"}, false)
	for _, want := range []string{"Report, window 24h:", "Greeting, completed: 2", "Hidden as above this chat's class: 1.", "[AI text; it read text from: web]\nAll quiet."} {
		if !strings.Contains(got, want) {
			t.Fatalf("%q lacks %q", got, want)
		}
	}
	if got := c.renderDigest("en", "1h", nil, 0, "", nil, false); got != "Report, window 1h:\nNo tasks in the window." {
		t.Fatalf("%q", got)
	}
	// A group bound with model_text: labeled gets the code line before the sources, after the
	// table, and the paragraph never replaces the table (REP-2).
	got = c.renderDigest("en", "24h", []digestRow{{Title: "Greeting", State: "completed", Count: 2}}, 0, "All quiet.", []string{"tool:web/fetch"}, true)
	want := "Greeting, completed: 2\n\nBot text, check the numbers against the table.\n[AI text; it read text from: tool:web/fetch]\nAll quiet."
	if !strings.Contains(got, want) {
		t.Fatalf("%q lacks %q", got, want)
	}
}
