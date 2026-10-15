// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/ids"
)

// SpendConfig holds the reconciliation settings of the deployment (BUD-10).
type SpendConfig struct {
	// ToleranceEUR and TolerancePct bound the difference of a day that needs no alarm: the
	// larger of the two passes.
	ToleranceEUR float64 `yaml:"tolerance_eur"`
	TolerancePct float64 `yaml:"tolerance_pct"`
	// EstimateReleaseAfter is how long an estimated line waits for a reconciliation before its
	// reserve is released by time.
	EstimateReleaseAfter string `yaml:"estimate_release_after"`
	// ReconcileOverdueDays is the age of the last reconciliation of a workspace that raises
	// alarm.spend.reconcile_overdue.
	ReconcileOverdueDays int `yaml:"reconcile_overdue_days"`

	releaseAfter time.Duration
}

func (s *SpendConfig) defaults() {
	if s.ToleranceEUR == 0 {
		s.ToleranceEUR = 0.5
	}
	if s.TolerancePct == 0 {
		s.TolerancePct = 2
	}
	s.releaseAfter = 7 * 24 * time.Hour
	if d, ok := parseDuration(s.EstimateReleaseAfter); ok && d > 0 {
		s.releaseAfter = d
	}
	if s.ReconcileOverdueDays == 0 {
		s.ReconcileOverdueDays = 35
	}
}

// SpendReconcileRequest is p2-spend-reconcile-request.
type SpendReconcileRequest struct {
	Op       string               `json:"op"`
	Day      string               `json:"day"`
	Provider string               `json:"provider"`
	Rows     []SpendReconcileItem `json:"rows"`
}

// SpendReconcileItem is the provider's daily sum of one workspace.
type SpendReconcileItem struct {
	Tenant         string `json:"tenant"`
	Workspace      string `json:"workspace"`
	SpentEURMicros int64  `json:"spent_eur_micros"`
	ReportID       string `json:"report_id"`
}

// SpendReconcile reconciles the usage of each workspace (tenant, provider) for one UTC day
// with the provider's daily sum (BUD-10, D-87 (1)). The farm sum of the day is the sum of its
// final lines of that provider; an estimated line is left out of it, gets the reconciled mark
// without a price change and from then on counts as zero in the limits, because the correction
// line carries what the provider billed for it. The difference is one signed correction line;
// a second request for the day replaces the first. A difference above the tolerance raises
// alarm.spend.estimate_drift; every reconciliation closes the overdue alarm of the workspace.
func (c *Core) SpendReconcile(ctx context.Context, op Operator, req SpendReconcileRequest) (*Done, error) {
	day, err := time.Parse("2006-01-02", req.Day)
	if err != nil {
		return nil, refuse("input_schema_invalid", "day is not a date")
	}
	// The provider report and its workspaces are farm level: only the operator of farm or the
	// reconciliation process with its key hands them in (BUD-10).
	if op.Tenant != "farm" {
		return nil, refuse("forbidden", "only an operator of farm reconciles spend")
	}
	provider := strings.ToLower(req.Provider)
	// Rows of one tenant add up: the farm has one workspace per tenant and provider.
	sums := map[string]SpendReconcileItem{}
	for _, r := range req.Rows {
		acc := sums[r.Tenant]
		acc.Tenant = r.Tenant
		acc.SpentEURMicros += r.SpentEURMicros
		if acc.ReportID == "" {
			acc.ReportID = r.ReportID
		}
		sums[r.Tenant] = acc
	}
	err = c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		if !day.Before(now.UTC().Truncate(24 * time.Hour)) {
			return refuse("input_schema_invalid", "only a past UTC day is reconciled")
		}
		// The report of the day is received, with rows or without: a provider day with no usage
		// still closes the spend gap after a restore (RET-6).
		if _, err := tx.Exec(ctx, `INSERT INTO spend_report_days (provider, day, created_at) VALUES ($1, $2, $3)
			ON CONFLICT (provider, day) DO UPDATE SET created_at = $3`, provider, day, now); err != nil {
			return err
		}
		for _, tenant := range slices.Sorted(mapKeys(sums)) {
			r := sums[tenant]
			var known bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM tenants WHERE name = $1)`, r.Tenant).Scan(&known); err != nil {
				return err
			}
			if !known {
				return refuse("not_found", "no tenant %s", r.Tenant)
			}
			if err := c.reconcileDay(ctx, tx, now, op, day, provider, r); err != nil {
				return err
			}
		}
		return c.closeSpendGap(ctx, tx, now)
	})
	if err != nil {
		return nil, err
	}
	return &Done{OK: true}, nil
}

func (c *Core) reconcileDay(ctx context.Context, tx pgx.Tx, now time.Time, op Operator, day time.Time, provider string, r SpendReconcileItem) error {
	from, to := day, day.Add(24*time.Hour)
	// Lock the workspace day, so two requests for it do not both write a correction.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('reconcile:' || $1 || ':' || $2 || ':' || $3, 0))`, r.Tenant, provider, day.Format("2006-01-02")); err != nil {
		return err
	}
	var farm int64
	var open bool
	if err := tx.QueryRow(ctx, `SELECT coalesce(sum(eur_micros) FILTER (WHERE state = 'committed'), 0), bool_or(state = 'reserved') IS TRUE
		FROM spend WHERE tenant = $1 AND provider = $2 AND time >= $3 AND time < $4 AND unit <> 'correction'`, r.Tenant, provider, from, to).Scan(&farm, &open); err != nil {
		return err
	}
	// A call of the day still waiting for its answer would be counted twice: once in the
	// correction, once when it settles.
	if open {
		return refuse("conflict", "the day still has open reserves of %s; reconcile it later", r.Tenant)
	}
	// A model call line without a provider (from before 0014, its profile no longer applied)
	// is not in the farm sum, so the correction would count it twice.
	var unnamed bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM spend WHERE tenant = $1 AND provider IS NULL AND unit = 'tokens' AND time >= $2 AND time < $3)`, r.Tenant, from, to).Scan(&unnamed); err != nil {
		return err
	}
	if unnamed {
		return refuse("conflict", "the day has model usage of %s without a provider", r.Tenant)
	}
	diff := r.SpentEURMicros - farm
	if _, err := tx.Exec(ctx, `DELETE FROM spend WHERE tenant = $1 AND provider = $2 AND unit = 'correction' AND time = $3`, r.Tenant, provider, from); err != nil {
		return err
	}
	if diff != 0 {
		if _, err := tx.Exec(ctx, `INSERT INTO spend (id, tenant, line, unit, state, eur_micros, correction_eur_micros, provider, reconciled_at, time)
			VALUES ($1, $2, 'correction', 'correction', 'committed', 0, $3, $4, $5, $6)`, ids.New(ids.Spend), r.Tenant, diff, provider, now, from); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE spend SET reconciled_at = $5 WHERE tenant = $1 AND provider = $2 AND time >= $3 AND time < $4
		AND state = 'estimated' AND reconciled_at IS NULL`, r.Tenant, provider, from, to, now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO spend_reconciliations (tenant, provider, day, provider_micros, farm_micros, correction_micros, report_id, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (tenant, provider, day) DO UPDATE SET provider_micros = $4, farm_micros = $5, correction_micros = $6, report_id = $7, created_at = $8`,
		r.Tenant, provider, from, r.SpentEURMicros, farm, diff, nullable(r.ReportID), now); err != nil {
		return err
	}
	drift := Alarm{Tenant: r.Tenant, Name: "spend.estimate_drift", Subject: AlarmSubject{Kind: "workspace", ID: strings.ToLower(provider)}, Severity: "warn"}
	if abs(diff) > c.spendTolerance(r.SpentEURMicros) {
		if err := c.fireAlarm(ctx, tx, now, drift); err != nil {
			return err
		}
	} else if err := c.resolveAlarm(ctx, tx, now, drift); err != nil {
		return err
	}
	if err := c.resolveAlarm(ctx, tx, now, overdueAlarm(r.Tenant, provider)); err != nil {
		return err
	}
	_, err := c.appendAudit(ctx, tx, now, Audit{
		Tenant: r.Tenant, Kind: "command", Actor: Actor{Principal: op.Principal, Via: "farmctl"},
		Command: "spend.reconcile", Target: &Target{Kind: "workspace", ID: provider}, OK: true,
		After: map[string]any{"day": day.Format("2006-01-02"), "provider_eur_micros": r.SpentEURMicros, "farm_eur_micros": farm, "correction_eur_micros": diff},
	})
	return err
}

func (c *Core) spendTolerance(providerMicros int64) int64 {
	s := c.cfg.Spend
	return max(int64(s.ToleranceEUR*1e6), providerMicros*int64(s.TolerancePct*100)/10000)
}

func abs(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}

func overdueAlarm(tenant, provider string) Alarm {
	return Alarm{Tenant: tenant, Name: "spend.reconcile_overdue", Subject: AlarmSubject{Kind: "workspace", ID: strings.ToLower(provider)}, Severity: "warn"}
}

// workspaceAge is the time since the last reconciliation of a workspace, or since its first
// usage line when it was never reconciled.
type workspaceAge struct {
	Tenant, Provider string
	Since            time.Time
	Reconciled       bool
}

// workspaceAges lists the workspaces (tenant, provider) that have usage lines, with the age of
// their last reconciliation; spend_workspaces keeps the first line of each, so this reads no
// usage lines.
func workspaceAges(ctx context.Context, tx pgx.Tx, tenant string) ([]workspaceAge, error) {
	rows, err := tx.Query(ctx, `SELECT w.tenant, w.provider, coalesce(r.last, w.first_at), r.last IS NOT NULL
		FROM spend_workspaces w LEFT JOIN (SELECT tenant, provider, max(created_at) AS last FROM spend_reconciliations GROUP BY tenant, provider) r
			ON r.tenant = w.tenant AND r.provider = w.provider
		WHERE $1 = '' OR w.tenant = $1 ORDER BY w.tenant, w.provider`, tenant)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []workspaceAge
	for rows.Next() {
		var w workspaceAge
		if err := rows.Scan(&w.Tenant, &w.Provider, &w.Since, &w.Reconciled); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// budgetLoop releases the reserve of estimated lines that no reconciliation reached within
// spend.estimate_release_after, and raises the overdue alarm of a workspace without a
// reconciliation for spend.reconcile_overdue_days; only a reconciliation closes it (BUD-10).
func (c *Core) budgetLoop(ctx context.Context) error {
	return c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		if !c.slowPass(&c.budgetAt, now) {
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE spend SET released_at = $1 WHERE state = 'estimated' AND reconciled_at IS NULL AND released_at IS NULL AND time <= $2`,
			now, now.Add(-c.cfg.Spend.releaseAfter)); err != nil {
			return err
		}
		ws, err := workspaceAges(ctx, tx, "")
		if err != nil {
			return err
		}
		overdue := time.Duration(c.cfg.Spend.ReconcileOverdueDays) * 24 * time.Hour
		for _, w := range ws {
			if now.Sub(w.Since) > overdue {
				if err := c.fireAlarm(ctx, tx, now, overdueAlarm(w.Tenant, w.Provider)); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// lineUsage is the usage of a budget line over the last 30 days next to its monthly limit
// (BUD-15).
type lineUsage struct {
	Line     string
	Used     int64
	MonthEUR *float64
}

func (c *Core) lineUsages(ctx context.Context, tx pgx.Tx, now time.Time, tv *TenantView) ([]lineUsage, error) {
	lines := map[string]*float64{}
	for name, l := range tv.Spec.Budgets.Lines {
		lines[name] = l.MonthEUR
	}
	rows, err := tx.Query(ctx, `SELECT line FROM spend WHERE tenant = $1 AND time >= $2 AND unit NOT IN ('task', 'correction') GROUP BY line`, tv.Name, now.Add(-30*24*time.Hour))
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var l string
		if err := rows.Scan(&l); err != nil {
			rows.Close()
			return nil, err
		}
		if _, ok := lines[l]; !ok {
			lines[l] = nil
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var out []lineUsage
	for _, l := range slices.Sorted(mapKeys(lines)) {
		used, err := windowUsed(ctx, tx, tv.Name, spendFilter{line: l}, now.Add(-30*24*time.Hour), "")
		if err != nil {
			return nil, err
		}
		out = append(out, lineUsage{Line: l, Used: used, MonthEUR: lines[l]})
	}
	return out, nil
}

// doctorMoney adds the money and rollout lines of doctor: a drain past its deadline (LCY-6),
// the age of the last reconciliation of each workspace of the tenant (BUD-10), the usage of each
// budget line over 30 days next to its limit (BUD-15) and the age of the profile prices
// (GW-17: older than 60 days warns, older than 120 days fails).
// The catalog is loaded before the transaction: Catalog reads through the pool, which a
// single-connection pool would not hand out while the transaction holds it.
func (c *Core) doctorMoney(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, tenant string, add func(DoctorCheck)) error {
	if err := c.doctorBackup(ctx, tx, now, add); err != nil {
		return err
	}
	until, err := drainUntil(ctx, tx)
	if err != nil {
		return err
	}
	drain := DoctorCheck{ID: "core.draining", OK: true, Severity: "info", Value: nil}
	if until != nil {
		drain.Value = fmtTime(*until)
		if now.After(*until) {
			drain.OK, drain.Severity = false, "warn"
		}
	}
	add(drain)
	ws, err := workspaceAges(ctx, tx, tenant)
	if err != nil {
		return err
	}
	overdue := time.Duration(c.cfg.Spend.ReconcileOverdueDays) * 24 * time.Hour
	for _, w := range ws {
		age := int64(now.Sub(w.Since).Seconds())
		ch := DoctorCheck{ID: checkID("spend.reconcile_age_s." + w.Provider), OK: true, Severity: "info", Value: age, Tenant: w.Tenant}
		if now.Sub(w.Since) > overdue {
			ch.OK, ch.Severity = false, "warn"
		}
		add(ch)
	}
	if tv := cat.Tenants[tenant]; tv != nil {
		lines, err := c.lineUsages(ctx, tx, now, tv)
		if err != nil {
			return err
		}
		for _, l := range lines {
			add(DoctorCheck{ID: checkID("budget.used_30d_eur_micros." + l.Line), OK: true, Severity: "info", Value: l.Used, Tenant: tenant})
			add(DoctorCheck{ID: checkID("budget.month_eur." + l.Line), OK: true, Severity: "info", Value: l.MonthEUR, Tenant: tenant})
		}
	}
	for _, k := range slices.SortedFunc(mapKeys(cat.Profiles), func(a, b [2]string) int { return strings.Compare(a[0]+"/"+a[1], b[0]+"/"+b[1]) }) {
		if k[0] != "" && k[0] != tenant {
			continue
		}
		checked, err := time.Parse("2006-01-02", cat.Profiles[k].Price.Checked)
		if err != nil {
			continue
		}
		days := int64(now.Sub(checked).Hours() / 24)
		ch := DoctorCheck{ID: checkID(strings.TrimPrefix(k[0]+".", ".") + "model.price_age_days." + k[1]), OK: true, Severity: "info", Value: days}
		switch {
		case days > 120:
			ch.OK, ch.Severity = false, "fail"
		case days > 60:
			ch.OK, ch.Severity = false, "warn"
		}
		add(ch)
	}
	return nil
}

var checkIDBad = regexp.MustCompile(`[^a-z0-9_.]`)

// checkID makes a doctor check id of a name with other characters: lowercase, other
// characters as _, at most 64.
func checkID(s string) string {
	s = checkIDBad.ReplaceAllString(strings.ToLower(s), "_")
	if len(s) > 64 {
		s = s[:64]
	}
	return s
}

// slowPassEvery is the pause between two passes of a loop whose work is not urgent (budget,
// retention): the tick runs every second, the pass at most once a minute by the store clock.
const slowPassEvery = time.Minute

// slowPass says whether a slow loop runs at now, and records the pass.
func (c *Core) slowPass(last *atomic.Int64, now time.Time) bool {
	// A loop.tick on a test bench always runs the pass it asks for.
	if c.manual.Load() {
		last.Store(now.UnixNano())
		return true
	}
	prev := last.Load()
	if prev != 0 && now.UnixNano()-prev < int64(slowPassEvery) {
		return false
	}
	return last.CompareAndSwap(prev, now.UnixNano())
}
