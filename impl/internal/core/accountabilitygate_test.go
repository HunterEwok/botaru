// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"slices"
	"strings"
	"testing"

	"github.com/HunterEwok/botaru/impl/internal/manifest"
)

// Tests of the eval gate of the commission persona (GOV-46).

// The eval gate of the commission persona (GOV-46): on a v0.3 core a reviewer role whose static
// fingerprint changed needs, besides its PRV-3 report, a gov-res-aware report without failures
// for that fingerprint before a production tenant enables it; other roles and a core before v0.3
// do not ask for it.
func TestCommissionUnawareGate(t *testing.T) {
	doc := func(kind, tenant, name string, spec map[string]any) manifest.Doc {
		return manifest.Doc{Kind: kind, Tenant: tenant, Name: name, Value: map[string]any{"apiVersion": "botaru/v1alpha1", "kind": kind,
			"metadata": map[string]any{"name": name}, "spec": spec}}
	}
	base := []manifest.Doc{
		doc("Role", "", "rev", map[string]any{"profile": "reviewer", "claims": []any{"review.security.v1"}}),
		doc("TaskType", "", "review.security.v1", map[string]any{"tier": 1}),
		doc("Tenant", "", "demo", map[string]any{"kind": "production"}),
		doc("Governance", "demo", "charter", map[string]any{"positions": map[string]any{"commission": map[string]any{"persona": map[string]any{"ref": "p.md", "sha256": strings.Repeat("b", 64)}}}}),
	}
	fp, _, err := newEvalIndex(base).staticFingerprint("rev", "demo")
	if err != nil {
		t.Fatal(err)
	}
	reports := func(suites ...string) manifest.Doc {
		var list []any
		for _, s := range suites {
			list = append(list, map[string]any{"fingerprint": fp, "subject": map[string]any{"kind": "role", "name": "rev"}, "suite": s, "date": "2026-10-01",
				"cases": 40, "failures": 0, "security_failures": 0})
		}
		return doc("EvalReports", "", "evals", map[string]any{"reports": list})
	}
	gate := func(docs []manifest.Doc) []ApplyError {
		index := make([]int, len(docs))
		errs, _, err := (&Core{}).evalGate(docs, index, nil)
		if err != nil {
			t.Fatal(err)
		}
		return errs
	}
	if errs := gate(append(slices.Clone(base), reports("com"))); len(errs) != 1 || errs[0].Code != "eval_report_missing" {
		t.Fatalf("a commission role without gov-res-aware: %+v", errs)
	}
	if errs := gate(append(slices.Clone(base), reports("com", suiteUnawareness))); len(errs) != 0 {
		t.Fatalf("with gov-res-aware: %+v", errs)
	}
}
