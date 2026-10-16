// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// campaignFullList answers the "full list" button of a campaign approval card (ORC-2): the
// card shows the first 5 items, and a press by a person the card was delivered to, from that
// chat with the card's nonce, plans one campaign_list delivery from the record to that
// person's direct chat. It decides nothing and changes no state; a repeat while that person's
// list is still on its way or delivered plans no second one.
func (c *Core) campaignFullList(ctx context.Context, w *Worker, ob OnBehalf, req DecideRequest) (*DecideResult, error) {
	cat, err := c.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	var out *DecideResult
	err = c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		cmp, err := loadCampaign(ctx, tx, w.Tenant, req.ID, false)
		if errors.Is(err, pgx.ErrNoRows) {
			return refuse("not_found", "no such campaign")
		}
		if err != nil {
			return err
		}
		if cmp.Nonce != req.Nonce {
			return refuse("forbidden", "the nonce is not the card's")
		}
		press, err := c.checkPress(ctx, tx, now, cat, w, ob, "campaign", cmp.ID, []string{campaignApprovalCard})
		if err != nil {
			return err
		}
		out = &DecideResult{SubjectKind: "campaign", ID: cmp.ID, Decision: req.Decision, State: cmp.State}
		n, err := campaignCount(ctx, tx, cmp.ID)
		if err != nil {
			return err
		}
		if n.total() <= campaignItemFirstOnCard {
			return refuse("input_schema_invalid", "the card shows every item")
		}
		var sent bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM deliveries WHERE subject_kind = 'campaign' AND subject_id = $1 AND kind = $2
			AND recipient = $3 AND state IN ('planned', 'sending', 'delivered'))`, cmp.ID, campaignListKind, press.principal).Scan(&sent); err != nil {
			return err
		}
		if sent {
			return nil
		}
		return c.planCampaignCard(ctx, tx, now, press.tv, cmp, campaignListKind, campaignApprovalCard, press.principal)
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
