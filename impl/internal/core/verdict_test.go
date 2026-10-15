// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/HunterEwok/botaru/impl/internal/spec"
)

// The verdict tables of the conformance fixtures com-03 and com-06 give the same outcome, mean
// and strict thresholds here (COM-4, COM-14).
func TestVerdictFixtures(t *testing.T) {
	root, err := spec.FindRoot("")
	if err != nil {
		t.Fatal(err)
	}
	veto := map[string]bool{"security": true, "operational": true}
	for _, f := range []struct {
		file   string
		lenses []string
		none   bool
	}{
		{"conformance/fixtures/commission/verdict.yaml", []string{"security", "operational", "cost", "value"}, false},
		{"conformance/fixtures/commission/kind-none.yaml", []string{"security", "value"}, true},
	} {
		raw, err := os.ReadFile(filepath.Join(root, f.file))
		if err != nil {
			t.Fatal(err)
		}
		var fx struct {
			Given struct {
				Commission map[string]any `yaml:"commission"`
			} `yaml:"given"`
			Cases []struct {
				Description string `yaml:"description"`
				Given       struct {
					Reviews       map[string]any `yaml:"reviews"`
					Risk          string         `yaml:"risk"`
					UntrustedSeen bool           `yaml:"untrusted_seen"`
				} `yaml:"given"`
				Steps  []any          `yaml:"steps"`
				Expect map[string]any `yaml:"expect"`
			} `yaml:"cases"`
		}
		if err := yaml.Unmarshal(raw, &fx); err != nil {
			t.Fatal(err)
		}
		p := policyFrom(fx.Given.Commission)
		for _, cs := range fx.Cases {
			want, ok := cs.Expect["verdict"]
			if !ok || cs.Steps != nil {
				continue
			}
			in := verdictInput{KindNone: f.none, Risk: cs.Given.Risk, Untrusted: cs.Given.UntrustedSeen, Lenses: f.lenses, VetoLens: veto}
			if in.Risk == "" {
				in.Risk = "low"
			}
			for lens, v := range cs.Given.Reviews {
				switch x := v.(type) {
				case int:
					in.Reviews = append(in.Reviews, lensReview{Lens: lens, State: "done", Score: x})
				case string:
					in.Reviews = append(in.Reviews, lensReview{Lens: lens, State: x})
				case map[string]any:
					s, _ := x["score"].(int)
					vt, _ := x["veto"].(bool)
					in.Reviews = append(in.Reviews, lensReview{Lens: lens, State: "done", Score: s, Veto: vt})
				case []any:
					for _, sh := range x {
						m := sh.(map[string]any)
						s, _ := m["score"].(int)
						in.Reviews = append(in.Reviews, lensReview{Lens: m["lens"].(string), State: "done", Score: s, Shadow: true})
					}
				}
			}
			got := verdict(p, in)
			exp := fmt.Sprint(want)
			if m, ok := want.(map[string]any); ok {
				exp = fmt.Sprintf("%v(%v)", m["state"], m["reason"])
			}
			if got["outcome"] != exp {
				t.Errorf("%s: %s: verdict %v, want %s", f.file, cs.Description, got["outcome"], exp)
			}
			if wm, ok := cs.Expect["weighted_mean"]; ok && fmt.Sprint(got["weighted_mean"]) != fmt.Sprint(wm) {
				t.Errorf("%s: %s: mean %v, want %v", f.file, cs.Description, got["weighted_mean"], wm)
			}
			if st, ok := cs.Expect["strict"]; ok && fmt.Sprint(got["strict"]) != fmt.Sprint(st) {
				t.Errorf("%s: %s: strict %v, want %v", f.file, cs.Description, got["strict"], st)
			}
			// The same input gives the same verdict.
			if again := verdict(p, in); fmt.Sprint(again) != fmt.Sprint(got) {
				t.Errorf("%s: %s: not deterministic", f.file, cs.Description)
			}
		}
	}
}
