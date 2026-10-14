// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"testing"
	"time"
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
