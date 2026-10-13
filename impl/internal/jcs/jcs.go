// SPDX-License-Identifier: AGPL-3.0-only

// Package jcs writes the canonical JSON form of RFC 8785 (JSON Canonicalization Scheme) and the
// sha256:<hex> hash over it, as spec/hashing.md requires. Input values have the shapes that
// package ijson returns: map[string]any, []any, string, json.Number, bool and nil; other Go
// values are first converted with ijson.Normalize.
package jcs

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"

	"github.com/HunterEwok/botaru/impl/internal/ijson"
)

// Canonical returns the RFC 8785 canonical form of v.
func Canonical(v any) ([]byte, error) {
	n, err := ijson.Normalize(v)
	if err != nil {
		return nil, err
	}
	var sb strings.Builder
	if err := write(&sb, n); err != nil {
		return nil, err
	}
	return []byte(sb.String()), nil
}

// Hash returns sha256:<64 lowercase hex digits> over the canonical form of v.
func Hash(v any) (string, error) {
	b, err := Canonical(v)
	if err != nil {
		return "", err
	}
	return HashBytes(b), nil
}

// HashBytes returns sha256:<hex> over b.
func HashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// ZeroHash is the prev_hash of the first audit record of a chain (spec/hashing.md).
const ZeroHash = "sha256:0000000000000000000000000000000000000000000000000000000000000000"

func write(sb *strings.Builder, v any) error {
	switch x := v.(type) {
	case nil:
		sb.WriteString("null")
	case bool:
		if x {
			sb.WriteString("true")
		} else {
			sb.WriteString("false")
		}
	case string:
		writeString(sb, x)
	case json.Number:
		s, err := number(string(x))
		if err != nil {
			return err
		}
		sb.WriteString(s)
	case []any:
		sb.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				sb.WriteByte(',')
			}
			if err := write(sb, e); err != nil {
				return err
			}
		}
		sb.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool { return lessUTF16(keys[i], keys[j]) })
		sb.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				sb.WriteByte(',')
			}
			writeString(sb, k)
			sb.WriteByte(':')
			if err := write(sb, x[k]); err != nil {
				return err
			}
		}
		sb.WriteByte('}')
	default:
		return fmt.Errorf("jcs: unsupported value of type %T", v)
	}
	return nil
}

// lessUTF16 orders strings by their UTF-16 code units, as RFC 8785 section 3.2.3 requires.
func lessUTF16(a, b string) bool {
	ua := utf16.Encode([]rune(a))
	ub := utf16.Encode([]rune(b))
	for i := 0; i < len(ua) && i < len(ub); i++ {
		if ua[i] != ub[i] {
			return ua[i] < ub[i]
		}
	}
	return len(ua) < len(ub)
}

func writeString(sb *strings.Builder, s string) {
	sb.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			sb.WriteString(`\"`)
		case '\\':
			sb.WriteString(`\\`)
		case '\b':
			sb.WriteString(`\b`)
		case '\f':
			sb.WriteString(`\f`)
		case '\n':
			sb.WriteString(`\n`)
		case '\r':
			sb.WriteString(`\r`)
		case '\t':
			sb.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(sb, `\u%04x`, r)
			} else {
				sb.WriteRune(r)
			}
		}
	}
	sb.WriteByte('"')
}

// number writes a JSON number literal the way ECMAScript Number.prototype.toString does, which
// is the number serialization of RFC 8785 section 3.2.2.3.
func number(lit string) (string, error) {
	f, err := strconv.ParseFloat(lit, 64)
	if err != nil {
		return "", fmt.Errorf("jcs: invalid number %q", lit)
	}
	return FormatFloat(f)
}

// FormatFloat formats f as ECMAScript Number.prototype.toString(10).
func FormatFloat(f float64) (string, error) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return "", fmt.Errorf("jcs: %v is not a JSON number", f)
	}
	if f == 0 {
		return "0", nil // also for -0
	}
	neg := f < 0
	if neg {
		f = -f
	}
	// Shortest digits that round-trip, in scientific notation: d.ddddde±x.
	sci := strconv.FormatFloat(f, 'e', -1, 64)
	mant, expStr, _ := strings.Cut(sci, "e")
	digits := strings.Replace(mant, ".", "", 1)
	exp, _ := strconv.Atoi(expStr)
	k := len(digits)
	n := exp + 1 // position of the decimal point relative to the digits
	var out string
	switch {
	case k <= n && n <= 21:
		out = digits + strings.Repeat("0", n-k)
	case 0 < n && n <= 21:
		out = digits[:n] + "." + digits[n:]
	case -6 < n && n <= 0:
		out = "0." + strings.Repeat("0", -n) + digits
	default:
		e := n - 1
		sign := "+"
		if e < 0 {
			sign = "-"
			e = -e
		}
		if k == 1 {
			out = digits + "e" + sign + strconv.Itoa(e)
		} else {
			out = digits[:1] + "." + digits[1:] + "e" + sign + strconv.Itoa(e)
		}
	}
	if neg {
		out = "-" + out
	}
	return out, nil
}
