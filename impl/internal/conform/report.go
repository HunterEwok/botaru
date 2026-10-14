// SPDX-License-Identifier: AGPL-3.0-only

package conform

import (
	"encoding/xml"
	"fmt"
	"io"
	"sort"
	"strings"
)

// WriteJUnit writes the JUnit XML report: one test per case, with the rule numbers and the case
// id in its name (conformance/runner.md, "Report and expected failures"). An unchecked case is
// reported as skipped with what it needs, never as passed.
func WriteJUnit(w io.Writer, results []Result) error {
	type failure struct {
		Message string `xml:"message,attr"`
	}
	type skipped struct {
		Message string `xml:"message,attr"`
	}
	type testcase struct {
		Name      string   `xml:"name,attr"`
		Classname string   `xml:"classname,attr"`
		Failure   *failure `xml:"failure,omitempty"`
		Skipped   *skipped `xml:"skipped,omitempty"`
	}
	type suite struct {
		Name     string     `xml:"name,attr"`
		Tests    int        `xml:"tests,attr"`
		Failures int        `xml:"failures,attr"`
		Skipped  int        `xml:"skipped,attr"`
		Cases    []testcase `xml:"testcase"`
	}
	type suites struct {
		XMLName xml.Name `xml:"testsuites"`
		Suites  []*suite `xml:"testsuite"`
	}
	byFile := map[string]*suite{}
	var out suites
	for _, r := range results {
		s := byFile[r.Case.File]
		if s == nil {
			s = &suite{Name: r.Case.File}
			byFile[r.Case.File] = s
			out.Suites = append(out.Suites, s)
		}
		tc := testcase{Name: strings.Join(r.Rules, ",") + " " + r.Case.ID, Classname: r.Case.Fixture}
		switch r.Status {
		case Failed:
			tc.Failure = &failure{Message: r.Detail}
			s.Failures++
		case Unchecked:
			tc.Skipped = &skipped{Message: "unchecked: " + r.Detail}
			s.Skipped++
		case Skipped:
			tc.Skipped = &skipped{Message: r.Detail}
			s.Skipped++
		}
		s.Tests++
		s.Cases = append(s.Cases, tc)
	}
	if _, err := io.WriteString(w, xml.Header); err != nil {
		return err
	}
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	return enc.Encode(out)
}

// RuleCoverage is how the cases of one rule came out.
type RuleCoverage struct {
	Rule                                *Rule
	Passed, Failed, Unchecked, Skipped  int
	FirstNeed, FirstFailure, FailedCase string
}

// State names the coverage of a rule: done when it has cases and all of them passed; partial
// when some passed; failing when one failed and none passed; open when its cases are only
// unchecked; no-case when no behaviour case names it (a lint, eval or suite rule).
func (c *RuleCoverage) State() string {
	n := c.Passed + c.Failed + c.Unchecked
	switch {
	case n == 0:
		return "no-case"
	case c.Passed == n:
		return "done"
	case c.Passed > 0:
		return "partial"
	case c.Failed > 0:
		return "failing"
	}
	return "open"
}

// Coverage counts per rule of the claim.
func Coverage(rules map[string]*Rule, claim Claim, results []Result) []*RuleCoverage {
	by := map[string]*RuleCoverage{}
	for _, r := range rules {
		if RuleInScope(r, claim) {
			by[r.ID] = &RuleCoverage{Rule: r}
		}
	}
	for _, res := range results {
		for _, id := range res.Rules {
			c := by[id]
			if c == nil {
				continue
			}
			switch res.Status {
			case Passed:
				c.Passed++
			case Failed:
				c.Failed++
				if c.FirstFailure == "" {
					c.FirstFailure, c.FailedCase = res.Detail, res.Case.ID
				}
			case Unchecked:
				c.Unchecked++
				if c.FirstNeed == "" {
					c.FirstNeed = res.Detail
				}
			case Skipped:
				c.Skipped++
			}
		}
	}
	out := make([]*RuleCoverage, 0, len(by))
	for _, c := range by {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Rule.Profile != out[j].Rule.Profile {
			return out[i].Rule.Profile < out[j].Rule.Profile
		}
		return ruleLess(out[i].Rule.ID, out[j].Rule.ID)
	})
	return out
}

// ruleLess orders rule ids by prefix, then by number (APR-2 before APR-10).
func ruleLess(a, b string) bool {
	pa, na := splitRule(a)
	pb, nb := splitRule(b)
	if pa != pb {
		return pa < pb
	}
	return na < nb
}

func splitRule(id string) (string, int) {
	i := strings.LastIndexByte(id, '-')
	if i < 0 {
		return id, 0
	}
	n := 0
	fmt.Sscanf(id[i+1:], "%d", &n)
	return id[:i], n
}

var states = []string{"done", "partial", "failing", "open", "no-case"}

// WriteCoverage writes the coverage table in Markdown: a line per profile, the things the open
// cases need most, the failing cases, and a line per rule.
func WriteCoverage(w io.Writer, claim Claim, cov []*RuleCoverage, results []Result) error {
	p := func(f string, a ...any) { fmt.Fprintf(w, f, a...) }
	p("# Rule coverage, claim %s %v\n\n", claim.Version, claim.Profiles)
	p("A rule is done when every behaviour case that names it passes on the reference core; an unchecked case never counts (conformance/runner.md).\n\n")
	p("| Profile | Rules | %s |\n|---|---|%s\n", strings.Join(states, " | "), strings.Repeat("---|", len(states)))
	byProfile := map[string]map[string]int{}
	var profiles []string
	for _, c := range cov {
		m := byProfile[c.Rule.Profile]
		if m == nil {
			m = map[string]int{}
			byProfile[c.Rule.Profile] = m
			profiles = append(profiles, c.Rule.Profile)
		}
		m[c.State()]++
		m["all"]++
	}
	total := map[string]int{}
	for _, pr := range profiles {
		row := []string{pr, fmt.Sprint(byProfile[pr]["all"])}
		total["all"] += byProfile[pr]["all"]
		for _, s := range states {
			row = append(row, fmt.Sprint(byProfile[pr][s]))
			total[s] += byProfile[pr][s]
		}
		p("| %s |\n", strings.Join(row, " | "))
	}
	row := []string{"all", fmt.Sprint(total["all"])}
	for _, s := range states {
		row = append(row, fmt.Sprint(total[s]))
	}
	p("| %s |\n\n", strings.Join(row, " | "))

	counts := map[Status]int{}
	needs := map[string]int{}
	for _, r := range results {
		counts[r.Status]++
		if r.Status == Unchecked {
			needs[r.Detail]++
		}
	}
	p("Cases: %d passed, %d failed, %d unchecked, %d skipped (later version).\n\n", counts[Passed], counts[Failed], counts[Unchecked], counts[Skipped])
	type kv struct {
		k string
		v int
	}
	var top []kv
	for k, v := range needs {
		top = append(top, kv{k, v})
	}
	sort.Slice(top, func(i, j int) bool {
		if top[i].v != top[j].v {
			return top[i].v > top[j].v
		}
		return top[i].k < top[j].k
	})
	if len(top) > 0 {
		p("## What unchecked cases need first\n\n| Need | Cases |\n|---|---|\n")
		for i, t := range top {
			if i == 40 {
				p("| %d more needs | |\n", len(top)-40)
				break
			}
			p("| %s | %d |\n", mdCell(t.k), t.v)
		}
		p("\n")
	}
	var failed []Result
	for _, r := range results {
		if r.Status == Failed {
			failed = append(failed, r)
		}
	}
	if len(failed) > 0 {
		p("## Failing cases\n\n| Case | Rules | Mismatch |\n|---|---|---|\n")
		for _, r := range failed {
			p("| %s | %s | %s |\n", r.Case.ID, strings.Join(r.Rules, ", "), mdCell(r.Detail))
		}
		p("\n")
	}
	p("## Rules\n\n| Rule | Profile | State | Passed | Failed | Unchecked | First need or failure |\n|---|---|---|---|---|---|---|\n")
	for _, c := range cov {
		note := c.FirstNeed
		if c.FirstFailure != "" {
			note = c.FailedCase + ": " + c.FirstFailure
		}
		p("| %s | %s | %s | %d | %d | %d | %s |\n", c.Rule.ID, c.Rule.Profile, c.State(), c.Passed, c.Failed, c.Unchecked, mdCell(note))
	}
	return nil
}

func mdCell(s string) string {
	return strings.NewReplacer("|", "\\|", "\n", " ").Replace(s)
}

// Passing returns the ids of the passed cases, sorted.
func Passing(results []Result) []string {
	var out []string
	for _, r := range results {
		if r.Status == Passed {
			out = append(out, r.Case.ID)
		}
	}
	sort.Strings(out)
	return out
}
