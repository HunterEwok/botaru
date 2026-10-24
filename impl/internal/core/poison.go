// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// skipFor is how long a drain loop skips a row it failed on while the store answered.
const skipFor = time.Minute

// skipList holds the rows the drain loops (reaper, deadline, stall) skip for a while: a loop
// takes the oldest due row first, so one row that keeps failing would otherwise stop every
// lease and deadline behind it, farm-wide (OBS-8).
type skipList struct {
	mu    sync.Mutex
	until map[string]time.Time
}

// ids are the skipped ids of a loop at now.
func (s *skipList) ids(loop string, now time.Time) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []string{}
	for k, u := range s.until {
		if now.After(u) {
			delete(s.until, k)
			continue
		}
		if l, id, ok := cutKey(k); ok && l == loop {
			out = append(out, id)
		}
	}
	return out
}

func (s *skipList) add(loop, id string, until time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.until == nil {
		s.until = map[string]time.Time{}
	}
	s.until[loop+"\x00"+id] = until
}

func cutKey(k string) (string, string, bool) {
	for i := 0; i < len(k); i++ {
		if k[i] == 0 {
			return k[:i], k[i+1:], true
		}
	}
	return "", "", false
}

// skipRow decides what a drain loop does with an error on one row: while the store answers,
// the row is skipped for skipFor and the loop goes on (true); when the store does not answer,
// the error ends the pass (false).
func (c *Core) skipRow(ctx context.Context, loop, id string, err error) bool {
	if id == "" || ctx.Err() != nil || transient(err) || c.st.Ping(ctx) != nil {
		return false
	}
	c.log.Error("a background loop failed on one row; it skips the row for a while", "loop", loop, "id", id, "err", err)
	c.skipped.add(loop, id, time.Now().Add(skipFor))
	return true
}

// skippedErr is the error of a pass while the loop skips a failing row: the loop does not mark
// its tick as long as a row stays on its skip list, so health reports it once the tick grows old
// (OBS-4, OBS-9), while the other rows go on. also names the keys of loops that run inside it.
func (c *Core) skippedErr(loop string, skipped bool, also ...string) error {
	now := time.Now()
	n := len(c.skipped.ids(loop, now))
	for _, l := range also {
		n += len(c.skipped.ids(l, now))
	}
	if !skipped && n == 0 {
		return nil
	}
	return fmt.Errorf("%s: a row failed and is skipped for %s (see the log)", loop, skipFor)
}

// transient says whether an error is the store's, not the row's: a lost connection, a
// serialization failure or a deadlock end the pass and are retried by the next one.
func transient(err error) bool {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return strings.HasPrefix(pe.Code, "08") || strings.HasPrefix(pe.Code, "40") || strings.HasPrefix(pe.Code, "57P")
	}
	return false
}
