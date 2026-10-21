// SPDX-License-Identifier: AGPL-3.0-only

package sim

import (
	"bytes"
	"context"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/HunterEwok/botaru/impl/internal/spec"
	"github.com/HunterEwok/botaru/impl/internal/testbench"
	"github.com/HunterEwok/botaru/impl/internal/yamlx"
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

func startStand(t *testing.T, maxMicros int64, farm *Farm) (*Stand, *syncBuffer) {
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
	s, err := Start(t.Context(), Config{Root: root, DSN: testbench.MigratedDSN(t), Work: world, Exe: exe, Farm: farm, MaxMicros: maxMicros, Seed: 42, Step: time.Second, Log: log})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s, console
}

// scenariosApart are the scripted scenarios TestScenarios plays on a second stand beside the
// others, so that the two halves take about the same time.
var scenariosApart = map[string]bool{"scaffold-evolution": true, "bot-messages": true, "follow-up-memory": true, "erasure": true, "translator-refuses": true}

// scenariosOnTenants are the scenarios the smoke farm can play that TestScenarios plays on the
// farm of tenants instead, where they reach the part a second tenant gives them.
var scenariosOnTenants = map[string]bool{"manager-work": true}

// tenantsFarm is a farm of three tenants for the scenarios of tenants the smoke farm cannot play:
// a home with every role on two database machines, a second tenant with the farm manager, and a
// tight-budget tenant without one.
const tenantsFarm = `name: tenants
tenants:
  - name: ops-dept
    hosts: [{id: web1, kind: web}, {id: db1, kind: db}, {id: db2, kind: db}, {id: ctd1, kind: ctd}]
    manager: true
  - name: lab-dept
    hosts: [{id: db3, kind: db}]
    manager: true
    replicas: {hello: 1, ops-reader: 1, pipeline-reader: 1, reviewer-a: 1, reviewer-b: 1, farm-manager: 1, farm-manager-reader: 1, data-job: 1}
  - name: small-dept
    hosts: [{id: web5, kind: web}, {id: db4, kind: db}]
    tight_budget: true
    replicas: {hello: 1, ops-reader: 1, reviewer-a: 1, reviewer-b: 1, data-job: 1}
`

// TestScenarios runs every scripted scenario on the mock model on stands of their own, the smoke
// farm's in two halves and the scenarios of several tenants on a farm of three: each check of the
// director passes, and nothing of the host was touched (the world's stubs answered every program).
// The five parts of TestScenarios and TestFarmFaults are one more than the four processors of a
// CI runner, so the short part of the tenants starts when the first other part ends.
func TestScenarios(t *testing.T) {
	t.Parallel()
	smoke, err := LoadFarm("smoke")
	if err != nil {
		t.Fatal(err)
	}
	var tenants Farm
	if err := yamlx.Unmarshal([]byte(tenantsFarm), &tenants); err != nil {
		t.Fatal(err)
	}
	if err := tenants.check(); err != nil {
		t.Fatal(err)
	}
	var halves [2][]string
	for _, n := range FitScenarios(smoke) {
		// The farm's own faults take minutes of fault time; TestFarmFaults runs them beside.
		if n == "farm-faults" || scenariosOnTenants[n] {
			continue
		}
		half := 0
		if scenariosApart[n] {
			half = 1
		}
		halves[half] = append(halves[half], n)
	}
	var several []string
	for _, n := range FitScenarios(&tenants) {
		if Find(n).Unfit(smoke) != "" || scenariosOnTenants[n] {
			several = append(several, n)
		}
	}
	// Every scenario is in a part, and every scenario a part names exists.
	played := map[string]bool{"farm-faults": true}
	for _, n := range slices.Concat(halves[0], halves[1], several) {
		played[n] = true
	}
	for _, sc := range Scenarios {
		if !played[sc.Name] {
			t.Errorf("scenario %s is in no part of the tests", sc.Name)
		}
	}
	for n := range maps.Keys(scenariosApart) {
		if Find(n) == nil {
			t.Errorf("scenariosApart names no scenario %s", n)
		}
	}
	if t.Failed() {
		return
	}
	parts := []struct {
		name  string
		farm  *Farm
		names []string
	}{{"half1", smoke, halves[0]}, {"half2", smoke, halves[1]}, {"tenants", &tenants, several}}
	for _, p := range parts {
		t.Run(p.name, func(t *testing.T) {
			t.Parallel()
			t.Logf("scenarios %v", p.names)
			playOnStand(t, p.farm, func(ctx context.Context, d *Director) error { return d.RunScenarios(ctx, p.names) })
		})
	}
}

// playOnStand plays scenarios on a stand of their own: each check of the director passes, the
// mock answered model calls, no fuse tripped, the invariant watch finds no breach, and the event
// file holds the model calls and the chat.
func playOnStand(t *testing.T, farm *Farm, play func(context.Context, *Director) error) {
	s, console := startStand(t, 5_000_000, farm)
	d := &Director{S: s, Approve: "safe", Timeout: 90 * time.Second, Seed: 42}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go d.HandleCards(ctx)
	if err := play(ctx, d); err != nil {
		t.Fatalf("%v\n%s", err, console)
	}
	res := d.Results()
	if len(res) < 4 {
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
	s, console := startStand(t, 1000, nil)
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

// TestFarmFaults runs the farm's own faults F-1 to F-8 on the mock model beside TestScenarios,
// the faults of a call and those of the core on stands of their own: every expected outcome
// holds and the invariant watch finds no breach.
func TestFarmFaults(t *testing.T) {
	t.Parallel()
	for _, part := range []struct {
		name string
		part faultPart
	}{{"calls", faultsCalls}, {"core", faultsCore}} {
		t.Run(part.name, func(t *testing.T) {
			t.Parallel()
			sc := *Find("farm-faults")
			sc.Run = func(ctx context.Context, d *Director, name string) error {
				return playFarmFaults(ctx, d, name, part.part)
			}
			playOnStand(t, nil, func(ctx context.Context, d *Director) error { return d.runScenario(ctx, &sc) })
		})
	}
}
