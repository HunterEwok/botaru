// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/ids"
)

// The audit sample of the commission (DIG-5, DIG-6, spec/lifecycles/proposal.md). Every month
// the core draws, with a fresh recorded seed, a uniform sample of the proposals that got a
// verdict that month and were not recommended; an item goes to people as a blind card "Would
// you accept?", and only after "Would accept" does the card show the scores and vetoes.

const (
	proposalSampleCard = "proposal_sample_card"
	// The charter defaults of commission.audit_sample: min_per_month is at least 4 by the
	// schema; share and max_per_digest follow the skeleton example (docs/improvements.md).
	defaultSampleMin       = 4
	defaultSampleShare     = 0.25
	defaultSamplePerDigest = 2
	// sampleStatusDays, sampleUnderMin and sampleUnderShare are the commission_underrates
	// flag: at least 10 answers over 180 days and a weighted share of "Would accept" of at
	// least 0.3 (DIG-5).
	sampleStatusDays = 180
	sampleUnderMin   = 10
	sampleUnderShare = 0.3
)

// sampleDecisions are the buttons of the blind card and of the judged item (DIG-6).
var sampleDecisions = []string{"would_accept", "would_not_accept", "override_veto"}

// sampleConfig is commission.audit_sample of the tenant charter over the defaults.
func sampleConfig(cat *Catalog, tenant string) (minPer int, share float64, perDigest int) {
	gov := tenantSpecOf(cat, "Governance", tenant)
	as := mmap(mmap(gov, "commission"), "audit_sample")
	minPer, share, perDigest = defaultSampleMin, defaultSampleShare, defaultSamplePerDigest
	if n, ok := toFloat(as["min_per_month"]); ok && int(n) >= defaultSampleMin {
		minPer = int(n)
	}
	if n, ok := toFloat(as["share"]); ok {
		share = n
	}
	if n, ok := toFloat(as["max_per_digest"]); ok {
		perDigest = int(n)
	}
	return minPer, share, perDigest
}

// sampleSize is N = min(T, max(min_per_month, ceil(share × T))) (DIG-5).
func sampleSize(t, minPer int, share float64) int {
	return min(t, max(minPer, int(math.Ceil(share*float64(t)-1e-9))))
}

// sampleDraws draws the sample of the month before the current one of every tenant once: the
// first pass of the proposal loop in a new month of the tenant's zone does it.
func (c *Core) sampleDraws(ctx context.Context, cat *Catalog) error {
	var errs []error
	for _, t := range slices.Sorted(mapKeys(cat.Tenants)) {
		tv := cat.Tenants[t]
		if t == farmTenant {
			continue
		}
		if err := c.tx(ctx, func(tx pgx.Tx, now time.Time) error { return c.drawSample(ctx, tx, now, cat, tv) }); err != nil {
			errs = append(errs, fmt.Errorf("audit sample of %s: %w", t, err))
		}
	}
	return errors.Join(errs...)
}

// drawSample draws the audit sample of the previous month (DIG-5): the population T is the
// tenant's proposals whose verdict was computed that month and that are dismissed by a veto, a
// minimum or a low score, or parked by score, and still in that state; the core draws N of
// them uniformly with a fresh seed by a seeded shuffle of the ids in order, records the seed
// and the drawn ids, and moves each drawn item to sampled with its month and inclusion_prob.
func (c *Core) drawSample(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, tv *TenantView) error {
	loc := tenantLocation(cat, tv.Name)
	local := now.In(loc)
	end := time.Date(local.Year(), local.Month(), 1, 0, 0, 0, 0, loc)
	start := end.AddDate(0, -1, 0)
	month := start.Format("2006-01")
	var done bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM audit_samples WHERE tenant = $1 AND month = $2)`, tv.Name, month).Scan(&done); err != nil || done {
		return err
	}
	rows, err := tx.Query(ctx, `SELECT id FROM proposals WHERE tenant = $1
		AND (record->'verdict'->>'computed_at')::timestamptz >= $2 AND (record->'verdict'->>'computed_at')::timestamptz < $3
		AND (state = 'parked' AND record->>'state_reason' = 'score' OR state = 'dismissed' AND record->>'state_reason' IN ('veto', 'below_min', 'low_score'))
		ORDER BY id FOR UPDATE`, tv.Name, start, end)
	if err != nil {
		return err
	}
	pop, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	minPer, share, _ := sampleConfig(cat, tv.Name)
	n := sampleSize(len(pop), minPer, share)
	var seed [32]byte
	if _, err := crand.Read(seed[:]); err != nil {
		return err
	}
	drawn := slices.Clone(pop)
	r := rand.New(rand.NewChaCha8(seed))
	r.Shuffle(len(drawn), func(i, j int) { drawn[i], drawn[j] = drawn[j], drawn[i] })
	drawn = drawn[:n]
	id := ids.New(ids.Sample)
	tag, err := tx.Exec(ctx, `INSERT INTO audit_samples (id, tenant, month, seed, population, size, drawn, created_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (tenant, month) DO NOTHING`, id, tv.Name, month, hex.EncodeToString(seed[:]), len(pop), n, strAny(append([]string{}, drawn...)), now)
	if err != nil || tag.RowsAffected() == 0 {
		return err
	}
	if _, err := c.appendAudit(ctx, tx, now, Audit{
		Tenant: tv.Name, Kind: "command", Actor: Actor{Principal: "system", Via: "core"},
		Command: "audit_sample.draw", Target: &Target{Kind: "audit_sample", ID: id}, OK: true,
		After: map[string]any{"month": month, "population": len(pop), "size": n, "drawn": strAny(append([]string{}, drawn...))},
	}); err != nil {
		return err
	}
	for _, pid := range drawn {
		p, err := loadProposal(ctx, tx, pid)
		if err != nil {
			return err
		}
		p.Rec["sample"] = map[string]any{"month": month, "inclusion_prob": float64(n) / float64(len(pop)), "seed_ref": id}
		if err := c.moveProposal(ctx, tx, now, p, "sampled", ""); err != nil {
			return err
		}
	}
	return nil
}

// pickSamples are the sampled items the next digest presents, the oldest first, at most
// audit_sample.max_per_digest (DIG-1, DIG-5).
func pickSamples(ctx context.Context, tx pgx.Tx, tenant string, limit int) ([]*proposalRow, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := tx.Query(ctx, `SELECT id FROM proposals WHERE tenant = $1 AND state = 'sampled' ORDER BY state_at, id LIMIT $2 FOR UPDATE`, tenant, limit)
	if err != nil {
		return nil, err
	}
	list, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	var out []*proposalRow
	for _, id := range list {
		p, err := loadProposal(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

// presentSample gives a sampled item its card nonce and sends the blind card only by direct
// message to the people who may decide on its category and whose ceiling covers it (DIG-5).
func (c *Core) presentSample(ctx context.Context, tx pgx.Tx, now time.Time, tv *TenantView, p *proposalRow) error {
	nonce := ids.Secret(16)
	if _, err := tx.Exec(ctx, `UPDATE proposals SET nonce = $2, presented_at = $3 WHERE id = $1`, p.ID, nonce, now); err != nil {
		return err
	}
	p.Nonce, p.PresentedAt = nonce, &now
	if err := c.moveProposal(ctx, tx, now, p, "sample_presented", ""); err != nil {
		return err
	}
	people, err := c.deciders(ctx, tx, now, tv, p.body().Category, asString(p.Rec["data_mark"]))
	if err != nil {
		return err
	}
	for _, person := range people {
		if err := c.planProposalCard(ctx, tx, now, tv, p, proposalSampleCard, person, nil); err != nil {
			return err
		}
	}
	return nil
}

// sampleVetoCards sends the judged card of an item with a veto also to the people with
// proposal.override_veto:<category> whose ceiling covers it and who have no card of it yet, so
// "Lift veto" reaches them (DIG-6).
func (c *Core) sampleVetoCards(ctx context.Context, tx pgx.Tx, now time.Time, tv *TenantView, p *proposalRow) error {
	people, err := peopleWithCap(ctx, tx, now, tv, "proposal.override_veto:"+p.body().Category)
	if err != nil {
		return err
	}
	mark := asString(p.Rec["data_mark"])
	if people, err = c.withinCeiling(ctx, tx, now, tv, people, mark); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `SELECT DISTINCT coalesce(recipient, '') FROM deliveries WHERE subject_kind = 'proposal' AND subject_id = $1 AND kind = $2`, p.ID, proposalSampleCard)
	if err != nil {
		return err
	}
	have, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, person := range people {
		if slices.Contains(have, person) {
			continue
		}
		if err := c.planProposalCard(ctx, tx, now, tv, p, proposalSampleCard, person, nil); err != nil {
			return err
		}
	}
	return nil
}

// sampleVeto says whether the verdict of an item has a veto of a veto lens.
func (c *Core) sampleVeto(p *proposalRow) bool {
	defs := c.lensDefs()
	for _, l := range asList(mmapAny(p.Rec["verdict"])["lenses"]) {
		lm := mmapAny(l)
		if lm["veto"] == true && defs[asString(lm["lens"])].Veto {
			return true
		}
	}
	return false
}

// sampleStep closes an item nobody answered or decided within decide_ttl (DIG-6).
func (c *Core) sampleStep(ctx context.Context, tx pgx.Tx, now time.Time, tv *TenantView, p *proposalRow) error {
	at := p.StateAt
	if p.PresentedAt != nil {
		at = *p.PresentedAt
	}
	if now.Sub(at) <= durationOr(tv.Spec.Insights.DecideTTL, defaultDecideTTL) {
		return nil
	}
	if err := c.moveProposal(ctx, tx, now, p, "sample_closed", ""); err != nil {
		return err
	}
	return c.planProposalCardUpdates(ctx, tx, now, p)
}

// decideSample is decide on an audit sample item (DIG-6): on the blind card "Would accept" or
// "Would not accept", recorded in the sample before the person sees the scores; after "Would
// accept" an item without a veto has "Accept", which acts as the ordinary one (PRP-4), and an
// item with a veto has only "Lift veto" for a person with proposal.override_veto:<category>
// who opened "Details" of the judged card; "Reject" closes the item.
func (c *Core) decideSample(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, press *cardPress, p *proposalRow, req DecideRequest, views int, deny func(string, string) error) (string, error) {
	b := p.body()
	ans := mmapAny(p.Rec["sample_answer"])
	// A repeat of the same decision by the same person gets the same answer (IDM-3).
	if d := mmapAny(p.Rec["decision"]); d["decided_by"] == press.principal && d["decision"] == req.Decision ||
		ans["by"] == press.principal && ans["answer"] == req.Decision && p.State != "sample_presented" {
		return p.State, nil
	}
	if p.State != "sample_presented" && p.State != "sample_judged" {
		if p.State == "sample_closed" || p.State == "expired" {
			return "", refuse("expired", "the item is closed")
		}
		return "", refuse("already_decided", "the item is decided")
	}
	right := "proposal.decide:" + b.Category
	if req.Decision == "override_veto" {
		right = "proposal.override_veto:" + b.Category
	}
	if !hasCap(press.tv, press.groups, right) {
		return "", deny("forbidden", "the person has no "+right)
	}
	if p.PresentedAt != nil && now.Sub(*p.PresentedAt) > durationOr(press.tv.Spec.Insights.DecideTTL, defaultDecideTTL) {
		if err := c.moveProposal(ctx, tx, now, p, "sample_closed", ""); err != nil {
			return "", err
		}
		if err := c.planProposalCardUpdates(ctx, tx, now, p); err != nil {
			return "", err
		}
		return "", &committed{p: refuse("expired", "the card has expired")}
	}
	responseMS := max(now.Sub(press.deliveredAt).Milliseconds(), 0)
	if p.State == "sample_presented" {
		var state string
		switch req.Decision {
		case "would_accept":
			state = "sample_judged"
		case "would_not_accept":
			state = "sample_closed"
		default:
			return "", deny("forbidden", "the blind card is answered first")
		}
		// The answer is recorded in the sample before the scores are shown (DIG-6).
		s := mmapAny(p.Rec["sample"])
		s["answer"] = req.Decision
		p.Rec["sample_answer"] = map[string]any{"answer": req.Decision, "by": press.principal, "at": fmtTime(now), "response_ms": responseMS, "details_opened": views > 0}
		if err := c.moveProposal(ctx, tx, now, p, state, ""); err != nil {
			return "", err
		}
		if state == "sample_judged" && c.sampleVeto(p) {
			return state, c.sampleVetoCards(ctx, tx, now, press.tv, p)
		}
		return state, nil
	}
	decision := map[string]any{"decision": req.Decision, "reason_code": nil, "decided_by": press.principal, "at": fmtTime(now),
		"response_ms": responseMS, "details_opened": views > 0, "defer_count": 0}
	veto := c.sampleVeto(p)
	var state string
	switch req.Decision {
	case "reject":
		code := req.ReasonCode
		if code == "" {
			code = "other"
		}
		if !c.declineReason(code) {
			return "", refuse("input_schema_invalid", "the reason is not in decline-reasons.yaml")
		}
		decision["reason_code"] = code
		state = "sample_closed"
	case "accept", "override_veto":
		if veto != (req.Decision == "override_veto") {
			return "", deny("forbidden", "an item with a veto has only Lift veto, one without only Accept")
		}
		if veto {
			// APR-14: "Lift veto" only after this person opened "Details" of the judged card,
			// the one with the scores and the veto; the P1 card.details record comes first.
			opened, err := detailsOpened(ctx, tx, "proposal", p.ID, press.principal, parseTime(asString(mmapAny(p.Rec["sample_answer"])["at"])))
			if err != nil {
				return "", err
			}
			if c.missingDetails(proposalSampleCard, req.Decision, opened) {
				return "", deny("details_not_opened", `open "Details" first`)
			}
		}
		if b.Change.Kind == "none" {
			state = "noted"
			break
		}
		if holders, err := lockHolders(ctx, tx, p.Tenant, p.ID, lockKeys(b)); err != nil {
			return "", err
		} else if len(holders) > 0 {
			return "", deny("forbidden", "another accepted change holds the same field")
		}
		code, err := c.realize(ctx, tx, now, cat, press, p)
		if err != nil {
			return "", err
		}
		if code != "" {
			return "", deny(code, "the person cannot start the realization of this proposal")
		}
		state = "realizing"
	default:
		return "", deny("forbidden", "a judged item is accepted, its veto lifted or it is rejected")
	}
	if req.Decision == "override_veto" {
		// A lifted veto gives the author no credit and no promotion trigger (DIG-6, GOV-14).
		p.Rec["veto_overridden"] = true
		if _, err := c.appendAudit(ctx, tx, now, Audit{
			Tenant: p.Tenant, Kind: "decision", Actor: Actor{Principal: press.principal, Via: "channel"},
			Command: "proposal.override_veto", Target: &Target{Kind: "proposal", ID: p.ID}, OK: true,
			After: map[string]any{"veto_overridden": true, "state": state},
		}); err != nil {
			return "", err
		}
	}
	p.Rec["decision"] = decision
	if err := c.moveProposal(ctx, tx, now, p, state, ""); err != nil {
		return "", err
	}
	if state == "realizing" {
		if err := c.parkConflicts(ctx, tx, now, p); err != nil {
			return "", err
		}
	}
	return state, decisionPoints(ctx, tx, now, p, req.Decision, asString(decision["reason_code"]))
}

// sampleCardFields draws the card of a sample item (DIG-6): the blind card has the closed
// fields, the evidence as numbers, the change in code words, the price and the risk, without
// lens scores, vetoes or the verdict; the judged card adds the scores and vetoes.
func (c *Core) sampleCardFields(p *proposalRow, card, ctxt map[string]any, until time.Time) []any {
	b := p.body()
	delete(ctxt, "weighted_mean")
	card["title"] = cut("Would you accept? "+asString(card["title"]), 200)
	for i, e := range asList(p.Rec["evidence_seen"]) {
		if i >= 2 {
			break
		}
		em := mmapAny(e)
		ref := mmapAny(em["ref"])
		ctxt[fmt.Sprintf("evidence%d", i+1)] = fmt.Sprintf("%s %s %s %s = %v", ref["series"], ref["subject"], ref["window"], ref["rollup"], em["value"])
	}
	if mc := b.Change.ManifestChange; mc != nil {
		for i, op := range mc.Ops {
			if i >= 2 {
				break
			}
			ctxt[fmt.Sprintf("op%d", i+1)] = fmt.Sprintf("%s %s %s = %v", op.Target.Kind, op.Target.Name, op.Field, op.Value)
		}
	}
	cost := mmapAny(mmapAny(p.Rec["body"])["cost"])
	if v, ok := cost["one_time_eur"]; ok {
		ctxt["cost_one_time_eur"] = v
	}
	if v, ok := cost["monthly_eur"]; ok {
		ctxt["cost_monthly_eur"] = v
	}
	card["expires_at"] = fmtTime(until)
	card["footer"] = map[string]any{"why_you": "proposal.decide:" + b.Category, "until": fmtTime(until), "on_silence": "the item is closed without a decision"}
	button := func(code string) map[string]any {
		return map[string]any{"code": code, "subject_kind": "proposal", "id": p.ID, "nonce": p.Nonce}
	}
	switch p.State {
	case "sample_presented":
		return []any{button("would_accept"), button("would_not_accept"), button("details")}
	case "sample_judged":
		var scores []string
		for _, l := range asList(mmapAny(p.Rec["verdict"])["lenses"]) {
			lm := mmapAny(l)
			s := fmt.Sprintf("%v %v", lm["lens"], lm["score"])
			if lm["veto"] == true {
				s += " veto"
			}
			scores = append(scores, s)
		}
		ctxt["scores"] = strings.Join(scores, ", ")
		if c.sampleVeto(p) {
			return []any{button("override_veto"), button("reject"), button("details")}
		}
		return []any{button("accept"), button("reject"), button("details")}
	}
	return []any{}
}

// auditSampleStatus is the audit sample over 180 days in P2 commission.status (DIG-5): the
// number of answers, the share of "Would accept" weighted by 1/inclusion_prob and the flag
// commission_underrates at 10 answers or more and a share of 0.3 or more.
func auditSampleStatus(ctx context.Context, tx pgx.Tx, now time.Time, tenant string) (map[string]any, error) {
	rows, err := tx.Query(ctx, `SELECT record->'sample_answer'->>'answer', (record->'sample'->>'inclusion_prob')::float8 FROM proposals
		WHERE tenant = $1 AND record ? 'sample_answer' AND (record->'sample_answer'->>'at')::timestamptz >= $2`, tenant, now.AddDate(0, 0, -sampleStatusDays))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var n int
	var yes, all float64
	for rows.Next() {
		var answer string
		var prob float64
		if err := rows.Scan(&answer, &prob); err != nil {
			return nil, err
		}
		if prob <= 0 {
			continue
		}
		n++
		all += 1 / prob
		if answer == "would_accept" {
			yes += 1 / prob
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := map[string]any{"answers": n, "would_accept_share": nil, "commission_underrates": false}
	if all > 0 {
		share := math.Round(yes/all*1e6) / 1e6
		out["would_accept_share"] = share
		out["commission_underrates"] = n >= sampleUnderMin && share >= sampleUnderShare
	}
	return out, nil
}
