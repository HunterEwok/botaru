// SPDX-License-Identifier: AGPL-3.0-only

package sim

import (
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestM1(t *testing.T) {
	var tasks []peopleTask
	for i := range 20 {
		state := "completed"
		if i < 5 {
			state = "failed"
		}
		tasks = append(tasks, peopleTask{typ: "a", state: state})
	}
	// A refusal of a rule and an open task do not count.
	tasks = append(tasks, peopleTask{typ: "a", state: "failed", reason: "budget_exhausted"}, peopleTask{typ: "a", state: "working"})
	for range 80 {
		tasks = append(tasks, peopleTask{typ: "b", state: "completed"})
	}
	m := m1(tasks)
	// 95 of 100 in all, but a: 15 of 20 is below 80%.
	if m.Status != "fail" || !strings.Contains(m.Value, "95 of 100") || !strings.Contains(m.Detail, "a 75.0% of 20") {
		t.Fatalf("%+v", m)
	}
	if m := m1(tasks[5:]); m.Status != "pass" {
		t.Fatalf("%+v", m)
	}
	if m := m1(nil); m.Status != "n/a" {
		t.Fatalf("%+v", m)
	}
}

func TestM2Peaks(t *testing.T) {
	start := time.Now()
	in := metricsInput{load: &LoadProfile{Peaks: []LoadPeak{{At: time.Minute, For: time.Minute, Factor: 3}}}, loadStart: start}
	lease := func(t time.Time, wait time.Duration) *time.Time { l := t.Add(wait); return &l }
	calm, peak := start.Add(10*time.Second), start.Add(90*time.Second)
	tasks := []peopleTask{
		{created: calm, firstLease: lease(calm, time.Minute)},
		{created: peak, firstLease: lease(peak, 7*time.Minute)},
		{created: calm, state: "completed"},
	}
	in.now = start.Add(3 * time.Minute)
	if m := m2(tasks, in); m.Status != "pass" || !strings.Contains(m.Value, "60.0 s of 1") || !strings.Contains(m.Value, "in peaks 420.0 s of 1") {
		t.Fatalf("%+v", m)
	}
	tasks[1].firstLease = lease(peak, 9*time.Minute)
	if m := m2(tasks, in); m.Status != "fail" {
		t.Fatalf("%+v", m)
	}
	// A task not taken counts with its wait so far.
	tasks[1].firstLease = lease(peak, time.Minute)
	in.now = start.Add(20 * time.Minute)
	tasks = append(tasks, peopleTask{created: calm, state: "submitted"})
	if m := m2(tasks, in); m.Status != "fail" || m.Detail == "" {
		t.Fatalf("a starving queue: %+v", m)
	}
}

func TestGrowth(t *testing.T) {
	start := time.Now()
	var ss []procSample
	for i := range 40 {
		ss = append(ss, procSample{At: start.Add(time.Duration(i) * 30 * time.Second), RSSMB: 100 + float64(i), Goroutines: 200})
	}
	in := metricsInput{start: start, samples: ss}
	// From minute 10 to 19.5: 20 samples, RSS 120..139.
	m := m12(in)
	if m.Status != "pass" || !strings.Contains(m.Value, "goroutines 0.0%") {
		t.Fatalf("%+v", m)
	}
	for i := range ss {
		if i >= 30 {
			ss[i].Goroutines = 300
		}
	}
	if m := m12(in); m.Status != "fail" {
		t.Fatalf("%+v", m)
	}
	if m := m12(metricsInput{start: start, samples: ss[:22]}); m.Status != "n/a" {
		t.Fatalf("a short run: %+v", m)
	}
}

func TestM13(t *testing.T) {
	// A known gap is recorded, not failed.
	if m := m13([]Result{{OK: true}, {OK: false, Gap: "x"}}); m.Status != "pass" || m.Detail == "" {
		t.Fatalf("%+v", m)
	}
	if m := m13([]Result{{OK: true}, {OK: false}}); m.Status != "fail" {
		t.Fatalf("%+v", m)
	}
	if m := m13([]Result{{OK: true}}); m.Status != "pass" {
		t.Fatalf("%+v", m)
	}
}

// M-11 fails when connections beyond the core's pool grow.
func TestM11(t *testing.T) {
	start := time.Now()
	ss := []procSample{{At: start, PoolConns: 4, PoolMax: 4, DBConns: 5}, {At: start.Add(time.Minute), PoolConns: 4, PoolMax: 4, DBConns: 7}}
	if m := m11(metricsInput{start: start, samples: ss}); m.Status != "pass" {
		t.Fatalf("%+v", m)
	}
	ss = append(ss, procSample{At: start.Add(2 * time.Minute), PoolConns: 2, PoolMax: 4, DBConns: 9})
	if m := m11(metricsInput{start: start, samples: ss}); m.Status != "fail" {
		t.Fatalf("a leak: %+v", m)
	}
}

func TestTimes(t *testing.T) {
	var ds []time.Duration
	for i := 1; i <= 100; i++ {
		ds = append(ds, time.Duration(i)*time.Second)
	}
	if got := times(ds); got.N != 100 || got.P50 != 50 || got.P95 != 95 {
		t.Fatalf("%+v", got)
	}
}

func TestReportMD(t *testing.T) {
	m := &Metrics{At: time.Now(), Elapsed: "1m", Thresholds: []Metric{{Code: "M-1", Name: "a | b", Value: "1", Threshold: "2", Status: "pass"}},
		Spend: Spend{Rows: []SpendRow{{Tenant: "t", Rank: 1, StandEUR: "0.1000"}}, StandEUR: "0.1000"}}
	md := reportMD(m, []Result{{Scenario: "s", Check: "c", Gap: "g"}}, nil)
	for _, want := range []string{"| M-1 | a \\| b | 1 | 2 | pass |", "| t | 1 |", "| total | | | | | | 0.1000 |", "- GAP s c"} {
		if !strings.Contains(md, want) {
			t.Fatalf("no %q in\n%s", want, md)
		}
	}
}

func TestGzipAtomic(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "e.jsonl"), filepath.Join(dir, "out", "e.jsonl.gz")
	if err := os.WriteFile(src, []byte("{\"a\":1}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := gzipAtomic(src, dst); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(zr)
	if err != nil || string(raw) != "{\"a\":1}\n" {
		t.Fatalf("%q %v", raw, err)
	}
	if _, err := os.Stat(dst + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("the temporary file stays: %v", err)
	}
}
