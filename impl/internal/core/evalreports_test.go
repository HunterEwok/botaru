// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
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
// EvalReports index for its static fingerprint; a test tenant needs none. A stored role is
// checked again on every apply by its current fingerprint (TRN-6).
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

	// The role and its type are stored; a new production tenant, or a changed stored type, takes
	// the reports away, and the finding goes on the object of the apply.
	stored := func(docs string, submitted ...string) []ApplyError {
		ds, err := manifest.ParseYAML(strings.NewReader(docs), "test")
		if err != nil {
			t.Fatal(err)
		}
		index := make([]int, len(ds))
		for i := range ds {
			index[i] = -1
			if slices.Contains(submitted, ds[i].Kind) {
				index[i] = i
			}
			if ds[i].Kind != "Tenant" {
				ds[i].Tenant = ""
			}
		}
		errs, err := c.lintManifests(ds, index, nil)
		if err != nil {
			t.Fatal(err)
		}
		return slices.DeleteFunc(errs, func(e ApplyError) bool { return e.Code != "eval_report_missing" })
	}
	if errs := stored(base, "Tenant"); len(errs) != 1 || errs[0].Kind != "Tenant" {
		t.Errorf("new production tenant over a stored role: %+v", errs)
	}
	if errs := stored(base+reports(0.95, 30, 0), "Tenant"); len(errs) != 0 {
		t.Errorf("stored role with reports: %+v", errs)
	}
	changed := strings.Replace(base, `title: "Translation"`, `title: "Translation 2"`, 1)
	if errs := stored(changed+reports(0.95, 30, 0), "TaskType"); len(errs) != 1 || errs[0].Kind != "TaskType" {
		t.Errorf("changed claimed type: %+v", errs)
	}
}

// setStoredDoc changes a stored manifest in place and moves the set hash, so the next catalog
// read sees it; the test bench's apply only takes kind: test tenants (APR-9).
func (b *bench) setStoredDoc(t *testing.T, kind, tenant, name, doc string) {
	t.Helper()
	ctx := context.Background()
	if _, err := b.c.st.Pool.Exec(ctx, `INSERT INTO manifests (kind, tenant, name, doc, hash, commit, applied_at) VALUES ($1, $2, $3, $4, 'x', 'x', now())
		ON CONFLICT (kind, tenant, name) DO UPDATE SET doc = EXCLUDED.doc`, kind, tenant, name, doc); err != nil {
		t.Fatal(err)
	}
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE apply_status SET set_hash = set_hash || 'x'`); err != nil {
		t.Fatal(err)
	}
}

// A seat task of a production tenant waits with seat_eval_missing while the EvalReports index
// has no mgr-inj entry without security failures for the seat model profile and its current
// fingerprint (model, parameters and manager persona), and the farm
// operators get seat_eval_missing_notice once; with the entry the task is handed out (MGR-23).
func TestSeatEvalGate(t *testing.T) {
	ctx := context.Background()
	b := newManagerBench(t)
	w := b.enroll(t, EnrollRequest{Tenant: "demo", Kind: "agent", Role: "farm-manager"})
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE manifests SET doc = jsonb_set(doc, '{spec,kind}', '"production"') WHERE kind = 'Tenant' AND name = 'demo'`); err != nil {
		t.Fatal(err)
	}
	b.setStoredDoc(t, "EvalReports", "", "evals", `{"apiVersion": "botaru/v1alpha1", "kind": "EvalReports", "metadata": {"name": "evals"},
		"spec": {"reports": [{"subject": {"kind": "seat", "name": "manager-std"}, "suite": "mgr-inj", "date": "2026-10-01", "cases": 12, "failures": 1, "security_failures": 1}]}}`)
	person := b.farmOperator(t)
	b.diskAlarm(t, true, 3)
	b.advance(t, 6*60)
	b.loop(t, "incident")
	if _, err := b.c.SeatAssign(ctx, b.farmOp(), SeatRequest{Seat: managerSeatID}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if item := b.claim(t, w, "attempt"); item != nil {
			t.Fatal("a seat task was handed out without the seat eval")
		}
	}
	var reason string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT coalesce(reason, '') FROM tasks WHERE tenant = 'demo' AND type = $1`, incidentRespondType).Scan(&reason); err != nil || reason != "seat_eval_missing" {
		t.Fatalf("reason %q %v", reason, err)
	}
	var notices int
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM deliveries WHERE kind = 'seat_eval_missing_notice' AND recipient = $1`, person).Scan(&notices); err != nil || notices != 1 {
		t.Fatalf("notices %d %v", notices, err)
	}
	cat, err := b.c.Catalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	fp, err := seatFingerprint(cat, "manager-std")
	if err != nil || fp == "" {
		t.Fatalf("fingerprint %q %v", fp, err)
	}
	seatReport := func(fp string) string {
		return fmt.Sprintf(`{"apiVersion": "botaru/v1alpha1", "kind": "EvalReports", "metadata": {"name": "evals"},
		"spec": {"reports": [{"fingerprint": "%s", "subject": {"kind": "seat", "name": "manager-std"}, "suite": "mgr-inj", "date": "2026-10-02", "cases": 12, "failures": 0, "security_failures": 0}]}}`, fp)
	}
	// A clean report of another fingerprint (an earlier model or persona) does not count.
	b.setStoredDoc(t, "EvalReports", "", "evals", seatReport("sha256:"+strings.Repeat("0", 64)))
	if item := b.claim(t, w, "attempt"); item != nil {
		t.Fatal("a seat task was handed out with the report of another fingerprint")
	}
	b.setStoredDoc(t, "EvalReports", "", "evals", seatReport(fp))
	if item := b.claim(t, w, "attempt"); item == nil {
		t.Fatal("the seat task waits with a passing seat eval")
	}
}
