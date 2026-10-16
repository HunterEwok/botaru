// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// accountStaleAfter is the age after which doctor no longer trusts an adapter's account report;
// the adapter sends one every 10 minutes (MON-4).
const accountStaleAfter = time.Hour

// accountReport is the data of ext.monitor.account.v1.
type accountReport struct {
	Findings  []string `json:"findings"`
	Visible   []string `json:"visible"`
	Scripts   []string `json:"scripts"`
	Truncated bool     `json:"truncated,omitempty"`
}

// accountSignal keeps the newest report of the adapter about its own monitoring system account
// (MON-4, OBS-11). The report was read through the monitoring system API with the adapter's own
// user, so no other process holds that user; it names hosts only by the hashes of their names.
// An unchanged report only moves its time; a changed one is also an event.
func (c *Core) accountSignal(ctx context.Context, w *Worker, data map[string]any) (*ActivityResult, error) {
	if err := c.spec.Validate("ext-monitor-account", data); err != nil {
		return nil, refuse("input_schema_invalid", "the account report is outside its schema")
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	out := &ActivityResult{Result: "ignored"}
	err = c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		var old []byte
		err := tx.QueryRow(ctx, `SELECT report FROM monitor_accounts WHERE tenant = $1 FOR UPDATE`, w.Tenant).Scan(&old)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO monitor_accounts (tenant, report, checked_at) VALUES ($1, $2, $3)
			ON CONFLICT (tenant) DO UPDATE SET report = excluded.report, checked_at = excluded.checked_at`, w.Tenant, raw, now); err != nil {
			return err
		}
		var was any
		_ = json.Unmarshal(old, &was)
		if old != nil && reflect.DeepEqual(was, normJSON(data)) {
			return nil
		}
		return c.appendEvent(ctx, tx, now, w.Tenant, Event{Type: "ext.monitor.account.v1", Subject: w.Tenant, Data: data})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// nodeHash is how an account report names a node: sha256:<hex> of its monitoring system host
// name (spec/model/event-data/ext-monitor-account.schema.json).
func nodeHash(node string) string {
	sum := sha256.Sum256([]byte(node))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// accountWider is what makes a tenant's adapter account wider than MON-4 allows, by the report
// and the catalogs: the role rights the report found; a tenant account that sees a farm node, a
// node of another tenant's catalog (sees:<node>) or nodes no catalog of the tenant links
// (sees_unlinked:<n>); the farm account that sees a tenant node; any account with a script on a
// farm node (script:<node>, also outside the farm node group, DEP-11); and an account that sees
// more hosts than a report holds.
func accountWider(cat *Catalog, farmNodes []string, tenant string, r accountReport) []string {
	out := slices.Clone(r.Findings)
	farm := map[string]string{}
	for _, n := range farmNodes {
		farm[nodeHash(n)] = n
	}
	own, other := map[string]bool{}, map[string]string{}
	for _, t := range slices.Sorted(mapKeys(cat.Tenants)) {
		for _, h := range monitorHosts(cat, t) {
			if t == tenant {
				own[nodeHash(h.Node)] = true
			} else if _, ok := other[nodeHash(h.Node)]; !ok {
				other[nodeHash(h.Node)] = h.Node
			}
		}
	}
	var seen []string
	unlinked := 0
	for _, hv := range r.Visible {
		switch {
		case other[hv] != "" && !own[hv]:
			seen = append(seen, other[hv])
		case tenant == farmTenant:
		case farm[hv] != "":
			seen = append(seen, farm[hv])
		case !own[hv]:
			unlinked++
		}
	}
	slices.Sort(seen)
	for _, n := range slices.Compact(seen) {
		out = append(out, "sees:"+n)
	}
	if unlinked > 0 {
		out = append(out, fmt.Sprintf("sees_unlinked:%d", unlinked))
	}
	var scripts []string
	for _, hs := range r.Scripts {
		if n := farm[hs]; n != "" {
			scripts = append(scripts, n)
		}
	}
	slices.Sort(scripts)
	for _, n := range slices.Compact(scripts) {
		out = append(out, "script:"+n)
	}
	if r.Truncated {
		out = append(out, "truncated")
	}
	return out
}

// doctorAccounts adds per tenant the line monitor.account_scope: whether the tenant's adapter
// account sees and may do only what MON-4 allows (OBS-11). It fails on a wider account and
// warns when the adapter sent no report in the last hour, since the scope is then unknown.
func (c *Core) doctorAccounts(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, tenant string, add func(DoctorCheck)) error {
	if !c.monitorOn() {
		return nil
	}
	reports := map[string]accountReport{}
	at := map[string]time.Time{}
	rows, err := tx.Query(ctx, `SELECT tenant, report, checked_at FROM monitor_accounts ORDER BY tenant`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var t string
		var raw []byte
		var when time.Time
		if err := rows.Scan(&t, &raw, &when); err != nil {
			rows.Close()
			return err
		}
		var r accountReport
		if err := json.Unmarshal(raw, &r); err != nil {
			rows.Close()
			return err
		}
		reports[t], at[t] = r, when
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	// Every tenant with an adapter worker, and every tenant that reported.
	wrows, err := tx.Query(ctx, `SELECT DISTINCT tenant FROM workers WHERE kind = 'monitor' AND state <> 'revoked'`)
	if err != nil {
		return err
	}
	tenants, err := pgx.CollectRows(wrows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for t := range reports {
		if !slices.Contains(tenants, t) {
			tenants = append(tenants, t)
		}
	}
	slices.Sort(tenants)
	for _, t := range tenants {
		if tenant != farmTenant && t != tenant {
			continue
		}
		ch := DoctorCheck{ID: "monitor.account_scope", Tenant: t}
		r, ok := reports[t]
		switch {
		case !ok || now.Sub(at[t]) > accountStaleAfter:
			ch.OK, ch.Severity, ch.Value = false, "warn", "unchecked"
		default:
			wide := accountWider(cat, c.cfg.FarmHosts, t, r)
			ch.OK, ch.Severity = len(wide) == 0, sevOf(len(wide) == 0, "fail")
			if len(wide) > 0 {
				ch.Value = strings.Join(wide, ", ")
			}
		}
		add(ch)
	}
	return nil
}
