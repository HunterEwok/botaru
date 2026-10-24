// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"testing"
)

// SEC-13: privacy_notice is a code delivery of its own with notice_version, also on a channel
// with ai_disclosure: native, which gets no ai_notice; it goes out at the first contact and
// again at the next contact after notice_version grew, not at every contact.
func TestBackfillPrivacyNoticeVersion(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	edit := func(version int) func(string, []map[string]any) []map[string]any {
		return func(_ string, ms []map[string]any) []map[string]any {
			for _, m := range ms {
				s := m["spec"].(map[string]any)
				switch m["kind"] {
				case "Channel":
					s["ai_disclosure"] = "native"
				case "Tenant":
					s["privacy"].(map[string]any)["notice_version"] = version
				}
			}
			return ms
		}
	}
	b := newBench(t)
	if res, err := b.c.Apply(ctx, farmOf(b.op), ApplyRequest{Commit: "1300000000000000000000000000000000000003", Manifests: edit(3)("", helloManifests(t))}); err != nil || !res.Applied {
		t.Fatalf("apply %+v %v", res, err)
	}
	// A driver that reports the messenger's own bot marking.
	r, err := b.c.Enroll(ctx, b.op, EnrollRequest{Tenant: "demo", Kind: "channel", Target: "telegram"})
	if err != nil {
		t.Fatal(err)
	}
	w, err := b.c.AuthenticateWorker(ctx, r.Token)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.c.Hello(ctx, w, HelloRequest{Kind: "channel", ProtocolVersions: []string{"v1"}, Driver: &DriverCaps{AIDisclosure: "native"}}); err != nil {
		t.Fatal(err)
	}
	if b.channel, err = b.c.AuthenticateWorker(ctx, r.Token); err != nil {
		t.Fatal(err)
	}
	notices := func() map[string]int {
		t.Helper()
		rows, err := b.c.st.Pool.Query(ctx, `SELECT kind, coalesce(notice_version, ''), body->>'text_key', body->'params'->>'notice_version' FROM deliveries
			WHERE kind IN ('ai_notice', 'privacy_notice') AND target_id = 'direct:1001'`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string]int{}
		for rows.Next() {
			var kind, version, key, param string
			if err := rows.Scan(&kind, &version, &key, &param); err != nil {
				t.Fatal(err)
			}
			if kind == "privacy_notice" && (key != "notice.privacy" || param != version) {
				t.Errorf("SEC-13: privacy_notice %s with key %s and the parameter %s", version, key, param)
			}
			out[kind+":"+version]++
		}
		return out
	}
	b.hello(t, "1")
	if got := notices(); len(got) != 1 || got["privacy_notice:3"] != 1 {
		t.Fatalf("SEC-13: the first contact on a native channel %v", got)
	}
	b.hello(t, "2")
	if got := notices(); got["privacy_notice:3"] != 1 || len(got) != 1 {
		t.Fatalf("SEC-13: a contact without a new version %v", got)
	}
	if res, err := b.c.Apply(ctx, farmOf(b.op), ApplyRequest{Commit: "1300000000000000000000000000000000000013", Manifests: edit(4)("", helloManifests(t))}); err != nil || !res.Applied {
		t.Fatalf("apply %+v %v", res, err)
	}
	b.hello(t, "3")
	if got := notices(); got["privacy_notice:3"] != 1 || got["privacy_notice:4"] != 1 || len(got) != 2 {
		t.Fatalf("SEC-13: the contact after the version grew %v", got)
	}
}
