// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"time"
)

// The schedule check before "Accept" of a proposal (TIM-5): code finds the local times of the
// new schedule that disappear or repeat at the time transitions of its zone within a year,
// from the transition dates of the zone, without listing the slots.

// maxTimeNotes bounds each list of the card.
const maxTimeNotes = 10

// zoneShift is one transition of a zone: the instant and the offsets before and after it.
type zoneShift struct {
	at            time.Time
	before, after int
}

// zoneShifts are the transitions of loc in [from, to): an hourly scan of the offset, then a
// search of the exact second.
func zoneShifts(loc *time.Location, from, to time.Time) []zoneShift {
	var out []zoneShift
	_, prev := from.In(loc).Zone()
	for t := from; t.Before(to); t = t.Add(time.Hour) {
		next := t.Add(time.Hour)
		_, off := next.In(loc).Zone()
		if off == prev {
			continue
		}
		lo, hi := t, next
		for hi.Sub(lo) > time.Second {
			mid := lo.Add(hi.Sub(lo) / 2)
			if _, o := mid.In(loc).Zone(); o == prev {
				lo = mid
			} else {
				hi = mid
			}
		}
		out = append(out, zoneShift{at: hi.Truncate(time.Second), before: prev, after: off})
		prev = off
	}
	return out
}

// scheduleTimes are the local times of the expression in loc that disappear (a jump forward)
// or repeat (a jump back) at the transitions of the next year from now, as "YYYY-MM-DD HH:MM".
func scheduleTimes(cs *cronSpec, loc *time.Location, now time.Time) (disappear, repeat []string) {
	for _, s := range zoneShifts(loc, now, now.AddDate(1, 0, 0)) {
		delta := time.Duration(s.after-s.before) * time.Second
		// The wall clock at the transition by the offset before it.
		wall := s.at.In(time.FixedZone("", s.before))
		start, span, list := wall, delta, &disappear
		if delta < 0 {
			start, span, list = wall.Add(delta), -delta, &repeat
		}
		for k := time.Duration(0); k < span; k += time.Minute {
			w := start.Add(k)
			d := time.Date(w.Year(), w.Month(), w.Day(), 0, 0, 0, 0, time.UTC)
			if !cs.month[int(w.Month())] || !cs.dayMatches(d) || !cs.hour[w.Hour()] || !cs.min[w.Minute()] {
				continue
			}
			if len(*list) < maxTimeNotes {
				*list = append(*list, w.Format("2006-01-02 15:04"))
			}
		}
	}
	return disappear, repeat
}
