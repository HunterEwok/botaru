// SPDX-License-Identifier: AGPL-3.0-only

package kbjob

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// The cut of kb.ingest.v1 (KNW-4, D-97): paragraphs joined up to the target, headings as
// titles, numbers in document order, the limits of p1-kb-index-put-request and the skip codes
// of the type's result.
func TestCut(t *testing.T) {
	frags, skipped := Cut([]byte("# One\n\nalpha\n\nbeta\n## Two\ngamma"))
	if skipped != "" || len(frags) != 2 || frags[0].Title != "One" || frags[0].Text != "alpha\n\nbeta" || frags[1].Title != "Two" || frags[1].N != 1 {
		t.Fatalf("cut %+v %q", frags, skipped)
	}
	for _, c := range []struct {
		in, want string
	}{{"\xff\xfe", "unreadable"}, {"a\x00b", "unreadable"}, {" \n\n ", "empty"}, {"# Only a title", "empty"}} {
		if f, s := Cut([]byte(c.in)); s != c.want || f != nil {
			t.Errorf("%q: %q %v", c.in, s, f)
		}
	}
	big, _ := Cut([]byte(strings.Repeat("word ", 3000)))
	if len(big) < 10 {
		t.Fatalf("a long paragraph gave %d fragments", len(big))
	}
	for i, f := range big {
		if n := utf8.RuneCountInString(f.Text); n > maxRunes || f.N != i {
			t.Fatalf("fragment %d: %d characters, n %d", i, n, f.N)
		}
	}
	// A run without spaces is cut hard at the limit.
	hard, _ := Cut([]byte(strings.Repeat("x", 9000)))
	for _, f := range hard {
		if utf8.RuneCountInString(f.Text) > maxRunes {
			t.Fatal("a fragment above 4000 characters")
		}
	}
	if _, s := Cut([]byte(strings.Repeat("p\n\n", 2*targetRunes) + strings.Repeat("# h\n\nx\n\n", maxFragments+1))); s != "too_large" {
		t.Fatalf("too many fragments: %q", s)
	}
	long := strings.Repeat("t", 300)
	if f, _ := Cut([]byte("# " + long + "\n\nbody")); utf8.RuneCountInString(f[0].Title) != maxTitle {
		t.Fatalf("title of %d characters", utf8.RuneCountInString(f[0].Title))
	}
}
