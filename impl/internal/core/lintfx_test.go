// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/HunterEwok/botaru/impl/internal/ids"
	"github.com/HunterEwok/botaru/impl/internal/manifest"
	"github.com/HunterEwok/botaru/impl/internal/pgtest"
	"github.com/HunterEwok/botaru/impl/internal/spec"
	"github.com/HunterEwok/botaru/impl/internal/store"
	"github.com/HunterEwok/botaru/impl/internal/texts"
	"github.com/HunterEwok/botaru/impl/internal/yamlx"
)

var updateLint = flag.Bool("update-lint", false, "rewrite testdata/lint-passing.txt from this run")

// lintExtras are the case fields of conformance/fixtures/manifests that check something other
// than apply: the additive check of git, commit trailers, the conformance claim, driver
// schemas, flows, the deployment description and skills.
var lintExtras = []string{"before", "commit", "conforms_to", "driver_schema", "flow", "deploy", "skills"}

// exampleManifests reads the manifests of examples/ outside the deployment description, as the
// skeleton lint does, without the fields and kinds this core refuses as later (MAN-11).
func exampleManifests(t *testing.T, root string, sp *spec.Spec) []map[string]any {
	t.Helper()
	var out []map[string]any
	err := filepath.WalkDir(filepath.Join(root, "examples"), func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".yaml") || strings.Contains(p, "/examples/deploy/") {
			return err
		}
		docs, err := manifest.ReadFiles([]string{p})
		if err != nil {
			return nil
		}
		for _, d := range docs {
			if _, ok := d.Value["apiVersion"]; !ok {
				continue
			}
			ptrs, err := sp.Unsupported(d.Value)
			if err != nil {
				return err
			}
			ok := true
			for _, ptr := range ptrs {
				if ptr == "/kind" || sp.Droppable(d.Value, ptr) != nil || !manifest.DropPointer(d.Value, ptr) {
					ok = false
					break
				}
			}
			if ok {
				out = append(out, d.Value)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func objKey(m map[string]any) string {
	md, _ := m["metadata"].(map[string]any)
	return fmt.Sprintf("%v/%v/%v", m["kind"], md["tenant"], md["name"])
}

// TestLintFixtures runs the cases of conformance/fixtures/manifests through apply with dry_run
// and compares the error codes with the expectation, as ci/check_manifests.py does for the
// skeleton lint. A case that carries a field of a later version than this core is skipped.
// The test fails when the set of passing cases differs from testdata/lint-passing.txt: a case
// that stopped passing is a regression, one that started to pass is recorded with
// -update-lint.
func TestLintFixtures(t *testing.T) {
	ctx := context.Background()
	root, err := spec.FindRoot("")
	if err != nil {
		t.Fatal(err)
	}
	sp, err := spec.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := texts.Load(root, "")
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, pgtest.DSN(t))
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
	examples := exampleManifests(t, root, sp)
	// The examples as this core sees them miss the objects of later versions, so references
	// to those fail in every case; such errors of the examples alone are not counted.
	base := map[string]bool{}
	if res, err := c.Apply(ctx, Operator{Principal: ids.PrincipalID(), Tenant: "farm"}, ApplyRequest{Commit: strings.Repeat("0", 40), Manifests: examples, DryRun: true}); err == nil {
		for _, e := range res.Errors {
			base[e.Kind+"/"+e.Name+"/"+e.Code] = true
		}
	}
	files, err := filepath.Glob(filepath.Join(root, "conformance/fixtures/manifests/*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var passing []string
	var report []string
	for _, file := range files {
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		var fx map[string]any
		if err := yamlx.Unmarshal(b, &fx); err != nil {
			t.Fatal(err)
		}
		cases, _ := fx["cases"].([]any)
		for i, ca := range cases {
			cs, _ := ca.(map[string]any)
			id := fmt.Sprintf("%v#%d", fx["id"], i)
			extra := false
			for _, k := range lintExtras {
				if _, ok := cs[k]; ok {
					extra = true
				}
			}
			if extra {
				continue
			}
			var own []map[string]any
			later := false
			for _, m := range anyList(cs["manifests"]) {
				mm, _ := m.(map[string]any)
				if mm == nil {
					continue
				}
				if ptrs, err := sp.Unsupported(mm); err == nil && len(ptrs) > 0 {
					later = true
				}
				own = append(own, mm)
			}
			if later {
				continue
			}
			docs := own
			ownKeys := map[string]bool{}
			for _, m := range own {
				md, _ := m["metadata"].(map[string]any)
				ownKeys[fmt.Sprintf("%v/%v", m["kind"], md["name"])] = true
			}
			withEx, ok := cs["with_examples"].(bool)
			if !ok {
				withEx, _ = fx["with_examples"].(bool)
			}
			if withEx {
				keys := map[string]bool{}
				for _, m := range own {
					keys[objKey(m)] = true
				}
				docs = nil
				for _, m := range examples {
					if !keys[objKey(m)] {
						docs = append(docs, deepCopy(m))
					}
				}
				docs = append(docs, own...)
			}
			if len(docs) == 0 {
				continue
			}
			farm := []string{"core1", "backup1"}
			for _, src := range []map[string]any{fx, cs} {
				if hs, ok := src["farm_hosts"].([]any); ok {
					farm = nil
					for _, h := range hs {
						farm = append(farm, fmt.Sprint(h))
					}
				}
			}
			c.cfg.FarmHosts = farm
			res, err := c.Apply(ctx, Operator{Principal: ids.PrincipalID(), Tenant: "farm"}, ApplyRequest{Commit: strings.Repeat("0", 40), Manifests: docs, DryRun: true})
			got := map[string]bool{}
			if err != nil {
				var p *Problem
				if pe, ok := err.(*Problem); ok {
					p = pe
				}
				if p == nil {
					t.Fatalf("%s: %v", id, err)
				}
				got[p.Reason] = true
			} else {
				for _, e := range res.Errors {
					if withEx && base[e.Kind+"/"+e.Name+"/"+e.Code] && !ownKeys[e.Kind+"/"+e.Name] {
						continue
					}
					got[e.Code] = true
					if os.Getenv("BOTARU_LINT_CASE") == id {
						t.Logf("%s: %+v", id, e)
					}
				}
			}
			pass := false
			want := "ok"
			switch exp := cs["expect"].(type) {
			case nil, string:
				pass = len(got) == 0
			case map[string]any:
				want = fmt.Sprint(exp["error"])
				also := map[string]bool{}
				allAlso := false
				switch a := exp["also"].(type) {
				case bool:
					allAlso = a
				case []any:
					for _, x := range a {
						also[fmt.Sprint(x)] = true
					}
				}
				pass = got[want]
				for code := range got {
					if code != want && !also[code] && !allAlso {
						pass = false
					}
				}
			}
			if pass {
				passing = append(passing, id)
			} else {
				codes := make([]string, 0, len(got))
				for k := range got {
					codes = append(codes, k)
				}
				sort.Strings(codes)
				report = append(report, fmt.Sprintf("%s (%v): want %s, got %v", id, cs["name"], want, codes))
			}
		}
	}
	sort.Strings(passing)
	if os.Getenv("BOTARU_LINT_REPORT") != "" {
		t.Logf("failing:\n%s", strings.Join(report, "\n"))
	}
	path := filepath.Join("testdata", "lint-passing.txt")
	if *updateLint {
		head := "# Cases of conformance/fixtures/manifests that pass apply (TestLintFixtures); rewrite with -update-lint.\n"
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(head+strings.Join(passing, "\n")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{}
	for _, l := range strings.Split(string(b), "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
			want[l] = true
		}
	}
	got := map[string]bool{}
	for _, id := range passing {
		got[id] = true
		if !want[id] {
			t.Errorf("%s passes now; record it with go test ./internal/core -run TestLintFixtures -update-lint", id)
		}
	}
	for id := range want {
		if !got[id] {
			t.Errorf("%s no longer passes", id)
		}
	}
}

func anyList(v any) []any { l, _ := v.([]any); return l }

func deepCopy(v map[string]any) map[string]any {
	var cp func(any) any
	cp = func(x any) any {
		switch t := x.(type) {
		case map[string]any:
			m := make(map[string]any, len(t))
			for k, v := range t {
				m[k] = cp(v)
			}
			return m
		case []any:
			l := make([]any, len(t))
			for i, v := range t {
				l[i] = cp(v)
			}
			return l
		}
		return x
	}
	return cp(v).(map[string]any)
}
