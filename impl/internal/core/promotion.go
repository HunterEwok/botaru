// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/ids"
)

// Promotion of a functionary into the commission (GOV-9, GOV-12, GOV-14, GOV-19, GOV-21,
// GOV-31, GOV-32, spec/lifecycles/case.md, spec/lifecycles/promotion-entry.md). The
// governance loop turns an executed, accepted proposal of a functionary into a trigger once its
// probation, trial and attributions allow it, keeps one queue entry per bot, and opens one case
// at a time for the head of the queue: a vacant seat, or a seat freed from the list L. Nothing
// changes before a person's "Confirm" on promotion_card; there are no ballots and no bot takes
// part in the choice.
// This file holds the governance loop, the triggers and the queue; the cases and their
// decisions are in promotioncase.go and promotiondecide.go.

const (
	// The charter defaults; each is also the minimum the charter may set (GOV-14). The charter
	// lint is not built, so a lower value counts as the minimum.
	govProbation    = 14 * 24 * time.Hour
	govMinTenure    = 30 * 24 * time.Hour
	govSwapCooldown = 30 * 24 * time.Hour
	govProtectNew   = 60 * 24 * time.Hour
	govConfirmTTL   = 7 * 24 * time.Hour
	// govFiguresDays is the window of the seat figures (GOV-12) and govCleanDays the window
	// without confirmed attributions for eligibility (GOV-14).
	govFiguresDays = 180
	govCleanDays   = 90
	// selfMeasuredCard asks people to count a self-measured success (GOV-14).
	selfMeasuredCard = "self_measured_card"
	selfMeasuredTTL  = 7 * 24 * time.Hour
)

// govDuration is a duration of the tenant charter, never below its default minimum.
func govDuration(cat *Catalog, tenant, block, field string, def time.Duration) time.Duration {
	return max(charterDuration(cat, tenant, block, field, def), def)
}

// governanceLoop runs the governance of every tenant with commission seats in its charter: the
// attribution candidates, the promotion triggers and queue, and the case (background loop
// governance).
func (c *Core) governanceLoop(ctx context.Context) error {
	cat, err := c.Catalog(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, t := range slices.Sorted(mapKeys(cat.Tenants)) {
		if t == farmTenant {
			continue
		}
		tv := cat.Tenants[t]
		if len(charterSeats(cat, t)) == 0 {
			// Script links need no commission (TRN-7).
			if err := c.tx(ctx, func(tx pgx.Tx, now time.Time) error { return c.scriptLinkStep(ctx, tx, now, cat, tv) }); err != nil {
				errs = append(errs, fmt.Errorf("script links of %s: %w", t, err))
			}
			continue
		}
		if err := c.tx(ctx, func(tx pgx.Tx, now time.Time) error { return c.governTenant(ctx, tx, now, cat, tv) }); err != nil {
			errs = append(errs, fmt.Errorf("governance of %s: %w", t, err))
		}
	}
	return errors.Join(errs...)
}

func (c *Core) governTenant(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, tv *TenantView) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(7300107, hashtext($1))`, tv.Name); err != nil {
		return err
	}
	since, err := c.governanceEnabled(ctx, tx, now, tv.Name)
	if err != nil {
		return err
	}
	if err := c.attributionStep(ctx, tx, now, cat, tv); err != nil {
		return err
	}
	if err := c.triggerStep(ctx, tx, now, cat, tv, since); err != nil {
		return err
	}
	if err := c.entryStep(ctx, tx, now, cat, tv.Name); err != nil {
		return err
	}
	// From v0.3: the forecasts are scored, the waiting departures for links go at their pace,
	// and the admins learn when accountability can be switched on (COM-16, GOV-36, GOV-42).
	if err := c.scoreStep(ctx, tx, now, cat, tv); err != nil {
		return err
	}
	if err := c.departureStep(ctx, tx, now, cat, tv.Name); err != nil {
		return err
	}
	if err := c.readyStep(ctx, tx, now, cat, tv); err != nil {
		return err
	}
	// From v0.3 the weekly commission summary to people (COM-17, GOV-36).
	if err := c.weeklyStep(ctx, tx, now, cat, tv); err != nil {
		return err
	}
	return c.caseStep(ctx, tx, now, cat, tv)
}

// governanceEnabled writes governance.enabled at the first pass over a tenant; its time is
// governance_since (GOV-14).
func (c *Core) governanceEnabled(ctx context.Context, tx pgx.Tx, now time.Time, tenant string) (time.Time, error) {
	tag, err := tx.Exec(ctx, `INSERT INTO governance_tenants (tenant, since, updated_at) VALUES ($1, $2, $2) ON CONFLICT DO NOTHING`, tenant, now)
	if err != nil {
		return time.Time{}, err
	}
	if tag.RowsAffected() > 0 {
		if _, err := c.appendAudit(ctx, tx, now, Audit{Tenant: tenant, Kind: "command", Actor: Actor{Principal: "system", Via: "core"},
			Command: "governance.enabled", Target: &Target{Kind: "tenant", ID: tenant}, OK: true, After: map[string]any{"since": fmtTime(now)}}); err != nil {
			return time.Time{}, err
		}
		return now, nil
	}
	var since time.Time
	err = tx.QueryRow(ctx, `SELECT since FROM governance_tenants WHERE tenant = $1`, tenant).Scan(&since)
	return since, err
}

// governanceSince is governance_since of P2 commission.status, or nil.
func governanceSince(ctx context.Context, tx pgx.Tx, tenant string) (any, error) {
	var since time.Time
	err := tx.QueryRow(ctx, `SELECT since FROM governance_tenants WHERE tenant = $1`, tenant).Scan(&since)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return fmtTime(since), nil
}

func (c *Core) govAudit(ctx context.Context, tx pgx.Tx, now time.Time, tenant, command, kind, id, principal string, after map[string]any) error {
	via := "core"
	if strings.HasPrefix(principal, "human:") {
		via = "channel"
	}
	_, err := c.appendAudit(ctx, tx, now, Audit{Tenant: tenant, Kind: "command", Actor: Actor{Principal: principalOr(principal), Via: via},
		Command: command, Target: &Target{Kind: kind, ID: id}, OK: true, After: after})
	return err
}

// triggerStep evaluates every executed proposal of the tenant that has no track row yet: a
// trigger, only a success in the track record, or not yet (GOV-14, GOV-30, GOV-32).
func (c *Core) triggerStep(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, tv *TenantView, since time.Time) error {
	rows, err := tx.Query(ctx, `SELECT id FROM proposals p WHERE tenant = $1 AND state = 'realized'
		AND NOT EXISTS (SELECT 1 FROM promotion_track t WHERE t.proposal = p.id) ORDER BY id FOR UPDATE`, tv.Name)
	if err != nil {
		return err
	}
	list, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, id := range list {
		p, err := loadProposal(ctx, tx, id)
		if err != nil {
			return err
		}
		bot, verdict, err := c.successOf(ctx, tx, now, cat, tv, p, since)
		if err != nil {
			return err
		}
		if verdict == "wait" || bot == "" {
			continue
		}
		trigger := verdict == "trigger"
		reason := any(nil)
		if !trigger {
			reason = verdict
		}
		if _, err := tx.Exec(ctx, `INSERT INTO promotion_track (proposal, tenant, bot, at, trigger, reason) VALUES ($1, $2, $3, $4, $5, $6)`,
			p.ID, tv.Name, bot, now, trigger, reason); err != nil {
			return err
		}
		if trigger {
			if err := c.queueEntry(ctx, tx, now, tv.Name, bot, p.ID); err != nil {
				return err
			}
		}
	}
	return nil
}

// successOf says what an executed proposal gives: its author bot and trigger, a reason it gives
// only a track row, or wait. A proposal without a functionary author gives nothing.
func (c *Core) successOf(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, tv *TenantView, p *proposalRow, since time.Time) (string, string, error) {
	botID := asString(mmapAny(p.Rec["origin"])["acting_bot"])
	if botID == "" {
		return "", "", nil
	}
	b, err := loadBot(ctx, tx, botID, false)
	if err != nil || b == nil {
		return "", "", err
	}
	// Only a functionary with a model enters the queue: personal assistants, translators
	// and roles without a model do not (GOV-14).
	if !slices.Contains(functionaryProfiles, profileOf(cat, b.Role)) || !hasModel(cat, b.Role) {
		return "", "", nil
	}
	// A bot that left a seat for a link gets no trigger for 180 days (GOV-42).
	if at, err := unseatedRecently(ctx, tx, now, botID); err != nil {
		return "", "", err
	} else if at != nil {
		return botID, "after_unseat", nil
	}
	d := mmapAny(p.Rec["decision"])
	switch {
	case p.Rec["veto_overridden"] == true:
		return botID, "veto_overridden", nil
	case !strings.HasPrefix(asString(d["decided_by"]), "human:") || d["decision"] != "accept" && d["decision"] != "override_veto":
		return botID, "not_accepted", nil
	case len(asList(p.Rec["related_to"])) > 0:
		// A late related proposal gives no trigger (PRP-17).
		return botID, "late_related", nil
	}
	exclude := []string{"none"}
	if gov := mmap(tenantSpecOf(cat, "Governance", tv.Name), "promotion"); gov["exclude_change_kinds"] != nil {
		exclude = strList(gov["exclude_change_kinds"])
	}
	if slices.Contains(exclude, p.body().Change.Kind) {
		return botID, "excluded_kind", nil
	}
	realized := parseTime(asString(mmapAny(p.Rec["realization"])["realized_at"]))
	until := realized.Add(govDuration(cat, tv.Name, "promotion", "probation", govProbation))
	if now.Before(until) {
		return botID, "wait", nil
	}
	if until.Before(since) {
		return botID, "before_governance", nil
	}
	t := mmapAny(p.Rec["trial"])
	switch outcome := asString(t["outcome"]); {
	case outcome == "":
		return botID, "wait", nil
	case outcome == "worse" || outcome == "canceled" || t["state"] == "canceled":
		return botID, "trial_" + outcome, nil
	}
	var open, confirmed bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM attributions WHERE proposal = $1 AND state = ANY($2)),
		EXISTS (SELECT 1 FROM attributions WHERE proposal = $1 AND state = 'confirmed')`, p.ID, attributionOpen).Scan(&open, &confirmed); err != nil {
		return "", "", err
	}
	if confirmed {
		return botID, "attribution", nil
	}
	if open {
		return botID, "wait", nil
	}
	if t["self_measured"] == true && p.Rec["self_measured_count"] == nil {
		// A self-measured outcome gives a trigger only with "Count it" (GOV-14).
		sent := mmapAny(p.Rec["self_measured_card"])
		if sent == nil {
			return botID, "wait", c.askSelfMeasured(ctx, tx, now, tv, p)
		}
		if now.Sub(parseTime(asString(sent["at"]))) > selfMeasuredTTL {
			return botID, "self_measured", nil
		}
		return botID, "wait", nil
	}
	return botID, "trigger", nil
}

// askSelfMeasured sends self_measured_card to the people with governance.confirm:commission.
func (c *Core) askSelfMeasured(ctx context.Context, tx pgx.Tx, now time.Time, tv *TenantView, p *proposalRow) error {
	nonce := ids.Secret(16)
	if _, err := tx.Exec(ctx, `UPDATE proposals SET nonce = $2 WHERE id = $1`, p.ID, nonce); err != nil {
		return err
	}
	p.Nonce = nonce
	p.Rec["self_measured_card"] = map[string]any{"at": fmtTime(now)}
	if err := c.saveProposal(ctx, tx, now, p); err != nil {
		return err
	}
	people, err := peopleWithCap(ctx, tx, now, tv, "governance.confirm:commission")
	if err != nil {
		return err
	}
	// Only people whose ceiling covers the proposal's mark get its figures (DIG-2).
	if people, err = c.withinCeiling(ctx, tx, now, tv, people, asString(p.Rec["data_mark"])); err != nil {
		return err
	}
	for _, person := range people {
		if err := c.planProposalCard(ctx, tx, now, tv, p, selfMeasuredCard, person, nil); err != nil {
			return err
		}
	}
	return nil
}

// countSelfMeasured is "Count it" on self_measured_card (GOV-14).
func (c *Core) countSelfMeasured(ctx context.Context, tx pgx.Tx, now time.Time, press *cardPress, p *proposalRow, deny func(string, string) error) (string, error) {
	if !hasCap(press.tv, press.groups, "governance.confirm:commission") {
		return "", deny("forbidden", "the person has no governance.confirm:commission")
	}
	if p.State != "realized" || mmapAny(p.Rec["trial"])["self_measured"] != true {
		return "", refuse("expired", "the change is not a realized self-measured one")
	}
	if p.Rec["self_measured_count"] != nil {
		return p.State, nil
	}
	sent := mmapAny(p.Rec["self_measured_card"])
	if sent == nil || now.Sub(parseTime(asString(sent["at"]))) > selfMeasuredTTL {
		return "", refuse("expired", "the card has expired")
	}
	p.Rec["self_measured_count"] = map[string]any{"by": press.principal, "at": fmtTime(now)}
	if err := c.saveProposal(ctx, tx, now, p); err != nil {
		return "", err
	}
	return p.State, c.govAudit(ctx, tx, now, p.Tenant, "decide", "proposal", p.ID, press.principal, map[string]any{"decision": "count"})
}

// queueEntry puts a bot into the queue at the time of its trigger, or writes the success only
// into the track record when the bot already has a live entry (GOV-14).
func (c *Core) queueEntry(ctx context.Context, tx pgx.Tx, now time.Time, tenant, bot, proposal string) error {
	tag, err := tx.Exec(ctx, `INSERT INTO promotion_entries (tenant, bot, state, trigger_proposal, trigger_at, updated_at) VALUES ($1, $2, 'queued', $3, $4, $4)
		ON CONFLICT (tenant, bot) DO UPDATE SET state = 'queued', trigger_proposal = $3, trigger_at = $4, cooldown_until = NULL,
			wrong_candidate_count = 0, removed_reason = NULL, updated_at = $4 WHERE promotion_entries.state = 'removed'`, tenant, bot, proposal, now)
	if err != nil || tag.RowsAffected() == 0 {
		return err
	}
	return c.govAudit(ctx, tx, now, tenant, "governance.entry.queued", "bot", bot, "", map[string]any{"trigger_proposal": proposal, "trigger_at": fmtTime(now)})
}

// removeEntry takes an entry out of the queue with its reason.
func (c *Core) removeEntry(ctx context.Context, tx pgx.Tx, now time.Time, tenant, bot, reason string) error {
	tag, err := tx.Exec(ctx, `UPDATE promotion_entries SET state = 'removed', removed_reason = $3, updated_at = $4 WHERE tenant = $1 AND bot = $2 AND state <> 'removed'`,
		tenant, bot, reason, now)
	if err != nil || tag.RowsAffected() == 0 {
		return err
	}
	return c.govAudit(ctx, tx, now, tenant, "governance.entry.removed", "bot", bot, "", map[string]any{"removed_reason": reason})
}

// entryStep removes the entries of bots that left (retired) or took a seat through P2
// seat.assign (seated); the core reads both from the bot records on each pass.
func (c *Core) entryStep(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, tenant string) error {
	rows, err := tx.Query(ctx, `SELECT e.bot, b.state, b.mode FROM promotion_entries e JOIN bots b ON b.id = e.bot
		WHERE e.tenant = $1 AND e.state IN ('queued', 'waiting_eligibility') AND (b.state IN ('retired', 'erased') OR b.mode = 'seat') ORDER BY e.bot`, tenant)
	if err != nil {
		return err
	}
	type gone struct{ bot, state, mode string }
	var list []gone
	for rows.Next() {
		var g gone
		if err := rows.Scan(&g.bot, &g.state, &g.mode); err != nil {
			rows.Close()
			return err
		}
		list = append(list, g)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, g := range list {
		reason := "retired"
		if g.mode == "seat" && g.state == "active" {
			reason = "seated"
		}
		if err := c.removeEntry(ctx, tx, now, tenant, g.bot, reason); err != nil {
			return err
		}
	}
	return nil
}

// attributionConfirmed: a confirmed attribution takes away the trigger of its change; an entry
// with that trigger takes the next trigger of its track record, or leaves the queue (GOV-14).
func (c *Core) attributionConfirmed(ctx context.Context, tx pgx.Tx, now time.Time, a *attributionRow) error {
	if _, err := tx.Exec(ctx, `UPDATE promotion_track SET trigger = false, reason = 'attribution' WHERE proposal = $1`, a.Proposal); err != nil {
		return err
	}
	var bot string
	var at time.Time
	err := tx.QueryRow(ctx, `SELECT bot, trigger_at FROM promotion_entries WHERE tenant = $1 AND trigger_proposal = $2 AND state <> 'removed' FOR UPDATE`, a.Tenant, a.Proposal).Scan(&bot, &at)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var next string
	var nextAt time.Time
	err = tx.QueryRow(ctx, `SELECT proposal, at FROM promotion_track WHERE tenant = $1 AND bot = $2 AND trigger AND at > $3 ORDER BY at, proposal LIMIT 1`, a.Tenant, bot, at).Scan(&next, &nextAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return c.removeEntry(ctx, tx, now, a.Tenant, bot, "attribution")
	}
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE promotion_entries SET trigger_proposal = $3, trigger_at = $4, updated_at = $5 WHERE tenant = $1 AND bot = $2`, a.Tenant, bot, next, nextAt, now); err != nil {
		return err
	}
	return c.govAudit(ctx, tx, now, a.Tenant, "governance.entry.queued", "bot", bot, "", map[string]any{"trigger_proposal": next, "trigger_at": fmtTime(nextAt)})
}

// eligible checks the candidate at opening and before "Confirm" (GOV-14): the identity is
// active and not under a hold of people, its tenure is at least min_tenure, and in 90 days
// there is no confirmed attribution to its changes.
func (c *Core) eligible(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, tenant, botID string) (bool, error) {
	b, err := loadBot(ctx, tx, botID, false)
	if err != nil || b == nil || b.State != "active" || b.Mode != "worker" {
		return false, err
	}
	tenure := b.CreatedAt
	var skipTenure bool
	if err := tx.QueryRow(ctx, `SELECT coalesce((SELECT skip_tenure FROM promotion_entries WHERE tenant = $1 AND bot = $2), false)`, tenant, botID).Scan(&skipTenure); err != nil {
		return false, err
	}
	if !skipTenure {
		// After leaving a seat for a link: no case for 180 days, and the tenure counts from the
		// departure; a bot returned after a canceled link waits for no min_tenure (GOV-42).
		at, err := unseatedRecently(ctx, tx, now, botID)
		if err != nil || at != nil {
			return false, err
		}
		var left *time.Time
		if err := tx.QueryRow(ctx, `SELECT max(done_at) FROM link_departures WHERE bot = $1 AND state = 'done'`, botID).Scan(&left); err != nil {
			return false, err
		}
		if left != nil && left.After(tenure) {
			tenure = *left
		}
	}
	if !skipTenure && now.Sub(tenure) < govDuration(cat, tenant, "promotion", "min_tenure", govMinTenure) {
		return false, nil
	}
	if held, err := botHeld(ctx, tx, now, b); err != nil || held {
		return false, err
	}
	var dirty bool
	err = tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM attributions a JOIN proposals p ON p.id = a.proposal
		WHERE a.tenant = $1 AND a.kind = 'change' AND a.state = 'confirmed' AND p.record->'origin'->>'acting_bot' = $2 AND (a.record->'answer'->>'at')::timestamptz >= $3)`,
		tenant, botID, now.AddDate(0, 0, -govCleanDays)).Scan(&dirty)
	return !dirty, err
}
