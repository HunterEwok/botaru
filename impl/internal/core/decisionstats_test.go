// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"testing"
	"time"
)

// Tests of the decision statistics of people and the trial figures of My outcomes.

// A person's "Accept" writes the decision statistics point of the author role with the category
// as its second dimension, and the statistics by role and category count it (DIG-4); a row of
// "My outcomes" with a trial outcome carries the trial figures (DIG-8).
func TestDecisionStatsAndTrialFigures(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newCommissionBench(t, true, presentEdit)
	id, _ := acceptLimits(t, b)
	var enum, dim, subject string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT enum, dim, subject FROM obs_points WHERE series = $1`, seriesAcceptShare).Scan(&enum, &dim, &subject); err != nil {
		t.Fatal(err)
	}
	if enum != "accepted" || dim != "reliability" || subject != "role:hello" {
		t.Fatalf("point %s %s %s", enum, dim, subject)
	}
	tx, err := b.c.st.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	stats, err := decisionStats(ctx, tx, time.Now(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if len(stats) != 1 || stats[0].(map[string]any)["accept_share"] != 1.0 || stats[0].(map[string]any)["decided"] != 1 {
		t.Fatalf("stats %v", stats)
	}
	// The trial ended: the row shows the figures before and after and the broken guard.
	if _, err := tx.Exec(ctx, `UPDATE proposals SET record = jsonb_set(jsonb_set(jsonb_set(record, '{trial,outcome}', '"worse"'),
		'{trial,after}', '{"value": 0.5, "samples": 20, "without_author": {"value": 0.4, "samples": 10}, "without_role": {"value": null, "samples": 0}}'),
		'{trial,before}', '{"value": 0.75, "samples": 20, "without_author": {"value": 0.5, "samples": 10}, "without_role": {"value": null, "samples": 0}}') WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	var worker string
	if err := tx.QueryRow(ctx, `SELECT record->'origin'->>'worker' FROM proposals WHERE id = $1`, id).Scan(&worker); err != nil {
		t.Fatal(err)
	}
	mo, err := myOutcomes(ctx, tx, "demo", worker, myOutcomesRows)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.c.spec.Validate("urn:botaru:schema:insight-my-results:1", mo); err != nil {
		t.Fatalf("my outcomes %v: %v", mo, err)
	}
	row := mo["rows"].([]any)[0].(map[string]any)
	tn, _ := row["trial_numbers"].(map[string]any)
	if row["id"] != id || tn["before"] != 0.75 || tn["after"] != 0.5 || tn["change_all"] != -0.25 || tn["change_without_author"] != -0.1 || tn["change_without_role"] != nil {
		t.Fatalf("row %v", row)
	}
}
