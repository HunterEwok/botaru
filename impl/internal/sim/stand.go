// SPDX-License-Identifier: AGPL-3.0-only

package sim

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/HunterEwok/botaru/impl/internal/core"
	"github.com/HunterEwok/botaru/impl/internal/evals"
	"github.com/HunterEwok/botaru/impl/internal/ids"
	"github.com/HunterEwok/botaru/impl/internal/monitor/zabbix"
	"github.com/HunterEwok/botaru/impl/internal/p1client"
)

// Config is what a stand is built from.
type Config struct {
	// Root is the repository root (spec, examples).
	Root string
	// DSN is an empty or a migrated database for the store.
	DSN string
	// Work is the work directory of the world; it must be empty.
	Work string
	// Exe is the program the toolhosts start the own plugins with (botaru-sim plugin KIND) and
	// the stubs run (this binary).
	Exe string
	// Real runs the real model provider with Key and Model; else the Brain answers.
	Real    bool
	Key     string
	BaseURL string
	// Model replaces the model of every model profile in a real run; else RankModels gives
	// each profile the model of its spec.rank. Prices are the EUR per million tokens (input,
	// output) of every model they name.
	Model      string
	RankModels map[int]string
	Prices     map[string][2]float64
	// MaxMicros is the money fuse: the spend of the store at which every tenant is held.
	MaxMicros int64
	// Seed makes the generated logs repeatable.
	Seed int64
	// Step is the period of the world physics, 5 seconds by default.
	Step time.Duration
	// Farm is the farm profile: its tenants, machines and bot workers; nil: smoke.
	Farm   *Farm
	Log    *Log
	Logger *slog.Logger
}

// Stand is a running simulator: the bench with the core and the workers, the world with its
// physics, the monitoring system and its adapter, the queue worker of the oneshot actions and
// the watchers that print what happens.
type Stand struct {
	cfg   Config
	farm  *Farm
	Bench *evals.Bench
	Mon   *Monitor
	Brain *Brain
	log   *Log
	// repo is the farm git of the stand set, holdout the folder of the hidden cases of its
	// merge gate (D-120).
	repo    *farmRepo
	holdout string

	mu sync.Mutex
	// cards are the cards the bot sent with buttons, by number.
	cards    []*Card
	cardByID map[string]*Card
	// chats maps a chat id of the fake Bot API to the seed chat name.
	chats map[string]string
	// tripped is set when the fuse held the tenants.
	tripped bool
	// watched is the number of done oneshot runs the last pipeline watch of each tenant saw.
	watched map[string]int
	// insights says whether the analysis schedule of ops-reader runs.
	insights bool
	// insightInstruction is the instruction of ops.insight.v1 before the evolution mode
	// extended it, kept while the mode is on.
	insightInstruction string
	// evolveActive says whether the farm git holds the extended instruction.
	evolveActive bool
	// held is set once the holds of the fuse are set.
	held    bool
	spent   int64
	stopped chan struct{}
	cancel  context.CancelFunc
	done    sync.WaitGroup
	closers []func()
	fuseMu  sync.Mutex
}

// Card is a message of the bot with buttons.
type Card struct {
	No        int
	Chat      string // seed chat name
	MessageID int64
	Text      string
	// Subject is the approval or proposal id the buttons carry.
	Subject string
	Buttons []Button
	// Pressed is set once a person pressed one of its buttons.
	Pressed string
}

// Button is one inline button.
type Button struct {
	Label, Data string
}

// Code is the short code of the button: ap, rj, dt...
func (b Button) Code() string {
	parts := strings.Split(b.Data, ":")
	return parts[len(parts)-1]
}

// Start builds the world, starts the bench on it and the simulator's own workers.
func Start(ctx context.Context, cfg Config) (_ *Stand, err error) {
	if cfg.Step <= 0 {
		cfg.Step = 5 * time.Second
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	if cfg.Farm == nil {
		if cfg.Farm, err = LoadFarm("smoke"); err != nil {
			return nil, err
		}
	}
	f := cfg.Farm
	s := &Stand{cfg: cfg, farm: f, log: cfg.Log, Brain: &Brain{Home: f.Home().Name}, cardByID: map[string]*Card{}, chats: map[string]string{}, watched: map[string]int{},
		stopped: make(chan struct{})}
	if s.log == nil {
		if s.log, err = NewLog(nil, ""); err != nil {
			return nil, err
		}
	}
	now := time.Now().UTC()
	if err := InitFarmFiles(cfg.Work, f, cfg.Seed, now); err != nil {
		return nil, fmt.Errorf("world: %w", err)
	}
	if s.repo, err = initRepo(cfg.Work, f.Home().Name); err != nil {
		return nil, fmt.Errorf("the farm git: %w", err)
	}
	s.holdout = filepath.Join(cfg.Work, "holdout")
	if err := writeCases(s.holdout, casesFor(hiddenCases, f.Home().Name)); err != nil {
		return nil, err
	}
	roots := map[string]string{}
	for _, t := range f.Tenants {
		for _, h := range t.Hosts {
			if roots[h.ID], err = WriteStubs(cfg.Work, h.ID, cfg.Exe, nil); err != nil {
				return nil, err
			}
		}
	}
	s.Mon = &Monitor{Work: cfg.Work, HookToken: "sim-hook-" + ids.Secret(16), Hooks: map[string]string{}}
	s.Mon.OnPush = func(key, value string) {
		if strings.HasPrefix(key, "botaru.alarm[") {
			s.log.Add(KindMonitor, "alarm exported: "+value, map[string]any{"key": key})
		}
	}
	s.Mon.OnScript = func(host, id, input string) {
		s.log.Add(KindMonitor, fmt.Sprintf("script %s run on %s", id, host), map[string]any{"scriptid": id, "input": input})
	}
	s.Mon.OnAck = func(id, msg string) { s.log.Add(KindMonitor, "note on problem "+id+": "+msg, nil) }

	model := evals.ModelConfig{Real: true, BaseURL: cfg.BaseURL, Key: cfg.Key}
	if !cfg.Real {
		u, stop, err := serve(s.Brain)
		if err != nil {
			return nil, err
		}
		defer func() {
			if err != nil {
				stop()
			}
		}()
		s.closeLater(stop)
		model = evals.ModelConfig{Real: true, BaseURL: u, Key: "sim-mock-key"}
	}
	skills, err := skillFolders(cfg.Root)
	if err != nil {
		return nil, err
	}
	farm, err := readFarm(cfg.Root)
	if err != nil {
		return nil, err
	}
	var tenants []evals.BenchTenant
	for _, t := range f.Tenants {
		bt := evals.BenchTenant{Name: t.Name}
		if t.Replicas != nil {
			bt.Agents = map[string]int{}
			for r, n := range t.Replicas {
				if r != "data-job" {
					bt.Agents[r] = n
				}
			}
		}
		tenants = append(tenants, bt)
	}
	editSeed := func(sd *evals.Seed) { editSeed(sd); farmSeed(sd); farmSeedTenants(sd, f) }
	var editErr error
	bcfg := evals.BenchConfig{
		Root: cfg.Root, DSN: cfg.DSN, WorkDir: filepath.Join(cfg.Work, "bench"), Model: model, Logger: cfg.Logger,
		Plugin: func(kind string, args []string) ([]string, []string) {
			if kind == "pipeline" {
				kind = "queue"
			}
			if slices.Contains(SimPluginKinds, kind) {
				return append([]string{cfg.Exe, "simplugin", kind}, args...), nil
			}
			return append([]string{cfg.Exe, "plugin", kind}, args...), nil
		},
		Roles:    farmRoles,
		Tenants:  tenants,
		EditSeed: editSeed,
		Skills:   skills,
		Edit: func(ms []map[string]any) []map[string]any {
			return editManifests(farmManifests(ms, farm, s.Insights()), cfg)
		},
		EditTenants: func(ms []map[string]any) []map[string]any {
			out, err := tenantManifests(ms, f, cfg.Work)
			if err != nil {
				editErr = err
				return ms
			}
			return out
		},
		Monitor: s.Mon,
		CoreConfig: func(c *core.Config) {
			c.Monitor = core.MonitorConfig{Enabled: true}
		},
		Hosts:       func(e evals.HostEnv) []evals.HostPlugins { return hostPlugins(cfg, f, roots, e) },
		OnBotAPI:    s.onBotAPI,
		OnModelCall: s.onModelCall,
		ModelGate:   s.gate,
		// The first apply commits the stand set to the farm git; later changes are commits of
		// their own (SetInsights, Realize, RevertProposal).
		Commit: func(ms []map[string]any) (string, string, []map[string]any, error) {
			if editErr != nil {
				return "", "", nil, editErr
			}
			return s.repo.writeSet(ctx, ms, "The stand set of botaru-sim")
		},
		PersonOperators: []string{Decider},
	}
	if err := os.MkdirAll(bcfg.WorkDir, 0o755); err != nil {
		return nil, err
	}
	// The chats are known before the bench starts, so its first messages name their chat.
	sd, err := evals.LoadSeed(cfg.Root)
	if err != nil {
		return nil, err
	}
	editSeed(sd)
	for _, c := range sd.Chats {
		s.chats[c.ExternalID] = c.Name
	}
	for t, c := range sd.TenantCanaries {
		if f.Tenant(t) != nil {
			if err := plantCanary(cfg.Work, t, c); err != nil {
				return nil, err
			}
		}
	}
	if s.Bench, err = evals.StartBench(ctx, bcfg); err != nil {
		s.Close()
		return nil, fmt.Errorf("bench: %w", err)
	}
	for _, t := range f.Tenants {
		if err := s.startAdapter(ctx, t.Name); err != nil {
			s.Close()
			return nil, err
		}
	}
	runCtx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	for _, t := range f.Tenants {
		for range t.Workers("data-job") {
			if err := s.startJob(ctx, runCtx, t.Name); err != nil {
				s.Close()
				return nil, fmt.Errorf("the data-job worker of %s: %w", t.Name, err)
			}
		}
	}
	// The farm operator gives the farm manager seat to its bot (MGR-1).
	var seat map[string]any
	if err := s.Bench.P2(ctx, "farm", "seat.assign", map[string]any{"seat": "farm-manager"}, &seat); err != nil {
		s.Close()
		return nil, fmt.Errorf("the farm manager seat: %w", err)
	}
	s.log.Addf(KindDirector, "the farm manager seat is %v", seat["state"])
	for _, fn := range []func(context.Context){s.physics, s.tailWorld, s.watchStore, s.fuse} {
		s.done.Add(1)
		go func() { defer s.done.Done(); fn(runCtx) }()
	}
	for _, t := range f.Tenants {
		s.done.Add(1)
		go func() { defer s.done.Done(); s.queueWorker(runCtx, t.Name) }()
	}
	return s, nil
}

// Farm is the farm profile of the stand.
func (s *Stand) Farm() *Farm { return s.farm }

// Home is the home tenant: the tenant of the scenarios; demo on a stand built without a farm.
func (s *Stand) Home() string {
	if s.farm == nil {
		return templateTenant
	}
	return s.farm.Home().Name
}

// HomeHost is the home tenant's first machine of a kind: web1, db1 and ctd1 of the scenarios.
func (s *Stand) HomeHost(kind string) string {
	if s.farm == nil {
		return templateHosts[kind]
	}
	return s.farm.Home().First(kind)
}

// closeLater adds a closer of the stand's own listeners.
func (s *Stand) closeLater(f func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closers = append(s.closers, f)
}

// serve listens on a local port.
func serve(h http.Handler) (string, func(), error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, err
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(l) }()
	return "http://" + l.Addr().String(), func() { _ = srv.Close() }, nil
}

// editSeed gives the virtual people what the scenarios need: alice reads the logs (staff), and
// the approvers approve the pipeline cleanup.
func editSeed(sd *evals.Seed) {
	for i := range sd.People {
		if sd.People[i].Name == "alice" && !slices.Contains(sd.People[i].Groups, "staff") {
			sd.People[i].Groups = append(sd.People[i].Groups, "staff")
		}
	}
	if g := sd.Groups["approver"]; g != nil {
		caps, _ := g["caps"].([]any)
		g["caps"] = append(caps, "action.approve:cleanup_pipeline_tmp")
	}
}

// editManifests sets the model and the prices of a real run in every model profile: Model in
// each, or the model of the profile's rank.
func editManifests(ms []map[string]any, cfg Config) []map[string]any {
	if !cfg.Real || cfg.Model == "" && len(cfg.RankModels) == 0 {
		return ms
	}
	for _, m := range ms {
		if m["kind"] != "ModelProfile" {
			continue
		}
		sp, _ := m["spec"].(map[string]any)
		model := cfg.Model
		if model == "" {
			// The rank comes as whatever number type the manifest reader gives (json.Number).
			model = rankModel(cfg.RankModels, int(num(sp["rank"])))
		}
		sp["model"] = model
		pr, _ := sp["price"].(map[string]any)
		if pr == nil {
			pr = map[string]any{}
			sp["price"] = pr
		}
		p := cfg.Prices[model]
		pr["input"], pr["output"] = p[0], p[1]
	}
	widenBudgets(ms)
	return ms
}

// widenBudgets raises the budgets of the examples that the reserve of a real model's call would
// not fit: the tree budget of a task type below twice the reserve of one call of the profiles of
// the roles that claim it (the apply lint wants one call's output, tree_budget_below_call; the
// gateway reserves the input as well before each call), and a manager line below the seat's
// worst day (manager_line_too_small, MGR-20) or four reserves of the seat. The examples are priced for
// their own illustrative models; the money fuse of the run still caps the spend. Nothing is
// lowered.
func widenBudgets(ms []map[string]any) {
	worst, prices := map[string]float64{}, map[string]map[string]any{}
	for _, m := range ms {
		if m["kind"] == "ModelProfile" {
			sp := mapOf(m["spec"])
			name, _ := mapOf(m["metadata"])["name"].(string)
			prices[name] = sp
			worst[name] = callReserve(sp)
		}
	}
	purpose := map[string]string{}
	for _, m := range ms {
		if m["kind"] == "TaskType" {
			name, _ := mapOf(m["metadata"])["name"].(string)
			purpose[name], _ = mapOf(m["spec"])["purpose"].(string)
		}
	}
	need := map[string]float64{}
	for _, m := range ms {
		if m["kind"] != "Role" {
			continue
		}
		sp := mapOf(m["spec"])
		for _, c := range asSlice(sp["claims"]) {
			t, _ := c.(string)
			slot := "base"
			if purpose[t] == "insight" || purpose[t] == "aggregate" {
				slot = "analysis"
			}
			prof, _ := mapOf(sp["models"])[slot].(string)
			need[t] = max(need[t], worst[prof])
		}
	}
	var seat map[string]any
	reviews := 1.0 // how much the tree budgets of the review types rose
	for _, m := range ms {
		switch m["kind"] {
		case "TaskType":
			name, _ := mapOf(m["metadata"])["name"].(string)
			sp := mapOf(m["spec"])
			if w, cur := cents(2*need[name]), num(sp["tree_budget_eur"]); w > cur {
				sp["tree_budget_eur"] = w
				if purpose[name] == "review" && cur > 0 {
					reviews = max(reviews, w/cur)
				}
			}
		case "Governance":
			if p, _ := mapOf(mapOf(m["spec"])["farm_manager"])["model_profile"].(string); p != "" {
				seat = prices[p]
			}
		}
	}
	// The review line holds the reviews of the role's proposals a month (BUD-13): it rises with
	// their price.
	for _, m := range ms {
		if m["kind"] != "Tenant" || reviews == 1 {
			continue
		}
		line := mapOf(mapOf(mapOf(mapOf(m["spec"])["budgets"])["lines"])["review"])
		if line != nil && num(line["month_eur"]) > 0 {
			line["month_eur"] = cents(reviews * num(line["month_eur"]))
		}
	}
	if seat == nil {
		return
	}
	// Three readings of one worst-case incident, 60000 tokens in, three answers at the
	// profile's max_output_tokens, with the default daily review and post-mortem ceilings.
	day := cents(max(1.5*((60000*num(mapOf(seat["price"])["input"])+3*num(seat["max_output_tokens"])*num(mapOf(seat["price"])["output"]))/1e6+0.8),
		4*callReserve(seat)))
	for _, m := range ms {
		if m["kind"] != "Tenant" {
			continue
		}
		line := mapOf(mapOf(mapOf(mapOf(m["spec"])["budgets"])["lines"])["manager"])
		if line == nil || num(line["day_eur"]) >= day {
			continue
		}
		line["day_eur"] = day
		if month := cents(15 * day); num(line["month_eur"]) < month {
			line["month_eur"] = month
		}
	}
}

// callReserve is what the gateway reserves for one call of a profile in EUR (gateway.Reserve):
// the output limit at the output price, and a request of 60000 bytes, which an agent with a few
// tool results reaches, at the input price.
func callReserve(profile map[string]any) float64 {
	pr := mapOf(profile["price"])
	return (num(profile["max_output_tokens"])*num(pr["output"]) + 60000*num(pr["input"])) / 1e6
}

// num reads a number of a manifest, whatever number type the reader gave.
func num(v any) float64 {
	f, _ := strconv.ParseFloat(fmt.Sprint(v), 64)
	return f
}

// cents rounds an amount in EUR up to whole cents.
func cents(eur float64) float64 { return math.Ceil(eur*100-1e-9) / 100 }

// rankModel is the model of a rank: the one of the highest rank not above it, else the lowest.
func rankModel(ranks map[int]string, rank int) string {
	best, low := -1, -1
	for r := range ranks {
		if r <= rank && r > best {
			best = r
		}
		if low < 0 || r < low {
			low = r
		}
	}
	if best < 0 {
		best = low
	}
	return ranks[best]
}

// startAdapter enrolls and starts the monitoring adapter botaru-zabbix of a tenant with its
// webhook, for the tenant's machines.
func (s *Stand) startAdapter(ctx context.Context, tenant string) error {
	tok, err := s.Bench.Enroll(ctx, map[string]any{"tenant": tenant, "role": "zabbix", "kind": "monitor", "target": "zabbix"})
	if err != nil {
		return fmt.Errorf("monitoring adapter of %s: %w", tenant, err)
	}
	api, stop, err := serve(s.Mon)
	if err != nil {
		return err
	}
	s.closeLater(stop)
	hosts := map[string]string{}
	for _, h := range s.farm.Tenant(tenant).Hosts {
		hosts[h.ID+".example.org"] = h.ID
	}
	a := zabbix.New(zabbix.Config{
		Core:   &p1client.Client{BaseURL: s.Bench.P1(), Token: tok, HTTP: &http.Client{Timeout: 2 * time.Minute}},
		Tenant: tenant, Target: "zabbix", API: api + "/api_jsonrpc.php", APIToken: "sim-monitor-" + ids.Secret(8),
		Hosts:        hosts,
		WebhookToken: s.Mon.HookToken, Logger: s.cfg.Logger, MaintenanceEvery: 60 * time.Second,
	})
	hook, stop, err := serve(a.Webhook())
	if err != nil {
		return err
	}
	s.closeLater(stop)
	s.Mon.Hooks[tenant] = hook
	if err := a.Hello(ctx); err != nil {
		return fmt.Errorf("monitoring adapter hello: %w", err)
	}
	s.Bench.Go(a.Run)
	return nil
}

// Close stops the simulator's workers and the bench.
func (s *Stand) Close() {
	if s.cancel != nil {
		s.cancel()
	}
	s.done.Wait()
	if s.Bench != nil {
		s.Bench.Close()
	}
	s.mu.Lock()
	cs := s.closers
	s.closers = nil
	s.mu.Unlock()
	for i := len(cs) - 1; i >= 0; i-- {
		cs[i]()
	}
}

// Work is the work directory of the world.
func (s *Stand) Work() string { return s.cfg.Work }

// Stopped is closed when the fuse trips.
func (s *Stand) Stopped() <-chan struct{} { return s.stopped }

// Spent is the spend of the store at the last look, in micro-euros.
func (s *Stand) Spent() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.spent
}

// physics advances the world each step and sends the webhook of each problem change.
func (s *Stand) physics(ctx context.Context) {
	r := rand.New(rand.NewPCG(uint64(s.cfg.Seed), 7))
	t := time.NewTicker(s.cfg.Step)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		changes, err := Step(s.cfg.Work, time.Now().UTC(), r)
		if err != nil {
			s.log.Addf(KindWorld, "the physics step failed: %v", err)
			continue
		}
		if len(changes) == 0 {
			continue
		}
		w, err := LoadWorld(s.cfg.Work)
		if err != nil {
			continue
		}
		for _, c := range changes {
			// Like a Zabbix media type, the webhook is tried up to three times.
			for try := 1; ; try++ {
				err := s.Mon.Notify(ctx, w, c)
				if err == nil || ctx.Err() != nil {
					break
				}
				if try == 3 {
					s.log.Addf(KindMonitor, "the problem webhook failed: %v", err)
					break
				}
				select {
				case <-ctx.Done():
				case <-time.After(time.Duration(try) * time.Second):
				}
			}
		}
	}
}

// Queue run requests of the oneshot executor (internal/plugins Queue): the simulator plays the
// root request parser of the installation.
type queued struct {
	ID   string         `json:"id"`
	Tool string         `json:"tool"`
	Args map[string]any `json:"args"`
}

// queueWorker runs the requests the pipeline plugin of a tenant queued and moves each to done/.
func (s *Stand) queueWorker(ctx context.Context, tenant string) {
	dir := queueDir(s.cfg.Work, tenant)
	doneDir := filepath.Join(dir, "done")
	_ = os.MkdirAll(doneDir, 0o755)
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
		for _, f := range files {
			raw, err := os.ReadFile(f)
			if err != nil {
				continue
			}
			var q queued
			if json.Unmarshal(raw, &q) != nil {
				_ = os.Rename(f, filepath.Join(doneDir, filepath.Base(f)+".bad"))
				continue
			}
			// A retried request of a run already played is not played again.
			if _, err := os.Stat(filepath.Join(doneDir, filepath.Base(f))); err == nil {
				_ = os.Remove(f)
				continue
			}
			s.runQueued(tenant, q)
			_ = os.Rename(f, filepath.Join(doneDir, filepath.Base(f)))
		}
	}
}

// runQueued plays one oneshot run of a tenant in the world: cleanup_tmp frees the pipeline
// temporary files on the data volume of the tenant's first db machine older than the given days
// (the older the bound, the less is freed).
func (s *Stand) runQueued(tenant string, q queued) {
	db := s.farm.Tenant(tenant).First(KindDB)
	switch q.Tool {
	case "cleanup_tmp":
		days := 7
		if v, ok := q.Args["older_than_days"].(float64); ok {
			days = int(v)
		}
		share := 1 - float64(days-3)/40 // 3 days: all, 30 days: about a third
		_ = Update(s.cfg.Work, func(w *World) error {
			d := w.Hosts[db].Disks["/data"]
			freed := int64(float64(d.TmpBytes) * share)
			d.TmpBytes -= freed
			d.FreeRatio = min(0.99, d.FreeRatio+float64(freed)/float64(d.SizeBytes))
			delete(w.Faults, FaultDiskFilling+":"+db)
			if tenant == w.Home {
				delete(w.Faults, FaultDiskFilling)
			}
			w.Note("queue", db, "oneshot pipeline-tmp-clean-%s.service (run %s): freed %.1f GB of pipeline temporary files older than %d days; /data free %.1f%%",
				tenant, q.ID, float64(freed)/1e9, days, 100*d.FreeRatio)
			return nil
		})
	default:
		_ = Update(s.cfg.Work, func(w *World) error {
			w.Note("queue", "", "oneshot run %s of %s in %s: accepted, nothing to change in the world", q.ID, q.Tool, tenant)
			return nil
		})
	}
}

// tailWorld prints the world events as they are appended.
func (s *Stand) tailWorld(ctx context.Context) {
	path := filepath.Join(s.cfg.Work, eventsFile)
	var off int64
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		if _, err := f.Seek(off, 0); err != nil {
			f.Close()
			continue
		}
		dec := json.NewDecoder(f)
		for {
			var e Event
			if err := dec.Decode(&e); err != nil {
				break
			}
			host := ""
			if e.Host != "" {
				host = e.Host + ": "
			}
			s.log.Add(KindWorld, host+e.Detail, map[string]any{"kind": e.Kind, "host": e.Host})
		}
		off += dec.InputOffset()
		f.Close()
	}
}

// watchStore prints the tool calls, approvals and tasks as their state changes.
func (s *Stand) watchStore(ctx context.Context) {
	seen := map[string]string{}
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		pool := s.Bench.Store().Pool
		rows, err := pool.Query(ctx, `SELECT id, task, tool, args, state, coalesce(reason, ''), coalesce(approval_id, ''), coalesce(grant_id, ''), coalesce(target_host, '')
			FROM toolcalls WHERE updated_at > now() - interval '1 hour' ORDER BY created_at`)
		if err == nil {
			for rows.Next() {
				var id, task, tool, state, reason, appr, grant, host string
				var args json.RawMessage
				if rows.Scan(&id, &task, &tool, &args, &state, &reason, &appr, &grant, &host) != nil {
					continue
				}
				if seen["tc:"+id] == state {
					continue
				}
				seen["tc:"+id] = state
				if state != "done" && state != "denied" && state != "error" && state != "exec_unknown" && state != "requested" {
					continue
				}
				msg := fmt.Sprintf("%s %s on %s: %s", tool, args, orDash(host), state)
				if reason != "" {
					msg += " (" + reason + ")"
				}
				if appr != "" {
					msg += ", approval " + appr
				}
				if grant != "" {
					msg += ", grant " + grant
				}
				s.log.Add(KindTool, msg, map[string]any{"id": id, "task": task, "tool": tool, "state": state, "reason": reason})
			}
			rows.Close()
		}
		rows, err = pool.Query(ctx, `SELECT id, action, state, coalesce(decided_by, ''), coalesce(reason, '') FROM approvals ORDER BY created_at`)
		if err == nil {
			for rows.Next() {
				var id, action, state, by, reason string
				if rows.Scan(&id, &action, &state, &by, &reason) != nil {
					continue
				}
				if seen["ap:"+id] == state {
					continue
				}
				seen["ap:"+id] = state
				msg := fmt.Sprintf("approval %s of %s: %s", id, action, state)
				if by != "" && state != "pending" {
					msg += " by " + s.personName(by)
				}
				if reason != "" {
					msg += " (" + reason + ")"
				}
				s.log.Add(KindCard, msg, map[string]any{"approval": id, "action": action, "state": state})
			}
			rows.Close()
		}
		rows, err = pool.Query(ctx, `SELECT id, tenant, type, state, coalesce(reason, ''), coalesce(result->>'answer', '') FROM tasks
			WHERE tenant = ANY($1) AND updated_at > now() - interval '1 hour' ORDER BY created_at`, s.TenantNames())
		if err == nil {
			for rows.Next() {
				var id, tenant, typ, state, reason, answer string
				if rows.Scan(&id, &tenant, &typ, &state, &reason, &answer) != nil {
					continue
				}
				if seen["t:"+id] == state {
					continue
				}
				seen["t:"+id] = state
				msg := fmt.Sprintf("task %s %s: %s", id, typ, state)
				if tenant != s.Home() {
					msg = tenant + ": " + msg
				}
				if reason != "" {
					msg += " (" + reason + ")"
				}
				// The result as the core keeps it; the chat may get less (outbound privacy).
				if answer != "" {
					msg += "\nresult: " + clip(answer, 600)
				}
				s.log.Add(KindDirector, msg, map[string]any{"task": id, "tenant": tenant, "type": typ, "state": state, "reason": reason})
			}
			rows.Close()
		}
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// personName is the seed name of a principal id, or the id.
func (s *Stand) personName(id string) string {
	id = strings.TrimPrefix(id, "principal:")
	for _, p := range s.Bench.Seed().People {
		if p.ID == id || strings.HasSuffix(id, p.ID) {
			return p.Name
		}
	}
	return id
}

// fuse reads the spend of the store every second and holds every tenant once it reaches the
// cap; the task generation stops with it (Stopped).
func (s *Stand) fuse(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if err := s.CheckFuse(ctx); err != nil && ctx.Err() == nil {
			s.log.Addf(KindFuse, "the fuse check failed: %v", err)
		}
	}
}

// CheckFuse reads the spend once and trips the fuse at the cap. One check runs at a time, so a
// later check returns only after the holds of a trip are set.
func (s *Stand) CheckFuse(ctx context.Context) error {
	s.fuseMu.Lock()
	defer s.fuseMu.Unlock()
	n, err := s.Bench.SpendTotal(ctx)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.spent = n
	trip := s.cfg.MaxMicros > 0 && n >= s.cfg.MaxMicros && !s.tripped
	if trip {
		s.tripped = true
	}
	hold := s.tripped && !s.held
	s.mu.Unlock()
	if trip {
		s.log.Add(KindFuse, fmt.Sprintf("the spend %s EUR reached the cap %s EUR: holding every tenant (no_enqueue, no_claim, cancel_running), refusing further model calls and stopping the director",
			EUR(n), EUR(s.cfg.MaxMicros)), map[string]any{"spent_micros": n, "cap_micros": s.cfg.MaxMicros})
		close(s.stopped)
	}
	if !hold {
		return nil
	}
	// A failed hold is tried again at the next check.
	hctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.Bench.StopSpend(hctx); err != nil {
		return fmt.Errorf("holding the tenants: %w", err)
	}
	s.mu.Lock()
	s.held = true
	s.mu.Unlock()
	s.log.Addf(KindFuse, "every tenant is held")
	return nil
}

// ReleaseContainment releases the active holds the incidents set (contain_auto) and the farm
// manager safeguard through P2, as a tenant admin and the farm operator do once the cause is gone.
func (s *Stand) ReleaseContainment(ctx context.Context) error {
	// The containment of incidents (reason contain_auto): not the maintenance holds of the
	// monitoring system (MON-9) nor the other guards of the core, which a person releases on
	// purpose.
	// Besides them, the safeguard of the farm manager seat (manager_breaker, MGR-14), which a
	// real model trips with three refused control operations: the farm operator reads its page
	// and releases it between scenarios, so the next incident has a manager again.
	rows, err := s.Bench.Store().Pool.Query(ctx, `SELECT id, tenant, reason FROM holds WHERE author_kind = 'contain_auto'
		AND (reason = 'contain_auto' AND coalesce(source, '') NOT LIKE 'system:%' OR reason = 'manager_breaker') AND state = 'active'`)
	if err != nil {
		return err
	}
	type hold struct{ id, tenant, reason string }
	var holds []hold
	for rows.Next() {
		var h hold
		if err := rows.Scan(&h.id, &h.tenant, &h.reason); err != nil {
			rows.Close()
			return err
		}
		holds = append(holds, h)
	}
	rows.Close()
	for _, h := range holds {
		if err := s.Bench.P2(ctx, h.tenant, "hold.release", map[string]any{"id": h.id}, nil); err != nil {
			s.log.Addf(KindDirector, "releasing the incident hold %s: %v", h.id, err)
			continue
		}
		if h.reason == "manager_breaker" {
			s.log.Addf(KindDirector, "the farm operator released the safeguard hold %s of the farm manager seat (manager_breaker)", h.id)
			continue
		}
		s.log.Addf(KindDirector, "a tenant admin released the incident hold %s", h.id)
	}
	return nil
}

// Insights says whether the analysis schedule runs.
func (s *Stand) Insights() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.insights
}

// SetInsights starts or pauses the analysis schedule of ops-reader in the home tenant: the change
// of its paused field is a commit of the farm git, applied; the schedules of the other tenants
// stay as the first apply set them. A schedule already in that state is left alone.
// On start the stand plays the merge window of the analyses forward (INS-4: no scheduled
// analysis within an hour of the last one), since a run of the stand plays weeks in minutes:
// the analyses of the last hour are dated back by that hour.
func (s *Stand) SetInsights(ctx context.Context, on bool) error {
	if err := s.SetInsightsIn(ctx, s.Home(), on); err != nil {
		return err
	}
	s.mu.Lock()
	s.insights = on
	s.mu.Unlock()
	return nil
}

// SetInsightsIn turns the analysis schedule of ops-reader on or off in one tenant, dating its
// analyses of the last hour back when it turns on (the merge window of INS-4).
func (s *Stand) SetInsightsIn(ctx context.Context, tenant string, on bool) error {
	if on {
		if _, err := s.Bench.Store().Pool.Exec(ctx, `UPDATE tasks SET created_at = created_at - interval '61 minutes'
			WHERE tenant = $1 AND type = 'ops.insight.v1' AND created_at > now() - interval '61 minutes'`, tenant); err != nil {
			return err
		}
	}
	msg := "Pause the analysis schedule of ops-reader"
	if on {
		msg = "Resume the analysis schedule of ops-reader"
	}
	if tenant != s.Home() {
		msg += " in " + tenant
	}
	commit, err := s.repo.setField(ctx, "Schedule", tenant, "ops-insight", "spec.paused", !on, msg)
	if err != nil {
		return err
	}
	if commit != "" {
		return s.applyOrDrop(ctx, commit)
	}
	return nil
}

// Tripped says whether the fuse tripped.
func (s *Stand) Tripped() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tripped
}

// EUR writes micro-euros as euros.
func EUR(micros int64) string { return strconv.FormatFloat(float64(micros)/1e6, 'f', 4, 64) }

// onBotAPI prints what the bot sends and keeps its cards.
func (s *Stand) onBotAPI(method string, req map[string]any, result any) {
	if method != "sendMessage" && method != "editMessageText" && method != "sendDocument" {
		return
	}
	chat := s.chats[fmt.Sprint(jsonNum(req["chat_id"]))]
	who := chat
	text, _ := req["text"].(string)
	var mid int64
	if r, ok := result.(map[string]any); ok {
		mid = int64(jsonNum(r["message_id"]))
	}
	if method == "editMessageText" {
		mid = int64(jsonNum(req["message_id"]))
	}
	var buttons []Button
	if rm, ok := req["reply_markup"].(map[string]any); ok {
		rows, _ := rm["inline_keyboard"].([]any)
		for _, row := range rows {
			cells, _ := row.([]any)
			for _, c := range cells {
				cm, _ := c.(map[string]any)
				label, _ := cm["text"].(string)
				data, _ := cm["callback_data"].(string)
				if data != "" {
					buttons = append(buttons, Button{Label: label, Data: data})
				}
			}
		}
	}
	if len(buttons) == 0 {
		verb := "says"
		if method == "editMessageText" {
			verb = "edits a message"
		}
		s.log.Add(KindChatOut, fmt.Sprintf("bot %s to %s:\n%s", verb, who, clip(text, 1500)), map[string]any{"chat": chat, "message_id": mid, "text": text})
		return
	}
	s.mu.Lock()
	subject, _, _ := strings.Cut(buttons[0].Data, ":")
	c := s.cardByID[chat+"/"+strconv.FormatInt(mid, 10)]
	if c == nil {
		c = &Card{No: len(s.cards) + 1, Chat: chat, MessageID: mid}
		s.cards = append(s.cards, c)
		s.cardByID[chat+"/"+strconv.FormatInt(mid, 10)] = c
	}
	c.Text, c.Subject, c.Buttons = text, subject, buttons
	s.mu.Unlock()
	var b strings.Builder
	fmt.Fprintf(&b, "card %d to %s:\n%s\n", c.No, who, clip(text, 1500))
	for i, bt := range buttons {
		fmt.Fprintf(&b, "[%d] %s  ", i+1, bt.Label)
	}
	s.log.Add(KindChatOut, b.String(), map[string]any{"card": c.No, "chat": chat, "message_id": mid, "subject": subject, "text": text,
		"buttons": buttons})
}

func jsonNum(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int64:
		return float64(x)
	case int:
		return float64(x)
	case string:
		f, _ := strconv.ParseFloat(x, 64)
		return f
	}
	return 0
}

// Cards are the cards so far.
func (s *Stand) Cards() []Card {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Card, len(s.cards))
	for i, c := range s.cards {
		out[i] = *c
	}
	return out
}

// Press presses a button of a card by its code (ap, rj, dt) as the person of the card's chat,
// or as who for a group chat.
func (s *Stand) Press(no int, code, who string) error {
	s.mu.Lock()
	if no < 1 || no > len(s.cards) {
		s.mu.Unlock()
		return fmt.Errorf("no card %d", no)
	}
	c := s.cards[no-1]
	var data, label string
	// Approve on the decision card of a proposal is its Accept.
	if code == "ap" && !slices.ContainsFunc(c.Buttons, func(b Button) bool { return b.Code() == "ap" }) {
		code = "ac"
	}
	for _, b := range c.Buttons {
		if b.Code() == code {
			data, label = b.Data, b.Label
		}
	}
	if data != "" && code != "dt" {
		c.Pressed = code
	}
	s.mu.Unlock()
	if data == "" {
		return fmt.Errorf("card %d has no button %s", no, code)
	}
	if who == "" {
		who = s.chatPerson(c.Chat)
	}
	if who == "" {
		return fmt.Errorf("card %d is in a group chat: name the person", no)
	}
	s.log.Add(KindChatIn, fmt.Sprintf("%s presses [%s] on card %d", who, label, no), map[string]any{"card": no, "person": who, "code": code})
	return s.Bench.Press(who, c.Chat, c.MessageID, data)
}

// chatPerson is the person of a direct chat.
func (s *Stand) chatPerson(chat string) string {
	for _, p := range s.Bench.Seed().People {
		if slices.Contains(p.Chats, chat) {
			if c := s.Bench.Seed().Chat(chat); c != nil && c.Kind == "direct" {
				return p.Name
			}
		}
	}
	return ""
}

// Say sends a message of a person in a chat and returns the idempotency key of the task it
// makes.
func (s *Stand) Say(who, chat, text string) (string, error) {
	s.log.Add(KindChatIn, fmt.Sprintf("%s in %s: %s", who, chat, text), map[string]any{"person": who, "chat": chat, "text": text})
	return s.Bench.Say(who, chat, text)
}

// onModelCall prints a model call: the model, the tokens, the stop reason and the tool calls
// the model asked for.
func (s *Stand) onModelCall(req, resp []byte, status int) {
	// The fuse is checked after every model call as well as every second.
	defer s.checkFuseNow()
	var r struct {
		Model      string  `json:"model"`
		StopReason string  `json:"stop_reason"`
		Content    []block `json:"content"`
		Usage      struct {
			In          int64 `json:"input_tokens"`
			Out         int64 `json:"output_tokens"`
			CacheRead   int64 `json:"cache_read_input_tokens"`
			CacheCreate int64 `json:"cache_creation_input_tokens"`
		} `json:"usage"`
		Error *struct {
			Type, Message string
		} `json:"error"`
	}
	_ = json.Unmarshal(resp, &r)
	if status != 200 || r.Error != nil {
		msg := fmt.Sprintf("model call failed: status %d", status)
		if r.Error != nil {
			msg += " " + r.Error.Type + ": " + clip(r.Error.Message, 200)
		}
		s.log.Add(KindModel, msg, map[string]any{"status": status})
		return
	}
	in := r.Usage.In + r.Usage.CacheRead + r.Usage.CacheCreate
	p := s.price(r.Model)
	cost := int64(float64(in)*p[0] + float64(r.Usage.Out)*p[1]) // EUR per Mtok -> micro-euros
	msg := fmt.Sprintf("%s: %d tokens in, %d out, stop %s", r.Model, in, r.Usage.Out, r.StopReason)
	if s.cfg.Real {
		msg += fmt.Sprintf(", about %d µEUR", cost)
	}
	var tools []string
	for _, b := range r.Content {
		if b.Type == "tool_use" {
			tools = append(tools, b.Name+" "+string(b.Input))
		}
	}
	if len(tools) > 0 {
		msg += "; asks for " + strings.Join(tools, "; ")
	} else if s.cfg.Real {
		// The final answer of a real model is printed too: a result the core refuses
		// (output_schema_invalid) can then be read in the log.
		var text strings.Builder
		for _, b := range r.Content {
			if b.Type == "text" {
				text.WriteString(b.Text)
			}
		}
		msg += "; answers " + clip(strings.Join(strings.Fields(text.String()), " "), 600)
	}
	s.log.Add(KindModel, msg, map[string]any{"model": r.Model, "input_tokens": in, "output_tokens": r.Usage.Out, "stop": r.StopReason,
		"cost_micros": cost, "tools": tools})
}

// price is the price of the model of an answer; a provider that answers with another id than the
// profile named is counted at the highest price of the run.
func (s *Stand) price(model string) [2]float64 {
	if p, ok := s.cfg.Prices[model]; ok {
		return p
	}
	var top [2]float64
	for _, p := range s.cfg.Prices {
		if p[1] > top[1] {
			top = p
		}
	}
	return top
}

// checkFuseNow checks the fuse after a model call, answered or not.
func (s *Stand) checkFuseNow() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.CheckFuse(ctx); err != nil {
		s.log.Addf(KindFuse, "the fuse check failed: %v", err)
	}
}

// gate lets a model call reach the provider only below the cap. P4 reserved the call's worst
// case in the store before it calls the provider, so the spend read here includes this call: a
// call whose worst case would reach the cap is refused, and so is every call once the fuse
// tripped. A spend that cannot be read refuses the call too.
func (s *Stand) gate() bool {
	if s.Tripped() {
		return false
	}
	if s.cfg.MaxMicros <= 0 {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	n, err := s.Bench.SpendTotal(ctx)
	if err != nil {
		s.log.Addf(KindFuse, "the spend could not be read, the model call is refused: %v", err)
		return false
	}
	// onModelCall sees the refused call too and trips the fuse.
	return n < s.cfg.MaxMicros
}

// errStopped is the error of a director step after the fuse tripped.
var errStopped = errors.New("the money fuse stopped the run")

// waitFor polls cond until it holds, the timeout passes or the fuse trips.
func (s *Stand) waitFor(ctx context.Context, timeout time.Duration, cond func() (bool, error)) error {
	deadline := time.Now().Add(timeout)
	for {
		ok, err := cond()
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		if time.Now().After(deadline) {
			return context.DeadlineExceeded
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.stopped:
			return errStopped
		case <-time.After(500 * time.Millisecond):
		}
	}
}
