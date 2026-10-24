// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"testing"

	"github.com/HunterEwok/botaru/impl/internal/ids"
	"github.com/HunterEwok/botaru/impl/internal/spec"
	"github.com/HunterEwok/botaru/impl/internal/store"
	"github.com/HunterEwok/botaru/impl/internal/testbench"
)

// The example set of examples/ applies with no error but two known kinds, so the lint fixtures,
// which apply their cases over it, never hide a new error of the examples themselves.
// The two: a role without the eval report an installation writes by running its evals, and a
// reference to a sandbox tool, whose profile this core does not claim.
func TestExamplesApplyClean(t *testing.T) {
	t.Parallel()
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
	c, err := New(ctx, Config{TokenKey: []byte("0123456789abcdef0123456789abcdef"), Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}, st, sp, tx)
	if err != nil {
		t.Fatal(err)
	}
	personas, err := testbench.SharedPersonas(root)
	if err != nil {
		t.Fatal(err)
	}
	exs := exampleManifests(t, root, sp)
	res, err := c.Apply(ctx, Operator{Principal: ids.PrincipalID(), Tenant: "farm"}, ApplyRequest{Commit: strings.Repeat("0", 40),
		Manifests: copies(exs), DryRun: true, Personas: personas})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range res.Errors {
		if e.Code == "eval_report_missing" && e.Kind == "Role" || e.Code == "ref_missing" && sandboxRef(exs, e.Path) {
			continue
		}
		t.Errorf("%+v", e)
	}
}

// sandboxRef says whether a JSON pointer of the apply request names a sandbox/ tool reference.
func sandboxRef(ms []map[string]any, ptr string) bool {
	var v any = map[string]any{"manifests": anySlice(ms)}
	for _, tok := range strings.Split(strings.TrimPrefix(ptr, "/"), "/") {
		switch x := v.(type) {
		case map[string]any:
			v = x[tok]
		case []any:
			i, err := strconv.Atoi(tok)
			if err != nil || i < 0 || i >= len(x) {
				return false
			}
			v = x[i]
		default:
			return false
		}
	}
	s, _ := v.(string)
	return strings.HasPrefix(s, "sandbox/")
}

func anySlice(ms []map[string]any) []any {
	out := make([]any, len(ms))
	for i, m := range ms {
		out[i] = m
	}
	return out
}

// copies deep-copies the manifests, so apply cannot change what the paths are read against.
func copies(ms []map[string]any) []map[string]any {
	out := make([]map[string]any, len(ms))
	for i, m := range ms {
		out[i] = testbench.DeepCopy(m).(map[string]any)
	}
	return out
}
