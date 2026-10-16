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
	"unicode"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/ids"
)

// The incident attribution, minimal (GOV-13, GOV-30): the governance loop creates a candidate
// by the rule direct of attribution-rules.yaml (an incident of a class with change_sensitive
// whose subject is a target of an executed change, opened within the attribution window), a
// person with governance.attribute answers "Yes", "No" or "Don't know" on attribution_card,
// silence leaves it unknown, and undecided candidates get a weekly reminder at most 3 times.
// A dismissed incident cancels the candidates without "Yes" and disputes a "Yes".

const (
	defaultAttributionWindow = 14 * 24 * time.Hour
	attributionTTL           = 7 * 24 * time.Hour
	attributionRemindEvery   = 7 * 24 * time.Hour
	attributionRemindMax     = 3
	// disputeTTL is the silence after which a disputed "Yes" is withdrawn (D-79).
	disputeTTL = 7 * 24 * time.Hour
	// attributionLookback bounds the incidents the loop looks at.
	attributionLookback = 180 * 24 * time.Hour
)

// attributionOpen are the states of a candidate that block a promotion trigger (GOV-30).
var attributionOpen = []string{"candidate", "unknown", "disputed"}

// snakeKind is a manifest kind in the form of an incident subject kind: TaskType is task_type.
func snakeKind(kind string) string {
	var b strings.Builder
	for i, r := range kind {
		if unicode.IsUpper(r) {
			if i > 0 {
				b.WriteByte('_')
			}
			r = unicode.ToLower(r)
		}
		b.WriteRune(r)
	}
	return b.String()
}

// changeTouches says whether a change names the incident subject as a target directly (the
// rule direct). The rule dependency_1 needs the catalog dependencies and is not built.
func changeTouches(ch proposalChange, subjectKind, subjectID string) bool {
	if ch.ManifestChange == nil {
		return false
	}
	for _, op := range ch.ManifestChange.Ops {
		if snakeKind(op.Target.Kind) == subjectKind && op.Target.Name == subjectID {
			return true
		}
	}
	return false
}

// attributionWindowOpen says whether the attribution window of an executed change is open:
// attribution.window from realized, and on while the change can still give a promotion trigger
// or gave the trigger of a live queue entry (GOV-13).
func attributionWindowOpen(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, tenant, proposal string, realized time.Time) (bool, error) {
	if now.Before(realized.Add(charterDuration(cat, tenant, "attribution", "window", defaultAttributionWindow))) {
		return true, nil
	}
	var tracked, live bool
	var state, bot string
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM promotion_track WHERE proposal = $1),
		EXISTS (SELECT 1 FROM promotion_entries WHERE trigger_proposal = $1 AND state <> 'removed'),
		coalesce((SELECT state FROM proposals WHERE id = $1), ''), coalesce((SELECT record->'origin'->>'acting_bot' FROM proposals WHERE id = $1), '')`,
		proposal).Scan(&tracked, &live, &state, &bot); err != nil {
		return false, err
	}
	if live {
		return true, nil
	}
	if tracked || state != "realized" || bot == "" {
		return false, nil
	}
	// Only a change that can still give a trigger keeps the window open: a realized one of a
	// functionary with a model.
	var role string
	if err := tx.QueryRow(ctx, `SELECT role FROM bots WHERE id = $1`, bot).Scan(&role); errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	return slices.Contains(functionaryProfiles, profileOf(cat, role)) && hasModel(cat, role), nil
}

// attributionStep is the attribution part of the governance loop of a tenant.
func (c *Core) attributionStep(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, tv *TenantView) error {
	if err := c.createAttributions(ctx, tx, now, cat, tv); err != nil {
		return err
	}
	if c.claimsV03() {
		if err := c.createRejectionLinks(ctx, tx, now, cat, tv); err != nil {
			return err
		}
	}
	if err := c.dismissedAttributions(ctx, tx, now, tv.Name, ""); err != nil {
		return err
	}
	return c.remindAttributions(ctx, tx, now, tv)
}

// createAttributions creates the candidates of the rule direct for the incidents of the tenant
// of a change_sensitive class (GOV-13).
func (c *Core) createAttributions(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, tv *TenantView) error {
	rows, err := tx.Query(ctx, `SELECT id, class, subject_kind, subject_id, opened_at FROM incidents
		WHERE tenant = $1 AND state <> 'dismissed' AND opened_at >= $2 ORDER BY opened_at, id`, tv.Name, now.Add(-attributionLookback))
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
		if classByName(cat, i.class)["change_sensitive"] == true {
			incs = append(incs, i)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(incs) == 0 {
		return err
	}
	prows, err := tx.Query(ctx, `SELECT id FROM proposals WHERE tenant = $1 AND state IN ('realized', 'reverted')
		AND record->'realization'->>'realized_at' IS NOT NULL ORDER BY id`, tv.Name)
	if err != nil {
		return err
	}
	pids, err := pgx.CollectRows(prows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, pid := range pids {
		p, err := readProposal(ctx, tx, pid)
		if err != nil {
			return err
		}
		realized := parseTime(asString(mmapAny(p.Rec["realization"])["realized_at"]))
		ch := p.body().Change
		for _, i := range incs {
			if i.at.Before(realized) || !changeTouches(ch, i.kind, i.subject) {
				continue
			}
			open, err := attributionWindowOpen(ctx, tx, i.at, cat, tv.Name, p.ID, realized)
			if err != nil {
				return err
			}
			if !open {
				continue
			}
			if err := c.createAttribution(ctx, tx, now, tv, i.id, p.ID, "change", "direct", "system"); err != nil {
				return err
			}
		}
	}
	return nil
}

// createAttribution writes a candidate once per incident and proposal and sends its card: the
// attribution card for a change, and from v0.3 the link card for a rejected proposal only with
// commission.accountability on (GOV-36, GOV-40). A v0.3 core writes attribution.v2.
func (c *Core) createAttribution(ctx context.Context, tx pgx.Tx, now time.Time, tv *TenantView, incident, proposal, kind, rule, by string) error {
	id := ids.New(ids.Attribution)
	rec := map[string]any{"id": id, "tenant": tv.Name, "incident": incident, "proposal": proposal, "rule": rule, "created_by": by,
		"created_at": fmtTime(now), "state": "candidate", "answer": nil, "reminders": 0, "cancel_reason": nil}
	schema := "urn:botaru:schema:attribution:1"
	if c.claimsV03() {
		rec["kind"], schema = kind, "urn:botaru:schema:attribution:2"
	}
	if err := c.spec.Validate(schema, rec); err != nil {
		return fmt.Errorf("attribution record: %w", err)
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `INSERT INTO attributions (id, tenant, incident, proposal, state, record, nonce, presented_at, created_at, updated_at, kind)
		VALUES ($1, $2, $3, $4, 'candidate', $5, $6, $7, $7, $7, $8) ON CONFLICT (incident, proposal) DO NOTHING`, id, tv.Name, incident, proposal, raw, ids.Secret(16), now, kind)
	if err != nil || tag.RowsAffected() == 0 {
		return err
	}
	if _, err := c.appendAudit(ctx, tx, now, Audit{
		Tenant: tv.Name, Kind: "command", Actor: Actor{Principal: "system", Via: "core"}, Command: "attribution.create",
		Target: &Target{Kind: "attribution", ID: id}, OK: true, After: map[string]any{"incident": incident, "proposal": proposal, "rule": rule, "state": "candidate", "kind": kind},
	}); err != nil {
		return err
	}
	card, err := c.attributionCardKind(ctx, tx, tv, kind)
	if err != nil || card == "" {
		return err
	}
	_, err = c.planGovCards(ctx, tx, now, tv, "attribution", id, card, "governance.attribute", proposalMark(ctx, tx, proposal), nil)
	return err
}

// attributionCardKind is the card of a candidate of a kind, or "" when none goes: a link to a
// rejected proposal gets its card only with commission.accountability on; in shadow code finds
// it for the statistics and sends nothing (GOV-36, GOV-40).
func (c *Core) attributionCardKind(ctx context.Context, tx pgx.Tx, tv *TenantView, kind string) (string, error) {
	if kind != "rejected_proposal" {
		return attributionCard, nil
	}
	cat, err := c.Catalog(ctx)
	if err != nil {
		return "", err
	}
	on, err := c.accountabilityOn(ctx, tx, cat, tv.Name)
	if err != nil || on == nil {
		return "", err
	}
	return rejectionLinkCard, nil
}

// attributionRow is an attribution candidate under its lock.
type attributionRow struct {
	ID, Tenant, Incident, Proposal, State, Nonce, Kind string
	Rec                                                map[string]any
	PresentedAt, RemindedAt                            *time.Time
}

func loadAttribution(ctx context.Context, tx pgx.Tx, id string, lock bool) (*attributionRow, error) {
	q := `SELECT id, tenant, incident, proposal, state, record, coalesce(nonce, ''), presented_at, reminded_at, kind FROM attributions WHERE id = $1`
	if lock {
		q += ` FOR UPDATE`
	}
	a := &attributionRow{}
	var raw []byte
	err := tx.QueryRow(ctx, q, id).Scan(&a.ID, &a.Tenant, &a.Incident, &a.Proposal, &a.State, &raw, &a.Nonce, &a.PresentedAt, &a.RemindedAt, &a.Kind)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return a, json.Unmarshal(raw, &a.Rec)
}

// moveAttribution writes the new state of a candidate with its audit record.
func (c *Core) moveAttribution(ctx context.Context, tx pgx.Tx, now time.Time, a *attributionRow, state, actor, command string) error {
	before := a.State
	a.State, a.Rec["state"] = state, state
	raw, err := json.Marshal(a.Rec)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE attributions SET state = $2, record = $3, updated_at = $4 WHERE id = $1`, a.ID, state, raw, now); err != nil {
		return err
	}
	after := map[string]any{"state": state}
	if ans := mmapAny(a.Rec["answer"]); ans != nil {
		after["decision"], after["response_ms"], after["details_opened"] = ans["decision"], ans["response_ms"], ans["details_opened"]
	}
	via := "core"
	if strings.HasPrefix(actor, "human:") {
		via = "channel"
	}
	if _, err := c.appendAudit(ctx, tx, now, Audit{
		Tenant: a.Tenant, Kind: "decision", Actor: Actor{Principal: principalOr(actor), Via: via}, Command: command,
		Target: &Target{Kind: "attribution", ID: a.ID}, OK: true, Before: map[string]any{"state": before}, After: after,
	}); err != nil {
		return err
	}
	return c.govCardUpdates(ctx, tx, now, a.Tenant, "attribution", a.ID)
}

// dismissedAttributions: an incident declared false cancels its candidates without "Yes" and
// disputes a "Yes"; a dispute nobody answers is withdrawn after 7 days (GOV-13, D-79). The
// dispute card to the author of "Yes" is not built, so a dispute ends by that silence. The
// incident dismissal calls it for its incident in the same transaction; the governance loop
// ends the disputes. A canceled candidate gives the trigger back (GOV-14).
func (c *Core) dismissedAttributions(ctx context.Context, tx pgx.Tx, now time.Time, tenant, incident string) error {
	rows, err := tx.Query(ctx, `SELECT a.id FROM attributions a JOIN incidents i ON i.id = a.incident
		WHERE a.tenant = $1 AND ($3 = '' OR a.incident = $3) AND (i.state = 'dismissed' AND a.state IN ('candidate', 'unknown', 'rejected', 'confirmed')
		OR a.state = 'disputed' AND (a.record->>'disputed_at')::timestamptz <= $2) ORDER BY a.id FOR UPDATE OF a`, tenant, now.Add(-disputeTTL), incident)
	if err != nil {
		return err
	}
	list, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, id := range list {
		a, err := loadAttribution(ctx, tx, id, true)
		if err != nil || a == nil {
			return err
		}
		state := "canceled"
		switch a.State {
		case "confirmed":
			state = "disputed"
			a.Rec["disputed_at"] = fmtTime(now)
		case "disputed":
			delete(a.Rec, "disputed_at")
		}
		if state == "canceled" {
			a.Rec["cancel_reason"] = "incident_dismissed"
			delete(a.Rec, "disputed_at")
		}
		if err := c.moveAttribution(ctx, tx, now, a, state, "system", "attribution."+state); err != nil {
			return err
		}
		if state == "canceled" {
			if err := attributionCanceled(ctx, tx, a); err != nil {
				return err
			}
			// A link canceled this way returns the members who left for it (GOV-42).
			if err := c.linkCanceled(ctx, tx, now, a); err != nil {
				return err
			}
		}
	}
	return nil
}

// attributionCanceled: when no confirmed attribution is left on the change, a track row that
// lost its trigger to an attribution is dropped, so the governance loop evaluates the change
// again under the common conditions; the trigger time is then that pass, not earlier than the
// cancellation, and a removed entry comes back (GOV-14). A change that is no longer realized
// keeps its row, with the reason attribution_canceled, since it cannot be evaluated again.
func attributionCanceled(ctx context.Context, tx pgx.Tx, a *attributionRow) error {
	const lost = `proposal = $1 AND NOT trigger AND reason = 'attribution'
		AND NOT EXISTS (SELECT 1 FROM attributions WHERE proposal = $1 AND state = 'confirmed')`
	if _, err := tx.Exec(ctx, `DELETE FROM promotion_track WHERE `+lost+`
		AND EXISTS (SELECT 1 FROM proposals WHERE id = $1 AND state = 'realized')`, a.Proposal); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE promotion_track SET reason = 'attribution_canceled' WHERE `+lost, a.Proposal)
	return err
}

// remindAttributions moves an unanswered candidate to unknown at the card expiry and reminds
// the people once a week about the undecided ones, at most 3 times each (GOV-30).
func (c *Core) remindAttributions(ctx context.Context, tx pgx.Tx, now time.Time, tv *TenantView) error {
	rows, err := tx.Query(ctx, `SELECT id FROM attributions WHERE tenant = $1 AND state IN ('candidate', 'unknown') ORDER BY id FOR UPDATE`, tv.Name)
	if err != nil {
		return err
	}
	list, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, id := range list {
		a, err := loadAttribution(ctx, tx, id, true)
		if err != nil || a == nil {
			return err
		}
		if a.State == "candidate" && a.PresentedAt != nil && now.Sub(*a.PresentedAt) > attributionTTL {
			// Silence on the card: undecided, it does not expire (GOV-30).
			if err := c.moveAttribution(ctx, tx, now, a, "unknown", "system", "attribution.unknown"); err != nil {
				return err
			}
		}
		n, _ := toFloat(a.Rec["reminders"])
		last := a.PresentedAt
		if a.RemindedAt != nil {
			last = a.RemindedAt
		}
		if int(n) >= attributionRemindMax || last == nil || now.Sub(*last) < attributionRemindEvery {
			continue
		}
		card, err := c.attributionCardKind(ctx, tx, tv, a.Kind)
		if err != nil {
			return err
		}
		if card == "" {
			continue
		}
		a.Rec["reminders"] = int(n) + 1
		raw, err := json.Marshal(a.Rec)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE attributions SET record = $2, reminded_at = $3, updated_at = $3 WHERE id = $1`, a.ID, raw, now); err != nil {
			return err
		}
		if _, err := c.planGovCards(ctx, tx, now, tv, "attribution", a.ID, card, "governance.attribute", proposalMark(ctx, tx, a.Proposal), map[string]any{"reminder": true}); err != nil {
			return err
		}
	}
	return nil
}

// attributionCardFields draws attribution_card from the records: the incident, the change and
// the buttons "Yes", "No" and "Don't know" while the answer may still change (GOV-13).
func (c *Core) attributionCardFields(ctx context.Context, tx pgx.Tx, cat *Catalog, id string) (map[string]any, error) {
	a, err := loadAttribution(ctx, tx, id, false)
	if err != nil || a == nil {
		return nil, err
	}
	if a.Kind == "rejected_proposal" {
		return c.rejectionLinkCardFields(ctx, tx, cat, a)
	}
	var class, kind, subject string
	var opened time.Time
	if err := tx.QueryRow(ctx, `SELECT class, subject_kind, subject_id, opened_at FROM incidents WHERE id = $1`, a.Incident).Scan(&class, &kind, &subject, &opened); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	p, err := readProposal(ctx, tx, a.Proposal)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	ctxt := map[string]any{"incident": a.Incident, "class": class, "subject": kind + ":" + subject, "opened_at": fmtTime(opened), "proposal": a.Proposal, "rule": a.Rec["rule"]}
	if p != nil {
		b := p.body()
		ctxt["change"] = b.Change.Kind
		ctxt["realized_at"] = mmapAny(p.Rec["realization"])["realized_at"]
		ctxt["role"] = p.Role
	}
	if ans := mmapAny(a.Rec["answer"]); ans != nil {
		ctxt["answer"] = ans["decision"]
	}
	until := time.Time{}
	if a.PresentedAt != nil {
		until = a.PresentedAt.Add(attributionTTL)
	}
	card := map[string]any{"card": attributionCard, "subject_kind": "attribution", "id": a.ID, "nonce": a.Nonce,
		"title": cut(fmt.Sprintf("Did the change cause the incident? %s on %s:%s", class, kind, subject), 200), "context": ctxt,
		"requested_by": map[string]any{"initiator": "system", "source": "proposal"}, "initiator_display": "the governance loop", "state": a.State,
		"footer": map[string]any{"why_you": "governance.attribute", "on_silence": "not decided; no promotion trigger appears"}}
	if !until.IsZero() {
		card["expires_at"] = fmtTime(until)
		card["footer"].(map[string]any)["until"] = fmtTime(until)
	}
	buttons := []any{}
	if slices.Contains([]string{"candidate", "unknown", "rejected"}, a.State) && a.Nonce != "" {
		for _, code := range []string{"yes", "no", "unknown", "details"} {
			buttons = append(buttons, map[string]any{"code": code, "subject_kind": "attribution", "id": a.ID, "nonce": a.Nonce})
		}
	}
	card["buttons"] = buttons
	return card, nil
}

// decideAttribution is P1 decide on an attribution candidate (GOV-13): only a person with
// governance.attribute; "Yes" and "No" only after "Details" of this card (APR-14); "Yes" is
// final, "No" and "Don't know" change while the attribution window is open. A confirmed
// attribution removes the promotion trigger of its change (GOV-14).
func (c *Core) decideAttribution(ctx context.Context, w *Worker, ob OnBehalf, req DecideRequest) (*DecideResult, error) {
	if req.Decision != "yes" && req.Decision != "no" && req.Decision != "unknown" {
		return nil, refuse("input_schema_invalid", "an attribution is answered yes, no or unknown")
	}
	cat, err := c.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	out := &DecideResult{SubjectKind: "attribution", ID: req.ID, Decision: req.Decision}
	err = c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		a, err := loadAttribution(ctx, tx, req.ID, true)
		if err != nil {
			return err
		}
		if a == nil || a.Tenant != w.Tenant {
			return refuse("not_found", "no such attribution")
		}
		if a.Nonce == "" || a.Nonce != req.Nonce {
			return refuse("forbidden", "the nonce is not the card's")
		}
		press, err := c.checkPress(ctx, tx, now, cat, w, ob, "attribution", a.ID, attributionCards)
		if err != nil {
			return err
		}
		deny := func(reason, detail string) error {
			if _, err := c.appendAudit(ctx, tx, now, Audit{
				Tenant: a.Tenant, Kind: "decision", Actor: Actor{Principal: press.principal, Via: "channel"},
				Command: "decide", Target: &Target{Kind: "attribution", ID: a.ID}, OK: false, Reason: reason,
				After: map[string]any{"decision": req.Decision},
			}); err != nil {
				return err
			}
			return &committed{p: refuse(reason, "%s", detail)}
		}
		ans := mmapAny(a.Rec["answer"])
		if ans["by"] == press.principal && ans["decision"] == req.Decision {
			out.State = a.State
			return nil
		}
		if !hasCap(press.tv, press.groups, "governance.attribute") {
			return deny("forbidden", "the person has no governance.attribute")
		}
		switch a.State {
		case "candidate", "unknown", "rejected":
		case "canceled":
			return refuse("expired", "the attribution is canceled")
		default:
			return refuse("already_decided", "the attribution is decided")
		}
		if a.State != "candidate" {
			open := false
			if a.Kind == "rejected_proposal" {
				// A link to a rejected proposal has no realization: its answer may change for the
				// attribution window from its creation.
				open = now.Before(parseTime(asString(a.Rec["created_at"])).Add(charterDuration(cat, a.Tenant, "attribution", "window", defaultAttributionWindow)))
			} else {
				p, err := readProposal(ctx, tx, a.Proposal)
				if err != nil {
					return err
				}
				if open, err = attributionWindowOpen(ctx, tx, now, cat, a.Tenant, a.Proposal, parseTime(asString(mmapAny(p.Rec["realization"])["realized_at"]))); err != nil {
					return err
				}
			}
			if !open {
				return refuse("already_decided", "the attribution window is closed; the answer is final")
			}
		}
		opened := false
		if a.PresentedAt != nil {
			// APR-14: "Yes" and "No" only after "Details" of this card.
			if opened, err = detailsOpened(ctx, tx, "attribution", a.ID, press.principal, *a.PresentedAt); err != nil {
				return err
			}
		}
		if req.Decision != "unknown" && !opened {
			return deny("details_not_opened", `open "Details" first`)
		}
		state := map[string]string{"yes": "confirmed", "no": "rejected", "unknown": "unknown"}[req.Decision]
		a.Rec["answer"] = map[string]any{"decision": req.Decision, "by": press.principal, "at": fmtTime(now),
			"response_ms": max(now.Sub(press.deliveredAt).Milliseconds(), 0), "details_opened": opened}
		if err := c.moveAttribution(ctx, tx, now, a, state, press.principal, "decide"); err != nil {
			return err
		}
		if state == "confirmed" {
			if a.Kind == "change" {
				if err := c.attributionConfirmed(ctx, tx, now, a); err != nil {
					return err
				}
			}
			// From v0.3 code finds who answers for the link, and with accountability on they
			// leave in this transaction or wait for the pace (GOV-41, GOV-42).
			if err := c.linkConfirmed(ctx, tx, now, cat, a); err != nil {
				return err
			}
		}
		out.State = state
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// proposalMark is the data mark of a proposal, for the ceiling of the attribution card (DIG-2);
// "restricted" when it cannot be read, so the card goes only to the people who see everything.
func proposalMark(ctx context.Context, tx pgx.Tx, proposal string) string {
	var mark string
	if err := tx.QueryRow(ctx, `SELECT coalesce(record->>'data_mark', '') FROM proposals WHERE id = $1`, proposal).Scan(&mark); err != nil || mark == "" {
		return "restricted"
	}
	return mark
}
