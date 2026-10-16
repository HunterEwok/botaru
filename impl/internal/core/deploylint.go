// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"

	"github.com/HunterEwok/botaru/impl/internal/spec"
	"github.com/HunterEwok/botaru/impl/internal/yamlx"
)

// The deployment lint (DEP-10). The deployment description is data by the schema
// spec/model/deploy-topology.schema.json: the machines and their kind, the farm processes and
// plugins with their OS user, networks and named secrets, the OS users of action executors per
// tenant, the monitoring system or the watchdog, the backup location, the farm nodes and the
// rotation method of each named secret. The file is not run. The core lints it at start, before
// it takes any work, and P2 doctor lints it again from the file (farmctl doctor), so a
// description changed for a rollout is checked before the rollout. This is the Go port of
// lint_deploy of ci/check_manifests.py with the checks the rule texts name on top of it: DEP-1,
// DEP-3, DEP-4, DEP-6, DEP-7, DEP-8, DEP-9, DEP-11, SEC-2, SEC-5, MON-2, watch_missing and
// backup_external_required. The codes of the Python lint keep their names; the codes it does not
// have are this build's own.

// DeployDescription is a deployment description (deploy-topology).
type DeployDescription struct {
	Monitoring *struct {
		URL     string `json:"url"`
		Address string `json:"address"`
	} `json:"monitoring"`
	Watch *struct {
		NotifyRef string `json:"notify_ref"`
	} `json:"watch"`
	Backup *struct {
		Target         string `json:"target"`
		Host           string `json:"host"`
		Encrypted      bool   `json:"encrypted"`
		KeyRef         string `json:"key_ref"`
		CredentialsRef string `json:"credentials_ref"`
	} `json:"backup"`
	FarmHosts  []string                    `json:"farm_hosts"`
	HealthKeys []string                    `json:"health_keys"`
	Secrets    map[string]DeploySecretRule `json:"secrets"`
	Hosts      map[string]DeployHost       `json:"hosts"`

	hostOf   map[*DeployProcess]string
	parentOf map[*DeployProcess]*DeployProcess
}

// DeploySecretRule is the rotation method of a named secret (SEC-5).
type DeploySecretRule struct {
	Rotation string `json:"rotation"`
	Window   string `json:"window"`
}

// DeployHost is one machine of the description.
type DeployHost struct {
	Kind      string                       `json:"kind"`
	Processes []*DeployProcess             `json:"processes"`
	OSUsers   map[string]map[string]string `json:"os_users"`
}

// DeployProcess is a farm or plugin process on a machine.
type DeployProcess struct {
	Name      string   `json:"name"`
	Kind      string   `json:"kind"`
	Tenant    string   `json:"tenant"`
	User      string   `json:"user"`
	Networks  []string `json:"networks"`
	Secrets   []string `json:"secrets"`
	Placement string   `json:"placement"`
	Tools     string   `json:"tools"`
	Parent    string   `json:"parent"`
	Mode      string   `json:"mode"`
	Egress    []string `json:"egress"`
	Listeners map[string]struct {
		Networks []string `json:"networks"`
	} `json:"listeners"`
}

// DeployFinding is one finding of the lint: an error fails the description, a note does not.
type DeployFinding struct {
	Level string `json:"level"`
	Code  string `json:"code"`
	Msg   string `json:"msg"`
}

// DeployLint is the outcome of the lint of one description.
type DeployLint struct {
	Description *DeployDescription
	Findings    []DeployFinding
}

// Errors are the findings that fail the description.
func (l *DeployLint) Errors() []DeployFinding {
	var out []DeployFinding
	for _, f := range l.Findings {
		if f.Level == "error" {
			out = append(out, f)
		}
	}
	return out
}

// Err is nil when the description passes, else an error that names every failing finding.
func (l *DeployLint) Err() error {
	errs := l.Errors()
	if len(errs) == 0 {
		return nil
	}
	parts := make([]string, len(errs))
	for i, e := range errs {
		parts[i] = "[" + e.Code + "] " + e.Msg
	}
	return fmt.Errorf("deployment description fails the lint (DEP-10): %s", strings.Join(parts, "; "))
}

// LintDeployFile reads and lints a deployment description file.
func LintDeployFile(sp *spec.Spec, path string) (*DeployLint, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return LintDeploy(sp, b)
}

// LintDeploy lints a deployment description (DEP-10). A description that does not parse as
// YAML is an error of the call; one that fails the schema is a finding schema_invalid, and the
// placement checks then do not run.
func LintDeploy(sp *spec.Spec, raw []byte) (*DeployLint, error) {
	var doc any
	if err := yamlx.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("deployment description: %w", err)
	}
	out := &DeployLint{}
	f := func(level, code, format string, args ...any) {
		out.Findings = append(out.Findings, DeployFinding{Level: level, Code: code, Msg: fmt.Sprintf(format, args...)})
	}
	if err := sp.Validate("deploy-topology", doc); err != nil {
		f("error", "schema_invalid", "deployment description: %v", err)
		return out, nil
	}
	b, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	var d DeployDescription
	if err := json.Unmarshal(b, &d); err != nil {
		return nil, err
	}
	d.index()
	out.Description = &d
	lintDeployDescription(&d, f)
	return out, nil
}

// index links each process to its machine and each plugin to its toolhost on that machine.
func (d *DeployDescription) index() {
	d.hostOf = map[*DeployProcess]string{}
	d.parentOf = map[*DeployProcess]*DeployProcess{}
	for _, hn := range slices.Sorted(mapKeys(d.Hosts)) {
		for _, p := range d.Hosts[hn].Processes {
			d.hostOf[p] = hn
		}
	}
	for _, hn := range slices.Sorted(mapKeys(d.Hosts)) {
		for _, p := range d.Hosts[hn].Processes {
			if p.Kind != "plugin" || p.Parent == "" {
				continue
			}
			for _, q := range d.Hosts[hn].Processes {
				if q.Name == p.Parent && q.Kind == "toolhost" {
					d.parentOf[p] = q
				}
			}
		}
	}
}

// processes are all processes of the description in the order of their machines.
func (d *DeployDescription) processes() []*DeployProcess {
	var out []*DeployProcess
	for _, hn := range slices.Sorted(mapKeys(d.Hosts)) {
		out = append(out, d.Hosts[hn].Processes...)
	}
	return out
}

// tenantOf is the tenant of a process: its own, or for a plugin the tenant of its toolhost.
func (d *DeployDescription) tenantOf(p *DeployProcess) string {
	if p.Tenant != "" {
		return p.Tenant
	}
	if t := d.parentOf[p]; t != nil {
		return t.Tenant
	}
	return ""
}

// userOf is the OS user a process runs as: its own, or for a child plugin its toolhost's.
func (d *DeployDescription) userOf(p *DeployProcess) string {
	if p.User != "" {
		return p.User
	}
	if p.Kind == "plugin" && p.Mode != "container" {
		if t := d.parentOf[p]; t != nil {
			return t.User
		}
	}
	return ""
}

// identity names a process across machines: a toolhost of a tenant described on two machines
// under one name is one process (one worker token), and so are its plugins; any other process
// is itself on its machine.
func (d *DeployDescription) identity(p *DeployProcess) string {
	switch {
	case p.Kind == "toolhost":
		return "toolhost/" + p.Tenant + "/" + p.Name
	case p.Kind == "plugin" && d.parentOf[p] != nil:
		return d.identity(d.parentOf[p]) + "/" + p.Name
	}
	return p.Kind + "/" + p.Name + "@" + d.hostOf[p]
}

// tenantKinds are the processes that act for one tenant and name it (SEC-6, TEN-3).
var tenantKinds = []string{"channel", "toolhost", "monitor"}

// farmKinds are the processes of the farm itself, which no tenant's work runs in.
var farmKinds = []string{"core", "store", "watch", "backup"}

// coreKinds are the processes of the core side: the core with farmctl next to it (DEP-8), the
// store and the backup of the store.
var coreKinds = []string{"core", "store", "backup"}

// farmNodeKinds are the processes that make a machine a farm node (DEP-11).
var farmNodeKinds = []string{"core", "store", "channel", "watch", "backup"}

func lintDeployDescription(d *DeployDescription, f func(level, code, format string, args ...any)) {
	procs := d.processes()
	isFarm := func(h string) bool { return slices.Contains(d.FarmHosts, h) }
	core := map[string]bool{}
	var p1Nets []string
	p1Known := true
	for _, p := range procs {
		if p.Kind == "core" {
			core[d.hostOf[p]] = true
			l, ok := p.Listeners["p1"]
			p1Known = p1Known && ok && len(l.Networks) > 0
			p1Nets = append(p1Nets, l.Networks...)
		}
	}
	// SEC-6, TEN-3: a channel, a toolhost and a monitoring adapter act for one tenant, and the
	// description names it, so a revoked secret holds that tenant's scope.
	for _, p := range procs {
		if slices.Contains(tenantKinds, p.Kind) && p.Tenant == "" {
			f("error", "process_tenant_missing", "process %s of kind %s on %s names no tenant (SEC-6, TEN-3)", p.Name, p.Kind, d.hostOf[p])
		}
	}
	production := false
	for h := range core {
		if d.Hosts[h].Kind == "production" {
			production = true
		}
	}

	// Listeners of the core (A4, D-86 (4), IDN-7): no test-control port on a production machine,
	// and the P2 network is not a worker network (a note).
	workerNets := map[string]bool{}
	for _, p := range procs {
		if !slices.Contains([]string{"core", "store", "backup", "watch"}, p.Kind) {
			for _, n := range p.Networks {
				workerNets[n] = true
			}
		}
	}
	for _, p := range procs {
		if p.Kind != "core" {
			continue
		}
		h := d.hostOf[p]
		if _, ok := p.Listeners["test"]; ok && d.Hosts[h].Kind == "production" {
			f("error", "listener_test_in_production", "machine %s: process %s has a stand port listener on a kind: production machine (D-86 (4))", h, p.Name)
		}
		var shared []string
		for _, n := range p.Listeners["p2"].Networks {
			if workerNets[n] {
				shared = append(shared, n)
			}
		}
		if len(shared) > 0 {
			sort.Strings(shared)
			f("note", "listener_admin_worker_network", "machine %s: P2 network %v is among the worker networks (IDN-7)", h, shared)
		}
	}

	// MON-2, D-88: a monitoring system or a watchdog with its own notification path.
	watchHosts := map[string]bool{}
	for _, p := range procs {
		if p.Kind == "watch" {
			watchHosts[d.hostOf[p]] = true
		}
	}
	if d.Monitoring == nil {
		if len(watchHosts) == 0 || d.Watch == nil || d.Watch.NotifyRef == "" {
			f("error", "watch_missing", "no monitoring system (the monitor profile is not claimed), and no kind: watch watcher with watch.notify_ref: nobody checks that the core is alive (MON-2, D-88)")
		} else if onlyOn(watchHosts, core) {
			f("note", "watch_same_host", "the watcher is only on the core machine: it will not see that machine die (MON-2)")
		}
	} else if d.Watch != nil {
		f("note", "watch_with_monitoring", "there is both a monitoring system and a watcher: the monitoring system checks that the core is alive (MON-2)")
	}
	// The poller of health holds no credential of the core, the store, the channels or the
	// model provider: a watchdog holds only the health key and its own notification path, and
	// the health key is in no other process (MON-2).
	allowedWatch := slices.Clone(d.HealthKeys)
	if d.Watch != nil && d.Watch.NotifyRef != "" {
		allowedWatch = append(allowedWatch, d.Watch.NotifyRef)
	}
	for _, p := range procs {
		for _, s := range p.Secrets {
			if p.Kind == "watch" && !slices.Contains(allowedWatch, s) {
				f("error", "poller_credentials", "watcher %s on %s holds secret %s: a poller gets only the health key and its own notification path (MON-2)", p.Name, d.hostOf[p], s)
			}
			if p.Kind != "watch" && p.Kind != "monitor" && slices.Contains(d.HealthKeys, s) {
				f("error", "poller_credentials", "process %s on %s holds the health key %s, which only the poller holds (MON-2)", p.Name, d.hostOf[p], s)
			}
		}
	}

	// RET-5, D-88: backups on a separate machine, or encrypted to external storage.
	backupHosts := map[string]bool{}
	for _, p := range procs {
		if p.Kind == "backup" {
			backupHosts[d.hostOf[p]] = true
		}
	}
	offCore := false
	for h := range backupHosts {
		if !core[h] {
			offCore = true
		}
	}
	bk := d.Backup
	if production && !offCore {
		if bk == nil || bk.Target != "external" || !bk.Encrypted || bk.KeyRef == "" {
			f("error", "backup_external_required", "no copies on a separate machine: backup.target: external, encrypted: true and key_ref are required (RET-5, D-88)")
		}
	}
	if bk != nil && bk.Target == "host" && bk.Host != "" && core[bk.Host] {
		f("error", "backup_external_required", "backup.host %s is the core machine (RET-5)", bk.Host)
	}

	// SEC-5: each named secret has a rotation method; overlap names its window.
	named := map[string]bool{}
	for _, s := range d.HealthKeys {
		named[s] = true
	}
	if d.Watch != nil && d.Watch.NotifyRef != "" {
		named[d.Watch.NotifyRef] = true
	}
	if bk != nil {
		for _, s := range []string{bk.KeyRef, bk.CredentialsRef} {
			if s != "" {
				named[s] = true
			}
		}
	}
	for _, p := range procs {
		for _, s := range p.Secrets {
			named[s] = true
		}
	}
	for _, s := range slices.Sorted(mapKeys(named)) {
		m, ok := d.Secrets[s]
		switch {
		case !ok || (m.Rotation != "overlap" && m.Rotation != "cutover"):
			f("error", "secret_rotation_missing", "secret %s has no rotation method in secrets (SEC-5)", s)
		case m.Rotation == "overlap" && m.Window == "":
			f("error", "secret_rotation_missing", "secret %s: rotation overlap without a window (SEC-5)", s)
		}
	}

	// SEC-2: a process gets only the secrets named for it, as a file of its own: a secret
	// named by two processes would sit where both see it.
	owners := map[string][]string{}
	for _, p := range procs {
		for _, s := range p.Secrets {
			if id := d.identity(p); !slices.Contains(owners[s], id) {
				owners[s] = append(owners[s], id)
			}
		}
	}
	for _, s := range slices.Sorted(mapKeys(owners)) {
		if len(owners[s]) > 1 {
			f("error", "secret_shared", "secret %s is named by %v: each process gets its own secrets (SEC-2)", s, owners[s])
		}
	}

	// DEP-3, DEP-10: every process has an OS user that is not root; a plugin that runs as a
	// child process of its toolhost runs as the toolhost's user.
	for _, p := range procs {
		if p.Kind == "plugin" && p.Mode != "container" {
			continue
		}
		if p.User == "" {
			f("error", "process_user_missing", "process %s on %s has no OS user in the description (DEP-3, DEP-10)", p.Name, d.hostOf[p])
		}
	}

	// DEP-1: a sandbox VM holds only sandbox toolhosts, is no farm node and shares no secret;
	// a sandbox toolhost lives only on a sandbox VM, and a host toolhost not on one.
	for _, hn := range slices.Sorted(mapKeys(d.Hosts)) {
		h := d.Hosts[hn]
		if h.Kind != "sandbox" {
			for _, p := range h.Processes {
				if p.Kind == "toolhost" && p.Placement == "sandbox" {
					f("error", "sandbox_on_production", "sandbox toolhost %s is on the production machine %s: placement: sandbox lives only on a sandbox VM (DEP-1)", p.Name, hn)
				}
			}
			continue
		}
		if isFarm(hn) {
			f("error", "sandbox_production_data", "sandbox VM %s is a farm node (DEP-1)", hn)
		}
		if len(h.OSUsers) > 0 {
			f("error", "sandbox_production_data", "sandbox VM %s has action executors (DEP-1)", hn)
		}
		for _, p := range h.Processes {
			if p.Kind != "toolhost" || p.Placement != "sandbox" {
				f("error", "sandbox_production_data", "sandbox VM %s runs %s of kind %s: it has only the sandbox toolhost (DEP-1)", hn, p.Name, p.Kind)
			}
		}
	}

	// DEP-8: only core processes reach the store: no other process is on a network of the
	// store (farmctl on the core machine counts as the core).
	storeNets := map[string]bool{}
	for _, p := range procs {
		if p.Kind == "store" {
			for _, n := range p.Networks {
				storeNets[n] = true
			}
		}
	}
	for _, p := range procs {
		if slices.Contains(coreKinds, p.Kind) {
			continue
		}
		for _, n := range p.Networks {
			if storeNets[n] {
				f("error", "store_network_reachable", "process %s of kind %s on %s is on the store network %s: only core processes reach the store (DEP-8)", p.Name, p.Kind, d.hostOf[p], n)
			}
		}
	}

	// DEP-11: a machine with the core, the store, a channel, the watchdog or backups is a farm
	// node; a farm node holds no toolhost, plugin or action executor of a tenant; a machine
	// holds toolhosts, plugins and executors of one tenant only (D-75).
	for _, hn := range slices.Sorted(mapKeys(d.Hosts)) {
		h := d.Hosts[hn]
		tenants := map[string]bool{}
		for _, p := range h.Processes {
			if slices.Contains(farmNodeKinds, p.Kind) && !isFarm(hn) {
				f("error", "farm_host_missing", "machine %s runs %s of kind %s but is not in farm_hosts (DEP-11)", hn, p.Name, p.Kind)
			}
			if p.Kind == "toolhost" || p.Kind == "plugin" {
				if isFarm(hn) {
					f("error", "tenant_on_farm_host", "%s %s is on the farm node %s (DEP-11)", p.Kind, p.Name, hn)
				}
				if t := d.tenantOf(p); t != "" {
					tenants[t] = true
				}
			}
		}
		for _, action := range slices.Sorted(mapKeys(h.OSUsers)) {
			for t := range h.OSUsers[action] {
				tenants[t] = true
			}
			if isFarm(hn) {
				f("error", "tenant_on_farm_host", "the executor of %s is on the farm node %s (DEP-11)", action, hn)
			}
		}
		if len(tenants) > 1 {
			f("error", "host_two_tenants", "machine %s holds toolhosts, plugins or executors of tenants %v (DEP-11, D-75)", hn, slices.Sorted(mapKeys(tenants)))
		}
	}
	if bk != nil && bk.Target == "host" && bk.Host != "" && !isFarm(bk.Host) {
		f("error", "farm_host_missing", "backup.host %s is not in farm_hosts (DEP-11)", bk.Host)
	}

	// DEP-6, DEP-7: a plugin belongs to a toolhost on its machine. A child process plugin has
	// no secret and no egress; a plugin with a secret or egress runs in its own container with
	// its own OS user, on networks that hold only it and its toolhost. A toolhost's networks
	// lead only to P1 and to its plugins' containers.
	netMembers := map[string][]*DeployProcess{}
	for _, p := range procs {
		for _, n := range p.Networks {
			netMembers[n] = append(netMembers[n], p)
		}
	}
	for _, p := range procs {
		if p.Kind != "plugin" {
			continue
		}
		parent := d.parentOf[p]
		if parent == nil {
			f("error", "plugin_parent_unknown", "plugin %s on %s names no toolhost of its machine as parent (DEP-7)", p.Name, d.hostOf[p])
			continue
		}
		if p.Mode != "container" {
			if len(p.Secrets) > 0 || len(p.Egress) > 0 || len(p.Networks) > 0 {
				f("error", "plugin_container_required", "plugin %s on %s runs as a child process with a secret, egress or a network of its own: it runs in its own container (DEP-7)", p.Name, d.hostOf[p])
			}
			continue
		}
		if p.User != "" && (p.User == parent.User || slices.ContainsFunc(procs, func(q *DeployProcess) bool { return d.identity(q) != d.identity(p) && q.User == p.User })) {
			f("error", "plugin_user_shared", "plugin container %s on %s runs as %s, which another process uses (DEP-7)", p.Name, d.hostOf[p], p.User)
		}
		if len(p.Networks) == 0 {
			f("error", "plugin_network_shared", "plugin container %s on %s has no network of its own (DEP-7)", p.Name, d.hostOf[p])
		}
		for _, n := range p.Networks {
			for _, q := range netMembers[n] {
				if id := d.identity(q); id != d.identity(p) && id != d.identity(parent) {
					f("error", "plugin_network_shared", "plugin container %s on %s shares network %s with %s: it accepts connections only from its toolhost (DEP-6, DEP-7)", p.Name, d.hostOf[p], n, q.Name)
				}
			}
		}
	}
	for _, p := range procs {
		if p.Kind != "toolhost" {
			continue
		}
		for _, n := range p.Networks {
			if slices.Contains(p1Nets, n) {
				continue
			}
			own := slices.ContainsFunc(netMembers[n], func(q *DeployProcess) bool {
				return q.Kind == "plugin" && q.Mode == "container" && d.parentOf[q] == p
			})
			switch {
			case own:
			case !p1Known:
				f("note", "toolhost_network", "toolhost %s on %s is on network %s; the core names no P1 network, so the lint cannot tell whether it is P1 (DEP-6, DEP-7)", p.Name, d.hostOf[p], n)
			default:
				f("error", "toolhost_network", "toolhost %s on %s is on network %s, which is neither P1 nor a network of its plugin containers (DEP-6, DEP-7)", p.Name, d.hostOf[p], n)
			}
		}
	}

	// DEP-4, DEP-7, DEP-9: toolhosts of different tenants run as different OS users; the
	// action toolhost's user and each executor user is used by no other process and no other
	// tenant; an executor is on a machine with an action toolhost of its tenant.
	userTenants := map[string]map[string]bool{}
	for _, p := range procs {
		if p.Kind != "toolhost" || p.User == "" {
			continue
		}
		if userTenants[p.User] == nil {
			userTenants[p.User] = map[string]bool{}
		}
		userTenants[p.User][p.Tenant] = true
	}
	for _, u := range slices.Sorted(mapKeys(userTenants)) {
		if len(userTenants[u]) > 1 {
			f("error", "tenant_user_shared", "OS user %s runs toolhosts of tenants %v (DEP-7)", u, slices.Sorted(mapKeys(userTenants[u])))
		}
	}
	for _, p := range procs {
		if p.Kind != "toolhost" || p.Tools != "write" || p.User == "" {
			continue
		}
		// The same action toolhost described on two machines, and its child plugins, are one.
		same := func(q *DeployProcess) bool {
			return q != nil && q.Kind == "toolhost" && q.Name == p.Name && q.Tenant == p.Tenant
		}
		for _, q := range procs {
			if q != p && d.userOf(q) == p.User && !same(q) && !same(d.parentOf[q]) {
				f("error", "action_user_shared", "the action toolhost %s runs as %s, which %s also uses (DEP-9)", p.Name, p.User, q.Name)
			}
		}
	}
	execUsers := map[string]string{}
	for _, hn := range slices.Sorted(mapKeys(d.Hosts)) {
		h := d.Hosts[hn]
		for _, action := range slices.Sorted(mapKeys(h.OSUsers)) {
			for _, t := range slices.Sorted(mapKeys(h.OSUsers[action])) {
				u := h.OSUsers[action][t]
				if prev, ok := execUsers[u]; ok && prev != t {
					f("error", "executor_user_shared", "OS user %s executes actions of tenants %s and %s: an executor user is separate per tenant (DEP-4, TEN-6)", u, prev, t)
				}
				execUsers[u] = t
				if slices.ContainsFunc(procs, func(q *DeployProcess) bool { return d.userOf(q) == u }) {
					f("error", "executor_user_shared", "the executor user %s of %s is also the user of a farm process (DEP-4, DEP-9)", u, action)
				}
				if !slices.ContainsFunc(h.Processes, func(q *DeployProcess) bool { return q.Kind == "toolhost" && q.Tools == "write" && q.Tenant == t }) {
					f("error", "executor_without_action_toolhost", "machine %s has an executor of %s for %s but no action toolhost of that tenant (DEP-4, DEP-9)", hn, action, t)
				}
			}
		}
	}
}

// onlyOn says whether every key of a is a key of b.
func onlyOn(a, b map[string]bool) bool {
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

// secretUses are the uses of a named secret by the deployment description for secret.revoke
// (SEC-6): Scopes are the hold scopes, Farm says a process of the farm itself holds it, and
// Unresolved says a process holds it whose tenant the description does not give.
type secretUses struct {
	Scopes     []HoldScope
	Farm       bool
	Unresolved bool
}

// secretScopes are the uses the description gives a named secret: a plugin of a tenant is its
// plugin scope, a channel process named as a channel of its tenant is that channel, and any
// other process of a tenant (a toolhost, a monitoring adapter, a channel process by another
// name) is the whole tenant, so a revoked secret stops at least everything that could use it.
// The core holds the provider keys of the tenants' workspaces: providerTenants are the tenants
// whose provider key the secret is, each a tenant scope. Any other secret of the core, the
// store, the watchdog or the backup is the farm's own: the operator replaces it by the recipe.
// A process of any other kind without a tenant is unresolved.
func (d *DeployDescription) secretScopes(secret string, channels func(tenant, name string) bool, providerTenants []string) secretUses {
	var u secretUses
	add := func(s HoldScope) {
		if !slices.Contains(u.Scopes, s) {
			u.Scopes = append(u.Scopes, s)
		}
	}
	for _, p := range d.processes() {
		if !slices.Contains(p.Secrets, secret) {
			continue
		}
		t := d.tenantOf(p)
		switch {
		case p.Kind == "core" && len(providerTenants) > 0:
			for _, pt := range providerTenants {
				add(HoldScope{Kind: "tenant", Tenant: pt})
			}
		case slices.Contains(farmKinds, p.Kind):
			u.Farm = true
		case t == "":
			u.Unresolved = true
		case p.Kind == "plugin":
			add(HoldScope{Kind: "plugin", Tenant: t, Name: p.Name})
		case p.Kind == "channel" && channels(t, p.Name):
			add(HoldScope{Kind: "channel", Tenant: t, Name: p.Name})
		default:
			add(HoldScope{Kind: "tenant", Tenant: t})
		}
	}
	return u
}

// loadDeploy lints the deployment description of the settings at core start (DEP-10): a
// description that fails stops the start before the core takes work. Its farm nodes are the
// farm_hosts of the settings when those name none, and must equal them when they do (DEP-11).
func (c *Core) loadDeploy() error {
	if c.cfg.DeployFile == "" {
		return nil
	}
	l, err := LintDeployFile(c.spec, c.cfg.DeployFile)
	if err != nil {
		return err
	}
	for _, n := range l.Findings {
		if n.Level != "error" {
			c.log.Warn("deployment description", "code", n.Code, "msg", n.Msg)
		}
	}
	if err := l.Err(); err != nil {
		return err
	}
	d := l.Description
	switch {
	case len(c.cfg.FarmHosts) == 0:
		c.cfg.FarmHosts = slices.Clone(d.FarmHosts)
	case !sameSet(c.cfg.FarmHosts, d.FarmHosts):
		return fmt.Errorf("core: farm_hosts %v of the settings differ from farm_hosts %v of the deployment description (DEP-11)", c.cfg.FarmHosts, d.FarmHosts)
	}
	if err := c.coreSecretsKnown(d); err != nil {
		return err
	}
	c.deploy = d
	return nil
}

// coreSecretsKnown fails closed on the secrets of the core process (SEC-6): secret.revoke
// treats a core secret as the farm's own unless it is a provider key, so each secret the
// description gives the core must be a provider key of key_files (by its file name) or one of
// the core's own secrets, and each provider key must be among the core's secrets, or the
// tenants a key serves would get no hold when it is revoked.
func (c *Core) coreSecretsKnown(d *DeployDescription) error {
	held := map[string]bool{}
	for _, p := range d.processes() {
		if p.Kind != "core" {
			continue
		}
		for _, s := range p.Secrets {
			held[s] = true
			if _, key := c.cfg.ProviderKeySecrets[s]; !key && !slices.Contains(c.cfg.CoreSecrets, s) {
				return fmt.Errorf("core: the deployment description gives the core process the secret %s, which is neither a provider key of key_files nor a secret file of the settings or farm_secrets (SEC-6)", s)
			}
		}
	}
	for _, s := range slices.Sorted(mapKeys(c.cfg.ProviderKeySecrets)) {
		if !held[s] {
			return fmt.Errorf("core: the provider key %s of key_files is not among the core's secrets in the deployment description (SEC-6)", s)
		}
	}
	return nil
}

func sameSet(a, b []string) bool {
	x, y := slices.Clone(a), slices.Clone(b)
	slices.Sort(x)
	slices.Sort(y)
	return slices.Equal(x, y)
}

// doctorDeploy lints the deployment description file again for doctor (farmctl doctor,
// DEP-10): deploy.lint fails with the number of errors, and each finding is a check of its
// code, failing for an error and warning for a note. Without a description in the settings
// the one check deploy.description warns.
func (c *Core) doctorDeploy(add func(DoctorCheck)) {
	if c.cfg.DeployFile == "" {
		add(DoctorCheck{ID: "deploy.description", OK: false, Severity: "warn", Value: nil})
		return
	}
	l, err := LintDeployFile(c.spec, c.cfg.DeployFile)
	if err != nil {
		add(DoctorCheck{ID: "deploy.lint", OK: false, Severity: "fail", Value: "unreadable"})
		return
	}
	errs := len(l.Errors())
	add(DoctorCheck{ID: "deploy.lint", OK: errs == 0, Severity: map[bool]string{true: "info", false: "fail"}[errs == 0], Value: errs})
	seen := map[string]bool{}
	for _, n := range l.Findings {
		id := checkID("deploy." + n.Code)
		if seen[id] {
			continue
		}
		seen[id] = true
		sev := "warn"
		if n.Level == "error" {
			sev = "fail"
		}
		add(DoctorCheck{ID: id, OK: false, Severity: sev, Value: n.Msg})
	}
}

// channel says whether a tenant view has a channel of the name; a nil view has none.
func (tv *TenantView) channel(name string) bool {
	if tv == nil {
		return false
	}
	_, ok := tv.Channels[name]
	return ok
}
