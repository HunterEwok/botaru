// SPDX-License-Identifier: AGPL-3.0-only

package gate

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HunterEwok/botaru/impl/internal/evals"
	"github.com/HunterEwok/botaru/impl/internal/jcs"
	"github.com/HunterEwok/botaru/impl/internal/manifest"
)

const tenantYAML = `# Tenant demo.
apiVersion: botaru/v1alpha1
kind: Tenant
metadata: {name: demo}
spec:
  limits:
    max_queued: 10   # per type
    max_running: 4
  dispatch:
    rules:
      - match: {type: data.check.v1}
        group: checkers
        timeouts: {attempt: 5m}
`

const schedulesYAML = `# Schedules of demo.
apiVersion: botaru/v1alpha1
kind: Schedule
metadata: {name: nightly, tenant: demo}
spec:
  cron: "15 6 * * *"
  timezone: 'Europe/Berlin'
  task:
    type: data.check.v1
---
apiVersion: botaru/v1alpha1
kind: Schedule
metadata: {name: hourly, tenant: demo}
spec:
  cron: "0 * * * *"
  task: {type: data.check.v1}
`

const rolesYAML = `apiVersion: botaru/v1alpha1
kind: TaskType
metadata: {name: data.check.v1}
spec: {}
---
apiVersion: botaru/v1alpha1
kind: Role
metadata: {name: checker}
spec:
  claims: [data.check.v1]
---
apiVersion: botaru/v1alpha1
kind: BotGroup
metadata: {name: checkers, tenant: demo}
spec:
  select: {roles: [checker]}
`

const evalSchedule = `apiVersion: botaru/v1alpha1
kind: Schedule
metadata: {name: in-evals, tenant: demo}
spec:
  cron: "0 1 * * *"
  task: {type: data.check.v1}
`

func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := git(context.Background(), dir, args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func writeFile(t *testing.T, dir, rel, body string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// newRepo is a farm manifests checkout with one commit on branch main.
func newRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	dir := t.TempDir()
	gitT(t, dir, "init", "-q", "-b", "main")
	gitT(t, dir, "config", "user.name", "Gate Test")
	gitT(t, dir, "config", "user.email", "gate@example.org")
	gitT(t, dir, "config", "commit.gpgsign", "false")
	writeFile(t, dir, "tenants/demo/tenant.yaml", tenantYAML)
	writeFile(t, dir, "tenants/demo/schedules.yaml", schedulesYAML)
	writeFile(t, dir, "roles.yaml", rolesYAML)
	writeFile(t, dir, "evals/schedules.yaml", evalSchedule)
	writeFile(t, dir, "ci/check.py", "print('ok')\n")
	gitT(t, dir, "add", "-A")
	gitT(t, dir, "commit", "-q", "-m", "initial")
	return dir
}

func passAll(ids ...string) evals.SuiteResult {
	out := evals.SuiteResult{}
	for _, id := range ids {
		out[id] = evals.CaseOutcome{Case: id, Status: evals.Passed, Seconds: 5, ToolCalls: 3}
	}
	return out
}

func cronChange(cron string) Change {
	return Change{Proposal: "prp_1", Tenant: "demo", Ops: []Op{{Kind: "Schedule", Name: "nightly", Field: "spec.cron", Value: cron}}}
}

func stepOf(r *Report, name string) Step {
	for _, s := range r.Steps {
		if s.Name == name {
			return s
		}
	}
	return Step{}
}

func noWorktrees(t *testing.T, repo string) {
	t.Helper()
	if n := len(strings.Split(gitT(t, repo, "worktree", "list"), "\n")); n != 1 {
		t.Fatalf("worktrees left: %s", gitT(t, repo, "worktree", "list"))
	}
}

// A valid one-field change passes every step; the file differs by that line only, and the
// worktrees are gone afterwards (EVO-2, EVO-9).
func TestGateValidDiff(t *testing.T) {
	repo := newRepo(t)
	holdout := t.TempDir()
	var lintDirs, suiteRoots []string
	cfg := Config{Repo: repo,
		Lint: func(_ context.Context, dir string, sets []string) ([]string, error) {
			lintDirs = append(lintDirs, dir)
			b, _ := os.ReadFile(filepath.Join(dir, "tenants/demo/schedules.yaml"))
			if !strings.Contains(string(b), `"15 7 * * *"`) {
				return []string{"the candidate is not in the worktree"}, nil
			}
			return nil, nil
		},
		Suite: func(_ context.Context, root string, roles []string, h string) (evals.SuiteResult, error) {
			suiteRoots = append(suiteRoots, root)
			if len(roles) != 1 || roles[0] != "checker" {
				t.Errorf("roles %v", roles)
			}
			if h != "" {
				return passAll("h1", "h2"), nil
			}
			return passAll("v1", "v2"), nil
		},
		HoldoutDir: holdout}
	rep, err := Run(context.Background(), cfg, cronChange("15 7 * * *"))
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Passed() || rep.Code != "" || rep.HoldoutPassed == nil || !*rep.HoldoutPassed {
		t.Fatalf("report %+v", rep)
	}
	for _, s := range Steps {
		if st := stepOf(rep, s); st.Status != Passed {
			t.Fatalf("step %s: %+v", s, st)
		}
	}
	if rep.Fields != 1 || rep.DiffLines != 2 || len(rep.Files) != 1 || rep.Files[0] != "tenants/demo/schedules.yaml" {
		t.Fatalf("diff %d fields %d lines %v", rep.Fields, rep.DiffLines, rep.Files)
	}
	if !strings.Contains(rep.Diff, `-  cron: "15 6 * * *"`) || !strings.Contains(rep.Diff, `+  cron: "15 7 * * *"`) {
		t.Fatalf("diff %s", rep.Diff)
	}
	if len(rep.Order) != 1 || rep.Order[0] != "Schedule/demo/nightly" || rep.VisibleBase != 1 || rep.VisibleCandidate != 1 {
		t.Fatalf("order %v rates %v %v", rep.Order, rep.VisibleBase, rep.VisibleCandidate)
	}
	if len(suiteRoots) != 4 || suiteRoots[0] == suiteRoots[1] || lintDirs[0] != suiteRoots[1] {
		t.Fatalf("suite roots %v lint %v", suiteRoots, lintDirs)
	}
	for _, d := range append(lintDirs, suiteRoots...) {
		if !strings.Contains(d, "botaru-gate-") {
			t.Fatalf("not a gate worktree: %s", d)
		}
		if _, err := os.Stat(d); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("worktree %s left behind", d)
		}
	}
	noWorktrees(t, repo)
	// The checkout of the person is untouched.
	if st := gitT(t, repo, "status", "--porcelain"); st != "" {
		t.Fatalf("checkout changed: %s", st)
	}
	// Without lint, suite and hidden folder the steps are not_run, and the verdict still says
	// what ran.
	rep, err = Run(context.Background(), Config{Repo: repo}, cronChange("15 7 * * *"))
	if err != nil || !rep.Passed() || stepOf(rep, "lint").Status != NotRun || stepOf(rep, "holdout").Status != NotRun || rep.HoldoutPassed != nil {
		t.Fatalf("bare %+v %v", rep, err)
	}
}

// A change of the skeleton, a symbolic link, a protected field or kind, or a target under
// evals/ is forbidden_path_modified, whatever its size (EVO-2).
func TestGateForbiddenPaths(t *testing.T) {
	repo := newRepo(t)
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		ch    Change
		extra func(dir string) error
	}{
		{"evals", cronChange("15 7 * * *"), func(dir string) error {
			return os.WriteFile(filepath.Join(dir, "evals/cases.yaml"), []byte("id: x\n"), 0o644)
		}},
		{"ci", cronChange("15 7 * * *"), func(dir string) error {
			return os.WriteFile(filepath.Join(dir, "ci/check.py"), []byte("print('pass')\n"), 0o644)
		}},
		{"symlink", cronChange("15 7 * * *"), func(dir string) error {
			return os.Symlink("/etc/passwd", filepath.Join(dir, "tenants/demo/link.yaml"))
		}},
		{"field", Change{Proposal: "prp_1", Tenant: "demo", Ops: []Op{{Kind: "Schedule", Name: "nightly", Field: "spec.task.type", Value: "x.v1"}}}, nil},
		{"budget", Change{Proposal: "prp_1", Tenant: "demo", Ops: []Op{{Kind: "Tenant", Name: "demo", Field: "spec.budgets.monthly_eur", Value: 1000}}}, nil},
		{"kind", Change{Proposal: "prp_1", Tenant: "demo", Ops: []Op{{Kind: "Governance", Name: "demo", Field: "spec.cron", Value: "x"}}}, nil},
		{"target in evals", Change{Proposal: "prp_1", Tenant: "demo", Ops: []Op{{Kind: "Schedule", Name: "in-evals", Field: "spec.cron", Value: "0 2 * * *"}}}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rep, err := run(ctx, Config{Repo: repo}, tc.ch, tc.extra)
			if err != nil {
				t.Fatal(err)
			}
			if rep.Passed() || rep.Code != ForbiddenPathModified || stepOf(rep, "diff").Status != Failed || stepOf(rep, "lint").Status != NotRun {
				t.Fatalf("report %+v", rep)
			}
		})
	}
	noWorktrees(t, repo)
}

// More fields, lines or files than the limits is diff_size_exceeded; a field the editor cannot
// write in place is diff_unrenderable (EVO-2).
func TestGateDiffSize(t *testing.T) {
	repo := newRepo(t)
	ctx := context.Background()
	two := Change{Proposal: "prp_1", Tenant: "demo", Ops: []Op{
		{Kind: "Tenant", Name: "demo", Field: "spec.limits.max_queued", Value: 8},
		{Kind: "Tenant", Name: "demo", Field: "spec.limits.max_running", Value: 3}}}
	rep, err := Run(ctx, Config{Repo: repo}, two)
	if err != nil || rep.Code != DiffSizeExceeded || !strings.Contains(stepOf(rep, "diff").Detail, "2 fields") {
		t.Fatalf("two fields %+v %v", rep, err)
	}
	// Allowed by the tenant: two fields pass.
	if rep, err = Run(ctx, Config{Repo: repo, MaxFields: 2}, two); err != nil || !rep.Passed() || rep.DiffLines != 4 {
		t.Fatalf("two fields allowed %+v %v", rep, err)
	}
	if rep, err = Run(ctx, Config{Repo: repo, MaxDiffLines: 1}, cronChange("15 7 * * *")); err != nil || rep.Code != DiffSizeExceeded {
		t.Fatalf("lines %+v %v", rep, err)
	}
	rep, err = run(ctx, Config{Repo: repo}, cronChange("15 7 * * *"), func(dir string) error {
		return os.WriteFile(filepath.Join(dir, "roles.yaml"), []byte(rolesYAML+"# note\n"), 0o644)
	})
	if err != nil || rep.Code != DiffSizeExceeded || !strings.Contains(stepOf(rep, "diff").Detail, "2 files") {
		t.Fatalf("files %+v %v", rep, err)
	}
	// Another document of the same file changed: not the fields of the change.
	rep, err = run(ctx, Config{Repo: repo}, cronChange("15 7 * * *"), func(dir string) error {
		p := filepath.Join(dir, "tenants/demo/schedules.yaml")
		b, _ := os.ReadFile(p)
		return os.WriteFile(p, []byte(strings.Replace(string(b), `"0 * * * *"`, `"5 * * * *"`, 1)), 0o644)
	})
	if err != nil || rep.Code != DiffUnrenderable {
		t.Fatalf("other document %+v %v", rep, err)
	}
	// The same value as the file holds.
	if rep, err = Run(ctx, Config{Repo: repo}, cronChange("15 6 * * *")); err != nil || rep.Code != DiffUnrenderable {
		t.Fatalf("no change %+v %v", rep, err)
	}
	noWorktrees(t, repo)
}

// A change that leaves a reference near it unresolved is coupling_broken; a reference that was
// already missing on the base is not the change's (EVO-1, EVO-9).
func TestGateCoupling(t *testing.T) {
	repo := newRepo(t)
	ctx := context.Background()
	wide := func(kind, field string) bool { return Proposable(kind, field) || field == "spec.task.type" }
	ch := Change{Proposal: "prp_1", Tenant: "demo", Ops: []Op{{Kind: "Schedule", Name: "nightly", Field: "spec.task.type", Value: "data.gone.v1"}}}
	rep, err := Run(ctx, Config{Repo: repo, Proposable: wide}, ch)
	if err != nil || rep.Code != CouplingBroken || stepOf(rep, "coupling").Status != Failed || !strings.Contains(stepOf(rep, "coupling").Detail, "TaskType/data.gone.v1") {
		t.Fatalf("broken %+v %v", rep, err)
	}
	// A dangling reference of the base stays the base's.
	writeFile(t, repo, "tenants/demo/extra.yaml", "apiVersion: botaru/v1alpha1\nkind: Schedule\nmetadata: {name: old, tenant: demo}\nspec:\n  cron: \"0 3 * * *\"\n  task: {type: data.missing.v1}\n")
	gitT(t, repo, "add", "-A")
	gitT(t, repo, "commit", "-q", "-m", "dangling")
	ch = Change{Proposal: "prp_1", Tenant: "demo", Ops: []Op{{Kind: "Schedule", Name: "old", Field: "spec.cron", Value: "0 4 * * *"}}}
	if rep, err = Run(ctx, Config{Repo: repo}, ch); err != nil || !rep.Passed() {
		t.Fatalf("old dangling %+v %v", rep, err)
	}
}

// A candidate that holds the visible cases and loses hidden ones is stopped by the anti-gaming
// guard; the report carries the code and the boolean, never a hidden case (EVO-7, EVO-8).
func TestGateRewardHacking(t *testing.T) {
	repo := newRepo(t)
	ctx := context.Background()
	holdout := t.TempDir()
	var base string
	cfg := Config{Repo: repo, HoldoutDir: holdout,
		Suite: func(_ context.Context, root string, _ []string, h string) (evals.SuiteResult, error) {
			if base == "" {
				base = root
			}
			if h == "" {
				return passAll("v1"), nil
			}
			if root == base {
				return passAll("secret-case-1", "secret-case-2"), nil
			}
			r := passAll("secret-case-1", "secret-case-2")
			r["secret-case-2"] = evals.CaseOutcome{Case: "secret-case-2", Status: evals.Failed}
			return r, nil
		}}
	rep, err := Run(ctx, cfg, cronChange("15 7 * * *"))
	if err != nil {
		t.Fatal(err)
	}
	if rep.Passed() || rep.Code != evals.RewardHacking || rep.HoldoutPassed == nil || *rep.HoldoutPassed || stepOf(rep, "regression").Status != Passed {
		t.Fatalf("report %+v", rep)
	}
	raw, _ := json.Marshal(rep)
	if strings.Contains(string(raw), "secret-case") {
		t.Fatalf("a hidden case left the gate: %s", raw)
	}
	// A hidden folder inside the repository is refused.
	cfg.HoldoutDir = filepath.Join(repo, "evals")
	if _, err := Run(ctx, cfg, cronChange("15 7 * * *")); err == nil || !strings.Contains(err.Error(), "inside") {
		t.Fatalf("holdout inside: %v", err)
	}
	// A visible regression stops the gate before the hidden cases.
	cfg.HoldoutDir, base = holdout, ""
	cfg.Suite = func(_ context.Context, root string, _ []string, h string) (evals.SuiteResult, error) {
		if base == "" {
			base = root
		}
		if root == base {
			return passAll("v1", "v2"), nil
		}
		r := passAll("v1")
		r["v2"] = evals.CaseOutcome{Case: "v2", Status: evals.Failed}
		return r, nil
	}
	if rep, err = Run(ctx, cfg, cronChange("15 7 * * *")); err != nil || rep.Code != RegressionFailed || stepOf(rep, "holdout").Status != NotRun {
		t.Fatalf("regression %+v %v", rep, err)
	}
	noWorktrees(t, repo)
}

// The revert command returns the manifest to its hash before the change, as one commit with
// the Botaru-Revert line on the person's branch (EVO-10).
func TestRevert(t *testing.T) {
	repo := newRepo(t)
	ctx := context.Background()
	orig, err := manifest.ReadFiles([]string{filepath.Join(repo, "tenants/demo/tenant.yaml")})
	if err != nil {
		t.Fatal(err)
	}
	origHash, _ := jcs.Hash(orig[0].Value)
	origBytes, _ := os.ReadFile(filepath.Join(repo, "tenants/demo/tenant.yaml"))
	// The realization: the executor applies the patch of the accepted proposal.
	ch := Change{Proposal: "prp_7", Tenant: "demo", Ops: []Op{{Kind: "Tenant", Name: "demo", Field: "spec.limits.max_queued", Value: 7}}}
	patch, err := Patch(ctx, Config{Repo: repo}, ch, "Lower the queue limit")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(patch), "Botaru-Proposal: prp_7") {
		t.Fatalf("patch %s", patch)
	}
	pf := filepath.Join(t.TempDir(), "p.patch")
	if err := os.WriteFile(pf, patch, 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, repo, "am", "-q", pf)
	noWorktrees(t, repo)

	req := RevertRequest{Repo: repo, Tenant: "demo", Proposal: "prp_7", Reason: "harm_observed",
		Fields: []RevertField{{Kind: "Tenant", Name: "demo", Field: "spec.limits.max_queued", Accepted: 7, Previous: 10}}}
	bad := req
	bad.Reason = "because"
	if _, err := Revert(ctx, bad); err == nil {
		t.Fatal("a reason outside the list")
	}
	// A dirty checkout is refused.
	writeFile(t, repo, "roles.yaml", rolesYAML+"# local\n")
	if _, err := Revert(ctx, req); err == nil || !strings.Contains(err.Error(), "uncommitted") {
		t.Fatalf("dirty: %v", err)
	}
	gitT(t, repo, "checkout", "--", "roles.yaml")
	// A field edited since the change is refused.
	stale := req
	stale.Fields = []RevertField{{Kind: "Tenant", Name: "demo", Field: "spec.limits.max_queued", Accepted: 6, Previous: 10}}
	if _, err := Revert(ctx, stale); err == nil || !strings.Contains(err.Error(), "not the accepted") {
		t.Fatalf("stale: %v", err)
	}
	res, err := Revert(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Branch != "main" || gitT(t, repo, "rev-parse", "HEAD") != res.Commit {
		t.Fatalf("result %+v", res)
	}
	if msg := gitT(t, repo, "log", "-1", "--format=%B"); !strings.Contains(msg, "Botaru-Revert: prp_7 harm_observed") {
		t.Fatalf("message %s", msg)
	}
	after, err := manifest.ReadFiles([]string{filepath.Join(repo, "tenants/demo/tenant.yaml")})
	if err != nil {
		t.Fatal(err)
	}
	if h, _ := jcs.Hash(after[0].Value); h != origHash || res.TargetHashes["Tenant/demo"][1] != origHash {
		t.Fatalf("hash after the revert %s, before the change %s (%v)", h, origHash, res.TargetHashes)
	}
	if b, _ := os.ReadFile(filepath.Join(repo, "tenants/demo/tenant.yaml")); string(b) != string(origBytes) {
		t.Fatalf("file after the revert:\n%s", b)
	}
	noWorktrees(t, repo)
	// A second revert finds the previous value, not the accepted one.
	if _, err := Revert(ctx, req); err == nil {
		t.Fatal("a second revert")
	}
}

func TestEvalRunnerNoCases(t *testing.T) {
	r, err := EvalRunner{Bin: "/nonexistent"}.Suite()(context.Background(), t.TempDir(), nil, "")
	if err != nil || len(r) != 0 {
		t.Fatalf("%v %v", r, err)
	}
}

// A path git would quote (a non-ASCII name) is still checked, and the editor never writes
// through a symbolic link, inside the worktree or outside it (EVO-2).
func TestGateQuotedAndLinkedPaths(t *testing.T) {
	repo := newRepo(t)
	ctx := context.Background()
	if err := os.Rename(filepath.Join(repo, "evals/schedules.yaml"), filepath.Join(repo, "evals/schédules.yaml")); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.yaml")
	body := strings.ReplaceAll(strings.ReplaceAll(evalSchedule, "in-evals", "far"), "evals", "x")
	if err := os.WriteFile(outside, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(repo, "tenants/demo/far.yaml")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, repo, "shared/near.yaml", strings.ReplaceAll(body, "far", "near"))
	if err := os.Symlink("../../shared/near.yaml", filepath.Join(repo, "tenants/demo/near.yaml")); err != nil {
		t.Fatal(err)
	}
	gitT(t, repo, "add", "-A")
	gitT(t, repo, "commit", "-q", "-m", "paths")
	one := func(name string) Change {
		return Change{Proposal: "prp_1", Tenant: "demo", Ops: []Op{{Kind: "Schedule", Name: name, Field: "spec.cron", Value: "0 3 * * *"}}}
	}
	for _, tc := range []struct {
		name string
		cfg  Config
	}{
		{"in-evals", Config{Repo: repo}},
		{"far", Config{Repo: repo}},
		{"near", Config{Repo: repo, Sets: []string{"tenants", "roles.yaml"}}},
	} {
		rep, err := Run(ctx, tc.cfg, one(tc.name))
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if rep.Passed() || rep.Code != ForbiddenPathModified {
			t.Fatalf("%s: %+v", tc.name, rep)
		}
	}
	if b, _ := os.ReadFile(outside); string(b) != body {
		t.Fatalf("the file outside changed:\n%s", b)
	}
	noWorktrees(t, repo)
}

// A field the change added is taken out by the revert, with the map it created, so the file
// returns byte for byte (EVO-10).
func TestRevertAddedField(t *testing.T) {
	repo := newRepo(t)
	ctx := context.Background()
	origBytes, _ := os.ReadFile(filepath.Join(repo, "tenants/demo/tenant.yaml"))
	ch := Change{Proposal: "prp_8", Tenant: "demo", Ops: []Op{{Kind: "Tenant", Name: "demo", Field: "spec.campaigns.max_active", Value: 2}}}
	patch, err := Patch(ctx, Config{Repo: repo}, ch, "")
	if err != nil {
		t.Fatal(err)
	}
	pf := filepath.Join(t.TempDir(), "p.patch")
	if err := os.WriteFile(pf, patch, 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, repo, "am", "-q", pf)
	res, err := Revert(ctx, RevertRequest{Repo: repo, Tenant: "demo", Proposal: "prp_8", Reason: "other",
		Fields: []RevertField{{Kind: "Tenant", Name: "demo", Field: "spec.campaigns.max_active", Accepted: 2, Absent: true}}})
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(repo, "tenants/demo/tenant.yaml")); string(b) != string(origBytes) || res.Commit == "" {
		t.Fatalf("file after the revert:\n%s", b)
	}
	noWorktrees(t, repo)
}
