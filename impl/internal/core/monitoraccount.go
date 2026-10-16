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

// accountSignal keeps the newest report of an adapter worker about its own monitoring system
// account (MON-4, OBS-11), per tenant and worker. The report was read through the monitoring
// system API with the adapter's own user, so no other process holds that user; it names hosts
// only by the hashes of their names, and those lists stay in monitor_accounts. An unchanged
// report only moves its time; a changed one is also an event that carries the role findings and
// the counts of the lists, never the lists themselves.
func (c *Core) accountSignal(ctx context.Context, w *Worker, data map[string]any) (*ActivityResult, error) {
	if err := c.spec.Validate("ext-monitor-account", data); err != nil {
		return nil, refuse("input_schema_invalid", "the account report is outside its schema")
	}
	// One schema serves the signal and the event of the catalog (ext-monitor-account): the
	// event goes without the host lists, so the schema cannot require them, and the signal's
	// need for both lists is checked here. A separate signal schema would be an orphan of the
	// events catalog.
	if data["visible"] == nil || data["scripts"] == nil {
		return nil, refuse("input_schema_invalid", "the account report has no visible or scripts list")
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	var r accountReport
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, err
	}
	out := &ActivityResult{Result: "ignored"}
	err = c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		var old []byte
		err := tx.QueryRow(ctx, `SELECT report FROM monitor_accounts WHERE tenant = $1 AND worker = $2 FOR UPDATE`, w.Tenant, w.ID).Scan(&old)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO monitor_accounts (tenant, worker, report, checked_at) VALUES ($1, $2, $3, $4)
			ON CONFLICT (tenant, worker) DO UPDATE SET report = excluded.report, checked_at = excluded.checked_at`, w.Tenant, w.ID, raw, now); err != nil {
			return err
		}
		var was any
		_ = json.Unmarshal(old, &was)
		if old != nil && reflect.DeepEqual(was, normJSON(data)) {
			return nil
		}
		ev := map[string]any{"findings": r.Findings, "visible_count": len(r.Visible), "scripts_count": len(r.Scripts)}
		if ev["findings"] == nil {
			ev["findings"] = []string{}
		}
		if r.Truncated {
			ev["truncated"] = true
		}
		return c.appendEvent(ctx, tx, now, w.Tenant, Event{Type: "ext.monitor.account.v1", Subject: w.ID, Data: ev})
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
// and the catalogs: the role rights the report found; a tenant account that sees a node of
// another tenant's catalog, a farm node or nodes no catalog of the tenant links; the farm
// account that sees a tenant node; any account with a script on a farm node (also outside the
// farm node group, DEP-11); and an account that sees more hosts than a report holds.
//
// Only the farm operator reads node names (named): sees:<node>, script:<node>. Every other
// viewer gets counts by class, sees_other_tenant:<n>, sees_farm:<n>, sees_unlinked:<n> and
// script_farm:<n>, so a tenant's doctor never names a node of another tenant or of the farm.
func accountWider(cat *Catalog, farmNodes []string, tenant string, r accountReport, named bool) []string {
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
	otherN, farmN, unlinked := 0, 0, 0
	for _, hv := range r.Visible {
		switch {
		case other[hv] != "" && !own[hv]:
			seen = append(seen, other[hv])
			otherN++
		case tenant == farmTenant:
		case farm[hv] != "":
			seen = append(seen, farm[hv])
			farmN++
		case !own[hv]:
			unlinked++
		}
	}
	var scripts []string
	for _, hs := range r.Scripts {
		if n := farm[hs]; n != "" {
			scripts = append(scripts, n)
		}
	}
	count := func(class string, n int) {
		if n > 0 {
			out = append(out, fmt.Sprintf("%s:%d", class, n))
		}
	}
	if named {
		slices.Sort(seen)
		for _, n := range slices.Compact(seen) {
			out = append(out, "sees:"+n)
		}
		count("sees_unlinked", unlinked)
		slices.Sort(scripts)
		for _, n := range slices.Compact(scripts) {
			out = append(out, "script:"+n)
		}
	} else {
		count("sees_other_tenant", otherN)
		count("sees_farm", farmN)
		count("sees_unlinked", unlinked)
		count("script_farm", len(scripts))
	}
	if r.Truncated {
		out = append(out, "truncated")
	}
	return out
}

// doctorAccounts adds per tenant the line monitor.account_scope: whether every adapter account
// of the tenant sees and may do only what MON-4 allows (OBS-11). It fails when any worker's
// account is wider, and warns while an adapter worker sent no report in the last hour, since its
// scope is then unknown. named is true for the farm operator, the only viewer of node names.
func (c *Core) doctorAccounts(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, tenant string, named bool, add func(DoctorCheck)) error {
	if !c.monitorOn() {
		return nil
	}
	type report struct {
		r  accountReport
		at time.Time
	}
	reports := map[string]map[string]report{}
	rows, err := tx.Query(ctx, `SELECT tenant, worker, report, checked_at FROM monitor_accounts ORDER BY tenant, worker`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var t, w string
		var raw []byte
		var when time.Time
		if err := rows.Scan(&t, &w, &raw, &when); err != nil {
			rows.Close()
			return err
		}
		var r accountReport
		if err := json.Unmarshal(raw, &r); err != nil {
			rows.Close()
			return err
		}
		if reports[t] == nil {
			reports[t] = map[string]report{}
		}
		reports[t][w] = report{r, when}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	// Every adapter worker of every tenant, and every worker that reported.
	workers := map[string][]string{}
	wrows, err := tx.Query(ctx, `SELECT tenant, id FROM workers WHERE kind = 'monitor' AND state <> 'revoked' ORDER BY tenant, id`)
	if err != nil {
		return err
	}
	for wrows.Next() {
		var t, w string
		if err := wrows.Scan(&t, &w); err != nil {
			wrows.Close()
			return err
		}
		workers[t] = append(workers[t], w)
	}
	wrows.Close()
	if err := wrows.Err(); err != nil {
		return err
	}
	for t, rs := range reports {
		for w := range rs {
			if !slices.Contains(workers[t], w) {
				workers[t] = append(workers[t], w)
			}
		}
	}
	for _, t := range slices.Sorted(mapKeys(workers)) {
		if tenant != farmTenant && t != tenant {
			continue
		}
		ch := DoctorCheck{ID: "monitor.account_scope", Tenant: t, OK: true, Severity: "info"}
		var wide []string
		unchecked := false
		ws := workers[t]
		slices.Sort(ws)
		for _, w := range ws {
			rep, ok := reports[t][w]
			if !ok || now.Sub(rep.at) > accountStaleAfter {
				unchecked = true
				continue
			}
			for _, x := range accountWider(cat, c.cfg.FarmHosts, t, rep.r, named) {
				if !slices.Contains(wide, x) {
					wide = append(wide, x)
				}
			}
		}
		switch {
		case len(wide) > 0:
			ch.OK, ch.Severity, ch.Value = false, "fail", strings.Join(wide, ", ")
		case unchecked:
			ch.OK, ch.Severity, ch.Value = false, "warn", "unchecked"
		}
		add(ch)
	}
	return nil
}
