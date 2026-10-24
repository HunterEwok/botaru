// SPDX-License-Identifier: AGPL-3.0-only

package telegram

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

// Fuzzing the whole outgoing text: whatever a model writes, the message fits Telegram, stays
// valid UTF-8 and opens no link outside the allowlist (INJ-11).
func FuzzOutText(f *testing.F) {
	for _, s := range []string{"see evil.example.com", strings.Repeat("\U0001F600", 2100) + " evil.example.com", "docs.example.org/a?b=c", "a​b.example.com"} {
		f.Add(s)
	}
	c := New(Config{LinkAllowlist: []string{"docs.example.org"}})
	f.Fuzz(func(t *testing.T, s string) {
		out := c.outText(s)
		if units(out) > 4096 {
			t.Fatalf("%d UTF-16 units", units(out))
		}
		if utf8.ValidString(s) && !utf8.ValidString(out) {
			t.Fatalf("%q gave invalid UTF-8 %q", s, out)
		}
		if o := opens(c, out); len(o) > 0 {
			t.Fatalf("%q gave %q, which opens %q", s, out, o)
		}
	})
}

// Fuzzing the button data a person's client sends back: it never panics, and what it accepts
// names a known subject, a nonce and a known button.
func FuzzParseData(f *testing.F) {
	for _, s := range []string{"apr_1:n:a", "::", "apr_1:n:a:x", "x_1:n:a"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		kind, id, nonce, code, ok := parseData(s)
		if !ok {
			return
		}
		if kind == "" || id == "" || nonce == "" || code == "" || strings.Contains(id+nonce, ":") {
			t.Fatalf("%q gave %q %q %q %q", s, kind, id, nonce, code)
		}
	})
}

// Fuzzing an update from the Bot API: the activity, when there is one, has a sender, no U+0000
// and at most 4096 characters of text.
func FuzzActivity(f *testing.F) {
	for _, s := range []string{
		`{"update_id":1,"message":{"message_id":2,"from":{"id":3},"chat":{"id":4,"type":"private"},"text":"hi"}}`,
		`{"update_id":1,"message":{"from":{"id":3,"is_bot":true},"chat":{"type":"group"},"caption":"a\u0000b","forward_origin":{"type":"user"}}}`,
		`{"message":{"sender_chat":{},"chat":{"type":"supergroup"},"quote":{},"external_reply":{"origin":{"type":"user","sender_user":{"id":9}}}}}`,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		var u update
		if json.Unmarshal(data, &u) != nil {
			return
		}
		a := Activity("tg", u)
		if a == nil {
			return
		}
		s, _ := a["sender"].(map[string]any)
		if id, _ := s["identity"].(string); !strings.HasPrefix(id, "telegram:") {
			t.Fatalf("sender %v", a["sender"])
		}
		if text, ok := a["text"].(string); ok && (strings.Contains(text, "\x00") || utf8.RuneCountInString(text) > 4096 || text == "") {
			t.Fatalf("text %q", text)
		}
		if a["chat"].(map[string]any)["kind"] != "direct" && a["chat"].(map[string]any)["kind"] != "group" {
			t.Fatalf("chat %v", a["chat"])
		}
	})
}
