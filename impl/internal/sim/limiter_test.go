// SPDX-License-Identifier: AGPL-3.0-only

package sim

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// The limiter lets at most parallel calls run at once and holds the others until a slot is free.
func TestLimiterParallel(t *testing.T) {
	l := newLimiter(0, 2)
	var mu sync.Mutex
	running, most := 0, 0
	var wg sync.WaitGroup
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := l.acquire(context.Background(), nil); err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			running++
			most = max(most, running)
			mu.Unlock()
			time.Sleep(60 * time.Millisecond)
			mu.Lock()
			running--
			mu.Unlock()
			l.release()
		}()
	}
	wg.Wait()
	st := l.stats()
	if most != 2 || st.Calls != 6 || st.Held < 4 || st.MaxQueued < 4 {
		t.Fatalf("most %d at once, %+v", most, st)
	}
}

// Past the rate a call waits for the minute, and a call whose context ends gives up.
func TestLimiterRate(t *testing.T) {
	l := newLimiter(2, 0)
	for range 2 {
		if _, err := l.acquire(context.Background(), nil); err != nil {
			t.Fatal(err)
		}
		l.release()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := l.acquire(ctx, nil); err == nil {
		t.Fatal("a third call in the minute got through")
	}
	if newLimiter(0, 0) != nil {
		t.Fatal("no limits is no limiter")
	}
}

// The waiting calls start in the order they came, and a stopped run lets none through.
func TestLimiterOrder(t *testing.T) {
	l := newLimiter(0, 1)
	if _, err := l.acquire(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	var order []int
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := l.acquire(context.Background(), nil); err == nil {
				mu.Lock()
				order = append(order, i)
				mu.Unlock()
				time.Sleep(10 * time.Millisecond)
				l.release()
			}
		}()
		time.Sleep(20 * time.Millisecond)
	}
	l.release()
	wg.Wait()
	if !slices.Equal(order, []int{0, 1, 2}) {
		t.Fatalf("order %v", order)
	}
	if _, err := l.acquire(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := l.acquire(context.Background(), func() bool { return true }); !errors.Is(err, errLimiterStopped) {
		t.Fatalf("a stopped run: %v", err)
	}
	if len(l.queue) != 0 {
		t.Fatalf("the queue keeps %v", l.queue)
	}
}

// limitP4 holds the model calls and passes the other requests of P4.
func TestLimitP4(t *testing.T) {
	l := newLimiter(0, 1)
	var waited time.Duration
	h := limitP4(l, nil, func(w time.Duration) { waited = w })(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	if _, err := l.acquire(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	go func() { time.Sleep(100 * time.Millisecond); l.release() }()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/p4/v1/messages", nil))
	if rec.Code != http.StatusOK || waited < 50*time.Millisecond {
		t.Fatalf("status %d, waited %v", rec.Code, waited)
	}
	if st := l.stats(); st.Calls != 2 || st.Held != 1 {
		t.Fatalf("%+v", st)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/p4/v1/models", nil))
	if st := l.stats(); rec.Code != http.StatusOK || st.Calls != 2 {
		t.Fatalf("a request that is no model call: %d %+v", rec.Code, st)
	}
}

func TestPlanFile(t *testing.T) {
	key := PlanKey("standard", "", "standard", time.Hour, "")
	if got := PlanFile("/o", key); got != filepath.Join("/o", "plan-farm-standard-load-standard-duration-1h0m0s.json") {
		t.Fatal(got)
	}
	if PlanKey("smoke", "a, b", "", 0, "") != PlanKey("smoke", "a,b", "", 0, "") {
		t.Fatal("the key depends on the spaces of the list")
	}
	long := PlanKey("smoke", strings.Repeat("scenario-name,", 30), "", 0, "claude-haiku-5-5")
	if base := filepath.Base(PlanFile("/o", long)); len(base) > 120 {
		t.Fatalf("a long key gives the name %s", base)
	}
	p := Plan{Key: key, MaxEUR: 23.5, Ranks: []PlanRank{{Rank: 1, Model: "m"}}}
	path := PlanFile(t.TempDir(), key)
	if err := p.Save(path); err != nil {
		t.Fatal(err)
	}
	got, err := LoadPlan(path)
	if err != nil || got.MaxEUR != 23.5 || got.Key != key || len(got.Ranks) != 1 {
		t.Fatalf("%+v %v", got, err)
	}
}
