// SPDX-License-Identifier: AGPL-3.0-only

// Command botaru-eval runs the eval cases of evals/cases/ on a test bench it stands up in this
// process: the core, a fake Bot API, a fake Zabbix and toolhosts with the own plugins, on a fresh
// database of a given PostgreSQL server (docs/quality.md, "Eval test bench").
//
//	botaru-eval -dry-plan [-claim v0.3 -profiles core,messaging,...]
//	botaru-eval -dsn postgres://... [-role ops-reader] [-case inj-01,inj-02]            # mock model
//	BOTARU_EVAL_PROVIDER_KEY=... botaru-eval -real -dsn ... -model-profiles DIR -out DIR  # real model
//
// Without -real the mock model runs: only the cases a mock can answer (the canaries the core
// refuses in code and the cases with setup.model: mock) can pass, every other case is not_run.
// The provider key comes only from the environment or a key file; it is never written anywhere.
// The exit status is 1 on a security failure, an invalid or not finished run, 2 on a usage error.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/conform"
	"github.com/HunterEwok/botaru/impl/internal/evals"
	"github.com/HunterEwok/botaru/impl/internal/ids"
	"github.com/HunterEwok/botaru/impl/internal/money"
	"github.com/HunterEwok/botaru/impl/internal/pluginmain"
	"github.com/HunterEwok/botaru/impl/internal/spec"
	"github.com/HunterEwok/botaru/impl/internal/version"
)

// keyEnv is the environment variable of the provider key of the eval workspace.
const keyEnv = "BOTARU_EVAL_PROVIDER_KEY"

func main() {
	// The bench's toolhosts start the own plugins as children of this binary (DEP-7).
	if len(os.Args) > 1 && os.Args[1] == "plugin" {
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: botaru-eval plugin KIND [flags]")
			os.Exit(2)
		}
		if err := pluginmain.Main(os.Args[2], os.Args[3:]); err != nil {
			fmt.Fprintln(os.Stderr, "botaru-eval plugin:", err)
			os.Exit(1)
		}
		return
	}
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

type options struct {
	root, dsn, claim, profiles, roles, cases string
	dryPlan, real, keepDB, verbose           bool
	modelProfiles, keyFile, providerURL      string
	maxEUR                                   float64
	repeats                                  int
	out, index                               string
}

func run(args []string, stdout, stderr io.Writer) int {
	var o options
	fs := flag.NewFlagSet("botaru-eval", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&o.root, "root", "", "the skeleton root (default: found from the working directory)")
	fs.StringVar(&o.dsn, "dsn", os.Getenv("BOTARU_EVAL_DSN"), "a PostgreSQL server; the run creates a fresh database on it and drops it afterwards")
	fs.StringVar(&o.claim, "claim", spec.Version, "the claimed version: cases and rules up to it")
	fs.StringVar(&o.profiles, "profiles", strings.Join(spec.Profiles, ","), "the claimed conformance profiles, comma separated, or all")
	fs.StringVar(&o.roles, "role", "", "only the cases of these role folders, comma separated")
	fs.StringVar(&o.cases, "case", "", "only these case ids, comma separated")
	fs.BoolVar(&o.dryPlan, "dry-plan", false, "print the claimed eval rules with their cases and the cost estimate, run nothing")
	fs.BoolVar(&o.real, "real", false, "run against a real provider (the key from $"+keyEnv+" or -key-file)")
	fs.StringVar(&o.modelProfiles, "model-profiles", "", "a directory of ModelProfile manifests that replace the example profiles of the same name (real models and prices)")
	fs.StringVar(&o.keyFile, "key-file", "", "a file holding the provider key, instead of $"+keyEnv)
	fs.StringVar(&o.providerURL, "provider-url", "", "the provider address (default: the binding's own)")
	fs.Float64Var(&o.maxEUR, "max-eur", 0, "the spend cap of the run in EUR (default and maximum: budget.max_eur_per_run)")
	fs.IntVar(&o.repeats, "repeats", 1, "runs of each blocking case; all must pass (3 before a model or runtime change)")
	fs.StringVar(&o.out, "out", "", "a directory for report.xml (JUnit), results.jsonl and summary.txt")
	fs.StringVar(&o.index, "index", "", "the EvalReports manifest to add the entries of a real run to (evals/reports/index.yaml)")
	fs.BoolVar(&o.keepDB, "keep-db", false, "keep the run's database for a look afterwards")
	fs.BoolVar(&o.verbose, "v", false, "log the bench")
	showVersion := fs.Bool("version", false, "print the version")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *showVersion {
		fmt.Fprintln(stdout, version.String("botaru-eval"))
		return 0
	}
	code, err := runWith(o, stdout, stderr)
	if err != nil {
		fmt.Fprintln(stderr, "botaru-eval:", err)
		if code == 0 {
			code = 1
		}
	}
	return code
}

var allProfiles = []string{"core", "messaging", "campaign", "insights", "manager", "governance", "monitor", "knowledge", "ingress"}

func split(s string) []string {
	var out []string
	for _, x := range strings.Split(s, ",") {
		if x = strings.TrimSpace(x); x != "" {
			out = append(out, x)
		}
	}
	return out
}

func runWith(o options, stdout, stderr io.Writer) (int, error) {
	root, err := spec.FindRoot(o.root)
	if err != nil {
		return 2, err
	}
	claim := conform.Claim{Version: o.claim, Profiles: split(o.profiles)}
	if o.profiles == "all" {
		claim.Profiles = allProfiles
	}
	rules, err := conform.LoadRules(root)
	if err != nil {
		return 1, err
	}
	catalog, err := evals.LoadCatalog(root)
	if err != nil {
		return 1, err
	}
	th, err := evals.LoadThresholds(root)
	if err != nil {
		return 1, err
	}
	all, err := evals.LoadCases(root)
	if err != nil {
		return 1, err
	}
	in, later := evals.Select(all, evals.Filter{Roles: split(o.roles), IDs: split(o.cases), Version: claim.Version})
	prices, err := evals.Prices(root, o.modelProfiles)
	if err != nil {
		return 1, err
	}
	plan := evals.NewPlan(root, rules, catalog, all, in, later, claim, prices)
	repeats := max(o.repeats, 1)
	if o.dryPlan {
		return 0, evals.WritePlan(stdout, plan, prices, max(repeats, th.Repeats))
	}

	// The cost guard (docs/quality.md, "Cost"): a run may only lower budget.max_eur_per_run.
	capMicros := th.MaxRunMicros
	if o.maxEUR > 0 {
		capMicros = money.FromEUR(o.maxEUR)
		if capMicros > th.MaxRunMicros {
			return 2, fmt.Errorf("-max-eur %.2f is above budget.max_eur_per_run of evals/thresholds.yaml; a run can only lower it (split the run with -role)", o.maxEUR)
		}
	}
	model := evals.ModelConfig{Real: o.real, BaseURL: o.providerURL, ProfilesDir: o.modelProfiles}
	if o.real {
		key, err := providerKey(o.keyFile)
		if err != nil {
			return 2, err
		}
		model.Key = key
		est := plan.EstimateWith(repeats)
		fmt.Fprintf(stdout, "Real run: %d cases, estimate %s EUR (%d repeats of the blocking cases), cap %s EUR\n",
			len(plan.Cases), eur(est), repeats, eur(capMicros))
		if est > capMicros {
			fmt.Fprintln(stdout, "The estimate is above the cap: the run stops before the case that would cross it.")
		}
	}
	if o.dsn == "" {
		return 2, errors.New("-dsn (or $BOTARU_EVAL_DSN) is required for a run")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	dsn, drop, err := freshDatabase(ctx, o.dsn)
	if err != nil {
		return 1, err
	}
	if o.keepDB {
		fmt.Fprintln(stderr, "botaru-eval: keeping the database", dsn)
	} else {
		defer drop()
	}
	work, err := os.MkdirTemp("", "botaru-eval-")
	if err != nil {
		return 1, err
	}
	defer os.RemoveAll(work)
	exe, err := os.Executable()
	if err != nil {
		return 1, err
	}
	level := slog.LevelError
	if o.verbose {
		level = slog.LevelInfo
	}
	b, err := evals.StartBench(ctx, evals.BenchConfig{Root: root, DSN: dsn, WorkDir: work, Model: model,
		Logger: slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: level})),
		Plugin: func(kind string, args []string) ([]string, []string) {
			return append([]string{exe, "plugin", kind}, args...), nil
		}})
	if err != nil {
		return 1, fmt.Errorf("bench: %w", err)
	}
	defer b.Close()
	r := &evals.Runner{Root: root, Bench: b, Real: o.real, Thresholds: th, Prices: prices, MaxRunMicros: capMicros, Repeats: repeats, Progress: stdout}
	rep := r.Run(ctx, plan)
	if err := writeReports(ctx, o, b, rep, stdout); err != nil {
		return 1, err
	}
	_, sec := rep.Counts()
	if sec > 0 || rep.Stopped != "" || rep.Invalid != "" {
		return 1, nil
	}
	return 0, nil
}

func writeReports(ctx context.Context, o options, b *evals.Bench, rep *evals.Report, stdout io.Writer) error {
	if err := evals.WriteSummary(stdout, rep); err != nil {
		return err
	}
	if o.out != "" {
		if err := os.MkdirAll(o.out, 0o755); err != nil {
			return err
		}
		for name, w := range map[string]func(io.Writer, *evals.Report) error{
			"report.xml": evals.WriteJUnit, "results.jsonl": evals.WriteResults, "summary.txt": evals.WriteSummary} {
			if err := writeFile(filepath.Join(o.out, name), func(f io.Writer) error { return w(f, rep) }); err != nil {
				return err
			}
		}
	}
	if o.index == "" {
		return nil
	}
	entries, notes, err := evals.IndexEntries(ctx, b, rep)
	if err != nil {
		return err
	}
	for _, n := range notes {
		fmt.Fprintln(stdout, "index:", n)
	}
	if len(entries) == 0 {
		fmt.Fprintln(stdout, "index: no entry (only a finished real run with every case of a set rated writes one)")
		return nil
	}
	if err := evals.WriteIndex(o.index, entries); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "index: %d entries added to %s\n", len(entries), o.index)
	return nil
}

func writeFile(path string, w func(io.Writer) error) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := w(f); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// providerKey reads the key of the eval workspace from the key file or the environment.
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
	name := "botaru_eval_" + strings.ToLower(strings.TrimPrefix(ids.New("x"), "x_"))
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
	if strings.Contains(dsn, "://") {
		head, query, _ := strings.Cut(dsn, "?")
		if i := strings.Index(head[strings.Index(head, "://")+3:], "/"); i >= 0 {
			head = head[:strings.Index(head, "://")+3+i]
		}
		out := head + "/" + db
		if query != "" {
			out += "?" + query
		}
		return out
	}
	return dsn + " dbname=" + db
}

func eur(m int64) string { return fmt.Sprintf("%.4f", float64(m)/1e6) }
