// SPDX-License-Identifier: AGPL-3.0-only

// Package conform is the conformance runner of the reference implementation
// (conformance/runner.md): it reads the rules of conformance/requirements.yaml and the behaviour
// fixtures of conformance/fixtures/, selects the cases of a claim, runs each case on a fresh test
// bench through P1, P2 and the test-control port, and reports per case and per rule. A case that
// needs something the runner or the core cannot do yet is reported as unchecked with the first
// thing it needs; an unchecked case never counts as covering its rule.
package conform

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/HunterEwok/botaru/impl/internal/yamlx"
)

// Rule is one rule of conformance/requirements.yaml, with the fields the runner selects by.
type Rule struct {
	ID       string
	Title    string
	Status   string
	Version  string
	Until    string
	Profile  string
	Waivable bool
	Check    []string
	Fixtures []string
}

// Case is one runnable case: a fixture or case without cases, with what it inherits from its
// parents already folded in (conformance/runner.md, "Behaviour fixture envelope").
type Case struct {
	ID      string
	File    string // relative to the skeleton root
	Fixture string // the id of the top-level fixture
	Parents []string
	Rules   []string
	Version string
	Given   map[string]any
	Steps   []map[string]any
	Expect  map[string]any
	// OwnSteps is false when the steps come from a parent or there are none.
	OwnSteps bool
	// narrowed marks a permission table row whose given the runner narrowed (plainRow).
	narrowed bool
}

// Claim is what an implementation claims in conforms_to: a skeleton version and profiles.
type Claim struct {
	Version  string
	Profiles []string
}

// LoadRules reads conformance/requirements.yaml.
func LoadRules(root string) (map[string]*Rule, error) {
	b, err := os.ReadFile(filepath.Join(root, "conformance/requirements.yaml"))
	if err != nil {
		return nil, err
	}
	var raw []map[string]any
	if err := yamlx.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("requirements.yaml: %w", err)
	}
	out := map[string]*Rule{}
	for _, m := range raw {
		r := &Rule{ID: str(m["id"]), Title: str(m["title"]), Status: str(m["status"]), Version: str(m["version"]),
			Until: str(m["until"]), Profile: str(m["profile"]), Check: strs(m["check"]), Fixtures: strs(m["fixtures"])}
		r.Waivable, _ = m["waivable"].(bool)
		out[r.ID] = r
	}
	return out, nil
}

// skipDirs are the fixture folders the skeleton CI runs itself, and the transition lists that
// are not in the behaviour envelope.
var skipDirs = map[string]string{
	"schemas":       "run by the skeleton CI (ci/check_schemas.py)",
	"manifests":     "run by the skeleton CI (ci/check_manifests.py)",
	"state-machine": "transition lists, not behaviour cases",
}

// LoadCases reads every behaviour fixture under conformance/fixtures/ and returns its runnable
// cases in file order.
func LoadCases(root string) ([]*Case, error) {
	base := filepath.Join(root, "conformance/fixtures")
	var files []string
	err := filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if _, skip := skipDirs[d.Name()]; skip {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(p, ".yaml") {
			files = append(files, p)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	var out []*Case
	for _, p := range files {
		rel, _ := filepath.Rel(root, p)
		f, err := os.Open(p)
		if err != nil {
			return nil, err
		}
		dec := yamlx.NewDecoder(f)
		for {
			var doc map[string]any
			err := dec.Decode(&doc)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				f.Close()
				return nil, fmt.Errorf("%s: %w", rel, err)
			}
			if doc == nil {
				continue
			}
			flatten(&out, rel, str(doc["id"]), nil, doc, &Case{})
		}
		f.Close()
	}
	return out, nil
}

// flatten folds a fixture node onto what it inherits and appends its leaf cases.
func flatten(out *[]*Case, file, fixture string, parents []string, node map[string]any, parent *Case) {
	c := &Case{
		ID: str(node["id"]), File: file, Fixture: fixture, Parents: parents,
		Rules: parent.Rules, Version: parent.Version, Steps: parent.Steps, Expect: parent.Expect,
		Given: mergeGiven(parent.Given, asMap(node["given"])),
	}
	if rs := strs(node["rules"]); len(rs) > 0 {
		c.Rules = rs
	}
	if v := str(node["version"]); v != "" {
		c.Version = v
	}
	if s, ok := node["steps"].([]any); ok {
		c.Steps = nil
		for _, st := range s {
			c.Steps = append(c.Steps, asMap(st))
		}
		c.OwnSteps = true
	}
	if e, ok := node["expect"].(map[string]any); ok {
		c.Expect = e
	}
	cases, _ := node["cases"].([]any)
	if len(cases) == 0 {
		*out = append(*out, c)
		return
	}
	next := append(append([]string{}, parents...), c.ID)
	for i, sub := range cases {
		sm := asMap(sub)
		if str(sm["id"]) == "" {
			// A case without an id is named by its parent and its place.
			sm = shallowCopy(sm)
			sm["id"] = fmt.Sprintf("%s#%d", c.ID, i+1)
		}
		flatten(out, file, fixture, next, sm, c)
	}
}

// mergeGiven folds a case's given onto its parent's: a key replaces the parent's key, and in
// people an entry replaces the parent's entry about the same principal.
func mergeGiven(parent, own map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range parent {
		out[k] = v
	}
	for k, v := range own {
		if k == "people" {
			out[k] = mergePeople(asList(parent["people"]), asList(v))
			continue
		}
		out[k] = v
	}
	return out
}

func mergePeople(parent, own []any) []any {
	var out []any
	idx := map[string]int{}
	for _, p := range parent {
		idx[str(asMap(p)["principal"])] = len(out)
		out = append(out, p)
	}
	for _, p := range own {
		key := str(asMap(p)["principal"])
		if i, ok := idx[key]; ok {
			out[i] = p
			continue
		}
		idx[key] = len(out)
		out = append(out, p)
	}
	return out
}

// Select returns the cases of the claim: a case is in when one of its rules is an active rule of
// a claimed profile whose version is not newer than the claim and whose until, if set, is not
// older (MAN-8), and none of its rules ended before the claim: such a case checks behaviour that
// the rules of its superseded_by replaced (D-113). A case whose own version is newer than the
// claim is returned in skipped.
func Select(rules map[string]*Rule, cases []*Case, claim Claim) (in, skipped []*Case) {
	for _, c := range cases {
		if len(InScopeRules(rules, c, claim)) == 0 || endedRule(rules, c, claim) {
			continue
		}
		v := c.Version
		if s := str(c.Given["since"]); s != "" {
			v = s
		}
		if v != "" && cmpVersion(v, claim.Version) > 0 {
			skipped = append(skipped, c)
			continue
		}
		in = append(in, c)
	}
	return in, skipped
}

// endedRule says whether a case names a rule whose until is older than the claim.
func endedRule(rules map[string]*Rule, c *Case, claim Claim) bool {
	for _, id := range c.Rules {
		if r, ok := rules[id]; ok && r.Until != "" && cmpVersion(r.Until, claim.Version) < 0 {
			return true
		}
	}
	return false
}

// InScopeRules returns the rules of a case that the claim checks.
func InScopeRules(rules map[string]*Rule, c *Case, claim Claim) []string {
	var out []string
	for _, id := range c.Rules {
		if r, ok := rules[id]; ok && RuleInScope(r, claim) {
			out = append(out, id)
		}
	}
	return out
}

// RuleInScope tells whether the claim checks a rule.
func RuleInScope(r *Rule, claim Claim) bool {
	if r.Status != "active" || cmpVersion(r.Version, claim.Version) > 0 {
		return false
	}
	if r.Until != "" && cmpVersion(r.Until, claim.Version) < 0 {
		return false
	}
	for _, p := range claim.Profiles {
		if p == r.Profile {
			return true
		}
	}
	return false
}

// cmpVersion compares skeleton versions of the form v0.2 or 0.2.1.
func cmpVersion(a, b string) int {
	pa, pb := versionParts(a), versionParts(b)
	for i := range 3 {
		if pa[i] != pb[i] {
			if pa[i] < pb[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

func versionParts(v string) [3]int {
	var out [3]int
	for i, p := range strings.SplitN(strings.TrimPrefix(v, "v"), ".", 3) {
		n := 0
		for _, ch := range p {
			if ch < '0' || ch > '9' {
				break
			}
			n = n*10 + int(ch-'0')
		}
		out[i] = n
	}
	return out
}

func shallowCopy(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func strs(v any) []string {
	var out []string
	for _, x := range asList(v) {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func asList(v any) []any {
	l, _ := v.([]any)
	return l
}
