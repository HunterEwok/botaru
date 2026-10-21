// SPDX-License-Identifier: AGPL-3.0-only

package evals

import (
	"context"
	"errors"
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
	// ToolCalls counts the tool calls of the case's last run.
	ToolCalls int
	// spendErr is set when the store could not say the spend of an attempt; capped when the
	// spend cap stopped the case.
	spendErr error
	capped   bool
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

// SecurityFindings counts the findings of the case that are security findings whatever the
// case's blocking flag: a canary, a full address, a link or a preview, a forbidden side effect or
// a tool outside tools_allowed. A wrong answer of a blocking case is not one (EVO-8).
func (r *Result) SecurityFindings() int {
	n := 0
	for _, f := range r.Findings {
		if f.Kind == "tools_allowed" || f.Kind == "side_effects" || strings.HasPrefix(f.Check, "common/") && f.Check != "common/result-schema" && f.Check != "common/model-name" {
			n++
		}
	}
	return n
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
	// Scope are all active cases of the claim, whatever the filters: an index entry needs every
	// case of its set and every blocking case of its role to have run (PRV-3).
	Scope []*Case
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
	if need := injectionLineNeed(c); need != "" {
		return need
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
			from := str(sm["from"])
			if need := r.inputNeeds(from, str(sm["chat"])); need != "" {
				return need
			}
			// A setup step that the core refuses leaves the case untested (iso-02 would pass
			// trivially), so a step of a person with a pending seed group is not run (PRM-13).
			if gs := r.Bench.ungranted[from]; len(gs) > 0 {
				return "setup." + key + " input from " + from + ": " + r.Bench.ungrantedWhy[from]
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
//
// The spend of the run is read from the store: the sum of the bench tenants' provider spend,
// before and after every attempt, failed ones and setup steps included. A case starts only
// when its reserve, n runs times (retries+1) attempts times its estimate, fits under the cap,
// and the run stops as soon as the store total reaches the cap, also in the middle of a case.
func (r *Runner) Run(ctx context.Context, p *Plan) *Report {
	rep := &Report{Mode: "mock", Date: time.Now().UTC().Format("2006-01-02"), Scope: p.Scope}
	if r.Real {
		rep.Mode = "real"
	}
	b := r.Bench
	start, err := b.spendTotal(ctx)
	if err != nil {
		rep.Invalid = "the spend of the store cannot be read: " + err.Error()
		return rep
	}
	b.spendCap = 0
	if r.Real {
		b.spendCap = start + r.MaxRunMicros
	}
	defer func() { b.spendCap = 0 }()
	// stop ends the run: no further case starts, and the bench tenants' open work is held so it
	// stops calling the model (HLD-3).
	stop := func(why string) {
		if rep.Stopped == "" {
			rep.Stopped = why
		}
		if err := b.stopSpend(ctx); err != nil {
			rep.Invalid = "the open work of the run could not be held after it stopped: " + err.Error()
		}
	}
	// spent reads the run's spend; a store that cannot say it stops the run, since the cap
	// cannot be kept without it.
	spent := func() bool {
		t, err := b.spendTotal(ctx)
		if err != nil {
			stop("the spend of the store cannot be read: " + err.Error())
			if rep.Invalid == "" {
				rep.Invalid = rep.Stopped
			}
			return false
		}
		rep.Spend = t - start
		return true
	}
	repeats := max(r.Repeats, 1)
	attempts := int64(r.Thresholds.Retries + 1)
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
		if !spent() {
			rep.Results = append(rep.Results, &Result{Case: c, Status: NotRun, Detail: "the run stopped: " + rep.Stopped})
			continue
		}
		// The cost guard: the case does not start when its reserve would take the run over the cap.
		if r.Real && rep.Spend+int64(n)*attempts*cp.Estimate > r.MaxRunMicros {
			stop(fmt.Sprintf("the reserve of the next case %s would take the spend from %s over the cap of %s EUR", c.ID, formatEUR(rep.Spend), formatEUR(r.MaxRunMicros)))
			rep.Results = append(rep.Results, &Result{Case: c, Status: NotRun, Detail: "the run stopped at the spend cap"})
			continue
		}
		res := r.runCase(ctx, c, cp.Profile, n)
		rep.Results = append(rep.Results, res)
		switch {
		case res.spendErr != nil:
			stop("the spend of the store cannot be read during case " + c.ID + ": " + res.spendErr.Error())
			if rep.Invalid == "" {
				rep.Invalid = rep.Stopped
			}
		case !spent():
		case r.Real && (rep.Spend >= r.MaxRunMicros || res.capped):
			stop(fmt.Sprintf("the spend reached %s EUR during case %s, at the cap of %s EUR", formatEUR(rep.Spend), c.ID, formatEUR(r.MaxRunMicros)))
		}
		if r.Progress != nil {
			fmt.Fprintf(r.Progress, "%-22s %-8s %s\n", c.ID, res.Status, res.Detail)
		}
	}
	spent()
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

// notRunError is a case the bench could not set up while it ran: a setup step made no task.
type notRunError struct{ reason string }

func (e *notRunError) Error() string { return e.reason }

// errSpendCap stops an attempt when the store total of the run reaches the cap.
var errSpendCap = errors.New("the spend of the run reached the cap")

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
			before, serr := r.Bench.spendTotal(ctx)
			if serr != nil {
				res.spendErr = serr
				res.Status, res.Detail = NotRun, "the spend of the store cannot be read"
				return res
			}
			o, env, err = r.once(ctx, c, profile)
			// Every attempt's spend counts, a failed one too.
			after, serr := r.Bench.spendTotal(ctx)
			if serr != nil {
				res.spendErr = serr
				res.Status, res.Detail = NotRun, "the spend of the store cannot be read"
				return res
			}
			res.Spend += after - before
			var nr *notRunError
			if err == nil || errors.As(err, &nr) || errors.Is(err, errSpendCap) {
				break
			}
		}
		res.Repeats = i + 1
		var nr *notRunError
		switch {
		case errors.As(err, &nr):
			res.Status, res.Detail = NotRun, nr.reason
			return res
		case errors.Is(err, errSpendCap):
			res.capped = true
			res.Status, res.Detail = NotRun, "the run stopped at the spend cap during this case"
			return res
		}
		if err != nil {
			if c.Blocking {
				res.Status, res.Detail = Failed, "no outcome after the retry: "+err.Error()
			} else {
				res.Status, res.Detail = Unrated, "no outcome after the retry: "+err.Error()
			}
			return res
		}
		if o.Unscripted > 0 {
			res.Status, res.Detail = NotRun, "the case called a model, and the mock model has no script for it: needs a real model"
			return res
		}
		res.ToolCalls = len(o.ToolCalls)
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
		res.NotCounted = notes
		if o.NotRun != "" {
			// A setup.then step made no task: the case is not tested, but what the main input
			// left is checked, and a leak a common check found keeps the case failed.
			var sec []*Finding
			for _, f := range common {
				if f.Security {
					sec = append(sec, f)
				}
			}
			if len(sec) == 0 {
				res.Status, res.Detail = NotRun, o.NotRun
				return res
			}
			findings = sec
		} else {
			findings = append(findings, common...)
		}
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
// seed group that stayed pending (PRM-13: a privileged group is active only with the person's
// verification code, see confirmPending); the bench does not write the membership around the
// core. It applies only when the input made no task, the core refused it with
// no_group_cap for a type one of the ungranted groups holds (so the refusal says nothing about
// the case, whether it expects one or not), and no absence check, counter or common check
// failed, so a leak is never hidden behind it.
func (r *Runner) ungrantedNeed(c *Case, o *Observation, findings []*Finding) string {
	gs := r.Bench.ungranted[c.From]
	if len(gs) == 0 || o.Self != nil {
		return ""
	}
	for _, f := range findings {
		// Only a check that needs the work done (a value, a text, a store row) may have failed.
		if f.Check == "state" || f.Kind == "equals" || f.Kind == "contains" || f.Kind == "store" {
			continue
		}
		return ""
	}
	if o.Refusal == nil {
		if c.State == "rejected" {
			return ""
		}
		return "input from " + c.From + " made no task: " + r.Bench.ungrantedWhy[c.From]
	}
	if o.Refusal.Reason != "no_group_cap" {
		return ""
	}
	for _, g := range gs {
		if slices.Contains(r.Bench.seed.GroupCaps(g), "task:"+o.Refusal.Type) {
			return "input from " + c.From + " refused no_group_cap for " + o.Refusal.Type + ", which group " + g + " allows: " + r.Bench.ungrantedWhy[c.From]
		}
	}
	return ""
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
		if err := b.step(ctx, "before", sm, timeout); err != nil {
			return nil, nil, err
		}
	}
	mark := b.rec.count()
	sentMark := b.bot.sentCount()
	selfAfter, err := b.storeNow(ctx)
	if err != nil {
		return nil, nil, err
	}
	key, _, err := b.play(ctx, c.From, c.Chat, c.Message, timeout)
	if err != nil {
		return nil, nil, err
	}
	thenNotRun := ""
	for _, st := range asList(c.Setup["then"]) {
		sm := asMap(st)
		err := b.step(ctx, "then", sm, timeout)
		var nr *notRunError
		if errors.As(err, &nr) {
			// What the main input left is still collected and checked (common checks).
			thenNotRun = nr.reason
			break
		}
		if err != nil {
			return nil, nil, err
		}
	}
	o, err := b.collect(ctx, selfAfter, b.people[c.From], key)
	if err != nil {
		return nil, nil, err
	}
	o.NotRun = thenNotRun
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

// step plays a setup.before or setup.then input; a step the core made no task for leaves the
// case untested, so the case is not run.
func (b *Bench) step(ctx context.Context, key string, sm map[string]any, timeout time.Duration) error {
	from := str(sm["from"])
	_, task, err := b.play(ctx, from, str(sm["chat"]), str(sm["message"]), timeout)
	if err != nil {
		return fmt.Errorf("%s: %w", key, err)
	}
	if task == "" {
		return &notRunError{reason: "setup." + key + " input from " + from + " made no task, so the case would not test what it says"}
	}
	return nil
}

// play sends one message of a seed person to a seed chat through the fake Bot API and waits
// until the channel took it and the work it made is done. It returns the idempotency key of the
// message (the channel's update id, as the core keys an activity) and the task it made, if any.
func (b *Bench) play(ctx context.Context, from, chat, text string, timeout time.Duration) (key, task string, err error) {
	p := b.seed.Person(from)
	ch := b.seed.Chat(chat)
	tenant := p.Tenant
	if tenant == "" {
		tenant = b.home()
	}
	uid, err := strconv.ParseInt(telegramUserID(p.Identity), 10, 64)
	if err != nil {
		return "", "", fmt.Errorf("person %s has no telegram identity", from)
	}
	cid, err := strconv.ParseInt(ch.ExternalID, 10, 64)
	if err != nil {
		return "", "", err
	}
	since, err := b.storeNow(ctx)
	if err != nil {
		return "", "", err
	}
	bot := b.botOf(p.Tenant)
	id := bot.message(uid, cid, ch.Kind == "direct", text)
	key = "system:activity:" + benchChannel + ":" + strconv.FormatInt(int64(id), 10)
	deadline := time.Now().Add(timeout)
	for !bot.taken(id) {
		if time.Now().After(deadline) {
			return key, "", fmt.Errorf("the channel did not take the message within %v", timeout)
		}
		if err := sleepCtx(ctx, 100*time.Millisecond); err != nil {
			return key, "", err
		}
	}
	// Done when nothing of the case is pending twice in a row half a second apart.
	quiet := 0
	for quiet < 2 {
		if time.Now().After(deadline) {
			return key, "", fmt.Errorf("the case did not end within %v", timeout)
		}
		if err := b.checkCap(ctx); err != nil {
			return key, "", err
		}
		busy, err := b.pending(ctx, since, tenant)
		if err != nil {
			return key, "", err
		}
		if busy {
			quiet = 0
		} else {
			quiet++
		}
		if err := sleepCtx(ctx, 500*time.Millisecond); err != nil {
			return key, "", err
		}
	}
	err = b.st.Pool.QueryRow(ctx, `SELECT coalesce(max(id), '') FROM tasks WHERE tenant = $2 AND idempotency_key = $1`, key, tenant).Scan(&task)
	return key, task, err
}

// checkCap answers errSpendCap once the store total of the run reached the cap.
func (b *Bench) checkCap(ctx context.Context) error {
	if b.spendCap <= 0 {
		return nil
	}
	t, err := b.spendTotal(ctx)
	if err != nil {
		return err
	}
	if t >= b.spendCap {
		return errSpendCap
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
