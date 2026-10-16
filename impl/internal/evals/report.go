// SPDX-License-Identifier: AGPL-3.0-only

package evals

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

type junitSuites struct {
	XMLName xml.Name     `xml:"testsuites"`
	Suites  []junitSuite `xml:"testsuite"`
}

type junitSuite struct {
	Name     string      `xml:"name,attr"`
	Tests    int         `xml:"tests,attr"`
	Failures int         `xml:"failures,attr"`
	Errors   int         `xml:"errors,attr"`
	Skipped  int         `xml:"skipped,attr"`
	Cases    []junitCase `xml:"testcase"`
}

type junitCase struct {
	Name      string      `xml:"name,attr"`
	Classname string      `xml:"classname,attr"`
	Time      string      `xml:"time,attr"`
	Failure   *junitMsg   `xml:"failure,omitempty"`
	Error     *junitMsg   `xml:"error,omitempty"`
	Skipped   *junitSkip  `xml:"skipped,omitempty"`
	Props     *junitProps `xml:"properties,omitempty"`
}

type junitMsg struct {
	Message string `xml:"message,attr"`
	Body    string `xml:",chardata"`
}

type junitSkip struct {
	Message string `xml:"message,attr"`
}

type junitProps struct {
	Props []junitProp `xml:"property"`
}

type junitProp struct {
	Name  string `xml:"name,attr"`
	Value string `xml:"value,attr"`
}

// WriteJUnit writes the JUnit XML report, one test per case with its rules in the name, as the
// conformance runner does (docs/quality.md: both kinds write JUnit, so CI reads them the same
// way). A case not run or of a later version is skipped with its reason; an unrated case is an
// error.
func WriteJUnit(w io.Writer, rep *Report) error {
	s := junitSuite{Name: "botaru-eval-" + rep.Mode}
	for _, r := range rep.Results {
		name := r.Case.ID
		if len(r.Case.Rules) > 0 {
			name += " (" + strings.Join(r.Case.Rules, ", ") + ")"
		}
		jc := junitCase{Name: name, Classname: "evals." + r.Case.Role, Time: fmt.Sprintf("%.1f", r.Seconds)}
		props := []junitProp{{"status", string(r.Status)}, {"blocking", fmt.Sprint(r.Case.Blocking)}}
		if r.Status == Failed {
			props = append(props, junitProp{"security", fmt.Sprint(r.Security())})
		}
		jc.Props = &junitProps{Props: props}
		switch r.Status {
		case Failed:
			jc.Failure = &junitMsg{Message: "failed", Body: r.Detail}
			s.Failures++
		case Unrated:
			jc.Error = &junitMsg{Message: "not rated", Body: r.Detail}
			s.Errors++
		case NotRun, Skipped:
			jc.Skipped = &junitSkip{Message: string(r.Status) + ": " + r.Detail}
			s.Skipped++
		}
		s.Cases = append(s.Cases, jc)
		s.Tests++
	}
	if _, err := io.WriteString(w, xml.Header); err != nil {
		return err
	}
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	if err := enc.Encode(junitSuites{Suites: []junitSuite{s}}); err != nil {
		return err
	}
	_, err := io.WriteString(w, "\n")
	return err
}

// WriteResults writes results.jsonl: one line per case with its outcome, the reason and the
// spend; it stays on the test bench machine with the run.
func WriteResults(w io.Writer, rep *Report) error {
	enc := json.NewEncoder(w)
	for _, r := range rep.Results {
		line := map[string]any{"case": r.Case.ID, "role": r.Case.Role, "status": r.Status, "blocking": r.Case.Blocking, "rules": r.Case.Rules,
			"detail": r.Detail, "repeats": r.Repeats, "eur_micros": r.Spend, "security_failure": r.Security()}
		if len(r.NotCounted) > 0 {
			line["not_counted"] = r.NotCounted
		}
		if err := enc.Encode(line); err != nil {
			return err
		}
	}
	return nil
}

// WriteSummary prints the outcome of a run.
func WriteSummary(w io.Writer, rep *Report) error {
	n, sec := rep.Counts()
	var b strings.Builder
	fmt.Fprintf(&b, "Eval run (%s model), %s\n", rep.Mode, rep.Date)
	fmt.Fprintf(&b, "passed %d, failed %d, not rated %d, not run %d, skipped %d; security failures %d; spend %s EUR\n",
		n[Passed], n[Failed], n[Unrated], n[NotRun], n[Skipped], sec, formatEUR(rep.Spend))
	switch {
	case rep.Stopped != "":
		fmt.Fprintf(&b, "Outcome: not finished (%s)\n", rep.Stopped)
	case rep.Invalid != "":
		fmt.Fprintf(&b, "Outcome: invalid (%s)\n", rep.Invalid)
	case sec > 0:
		b.WriteString("Outcome: FAIL (security failures)\n")
	default:
		b.WriteString("Outcome: no security failure among the cases that ran\n")
	}
	for _, r := range rep.Results {
		if r.Status == Failed || r.Status == Unrated {
			fmt.Fprintf(&b, "  %s %s: %s\n", r.Case.ID, r.Status, r.Detail)
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

var reCaseNumber = regexp.MustCompile(`-[0-9]{2,3}$`)

// suiteOf is the eval set of a case: its id without the number (mgr-inj-01 -> mgr-inj).
func suiteOf(id string) string { return reCaseNumber.ReplaceAllString(id, "") }

// roleNames are the role manifest names of the bench role folders.
var roleNames = map[string]string{"hello": "hello", "ops-reader": "ops-reader"}

// IndexEntries are the EvalReports entries of a real run (PRV-3): one per role and set, with
// the static fingerprint of the role as the bench applied it, the date and the counts; metadata
// only, no inputs and no model text. An entry is written only when the run holds every active
// case of that role and set for the claim, each with an outcome, and every blocking case of the
// role has an outcome, so an entry never stands for cases that did not run: a run narrowed with
// -case or -role, or with cases not run, writes no entry for the sets it leaves incomplete. A
// mock run writes none, because it does not measure a model.
func IndexEntries(ctx context.Context, b *Bench, rep *Report) ([]map[string]any, []string, error) {
	if rep.Mode != "real" || rep.Stopped != "" || rep.Invalid != "" {
		return nil, nil, nil
	}
	type key struct{ role, suite string }
	got := map[string]*Result{}
	sets := map[key]bool{}
	for _, r := range rep.Results {
		if r.Status == Skipped {
			continue
		}
		got[r.Case.ID] = r
		sets[key{r.Case.Role, suiteOf(r.Case.ID)}] = true
	}
	rated := func(id string) bool {
		r := got[id]
		return r != nil && (r.Status == Passed || r.Status == Failed)
	}
	keys := slices.SortedFunc(mapKeys(sets), func(a, b key) int { return strings.Compare(a.role+"/"+a.suite, b.role+"/"+b.suite) })
	var out []map[string]any
	var notes []string
	for _, k := range keys {
		role := roleNames[k.role]
		if role == "" {
			continue
		}
		var missing, open, blocking []string
		failures, sec, n := 0, 0, 0
		for _, c := range rep.Scope {
			if c.Role != k.role {
				continue
			}
			if c.Blocking && !rated(c.ID) {
				blocking = append(blocking, c.ID)
			}
			if suiteOf(c.ID) != k.suite {
				continue
			}
			n++
			switch r := got[c.ID]; {
			case r == nil:
				missing = append(missing, c.ID)
			case !rated(c.ID):
				open = append(open, c.ID)
			case r.Status == Failed:
				failures++
				if r.Security() {
					sec++
				}
			}
		}
		switch {
		case len(missing) > 0:
			notes = append(notes, fmt.Sprintf("%s/%s: no index entry, the run does not hold every case of the set (missing %s)", k.role, k.suite, strings.Join(missing, ", ")))
			continue
		case len(open) > 0:
			notes = append(notes, fmt.Sprintf("%s/%s: no index entry, cases without an outcome: %s", k.role, k.suite, strings.Join(open, ", ")))
			continue
		case len(blocking) > 0:
			notes = append(notes, fmt.Sprintf("%s/%s: no index entry, blocking cases of the role without an outcome: %s", k.role, k.suite, strings.Join(blocking, ", ")))
			continue
		}
		fp, ok, err := b.c.StaticFingerprint(ctx, "demo", role)
		if err != nil {
			return nil, nil, err
		}
		e := map[string]any{"subject": map[string]any{"kind": "role", "name": role}, "suite": k.suite, "date": rep.Date,
			"cases": n, "failures": failures, "security_failures": sec}
		if ok {
			e["fingerprint"] = fp
		}
		out = append(out, e)
	}
	return out, notes, nil
}

// WriteIndex adds entries to the EvalReports manifest of evals/reports/index.yaml, creating it
// when it does not exist (spec/manifests/eval-reports.schema.json). The comments of an existing
// file are kept, and the file is replaced by a rename, so a crash never leaves half of it.
func WriteIndex(path string, entries []map[string]any) error {
	var root yaml.Node
	head := ""
	if raw, err := os.ReadFile(path); err == nil {
		if err := yaml.Unmarshal(raw, &root); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if len(root.Content) == 0 {
		head = "# The eval report index (PRV-3): run metadata only, written by botaru-eval.\n"
		doc := map[string]any{"apiVersion": "botaru/v1alpha1", "kind": "EvalReports", "metadata": map[string]any{"name": "evals"},
			"spec": map[string]any{"reports": []any{}}}
		var m yaml.Node
		if err := m.Encode(doc); err != nil {
			return err
		}
		root = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{&m}}
	}
	doc := root.Content[0]
	if doc.Kind != yaml.MappingNode || nodeValue(doc, "kind") == nil || nodeValue(doc, "kind").Value != "EvalReports" {
		return fmt.Errorf("%s is not an EvalReports manifest", path)
	}
	spec := nodeValue(doc, "spec")
	if spec == nil || spec.Kind != yaml.MappingNode {
		return fmt.Errorf("%s: spec is not a mapping", path)
	}
	reports := nodeValue(spec, "reports")
	if reports == nil {
		spec.Content = append(spec.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: "reports"}, &yaml.Node{Kind: yaml.SequenceNode})
		reports = spec.Content[len(spec.Content)-1]
	}
	if reports.Kind != yaml.SequenceNode {
		return fmt.Errorf("%s: spec.reports is not a list", path)
	}
	reports.Style = 0
	for _, e := range entries {
		var n yaml.Node
		if err := n.Encode(e); err != nil {
			return err
		}
		reports.Content = append(reports.Content, &n)
	}
	out, err := yaml.Marshal(&root)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".index-*.yaml")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append([]byte(head), out...)); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// nodeValue is the value of key in a YAML mapping node, or nil.
func nodeValue(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}
