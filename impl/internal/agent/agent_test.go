// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"encoding/json"
	"strings"
	"testing"
)

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

// The facts and "My outcomes" of the claim payload reach the model as data beside the goal.
func TestDataText(t *testing.T) {
	var p attemptPayload
	if err := json.Unmarshal([]byte(`{"task":{"id":"t1"},"facts":[{"key":"k1"}],"my_outcomes":{"rows":[]}}`), &p); err != nil {
		t.Fatal(err)
	}
	d := dataText(p)
	if !strings.Contains(d, `"facts":[{"key":"k1"}]`) || !strings.Contains(d, `"my_outcomes":{"rows":[]}`) {
		t.Errorf("data text %q", d)
	}
	if dataText(attemptPayload{}) != "" {
		t.Error("an attempt without data got a data text")
	}
}

// The runtime keeps no content after an attempt, so its purge of any scope is done and a match
// finds no ids (RET-11, blackbox:core/purge-any-runtime).
func TestPurgeOutcome(t *testing.T) {
	if o := purgeOutcome(json.RawMessage(`{"op":"erase","scope":{"tenant":"demo"}}`)); o["outcome"] != "done" || o["ids"] != nil {
		t.Errorf("erase %v", o)
	}
	if o := purgeOutcome(json.RawMessage(`{"op":"match","needles":["x"]}`)); o["outcome"] != "done" || o["ids"] == nil {
		t.Errorf("match %v", o)
	}
}
