// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/HunterEwok/botaru/impl/internal/jcs"
	"github.com/HunterEwok/botaru/impl/internal/manifest"
)

const trnSet = `apiVersion: botaru/v1alpha1
kind: Tenant
metadata: {name: t1}
spec: {kind: %s, timezone: UTC}
---
apiVersion: botaru/v1alpha1
kind: TaskType
metadata: {name: fx.translate.v1}
spec: {title: "Translation", worker_kind: agent, purpose: translate, tier: 0, data_class: summary,
       params_schema: {type: object, additionalProperties: false, properties: {}}, proposable: false, tree_budget_eur: 0.2}
---
apiVersion: botaru/v1alpha1
kind: Role
metadata: {name: fx-trn}
spec: {profile: translator, worker_kind: agent, claims: [fx.translate.v1], models: {base: translate-std}}
`

// A translator role of a production tenant passes apply only with both translator sets in the
// EvalReports index for its static fingerprint; a test tenant needs none (TRN-6).
func TestTranslatorEvalGate(t *testing.T) {
	c := lintCore(t)
	base := fmt.Sprintf(trnSet, "production")
	ds, err := manifest.ParseYAML(strings.NewReader(base), "test")
	if err != nil {
		t.Fatal(err)
	}
	hash := func(kind string) string {
		for _, d := range ds {
			if d.Kind == kind {
				h, err := jcs.Hash(d.Value)
				if err != nil {
					t.Fatal(err)
				}
				return h
			}
		}
		return ""
	}
	fp, err := staticFingerprint(hash("Role"), map[string]string{"fx.translate.v1": hash("TaskType")}, "", "translate-std", "", "")
	if err != nil {
		t.Fatal(err)
	}
	reports := func(match float64, cases, sec int) string {
		return fmt.Sprintf(`---
apiVersion: botaru/v1alpha1
kind: EvalReports
metadata: {name: evals}
spec:
  reports:
    - {fingerprint: "%s", subject: {kind: role, name: fx-trn}, suite: trn, date: 2026-10-01, cases: %d, failures: 0, security_failures: 0, structural_match: %v}
    - {fingerprint: "%s", subject: {kind: role, name: fx-trn}, suite: trn-inj, date: 2026-10-01, cases: 6, failures: 0, security_failures: %d}
`, fp, cases, match, fp, sec)
	}
	for _, tc := range []struct {
		name, docs string
		missing    bool
	}{
		{"no report", base, true},
		{"both sets pass", base + reports(0.95, 30, 0), false},
		{"match below threshold", base + reports(0.8, 30, 0), true},
		{"too few cases", base + reports(0.95, 29, 0), true},
		{"injection security failure", base + reports(0.95, 30, 1), true},
		{"test tenant", fmt.Sprintf(trnSet, "test"), false},
	} {
		if got := slices.Contains(lintCodes(t, c, tc.docs), "eval_report_missing"); got != tc.missing {
			t.Errorf("%s: eval_report_missing %v, want %v", tc.name, got, tc.missing)
		}
	}
}
