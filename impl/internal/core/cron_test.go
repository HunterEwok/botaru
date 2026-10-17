// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/manifest"
)

// Slots across both 2027 transitions of Europe/Berlin: 02:30 does not exist on 28 March and is
// a gap; it occurs twice on 31 October and runs once, at the first instant (TIM-3).
func TestCronSlots(t *testing.T) {
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}
	c, err := parseCron("30 2 * * *")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		after, want string
		gap         bool
	}{
		{"2027-03-27T12:00:00Z", "2027-03-28T00:59Z", true},
		{"2027-03-28T01:00:00Z", "2027-03-29T00:30Z", false},
		{"2027-10-30T12:00:00Z", "2027-10-31T00:30Z", false},
		{"2027-10-31T00:30:00Z", "2027-11-01T01:30Z", false},
	}
	for _, tc := range cases {
		after, _ := time.Parse(time.RFC3339, tc.after)
		s, ok := c.nextSlot(after, berlin)
		if !ok || fmtSlot(s.At) != tc.want || s.Gap != tc.gap {
			t.Errorf("after %s: %s gap %v, want %s gap %v", tc.after, fmtSlot(s.At), s.Gap, tc.want, tc.gap)
		}
	}
	for _, bad := range []string{"* * * *", "60 * * * *", "* * 0 * *", "*/0 * * * *", "5-1 * * * *", "a * * * *"} {
		if _, err := parseCron(bad); err == nil {
			t.Errorf("%q passed", bad)
		}
	}
	// Day of month and day of week both restricted: either matches.
	c, _ = parseCron("0 9 1 * 1")
	after, _ := time.Parse(time.RFC3339, "2027-02-01T10:00:00Z") // a Monday, the 1st
	s, _ := c.nextSlot(after, time.UTC)
	if fmtSlot(s.At) != "2027-02-08T09:00Z" {
		t.Errorf("dom or dow: %s", fmtSlot(s.At))
	}
}

// The limit on lowering a schedule's frequency counts from the value applied 90 days earlier:
// one halving passes, a second halving within the 90 days does not (PRP-16).
func TestCronCumulative(t *testing.T) {
	ctx := context.Background()
	b := newBench(t)
	cat := &Catalog{Tenants: map[string]*TenantView{"demo": {Name: "demo", Schedules: map[string]manifest.Schedule{"nightly": {Cron: "0 */2 * * *"}}}}}
	set := func(cron string) proposalChange {
		var ch proposalChange
		ch.Kind = "manifest_change"
		ch.ManifestChange = &struct {
			Ops []manifestOp `json:"ops"`
		}{Ops: []manifestOp{{Op: "set", Field: "spec.cron", Value: cron}}}
		ch.ManifestChange.Ops[0].Target.Kind, ch.ManifestChange.Ops[0].Target.Name = "Schedule", "nightly"
		return ch
	}
	check := func(cron string) error {
		return b.c.tx(ctx, func(tx pgx.Tx, now time.Time) error { return b.c.cronHistory(ctx, tx, now, cat, "demo", set(cron)) })
	}
	if err := check("0 */4 * * *"); err != nil {
		t.Fatalf("one halving: %v", err)
	}
	// Ten days ago the schedule ran every hour, so every four hours is a quarter of that.
	if _, err := b.c.st.Pool.Exec(ctx, `INSERT INTO manifest_versions (kind, tenant, name, doc, replaced_at)
		VALUES ('Schedule', 'demo', 'nightly', '{"spec": {"cron": "0 * * * *"}}', botaru_now() - interval '10 days')`); err != nil {
		t.Fatal(err)
	}
	var rj rejection
	if err := check("0 */4 * * *"); err == nil || !asRejection(err, &rj) || rj != "forbidden_change" {
		t.Fatalf("second halving: %v", err)
	}
	if err := check("0 */2 * * *"); err != nil {
		t.Fatalf("half of the old frequency: %v", err)
	}
}

func asRejection(err error, rj *rejection) bool {
	r, ok := err.(rejection)
	*rj = r
	return ok
}
