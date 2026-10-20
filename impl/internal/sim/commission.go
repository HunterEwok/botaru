// SPDX-License-Identifier: AGPL-3.0-only

package sim

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/HunterEwok/botaru/impl/internal/gate"
	"github.com/HunterEwok/botaru/impl/internal/jcs"
)

// incidentAndCommission plays the two governance loops of the farm: an incident with the farm
// manager, and a proposal through the commission to a person.
func incidentAndCommission(ctx context.Context, d *Director, sc string) error {
	start := time.Now().UTC()
	db := d.S.Bench.Store().Pool
	if err := d.ensureInvestigation(ctx); err != nil {
		return err
	}

	// 1. The incident.
	if err := d.world("the pipeline temporary files fill db1:/data to 3% free (the director's fault)", func(w *World) {
		w.Hosts["db1"].Disks["/data"].Fill(0.03)
		w.Faults[FaultDiskFilling] = true
	}); err != nil {
		return err
	}
	p, err := d.waitProblem(ctx, "disk:db1:/data")
	if err != nil {
		return fmt.Errorf("the monitoring problem did not open: %w", err)
	}
	d.check(sc, "problem_opened", true, "event %s of trigger %s", p.EventID, p.TriggerID)
	var incident, class, istate string
	err = d.S.waitFor(ctx, d.Timeout, func() (bool, error) {
		e := db.QueryRow(ctx, `SELECT id, class, state FROM incidents WHERE tenant = 'demo' AND class = 'disk_full' AND since >= $1
			ORDER BY since DESC LIMIT 1`, start).Scan(&incident, &class, &istate)
		return e == nil, nil
	})
	d.check(sc, "incident_opened", err == nil, "%s %s %s", incident, class, istate)
	if err != nil {
		return nil
	}
	var mstate, summary string
	err = d.S.waitFor(ctx, d.Timeout, func() (bool, error) {
		e := db.QueryRow(ctx, `SELECT state, coalesce(result->>'summary', '') FROM tasks WHERE tenant = 'demo' AND type = 'manager.incident.respond.v1'
			AND created_at >= $1 ORDER BY created_at LIMIT 1`, start).Scan(&mstate, &summary)
		return e == nil && (mstate == "completed" || mstate == "failed" || mstate == "canceled"), nil
	})
	d.check(sc, "manager_answered", err == nil && mstate == "completed", "farm manager task %s: %s", mstate, clip(summary, 200))
	// The people clean up as the manager proposed; the alarm resolves and the incident with it.
	if err := d.world("the operators clean up the pipeline temporary files on db1 (the manager's proposal)", func(w *World) {
		disk := w.Hosts["db1"].Disks["/data"]
		disk.FreeRatio = min(0.99, disk.FreeRatio+float64(disk.TmpBytes)/float64(disk.SizeBytes))
		disk.TmpBytes = 0
		delete(w.Faults, FaultDiskFilling)
	}); err != nil {
		return err
	}
	err = d.S.waitFor(ctx, d.Timeout, func() (bool, error) {
		e := db.QueryRow(ctx, `SELECT state FROM incidents WHERE id = $1`, incident).Scan(&istate)
		return e == nil && (istate == "resolved" || istate == "closed"), nil
	})
	d.check(sc, "incident_resolved", err == nil, "incident %s %s", incident, istate)

	// 2. The commission. The world's history first: investigations with repeated and long
	// attempts, so the analysis has series to read (one investigation alone gives too little).
	if err := d.S.SeedInvestigationLoad(ctx); err != nil {
		return fmt.Errorf("seeding the observations: %w", err)
	}
	if err := d.S.SetInsights(ctx, true); err != nil {
		return fmt.Errorf("starting the analysis schedule: %w", err)
	}
	defer func() {
		if err := d.S.SetInsights(context.WithoutCancel(ctx), false); err != nil {
			d.S.log.Addf(KindDirector, "pausing the analysis schedule: %v", err)
		}
	}()
	d.S.log.Addf(KindDirector, "the analysis schedule of ops-reader runs (every minute, at most twice a week)")
	var prop, pstate string
	err = d.S.waitFor(ctx, d.Timeout+time.Minute, func() (bool, error) {
		e := db.QueryRow(ctx, `SELECT id, state FROM proposals WHERE tenant = 'demo' AND created_at >= $1 ORDER BY created_at LIMIT 1`, start).Scan(&prop, &pstate)
		return e == nil, nil
	})
	d.check(sc, "proposal_created", err == nil, "proposal %s %s", prop, pstate)
	if err != nil {
		return nil
	}
	var reviews int
	err = d.S.waitFor(ctx, d.Timeout, func() (bool, error) {
		e := db.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE tenant = 'demo' AND type LIKE 'review.%' AND state = 'completed'
			AND idempotency_key LIKE 'system:review:' || $1 || ':%'`, prop).Scan(&reviews)
		return e == nil && reviews >= 2, nil
	})
	d.check(sc, "commission_reviewed", err == nil, "%d lens reviews completed", reviews)
	// The proposal reaches a verdict; a recommended one goes to the deciders on a card. A real
	// model may propose a manifest change, which the gate-required demo shows only after the
	// merge gate: the director runs it once, as the operator would.
	gated := false
	err = d.S.waitFor(ctx, d.Timeout+time.Minute, func() (bool, error) {
		var kind string
		e := db.QueryRow(ctx, `SELECT state, kind FROM proposals WHERE id = $1`, prop).Scan(&pstate, &kind)
		if e == nil && pstate == "recommended" && kind == "manifest_change" && !gated {
			gated = true
			if _, _, err := d.S.GateProposal(ctx, prop); err != nil {
				d.S.log.Addf(KindDirector, "the merge gate of %s: %v", prop, err)
			}
		}
		return e == nil && pstate != "screened" && pstate != "in_review" && pstate != "recommended", nil
	})
	d.check(sc, "presented", err == nil && pstate == "presented", "proposal %s", pstate)
	if err != nil || pstate != "presented" {
		return nil
	}
	// bob decides on the card by the policy (safe and all accept); then the proposal is noted.
	err = d.S.waitFor(ctx, d.Timeout, func() (bool, error) {
		e := db.QueryRow(ctx, `SELECT state FROM proposals WHERE id = $1`, prop).Scan(&pstate)
		return e == nil && pstate != "presented", nil
	})
	// Accepted (a change of the kind none is noted at once) or declined on the card.
	d.check(sc, "decided", err == nil && (pstate == "noted" || pstate == "realizing" || pstate == "declined"),
		"proposal %s (needs a person's decision; policy %s)", pstate, d.Approve)
	return nil
}

// ensureInvestigation makes sure there is a completed investigation of ops-reader: the analysis
// cites the farm's own series.
func (d *Director) ensureInvestigation(ctx context.Context) error {
	var investigations int
	_ = d.S.Bench.Store().Pool.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE tenant = 'demo' AND type = 'ops.investigate.v1' AND state = 'completed'`).Scan(&investigations)
	if investigations > 0 {
		return nil
	}
	key, err := d.S.Say("bob", "bob-dm", "/logs Are there errors in the nginx error log of the last hour?")
	if err != nil {
		return err
	}
	_, _, _, err = d.taskEnd(ctx, key)
	return err
}

// labRequests are the everyday asks of the lab to the data bots: a raster and a layer to
// geo-reader, a failed pipeline run to pipeline-reader; and the site checks the data-job
// worker ran on its schedule.
func labRequests(ctx context.Context, d *Director, sc string) error {
	for _, ask := range []struct{ who, chat, text, want, check string }{
		{"alice", "alice-dm", "/geo What is in the bathy-50m raster and the map-areas layer?", "GTiff|GeoTIFF", "geo_described"},
		{"bob", "bob-dm", "/pipeline Why did the last CTD run fail?", "st-05", "pipeline_explained"},
	} {
		key, err := d.S.Say(ask.who, ask.chat, ask.text)
		if err != nil {
			return err
		}
		state, reason, result, err := d.taskEnd(ctx, key)
		if err != nil {
			return err
		}
		raw, _ := json.Marshal(result)
		// want lists the words of which the answer names one: the driver's name GTiff, or
		// GeoTIFF as a real model writes it.
		named := slices.ContainsFunc(strings.Split(ask.want, "|"), func(w string) bool { return strings.Contains(string(raw), w) })
		d.check(sc, ask.check, state == "completed" && named, "state %s %s: %s", state, reason, clip(string(raw), 200))
	}
	var checks int
	err := d.S.waitFor(ctx, d.Timeout+5*time.Minute, func() (bool, error) {
		e := d.S.Bench.Store().Pool.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE tenant = 'demo' AND type = 'check.http.v1' AND state = 'completed'`).Scan(&checks)
		return e == nil && checks > 0, nil
	})
	d.check(sc, "job_checks_ran", err == nil, "%d site check runs of data-job completed", checks)
	return nil
}

// evolveSignature is the approach of the mock's manifest change in the archive (EVO-5).
const evolveSignature = "Tenant.spec.limits.max_queued:down"

// scaffoldEvolution plays the safe scaffold evolution (D-120): ops-reader's analysis proposes a
// manifest change, the commission recommends it, the merge gate passes it on the farm git, bob
// accepts it on a card, the stand applies the patch from the farm git, then reverts it in one
// step for harm; the archive keeps the outcome and a review rule refuses the same approach.
func scaffoldEvolution(ctx context.Context, d *Director, sc string) error {
	start := time.Now().UTC()
	db := d.S.Bench.Store().Pool
	if err := d.ensureInvestigation(ctx); err != nil {
		return err
	}
	// The world's history: investigations that took repeated and long attempts, so the
	// analysis reads a load worth a lower queue limit.
	if err := d.S.SeedInvestigationLoad(ctx); err != nil {
		return fmt.Errorf("seeding the observations: %w", err)
	}
	defer func() {
		if err := d.S.SetEvolve(context.WithoutCancel(ctx), false); err != nil {
			d.S.log.Addf(KindDirector, "ending the evolution mode: %v", err)
		}
	}()
	if err := d.S.SetEvolve(ctx, true); err != nil {
		return fmt.Errorf("the evolution mode: %w", err)
	}
	if err := d.S.SetInsights(ctx, true); err != nil {
		return fmt.Errorf("starting the analysis schedule: %w", err)
	}
	defer func() {
		if err := d.S.SetInsights(context.WithoutCancel(ctx), false); err != nil {
			d.S.log.Addf(KindDirector, "pausing the analysis schedule: %v", err)
		}
	}()
	d.S.log.Addf(KindDirector, "the analysis schedule of ops-reader runs, in the evolution mode (the load check)")

	// 1. The manifest change and its variant.
	var prop, pstate string
	err := d.S.waitFor(ctx, d.Timeout+time.Minute, func() (bool, error) {
		e := db.QueryRow(ctx, `SELECT id, state FROM proposals WHERE tenant = 'demo' AND kind = 'manifest_change' AND created_at >= $1
			ORDER BY created_at LIMIT 1`, start).Scan(&prop, &pstate)
		return e == nil, nil
	})
	if err != nil {
		d.check(sc, "proposal_manifest_created", false, "no manifest change: %v", err)
		return nil
	}
	// The gate runs at the applied commit: the schedule pauses first, so no apply comes between.
	if err := d.S.SetInsights(ctx, false); err != nil {
		return fmt.Errorf("pausing the analysis schedule: %w", err)
	}
	variant := func() map[string]any {
		vs, _ := d.S.Variants(ctx)
		for _, v := range vs {
			if v["proposal_id"] == prop {
				return v
			}
		}
		return nil
	}
	v := variant()
	d.check(sc, "proposal_manifest_created", v != nil && v["trial_outcome"] == "pending" && v["target_kind"] == "Tenant" &&
		v["target_name"] == "demo" && v["diversity_signature"] == evolveSignature,
		"proposal %s %s, variant %v of %v:%v, signature %v, outcome %v", prop, pstate, v["variant_id"], v["target_kind"], v["target_name"],
		v["diversity_signature"], v["trial_outcome"])

	// 2. The commission.
	err = d.S.waitFor(ctx, d.Timeout+time.Minute, func() (bool, error) {
		e := db.QueryRow(ctx, `SELECT state FROM proposals WHERE id = $1`, prop).Scan(&pstate)
		return e == nil && pstate != "screened" && pstate != "in_review", nil
	})
	var reviews int
	_ = db.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE tenant = 'demo' AND type LIKE 'review.%' AND state = 'completed'
		AND idempotency_key LIKE 'system:review:' || $1 || ':%'`, prop).Scan(&reviews)
	d.check(sc, "commission_recommended", err == nil && pstate == "recommended" && reviews >= 2, "proposal %s after %d lens reviews", pstate, reviews)
	if pstate != "recommended" {
		return nil
	}

	// 3. The merge gate on the farm git.
	rep, after, err := d.S.GateProposal(ctx, prop)
	if err != nil {
		d.check(sc, "gate_passed", false, "%v", err)
		return nil
	}
	var verdict string
	_ = db.QueryRow(ctx, `SELECT coalesce(record->'gate'->>'state', '') FROM proposals WHERE id = $1`, prop).Scan(&verdict)
	steps, checked := map[string]string{}, true
	var list []string
	for _, st := range rep.Steps {
		steps[st.Name] = st.Status
		list = append(list, st.Name+" "+st.Status)
	}
	for _, n := range []string{"diff", "lint", "coupling", "regression", "holdout"} {
		checked = checked && steps[n] == gate.Passed
	}
	d.check(sc, "gate_passed", rep.Passed() && verdict == "passed" && checked, "verdict %s recorded as %q at %s, proposal %s; %s",
		rep.Verdict, verdict, rep.BaseSHA[:12], after, strings.Join(list, ", "))
	if !rep.Passed() {
		return nil
	}

	// 4. bob accepts on the card; the stand applies the patch from the farm git.
	err = d.S.waitFor(ctx, d.Timeout+time.Minute, func() (bool, error) {
		e := db.QueryRow(ctx, `SELECT state FROM proposals WHERE id = $1`, prop).Scan(&pstate)
		return e == nil && pstate != "recommended" && pstate != "presented", nil
	})
	var trial bool
	var decidedBy string
	_ = db.QueryRow(ctx, `SELECT record->'trial' IS NOT NULL AND record->'trial' <> 'null', coalesce(record->'decision'->>'decided_by', '')
		FROM proposals WHERE id = $1`, prop).Scan(&trial, &decidedBy)
	who := d.S.personName(decidedBy)
	if err != nil || pstate != "realizing" || !trial || who != Decider {
		d.check(sc, "accepted_and_realized", false, "proposal %s, trial %v, decided by %q (needs a person's decision; policy %s)", pstate, trial, who, d.Approve)
		return nil
	}
	before, err := tenantHash(ctx, d)
	if err != nil {
		return err
	}
	commit, err := d.S.Realize(ctx, prop)
	if err != nil {
		d.check(sc, "accepted_and_realized", false, "applying the accepted change: %v", err)
		return nil
	}
	err = d.S.waitFor(ctx, d.Timeout, func() (bool, error) {
		e := db.QueryRow(ctx, `SELECT state FROM proposals WHERE id = $1`, prop).Scan(&pstate)
		return e == nil && pstate != "realizing", nil
	})
	var queued string
	_ = db.QueryRow(ctx, `SELECT coalesce(doc->'spec'->'limits'->>'max_queued', '') FROM manifests WHERE kind = 'Tenant' AND tenant = 'demo' AND name = 'demo'`).Scan(&queued)
	card := slices.ContainsFunc(d.S.Cards(), func(c Card) bool { return c.Subject == prop && c.Pressed != "" })
	msg, _ := d.S.repo.git(ctx, "log", "-1", "--format=%B", commit)
	trailer := strings.Contains(msg, "Botaru-Proposal: "+prop)
	// A real model may name another value; any lower limit is the safe direction (PRP-3).
	lowered := false
	if n, perr := strconv.Atoi(queued); perr == nil && n > 0 && n < 5000 {
		lowered = true
	}
	d.check(sc, "accepted_and_realized", err == nil && pstate == "realized" && lowered && card && trailer,
		"%s accepted on the card (pressed %v), trial recorded; commit %s (Botaru-Proposal %v) applied, proposal %s, max_queued %s",
		who, card, commit[:12], trailer, pstate, queued)
	if pstate != "realized" {
		return nil
	}

	// 5. The revert in one step, the archive and the review rule.
	res, err := d.S.RevertProposal(ctx, prop, "harm_observed")
	if err != nil {
		d.check(sc, "reverted_and_rule_recorded", false, "the revert: %v", err)
		return nil
	}
	err = d.S.waitFor(ctx, d.Timeout, func() (bool, error) {
		e := db.QueryRow(ctx, `SELECT state FROM proposals WHERE id = $1`, prop).Scan(&pstate)
		return e == nil && pstate != "realized", nil
	})
	back, herr := tenantHash(ctx, d)
	v = variant()
	var rule map[string]any
	rules, _ := d.S.ReviewRules(ctx)
	for _, r := range rules {
		if r["role"] == "ops-reader" && r["target_kind"] == "Tenant" && r["target_name"] == "demo" && r["signature"] == evolveSignature {
			rule = r
		}
	}
	// The same approach again: the analysis runs once more and the core refuses it.
	mark, merr := d.S.Bench.StoreNow(ctx)
	var refused int
	if merr == nil && rule != nil {
		if err := d.S.SetInsights(ctx, true); err != nil {
			return fmt.Errorf("starting the analysis schedule: %w", err)
		}
		_ = d.S.waitFor(ctx, d.Timeout+time.Minute, func() (bool, error) {
			e := db.QueryRow(ctx, `SELECT count(*) FROM events WHERE tenant = 'demo' AND type = 'proposal.rejected.v1'
				AND data->>'reason' = 'review_rule' AND time >= $1`, mark).Scan(&refused)
			return e == nil && refused > 0, nil
		})
		if err := d.S.SetInsights(ctx, false); err != nil {
			return fmt.Errorf("pausing the analysis schedule: %w", err)
		}
	}
	var again int
	_ = db.QueryRow(ctx, `SELECT count(*) FROM proposals WHERE tenant = 'demo' AND kind = 'manifest_change' AND created_at >= $1`, mark).Scan(&again)
	d.check(sc, "reverted_and_rule_recorded", err == nil && pstate == "reverted" && herr == nil && back == before && v != nil &&
		v["trial_outcome"] == "reverted" && rule != nil && refused > 0 && again == 0,
		"commit %s with Botaru-Revert, proposal %s; Tenant demo hash %s back to %s; variant outcome %v; review rule %v %v; resubmissions refused with rejected(review_rule): %d, new proposals %d",
		res.Commit[:12], pstate, clip(back, 23), clip(before, 23), v["trial_outcome"], rule["rule"], rule["signature"], refused, again)
	return nil
}

// tenantHash is the JCS hash of the applied Tenant manifest of demo.
func tenantHash(ctx context.Context, d *Director) (string, error) {
	var doc map[string]any
	if err := d.S.Bench.Store().Pool.QueryRow(ctx, `SELECT doc FROM manifests WHERE kind = 'Tenant' AND tenant = 'demo' AND name = 'demo'`).Scan(&doc); err != nil {
		return "", err
	}
	return jcs.Hash(doc)
}
