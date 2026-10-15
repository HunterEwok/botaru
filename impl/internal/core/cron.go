// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// cronSpec is a five-field cron expression: minute, hour, day of month, month, day of week
// (0 and 7 are Sunday). As in classic cron, when both day fields are restricted a day matches
// either of them.
type cronSpec struct {
	min, hour, dom, month, dow [64]bool
	domAny, dowAny             bool
}

func parseCron(expr string) (*cronSpec, error) {
	f := strings.Fields(expr)
	if len(f) != 5 {
		return nil, fmt.Errorf("cron has %d fields, not 5", len(f))
	}
	var c cronSpec
	ranges := []struct {
		set    *[64]bool
		lo, hi int
	}{{&c.min, 0, 59}, {&c.hour, 0, 23}, {&c.dom, 1, 31}, {&c.month, 1, 12}, {&c.dow, 0, 7}}
	for i, r := range ranges {
		if err := cronField(f[i], r.lo, r.hi, r.set); err != nil {
			return nil, fmt.Errorf("cron field %d: %w", i+1, err)
		}
	}
	if c.dow[7] {
		c.dow[0] = true
	}
	c.domAny, c.dowAny = f[2] == "*", f[4] == "*"
	return &c, nil
}

func cronField(s string, lo, hi int, set *[64]bool) error {
	for part := range strings.SplitSeq(s, ",") {
		rng, stepS, hasStep := strings.Cut(part, "/")
		step := 1
		if hasStep {
			n, err := strconv.Atoi(stepS)
			if err != nil || n < 1 {
				return fmt.Errorf("bad step %q", stepS)
			}
			step = n
		}
		a, b := lo, hi
		if rng != "*" {
			from, to, isRange := strings.Cut(rng, "-")
			x, err := strconv.Atoi(from)
			if err != nil {
				return fmt.Errorf("bad value %q", from)
			}
			a, b = x, x
			if isRange {
				if b, err = strconv.Atoi(to); err != nil {
					return fmt.Errorf("bad value %q", to)
				}
			} else if hasStep {
				b = hi
			}
		}
		if a < lo || b > hi || a > b {
			return fmt.Errorf("%d-%d is outside %d-%d", a, b, lo, hi)
		}
		for v := a; v <= b; v += step {
			set[v] = true
		}
	}
	return nil
}

func (c *cronSpec) dayMatches(d time.Time) bool {
	dom, dow := c.dom[d.Day()], c.dow[int(d.Weekday())]
	switch {
	case c.domAny && c.dowAny:
		return true
	case c.domAny:
		return dow
	case c.dowAny:
		return dom
	}
	return dom || dow
}

// slotTime is the result of nextSlot: the UTC instant of the slot, and gap when the local time
// of the slot does not exist (a switch to daylight saving time, TIM-3).
type slotTime struct {
	At  time.Time
	Gap bool
}

// nextSlot finds the first slot strictly after after: the local times of the expression in
// loc, in order of wall clock. A local time that occurs twice runs once, at its first
// instant; one that does not exist is a gap at the instant the clock jumps to. It looks a
// little over a year ahead.
func (c *cronSpec) nextSlot(after time.Time, loc *time.Location) (slotTime, bool) {
	start := after.In(loc)
	day := time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, time.UTC)
	for i := 0; i < 370; i++ {
		d := day.AddDate(0, 0, i)
		if !c.month[int(d.Month())] || !c.dayMatches(d) {
			continue
		}
		for h := 0; h < 24; h++ {
			if !c.hour[h] {
				continue
			}
			for m := 0; m < 60; m++ {
				if !c.min[m] {
					continue
				}
				at, gap := wallInstant(d.Year(), d.Month(), d.Day(), h, m, loc)
				if at.After(after) {
					return slotTime{At: at.UTC(), Gap: gap}, true
				}
			}
		}
	}
	return slotTime{}, false
}

// wallInstant is the first instant at which the clock of loc shows the given local time, and
// whether that local time does not exist; then it is the minute before the jump.
func wallInstant(y int, mo time.Month, d, h, m int, loc *time.Location) (time.Time, bool) {
	t := time.Date(y, mo, d, h, m, 0, 0, loc)
	lt := t.In(loc)
	if lt.Hour() != h || lt.Minute() != m || lt.Day() != d {
		// The local time was skipped: the slot is the last minute before the jump, so it does
		// not take the instant of a later local time.
		for probe := t.Add(-3 * time.Hour); probe.Before(t.Add(3 * time.Hour)); probe = probe.Add(time.Minute) {
			p := probe.In(loc)
			if p.Day() == d && (p.Hour() > h || (p.Hour() == h && p.Minute() > m)) {
				return probe.Truncate(time.Minute).Add(-time.Minute), true
			}
		}
		return t, true
	}
	// Of two instants with the same local time, the earlier one.
	for _, back := range []time.Duration{-2 * time.Hour, -time.Hour, -30 * time.Minute} {
		e := t.Add(back).In(loc)
		if e.Day() == d && e.Hour() == h && e.Minute() == m {
			return t.Add(back), false
		}
	}
	return t, false
}

// fmtSlot writes a slot in the schedule_slot format: UTC to the minute.
func fmtSlot(t time.Time) string { return t.UTC().Format("2006-01-02T15:04Z") }
