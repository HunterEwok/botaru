// SPDX-License-Identifier: AGPL-3.0-only

package telegram

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/HunterEwok/botaru/impl/internal/spec"
	"github.com/HunterEwok/botaru/impl/internal/testbench"
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

// opens reports the links and mentions Telegram would mark in s that are not on the allowlist.
func opens(c *Channel, s string) []string {
	s = cleanText(s)
	var out []string
	kept := map[string]bool{}
	for _, l := range findLinks(s) {
		if c.verdict(l) != keep {
			out = append(out, s[l.from:l.to])
		} else {
			kept[s[l.from:l.to]] = true
		}
	}
	for _, m := range findMentions(s) {
		out = append(out, s[m[0]:m[1]])
	}
	for _, m := range schemeRE.FindAllString(s, -1) {
		if m = strings.TrimRight(m, ".,;:!?'`)"); !kept[m] {
			out = append(out, m)
		}
	}
	return out
}

func TestCutLinks(t *testing.T) {
	c := New(Config{LinkAllowlist: []string{"docs.example.org"}})
	got := c.cutLinks("see https://docs.example.org/a and http://evil.example.com/x or www.other.org.")
	want := "see https://docs.example.org/a and [link removed] or www\u2024other\u2024org."
	if got != want {
		t.Fatalf("%q", got)
	}
	// INJ-11: whatever the scheme, on any top-level domain, around a word edge, behind a user
	// part, after a kept link, and joined by what Telegram drops before it looks.
	for _, in := range []string{"evil.example.com/login", "evil.example.com", "t.me/evil_bot", "tg://resolve?domain=evil", "tg:resolve",
		"mailto:a@example.com", "a@example.com", "ftp://evil.example.com/f", "203.0.113.7/?d=1", "203.0.113.7:8080",
		"evil.example.cloud", "Evil.Example.ORG/path", "\u043f\u0440\u0438\u043c\u0435\u0440.\u0440\u0444",
		"xn--e1afmkfd.xn--p1ai", "login.example.zip/x", "javascript:alert(1)", "docs.example.org.evil.example.com/a",
		"tg://docs.example.org", "evil.gov", "evil.fr", "evil.ml", "evil.sh/x", "evil.onion", "wallet.ton/x", "evil.goo",
		"\u043f\u0440\u0438\u043c\u0435\u0440.\u043e\u043d\u043b\u0430\u0439\u043d",
		"\u043f\u0440\u0438\u0432\u0435\u0442https://evil.example.com", "1https://evil.example.com", "_https://evil.example.com",
		"a/https://evil.example.com", "note:evil.example.com/login", "docs.example.org@evil.example.com/login",
		"admin@evil.example.com/login", "https://docs.example.org@evil.example.com/", "evil.example.com.@docs.example.org",
		"foo://evil.example.com", "https://docs.example.org/a\u00a0evil.example.com/x", "https://docs.example.org/a\"evil.example.com/x",
		"docs.example.org/\u00abevil.example.com/x\u00bb", "https://docs.example.org/a\u2028evil.example.com", "(evil.example.com/x)",
		"https://evil.example.com:443/a", "HTTPS://EVIL.EXAMPLE.COM", "evil.example.com:8080", "q.com:1x.example.com",
		"evil\r.example.com", "evil\u2028.example.com", "evil.example\u202e.com", "evil.co\u0333m", "evil.co\u033fm",
		"https\r://evil.example.zz", "tg:\r//resolve?domain=evil", "a@evil\r.example.com", "docs.example.org\r@evil.example.com",
		"@evil_bot", "@evil.example.com", "@@evil_bot.example.com/x"} {
		src := "open " + in + " now"
		got := c.cutLinks(src)
		if left := opens(c, got); len(left) > 0 || got == src {
			t.Errorf("%q: %q leaves %q", in, got, left)
		}
	}
	// What an ops answer names stays: units, files, versions, times, a line of a file, a link of
	// the allowlist.
	for _, in := range []string{"nginx.service", "error.log", "v1.2.3", "12:30", "data.yaml", "main.go:42", "data.yaml:12",
		"nginx.conf/sites", "docs.example.org", "https://docs.example.org/guide", "note: restart", "3.5 GB",
		"/etc/nginx/nginx.conf", "#tag.com", "Done. Next", "docs.example.org/a?b=1#c", "db.internal:5432",
		"backup-2024.01.02.tar.gz", "a\nb"} {
		if got := c.cutLinks(in); got != in {
			t.Errorf("%q became %q", in, got)
		}
	}
	// A bare host, an IPv4 address and a mention only lose the dot or the @ Telegram links by.
	for in, want := range map[string]string{"run deploy.sh now": "run deploy\u2024sh now", "README.md": "README\u2024md",
		"host 192.0.2.5 is down": "host 192\u20240\u20242\u20245 is down", "192.0.2.0/24": "192\u20240\u20242\u20240/24",
		"ask @evilsupport": "ask \uff20evilsupport", "obj.info()": "obj\u2024info()", "a\tb\rc": "a bc"} {
		if got := c.cutLinks(in); got != want {
			t.Errorf("%q became %q, want %q", in, got, want)
		}
	}
}

// The cut to the Telegram length comes before the link check, so a cut word cannot become a
// link; a #tag@channel loses its @ as a mention does.
func TestOutText(t *testing.T) {
	c := New(Config{LinkAllowlist: []string{"docs.example.org"}})
	long := strings.Repeat("a", 4095-len("evil.example.com")-1) + " evil.example.comx"
	if got := c.outText(long); len(opens(c, got)) > 0 || utf8.RuneCountInString(got) > 4096 {
		t.Fatalf("the cut text opens %q", opens(c, got))
	}
	if got := c.outText(strings.Repeat("\U0001F600", 3000)); units(got) > 4096 {
		t.Fatalf("%d UTF-16 units", units(got))
	}
	for _, in := range []string{"g\u0130thub.docs.example.org", "d\u0130cs.example.org/a"} {
		if c := New(Config{LinkAllowlist: []string{"g\u0130thub.docs.example.org", "docs.example.org", "dics.example.org"}}); c.outText(in) == in {
			t.Errorf("%q kept", in)
		}
	}
	for _, in := range []string{"#news@evilchannel", "$ABC@evilchannel", "x\u0085@evil.example.com", "a\u0085b@-x.example.com"} {
		if got := c.outText(in); len(opens(c, got)) > 0 || strings.Contains(got, "@") {
			t.Errorf("%q: %q", in, got)
		}
	}
}

// Fuzzing the cut: whatever the text, the result opens no link outside the allowlist.
func FuzzCutLinks(f *testing.F) {
	for _, s := range []string{"see evil.example.com", "https://docs.example.org/a b.com", "x@y.example.org/z", "tg://a", "@abc.de"} {
		f.Add(s)
	}
	c := New(Config{LinkAllowlist: []string{"docs.example.org"}})
	f.Fuzz(func(t *testing.T, s string) {
		if out := c.cutLinks(s); len(opens(c, out)) > 0 {
			t.Fatalf("%q gave %q, which opens %q", s, out, opens(c, out))
		}
	})
}

// A digest is drawn from the catalog texts: the table, the hidden count and the model paragraph
// under its sources line as a separate part (REP-2).
func TestRenderDigest(t *testing.T) {
	root, err := spec.FindRoot("")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := testbench.SharedTexts(root)
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
