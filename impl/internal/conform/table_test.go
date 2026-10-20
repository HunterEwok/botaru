// SPDX-License-Identifier: AGPL-3.0-only

package conform

import (
	"context"
	"testing"

	"github.com/HunterEwok/botaru/impl/internal/spec"
	"github.com/HunterEwok/botaru/impl/internal/testbench"
)

// TestFunctionTables runs the decision tables of pure core functions through function.eval at a
// claim of v0.3, ahead of the claim of the reference core (conformance/runner.md, "Decision
// table"): the rows of fx-rep-no-numbers check reporter.number_check (REP-4); a row that also
// expects facts of the delivered digest stays unchecked, and com-05 is unchecked because this
// build has no pure commission.accuracy (COM-17).
func TestFunctionTables(t *testing.T) {
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
	claim := Claim{Version: "v0.3", Profiles: []string{"core", "insights", "messaging"}}
	r, err := NewRunner(root, claim, &inProcess{t: t, root: root, sp: sp, tx: tx})
	if err != nil {
		t.Fatal(err)
	}
	all, err := LoadCases(root)
	if err != nil {
		t.Fatal(err)
	}
	var cases []*Case
	for _, c := range all {
		if c.Fixture == "fx-rep-no-numbers" || c.Fixture == "com-05" {
			cases = append(cases, c)
		}
	}
	want := map[string]Status{
		"fx-rep-nn-a": Passed, "fx-rep-nn-b": Unchecked, "fx-rep-nn-c": Passed, "fx-rep-nn-d": Passed, "fx-rep-nn-e": Passed,
		"fx-rep-nn-f": Passed, "fx-rep-nn-g": Passed, "fx-rep-nn-h": Unchecked, "fx-rep-nn-i": Unchecked,
		"com-05-a": Unchecked, "com-05-g": Unchecked,
	}
	got := map[string]Result{}
	for _, res := range r.Run(context.Background(), cases) {
		got[res.Case.ID] = res
	}
	for id, st := range want {
		if res, ok := got[id]; !ok || res.Status != st {
			t.Errorf("%s: %s %s, want %s", id, res.Status, res.Detail, st)
		}
	}
}
