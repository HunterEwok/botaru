// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// deployBase is a valid single-server description with a watchdog, external encrypted
// backups, a read toolhost with a container plugin and an action toolhost with its executor on
// a tenant machine, and a sandbox VM.
const deployBase = `
farm_hosts: [core1]
health_keys: [health-key]
watch: {notify_ref: watch-notify}
backup: {target: external, url: "https://backup.example.org/farm", location_id: offsite-1, encrypted: true, key_ref: backup-key}
secrets:
  backup-key: {rotation: overlap, window: 60d}
  health-key: {rotation: overlap, window: 1d}
  store-core: {rotation: overlap, window: 1d}
  watch-notify: {rotation: cutover}
  wt-channel: {rotation: overlap, window: 7d}
  wt-read: {rotation: overlap, window: 7d}
  wt-act: {rotation: overlap, window: 7d}
  wt-sbx: {rotation: overlap, window: 7d}
  metrics-token: {rotation: overlap, window: 7d}
hosts:
  core1:
    kind: production
    processes:
      - {name: farmd, kind: core, user: botaru-core, networks: [core, store], secrets: [store-core],
         listeners: {p1: {networks: [core]}, p2: {networks: [admin]}}}
      - {name: store, kind: store, user: botaru-store, networks: [store]}
      - {name: telegram, kind: channel, tenant: demo, user: botaru-ch, networks: [core], secrets: [wt-channel]}
      - {name: watch, kind: watch, user: botaru-watch, networks: [admin], secrets: [health-key, watch-notify]}
      - {name: backup, kind: backup, user: botaru-backup, networks: [store], secrets: [backup-key]}
  web1:
    kind: production
    processes:
      - {name: th-read, kind: toolhost, tenant: demo, placement: host, user: botaru-read, networks: [core, plug-metrics], secrets: [wt-read]}
      - {name: logs, kind: plugin, parent: th-read, mode: stdio}
      - {name: metrics, kind: plugin, parent: th-read, mode: container, user: botaru-metrics, networks: [plug-metrics],
         secrets: [metrics-token], egress: [metrics.example.org]}
      - {name: th-act, kind: toolhost, tenant: demo, placement: host, tools: write, user: botaru-act, networks: [core], secrets: [wt-act]}
    os_users: {ctd-qc-run: {demo: botaru-exec-demo}}
  sbx1:
    kind: sandbox
    processes:
      - {name: th-sbx, kind: toolhost, tenant: demo, placement: sandbox, user: botaru-sbx, networks: [core], secrets: [wt-sbx]}
`

// deployCodes lints a description and returns the codes of its errors.
func deployCodes(t *testing.T, c *Core, doc string) []string {
	t.Helper()
	l, err := LintDeploy(c.spec, []byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, f := range l.Errors() {
		if !slices.Contains(out, f.Code) {
			out = append(out, f.Code)
		}
	}
	slices.Sort(out)
	return out
}

// The deployment descriptions of the skeleton examples and of impl/deploy/compose pass the
// deployment lint, and the base of these tests does too (DEP-10, D-88).
func TestDeployLintExamples(t *testing.T) {
	c := lintCore(t)
	for _, p := range []string{"examples/deploy/farm.yaml", "examples/deploy/single.yaml", "impl/deploy/compose/deploy.yaml"} {
		b, err := os.ReadFile(filepath.Join(c.spec.Root, p))
		if err != nil {
			t.Fatal(err)
		}
		if got := deployCodes(t, c, string(b)); len(got) > 0 {
			t.Errorf("%s: %v", p, got)
		}
	}
	if got := deployCodes(t, c, deployBase); len(got) > 0 {
		t.Fatalf("base: %v", got)
	}
	if got := deployCodes(t, c, "farm_hosts: [core1]\nhosts: {core1: {kind: production, processes: [{name: x, kind: agent}]}}\n"); !slices.Equal(got, []string{"schema_invalid"}) {
		t.Fatalf("schema: %v", got)
	}
}

// Each placement rule of the deployment lint fails a description that breaks it (DEP-1,
// DEP-3, DEP-4, DEP-6, DEP-7, DEP-8, DEP-9, DEP-11, SEC-2, SEC-5, MON-2, RET-5, D-75, D-86).
func TestDeployLintRules(t *testing.T) {
	c := lintCore(t)
	cases := []struct {
		rule, old, new, want string
	}{
		// DEP-1: a sandbox VM holds only the sandbox toolhost; placement: sandbox only there.
		{"DEP-1", "{name: th-sbx, kind: toolhost, tenant: demo, placement: sandbox, user: botaru-sbx, networks: [core], secrets: [wt-sbx]}",
			"{name: th-sbx, kind: toolhost, tenant: demo, placement: sandbox, user: botaru-sbx, networks: [core], secrets: [wt-sbx]}\n      - {name: cache, kind: store, user: botaru-cache, networks: [cache]}", "sandbox_production_data"},
		{"DEP-1", "placement: host, user: botaru-read", "placement: sandbox, user: botaru-read", "sandbox_on_production"},
		// DEP-3, DEP-10: every process has an OS user.
		{"DEP-3", "{name: store, kind: store, user: botaru-store, networks: [store]}", "{name: store, kind: store, networks: [store]}", "process_user_missing"},
		// DEP-4: an executor user is separate per tenant and runs no farm process.
		{"DEP-4", "{ctd-qc-run: {demo: botaru-exec-demo}}", "{ctd-qc-run: {demo: botaru-act}}", "executor_user_shared"},
		{"DEP-4", "tools: write, user: botaru-act", "user: botaru-act", "executor_without_action_toolhost"},
		// DEP-6, DEP-7: a plugin container's network holds only it and its toolhost.
		{"DEP-6", "{name: telegram, kind: channel, tenant: demo, user: botaru-ch, networks: [core]", "{name: telegram, kind: channel, tenant: demo, user: botaru-ch, networks: [core, plug-metrics]", "plugin_network_shared"},
		// DEP-7: a child process plugin has no secret and no egress; a toolhost's networks lead
		// only to P1 and its plugin containers; toolhosts of two tenants do not share a user.
		{"DEP-7", "{name: logs, kind: plugin, parent: th-read, mode: stdio}", "{name: logs, kind: plugin, parent: th-read, mode: stdio, egress: [x.example.org]}", "plugin_container_required"},
		{"DEP-7", "networks: [core], secrets: [wt-act]", "networks: [core, lan], secrets: [wt-act]", "toolhost_network"},
		{"DEP-7", "tenant: demo, placement: sandbox, user: botaru-sbx", "tenant: demo2, placement: sandbox, user: botaru-read", "tenant_user_shared"},
		{"DEP-7", "{name: logs, kind: plugin, parent: th-read, mode: stdio}", "{name: logs, kind: plugin, parent: th-none, mode: stdio}", "plugin_parent_unknown"},
		// One toolhost described on two machines is one process: its token and its plugin's user
		// are not shared (SEC-2, DEP-7).
		{"SEC-2", "  sbx1:\n", "  web2:\n    kind: production\n    processes:\n" +
			"      - {name: th-read, kind: toolhost, tenant: demo, placement: host, user: botaru-read, networks: [core, plug-metrics], secrets: [wt-read]}\n" +
			"      - {name: metrics, kind: plugin, parent: th-read, mode: container, user: botaru-metrics, networks: [plug-metrics], secrets: [metrics-token], egress: [metrics.example.org]}\n" +
			"  sbx1:\n", ""},
		// SEC-6, TEN-3: a channel, a toolhost and a monitoring adapter name their tenant.
		{"SEC-6", "{name: telegram, kind: channel, tenant: demo, ", "{name: telegram, kind: channel, ", "process_tenant_missing"},
		// DEP-8: only core processes reach the store.
		{"DEP-8", "user: botaru-ch, networks: [core]", "user: botaru-ch, networks: [core, store]", "store_network_reachable"},
		// DEP-9: the action toolhost's OS user is no other process's.
		{"DEP-9", "user: botaru-act, networks: [core]", "user: botaru-read, networks: [core]", "action_user_shared"},
		// DEP-11: a farm node is listed, holds no tenant toolhost, and a machine serves one tenant.
		{"DEP-11", "farm_hosts: [core1]", "farm_hosts: [web1]", "farm_host_missing"},
		{"DEP-11", "farm_hosts: [core1]", "farm_hosts: [core1, web1]", "tenant_on_farm_host"},
		{"DEP-11", "tools: write, user: botaru-act, networks: [core], secrets: [wt-act]}", "tools: write, user: botaru-act, networks: [core], secrets: [wt-act]}\n      - {name: th-other, kind: toolhost, tenant: demo2, placement: host, user: botaru-other, networks: [core]}", "host_two_tenants"},
		// SEC-2: a secret is named for one process only.
		{"SEC-2", "user: botaru-ch, networks: [core], secrets: [wt-channel]", "user: botaru-ch, networks: [core], secrets: [wt-channel, store-core]", "secret_shared"},
		// SEC-5: each named secret has a rotation method, overlap with a window.
		{"SEC-5", "  wt-sbx: {rotation: overlap, window: 7d}\n", "", "secret_rotation_missing"},
		{"SEC-5", "wt-sbx: {rotation: overlap, window: 7d}", "wt-sbx: {rotation: overlap}", "secret_rotation_missing"},
		// MON-2: a watchdog without a monitoring system; the poller holds only the health key
		// and its own path, and no other process holds the health key.
		{"MON-2", "watch: {notify_ref: watch-notify}\n", "", "watch_missing"},
		{"MON-2", "secrets: [health-key, watch-notify]", "secrets: [health-key, watch-notify, store-core]", "poller_credentials"},
		{"MON-2", "secrets: [store-core],\n", "secrets: [store-core, health-key],\n", "poller_credentials"},
		// RET-5: no backup off the core machine and no encrypted external target.
		{"RET-5", "encrypted: true, key_ref: backup-key}", "encrypted: false, key_ref: backup-key}", "backup_external_required"},
		// D-86 (4): no test-control port on a production machine.
		{"DEP-10", "p2: {networks: [admin]}", "p2: {networks: [admin]}, test: {networks: [test]}", "listener_test_in_production"},
	}
	for _, tc := range cases {
		if !strings.Contains(deployBase, tc.old) {
			t.Fatalf("%s %s: the base has no %q", tc.rule, tc.want, tc.old)
		}
		got := deployCodes(t, c, strings.Replace(deployBase, tc.old, tc.new, 1))
		if tc.want == "" && len(got) > 0 {
			t.Errorf("%s: want no error, got %v", tc.rule, got)
		}
		if tc.want != "" && !slices.Contains(got, tc.want) {
			t.Errorf("%s: want %s, got %v", tc.rule, tc.want, got)
		}
	} // A core without a P1 listener leaves the P1 networks unknown: a toolhost network is a
	// note, not an error (DEP-7).
	doc := strings.Replace(strings.Replace(deployBase, ",\n         listeners: {p1: {networks: [core]}, p2: {networks: [admin]}}", "", 1),
		"networks: [core], secrets: [wt-act]", "networks: [core, lan], secrets: [wt-act]", 1)
	if got := deployCodes(t, c, doc); slices.Contains(got, "toolhost_network") {
		t.Errorf("unknown P1 networks: %v", got)
	}
}

// The secret uses of the description give secret.revoke its hold scopes: a plugin its plugin
// scope, a channel process named as a channel its channel, another tenant process the tenant,
// a farm process none (SEC-6, DEP-10).
func TestDeploySecretScopes(t *testing.T) {
	c := lintCore(t)
	l, err := LintDeploy(c.spec, []byte(deployBase))
	if err != nil || l.Err() != nil {
		t.Fatal(err, l.Err())
	}
	d := l.Description
	channel := func(tenant, name string) bool { return tenant == "demo" && name == "telegram" }
	for secret, want := range map[string][]HoldScope{
		"metrics-token": {{Kind: "plugin", Tenant: "demo", Name: "metrics"}},
		"wt-channel":    {{Kind: "channel", Tenant: "demo", Name: "telegram"}},
		"wt-read":       {{Kind: "tenant", Tenant: "demo"}},
		"store-core":    nil,
	} {
		if got := d.secretScopes(secret, channel, nil); !slices.Equal(got.Scopes, want) || got.Unresolved || got.Farm != (secret == "store-core") {
			t.Errorf("%s: %+v, want %v", secret, got, want)
		}
	}
	// The core holds a tenant's provider key: the tenant's scope, not a farm secret.
	if got := d.secretScopes("store-core", channel, []string{"demo"}); !slices.Equal(got.Scopes, []HoldScope{{Kind: "tenant", Tenant: "demo"}}) || got.Farm {
		t.Errorf("provider key %+v", got)
	}
}
