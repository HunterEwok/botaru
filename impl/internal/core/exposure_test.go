// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"testing"

	"github.com/HunterEwok/botaru/impl/internal/mcp"
)

// Tests of the model exposure of datasets at call time (PRV-4).

// At call time an agent role does not read a dataset with model_exposure none, and reads one
// with aggregates only through a tool of numbers and enum values (PRV-4). The lint refuses the
// none case at apply, so the catalog in memory is changed after it to reach the call check.
func TestModelExposureCall(t *testing.T) {
	ctx := context.Background()
	b := newToolBenchWith(t, nil, func(ms []map[string]any) []map[string]any {
		for _, m := range ms {
			if m["kind"] == "Catalog" {
				for _, r := range m["spec"].(map[string]any)["resources"].([]any) {
					if rm := r.(map[string]any); rm["id"] == "web-checks" {
						rm["kind"] = "dataset" // aggregates by default
					}
				}
			}
		}
		return ms
	})
	_, h := b.attemptTools(t, "1", false)
	call := func() string {
		r, err := b.c.P3ToolsCall(ctx, h, mcp.CallParams{Name: "checks_latest", Arguments: map[string]any{"check": "www-health"}})
		if err != nil {
			t.Fatal(err)
		}
		return callText(r)
	}
	// Without a toolhost an allowed call ends with plugin_unavailable, after the checks.
	if got := call(); got != "error: plugin_unavailable" {
		t.Fatalf("aggregates through a trusted tool: %q", got)
	}
	cat, err := b.c.Catalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	spec, _ := tenantCatalog(cat, "demo")
	for _, r := range spec["resources"].([]any) {
		if rm := r.(map[string]any); rm["id"] == "web-checks" {
			rm["model_exposure"] = "none"
		}
	}
	if got := call(); got != noPermission {
		t.Fatalf("none: %q", got)
	}
}
