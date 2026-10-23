// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/HunterEwok/botaru/impl/internal/manifest"
)

// A type checked by the core's own result schema hands the runtime that schema with every
// $ref inlined, so that a real model can read it as text; any other type gets none.
func TestCoreResultSchemaInlined(t *testing.T) {
	t.Parallel()
	b := newBench(t)
	for _, tc := range []struct {
		purpose string
		record  map[string]any
		valid   map[string]any
	}{
		{"insight", nil, map[string]any{"code": "nothing_found"}},
		{"aggregate", nil, map[string]any{"code": "nothing_found"}},
		{"review", map[string]any{"lens": "security", "review_schema": "v2"}, nil},
		{"review", map[string]any{"lens": "security"}, nil},
		{"review", map[string]any{"lens": necessityLens}, nil},
		{"inspect", nil, map[string]any{"findings": []any{map[string]any{"kind": "disk_growth", "subject": map[string]any{"kind": "resource", "id": "db1"},
			"evidence": []any{}, "values": map[string]any{"free_pct": 3}, "confidence": "medium"}}}},
	} {
		s := b.c.coreResultSchema(manifest.TaskType{Purpose: tc.purpose}, tc.record)
		raw, _ := json.Marshal(s)
		if s == nil || strings.Contains(string(raw), `"$ref"`) || strings.Contains(string(raw), `"$defs"`) {
			t.Fatalf("%s %v: schema %s", tc.purpose, tc.record, raw)
		}
		if tc.valid != nil {
			if err := b.c.rs.validate(s, tc.valid); err != nil {
				t.Errorf("%s: %v does not pass the inlined schema: %v", tc.purpose, tc.valid, err)
			}
			if err := b.c.rs.validate(s, map[string]any{"answer": "prose"}); err == nil {
				t.Errorf("%s: a prose answer passed the inlined schema", tc.purpose)
			}
		}
	}
	// The proposal body reaches the model with its fields, not a reference.
	raw, _ := json.Marshal(b.c.coreResultSchema(manifest.TaskType{Purpose: "insight"}, nil))
	if !strings.Contains(string(raw), `"proposals"`) || !strings.Contains(string(raw), `"properties"`) {
		t.Errorf("insight schema %s", raw)
	}
	// A reading is told the closed list of the numbers a finding may name.
	insp := b.c.coreResultSchema(manifest.TaskType{Purpose: "inspect"}, nil)
	if err := b.c.rs.validate(insp, map[string]any{"findings": []any{map[string]any{"kind": "none", "subject": map[string]any{"kind": "resource", "id": "db1"},
		"evidence": []any{}, "values": map[string]any{"not_a_value": 1}, "confidence": "low"}}}); err == nil {
		t.Error("a finding with a number outside the closed list passed the inspect schema")
	}
	if b.c.coreResultSchema(manifest.TaskType{Purpose: "work"}, nil) != nil {
		t.Error("a work type got a core result schema")
	}
}
