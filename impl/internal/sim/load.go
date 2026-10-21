// SPDX-License-Identifier: AGPL-3.0-only

package sim

import (
	"context"
	"embed"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/HunterEwok/botaru/impl/internal/yamlx"
)

// The load generator of the continuous mode (botaru-sim -load): it sends each tenant the kinds of
// work of a load profile as a Poisson stream per tenant and kind, with a warm-up, peaks and hard
// caps on the costly work, and writes every work it starts to the events with its code.
//
//go:embed loads/*.yaml
var loadFS embed.FS

// LoadProfile is a load profile.
type LoadProfile struct {
	Name string `yaml:"name"`
	// Warmup ramps every rate up from zero over its length.
	Warmup time.Duration `yaml:"warmup"`
	// FaultLife is how long a fault of the profile stays before the generator heals it.
	FaultLife time.Duration `yaml:"fault_life"`
	Peaks     []LoadPeak    `yaml:"peaks"`
	Caps      LoadCaps      `yaml:"caps"`
	Works     []LoadWork    `yaml:"works"`
}

// LoadPeak multiplies every rate by Factor from At after the start for For.
type LoadPeak struct {
	At     time.Duration `yaml:"at"`
	For    time.Duration `yaml:"for"`
	Factor float64       `yaml:"factor"`
}

// LoadCaps are the hard caps on the costly work; zero is no cap. A work over a cap is not
// started and counts as capped.
type LoadCaps struct {
	// AnalysesPerHour caps the analyses of ops-reader per tenant in any hour.
	AnalysesPerHour int `yaml:"analyses_per_hour"`
	// ProposalsPerRun caps the proposals of the whole farm in the run: at it, no more analyses.
	ProposalsPerRun int `yaml:"proposals_per_run"`
	// ManagerTasksPerHour caps the tasks of the farm manager per tenant in any hour: at it, no
	// more work that wakes the manager (incidents).
	ManagerTasksPerHour int `yaml:"manager_tasks_per_hour"`
}

// LoadWork is one kind of work: its code, its rate per tenant and hour, and the tenants it goes
// to (all when empty).
type LoadWork struct {
	Code    string   `yaml:"code"`
	PerHour float64  `yaml:"per_hour"`
	Tenants []string `yaml:"tenants"`
}

// LoadNames are the names of the embedded load profiles.
func LoadNames() []string {
	ents, _ := loadFS.ReadDir("loads")
	var out []string
	for _, e := range ents {
		out = append(out, strings.TrimSuffix(e.Name(), ".yaml"))
	}
	slices.Sort(out)
	return out
}

// LoadLoad reads a load profile: an embedded one by name, else a file.
func LoadLoad(nameOrFile string) (*LoadProfile, error) {
	var raw []byte
	var err error
	if slices.Contains(LoadNames(), nameOrFile) {
		raw, err = loadFS.ReadFile("loads/" + nameOrFile + ".yaml")
	} else {
		raw, err = os.ReadFile(nameOrFile)
	}
	if err != nil {
		return nil, fmt.Errorf("load profile %s (embedded: %s): %w", nameOrFile, strings.Join(LoadNames(), ", "), err)
	}
	var p LoadProfile
	if err := yamlx.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("load profile %s: %w", nameOrFile, err)
	}
	return &p, p.check()
}

func (p *LoadProfile) check() error {
	if p.FaultLife <= 0 {
		p.FaultLife = 4 * time.Minute
	}
	seen := map[string]bool{}
	for _, w := range p.Works {
		if loadKinds[w.Code] == nil {
			return fmt.Errorf("load %s: unknown work %s (known: %s)", p.Name, w.Code, strings.Join(LoadCodes(), ", "))
		}
		if seen[w.Code] || w.PerHour < 0 || math.IsNaN(w.PerHour) {
			return fmt.Errorf("load %s: work %s repeated or with a bad rate", p.Name, w.Code)
		}
		seen[w.Code] = true
	}
	for _, k := range p.Peaks {
		if k.Factor <= 0 || k.For <= 0 {
			return fmt.Errorf("load %s: a peak needs a positive factor and length", p.Name)
		}
	}
	return nil
}

// factor is the multiplier of every rate at a time since the start: the warm-up ramp times the
// peaks that cover it.
func (p *LoadProfile) factor(since time.Duration) float64 {
	f := 1.0
	if p.Warmup > 0 && since < p.Warmup {
		f = float64(since) / float64(p.Warmup)
	}
	for _, k := range p.Peaks {
		if since >= k.At && since < k.At+k.For {
			f *= k.Factor
		}
	}
	return f
}

// Load cost classes.
const (
	classPlain    = ""
	classAnalysis = "analysis"
	classManager  = "manager"
)

// loadKind is a kind of work the generator can start.
type loadKind struct {
	what  string
	class string
	// role is the bot role the work needs in the tenant; "" for none.
	role string
	// host is the machine kind the work needs in the tenant; "" for none.
	host string
	// fault names the fault the work leaves on that machine for the fault life; a work whose
	// fault is already on the machine is skipped, so faults never overlap.
	fault string
	do    func(ctx context.Context, g *loadGen, tenant string) error
}

const analysisWhat = "an analysis of ops-reader's own results"

// loadKinds are the kinds of work by code (the codes of the load test brief).
var loadKinds = map[string]*loadKind{
	"R-01": {what: "the site is down: nginx fails, a person asks", role: "ops-reader", host: KindWeb, fault: "nginx", do: func(ctx context.Context, g *loadGen, t string) error {
		web := g.host(t, KindWeb)
		return g.fault(ctx, t, "nginx:"+web, "nginx-down on "+web, func(w *World) {
			s := w.Service(web, UnitNginx)
			s.State, s.Changed = "failed", time.Now().UTC()
		}, func(w *World) {
			if s := w.Service(web, UnitNginx); s != nil && s.State != "active" {
				s.State, s.Changed = "active", time.Now().UTC()
			}
		}, "bob", "/logs The site does not open, is it down?")
	}},
	"R-02": {what: "the site is slow, a person asks", role: "ops-reader", host: KindWeb, fault: "slow", do: func(ctx context.Context, g *loadGen, t string) error {
		key := FaultSlowSite + ":" + g.host(t, KindWeb)
		return g.fault(ctx, t, "slow:"+g.host(t, KindWeb), "slow site on "+g.host(t, KindWeb), func(w *World) { w.Faults[key] = true },
			func(w *World) { delete(w.Faults, key) }, "alice", "/logs Why is the site so slow right now?")
	}},
	"R-03": {what: "the disk fills, a person asks for a cleanup", class: classManager, role: "ops-reader", host: KindDB, fault: "disk", do: func(ctx context.Context, g *loadGen, t string) error {
		db := g.host(t, KindDB)
		key := FaultDiskFilling + ":" + db
		return g.fault(ctx, t, "disk:"+db, "disk filling on "+db, func(w *World) {
			w.Hosts[db].Disks["/data"].Fill(0.10)
			w.Faults[key] = true
		}, func(w *World) { healDisk(w, db, key) }, "bob", "/logs The disk on "+db+" is filling up, can you free some space?")
	}},
	"R-04": {what: "a health summary", role: "ops-reader", do: func(ctx context.Context, g *loadGen, t string) error {
		return g.say(t, "alice", "/health How are the machines doing?")
	}},
	"R-05": {what: "a staff /logs command", role: "ops-reader", do: func(ctx context.Context, g *loadGen, t string) error {
		return g.say(t, "bob", "/logs Show the errors of the last hour")
	}},
	"R-06": {what: "describe a raster and a layer", role: "geo-reader", do: func(ctx context.Context, g *loadGen, t string) error {
		return g.say(t, "alice", "/geo What is in the bathy-50m raster and the map-areas layer?")
	}},
	"R-08": {what: "why a pipeline run failed", role: "pipeline-reader", do: func(ctx context.Context, g *loadGen, t string) error {
		return g.say(t, "bob", "/pipeline Why did the last CTD run fail?")
	}},
	"R-17": {what: "an incident: the disk is full", class: classManager, host: KindDB, fault: "disk", do: func(ctx context.Context, g *loadGen, t string) error {
		db := g.host(t, KindDB)
		key := FaultDiskFilling + ":" + db
		return g.fault(ctx, t, "disk:"+db, "disk full on "+db, func(w *World) {
			w.Hosts[db].Disks["/data"].Fill(0.03)
			w.Faults[key] = true
		}, func(w *World) { healDisk(w, db, key) }, "", "")
	}},
	"R-19": {what: analysisWhat, class: classAnalysis, role: "ops-reader", do: func(ctx context.Context, g *loadGen, t string) error {
		return g.analysis(ctx, t)
	}},
}

// LoadCodes are the codes of the work the generator can start.
func LoadCodes() []string {
	out := make([]string, 0, len(loadKinds))
	for c := range loadKinds {
		out = append(out, c)
	}
	slices.Sort(out)
	return out
}

func healDisk(w *World, db, key string) {
	delete(w.Faults, key)
	if d := w.Hosts[db].Disks["/data"]; d != nil && d.FreeRatio < 0.5 {
		d.FreeRatio, d.TmpBytes = 0.82, 30e9
	}
}

// loadGen is one run of the generator.
type loadGen struct {
	d    *Director
	p    *LoadProfile
	r    *rand.Rand
	step time.Duration
	// insights keeps one analysis switch at a time: each is a commit of the farm git.
	insights sync.Mutex
	mu       sync.Mutex
	// analysing counts the analyses of each tenant waiting for their turn.
	analysing map[string]int
	// costly are the times the costly work of each "class tenant" started: an analysis when its
	// turn switched the schedule on, an incident when it broke the world. The caps count them
	// here, since the store's own count lags behind and the analyses are dated back (INS-4).
	costly map[string][]time.Time
	// faulty are the faults on the machines now, by "fault:host".
	faulty map[string]bool
	// started and skipped count the work by code and tenant; skipped by reason too.
	started map[string]int
	skipped map[string]int
	wg      sync.WaitGroup
}

// LoadSummary counts the work of a run of the generator: started by "code tenant", skipped by
// "code tenant reason".
type LoadSummary struct {
	Started map[string]int `json:"started"`
	Skipped map[string]int `json:"skipped"`
}

// RunLoad runs the load profile until the context ends and returns what it started and skipped.
// One stream per tenant and work: each second the number of arrivals is drawn from a Poisson
// law with the work's rate at that moment.
func (d *Director) RunLoad(ctx context.Context, p *LoadProfile, seed int64) LoadSummary {
	g := &loadGen{d: d, p: p, r: rand.New(rand.NewPCG(uint64(seed), 17)), step: d.S.cfg.Step, started: map[string]int{}, skipped: map[string]int{}, analysing: map[string]int{}, costly: map[string][]time.Time{}, faulty: map[string]bool{}}
	d.S.log.Addf(KindDirector, "load %s: %d kinds of work over %s", p.Name, len(p.Works), strings.Join(d.S.TenantNames(), ", "))
	start := time.Now()
	t := time.NewTicker(time.Second)
	defer t.Stop()
loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case <-t.C:
		}
		f := p.factor(time.Since(start))
		for _, tenant := range d.S.TenantNames() {
			for _, w := range p.Works {
				if len(w.Tenants) > 0 && !slices.Contains(w.Tenants, tenant) {
					continue
				}
				for range poisson(g.r, w.PerHour*f/3600) {
					g.start(ctx, w.Code, tenant)
				}
			}
		}
	}
	g.wg.Wait()
	g.mu.Lock()
	defer g.mu.Unlock()
	return LoadSummary{Started: g.started, Skipped: g.skipped}
}

// poisson draws a Poisson number with the mean (Knuth; the means here are far below one).
func poisson(r *rand.Rand, mean float64) int {
	if mean <= 0 {
		return 0
	}
	l, k, p := math.Exp(-mean), 0, 1.0
	for {
		p *= r.Float64()
		if p <= l {
			return k
		}
		k++
	}
}

// start starts one work in a tenant unless the tenant cannot take it or a cap stops it.
func (g *loadGen) start(ctx context.Context, code, tenant string) {
	k := loadKinds[code]
	if reason := g.blocked(ctx, k, tenant); reason != "" {
		g.count(g.skipped, code+" "+tenant+" "+reason)
		g.d.S.log.Add(KindLoad, fmt.Sprintf("%s in %s skipped: %s", code, tenant, reason), map[string]any{"code": code, "tenant": tenant, "skipped": reason})
		return
	}
	g.mu.Lock()
	switch {
	case k.class == classAnalysis:
		// An analysis counts as started when its turn comes (analysis).
		g.analysing[tenant]++
	case k.class == classManager:
		g.costly[k.class+" "+tenant] = append(g.costly[k.class+" "+tenant], time.Now())
	}
	if k.fault != "" {
		g.faulty[k.fault+":"+g.host(tenant, k.host)] = true
	}
	g.mu.Unlock()
	if k.class == classAnalysis {
		g.d.S.log.Add(KindLoad, fmt.Sprintf("%s in %s waits for its turn: %s", code, tenant, k.what), map[string]any{"code": code, "tenant": tenant, "class": k.class, "queued": true})
	} else {
		g.count(g.started, code+" "+tenant)
		g.d.S.log.Add(KindLoad, fmt.Sprintf("%s in %s: %s", code, tenant, k.what), map[string]any{"code": code, "tenant": tenant, "class": k.class})
	}
	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		if err := k.do(ctx, g, tenant); err != nil && ctx.Err() == nil {
			g.d.S.log.Addf(KindDirector, "load %s in %s: %v", code, tenant, err)
		}
	}()
}

func (g *loadGen) count(m map[string]int, key string) {
	g.mu.Lock()
	m[key]++
	g.mu.Unlock()
}

// blocked says why a work cannot start in a tenant now: no worker of its role or no machine of
// its kind there, or a cap; "" when it can.
func (g *loadGen) blocked(ctx context.Context, k *loadKind, tenant string) string {
	return g.blockedAs(ctx, k, tenant, true)
}

// blockedAs is blocked; withPending also counts the analyses started but not yet in the store.
func (g *loadGen) blockedAs(ctx context.Context, k *loadKind, tenant string, withPending bool) string {
	if k.role != "" && g.d.S.TenantWorkers(tenant, k.role) == 0 {
		return "no_worker"
	}
	if k.host != "" && g.host(tenant, k.host) == "" {
		return "no_machine"
	}
	g.mu.Lock()
	busy := k.fault != "" && g.faulty[k.fault+":"+g.host(tenant, k.host)]
	g.mu.Unlock()
	if busy {
		return "fault_on_machine"
	}
	c := g.p.Caps
	pool := g.d.S.Bench.Store().Pool
	n := func(q string, args ...any) (int, bool) {
		var v int
		err := pool.QueryRow(ctx, q, args...).Scan(&v)
		return v, err == nil
	}
	switch k.class {
	case classAnalysis:
		if tenant == g.d.S.Home() && g.d.S.Insights() {
			// The console or a scenario runs the home analysis; the load leaves it alone.
			return "insights_on"
		}
		if c.AnalysesPerHour > 0 && g.recent(classAnalysis, tenant)+g.pending(tenant, withPending) >= c.AnalysesPerHour {
			return "cap_analyses_per_hour"
		}
		if c.ProposalsPerRun > 0 {
			v, ok := n(`SELECT count(*) FROM proposals WHERE tenant = ANY($1)`, g.d.S.TenantNames())
			if !ok {
				return "error"
			}
			if v >= c.ProposalsPerRun {
				return "cap_proposals_per_run"
			}
		}
	case classManager:
		if c.ManagerTasksPerHour > 0 {
			v, ok := n(`SELECT count(*) FROM tasks WHERE tenant = $1 AND type LIKE 'manager.%' AND created_at > now() - interval '1 hour'`, tenant)
			if !ok {
				return "error"
			}
			if max(v, g.recent(classManager, tenant)) >= c.ManagerTasksPerHour {
				return "cap_manager_tasks_per_hour"
			}
		}
	}
	return ""
}

// recent counts the costly work of a class that started in a tenant in the last hour.
func (g *loadGen) recent(class, tenant string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	key, since := class+" "+tenant, time.Now().Add(-time.Hour)
	ts := g.costly[key]
	for len(ts) > 0 && ts[0].Before(since) {
		ts = ts[1:]
	}
	g.costly[key] = ts
	return len(ts)
}

// pending counts the analyses of a tenant waiting for their turn.
func (g *loadGen) pending(tenant string, with bool) int {
	if !with {
		return 0
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.analysing[tenant]
}

// host is the tenant's first machine of a kind, "" when it has none.
func (g *loadGen) host(tenant, kind string) string { return g.d.S.TenantHost(tenant, kind) }

// say sends a person's message in their direct chat in the tenant.
func (g *loadGen) say(tenant, who, text string) error {
	_, err := g.d.S.Say(g.d.S.PersonIn(tenant, who), g.d.S.PersonIn(tenant, who+"-dm"), text)
	return err
}

// fault breaks the world, lets a person ask a little later when who is set, and heals the fault
// after the profile's fault life.
func (g *loadGen) fault(ctx context.Context, tenant, key, what string, breakIt, heal func(*World), who, ask string) error {
	defer func() {
		g.mu.Lock()
		delete(g.faulty, key)
		g.mu.Unlock()
	}()
	if err := Update(g.d.S.cfg.Work, func(w *World) error {
		breakIt(w)
		w.Note("director", "", "load fault in %s: %s", tenant, what)
		return nil
	}); err != nil {
		return err
	}
	// When the run ends first, the fault is healed all the same, so the world is left whole.
	if who != "" && sleepCtx(ctx, 3*g.step) {
		if err := g.say(tenant, who, ask); err != nil {
			g.d.S.log.Addf(KindDirector, "load fault in %s: %v", tenant, err)
		}
	}
	sleepCtx(ctx, g.p.FaultLife)
	return Update(g.d.S.cfg.Work, func(w *World) error {
		heal(w)
		w.Note("director", "", "load fault in %s healed: %s", tenant, what)
		return nil
	})
}

// analysis turns the analysis schedule of the tenant on until its first new analysis is in the
// store, at most 75 seconds (the schedule runs each minute), and off again.
func (g *loadGen) analysis(ctx context.Context, tenant string) error {
	defer func() {
		g.mu.Lock()
		g.analysing[tenant]--
		g.mu.Unlock()
	}()
	g.insights.Lock()
	defer g.insights.Unlock()
	if ctx.Err() != nil {
		return nil
	}
	// Analyses wait here one after another; the caps are read again when its turn comes.
	if reason := g.blockedAs(ctx, &loadKind{class: classAnalysis}, tenant, false); reason != "" {
		g.count(g.skipped, "R-19 "+tenant+" "+reason)
		g.d.S.log.Add(KindLoad, fmt.Sprintf("R-19 in %s skipped at its turn: %s", tenant, reason), map[string]any{"code": "R-19", "tenant": tenant, "skipped": reason})
		return nil
	}
	on := time.Now()
	g.mu.Lock()
	g.costly[classAnalysis+" "+tenant] = append(g.costly[classAnalysis+" "+tenant], on)
	g.mu.Unlock()
	g.count(g.started, "R-19 "+tenant)
	g.d.S.log.Add(KindLoad, fmt.Sprintf("R-19 in %s: %s", tenant, analysisWhat), map[string]any{"code": "R-19", "tenant": tenant, "class": classAnalysis})
	if err := g.d.S.SetInsightsIn(ctx, tenant, true); err != nil {
		return err
	}
	pool := g.d.S.Bench.Store().Pool
	for end := on.Add(75 * time.Second); time.Now().Before(end) && sleepCtx(ctx, 5*time.Second); {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE tenant = $1 AND type = 'ops.insight.v1' AND created_at >= $2`, tenant, on).Scan(&n); err == nil && n > 0 {
			break
		}
	}
	return g.d.S.SetInsightsIn(context.WithoutCancel(ctx), tenant, false)
}

// sleepCtx sleeps for d; false when the context ended first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
