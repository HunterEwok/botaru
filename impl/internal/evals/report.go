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

	"github.com/HunterEwok/botaru/impl/internal/yamlx"
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

// IndexEntries are the EvalReports entries of a real run (PRV-3): one per role and set whose
// cases all have an outcome, with the static fingerprint of the role as the bench applied it, the
// date and the counts; metadata only, no inputs and no model text. A set with a case not run or
// not rated gets no entry, so an entry never stands for cases that did not run; a mock run
// writes none, because it does not measure a model.
func IndexEntries(ctx context.Context, b *Bench, rep *Report) ([]map[string]any, []string, error) {
	if rep.Mode != "real" || rep.Stopped != "" || rep.Invalid != "" {
		return nil, nil, nil
	}
	type key struct{ role, suite string }
	groups := map[key][]*Result{}
	for _, r := range rep.Results {
		if r.Status == Skipped {
			continue
		}
		k := key{r.Case.Role, suiteOf(r.Case.ID)}
		groups[k] = append(groups[k], r)
	}
	keys := slices.SortedFunc(mapKeys(groups), func(a, b key) int { return strings.Compare(a.role+"/"+a.suite, b.role+"/"+b.suite) })
	var out []map[string]any
	var notes []string
	for _, k := range keys {
		rs := groups[k]
		role := roleNames[k.role]
		if role == "" {
			continue
		}
		failures, sec, open := 0, 0, 0
		for _, r := range rs {
			switch r.Status {
			case Failed:
				failures++
				if r.Security() {
					sec++
				}
			case NotRun, Unrated:
				open++
			}
		}
		if open > 0 {
			notes = append(notes, fmt.Sprintf("%s/%s: %d of %d cases without an outcome, no index entry", k.role, k.suite, open, len(rs)))
			continue
		}
		fp, ok, err := b.c.StaticFingerprint(ctx, "demo", role)
		if err != nil {
			return nil, nil, err
		}
		e := map[string]any{"subject": map[string]any{"kind": "role", "name": role}, "suite": k.suite, "date": rep.Date,
			"cases": len(rs), "failures": failures, "security_failures": sec}
		if ok {
			e["fingerprint"] = fp
		}
		out = append(out, e)
	}
	return out, notes, nil
}

// WriteIndex adds entries to the EvalReports manifest of evals/reports/index.yaml, creating it
// when it does not exist (spec/manifests/eval-reports.schema.json).
func WriteIndex(path string, entries []map[string]any) error {
	doc := map[string]any{"apiVersion": "botaru/v1alpha1", "kind": "EvalReports", "metadata": map[string]any{"name": "evals"},
		"spec": map[string]any{"reports": []any{}}}
	if raw, err := os.ReadFile(path); err == nil {
		var old map[string]any
		if err := yamlx.Unmarshal(raw, &old); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		if str(old["kind"]) != "EvalReports" {
			return fmt.Errorf("%s is not an EvalReports manifest", path)
		}
		doc = old
	} else if !os.IsNotExist(err) {
		return err
	}
	sp := asMap(doc["spec"])
	if sp == nil {
		sp = map[string]any{}
		doc["spec"] = sp
	}
	reports := asList(sp["reports"])
	for _, e := range entries {
		reports = append(reports, e)
	}
	sp["reports"] = reports
	out, err := yaml.Marshal(doc)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	head := "# The eval report index (PRV-3): run metadata only, written by botaru-eval.\n"
	return os.WriteFile(path, append([]byte(head), out...), 0o644)
}
