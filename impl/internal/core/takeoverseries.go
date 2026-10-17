// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// The series farm.takeover.count of the takeovers from v0.3 (MGR-48, PRP-22).

// seriesTakeoverCount counts the takeovers by the functionary role and the task type of the
// original task; input for people, never a trial metric: it has no outcome block (MGR-48).
const seriesTakeoverCount = "farm.takeover.count"

// takeoverSeries is the definition of farm.takeover.count for a tenant from v0.3 (MGR-48).
func takeoverSeries() seriesDef {
	return seriesDef{Name: seriesTakeoverCount, CatalogKind: "task_type", DataClass: coreSeriesMark, Source: "core",
		RawDays: obsRawDays, RollDays: obsRollupDays, Rollups: []string{"day.max", "day.p95"}}
}

// takeoverPoint counts one takeover in farm.takeover.count on the task type of the original task,
// with the functionary role that runs the new task; the takeover id keys it, so it is written
// once (MGR-48).
func takeoverPoint(ctx context.Context, tx pgx.Tx, now time.Time, tenant, typ, role, takeover string) error {
	return writePoint(ctx, tx, now, obsPoint{Tenant: tenant, Series: seriesTakeoverCount, Subject: "task_type:" + typ, Source: "core",
		Mark: coreSeriesMark, Role: role, At: now, Num: num(1), Key: "takeover:" + takeover})
}

// notTakeoverPoints leaves out of a trial computation every point of an attempt on the seat
// model: the points keyed by the lease, by the task and by a run.finished event of a takeover
// task (MGR-48, PRP-22). $1 is the tenant.
const notTakeoverPoints = ` AND NOT (key LIKE 'lease:%' AND split_part(key, ':', 2) IN (SELECT id FROM tasks WHERE tenant = $1 AND record ? 'takeover_of'))` +
	` AND NOT (key LIKE 'task:%' AND substr(key, 6) IN (SELECT id FROM tasks WHERE tenant = $1 AND record ? 'takeover_of'))` +
	` AND NOT (key LIKE 'event:%' AND substr(key, 7) IN (SELECT e.id FROM tasks t JOIN events e ON e.tenant = t.tenant AND e.data->>'task' = t.id
		AND e.type = '` + runFinishedType + `' WHERE t.tenant = $1 AND t.record ? 'takeover_of'))`
