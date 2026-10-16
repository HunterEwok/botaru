// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// P1CampaignPropose is P1 campaign.propose: a live attempt of a role that proposes campaigns
// proposes one with its lease token, as P3 propose_campaign does (ORC-1); the idempotency_key
// of the request names the proposal within the attempt's task, so a repeat with the same key
// and plan is the same campaign and another plan is idempotency_conflict. The v0.2 rollout
// fields waves and rate_per_min (ORC-15) are deferred and refused.
func (c *Core) P1CampaignPropose(ctx context.Context, w *Worker, header string, args map[string]any) (map[string]any, error) {
	if r, ok := args["rollout"].(map[string]any); ok {
		for _, f := range []string{"waves", "rate_per_min"} {
			if _, has := r[f]; has {
				return nil, refuse("version_unsupported", "rollout.%s (ORC-15) is not served by this version", f)
			}
		}
	}
	key, _ := args["idempotency_key"].(string)
	body := map[string]any{}
	for k, v := range args {
		if k != "idempotency_key" {
			body[k] = v
		}
	}
	cat, err := c.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	var out campaignOutcome
	err = c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		l, _, err := c.heldLease(ctx, tx, w, header, "campaign.propose", nil)
		if err != nil {
			return err
		}
		if l.Kind != "attempt" || l.State != "active" || !l.Until.After(now) {
			return refuse("fenced", "a campaign is proposed under a live attempt lease")
		}
		g := &toolsGrant{Tenant: l.Tenant, Task: l.WorkID, Worker: w.ID, Role: w.Role, LeaseNo: l.LeaseNo}
		if err := c.loadGrantTask(ctx, tx, g); err != nil {
			return err
		}
		if key == "" {
			out, err = c.proposeCampaign(ctx, tx, now, cat, g, true, body)
		} else {
			out, err = c.proposeCampaignKeyed(ctx, tx, now, cat, g, true, body, key)
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	// A refusal is problem details in binding v1 (spec/bindings/http/README.md, "Errors").
	if out.State == "rejected" {
		return nil, refuse(out.Reason, "the proposal is refused")
	}
	return map[string]any{"campaign": out.Campaign, "state": out.State}, nil
}
