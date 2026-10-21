// SPDX-License-Identifier: AGPL-3.0-only

package sim

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// RunInfo is what results.json says of the run besides its checks.
type RunInfo struct {
	Mode    string  `json:"mode"`
	Model   string  `json:"model,omitempty"`
	MaxEUR  float64 `json:"max_eur"`
	Approve string  `json:"auto_approve"`
}

// Reporter writes the results of a run while it runs: results.json, metrics.json and report.md
// in the run's folder every 5 minutes, after each scenario and at the end, and copies them with
// the compressed events to an export folder, so a run killed midway leaves a result at most 5
// minutes old (the load test brief, D-9 and D-13). It samples the process every 30 seconds.
type Reporter struct {
	S *Stand
	D *Director
	// Dir is the run's folder; Export, when set, the folder the results are copied to, in a
	// folder of the run's name.
	Dir    string
	Export string
	Info   RunInfo

	mu        sync.Mutex
	start     time.Time
	samples   []procSample
	cpu       cpuTimes
	load      *LoadProfile
	loadStart time.Time
	loadSum   *LoadSummary
	// budgetWaiting are the tasks seen waiting with budget_exhausted, by tenant (M-6).
	budgetWaiting map[string]map[string]bool
	// writeMu keeps one write at a time.
	writeMu sync.Mutex
}

// Sample and write periods.
const (
	samplePeriod = 30 * time.Second
	writePeriod  = 5 * time.Minute
)

// maxProcSamples bounds the samples kept: a week of samples every 30 seconds.
const maxProcSamples = 7 * 24 * 120

// NewReporter starts a reporter for a stand and its director.
func NewReporter(s *Stand, d *Director, dir, export string, info RunInfo) *Reporter {
	r := &Reporter{S: s, D: d, Dir: dir, Export: export, Info: info, start: time.Now(), cpu: readCPU(), budgetWaiting: map[string]map[string]bool{}}
	return r
}

// Run samples the process and writes the results every period until ctx ends.
func (r *Reporter) Run(ctx context.Context) {
	r.sample(ctx)
	st, wt := time.NewTicker(samplePeriod), time.NewTicker(writePeriod)
	defer st.Stop()
	defer wt.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-st.C:
			r.sample(ctx)
		case <-wt.C:
			if _, err := r.Write(ctx, false); err != nil && ctx.Err() == nil {
				r.S.log.Addf(KindDirector, "the results could not be written: %v", err)
			}
		}
	}
}

// sample takes one sample of the process and notes the tasks waiting for money.
func (r *Reporter) sample(ctx context.Context) {
	sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	r.mu.Lock()
	prev := r.cpu
	r.mu.Unlock()
	sm, now := r.S.takeSample(sctx, prev)
	waiting := map[string][]string{}
	if rows, err := r.S.Bench.Store().Pool.Query(sctx, `SELECT tenant, id FROM tasks WHERE state = 'submitted' AND reason = 'budget_exhausted'`); err == nil {
		for rows.Next() {
			var tenant, id string
			if rows.Scan(&tenant, &id) == nil {
				waiting[tenant] = append(waiting[tenant], id)
			}
		}
		rows.Close()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cpu = now
	r.samples = append(r.samples, sm)
	if len(r.samples) > maxProcSamples {
		r.samples = r.samples[len(r.samples)-maxProcSamples:]
	}
	for tenant, ids := range waiting {
		if r.budgetWaiting[tenant] == nil {
			r.budgetWaiting[tenant] = map[string]bool{}
		}
		for _, id := range ids {
			r.budgetWaiting[tenant][id] = true
		}
	}
}

// SetLoad notes the load profile of the run and when it began (the peaks of M-2, the warm-up of
// M-12).
func (r *Reporter) SetLoad(p *LoadProfile, start time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.load, r.loadStart = p, start
}

// SetLoadSummary notes what the load started and skipped, written to load.json.
func (r *Reporter) SetLoadSummary(l *LoadSummary) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.loadSum = l
}

// AfterScenario writes the results after a scenario.
func (r *Reporter) AfterScenario(ctx context.Context, name string) {
	if _, err := r.Write(ctx, false); err != nil && ctx.Err() == nil {
		r.S.log.Addf(KindDirector, "the results after %s could not be written: %v", name, err)
	}
}

// Write writes results.json, metrics.json, report.md (and load.json once the load ended) in the
// run's folder and copies them with the compressed events to the export folder. The spend of
// results.json is the stand column of metrics.json, read in the same pass. When the metrics
// cannot be read (the final pass tries twice), results.json is written still, with the spend
// the fuse read and the error, and Write returns the error with nil metrics. A file that cannot be
// written is returned as an error with the metrics read. A periodic write the end of the run cut
// short writes nothing.
func (r *Reporter) Write(ctx context.Context, final bool) (*Metrics, error) {
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	if final {
		r.sample(ctx)
	}
	r.mu.Lock()
	in := metricsInput{start: r.start, loadStart: r.loadStart, load: r.load, final: final, samples: append([]procSample(nil), r.samples...),
		budgetWaiting: map[string]map[string]bool{}}
	for t, ids := range r.budgetWaiting {
		// sample adds to the inner maps under r.mu while the metrics read them.
		in.budgetWaiting[t] = maps.Clone(ids)
	}
	loadSum := r.loadSum
	r.mu.Unlock()
	in.results = r.D.Results()
	m, merr := r.S.computeMetrics(ctx, in)
	if merr != nil && final && ctx.Err() == nil {
		m, merr = r.S.computeMetrics(ctx, in)
	}
	if merr != nil && !final && ctx.Err() != nil {
		// A periodic write cut short by the end of the run leaves the last good files.
		return nil, merr
	}
	spent := EUR(r.S.Spent())
	if m != nil {
		spent = m.Spend.StandEUR
	} else if n, err := r.S.Bench.SpendTotal(ctx); err == nil {
		spent = EUR(n)
	}
	results := map[string]any{"mode": r.Info.Mode, "model": r.Info.Model, "spent_eur": spent, "max_eur": r.Info.MaxEUR,
		"fuse_tripped": r.S.Tripped(), "halted": r.S.Halted(), "auto_approve": r.Info.Approve, "final": final, "checks": in.results,
		"violations": r.S.Violations()}
	if merr != nil {
		results["metrics_error"] = merr.Error()
	}
	files := map[string][]byte{}
	docs := map[string]any{"results.json": results}
	if m != nil {
		docs["metrics.json"] = m
		files["report.md"] = []byte(reportMD(m, in.results, r.S.Violations()))
	} else {
		files["report.md"] = []byte(fmt.Sprintf("# botaru-sim run report\n\nThe metrics could not be read at %s: %s\n", time.Now().UTC().Format(time.RFC3339), merr))
	}
	if loadSum != nil {
		docs["load.json"] = loadSum
	}
	for name, v := range docs {
		raw, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return nil, err
		}
		files[name] = append(raw, '\n')
	}
	// metrics.json first and results.json last, so a results.json is never newer than the
	// metrics beside it.
	order := []string{"metrics.json", "load.json", "report.md", "results.json"}
	put := func(dir string) error {
		for _, name := range order {
			if raw, ok := files[name]; ok {
				if err := writeShared(filepath.Join(dir, name), raw); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := put(r.Dir); err != nil {
		return m, errors.Join(merr, err)
	}
	if r.Export != "" {
		dst := filepath.Join(r.Export, filepath.Base(r.Dir))
		if err := os.MkdirAll(dst, 0o755); err != nil {
			return m, errors.Join(merr, err)
		}
		if err := put(dst); err != nil {
			return m, errors.Join(merr, err)
		}
		if err := gzipAtomic(filepath.Join(r.Dir, "sim-events.jsonl"), filepath.Join(dst, "sim-events.jsonl.gz")); err != nil {
			return m, errors.Join(merr, err)
		}
	}
	return m, merr
}

// writeShared writes a file others read through a temporary file, readable by all before the
// rename, and the rename (a mount without modes keeps its own).
func writeShared(path string, raw []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	_ = tmp.Chmod(0o644)
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// gzipAtomic compresses src into dst through a temporary file and a rename; the events are
// appended meanwhile, so the copy ends at some line of the moment.
func gzipAtomic(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	zw := gzip.NewWriter(out)
	_, err = io.Copy(zw, in)
	if cerr := zw.Close(); err == nil {
		err = cerr
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

// mdCell makes a text safe in a table cell.
func mdCell(s string) string {
	return strings.NewReplacer("|", "\\|", "\n", " ").Replace(s)
}

// reportMD is report.md: the metrics against their thresholds, the spend at both prices, the
// observed values and the checks that did not pass.
func reportMD(m *Metrics, results []Result, viol []Violation) string {
	var b strings.Builder
	state := "running"
	if m.Final {
		state = "final"
	}
	fmt.Fprintf(&b, "# botaru-sim run report\n\n%s, %s after the start (%s)", m.At.Format(time.RFC3339), m.Elapsed, state)
	if m.Load != "" {
		fmt.Fprintf(&b, ", load profile %s", m.Load)
	}
	b.WriteString(".\n\n## Metrics against thresholds\n\n| Code | Metric | Value | Threshold | Status | Detail |\n|---|---|---|---|---|---|\n")
	for _, t := range m.Thresholds {
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s |\n", t.Code, mdCell(t.Name), mdCell(t.Value), mdCell(t.Threshold), t.Status, mdCell(t.Detail))
	}
	fmt.Fprintf(&b, "\nInvariant breaches: %d.\n", len(viol))
	for _, v := range viol {
		fmt.Fprintf(&b, "- %s %s\n", v.Code, mdCell(v.What))
	}
	fmt.Fprintf(&b, "\n## Spend\n\nThe stand price is what the core charged at the prices of the profiles (the fuse and results.json read it); "+
		"the provider price is the tokens at the provider's price of the model, so it leaves out the costs of tools (rank 0). Over %.2f h.\n\n", m.Spend.Hours)
	b.WriteString("| Tenant | Rank | Model | Calls | Input tokens | Output tokens | Stand EUR | Provider EUR | Stand EUR/h | Provider EUR/h |\n|---|---|---|---|---|---|---|---|---|---|\n")
	for _, r := range m.Spend.Rows {
		fmt.Fprintf(&b, "| %s | %d | %s | %d | %d | %d | %s | %s | %s | %s |\n", r.Tenant, r.Rank, r.Model, r.Calls, r.InputTokens, r.OutputTokens,
			r.StandEUR, r.ProviderEUR, r.StandPerHour, r.ProviderPerHour)
	}
	fmt.Fprintf(&b, "| total | | | | | | %s | %s | | |\n", m.Spend.StandEUR, m.Spend.ProviderEUR)
	o := m.Observed
	b.WriteString("\n## Observed\n\n| Kind of work | Answers | Median s | 95th percentile s |\n|---|---|---|---|\n")
	for _, t := range o.AnswerTime {
		fmt.Fprintf(&b, "| %s | %d | %.1f | %.1f |\n", t.Type, t.N, t.P50, t.P95)
	}
	fmt.Fprintf(&b, "\n- Tasks with a second attempt: %s.\n", pct(o.SecondAttemptShare))
	fmt.Fprintf(&b, "- Refusals of a rule: %s.\n", countsLine(o.RefusedByRule))
	fmt.Fprintf(&b, "- Provider answers by status: %s; 429 among them %s. Errors the stand put in: %s.\n", countsLine(o.ProviderAnswers), pct(o.Share429), countsLine(o.Injected))
	fmt.Fprintf(&b, "- Incident opening: %d, median %.0f s, 95th percentile %.0f s.\n", o.IncidentOpening.N, o.IncidentOpening.P50, o.IncidentOpening.P95)
	fmt.Fprintf(&b, "- People with messages: %d; messages per person %.1f, cards per person %.1f (most %d), cards put off %d.\n",
		o.People, o.MessagesPerPerson, o.CardsPerPerson, o.MaxCardsPerPerson, o.PacedCards)
	fmt.Fprintf(&b, "- Approvals with cards: %d, decided before they expired %s.\n", o.Approvals, pct(o.DecidedInTime))
	fmt.Fprintf(&b, "- Processor: the stand process %.0f%% on average, %.0f%% at most (100%% is one processor); the host %.0f%% on average, %.0f%% at most.\n",
		o.CPU.ProcessMean, o.CPU.ProcessMax, o.CPU.HostMean, o.CPU.HostMax)
	if l := o.Limiter; l != nil {
		fmt.Fprintf(&b, "- The stand's limiter (%d calls a minute, %d at once): %d calls, %d held, %.0f s in all, median %.1f s, 95th percentile %.1f s; at most %d waiting (before P4, which reserves no money for a waiting call).\n",
			l.PerMinute, l.Parallel, l.Calls, l.Held, l.TotalS, l.Wait.P50, l.Wait.P95, l.MaxQueued)
	}
	p := m.Process
	fmt.Fprintf(&b, "- Process: %d samples; memory %.0f MB resident, %.0f MB heap; %d goroutines; store connections %d now, pool peak %d of %d, database peak %d.\n",
		p.Samples, p.RSSMB, p.HeapMB, p.Goroutines, p.DBConns, p.PoolPeak, p.PoolMax, p.DBPeak)
	b.WriteString("\nNot measured on the stand: the outcome of the trial of an accepted change (better or worse, a window of 14 days) and the incidents that could have been prevented.\n")
	var notPassed []Result
	for _, r := range results {
		if !r.OK {
			notPassed = append(notPassed, r)
		}
	}
	fmt.Fprintf(&b, "\n## Checks\n\n%d checks, %d not passed.\n", len(results), len(notPassed))
	for _, r := range notPassed {
		mark := "FAIL"
		if r.Gap != "" {
			mark = "GAP"
		}
		fmt.Fprintf(&b, "- %s %s %s: %s\n", mark, r.Scenario, r.Check, mdCell(r.Detail))
	}
	return b.String()
}

// countsLine prints counts by key, "none" for none.
func countsLine(m map[string]int) string {
	var out []string
	for _, k := range slices.Sorted(maps.Keys(m)) {
		out = append(out, fmt.Sprintf("%s %d", k, m[k]))
	}
	if len(out) == 0 {
		return "none"
	}
	return strings.Join(out, ", ")
}
