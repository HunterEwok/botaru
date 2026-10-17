// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/ids"
)

// Promotion cases: their rows and steps, the holds on them, the opening of a case and its card.

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
	var proposed any
	var tiedSeats []any
	flags := []any{}
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
		// From v0.3 the card shows the accuracy of every seat of L, and with accountability
		// on code proposes the leaver (COM-17, GOV-43).
		lv, err := c.proposeLeaver(ctx, tx, now, cat, tv.Name, seats.list)
		if err != nil {
			return err
		}
		for _, f := range figures {
			fm := f.(map[string]any)
			a := lv.acc[asString(fm["seat"])]
			fm["accuracy"], fm["scored_forecasts"] = a.view(), a.N
		}
		if lv.proposed != "" {
			proposed = lv.proposed
		}
		for _, t := range lv.tied {
			tiedSeats = append(tiedSeats, t)
		}
		if lv.onlyScoredVeto {
			flags = append(flags, "evicts_only_scored_veto")
		}
	}
	rec := map[string]any{"id": id, "tenant": tv.Name, "kind": "promotion", "state": "opened", "hold": false,
		"trigger":  map[string]any{"proposal": head.trigger, "realized_at": fmtTime(realized), "probation_until": fmtTime(realized.Add(govDuration(cat, tv.Name, "promotion", "probation", govProbation)))},
		"subject":  map[string]any{"candidate": head.bot, "mode": mode},
		"snapshot": map[string]any{"list": list, "seats": figures, "proposed": proposed}, "flags": flags, "outcome": nil, "confirmation": nil}
	if len(tiedSeats) > 0 {
		rec["snapshot"].(map[string]any)["tied"] = tiedSeats
	}
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
		"candidate": head.bot, "mode": mode, "trigger": head.trigger, "list": list, "seats": figures, "vacancy": nullable(vacancy), "card": promotionCard,
		"proposed": proposed}); err != nil {
		return err
	}
	for _, f := range flags {
		if err := c.govAudit(ctx, tx, now, tv.Name, "governance.flag.set", "governance_case", id, "", map[string]any{"flag": f, "seat": proposed}); err != nil {
			return err
		}
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
			EXISTS (SELECT 1 FROM attributions a WHERE a.proposal = p.id AND a.kind = 'change' AND a.state = 'confirmed')
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
		if a, ok := sm["accuracy"]; ok {
			line += fmt.Sprintf(" accuracy %v scored %v", a, sm["scored_forecasts"])
		}
		if sm["seat"] == mmapAny(k.Rec["snapshot"])["proposed"] {
			line += " proposed"
		}
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
