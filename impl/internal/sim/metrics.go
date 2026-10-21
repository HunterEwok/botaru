// SPDX-License-Identifier: AGPL-3.0-only

package sim

import (
	"context"
	"fmt"
	"maps"
	"math"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// The metrics of a run (the load test brief, "What we measure"): the threshold metrics M-1 to
// M-13 against their thresholds, and the observed values without a threshold. They are read
// from the store, from what the stand saw (the provider front, the faults, the checks) and
// from samples of the process taken every 30 seconds.

// Metric is one threshold metric.
type Metric struct {
	Code      string `json:"code"`
	Name      string `json:"name"`
	Value     string `json:"value"`
	Threshold string `json:"threshold"`
	// Status is pass, fail, or n/a: the run did no work the metric reads, or the metric is
	// checked in a load run and the run had no load.
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// Metrics is metrics.json.
type Metrics struct {
	At      time.Time `json:"at"`
	Final   bool      `json:"final"`
	Elapsed string    `json:"elapsed"`
	// Load is the load profile of the run, "" for a run of scenarios only.
	Load       string   `json:"load,omitempty"`
	Thresholds []Metric `json:"thresholds"`
	Observed   Observed `json:"observed"`
	Spend      Spend    `json:"spend"`
	Process    Process  `json:"process"`
}

// Observed are the values the brief records without a threshold.
type Observed struct {
	// AnswerTime is the time from a person's message to the end of its task, by task type.
	AnswerTime []TypeTimes `json:"answer_time"`
	// SecondAttemptShare is the share of people's tasks that needed a second attempt.
	SecondAttemptShare float64 `json:"second_attempt_share"`
	// RefusedByRule are the refusals of a rule (no right, no budget, the attention limit, a type
	// not allowed) by reason; they are not failures (M-1).
	RefusedByRule map[string]int `json:"refused_by_rule"`
	// ProviderAnswers are the provider's own answers to model calls by status, and Injected the
	// errors the stand put in (F-3); Share429 is the share of 429 among the provider's answers.
	ProviderAnswers map[string]int `json:"provider_answers"`
	Injected        map[string]int `json:"injected"`
	Share429        float64        `json:"share_429"`
	// IncidentOpening is the time from the start of an incident's alarm to its opening.
	IncidentOpening Times `json:"incident_opening"`
	// Cards and messages per person, the cards the core put off (paced), and the share of cards
	// decided before they expired.
	People            int     `json:"people"`
	MessagesPerPerson float64 `json:"messages_per_person"`
	CardsPerPerson    float64 `json:"cards_per_person"`
	MaxCardsPerPerson int     `json:"max_cards_per_person"`
	PacedCards        int     `json:"paced_cards"`
	DecidedInTime     float64 `json:"decided_in_time"`
	Approvals         int     `json:"approvals"`
	// CPU is the processor load of the stand process and of the host, in percent.
	CPU CPUSummary `json:"cpu"`
}

// TypeTimes are the answer times of one task type.
type TypeTimes struct {
	Type string `json:"type"`
	Times
}

// Times are the median and the 95th percentile of durations, in seconds.
type Times struct {
	N   int     `json:"n"`
	P50 float64 `json:"p50_s"`
	P95 float64 `json:"p95_s"`
}

// CPUSummary is the mean and the peak of the processor load in percent.
type CPUSummary struct {
	ProcessMean float64 `json:"process_mean"`
	ProcessMax  float64 `json:"process_max"`
	HostMean    float64 `json:"host_mean"`
	HostMax     float64 `json:"host_max"`
}

// Spend is the spend of the run by tenant and rank at two prices: the stand's (what the core
// charged, at the prices of the profiles, which the fuse reads and results.json reports) and the
// provider's (the tokens at the provider's price of the model, Haiku at the price of a short
// prompt; in a mock run the model of the profile's rank).
type Spend struct {
	Rows []SpendRow `json:"rows"`
	// StandEUR and ProviderEUR are the totals; Hours is the length the rates per hour are of.
	StandEUR    string  `json:"stand_eur"`
	ProviderEUR string  `json:"provider_eur"`
	Hours       float64 `json:"hours"`
	standMicros int64
}

// SpendRow is the spend of one tenant at one rank; rank 0 is the spend without a model profile
// (a tool's own cost).
type SpendRow struct {
	Tenant          string `json:"tenant"`
	Rank            int    `json:"rank"`
	Model           string `json:"model,omitempty"`
	Calls           int    `json:"calls"`
	InputTokens     int64  `json:"input_tokens"`
	OutputTokens    int64  `json:"output_tokens"`
	CacheWrite      int64  `json:"cache_write_tokens,omitempty"`
	CacheRead       int64  `json:"cache_read_tokens,omitempty"`
	StandEUR        string `json:"stand_eur"`
	ProviderEUR     string `json:"provider_eur"`
	StandPerHour    string `json:"stand_eur_per_hour"`
	ProviderPerHour string `json:"provider_eur_per_hour"`
	stand, provider int64
}

// Process is what the samples of the process show.
type Process struct {
	Samples int `json:"samples"`
	// PoolMax is the size of the core's store pool; PoolPeak the most connections it held and
	// DBPeak the most connections of the run's database (all pools of the process).
	PoolMax  int32 `json:"pool_max"`
	PoolPeak int32 `json:"pool_peak"`
	DBPeak   int   `json:"db_peak"`
	// The last procSample.
	RSSMB      float64 `json:"rss_mb"`
	HeapMB     float64 `json:"heap_mb"`
	Goroutines int     `json:"goroutines"`
	DBConns    int     `json:"db_conns"`
}

// procSample is one look at the process.
type procSample struct {
	At         time.Time
	RSSMB      float64
	HeapMB     float64
	Goroutines int
	PoolConns  int32
	PoolMax    int32
	DBConns    int
	ProcCPU    float64
	HostCPU    float64
}

// cpuTimes are the clock ticks of the process and of the host at a moment.
type cpuTimes struct {
	at         time.Time
	proc       float64
	hostBusy   float64
	hostTotal  float64
	procOK     bool
	hostTotalK bool
}

// readCPU reads /proc; off Linux it reads nothing and the load stays 0.
func readCPU() cpuTimes {
	c := cpuTimes{at: time.Now()}
	if raw, err := os.ReadFile("/proc/self/stat"); err == nil {
		// The fields after the command name, which ends at the last ')'.
		if i := strings.LastIndexByte(string(raw), ')'); i > 0 {
			f := strings.Fields(string(raw[i+1:]))
			if len(f) > 13 {
				u, _ := strconv.ParseFloat(f[11], 64)
				s, _ := strconv.ParseFloat(f[12], 64)
				c.proc, c.procOK = u+s, true
			}
		}
	}
	if raw, err := os.ReadFile("/proc/stat"); err == nil {
		line, _, _ := strings.Cut(string(raw), "\n")
		f := strings.Fields(line)
		if len(f) > 4 && f[0] == "cpu" {
			for i, v := range f[1:] {
				n, _ := strconv.ParseFloat(v, 64)
				c.hostTotal += n
				// idle and iowait are not busy.
				if i != 3 && i != 4 {
					c.hostBusy += n
				}
			}
			c.hostTotalK = true
		}
	}
	return c
}

// clockTicks is the kernel's USER_HZ, 100 on every Linux the stand runs on.
const clockTicks = 100

// load is the processor load between two readings in percent: of the process (100 is one whole
// processor) and of the host (100 is every processor busy).
func load(a, b cpuTimes) (proc, host float64) {
	wall := b.at.Sub(a.at).Seconds()
	if a.procOK && b.procOK && wall > 0 {
		proc = 100 * (b.proc - a.proc) / clockTicks / wall
	}
	if a.hostTotalK && b.hostTotalK && b.hostTotal > a.hostTotal {
		host = 100 * (b.hostBusy - a.hostBusy) / (b.hostTotal - a.hostTotal)
	}
	return proc, host
}

// rssMB is the resident memory of the process.
func rssMB() float64 {
	raw, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return 0
	}
	f := strings.Fields(string(raw))
	if len(f) < 2 {
		return 0
	}
	pages, _ := strconv.ParseFloat(f[1], 64)
	return pages * float64(os.Getpagesize()) / (1 << 20)
}

// takeSample looks at the process and the store once; prev is the last CPU reading.
func (s *Stand) takeSample(ctx context.Context, prev cpuTimes) (procSample, cpuTimes) {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	now := readCPU()
	sm := procSample{At: now.at, RSSMB: rssMB(), HeapMB: float64(ms.HeapAlloc) / (1 << 20), Goroutines: runtime.NumGoroutine()}
	sm.ProcCPU, sm.HostCPU = load(prev, now)
	pool := s.Bench.Store().Pool
	st := pool.Stat()
	sm.PoolConns, sm.PoolMax = st.TotalConns(), st.MaxConns()
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND backend_type = 'client backend'`).Scan(&sm.DBConns)
	return sm, now
}

// ruleReasons are the reasons of a refusal by a rule: not a failure of the farm (M-1).
var ruleReasons = []string{"budget_exhausted", "service_rate_limited", "type_not_allowed", "no_group_cap", "no_cap", "person_inactive", "held", "rights_revoked"}

// peopleTask is a task a person put from a chat.
type peopleTask struct {
	id, tenant, typ, state, reason string
	created, ended                 time.Time
	firstLease                     *time.Time
	attempts                       int
	// held says a hold that stops claims is active in the task's tenant (the hold of a runaway
	// spend, the fuse): its wait is the hold's, not the queue's.
	held bool
}

// metricsInput is what a report reads besides the store.
type metricsInput struct {
	start, loadStart, now time.Time
	load                  *LoadProfile
	final                 bool
	samples               []procSample
	budgetWaiting         map[string]map[string]bool
	results               []Result
}

// warmup is the time after the start of the load the growth of the process is measured from
// (M-12), and the least time after it a growth is measured over.
const (
	warmup     = 10 * time.Minute
	growthSpan = 4 * time.Minute
)

// computeMetrics reads the metrics of the run so far.
func (s *Stand) computeMetrics(ctx context.Context, in metricsInput) (*Metrics, error) {
	db := s.Bench.Store().Pool
	now := time.Now().UTC()
	in.now = now
	m := &Metrics{At: now, Final: in.final, Elapsed: now.Sub(in.start).Round(time.Second).String()}
	if in.load != nil {
		m.Load = in.load.Name
	}
	// The people's tasks: those a channel enqueued for a person.
	var tasks []peopleTask
	rows, err := db.Query(ctx, `SELECT t.id, t.tenant, t.type, t.state, coalesce(t.reason, ''), t.created_at, t.updated_at,
		(SELECT min(l.created_at) FROM leases l WHERE l.work_id = t.id AND l.kind = 'attempt'),
		(SELECT count(*) FROM leases l WHERE l.work_id = t.id AND l.kind = 'attempt'),
		EXISTS (SELECT 1 FROM holds h WHERE h.state = 'active' AND 'no_claim' = ANY(h.effects) AND (h.scope_kind = 'farm' OR h.scope_tenant = t.tenant))
		FROM tasks t WHERE t.record->>'enqueued_by' LIKE 'channel:%' AND t.created_at >= $1`, in.start)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var t peopleTask
		if err := rows.Scan(&t.id, &t.tenant, &t.typ, &t.state, &t.reason, &t.created, &t.ended, &t.firstLease, &t.attempts, &t.held); err != nil {
			rows.Close()
			return nil, err
		}
		tasks = append(tasks, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// The refusals of a rule, by tenant and reason: at the enqueue of a person (no task), and
	// tasks, attempts and deliveries that ended with the reason.
	refused := map[string]map[string]int{}
	add := func(tenant, reason string, n int) {
		if refused[tenant] == nil {
			refused[tenant] = map[string]int{}
		}
		refused[tenant][reason] += n
	}
	q := func(sql string, args ...any) error {
		r, err := db.Query(ctx, sql, args...)
		if err != nil {
			return err
		}
		defer r.Close()
		for r.Next() {
			var tenant, reason string
			var n int
			if err := r.Scan(&tenant, &reason, &n); err != nil {
				return err
			}
			add(tenant, reason, n)
		}
		return r.Err()
	}
	if err := q(`SELECT tenant, record->>'reason', count(*)::int FROM audit WHERE record->>'ok' = 'false' AND record->>'reason' = ANY($2)
		AND at >= $1 GROUP BY 1, 2`, in.start, ruleReasons); err != nil {
		return nil, err
	}
	for _, t := range tasks {
		if t.state != "completed" && slices.Contains(ruleReasons, t.reason) {
			add(t.tenant, t.reason, 1)
		}
	}
	if err := q(`SELECT tenant, reason, count(*)::int FROM leases WHERE kind = 'attempt' AND reason = ANY($2) AND created_at >= $1 GROUP BY 1, 2`,
		in.start, ruleReasons); err != nil {
		return nil, err
	}
	if err := q(`SELECT tenant, reason, count(*)::int FROM deliveries WHERE reason = ANY($2) AND created_at >= $1 GROUP BY 1, 2`,
		in.start, ruleReasons); err != nil {
		return nil, err
	}
	loadOnly := func(mt Metric) Metric {
		if in.load == nil && mt.Status != "n/a" {
			mt.Detail = strings.TrimSuffix("checked in a load run; "+mt.Detail, "; ")
			mt.Status = "n/a"
		}
		return mt
	}
	m.Thresholds = append(m.Thresholds,
		loadOnly(m1(tasks)),
		loadOnly(m2(tasks, in)),
	)
	m3, err := s.m3(ctx, in.start, now)
	if err != nil {
		return nil, err
	}
	m4, err := s.m4(ctx, in.start)
	if err != nil {
		return nil, err
	}
	m.Thresholds = append(m.Thresholds, loadOnly(m3), loadOnly(m4), s.m5(ctx))
	m6, err := s.m6(ctx, in, refused)
	if err != nil {
		return nil, err
	}
	// A person's own request refused, and the people's tasks in which a bot's request was refused
	// (the attention limit refuses mostly those: the bot gets an error and its task goes on).
	var peopleLimited int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM audit WHERE record->>'ok' = 'false' AND record->>'reason' = 'service_rate_limited'
		AND record->'actor'->>'principal' LIKE 'human:%' AND at >= $1`, in.start).Scan(&peopleLimited); err != nil {
		return nil, err
	}
	limitedTasks := map[string]bool{}
	lr, err := db.Query(ctx, `SELECT DISTINCT record->'actor'->>'task' FROM audit WHERE record->>'ok' = 'false'
		AND record->>'reason' = 'service_rate_limited' AND record->'actor'->>'task' IS NOT NULL AND at >= $1`, in.start)
	if err != nil {
		return nil, err
	}
	for lr.Next() {
		var id string
		if err := lr.Scan(&id); err != nil {
			lr.Close()
			return nil, err
		}
		limitedTasks[id] = true
	}
	lr.Close()
	if err := lr.Err(); err != nil {
		return nil, err
	}
	m.Thresholds = append(m.Thresholds, loadOnly(m6), loadOnly(m7(tasks, peopleLimited, limitedTasks)))
	m8, m9, err := s.m8m9(ctx, in.start, now)
	if err != nil {
		return nil, err
	}
	m10, err := s.m10(ctx, in)
	if err != nil {
		return nil, err
	}
	m.Thresholds = append(m.Thresholds, loadOnly(m8), loadOnly(m9), loadOnly(m10), m11(in), m12(in), m13(in.results))
	if m.Observed, err = s.observed(ctx, in, tasks, refused); err != nil {
		return nil, err
	}
	if m.Spend, err = s.spend(ctx, now.Sub(in.start)); err != nil {
		return nil, err
	}
	m.Process = process(in.samples)
	return m, nil
}

// share is a/b, 0 for no b.
func share(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return float64(a) / float64(b)
}

func pct(v float64) string { return strconv.FormatFloat(100*v, 'f', 1, 64) + "%" }

// pass is the status of a check.
func pass(ok bool) string {
	if ok {
		return "pass"
	}
	return "fail"
}

// times are the median and the 95th percentile of durations.
func times(ds []time.Duration) Times {
	if len(ds) == 0 {
		return Times{}
	}
	ds = slices.Clone(ds)
	slices.Sort(ds)
	at := func(p float64) float64 {
		i := int(math.Ceil(p*float64(len(ds)))) - 1
		return ds[max(i, 0)].Seconds()
	}
	return Times{N: len(ds), P50: at(0.5), P95: at(0.95)}
}

// m1 is the share of people's tasks with a result, in all and by task type with 20 tasks or
// more; tasks a rule refused and tasks still open do not count.
func m1(tasks []peopleTask) Metric {
	mt := Metric{Code: "M-1", Name: "people's tasks with a result", Threshold: ">= 90% in all; >= 80% for each kind of work with 20 tasks or more"}
	done, all := 0, 0
	byType := map[string][2]int{}
	for _, t := range tasks {
		if t.state != "completed" && t.state != "failed" && t.state != "canceled" || t.state != "completed" && slices.Contains(ruleReasons, t.reason) {
			continue
		}
		all++
		c := byType[t.typ]
		c[1]++
		if t.state == "completed" {
			done++
			c[0]++
		}
		byType[t.typ] = c
	}
	if all == 0 {
		mt.Value, mt.Status = "no ended task", "n/a"
		return mt
	}
	ok := share(done, all) >= 0.9
	var low []string
	for _, typ := range slices.Sorted(maps.Keys(byType)) {
		c := byType[typ]
		if c[1] >= 20 && share(c[0], c[1]) < 0.8 {
			ok = false
			low = append(low, fmt.Sprintf("%s %s of %d", typ, pct(share(c[0], c[1])), c[1]))
		}
	}
	mt.Value = fmt.Sprintf("%s (%d of %d)", pct(share(done, all)), done, all)
	mt.Status = pass(ok)
	if len(low) > 0 {
		mt.Detail = "below 80%: " + strings.Join(low, ", ")
	}
	return mt
}

// m2 is the 95th percentile of the queue wait of people's tasks: from the task to its first
// attempt lease, apart for the peaks of the load. The wait in the stand's limiter falls inside
// the attempt, so it is not in it.
func m2(tasks []peopleTask, in metricsInput) Metric {
	mt := Metric{Code: "M-2", Name: "queue wait of a person's task, 95th percentile", Threshold: "<= 5 min; <= 8 min in a peak"}
	var calm, peak []time.Duration
	open := 0
	for _, t := range tasks {
		var w time.Duration
		switch {
		case t.firstLease != nil:
			w = max(t.firstLease.Sub(t.created), 0)
		case slices.Contains(ruleReasons, t.reason):
			continue
		case t.state == "submitted" && !t.held:
			// Not taken yet: it has waited at least this long.
			w = max(in.now.Sub(t.created), 0)
			open++
		case t.state == "failed" || t.state == "canceled":
			// Ended without ever being taken (its deadline passed in the queue).
			w = max(t.ended.Sub(t.created), 0)
		default:
			continue
		}
		if inPeak(in, t.created) {
			peak = append(peak, w)
		} else {
			calm = append(calm, w)
		}
	}
	c, p := times(calm), times(peak)
	if c.N+p.N == 0 {
		mt.Value, mt.Status = "no task taken", "n/a"
		return mt
	}
	mt.Value = fmt.Sprintf("%.1f s of %d", c.P95, c.N)
	if p.N > 0 {
		mt.Value += fmt.Sprintf("; in peaks %.1f s of %d", p.P95, p.N)
	}
	mt.Status = pass(c.P95 <= 300 && p.P95 <= 480)
	if open > 0 {
		mt.Detail = fmt.Sprintf("%d not taken yet count with their wait so far", open)
	}
	return mt
}

// inPeak says whether a moment falls in a peak of the load profile.
func inPeak(in metricsInput, at time.Time) bool {
	if in.load == nil || in.loadStart.IsZero() {
		return false
	}
	for _, p := range in.load.Peaks {
		from := in.loadStart.Add(p.At)
		if !at.Before(from) && at.Before(from.Add(p.For)) {
			return true
		}
	}
	return false
}

// m3 is the share of scheduled tasks taken within their start window: the first attempt lease
// before start_by_at (the core sets it from timeouts.start_by of the type). A task not taken whose
// window has not passed yet is pending; a slot skipped for another reason than a pause is not on
// time. How the task ended does not count here (M-1 and the checks read that).
func (s *Stand) m3(ctx context.Context, start, now time.Time) (Metric, error) {
	mt := Metric{Code: "M-3", Name: "scheduled work started on time (start_by)", Threshold: ">= 99%"}
	db := s.Bench.Store().Pool
	var onTime, late, skipped int
	if err := db.QueryRow(ctx, `SELECT
		count(*) FILTER (WHERE l.first IS NOT NULL AND (t.start_by_at IS NULL OR l.first <= t.start_by_at)),
		count(*) FILTER (WHERE l.first IS NOT NULL AND l.first > t.start_by_at OR l.first IS NULL AND t.start_by_at < $2)
		FROM tasks t LEFT JOIN LATERAL (SELECT min(created_at) AS first FROM leases WHERE work_id = t.id AND kind = 'attempt') l ON true
		WHERE t.record->>'enqueued_by' LIKE 'schedule:%' AND t.created_at >= $1`, start, now).Scan(&onTime, &late); err != nil {
		return mt, err
	}
	// A slot skipped for any reason but a pause is not on time; the reasons are shown, since a
	// fault of the run (an open provider circuit, a hold) skips slots too.
	reasons := map[string]int{}
	rows, err := db.Query(ctx, `SELECT coalesce(data->>'reason', ''), count(*)::int FROM events WHERE type = 'schedule.skipped'
		AND coalesce(data->>'reason', '') <> 'paused' AND time >= $1 GROUP BY 1`, start)
	if err != nil {
		return mt, err
	}
	for rows.Next() {
		var reason string
		var n int
		if err := rows.Scan(&reason, &n); err != nil {
			rows.Close()
			return mt, err
		}
		reasons[orDash(reason)] = n
		skipped += n
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return mt, err
	}
	all := onTime + late + skipped
	if all == 0 {
		mt.Value, mt.Status = "no slot", "n/a"
		return mt, nil
	}
	mt.Value = fmt.Sprintf("%s (%d of %d)", pct(share(onTime, all)), onTime, all)
	mt.Status = pass(share(onTime, all) >= 0.99)
	if late+skipped > 0 {
		mt.Detail = fmt.Sprintf("late %d, skipped %d", late, skipped)
		for _, r := range slices.Sorted(maps.Keys(reasons)) {
			mt.Detail += fmt.Sprintf("; %s %d", r, reasons[r])
		}
	}
	return mt, nil
}

// m4 is the share of attempts the core ended for a result outside its schema.
func (s *Stand) m4(ctx context.Context, start time.Time) (Metric, error) {
	mt := Metric{Code: "M-4", Name: "attempts with output_schema_invalid", Threshold: "<= 2%"}
	var bad, all int
	if err := s.Bench.Store().Pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE reason LIKE 'output_schema_invalid%'), count(*)
		FROM leases WHERE kind = 'attempt' AND state <> 'active' AND created_at >= $1`, start).Scan(&bad, &all); err != nil {
		return mt, err
	}
	if all == 0 {
		mt.Value, mt.Status = "no attempt", "n/a"
		return mt, nil
	}
	mt.Value = fmt.Sprintf("%s (%d of %d)", pct(share(bad, all)), bad, all)
	mt.Status = pass(share(bad, all) <= 0.02)
	return mt, nil
}

// m5 is whether the task of each F-1 ended within two lease terms of the stop.
func (s *Stand) m5(ctx context.Context) Metric {
	mt := Metric{Code: "M-5", Name: "a task completes after F-1", Threshold: "within two lease terms"}
	s.mu.Lock()
	runs := slices.Clone(s.faultRuns)
	s.mu.Unlock()
	db := s.Bench.Store().Pool
	n, ok, pending := 0, true, 0
	var bad []string
	for _, r := range runs {
		if r.Code != "F-1" || r.Task == "" {
			continue
		}
		var state string
		var ended time.Time
		if err := db.QueryRow(ctx, `SELECT state, updated_at FROM tasks WHERE id = $1`, r.Task).Scan(&state, &ended); err != nil {
			continue
		}
		if state == "submitted" || state == "working" {
			// Still running: a failure only once two terms of the longest lease passed.
			if time.Since(r.At) > 2*maxLeaseTerm+10*time.Second {
				ok = false
				bad = append(bad, r.Task+" still "+state)
				n++
			} else {
				pending++
			}
			continue
		}
		var termS float64
		_ = db.QueryRow(ctx, `SELECT coalesce(max(extract(epoch FROM lease_until - created_at)), 60)::float8
			FROM leases WHERE work_id = $1 AND kind = 'attempt' AND state = 'expired'`, r.Task).Scan(&termS)
		term := time.Duration(termS * float64(time.Second))
		n++
		if state != "completed" || ended.Sub(r.At) > 2*term+10*time.Second {
			ok = false
			bad = append(bad, fmt.Sprintf("%s %s after %s (term %s)", r.Task, state, ended.Sub(r.At).Round(time.Second), term.Round(time.Second)))
		}
	}
	if n == 0 {
		mt.Value, mt.Status = "no F-1 ended", "n/a"
		if pending > 0 {
			mt.Value = fmt.Sprintf("%d F-1 pending", pending)
		}
		return mt
	}
	mt.Value = fmt.Sprintf("%d of %d in time", n-len(bad), n)
	if pending > 0 {
		mt.Value += fmt.Sprintf(", %d pending", pending)
	}
	mt.Status = pass(ok)
	mt.Detail = strings.Join(bad, "; ")
	return mt
}

// maxLeaseTerm is the longest attempt lease of the stand's task types.
const maxLeaseTerm = 120 * time.Second

// tightBudget are the tenants whose budget is meant to run out in a load run.
func (s *Stand) tightBudget() []string {
	var out []string
	for _, t := range s.farm.Tenants {
		if t.TightBudget {
			out = append(out, t.Name)
		}
	}
	return out
}

// m6 counts the stops for money by tenant: a refused reserve (budget_exhausted), a task that
// waited with that reason (sampled), a hold for runaway_spend.
func (s *Stand) m6(ctx context.Context, in metricsInput, refused map[string]map[string]int) (Metric, error) {
	mt := Metric{Code: "M-6", Name: "stops for money", Threshold: "only in the tenants with a tight budget; 0 elsewhere"}
	stops := map[string]int{}
	for tenant, rs := range refused {
		stops[tenant] += rs["budget_exhausted"]
	}
	for tenant, ids := range in.budgetWaiting {
		stops[tenant] += len(ids)
	}
	r, err := s.Bench.Store().Pool.Query(ctx, `SELECT tenant, count(*)::int FROM holds WHERE reason = 'runaway_spend' AND created_at >= $1 GROUP BY 1`, in.start)
	if err != nil {
		return mt, err
	}
	for r.Next() {
		var tenant string
		var n int
		if err := r.Scan(&tenant, &n); err != nil {
			r.Close()
			return mt, err
		}
		stops[tenant] += n
	}
	r.Close()
	if err := r.Err(); err != nil {
		return mt, err
	}
	tight := s.tightBudget()
	ok := true
	var parts []string
	for _, t := range slices.Sorted(maps.Keys(stops)) {
		if stops[t] == 0 {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s %d", t, stops[t]))
		if !slices.Contains(tight, t) {
			ok = false
		}
	}
	mt.Value = "none"
	if len(parts) > 0 {
		mt.Value = strings.Join(parts, ", ")
	}
	mt.Status = pass(ok)
	if len(tight) > 0 {
		mt.Detail = "tight budget: " + strings.Join(tight, ", ")
	}
	return mt, nil
}

// m7 is the share of service_rate_limited refusals of people's work (their enqueue refused, their
// task ended with it, or a bot working their task was refused) against people's tasks.
func m7(tasks []peopleTask, peopleLimited int, limitedTasks map[string]bool) Metric {
	mt := Metric{Code: "M-7", Name: "service_rate_limited refusals", Threshold: "<= 5% of people's tasks (above: the load profile does not fit the limits)"}
	n := peopleLimited
	for _, t := range tasks {
		if t.state != "completed" && t.reason == "service_rate_limited" || limitedTasks[t.id] {
			n++
		}
	}
	if len(tasks) == 0 {
		mt.Value, mt.Status = fmt.Sprintf("%d, no people's task", n), "n/a"
		return mt
	}
	mt.Value = fmt.Sprintf("%s (%d against %d tasks)", pct(share(n, len(tasks))), n, len(tasks))
	mt.Status = pass(share(n, len(tasks)) <= 0.05)
	return mt
}

// m8m9 are the farm manager's answers to the incidents of the tenants with a manager (within
// 5 minutes of the opening) and the notices of an incident in a tenant without one, delivered
// within 2 minutes. An incident opened less than the bound ago without an answer yet is pending,
// not late.
func (s *Stand) m8m9(ctx context.Context, start, now time.Time) (Metric, Metric, error) {
	m8 := Metric{Code: "M-8", Name: "the farm manager answered an incident", Threshold: "within 5 min of the opening (tenants with a manager)"}
	m9 := Metric{Code: "M-9", Name: "the notice of an incident reached the admin", Threshold: "within 2 min (tenants without a manager)"}
	manager := map[string]bool{}
	for _, t := range s.farm.Tenants {
		manager[t.Name] = t.Manager
	}
	r, err := s.Bench.Store().Pool.Query(ctx, `SELECT i.id, i.tenant, i.opened_at,
		(SELECT min(t.updated_at) FROM tasks t WHERE t.tenant = i.tenant AND t.type = 'manager.incident.respond.v1' AND t.state = 'completed'
			AND t.idempotency_key LIKE 'system:incident:' || i.id || ':%'),
		(SELECT min(d.updated_at) FROM deliveries d WHERE d.tenant = i.tenant AND d.kind = 'manager_notice' AND d.subject_kind = 'incident'
			AND d.subject_id = i.id AND d.state = 'delivered')
		FROM incidents i WHERE i.opened_at >= $1`, start)
	if err != nil {
		return m8, m9, err
	}
	type res struct{ n, late, pending int }
	var a8, a9 res
	var slow8, slow9 []string
	for r.Next() {
		var id, tenant string
		var opened time.Time
		var answered, noticed *time.Time
		if err := r.Scan(&id, &tenant, &opened, &answered, &noticed); err != nil {
			r.Close()
			return m8, m9, err
		}
		judge := func(a *res, at *time.Time, bound time.Duration, slow *[]string) {
			switch {
			case at != nil && at.Sub(opened) <= bound:
				a.n++
			case at == nil && now.Sub(opened) <= bound:
				a.pending++
			default:
				a.n++
				a.late++
				*slow = append(*slow, tenant+" "+id)
			}
		}
		if manager[tenant] {
			judge(&a8, answered, 5*time.Minute, &slow8)
		} else {
			judge(&a9, noticed, 2*time.Minute, &slow9)
		}
	}
	r.Close()
	if err := r.Err(); err != nil {
		return m8, m9, err
	}
	fill := func(mt *Metric, a res, slow []string) {
		if a.n == 0 {
			mt.Value, mt.Status = "no incident", "n/a"
		} else {
			mt.Value = fmt.Sprintf("%d of %d in time", a.n-a.late, a.n)
			mt.Status = pass(a.late == 0)
		}
		var d []string
		if a.pending > 0 {
			d = append(d, fmt.Sprintf("%d pending", a.pending))
		}
		if len(slow) > 0 {
			d = append(d, "late or missing: "+strings.Join(slow, ", "))
		}
		mt.Detail = strings.Join(d, "; ")
	}
	fill(&m8, a8, slow8)
	fill(&m9, a9, slow9)
	return m8, m9, nil
}

// m10 is the path of improvement: in each tenant with a manager at least one proposal its
// commission passed on, and the scenario of the evolution passed whole when it ran.
func (s *Stand) m10(ctx context.Context, in metricsInput) (Metric, error) {
	mt := Metric{Code: "M-10", Name: "the path of improvement", Threshold: "a proposal passed the commission in each tenant with a manager; the evolution scenario passed"}
	ok := true
	var parts []string
	for _, t := range s.farm.Tenants {
		if !t.Manager {
			continue
		}
		var n int
		if err := s.Bench.Store().Pool.QueryRow(ctx, `SELECT count(*) FROM proposals WHERE tenant = $1
			AND (presented_at IS NOT NULL OR state = 'recommended' OR state = 'dismissed' AND record->>'state_reason' = 'gate_failed')
			AND created_at >= $2`,
			t.Name, in.start).Scan(&n); err != nil {
			return mt, err
		}
		parts = append(parts, fmt.Sprintf("%s %d", t.Name, n))
		if n == 0 {
			ok = false
		}
	}
	if len(parts) == 0 {
		mt.Value, mt.Status = "no tenant with a manager", "n/a"
		return mt, nil
	}
	evo, failed := 0, 0
	for _, r := range in.results {
		if r.Scenario == "scaffold-evolution" {
			evo++
			if !r.OK && r.Gap == "" {
				failed++
			}
		}
	}
	switch {
	case evo == 0:
		// A load run has no scenarios: the evolution scenario is checked in a scenario run.
		parts = append(parts, "the evolution scenario did not run in this run")
	case failed > 0:
		ok = false
		parts = append(parts, fmt.Sprintf("the evolution scenario failed %d of %d checks", failed, evo))
	default:
		parts = append(parts, "the evolution scenario passed")
	}
	mt.Value = strings.Join(parts, "; ")
	mt.Status = pass(ok)
	return mt, nil
}

// afterWarmup are the samples from the warm-up on, nil when they span less than growthSpan.
func afterWarmup(in metricsInput) []procSample {
	from := in.start
	if !in.loadStart.IsZero() {
		from = in.loadStart
	}
	from = from.Add(warmup)
	var out []procSample
	for _, s := range in.samples {
		if !s.At.Before(from) {
			out = append(out, s)
		}
	}
	if len(out) < 2 || out[len(out)-1].At.Sub(out[0].At) < growthSpan {
		return nil
	}
	return out
}

// growth compares the mean of the last quarter of the samples with the mean of the first.
func growth(ss []procSample, v func(procSample) float64) float64 {
	q := max(len(ss)/4, 1)
	mean := func(xs []procSample) float64 {
		t := 0.0
		for _, x := range xs {
			t += v(x)
		}
		return t / float64(len(xs))
	}
	first, last := mean(ss[:q]), mean(ss[len(ss)-q:])
	if first <= 0 {
		return 0
	}
	return last/first - 1
}

// m11 are the store connections: the connections of the run's database beyond the core's pool
// never more than at the start (and two), so nothing opens connections past the pool, and no
// growth after the warm-up.
func m11(in metricsInput) Metric {
	mt := Metric{Code: "M-11", Name: "store connections", Threshold: "not above the pool, no growth"}
	samples := in.samples
	if len(samples) == 0 {
		mt.Value, mt.Status = "no sample", "n/a"
		return mt
	}
	p := process(samples)
	base := samples[0].DBConns - int(samples[0].PoolConns)
	most := base
	for _, sm := range samples {
		most = max(most, sm.DBConns-int(sm.PoolConns))
	}
	mt.Value = fmt.Sprintf("pool peak %d of %d; database peak %d; beyond the pool %d at the start, %d at most", p.PoolPeak, p.PoolMax, p.DBPeak, base, most)
	ok := most <= base+2
	if ss := afterWarmup(in); ss != nil {
		g := growth(ss, func(s procSample) float64 { return float64(s.DBConns) })
		mt.Value += "; growth " + pct(g)
		// A few connections more is no growth: the pool opens them as the load needs.
		ok = ok && (g <= 0.2 || ss[len(ss)-1].DBConns-ss[0].DBConns <= 2)
	}
	mt.Status = pass(ok)
	return mt
}

// m12 is the growth of the memory and the goroutines after 10 minutes of warm-up.
func m12(in metricsInput) Metric {
	mt := Metric{Code: "M-12", Name: "growth of memory and goroutines after the warm-up", Threshold: "<= 20%"}
	ss := afterWarmup(in)
	if ss == nil {
		mt.Value, mt.Status = fmt.Sprintf("the run is shorter than %s of warm-up and %s", warmup, growthSpan), "n/a"
		return mt
	}
	gm := growth(ss, func(s procSample) float64 { return s.RSSMB })
	gg := growth(ss, func(s procSample) float64 { return float64(s.Goroutines) })
	mt.Value = fmt.Sprintf("memory %s, goroutines %s", pct(gm), pct(gg))
	mt.Status = pass(gm <= 0.2 && gg <= 0.2)
	return mt
}

// m13 are the checks of the scenarios: every one passed or stopped at a known gap of the core.
func m13(results []Result) Metric {
	mt := Metric{Code: "M-13", Name: "scenario checks", Threshold: "100% PASS"}
	if len(results) == 0 {
		mt.Value, mt.Status = "no check", "n/a"
		return mt
	}
	ok, gaps := 0, 0
	for _, r := range results {
		switch {
		case r.OK:
			ok++
		case r.Gap != "":
			gaps++
		}
	}
	mt.Value = fmt.Sprintf("%d of %d", ok, len(results))
	// A known gap of the core is recorded and does not decide the outcome (the brief, 9.7).
	mt.Status = pass(ok+gaps == len(results))
	if gaps > 0 {
		mt.Detail = fmt.Sprintf("%d stopped at a known gap of the core", gaps)
	}
	return mt
}

// observed reads the values without a threshold.
func (s *Stand) observed(ctx context.Context, in metricsInput, tasks []peopleTask, refused map[string]map[string]int) (Observed, error) {
	var o Observed
	byType := map[string][]time.Duration{}
	second, taken := 0, 0
	for _, t := range tasks {
		if t.state == "completed" {
			byType[t.typ] = append(byType[t.typ], t.ended.Sub(t.created))
		}
		if t.attempts > 0 {
			taken++
			if t.attempts > 1 {
				second++
			}
		}
	}
	for _, typ := range slices.Sorted(maps.Keys(byType)) {
		o.AnswerTime = append(o.AnswerTime, TypeTimes{Type: typ, Times: times(byType[typ])})
	}
	o.SecondAttemptShare = share(second, taken)
	o.RefusedByRule = map[string]int{}
	for _, rs := range refused {
		for reason, n := range rs {
			o.RefusedByRule[reason] += n
		}
	}
	o.ProviderAnswers, o.Injected = map[string]int{}, map[string]int{}
	answered, all := s.front.answers(), 0
	for code, n := range answered {
		o.ProviderAnswers[strconv.Itoa(code)] = n
		all += n
	}
	o.Share429 = share(answered[429], all)
	injected, delayed := s.front.counts()
	for code, n := range injected {
		o.Injected[strconv.Itoa(code)] = n
	}
	if delayed > 0 {
		o.Injected["delayed"] = delayed
	}
	db := s.Bench.Store().Pool
	var opening []time.Duration
	// From the alarm episode that opened the incident to the incident.
	r, err := db.Query(ctx, `SELECT i.opened_at - e.opened_at FROM incidents i JOIN alarm_episodes e ON e.id = i.episodes[1]
		WHERE i.opened_at >= $1`, in.start)
	if err != nil {
		return o, err
	}
	for r.Next() {
		var d time.Duration
		if err := r.Scan(&d); err != nil {
			r.Close()
			return o, err
		}
		opening = append(opening, d)
	}
	r.Close()
	if err := r.Err(); err != nil {
		return o, err
	}
	o.IncidentOpening = times(opening)
	// Messages and cards per person who got any.
	var people, messages, cards int
	if err := db.QueryRow(ctx, `SELECT count(DISTINCT recipient), count(*), count(*) FILTER (WHERE card_expires_at IS NOT NULL),
		count(*) FILTER (WHERE card_expires_at IS NOT NULL AND paced)
		FROM deliveries WHERE recipient LIKE 'human:%' AND created_at >= $1`, in.start).Scan(&people, &messages, &cards, &o.PacedCards); err != nil {
		return o, err
	}
	if err := db.QueryRow(ctx, `SELECT coalesce(max(n), 0) FROM (SELECT count(*) AS n FROM deliveries WHERE recipient LIKE 'human:%'
		AND card_expires_at IS NOT NULL AND created_at >= $1 GROUP BY recipient) c`, in.start).Scan(&o.MaxCardsPerPerson); err != nil {
		return o, err
	}
	o.People = people
	o.MessagesPerPerson, o.CardsPerPerson = share(messages, people), share(cards, people)
	var decided int
	if err := db.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE decided_at IS NOT NULL AND (expires_at IS NULL OR decided_at <= expires_at))
		FROM approvals WHERE cards_at IS NOT NULL AND created_at >= $1`, in.start).Scan(&o.Approvals, &decided); err != nil {
		return o, err
	}
	o.DecidedInTime = share(decided, o.Approvals)
	o.CPU = cpuSummary(in.samples)
	return o, nil
}

func cpuSummary(ss []procSample) CPUSummary {
	var c CPUSummary
	// The first procSample has no reading before it.
	if len(ss) < 2 {
		return c
	}
	for _, s := range ss[1:] {
		c.ProcessMean += s.ProcCPU
		c.HostMean += s.HostCPU
		c.ProcessMax = max(c.ProcessMax, s.ProcCPU)
		c.HostMax = max(c.HostMax, s.HostCPU)
	}
	n := float64(len(ss) - 1)
	c.ProcessMean /= n
	c.HostMean /= n
	return c
}

// process sums the samples.
func process(ss []procSample) Process {
	p := Process{Samples: len(ss)}
	for _, s := range ss {
		p.PoolMax = max(p.PoolMax, s.PoolMax)
		p.PoolPeak = max(p.PoolPeak, s.PoolConns)
		p.DBPeak = max(p.DBPeak, s.DBConns)
	}
	if len(ss) > 0 {
		l := ss[len(ss)-1]
		p.RSSMB, p.HeapMB, p.Goroutines, p.DBConns = l.RSSMB, l.HeapMB, l.Goroutines, l.DBConns
	}
	return p
}

// spend reads the spend by tenant and profile and prices the tokens at the provider's price.
// The stand column sums what spendTotal sums, so its total is the spend of results.json.
func (s *Stand) spend(ctx context.Context, elapsed time.Duration) (Spend, error) {
	sp := Spend{Hours: elapsed.Hours()}
	rows, err := s.Bench.Store().Pool.Query(ctx, `SELECT tenant, coalesce(profile, ''), count(*) FILTER (WHERE tokens IS NOT NULL)::int,
		coalesce(sum((tokens->>'input')::bigint), 0)::bigint, coalesce(sum((tokens->>'output')::bigint), 0)::bigint,
		coalesce(sum((tokens->>'cache_write')::bigint), 0)::bigint, coalesce(sum((tokens->>'cache_read')::bigint), 0)::bigint,
		coalesce(sum(eur_micros + coalesce(correction_eur_micros, 0)), 0)::bigint
		FROM spend WHERE unit <> 'task' GROUP BY 1, 2`)
	if err != nil {
		return sp, err
	}
	type key struct {
		tenant string
		rank   int
	}
	byKey := map[key]*SpendRow{}
	var total, provider int64
	err = forRows(rows, func(r pgx.Rows) error {
		var tenant, profile string
		var calls int
		var in, out, cw, cr, micros int64
		if err := r.Scan(&tenant, &profile, &calls, &in, &out, &cw, &cr, &micros); err != nil {
			return err
		}
		model, rank := s.profileModel(profile)
		row := byKey[key{tenant, rank}]
		if row == nil {
			row = &SpendRow{Tenant: tenant, Rank: rank, Model: model}
			byKey[key{tenant, rank}] = row
		}
		p := s.cfg.ProviderPrices[model]
		// The provider bills a cache write at 1.25 and a cache read at 0.1 of the input price.
		cost := int64((float64(in)+1.25*float64(cw)+0.1*float64(cr))*p[0] + float64(out)*p[1])
		row.Calls += calls
		row.CacheWrite += cw
		row.CacheRead += cr
		row.InputTokens += in
		row.OutputTokens += out
		row.stand += micros
		row.provider += cost
		total += micros
		provider += cost
		return nil
	})
	if err != nil {
		return sp, err
	}
	perHour := func(m int64) string {
		if sp.Hours <= 0 {
			return EUR(0)
		}
		return EUR(int64(float64(m) / sp.Hours))
	}
	for _, k := range slices.SortedFunc(maps.Keys(byKey), func(a, b key) int {
		if c := strings.Compare(a.tenant, b.tenant); c != 0 {
			return c
		}
		return a.rank - b.rank
	}) {
		row := byKey[k]
		if row.stand == 0 && row.provider == 0 && row.Calls == 0 {
			continue
		}
		row.StandEUR, row.ProviderEUR = EUR(row.stand), EUR(row.provider)
		row.StandPerHour, row.ProviderPerHour = perHour(row.stand), perHour(row.provider)
		sp.Rows = append(sp.Rows, *row)
	}
	sp.StandEUR, sp.ProviderEUR, sp.standMicros = EUR(total), EUR(provider), total
	return sp, nil
}

// forRows calls f for each row and closes the rows.
func forRows(rows pgx.Rows, f func(pgx.Rows) error) error {
	defer rows.Close()
	for rows.Next() {
		if err := f(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}
