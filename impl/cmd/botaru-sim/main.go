// SPDX-License-Identifier: AGPL-3.0-only

// Command botaru-sim runs the farm simulator (impl/SIM.md): the core, the channel, the bots and
// the toolhosts of a test bench whose tools work on a virtual world under the work directory,
// with the mock model or a real model, a director that breaks the world and plays the people,
// and a hard money fuse.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/rand/v2"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/ids"
	"github.com/HunterEwok/botaru/impl/internal/money"
	"github.com/HunterEwok/botaru/impl/internal/pluginmain"
	"github.com/HunterEwok/botaru/impl/internal/sim"
	"github.com/HunterEwok/botaru/impl/internal/spec"
	"github.com/HunterEwok/botaru/impl/internal/version"
)

// keyEnv is the environment variable of the provider key.
const keyEnv = "BOTARU_SIM_PROVIDER_KEY"

func main() {
	// A stub of the world (systemctl, df, journalctl) runs this binary with the stub's name in
	// its environment; it answers from the world and never runs the real program.
	if ok, code := sim.StubFromEnv(os.Args[1:], os.Stdout); ok {
		os.Exit(code)
	}
	// The toolhosts start the own plugins as children of this binary (DEP-7).
	if len(os.Args) > 1 && os.Args[1] == "plugin" {
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: botaru-sim plugin KIND [flags]")
			os.Exit(2)
		}
		pluginmain.Exit("botaru-sim plugin", pluginmain.Main(os.Args[2], os.Args[3:]))
	}
	// The plugins of the data bots answer from the world's data files.
	if len(os.Args) > 1 && os.Args[1] == "simplugin" {
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: botaru-sim simplugin KIND -manifest FILE -world DIR")
			os.Exit(2)
		}
		pluginmain.Exit("botaru-sim simplugin", sim.ServePlugin(os.Args[2], os.Args[3:]))
	}
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// prices are the list prices of models in USD per million tokens (input, output); the
// simulator counts them as EUR, which overstates the spend a little and so trips the fuse
// early rather than late. -price-in and -price-out override them.
var prices = map[string][2]float64{
	"claude-opus-5-5":            {5, 25},
	"claude-sonnet-5-5":          {3, 15},
	"claude-haiku-5-5":           {1, 5},
	"claude-opus-4-1-20250805":   {15, 75},
	"claude-opus-4-20250514":     {15, 75},
	"claude-sonnet-4-5-20250929": {3, 15},
	"claude-sonnet-4-20250514":   {3, 15},
	"claude-haiku-4-5-20251001":  {1, 5},
	"claude-3-5-haiku-20241022":  {0.8, 4},
}

// priceOf finds the price of a model by its exact id, then by its family.
func priceOf(model string) ([2]float64, bool) {
	if p, ok := prices[model]; ok {
		return p, true
	}
	for _, fam := range []struct {
		part string
		p    [2]float64
	}{{"opus", [2]float64{15, 75}}, {"sonnet", [2]float64{3, 15}}, {"haiku", [2]float64{1, 5}}} {
		if strings.Contains(model, fam.part) {
			return fam.p, true
		}
	}
	return [2]float64{}, false
}

type options struct {
	root, dsn, out, scenario, approve, model, keyFile, providerURL string
	mock, real, interactive, keepDB, verbose, showVersion          bool
	maxEUR, priceIn, priceOut                                      float64
	duration, faultEvery, step, timeout                            time.Duration
	seed                                                           int64
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	var o options
	fs := flag.NewFlagSet("botaru-sim", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&o.root, "root", "", "the skeleton root (default: found from the working directory)")
	fs.StringVar(&o.dsn, "dsn", firstEnv("BOTARU_SIM_DSN", "BOTARU_EVAL_DSN"), "a PostgreSQL server; the run creates a fresh database on it and drops it afterwards ($BOTARU_SIM_DSN)")
	fs.StringVar(&o.out, "out", "./sim-out", "the output directory; each run writes a folder run-<time> in it with the world, sim-events.jsonl and results.json")
	fs.StringVar(&o.scenario, "scenario", "", "scenarios to run, comma separated, or all ("+strings.Join(scenarioNames(), ", ")+")")
	fs.StringVar(&o.approve, "auto-approve", "safe", "the policy of the virtual approvers for approval cards: none, safe, all or random")
	fs.BoolVar(&o.mock, "mock", false, "answer with the built-in mock model (the default)")
	fs.BoolVar(&o.real, "real", false, "call the real model provider with the key from $"+keyEnv+" or -key-file; needs -model and -max-eur")
	fs.StringVar(&o.model, "model", "", "the model of every profile in a real run, for example claude-haiku-4-5-20251001")
	fs.StringVar(&o.keyFile, "key-file", "", "a file holding the provider key, instead of $"+keyEnv)
	fs.StringVar(&o.providerURL, "provider-url", "", "the provider address (default: the binding's own)")
	fs.Float64Var(&o.maxEUR, "max-eur", 0, "the money fuse in EUR: no model call whose reserved worst case would reach this spend goes out; at it every tenant is held and the run stops (required with -real; 1.00 for -mock)")
	fs.Float64Var(&o.priceIn, "price-in", 0, "the input price in EUR per million tokens (default: the list price of -model)")
	fs.Float64Var(&o.priceOut, "price-out", 0, "the output price in EUR per million tokens (default: the list price of -model)")
	fs.BoolVar(&o.interactive, "interactive", false, "the continuous mode with commands on the standard input (approve N, reject N, say, fault, status, quit)")
	fs.DurationVar(&o.duration, "duration", 0, "how long the continuous mode runs (0 with -interactive: until quit, Ctrl+C or the fuse)")
	fs.DurationVar(&o.faultEvery, "fault-every", 3*time.Minute, "the continuous mode breaks the world this often")
	fs.DurationVar(&o.step, "step", 5*time.Second, "the period of the world physics")
	fs.DurationVar(&o.timeout, "timeout", 3*time.Minute, "how long a scenario waits for each step")
	fs.Int64Var(&o.seed, "seed", 42, "the seed of the generated logs, faults and random decisions")
	fs.BoolVar(&o.keepDB, "keep-db", false, "keep the run's database for a look afterwards")
	fs.BoolVar(&o.verbose, "v", false, "log the bench to the standard error")
	fs.BoolVar(&o.showVersion, "version", false, "print the version")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintln(stderr, "botaru-sim: unexpected arguments:", strings.Join(fs.Args(), " "))
		return 2
	}
	if o.showVersion {
		fmt.Fprintln(stdout, version.String("botaru-sim"))
		return 0
	}
	code, err := runWith(o, stdin, stdout, stderr)
	if err != nil {
		fmt.Fprintln(stderr, "botaru-sim:", err)
		if code == 0 {
			code = 1
		}
	}
	return code
}

func firstEnv(names ...string) string {
	for _, n := range names {
		if v := os.Getenv(n); v != "" {
			return v
		}
	}
	return ""
}

func scenarioNames() []string {
	var out []string
	for _, s := range sim.Scenarios {
		out = append(out, s.Name)
	}
	return out
}

func split(s string) []string {
	var out []string
	for _, x := range strings.Split(s, ",") {
		if x = strings.TrimSpace(x); x != "" {
			out = append(out, x)
		}
	}
	return out
}

// check validates the options and fills the defaults.
func (o *options) check() (int, error) {
	if o.mock && o.real {
		return 2, errors.New("-mock and -real exclude each other")
	}
	if !slices.Contains(sim.Policies, o.approve) {
		return 2, fmt.Errorf("-auto-approve is one of %s", strings.Join(sim.Policies, ", "))
	}
	if o.maxEUR < 0 || math.IsNaN(o.maxEUR) || math.IsInf(o.maxEUR, 0) {
		return 2, errors.New("-max-eur must be a positive amount")
	}
	if o.real {
		if o.model == "" {
			return 2, errors.New("-real needs -model")
		}
		if o.maxEUR == 0 {
			return 2, errors.New("-real needs -max-eur: the money fuse is not optional")
		}
		p, known := priceOf(o.model)
		if o.priceIn == 0 {
			o.priceIn = p[0]
		}
		if o.priceOut == 0 {
			o.priceOut = p[1]
		}
		if !known && (o.priceIn == 0 || o.priceOut == 0) {
			return 2, fmt.Errorf("no list price of %s: give -price-in and -price-out", o.model)
		}
	} else if o.maxEUR == 0 {
		o.maxEUR = 1
	}
	// A cap that rounds to nothing, or one past any real budget, would leave no fuse.
	if o.maxEUR < 0.000001 || o.maxEUR > 1_000_000 {
		return 2, errors.New("-max-eur must be between 0.000001 and 1000000")
	}
	if o.priceIn < 0 || o.priceOut < 0 {
		return 2, errors.New("the prices must not be negative")
	}
	for _, n := range split(o.scenario) {
		if n != "all" && sim.Find(n) == nil {
			return 2, fmt.Errorf("no scenario %s (%s)", n, strings.Join(scenarioNames(), ", "))
		}
	}
	if o.scenario == "" && !o.interactive && o.duration == 0 {
		o.scenario = "all"
	}
	if o.step < time.Second || o.timeout < 10*time.Second || o.faultEvery < 10*time.Second {
		return 2, errors.New("-step is at least 1s, -timeout and -fault-every at least 10s")
	}
	if o.dsn == "" {
		return 2, errors.New("-dsn (or $BOTARU_SIM_DSN) is required")
	}
	return 0, nil
}

func runWith(o options, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	if code, err := o.check(); err != nil {
		return code, err
	}
	root, err := spec.FindRoot(o.root)
	if err != nil {
		return 2, err
	}
	var key string
	if o.real {
		if key, err = providerKey(o.keyFile); err != nil {
			return 2, err
		}
	}
	runDir := filepath.Join(o.out, "run-"+time.Now().UTC().Format("20060102-150405"))
	work := filepath.Join(runDir, "world")
	if err := os.MkdirAll(work, 0o755); err != nil {
		return 1, err
	}
	if work, err = filepath.Abs(work); err != nil {
		return 1, err
	}
	exe, err := os.Executable()
	if err != nil {
		return 1, err
	}
	log, err := sim.NewLog(stdout, filepath.Join(runDir, "sim-events.jsonl"))
	if err != nil {
		return 1, err
	}
	defer log.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	dsn, drop, err := freshDatabase(ctx, o.dsn)
	if err != nil {
		return 1, err
	}
	if o.keepDB {
		fmt.Fprintln(stderr, "botaru-sim: keeping the database", redactDSN(dsn))
	} else {
		defer drop()
	}
	level := slog.LevelError
	if o.verbose {
		level = slog.LevelInfo
	}
	capMicros := money.FromEUR(o.maxEUR)
	mode := "mock model"
	if o.real {
		mode = fmt.Sprintf("real model %s at %.2f/%.2f EUR per million tokens in/out", o.model, o.priceIn, o.priceOut)
	}
	log.Addf(sim.KindDirector, "botaru-sim %s: %s, money fuse %s EUR, auto-approve %s, world in %s", version.Version, mode, sim.EUR(capMicros), o.approve, work)
	s, err := sim.Start(ctx, sim.Config{Root: root, DSN: dsn, Work: work, Exe: exe, Real: o.real, Key: key, BaseURL: o.providerURL,
		Model: o.model, PriceIn: o.priceIn, PriceOut: o.priceOut, MaxMicros: capMicros, Seed: o.seed, Step: o.step, Log: log,
		Logger: slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: level}))})
	if err != nil {
		return 1, err
	}
	defer s.Close()
	log.Addf(sim.KindDirector, "the stand is up: core, telegram channel, ops-reader bot, toolhosts of web1, db1 and ctd1, monitoring adapter")
	d := &sim.Director{S: s, Approve: o.approve, Timeout: o.timeout, Seed: o.seed}
	dctx, dcancel := context.WithCancel(ctx)
	defer dcancel()
	go func() {
		select {
		case <-s.Stopped():
			dcancel()
		case <-dctx.Done():
		}
	}()
	go d.HandleCards(dctx)
	var runErr error
	if o.scenario != "" {
		names := split(o.scenario)
		if slices.Contains(names, "all") {
			names = scenarioNames()
		}
		runErr = d.RunScenarios(dctx, names)
	}
	if runErr == nil && (o.interactive || o.duration > 0) {
		runErr = continuous(dctx, d, o, stdin, stdout)
	}
	_ = s.CheckFuse(context.Background())
	res := d.Results()
	if err := writeResults(filepath.Join(runDir, "results.json"), res, s, o); err != nil {
		return 1, err
	}
	failed := 0
	for _, r := range res {
		if !r.OK {
			failed++
		}
	}
	fmt.Fprintf(stdout, "\n%d checks, %d failed; spend %s EUR of the %s EUR fuse; %d model calls answered by the mock\nevents: %s\n",
		len(res), failed, sim.EUR(s.Spent()), sim.EUR(capMicros), s.Brain.Calls(), filepath.Join(runDir, "sim-events.jsonl"))
	switch {
	case s.Tripped():
		return 3, errors.New("the money fuse stopped the run")
	case runErr != nil && !errors.Is(runErr, context.Canceled):
		return 1, runErr
	case failed > 0:
		return 1, nil
	}
	return 0, nil
}

func writeResults(path string, res []sim.Result, s *sim.Stand, o options) error {
	raw, err := json.MarshalIndent(map[string]any{"mode": map[bool]string{true: "real", false: "mock"}[o.real], "model": o.model,
		"spent_eur": sim.EUR(s.Spent()), "max_eur": o.maxEUR, "fuse_tripped": s.Tripped(), "auto_approve": o.approve, "checks": res}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(raw, '\n'), 0o644)
}

// faults are the faults of the continuous mode.
var faults = []struct {
	name string
	who  string
	ask  string
	do   func(w *sim.World)
}{
	{"nginx-down", "bob", "/logs The site does not open, is it down?", func(w *sim.World) {
		s := w.Service("web1", sim.UnitNginx)
		s.State, s.Changed = "failed", time.Now().UTC()
	}},
	{"slow-site", "alice", "/logs Why is the site so slow right now?", func(w *sim.World) { w.Faults[sim.FaultSlowSite] = true }},
	{"disk-filling", "bob", "/logs The disk on db1 is filling up, can you free some space?", func(w *sim.World) {
		d := w.Hosts["db1"].Disks["/data"]
		d.Fill(0.10)
		w.Faults[sim.FaultDiskFilling] = true
	}},
}

// labAsks are the everyday questions the people of the lab put to the data bots between faults.
var labAsks = []struct{ who, ask string }{
	{"alice", "/geo What is in the bathy-50m raster and the map-areas layer?"},
	{"bob", "/pipeline Why did the last CTD run fail?"},
}

// continuous runs the world until the duration ends, quit, Ctrl+C or the fuse: it breaks the
// world every -fault-every, a person asks about it, and the console takes commands.
func continuous(ctx context.Context, d *sim.Director, o options, stdin io.Reader, stdout io.Writer) error {
	if o.duration > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, o.duration)
		defer cancel()
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	lines := make(chan string)
	if o.interactive {
		fmt.Fprintln(stdout, "commands: approve N | reject N | details N | say PERSON TEXT | fault NAME | heal | insights on|off | evolve on|off | "+
			"gate ID | realize ID | revert ID [REASON] | variants | rules | cards | status | quit")
		go func() {
			sc := bufio.NewScanner(stdin)
			for sc.Scan() {
				select {
				case lines <- sc.Text():
				case <-ctx.Done():
					return
				}
			}
		}()
	}
	r := rand.New(rand.NewPCG(uint64(o.seed), 13))
	t := time.NewTicker(o.faultEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return nil
			}
			return ctx.Err()
		case <-t.C:
			// Now and then a question of the lab instead of a fault.
			i := r.IntN(len(faults) + len(labAsks))
			if i >= len(faults) {
				a := labAsks[i-len(faults)]
				_, _ = d.S.Say(a.who, a.who+"-dm", a.ask)
				continue
			}
			f := faults[i]
			if err := breakWorld(ctx, d, f.name); err != nil {
				return err
			}
			go func() {
				// The person notices a little later.
				select {
				case <-ctx.Done():
					return
				case <-time.After(3 * o.step):
				}
				_, _ = d.S.Say(f.who, f.who+"-dm", f.ask)
			}()
		case l := <-lines:
			if quit := command(ctx, d, l, stdout); quit {
				return nil
			}
		}
	}
}

func breakWorld(ctx context.Context, d *sim.Director, name string) error {
	for _, f := range faults {
		if f.name == name {
			return sim.Update(d.S.Work(), func(w *sim.World) error {
				f.do(w)
				w.Note("director", "", "fault %s", name)
				return nil
			})
		}
	}
	return fmt.Errorf("no fault %s", name)
}

// command runs one console command; it reports whether to quit.
func command(ctx context.Context, d *sim.Director, line string, out io.Writer) bool {
	f := strings.Fields(line)
	if len(f) == 0 {
		return false
	}
	card := func() int {
		if len(f) < 2 {
			return 0
		}
		n, _ := strconv.Atoi(strings.TrimPrefix(f[1], "#"))
		return n
	}
	var err error
	switch f[0] {
	case "quit", "exit":
		return true
	case "approve", "a":
		err = d.S.Press(card(), "ap", "")
	case "reject", "r":
		err = d.S.Press(card(), "rj", "")
	case "details", "d":
		err = d.S.Press(card(), "dt", "")
	case "say":
		if len(f) < 3 {
			err = errors.New("say PERSON TEXT")
			break
		}
		_, err = d.S.Say(f[1], f[1]+"-dm", strings.Join(f[2:], " "))
	case "fault":
		if len(f) < 2 {
			err = errors.New("fault NAME: nginx-down, slow-site or disk-filling")
			break
		}
		err = breakWorld(ctx, d, f[1])
	case "heal":
		err = d.Heal(ctx)
	case "insights":
		if len(f) < 2 || f[1] != "on" && f[1] != "off" {
			err = errors.New("insights on|off")
			break
		}
		err = d.S.SetInsights(ctx, f[1] == "on")
	case "evolve":
		if len(f) < 2 || f[1] != "on" && f[1] != "off" {
			err = errors.New("evolve on|off")
			break
		}
		if err = d.S.SetEvolve(ctx, f[1] == "on"); err == nil {
			fmt.Fprintf(out, "the analyses %s\n", map[bool]string{true: "do the queue check and propose a manifest change", false: "follow their own instruction"}[f[1] == "on"])
		}
	case "gate":
		if len(f) != 2 {
			err = errors.New("gate PROPOSAL")
			break
		}
		rep, state, gerr := d.S.GateProposal(ctx, f[1])
		if rep != nil {
			fmt.Fprintf(out, "gate %s: %s %s; the proposal is %s\n", f[1], rep.Verdict, rep.Code, state)
		}
		err = gerr
	case "realize":
		if len(f) != 2 {
			err = errors.New("realize PROPOSAL")
			break
		}
		commit, rerr := d.S.Realize(ctx, f[1])
		if rerr == nil {
			fmt.Fprintf(out, "applied %s at %s\n", f[1], commit)
		}
		err = rerr
	case "revert":
		if len(f) != 2 && len(f) != 3 {
			err = errors.New("revert PROPOSAL [harm_observed|superseded|other]")
			break
		}
		reason := "harm_observed"
		if len(f) == 3 {
			reason = f[2]
		}
		res, rerr := d.S.RevertProposal(ctx, f[1], reason)
		if rerr == nil {
			fmt.Fprintf(out, "reverted %s in %s at %s\n", f[1], res.File, res.Commit)
		}
		err = rerr
	case "variants":
		vs, verr := d.S.Variants(ctx)
		for _, v := range vs {
			fmt.Fprintf(out, "%s%v %v:%v %v outcome %v live %v children %v (better %v) proposal %v\n", strings.Repeat("  ", depth(v["depth"])),
				v["variant_id"], v["target_kind"], v["target_name"], v["diversity_signature"], v["trial_outcome"], v["live"],
				v["children_count"], v["successful_children_count"], v["proposal_id"])
		}
		if verr == nil && len(vs) == 0 {
			fmt.Fprintln(out, "no variants")
		}
		err = verr
	case "rules":
		rs, rerr := d.S.ReviewRules(ctx)
		for _, r := range rs {
			fmt.Fprintf(out, "rule %v of %v: %v:%v problem %v approach %v, %v (%v), %v time(s)\n", r["rule"], r["role"], r["target_kind"],
				r["target_name"], r["problem_code"], r["signature"], r["outcome"], r["reason_code"], r["count"])
		}
		if rerr == nil && len(rs) == 0 {
			fmt.Fprintln(out, "no live review rules")
		}
		err = rerr
	case "cards":
		for _, c := range d.S.Cards() {
			fmt.Fprintf(out, "card %d in %s, pressed %q: %s\n", c.No, c.Chat, c.Pressed, strings.ReplaceAll(c.Text, "\n", " "))
		}
	case "status":
		w, werr := sim.LoadWorld(d.S.Work())
		if werr != nil {
			err = werr
			break
		}
		for _, h := range w.HostNames() {
			for u, s := range w.Hosts[h].Services {
				fmt.Fprintf(out, "%s %s %s (restarts %d)\n", h, u, s.State, s.Restarts)
			}
			for m, dk := range w.Hosts[h].Disks {
				fmt.Fprintf(out, "%s %s free %.1f%%\n", h, m, 100*dk.FreeRatio)
			}
		}
		fmt.Fprintf(out, "spend %s EUR\n", sim.EUR(d.S.Spent()))
	default:
		err = fmt.Errorf("unknown command %s", f[0])
	}
	if err != nil {
		fmt.Fprintln(out, "error:", err)
	}
	return false
}

// depth is the depth of a variant in its lineage, from variant.list.
func depth(v any) int {
	n, _ := v.(float64)
	return int(n)
}

// providerKey reads the provider key from the key file or the environment.
func providerKey(file string) (string, error) {
	if file != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			return "", err
		}
		if k := strings.TrimSpace(string(b)); k != "" {
			return k, nil
		}
		return "", fmt.Errorf("%s is empty", file)
	}
	if k := strings.TrimSpace(os.Getenv(keyEnv)); k != "" {
		return k, nil
	}
	return "", fmt.Errorf("a real run needs the provider key in $%s or -key-file", keyEnv)
}

// freshDatabase creates an empty database on the server of dsn for the run and returns its DSN
// and a function that drops it.
func freshDatabase(ctx context.Context, dsn string) (string, func(), error) {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return "", nil, err
	}
	defer conn.Close(ctx)
	name := "botaru_sim_" + strings.ToLower(strings.TrimPrefix(ids.New("x"), "x_"))
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		return "", nil, err
	}
	drop := func() {
		c, err := pgx.Connect(context.Background(), dsn)
		if err != nil {
			return
		}
		defer c.Close(context.Background())
		_, _ = c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	}
	return withDatabase(dsn, name), drop, nil
}

// withDatabase is dsn with its database replaced by db.
func withDatabase(dsn, db string) string {
	if u, err := url.Parse(dsn); err == nil && (u.Scheme == "postgres" || u.Scheme == "postgresql") {
		q := u.Query()
		q.Del("dbname")
		q.Del("database")
		u.RawQuery = q.Encode()
		u.Path = "/" + db
		u.RawPath = ""
		return u.String()
	}
	var kept []string
	for _, f := range strings.Fields(dsn) {
		if k, _, _ := strings.Cut(f, "="); k != "dbname" {
			kept = append(kept, f)
		}
	}
	return strings.Join(append(kept, "dbname="+db), " ")
}

// redactDSN is dsn without its password, for printing.
func redactDSN(dsn string) string {
	if u, err := url.Parse(dsn); err == nil && u.User != nil {
		if _, ok := u.User.Password(); ok {
			u.User = url.UserPassword(u.User.Username(), "xxxxx")
		}
		return u.String()
	}
	var kept []string
	for _, f := range strings.Fields(dsn) {
		if strings.HasPrefix(f, "password=") {
			f = "password=xxxxx"
		}
		kept = append(kept, f)
	}
	return strings.Join(kept, " ")
}
