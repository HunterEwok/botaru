// SPDX-License-Identifier: AGPL-3.0-only

package version

import "testing"

func TestString(t *testing.T) {
	if got, want := String("botaru-core"), "botaru-core dev"; got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
}
