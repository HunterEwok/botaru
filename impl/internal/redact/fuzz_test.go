// SPDX-License-Identifier: AGPL-3.0-only

package redact

import (
	"strings"
	"testing"
)

// Fuzzing the masker: whatever text comes before or after it, a password on its own line
// never survives, and masking twice gives the same text as masking once.
func FuzzSecrets(f *testing.F) {
	for _, s := range []string{"", "token: 42", "password=\"a\nb", "Authorization: Bearer x", "x=1 pass=2", "https://" + "u:pw" + "@example.org/x"} {
		f.Add(s, s)
	}
	const secret = "Zq7x" + "V9pL2mK4wR8t"
	f.Fuzz(func(t *testing.T, before, after string) {
		once := Secrets(before)
		if twice := Secrets(once); twice != once {
			t.Fatalf("not stable: %q -> %q -> %q", before, once, twice)
		}
		in := before + "\npassword=" + secret + "\n" + after
		if out := Secrets(in); strings.Contains(out, secret) && !strings.Contains(before+after, secret) {
			t.Fatalf("%q kept the password: %q", in, out)
		}
	})
}
