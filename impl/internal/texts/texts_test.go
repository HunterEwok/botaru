// SPDX-License-Identifier: AGPL-3.0-only

package texts

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRender(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "xx.yaml"), []byte("locale: xx\ntexts:\n  ai_notice: X {a}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Render("xx", "ai_notice", map[string]any{"a": 1}); got != "X 1" {
		t.Fatalf("got %q", got)
	}
	if got := c.Render("xx", "privacy_notice", map[string]any{"notice_version": 3, "notice_ref": "u"}); !strings.Contains(got, "version 3: u") {
		t.Fatalf("fallback: %q", got)
	}
	if got := c.Render("en", "no.such.key", nil); got != "no.such.key" {
		t.Fatalf("got %q", got)
	}
}
