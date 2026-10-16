// SPDX-License-Identifier: AGPL-3.0-only

package e2e

import (
	"strings"
	"testing"

	"github.com/HunterEwok/botaru/impl/internal/testbench"
)

// A channel performs no operator operations: neither its worker token nor the P2 secret of its
// driver opens an operator operation on P2, a worker token is not accepted on P2 at all, and P2
// listens apart from P1, which serves no P2 path (IDN-7, adapter:channel/no-operator-ops).
func TestBackfillChannelNoOperatorOps(t *testing.T) {
	r := newRig(t)
	ms, err := testbench.HelloBotManifests(r.root)
	if err != nil {
		t.Fatal(err)
	}
	var applied struct {
		Applied bool `json:"applied"`
	}
	r.p2call(t, r.opToken, "apply", map[string]any{"commit": strings.Repeat("a", 40), "manifests": ms}, &applied)
	if !applied.Applied {
		t.Fatal("apply refused")
	}
	var ch struct {
		Token   string `json:"token"`
		P2Token string `json:"p2_token"`
	}
	r.p2call(t, r.opToken, "worker.enroll", map[string]any{"tenant": "demo", "role": "telegram", "kind": "channel", "target": "telegram"}, &ch)
	if ch.Token == "" || ch.P2Token == "" {
		t.Fatalf("enroll %+v", ch)
	}
	ops := map[string]map[string]any{
		"apply":         {"commit": strings.Repeat("b", 40), "manifests": ms},
		"worker.enroll": {"tenant": "demo", "role": "telegram", "kind": "channel", "target": "telegram"},
		"hold.set":      {"scope": map[string]any{"kind": "tenant", "tenant": "demo"}, "effects": []any{"no_claim"}, "reason": "manual"},
		"grant":         {"op": "grant", "subject": "human:p_x", "group": "hello"},
		"status":        {},
	}
	for op, body := range ops {
		// IDN-7: the channel's credentials get unauthenticated on every operator operation.
		for name, token := range map[string]string{"worker token": ch.Token, "driver secret": ch.P2Token} {
			if code := r.p2call(t, token, op, body, nil); code != 401 {
				t.Errorf("%s of the channel on %s: %d", name, op, code)
			}
		}
		// IDN-7: P1 serves no P2 path, even with the operator credential.
		if code, _ := rawPost(t, r.p1.URL+"/v1/p2/"+op, r.opToken, "", "{}"); code != 404 {
			t.Errorf("P1 served /v1/p2/%s: %d", op, code)
		}
	}
	// The channel's worker token still works on P1.
	if code, _ := rawPost(t, r.p1.URL+"/v1/p1/hello", ch.Token, "", `{"worker_kind": "channel"}`); code == 401 {
		t.Fatal("the channel's worker token is refused on P1")
	}
}
