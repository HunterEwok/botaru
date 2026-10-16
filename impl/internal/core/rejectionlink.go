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
)

// Links of incidents to rejected proposals and who answers for a link (GOV-38, GOV-40, GOV-41,
// GOV-42). Only code finds a candidate; only a person confirms it, on the card with
// commission.accountability on. The holders who answer for a confirmed link leave their seats
// and are parked, at most one per tenant in 30 days. In shadow code finds the cases and keeps
// the figures for people, and nobody leaves (GOV-36).

const (
	// rejectedLookback is how long before an incident a rejecting verdict can be linked to it.
	rejectedLookback = 90 * 24 * time.Hour
	// linksPerIncident, linksPerRoleCases are the ceilings of GOV-38.
	linksPerIncident  = 3
	linksPerRoleCases = 2
	// selfAlarmShare is the share of the alarm series points of the author's role in the 7 days
	// before the incident from which there is no candidate (GOV-38, as PRP-22).
	selfAlarmShare = 0.3
	selfAlarmDays  = 7
	// departurePace is the least time between two departures for a link in a tenant, and
	// unseatCooldown how long a bot that left for a link gets no trigger and no case (GOV-42).
	departurePace  = 30 * 24 * time.Hour
	unseatCooldown = 180 * 24 * time.Hour
	// parkedFactsFor is how long the facts of a parked bot are offered to the next holder.
	parkedFactsFor = 30 * 24 * time.Hour
)

// externalCauses are the causes a person records for an incident that never link it (GOV-38).
var externalCauses = []string{"external_load", "attack"}

// createRejectionLinks creates the candidates of the rules rejected_direct and
// rejected_dependency_1 (GOV-38): an incident whose subject is among the subjects of the problem
// of a proposal the commission dismissed (veto, below the minimum, low score) or parked by score
// and that expired, with that verdict after governance_v03_since, earlier than the incident
// opened and not earlier than 90 days before; the change names the subject as its target, or an
// object the subject depends on by one step in the catalog (catalogdeps.go, the same step as
// dependency_1 of GOV-13); the problem code is in the class's problem_codes; the
// problem names at most 3 subjects. Never for an external cause, untrusted_seen, a proposal of
// the farm manager seat holder (origin.level 2), or an alarm series written by the author's role
// with a share of 0.3 or more in the 7 days before; at most one case per proposal, 3 candidates
// per incident and 2 cases per author role in 90 days. The model takes no part.
func (c *Core) createRejectionLinks(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, tv *TenantView) error {
	since, _, err := v03State(ctx, tx, tv.Name)
	if err != nil || since == nil {
		return err
	}
	rows, err := tx.Query(ctx, `SELECT id, class, subject_kind, subject_id, opened_at FROM incidents
		WHERE tenant = $1 AND state <> 'dismissed' AND opened_at >= $2 AND coalesce(record->>'cause', '') <> ALL($3) ORDER BY opened_at, id`,
		tv.Name, maxTime(*since, now.Add(-attributionLookback)), externalCauses)
	if err != nil {
		return err
	}
	type inc struct {
		id, class, kind, subject string
		at                       time.Time
	}
	var incs []inc
	for rows.Next() {
		var i inc
		if err := rows.Scan(&i.id, &i.class, &i.kind, &i.subject, &i.at); err != nil {
			rows.Close()
			return err
		}
		incs = append(incs, i)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, i := range incs {
		codes := strList(classByName(cat, i.class)["problem_codes"])
		if len(codes) == 0 {
			continue
		}
		deps, err := subjectDeps(ctx, tx, cat, tv.Name, i.kind, i.subject)
		if err != nil {
			return err
		}
		prows, err := tx.Query(ctx, `SELECT id FROM proposals WHERE tenant = $1 AND kind <> 'none'
			AND (record->'verdict'->>'outcome' = ANY($2) OR record->'verdict'->>'outcome' = 'parked(score)' AND state = 'expired')
			AND (record->'verdict'->>'computed_at')::timestamptz < $3 AND (record->'verdict'->>'computed_at')::timestamptz >= $4
			AND (record->'verdict'->>'computed_at')::timestamptz >= $5
			ORDER BY (record->'verdict'->>'computed_at')::timestamptz, id`,
			tv.Name, []string{"dismissed(veto)", "dismissed(below_min)", "dismissed(low_score)"}, i.at, i.at.Add(-rejectedLookback), *since)
		if err != nil {
			return err
		}
		pids, err := pgx.CollectRows(prows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		subject := i.kind + ":" + i.subject
		for _, pid := range pids {
			var n int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM attributions WHERE incident = $1 AND kind = 'rejected_proposal' AND state <> 'canceled'`, i.id).Scan(&n); err != nil {
				return err
			}
			if n >= linksPerIncident {
				break
			}
			p, err := readProposal(ctx, tx, pid)
			if err != nil {
				return err
			}
			b := p.body()
			origin := mmapAny(p.Rec["origin"])
			level, _ := toFloat(origin["level"])
			rule := changeRule(b.Change, i.kind, i.subject, deps, "rejected_direct", "rejected_dependency_1")
			if level == 2 || mmapAny(p.Rec["untrusted_seen"])["value"] == true || !slices.Contains(codes, b.Problem.Code) ||
				len(b.Problem.Subjects) > 3 || !slices.Contains(b.Problem.Subjects, subject) || rule == "" {
				continue
			}
			var hasCase bool
			var roleCases int
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM attributions WHERE proposal = $1 AND kind = 'rejected_proposal' AND state <> 'canceled'),
				(SELECT count(*) FROM attributions a JOIN proposals q ON q.id = a.proposal WHERE a.tenant = $2 AND a.kind = 'rejected_proposal'
					AND a.state <> 'canceled' AND q.role = $3 AND a.created_at >= $4)`, p.ID, tv.Name, p.Role, now.Add(-rejectedLookback)).Scan(&hasCase, &roleCases); err != nil {
				return err
			}
			if hasCase || roleCases >= linksPerRoleCases {
				continue
			}
			share, err := authorAlarmShare(ctx, tx, tv.Name, subject, mstr(mmap(classByName(cat, i.class), "detect"), "alarm"), i.at, asString(origin["worker"]), p.Role)
			if err != nil {
				return err
			}
			if share >= selfAlarmShare {
				continue
			}
			if err := c.createAttribution(ctx, tx, now, tv, i.id, p.ID, "rejected_proposal", rule, "system"); err != nil {
				return err
			}
		}
	}
	return nil
}

// authorAlarmShare is the share of the points of the alarm series of the incident class on the
// incident subject in the 7 days before the incident that the author's worker or its role wrote
// (GOV-38, as PRP-22). The events catalog does not say which series an alarm is built from, so the
// alarm series is the series named like the alarm, alarm.<name>.vN giving <name>; an alarm of the
// monitoring system has no such series and gives 0.
func authorAlarmShare(ctx context.Context, tx pgx.Tx, tenant, subject, alarm string, at time.Time, worker, role string) (float64, error) {
	series := alarmSeriesName(alarm)
	if series == "" {
		return 0, nil
	}
	var total, own int
	err := tx.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE worker = $5 OR role = $6) FROM obs_points
		WHERE tenant = $1 AND subject = $2 AND at >= $3 AND at < $4 AND series = $7`, tenant, subject, at.AddDate(0, 0, -selfAlarmDays), at, worker, role, series).Scan(&total, &own)
	if err != nil || total == 0 {
		return 0, err
	}
	return float64(own) / float64(total), nil
}

// alarmSeriesName is the series named like an alarm: alarm.<name>.vN gives <name>.
func alarmSeriesName(alarm string) string {
	name, ok := strings.CutPrefix(alarm, "alarm.")
	if !ok {
		return ""
	}
	if i := strings.LastIndex(name, ".v"); i > 0 && strings.Trim(name[i+2:], "0123456789") == "" && len(name) > i+2 {
		name = name[:i]
	}
	return name
}

// managerAgainst says whether the farm manager's review of a proposal was against it: a veto, a
// score of 1 or less, or a missing or invalid review (GOV-41). The manager never answers for a
// link (D-33); its being against is shown on the link card and counted in the holder report
// (MGR-50).
func managerAgainst(p *proposalRow) bool {
	for _, r := range asList(p.Rec["reviews"]) {
		rm := mmapAny(r)
		if asString(rm["lens"]) != "farm_manager" {
			continue
		}
		if rm["state"] != "done" {
			return true
		}
		res := mmapAny(rm["result"])
		score, _ := toFloat(res["score"])
		return res["veto"] == true || score <= 1
	}
	return false
}

// managerAgainstLinks counts the links to rejected proposals confirmed in [since, until) whose
// proposal the farm manager was against (GOV-41, MGR-50), across the farm.
func managerAgainstLinks(ctx context.Context, tx pgx.Tx, since, until time.Time) (int, error) {
	var n int
	err := tx.QueryRow(ctx, `SELECT count(*) FROM attributions WHERE kind = 'rejected_proposal' AND state = 'confirmed'
		AND record->>'manager_against' = 'true' AND (record->'answer'->>'at')::timestamptz >= $1 AND (record->'answer'->>'at')::timestamptz < $2`, since, until).Scan(&n)
	return n, err
}

// answerer is a seat holder's review of a linked proposal as GOV-41 reads it.
type answerer struct {
	Seat, Bot, Lens          string
	Veto, Against, For, Miss bool
	Answers                  bool
}

// lensAccountable is the accountable_for of a lens of spec/model/lenses.yaml (GOV-41).
func (c *Core) lensAccountable(lens string) []string {
	reg, err := c.spec.Registry("spec/model/lenses.yaml")
	if err != nil {
		return nil
	}
	e, _ := reg.Entry(lens)
	return strList(e["accountable_for"])
}

// answerers finds who answers for a confirmed link (GOV-41): only real reviews of seat holders
// in the current seat epoch; a veto or a score of 1 or less is against, the rest for, a missing
// or invalid review both. For a link to a rejected proposal the holders who were against answer,
// a veto never; nobody when people rejected it themselves (a blind "Would not accept", a decline,
// an expiry with people). For an attribution to a recommended change the holders who were for it
// answer when their lens's accountable_for has the class's area. A proposal of the farm manager
// seat holder, untrusted_seen and an external cause make nobody answer; the farm manager lens
// never answers (D-33). It returns every holder review with its mark.
func (c *Core) answerers(ctx context.Context, tx pgx.Tx, cat *Catalog, a *attributionRow, p *proposalRow) ([]answerer, error) {
	var class, cause string
	if err := tx.QueryRow(ctx, `SELECT class, coalesce(record->>'cause', '') FROM incidents WHERE id = $1`, a.Incident).Scan(&class, &cause); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	level, _ := toFloat(mmapAny(p.Rec["origin"])["level"])
	nobody := level == 2 || mmapAny(p.Rec["untrusted_seen"])["value"] == true || slices.Contains(externalCauses, cause)
	if a.Kind == "rejected_proposal" {
		d := mmapAny(p.Rec["decision"])
		switch {
		case asString(mmapAny(p.Rec["sample_answer"])["answer"]) == "would_not_accept":
			nobody = true
		case d["decision"] == "reject" && d["reason_code"] != "other":
			nobody = true
		case p.State == "expired" && p.PresentedAt != nil:
			nobody = true
		}
	}
	area := asString(classByName(cat, class)["area"])
	var out []answerer
	for _, r := range asList(p.Rec["reviews"]) {
		rm := mmapAny(r)
		lens := asString(rm["lens"])
		if rm["executor"] != "seat" || lens == "farm_manager" || asString(rm["holder"]) == "" {
			continue
		}
		s, err := loadSeat(ctx, tx, p.Tenant, asString(rm["seat"]), false)
		if err != nil {
			return nil, err
		}
		epoch, _ := toFloat(rm["seat_epoch"])
		if s == nil || s.Holder != asString(rm["holder"]) || s.Epoch != int(epoch) {
			continue
		}
		x := answerer{Seat: s.ID, Bot: s.Holder, Lens: lens}
		if rm["state"] != "done" {
			x.Miss, x.Against, x.For = true, true, true
		} else {
			res := mmapAny(rm["result"])
			score, _ := toFloat(res["score"])
			x.Veto = res["veto"] == true
			x.Against = x.Veto || score <= 1
			x.For = !x.Against
		}
		if !nobody {
			if a.Kind == "rejected_proposal" {
				x.Answers = x.Against && !x.Veto
			} else {
				x.Answers = x.For && area != "" && slices.Contains(c.lensAccountable(lens), area)
			}
		}
		out = append(out, x)
	}
	return out, nil
}

// linkConfirmed runs, in the transaction of a person's "Yes" on a v0.3 core, who answers for the
// link (GOV-41): the answering seats go into the record and the audit; with accountability on,
// for an incident opened and a confirmation after accountability_on_since, each answering holder
// waits for its departure, which the pace lets happen at once or later (GOV-42). Only proposals
// with a commission verdict after governance_v03_since count (GOV-36).
func (c *Core) linkConfirmed(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, a *attributionRow) error {
	if !claimsV03() {
		return nil
	}
	since, _, err := v03State(ctx, tx, a.Tenant)
	if err != nil || since == nil {
		return err
	}
	p, err := readProposal(ctx, tx, a.Proposal)
	if err != nil {
		return err
	}
	if parseTime(asString(mmapAny(p.Rec["verdict"])["computed_at"])).Before(*since) {
		return nil
	}
	list, err := c.answerers(ctx, tx, cat, a, p)
	if err != nil {
		return err
	}
	seats := []any{}
	var answering []answerer
	for _, x := range list {
		if x.Answers {
			n, _ := seatNumber(x.Seat)
			seats = append(seats, n)
			answering = append(answering, x)
		}
	}
	a.Rec["answerable"] = seats
	if a.Kind == "rejected_proposal" {
		a.Rec["manager_against"] = managerAgainst(p)
	}
	raw, err := json.Marshal(a.Rec)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE attributions SET record = $2 WHERE id = $1`, a.ID, raw); err != nil {
		return err
	}
	if err := c.govAudit(ctx, tx, now, a.Tenant, "governance.link.confirmed", "attribution", a.ID, "", map[string]any{"kind": a.Kind, "seats": seats, "manager_against": a.Rec["manager_against"] == true}); err != nil {
		return err
	}
	on, err := c.accountabilityOn(ctx, tx, cat, a.Tenant)
	if err != nil || on == nil || len(answering) == 0 {
		return err
	}
	var opened time.Time
	if err := tx.QueryRow(ctx, `SELECT opened_at FROM incidents WHERE id = $1`, a.Incident).Scan(&opened); err != nil {
		return err
	}
	if opened.Before(*on) {
		return nil
	}
	for _, x := range answering {
		if _, err := tx.Exec(ctx, `INSERT INTO link_departures (tenant, attribution, seat, bot, state, created_at, updated_at) VALUES ($1, $2, $3, $4, 'waiting', $5, $5)
			ON CONFLICT DO NOTHING`, a.Tenant, a.ID, x.Seat, x.Bot, now); err != nil {
			return err
		}
	}
	return c.departureStep(ctx, tx, now, cat, a.Tenant)
}

// departureStep lets the next waiting departure happen when the tenant had none in 30 days
// (GOV-42): a holder that no longer holds the seat is not looked for, the last holder of a veto
// lens never leaves this way, and the others wait in order and are shown to people.
func (c *Core) departureStep(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, tenant string) error {
	on, err := c.accountabilityOn(ctx, tx, cat, tenant)
	if err != nil {
		return err
	}
	if on == nil {
		// The mode left on: nobody leaves for a link any more, so the waiting departures end
		// (GOV-36, GOV-42), each with its audit record.
		if !claimsV03() {
			return nil
		}
		rows, err := tx.Query(ctx, `UPDATE link_departures SET state = 'canceled', reason = 'accountability_shadow', updated_at = $2
			WHERE tenant = $1 AND state = 'waiting' RETURNING bot, seat, attribution`, tenant, now)
		if err != nil {
			return err
		}
		type gone struct{ Bot, Seat, Attribution string }
		list, err := pgx.CollectRows(rows, pgx.RowToStructByPos[gone])
		if err != nil {
			return err
		}
		for _, g := range list {
			if err := c.govAudit(ctx, tx, now, tenant, "governance.seat.parked", "bot", g.Bot, "", map[string]any{
				"seat": g.Seat, "attribution": g.Attribution, "state": "canceled", "reason": "accountability_shadow"}); err != nil {
				return err
			}
		}
		return nil
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(7300108, hashtext($1))`, tenant); err != nil {
		return err
	}
	var recent bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM link_departures WHERE tenant = $1 AND done_at IS NOT NULL AND done_at > $2 AND state IN ('done', 'returned'))`,
		tenant, now.Add(-departurePace)).Scan(&recent); err != nil || recent {
		return err
	}
	// The link must still be confirmed: a disputed one waits, a canceled or rejected one drops.
	rows, err := tx.Query(ctx, `SELECT d.id, d.attribution, d.seat, d.bot, coalesce(a.state, 'canceled') FROM link_departures d LEFT JOIN attributions a ON a.id = d.attribution
		WHERE d.tenant = $1 AND d.state = 'waiting' ORDER BY d.created_at, d.id FOR UPDATE OF d`, tenant)
	if err != nil {
		return err
	}
	type dep struct {
		id                            int64
		attribution, seat, bot, state string
	}
	var deps []dep
	for rows.Next() {
		var d dep
		if err := rows.Scan(&d.id, &d.attribution, &d.seat, &d.bot, &d.state); err != nil {
			rows.Close()
			return err
		}
		deps = append(deps, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	end := func(d dep, state, reason string) error {
		_, err := tx.Exec(ctx, `UPDATE link_departures SET state = $2, reason = $3, done_at = CASE WHEN $2 = 'done' THEN $4 ELSE done_at END, updated_at = $4 WHERE id = $1`,
			d.id, state, nullable(reason), now)
		return err
	}
	defs := c.lensDefs()
	for _, d := range deps {
		switch d.state {
		case "confirmed":
		case "disputed":
			continue
		default:
			if err := end(d, "canceled", "link_canceled"); err != nil {
				return err
			}
			continue
		}
		s, err := loadSeat(ctx, tx, tenant, d.seat, true)
		if err != nil {
			return err
		}
		if s == nil || s.Holder != d.bot || s.State != "held" {
			if err := end(d, "canceled", "seat_changed"); err != nil {
				return err
			}
			continue
		}
		if defs[s.Lens].Veto {
			var others int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM seats WHERE tenant = $1 AND kind = 'commission' AND lens = $2 AND state = 'held' AND id <> $3`,
				tenant, s.Lens, s.ID).Scan(&others); err != nil {
				return err
			}
			if others == 0 {
				if err := end(d, "skipped", "last_veto_holder"); err != nil {
					return err
				}
				continue
			}
		}
		b, err := loadBot(ctx, tx, d.bot, true)
		if err != nil || b == nil {
			return err
		}
		if err := c.parkHolder(ctx, tx, now, cat, s, b, d.attribution); err != nil {
			return err
		}
		return end(d, "done", "")
	}
	return nil
}

// parkHolder takes a holder out of its seat for a confirmed link (GOV-42, IDN-16): the seat
// becomes vacant with the epoch + 1, a seat event and an audit record, and its running attempts
// end with seat_changed; the bot is not retired but parked (unbound, with since, role and seat in
// its record), keeps its memory and track record, offers its facts to the next holder of the
// seat for 30 days, leaves the promotion queue, and the operators get seat_parked_notice.
func (c *Core) parkHolder(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, s *seatRow, b *botRow, attribution string) error {
	if _, err := c.moveSeat(ctx, tx, now, s, "", "vacant", "seat.vacated.v1", "governance.link", "system"); err != nil {
		return err
	}
	parked, err := json.Marshal(map[string]any{"parked": map[string]any{"since": fmtTime(now), "role": b.Role, "seat": s.ID}})
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE bots SET mode = 'worker', seat = NULL, worker_id = NULL, state = 'unbound', unbound_at = $2, epoch = epoch + 1,
		record = record || $3::jsonb, updated_at = $2 WHERE id = $1`, b.ID, now, parked); err != nil {
		return err
	}
	if err := c.fenceAttempts(ctx, tx, now, "seat_changed", `acting_bot = $1`, b.ID); err != nil {
		return err
	}
	if err := c.offerFacts(ctx, tx, now, cat, b, "", s.ID, now.Add(parkedFactsFor)); err != nil {
		return err
	}
	// Its earlier queue entry leaves with parked (GOV-42).
	if err := c.removeEntry(ctx, tx, now, s.Tenant, b.ID, "parked"); err != nil {
		return err
	}
	if err := c.govAudit(ctx, tx, now, s.Tenant, "governance.seat.parked", "bot", b.ID, "", map[string]any{"seat": s.ID, "role": b.Role, "attribution": attribution}); err != nil {
		return err
	}
	if err := c.noticeAdmins(ctx, tx, now, cat, []string{s.Tenant}, "seat_parked_notice", "notice.seat_parked", map[string]any{
		"seat": s.ID, "bot": b.ID, "role": b.Role, "tenant": s.Tenant, "at": fmtTime(now)}, "admin:governance"); err != nil {
		return err
	}
	return c.seatCaseChanged(ctx, tx, now, s.Tenant, s.ID)
}

// linkCanceled returns the members who left for a link that is canceled because the incident
// was declared false (GOV-42): a departure still waiting is dropped; a parked bot whose seat is
// still vacant gets the operators seat_return_notice with the command to return it, otherwise it
// goes to the head of the promotion queue without the cooldown and without min_tenure, and the
// new holder stays.
func (c *Core) linkCanceled(ctx context.Context, tx pgx.Tx, now time.Time, a *attributionRow) error {
	if _, err := tx.Exec(ctx, `UPDATE link_departures SET state = 'canceled', reason = 'link_canceled', updated_at = $2 WHERE attribution = $1 AND state = 'waiting'`, a.ID, now); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `SELECT id, seat, bot FROM link_departures WHERE attribution = $1 AND state = 'done' ORDER BY id FOR UPDATE`, a.ID)
	if err != nil {
		return err
	}
	type dep struct {
		id        int64
		seat, bot string
	}
	var deps []dep
	for rows.Next() {
		var d dep
		if err := rows.Scan(&d.id, &d.seat, &d.bot); err != nil {
			rows.Close()
			return err
		}
		deps = append(deps, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(deps) == 0 {
		return err
	}
	cat, err := c.Catalog(ctx)
	if err != nil {
		return err
	}
	for _, d := range deps {
		s, err := loadSeat(ctx, tx, a.Tenant, d.seat, false)
		if err != nil {
			return err
		}
		how := "queue_head"
		if s != nil && s.State == "vacant" {
			how = "notice"
			if err := c.noticeAdmins(ctx, tx, now, cat, []string{a.Tenant}, "seat_return_notice", "notice.seat_return", map[string]any{
				"seat": d.seat, "bot": d.bot, "tenant": a.Tenant, "incident": a.Incident}, "admin:governance"); err != nil {
				return err
			}
		} else {
			var head *time.Time
			if err := tx.QueryRow(ctx, `SELECT min(trigger_at) FROM promotion_entries WHERE tenant = $1 AND state <> 'removed'`, a.Tenant).Scan(&head); err != nil {
				return err
			}
			at := now
			if head != nil && !head.After(now) {
				at = head.Add(-time.Second)
			}
			if _, err := tx.Exec(ctx, `INSERT INTO promotion_entries (tenant, bot, state, trigger_proposal, trigger_at, skip_tenure, updated_at) VALUES ($1, $2, 'queued', $3, $4, true, $5)
				ON CONFLICT (tenant, bot) DO UPDATE SET state = 'queued', trigger_at = $4, cooldown_until = NULL, skip_tenure = true, removed_reason = NULL, updated_at = $5
				WHERE promotion_entries.state <> 'in_case'`, a.Tenant, d.bot, a.Proposal, at, now); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE link_departures SET state = 'returned', reason = $2, updated_at = $3 WHERE id = $1`, d.id, how, now); err != nil {
			return err
		}
		if err := c.govAudit(ctx, tx, now, a.Tenant, "governance.seat.returned", "bot", d.bot, "", map[string]any{"seat": d.seat, "attribution": a.ID, "how": how}); err != nil {
			return err
		}
	}
	return nil
}

// unseatedRecently is the time a bot left a seat for a link within the last 180 days, or nil: it
// gets no trigger and no case then, and its tenure counts from the departure (GOV-42).
func unseatedRecently(ctx context.Context, tx pgx.Tx, now time.Time, bot string) (*time.Time, error) {
	var at *time.Time
	err := tx.QueryRow(ctx, `SELECT max(done_at) FROM link_departures WHERE bot = $1 AND state = 'done' AND done_at > $2`, bot, now.Add(-unseatCooldown)).Scan(&at)
	return at, err
}

// rejectionLinkCardFields draws rejection_link_card from the records (GOV-40), without model
// text: one card per incident with up to 3 candidates. The card shows the incident, where the
// alarm came from and whether code writes its series, and per candidate what the proposal would
// have changed, the rule by which code linked them, the author's share in the series, who would
// leave and whether each one's lens changed the verdict, and whether the farm manager was
// against it (GOV-41). Every open candidate has "Yes" and "No", and "Don't know" while the
// buttons fit the card (8): with three candidates "Don't know" is left to silence, which moves a
// candidate to unknown as well. One "Details" opens all candidates of the card (APR-14). a is
// the candidate that carries the card.
func (c *Core) rejectionLinkCardFields(ctx context.Context, tx pgx.Tx, cat *Catalog, a *attributionRow) (map[string]any, error) {
	var class, kind, subject, origin string
	var opened time.Time
	if err := tx.QueryRow(ctx, `SELECT class, subject_kind, subject_id, opened_at, coalesce(record->'alarm'->>'origin', '') FROM incidents WHERE id = $1`, a.Incident).
		Scan(&class, &kind, &subject, &opened, &origin); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	ic := classByName(cat, class)
	alarm := mstr(mmap(ic, "detect"), "alarm")
	group, err := linkGroup(ctx, tx, a)
	if err != nil {
		return nil, err
	}
	ctxt := map[string]any{"incident": a.Incident, "class": class, "subject": kind + ":" + subject, "opened_at": fmtTime(opened),
		"alarm": alarm, "alarm_origin": origin, "series_by_code": alarmSeriesName(alarm) != ""}
	open := []*attributionRow{}
	state := a.State
	for n, m := range group {
		p, err := readProposal(ctx, tx, m.Proposal)
		if err != nil {
			return nil, err
		}
		b := p.body()
		share, err := authorAlarmShare(ctx, tx, a.Tenant, kind+":"+subject, alarm, opened, asString(mmapAny(p.Rec["origin"])["worker"]), p.Role)
		if err != nil {
			return nil, err
		}
		var targets []string
		if mc := b.Change.ManifestChange; mc != nil {
			for _, op := range mc.Ops {
				targets = append(targets, op.Target.Kind+"/"+op.Target.Name+" "+op.Field)
			}
		}
		list, err := c.answerers(ctx, tx, cat, m, p)
		if err != nil {
			return nil, err
		}
		var seats []string
		for _, x := range list {
			seats = append(seats, fmt.Sprintf("%s %s against=%v veto=%v leaves=%v", x.Seat, x.Lens, x.Against, x.Veto, x.Answers))
		}
		line := fmt.Sprintf("%s: %s (%s) problem %s, %s, rule %s, author share %.2f; seats: %s; manager against=%v; state %s",
			m.Proposal, b.Change.Kind, strings.Join(targets, "; "), b.Problem.Code, asString(mmapAny(p.Rec["verdict"])["outcome"]), asString(m.Rec["rule"]),
			share, strings.Join(seats, ", "), managerAgainst(p), m.State)
		if ans := mmapAny(m.Rec["answer"]); ans != nil {
			line += ", answer " + asString(ans["decision"])
		}
		ctxt[fmt.Sprintf("candidate%d", n+1)] = cut(line, 600)
		if slices.Contains([]string{"candidate", "unknown", "rejected"}, m.State) && m.Nonce != "" {
			open = append(open, m)
			state = "candidate"
		}
	}
	card := map[string]any{"card": rejectionLinkCard, "subject_kind": "attribution", "id": a.ID, "nonce": a.Nonce,
		"title": cut(fmt.Sprintf("Could a rejected proposal have prevented the incident? %s on %s:%s", class, kind, subject), 200), "context": ctxt,
		"requested_by": map[string]any{"initiator": "system", "source": "proposal"}, "initiator_display": "the governance loop", "state": state,
		"footer": map[string]any{"why_you": "governance.attribute", "on_silence": "nothing changes; nobody leaves"}}
	if a.PresentedAt != nil {
		until := a.PresentedAt.Add(attributionTTL)
		card["expires_at"] = fmtTime(until)
		card["footer"].(map[string]any)["until"] = fmtTime(until)
	}
	buttons := []any{}
	codes := []string{"yes", "no", "unknown"}
	if len(open) > 2 {
		codes = codes[:2]
	}
	for _, m := range open {
		for _, code := range codes {
			buttons = append(buttons, map[string]any{"code": code, "subject_kind": "attribution", "id": m.ID, "nonce": m.Nonce})
		}
	}
	if len(open) > 0 && a.Nonce != "" {
		buttons = append(buttons, map[string]any{"code": "details", "subject_kind": "attribution", "id": a.ID, "nonce": a.Nonce})
	}
	card["buttons"] = buttons
	return card, nil
}

// linkGroup are the candidates to rejected proposals that share the card of a's incident (GOV-40),
// in the order they were found: at most 3 per incident (GOV-38).
func linkGroup(ctx context.Context, tx pgx.Tx, a *attributionRow) ([]*attributionRow, error) {
	rows, err := tx.Query(ctx, `SELECT id FROM attributions WHERE card_of = $1 ORDER BY created_at, id LIMIT $2`, a.cardSubject(), linksPerIncident)
	if err != nil {
		return nil, err
	}
	list, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	var out []*attributionRow
	for _, id := range list {
		m, err := loadAttribution(ctx, tx, id, false)
		if err != nil {
			return nil, err
		}
		if m != nil {
			out = append(out, m)
		}
	}
	if len(out) == 0 {
		out = []*attributionRow{a}
	}
	return out, nil
}
