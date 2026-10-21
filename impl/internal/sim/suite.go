// SPDX-License-Identifier: AGPL-3.0-only

package sim

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/HunterEwok/botaru/impl/internal/evals"
	"github.com/HunterEwok/botaru/impl/internal/gate"
	"github.com/HunterEwok/botaru/impl/internal/manifest"
)

// The eval suite of the stand's merge gate (EVO-2, EVO-7, EVO-8). A run of botaru-eval stands up
// a whole bench of its own per side and per case set, four in one gate run, which is more than a
// scenario can wait for; and the farm git of the stand is not a skeleton root botaru-eval reads.
// So the stand checks its cases in this process: each case names a fact of the scaffold a role
// relies on (a field of a manifest with a bound, or a list that must hold a value), read from
// the farm folder of the side's worktree. The visible cases live in the farm git under
// evals/cases/<role>/, where a scaffold change may not touch them (gate.ForbiddenPrefixes); the
// hidden ones in <work>/holdout/<role>/, outside the farm git, where no candidate reads them.
// Only the outcome of a case leaves the suite, never its text.

// suiteCase is one case of the stand's suite.
type suiteCase struct {
	ID    string `yaml:"id"`
	Role  string `yaml:"role"`
	Title string `yaml:"title"`
	Check struct {
		Kind   string `yaml:"kind"`
		Tenant string `yaml:"tenant"`
		Name   string `yaml:"name"`
		Field  string `yaml:"field"`
		// AtLeast and AtMost bound the number the field holds (a duration in seconds);
		// Contains is a value its list must hold.
		AtLeast  *float64 `yaml:"at_least,omitempty"`
		AtMost   *float64 `yaml:"at_most,omitempty"`
		Contains string   `yaml:"contains,omitempty"`
	} `yaml:"check"`
}

func atLeast(f float64) *float64 { return &f }

// within is a case on a duration field between two bounds in seconds.
func within(c suiteCase, lo, hi float64) suiteCase {
	c.Check.AtLeast, c.Check.AtMost = &lo, &hi
	return c
}

// visibleCases are the visible cases of ops-reader in the farm git.
var visibleCases = []suiteCase{
	newCase("vis-ops-claims", "ops-reader claims the investigations it answers", "Role", "", "ops-reader", "spec.claims", nil, "ops.investigate.v1"),
	newCase("vis-ops-queue", "the queue of demo holds a day of the stand's minute schedules", "Tenant", "", "demo", "spec.limits.max_queued", atLeast(1440), ""),
	within(newCase("vis-ops-attempt", "an investigation attempt has at least the five minutes of its type", "Tenant", "", "demo",
		"spec.dispatch.rules[].timeouts.attempt", nil, ""), 300, 1800),
}

// hiddenCases are the hidden cases of ops-reader under <work>/holdout.
var hiddenCases = []suiteCase{
	newCase("hold-ops-insight", "ops-reader keeps its analysis type", "Role", "", "ops-reader", "spec.insight.types", nil, "ops.insight.v1"),
	newCase("hold-ops-enqueue", "demo takes the enqueues of an hour of every schedule", "Tenant", "", "demo", "spec.limits.max_enqueue_per_hour", atLeast(240), ""),
	newCase("hold-ops-queue", "the queue of demo holds the investigations of a busy day", "Tenant", "", "demo", "spec.limits.max_queued", atLeast(1000), ""),
	within(newCase("hold-ops-attempt", "an investigation attempt leaves room for a retry within the 30 minutes of its task", "Tenant", "", "demo",
		"spec.dispatch.rules[].timeouts.attempt", nil, ""), 300, 900),
}

func newCase(id, title, kind, tenant, name, field string, min *float64, contains string) suiteCase {
	c := suiteCase{ID: id, Role: "ops-reader", Title: title}
	if tenant == "" && kind != "Tenant" {
		tenant = "demo"
	}
	c.Check.Kind, c.Check.Tenant, c.Check.Name, c.Check.Field, c.Check.AtLeast, c.Check.Contains = kind, tenant, name, field, min, contains
	return c
}

// writeCases writes cases as <dir>/<role>/<id>.yaml.
func writeCases(dir string, cases []suiteCase) error {
	for _, c := range cases {
		if err := writeYAML(filepath.Join(dir, c.Role, c.ID+".yaml"), c); err != nil {
			return err
		}
	}
	return nil
}

// standSuite is the gate.Suite of the stand: the visible cases of the root's evals/cases, or the
// hidden ones of holdoutDir, of the given roles (every role when nil), checked against the farm
// folder of the root.
func standSuite() gate.Suite {
	return func(_ context.Context, root string, roles []string, holdoutDir string) (evals.SuiteResult, error) {
		dir := filepath.Join(root, "evals", "cases")
		if holdoutDir != "" {
			dir = holdoutDir
		}
		out := evals.SuiteResult{}
		folders, err := os.ReadDir(dir)
		if os.IsNotExist(err) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		docs, err := manifest.ReadFiles([]string{filepath.Join(root, farmSet)})
		if err != nil {
			return nil, err
		}
		set := make([]map[string]any, len(docs))
		for i, d := range docs {
			set[i] = d.Value
		}
		for _, f := range folders {
			if !f.IsDir() || roles != nil && !slices.Contains(roles, f.Name()) {
				continue
			}
			files, err := filepath.Glob(filepath.Join(dir, f.Name(), "*.yaml"))
			if err != nil {
				return nil, err
			}
			for _, file := range files {
				raw, err := os.ReadFile(file)
				if err != nil {
					return nil, err
				}
				var c suiteCase
				if err := yaml.Unmarshal(raw, &c); err != nil || c.ID == "" {
					// The name of a hidden file stays out of the error (EVO-7).
					if holdoutDir != "" {
						return nil, fmt.Errorf("a hidden case of %s does not parse", f.Name())
					}
					return nil, fmt.Errorf("%s: the case does not parse: %v", file, err)
				}
				st := evals.Failed
				if c.passes(set) {
					st = evals.Passed
				}
				out[c.ID] = evals.CaseOutcome{Case: c.ID, Status: st}
			}
		}
		return out, nil
	}
}

// passes says whether the set holds the fact of the case.
func (c suiteCase) passes(set []map[string]any) bool {
	m := findManifest(set, c.Check.Kind, c.Check.Tenant, c.Check.Name)
	if m == nil {
		return false
	}
	v, ok := fieldValue(m, c.Check.Field)
	if !ok {
		return false
	}
	if c.Check.AtLeast != nil || c.Check.AtMost != nil {
		n, ok := number(v)
		if !ok || c.Check.AtLeast != nil && n < *c.Check.AtLeast || c.Check.AtMost != nil && n > *c.Check.AtMost {
			return false
		}
	}
	return c.Check.Contains == "" || containsValue(v, c.Check.Contains)
}

func number(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	case string:
		if d, err := time.ParseDuration(strings.TrimSpace(x)); err == nil {
			return d.Seconds(), true
		}
		var f float64
		_, err := fmt.Sscan(strings.TrimSpace(x), &f)
		return f, err == nil
	}
	return 0, false
}
