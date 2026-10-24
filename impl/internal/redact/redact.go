// SPDX-License-Identifier: AGPL-3.0-only

// Package redact masks addresses and secrets in tool output (INJ-4). The toolhost masks by
// the kinds a tool manifest declares in redact; the core checks the output once more for
// secrets before the runtime sees it, so a secret the toolhost missed still does not reach
// the model (SEC-1).
package redact

import (
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"
)

// Kinds of redaction of the ToolPlugin manifest.
const (
	IP     = "ip"
	Email  = "email"
	Secret = "secret"
)

// secretNames are the names whose value is a secret.
const secretNames = `(?:password|passwd|passphrase|secret|client_secret|secret_key|secret_access_key|token|access_token|refresh_token|auth_token|session_token|api[_\-]?key|apikey|private_key)`

// shortNames are secret names too common as parts of other words to take a prefix.
const shortNames = `(?:pwd|pass)`

var (
	ipv4 = regexp.MustCompile(`\b(?:25[0-5]|2[0-4][0-9]|1?[0-9]?[0-9])(?:\.(?:25[0-5]|2[0-4][0-9]|1?[0-9]?[0-9])){3}\b`)
	// IPv6: the full form with seven colons, or a compressed form with "::" and at least one
	// group of hex digits. Clock times (12:30:45) have neither and stay readable.
	ipv6Full = regexp.MustCompile(`(?i)\b(?:[0-9a-f]{1,4}:){7}[0-9a-f]{1,4}\b`)
	ipv6Comp = regexp.MustCompile(`(?i)(?:\b[0-9a-f]{1,4}(?::[0-9a-f]{1,4}){0,6})?::(?:[0-9a-f]{1,4}(?::[0-9a-f]{1,4}){0,6}\b)?`)
	email    = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9\-]+(?:\.[A-Za-z0-9\-]+)*\.[A-Za-z]{2,}`)

	privateKey = regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----[\s\S]*?(?:-----END [A-Z0-9 ]*PRIVATE KEY-----|\z)`)
	authHeader = regexp.MustCompile(`(?i)\b((?:proxy-)?authorization)\s*:\s*[^\r\n]*`)
	cookie     = regexp.MustCompile(`(?i)\b(set-cookie|cookie)\s*:\s*[^\r\n]*`)
	// A secret name, alone or as the last part of a longer name (DB_PASSWORD, db.password,
	// x-api-key), then =, :, => or := and the value: a quoted string whole (to the end of the
	// line when the closing quote is missing, as in a cut log line), or a bare word.
	keyValue = regexp.MustCompile(`(?i)(^|[^A-Za-z0-9_])((?:[A-Za-z0-9]+[_.\-])*` + secretNames + `|` + shortNames + `)(["']?\s*(?:=>|:=|[=:])\s*)("(?:[^"\\\n]|\\.)*"?|'[^'\n]*'?|[^\s"'&,;]+)`)
	// An object key that names a secret, for Value.
	secretKey = regexp.MustCompile(`(?i)^(?:(?:[A-Za-z0-9]+[_.\-])*` + secretNames + `|` + shortNames + `)$`)
	// A bearer token without its header, as in a command line or a log line: 16 characters or
	// more with a digit or a sign in them, so that "bearer" before a long word stays.
	bearer   = regexp.MustCompile(`(?i)\b(bearer)[ \t]+([A-Za-z0-9._~+/\-]{16,}=*)`)
	urlCreds = regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.\-]*://)[^\s/:@]+:[^\s/@]+@`)
	// Known token forms: Anthropic keys, this farm's own tokens and handles, cloud and chat
	// tokens, bot tokens and JSON Web Tokens.
	tokens = regexp.MustCompile(`\b(?:sk-ant-[A-Za-z0-9_\-]{8,}|sk-[A-Za-z0-9_\-]{20,}|(?:bw1|bh|bc2|bo2)_[A-Za-z0-9_\-]{16,}|AKIA[0-9A-Z]{16}|gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|xox[abprs]-[A-Za-z0-9\-]{10,}|[0-9]{6,12}:AA[A-Za-z0-9_\-]{30,}|eyJ[A-Za-z0-9_\-]{8,}\.eyJ[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,})`)
)

// Text masks the kinds in s.
func Text(s string, kinds []string) string {
	for _, k := range kinds {
		if k == Secret {
			s = Secrets(s)
		}
	}
	for _, k := range kinds {
		if k == Email {
			s = email.ReplaceAllString(s, "[email]")
		}
	}
	for _, k := range kinds {
		if k == IP {
			s = ipv6Full.ReplaceAllString(s, "[ip]")
			s = ipv6Comp.ReplaceAllStringFunc(s, func(m string) string {
				// "::" alone, as in a C++ name, is not an address.
				if strings.Trim(m, ":") == "" {
					return m
				}
				return "[ip]"
			})
			s = ipv4.ReplaceAllString(s, "[ip]")
		}
	}
	return s
}

// Secrets masks secrets: private key blocks, authorization and cookie headers, credentials in
// URLs, key=value pairs with a secret name and known token forms.
func Secrets(s string) string {
	s = privateKey.ReplaceAllString(s, "[secret]")
	s = authHeader.ReplaceAllString(s, "$1: [secret]")
	s = cookie.ReplaceAllString(s, "$1: [secret]")
	s = urlCreds.ReplaceAllString(s, "$1[secret]@")
	s = keyValue.ReplaceAllStringFunc(s, maskValue)
	s = bearer.ReplaceAllStringFunc(s, func(m string) string {
		g := bearer.FindStringSubmatch(m)
		if !strings.ContainsAny(g[2], "0123456789._~+/-=") {
			return m
		}
		return g[1] + " [secret]"
	})
	s = tokens.ReplaceAllString(s, "[secret]")
	return s
}

// maskValue masks the value of one keyValue match and keeps its quotes.
func maskValue(m string) string {
	g := keyValue.FindStringSubmatch(m)
	v := g[4]
	open, closed := "", ""
	if v[0] == '"' || v[0] == '\'' {
		open = v[:1]
		if len(v) > 1 && v[len(v)-1] == v[0] {
			closed = open
		}
	}
	return g[1] + g[2] + g[3] + open + "[secret]" + closed
}

// HasSecret says whether s still holds something Secrets would mask.
func HasSecret(s string) bool { return Secrets(s) != s }

// Value masks every string inside a JSON value, object keys included.
func Value(v any, kinds []string) any {
	switch x := v.(type) {
	case string:
		return Text(x, kinds)
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = Value(e, kinds)
		}
		return out
	case map[string]any:
		// Keys are masked too: an object key can carry a secret as well as a value.
		// A string under a key that names a secret is a secret whole: {"password": "x"}
		// has no "password=" in any one string.
		masks := slices.Contains(kinds, Secret)
		out := make(map[string]any, len(x))
		for k, e := range x {
			if _, ok := e.(string); ok && masks && secretKey.MatchString(k) {
				e = "[secret]"
			}
			out[Text(k, kinds)] = Value(e, kinds)
		}
		return out
	}
	return v
}

// Cut shortens s to at most n bytes on a rune boundary and says whether it cut.
func Cut(s string, n int) (string, bool) {
	if n <= 0 || len(s) <= n {
		return s, false
	}
	i := n
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	return s[:i], true
}
