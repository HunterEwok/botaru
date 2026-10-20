// SPDX-License-Identifier: AGPL-3.0-only

package testbench

import (
	"reflect"
	"testing"

	"github.com/HunterEwok/botaru/impl/internal/spec"
)

// The shared manifests come back as deep copies: a change to one copy reaches neither the
// cache nor the next copy, and no map or list of a kind DeepCopy does not know is left shared.
func TestSharedHelloBotManifestsCopy(t *testing.T) {
	root, err := spec.FindRoot("")
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := HelloBotManifests(root)
	if err != nil {
		t.Fatal(err)
	}
	a, err := SharedHelloBotManifests(root)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a, fresh) {
		t.Fatal("the shared manifests differ from a fresh read")
	}
	var walk func(v any, at string)
	walk = func(v any, at string) {
		switch x := v.(type) {
		case map[string]any:
			for k, e := range x {
				walk(e, at+"/"+k)
			}
			x["changed"] = true
		case []any:
			for _, e := range x {
				walk(e, at+"/-")
			}
		default:
			if k := reflect.ValueOf(v).Kind(); k == reflect.Map || k == reflect.Slice || k == reflect.Pointer {
				t.Fatalf("%s: %T is not copied by DeepCopy", at, v)
			}
		}
	}
	for _, m := range a {
		walk(m, "")
	}
	b, err := SharedHelloBotManifests(root)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(b, fresh) {
		t.Fatal("a change to one copy reached the next one")
	}

	p, err := SharedPersonas(root)
	if err != nil {
		t.Fatal(err)
	}
	for k := range p {
		delete(p, k)
	}
	q, err := SharedPersonas(root)
	if err != nil || len(q) == 0 {
		t.Fatalf("personas after a change to a copy: %d %v", len(q), err)
	}
}
