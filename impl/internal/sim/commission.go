// SPDX-License-Identifier: AGPL-3.0-only

package sim

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

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
		w.Hosts[d.S.HomeHost(KindDB)].Disks["/data"].Fill(0.03)
		w.Faults[FaultDiskFilling] = true
	}); err != nil {
		return err
	}
	p, err := d.waitProblem(ctx, "disk:"+d.S.HomeHost(KindDB)+":/data")
	if err != nil {
		return fmt.Errorf("the monitoring problem did not open: %w", err)
	}
	d.check(sc, "problem_opened", true, "event %s of trigger %s", p.EventID, p.TriggerID)
	var incident, class, istate string
	err = d.S.waitFor(ctx, d.Timeout, func() (bool, error) {
		e := db.QueryRow(ctx, `SELECT id, class, state FROM incidents WHERE tenant = $2 AND class = 'disk_full' AND since >= $1
			ORDER BY since DESC LIMIT 1`, start, d.S.Home()).Scan(&incident, &class, &istate)
		return e == nil, nil
	})
	d.check(sc, "incident_opened", err == nil, "%s %s %s", incident, class, istate)
	if err != nil {
		return nil
	}
	// The people clean up only after the manager's task ended, so a quick close of the incident
	// never cancels it; the reason of a task that did not complete is printed.
	var mstate, mreason, summary string
	err = d.S.waitFor(ctx, d.Timeout, func() (bool, error) {
		e := db.QueryRow(ctx, `SELECT state, coalesce(reason, ''), coalesce(result->>'summary', '') FROM tasks WHERE tenant = $2
			AND type = 'manager.incident.respond.v1' AND created_at >= $1 ORDER BY created_at LIMIT 1`, start, d.S.Home()).Scan(&mstate, &mreason, &summary)
		return e == nil && (mstate == "completed" || mstate == "failed" || mstate == "canceled"), nil
	})
	d.check(sc, "manager_answered", err == nil && mstate == "completed", "farm manager task %s %s: %s", mstate, mreason, clip(summary, 200))
	// The people clean up as the manager proposed; the alarm resolves and the incident with it.
	if err := d.world("the operators clean up the pipeline temporary files on db1 (the manager's proposal)", func(w *World) {
		disk := w.Hosts[d.S.HomeHost(KindDB)].Disks["/data"]
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
	// The reading the manager asked for (MGR-25) and the analysis after the incident (MGR-53).
	// A missing reading fails its own check and the scenario goes on to the commission.
	rstate, rreason, _, err := d.typeEnd(ctx, d.S.Home(), "manager.inspect.v1", start)
	var findings int
	_ = db.QueryRow(ctx, `SELECT count(*) FROM findings WHERE tenant = $1 AND incident = $2 AND created_at >= $3`, d.S.Home(), incident, start).Scan(&findings)
	d.check(sc, "manager_read", err == nil && rstate == "completed" && findings > 0, "reading %s %s, %d findings %v", rstate, rreason, findings, err)
	// One analysis per incident: an incident an earlier scenario left that opened again keeps the
	// analysis of its first resolution (IDM-2).
	var astate, areason string
	err = d.S.waitFor(ctx, d.Timeout, func() (bool, error) {
		e := db.QueryRow(ctx, `SELECT state, coalesce(reason, '') FROM tasks WHERE tenant = $1 AND type = 'manager.postmortem.v1'
			AND record->>'postmortem' = $2 ORDER BY created_at DESC LIMIT 1`, d.S.Home(), incident).Scan(&astate, &areason)
		return e == nil && (astate == "completed" || astate == "failed" || astate == "canceled"), nil
	})
	d.check(sc, "postmortem", err == nil && astate == "completed", "analysis after the incident %s %s", astate, areason)

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
		e := db.QueryRow(ctx, `SELECT id, state FROM proposals WHERE tenant = $2 AND created_at >= $1 ORDER BY created_at LIMIT 1`, start, d.S.Home()).Scan(&prop, &pstate)
		return e == nil, nil
	})
	d.check(sc, "proposal_created", err == nil, "proposal %s %s", prop, pstate)
	if err != nil {
		return nil
	}
	var reviews int
	err = d.S.waitFor(ctx, d.Timeout, func() (bool, error) {
		e := db.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE tenant = $2 AND type LIKE 'review.%' AND state = 'completed'
			AND idempotency_key LIKE 'system:review:' || $1 || ':%'`, prop, d.S.Home()).Scan(&reviews)
		return e == nil && reviews >= 2, nil
	})
	d.check(sc, "commission_reviewed", err == nil, "%d lens reviews completed", reviews)
	// The way of a proposal to a person and into the farm git is scaffold-evolution's to check;
	// here the proposal only leaves the open states, so that it holds no slot of the bot (one
	// open proposal per bot, PRP-5) when the next analysis hands in its change.
	d.settle(ctx, prop)
	return nil
}

// openStates are the states of an open proposal (openProposalStates of the core).
var openStates = []string{"screened", "in_review", "recommended", "presented", "parked"}

// settle waits until a proposal leaves the open states: bob decides its card by the policy, and
// a recommended manifest change, which the gate-required demo presents only after the merge
// gate, goes through the gate once, as the operator would run it. A parked proposal leaves only
// by park_ttl (PRP-6), so its park is dated back once, as if that time had passed. It checks
// nothing.
func (d *Director) settle(ctx context.Context, prop string) {
	if d.Approve == "none" {
		d.S.log.Addf(KindDirector, "proposal %s stays open: no virtual person decides a card (policy none)", prop)
		return
	}
	db := d.S.Bench.Store().Pool
	gated, aged := false, false
	var state string
	err := d.S.waitFor(ctx, 2*d.Timeout+time.Minute, func() (bool, error) {
		var kind string
		if e := db.QueryRow(ctx, `SELECT state, kind FROM proposals WHERE id = $1`, prop).Scan(&state, &kind); e != nil {
			return false, nil
		}
		if state == "recommended" && kind == "manifest_change" && !gated {
			gated = true
			if _, _, err := d.S.GateProposal(ctx, prop); err != nil {
				d.S.log.Addf(KindDirector, "the merge gate of %s: %v", prop, err)
			}
		}
		if state == "parked" && !aged {
			// The core may move it on between the read and the write; then it is dated back on
			// its next park.
			tag, err := db.Exec(ctx, `UPDATE proposals SET record = jsonb_set(record, '{parked_since}', '"2000-01-01T00:00:00Z"') WHERE id = $1 AND state = 'parked'`, prop)
			switch {
			case err != nil:
				d.S.log.Addf(KindDirector, "dating back the park of %s: %v", prop, err)
			case tag.RowsAffected() == 1:
				aged = true
				d.S.log.Addf(KindDirector, "proposal %s is parked: its park is dated back past park_ttl", prop)
			}
		}
		return !slices.Contains(openStates, state), nil
	})
	if err != nil {
		d.S.log.Addf(KindDirector, "proposal %s is still %s: it holds the bot's slot of an open proposal", prop, state)
		return
	}
	d.S.log.Addf(KindDirector, "proposal %s ended %s", prop, state)
}

// ensureInvestigation makes sure there is a completed investigation of ops-reader: the analysis
// cites the farm's own series.
func (d *Director) ensureInvestigation(ctx context.Context) error {
	var investigations int
	_ = d.S.Bench.Store().Pool.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE tenant = $1 AND type = 'ops.investigate.v1' AND state = 'completed'`, d.S.Home()).Scan(&investigations)
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
	for _, ask := range []struct {
		who, chat, text, check string
		tools                  []string
		want                   *regexp.Regexp
	}{
		// The answer names a fact of the tools' output: the format, the coordinate system, the
		// size, the deepest value, the band type or the layer's driver.
		{"alice", "alice-dm", "/geo What is in the bathy-50m raster and the map-areas layer?", "geo_described",
			[]string{"geo/describe", "geo/describe_vector"}, regexp.MustCompile(`GTiff|GeoTIFF|EPSG:3857|\b4000\s*(x|×|by)\s*3000\b|312\.4|Float32|GeoJSON`)},
		{"bob", "bob-dm", "/pipeline Why did the last CTD run fail?", "pipeline_explained", nil, regexp.MustCompile(`st-05`)},
	} {
		lab := d.S.LabTenant()
		key, err := d.S.Say(d.S.PersonIn(lab, ask.who), d.S.PersonIn(lab, ask.chat), ask.text)
		if err != nil {
			return err
		}
		state, reason, result, err := d.taskEnd(ctx, key)
		if err != nil {
			return err
		}
		raw, _ := json.Marshal(result)
		var missing []string
		for _, tool := range ask.tools {
			var done int
			_ = d.S.Bench.Store().Pool.QueryRow(ctx, `SELECT count(*) FROM toolcalls t JOIN tasks k ON k.id = t.task
				WHERE k.tenant = $3 AND k.idempotency_key = $1 AND t.tool = $2 AND t.state = 'done'`, key, tool, lab).Scan(&done)
			if done == 0 {
				missing = append(missing, tool)
			}
		}
		named := ask.want.Match(raw)
		d.check(sc, ask.check, state == "completed" && len(missing) == 0 && named, "state %s %s, tools not called %v, a fact named %v: %s",
			state, reason, missing, named, clip(string(raw), 200))
	}
	var checks int
	err := d.S.waitFor(ctx, d.Timeout+5*time.Minute, func() (bool, error) {
		e := d.S.Bench.Store().Pool.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE tenant = $1 AND type = 'check.http.v1' AND state = 'completed'`, d.S.Home()).Scan(&checks)
		return e == nil && checks > 0, nil
	})
	d.check(sc, "job_checks_ran", err == nil, "%d site check runs of data-job completed", checks)
	return describeFile(ctx, d, sc)
}

// evolveField is the field the scaffold evolution changes, and evolveSignature its approach in the
// archive (EVO-5).
const (
	evolveField     = "spec.dispatch.rules[].timeouts.attempt"
	evolveSignature = "Tenant." + evolveField + ":up"
)

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
	// The world's history: investigations whose attempts the deadline cut, so the analysis reads
	// a load worth a longer attempt deadline.
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

	// 1. The manifest change and its variant. An analysis that ends without one is printed, and
	// the schedule runs it once more before the check fails, after the open proposals of the bot,
	// which hold its one slot (PRP-5), have ended.
	var prop, pstate, last string
	seen := map[string]bool{}
	errEnded := errors.New("the analysis ended without a manifest change")
	var err error
	for try := 0; ; try++ {
		err = d.S.waitFor(ctx, 2*d.Timeout+time.Minute, func() (bool, error) {
			if e := db.QueryRow(ctx, `SELECT id, state FROM proposals WHERE tenant = $2 AND kind = 'manifest_change' AND created_at >= $1
				ORDER BY created_at LIMIT 1`, start, d.S.Home()).Scan(&prop, &pstate); e == nil {
				return true, nil
			}
			var id, state string
			var ended time.Time
			var raw []byte
			if e := db.QueryRow(ctx, `SELECT id, state, updated_at, result FROM tasks WHERE tenant = $3 AND type = $1
				AND created_at >= $2 AND state IN ('completed', 'failed', 'canceled') ORDER BY created_at DESC LIMIT 1`, insightType, start, d.S.Home()).
				Scan(&id, &state, &ended, &raw); e != nil || seen[id] || time.Since(ended) < 3*time.Second {
				return false, nil
			}
			seen[id] = true
			last = fmt.Sprintf("analysis %s %s without a manifest change: %s", id, state, clip(string(raw), 400))
			d.S.log.Addf(KindDirector, "%s", last)
			return false, errEnded
		})
		if !errors.Is(err, errEnded) || try == 1 {
			break
		}
		rows, qerr := db.Query(ctx, `SELECT id FROM proposals WHERE tenant = $2 AND role = 'ops-reader' AND state = ANY($1)`, openStates, d.S.Home())
		if qerr != nil {
			return qerr
		}
		open, qerr := pgx.CollectRows(rows, pgx.RowTo[string])
		if qerr != nil {
			return qerr
		}
		for _, p := range open {
			d.settle(ctx, p)
		}
		if err := d.S.SetInsights(ctx, false); err != nil {
			return fmt.Errorf("pausing the analysis schedule: %w", err)
		}
		if err := d.S.SetInsights(ctx, true); err != nil {
			return fmt.Errorf("starting the analysis schedule: %w", err)
		}
	}
	if err != nil {
		msg := err.Error()
		if last != "" && msg != last {
			msg += "; " + last
		}
		d.check(sc, "proposal_manifest_created", false, "no manifest change: %s", msg)
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
		v["target_name"] == d.S.Home() && v["diversity_signature"] == evolveSignature,
		"proposal %s %s, variant %v of %v:%v, signature %v, outcome %v", prop, pstate, v["variant_id"], v["target_kind"], v["target_name"],
		v["diversity_signature"], v["trial_outcome"])

	// 2. The commission.
	err = d.S.waitFor(ctx, d.Timeout+time.Minute, func() (bool, error) {
		e := db.QueryRow(ctx, `SELECT state FROM proposals WHERE id = $1`, prop).Scan(&pstate)
		return e == nil && pstate != "screened" && pstate != "in_review", nil
	})
	var reviews int
	_ = db.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE tenant = $2 AND type LIKE 'review.%' AND state = 'completed'
		AND idempotency_key LIKE 'system:review:' || $1 || ':%'`, prop, d.S.Home()).Scan(&reviews)
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
	var attempt string
	_ = db.QueryRow(ctx, `SELECT coalesce(doc->'spec'->'dispatch'->'rules'->0->'timeouts'->>'attempt', '') FROM manifests WHERE kind = 'Tenant' AND tenant = $1 AND name = $1`, d.S.Home()).Scan(&attempt)
	card := slices.ContainsFunc(d.S.Cards(), func(c Card) bool { return c.Subject == prop && c.Pressed != "" })
	msg, _ := d.S.repo.git(ctx, "log", "-1", "--format=%B", commit)
	trailer := strings.Contains(msg, "Botaru-Proposal: "+prop)
	// A real model may name another value; the core lets the deadline rise up to two times.
	cur, _ := time.ParseDuration(InvestigateAttempt)
	nv, perr := time.ParseDuration(attempt)
	raised := perr == nil && nv > cur
	d.check(sc, "accepted_and_realized", err == nil && pstate == "realized" && raised && card && trailer,
		"%s accepted on the card (pressed %v), trial recorded; commit %s (Botaru-Proposal %v) applied, proposal %s, attempt deadline %s",
		who, card, commit[:12], trailer, pstate, attempt)
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
		if r["role"] == "ops-reader" && r["target_kind"] == "Tenant" && r["target_name"] == d.S.Home() && r["signature"] == evolveSignature {
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
			e := db.QueryRow(ctx, `SELECT count(*) FROM events WHERE tenant = $2 AND type = 'proposal.rejected.v1'
				AND data->>'reason' = 'review_rule' AND time >= $1`, mark, d.S.Home()).Scan(&refused)
			return e == nil && refused > 0, nil
		})
		if err := d.S.SetInsights(ctx, false); err != nil {
			return fmt.Errorf("pausing the analysis schedule: %w", err)
		}
	}
	var again int
	_ = db.QueryRow(ctx, `SELECT count(*) FROM proposals WHERE tenant = $2 AND kind = 'manifest_change' AND created_at >= $1`, mark, d.S.Home()).Scan(&again)
	d.check(sc, "reverted_and_rule_recorded", err == nil && pstate == "reverted" && herr == nil && back == before && v != nil &&
		v["trial_outcome"] == "reverted" && rule != nil && refused > 0 && again == 0,
		"commit %s with Botaru-Revert, proposal %s; Tenant demo hash %s back to %s; variant outcome %v; review rule %v %v; resubmissions refused with rejected(review_rule): %d, new proposals %d",
		res.Commit[:12], pstate, clip(back, 23), clip(before, 23), v["trial_outcome"], rule["rule"], rule["signature"], refused, again)
	return nil
}

// tenantHash is the JCS hash of the applied Tenant manifest of demo.
func tenantHash(ctx context.Context, d *Director) (string, error) {
	var doc map[string]any
	if err := d.S.Bench.Store().Pool.QueryRow(ctx, `SELECT doc FROM manifests WHERE kind = 'Tenant' AND tenant = $1 AND name = $1`, d.S.Home()).Scan(&doc); err != nil {
		return "", err
	}
	return jcs.Hash(doc)
}
