// SPDX-License-Identifier: AGPL-3.0-only

package texts

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HunterEwok/botaru/impl/internal/spec"
)

// The texts come from the skeleton message catalog; an installation locale replaces keys one by
// one, and a key it lacks takes the en text (DLV-9, fixture:channel/locale-en).
func TestRender(t *testing.T) {
	root, err := spec.FindRoot("")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "et.yaml"), []byte("locale: et\ntexts:\n  notice.ai: X {tenant}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(root, dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Render("et", "notice.ai", map[string]any{"tenant": "demo"}); got != "X demo" {
		t.Fatalf("got %q", got)
	}
	if got := c.Render("et", "notice.privacy", map[string]any{"notice_version": 3, "notice_ref": "u"}); !strings.Contains(got, "Full text: u (version 3)") {
		t.Fatalf("fallback: %q", got)
	}
	if got := c.Render("en", "notice.ai", map[string]any{"tenant": "demo", "operator": "op"}); !strings.HasPrefix(got, "This is the bot of tenant demo.") {
		t.Fatalf("en: %q", got)
	}
	if got := c.Render("en", "notice.ai", map[string]any{"tenant": "demo"}); strings.Contains(got, "{") || !strings.Contains(got, "operator -,") {
		t.Fatalf("a missing parameter: %q", got)
	}
	if got := c.Render("en", "notice.ai", map[string]any{"tenant": "demo", "operator": "{tenant}"}); !strings.Contains(got, "operator {tenant}") {
		t.Fatalf("a value is not filled again: %q", got)
	}
	if got := c.Render("en", "no.such.key", nil); got != "no.such.key" {
		t.Fatalf("got %q", got)
	}
	if c.Has("number.words") {
		t.Fatal("a list became a text")
	}
	if w := c.List("xx", "number.words"); len(w) < 21 || w[0] != "zero" || w[20] != "twenty" {
		t.Fatalf("the number words %v", w)
	}
	// Every key the code of this implementation renders is in the skeleton catalog.
	for _, k := range []string{"notice.ai", "notice.privacy", "notice.principal_left", "notice.principal_left.no_chats",
		"provenance.model", "task.rejected", "task.failed", "activity.not_command", "activity.unknown_command", "activity.not_verified"} {
		if !c.Has(k) {
			t.Errorf("the catalog has no %s", k)
		}
	}
	if _, err := Load(t.TempDir(), ""); err == nil {
		t.Fatal("a root without the en catalog loaded")
	}
}
