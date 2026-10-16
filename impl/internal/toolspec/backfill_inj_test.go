// SPDX-License-Identifier: AGPL-3.0-only

package toolspec

import "testing"

// INJ-3: access sets the read-only annotation of the live tools/list, and an annotation that
// contradicts the manifest (readOnlyHint: false on a read tool) changes the fingerprint, so a
// plugin that reports it does not match the manifest the core expects.
func TestBackfillAnnotationFromManifest(t *testing.T) {
	f := false
	p := Plugin{Name: "checks", Placement: "host", Tools: []Tool{
		{Name: "latest", Access: "read", Target: "any", UntrustedOutput: &f},
		{Name: "restart", Access: "write", Target: "any", UntrustedOutput: &f},
	}}
	live := p.LiveTools()
	hint := map[string]bool{}
	for _, x := range live {
		m := x.(map[string]any)
		hint[m["name"].(string)] = m["annotations"].(map[string]any)["readOnlyHint"].(bool)
	}
	if !hint["latest"] || hint["restart"] {
		t.Fatalf("INJ-3: readOnlyHint does not follow access: %v", hint)
	}
	want, err := Fingerprint(live)
	if err != nil {
		t.Fatal(err)
	}
	lying := p.LiveTools()
	lying[0].(map[string]any)["annotations"] = map[string]any{"readOnlyHint": false}
	got, err := Fingerprint(lying)
	if err != nil {
		t.Fatal(err)
	}
	if got == want {
		t.Fatal("INJ-3: an annotation contradicting the manifest keeps the fingerprint")
	}
	// The untrusted flag defaults to true: only an explicit false with a closed output trusts.
	if (Tool{}).Untrusted() != true || (Tool{UntrustedOutput: &f}).TrustedOutput() {
		t.Fatal("INJ-3: output without a closed schema is trusted")
	}
}
