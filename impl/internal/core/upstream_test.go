// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"testing"
	"time"
)

// Tests of the upstream health that doctor shows (OBS-10).

// A channel's last successful poll and P4's provider pair state show in doctor (OBS-10).
func TestDoctorUpstream(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newBench(t)
	at := b.now(t).Add(-time.Minute).UTC().Format(time.RFC3339Nano)
	if _, err := b.c.Hello(ctx, b.channel, HelloRequest{Kind: "channel", ProtocolVersions: []string{"v1"},
		Health: []ComponentHealth{{Component: "channel_upstream", Name: "telegram", State: "ok", Time: at}}}); err != nil {
		t.Fatal(err)
	}
	b.exec(t, `INSERT INTO provider_pairs (tenant, pool, state, updated_at) VALUES ('demo', 'family-a', 'open', now())`)
	doc, err := b.c.Doctor(ctx, b.op, TenantRequest{})
	if err != nil {
		t.Fatal(err)
	}
	checks := map[string]DoctorCheck{}
	for _, ch := range doc.Checks {
		checks[ch.ID] = ch
	}
	if ch, ok := checks["channel.last_poll_age_s.telegram"]; !ok || !ch.OK || ch.Value.(int64) < 50 || ch.Value.(int64) > 120 {
		t.Fatalf("poll age %+v", ch)
	}
	if ch, ok := checks["provider.state.family_a"]; !ok || ch.OK || ch.Value != "open" || ch.Severity != "warn" {
		t.Fatalf("provider state %+v", ch)
	}
	if ch := checks["deploy.description"]; ch.Severity != "warn" {
		t.Fatalf("no deployment description %+v", ch)
	}
}
