// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/HunterEwok/botaru/impl/internal/ids"
	"github.com/HunterEwok/botaru/impl/internal/spec"
	"github.com/HunterEwok/botaru/impl/internal/store"
	"github.com/HunterEwok/botaru/impl/internal/testbench"
	"github.com/HunterEwok/botaru/impl/internal/yamlx"
)

// fxRunner runs single cases of conformance/fixtures/manifests through apply with dry_run, as
// TestLintFixtures does, so that a test can name the rule a case checks.
type fxRunner struct {
	c        *Core
	root     string
	examples []map[string]any
	personas map[string]string
	base     map[string]bool
}

func newFxRunner(t *testing.T) *fxRunner {
	t.Helper()
	ctx := context.Background()
	root, err := spec.FindRoot("")
	if err != nil {
		t.Fatal(err)
	}
	sp, err := testbench.SharedSpec(root)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := testbench.SharedTexts(root)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, testbench.MigratedDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	c, err := New(ctx, Config{TokenKey: []byte("0123456789abcdef0123456789abcdef"), Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}, st, sp, tx)
	if err != nil {
		t.Fatal(err)
	}
	r := &fxRunner{c: c, root: root, examples: exampleManifests(t, root, sp), base: map[string]bool{}}
	if r.personas, err = testbench.SharedPersonas(root); err != nil {
		t.Fatal(err)
	}
	if res, err := c.Apply(ctx, Operator{Principal: ids.PrincipalID(), Tenant: "farm"}, ApplyRequest{Commit: strings.Repeat("0", 40), Manifests: r.examples, DryRun: true, Personas: r.personas}); err == nil {
		for _, e := range res.Errors {
			r.base[e.Kind+"/"+e.Name+"/"+e.Code] = true
		}
	}
	return r
}

// codes runs the case named name of the fixture file and returns its error codes, sorted.
func (r *fxRunner) codes(t *testing.T, file, name string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(r.root, "conformance/fixtures/manifests", file))
	if err != nil {
		t.Fatal(err)
	}
	var fx map[string]any
	if err := yamlx.Unmarshal(b, &fx); err != nil {
		t.Fatal(err)
	}
	var cs map[string]any
	for _, ca := range anyList(fx["cases"]) {
		if m, _ := ca.(map[string]any); m != nil && m["name"] == name {
			cs = m
		}
	}
	if cs == nil {
		t.Fatalf("%s: no case %q", file, name)
	}
	var own []map[string]any
	ownKeys := map[string]bool{}
	for _, m := range anyList(cs["manifests"]) {
		mm, _ := m.(map[string]any)
		own = append(own, deepCopy(mm))
		md, _ := mm["metadata"].(map[string]any)
		ownKeys[mm["kind"].(string)+"/"+md["name"].(string)] = true
	}
	withEx, ok := cs["with_examples"].(bool)
	if !ok {
		withEx, _ = fx["with_examples"].(bool)
	}
	docs := own
	if withEx {
		keys := map[string]bool{}
		for _, m := range own {
			keys[objKey(m)] = true
		}
		docs = nil
		for _, m := range r.examples {
			if !keys[objKey(m)] {
				docs = append(docs, deepCopy(m))
			}
		}
		docs = append(docs, own...)
	}
	r.c.cfg.FarmHosts = []string{"core1", "backup1"}
	res, err := r.c.Apply(context.Background(), Operator{Principal: ids.PrincipalID(), Tenant: "farm"}, ApplyRequest{Commit: strings.Repeat("0", 40), Manifests: docs, DryRun: true, Personas: r.personas})
	if err != nil {
		if p, ok := AsProblem(err); ok && p != nil {
			return []string{p.Reason}
		}
		t.Fatal(err)
	}
	var out []string
	for _, e := range res.Errors {
		if withEx && r.base[e.Kind+"/"+e.Name+"/"+e.Code] && !ownKeys[e.Kind+"/"+e.Name] || e.Code == "eval_report_missing" {
			continue
		}
		if !slices.Contains(out, e.Code) {
			out = append(out, e.Code)
		}
	}
	sort.Strings(out)
	return out
}

// fxCase is one fixture case and the code a rule expects of it ("" for a set that passes).
type fxCase struct {
	rule, file, name, want string
}

// TestBackfillLintCases runs the manifest fixtures that check rules of the apply lint and
// asserts the code each rule gives; every row names its rule.
func TestBackfillLintCases(t *testing.T) {
	r := newFxRunner(t)
	for _, tc := range []fxCase{
		// INJ-1: a free string on a production machine does not pass the closed schema, nor
		// does an action argument with its own pattern.
		{"INJ-1", "tools.yaml", "string without a format on a production machine", "schema_invalid"},
		{"INJ-1", "actions.yaml", "action arguments with their own pattern", "schema_invalid"},
		{"INJ-1", "tools.yaml", "read plugin with target_from and redaction", ""},
		// INJ-3: untrusted_output false only for output of numbers and enum values.
		{"INJ-3", "tools.yaml", "untrusted_output false with free text output", "untrusted_output_false_text"},
		// INJ-14: execve without a shell, an absolute path, a whole parameter element after --.
		{"INJ-14", "tools.yaml", "command through a shell", "cmd_forbidden_program"},
		{"INJ-14", "tools.yaml", "program path is not absolute", "cmd_argv_path"},
		{"INJ-14", "tools.yaml", "parameter inside an element and without a separator", "cmd_param_position"},
		{"INJ-14", "tools.yaml", "command parameter with two schemas", "cmd_param_conflict"},
		// MAN-1: the wrapper, a registered kind, no status in git, the apiVersion, references.
		{"MAN-1", "wrapper.yaml", "role in a wrapper passes", ""},
		{"MAN-1", "wrapper.yaml", "kind without a schema", "unknown_kind"},
		{"MAN-1", "wrapper.yaml", "status field in git", "status_in_git"},
		{"MAN-1", "wrapper.yaml", "apiVersion after release 1.0.0 ahead of time", "api_version"},
		{"MAN-1", "wrapper.yaml", "extra field in spec", "schema_invalid"},
		{"MAN-1", "wrapper.yaml", "route refers to an undeclared type", "ref_missing"},
		// TEN-6: a global manifest carries no tenant and no tenant data.
		{"TEN-6", "wrapper.yaml", "global role with a tenant", "tenant_on_global"},
		{"TEN-6", "wrapper.yaml", "global task type with a tenant chat address", "global_tenant_data"},
		// MAN-4: the forbidden fields of a driver configuration.
		{"MAN-4", "driver-config.yaml", "egress value in a sink config", "driver_config_forbidden"},
		{"MAN-4", "driver-config.yaml", "channel config without forbidden fields", ""},
		// MAN-9: the routine fields of an action and one schema source behind args_from.
		{"MAN-9", "actions.yaml", "routine pipeline with all fields", ""},
		{"MAN-9", "actions.yaml", "routine without writes_only", "routine_fields"},
		{"MAN-9", "actions.yaml", "routine without runs_per_day", "routine_fields"},
		{"MAN-9", "tools.yaml", "write tool with args_from and its own schema", "args_from_second_schema"},
		// MEM-2: person memory only for summary types without untrusted output; a reporter
		// holds no memory.
		{"MEM-2", "role-profiles.yaml", "person memory with untrusted output", "memory_person"},
		{"MEM-2", "role-profiles.yaml", "person memory for a type of class internal", "memory_person"},
		{"MEM-2", "role-profiles.yaml", "reviewer reads bot memory", ""},
		{"MEM-2", "fx-rep-01.yaml", "reporter with person memory", "role_profile_memory"},
		// MSG-2: a message type declares its genus fields; state has no agent producer.
		{"MSG-2", "message-types.yaml", "bots' report", ""},
		{"MSG-2", "message-types.yaml", "self-report state from an agent", "state_agent_producer"},
		{"MSG-2", "message-types.yaml", "report without a ttl", "message_fields"},
		{"MSG-2", "message-types.yaml", "name does not start with a genus", "message_name"},
		// PRP-10: proposable: false for types other than work and types with alarms.
		{"PRP-10", "tasktypes.yaml", "report type without proposable false", "proposable_required_false"},
		{"PRP-10", "schedules-routes.yaml", "type with an alarm and a schedule that can be proposed", "proposable_required_false"},
		// REP-11: a type for a group chat declares summary_fields.
		{"REP-11", "tenant.yaml", "a type for a group chat without summary_fields", "summary_fields_required"},
		{"REP-11", "tenant.yaml", "a type with summary_fields for a bound chat", ""},
		// MON-4 (DEP-11): a farm node is never a resource of a tenant catalog.
		{"MON-4", "catalog.yaml", "monitor_host on a farm node", "farm_host_in_catalog"},
		{"MON-4", "catalog.yaml", "resource on a farm node", "farm_host_in_catalog"},
		// SEC-8 (GW-15): a provider of a model profile is a subprocessor of the tenant.
		{"SEC-8", "model-profiles.yaml", "subprocessor outside the privacy block", "subprocessor_undeclared"},
		// MAN-6: an error in the third file fails the whole set.
		{"MAN-6", "atomic-apply.yaml", "third file with a schema error fails the whole set", "cap_star"},
		{"MAN-6", "atomic-apply.yaml", "the same set with a valid group is applied in full", ""},
	} {
		t.Run(tc.rule+"/"+tc.name, func(t *testing.T) {
			got := r.codes(t, tc.file, tc.name)
			if tc.want == "" && len(got) > 0 || tc.want != "" && !slices.Contains(got, tc.want) {
				t.Fatalf("%s: %s: got %v, want %q", tc.rule, tc.name, got, tc.want)
			}
		})
	}
}
