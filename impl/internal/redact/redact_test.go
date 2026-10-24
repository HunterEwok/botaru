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
		{"DB_PASSWORD=hunter2 next", "DB_PASSWORD=[secret] next"},
		{"db.password: hunter2", "db.password: [secret]"},
		{"X-Api-Key=abc123", "X-Api-Key=[secret]"},
		{"AWS_SECRET_ACCESS_KEY=abc/def", "AWS_SECRET_ACCESS_KEY=[secret]"},
		{`password = "two words"`, `password = "[secret]"`},
		{`password='two words' x`, `password='[secret]' x`},
		{`{"token":"a\"b c"}`, `{"token":"[secret]"}`},
		{"curl -H 'X: 1' --oauth2-bearer abcdefghijklmnop1234", "curl -H 'X: 1' --oauth2-bearer [secret]"},
		{"Bearer abcdefghijklmnop1234==", "Bearer [secret]"},
		{"a Bearer token is needed", "a Bearer token is needed"},
		{"bypass=1 compass: north", "bypass=1 compass: north"},
		{"password=a,token=b", "password=[secret],token=[secret]"},
		{"password: \"abc def\nnext", "password: \"[secret]\nnext"},
		{"'password' => 'hunter2'", "'password' => '[secret]'"},
		{`password := "hunter2"`, `password := "[secret]"`},
		{"the flag bearer responsibilities are many", "the flag bearer responsibilities are many"},
		{"standard bearer\nInternationalization1", "standard bearer\nInternationalization1"},
		{"first_pass=abc compass=x", "first_pass=abc compass=x"},
		{"Pass: 12, Fail: 0", "Pass: 12, Fail: 0"},
		{"pass=hunter2 smtp.pass=x", "pass=[secret] smtp.pass=[secret]"},
		{"user: admin\npass: Sup3rS3cret!", "user: admin\npass: [secret]"},
		{"redis-pass: abc e2e-pass: 5/5", "redis-pass: [secret] e2e-pass: 5/5"},
		{`{"pass": "hunter2"}`, `{"pass": "[secret]"}`},
		{"password:\n  hunter2\nnext: 1", "password:\n  [secret]\nnext: 1"},
		{"DB_PASS=hunter2 MYSQL_PWD=x", "DB_PASS=[secret] MYSQL_PWD=[secret]"},
		{"Reset your password:\nClick the link", "Reset your password:\nClick the link"},
		{"if token == nil {", "if token == nil {"},
		{`{"password": ""}`, `{"password": ""}`},
		{"Bearer authentication-required here", "Bearer authentication-required here"},
		{"pwd=abc", "pwd=[secret]"},
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
	w := Value(map[string]any{"DB_PASSWORD": "hunter2", "Token": "two words", "token_count": 5.0, "name": "x"}, []string{Secret}).(map[string]any)
	if w["DB_PASSWORD"] != "[secret]" || w["Token"] != "[secret]" || w["token_count"] != 5.0 || w["name"] != "x" {
		t.Fatalf("a value under a secret key %v", w)
	}
	if u := Value(map[string]any{"password": "x"}, []string{IP}).(map[string]any); u["password"] != "x" {
		t.Fatalf("masked without the secret kind %v", u)
	}
	for _, s := range []string{"password=[secret]", `"token": "[secret]"`, "Bearer [secret]", "Authorization: [secret]"} {
		if HasSecret(s) {
			t.Errorf("HasSecret(%q) on masked text", s)
		}
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
