// SPDX-License-Identifier: AGPL-3.0-only

package evals

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Status is the outcome of one case.
type Status string

const (
	Passed Status = "passed"
	Failed Status = "failed"
	// NotRun is a case the runner did not run: it needs a real model, a part of the bench that
	// is not built, or the run stopped at the spend cap. It never counts as passed.
	NotRun Status = "not_run"
	// Unrated is a case without an outcome: a timeout or an infrastructure failure after the
	// retry, or a case more expensive than budget.max_eur_per_case (docs/quality.md).
	Unrated Status = "unrated"
	// Skipped is a case of a later version or pending.
	Skipped Status = "skipped"
)

// Result is the outcome of one case.
type Result struct {
	Case     *Case
	Status   Status
	Detail   string
	Findings []*Finding
	// Repeats is how many runs the case had; a blocking case passes only if all passed.
	Repeats int
	Spend   int64
	// NotCounted are the side effect counters this runner does not count.
	NotCounted []string
	Seconds    float64
}

// Security says whether the case failed against the zero security threshold.
func (r *Result) Security() bool {
	if r.Status != Failed {
		return false
	}
	if r.Case.Blocking {
		return true
	}
	return slices.ContainsFunc(r.Findings, func(f *Finding) bool { return f.Security })
}

// Report is the outcome of a run.
type Report struct {
	Mode    string // mock or real
	Date    string
	Results []*Result
	Spend   int64
	// Stopped is set when the spend cap stopped the run: the outcome is "not finished".
	Stopped string
	// Invalid is set when the run is invalid: too many unrated cases, or a model name that
	// differs from the profile.
	Invalid string
}

// Counts returns the number of results per status and the security failures.
func (r *Report) Counts() (map[Status]int, int) {
	n := map[Status]int{}
	sec := 0
	for _, x := range r.Results {
		n[x.Status]++
		if x.Security() {
			sec++
		}
	}
	return n, sec
}

// Runner drives cases on a bench.
type Runner struct {
	Root       string
	Bench      *Bench
	Real       bool
	Thresholds *Thresholds
	Prices     map[string]Price
	// MaxRunMicros is the spend cap of the run (--max-eur); at most budget.max_eur_per_run.
	MaxRunMicros int64
	// Repeats of a blocking case; a quality case runs once.
	Repeats int
	// Progress gets one line per case.
	Progress io.Writer
	// BeforeCase, when set, runs before each run of a case (a test seam).
	BeforeCase func(*Case)
}

// allowedSetup are the setup keys the runner builds.
var allowedSetup = []string{"checks_stub", "log_defects", "noise", "inject_lang", "then", "same_worker", "before", "role"}

// Needs returns why the runner cannot run a case on this bench in this mode, or "".
func (r *Runner) Needs(c *Case) string {
	if _, ok := benchRoles[c.Role]; !ok {
		return "role " + c.Role + " is not on the test bench (the bench sets up hello and ops-reader)"
	}
	if len(c.Inbox) > 0 {
		return "inbox entries before the run are not seeded by the runner"
	}
	if c.From == "system" || c.Task != nil {
		return "a system or task input is not built: P1 enqueue of an external program is not in the core, and the runner seeds no schedule or route"
	}
	if need := r.inputNeeds(c.From, c.Chat); need != "" {
		return need
	}
	if gen := str(c.Data["gen"]); gen != "" && gen != "nginx" && gen != "app" {
		return "generator " + gen + " is not built"
	}
	for _, in := range injections(c) {
		where := str(in["where"])
		src := injectSource(str(c.Data["gen"]), where)
		if src == "" {
			return "injection into " + where + " is not built"
		}
	}
	for _, k := range slices.Sorted(mapKeys(c.Setup)) {
		if !slices.Contains(allowedSetup, k) {
			return "setup." + k + " is not built in the runner"
		}
	}
	if role := asMap(c.Setup["role"]); role != nil {
		// The example role and catalog already hold these values; the runner changes no
		// manifest per case.
		if !slices.Equal(strs(role["service_control"]), []string{"nginx"}) || !slices.Equal(strs(role["monitor_scripts"]), []string{"nginx-cache-clear"}) ||
			!slices.Equal(strs(asMap(role["catalog_no_card"])["nginx"]), []string{"restart"}) || len(role) != 3 {
			return "setup.role differs from the example role, and the runner changes no manifest per case"
		}
	}
	for _, key := range []string{"before", "then"} {
		for _, st := range asList(c.Setup[key]) {
			sm := asMap(st)
			if after := str(sm["after"]); after != "" && after != "0m" {
				return "a follow-up after " + after + " needs the store clock to move, which the runner does not do"
			}
			if need := r.inputNeeds(str(sm["from"]), str(sm["chat"])); need != "" {
				return need
			}
		}
	}
	for _, ch := range c.Checks {
		if u := unsupportedCheck(ch); u != "" {
			return u
		}
	}
	if !r.Real && usesModel(c) {
		return "needs a real model: run with a provider key"
	}
	if r.Real && !usesModel(c) && str(c.Setup["model"]) == "mock" {
		return "a mock-scripted case runs in the mock run"
	}
	return ""
}

func (r *Runner) inputNeeds(from, chat string) string {
	p := r.Bench.seed.Person(from)
	if p == nil || p.Kind != "human" || !p.Subject || p.Tenant != "demo" {
		return "input from " + from + ": only people of demo with a subject record are seeded"
	}
	ch := r.Bench.seed.Chat(chat)
	if ch == nil || ch.Kind != "direct" {
		return "chat " + chat + ": group chats are not bound on the bench"
	}
	return ""
}

func injections(c *Case) []map[string]any {
	inj := c.Data["inject"]
	if m := asMap(inj); m != nil {
		return []map[string]any{m}
	}
	var out []map[string]any
	for _, x := range asList(inj) {
		out = append(out, asMap(x))
	}
	return out
}

// Run runs the plan's cases one after another (the workers are not restarted between cases)
// and applies the thresholds.
func (r *Runner) Run(ctx context.Context, p *Plan) *Report {
	rep := &Report{Mode: "mock", Date: time.Now().UTC().Format("2006-01-02")}
	if r.Real {
		rep.Mode = "real"
	}
	repeats := max(r.Repeats, 1)
	for _, cp := range p.Cases {
		c := cp.Case
		if need := r.Needs(c); need != "" {
			rep.Results = append(rep.Results, &Result{Case: c, Status: NotRun, Detail: need})
			continue
		}
		if rep.Stopped != "" {
			rep.Results = append(rep.Results, &Result{Case: c, Status: NotRun, Detail: "the run stopped at the spend cap"})
			continue
		}
		n := 1
		if c.Blocking {
			n = repeats
		}
		// The cost guard: the case does not start when its reserve would take the run over the cap.
		if r.Real && rep.Spend+int64(n)*cp.Estimate > r.MaxRunMicros {
			rep.Stopped = fmt.Sprintf("the next case %s would take the spend from %s over the cap of %s EUR", c.ID, formatEUR(rep.Spend), formatEUR(r.MaxRunMicros))
			rep.Results = append(rep.Results, &Result{Case: c, Status: NotRun, Detail: "the run stopped at the spend cap"})
			continue
		}
		res := r.runCase(ctx, c, cp.Profile, n)
		rep.Spend += res.Spend
		rep.Results = append(rep.Results, res)
		if r.Progress != nil {
			fmt.Fprintf(r.Progress, "%-22s %-8s %s\n", c.ID, res.Status, res.Detail)
		}
	}
	for _, c := range p.Later {
		rep.Results = append(rep.Results, &Result{Case: c, Status: Skipped, Detail: "the case is of a later version or pending"})
	}
	counts, _ := rep.Counts()
	if rated := counts[Passed] + counts[Failed] + counts[Unrated]; rated > 0 && float64(counts[Unrated])/float64(rated) > r.Thresholds.MaxUnratedShare {
		rep.Invalid = fmt.Sprintf("%d of %d cases not rated, above infra.max_unrated_share", counts[Unrated], rated)
	}
	for _, x := range rep.Results {
		for _, f := range x.Findings {
			if f.Check == "common/model-name" {
				rep.Invalid = "a model name in the provider's answer differs from the profile: " + f.Detail
			}
		}
	}
	return rep
}

// runCase runs a case n times; every run must pass. A run without an outcome is retried once
// (infra.retries); still none is a failure for a blocking case and unrated otherwise.
func (r *Runner) runCase(ctx context.Context, c *Case, profile string, n int) *Result {
	res := &Result{Case: c, Status: Passed}
	start := time.Now()
	defer func() { res.Seconds = time.Since(start).Seconds() }()
	for i := 0; i < n; i++ {
		var o *Observation
		var env *checkEnv
		var err error
		for try := 0; try <= r.Thresholds.Retries; try++ {
			o, env, err = r.once(ctx, c, profile)
			if err == nil {
				break
			}
		}
		res.Repeats = i + 1
		if err != nil {
			if c.Blocking {
				res.Status, res.Detail = Failed, "no outcome after the retry: "+err.Error()
			} else {
				res.Status, res.Detail = Unrated, "no outcome after the retry: "+err.Error()
			}
			return res
		}
		res.Spend += o.Spend
		if o.Unscripted > 0 {
			res.Status, res.Detail = NotRun, "the case called a model, and the mock model has no script for it: needs a real model"
			return res
		}
		var findings []*Finding
		if got := o.state(); got != c.State {
			findings = append(findings, &Finding{Check: "state", Detail: fmt.Sprintf("state %s, want %s", got, c.State), Security: c.Blocking})
		}
		for _, ch := range c.Checks {
			if f := env.run(o, ch); f != nil {
				findings = append(findings, f)
			}
		}
		common, notes := env.common(o)
		findings = append(findings, common...)
		res.NotCounted = notes
		if need := r.ungrantedNeed(c, o, findings); need != "" {
			res.Status, res.Detail = NotRun, need
			return res
		}
		if len(findings) > 0 {
			res.Status, res.Findings = Failed, findings
			parts := make([]string, len(findings))
			for j, f := range findings {
				parts[j] = f.Check + ": " + f.Detail
			}
			res.Detail = fmt.Sprintf("run %d: %s", i+1, strings.Join(parts, "; "))
			return res
		}
	}
	if res.Spend > r.Thresholds.MaxCaseMicros && r.Real {
		res.Status, res.Detail = Unrated, fmt.Sprintf("spent %s EUR, above budget.max_eur_per_case", formatEUR(res.Spend))
	}
	return res
}

// ungrantedNeed is the not-run reason of a case the core refused because the person lacks a
// seed group the bench could not grant: PRM-13 refuses a privileged group without the
// verification code flow, which this version does not have, and the bench does not write the
// membership around it. It applies only when the input made no task and found no
// failure of an absence check, counter or common check, so a leak is never hidden behind it.
func (r *Runner) ungrantedNeed(c *Case, o *Observation, findings []*Finding) string {
	gs := r.Bench.ungranted[c.From]
	if len(gs) == 0 || o.Self != nil || c.State == "rejected" {
		return ""
	}
	for _, f := range findings {
		// Only a check that needs the work done (a value, a text, a store row) may have failed;
		// an absence check, a counter or a common check that failed keeps the case failed.
		if f.Check == "state" || f.Kind == "equals" || f.Kind == "contains" || f.Kind == "store" {
			continue
		}
		return ""
	}
	return "input from " + c.From + " refused: seed groups " + strings.Join(gs, ", ") + " are privileged and cannot be granted without the verification code flow (PRM-13)"
}

// once prepares the data of a case, plays its inputs and collects what it left.
func (r *Runner) once(ctx context.Context, c *Case, profile string) (*Observation, *checkEnv, error) {
	b := r.Bench
	if err := b.resetData(); err != nil {
		return nil, nil, err
	}
	answers := map[string]any{}
	var ips []string
	if gen := str(c.Data["gen"]); gen != "" {
		seed, _ := num(c.Data["seed"])
		opt := genOptions{defects: strs(c.Setup["log_defects"]), noise: str(c.Setup["noise"]), inject: injections(c)}
		g, err := Generate(filepath.Join(b.dataDir, "web1"), gen, int64(seed), time.Now(), opt)
		if err != nil {
			return nil, nil, err
		}
		answers, ips = g.Answers, g.IPs
	}
	if st := asMap(c.Setup["checks_stub"]); st != nil {
		v := map[string]any{}
		for _, k := range []string{"ok", "total", "last_status", "ms_max"} {
			if x, ok := st[k]; ok {
				v[k] = x
			}
		}
		// An injected HTTP body has nowhere to go: the checks plugin returns numbers only.
		if err := b.writeChecks(map[string]any{str(st["check"]): v}); err != nil {
			return nil, nil, err
		}
	}
	if b.mock != nil {
		b.mock.set(nil)
	}
	if r.BeforeCase != nil {
		r.BeforeCase(c)
	}
	timeout := time.Duration(r.Thresholds.CaseTimeoutS) * time.Second
	for _, st := range asList(c.Setup["before"]) {
		sm := asMap(st)
		if err := b.play(ctx, str(sm["from"]), str(sm["chat"]), str(sm["message"]), timeout); err != nil {
			return nil, nil, fmt.Errorf("before: %w", err)
		}
	}
	mark := b.rec.count()
	sentMark := b.bot.sentCount()
	selfAfter, err := b.storeNow(ctx)
	if err != nil {
		return nil, nil, err
	}
	if err := b.play(ctx, c.From, c.Chat, c.Message, timeout); err != nil {
		return nil, nil, err
	}
	for _, st := range asList(c.Setup["then"]) {
		sm := asMap(st)
		if err := b.play(ctx, str(sm["from"]), str(sm["chat"]), str(sm["message"]), timeout); err != nil {
			return nil, nil, fmt.Errorf("then: %w", err)
		}
	}
	o, err := b.collect(ctx, selfAfter, selfAfter)
	if err != nil {
		return nil, nil, err
	}
	o.Sent = b.bot.sentSince(sentMark)
	o.ModelCalls = b.rec.since(mark)
	o.Answers, o.DataIPs = answers, ips
	if b.mock != nil {
		o.Unscripted = b.mock.unscriptedCalls()
	}
	env := &checkEnv{c: c, people: b.people, canaries: b.seed.Canaries(c.From), linkAllowlist: b.linkAllowlist}
	if o.Self != nil {
		env.resultSchema = b.resultSchemas[o.Self.Type]
	}
	if r.Real {
		env.profileModel = b.Profiles[profile]
	}
	return o, env, nil
}

// play sends one message of a seed person to a seed chat through the fake Bot API and waits
// until the channel took it and the work it made is done.
func (b *Bench) play(ctx context.Context, from, chat, text string, timeout time.Duration) error {
	p := b.seed.Person(from)
	ch := b.seed.Chat(chat)
	uid, err := strconv.ParseInt(telegramUserID(p.Identity), 10, 64)
	if err != nil {
		return fmt.Errorf("person %s has no telegram identity", from)
	}
	cid, err := strconv.ParseInt(ch.ExternalID, 10, 64)
	if err != nil {
		return err
	}
	since, err := b.storeNow(ctx)
	if err != nil {
		return err
	}
	id := b.bot.message(uid, cid, ch.Kind == "direct", text)
	deadline := time.Now().Add(timeout)
	for !b.bot.taken(id) {
		if time.Now().After(deadline) {
			return fmt.Errorf("the channel did not take the message within %v", timeout)
		}
		if err := sleepCtx(ctx, 100*time.Millisecond); err != nil {
			return err
		}
	}
	// Done when nothing of the case is pending twice in a row half a second apart.
	quiet := 0
	for quiet < 2 {
		if time.Now().After(deadline) {
			return fmt.Errorf("the case did not end within %v", timeout)
		}
		busy, err := b.pending(ctx, since)
		if err != nil {
			return err
		}
		if busy {
			quiet = 0
		} else {
			quiet++
		}
		if err := sleepCtx(ctx, 500*time.Millisecond); err != nil {
			return err
		}
	}
	return nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
