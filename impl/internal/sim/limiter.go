// SPDX-License-Identifier: AGPL-3.0-only

package sim

import (
	"context"
	"sync"
	"time"
)

// limiter holds the model calls of the stand to a rate per minute and a number in flight, before
// they reach the provider (the load test brief, D-11): a run then stays inside the provider's
// limits however the load is drawn, and the wait it adds is measured apart from the farm's own.
type limiter struct {
	perMin, parallel int

	mu       sync.Mutex
	starts   []time.Time
	inFlight int
	queued   int
	// The waits of every call, for the metrics.
	calls, held, maxQueued int
	waits                  []time.Duration
}

// LimiterStats are the waits of the model calls in the limiter.
type LimiterStats struct {
	PerMinute int `json:"per_minute"`
	Parallel  int `json:"parallel"`
	Calls     int `json:"calls"`
	// Held are the calls that waited; Wait the times they waited; MaxQueued the most calls that
	// waited at once.
	Held      int     `json:"held"`
	Wait      Times   `json:"wait"`
	TotalS    float64 `json:"total_s"`
	MaxQueued int     `json:"max_queued"`
}

// limiterPoll is how often a waiting call looks again.
const limiterPoll = 50 * time.Millisecond

func newLimiter(perMin, parallel int) *limiter {
	if perMin <= 0 && parallel <= 0 {
		return nil
	}
	return &limiter{perMin: perMin, parallel: parallel}
}

// acquire waits until a call may start, or the context ends; it returns the wait. A call that
// acquired must release.
func (l *limiter) acquire(ctx context.Context) (time.Duration, error) {
	begin := time.Now()
	l.mu.Lock()
	l.queued++
	l.maxQueued = max(l.maxQueued, l.queued)
	l.mu.Unlock()
	defer func() {
		l.mu.Lock()
		l.queued--
		l.mu.Unlock()
	}()
	for {
		now := time.Now()
		l.mu.Lock()
		cut := 0
		for cut < len(l.starts) && now.Sub(l.starts[cut]) >= time.Minute {
			cut++
		}
		l.starts = l.starts[cut:]
		if (l.parallel <= 0 || l.inFlight < l.parallel) && (l.perMin <= 0 || len(l.starts) < l.perMin) {
			l.inFlight++
			l.starts = append(l.starts, now)
			wait := now.Sub(begin)
			l.calls++
			if wait >= time.Millisecond {
				l.held++
				l.waits = append(l.waits, wait)
			}
			l.mu.Unlock()
			return wait, nil
		}
		l.mu.Unlock()
		select {
		case <-ctx.Done():
			return time.Since(begin), ctx.Err()
		case <-time.After(limiterPoll):
		}
	}
}

func (l *limiter) release() {
	l.mu.Lock()
	l.inFlight--
	l.mu.Unlock()
}

// stats are the waits so far.
func (l *limiter) stats() LimiterStats {
	l.mu.Lock()
	defer l.mu.Unlock()
	st := LimiterStats{PerMinute: l.perMin, Parallel: l.parallel, Calls: l.calls, Held: l.held, Wait: times(l.waits), MaxQueued: l.maxQueued}
	for _, w := range l.waits {
		st.TotalS += w.Seconds()
	}
	return st
}
