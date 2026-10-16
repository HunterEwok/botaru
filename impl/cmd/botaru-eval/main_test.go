// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestCommand: the dry plan of the full claim lists the 22 eval rules; a spend cap above
// budget.max_eur_per_run is refused (docs/quality.md, "Cost": a run can only lower it), and a real
// run without a provider key does not start (INJ-12 is measured only with a real model, the key
// never in a file of the repository).
func TestCommand(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run([]string{"-dry-plan", "-claim", "v0.3", "-profiles", "all"}, &out, &errb); code != 0 {
		t.Fatalf("dry plan: %d %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "Claimed rules with check eval: 22 (19 non-waivable)") {
		t.Fatalf("dry plan:\n%s", out.String())
	}
	errb.Reset()
	if code := run([]string{"-max-eur", "50", "-dsn", "postgres://127.0.0.1:1/x"}, &out, &errb); code != 2 || !strings.Contains(errb.String(), "max_eur_per_run") {
		t.Fatalf("cap above the threshold: %d %s", code, errb.String())
	}
	for _, args := range [][]string{{"-max-eur", "0"}, {"-max-eur", "NaN"}, {"-max-eur", "-1"}, {"-case", "no-such-case"}, {"-role", "no-such-role"}} {
		errb.Reset()
		if code := run(append(args, "-dsn", "postgres://127.0.0.1:1/x"), &out, &errb); code != 2 {
			t.Fatalf("%v: %d %s", args, code, errb.String())
		}
	}
	t.Setenv(keyEnv, "")
	errb.Reset()
	if code := run([]string{"-real", "-dsn", "postgres://127.0.0.1:1/x"}, &out, &errb); code != 2 || !strings.Contains(errb.String(), keyEnv) {
		t.Fatalf("real run without a key: %d %s", code, errb.String())
	}
}

// TestDSN: the run's database replaces the one of the DSN, also one given as a query parameter,
// and a printed DSN carries no password.
func TestDSN(t *testing.T) {
	for in, want := range map[string]string{
		"postgres://u:pw@h:5432/postgres?sslmode=disable&dbname=other": "postgres://u:pw@h:5432/run1?sslmode=disable",
		"postgresql://h/x?database=y":                                  "postgresql://h/run1",
		"host=h dbname=postgres user=u":                                "host=h user=u dbname=run1",
	} {
		if got := withDatabase(in, "run1"); got != want {
			t.Fatalf("withDatabase(%q) = %q, want %q", in, got, want)
		}
	}
	if got := redactDSN("postgres://u:secret@h/x"); strings.Contains(got, "secret") {
		t.Fatalf("redactDSN: %s", got)
	}
	if got := redactDSN("host=h password=secret"); strings.Contains(got, "secret") {
		t.Fatalf("redactDSN: %s", got)
	}
}
