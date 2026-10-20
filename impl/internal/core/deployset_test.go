// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/HunterEwok/botaru/impl/internal/manifest"
	"github.com/HunterEwok/botaru/impl/internal/spec"
	"github.com/HunterEwok/botaru/impl/internal/testbench"
	"github.com/HunterEwok/botaru/impl/internal/toolspec"
)

// The apply of DEPLOY.md works as written: the file set of section 7 and of the compose section
// is the hello-bot set, and that set applies on a fresh store with -as-test -drop-unsupported
// (MAN-11, MGR-20: the example tenant has the farm manager off, since the set holds no farm
// charter with a seat profile).
func TestDeployApplySet(t *testing.T) {
	t.Parallel()
	root, err := spec.FindRoot("")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(root, "impl", "DEPLOY.md"))
	if err != nil {
		t.Fatal(err)
	}
	// Each apply command of the guide, with its continuation lines.
	cmd := regexp.MustCompile(`(?m)^\$[FX] apply (?:[^\n]*\\\n)*[^\n]*`)
	file := regexp.MustCompile(`examples/\S+\.yaml`)
	applies := cmd.FindAllString(string(raw), -1)
	if len(applies) < 2 {
		t.Fatalf("apply commands of DEPLOY.md: %d", len(applies))
	}
	for _, a := range applies {
		got := file.FindAllString(a, -1)
		if !slices.Equal(got, testbench.HelloBotFiles) {
			t.Errorf("DEPLOY.md applies %v, the hello-bot set is %v", got, testbench.HelloBotFiles)
		}
		if !strings.Contains(a, "-as-test") || !strings.Contains(a, "-drop-unsupported") {
			t.Errorf("DEPLOY.md apply without -as-test -drop-unsupported: %s", a)
		}
	}
	// newBench applies exactly that set and fails the test unless it is applied.
	b := newBench(t)
	cat, err := b.c.Catalog(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if cat.Tenants["demo"] == nil {
		t.Fatal("the demo tenant is not applied")
	}
}

// The way DEPLOY.md section 7 names to turn the farm manager on: the farm tenant and its charter,
// the seat model profile and the seat roles beside the first-bot set, with the manager on in the
// example tenant, apply (MGR-20).
func TestDeployManagerOn(t *testing.T) {
	t.Parallel()
	more := []string{"examples/tenants/farm/tenant.yaml", "examples/tenants/farm/governance.yaml",
		"examples/model-profiles/manager-std.yaml", "examples/roles/farm-manager/role.yaml", "examples/roles/farm-manager-reader/role.yaml"}
	newBenchWith(t, func(root string, ms []map[string]any) []map[string]any {
		paths := make([]string, len(more))
		for i, f := range more {
			paths[i] = filepath.Join(root, f)
		}
		docs, err := manifest.ReadFiles(paths)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range ms {
			if m["kind"] == "Tenant" {
				m["spec"].(map[string]any)["farm_manager"] = map[string]any{"enabled": true}
			}
		}
		for _, d := range docs {
			if d.Kind == "Tenant" {
				d.Value["spec"].(map[string]any)["kind"] = "test"
			}
			ms = append(ms, d.Value)
		}
		sp, err := testbench.SharedSpec(root)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := testbench.DropUnsupported(sp, ms); err != nil {
			t.Fatal(err)
		}
		return ms
	})
}

// D-114: only a built-in tool of a deferred rule is refused as deferred; a catalog plugin tool
// whose model name is the same (plugin link, tool hint) is a plugin tool and goes the plugin way.
func TestDeferredToolNotPlugin(t *testing.T) {
	t.Parallel()
	cat := &Catalog{Plugins: map[string]toolspec.Plugin{"link": {Name: "link", Tools: []toolspec.Tool{{Name: "hint"}}}}}
	if !pluginToolName(cat, "link_hint") {
		t.Fatal("link_hint of the plugin link is not a plugin tool")
	}
	for _, n := range []string{"skill_read", "link_other", "nounderscore"} {
		if pluginToolName(cat, n) {
			t.Fatalf("%s is a plugin tool", n)
		}
	}
}

// MON-5, D-88: the routes on a problem of the monitoring system run exactly when P2 status
// claims the profile monitor, that is only with the monitoring system configured.
func TestRoutesFollowTheClaim(t *testing.T) {
	t.Parallel()
	c := lintCore(t)
	for _, on := range []bool{false, true} {
		c.cfg.Monitor.Enabled = on
		if got := c.routesClaimed("ext.monitor.problem.v1"); got != on || slices.Contains(c.profiles(), "monitor") != on {
			t.Fatalf("monitor %v: routes %v, profiles %v", on, got, c.profiles())
		}
	}
	if !c.routesClaimed("ext.pipeline.done.v1") || !c.routesClaimed("job.finished.v1") {
		t.Fatal("ingress and messaging routes are claimed")
	}
}
