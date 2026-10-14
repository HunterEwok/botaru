// SPDX-License-Identifier: AGPL-3.0-only

// Package redact masks addresses and secrets in tool output (INJ-4). The toolhost masks by
// the kinds a tool manifest declares in redact; the core checks the output once more for
// secrets before the runtime sees it, so a secret the toolhost missed still does not reach
// the model (SEC-1).
package redact

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// Kinds of redaction of the ToolPlugin manifest.
const (
	IP     = "ip"
	Email  = "email"
	Secret = "secret"
)

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
	keyValue   = regexp.MustCompile(`(?i)\b(password|passwd|pwd|pass|secret|client_secret|token|access_token|refresh_token|api[_-]?key|apikey|private_key)("?\s*[=:]\s*"?)([^\s"'&,;]+)`)
	urlCreds   = regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.\-]*://)[^\s/:@]+:[^\s/@]+@`)
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
	s = keyValue.ReplaceAllString(s, "$1$2[secret]")
	s = tokens.ReplaceAllString(s, "[secret]")
	return s
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
		out := make(map[string]any, len(x))
		for k, e := range x {
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
