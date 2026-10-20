// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/HunterEwok/botaru/impl/internal/manifest"
)

// Tests of the monitoring adapter's account report and its doctor check (MON-4).

// MON-4, OBS-11: the adapter's account report decides what is wider than the rule allows, by
// the catalogs and the farm node list: the right to acknowledge problems, a demo account that
// sees a demo2 node or a farm node, a node no catalog of demo links, a script on core1; the farm
// account sees the farm nodes but no tenant node.
func TestAccountWider(t *testing.T) {
	t.Parallel()
	hosts := func(nodes ...string) map[string]any {
		var hs []any
		for i, n := range nodes {
			hs = append(hs, map[string]any{"id": "h" + string(rune('1'+i)), "monitor_host": n})
		}
		return map[string]any{"spec": map[string]any{"hosts": hs}}
	}
	cat := &Catalog{
		Tenants: map[string]*TenantView{"demo": {Name: "demo"}, "demo2": {Name: "demo2"}, "farm": {Name: "farm"}},
		RawGlobals: map[manifest.Key]map[string]any{
			{Kind: "Catalog", Tenant: "demo", Name: "demo"}:   hosts("web1.example.org"),
			{Kind: "Catalog", Tenant: "demo2", Name: "demo2"}: hosts("web2.example.org"),
		},
		Hashes: map[manifest.Key]string{},
	}
	farm := []string{"core1", "backup1"}
	h := nodeHash
	for _, named := range []bool{false, true} {
		if got := accountWider(cat, farm, "demo", accountReport{Visible: []string{h("web1.example.org")}, Scripts: []string{h("web1.example.org")}}, named); len(got) != 0 {
			t.Fatalf("MON-4: a narrow demo account %v", got)
		}
		if got := accountWider(cat, farm, "farm", accountReport{Visible: []string{h("core1"), h("backup1")}}, named); len(got) != 0 {
			t.Fatalf("MON-4: a narrow farm account %v", got)
		}
	}
	wide := accountReport{Findings: []string{"acknowledge_problems"},
		Visible: []string{h("web1.example.org"), h("web2.example.org"), h("core1"), h("db9.example.org")}, Scripts: []string{h("core1")}}
	// The farm operator reads the node names; any other viewer only counts by class.
	if got, want := accountWider(cat, farm, "demo", wide, true), []string{"acknowledge_problems", "sees:core1", "sees:web2.example.org", "sees_unlinked:1", "script:core1"}; !slices.Equal(got, want) {
		t.Fatalf("MON-4: a wide demo account for the farm operator %v", got)
	}
	if got, want := accountWider(cat, farm, "demo", wide, false), []string{"acknowledge_problems", "sees_other_tenant:1", "sees_farm:1", "sees_unlinked:1", "script_farm:1"}; !slices.Equal(got, want) {
		t.Fatalf("MON-4: a wide demo account for the tenant %v", got)
	}
	farmWide := accountReport{Visible: []string{h("core1"), h("web2.example.org")}, Scripts: []string{h("backup1")}, Truncated: true}
	if got := accountWider(cat, farm, "farm", farmWide, true); !slices.Equal(got, []string{"sees:web2.example.org", "script:backup1", "truncated"}) {
		t.Fatalf("MON-4: a wide farm account %v", got)
	}
	if got := accountWider(cat, farm, "farm", farmWide, false); !slices.Equal(got, []string{"sees_other_tenant:1", "script_farm:1", "truncated"}) {
		t.Fatalf("MON-4: a wide farm account by counts %v", got)
	}
}

// OBS-11: doctor shows per tenant whether every adapter account of the tenant sees only what
// MON-4 allows: a warning while a worker sent no report younger than an hour, a failure when any
// worker's account is wider, a pass when all are narrow; the adapter's report arrives as
// ext.monitor.account.v1. Only the farm operator reads node names: the tenant's own view counts
// by class and names no node of the farm or of another tenant, and the event log holds no host
// list. A revoked worker's report goes with it.
func TestDoctorAccountScope(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newBench(t)
	mon := b.monitorOn(t)
	b.c.cfg.FarmHosts = []string{"core1", "backup1"}
	farmOp := Operator{Principal: b.op.Principal, Tenant: "farm"}
	doctor := func(op Operator) *DoctorResult {
		t.Helper()
		d, err := b.c.Doctor(ctx, op, TenantRequest{Tenant: "demo"})
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	lineOf := func(op Operator) DoctorCheck {
		t.Helper()
		d := doctor(op)
		for _, ch := range d.Checks {
			if ch.ID == "monitor.account_scope" && ch.Tenant == "demo" {
				return ch
			}
		}
		t.Fatalf("no account line in %v", d.Checks)
		return DoctorCheck{}
	}
	line := func() DoctorCheck { t.Helper(); return lineOf(b.op) }
	if ch := line(); ch.OK || ch.Severity != "warn" || ch.Value != "unchecked" {
		t.Fatalf("OBS-11: no report yet %+v", ch)
	}
	report := func(w *Worker, findings []any, visible ...string) {
		t.Helper()
		hs := []any{}
		for _, v := range visible {
			hs = append(hs, nodeHash(v))
		}
		a := Activity{Kind: "signal", Channel: w.Target, UpdateID: "account:" + w.ID + strings.Join(visible, ","), Chat: Chat{Kind: "direct", ExternalID: "monitor"},
			Event: &SignalEvent{Type: "ext.monitor.account.v1", Data: map[string]any{"findings": findings, "visible": hs, "scripts": []any{}}}}
		a.Sender.Identity, a.Sender.Verified = "hook:monitor", true
		if _, err := b.c.SubmitActivity(ctx, w, a); err != nil {
			t.Fatal(err)
		}
	}
	report(mon, []any{"acknowledge_problems"}, "core1")
	if ch := line(); ch.OK || ch.Severity != "fail" || ch.Value != "acknowledge_problems, sees_farm:1" {
		t.Fatalf("MON-4: a wide account for the tenant %+v", ch)
	}
	raw, _ := json.Marshal(doctor(b.op))
	if strings.Contains(string(raw), "core1") {
		t.Fatalf("MON-4: the tenant's doctor names a farm node: %s", raw)
	}
	if ch := lineOf(farmOp); ch.OK || ch.Value != "acknowledge_problems, sees:core1" {
		t.Fatalf("MON-4: a wide account for the farm operator %+v", ch)
	}
	if n := countEvents(t, b, "ext.monitor.account.v1", mon.ID); n != 1 {
		t.Fatalf("%d account events", n)
	}
	var data map[string]any
	var evRaw []byte
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT data FROM events WHERE type = 'ext.monitor.account.v1' AND subject = $1`, mon.ID).Scan(&evRaw); err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(evRaw, &data)
	if _, ok := data["visible"]; ok || data["visible_count"] != 1.0 || strings.Contains(string(evRaw), nodeHash("core1")) {
		t.Fatalf("MON-4: the event log holds the host list: %s", evRaw)
	}
	validOut(t, b, "ext-monitor-account", data)
	report(mon, []any{})
	if ch := line(); !ch.OK || ch.Severity != "info" {
		t.Fatalf("MON-4: a narrow account %+v", ch)
	}
	// A second adapter worker of the tenant: unchecked until it reports, and its wide report
	// fails the line although the first account is narrow.
	mon2 := b.enroll(t, EnrollRequest{Tenant: "demo", Kind: "monitor", Target: "zabbix2"})
	if ch := line(); ch.OK || ch.Value != "unchecked" {
		t.Fatalf("OBS-11: a worker without a report %+v", ch)
	}
	report(mon2, []any{"close_problems"})
	if ch := line(); ch.OK || ch.Severity != "fail" || ch.Value != "close_problems" {
		t.Fatalf("MON-4: one wide worker of two %+v", ch)
	}
	if _, err := b.c.WorkerRevoke(ctx, b.op, WorkerRequest{Op: "worker.revoke", Worker: mon2.ID}); err != nil {
		t.Fatal(err)
	}
	if n := b.count(t, `SELECT count(*) FROM monitor_accounts WHERE worker = $1`, mon2.ID); n != 0 {
		t.Fatal("MON-4: a revoked worker's report stays")
	}
	if ch := line(); !ch.OK {
		t.Fatalf("MON-4: after the revoke %+v", ch)
	}
	b.advance(t, 2*3600)
	if ch := line(); ch.OK || ch.Value != "unchecked" {
		t.Fatalf("OBS-11: a stale report %+v", ch)
	}
	// A report outside its schema is refused, and so is one without its host lists.
	for _, d := range []map[string]any{
		{"findings": []any{"Admin " + "role"}, "visible": []any{}, "scripts": []any{}},
		{"findings": []any{}},
	} {
		a := Activity{Kind: "signal", Channel: "zabbix", UpdateID: "account:bad" + fmt.Sprint(len(d)), Chat: Chat{Kind: "direct", ExternalID: "monitor"},
			Event: &SignalEvent{Type: "ext.monitor.account.v1", Data: d}}
		a.Sender.Identity, a.Sender.Verified = "hook:monitor", true
		_, err := b.c.SubmitActivity(ctx, mon, a)
		wantReason(t, err, "input_schema_invalid")
	}
}
