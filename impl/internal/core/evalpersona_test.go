// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"fmt"
	"testing"

	"github.com/HunterEwok/botaru/impl/internal/manifest"
)

// The static fingerprint of the seat holder carries the persona it acts with (PRV-3): the
// commission seat role of the reviewer profile takes the commission persona with the lens
// personas of the charter's seats, so a changed lens persona changes its fingerprint; a role
// that holds no seat, or a seat role of another profile, keeps the persona of its position.
func TestSeatHolderPersona(t *testing.T) {
	lens := func(sha string) map[string]any {
		return map[string]any{"persona": map[string]any{"ref": "personas/lens-security.md", "sha256": sha}}
	}
	gov := func(lensSHA, seat string) map[string]any {
		return map[string]any{"apiVersion": "botaru/v1alpha1", "kind": "Governance", "metadata": map[string]any{"name": "demo", "tenant": "demo"},
			"spec": map[string]any{
				"positions": map[string]any{
					"functional": map[string]any{"persona": map[string]any{"ref": "personas/functional.md", "sha256": "11"}},
					"commission": map[string]any{"persona": map[string]any{"ref": "personas/reviewer.md", "sha256": "22"}},
				},
				"commission": map[string]any{"seat_role": seat, "seats": []any{map[string]any{"id": 1, "lens": "security", "fallback_role": "other"}},
					"lenses": map[string]any{"security": lens(lensSHA)}},
			}}
	}
	role := func(name string) map[string]any {
		profile := "worker"
		if name == "member" {
			profile = "reviewer"
		}
		return map[string]any{"apiVersion": "botaru/v1alpha1", "kind": "Role", "metadata": map[string]any{"name": name},
			"spec": map[string]any{"profile": profile, "worker_kind": "agent", "claims": []any{}, "models": map[string]any{"base": "base-std"}}}
	}
	indexOf := func(lensSHA, seat string) evalIndex {
		var docs []manifest.Doc
		for i, m := range []map[string]any{gov(lensSHA, seat), role("member"), role("other")} {
			docs = append(docs, manifest.FromValue(m, fmt.Sprintf("manifests/%d", i)))
		}
		return newEvalIndex(docs)
	}
	index := func(lensSHA string) evalIndex { return indexOf(lensSHA, "member") }
	a, b := index("aa"), index("bb")
	if p := a.positionPersona("demo", "other", "functional"); p.SHA256 != "11" {
		t.Fatalf("a role without a seat: %+v", p)
	}
	if p := a.positionPersona("demo", "member", "commission"); p.SHA256 == "11" || p.SHA256 == "22" || p.Ref != "personas/reviewer.md" {
		t.Fatalf("the seat holder: %+v", p)
	}
	fp := func(x evalIndex, r string) string {
		t.Helper()
		v, ok, err := x.staticFingerprint(r, "demo")
		if err != nil || !ok {
			t.Fatal(err)
		}
		return v
	}
	if fp(a, "member") == fp(b, "member") {
		t.Fatal("a changed lens persona kept the seat holder's fingerprint")
	}
	if fp(a, "other") != fp(b, "other") {
		t.Fatal("a changed lens persona moved the fingerprint of a role without a seat")
	}
	// A seat role of another profile keeps its position persona.
	if p := indexOf("aa", "other").positionPersona("demo", "other", "functional"); p.SHA256 != "11" {
		t.Fatalf("a worker role: %+v", p)
	}
	// Without a pinned lens persona the part is the commission persona alone.
	if p := index("").positionPersona("demo", "member", "commission"); p.SHA256 != "22" {
		t.Fatalf("no lens persona: %+v", p)
	}
}
