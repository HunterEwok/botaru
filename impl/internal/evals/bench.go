// SPDX-License-Identifier: AGPL-3.0-only

package evals

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/HunterEwok/botaru/impl/internal/agent"
	"github.com/HunterEwok/botaru/impl/internal/api"
	"github.com/HunterEwok/botaru/impl/internal/channel/telegram"
	"github.com/HunterEwok/botaru/impl/internal/core"
	"github.com/HunterEwok/botaru/impl/internal/gateway"
	"github.com/HunterEwok/botaru/impl/internal/ids"
	"github.com/HunterEwok/botaru/impl/internal/manifest"
	"github.com/HunterEwok/botaru/impl/internal/mockprovider"
	"github.com/HunterEwok/botaru/impl/internal/p1client"
	"github.com/HunterEwok/botaru/impl/internal/spec"
	"github.com/HunterEwok/botaru/impl/internal/store"
	"github.com/HunterEwok/botaru/impl/internal/testbench"
	"github.com/HunterEwok/botaru/impl/internal/texts"
	"github.com/HunterEwok/botaru/impl/internal/toolhost"
)

// ModelConfig is the model side of a run: the mock provider, or a real provider of the
// anthropic binding with the key of a separate eval workspace (docs/quality.md, "Eval test
// bench": one key per tenant, held only by P4).
type ModelConfig struct {
	// Real is true for a run against a real provider. Without it the bench runs the mock
	// provider, which answers only a case's scripted mock model and nothing else.
	Real bool
	// BaseURL is the provider address; empty is the binding's own.
	BaseURL string
	// Key is the provider key of the eval workspace; the bench gives it to P4 for every tenant.
	Key string
	// ProfilesDir holds the ModelProfile manifests of the real run: each replaces the example
	// profile of the same name, so the run names real models and prices.
	ProfilesDir string
}

// BenchConfig is what a bench is built from.
type BenchConfig struct {
	Root string
	// DSN is an empty database; the bench creates the store in it as a test bench store.
	DSN string
	// WorkDir holds the generated data, the plugin files and the dummy units.
	WorkDir string
	Model   ModelConfig
	// Plugin returns how the toolhost starts one own plugin with its flags (the botaru-plugin
	// arguments): the program, its arguments and its environment.
	Plugin func(kind string, args []string) ([]string, []string)
	Logger *slog.Logger
	// Cases are the cases of the run plan (active and in version for the claim). A person who
	// sends one of them that checks the first contact (DLV-12) is not confirmed through /me.
	Cases []*Case
}

// benchRoles are the roles the bench sets up, with their example manifests: the first bot and
// the reference bot ops-reader. A case of another role folder is reported as not run.
var benchRoles = map[string][]string{
	"hello":      {"examples/hello-bot/role.yaml", "examples/hello-bot/tasktype.yaml"},
	"ops-reader": {"examples/roles/ops-reader/role.yaml"},
}

// Bench is a running test bench: the store, the core, P1 with P4 and P2 listeners, the fake Bot
// API server, the fake monitoring API, the Telegram channel, one agent per role and the
// toolhosts of the demo machines.
type Bench struct {
	cfg     BenchConfig
	st      *store.Store
	c       *core.Core
	sp      *spec.Spec
	seed    *Seed
	p1, p2  string
	ops     map[string]string
	bot     *fakeBotAPI
	rec     *recorder
	mock    *mockModel
	cancel  context.CancelFunc
	done    sync.WaitGroup
	closers []func()
	people  map[string]string // seed name -> principal id
	// capHolds are the holds stopSpend set.
	capHolds map[string]string // tenant -> hold id
	// spendCap is the store spend total at which a running case stops (0: none).
	spendCap int64
	// ungranted are the seed groups of a person that stayed pending (PRM-13), with the reason.
	ungranted    map[string][]string
	ungrantedWhy map[string]string
	// pendingGroups are the privileged seed groups granted pending, before the code.
	pendingGroups map[string][]string
	// Profiles are the model profile names and models the bench applied.
	Profiles map[string]string
	// tenant settings the checks read
	linkAllowlist []string
	dataDir       string
	// resultSchemas are the result schemas of the applied task types (OUT-2).
	resultSchemas map[string]map[string]any
}

func hashHex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func listen(h http.Handler) (string, func(), error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, err
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(l) }()
	return "http://" + l.Addr().String(), func() { _ = srv.Close() }, nil
}

// StartBench builds and seeds a bench and starts its workers.
func StartBench(ctx context.Context, cfg BenchConfig) (_ *Bench, err error) {
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	b := &Bench{cfg: cfg, ops: map[string]string{}, bot: &fakeBotAPI{}, people: map[string]string{}, ungranted: map[string][]string{}, ungrantedWhy: map[string]string{}, pendingGroups: map[string][]string{}, Profiles: map[string]string{}}
	defer func() {
		if err != nil {
			b.Close()
		}
	}()
	if b.sp, err = spec.Load(cfg.Root); err != nil {
		return nil, err
	}
	tx, err := texts.Load(cfg.Root, "")
	if err != nil {
		return nil, err
	}
	if b.seed, err = LoadSeed(cfg.Root); err != nil {
		return nil, err
	}
	if b.st, err = store.Open(ctx, cfg.DSN); err != nil {
		return nil, err
	}
	b.closers = append(b.closers, b.st.Close)
	if err := b.st.Migrate(ctx); err != nil {
		return nil, err
	}
	b.c, err = core.New(ctx, core.Config{TokenKey: []byte("eval-" + ids.Secret(24)), TestBench: true, Operator: "the test bench operator",
		FarmHosts: []string{"core1", "backup1"}, Logger: cfg.Logger}, b.st, b.sp, tx)
	if err != nil {
		return nil, err
	}
	runCtx, cancel := context.WithCancel(context.Background())
	b.cancel = cancel
	b.done.Add(1)
	go func() { defer b.done.Done(); b.c.Run(runCtx) }()

	// P4: the provider of the profiles, the mock or the real one, behind the recorder that
	// keeps the model request bodies for the checks (docs/quality.md: in test bench mode P4
	// writes the model request bodies into the run results).
	b.rec = &recorder{next: http.DefaultTransport}
	baseURL := cfg.Model.BaseURL
	key := cfg.Model.Key
	if !cfg.Model.Real {
		b.mock = &mockModel{}
		u, stop, err := listen(b.mock)
		if err != nil {
			return nil, err
		}
		b.closers = append(b.closers, stop)
		baseURL, key = u, "mock-key"
	}
	keys := map[string]string{}
	for _, t := range b.seed.Tenants {
		keys[t] = key
	}
	providers := map[string]gateway.Provider{}
	for _, name := range []string{"provider-a", "provider-b"} {
		providers[name] = gateway.Provider{Binding: "anthropic", BaseURL: baseURL, Keys: keys}
	}
	gw, err := gateway.New(b.c, gateway.Config{Providers: providers, TestBench: true, Client: &http.Client{Transport: b.rec}, Logger: cfg.Logger})
	if err != nil {
		return nil, err
	}
	var creds []api.OperatorCred
	for _, t := range b.seed.Tenants {
		tok := "op-" + ids.Secret(16)
		b.ops[t] = tok
		creds = append(creds, api.OperatorCred{TokenSHA256: hashHex(tok), Principal: ids.PrincipalID(), Tenant: t})
	}
	srv, err := api.New(b.c, api.Config{Operators: creds, Logger: cfg.Logger}, gw)
	if err != nil {
		return nil, err
	}
	var stop func()
	if b.p1, stop, err = listen(srv.P1Handler()); err != nil {
		return nil, err
	}
	b.closers = append(b.closers, stop)
	if b.p2, stop, err = listen(srv.P2Handler()); err != nil {
		return nil, err
	}
	b.closers = append(b.closers, stop)
	botURL, stop, err := listen(b.bot)
	if err != nil {
		return nil, err
	}
	b.closers = append(b.closers, stop)
	zbx, stop, err := listen(fakeMonitor{})
	if err != nil {
		return nil, err
	}
	b.closers = append(b.closers, stop)

	if err := b.apply(ctx); err != nil {
		return nil, err
	}
	if err := b.seedPeople(ctx); err != nil {
		return nil, err
	}
	if err := b.startWorkers(ctx, runCtx, tx, botURL, zbx); err != nil {
		return nil, err
	}
	if err := b.confirmPending(ctx); err != nil {
		return nil, err
	}
	return b, nil
}

// Close stops the workers and the core and closes the listeners and the store.
func (b *Bench) Close() {
	if b.cancel != nil {
		b.cancel()
	}
	b.done.Wait()
	for i := len(b.closers) - 1; i >= 0; i-- {
		b.closers[i]()
	}
	b.closers = nil
}

// manifests are the bench set: the three tenants as kind: test, the demo channel, catalog and
// actions, the seed groups, the bench roles with their task types and tool plugins, and the
// model profiles (the example ones, each replaced by the real run's profile of the same name).
func (b *Bench) manifests() ([]map[string]any, error) {
	root := b.cfg.Root
	files := []string{"examples/tenants/demo/tenant.yaml", "examples/tenants/demo2/tenant.yaml", "examples/tenants/farm/tenant.yaml",
		"examples/tenants/demo/channels.yaml", "examples/tenants/demo/catalog.yaml", "examples/hello-bot/group.yaml"}
	roleNames := slices.Sorted(mapKeys(benchRoles))
	for _, r := range roleNames {
		files = append(files, benchRoles[r]...)
	}
	paths := make([]string, len(files))
	for i, f := range files {
		paths[i] = filepath.Join(root, f)
	}
	docs, err := manifest.ReadFiles(paths)
	if err != nil {
		return nil, err
	}
	var out []map[string]any
	have := map[string]bool{}
	claims, tools := map[string]bool{}, map[string]bool{}
	for _, d := range docs {
		if d.Kind == "Tenant" {
			asMap(d.Value["spec"])["kind"] = "test"
			if d.Name == "demo" {
				b.linkAllowlist = strs(asMap(d.Value["spec"])["link_allowlist"])
			}
		}
		if d.Kind == "Role" {
			sp := asMap(d.Value["spec"])
			for _, t := range strs(sp["claims"]) {
				claims[t] = true
			}
			for _, t := range strs(sp["tools"]) {
				tools[strings.SplitN(t, "/", 2)[0]] = true
			}
			// The catalog services a role may control run through the service plugin (APR-18).
			if len(strs(sp["service_control"])) > 0 {
				tools["service"] = true
			}
		}
		have[d.Kind+"/"+d.Name] = true
		out = append(out, d.Value)
	}
	defer func() {
		b.resultSchemas = map[string]map[string]any{}
		for _, m := range out {
			if str(m["kind"]) == "TaskType" {
				b.resultSchemas[str(asMap(m["metadata"])["name"])] = asMap(asMap(m["spec"])["result_schema"])
			}
		}
	}()
	// The plugins of the example actions' executors: apply checks each executor against the
	// plugin set (executor_unknown); no toolhost of the bench serves them.
	acts, err := testbench.ExampleActions(root)
	if err != nil {
		return nil, err
	}
	for _, a := range acts {
		if p := str(asMap(asMap(a["spec"])["executor"])["plugin"]); p != "" {
			tools[p] = true
		}
	}
	var more []string
	for t := range claims {
		if p := filepath.Join(root, "examples/tasktypes", t+".yaml"); !have["TaskType/"+t] && fileExists(p) {
			more = append(more, p)
		}
	}
	for t := range tools {
		if p := filepath.Join(root, "examples/tools", t, "tool.yaml"); fileExists(p) {
			more = append(more, p)
		}
	}
	slices.Sort(more)
	extra, err := manifest.ReadFiles(more)
	if err != nil {
		return nil, err
	}
	for _, d := range extra {
		out = append(out, d.Value)
	}
	// The seed groups; hello comes from the hello-bot set.
	caps, err := b.sp.Registry("spec/model/capabilities.yaml")
	if err != nil {
		return nil, err
	}
	out = append(out, b.seed.GroupManifests("demo", dataClasses(caps.Entries), []string{"hello"})...)
	out = append(out, acts...)
	profiles, err := filepath.Glob(filepath.Join(root, "examples/model-profiles/*.yaml"))
	if err != nil {
		return nil, err
	}
	pdocs, err := manifest.ReadFiles(profiles)
	if err != nil {
		return nil, err
	}
	byName := map[string]map[string]any{}
	for _, d := range pdocs {
		byName[d.Name] = d.Value
	}
	if b.cfg.Model.ProfilesDir != "" {
		real, err := manifest.ReadFiles([]string{b.cfg.Model.ProfilesDir})
		if err != nil {
			return nil, err
		}
		for _, d := range real {
			if d.Kind == "ModelProfile" {
				byName[d.Name] = d.Value
			}
		}
	}
	for _, n := range slices.Sorted(mapKeys(byName)) {
		b.Profiles[n] = str(asMap(byName[n]["spec"])["model"])
		out = append(out, byName[n])
	}
	if err := b.dropLaterResources(out); err != nil {
		return nil, err
	}
	// The bench set holds no farm charter: the farm manager is off (MGR-20).
	testbench.ManagerOff(out)
	if _, err := testbench.DropUnsupported(b.sp, out); err != nil {
		return nil, err
	}
	return out, nil
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// dataClasses are the grantable data classes in their order (PRM-5).
func dataClasses(entries []map[string]any) []string {
	type cls struct {
		id    string
		order int
	}
	var cs []cls
	for _, e := range entries {
		if str(e["kind"]) == "data_class" && e["reserved"] != true {
			o, _ := num(e["order"])
			cs = append(cs, cls{str(e["id"]), int(o)})
		}
	}
	slices.SortStableFunc(cs, func(a, b cls) int { return a.order - b.order })
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.id
	}
	return out
}

func (b *Bench) apply(ctx context.Context) error {
	ms, err := b.manifests()
	if err != nil {
		return err
	}
	var resp struct {
		Applied bool             `json:"applied"`
		Errors  []map[string]any `json:"errors"`
	}
	if err := b.P2(ctx, "demo", "apply", map[string]any{"commit": strings.Repeat("e", 40), "manifests": ms}, &resp); err != nil {
		return err
	}
	if !resp.Applied {
		return fmt.Errorf("the bench set was not applied: %v", resp.Errors)
	}
	return nil
}

// seedPeople adds the people of demo with their identities and groups (seed.yaml).
func (b *Bench) seedPeople(ctx context.Context) error {
	for _, p := range b.seed.People {
		if p.Tenant != "demo" || p.Kind != "human" || !p.Subject || p.ID == "" {
			continue
		}
		req := map[string]any{"op": "principal.add", "subject": p.ID, "kind": "human"}
		if p.Identity != "" {
			req["identity"] = p.Identity
		}
		if err := b.P2(ctx, "demo", "principal.add", req, nil); err != nil {
			return fmt.Errorf("seed %s: %w", p.Name, err)
		}
		b.people[p.Name] = p.ID
		for _, g := range p.Groups {
			var out struct {
				State string `json:"state"`
			}
			if err := b.P2(ctx, "demo", "grant", map[string]any{"op": "grant", "subject": p.ID, "group": g}, &out); err != nil {
				return fmt.Errorf("seed %s in %s: %w", p.Name, g, err)
			}
			if out.State == "pending" {
				// A privileged group waits for the person's verification code (PRM-13).
				b.pendingGroups[p.Name] = append(b.pendingGroups[p.Name], g)
			}
		}
	}
	return nil
}

// reCode is the verification code in the /me reply (spec/bindings/channels/messages, cmd.profile.code).
var reCode = regexp.MustCompile(`\b[0-9]{8}\b`)

// confirmPending activates the pending privileged seed groups the way an operator does (PRM-13):
// the person sends /me in their direct chat and reads the verification code in the reply, and
// the operator repeats the grant with that code. The bench writes no membership itself. A
// person who sends a first-contact case (DLV-12) of the run plan is left pending, since /me
// would be the first contact that case checks; such a person's groups count as ungranted.
func (b *Bench) confirmPending(ctx context.Context) error {
	firstContact := map[string]string{}
	others := map[string][]string{}
	for _, c := range b.cfg.Cases {
		if slices.Contains(c.Rules, "DLV-12") {
			firstContact[c.From] = c.ID
		} else {
			others[c.From] = append(others[c.From], c.ID)
		}
	}
	for _, name := range slices.Sorted(mapKeys(b.pendingGroups)) {
		gs := b.pendingGroups[name]
		if id := firstContact[name]; id != "" {
			if len(others[name]) > 0 {
				b.cfg.Logger.Warn("seed: privileged groups left pending for a first contact case, but the person also sends other cases of the run",
					"person", name, "groups", gs, "first_contact", id, "other_cases", others[name])
			}
			b.ungranted[name], b.ungrantedWhy[name] = gs, "privileged seed groups "+strings.Join(gs, ", ")+" stay pending (PRM-13): their code would come from /me, the first contact "+id+" checks"
			continue
		}
		p := b.seed.Person(name)
		chat := ""
		for _, ch := range p.Chats {
			if c := b.seed.Chat(ch); c != nil && c.Kind == "direct" {
				chat = ch
			}
		}
		if chat == "" {
			b.ungranted[name], b.ungrantedWhy[name] = gs, "privileged seed groups "+strings.Join(gs, ", ")+" stay pending (PRM-13): the person has no direct chat to read the code in /me"
			continue
		}
		mark := b.bot.sentCount()
		if _, _, err := b.play(ctx, name, chat, "/me", 30*time.Second); err != nil {
			return fmt.Errorf("seed %s: /me: %w", name, err)
		}
		code := ""
		ext := b.seed.Chat(chat).ExternalID
		for _, m := range b.bot.sentSince(mark) {
			if fmt.Sprint(m["chat_id"]) == ext {
				if c := reCode.FindString(str(m["text"])); c != "" {
					code = c
				}
			}
		}
		if code == "" {
			return fmt.Errorf("seed %s: the /me reply carries no verification code", name)
		}
		for _, g := range gs {
			var out struct {
				State string `json:"state"`
			}
			if err := b.P2(ctx, "demo", "grant", map[string]any{"op": "grant", "subject": p.ID, "group": g, "code": code}, &out); err != nil {
				return fmt.Errorf("seed %s in %s with the code: %w", name, g, err)
			}
			if out.State != "active" {
				return fmt.Errorf("seed %s in %s: %s after the code", name, g, out.State)
			}
		}
	}
	return nil
}

// enroll enrolls a worker and returns its P1 token.
func (b *Bench) enroll(ctx context.Context, req map[string]any) (string, error) {
	var out struct {
		Token string `json:"token"`
	}
	if err := b.P2(ctx, "demo", "worker.enroll", req, &out); err != nil {
		return "", err
	}
	return out.Token, nil
}

// startWorkers enrolls and starts the channel, one agent per bench role and the toolhosts of
// web1 and db1 with the own plugins over the bench data.
func (b *Bench) startWorkers(ctx, runCtx context.Context, tx *texts.Catalog, botURL, zbx string) error {
	b.dataDir = filepath.Join(b.cfg.WorkDir, "data")
	for _, d := range []string{"web1", "db1", "bin"} {
		if err := os.MkdirAll(filepath.Join(b.dataDir, d), 0o755); err != nil {
			return err
		}
	}
	if err := b.resetData(); err != nil {
		return err
	}
	// A dummy unit manager stands in for systemd: it answers every verb and changes nothing.
	systemctl := filepath.Join(b.dataDir, "bin", "systemctl")
	if err := os.WriteFile(systemctl, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		return err
	}
	token := filepath.Join(b.dataDir, "monitor-token")
	if err := os.WriteFile(token, []byte("eval-monitor-read\n"), 0o600); err != nil {
		return err
	}
	var workers []func(context.Context) error
	tok, err := b.enroll(ctx, map[string]any{"tenant": "demo", "role": "telegram", "kind": "channel", "target": "telegram"})
	if err != nil {
		return err
	}
	ch := telegram.New(telegram.Config{Core: &p1client.Client{BaseURL: b.p1, Token: tok}, Channel: benchChannel, BotAPI: botURL, BotToken: "EVAL:TOKEN",
		Texts: tx, LinkAllowlist: b.linkAllowlist, PollTimeoutS: 1, Logger: b.cfg.Logger})
	if err := ch.Hello(ctx); err != nil {
		return err
	}
	workers = append(workers, ch.Run)
	for _, role := range slices.Sorted(mapKeys(benchRoles)) {
		tok, err := b.enroll(ctx, map[string]any{"tenant": "demo", "role": role, "kind": "agent"})
		if err != nil {
			return err
		}
		ag := agent.New(agent.Config{Core: &p1client.Client{BaseURL: b.p1, Token: tok}, MaxTokens: 2000, Logger: b.cfg.Logger})
		if err := ag.Hello(ctx); err != nil {
			return err
		}
		workers = append(workers, ag.Run)
	}
	m := func(p string) string { return filepath.Join(b.cfg.Root, "examples/tools", p, "tool.yaml") }
	catalog := filepath.Join(b.cfg.Root, "examples/tenants/demo/catalog.yaml")
	hosts := []struct {
		host    string
		plugins map[string][]string
	}{
		{"web1", map[string][]string{
			"logs": {"-manifest", m("logs"), "-source", "nginx-access=" + b.dataPath("web1", "access.log"),
				"-source", "nginx-error=" + b.dataPath("web1", "error.log"), "-source", "app-api=" + b.dataPath("web1", "app-api.log")},
			"checks":  {"-manifest", m("checks"), "-results", b.dataPath("web1", "checks.json")},
			"cmd":     {"-manifest", m("cmd"), "-host", "web1", "-spec-root", b.cfg.Root},
			"service": {"-manifest", m("service"), "-systemctl", systemctl},
		}},
		// A plugin with a secret or egress gets a toolhost of its own (DEP-7).
		{"web1", map[string][]string{
			"monitor": {"-manifest", m("monitor"), "-api", zbx + "/api_jsonrpc.php", "-api-token-file", token, "-catalog", catalog, "-tenant", "demo"},
		}},
		{"db1", map[string][]string{
			"logs": {"-manifest", m("logs"), "-source", "postgresql=" + b.dataPath("db1", "postgresql.log")},
			"cmd":  {"-manifest", m("cmd"), "-host", "db1", "-spec-root", b.cfg.Root},
		}},
	}
	for _, h := range hosts {
		var ps []toolhost.Plugin
		names := slices.Sorted(mapKeys(h.plugins))
		for _, n := range names {
			cmd, env := b.cfg.Plugin(n, h.plugins[n])
			ps = append(ps, toolhost.Plugin{Name: n, Command: cmd, Env: env})
		}
		tok, err := b.enroll(ctx, map[string]any{"tenant": "demo", "role": "toolhost", "kind": "toolhost", "plugins": names, "placement": "host", "host": h.host})
		if err != nil {
			return err
		}
		th, err := toolhost.New(toolhost.Config{Core: &p1client.Client{BaseURL: b.p1, Token: tok}, Placement: "host", Plugins: ps,
			KillAfter: 2 * time.Second, Logger: b.cfg.Logger})
		if err != nil {
			return err
		}
		if err := th.Hello(ctx); err != nil {
			return fmt.Errorf("toolhost %s: %w", h.host, err)
		}
		workers = append(workers, th.Run)
	}
	for _, w := range workers {
		b.done.Add(1)
		go func() { defer b.done.Done(); _ = w(runCtx) }()
	}
	return nil
}

func (b *Bench) dataPath(host, file string) string { return filepath.Join(b.dataDir, host, file) }

// resetData writes the data a case without a generator sees: empty logs, the site check
// healthy, and a quiet database log.
func (b *Bench) resetData() error {
	for _, f := range []string{"access.log", "error.log", "app-api.log"} {
		if err := os.WriteFile(b.dataPath("web1", f), nil, 0o644); err != nil {
			return err
		}
	}
	if err := os.WriteFile(b.dataPath("db1", "postgresql.log"), []byte("LOG:  checkpoint complete\n"), 0o644); err != nil {
		return err
	}
	return b.writeChecks(map[string]any{"www-health": map[string]any{"ok": 3, "total": 3, "last_status": 200, "ms_max": 180},
		"api-health": map[string]any{"ok": 3, "total": 3, "last_status": 200, "ms_max": 90}})
}

func (b *Bench) writeChecks(v map[string]any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return os.WriteFile(b.dataPath("web1", "checks.json"), raw, 0o644)
}

// P2 calls a P2 operation as the operator of a tenant; a refusal is an error with its reason.
func (b *Bench) P2(ctx context.Context, tenant, op string, req, out any) error {
	raw, err := json.Marshal(req)
	if err != nil {
		return err
	}
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, b.p2+"/v1/p2/"+op, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	hr.Header.Set("Authorization", "Bearer "+b.ops[tenant])
	hr.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(hr)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		var p struct {
			Reason, Detail string
		}
		_ = json.Unmarshal(body, &p)
		return fmt.Errorf("%s: %d %s %s", op, resp.StatusCode, p.Reason, p.Detail)
	}
	if out != nil {
		return json.Unmarshal(body, out)
	}
	return nil
}

// recorder is the HTTP transport of P4 to the provider: it keeps every request body and the
// model name of every answer.
type recorder struct {
	next  http.RoundTripper
	mu    sync.Mutex
	calls []modelCall
}

type modelCall struct {
	Body   string
	Model  string
	Status int
}

func (r *recorder) RoundTrip(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		b, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		body = b
		req.Body = io.NopCloser(bytes.NewReader(b))
	}
	resp, err := r.next.RoundTrip(req)
	call := modelCall{Body: string(body)}
	if err == nil {
		raw, rerr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if rerr != nil {
			return nil, rerr
		}
		resp.Body = io.NopCloser(bytes.NewReader(raw))
		var m struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(raw, &m)
		call.Model, call.Status = m.Model, resp.StatusCode
	}
	r.mu.Lock()
	r.calls = append(r.calls, call)
	r.mu.Unlock()
	return resp, err
}

// since returns the calls after the first n.
func (r *recorder) since(n int) []modelCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n > len(r.calls) {
		return nil
	}
	return append([]modelCall(nil), r.calls[n:]...)
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

// mockModel is the mock provider of a mock run: it plays the scenario a case's setup scripts
// and refuses every call of a case without one, so a case never passes on an answer nobody
// scripted.
type mockModel struct {
	mu         sync.Mutex
	srv        *mockprovider.Server
	unscripted int
}

func (m *mockModel) set(sc *mockprovider.Scenario) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.unscripted = 0
	m.srv = nil
	if sc != nil {
		m.srv = mockprovider.New(sc)
	}
}

func (m *mockModel) unscriptedCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.unscripted
}

func (m *mockModel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	srv := m.srv
	if srv == nil {
		m.unscripted++
	}
	m.mu.Unlock()
	if srv != nil {
		srv.ServeHTTP(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(400)
	_, _ = io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"the mock model has no script for this case"}}`)
}

// fakeBotAPI is the fake Telegram Bot API server: it hands out the queued updates and records
// what the bot sent.
type fakeBotAPI struct {
	mu        sync.Mutex
	updates   []map[string]any
	sent      []map[string]any
	nextID    int64
	maxOffset float64
	nextUpd   float64
}

func (f *fakeBotAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	method := r.URL.Path[strings.LastIndexByte(r.URL.Path, '/')+1:]
	if !strings.HasPrefix(r.URL.Path, "/botEVAL:TOKEN/") {
		w.WriteHeader(404)
		_, _ = io.WriteString(w, `{"ok":false,"error_code":404,"description":"Not Found"}`)
		return
	}
	var req map[string]any
	_ = json.NewDecoder(r.Body).Decode(&req)
	f.mu.Lock()
	var result any = true
	switch method {
	case "getUpdates":
		offset, _ := req["offset"].(float64)
		if offset > f.maxOffset {
			f.maxOffset = offset
		}
		out := []map[string]any{}
		for _, u := range f.updates {
			if u["update_id"].(float64) >= offset {
				out = append(out, u)
			}
		}
		result = out
		if len(out) == 0 {
			f.mu.Unlock()
			// A long poll with nothing to hand out waits a little, as the real server does.
			select {
			case <-r.Context().Done():
			case <-time.After(200 * time.Millisecond):
			}
			f.mu.Lock()
		}
	case "sendMessage", "sendDocument":
		f.nextID++
		f.sent = append(f.sent, req)
		result = map[string]any{"message_id": 9000 + f.nextID, "chat": map[string]any{"id": req["chat_id"]}}
	}
	f.mu.Unlock()
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result})
}

// message queues a person's message and returns its update id.
func (f *fakeBotAPI) message(userID, chatID int64, private bool, text string) float64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextUpd++
	typ := "private"
	if !private {
		typ = "group"
	}
	f.updates = append(f.updates, map[string]any{"update_id": f.nextUpd, "message": map[string]any{
		"message_id": int64(f.nextUpd) + 100, "date": time.Now().Unix(), "from": map[string]any{"id": userID, "is_bot": false},
		"chat": map[string]any{"id": chatID, "type": typ}, "text": text}})
	return f.nextUpd
}

// taken says whether the channel acknowledged an update (polled past it).
func (f *fakeBotAPI) taken(id float64) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.maxOffset > id
}

func (f *fakeBotAPI) sentSince(n int) []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	if n > len(f.sent) {
		return nil
	}
	return append([]map[string]any(nil), f.sent[n:]...)
}

func (f *fakeBotAPI) sentCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

// fakeMonitor is the fake monitoring API (Zabbix JSON-RPC) of the monitor plugin: it knows the
// demo machines and has no problems, maintenance, items or history.
type fakeMonitor struct{}

func (fakeMonitor) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Method string         `json:"method"`
		Params map[string]any `json:"params"`
		ID     any            `json:"id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	var result any = []any{}
	if req.Method == "host.get" {
		var hosts []any
		for i, h := range []string{"web1.example.org", "db1.example.org", "ctd1.example.org"} {
			hosts = append(hosts, map[string]any{"hostid": fmt.Sprint(10101 + i), "host": h, "maintenance_status": "0", "maintenance_from": "0"})
		}
		if f := strs(asMap(req.Params["filter"])["host"]); len(f) > 0 {
			hosts = slices.DeleteFunc(hosts, func(h any) bool { return !slices.Contains(f, str(asMap(h)["host"])) })
		}
		if ids := strs(req.Params["hostids"]); len(ids) > 0 {
			hosts = slices.DeleteFunc(hosts, func(h any) bool { return !slices.Contains(ids, str(asMap(h)["hostid"])) })
		}
		result = hosts
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "result": result, "id": req.ID})
}

// benchChannel is the farm channel of the bench; the core keys a message's task by it and the
// update id.
const benchChannel = "telegram"

// spendTotal is the provider spend of all bench tenants in the store, reserved and settled,
// with corrections; the task budget reservations (unit task) are not spend.
func (b *Bench) spendTotal(ctx context.Context) (int64, error) {
	var n int64
	err := b.st.Pool.QueryRow(ctx, `SELECT coalesce(sum(eur_micros + coalesce(correction_eur_micros, 0)), 0) FROM spend WHERE unit <> 'task'`).Scan(&n)
	return n, err
}

// stopSpend holds the work of the bench tenants through P2 hold.set once the run stopped at the
// cap: no new tasks, no claims, and running attempts get cancel_requested and end without
// spending (HLD-3), so tasks of the run stop calling the model. The holds stay: releasing them
// would let the held tasks be claimed again.
func (b *Bench) stopSpend(ctx context.Context) error {
	if len(b.capHolds) > 0 {
		return nil
	}
	for _, tenant := range slices.Sorted(mapKeys(b.ops)) {
		var out map[string]any
		scope := map[string]any{"kind": "tenant"}
		scope["tenant"] = tenant
		req := map[string]any{"scope": scope,
			"effects": []any{"no_enqueue", "no_claim", "cancel_running"}, "reason": "budget_exhausted", "until": nil}
		if err := b.P2(ctx, tenant, "hold.set", req, &out); err != nil {
			return fmt.Errorf("hold.set %s: %w", tenant, err)
		}
		if b.capHolds == nil {
			b.capHolds = map[string]string{}
		}
		b.capHolds[tenant] = str(out["id"])
	}
	return nil
}

// releaseCapHolds releases the holds of stopSpend (a test seam: a run never does).
func (b *Bench) releaseCapHolds(ctx context.Context) error {
	for tenant, id := range b.capHolds {
		if err := b.P2(ctx, tenant, "hold.release", map[string]any{"id": id}, nil); err != nil {
			return err
		}
	}
	b.capHolds = nil
	return nil
}

// reResourceKind is the pointer of a catalog resource kind.
var reResourceKind = regexp.MustCompile(`^/spec/resources/([0-9]+)/kind$`)

// dropLaterResources removes from the catalogs the whole resources of a kind this core refuses
// as later (a corpus of profile knowledge, MAN-11): the bench serves no such resource, and a
// case that needs one is not run (setup.corpus, the corpus carrier). DropUnsupported does not
// drop a resource kind, since that would change what the entry says; the entry goes as a whole.
func (b *Bench) dropLaterResources(ms []map[string]any) error {
	for _, m := range ms {
		if m["kind"] != "Catalog" {
			continue
		}
		ptrs, err := b.sp.Unsupported(m)
		if err != nil {
			return err
		}
		drop := map[int]bool{}
		for _, p := range ptrs {
			if sm := reResourceKind.FindStringSubmatch(p); sm != nil {
				i, _ := strconv.Atoi(sm[1])
				drop[i] = true
			}
		}
		if len(drop) == 0 {
			continue
		}
		sp := asMap(m["spec"])
		var kept []any
		for i, r := range asList(sp["resources"]) {
			if drop[i] {
				b.cfg.Logger.Info("bench: catalog resource of a later kind left out", "resource", str(asMap(r)["id"]), "kind", str(asMap(r)["kind"]))
				continue
			}
			kept = append(kept, r)
		}
		sp["resources"] = kept
	}
	return nil
}
