// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/HunterEwok/botaru/impl/internal/ids"
	"github.com/HunterEwok/botaru/impl/internal/manifest"
	"github.com/HunterEwok/botaru/impl/internal/spec"
	"github.com/HunterEwok/botaru/impl/internal/store"
	"github.com/HunterEwok/botaru/impl/internal/testbench"
)

// Tests of the quality gate on a static fingerprint change (PRV-3).

// newProductionCore is a core outside the test bench, where a tenant may be of kind production.
func newProductionCore(t *testing.T) *Core {
	t.Helper()
	ctx := context.Background()
	root, err := spec.FindRoot("")
	if err != nil {
		t.Fatal(err)
	}
	sp, err := testbench.SharedSpec(root)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := testbench.SharedTexts(root)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, testbench.MigratedDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	c, err := New(ctx, Config{TokenKey: []byte("0123456789abcdef0123456789abcdef"), Offline: true, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}, st, sp, tx)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// apply of a production tenant does not enable a tier 1 role without an eval report with zero
// security failures for its static fingerprint; a test tenant gets a warning; a role whose
// fingerprint did not change stays enabled; doctor warns eval_report_stale (PRV-3).
func TestEvalReportGate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := newProductionCore(t)
	op := Operator{Principal: ids.PrincipalID(), Tenant: "farm"}
	ms, at := helloSet(t)
	ms[at["Tenant"]]["spec"].(map[string]any)["kind"] = "production"
	ms[at["TaskType"]]["spec"].(map[string]any)["tier"] = 1
	reports := func(fp string, sec int) map[string]any {
		return map[string]any{"apiVersion": "botaru/v1alpha1", "kind": "EvalReports", "metadata": map[string]any{"name": "index"},
			"spec": map[string]any{"reports": []any{map[string]any{"subject": map[string]any{"kind": "role", "name": "hello"}, "fingerprint": fp,
				"suite": "hello", "date": "2026-10-01", "cases": 10, "failures": 0, "security_failures": sec}}}}
	}
	apply := func(set []map[string]any, dry bool) *ApplyResult {
		t.Helper()
		res, err := c.Apply(ctx, op, ApplyRequest{Commit: strings.Repeat("9", 40), Manifests: set, DryRun: dry})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	if res := apply(ms, true); !hasCode(res, "eval_report_missing") {
		t.Fatalf("no report: %+v", res.Errors)
	}
	var docs []manifest.Doc
	for i, m := range ms {
		docs = append(docs, manifest.FromValue(m, fmt.Sprintf("manifests/%d", i)))
	}
	fp, ok, err := newEvalIndex(docs).staticFingerprint("hello", "demo")
	if err != nil || !ok {
		t.Fatal(err)
	}
	if res := apply(append(slices.Clone(ms), reports(fp, 1)), true); !hasCode(res, "eval_report_missing") {
		t.Fatalf("a report with a security failure counted: %+v", res.Errors)
	}
	if res := apply(append(slices.Clone(ms), reports(fp, 0)), false); !res.Applied {
		t.Fatalf("with a report: %+v", res.Errors)
	}
	// The role did not change: an index without its entry does not take it back.
	other := fp[:len(fp)-1] + "0"
	if strings.HasSuffix(fp, "0") {
		other = fp[:len(fp)-1] + "1"
	}
	if res := apply(append(slices.Clone(ms), reports(other, 0)), false); !res.Applied {
		t.Fatalf("an unchanged role: %+v", res.Errors)
	}
	d, err := c.Doctor(ctx, Operator{Principal: op.Principal, Tenant: "demo"}, TenantRequest{Tenant: "demo"})
	if err != nil {
		t.Fatal(err)
	}
	stale := false
	for _, ch := range d.Checks {
		if ch.ID == "eval_report_stale" && !ch.OK && slices.Equal(ch.Value.([]string), []string{"hello"}) {
			stale = true
		}
	}
	if !stale {
		t.Fatalf("doctor %+v", d.Checks)
	}
	// A change of a part gives a new fingerprint without a report.
	ms[at["TaskType"]]["spec"].(map[string]any)["instruction"] = "Greet the person in one short line."
	if res := apply(append(slices.Clone(ms), reports(fp, 0)), true); !hasCode(res, "eval_report_missing") {
		t.Fatalf("a changed type: %+v", res.Errors)
	}
	// In a test tenant the same apply passes with a warning.
	ms[at["Tenant"]]["spec"].(map[string]any)["kind"] = "test"
	if res := apply(append(slices.Clone(ms), reports(fp, 0)), true); len(res.Errors) != 0 || len(res.Warnings) != 1 || res.Warnings[0].Code != "eval_report_missing" {
		t.Fatalf("a test tenant: %+v", res)
	}
	// A production tenant after a test tenant still refuses the role.
	demo2 := map[string]any{"apiVersion": "botaru/v1alpha1", "kind": "Tenant", "metadata": map[string]any{"name": "demo2"},
		"spec": map[string]any{"kind": "production", "farm_manager": map[string]any{"enabled": false}, "budgets": map[string]any{"month_eur": 60}}}
	if res := apply(append(slices.Clone(ms), reports(fp, 0), demo2), true); !hasCode(res, "eval_report_missing") {
		t.Fatalf("a production tenant after a test tenant: %+v", res)
	}
}
