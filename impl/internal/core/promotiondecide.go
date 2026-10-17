// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// A person's decision on a case, the P2 governance operations and the proposal of a leaver.

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
		if c.missingDetails(promotionCard, req.Decision, opened) {
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
			// A leaver code proposed is the only choice; with two tied worst seats the person
			// chooses among them (GOV-43).
			snap := mmapAny(k.Rec["snapshot"])
			if p := asString(snap["proposed"]); p != "" && p != seat {
				return deny("leaver_not_offered", "confirm the proposed seat or answer Not now")
			}
			if tied := strList(snap["tied"]); len(tied) > 0 && !slices.Contains(tied, seat) {
				return deny("leaver_not_offered", "choose one of the tied seats")
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

// leaverProposal is what code proposes for a case that frees a seat (GOV-43).
type leaverProposal struct {
	acc            map[string]accuracy
	proposed       string
	tied           []string
	onlyScoredVeto bool
}

// proposeLeaver proposes the holder of L with the worst accuracy in its lens (GOV-43, COM-17),
// only with commission.accountability on: a holder with fewer scored forecasts than
// accountability_min_forecasts counts with the median of its lens, or as the worst once it has
// held the seat for more than min_tenure plus 90 days by its last seat.assigned.v1 event; with no holder of L scored enough, or the
// two worst tied, code proposes nobody (and with a tie the person chooses among the tied seats).
// The reward protection of COM-18 is not built, so nobody is protected. The flag
// evicts_only_scored_veto marks a proposed holder that is the only holder of a veto lens with
// enough scored forecasts (GOV-19).
func (c *Core) proposeLeaver(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, tenant string, list []caseSeat) (leaverProposal, error) {
	out := leaverProposal{acc: map[string]accuracy{}}
	need := minForecasts(cat, tenant)
	lenses := map[string]string{}
	byLens := map[string][]float64{}
	enough := false
	rows, err := tx.Query(ctx, `SELECT id, coalesce(lens, ''), coalesce(holder, '') FROM seats WHERE tenant = $1 AND kind = 'commission' AND state = 'held' ORDER BY id`, tenant)
	if err != nil {
		return out, err
	}
	type held struct{ id, lens, holder string }
	var all []held
	for rows.Next() {
		var h held
		if err := rows.Scan(&h.id, &h.lens, &h.holder); err != nil {
			rows.Close()
			return out, err
		}
		all = append(all, h)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, err
	}
	scoredVeto := map[string]int{}
	defs := c.lensDefs()
	for _, h := range all {
		a, err := lensAccuracy(ctx, tx, now, tenant, h.lens, h.holder)
		if err != nil {
			return out, err
		}
		out.acc[h.id], lenses[h.id] = a, h.lens
		if a.N >= need && a.OK {
			byLens[h.lens] = append(byLens[h.lens], a.Value)
			if defs[h.lens].Veto {
				scoredVeto[h.lens]++
			}
		}
	}
	if accountabilityMode(cat, tenant) != "on" {
		return out, nil
	}
	if on, err := c.accountabilityOn(ctx, tx, cat, tenant); err != nil || on == nil {
		return out, err
	}
	type cand struct {
		seat string
		acc  accuracy
	}
	var cands []cand
	long := govDuration(cat, tenant, "promotion", "min_tenure", govMinTenure) + 90*24*time.Hour
	for _, s := range list {
		a := out.acc[s.id]
		switch {
		case a.N >= need && a.OK:
			enough = true
		default:
			var since *time.Time
			if err := tx.QueryRow(ctx, `SELECT max(time) FROM events WHERE tenant = $1 AND type = 'seat.assigned.v1' AND subject = $2 AND data->>'holder' = $3`,
				tenant, s.id, s.holder).Scan(&since); err != nil {
				return out, err
			}
			// A holder whose assignment event is gone (pruned) has no known tenure and counts
			// with the median, not as the worst.
			if since != nil && now.Sub(*since) > long {
				a = accuracy{Lens: a.Lens, N: a.N, Value: -2, OK: true}
			} else {
				a = accuracy{Lens: a.Lens, N: a.N, Value: median(byLens[lenses[s.id]]), OK: true}
			}
		}
		cands = append(cands, cand{seat: s.id, acc: a})
	}
	if !enough || len(cands) == 0 {
		return out, nil
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].acc.Value < cands[j].acc.Value })
	if len(cands) > 1 && tied(cands[0].acc, cands[1].acc) {
		for _, x := range cands {
			if tied(cands[0].acc, x.acc) {
				out.tied = append(out.tied, x.seat)
			}
		}
		return out, nil
	}
	out.proposed = cands[0].seat
	l := lenses[out.proposed]
	if a := out.acc[out.proposed]; defs[l].Veto && a.N >= need && a.OK && scoredVeto[l] == 1 {
		out.onlyScoredVeto = true
	}
	return out, nil
}
