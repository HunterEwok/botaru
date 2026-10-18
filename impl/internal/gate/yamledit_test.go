// SPDX-License-Identifier: AGPL-3.0-only

package gate

import (
	"errors"
	"testing"
)

func TestSetField(t *testing.T) {
	src := "# head\napiVersion: botaru/v1alpha1\nkind: X\nspec:\n  a: \"x\"   # keep\n  b: 'it''s'\n  c: 5\n  list:\n    - t: {u: 1}\n      v: 2m\n  flow: {k: 1}\n  block: |\n    text\n---\nkind: Y\nspec:\n  c: 1\n"
	for _, tc := range []struct {
		doc   int
		field string
		value any
		want  string
	}{
		{0, "spec.a", "y z", "# head\napiVersion: botaru/v1alpha1\nkind: X\nspec:\n  a: \"y z\"   # keep\n  b: 'it''s'\n  c: 5\n  list:\n    - t: {u: 1}\n      v: 2m\n  flow: {k: 1}\n  block: |\n    text\n---\nkind: Y\nspec:\n  c: 1\n"},
		{0, "spec.b", "o'k", "# head\napiVersion: botaru/v1alpha1\nkind: X\nspec:\n  a: \"x\"   # keep\n  b: 'o''k'\n  c: 5\n  list:\n    - t: {u: 1}\n      v: 2m\n  flow: {k: 1}\n  block: |\n    text\n---\nkind: Y\nspec:\n  c: 1\n"},
		{0, "spec.c", 7.0, "# head\napiVersion: botaru/v1alpha1\nkind: X\nspec:\n  a: \"x\"   # keep\n  b: 'it''s'\n  c: 7\n  list:\n    - t: {u: 1}\n      v: 2m\n  flow: {k: 1}\n  block: |\n    text\n---\nkind: Y\nspec:\n  c: 1\n"},
		{0, "spec.list[].v", "3m", "# head\napiVersion: botaru/v1alpha1\nkind: X\nspec:\n  a: \"x\"   # keep\n  b: 'it''s'\n  c: 5\n  list:\n    - t: {u: 1}\n      v: 3m\n  flow: {k: 1}\n  block: |\n    text\n---\nkind: Y\nspec:\n  c: 1\n"},
		{0, "spec.new.leaf", "on", "# head\napiVersion: botaru/v1alpha1\nkind: X\nspec:\n  new:\n    leaf: \"on\"\n  a: \"x\"   # keep\n  b: 'it''s'\n  c: 5\n  list:\n    - t: {u: 1}\n      v: 2m\n  flow: {k: 1}\n  block: |\n    text\n---\nkind: Y\nspec:\n  c: 1\n"},
		{1, "spec.c", 2, "# head\napiVersion: botaru/v1alpha1\nkind: X\nspec:\n  a: \"x\"   # keep\n  b: 'it''s'\n  c: 5\n  list:\n    - t: {u: 1}\n      v: 2m\n  flow: {k: 1}\n  block: |\n    text\n---\nkind: Y\nspec:\n  c: 2\n"},
	} {
		got, err := setField([]byte(src), tc.doc, tc.field, tc.value)
		if err != nil {
			t.Fatalf("%s: %v", tc.field, err)
		}
		if string(got) != tc.want {
			t.Fatalf("%s:\n%s\nwant\n%s", tc.field, got, tc.want)
		}
	}
	for _, field := range []string{"spec.flow.k", "spec.block", "spec.list[1].v", "spec.list", "spec.list[].t.u"} {
		if _, err := setField([]byte(src), 0, field, "1"); !errors.Is(err, errUnrenderable) {
			t.Fatalf("%s: %v", field, err)
		}
	}
	if _, err := setField([]byte(src), 0, "spec.c", map[string]any{"x": 1}); !errors.Is(err, errUnrenderable) {
		t.Fatalf("a map value: %v", err)
	}
}
