// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/ids"
	"github.com/HunterEwok/botaru/impl/internal/manifest"
)

// A bot has one open proposal on top of the role limits: the second body of the same analysis
// is refused proposal_rate_limited; a proposal parked as conflict behind another author's
// change does not take the slot (PRP-16).
func TestProposalOnePerBot(t *testing.T) {
	ctx := context.Background()
	b := newInsightBench(t, map[string]any{"max_open_proposals": 5})
	b.helloDone(t, "1", "", "hi")
	b.loop(t, "observe")
	if _, err := b.analysis(t, "1", false, map[string]any{"proposals": []any{noneBody("one", 1), noneBody("two", 1)}}); err != nil {
		t.Fatal(err)
	}
	if n := len(proposals(t, b)); n != 1 {
		t.Fatalf("records %d", n)
	}
	if got := rejections(t, b); len(got) != 1 || got[0] != "proposal_rate_limited" {
		t.Fatalf("rejections %v", got)
	}
	// The open one waits behind another author's change: the bot may propose again.
	holder := ids.New(ids.Proposal)
	if _, err := b.c.st.Pool.Exec(ctx, `INSERT INTO proposals (id, tenant, role, worker, task, fingerprint, kind, state, record, created_at, updated_at, state_at)
		SELECT $1, tenant, role, worker, task, 'other', kind, 'realizing', jsonb_set(record, '{origin,acting_bot}', to_jsonb($2::text)), created_at, updated_at, state_at FROM proposals`,
		holder, ids.New(ids.Bot)); err != nil {
		t.Fatal(err)
	}
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE proposals SET state = 'parked', record = record || jsonb_build_object('state', 'parked', 'state_reason', 'conflict', 'conflicts_with', jsonb_build_array($1::text))
		WHERE id <> $1`, holder); err != nil {
		t.Fatal(err)
	}
	if _, err := b.analysis(t, "2", false, map[string]any{"proposals": []any{noneBody("three", 1)}}); err != nil {
		t.Fatal(err)
	}
	if n := len(proposals(t, b)); n != 3 {
		t.Fatalf("records %d, rejections %v", n, rejections(t, b))
	}
}

// The limit on lowering a schedule's frequency counts from the value applied 90 days earlier:
// one halving passes, a second halving within the 90 days does not (PRP-16).
func TestCronCumulative(t *testing.T) {
	ctx := context.Background()
	b := newBench(t)
	cat := &Catalog{Tenants: map[string]*TenantView{"demo": {Name: "demo", Schedules: map[string]manifest.Schedule{"nightly": {Cron: "0 */2 * * *"}}}}}
	set := func(cron string) proposalChange {
		var ch proposalChange
		ch.Kind = "manifest_change"
		ch.ManifestChange = &struct {
			Ops []manifestOp `json:"ops"`
		}{Ops: []manifestOp{{Op: "set", Field: "spec.cron", Value: cron}}}
		ch.ManifestChange.Ops[0].Target.Kind, ch.ManifestChange.Ops[0].Target.Name = "Schedule", "nightly"
		return ch
	}
	check := func(cron string) error {
		return b.c.tx(ctx, func(tx pgx.Tx, now time.Time) error { return b.c.cronHistory(ctx, tx, now, cat, "demo", set(cron)) })
	}
	if err := check("0 */4 * * *"); err != nil {
		t.Fatalf("one halving: %v", err)
	}
	// Ten days ago the schedule ran every hour, so every four hours is a quarter of that.
	if _, err := b.c.st.Pool.Exec(ctx, `INSERT INTO manifest_versions (kind, tenant, name, doc, replaced_at)
		VALUES ('Schedule', 'demo', 'nightly', '{"spec": {"cron": "0 * * * *"}}', botaru_now() - interval '10 days')`); err != nil {
		t.Fatal(err)
	}
	var rj rejection
	if err := check("0 */4 * * *"); err == nil || !asRejection(err, &rj) || rj != "forbidden_change" {
		t.Fatalf("second halving: %v", err)
	}
	if err := check("0 */2 * * *"); err != nil {
		t.Fatalf("half of the old frequency: %v", err)
	}
}

func asRejection(err error, rj *rejection) bool {
	r, ok := err.(rejection)
	*rj = r
	return ok
}

// A person's "Accept" writes the decision statistics point of the author role with the category
// as its second dimension, and the statistics by role and category count it (DIG-4); a row of
// "My outcomes" with a trial outcome carries the trial figures (DIG-8).
func TestDecisionStatsAndTrialFigures(t *testing.T) {
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

// An aggregator reads only the series of its sources, and only its own tenant's points: a
// canary point of another tenant in the same series and subject is not in the answer (AGG-3,
// AGG-5).
func TestAggregatorReads(t *testing.T) {
	ctx := context.Background()
	b := newBench(t)
	cat := &Catalog{Tenants: map[string]*TenantView{}, Roles: map[string]manifest.Role{"agg": {Profile: "aggregator",
		Aggregate: &manifest.RoleAggregate{}}}, TaskTypes: map[string]manifest.TaskType{"agg.v1": {Purpose: "aggregate", Observes: []string{seriesTaskOutcome, seriesTaskAttempts}}}}
	cat.Roles["agg"].Aggregate.Sources.Series = []string{seriesTaskAttempts}
	if got := b.c.readableSeries(cat, "demo", "agg", "agg.v1"); len(got) != 1 || got[0] != seriesTaskAttempts {
		t.Fatalf("readable %v", got)
	}
	live, err := b.c.Catalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.c.st.Pool.Exec(ctx, `INSERT INTO obs_points (tenant, series, subject, at, num, source, data_mark, key, created_at)
		VALUES ('other', $1, 'task_type:hello.greet.v1', botaru_now(), 42, 'core', 'internal', 'canary', botaru_now())`, seriesTaskAttempts); err != nil {
		t.Fatal(err)
	}
	err = b.c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		out, err := b.c.queryObservations(ctx, tx, now, live, "demo", []string{seriesTaskAttempts}, "internal",
			obsQuery{Series: seriesTaskAttempts, Subject: "task_type:hello.greet.v1", Window: "1d", Rollup: "window.mean"})
		if err != nil {
			return err
		}
		for _, g := range asList(out["groups"]) {
			if len(asList(mmapAny(g)["points"])) != 0 {
				t.Errorf("another tenant's point is visible: %v", out)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
