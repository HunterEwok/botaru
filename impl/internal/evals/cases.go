// SPDX-License-Identifier: AGPL-3.0-only

// Package evals is the eval runner of the reference implementation (evals/README.md,
// docs/quality.md "Bot evaluation"): it reads the cases of evals/cases/, the catalog and the
// thresholds, plans a run against the eval rules of a claim with a cost estimate, stands up a
// test bench (the core with P1, P2 and P4, the real Telegram channel code against a fake Bot API
// server, the agent worker, toolhosts with the own plugins over generated data, a fake
// monitoring API), drives every case through the real path, checks the outcome with code and
// writes a JUnit report and the EvalReports index entries. The model is either the mock
// provider, which answers only what a case's setup scripts, or a real provider whose key the
// operator passes; a case that needs a real model is never passed on the mock.
package evals

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/HunterEwok/botaru/impl/internal/yamlx"
)

// Case is one eval case file of evals/cases/<role>/<id>.yaml (evals/case.schema.json).
type Case struct {
	ID, Role, File string
	Kind, Status   string
	Version        string
	Blocking       bool
	Rules          []string
	Summary        string
	BaseCase, Twin string
	From, Chat     string
	Message        string
	Task           map[string]any
	Data           map[string]any
	Inbox          []any
	Setup          map[string]any
	Lang           string
	State          string
	Checks         []map[string]any
}

// LoadCases reads every case file under evals/cases/, sorted by role and id.
func LoadCases(root string) ([]*Case, error) {
	base := filepath.Join(root, "evals/cases")
	var files []string
	err := filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(p, ".yaml") {
			files = append(files, p)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	out := make([]*Case, 0, len(files))
	for _, p := range files {
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		var m map[string]any
		if err := yamlx.Unmarshal(b, &m); err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		rel, _ := filepath.Rel(root, p)
		c := &Case{ID: str(m["id"]), Role: filepath.Base(filepath.Dir(p)), File: rel, Kind: str(m["kind"]), Status: str(m["status"]),
			Version: str(m["version"]), Rules: strs(m["rules"]), Summary: str(m["summary"]), BaseCase: str(m["base_case"]),
			Twin: str(m["twin"]), From: str(m["from"]), Chat: str(m["chat"]), Message: str(m["message"]), Task: asMap(m["task"]),
			Data: asMap(m["data"]), Inbox: asList(m["inbox"]), Setup: asMap(m["setup"]), Lang: str(m["lang"])}
		c.Blocking, _ = m["blocking"].(bool)
		if e := asMap(m["expect"]); e != nil {
			c.State = str(e["state"])
			for _, ch := range asList(e["checks"]) {
				c.Checks = append(c.Checks, asMap(ch))
			}
		}
		if c.ID == "" {
			return nil, fmt.Errorf("%s: no id", rel)
		}
		out = append(out, c)
	}
	return out, nil
}

// Filter selects cases: by role folder, by id and by the claimed version. Empty lists select all.
type Filter struct {
	Roles, IDs []string
	// Version is the claimed skeleton version: a case of a later version is not in the run.
	Version string
}

// Select returns the active cases of the filter and, separately, the cases the filter names that
// are pending or of a later version, which the report lists as skipped.
func Select(cases []*Case, f Filter) (in, later []*Case) {
	for _, c := range cases {
		if len(f.Roles) > 0 && !slices.Contains(f.Roles, c.Role) {
			continue
		}
		if len(f.IDs) > 0 && !slices.Contains(f.IDs, c.ID) {
			continue
		}
		if c.Status != "active" || f.Version != "" && cmpVersion(c.Version, f.Version) > 0 {
			later = append(later, c)
			continue
		}
		in = append(in, c)
	}
	return in, later
}

// cmpVersion compares skeleton versions of the form v0.2.
func cmpVersion(a, b string) int {
	pa, pb := versionParts(a), versionParts(b)
	for i := range pa {
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

func num(v any) (float64, bool) {
	switch t := v.(type) {
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	case float64:
		return t, true
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	}
	return 0, false
}
