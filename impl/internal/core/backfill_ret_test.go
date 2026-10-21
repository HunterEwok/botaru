// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/HunterEwok/botaru/impl/internal/spec"
	"github.com/HunterEwok/botaru/impl/internal/testbench"
)

// RET-1: every stored object schema (x-botaru-direction: record) names exactly one class of
// spec/model/retention-classes.yaml in x-botaru-retention, and the default periods the core
// deletes by are those of the registry.
func TestBackfillRetentionClasses(t *testing.T) {
	t.Parallel()
	root, err := spec.FindRoot("")
	if err != nil {
		t.Fatal(err)
	}
	sp, err := testbench.SharedSpec(root)
	if err != nil {
		t.Fatal(err)
	}
	reg, err := sp.Registry("spec/model/retention-classes.yaml")
	if err != nil {
		t.Fatal(err)
	}
	classes := map[string]map[string]any{}
	for _, e := range reg.Entries {
		classes[asString(e["id"])] = asMap(e["default"])
	}
	files, err := filepath.Glob(filepath.Join(root, "spec/model/*.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var records int
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var doc map[string]any
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatal(err)
		}
		if doc["x-botaru-direction"] != "record" {
			continue
		}
		records++
		if _, ok := classes[asString(doc["x-botaru-retention"])]; !ok {
			t.Errorf("RET-1: %s names the retention class %q", filepath.Base(f), doc["x-botaru-retention"])
		}
	}
	if records == 0 {
		t.Fatal("RET-1: no stored object schema")
	}
	for _, tc := range []struct {
		class, field string
		core         int
	}{
		{"content", "days", contentDays},
		{"content", "sessions_without_memory_days", sessionsNoMemoryDays},
		{"metadata", "days", metadataDays},
	} {
		if got, _ := classes[tc.class][tc.field].(int); got != tc.core {
			t.Errorf("RET-1: the class %s %s is %v in the registry, %d in the core", tc.class, tc.field, classes[tc.class][tc.field], tc.core)
		}
	}
}
