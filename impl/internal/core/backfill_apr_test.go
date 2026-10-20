// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"strings"
	"testing"
)

// backfillAction is an Action manifest with the given risk fields over a write tool of the
// plugin fixer.
func backfillAction(name string, fields map[string]any) map[string]any {
	s := map[string]any{"title": "Clean the site", "description": "Removes old temporary files of the site.",
		"args": map[string]any{"type": "object", "additionalProperties": false, "required": []any{"host"},
			"properties": map[string]any{"host": map[string]any{"enum_from": "catalog.hosts"}}}, "ttl": "15m",
		"executor": map[string]any{"plugin": "fixer", "tool": "clean", "os_mechanism": map[string]any{
			"oneshot": map[string]any{"unit": "site-clean-{tenant}.service", "started_by": "site-clean-{tenant}.path", "wait": false}}}}
	for k, v := range fields {
		if v == nil {
			delete(s, k)
			continue
		}
		s[k] = v
	}
	return map[string]any{"apiVersion": "botaru/v1alpha1", "kind": "Action", "metadata": map[string]any{"name": name}, "spec": s}
}

// Every action declares its risk: an action without risk fails apply; routine is a pipeline
// that writes only the tenant's catalogs, is idempotent and has run limits, and apply refuses it
// without these fields or with two approvals; the routine fields belong to routine only
// (APR-10).
func TestBackfillActionRisk(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newBench(t)
	routine := map[string]any{"risk": "routine", "idempotent": true, "writes_only": []any{"res:web1:tmp"}, "runs_per_hour": 2, "runs_per_day": 10}
	with := func(extra map[string]any) map[string]any {
		out := map[string]any{}
		for k, v := range routine {
			out[k] = v
		}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}
	for _, tc := range []struct {
		name   string
		fields map[string]any
		refuse bool
	}{
		{"low", map[string]any{"risk": "low"}, false},
		{"no-risk", map[string]any{"risk": nil}, true},
		{"routine", routine, false},
		{"routine-not-idempotent", with(map[string]any{"idempotent": nil}), true},
		{"routine-no-writes-only", with(map[string]any{"writes_only": nil}), true},
		{"routine-no-runs-per-hour", with(map[string]any{"runs_per_hour": nil}), true},
		{"routine-no-runs-per-day", with(map[string]any{"runs_per_day": nil}), true},
		{"routine-two-approvals", with(map[string]any{"approvals": 2}), true},
		{"low-with-run-limits", map[string]any{"risk": "low", "runs_per_day": 10}, true},
	} {
		set := []map[string]any{backfillAction("clean_site", tc.fields), {
			"apiVersion": "botaru/v1alpha1", "kind": "ToolPlugin", "metadata": map[string]any{"name": "fixer"},
			"spec": map[string]any{"placement": "host", "network": map[string]any{"egress": []any{}},
				"tools": []any{map[string]any{"name": "clean", "access": "write", "tier": 3, "output_class": "internal", "untrusted_output": false,
					"args_from": "action:clean_site", "target_from": "host", "output_limit_kb": 4, "timeout_s": 20}}},
		}}
		res, err := b.c.Apply(ctx, Operator{Principal: b.op.Principal, Tenant: "farm"}, ApplyRequest{Commit: strings.Repeat("4", 40), Manifests: set, DryRun: true})
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		var found []string
		for _, e := range res.Errors {
			// The action is the first manifest of the set: a refusal of it, by the schema or the
			// lint, names its path.
			if strings.HasPrefix(e.Path, "/manifests/0") || e.Code == "routine_fields" {
				found = append(found, e.Code+" "+e.Path)
			}
		}
		// APR-10: the risk class and the routine fields decide whether the set passes.
		if refused := len(found) > 0; refused != tc.refuse {
			t.Errorf("%s: refused %v (%v), want %v; all errors %+v", tc.name, refused, found, tc.refuse, res.Errors)
		}
	}
}
