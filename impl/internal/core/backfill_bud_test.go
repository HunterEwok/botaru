// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// doctorChecks runs doctor for the bench tenant and returns its checks by id.
func doctorChecks(t *testing.T, b *bench) map[string]DoctorCheck {
	t.Helper()
	doc, err := b.c.Doctor(context.Background(), b.op, TenantRequest{})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]DoctorCheck{}
	for _, ch := range doc.Checks {
		out[ch.ID] = ch
	}
	return out
}

// doctor shows the actual usage of each budget line over 30 days next to the line's limit, the
// daily review of the manager as a row of its own; a tenant with farm_manager.enabled: false and
// no manager line passes apply and has no manager rows (BUD-15).
func TestBackfillDoctorLineUsage(t *testing.T) {
	ctx := context.Background()
	spend := func(b *bench) {
		t.Helper()
		if _, err := b.c.st.Pool.Exec(ctx, `INSERT INTO spend (id, tenant, line, unit, state, eur_micros, time) VALUES
			('spd_w1', 'demo', 'work', 'tokens', 'committed', 1200, botaru_now()),
			('spd_w2', 'demo', 'work', 'tokens', 'committed', 34, botaru_now() - interval '29 days'),
			('spd_w3', 'demo', 'work', 'tokens', 'committed', 5000, botaru_now() - interval '31 days'),
			('spd_i1', 'demo', 'insight', 'tokens', 'committed', 700, botaru_now())`); err != nil {
			t.Fatal(err)
		}
	}
	on := newBench(t)
	spend(on)
	checks := doctorChecks(t, on)
	// BUD-15: the usage of the last 30 days next to the line's limit.
	if v := checks["budget.used_30d_eur_micros.work"].Value; v != int64(1234) {
		t.Errorf("work used %v, want 1234", v)
	}
	if v := checks["budget.used_30d_eur_micros.insight"].Value; v != int64(700) {
		t.Errorf("insight used %v, want 700", v)
	}
	if v, ok := checks["budget.month_eur.insight"].Value.(*float64); !ok || v == nil || *v != 15 {
		t.Errorf("insight limit %v", checks["budget.month_eur.insight"].Value)
	}
	// BUD-15: the manager's daily review is a row of its own.
	if _, ok := checks["budget.used_30d_eur_micros.manager_review"]; !ok {
		t.Error("no row of the manager's daily review")
	}

	off := newBenchWith(t, func(root string, ms []map[string]any) []map[string]any {
		for _, m := range ms {
			if m["kind"] == "Tenant" {
				s := m["spec"].(map[string]any)
				s["farm_manager"] = map[string]any{"enabled": false}
				delete(s["budgets"].(map[string]any)["lines"].(map[string]any), "manager")
			}
		}
		return ms
	})
	spend(off)
	checks = doctorChecks(t, off)
	if v := checks["budget.used_30d_eur_micros.work"].Value; v != int64(1234) {
		t.Errorf("without the manager: work used %v", v)
	}
	// BUD-15: no manager rows for a tenant without the manager.
	for id := range checks {
		if strings.Contains(id, "manager") {
			t.Errorf("a manager row without the manager: %s", id)
		}
	}
}

// A read campaign over the card threshold is paid from the tenant's month and its budget line
// for the amount on the card, reserved whole at the proposal, not from the initiator's day; a
// campaign without a card that lacks only the initiator's day gets a card instead of a refusal;
// the paused estimate_exceeded is not cleared by Continue (BUD-12).
func TestBackfillCampaignPaidByCard(t *testing.T) {
	ctx := context.Background()
	b := newCampaignReadBench(t, map[string]any{"confirm_read_over": map[string]any{"items": 2}})
	// The initiator's day limit: 0.15 EUR, of which 0.06 are used.
	cat, err := b.c.Catalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	g := cat.Tenants["demo"].Groups["hello"]
	day := 0.15
	g.DayEUR = &day
	cat.Tenants["demo"].Groups["hello"] = g
	if _, err := b.c.st.Pool.Exec(ctx, `INSERT INTO spend (id, tenant, line, unit, state, eur_micros, time, subject)
		VALUES ('spd_day', 'demo', 'work', 'tokens', 'committed', 60000, botaru_now(), $1)`, b.alice); err != nil {
		t.Fatal(err)
	}
	att, h := b.attemptTools(t, "1", false)
	tmpl := map[string]any{"kind": "task", "type": "hello.child.v1", "goal": "Check the machine."}
	// Three items over the threshold of two: a card.
	r := proposeCall(t, b, h, map[string]any{"template": tmpl, "items": map[string]any{"resources": []any{"web1", "web2", "web3"}}})
	if r.State != "proposed" {
		t.Fatalf("a campaign over the threshold: %+v", r)
	}
	// Two items under the threshold, but over the rest of the initiator's day: a card too.
	r2 := proposeCall(t, b, h, map[string]any{"template": tmpl, "items": map[string]any{"resources": []any{"web1", "web4"}}})
	if r2.State != "proposed" {
		t.Fatalf("a campaign short of the initiator's day: %+v", r2)
	}
	b.finish(t, att)
	for id, n := range map[string]int64{r.Campaign: 3, r2.Campaign: 2} {
		cmp := b.campaign(t, id)
		var reserve int64
		var subject *string
		if err := b.c.st.Pool.QueryRow(ctx, `SELECT eur_micros, subject FROM spend WHERE campaign = $1 AND unit = 'task'`, id).Scan(&reserve, &subject); err != nil {
			t.Fatal(err)
		}
		// BUD-12: the whole estimate is reserved at the proposal, the amount of the card, outside
		// the initiator's day.
		if !cmp.Card || !cmp.Rec.PaidByCard || float64(reserve) > cmp.Rec.Reserve.EUR*1e6 || reserve < cmp.Rec.ItemMicros*n {
			t.Errorf("campaign %s: card %v paid %v reserve %d of %.2f EUR", id, cmp.Card, cmp.Rec.PaidByCard, reserve, cmp.Rec.Reserve.EUR)
		}
		if subject != nil && *subject == b.alice {
			t.Errorf("campaign %s: the reserve is in the initiator's day", id)
		}
	}
	if !b.campaign(t, r2.Campaign).Rec.OverDayLimit || b.campaign(t, r.Campaign).Rec.OverDayLimit {
		t.Error("the day-limit card is not marked")
	}
	// BUD-12: an item that does not fit the rest of the reserve pauses the campaign with
	// estimate_exceeded, and Continue does not clear it.
	cmp := b.campaign(t, r.Campaign)
	err = b.c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		return b.c.setCampaignState(ctx, tx, now, cmp, "paused", "estimate_exceeded", "")
	})
	if err != nil {
		t.Fatal(err)
	}
	cmp = b.campaign(t, r.Campaign)
	err = b.c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		return b.c.resumeCampaign(ctx, tx, now, cat, cmp, b.alice,
			func(reason, detail string) error { return refuse(reason, "%s", detail) }, func(string) error { return nil })
	})
	wantReason(t, err, "estimate_exceeded")
}
