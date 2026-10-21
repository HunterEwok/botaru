// SPDX-License-Identifier: AGPL-3.0-only

package sim

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/HunterEwok/botaru/impl/internal/spec"
	"github.com/HunterEwok/botaru/impl/internal/testbench"
)

// syncBuffer is a console the test reads while the stand writes it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func startStand(t *testing.T, maxMicros int64) (*Stand, *syncBuffer) {
	t.Helper()
	root, err := spec.FindRoot("")
	if err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	console := &syncBuffer{}
	work := t.TempDir()
	log, err := NewLog(console, filepath.Join(work, "sim-events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })
	world := filepath.Join(work, "world")
	if err := os.Mkdir(world, 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := Start(t.Context(), Config{Root: root, DSN: testbench.MigratedDSN(t), Work: world, Exe: exe, MaxMicros: maxMicros, Seed: 42, Step: time.Second, Log: log})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s, console
}

// TestScenarios runs every scripted scenario on the mock model: each check of the director
// passes, and nothing of the host was touched (the world's stubs answered every program).
func TestScenarios(t *testing.T) {
	t.Parallel()
	s, console := startStand(t, 5_000_000)
	d := &Director{S: s, Approve: "safe", Timeout: 90 * time.Second, Seed: 42}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go d.HandleCards(ctx)
	var names []string
	for _, sc := range Scenarios {
		// The farm's own faults take minutes of fault time; TestFarmFaults runs them beside.
		if sc.Name != "farm-faults" {
			names = append(names, sc.Name)
		}
	}
	if err := d.RunScenarios(ctx, names); err != nil {
		t.Fatalf("%v\n%s", err, console)
	}
	res := d.Results()
	if len(res) < 12 {
		t.Fatalf("only %d checks\n%s", len(res), console)
	}
	for _, r := range res {
		if !r.OK && r.Gap == "" {
			t.Errorf("%s %s: %s", r.Scenario, r.Check, r.Detail)
		}
	}
	if t.Failed() {
		t.Log(console)
	}
	if s.Brain.Calls() == 0 || s.Tripped() {
		t.Fatalf("model calls %d, fuse %v", s.Brain.Calls(), s.Tripped())
	}
	if err := s.FinalWatch(ctx); err != nil {
		t.Fatal(err)
	}
	if v := s.Violations(); len(v) > 0 {
		t.Fatalf("breaches of the invariants: %+v", v)
	}
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(s.Work()), "sim-events.jsonl"))
	if err != nil || !strings.Contains(string(raw), `"kind":"P4 MODEL"`) || !strings.Contains(string(raw), `"kind":"BOT -> CHAT"`) {
		t.Fatalf("the event file: %v", err)
	}
}

// TestFuse: a cap below the first model call holds every tenant, and no task is claimed after.
func TestFuse(t *testing.T) {
	s, console := startStand(t, 1000)
	if _, err := s.Say("bob", "bob-dm", "/logs The site does not open, is it down?"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.Stopped():
	case <-time.After(60 * time.Second):
		t.Fatalf("the fuse did not trip\n%s", console)
	}
	if err := s.CheckFuse(t.Context()); err != nil {
		t.Fatal(err)
	}
	var holds int
	if err := s.Bench.Store().Pool.QueryRow(t.Context(), `SELECT count(*) FROM holds WHERE reason = 'budget_exhausted' AND state = 'active'`).Scan(&holds); err != nil {
		t.Fatal(err)
	}
	if holds != len(s.Bench.Seed().Tenants) {
		t.Fatalf("holds %d for %d tenants\n%s", holds, len(s.Bench.Seed().Tenants), console)
	}
	calls := s.Brain.Calls()
	if _, err := s.Say("alice", "alice-dm", "/logs Why was the site slow?"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Second)
	if s.Brain.Calls() != calls {
		t.Fatalf("model calls after the fuse: %d -> %d\n%s", calls, s.Brain.Calls(), console)
	}
}

// TestFarmFaults runs the farm's own faults F-1 to F-8 on the mock model, beside TestScenarios:
// every expected outcome holds and the invariant watch finds no breach.
func TestFarmFaults(t *testing.T) {
	t.Parallel()
	s, console := startStand(t, 5_000_000)
	d := &Director{S: s, Approve: "safe", Timeout: 90 * time.Second, Seed: 42}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go d.HandleCards(ctx)
	if err := d.RunScenarios(ctx, []string{"farm-faults"}); err != nil {
		t.Fatalf("%v\n%s", err, console)
	}
	for _, r := range d.Results() {
		if !r.OK && r.Gap == "" {
			t.Errorf("%s %s: %s", r.Scenario, r.Check, r.Detail)
		}
	}
	if err := s.FinalWatch(ctx); err != nil {
		t.Fatal(err)
	}
	if v := s.Violations(); len(v) > 0 {
		t.Errorf("breaches of the invariants: %+v", v)
	}
	if t.Failed() {
		t.Log(console)
	}
}
