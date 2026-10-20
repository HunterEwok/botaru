// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"testing"
)

// MEM-9: the seal does not extend retention: the identity loop enqueues the erasure request
// with reason bot_retired and the identity as its scope at the end of seal_ttl (30 days), not
// before, and only once.
func TestBackfillSealEndsInErasure(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newBench(t)
	old := b.botOf(t, b.agent.ID)
	if _, err := b.c.BotRetire(ctx, b.op, BotRetireRequest{Bot: old.ID}); err != nil {
		t.Fatal(err)
	}
	requests := func() int {
		t.Helper()
		var n int
		if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM erasure_requests WHERE reason = 'bot_retired' AND scope->>'bot' = $1 AND scope->>'tenant' = 'demo'`, old.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	b.advance(t, 29*24*3600)
	b.loop(t, "identity")
	if n := requests(); n != 0 {
		t.Fatalf("MEM-9: %d erasure requests a day before the end of the seal", n)
	}
	b.advance(t, 2*24*3600)
	b.loop(t, "identity")
	b.loop(t, "identity")
	if n := requests(); n != 1 {
		t.Fatalf("MEM-9: bot_retired requests at the end of the seal: %d", n)
	}
}
