// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// farmGuardDay is what the farm manager's daily review reads of the two farm guards of a tenant
// from v0.3 (PRP-26): the share of successful tasks (farm.task.outcome) and the 95th percentile
// of the queue wait (farm.queue.wait) over the whole tenant in [from, to), each with its number of
// samples, the same values a change trial compares. Closed numbers only; nil before v0.3.
func (c *Core) farmGuardDay(ctx context.Context, tx pgx.Tx, cat *Catalog, tenant string, from, to time.Time) ([]any, error) {
	series := c.tenantSeries(cat, tenant)
	out := []any{}
	for _, name := range farmGuards {
		g, ok := series[name]
		if !ok || g.Outcome == nil {
			continue
		}
		v, err := trialValue(ctx, tx, tenant, g, "tenant:"+tenant, from, to, "", "")
		if err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"series": name, "subject": "tenant:" + tenant, "compare": g.Outcome.Compare, "value": v["value"], "samples": v["samples"]})
	}
	return out, nil
}
