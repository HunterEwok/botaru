// SPDX-License-Identifier: AGPL-3.0-only

package agent

import "testing"

func TestParseResult(t *testing.T) {
	schema := map[string]any{"type": "object", "required": []any{"answer"}, "properties": map[string]any{"answer": map[string]any{"type": "string"}}}
	for in, want := range map[string]string{
		`{"answer":"hi"}`:                  "hi",
		"```json\n{\"answer\":\"x\"}\n```": "x",
		"Hello there!":                     "Hello there!",
	} {
		r, err := parseResult(in, schema)
		if err != nil || r["answer"] != want {
			t.Errorf("%q: %v %v", in, r, err)
		}
	}
	if _, err := parseResult("plain", map[string]any{"required": []any{"a", "b"}}); err == nil {
		t.Error("plain text passed a two-field result")
	}
}
