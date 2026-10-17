// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"strings"
	"testing"

	"github.com/HunterEwok/botaru/impl/internal/ids"
	"github.com/HunterEwok/botaru/impl/internal/mcp"
)

// Tests of the tool output cache (PRM-12).

// A read tool with cache_s answers the same call from the cache after the full check, with the
// flags of the first call, untrusted_seen and the flag cached on the call, audit and usage; a
// no_tool hold and an erasure request of the tenant purge the cache (PRM-12).
func TestToolOutputCache(t *testing.T) {
	ctx := context.Background()
	b := newToolBenchWith(t, nil, func(ms []map[string]any) []map[string]any {
		for _, m := range ms {
			if m["kind"] == "ToolPlugin" && m["metadata"].(map[string]any)["name"] == "checks" {
				m["spec"].(map[string]any)["tools"].([]any)[0].(map[string]any)["cache_s"] = 60
			}
		}
		return ms
	})
	th := b.toolhost(t, "checks")
	args := map[string]any{"check": "www-health"}
	out := map[string]any{"content": []any{}, "structuredContent": map[string]any{"check": "www-health", "ok": 3, "total": 3}}
	viaHost := func(h string) {
		t.Helper()
		done := callAsync(b, h, mcp.CallParams{Name: "checks_latest", Arguments: args})
		item := b.claimTool(t, th)
		if _, err := b.c.Complete(ctx, th, header(t, item), CompleteRequest{Output: out}); err != nil {
			t.Fatal(err)
		}
		if res := <-done; res.err != nil || !strings.Contains(callText(res.r), `"ok":3`) {
			t.Fatalf("call %+v %v", res.r, res.err)
		}
	}
	cached := func(h string) bool {
		t.Helper()
		r, err := b.c.P3ToolsCall(ctx, h, mcp.CallParams{Name: "checks_latest", Arguments: args})
		if err != nil {
			t.Fatal(err)
		}
		var c bool
		if err := b.c.st.Pool.QueryRow(ctx, `SELECT cached FROM toolcalls ORDER BY created_at DESC, id DESC LIMIT 1`).Scan(&c); err != nil {
			t.Fatal(err)
		}
		if c && !strings.Contains(callText(r), `"ok":3`) {
			t.Fatalf("cached answer %+v", r)
		}
		return c
	}
	att, h := b.attemptTools(t, "1", false)
	other, absent := ids.PrincipalID(), ids.PrincipalID()
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE tasks SET record = record || jsonb_build_object('personal_subjects', jsonb_build_array($2::text, $3::text)) WHERE id = $1`, att.WorkID, other, absent); err != nil {
		t.Fatal(err)
	}
	out["content"] = []any{map[string]any{"type": "text", "text": "checked by " + other}}
	viaHost(h)
	// The first call's flags are kept with the entry: of the task's personal_subjects only the
	// one the output names.
	var subjects string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT personal_subjects::text FROM tool_cache`).Scan(&subjects); err != nil || !strings.Contains(subjects, other) || strings.Contains(subjects, absent) {
		t.Fatalf("cached personal_subjects %q %v", subjects, err)
	}
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE tool_cache SET personal_seen = '["masking"]'`); err != nil {
		t.Fatal(err)
	}
	if !cached(h) {
		t.Fatal("the second call did not come from the cache")
	}
	var rec string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT record::text FROM tasks WHERE id = $1`, att.WorkID).Scan(&rec); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rec, `"untrusted_seen": true`) || !strings.Contains(rec, `"masking"`) {
		t.Fatalf("task record %s", rec)
	}
	var audits, lines int
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM audit WHERE record->'after'->>'cached' = 'true'`).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM spend WHERE cached`).Scan(&lines); err != nil {
		t.Fatal(err)
	}
	if audits != 1 || lines != 1 {
		t.Fatalf("cached audit %d, usage %d", audits, lines)
	}
	// A no_tool hold on the plugin purges the entry; after its release the call reaches the
	// toolhost again.
	hold, err := b.c.HoldSet(ctx, OperatorActor(b.op), HoldSetRequest{Op: "hold.set", Scope: HoldScope{Kind: "plugin", Tenant: "demo", Name: "checks"}, Effects: []string{"no_tool"}, Reason: "maintenance"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.c.HoldRelease(ctx, OperatorActor(b.op), HoldReleaseRequest{Op: "hold.release", ID: hold.ID}); err != nil {
		t.Fatal(err)
	}
	viaHost(h)
	if !cached(h) {
		t.Fatal("the refilled entry was not used")
	}
	if _, err := b.c.ErasureRequest(ctx, b.op, ErasureRequestInput{Scope: ErasureScope{Tenant: "demo", Subject: b.alice}, Reason: "subject_request"}); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM tool_cache`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("cache after the erasure request: %d %v", n, err)
	}
}
