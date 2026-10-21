// SPDX-License-Identifier: AGPL-3.0-only

package sim

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"
)

// limiter holds the model calls of the stand to a rate per minute and a number in flight (the
// load test brief, D-11): a run then stays inside the provider's limits however the load is drawn,
// and the wait it adds is measured apart from the farm's own.
//
// It stands before P4, on the agents' way to the gateway: a waiting call has no reserve yet and
// P4's deadline of the call has not begun, so waiting makes a call slower and costs nothing, and
// the provider never sees more than the limits. The calls start in the order they came; a call
// waits until a slot is free, its agent gives up (its client's timeout), the run stops or it has
// waited maxWait.
type limiter struct {
	perMin, parallel int
	// maxWait ends a wait that would leave the call too little of the agent's 10 minutes: a call
	// that starts after it may be charged and then lost to the agent's timeout.
	maxWait time.Duration

	mu       sync.Mutex
	starts   []time.Time
	inFlight int
	// queue holds the tickets of the waiting calls in the order they came: only the first may
	// start.
	queue  []uint64
	ticket uint64
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

// errLimiterStopped is the end of a wait when the run stopped.
var errLimiterStopped = errors.New("the run stopped while the call waited")

// errLimiterTooLong is the end of a wait of maxWait.
var errLimiterTooLong = errors.New("the call waited too long for a slot")

// limiterMaxWait is the longest wait: the agent's client gives a call 10 minutes, and P4's own
// deadline of a call is a minute or more.
const limiterMaxWait = 8 * time.Minute

func newLimiter(perMin, parallel int) *limiter {
	if perMin <= 0 && parallel <= 0 {
		return nil
	}
	return &limiter{perMin: perMin, parallel: parallel, maxWait: limiterMaxWait}
}

// free says whether a call may start now; l.mu is held.
func (l *limiter) free(now time.Time) bool {
	cut := 0
	for cut < len(l.starts) && now.Sub(l.starts[cut]) >= time.Minute {
		cut++
	}
	l.starts = l.starts[cut:]
	return (l.parallel <= 0 || l.inFlight < l.parallel) && (l.perMin <= 0 || len(l.starts) < l.perMin)
}

// start takes a slot for a call that waited wait; l.mu is held.
func (l *limiter) start(now time.Time, wait time.Duration) {
	l.inFlight++
	l.starts = append(l.starts, now)
	l.calls++
	if wait >= time.Millisecond {
		l.held++
		l.waits = append(l.waits, wait)
	}
}

// leave takes a ticket out of the queue; l.mu is held.
func (l *limiter) leave(t uint64) {
	for i, q := range l.queue {
		if q == t {
			l.queue = append(l.queue[:i], l.queue[i+1:]...)
			return
		}
	}
}

// acquire waits until a call may start, in the order the calls came, and returns the wait; it
// gives up with errLimiterStopped when stop says the run stopped, with errLimiterTooLong after
// maxWait, and with the context's error.
// A call that acquired must release.
func (l *limiter) acquire(ctx context.Context, stop func() bool) (time.Duration, error) {
	begin := time.Now()
	l.mu.Lock()
	if len(l.queue) == 0 && l.free(begin) {
		l.start(begin, 0)
		l.mu.Unlock()
		return 0, nil
	}
	l.ticket++
	t := l.ticket
	l.queue = append(l.queue, t)
	l.maxQueued = max(l.maxQueued, len(l.queue))
	l.mu.Unlock()
	tick := time.NewTicker(limiterPoll)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			l.mu.Lock()
			l.leave(t)
			l.mu.Unlock()
			return time.Since(begin), ctx.Err()
		case <-tick.C:
		}
		now := time.Now()
		l.mu.Lock()
		switch {
		case stop != nil && stop():
			l.leave(t)
			l.mu.Unlock()
			return now.Sub(begin), errLimiterStopped
		case l.maxWait > 0 && now.Sub(begin) >= l.maxWait:
			l.leave(t)
			l.mu.Unlock()
			return now.Sub(begin), errLimiterTooLong
		case l.queue[0] == t && l.free(now):
			l.queue = l.queue[1:]
			l.start(now, now.Sub(begin))
			l.mu.Unlock()
			return now.Sub(begin), nil
		}
		l.mu.Unlock()
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

// limitP4 holds the model calls of the agents to the limits before P4 sees them; held sees
// each call that waited. Other requests of P4 pass.
func limitP4(l *limiter, stop func() bool, held func(time.Duration)) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/messages") {
				next.ServeHTTP(w, r)
				return
			}
			wait, err := l.acquire(r.Context(), stop)
			switch {
			case errors.Is(err, errLimiterStopped):
				writeHalted(w)
				return
			case errors.Is(err, errLimiterTooLong):
				http.Error(w, err.Error(), http.StatusServiceUnavailable)
				return
			case err != nil:
				// The agent gave up on the call while it waited.
				http.Error(w, "the call was abandoned", http.StatusServiceUnavailable)
				return
			}
			defer l.release()
			if wait >= time.Millisecond && held != nil {
				held(wait)
			}
			next.ServeHTTP(w, r)
		})
	}
}
