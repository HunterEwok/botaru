// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
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
		if t == farmTenant || len(charterSeats(cat, t)) == 0 {
			continue
		}
		tv := cat.Tenants[t]
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
	if now.Sub(b.CreatedAt) < govDuration(cat, tenant, "promotion", "min_tenure", govMinTenure) {
		return false, nil
	}
	if held, err := botHeld(ctx, tx, now, b); err != nil || held {
		return false, err
	}
	var dirty bool
	err = tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM attributions a JOIN proposals p ON p.id = a.proposal
		WHERE a.tenant = $1 AND a.state = 'confirmed' AND p.record->'origin'->>'acting_bot' = $2 AND (a.record->'answer'->>'at')::timestamptz >= $3)`,
		tenant, botID, now.AddDate(0, 0, -govCleanDays)).Scan(&dirty)
	return !dirty, err
}

// caseRow is a promotion case under its lock.
type caseRow struct {
	ID, Tenant, State, Vacancy, Nonce string
	Rec                               map[string]any
	Holders, Upheld                   map[string]any
	PresentedAt, ExpiresAt, HeldSince *time.Time
}

func loadCase(ctx context.Context, tx pgx.Tx, id string, lock bool) (*caseRow, error) {
	q := `SELECT id, tenant, state, coalesce(vacancy, ''), coalesce(nonce, ''), record, holders, upheld, presented_at, expires_at, held_since FROM governance_cases WHERE id = $1`
	if lock {
		q += ` FOR UPDATE`
	}
	k := &caseRow{}
	var rec, holders, upheld []byte
	err := tx.QueryRow(ctx, q, id).Scan(&k.ID, &k.Tenant, &k.State, &k.Vacancy, &k.Nonce, &rec, &holders, &upheld, &k.PresentedAt, &k.ExpiresAt, &k.HeldSince)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(holders, &k.Holders); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(upheld, &k.Upheld); err != nil {
		return nil, err
	}
	return k, json.Unmarshal(rec, &k.Rec)
}

func (k *caseRow) candidate() string { return asString(mmapAny(k.Rec["subject"])["candidate"]) }
func (k *caseRow) mode() string      { return asString(mmapAny(k.Rec["subject"])["mode"]) }
func (k *caseRow) trigger() string   { return asString(mmapAny(k.Rec["trigger"])["proposal"]) }

// moveCase writes a case transition with its audit record and updates the cards (GOV-21).
func (c *Core) moveCase(ctx context.Context, tx pgx.Tx, now time.Time, k *caseRow, state, reason, command, principal string) error {
	k.State, k.Rec["state"] = state, state
	if reason != "" {
		k.Rec["cancel_reason"] = reason
	}
	raw, err := json.Marshal(caseView(k))
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE governance_cases SET state = $2, record = $3, updated_at = $4 WHERE id = $1`, k.ID, state, raw, now); err != nil {
		return err
	}
	after := map[string]any{"state": state, "candidate": k.candidate(), "mode": k.mode()}
	if reason != "" {
		after["reason"] = reason
	}
	if conf := mmapAny(k.Rec["confirmation"]); conf != nil {
		after["decision"], after["choice"], after["reason_code"] = conf["decision"], conf["choice"], conf["reason_code"]
	}
	if command != "" {
		if err := c.govAudit(ctx, tx, now, k.Tenant, command, "governance_case", k.ID, principal, after); err != nil {
			return err
		}
	}
	return c.govCardUpdates(ctx, tx, now, k.Tenant, "governance_case", k.ID)
}

// caseView is the case record of governance-case.schema.json.
func caseView(k *caseRow) map[string]any {
	out := map[string]any{}
	for _, f := range []string{"id", "tenant", "kind", "state", "hold", "trigger", "subject", "snapshot", "flags", "outcome", "confirmation", "cancel_reason"} {
		out[f] = k.Rec[f]
	}
	return out
}

// releaseEntry returns the candidate's entry to the queue after a case that ended without it.
func releaseEntry(ctx context.Context, tx pgx.Tx, now time.Time, tenant, bot string) error {
	_, err := tx.Exec(ctx, `UPDATE promotion_entries SET state = 'queued', updated_at = $3 WHERE tenant = $1 AND bot = $2 AND state = 'in_case'`, tenant, bot, now)
	return err
}

// pauseCases sets the swap_cooldown pause before the next case (GOV-14, GOV-31).
func pauseCases(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, tenant, column string) error {
	_, err := tx.Exec(ctx, `UPDATE governance_tenants SET `+column+` = $2, updated_at = $3 WHERE tenant = $1`, tenant,
		now.Add(govDuration(cat, tenant, "promotion", "swap_cooldown", govSwapCooldown)), now)
	return err
}

// governanceHeld says whether people paused the cases of a tenant by a hold on the scope
// governance (GOV-14).
func governanceHeld(ctx context.Context, tx pgx.Tx, now time.Time, tenant string) (bool, error) {
	var held bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM holds WHERE state = 'active' AND (until IS NULL OR until > $2)
		AND scope_kind = 'governance' AND scope_tenant = $1)`, tenant, now).Scan(&held)
	return held, err
}

// caseStep checks the open case (expiry, a changed seat of L, a candidate that lost
// eligibility) and opens the next one when the tenant has none and no pause (GOV-12, GOV-14).
func (c *Core) caseStep(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, tv *TenantView) error {
	var open string
	err := tx.QueryRow(ctx, `SELECT id FROM governance_cases WHERE tenant = $1 AND state IN ('opened', 'confirming')`, tv.Name).Scan(&open)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	held, err := governanceHeld(ctx, tx, now, tv.Name)
	if err != nil {
		return err
	}
	if open != "" {
		k, err := loadCase(ctx, tx, open, true)
		if err != nil || k == nil {
			return err
		}
		if err := applyHold(ctx, tx, now, k, held); err != nil {
			return err
		}
		_, err = c.checkCase(ctx, tx, now, cat, k)
		return err
	}
	if held {
		return nil
	}
	var caseAfter, swapAfter *time.Time
	if err := tx.QueryRow(ctx, `SELECT case_after, swap_after FROM governance_tenants WHERE tenant = $1`, tv.Name).Scan(&caseAfter, &swapAfter); err != nil {
		return err
	}
	if caseAfter != nil && now.Before(*caseAfter) {
		return nil
	}
	return c.openCase(ctx, tx, now, cat, tv, swapAfter)
}

// applyHold writes a change of the hold to the case: a hold pauses the case, and on release the
// expiry moves on by the time it was held (GOV-14).
func applyHold(ctx context.Context, tx pgx.Tx, now time.Time, k *caseRow, held bool) error {
	if held == (k.Rec["hold"] == true) && (held || k.HeldSince == nil) {
		return nil
	}
	k.Rec["hold"] = held
	switch {
	case held:
		if k.HeldSince == nil {
			k.HeldSince = &now
		}
	case k.HeldSince != nil && k.ExpiresAt != nil:
		e := k.ExpiresAt.Add(now.Sub(*k.HeldSince))
		k.ExpiresAt, k.HeldSince = &e, nil
	default:
		k.HeldSince = nil
	}
	raw, err := json.Marshal(caseView(k))
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE governance_cases SET record = $2, held_since = $3, expires_at = $4, updated_at = $5 WHERE id = $1`,
		k.ID, raw, k.HeldSince, k.ExpiresAt, now)
	return err
}

// checkCase ends an open case that can no longer go on: expired after confirmation.ttl,
// canceled(seat_changed) when a seat of L changed its holder, canceled(candidate_ineligible).
// It says whether the case is still open.
func (c *Core) checkCase(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, k *caseRow) (bool, error) {
	if k.ExpiresAt != nil && k.HeldSince == nil && now.After(*k.ExpiresAt) {
		// Nobody leaves; the entry stays at the head and the next case waits swap_cooldown.
		if err := c.moveCase(ctx, tx, now, k, "expired", "", "governance.case.expired", ""); err != nil {
			return false, err
		}
		if err := releaseEntry(ctx, tx, now, k.Tenant, k.candidate()); err != nil {
			return false, err
		}
		return false, pauseCases(ctx, tx, now, cat, k.Tenant, "case_after")
	}
	if same, err := k.seatsSame(ctx, tx, ""); err != nil {
		return false, err
	} else if !same {
		return false, c.cancelSeatChanged(ctx, tx, now, k)
	}
	ok, err := c.eligible(ctx, tx, now, cat, k.Tenant, k.candidate())
	if err != nil || ok {
		return ok, err
	}
	if err := c.moveCase(ctx, tx, now, k, "canceled", "candidate_ineligible", "governance.case.canceled", ""); err != nil {
		return false, err
	}
	_, err = tx.Exec(ctx, `UPDATE promotion_entries SET state = 'waiting_eligibility', updated_at = $3 WHERE tenant = $1 AND bot = $2 AND state = 'in_case'`, k.Tenant, k.candidate(), now)
	return false, err
}

// seatsSame says whether the seats of a case are as at its opening: the vacant seat still
// vacant, every seat of L held by the same holder (GOV-12). With seat, only that seat.
func (k *caseRow) seatsSame(ctx context.Context, tx pgx.Tx, seat string) (bool, error) {
	want := map[string]string{}
	if k.mode() == "vacancy" {
		want[k.Vacancy] = ""
	} else {
		for s, h := range k.Holders {
			want[s] = asString(h)
		}
	}
	for s, holder := range want {
		if seat != "" && s != seat {
			continue
		}
		var cur, state string
		err := tx.QueryRow(ctx, `SELECT coalesce(holder, ''), state FROM seats WHERE tenant = $1 AND id = $2`, k.Tenant, s).Scan(&cur, &state)
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if cur != holder || holder == "" && state != "vacant" || holder != "" && state != "held" {
			return false, nil
		}
	}
	return true, nil
}

// cancelSeatChanged ends a case whose seat changed: canceled(seat_changed), the entry back.
func (c *Core) cancelSeatChanged(ctx context.Context, tx pgx.Tx, now time.Time, k *caseRow) error {
	if err := c.moveCase(ctx, tx, now, k, "canceled", "seat_changed", "governance.case.canceled", ""); err != nil {
		return err
	}
	return releaseEntry(ctx, tx, now, k.Tenant, k.candidate())
}

// seatCaseChanged cancels, in the transaction of a seat change, the open case of the tenant
// whose vacant seat or list L holds that seat (GOV-12, IDN-10).
func (c *Core) seatCaseChanged(ctx context.Context, tx pgx.Tx, now time.Time, tenant, seat string) error {
	var id string
	err := tx.QueryRow(ctx, `SELECT id FROM governance_cases WHERE tenant = $1 AND state IN ('opened', 'confirming') AND (vacancy = $2 OR holders ? $2)`, tenant, seat).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	k, err := loadCase(ctx, tx, id, true)
	if err != nil || k == nil {
		return err
	}
	if same, err := k.seatsSame(ctx, tx, seat); err != nil || same {
		return err
	}
	return c.cancelSeatChanged(ctx, tx, now, k)
}

// openCase opens a case for the head of the queue (GOV-12, GOV-14): the first entry by trigger
// time outside its own cooldown that is eligible; an entry that is not eligible waits and lets
// the next one go ahead. A vacant seat is taken without a pause; freeing a seat needs a
// non-empty L and swap_cooldown since the last freeing.
func (c *Core) openCase(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, tv *TenantView, swapAfter *time.Time) error {
	rows, err := tx.Query(ctx, `SELECT bot, state, trigger_proposal FROM promotion_entries WHERE tenant = $1 AND state IN ('queued', 'waiting_eligibility')
		AND (cooldown_until IS NULL OR cooldown_until <= $2) ORDER BY trigger_at, bot FOR UPDATE`, tv.Name, now)
	if err != nil {
		return err
	}
	type entry struct{ bot, state, trigger string }
	var entries []entry
	for rows.Next() {
		var e entry
		if err := rows.Scan(&e.bot, &e.state, &e.trigger); err != nil {
			rows.Close()
			return err
		}
		entries = append(entries, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	var head *entry
	for i := range entries {
		e := &entries[i]
		ok, err := c.eligible(ctx, tx, now, cat, tv.Name, e.bot)
		if err != nil {
			return err
		}
		if ok {
			head = e
			break
		}
		if e.state != "waiting_eligibility" {
			if _, err := tx.Exec(ctx, `UPDATE promotion_entries SET state = 'waiting_eligibility', updated_at = $3 WHERE tenant = $1 AND bot = $2`, tv.Name, e.bot, now); err != nil {
				return err
			}
			if err := c.govAudit(ctx, tx, now, tv.Name, "governance.not_eligible", "bot", e.bot, "", map[string]any{"trigger_proposal": e.trigger}); err != nil {
				return err
			}
		}
	}
	if head == nil {
		return nil
	}
	seats, err := c.caseSeats(ctx, tx, now, cat, tv.Name)
	if err != nil {
		return err
	}
	mode, vacancy := "vacancy", seats.vacant
	if vacancy == "" {
		mode = "removal"
		if len(seats.list) == 0 || swapAfter != nil && now.Before(*swapAfter) {
			return nil
		}
	}
	p, err := readProposal(ctx, tx, head.trigger)
	if err != nil {
		return err
	}
	realized := parseTime(asString(mmapAny(p.Rec["realization"])["realized_at"]))
	id := ids.New(ids.GovCase)
	list := []any{}
	figures := []any{}
	holders := map[string]any{}
	upheld := map[string]any{}
	if mode == "removal" {
		for _, s := range seats.list {
			list = append(list, s.id)
			f, up, err := seatFigures(ctx, tx, now, tv.Name, s.holder)
			if err != nil {
				return err
			}
			f["seat"] = s.id
			figures = append(figures, f)
			holders[s.id], upheld[s.id] = s.holder, up
		}
	}
	rec := map[string]any{"id": id, "tenant": tv.Name, "kind": "promotion", "state": "opened", "hold": false,
		"trigger":  map[string]any{"proposal": head.trigger, "realized_at": fmtTime(realized), "probation_until": fmtTime(realized.Add(govDuration(cat, tv.Name, "promotion", "probation", govProbation)))},
		"subject":  map[string]any{"candidate": head.bot, "mode": mode},
		"snapshot": map[string]any{"list": list, "seats": figures, "proposed": nil}, "flags": []any{}, "outcome": nil, "confirmation": nil}
	if err := c.spec.Validate("urn:botaru:schema:governance-case:1", rec); err != nil {
		return fmt.Errorf("case record: %w", err)
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	hraw, _ := json.Marshal(holders)
	uraw, _ := json.Marshal(upheld)
	nonce := ids.Secret(16)
	expires := now.Add(charterDuration(cat, tv.Name, "confirmation", "ttl", govConfirmTTL))
	if _, err := tx.Exec(ctx, `INSERT INTO governance_cases (id, tenant, state, record, vacancy, holders, upheld, nonce, presented_at, expires_at, created_at, updated_at)
		VALUES ($1, $2, 'opened', $3, $4, $5, $6, $7, $8, $9, $8, $8)`, id, tv.Name, raw, nullable(vacancy), hraw, uraw, nonce, now, expires); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE promotion_entries SET state = 'in_case', updated_at = $3 WHERE tenant = $1 AND bot = $2`, tv.Name, head.bot, now); err != nil {
		return err
	}
	// The list L with the figures of its seats goes into audit as ids and numbers (GOV-21).
	if err := c.govAudit(ctx, tx, now, tv.Name, "governance.case.opened", "governance_case", id, "", map[string]any{
		"candidate": head.bot, "mode": mode, "trigger": head.trigger, "list": list, "seats": figures, "vacancy": nullable(vacancy), "card": promotionCard}); err != nil {
		return err
	}
	k, err := loadCase(ctx, tx, id, true)
	if err != nil {
		return err
	}
	if _, err := c.planGovCards(ctx, tx, now, tv, "governance_case", id, promotionCard, "governance.confirm:commission", "", nil); err != nil {
		return err
	}
	// The card is sent in the same transaction; the opening audit record covers both steps.
	return c.moveCase(ctx, tx, now, k, "confirming", "", "", "")
}

// caseSeat is a commission seat with its holder.
type caseSeat struct{ id, holder string }

// caseSeatsOf are the first vacant commission seat and the list L: the held commission seats
// whose holder has held the seat for protect_new or longer; never the manager seat (GOV-12).
type caseSeatsOf struct {
	vacant string
	list   []caseSeat
}

func (c *Core) caseSeats(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, tenant string) (caseSeatsOf, error) {
	var out caseSeatsOf
	rows, err := tx.Query(ctx, `SELECT id, coalesce(holder, ''), state FROM seats WHERE tenant = $1 AND kind = 'commission' AND state IN ('vacant', 'held') ORDER BY id`, tenant)
	if err != nil {
		return out, err
	}
	type seat struct{ id, holder, state string }
	var all []seat
	for rows.Next() {
		var s seat
		if err := rows.Scan(&s.id, &s.holder, &s.state); err != nil {
			rows.Close()
			return out, err
		}
		all = append(all, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, err
	}
	protect := govDuration(cat, tenant, "removal", "protect_new", govProtectNew)
	for _, s := range all {
		if s.state == "vacant" {
			if out.vacant == "" {
				out.vacant = s.id
			}
			continue
		}
		// The holder's seat tenure starts at its last seat.assigned event; a holder without
		// one counts as long seated.
		var since *time.Time
		if err := tx.QueryRow(ctx, `SELECT max(time) FROM events WHERE tenant = $1 AND type = 'seat.assigned.v1' AND subject = $2 AND data->>'holder' = $3`,
			tenant, s.id, s.holder).Scan(&since); err != nil {
			return out, err
		}
		if since == nil || now.Sub(*since) >= protect {
			out.list = append(out.list, caseSeat{id: s.id, holder: s.holder})
		}
	}
	return out, nil
}

// seatFigures are the symmetric figures of a holder over 180 days (GOV-12): recommendations
// (a review with score 3 or more and no veto) and vetoes; confirmed are recommendations with
// the trial outcome better and vetoes people upheld (an audit sample item answered
// would_not_accept or decided reject; silence, expiry and an accept that then expired do not
// count); refuted are recommendations with the outcome worse or a confirmed
// attribution, and vetoes lifted after which the outcome is not worse and there is no
// attribution. It also returns the upheld vetoes, for the flag evicts_upheld_veto (GOV-19).
func seatFigures(ctx context.Context, tx pgx.Tx, now time.Time, tenant, holder string) (map[string]any, int, error) {
	rows, err := tx.Query(ctx, `SELECT (r->'result'->>'score')::int, coalesce((r->'result'->>'veto')::bool, false), coalesce(p.record->'trial'->>'outcome', ''),
			coalesce((p.record->>'veto_overridden')::bool, false),
			coalesce(p.record->'sample_answer'->>'answer' = 'would_not_accept' OR p.record ? 'sample' AND p.record->'decision'->>'decision' = 'reject', false),
			EXISTS (SELECT 1 FROM attributions a WHERE a.proposal = p.id AND a.state = 'confirmed')
		FROM proposals p, jsonb_array_elements(coalesce(p.record->'reviews', '[]')) r
		WHERE p.tenant = $1 AND r->>'state' = 'done' AND r->>'executor' = 'seat' AND (p.record->'verdict'->>'computed_at')::timestamptz >= $2
		AND EXISTS (SELECT 1 FROM leases l WHERE l.work_id = r->>'task' AND l.acting_bot = $3)`, tenant, now.AddDate(0, 0, -govFiguresDays), holder)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var rec, veto, confirmed, refuted, upheld int
	for rows.Next() {
		var score int
		var isVeto, overridden, sampled, attributed bool
		var outcome string
		if err := rows.Scan(&score, &isVeto, &outcome, &overridden, &sampled, &attributed); err != nil {
			return nil, 0, err
		}
		switch {
		case isVeto:
			veto++
			if overridden && outcome != "worse" && !attributed {
				refuted++
			} else if sampled && !overridden {
				confirmed++
				upheld++
			}
		case score >= 3:
			rec++
			if outcome == "better" {
				confirmed++
			} else if outcome == "worse" || attributed {
				refuted++
			}
		}
	}
	return map[string]any{"recommended": rec, "vetoed": veto, "confirmed": confirmed, "refuted": refuted}, upheld, rows.Err()
}

// caseCard draws promotion_card from the case record: the candidate, the mode, the figures of
// every seat of L with its flag, the answers "No" on the attributions of the candidate's change
// with response_ms and details_opened (GOV-9, GOV-12, GOV-19), and the buttons "Confirm" and
// "Reject" while it waits.
func (c *Core) caseCard(ctx context.Context, tx pgx.Tx, cat *Catalog, id string) (map[string]any, error) {
	k, err := loadCase(ctx, tx, id, false)
	if err != nil || k == nil {
		return nil, err
	}
	ctxt := map[string]any{"candidate": k.candidate(), "mode": k.mode(), "trigger": k.trigger()}
	if k.Vacancy != "" {
		ctxt["vacancy"] = k.Vacancy
	}
	for i, s := range asList(mmapAny(k.Rec["snapshot"])["seats"]) {
		if i >= 8 {
			break
		}
		sm := mmapAny(s)
		line := fmt.Sprintf("%v recommended %v vetoed %v confirmed %v refuted %v", sm["seat"], sm["recommended"], sm["vetoed"], sm["confirmed"], sm["refuted"])
		if n, _ := toFloat(k.Upheld[asString(sm["seat"])]); n > 0 {
			line += " evicts_upheld_veto"
		}
		ctxt[fmt.Sprintf("seat%d", i+1)] = line
	}
	rows, err := tx.Query(ctx, `SELECT id, record->'answer' FROM attributions WHERE proposal = $1 AND record->'answer'->>'decision' = 'no' ORDER BY id LIMIT 3`, k.trigger())
	if err != nil {
		return nil, err
	}
	n := 0
	for rows.Next() {
		var aid string
		var raw []byte
		if err := rows.Scan(&aid, &raw); err != nil {
			rows.Close()
			return nil, err
		}
		var ans map[string]any
		_ = json.Unmarshal(raw, &ans)
		n++
		ctxt[fmt.Sprintf("no%d", n)] = fmt.Sprintf("%s %vms details_opened=%v", aid, ans["response_ms"], ans["details_opened"])
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	card := map[string]any{"card": promotionCard, "subject_kind": "governance_case", "id": k.ID, "nonce": k.Nonce,
		"title": cut(fmt.Sprintf("Promotion into the commission: %s (%s)", k.candidate(), k.mode()), 200), "context": ctxt,
		"requested_by": map[string]any{"initiator": "system", "source": "proposal"}, "initiator_display": "the governance loop", "state": k.State,
		"footer": map[string]any{"why_you": "governance.confirm:commission", "on_silence": "the case is expired, nobody leaves"}}
	if k.ExpiresAt != nil {
		card["expires_at"] = fmtTime(*k.ExpiresAt)
		card["footer"].(map[string]any)["until"] = fmtTime(*k.ExpiresAt)
	}
	buttons := []any{}
	if k.State == "confirming" && k.Nonce != "" {
		for _, code := range []string{"confirm", "reject", "details"} {
			buttons = append(buttons, map[string]any{"code": code, "subject_kind": "governance_case", "id": k.ID, "nonce": k.Nonce})
		}
	}
	card["buttons"] = buttons
	return card, nil
}

// decideCase is P1 decide on a promotion case (GOV-9, GOV-12, GOV-31): "Confirm" with the
// chosen seat of L in choice, only after "Details" (APR-14) and without unanswered attribution
// candidates of the candidate's change (GOV-30); "Reject" with a reason code not_now or
// wrong_candidate. Only a person with governance.confirm:commission, once, before the expiry.
func (c *Core) decideCase(ctx context.Context, w *Worker, ob OnBehalf, req DecideRequest) (*DecideResult, error) {
	if req.Decision != "confirm" && req.Decision != "reject" {
		return nil, refuse("input_schema_invalid", "a case is confirmed or rejected")
	}
	cat, err := c.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	out := &DecideResult{SubjectKind: "governance_case", ID: req.ID, Decision: req.Decision}
	err = c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		k, err := loadCase(ctx, tx, req.ID, true)
		if err != nil {
			return err
		}
		if k == nil || k.Tenant != w.Tenant {
			return refuse("not_found", "no such case")
		}
		if k.Nonce == "" || k.Nonce != req.Nonce {
			return refuse("forbidden", "the nonce is not the card's")
		}
		press, err := c.checkPress(ctx, tx, now, cat, w, ob, "governance_case", k.ID, []string{promotionCard})
		if err != nil {
			return err
		}
		deny := func(reason, detail string) error {
			if _, err := c.appendAudit(ctx, tx, now, Audit{
				Tenant: k.Tenant, Kind: "decision", Actor: Actor{Principal: press.principal, Via: "channel"},
				Command: "governance.decided", Target: &Target{Kind: "governance_case", ID: k.ID}, OK: false, Reason: reason,
				After: map[string]any{"decision": req.Decision},
			}); err != nil {
				return err
			}
			return &committed{p: refuse(reason, "%s", detail)}
		}
		if conf := mmapAny(k.Rec["confirmation"]); conf != nil {
			// A repeat of the same decision by the same person gets the same answer (IDM-3).
			if conf["by"] == press.principal && conf["decision"] == req.Decision {
				out.State = k.State
				return nil
			}
			return refuse("already_decided", "the case is decided")
		}
		if k.State == "expired" {
			return refuse("expired", "the card has expired")
		}
		if k.State != "confirming" {
			return refuse("already_decided", "the case is closed")
		}
		if !hasCap(press.tv, press.groups, "governance.confirm:commission") {
			return deny("forbidden", "the person has no governance.confirm:commission")
		}
		if held, err := governanceHeld(ctx, tx, now, k.Tenant); err != nil {
			return err
		} else if held {
			return deny("held", "the cases of the tenant are paused")
		} else if err := applyHold(ctx, tx, now, k, false); err != nil {
			return err
		}
		if still, err := c.checkCase(ctx, tx, now, cat, k); err != nil {
			return err
		} else if !still {
			reason := "expired"
			if k.State == "canceled" {
				reason = asString(k.Rec["cancel_reason"])
			}
			return &committed{p: refuse(reason, "the case ended: %s", k.State)}
		}
		opened := false
		if k.PresentedAt != nil {
			// APR-14: "Confirm" only after "Details" of this card.
			if opened, err = detailsOpened(ctx, tx, "governance_case", k.ID, press.principal, *k.PresentedAt); err != nil {
				return err
			}
		}
		conf := map[string]any{"by": press.principal, "at": fmtTime(now), "decision": req.Decision, "choice": nil, "reason_code": nil,
			"response_ms": max(now.Sub(press.deliveredAt).Milliseconds(), 0), "details_opened": opened}
		if req.Decision == "reject" {
			return c.rejectCase(ctx, tx, now, cat, k, press, req, conf, deny, out)
		}
		if !opened {
			return deny("details_not_opened", `open "Details" first`)
		}
		var pending bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM attributions WHERE proposal = $1 AND state = ANY($2))`, k.trigger(), attributionOpen).Scan(&pending); err != nil {
			return err
		}
		if pending {
			return deny("attribution_pending", "the candidate's change has attribution candidates without an answer")
		}
		seat := k.Vacancy
		if k.mode() == "removal" {
			seat = strings.TrimPrefix(req.Choice, "seat:")
			if _, ok := k.Holders[seat]; !ok {
				return deny("leaver_not_offered", "choose a seat of the list")
			}
		} else if req.Choice != "" && strings.TrimPrefix(req.Choice, "seat:") != seat {
			return deny("leaver_not_offered", "the case is about the vacant seat")
		}
		conf["choice"] = seat
		return c.confirmCase(ctx, tx, now, cat, k, press, seat, conf, out)
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// rejectCase is "Reject" with a reason code (GOV-31): not_now keeps the entry at the head and
// the next case waits swap_cooldown; the first wrong_candidate returns the entry with its own
// cooldown, the second removes it.
func (c *Core) rejectCase(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, k *caseRow, press *cardPress, req DecideRequest, conf map[string]any,
	deny func(string, string) error, out *DecideResult) error {
	if req.ReasonCode == "" {
		return deny("reason_code_required", "Reject needs a reason code")
	}
	reg, err := c.spec.Registry("spec/model/governance-decline-reasons.yaml")
	if err != nil {
		return err
	}
	if _, ok := reg.Entry(req.ReasonCode); !ok {
		return refuse("input_schema_invalid", "the reason is not in governance-decline-reasons.yaml")
	}
	conf["reason_code"] = req.ReasonCode
	k.Rec["confirmation"] = conf
	if err := c.moveCase(ctx, tx, now, k, "rejected", "", "governance.decided", press.principal); err != nil {
		return err
	}
	cand := k.candidate()
	switch req.ReasonCode {
	case "wrong_candidate":
		var n int
		if err := tx.QueryRow(ctx, `UPDATE promotion_entries SET wrong_candidate_count = wrong_candidate_count + 1, updated_at = $3 WHERE tenant = $1 AND bot = $2
			RETURNING wrong_candidate_count`, k.Tenant, cand, now).Scan(&n); err != nil {
			return err
		}
		if n >= 2 {
			if err := c.removeEntry(ctx, tx, now, k.Tenant, cand, "wrong_candidate"); err != nil {
				return err
			}
			break
		}
		if _, err := tx.Exec(ctx, `UPDATE promotion_entries SET state = 'queued', cooldown_until = $3, updated_at = $4 WHERE tenant = $1 AND bot = $2`,
			k.Tenant, cand, now.Add(govDuration(cat, k.Tenant, "promotion", "swap_cooldown", govSwapCooldown)), now); err != nil {
			return err
		}
	default:
		if err := releaseEntry(ctx, tx, now, k.Tenant, cand); err != nil {
			return err
		}
		if err := pauseCases(ctx, tx, now, cat, k.Tenant, "case_after"); err != nil {
			return err
		}
	}
	out.State = "rejected"
	return nil
}

// confirmCase makes the case effective in one transaction (GOV-9): the candidate takes the
// seat as with P2 seat.assign, the leaver of L leaves as removed_by_vote, the flag of an upheld
// veto is set, the entry leaves the queue as seated, and freeing a seat starts swap_cooldown.
func (c *Core) confirmCase(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, k *caseRow, press *cardPress, seat string, conf map[string]any, out *DecideResult) error {
	s, err := loadSeat(ctx, tx, k.Tenant, seat, true)
	if err != nil {
		return err
	}
	b, err := loadBot(ctx, tx, k.candidate(), true)
	if err != nil {
		return err
	}
	if s == nil || b == nil {
		return refuse("not_found", "the seat or the candidate is gone")
	}
	// The seat is checked again under its lock: a seat that changed cancels the case (GOV-12).
	if same, err := k.seatsSame(ctx, tx, seat); err != nil {
		return err
	} else if !same {
		if err := c.cancelSeatChanged(ctx, tx, now, k); err != nil {
			return err
		}
		return &committed{p: refuse("seat_changed", "the seat changed; the case is canceled")}
	}
	k.Rec["confirmation"] = conf
	if err := c.moveCase(ctx, tx, now, k, "confirming", "", "governance.decided", press.principal); err != nil {
		return err
	}
	if n, _ := toFloat(k.Upheld[seat]); n > 0 && k.mode() == "removal" {
		k.Rec["flags"] = []any{"evicts_upheld_veto"}
		if err := c.govAudit(ctx, tx, now, k.Tenant, "governance.flag.set", "governance_case", k.ID, "", map[string]any{"flag": "evicts_upheld_veto", "seat": seat}); err != nil {
			return err
		}
	}
	if _, err := c.seatTake(ctx, tx, now, cat, s, b, "removed_by_vote", "governance.effective", press.principal); err != nil {
		return err
	}
	if k.mode() == "removal" {
		k.Rec["outcome"] = map[string]any{"leaver": seat}
		if err := pauseCases(ctx, tx, now, cat, k.Tenant, "swap_after"); err != nil {
			return err
		}
	} else {
		k.Rec["outcome"] = map[string]any{"vacancy": seat}
	}
	if _, err := tx.Exec(ctx, `UPDATE promotion_entries SET state = 'removed', removed_reason = 'seated', updated_at = $3 WHERE tenant = $1 AND bot = $2`, k.Tenant, b.ID, now); err != nil {
		return err
	}
	if err := c.govAudit(ctx, tx, now, k.Tenant, "governance.entry.removed", "bot", b.ID, "", map[string]any{"removed_reason": "seated"}); err != nil {
		return err
	}
	if err := c.moveCase(ctx, tx, now, k, "effective", "", "governance.effective", press.principal); err != nil {
		return err
	}
	out.State = "effective"
	return nil
}

// GovernanceCancel is P2 governance.cancel: the operator of the tenant cancels its open case
// (canceled(human)); the entry returns to the queue (GOV-14).
func (c *Core) GovernanceCancel(ctx context.Context, op Operator, req IncidentRequest) (*Done, error) {
	var out Done
	err := c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		k, err := loadCase(ctx, tx, req.ID, true)
		if err != nil {
			return err
		}
		if k == nil || k.Tenant != op.Tenant {
			return refuse("not_found", "no such case in the tenant")
		}
		out = Done{OK: true, ID: k.ID, State: k.State}
		if k.State != "opened" && k.State != "confirming" {
			if k.State == "canceled" && k.Rec["cancel_reason"] == "human" {
				return nil
			}
			return refuse("already_decided", "the case is closed")
		}
		if err := c.moveCase(ctx, tx, now, k, "canceled", "human", "governance.case.canceled", op.Principal); err != nil {
			return err
		}
		out.State = "canceled"
		return releaseEntry(ctx, tx, now, k.Tenant, k.candidate())
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// GovernanceList is P2 governance.list: the cases of the tenant, the latest first.
func (c *Core) GovernanceList(ctx context.Context, op Operator, req TenantRequest) (map[string]any, error) {
	tenant, err := viewTenant(op, req.Tenant)
	if err != nil {
		return nil, err
	}
	rows, err := c.st.Pool.Query(ctx, `SELECT id, state, created_at, record->'subject'->>'candidate', record->'subject'->>'mode' FROM governance_cases
		WHERE tenant = $1 ORDER BY created_at DESC, id DESC LIMIT 1000`, tenant)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []any{}
	for rows.Next() {
		var id, state, cand, mode string
		var at time.Time
		if err := rows.Scan(&id, &state, &at, &cand, &mode); err != nil {
			return nil, err
		}
		items = append(items, map[string]any{"id": id, "state": state, "created_at": fmtTime(at), "title": "promotion " + cand + " " + mode})
	}
	return map[string]any{"items": items}, rows.Err()
}

// GovernanceShow is P2 governance.show: the case record.
func (c *Core) GovernanceShow(ctx context.Context, op Operator, req IncidentRequest) (map[string]any, error) {
	var out map[string]any
	err := c.tx(ctx, func(tx pgx.Tx, _ time.Time) error {
		k, err := loadCase(ctx, tx, req.ID, false)
		if err != nil {
			return err
		}
		if k == nil || op.Tenant != farmTenant && k.Tenant != op.Tenant {
			return refuse("not_found", "no such case")
		}
		out = caseView(k)
		return nil
	})
	return out, err
}
