// SPDX-License-Identifier: AGPL-3.0-only

package sim

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"math/rand/v2"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/HunterEwok/botaru/impl/internal/core"
)

// The faults of the farm itself (F-1 to F-8), as against the faults of the world the bots
// look after: a replica that goes silent, a plugin process killed, the provider failing or
// slow, the monitoring API away, a webhook twice, the store connections cut and a core rollout.
// Each fault heals itself; Expect says what the core is to show for it.

// FarmFault is one fault of the farm.
type FarmFault struct {
	Code string
	// Tenant says whether the fault acts in one tenant (F-1); the others act on the whole
	// farm: one provider front, one monitoring system, one store, one core.
	Tenant bool
	What   string
	// Expect is the outcome the core is to show: its alarms and its retries.
	Expect string
}

// FarmFaults are the faults of the farm in code order.
var FarmFaults = []FarmFault{
	{"F-1", true, "a replica holding an attempt lease goes silent (no pulses, the lease is not released) and starts again after " + replicaDown.String(),
		"the lease expires and the attempt is dispatched again (to another replica of the role if one runs); the task completes; " +
			"alarm.worker.stale for the silent worker, resolved when it calls again"},
	{"F-2", false, "the next plugin process the toolhosts start is killed (SIGKILL) while its tool call is in flight",
		"that tool call fails and the bot sees a tool error; the attempt goes on or is retried; the next call starts a new process"},
	{"F-3", false, fmt.Sprintf("the provider answers 500, 529 or 429 to the next %d model calls and then to %.0f%% of them for %s", providerErrFirst, providerErrShare*100, providerFaultFor),
		"the gateway maps 500 and 529 to provider_unavailable and 429 to provider_rate_limited; the agent ends the attempt at the failed call " +
			"without spending it, the task waits for the provider (provider_quota) and starts the attempt over from its first call; " +
			"an attempt in which the model had answered before the failure counts a run, so a task of several calls may end " +
			"runs_exhausted (retry.max_runs) while the fault lasts; work after the fault completes"},
	{"F-4", false, "the provider waits " + providerDelay.String() + " before every model call for " + providerFaultFor.String(),
		"calls stay under the gateway deadline (at least 60 s), so they succeed late; attempts take longer; no alarm"},
	{"F-5", false, "the monitoring API answers 503 for " + monitorDown.String(),
		"the monitor plugin and the adapter get errors; the alarm export waits and retries (alarm.monitor.export_stalled only after 15 minutes, " +
			"so not here); the export catches up once the API answers"},
	{"F-6", false, "the next webhook of an opened problem comes twice",
		"the adapter submits the same event twice; the core keeps one activity and one task for it (idempotency key)"},
	{"F-7", false, "every store connection of the run's database is cut (pg_terminate_backend)",
		"the transactions in flight fail once; the pool reconnects; the loops tick again; P1 may answer store_unavailable briefly"},
	{"F-8", false, "a core rollout: core.drain, the running attempts finish, the core starts again over the same store (test/process.stop)",
		"while draining, claims answer not_ready and running attempts keep their leases; the start ends the drain and hands out work again"},
}

// FaultCodes are the codes of the farm faults.
func FaultCodes() []string {
	out := make([]string, len(FarmFaults))
	for i, f := range FarmFaults {
		out[i] = f.Code
	}
	return out
}

// FarmFaultOf is the farm fault of a code, nil for none.
func FarmFaultOf(code string) *FarmFault {
	for i := range FarmFaults {
		if strings.EqualFold(FarmFaults[i].Code, code) {
			return &FarmFaults[i]
		}
	}
	return nil
}

const (
	replicaDown      = 150 * time.Second
	providerErrShare = 0.25
	// providerErrFirst model calls fail for sure when F-3 begins; the front serves the whole
	// farm, so more than one makes it likely that the next task meets one of them.
	providerErrFirst = 3
	providerFaultFor = 2 * time.Minute
	providerDelay    = 30 * time.Second
	monitorDown      = 2 * time.Minute
	drainFor         = 5 * time.Minute
	drainWait        = 2 * time.Minute
)

// providerFront stands between P4 and the provider (the mock or the real one): it passes every
// call on, except that while a fault is on it answers a share of the model calls with an error
// itself, without calling the provider, or waits before passing a call on.
type providerFront struct {
	pass http.Handler

	mu    sync.Mutex
	r     *rand.Rand
	until time.Time
	share float64
	codes []int
	delay time.Duration
	// next is the number of model calls still to fail before the share applies, so that a
	// fault always shows at least once.
	next int
	// injected counts the errors it answered by status and delayed the calls it held.
	injected map[int]int
	delayed  int
	// answered counts the answers of the provider itself to model calls by status (the
	// metrics: 429 and 5xx without an injected fault).
	answered map[int]int
	// observe sees each model request with the tenant its key names (the invariant watch).
	observe func(tenant string, body []byte)
	// halted says the run is halted: a model request is then refused, the one that caused the
	// halt included, so it never reaches the provider.
	halted func() bool
}

// tenantKeyMark joins the provider key and the tenant in the key P4 sends the front; the front
// cuts it off, so the provider gets the key alone.
const tenantKeyMark = "~sim-tenant="

func newProviderFront(target string, seed int64) (*providerFront, error) {
	u, err := url.Parse(target)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("the provider address %q is not a URL", target)
	}
	rp := &httputil.ReverseProxy{Rewrite: func(r *httputil.ProxyRequest) { r.SetURL(u) }}
	return &providerFront{pass: rp, r: rand.New(rand.NewPCG(uint64(seed), 29)), injected: map[int]int{}, answered: map[int]int{}}, nil
}

// set puts a fault on for a time: a share of errors with the codes (the first next calls fail
// for sure), or a delay.
func (p *providerFront) set(for_ time.Duration, share float64, next int, codes []int, delay time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.until, p.share, p.next, p.codes, p.delay = time.Now().Add(for_), share, next, codes, delay
}

// counts are the errors answered by status and the calls delayed so far.
func (p *providerFront) counts() (map[int]int, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[int]int, len(p.injected))
	for k, v := range p.injected {
		out[k] = v
	}
	return out, p.delayed
}

// answers are the answers of the provider itself to model calls by status.
func (p *providerFront) answers() map[int]int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return maps.Clone(p.answered)
}

// statusWriter keeps the status of an answer.
type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.code == 0 {
		w.code = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.code == 0 {
		w.code = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

// Unwrap lets the proxy flush through the writer.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// providerErrors are the error answers of the provider (the Messages API error shapes).
var providerErrors = map[int]struct{ typ, msg string }{
	500: {"api_error", "Internal server error"},
	529: {"overloaded_error", "Overloaded"},
	429: {"rate_limit_error", "Number of requests has exceeded your rate limit"},
}

func (p *providerFront) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	model := r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/messages")
	tenant := ""
	if k := r.Header.Get("x-api-key"); strings.Contains(k, tenantKeyMark) {
		key, t, _ := strings.Cut(k, tenantKeyMark)
		r.Header.Set("x-api-key", key)
		tenant = t
	}
	if model && p.observe != nil {
		body, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
		_ = r.Body.Close()
		if err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		p.observe(tenant, body)
	}
	if model && p.halted != nil && p.halted() {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, `{"type":"error","error":{"type":"api_error","message":"the stand halted the run"}}`)
		return
	}
	p.mu.Lock()
	on := model && time.Now().Before(p.until)
	code, delay := 0, time.Duration(0)
	if on && len(p.codes) > 0 && (p.next > 0 || p.r.Float64() < p.share) {
		p.next = max(p.next-1, 0)
		code = p.codes[p.r.IntN(len(p.codes))]
		p.injected[code]++
	} else if on && p.delay > 0 {
		delay = p.delay
		p.delayed++
	}
	p.mu.Unlock()
	if code != 0 {
		e := providerErrors[code]
		w.Header().Set("Content-Type", "application/json")
		if code == http.StatusTooManyRequests {
			w.Header().Set("retry-after", "5")
		}
		w.WriteHeader(code)
		fmt.Fprintf(w, `{"type":"error","error":{"type":%q,"message":%q}}`, e.typ, e.msg)
		return
	}
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
	}
	if !model {
		p.pass.ServeHTTP(w, r)
		return
	}
	sw := &statusWriter{ResponseWriter: w}
	p.pass.ServeHTTP(sw, r)
	p.mu.Lock()
	p.answered[sw.code]++
	p.mu.Unlock()
}

// crashFile is the mark that kills the next plugin process the toolhosts start (F-2): the
// process that removes it kills itself when its first tool call comes in.
func crashFile(work string) string { return filepath.Join(work, "faults", "plugin-crash") }

// CrashEnv is the environment variable that names the mark of F-2 to the plugin processes.
const CrashEnv = "BOTARU_SIM_PLUGIN_CRASH"

// FaultRun is what one injected fault left: the worker F-1 stopped, the task it held, and
// what the provider front and the monitor counted.
type FaultRun struct {
	Code string
	At   time.Time
	// Scope is the tenant of F-1, "farm" for the others.
	Scope  string
	Worker string
	Task   string
	// Cut is the number of store connections F-7 cut; Drained says that F-8 saw the drain
	// mark set, and Running the attempts that held leases when the drain began.
	Cut     int
	Drained bool
	Running int
}

// InjectFault puts a farm fault on and heals it after its time; the stand's run ends any heal
// still waiting. F-1 stops the running replica that holds the newest attempt lease in the tenant (or,
// with none, a replica of the tenant's role with the most replicas); F-8 returns once the core
// started again.
func (s *Stand) InjectFault(ctx context.Context, code, tenant string) (*FaultRun, error) {
	return s.injectFault(ctx, code, tenant, "")
}

// injectFault is InjectFault; for F-1 a task names the attempt whose replica goes silent.
func (s *Stand) injectFault(ctx context.Context, code, tenant, task string) (*FaultRun, error) {
	f := FarmFaultOf(code)
	if f == nil {
		return nil, fmt.Errorf("no farm fault %s (known: %s)", code, strings.Join(FaultCodes(), ", "))
	}
	if tenant == "" {
		tenant = s.Home()
	}
	run := &FaultRun{Code: f.Code, At: time.Now(), Scope: "farm"}
	if f.Tenant {
		run.Scope = tenant
	}
	var err error
	switch f.Code {
	case "F-1":
		err = s.silenceReplica(ctx, tenant, task, run)
	case "F-2":
		if err = os.MkdirAll(filepath.Dir(crashFile(s.cfg.Work)), 0o755); err == nil {
			err = os.WriteFile(crashFile(s.cfg.Work), []byte("armed\n"), 0o644)
		}
	case "F-3":
		s.front.set(providerFaultFor, providerErrShare, providerErrFirst, []int{500, 529, 429}, 0)
	case "F-4":
		s.front.set(providerFaultFor, 0, 0, nil, providerDelay)
	case "F-5":
		s.Mon.Down(monitorDown)
	case "F-6":
		s.Mon.Twice()
	case "F-7":
		var n int
		err = s.Bench.Store().Pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE pg_terminate_backend(pid)) FROM pg_stat_activity
			WHERE datname = current_database() AND pid <> pg_backend_pid()`).Scan(&n)
		run.Cut = n
	case "F-8":
		err = s.rollCore(ctx, run)
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", f.Code, err)
	}
	s.mu.Lock()
	s.faultRuns = append(s.faultRuns, *run)
	s.mu.Unlock()
	s.log.Add(KindFault, fmt.Sprintf("%s in %s: %s", f.Code, run.Scope, f.What), map[string]any{"code": f.Code, "scope": run.Scope, "worker": run.Worker, "task": run.Task, "expect": f.Expect})
	return run, nil
}

// silenceReplica stops the replica of F-1 as a crash would and starts it again after
// replicaDown, under the stand's run.
func (s *Stand) silenceReplica(ctx context.Context, tenant, want string, run *FaultRun) error {
	// The newest lease of a replica that still runs (a silent one keeps its lease until it
	// expires), or the lease of the task asked for.
	running := map[string]bool{}
	for _, n := range s.Bench.Workers() {
		running[n] = true
	}
	rows, err := s.Bench.Store().Pool.Query(ctx, `SELECT worker_id, work_id FROM leases WHERE tenant = $1 AND kind = 'attempt' AND state = 'active'
		AND ($2 = '' OR work_id = $2) ORDER BY created_at DESC`, tenant, want)
	if err != nil {
		return err
	}
	name, task := "", ""
	for rows.Next() {
		var id, t string
		if err := rows.Scan(&id, &t); err != nil {
			rows.Close()
			return err
		}
		if n := s.Bench.WorkerName(id); running[n] {
			name, task = n, t
			break
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if want != "" && name == "" {
		return fmt.Errorf("no running replica holds the lease of %s", want)
	}
	if name == "" {
		name = s.busiestReplica(tenant)
	}
	if name == "" {
		return errors.New("no agent worker to stop")
	}
	if !s.Bench.StopWorker(name) {
		return fmt.Errorf("worker %s is not running", name)
	}
	run.Worker, run.Task = name, task
	s.spawn(func(ctx context.Context) {
		select {
		case <-ctx.Done():
			return
		case <-time.After(replicaDown):
		}
		if err := s.Bench.RestartWorker(name); err != nil {
			s.log.Addf(KindFault, "F-1: starting %s again: %v", name, err)
			return
		}
		s.log.Addf(KindFault, "F-1: %s runs again", name)
	})
	return nil
}

// spawn runs fn under the stand's run unless the stand is closing; Close waits for it.
func (s *Stand) spawn(fn func(context.Context)) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing || s.runCtx == nil {
		return false
	}
	s.done.Add(1)
	go func() { defer s.done.Done(); fn(s.runCtx) }()
	return true
}

// InjectFaultLater puts a farm fault on in the background under the stand's run (F-8 waits for
// the drain) and passes an error to fail.
func (s *Stand) InjectFaultLater(code, tenant string, fail func(error)) {
	s.spawn(func(ctx context.Context) {
		if _, err := s.InjectFault(ctx, code, tenant); err != nil && ctx.Err() == nil {
			fail(err)
		}
	})
}

// busiestReplica is the first replica of the tenant's agent role with the most replicas.
func (s *Stand) busiestReplica(tenant string) string {
	count := map[string]int{}
	var names []string
	for _, n := range s.Bench.Workers() {
		p := strings.Split(n, "/")
		if len(p) == 3 && p[0] == tenant && p[1] != "toolhost" {
			count[p[1]]++
			names = append(names, n)
		}
	}
	best := ""
	for _, n := range names {
		r := strings.Split(n, "/")[1]
		if best == "" || count[r] > count[strings.Split(best, "/")[1]] {
			best = n
		}
	}
	return best
}

// rollCore plays the rollout of F-8: the farm operator drains the core, the running attempts
// finish (or the wait ends), and the core starts again over the same store, which ends the
// drain. The P1 and P2 listeners stay: a restart of the process is what the core's own
// test/process.stop does in place (the epoch, the leases of the downtime, the end of a drain).
func (s *Stand) rollCore(ctx context.Context, run *FaultRun) error {
	var done map[string]any
	if err := s.Bench.P2(ctx, "farm", "core.drain", map[string]any{"until": fmt.Sprintf("%ds", int(drainFor/time.Second))}, &done); err != nil {
		return fmt.Errorf("core.drain: %w", err)
	}
	var mark *time.Time
	if err := s.Bench.Store().Pool.QueryRow(ctx, `SELECT drain_until FROM farm_meta WHERE id = 1`).Scan(&mark); err != nil {
		return err
	}
	if err := s.Bench.Store().Pool.QueryRow(ctx, `SELECT count(*) FROM leases WHERE kind = 'attempt' AND state = 'active'`).Scan(&run.Running); err != nil {
		return err
	}
	run.Drained = mark != nil
	s.log.Addf(KindFault, "F-8: the core drains; waiting up to %s for the running attempts", drainWait)
	running := 0
	err := s.waitFor(ctx, drainWait, func() (bool, error) {
		if e := s.Bench.Store().Pool.QueryRow(ctx, `SELECT count(*) FROM leases WHERE kind = 'attempt' AND state = 'active'`).Scan(&running); e != nil {
			return false, nil
		}
		return running == 0, nil
	})
	// A drain once begun always ends, also when the run ends during the wait; an error of the
	// wait other than its end comes back after the start.
	if errors.Is(err, context.DeadlineExceeded) {
		err = nil
	} else if err != nil {
		s.log.Addf(KindFault, "F-8: the drain wait ended early (%v); the core starts again now", err)
	}
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	var req core.ProcessStopRequest
	req.Process.Kind, req.Mode = "core", "graceful"
	if _, serr := s.Bench.Core().TestProcessStop(sctx, req); serr != nil {
		return errors.Join(fmt.Errorf("the core start: %w", serr), err)
	}
	if err != nil {
		return err
	}
	s.log.Addf(KindFault, "F-8: the core started again (%d attempts still held their leases)", running)
	return nil
}

// FaultCounts are what the provider front and the monitor counted of the faults so far.
func (s *Stand) FaultCounts() map[string]int {
	out := map[string]int{}
	errs, delayed := s.front.counts()
	for code, n := range errs {
		out[fmt.Sprintf("provider_%d", code)] = n
	}
	out["provider_delayed"] = delayed
	out["monitor_refused"], out["webhook_repeated"] = s.Mon.FaultCounts()
	return out
}

// HealFarm ends the farm faults that are still on: the provider front, the monitor outage, the
// webhook twice and the plugin mark. A silent replica starts again on its own time.
func (s *Stand) HealFarm() {
	s.front.set(0, 0, 0, nil, 0)
	s.Mon.mu.Lock()
	s.Mon.downUntil, s.Mon.twice = time.Time{}, false
	s.Mon.mu.Unlock()
	_ = os.Remove(crashFile(s.cfg.Work))
}

// farmFaults plays the farm faults one after another in the home tenant on a person's question
// that reads logs (a plugin tool) and asks the model, and checks each one's expected outcome.
func farmFaults(ctx context.Context, d *Director, sc string) error {
	s := d.S
	home := s.Home()
	defer s.HealFarm()
	ask := func() (string, error) {
		return s.Say("alice", "alice-dm", "/logs Why is the site so slow right now?")
	}
	// leased waits for the task of a message to hold an attempt lease and returns its id.
	leased := func(key string) (task string, err error) {
		err = s.waitFor(ctx, d.Timeout, func() (bool, error) {
			e := s.Bench.Store().Pool.QueryRow(ctx, `SELECT l.work_id FROM leases l JOIN tasks t ON t.id = l.work_id
				WHERE l.kind = 'attempt' AND l.state = 'active' AND t.tenant = $1 AND t.idempotency_key = $2`, home, key).Scan(&task)
			return e == nil, nil
		})
		return
	}
	attempts := func(key string) (n int) {
		_ = s.Bench.Store().Pool.QueryRow(ctx, `SELECT count(*) FROM leases l JOIN tasks t ON t.id = l.work_id WHERE l.kind = 'attempt' AND t.tenant = $1 AND t.idempotency_key = $2`,
			home, key).Scan(&n)
		return
	}

	// F-3: errors of the provider on the next model calls and a share of the others.
	errs0, delayed0 := s.front.counts()
	if _, err := s.InjectFault(ctx, "F-3", home); err != nil {
		return err
	}
	key, err := ask()
	if err != nil {
		return err
	}
	state, reason, _, err := d.taskEnd(ctx, key)
	if err != nil {
		return err
	}
	errs, _ := s.front.counts()
	for c := range errs {
		errs[c] -= errs0[c]
	}
	var provReason string
	_ = s.Bench.Store().Pool.QueryRow(ctx, `SELECT coalesce(record->>'provider_reason', '') FROM tasks WHERE tenant = $1 AND idempotency_key = $2`, home, key).Scan(&provReason)
	// The task itself met the fault (its last provider reason is kept); the run cap may end a
	// task of several calls while the fault lasts (expected), and a provider failure never ends
	// it any other way.
	d.check(sc, "provider_errors_survived", errs[500]+errs[529]+errs[429] > 0 && provReason != "" &&
		(state == "completed" || state == "failed" && reason == "runs_exhausted"),
		"state %s %s (last provider reason %q); answered by the front: 500 %d, 529 %d, 429 %d; attempts %d", state, reason, provReason, errs[500], errs[529], errs[429], attempts(key))
	s.HealFarm()
	if key, err = ask(); err != nil {
		return err
	}
	state, reason, _, err = d.taskEnd(ctx, key)
	if err != nil {
		return err
	}
	d.check(sc, "provider_errors_recovered", state == "completed", "after the fault: state %s %s", state, reason)

	// F-4: a slow provider.
	if _, err := s.InjectFault(ctx, "F-4", home); err != nil {
		return err
	}
	start := time.Now()
	if key, err = ask(); err != nil {
		return err
	}
	// Each model call of the task waits providerDelay, so the task takes minutes.
	state, reason, _, err = d.taskEndWithin(ctx, key, max(d.Timeout, 4*time.Minute))
	if err != nil {
		return err
	}
	_, delayed := s.front.counts()
	delayed -= delayed0
	took := time.Since(start)
	d.check(sc, "provider_slow_survived", state == "completed" && delayed > 0 && took >= providerDelay,
		"state %s %s; %d calls delayed; the task took %s", state, reason, delayed, took.Round(time.Second))
	s.HealFarm()

	// F-2: the plugin process dies in its call.
	if _, err := s.InjectFault(ctx, "F-2", home); err != nil {
		return err
	}
	if key, err = ask(); err != nil {
		return err
	}
	state, reason, _, err = d.taskEnd(ctx, key)
	if err != nil {
		return err
	}
	_, markErr := os.Stat(crashFile(s.cfg.Work))
	var failed int
	_ = s.Bench.Store().Pool.QueryRow(ctx, `SELECT count(*) FROM toolcalls c JOIN tasks t ON t.id = c.task
		WHERE t.tenant = $1 AND t.idempotency_key = $2 AND c.state IN ('error', 'exec_unknown', 'abandoned')`, home, key).Scan(&failed)
	d.check(sc, "plugin_killed_survived", state == "completed" && os.IsNotExist(markErr) && failed > 0,
		"state %s %s; the mark taken %v; tool calls failed %d", state, reason, os.IsNotExist(markErr), failed)
	s.HealFarm()

	// F-1, with F-5 on meanwhile: a replica goes silent holding the lease of a slow call, and
	// the monitoring API is away for two minutes.
	s.front.set(providerDelay+10*time.Second, 0, 0, nil, providerDelay)
	if key, err = ask(); err != nil {
		return err
	}
	task, err := leased(key)
	if err != nil {
		return fmt.Errorf("the task of F-1 got no attempt lease: %w", err)
	}
	run, err := s.injectFault(ctx, "F-1", home, task)
	if err != nil {
		return err
	}
	refused0, _ := s.Mon.FaultCounts()
	if _, err := s.InjectFault(ctx, "F-5", home); err != nil {
		return err
	}
	others := 0
	role := strings.Split(run.Worker, "/")[1]
	for _, n := range s.Bench.Workers() {
		if strings.HasPrefix(n, home+"/"+role+"/") {
			others++
		}
	}
	// With one replica of the role the task waits for its restart.
	state, reason, _, err = d.taskEndWithin(ctx, key, max(d.Timeout, replicaDown+2*time.Minute))
	if err != nil {
		return err
	}
	var ended time.Time
	_ = s.Bench.Store().Pool.QueryRow(ctx, `SELECT updated_at FROM tasks WHERE id = $1`, task).Scan(&ended)
	// The lease term from the expired lease itself (its end less its start, at least the term).
	var expired int
	var termS float64
	_ = s.Bench.Store().Pool.QueryRow(ctx, `SELECT count(*), coalesce(max(extract(epoch FROM lease_until - created_at)), 60)::float8
		FROM leases WHERE work_id = $1 AND kind = 'attempt' AND state = 'expired'`, task).Scan(&expired, &termS)
	leaseTerm := time.Duration(termS * float64(time.Second))
	inTime := others == 0 || ended.Sub(run.At) <= 2*leaseTerm+10*time.Second
	d.check(sc, "replica_silent_task_done", state == "completed" && run.Task == task && expired > 0 && inTime,
		"stopped %s holding %s; state %s %s; expired leases %d; ended %s after the stop (other replicas %d)",
		run.Worker, run.Task, state, reason, expired, ended.Sub(run.At).Round(time.Second), others)
	workerID := ""
	_ = s.Bench.Store().Pool.QueryRow(ctx, `SELECT worker_id FROM leases WHERE work_id = $1 AND kind = 'attempt' ORDER BY lease_no LIMIT 1`, task).Scan(&workerID)
	stale := func(states ...string) (bool, error) {
		var n int
		e := s.Bench.Store().Pool.QueryRow(ctx, `SELECT count(*) FROM alarm_episodes WHERE tenant = $1 AND alarm LIKE 'alarm.worker.stale.%'
			AND subject->>'id' = lower($2) AND opened_at >= $3 AND state = ANY($4)`, home, workerID, run.At, states).Scan(&n)
		return n > 0, e
	}
	err = s.waitFor(ctx, replicaDown+90*time.Second, func() (bool, error) { return stale("firing", "resolved") })
	d.check(sc, "replica_silent_alarm", err == nil, "alarm.worker.stale for %s (%s)", run.Worker, workerID)
	err = s.waitFor(ctx, replicaDown+90*time.Second, func() (bool, error) { return stale("resolved") })
	d.check(sc, "replica_back_alarm_resolved", err == nil, "%s runs again", run.Worker)

	refused, _ := s.Mon.FaultCounts()
	refused -= refused0
	var stalled int
	_ = s.Bench.Store().Pool.QueryRow(ctx, `SELECT count(*) FROM alarm_episodes WHERE tenant = $1 AND alarm LIKE 'alarm.monitor.export_stalled.%' AND opened_at >= $2`,
		home, run.At).Scan(&stalled)
	served := s.Mon.Served()
	err = s.waitFor(ctx, 2*time.Minute, func() (bool, error) { return s.Mon.Served() > served, nil })
	d.check(sc, "monitor_away_recovered", refused > 0 && stalled == 0 && err == nil,
		"refused %d requests while away; export_stalled episodes %d; served again %v", refused, stalled, err == nil)

	// F-6: a problem webhook twice.
	if _, err := s.InjectFault(ctx, "F-6", home); err != nil {
		return err
	}
	web := s.HomeHost(KindWeb)
	if err := d.world("nginx.service on "+web+" failed (the director's fault, for F-6)", func(w *World) {
		sv := w.Service(web, UnitNginx)
		sv.State, sv.Changed = "failed", time.Now().UTC()
	}); err != nil {
		return err
	}
	p, err := d.waitProblem(ctx, "service:"+web+":"+UnitNginx)
	if err != nil {
		return fmt.Errorf("the problem of F-6 did not open: %w", err)
	}
	var events int
	err = s.waitFor(ctx, d.Timeout, func() (bool, error) {
		e := s.Bench.Store().Pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE tenant = $1 AND type = 'ext.monitor.problem.v1'
			AND data->>'eventid' = $2 AND data->>'state' = 'open'`, home, p.EventID).Scan(&events)
		return e == nil && events > 0 && s.Mon.Doubled() == p.EventID, nil
	})
	// The second webhook comes right after the first; a little more time shows any second event.
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(3 * time.Second):
	}
	_ = s.Bench.Store().Pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE tenant = $1 AND type = 'ext.monitor.problem.v1'
		AND data->>'eventid' = $2 AND data->>'state' = 'open'`, home, p.EventID).Scan(&events)
	doubled := s.Mon.Doubled()
	d.check(sc, "webhook_twice_one_event", err == nil && events == 1 && doubled == p.EventID, "event %s: %d problem events; the webhook of event %q went twice", p.EventID, events, doubled)
	if err := d.Heal(ctx); err != nil {
		return err
	}

	// F-7: the store connections cut.
	run, err = s.InjectFault(ctx, "F-7", home)
	if err != nil {
		return err
	}
	if key, err = ask(); err != nil {
		return err
	}
	state, reason, _, err = d.taskEnd(ctx, key)
	if err != nil {
		return err
	}
	d.check(sc, "store_cut_survived", run.Cut > 0 && state == "completed", "%d connections cut; state %s %s", run.Cut, state, reason)

	// F-8: a core rollout while a slow call runs.
	s.front.set(providerDelay+10*time.Second, 0, 0, nil, providerDelay)
	if key, err = ask(); err != nil {
		return err
	}
	if _, err := leased(key); err != nil {
		return fmt.Errorf("the task of F-8 got no attempt lease: %w", err)
	}
	roll, err := s.InjectFault(ctx, "F-8", home)
	if err != nil {
		return err
	}
	var drain *time.Time
	_ = s.Bench.Store().Pool.QueryRow(ctx, `SELECT drain_until FROM farm_meta WHERE id = 1`).Scan(&drain)
	state, reason, _, err = d.taskEnd(ctx, key)
	if err != nil {
		return err
	}
	s.HealFarm()
	after, err := ask()
	if err != nil {
		return err
	}
	astate, areason, _, err := d.taskEnd(ctx, after)
	if err != nil {
		return err
	}
	d.check(sc, "core_rollout", roll.Drained && roll.Running > 0 && drain == nil && state == "completed" && astate == "completed",
		"drain set %v with %d attempts running, ended %v; the task across the drain %s %s; a task after the start %s %s",
		roll.Drained, roll.Running, drain == nil, state, reason, astate, areason)
	return nil
}

// CrashOnCall is the farm fault F-2 in a plugin process: when a tool call comes in and the mark
// is there, the process that removes the mark kills itself (SIGKILL), so the call is in flight
// when it dies. A process that serves no tool call (a tool list) leaves the mark alone.
func CrashOnCall() {
	mark := os.Getenv(CrashEnv)
	if mark == "" {
		return
	}
	if _, err := os.Stat(mark); err != nil {
		return
	}
	in := os.Stdin
	r, w, err := os.Pipe()
	if err != nil {
		return
	}
	os.Stdin = r
	go func() {
		br := bufio.NewReader(in)
		for {
			line, err := br.ReadBytes('\n')
			if bytes.Contains(line, []byte(`"tools/call"`)) && os.Remove(mark) == nil {
				_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
			}
			if _, werr := w.Write(line); werr != nil || err != nil {
				_ = w.Close()
				return
			}
		}
	}()
}
