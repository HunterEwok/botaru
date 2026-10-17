// SPDX-License-Identifier: AGPL-3.0-only

package spec

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/dlclark/regexp2"

	"github.com/HunterEwok/botaru/impl/internal/ijson"
)

func load(t *testing.T) *Spec {
	t.Helper()
	root, err := FindRoot("")
	if err != nil {
		t.Fatal(err)
	}
	s, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestCompileAll(t *testing.T) {
	if err := load(t).CompileAll(); err != nil {
		t.Fatal(err)
	}
}

func TestValidate(t *testing.T) {
	s := load(t)
	ok, _ := ijson.Parse([]byte(`{"kinds":["attempt"],"wait_s":30}`))
	if err := s.Validate("p1-claim-request", ok); err != nil {
		t.Fatal(err)
	}
	bad, _ := ijson.Parse([]byte(`{"kinds":["attempt"],"tenant":"demo"}`))
	err := s.Validate("p1-claim-request", bad)
	var ve *ValidationError
	if !errors.As(err, &ve) || len(ve.Violations) == 0 {
		t.Fatalf("want a validation error, got %v", err)
	}
	// A lookahead pattern (ECMAScript) compiles and works.
	key, _ := ijson.Parse([]byte(`{"body":{"type":"hello.greet.v1","goal":"hi"},"idempotency_key":"system:x"}`))
	if err := s.Validate("enqueue-request", key); err == nil {
		t.Fatal("system: key accepted")
	}
}

func TestRegistry(t *testing.T) {
	r, err := load(t).Registry("spec/model/reasons.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !r.Has("lease_expired") || r.Has("no_such_reason") {
		t.Fatal("reasons registry lookup")
	}
}

// An installation with the binaries in <base>/bin and the checkout in <base>/src finds the
// skeleton without a flag; otherwise the error names the flag and the variable.
func TestFindRootNextToBin(t *testing.T) {
	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, "src", "spec"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "src", "spec", "VERSION"), []byte("0.1.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := findRoot("/", filepath.Join(base, "bin", "botaru-telegram"))
	if err != nil || got != filepath.Join(base, "src") {
		t.Fatalf("got %q %v", got, err)
	}
	_, err = findRoot("/", filepath.Join(t.TempDir(), "bin", "x"))
	if err == nil || !strings.Contains(err.Error(), "-spec-root") || !strings.Contains(err.Error(), EnvRoot) {
		t.Fatalf("error %v", err)
	}
}

// The date format of the registry takes exactly the calendar dates that exist (RFC 3339
// full-date), in the ECMAScript engine the schemas use and in Go's own regexp (the ingress
// driver).
func TestDateFormat(t *testing.T) {
	reg, err := load(t).Registry("spec/model/formats.yaml")
	if err != nil {
		t.Fatal(err)
	}
	e, ok := reg.Entry("date")
	if !ok {
		t.Fatal("formats.yaml has no date")
	}
	pat, _ := e["pattern"].(string)
	ecma, err := regexp2.Compile(pat, regexp2.ECMAScript)
	if err != nil {
		t.Fatal(err)
	}
	re2 := regexp.MustCompile(pat)
	for _, year := range []int{1900, 2000, 2024, 2026, 2100, 2400} {
		for month := 0; month <= 13; month++ {
			for day := 0; day <= 32; day++ {
				v := fmt.Sprintf("%04d-%02d-%02d", year, month, day)
				_, perr := time.Parse("2006-01-02", v)
				m, _ := ecma.MatchString(v)
				if m != (perr == nil) || re2.MatchString(v) != (perr == nil) {
					t.Fatalf("%s: pattern %v/%v, calendar %v", v, m, re2.MatchString(v), perr == nil)
				}
			}
		}
	}
	for _, v := range []string{"2026-1-01", "2026-01-01T00:00:00Z", "20260101", ""} {
		if re2.MatchString(v) {
			t.Fatalf("%q taken", v)
		}
	}
}
