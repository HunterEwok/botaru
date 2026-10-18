// SPDX-License-Identifier: AGPL-3.0-only

package gate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"
)

// errUnrenderable is a field the line editor does not write in place: a block scalar, a flow
// collection, an anchor or an alias, a value that is not a scalar, or a list index the file
// does not have. The gate then refuses with diff_unrenderable instead of rewriting the file.
var errUnrenderable = errors.New("the field cannot be written in place")

// setField writes v into the field path of the document number doc (0-based, every document of
// the stream counted) of a YAML file, changing only the bytes of the old value or inserting the
// missing keys as new lines, so that the diff is the field and nothing else. A path part may end
// in [] (the only element of a list) or [i].
func setField(src []byte, doc int, field string, v any) ([]byte, error) {
	root, err := documentNode(src, doc)
	if err != nil {
		return nil, err
	}
	lines := lineStarts(src)
	parts := strings.Split(field, ".")
	cur := root
	for i, part := range parts {
		name, idx, err := splitPart(part)
		if err != nil {
			return nil, err
		}
		if cur.Kind != yaml.MappingNode || cur.Style&yaml.FlowStyle != 0 || cur.Anchor != "" {
			return nil, fmt.Errorf("%s: %w", field, errUnrenderable)
		}
		val := mapValue(cur, name)
		if val == nil {
			if idx != noIndex {
				return nil, fmt.Errorf("%s: no list %s: %w", field, name, errUnrenderable)
			}
			return insertKeys(src, lines, cur, parts[i:], v)
		}
		if idx != noIndex {
			if val.Kind != yaml.SequenceNode || val.Style&yaml.FlowStyle != 0 {
				return nil, fmt.Errorf("%s: %w", field, errUnrenderable)
			}
			if idx == onlyElement {
				if len(val.Content) != 1 {
					return nil, fmt.Errorf("%s: %s has %d elements, not one: %w", field, name, len(val.Content), errUnrenderable)
				}
				idx = 0
			}
			if idx >= len(val.Content) {
				return nil, fmt.Errorf("%s: %w", field, errUnrenderable)
			}
			val = val.Content[idx]
		}
		if i < len(parts)-1 {
			cur = val
			continue
		}
		return replaceScalar(src, lines, val, v)
	}
	return nil, fmt.Errorf("%s: %w", field, errUnrenderable)
}

const (
	noIndex     = -1
	onlyElement = -2
)

func splitPart(part string) (string, int, error) {
	name, rest, ok := strings.Cut(part, "[")
	if !ok {
		return part, noIndex, nil
	}
	n, ok := strings.CutSuffix(rest, "]")
	if !ok || name == "" {
		return "", 0, fmt.Errorf("bad field part %q", part)
	}
	if n == "" {
		return name, onlyElement, nil
	}
	k, err := strconv.Atoi(n)
	if err != nil || k < 0 {
		return "", 0, fmt.Errorf("bad field part %q", part)
	}
	return name, k, nil
}

func documentNode(src []byte, doc int) (*yaml.Node, error) {
	dec := yaml.NewDecoder(bytes.NewReader(src))
	for i := 0; ; i++ {
		var n yaml.Node
		if err := dec.Decode(&n); err != nil {
			if errors.Is(err, io.EOF) {
				return nil, fmt.Errorf("no document %d", doc+1)
			}
			return nil, err
		}
		if i == doc {
			if n.Kind != yaml.DocumentNode || len(n.Content) != 1 {
				return nil, fmt.Errorf("document %d: %w", doc+1, errUnrenderable)
			}
			return n.Content[0], nil
		}
	}
}

func mapValue(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

func lineStarts(src []byte) []int {
	out := []int{0}
	for i, b := range src {
		if b == '\n' {
			out = append(out, i+1)
		}
	}
	return out
}

// offset is the byte offset of a node's line and column (both 1-based, the column in characters).
func offset(src []byte, lines []int, n *yaml.Node) (int, error) {
	if n.Line < 1 || n.Line > len(lines) {
		return 0, errUnrenderable
	}
	off := lines[n.Line-1]
	for c := 1; c < n.Column; c++ {
		if off >= len(src) || src[off] == '\n' {
			return 0, errUnrenderable
		}
		_, size := utf8.DecodeRune(src[off:])
		off += size
	}
	return off, nil
}

// replaceScalar swaps the bytes of a scalar on one line for the rendering of v in the same style.
func replaceScalar(src []byte, lines []int, n *yaml.Node, v any) ([]byte, error) {
	if n.Kind != yaml.ScalarNode || n.Anchor != "" || n.Tag == "!!binary" || n.Style&(yaml.LiteralStyle|yaml.FoldedStyle|yaml.TaggedStyle) != 0 {
		return nil, errUnrenderable
	}
	start, err := offset(src, lines, n)
	if err != nil {
		return nil, err
	}
	end := start
	lineEnd := bytes.IndexByte(src[start:], '\n')
	if lineEnd < 0 {
		lineEnd = len(src)
	} else {
		lineEnd += start
	}
	switch {
	case n.Style&yaml.DoubleQuotedStyle != 0:
		i := start + 1
		for ; i < lineEnd; i++ {
			if src[i] == '\\' {
				i++
				continue
			}
			if src[i] == '"' {
				break
			}
		}
		if i >= lineEnd {
			return nil, errUnrenderable
		}
		end = i + 1
	case n.Style&yaml.SingleQuotedStyle != 0:
		i := start + 1
		for ; i < lineEnd; i++ {
			if src[i] == '\'' {
				if i+1 < lineEnd && src[i+1] == '\'' {
					i++
					continue
				}
				break
			}
		}
		if i >= lineEnd {
			return nil, errUnrenderable
		}
		end = i + 1
	default:
		end = lineEnd
		if c := bytes.Index(src[start:lineEnd], []byte(" #")); c >= 0 {
			end = start + c
		}
		for end > start && (src[end-1] == ' ' || src[end-1] == '\t' || src[end-1] == '\r') {
			end--
		}
	}
	text, err := render(v, n.Style)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, len(src)+len(text))
	out = append(out, src[:start]...)
	out = append(out, text...)
	return append(out, src[end:]...), nil
}

// insertKeys adds the missing keys of a path to a block mapping as new lines before one of its
// keys that starts a line, with that key's indentation.
func insertKeys(src []byte, lines []int, m *yaml.Node, parts []string, v any) ([]byte, error) {
	var at, indent = -1, 0
	for i := 0; i+1 < len(m.Content); i += 2 {
		k := m.Content[i]
		off, err := offset(src, lines, k)
		if err != nil {
			return nil, err
		}
		ls := lines[k.Line-1]
		if strings.TrimLeft(string(src[ls:off]), " ") == "" {
			at, indent = ls, k.Column-1
			break
		}
	}
	if at < 0 {
		return nil, errUnrenderable
	}
	text, err := render(v, 0)
	if err != nil {
		return nil, err
	}
	var b strings.Builder
	for i, p := range parts {
		if strings.ContainsAny(p, "[]") || !plainKey(p) {
			return nil, errUnrenderable
		}
		b.WriteString(strings.Repeat(" ", indent+2*i))
		b.WriteString(p)
		b.WriteString(":")
		if i == len(parts)-1 {
			b.WriteString(" ")
			b.WriteString(text)
		}
		b.WriteString("\n")
	}
	out := make([]byte, 0, len(src)+b.Len())
	out = append(out, src[:at]...)
	out = append(out, b.String()...)
	return append(out, src[at:]...), nil
}

func plainKey(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

// render writes a scalar value on one line; a string keeps the quoting style of the old value.
func render(v any, style yaml.Style) (string, error) {
	switch x := v.(type) {
	case nil:
		return "null", nil
	case bool:
		return strconv.FormatBool(x), nil
	case json.Number:
		return x.String(), nil
	case int:
		return strconv.Itoa(x), nil
	case int64:
		return strconv.FormatInt(x, 10), nil
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return "", errUnrenderable
		}
		if x == math.Trunc(x) && math.Abs(x) < 1<<53 {
			return strconv.FormatInt(int64(x), 10), nil
		}
		return strconv.FormatFloat(x, 'g', -1, 64), nil
	case string:
		if strings.ContainsAny(x, "\n\r") {
			return "", errUnrenderable
		}
		switch {
		case style&yaml.DoubleQuotedStyle != 0:
			var b bytes.Buffer
			enc := json.NewEncoder(&b)
			enc.SetEscapeHTML(false)
			if err := enc.Encode(x); err != nil {
				return "", err
			}
			return strings.TrimSuffix(b.String(), "\n"), nil
		case style&yaml.SingleQuotedStyle != 0:
			return "'" + strings.ReplaceAll(x, "'", "''") + "'", nil
		}
		out, err := yaml.Marshal(x)
		if err != nil {
			return "", err
		}
		s := strings.TrimSuffix(string(out), "\n")
		if strings.Contains(s, "\n") {
			return "", errUnrenderable
		}
		return s, nil
	}
	return "", errUnrenderable
}
