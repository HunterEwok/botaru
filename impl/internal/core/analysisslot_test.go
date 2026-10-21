// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/jcs"
	"github.com/HunterEwok/botaru/impl/internal/manifest"
	"github.com/HunterEwok/botaru/impl/internal/spec"
)

// newAnalysisSlotBench is the commission bench whose hello role runs its analysis on the
// example profile analysis-std (rank 2) and its other work on base-std (rank 1); edits change
// the manifests before apply.
func newAnalysisSlotBench(t *testing.T, edits ...func([]map[string]any)) *bench {
	t.Helper()
	root, err := spec.FindRoot("")
	if err != nil {
		t.Fatal(err)
	}
	docs, err := manifest.ReadFiles([]string{filepath.Join(root, "examples/model-profiles/analysis-std.yaml")})
	if err != nil {
		t.Fatal(err)
	}
	return newCommissionBenchWith(t, false, []map[string]any{docs[0].Value}, append([]func([]map[string]any){func(ms []map[string]any) {
		specOf(ms, "Role", "hello")["models"].(map[string]any)["analysis"] = "analysis-std"
		if sp := specOf(ms, "Tenant", "demo"); sp != nil {
			if allowed, ok := sp["models_allowed"].([]any); ok && !slices.Contains(allowed, any("analysis-std")) {
				sp["models_allowed"] = append(allowed, "analysis-std")
			}
		}
	}}, edits...)...)
}

// slotOf resolves the model handle of an attempt and returns its slot and profile.
func slotOf(t *testing.T, b *bench, att *WorkItem) (string, string, error) {
	t.Helper()
	g, err := b.c.ResolveModelHandle(context.Background(), att.Payload["handles"].(map[string]any)["model"].(string))
	if err != nil {
		return "", "", err
	}
	return g.Slot, g.ProfileName, nil
}

// An analysis of the role's own results runs on the role's analysis slot, the slot the apply
// lint prices its tree budget by; the other work of the role keeps the main slot (PRF-1). The
// run fingerprint names the profile the attempt runs on.
func TestAnalysisSlot(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newAnalysisSlotBench(t)
	_, att := b.helloAttempt(t, "9")
	slot, profile, err := slotOf(t, b, att)
	if err != nil || slot != "base" || profile != "base-std" {
		t.Fatalf("hello attempt on %s/%s: %v", slot, profile, err)
	}
	if _, err := b.c.Complete(ctx, b.agent, header(t, att), CompleteRequest{Result: map[string]any{"answer": "hi"}}); err != nil {
		t.Fatal(err)
	}
	_, an := b.analysisAttempt(t, "1")
	slot, profile, err = slotOf(t, b, an)
	if err != nil || slot != "analysis" || profile != "analysis-std" {
		t.Fatalf("analysis attempt on %s/%s: %v", slot, profile, err)
	}
	var fp string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT attempt->'parts'->>'model' FROM leases WHERE work_id = $1 AND lease_no = $2`,
		an.Token.WorkID, an.Token.LeaseNo).Scan(&fp); err != nil {
		t.Fatal(err)
	}
	if want, err := jcs.Hash(map[string]any{"slot": "analysis", "profile": "analysis-std"}); err != nil || fp != want {
		t.Errorf("the model part of the run fingerprint %q, want %q (%v)", fp, want, err)
	}
	// The role's own fingerprint and the eval gate still read its main slot.
	cat, err := b.c.Catalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if s, p := mainSlot(cat.Roles["hello"]); s != "base" || p != "base-std" {
		t.Errorf("main slot %s/%s", s, p)
	}
}

// A call of the analysis slot is held to the analysis ceiling of the position, not to the base
// one: the functionary may analyse on rank 2 above its base ceiling of 1, and a charter that
// lowers the analysis ceiling to 1 refuses the call with rank_exceeded (GW-7, GOV-3).
func TestAnalysisSlotCeiling(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newAnalysisSlotBench(t, func(ms []map[string]any) {
		specOf(ms, "Governance", "demo")["positions"].(map[string]any)["functional"].(map[string]any)["analysis_max_rank"] = 1
	})
	_, att := b.helloAttempt(t, "9")
	if _, _, err := slotOf(t, b, att); err != nil {
		t.Fatalf("base call %v", err)
	}
	if _, err := b.c.Complete(ctx, b.agent, header(t, att), CompleteRequest{Result: map[string]any{"answer": "hi"}}); err != nil {
		t.Fatal(err)
	}
	_, an := b.analysisAttempt(t, "1")
	_, _, err := slotOf(t, b, an)
	wantReason(t, err, "rank_exceeded")
}

// An analysis waits for an open circuit of its own pair without an attempt, and the role's other
// work, on the main pair, still runs (D18, GW-10). The analysis profile gets a pool of its own here;
// the example profiles share one.
func TestAnalysisSlotPairWaits(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newAnalysisSlotBench(t, func(ms []map[string]any) {
		specOf(ms, "ModelProfile", "analysis-std")["pool"] = "family-c"
	})
	cat, err := b.c.Catalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	p, ok := cat.Profile("demo", "analysis-std")
	if !ok {
		t.Fatal("no analysis-std")
	}
	if _, err := b.c.st.Pool.Exec(ctx, `INSERT INTO provider_pairs (tenant, pool, state, until, updated_at) VALUES ('demo', $1, 'open', now() + interval '1 day', now())`,
		pairPool("analysis-std", p)); err != nil {
		t.Fatal(err)
	}
	var task string
	err = b.c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		res, err := b.c.enqueue(ctx, tx, now, cat, enqueueInput{Tenant: "demo", Type: "hello.insight.v1", Body: map[string]any{"type": "hello.insight.v1", "goal": "look"},
			IdempotencyKey: "system:schedule:insight:1", Initiator: "system", EnqueuedBy: "schedule:insight", Source: "schedule", SourceID: "insight"})
		if err != nil {
			return err
		}
		if res.Rejected != nil {
			return res.Rejected
		}
		task = res.TaskID
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if att := b.claim(t, b.agent, "attempt"); att != nil {
		t.Fatal("the analysis got an attempt while its pair waits")
	}
	var state, reason string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT state, coalesce(reason, '') FROM tasks WHERE id = $1`, task).Scan(&state, &reason); err != nil {
		t.Fatal(err)
	}
	if state != "submitted" || reason != "provider_unavailable" {
		t.Fatalf("analysis %s %s", state, reason)
	}
	if _, att := b.helloAttempt(t, "9"); att == nil {
		t.Fatal("no hello attempt")
	}
	// Once the pair closes, the analysis runs on its slot.
	if _, err := b.c.st.Pool.Exec(ctx, `DELETE FROM provider_pairs WHERE tenant = 'demo'`); err != nil {
		t.Fatal(err)
	}
	an := b.claim(t, b.agent, "attempt")
	if an == nil || an.Token.WorkID != task {
		t.Fatalf("the analysis got no attempt after the pair closed: %+v", an)
	}
	if slot, _, err := slotOf(t, b, an); err != nil || slot != "analysis" {
		t.Fatalf("analysis attempt on %s: %v", slot, err)
	}
}
