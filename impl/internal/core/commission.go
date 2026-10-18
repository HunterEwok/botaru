// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/manifest"
	"github.com/HunterEwok/botaru/impl/internal/money"
)

const (
	// The deadlines of the proposal flow when the tenant sets none (PRP-6, COM-6).
	defaultParkTTL       = 30 * 24 * time.Hour
	defaultRecommendTTL  = 28 * 24 * time.Hour
	defaultReviewWaitMax = 7 * 24 * time.Hour
	// roleReviewShare is the most of the review line one author role may take a month (COM-6).
	roleReviewShare = 0.3
	// maxReviewRounds is how many review rounds one proposal gets.
	maxReviewRounds = 3
	reviewSource    = "proposal"
)

// tenantSpecOf is the spec of the first manifest of a kind in a tenant, by name.
func tenantSpecOf(cat *Catalog, kind, tenant string) map[string]any {
	var keys []manifest.Key
	for k := range cat.RawGlobals {
		if k.Kind == kind && k.Tenant == tenant {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return nil
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].Name < keys[j].Name })
	spec, _ := cat.RawGlobals[keys[0]]["spec"].(map[string]any)
	return spec
}

// lensDef is a skeleton lens of spec/model/lenses.yaml.
type lensDef struct {
	ID, ReviewType string
	Veto, KindNone bool
}

func (c *Core) lensDefs() map[string]lensDef {
	out := map[string]lensDef{}
	reg, err := c.spec.Registry("spec/model/lenses.yaml")
	if err != nil {
		return out
	}
	for _, e := range reg.Entries {
		if e["since"] != "v0.1" || e["subject"] != "proposal" {
			continue
		}
		out[asString(e["id"])] = lensDef{ID: asString(e["id"]), ReviewType: asString(e["review_type"]), Veto: e["veto"] == true, KindNone: e["kind_none"] == true}
	}
	return out
}

// lensExecutor is who reviews a lens: the fallback role of the charter seat of the lens (the
// seat holders come with bot identity), or for a lens without a seat a role of profile
// reviewer that claims the lens type, or nothing (COM-7, COM-8).
type lensExecutor struct {
	Lens, Type, Role, Source string
	// Seat is the held seat that reviews the lens, with Source seat.
	Seat string
}

// commissionLenses are the lenses a proposal is reviewed by, with their executors: the lenses
// of the charter seats and the veto lenses always; for kind none only value and security
// (COM-7, COM-8, COM-14). A lens whose executor would break independence has none (COM-2).
func (c *Core) commissionLenses(cat *Catalog, tenant string, kindNone bool, authorRole string) ([]string, map[string]lensExecutor) {
	defs := c.lensDefs()
	gov := tenantSpecOf(cat, "Governance", tenant)
	com, _ := gov["commission"].(map[string]any)
	seats, _ := com["seats"].([]any)
	lenses := []string{}
	exec := map[string]lensExecutor{}
	seated := map[string]bool{}
	add := func(l string) {
		if !slices.Contains(lenses, l) {
			lenses = append(lenses, l)
		}
	}
	for _, l := range slices.Sorted(mapKeys(defs)) {
		if defs[l].Veto {
			add(l)
		}
	}
	for _, s := range seats {
		sm, _ := s.(map[string]any)
		l := asString(sm["lens"])
		d, ok := defs[l]
		if !ok {
			continue
		}
		add(l)
		seated[l] = true
		if _, has := exec[l]; has {
			continue
		}
		role := asString(sm["fallback_role"])
		if c.independentReviewer(cat, role, d.ReviewType, authorRole) {
			exec[l] = lensExecutor{Lens: l, Type: d.ReviewType, Role: role, Source: "fallback_role"}
		}
	}
	// A lens without a seat goes to a reviewer role that claims its type, the first by name.
	for _, l := range lenses {
		if seated[l] {
			continue
		}
		for _, rn := range slices.Sorted(mapKeys(cat.Roles)) {
			if cat.Roles[rn].Profile == "reviewer" && c.independentReviewer(cat, rn, defs[l].ReviewType, authorRole) {
				exec[l] = lensExecutor{Lens: l, Type: defs[l].ReviewType, Role: rn, Source: "fallback_role"}
				break
			}
		}
	}
	if kindNone {
		lenses = slices.DeleteFunc(lenses, func(l string) bool { return !defs[l].KindNone })
	}
	sort.Strings(lenses)
	return lenses, exec
}

// seatExecutors puts a held seat in front of the fallback of each lens it reviews: the seat role
// claims the lens type and is independent of the author, and the holder is not the author's
// bot; a seated lens counts without a fallback role (COM-2, COM-7, IDN-10).
func (c *Core) seatExecutors(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, p *proposalRow, lenses []string, exec map[string]lensExecutor) (map[string]lensExecutor, error) {
	out := maps.Clone(exec)
	if out == nil {
		out = map[string]lensExecutor{}
	}
	sr := seatRole(cat, p.Tenant)
	if sr == "" {
		return out, nil
	}
	defs := c.lensDefs()
	origin, _ := p.Rec["origin"].(map[string]any)
	author := asString(origin["acting_bot"])
	for _, l := range lenses {
		typ := defs[l].ReviewType
		if e, ok := exec[l]; ok {
			typ = e.Type
		}
		if typ == "" || !c.independentReviewer(cat, sr, typ, p.Role) {
			continue
		}
		seat, err := c.seatExecutor(ctx, tx, now, p.Tenant, l)
		if err != nil {
			return nil, err
		}
		if seat == nil || (author != "" && seat.Holder == author) {
			continue
		}
		out[l] = lensExecutor{Lens: l, Type: typ, Role: sr, Source: "seat", Seat: seat.ID}
	}
	return out, nil
}

// vetoCovered says whether every veto lens among lenses has an executor (COM-7).
func (c *Core) vetoCovered(lenses []string, exec map[string]lensExecutor) bool {
	defs := c.lensDefs()
	for _, l := range lenses {
		if _, ok := exec[l]; !ok && defs[l].Veto {
			return false
		}
	}
	return true
}

// independentReviewer: the reviewer role is not the author role, claims the review type, and
// its model profile is not the author's analysis profile (COM-2).
func (c *Core) independentReviewer(cat *Catalog, role, typ, authorRole string) bool {
	r, ok := cat.Roles[role]
	if !ok || role == authorRole || !slices.Contains(r.Claims, typ) {
		return false
	}
	if a, ok := cat.Roles[authorRole]; ok && a.Models["analysis"] != "" && r.Models["base"] == a.Models["analysis"] {
		return false
	}
	return true
}

// proposalLoop moves proposals by the transition table (PRP-6): code checks, reviews and the
// verdict, and the deadlines.
func (c *Core) proposalLoop(ctx context.Context) error {
	cat, err := c.Catalog(ctx)
	if err != nil {
		return err
	}
	var ids []string
	if err := c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		rows, err := tx.Query(ctx, `SELECT id FROM proposals WHERE state IN ('screened', 'in_review', 'recommended', 'parked', 'presented', 'realizing', 'sample_presented', 'sample_judged')
			OR state = 'realized' AND record->'trial'->>'outcome' IS NULL ORDER BY created_at, id`)
		if err != nil {
			return err
		}
		ids, err = pgx.CollectRows(rows, pgx.RowTo[string])
		return err
	}); err != nil {
		return err
	}
	var errs []error
	for _, id := range ids {
		if err := c.tx(ctx, func(tx pgx.Tx, now time.Time) error { return c.stepProposal(ctx, tx, now, cat, id) }); err != nil {
			errs = append(errs, fmt.Errorf("proposal %s: %w", id, err))
		}
	}
	// The audit sample of the last month is drawn before the digest that presents it (DIG-5).
	if err := c.sampleDraws(ctx, cat); err != nil {
		errs = append(errs, err)
	}
	// The digest goes after the moves, so a proposal recommended in this pass can be in it.
	if err := c.digestLoop(ctx, cat); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// proposalRow is a proposal record under its lock.
type proposalRow struct {
	ID, Tenant, Role, State string
	Rec                     map[string]any
	StateAt                 time.Time
	Nonce                   string
	PresentedAt             *time.Time
}

func (p *proposalRow) body() proposalBody {
	var b proposalBody
	raw, _ := json.Marshal(p.Rec["body"])
	_ = json.Unmarshal(raw, &b)
	return b
}

func (p *proposalRow) kind() string { return p.body().Change.Kind }

func (p *proposalRow) reason() string { return asString(p.Rec["state_reason"]) }

// moveProposal writes a transition of the table with its reason. The record carries the state;
// the events catalog has no proposal state event in v0.1.
func (c *Core) moveProposal(ctx context.Context, tx pgx.Tx, now time.Time, p *proposalRow, state, reason string) error {
	p.Rec["state"] = state
	if reason != "" {
		p.Rec["state_reason"] = reason
	} else {
		p.Rec["state_reason"] = nil
	}
	p.State, p.StateAt = state, now
	// park_ttl counts from the first park: a parked proposal screened and reviewed again and
	// parked again keeps its clock, so a proposal that cannot pass leaves by park_ttl (PRP-6);
	// waiting_since is the start of a wait in screened (COM-6).
	switch state {
	case "parked":
		if p.Rec["parked_since"] == nil {
			p.Rec["parked_since"] = fmtTime(now)
		}
	case "screened", "in_review":
	default:
		delete(p.Rec, "parked_since")
	}
	if state != "screened" {
		delete(p.Rec, "waiting_since")
	}
	raw, err := json.Marshal(p.Rec)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE proposals SET state = $2, record = $3, updated_at = $4, state_at = $4 WHERE id = $1`, p.ID, state, raw, now); err != nil {
		return err
	}
	return syncVariants(ctx, tx, now, p)
}

// since reads a time field of the record, or def.
func (p *proposalRow) since(field string, def time.Time) time.Time {
	if t, err := time.Parse(time.RFC3339, asString(p.Rec[field])); err == nil {
		return t
	}
	return def
}

func (c *Core) saveProposal(ctx context.Context, tx pgx.Tx, now time.Time, p *proposalRow) error {
	raw, err := json.Marshal(p.Rec)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE proposals SET record = $2, updated_at = $3 WHERE id = $1`, p.ID, raw, now); err != nil {
		return err
	}
	return syncVariants(ctx, tx, now, p)
}

func (c *Core) stepProposal(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, id string) error {
	p := &proposalRow{ID: id}
	var raw []byte
	var nonce *string
	err := tx.QueryRow(ctx, `SELECT tenant, role, state, record, state_at, nonce, presented_at FROM proposals WHERE id = $1 FOR UPDATE SKIP LOCKED`, id).
		Scan(&p.Tenant, &p.Role, &p.State, &raw, &p.StateAt, &nonce, &p.PresentedAt)
	p.Nonce = deref(nonce)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, &p.Rec); err != nil {
		return err
	}
	tv := cat.Tenants[p.Tenant]
	if tv == nil {
		return nil
	}
	ins := tv.Spec.Insights
	switch p.State {
	case "screened":
		return c.screenProposal(ctx, tx, now, cat, tv, p)
	case "in_review":
		return c.reviewProposal(ctx, tx, now, cat, tv, p)
	case "presented":
		return c.presentedStep(ctx, tx, now, tv, p)
	case "sample_presented", "sample_judged":
		return c.sampleStep(ctx, tx, now, tv, p)
	case "realizing":
		return c.realizingStep(ctx, tx, now, tv, p)
	case "realized":
		return c.trialStep(ctx, tx, now, cat, tv, p)
	case "recommended":
		if now.Sub(p.StateAt) > durationOr(ins.RecommendTTL, defaultRecommendTTL) {
			return c.moveProposal(ctx, tx, now, p, "expired", "")
		}
	case "parked":
		if now.Sub(p.since("parked_since", p.StateAt)) > durationOr(ins.ParkTTL, defaultParkTTL) {
			return c.moveProposal(ctx, tx, now, p, "expired", "")
		}
		lenses, exec := c.commissionLenses(cat, p.Tenant, p.kind() == "none", p.Role)
		exec, err := c.seatExecutors(ctx, tx, now, cat, p, lenses, exec)
		if err != nil {
			return err
		}
		switch p.reason() {
		case "review_budget":
			// Money for reviews came back: the proposal is screened again (COM-6).
			if c.reviewsFit(ctx, tx, now, cat, tv, p, lenses, exec) == nil {
				return c.moveProposal(ctx, tx, now, p, "screened", "")
			}
		case "conflict":
			// The lock is released when the holding change's trial ends (PRP-9).
			holders, err := lockHolders(ctx, tx, p.Tenant, p.ID, lockKeys(p.body()))
			if err != nil {
				return err
			}
			if len(holders) == 0 {
				p.Rec["conflicts_with"] = []any{}
				return c.moveProposal(ctx, tx, now, p, "screened", "")
			}
		case "veto_lens_missing":
			// The veto lens has an executor again (COM-7); a proposal gets at most
			// maxReviewRounds rounds, so an executor that never delivers does not loop it.
			if c.vetoCovered(lenses, exec) && p.round() < maxReviewRounds {
				return c.moveProposal(ctx, tx, now, p, "screened", "")
			}
		}
	}
	return nil
}

// screenProposal runs the code checks in their order before any review: the empty change,
// feasibility, a veto lens executor and the review money (COM-5, COM-6, COM-7). Schema,
// evidence and duplicates were checked at creation.
func (c *Core) screenProposal(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, tv *TenantView, p *proposalRow) error {
	b := p.body()
	if noOp(cat, tv, b.Change) {
		return c.moveProposal(ctx, tx, now, p, "dismissed", "no_op")
	}
	// A field another change holds waits until its trial ends (PRP-9).
	if holders, err := lockHolders(ctx, tx, p.Tenant, p.ID, lockKeys(b)); err != nil {
		return err
	} else if len(holders) > 0 {
		p.Rec["conflicts_with"] = holders
		return c.moveProposal(ctx, tx, now, p, "parked", "conflict")
	}
	if ok, err := hasDecider(ctx, tx, tv, b.Category); err != nil {
		return err
	} else if !ok {
		return c.moveProposal(ctx, tx, now, p, "dismissed", "other")
	}
	lenses, exec := c.commissionLenses(cat, p.Tenant, b.Change.Kind == "none", p.Role)
	exec, err := c.seatExecutors(ctx, tx, now, cat, p, lenses, exec)
	if err != nil {
		return err
	}
	// Without an executor of a veto lens no review goes out: the proposal enters in_review
	// with none and the verdict parks it as veto_lens_missing (COM-4, COM-7).
	if !c.vetoCovered(lenses, exec) {
		exec = nil
	}
	// A proposal that cannot start its reviews waits in screened. Only missing money parks it,
	// after review_wait_max from the start of the wait (COM-6); any other refusal (a missing
	// group cap, a hold, capacity) keeps it waiting with the code on the record.
	wait := func(pr *Problem) error {
		if p.Rec["waiting_since"] == nil {
			p.Rec["waiting_since"] = fmtTime(now)
		}
		p.Rec["waiting_for"] = pr.Reason
		// Any refusal parks after review_wait_max, so a gap in the configuration does not hold
		// the proposal and the open limits forever; waiting_for keeps the refusal code.
		if now.Sub(p.since("waiting_since", now)) > durationOr(tv.Spec.Insights.ReviewWaitMax, defaultReviewWaitMax) {
			return c.moveProposal(ctx, tx, now, p, "parked", "review_budget")
		}
		return c.saveProposal(ctx, tx, now, p)
	}
	// The farm manager's advisory lens comes after the others; a review line that is short
	// for it, or a manager review that cannot go out, skips it and marks the verdict (GOV-25).
	lenses, exec, manager, err := c.withManagerLens(ctx, tx, now, cat, p, lenses, exec)
	if err != nil {
		return err
	}
	delete(p.Rec, "no_manager_vote")
	skipManager := func() {
		lenses, exec = withoutManagerLens(lenses, exec)
		manager = false
		p.Rec["no_manager_vote"] = true
	}
	if len(exec) > 0 {
		pr := c.reviewsFit(ctx, tx, now, cat, tv, p, lenses, exec)
		if pr != nil && manager {
			skipManager()
			pr = c.reviewsFit(ctx, tx, now, cat, tv, p, lenses, exec)
		}
		if pr != nil {
			return wait(pr)
		}
	}
	// The reviews go out together under a savepoint: either every lens task with its reserve,
	// or none (COM-6).
	var reviews []any
	var reserved float64
	for {
		sp, err := tx.Begin(ctx)
		if err != nil {
			return err
		}
		reviews, reserved, err = c.enqueueReviews(ctx, sp, now, cat, tv, p, lenses, exec)
		if err != nil {
			_ = sp.Rollback(ctx)
			var pr *Problem
			if errors.As(err, &pr) && manager {
				skipManager()
				continue
			}
			if errors.As(err, &pr) {
				return wait(pr)
			}
			return err
		}
		if err := sp.Commit(ctx); err != nil {
			return err
		}
		break
	}
	p.Rec["review_lenses"] = lenses
	p.Rec["reviews"] = reviews
	// The role share counts the spend of earlier rounds and the reserve of this one in the
	// month of the first round (COM-6).
	spent, _ := toFloat(p.Rec["review_spent_eur"])
	p.Rec["review_reserved_eur"] = spent + reserved
	if p.Rec["review_started_at"] == nil {
		p.Rec["review_started_at"] = fmtTime(now)
	}
	p.Rec["review_round_at"] = fmtTime(now)
	if len(reviews) > 0 {
		p.Rec["review_round"] = p.round() + 1
	}
	delete(p.Rec, "waiting_for")
	return c.moveProposal(ctx, tx, now, p, "in_review", "")
}

// reviewsFit checks the review line for the sum of the review estimates and the author role's
// share of the line this month (COM-6).
func (c *Core) reviewsFit(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, tv *TenantView, p *proposalRow, lenses []string, exec map[string]lensExecutor) *Problem {
	sum := reviewPrice(cat, lenses, exec)
	if pr := c.checkBudget(ctx, tx, now, tv, budgetAsk{subject: "system", extra: sum, line: "review"}); pr != nil {
		return pr
	}
	line := tv.Spec.Budgets.Lines["review"]
	if line.MonthEUR == nil {
		return refuse("budget_exhausted", "the review line is 0")
	}
	var used float64
	// The month is the tenant's calendar month, as for the budget windows (TIM-2).
	loc := time.UTC
	if l, err := time.LoadLocation(tv.Spec.Timezone); err == nil && tv.Spec.Timezone != "" {
		loc = l
	}
	local := now.In(loc)
	start := time.Date(local.Year(), local.Month(), 1, 0, 0, 0, 0, loc)
	if err := tx.QueryRow(ctx, `SELECT coalesce(sum((record->>'review_reserved_eur')::float8), 0) FROM proposals
		WHERE tenant = $1 AND role = $2 AND id <> $3 AND (record->>'review_started_at')::timestamptz >= $4`, p.Tenant, p.Role, p.ID, start).Scan(&used); err != nil {
		return refuse("not_ready", "store")
	}
	// The proposal's own earlier rounds count when they started this month.
	if own, _ := toFloat(p.Rec["review_spent_eur"]); own > 0 && !p.since("review_started_at", time.Time{}).Before(start) {
		used += own
	}
	if used+float64(sum)/1e6 > roleReviewShare**line.MonthEUR+1e-9 {
		return refuse("budget_exhausted", "the author role's share of the review line is used up")
	}
	return nil
}

// enqueueReviews enqueues one review task per lens with an executor, by the key
// system:review:<proposal>:<lens> (COM-2, IDM-2), with the input block the core builds
// (COM-3); the task goes only to the lens executor role and never to the author's worker.
func (c *Core) enqueueReviews(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, tv *TenantView, p *proposalRow, lenses []string, exec map[string]lensExecutor) ([]any, float64, error) {
	input := reviewInput(p)
	reviews := []any{}
	reserved := 0.0
	origin, _ := p.Rec["origin"].(map[string]any)
	for _, l := range lenses {
		e, ok := exec[l]
		if !ok {
			continue
		}
		body := map[string]any{"type": e.Type, "goal": "Review the proposal through the " + l + " lens.", "params": map[string]any{"input": input}}
		// The review carries the proposal's mark; enqueue checks data:<mark> for the system
		// initiator against it (COM-3, PRM-6).
		res, err := c.enqueue(ctx, tx, now, cat, enqueueInput{Tenant: p.Tenant, Type: e.Type, Body: body,
			IdempotencyKey: reviewKey(p, l), Initiator: "system", EnqueuedBy: "system:proposal", Source: reviewSource, SourceID: p.ID,
			ContinuesMark: asString(p.Rec["data_mark"]), EstimateMin: money.FromEUR(cat.TaskTypes[e.Type].TreeBudgetEUR)})
		if err != nil {
			return nil, 0, err
		}
		if res.Rejected != nil {
			return nil, 0, res.Rejected
		}
		// A held seat of the lens reviews it on behalf of its holder, on workers of the seat
		// role; a vacant seat, a holder under a hold of people or the author's own bot leaves
		// it to the fallback role (COM-7, IDN-10).
		extra := map[string]any{"lens": l, "proposal": p.ID, "executor_role": e.Role, "not_worker": asString(origin["worker"])}
		review := map[string]any{"lens": l, "task": res.TaskID, "executor": e.Source, "state": "pending"}
		// A review enqueued on a v0.3 core hands in review-result.v2 with its forecast, which
		// code scores (COM-15, COM-16); the mark keeps the reviews of an earlier core's
		// records on review-result.v1.
		extra["review_schema"] = "v2"
		review["v03"], review["enqueued_at"] = true, fmtTime(now)
		if e.Source == "seat" {
			extra["seat"] = e.Seat
			// The holder and the seat epoch the review is written for: only reviews of the
			// holder in the current seat epoch answer for a link (GOV-41).
			if s, err := loadSeat(ctx, tx, p.Tenant, e.Seat, false); err != nil {
				return nil, 0, err
			} else if s != nil {
				review["holder"], review["seat"], review["seat_epoch"] = s.Holder, s.ID, s.Epoch
			}
			// The author's bot never reviews its own proposal, even if it takes the seat
			// before the review is claimed (COM-2); claim checks this against the holder.
			if a := asString(origin["acting_bot"]); a != "" {
				extra["not_bot"] = a
			}
		}
		raw, err := json.Marshal(extra)
		if err != nil {
			return nil, 0, err
		}
		if _, err := tx.Exec(ctx, `UPDATE tasks SET record = record || $2::jsonb WHERE id = $1`, res.TaskID, raw); err != nil {
			return nil, 0, err
		}
		reserved += cat.TaskTypes[e.Type].TreeBudgetEUR
		reviews = append(reviews, review)
	}
	return reviews, reserved, nil
}

// reviewInput is the data block of a review: closed fields of the body, the values the core
// reread, the change in code words, the author level and the marks; never the note, the
// author's identity or other lenses' scores (COM-2, COM-3). From v0.3 it has no author level
// and never says whether the proposal is a resubmission; it carries cost_eur, the spend the cost
// lens forecasts against (COM-15, COM-16, PRP-24).
func reviewInput(p *proposalRow) map[string]any {
	body, _ := p.Rec["body"].(map[string]any)
	in := map[string]any{}
	for _, k := range []string{"category", "problem", "change", "benefit", "cost", "risk"} {
		if v, ok := body[k]; ok {
			in[k] = v
		}
	}
	in["evidence"] = p.Rec["evidence_seen"]
	in["cost_eur"] = reviewCostEUR(body)
	in["self_evidence"] = p.Rec["self_evidence"] == true
	us, _ := p.Rec["untrusted_seen"].(map[string]any)
	in["untrusted_seen"] = us["value"] == true
	in["guarded"] = p.Rec["guarded"] == true
	in["conflicts_with"] = p.Rec["conflicts_with"]
	return in
}

// reviewProposal reads the fate of each lens task: done with its result, failed, or late after
// review_deadline; when no review is pending the code verdict moves the proposal (COM-4).
func (c *Core) reviewProposal(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, tv *TenantView, p *proposalRow) error {
	policy := policyFrom(tenantSpecOf(cat, "Commission", p.Tenant))
	started := p.since("review_round_at", p.since("review_started_at", p.StateAt))
	deadline := started.Add(durationOr(policy.ReviewDeadline, 24*time.Hour))
	reviews, _ := p.Rec["reviews"].([]any)
	pending := false
	changed := false
	for _, r := range reviews {
		rm := r.(map[string]any)
		if rm["state"] != "pending" {
			continue
		}
		var state string
		var result []byte
		var done time.Time
		err := tx.QueryRow(ctx, `SELECT state, result, updated_at FROM tasks WHERE id = $1`, rm["task"]).Scan(&state, &result, &done)
		if errors.Is(err, pgx.ErrNoRows) {
			state = "failed"
		} else if err != nil {
			return err
		}
		switch {
		// A review counts by the time it was handed in, not by when the loop reads it.
		case state == "completed" && (len(result) == 0 || string(result) == "null"):
			// A purged task has no result left: the review failed.
			rm["state"], changed = "failed", true
		case state == "completed" && done.After(deadline):
			rm["state"], changed = "late", true
		case state == "completed":
			var res map[string]any
			schema := reviewResultV1
			if rm["v03"] == true {
				schema = reviewResultV2
			}
			lens := asString(rm["lens"])
			switch {
			case json.Unmarshal(result, &res) != nil || c.spec.Validate(schema, res) != nil || res["lens"] != rm["lens"] || res["shadow"] == true:
				rm["state"] = "schema_invalid"
			case schema == reviewResultV2 && !forecastValid(lens, c.lensDefs()[lens].Veto, res):
				// A forecast that does not sum to 1 or contradicts the score makes the review
				// invalid: it counts as missing (COM-15, COM-4).
				rm["state"] = "schema_invalid"
			case schema == reviewResultV2:
				// The record keeps the review-result fields; the forecast beside them, seen only
				// by people and code (COM-16, GOV-46).
				rm["state"], rm["result"], rm["forecast"] = "done", reviewV1View(res), res["forecast"]
			default:
				rm["state"], rm["result"] = "done", res
			}
			changed = true
		case state == "failed" || state == "canceled":
			rm["state"], changed = "failed", true
		case now.After(deadline):
			rm["state"], changed = "late", true
		default:
			pending = true
		}
	}
	if pending {
		if changed {
			return c.saveProposal(ctx, tx, now, p)
		}
		return nil
	}
	b := p.body()
	in := verdictInput{KindNone: b.Change.Kind == "none", Guarded: p.Rec["guarded"] == true, VetoLens: map[string]bool{}}
	risk, _ := mmapAny(p.Rec["body"])["risk"].(map[string]any)
	in.Risk = asString(risk["level"])
	if in.Risk == "" {
		in.Risk = "low"
	}
	us, _ := p.Rec["untrusted_seen"].(map[string]any)
	in.Untrusted = us["value"] == true
	defs := c.lensDefs()
	// The verdict reads the lenses fixed at screening, so the same reviews give the same verdict
	// whatever the charter says now (COM-4).
	var lenses []string
	for _, l := range asList(p.Rec["review_lenses"]) {
		lenses = append(lenses, asString(l))
	}
	in.Lenses = lenses
	for _, l := range lenses {
		in.VetoLens[l] = defs[l].Veto
	}
	for _, r := range reviews {
		rm := r.(map[string]any)
		lr := lensReview{Lens: asString(rm["lens"]), State: asString(rm["state"])}
		if res, ok := rm["result"].(map[string]any); ok {
			n, _ := toFloat(res["score"])
			lr.Score, lr.Veto, lr.Shadow = int(n), res["veto"] == true, res["shadow"] == true
		}
		in.Reviews = append(in.Reviews, lr)
	}
	// The author role's share counts what the reviews spent once they are over (COM-6).
	tasks := []string{}
	for _, r := range reviews {
		tasks = append(tasks, asString(r.(map[string]any)["task"]))
	}
	var spent int64
	if err := tx.QueryRow(ctx, `SELECT coalesce(sum(eur_micros), 0) FROM spend WHERE root = ANY($1) AND unit <> 'task' AND state <> 'reserved'`, tasks).Scan(&spent); err != nil {
		return err
	}
	total, _ := toFloat(p.Rec["review_spent_eur"])
	total += float64(spent) / 1e6
	p.Rec["review_spent_eur"], p.Rec["review_reserved_eur"] = total, total
	v := verdict(policy, in)
	v["computed_at"] = fmtTime(now)
	if p.Rec["no_manager_vote"] == true {
		v["no_manager_vote"] = true
	}
	p.Rec["verdict"] = v
	if err := c.recordForecasts(ctx, tx, now, p); err != nil {
		return err
	}
	state, reason, _ := strings.Cut(asString(v["outcome"]), "(")
	reason = strings.TrimSuffix(reason, ")")
	return c.moveProposal(ctx, tx, now, p, state, reason)
}

func (p *proposalRow) round() int {
	n, _ := toFloat(p.Rec["review_round"])
	return int(n)
}

// reviewKey is the idempotency key of a lens review: system:review:<proposal>:<lens> for the
// first round (IDM-2), with the round number after a proposal is screened again, so a new round
// sends new tasks.
func reviewKey(p *proposalRow, lens string) string {
	k := "system:review:" + p.ID + ":" + lens
	if n := p.round() + 1; n > 1 {
		k += fmt.Sprintf(":%d", n)
	}
	return k
}

func mmapAny(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

// hasDecider says whether the tenant has an active person who may decide on the category
// (COM-5 feasibility, DIG-3).
func hasDecider(ctx context.Context, tx pgx.Tx, tv *TenantView, category string) (bool, error) {
	var groups []string
	for _, g := range slices.Sorted(mapKeys(tv.Groups)) {
		if hasCap(tv, groupClosure(tv, []string{g}), "proposal.decide:"+category) {
			groups = append(groups, g)
		}
	}
	if len(groups) == 0 {
		return false, nil
	}
	var n int
	err := tx.QueryRow(ctx, `SELECT count(*) FROM principals p JOIN memberships m ON m.tenant = p.tenant AND m.principal = p.id
		WHERE p.tenant = $1 AND p.state = 'active' AND p.kind = 'human' AND m.state = 'active' AND (m.until IS NULL OR m.until > botaru_now()) AND m.grp = ANY($2)`,
		tv.Name, groups).Scan(&n)
	return n > 0, err
}

// noOp says whether every value of a manifest change is already in place (COM-5).
func noOp(cat *Catalog, tv *TenantView, ch proposalChange) bool {
	if ch.Kind != "manifest_change" || ch.ManifestChange == nil {
		return false
	}
	for _, op := range ch.ManifestChange.Ops {
		cur, ok := currentValue(cat, tv, op)
		if !ok || fmt.Sprint(cur) != fmt.Sprint(op.Value) {
			return false
		}
	}
	return true
}

// currentValue is the value of the field a set operation names, as applied now.
func currentValue(cat *Catalog, tv *TenantView, op manifestOp) (any, bool) {
	switch op.Target.Kind {
	case "Schedule":
		raw := cat.RawGlobals[manifest.Key{Kind: "Schedule", Tenant: tv.Name, Name: op.Target.Name}]
		return pathValue(raw, op.Field)
	case "Tenant":
		return pathValue(tv.Raw, op.Field)
	}
	return nil, false
}

// pathValue reads spec.a.b or spec.a[].b (the single element) from a manifest.
func pathValue(m map[string]any, path string) (any, bool) {
	var cur any = m
	for _, part := range strings.Split(path, ".") {
		list := strings.HasSuffix(part, "[]")
		part = strings.TrimSuffix(part, "[]")
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		if cur, ok = mm[part]; !ok {
			return nil, false
		}
		if list {
			l, ok := cur.([]any)
			if !ok || len(l) != 1 {
				return nil, false
			}
			cur = l[0]
		}
	}
	return cur, true
}
