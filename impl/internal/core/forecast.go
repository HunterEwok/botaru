// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/money"
	"github.com/HunterEwok/botaru/impl/internal/store"
)

// Review forecasts and their scores (COM-15, COM-16, COM-17). From v0.3 a lens review hands in
// review-result.v2 with a forecast of its own lens outcome; code checks it, keeps it with the
// warning of the review, scores it with the Brier score once the outcome is known and computes
// the accuracy of a lens executor against the base rates of its lens. No bot context reads any of
// it (GOV-46).

const (
	reviewResultV1 = "urn:botaru:schema:review-result:1"
	reviewResultV2 = "urn:botaru:schema:review-result:2"
	// worstBrier is the Brier score of a missed forecast: a forecast of certainty on the wrong
	// class, over two or three classes (COM-17).
	worstBrier = 2.0
	// minAccuracyForecasts are the scored forecasts below which the accuracy is insufficient.
	minAccuracyForecasts = 10
	// forecastLookback bounds the forecasts the scoring pass looks at.
	forecastLookback = 365 * 24 * time.Hour
)

// lensClasses are the outcome classes a lens forecasts (COM-16): the veto lenses the bad outcome,
// cost the overspend, the other lenses the trial outcome classes.
func lensClasses(lens string, veto bool) []string {
	switch {
	case veto:
		return []string{"bad", "ok"}
	case lens == "cost":
		return []string{"over", "within"}
	}
	return []string{"better", "unchanged", "worse"}
}

// pBad is the probability a forecast gives the bad outcome of its lens.
func pBad(lens string, veto bool, f map[string]float64) float64 {
	switch {
	case veto:
		return f["bad"]
	case lens == "cost":
		return f["over"]
	}
	return f["worse"]
}

// forecastOf reads the forecast of a review-result.v2.
func forecastOf(res map[string]any) map[string]float64 {
	out := map[string]float64{}
	for k, v := range mmapAny(res["forecast"]) {
		if n, ok := toFloat(v); ok {
			out[k] = n
		}
	}
	return out
}

// forecastValid checks what the schema cannot (COM-15, COM-16): the probabilities of the lens's
// classes sum to 1, and a score of 3 or more does not go with a probability of a bad outcome of
// 0.5 or more; such a review is invalid and counts as missing (COM-4).
func forecastValid(lens string, veto bool, res map[string]any) bool {
	f := forecastOf(res)
	sum := 0.0
	for _, c := range lensClasses(lens, veto) {
		v, ok := f[c]
		if !ok {
			return false
		}
		sum += v
	}
	if math.Abs(sum-1) > 1e-6 {
		return false
	}
	score, _ := toFloat(res["score"])
	return !(score >= 3 && pBad(lens, veto, f) >= 0.5-1e-9)
}

// forecastDisagrees says whether the warning of a review disagrees with its own forecast: it
// warns (a veto or a score of at most 1) while the bad outcome is below 0.5, or it passes while
// the bad outcome is 0.5 or more (COM-17).
func forecastDisagrees(lens string, veto bool, score int, isVeto bool, f map[string]float64) bool {
	warns := isVeto || score <= 1
	bad := pBad(lens, veto, f) >= 0.5-1e-9
	return warns != bad
}

// brierOf is the multiclass Brier score of a forecast against the outcome (binary for the veto
// lenses and cost): the sum over the classes of the squared distance (COM-16).
func brierOf(classes []string, f map[string]float64, outcome string) float64 {
	b := 0.0
	for _, c := range classes {
		o := 0.0
		if c == outcome {
			o = 1
		}
		b += (f[c] - o) * (f[c] - o)
	}
	return b
}

// reviewResultSchema is the result schema of a review task: review-result.v2 for a review
// enqueued on a v0.3 core, review-result for the records of v0.1 and v0.2 (COM-15).
func reviewResultSchema(rec map[string]any) string {
	if rec["review_schema"] == "v2" {
		return reviewResultV2
	}
	return reviewResultV1
}

// reviewV1View is a review-result.v2 without its forecast: the fields of review-result that the
// proposal record keeps as the review's result (COM-15).
func reviewV1View(res map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range res {
		if k != "forecast" {
			out[k] = v
		}
	}
	return out
}

// recordForecasts keeps, when a proposal's reviews are over, a forecast row for every review of
// the round enqueued on a v0.3 core after governance_v03_since: the forecast of a valid review,
// and for a missing or invalid one only the mark (COM-16, GOV-41).
func (c *Core) recordForecasts(ctx context.Context, tx pgx.Tx, now time.Time, p *proposalRow) error {
	since, _, err := v03State(ctx, tx, p.Tenant)
	if err != nil || since == nil {
		return err
	}
	defs := c.lensDefs()
	for _, r := range asList(p.Rec["reviews"]) {
		rm := mmapAny(r)
		if rm["v03"] != true {
			continue
		}
		written := parseTime(asString(rm["enqueued_at"]))
		if written.Before(*since) {
			continue
		}
		lens := asString(rm["lens"])
		veto := defs[lens].Veto
		state := "missing"
		var forecast any
		var score any
		isVeto, disagrees := false, false
		switch rm["state"] {
		case "done":
			state = "done"
			res := mmapAny(rm["result"])
			f := map[string]float64{}
			for k, v := range mmapAny(rm["forecast"]) {
				f[k], _ = toFloat(v)
			}
			forecast = rm["forecast"]
			n, _ := toFloat(res["score"])
			score, isVeto = int(n), res["veto"] == true
			disagrees = forecastDisagrees(lens, veto, int(n), isVeto, f)
		case "schema_invalid":
			state = "invalid"
		}
		bot := asString(rm["holder"])
		if b, err := reviewBot(ctx, tx, asString(rm["task"])); err != nil {
			return err
		} else if b != "" {
			bot = b
		}
		var fraw []byte
		if forecast != nil {
			if fraw, err = json.Marshal(forecast); err != nil {
				return err
			}
		}
		var epoch any
		if n, ok := toFloat(rm["seat_epoch"]); ok {
			epoch = int(n)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO forecasts (task, tenant, proposal, lens, executor, bot, seat, seat_epoch, state, forecast, score, veto, disagrees, written_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14) ON CONFLICT DO NOTHING`,
			rm["task"], p.Tenant, p.ID, lens, asString(rm["executor"]), nullable(bot), nullable(asString(rm["seat"])), epoch, state, fraw, score, isVeto, disagrees, now); err != nil {
			return err
		}
	}
	return nil
}

// reviewBot is the identity a review task ran on behalf of, from its lease.
func reviewBot(ctx context.Context, q store.Querier, task string) (string, error) {
	var bot string
	err := q.QueryRow(ctx, `SELECT coalesce(acting_bot, '') FROM leases WHERE work_id = $1 AND acting_bot IS NOT NULL ORDER BY lease_no DESC LIMIT 1`, task).Scan(&bot)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return bot, err
}

// forecastRow is a kept forecast with its proposal.
type forecastRow struct {
	Task, Proposal, Lens, State string
	Veto, Disagrees             bool
	Forecast                    map[string]float64
}

// outcomeOf is a known outcome of a lens for a forecast: the class, its source and reference,
// the weight, and the attribution it comes from.
type outcomeOf struct {
	Class, Source, Ref, Attribution string
	Weight                          float64
}

// scoreStep scores the kept forecasts of a tenant whose outcome became known (COM-16): every
// forecast row of the last year, by each source that has an outcome for its lens; a score is
// written once per source into forecast_scores and into the review in the proposal record. A
// score from an attribution that was canceled since leaves both, so the case is scored again.
// A forecast is looked at again only when its proposal, an attribution of it or a resubmission of
// it changed since the last look, or once a day for outcomes that come with time (an attribution
// window that closes); the proposal is read without a lock and locked only to add a score.
func (c *Core) scoreStep(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, tv *TenantView) error {
	if !claimsV03() {
		return nil
	}
	rows, err := tx.Query(ctx, `DELETE FROM forecast_scores s USING attributions a WHERE s.tenant = $1 AND a.id = s.attribution AND a.state = 'canceled'
		RETURNING s.task, s.proposal, s.ref`, tv.Name)
	if err != nil {
		return err
	}
	type dropped struct{ Task, Proposal, Ref string }
	gone, err := pgx.CollectRows(rows, pgx.RowToStructByPos[dropped])
	if err != nil {
		return err
	}
	for _, d := range gone {
		p, err := loadProposal(ctx, tx, d.Proposal)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		for _, r := range asList(p.Rec["reviews"]) {
			rm := mmapAny(r)
			if rm["task"] != d.Task {
				continue
			}
			kept := []any{}
			for _, sc := range asList(rm["scores"]) {
				if sm := mmapAny(sc); sm["source"] != "attribution" || sm["ref"] != d.Ref {
					kept = append(kept, sc)
				}
			}
			rm["scores"] = kept
		}
		if err := c.saveProposal(ctx, tx, now, p); err != nil {
			return err
		}
	}
	rows, err = tx.Query(ctx, `SELECT f.task, f.proposal, f.lens, f.state, coalesce(f.forecast, '{}'), f.disagrees FROM forecasts f JOIN proposals p ON p.id = f.proposal
		WHERE f.tenant = $1 AND f.written_at >= $2 AND (f.checked_at IS NULL OR f.checked_at < $3 OR p.updated_at > f.checked_at
			OR EXISTS (SELECT 1 FROM attributions a WHERE a.proposal = f.proposal AND a.updated_at > f.checked_at)
			OR EXISTS (SELECT 1 FROM proposals r WHERE r.tenant = $1 AND r.record->'resubmission'->>'of' = f.proposal AND r.updated_at > f.checked_at))
		ORDER BY f.proposal, f.task`, tv.Name, now.Add(-forecastLookback), now.Add(-24*time.Hour))
	if err != nil {
		return err
	}
	defs := c.lensDefs()
	byProposal := map[string][]forecastRow{}
	var order, tasks []string
	for rows.Next() {
		var f forecastRow
		var raw []byte
		if err := rows.Scan(&f.Task, &f.Proposal, &f.Lens, &f.State, &raw, &f.Disagrees); err != nil {
			rows.Close()
			return err
		}
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		f.Forecast = map[string]float64{}
		for k, v := range m {
			f.Forecast[k], _ = toFloat(v)
		}
		f.Veto = defs[f.Lens].Veto
		if _, ok := byProposal[f.Proposal]; !ok {
			order = append(order, f.Proposal)
		}
		byProposal[f.Proposal] = append(byProposal[f.Proposal], f)
		tasks = append(tasks, f.Task)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, pid := range order {
		p, err := readProposal(ctx, tx, pid)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		type added struct {
			task  string
			entry map[string]any
		}
		var adds []added
		for _, f := range byProposal[pid] {
			outs, err := c.lensOutcomes(ctx, tx, now, cat, p, f.Lens, f.Veto)
			if err != nil {
				return err
			}
			for _, o := range outs {
				b := worstBrier
				if f.State == "done" && !f.Disagrees {
					b = brierOf(lensClasses(f.Lens, f.Veto), f.Forecast, o.Class)
				}
				tag, err := tx.Exec(ctx, `INSERT INTO forecast_scores (task, source, ref, tenant, proposal, lens, outcome, brier, weight, attribution, scored_at)
					VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11) ON CONFLICT DO NOTHING`,
					f.Task, o.Source, o.Ref, tv.Name, pid, f.Lens, o.Class, b, o.Weight, nullable(o.Attribution), now)
				if err != nil {
					return err
				}
				if tag.RowsAffected() > 0 {
					adds = append(adds, added{f.Task, map[string]any{"source": o.Source, "ref": o.Ref, "outcome": o.Class, "brier": b, "weight": o.Weight, "at": fmtTime(now)}})
				}
			}
		}
		if len(adds) == 0 {
			continue
		}
		// The scores go into the reviews of the record, for people (COM-16).
		if p, err = loadProposal(ctx, tx, pid); err != nil {
			return err
		}
		for _, a := range adds {
			for _, r := range asList(p.Rec["reviews"]) {
				if rm := mmapAny(r); rm["task"] == a.task {
					rm["scores"] = append(asList(rm["scores"]), a.entry)
				}
			}
		}
		if err := c.saveProposal(ctx, tx, now, p); err != nil {
			return err
		}
	}
	if len(tasks) > 0 {
		if _, err := tx.Exec(ctx, `UPDATE forecasts SET checked_at = $2 WHERE task = ANY($1)`, tasks, now); err != nil {
			return err
		}
	}
	return nil
}

// rejectedVerdicts are the verdicts of the commission that reject a proposal: dismissed by a
// veto, a minimum or a low score, or parked by score (PRP-24, GOV-38).
var rejectedVerdicts = []string{"dismissed(veto)", "dismissed(below_min)", "dismissed(low_score)", "parked(score)"}

// lensOutcomes are the known outcomes of a lens for a proposal (COM-16): the trial outcome
// (better, unchanged, worse; undetermined, mixed and canceled give none); for a veto lens a
// decline as too risky, a confirmed attribution or the trial outcome worse as bad, a trial that
// ended otherwise as ok once the attribution window closed without an open candidate, and the
// blind sample answers with the weight 1/inclusion_prob; for cost whether the spend of the change
// subjects over the trial window exceeded the cost_eur of the review input; and the trial of a
// resubmission with the same values (PRP-24). People's decisions do not enter the other lenses.
func (c *Core) lensOutcomes(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, p *proposalRow, lens string, veto bool) ([]outcomeOf, error) {
	var out []outcomeOf
	trialOut := func(q *proposalRow, source, ref string) error {
		t := mmapAny(q.Rec["trial"])
		res := asString(t["outcome"])
		switch {
		case res == "" || res == "undetermined" || res == "mixed" || res == "canceled":
			return nil
		case veto && res == "worse":
			out = append(out, outcomeOf{Class: "bad", Source: source, Ref: ref, Weight: 1})
		case veto:
			// An ok outcome waits until no attribution can still come (GOV-13).
			var open, confirmed bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM attributions WHERE proposal = $1 AND kind = 'change' AND state = ANY($2)),
				EXISTS (SELECT 1 FROM attributions WHERE proposal = $1 AND kind = 'change' AND state = 'confirmed')`, q.ID, attributionOpen).Scan(&open, &confirmed); err != nil {
				return err
			}
			if open || confirmed {
				return nil
			}
			realized := parseTime(asString(mmapAny(q.Rec["realization"])["realized_at"]))
			if w, err := attributionWindowOpen(ctx, tx, now, cat, q.Tenant, q.ID, realized); err != nil || w {
				return err
			}
			out = append(out, outcomeOf{Class: "ok", Source: source, Ref: ref, Weight: 1})
		case lens == "cost":
			over, ok, err := costOverrun(ctx, tx, p, q)
			if err != nil || !ok {
				return err
			}
			out = append(out, outcomeOf{Class: map[bool]string{true: "over", false: "within"}[over], Source: source, Ref: ref, Weight: 1})
		default:
			out = append(out, outcomeOf{Class: res, Source: source, Ref: ref, Weight: 1})
		}
		return nil
	}
	if err := trialOut(p, "trial", ""); err != nil {
		return nil, err
	}
	if veto {
		d := mmapAny(p.Rec["decision"])
		if d["decision"] == "reject" && d["reason_code"] == "too_risky" && asString(d["decided_by"]) != "" {
			out = append(out, outcomeOf{Class: "bad", Source: "decline", Weight: 1})
		}
		rows, err := tx.Query(ctx, `SELECT id FROM attributions WHERE proposal = $1 AND kind = 'change' AND state IN ('confirmed', 'disputed') ORDER BY id`, p.ID)
		if err != nil {
			return nil, err
		}
		atts, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return nil, err
		}
		for _, a := range atts {
			out = append(out, outcomeOf{Class: "bad", Source: "attribution", Ref: a, Attribution: a, Weight: 1})
		}
		// A blind sample answer: "Would not accept" is bad, "Would accept" ok (DIG-6, COM-17).
		if ans := asString(mmapAny(p.Rec["sample_answer"])["answer"]); ans != "" {
			prob, _ := toFloat(mmapAny(p.Rec["sample"])["inclusion_prob"])
			if prob > 0 {
				out = append(out, outcomeOf{Class: map[bool]string{true: "bad", false: "ok"}[ans == "would_not_accept"], Source: "sample", Weight: 1 / prob})
			}
		}
	}
	// A rejected proposal is scored by a resubmission with the same values whose trial ended
	// (PRP-24); one with other values scores nothing.
	if slices.Contains(rejectedVerdicts, asString(mmapAny(p.Rec["verdict"])["outcome"])) {
		rows, err := tx.Query(ctx, `SELECT id FROM proposals WHERE tenant = $1 AND record->'resubmission'->>'of' = $2
			AND (record->'resubmission'->>'same_values')::bool AND record->'trial'->>'outcome' IS NOT NULL ORDER BY created_at, id`, p.Tenant, p.ID)
		if err != nil {
			return nil, err
		}
		resubs, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return nil, err
		}
		for _, id := range resubs {
			q, err := loadProposal(ctx, tx, id)
			if err != nil {
				return nil, err
			}
			if err := trialOut(q, "resubmission", id); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

// reviewCostEUR is the cost_eur of the review input: the author's one-time cost and the monthly
// cost over the trial window (COM-15, COM-16).
func reviewCostEUR(body map[string]any) float64 {
	cost := mmapAny(body["cost"])
	one, _ := toFloat(cost["one_time_eur"])
	month, _ := toFloat(cost["monthly_eur"])
	return one + month*float64(trialDays)/30
}

// costOverrun says whether the spend of the change subjects over the trial window of q exceeds
// the cost_eur of p's review input (COM-16): the spend of the tasks of the task_type subjects of
// the trial and of the problem. Without a task_type subject the spend is not measured and the
// lens gets no score (undetermined).
func costOverrun(ctx context.Context, tx pgx.Tx, p, q *proposalRow) (bool, bool, error) {
	t := mmapAny(q.Rec["trial"])
	start := parseTime(asString(mmapAny(t["window"])["starts_at"]))
	if start.IsZero() {
		return false, false, nil
	}
	end := start.AddDate(0, 0, trialDays)
	var types []string
	for _, s := range append(asList(t["subjects"]), asList(mmapAny(mmapAny(q.Rec["body"])["problem"])["subjects"])...) {
		if k, id, ok := cutSubject(asString(s)); ok && k == "task_type" && !slices.Contains(types, id) {
			types = append(types, id)
		}
	}
	if len(types) == 0 {
		return false, false, nil
	}
	var spent int64
	if err := tx.QueryRow(ctx, `SELECT coalesce(sum(s.eur_micros), 0)::bigint FROM spend s JOIN tasks k ON k.id = s.task
		WHERE s.tenant = $1 AND k.type = ANY($2) AND s.state <> 'reserved' AND s.time >= $3 AND s.time < $4`,
		q.Tenant, types, start, end).Scan(&spent); err != nil {
		return false, false, err
	}
	return spent > money.FromEUR(reviewCostEUR(mmapAny(p.Rec["body"]))), true, nil
}

// cutSubject splits a subject kind:id.
func cutSubject(s string) (string, string, bool) {
	for i := 0; i < len(s); i++ {
		if s[i] == ':' {
			return s[:i], s[i+1:], true
		}
	}
	return "", "", false
}

// accuracy is the accuracy of a lens executor over its forecasts scored in the window (COM-17):
// 1 - B/B_ref, cut to -1..1, with the number of scored forecasts, the missed ones and the
// standard error of the value; OK is false with fewer than 10 scored forecasts (insufficient).
type accuracy struct {
	Lens   string
	N      int
	Missed int
	Value  float64
	SE     float64
	OK     bool
}

// view is the accuracy as people see it: the number, or insufficient.
func (a accuracy) view() any {
	if !a.OK {
		return "insufficient"
	}
	return math.Round(a.Value*1000) / 1000
}

func (a accuracy) text() string {
	if !a.OK {
		return fmt.Sprintf("%s insufficient (%d scored)", a.Lens, a.N)
	}
	return fmt.Sprintf("%s %.3f (%d scored, %d missed)", a.Lens, a.Value, a.N, a.Missed)
}

// tied says whether two accuracies differ by less than one standard error of the difference.
func tied(a, b accuracy) bool {
	return math.Abs(a.Value-b.Value) < math.Sqrt(a.SE*a.SE+b.SE*b.SE)
}

// lensAccuracy computes the accuracy of a seat holder in its lens (COM-17): its seat forecasts
// scored in the last 180 days against the reference forecast of the base rates of the lens
// outcomes in the tenant over the same days, on the same cases. A score from an attribution that
// is not confirmed (disputed) does not enter. Shadow reviews never count for a seat.
func lensAccuracy(ctx context.Context, q store.Querier, now time.Time, tenant, lens, bot string) (accuracy, error) {
	out := accuracy{Lens: lens}
	rows, err := q.Query(ctx, `SELECT s.task, s.proposal, s.source, s.ref, s.outcome, s.brier, s.weight, coalesce(f.bot, ''), f.executor, f.state <> 'done' OR f.disagrees
		FROM forecast_scores s JOIN forecasts f ON f.task = s.task
		WHERE s.tenant = $1 AND s.lens = $2 AND s.scored_at >= $3
		AND (s.attribution IS NULL OR EXISTS (SELECT 1 FROM attributions a WHERE a.id = s.attribution AND a.state = 'confirmed'))
		ORDER BY s.task, s.source, s.ref`, tenant, lens, now.AddDate(0, 0, -accuracyDays))
	if err != nil {
		return out, err
	}
	type scored struct {
		task, outcome      string
		brier, weight      float64
		mine, missed, seat bool
	}
	var all []scored
	rates := map[string]float64{}
	cases := map[string]bool{}
	total := 0.0
	for rows.Next() {
		var s scored
		var proposal, source, ref, fbot, executor string
		if err := rows.Scan(&s.task, &proposal, &source, &ref, &s.outcome, &s.brier, &s.weight, &fbot, &executor, &s.missed); err != nil {
			rows.Close()
			return out, err
		}
		s.mine, s.seat = fbot == bot, executor == "seat"
		all = append(all, s)
		// The base rates count each case once, whoever reviewed it.
		if k := proposal + "/" + source + "/" + ref; !cases[k] {
			cases[k] = true
			rates[s.outcome] += s.weight
			total += s.weight
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, err
	}
	if total == 0 {
		return out, nil
	}
	classes := slices.Sorted(mapKeys(rates))
	ref := map[string]float64{}
	for _, c := range classes {
		ref[c] = rates[c] / total
	}
	var w, b, bref float64
	var losses []float64
	tasks := map[string]bool{}
	missed := map[string]bool{}
	for _, s := range all {
		if !s.mine || !s.seat {
			continue
		}
		tasks[s.task] = true
		if s.missed {
			missed[s.task] = true
		}
		r := 0.0
		for _, c := range classes {
			o := 0.0
			if c == s.outcome {
				o = 1
			}
			r += (ref[c] - o) * (ref[c] - o)
		}
		// A class the lens never saw in the tenant counts for the forecast too: the reference
		// gives it 0.
		w += s.weight
		b += s.weight * s.brier
		bref += s.weight * r
		losses = append(losses, s.brier)
	}
	out.N, out.Missed = len(tasks), len(missed)
	if w == 0 {
		return out, nil
	}
	b, bref = b/w, bref/w
	switch {
	case bref > 0:
		out.Value = 1 - b/bref
	case b == 0:
		out.Value = 0
	default:
		out.Value = -1
	}
	out.Value = max(-1, min(1, out.Value))
	if n := len(losses); n > 1 && bref > 0 {
		mean := 0.0
		for _, l := range losses {
			mean += l
		}
		mean /= float64(n)
		v := 0.0
		for _, l := range losses {
			v += (l - mean) * (l - mean)
		}
		out.SE = math.Sqrt(v/float64(n-1)) / bref / math.Sqrt(float64(n))
	}
	out.OK = out.N >= minAccuracyForecasts
	return out, nil
}

// median is the median of a list of values, 0 for none.
func median(vs []float64) float64 {
	if len(vs) == 0 {
		return 0
	}
	s := slices.Clone(vs)
	sort.Float64s(s)
	if n := len(s); n%2 == 1 {
		return s[n/2]
	} else {
		return (s[n/2-1] + s[n/2]) / 2
	}
}
