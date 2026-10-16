// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// The cards of the governance subjects (GOV-9, GOV-13): promotion_card on a case and
// attribution_card on an attribution candidate go by direct message to the people with the
// right of the decision point; the channel draws them from the code record when it hands them
// out, so a card always shows the current state.

const (
	promotionCard   = "promotion_card"
	attributionCard = "attribution_card"
	// rejectionLinkCard asks a person whether a rejected proposal could have prevented an
	// incident (GOV-40).
	rejectionLinkCard = "rejection_link_card"
)

// attributionCards are the card kinds of the attribution subject.
var attributionCards = []string{attributionCard, rejectionLinkCard, scriptLinkCard}

// withinCeiling keeps the people whose data ceiling covers mark (DIG-2).
func (c *Core) withinCeiling(ctx context.Context, tx pgx.Tx, now time.Time, tv *TenantView, people []string, mark string) ([]string, error) {
	var out []string
	for _, p := range people {
		info, err := loadPrincipal(ctx, tx, now, tv.Name, p)
		if err != nil {
			return nil, err
		}
		if c.digestShow(mark, c.personMark(tv, groupClosure(tv, info.Groups))) == "full" {
			out = append(out, p)
		}
	}
	return out, nil
}

// planGovCards plans a card of a governance subject to the direct chat of every person with
// cap (and, when mark is set, whose data ceiling covers it); it returns how many were planned.
func (c *Core) planGovCards(ctx context.Context, tx pgx.Tx, now time.Time, tv *TenantView, subjectKind, id, kind, cap, mark string, extra map[string]any) (int, error) {
	people, err := peopleWithCap(ctx, tx, now, tv, cap)
	if err != nil {
		return 0, err
	}
	if mark != "" {
		if people, err = c.withinCeiling(ctx, tx, now, tv, people, mark); err != nil {
			return 0, err
		}
	}
	return c.planGovCardsTo(ctx, tx, now, tv, subjectKind, id, kind, people, extra)
}

// planGovCardsTo plans a card of a governance subject to the direct chat of each of people.
func (c *Core) planGovCardsTo(ctx context.Context, tx pgx.Tx, now time.Time, tv *TenantView, subjectKind, id, kind string, people []string, extra map[string]any) (int, error) {
	n := 0
	for _, person := range people {
		channel, chat, ok, err := cardChat(ctx, tx, tv, person)
		if err != nil {
			return 0, err
		}
		if !ok {
			continue
		}
		body := map[string]any{"form": "card", "card": kind}
		for k, v := range extra {
			body[k] = v
		}
		if _, err := c.planDelivery(ctx, tx, now, plannedDelivery{
			Tenant: tv.Name, Kind: kind, Addr: "governance", TargetKind: "chat", TargetID: chat.ID(),
			ExecKind: "channel", ExecName: channel, Recipient: person, Body: body, DataMark: "internal", DataOrigin: "code",
			SubjectKind: subjectKind, SubjectID: id,
		}); err != nil {
			return 0, err
		}
		n++
	}
	return n, nil
}

// govCardUpdates plans an update of every delivered card of a governance subject.
func (c *Core) govCardUpdates(ctx context.Context, tx pgx.Tx, now time.Time, tenant, subjectKind, id string) error {
	rows, err := tx.Query(ctx, `SELECT id, kind, exec_name, target_id, coalesce(recipient, '') FROM deliveries
		WHERE subject_kind = $1 AND subject_id = $2 AND kind <> $3 AND state IN ('planned', 'sending', 'delivered') ORDER BY seq`, subjectKind, id, cardUpdate)
	if err != nil {
		return err
	}
	type target struct{ id, kind, channel, chat, recipient string }
	var ts []target
	for rows.Next() {
		var t target
		if err := rows.Scan(&t.id, &t.kind, &t.channel, &t.chat, &t.recipient); err != nil {
			rows.Close()
			return err
		}
		ts = append(ts, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, t := range ts {
		if _, err := c.planDelivery(ctx, tx, now, plannedDelivery{
			Tenant: tenant, Kind: cardUpdate, Addr: subjectKind, TargetKind: "chat", TargetID: t.chat,
			ExecKind: "channel", ExecName: t.channel, Recipient: t.recipient, Body: map[string]any{"form": "card", "card": t.kind, "of": t.id},
			DataMark: "internal", DataOrigin: "code", SubjectKind: subjectKind, SubjectID: id,
		}); err != nil {
			return err
		}
	}
	return nil
}

// govCardBody draws a governance card or its update at hand-out.
func (c *Core) govCardBody(ctx context.Context, tx pgx.Tx, cat *Catalog, sk, kind, id, target, recipient, channel string, body map[string]any) (map[string]any, string, error) {
	cardKind, _ := body["card"].(string)
	reminder, _ := body["reminder"].(bool)
	var card map[string]any
	var err error
	switch sk {
	case "governance_case":
		card, err = c.caseCard(ctx, tx, cat, id)
	default:
		card, err = c.attributionCardFields(ctx, tx, cat, id)
	}
	if err != nil || card == nil {
		return nil, "", err
	}
	if reminder {
		card["reminder"] = true
	}
	replaces := ""
	if kind == cardUpdate {
		of, _ := body["of"].(string)
		var ext *string
		err := tx.QueryRow(ctx, `SELECT external_id FROM deliveries WHERE id = $6 AND subject_kind = $1 AND subject_id = $2 AND kind = $3
			AND exec_name = $4 AND target_id = $5 AND state = 'delivered' AND external_id IS NOT NULL`,
			sk, id, cardKind, channel, target, of).Scan(&ext)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, "", err
		}
		if ext == nil {
			return nil, "", nil
		}
		replaces = *ext
	}
	return map[string]any{"form": "card", "card": card}, replaces, nil
}

// govDetails is P1 card.details on a promotion or attribution card: the closed fields of the
// card to the same chat, and the opening is recorded for APR-14 and details_opened (APR-11).
func (c *Core) govDetails(ctx context.Context, w *Worker, ob OnBehalf, req CardOp) (map[string]any, error) {
	cat, err := c.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	err = c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		table, kinds := "governance_cases", []string{promotionCard}
		if req.SubjectKind == "attribution" {
			table, kinds = "attributions", attributionCards
		}
		var tenant string
		var nonce *string
		if err := tx.QueryRow(ctx, `SELECT tenant, nonce FROM `+table+` WHERE id = $1`, req.ID).Scan(&tenant, &nonce); errors.Is(err, pgx.ErrNoRows) || err == nil && tenant != w.Tenant {
			return refuse("not_found", "no such subject")
		} else if err != nil {
			return err
		}
		if deref(nonce) == "" || deref(nonce) != req.Nonce {
			return refuse("forbidden", "the nonce is not the card's")
		}
		press, err := c.checkPress(ctx, tx, now, cat, w, ob, req.SubjectKind, req.ID, kinds)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO card_views (subject_kind, subject_id, principal, opened_at) VALUES ($1, $2, $3, $4)
			ON CONFLICT (subject_kind, subject_id, principal) DO UPDATE SET opened_at = excluded.opened_at`, req.SubjectKind, req.ID, press.principal, now); err != nil {
			return err
		}
		var card map[string]any
		if req.SubjectKind == "attribution" {
			card, err = c.attributionCardFields(ctx, tx, cat, req.ID)
		} else {
			card, err = c.caseCard(ctx, tx, cat, req.ID)
		}
		if err != nil {
			return err
		}
		fields := mmapAny(card["context"])
		fields["title"], fields["state"] = card["title"], card["state"]
		out = map[string]any{"template": "card.details." + req.SubjectKind, "fields": fields, "deliver_to": "same_chat"}
		return nil
	})
	return out, err
}
