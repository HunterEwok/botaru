// SPDX-License-Identifier: AGPL-3.0-only

// Package ijson parses JSON in the strict I-JSON subset (RFC 7493) that the HTTP binding v1
// requires before any schema check (spec/bindings/http/README.md, spec/hashing.md): a repeated
// key in an object, invalid UTF-8, a lone surrogate, U+0000 in a string (the store cannot hold
// it) and a number outside ±(2^53 - 1) are errors.
// Values are returned as map[string]any, []any, string, json.Number, bool and nil, so the schema
// check, the handler and the hashes see the same value.
package ijson

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// MaxSafeInteger is 2^53 - 1, the largest integer that I-JSON allows.
const MaxSafeInteger = 1<<53 - 1

// Limits bound the size of a parsed document. A zero field means no limit for that field.
type Limits struct {
	MaxDepth int
}

// DefaultLimits are used by Parse.
var DefaultLimits = Limits{MaxDepth: 64}

// Error is a parse error with the byte offset where it was found.
type Error struct {
	Offset int
	Msg    string
}

func (e *Error) Error() string { return fmt.Sprintf("ijson: %s at offset %d", e.Msg, e.Offset) }

// ErrDepth is wrapped by the error returned for a document deeper than the limit.
var ErrDepth = errors.New("nesting too deep")

// Parse parses one JSON value with the default limits.
func Parse(data []byte) (any, error) { return ParseWithLimits(data, DefaultLimits) }

// ParseWithLimits parses one JSON value; trailing data other than whitespace is an error.
func ParseWithLimits(data []byte, lim Limits) (any, error) {
	if !utf8.Valid(data) {
		return nil, &Error{Offset: invalidUTF8Offset(data), Msg: "invalid UTF-8"}
	}
	p := &parser{data: data, lim: lim}
	p.skipWS()
	v, err := p.value(0)
	if err != nil {
		return nil, err
	}
	p.skipWS()
	if p.pos != len(p.data) {
		return nil, p.errf("trailing data")
	}
	return v, nil
}

// ParseObject parses a JSON document that must be an object.
func ParseObject(data []byte) (map[string]any, error) {
	v, err := Parse(data)
	if err != nil {
		return nil, err
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, &Error{Offset: 0, Msg: "not an object"}
	}
	return m, nil
}

func invalidUTF8Offset(b []byte) int {
	for i := 0; i < len(b); {
		r, n := utf8.DecodeRune(b[i:])
		if r == utf8.RuneError && n <= 1 {
			return i
		}
		i += n
	}
	return len(b)
}

type parser struct {
	data []byte
	pos  int
	lim  Limits
}

func (p *parser) errf(format string, args ...any) error {
	return &Error{Offset: p.pos, Msg: fmt.Sprintf(format, args...)}
}

func (p *parser) skipWS() {
	for p.pos < len(p.data) {
		switch p.data[p.pos] {
		case ' ', '\t', '\n', '\r':
			p.pos++
		default:
			return
		}
	}
}

func (p *parser) value(depth int) (any, error) {
	if p.pos >= len(p.data) {
		return nil, p.errf("unexpected end of input")
	}
	switch c := p.data[p.pos]; {
	case c == '{':
		return p.object(depth + 1)
	case c == '[':
		return p.array(depth + 1)
	case c == '"':
		return p.string()
	case c == 't':
		return true, p.literal("true")
	case c == 'f':
		return false, p.literal("false")
	case c == 'n':
		return nil, p.literal("null")
	case c == '-' || (c >= '0' && c <= '9'):
		return p.number()
	default:
		return nil, p.errf("unexpected character %q", c)
	}
}

func (p *parser) literal(s string) error {
	if !strings.HasPrefix(string(p.data[p.pos:min(len(p.data), p.pos+len(s))]), s) {
		return p.errf("invalid literal")
	}
	p.pos += len(s)
	return nil
}

func (p *parser) checkDepth(depth int) error {
	if p.lim.MaxDepth > 0 && depth > p.lim.MaxDepth {
		return &Error{Offset: p.pos, Msg: ErrDepth.Error()}
	}
	return nil
}

func (p *parser) object(depth int) (any, error) {
	if err := p.checkDepth(depth); err != nil {
		return nil, err
	}
	p.pos++ // {
	m := map[string]any{}
	p.skipWS()
	if p.pos < len(p.data) && p.data[p.pos] == '}' {
		p.pos++
		return m, nil
	}
	for {
		p.skipWS()
		if p.pos >= len(p.data) || p.data[p.pos] != '"' {
			return nil, p.errf("expected object key")
		}
		keyPos := p.pos
		k, err := p.string()
		if err != nil {
			return nil, err
		}
		if _, dup := m[k]; dup {
			return nil, &Error{Offset: keyPos, Msg: fmt.Sprintf("repeated key %q", k)}
		}
		p.skipWS()
		if p.pos >= len(p.data) || p.data[p.pos] != ':' {
			return nil, p.errf("expected ':'")
		}
		p.pos++
		p.skipWS()
		v, err := p.value(depth)
		if err != nil {
			return nil, err
		}
		m[k] = v
		p.skipWS()
		if p.pos >= len(p.data) {
			return nil, p.errf("unexpected end of object")
		}
		switch p.data[p.pos] {
		case ',':
			p.pos++
		case '}':
			p.pos++
			return m, nil
		default:
			return nil, p.errf("expected ',' or '}'")
		}
	}
}

func (p *parser) array(depth int) (any, error) {
	if err := p.checkDepth(depth); err != nil {
		return nil, err
	}
	p.pos++ // [
	a := []any{}
	p.skipWS()
	if p.pos < len(p.data) && p.data[p.pos] == ']' {
		p.pos++
		return a, nil
	}
	for {
		p.skipWS()
		v, err := p.value(depth)
		if err != nil {
			return nil, err
		}
		a = append(a, v)
		p.skipWS()
		if p.pos >= len(p.data) {
			return nil, p.errf("unexpected end of array")
		}
		switch p.data[p.pos] {
		case ',':
			p.pos++
		case ']':
			p.pos++
			return a, nil
		default:
			return nil, p.errf("expected ',' or ']'")
		}
	}
}

func (p *parser) string() (string, error) {
	p.pos++ // opening quote
	var sb strings.Builder
	for {
		if p.pos >= len(p.data) {
			return "", p.errf("unterminated string")
		}
		c := p.data[p.pos]
		switch {
		case c == '"':
			p.pos++
			return sb.String(), nil
		case c == '\\':
			if err := p.escape(&sb); err != nil {
				return "", err
			}
		case c < 0x20:
			return "", p.errf("control character in string")
		default:
			// The whole input is valid UTF-8, so copying bytes keeps it valid. A raw surrogate
			// cannot appear: UTF-8 cannot encode one.
			sb.WriteByte(c)
			p.pos++
		}
	}
}

func (p *parser) hex4() (rune, error) {
	if p.pos+4 > len(p.data) {
		return 0, p.errf("short unicode escape")
	}
	v, err := strconv.ParseUint(string(p.data[p.pos:p.pos+4]), 16, 16)
	if err != nil {
		return 0, p.errf("invalid unicode escape")
	}
	p.pos += 4
	return rune(v), nil
}

func (p *parser) escape(sb *strings.Builder) error {
	p.pos++ // backslash
	if p.pos >= len(p.data) {
		return p.errf("unterminated escape")
	}
	c := p.data[p.pos]
	p.pos++
	switch c {
	case '"', '\\', '/':
		sb.WriteByte(c)
	case 'b':
		sb.WriteByte('\b')
	case 'f':
		sb.WriteByte('\f')
	case 'n':
		sb.WriteByte('\n')
	case 'r':
		sb.WriteByte('\r')
	case 't':
		sb.WriteByte('\t')
	case 'u':
		r, err := p.hex4()
		if err != nil {
			return err
		}
		switch {
		case utf16.IsSurrogate(r) && r < 0xDC00:
			if p.pos+2 > len(p.data) || p.data[p.pos] != '\\' || p.data[p.pos+1] != 'u' {
				return p.errf("lone surrogate")
			}
			p.pos += 2
			r2, err := p.hex4()
			if err != nil {
				return err
			}
			if r2 < 0xDC00 || r2 > 0xDFFF {
				return p.errf("lone surrogate")
			}
			sb.WriteRune(utf16.DecodeRune(r, r2))
		case utf16.IsSurrogate(r):
			return p.errf("lone surrogate")
		case r == 0:
			return p.errf("U+0000 in a string")
		default:
			sb.WriteRune(r)
		}
	default:
		return p.errf("invalid escape")
	}
	return nil
}

func (p *parser) number() (any, error) {
	start := p.pos
	if p.data[p.pos] == '-' {
		p.pos++
	}
	digits := func() int {
		n := 0
		for p.pos < len(p.data) && p.data[p.pos] >= '0' && p.data[p.pos] <= '9' {
			p.pos++
			n++
		}
		return n
	}
	if p.pos < len(p.data) && p.data[p.pos] == '0' {
		p.pos++
	} else if digits() == 0 {
		return nil, p.errf("invalid number")
	}
	isInt := true
	if p.pos < len(p.data) && p.data[p.pos] == '.' {
		isInt = false
		p.pos++
		if digits() == 0 {
			return nil, p.errf("invalid number")
		}
	}
	if p.pos < len(p.data) && (p.data[p.pos] == 'e' || p.data[p.pos] == 'E') {
		isInt = false
		p.pos++
		if p.pos < len(p.data) && (p.data[p.pos] == '+' || p.data[p.pos] == '-') {
			p.pos++
		}
		if digits() == 0 {
			return nil, p.errf("invalid number")
		}
	}
	lit := string(p.data[start:p.pos])
	if isInt {
		n, err := strconv.ParseInt(lit, 10, 64)
		if err != nil || n > MaxSafeInteger || n < -MaxSafeInteger {
			return nil, &Error{Offset: start, Msg: "integer outside ±(2^53 - 1)"}
		}
	} else {
		f, err := strconv.ParseFloat(lit, 64)
		if err != nil || math.IsInf(f, 0) || math.Abs(f) > MaxSafeInteger {
			return nil, &Error{Offset: start, Msg: "number outside ±(2^53 - 1)"}
		}
	}
	return json.Number(lit), nil
}

// Normalize converts a value decoded by another decoder (encoding/json with UseNumber, or a
// YAML document) into the shapes that Parse returns: map[string]any, []any, string,
// json.Number, bool and nil. It fails on a value that JSON cannot carry.
func Normalize(v any) (any, error) {
	switch x := v.(type) {
	case nil, bool, string, json.Number:
		return x, nil
	case int:
		return json.Number(strconv.FormatInt(int64(x), 10)), nil
	case int64:
		return json.Number(strconv.FormatInt(x, 10)), nil
	case uint64:
		if x > MaxSafeInteger {
			return nil, fmt.Errorf("integer %d outside ±(2^53 - 1)", x)
		}
		return json.Number(strconv.FormatUint(x, 10)), nil
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return nil, fmt.Errorf("number %v is not JSON", x)
		}
		if x == math.Trunc(x) && math.Abs(x) <= MaxSafeInteger {
			return json.Number(strconv.FormatInt(int64(x), 10)), nil
		}
		return json.Number(strconv.FormatFloat(x, 'g', -1, 64)), nil
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			n, err := Normalize(e)
			if err != nil {
				return nil, err
			}
			out[k] = n
		}
		return out, nil
	case map[any]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			ks, ok := k.(string)
			if !ok {
				return nil, fmt.Errorf("object key %v is not a string", k)
			}
			n, err := Normalize(e)
			if err != nil {
				return nil, err
			}
			out[ks] = n
		}
		return out, nil
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			n, err := Normalize(e)
			if err != nil {
				return nil, err
			}
			out[i] = n
		}
		return out, nil
	default:
		// Fall back to a JSON round trip for typed values (structs, typed slices).
		b, err := json.Marshal(x)
		if err != nil {
			return nil, err
		}
		return Parse(b)
	}
}
