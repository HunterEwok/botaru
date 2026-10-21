// SPDX-License-Identifier: AGPL-3.0-only

package sim

import (
	"context"
	"math/rand/v2"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/HunterEwok/botaru/impl/internal/plugins"
)

// Every closed field of item_history has points on every machine of the world, through the
// monitor plugin with its standard item keys, for the history the world starts with and for
// the steps after it.
func TestMonitorItemHistory(t *testing.T) {
	work := t.TempDir()
	now := time.Now().UTC()
	if err := InitFiles(work, 7, now.Add(-2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := Step(work, now.Add(-time.Minute), rand.New(rand.NewPCG(7, 2))); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(&Monitor{Work: work})
	defer srv.Close()
	m := &plugins.Monitor{API: srv.URL, Token: "t", Hosts: map[string]string{}, Items: plugins.DefaultMonitorItems}
	for _, h := range NewWorld(now).HostNames() {
		m.Hosts[h] = h + ".example.org"
	}
	for host := range m.Hosts {
		for field := range plugins.DefaultMonitorItems {
			for _, step := range []string{"1m", "5m"} {
				r, err := m.Call(context.Background(), "item_history", map[string]any{"host": host, "item": field, "step": step}, nil)
				if err != nil {
					t.Fatalf("%s %s %s: %v", host, field, step, err)
				}
				pts, _ := r.StructuredContent["points"].([]any)
				if r.IsError || len(pts) == 0 {
					t.Errorf("%s %s %s: no points: %+v", host, field, step, r)
				}
			}
		}
	}
}
