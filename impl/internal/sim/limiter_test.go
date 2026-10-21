// SPDX-License-Identifier: AGPL-3.0-only

package sim

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// The limiter lets at most parallel calls run at once and holds the others.
func TestLimiterParallel(t *testing.T) {
	l := newLimiter(0, 2)
	var mu sync.Mutex
	running, most := 0, 0
	var wg sync.WaitGroup
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := l.acquire(context.Background()); err != nil {
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
		if _, err := l.acquire(context.Background()); err != nil {
			t.Fatal(err)
		}
		l.release()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := l.acquire(ctx); err == nil {
		t.Fatal("a third call in the minute got through")
	}
	if newLimiter(0, 0) != nil {
		t.Fatal("no limits is no limiter")
	}
}

func TestPlanFile(t *testing.T) {
	key := PlanKey("standard", "", "standard", time.Hour)
	if got := PlanFile("/o", key); got != filepath.Join("/o", "plan-farm-standard-load-standard-duration-1h0m0s.json") {
		t.Fatal(got)
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
