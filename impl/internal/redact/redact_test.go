// SPDX-License-Identifier: AGPL-3.0-only

package redact

import (
	"strings"
	"testing"
)

func TestText(t *testing.T) {
	all := []string{IP, Email, Secret}
	for _, c := range []struct{ in, want string }{
		{"from 192.0.2.12 port 22", "from [ip] port 22"},
		{"peer 2001:db8::1 and 2001:db8:0:0:0:0:0:1", "peer [ip] and [ip]"},
		{"at 12:30:45 in std::vector", "at 12:30:45 in std::vector"},
		{"mail alice@example.org now", "mail [email] now"},
		{"password=hunter2&x=1", "password=[secret]&x=1"},
		{`{"api_key": "abc123"}`, `{"api_key": "[secret]"}`},
		{"Authorization: Bearer abc.def", "Authorization: [secret]"},
		{"Cookie: sid=1; x=2\nnext", "Cookie: [secret]\nnext"},
		{"https://" + "u:pw" + "@example.org/x", "https://[secret]@example.org/x"},
		{"key sk-" + "ant-api03-abcdefghijkl", "key [secret]"},
		{"handle bh_abcdefghijklmnopqrst", "handle [secret]"},
		{"bot 123456789:" + "AAabcdefghijklmnopqrstuvwxyz0123456", "bot [secret]"},
		{"-----BEGIN RSA " + "PRIVATE KEY-----\nMIIE\n-----END RSA " + "PRIVATE KEY----- after", "[secret] after"},
		{"version 1.2.3 of 4", "version 1.2.3 of 4"},
	} {
		if got := Text(c.in, all); got != c.want {
			t.Errorf("Text(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	if got := Text("192.0.2.1 a@example.org", []string{Secret}); got != "192.0.2.1 a@example.org" {
		t.Errorf("secret only masked %q", got)
	}
	if !HasSecret("token: abc") || HasSecret("no secrets here") {
		t.Error("HasSecret")
	}
}

func TestValue(t *testing.T) {
	v := Value(map[string]any{"a": []any{"x 198.51.100.1", 3.0}, "b": "token=zz", "gh" + "p_abcdefghijklmnopqrstuvwxyz": 1.0}, []string{IP, Secret}).(map[string]any)
	if v["[secret]"] != 1.0 {
		t.Fatalf("a key was not masked: %v", v)
	}
	if v["a"].([]any)[0] != "x [ip]" || v["a"].([]any)[1] != 3.0 || v["b"] != "token=[secret]" {
		t.Fatalf("Value %v", v)
	}
}

func TestCut(t *testing.T) {
	r := "\u00e9" // 2 bytes
	got, cut := Cut(strings.Repeat(r, 10), 5)
	if !cut || got != r+r {
		t.Fatalf("Cut %q %v", got, cut)
	}
	if got, cut := Cut("abc", 5); cut || got != "abc" {
		t.Fatalf("short Cut %q %v", got, cut)
	}
}
