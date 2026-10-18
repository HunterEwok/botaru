// SPDX-License-Identifier: AGPL-3.0-only

// Package gate is the merge gate of a manifest change a bot proposed (EVO-2, EVO-9): code, not
// a model, checks the change in a temporary git worktree of the farm manifests before a person
// sees it, and writes the revert of a realized change in one command (EVO-10). The steps run in
// a fixed order and the first failure stops the gate:
//
//  1. diff: the fields of the change are written into the manifest file by a line editor; the
//     change may touch at most max_fields fields, max_diff_lines lines and one file, no symbolic
//     link, no path of the skeleton (ci/, spec/, conformance/, evals/, impl/, .github/) and no
//     protected kind or field; the file read back must differ by exactly those fields;
//  2. lint: the schema and set checks of the whole candidate set (an apply plan of the core);
//  3. coupling: no reference near a changed object is broken (manifest.Coupling) and the
//     changed objects have an order from callee to caller;
//  4. regression: the visible eval cases of the affected roles on the base and on the
//     candidate; a case that passed on the base must pass on the candidate;
//  5. holdout: the hidden cases on both sides and the anti-gaming guard (evals.CheckRewardHacking);
//     only the code leaves the gate, never a hidden case;
//  6. verdict: passed or failed with the code of the failed step.
//
// A step without what it needs (no lint, no suite, no hidden folder, no case rated on both
// sides) is not_run: recorded and shown, never counted as passed.
package gate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/HunterEwok/botaru/impl/internal/evals"
	"github.com/HunterEwok/botaru/impl/internal/ijson"
	"github.com/HunterEwok/botaru/impl/internal/jcs"
	"github.com/HunterEwok/botaru/impl/internal/manifest"
)

// The codes of the gate (spec/model/reasons.yaml); the holdout codes come from evals.
const (
	DiffSizeExceeded      = "diff_size_exceeded"
	ForbiddenPathModified = "forbidden_path_modified"
	DiffUnrenderable      = "diff_unrenderable"
	LintFailed            = "lint_failed"
	CouplingBroken        = "coupling_broken"
	RegressionFailed      = "regression_failed"
)

// The step statuses.
const (
	Passed = "passed"
	Failed = "failed"
	NotRun = "not_run"
)

// Steps are the step names in their order.
var Steps = []string{"diff", "lint", "coupling", "regression", "holdout"}

// Defaults of Tenant.spec.insights.gate.
const (
	DefaultMaxFields    = 1
	DefaultMaxDiffLines = 40
)

// ForbiddenPrefixes are the folders of the skeleton and of CI that a scaffold change never
// touches.
var ForbiddenPrefixes = []string{"ci/", "spec/", "conformance/", "evals/", "impl/", ".github/"}

// ProtectedKinds are the kinds whose manifests a scaffold change never touches: governance,
// the commission, incident classes, permissions, money and the ways out of the farm.
var ProtectedKinds = map[string]bool{"Governance": true, "Commission": true, "IncidentClass": true, "Group": true,
	"ModelProfile": true, "Sink": true, "Channel": true, "EvalReports": true}

// The proposable fields of the kinds a manifest change may target, as the core checks them
// (spec/model/proposable-fields.yaml, PRP-3); any other field is protected.
var (
	scheduleFields = map[string]bool{"spec.cron": true, "spec.timezone": true}
	tenantFields   = regexp.MustCompile(`^spec\.((limits|campaigns|scripts)\.[a-z_]+|dispatch\.rules\[[0-9]*\]\.(timeouts\.attempt|retry\.max_attempts|max_running))$`)
)

// Proposable says whether a manifest change may set the field of an object of the kind.
func Proposable(kind, field string) bool {
	switch kind {
	case "Schedule":
		return scheduleFields[field]
	case "Tenant":
		return tenantFields.MatchString(field)
	}
	return false
}

// Op is one set operation of a manifest change.
type Op struct {
	Kind  string `json:"kind"`
	Name  string `json:"name"`
	Field string `json:"field"`
	Value any    `json:"value"`
	// Remove takes the key out instead of setting it: the revert of a field the change added.
	Remove bool `json:"remove,omitempty"`
}

// Change is the manifest change of one proposal of a tenant.
type Change struct {
	Proposal string
	Tenant   string
	Ops      []Op
}

// Lint checks a candidate set: the folder of the worktree and the set paths in it. It returns
// the problems, or an error when it could not check.
type Lint func(ctx context.Context, dir string, sets []string) ([]string, error)

// Suite runs eval cases with the folder as the skeleton root: the visible cases of the roles
// (all roles when nil), or the hidden cases of holdoutDir when it is set.
type Suite func(ctx context.Context, root string, roles []string, holdoutDir string) (evals.SuiteResult, error)

// Config is one run of the gate.
type Config struct {
	Repo         string   // the git checkout of the farm manifests
	Base         string   // the base commit; HEAD when empty
	Sets         []string // the set paths relative to Repo; "." when empty
	MaxFields    int      // DefaultMaxFields when 0
	MaxDiffLines int      // DefaultMaxDiffLines when 0
	Lint         Lint
	Suite        Suite
	HoldoutDir   string
	// Proposable is the field allowlist; the package's Proposable when nil.
	Proposable func(kind, field string) bool
}

// Step is the outcome of one step. Detail never names a hidden case.
type Step struct {
	Name   string `json:"step"`
	Status string `json:"status"`
	Code   string `json:"code,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// Report is the outcome of a gate run.
type Report struct {
	Proposal  string   `json:"proposal"`
	BaseSHA   string   `json:"base_sha"`
	Verdict   string   `json:"verdict"`
	Code      string   `json:"code,omitempty"`
	Steps     []Step   `json:"steps"`
	Files     []string `json:"files"`
	Fields    int      `json:"fields"`
	DiffLines int      `json:"diff_lines"`
	// Order is the changed objects from callee to caller; Affected the roles the suite ran for
	// (empty: all).
	Order    []string `json:"order,omitempty"`
	Affected []string `json:"affected_roles,omitempty"`
	// The pass rates of the visible cases, -1 without a rated case.
	VisibleBase      float64 `json:"visible_pass_rate_base"`
	VisibleCandidate float64 `json:"visible_pass_rate_candidate"`
	// HoldoutPassed is nil when the hidden cases did not run.
	HoldoutPassed *bool `json:"holdout_passed"`
	// TargetHashes are the JCS hashes of the target objects before and after the change.
	TargetHashes map[string][2]string `json:"target_hashes"`
	Diff         string               `json:"-"`
}

// Passed says whether no step failed.
func (r *Report) Passed() bool { return r.Verdict == Passed }

func (r *Report) step(name, status, code, detail string) {
	r.Steps = append(r.Steps, Step{Name: name, Status: status, Code: code, Detail: detail})
	if status == Failed {
		r.Verdict, r.Code = Failed, code
		// The steps after a failure do not run.
		for _, s := range Steps[slices.Index(Steps, name)+1:] {
			r.Steps = append(r.Steps, Step{Name: s, Status: NotRun})
		}
	}
}

// Run runs the gate over a change.
func Run(ctx context.Context, cfg Config, ch Change) (*Report, error) {
	return run(ctx, cfg, ch, nil)
}

// run is Run with an extra edit of the candidate worktree after the change, for the tests of
// the guards.
func run(ctx context.Context, cfg Config, ch Change, extra func(dir string) error) (*Report, error) {
	cfg = withDefaults(cfg)
	base, err := resolve(ctx, cfg.Repo, cfg.Base)
	if err != nil {
		return nil, err
	}
	rep := &Report{Proposal: ch.Proposal, BaseSHA: base, Verdict: Passed, Files: []string{}, VisibleBase: -1, VisibleCandidate: -1,
		TargetHashes: map[string][2]string{}}
	cand, err := newWorktree(ctx, cfg.Repo, base, "candidate")
	if err != nil {
		return nil, err
	}
	defer cand.close()

	// 1. diff
	c, failure, err := prepare(ctx, cfg, cand.dir, ch, extra)
	if err != nil {
		return nil, err
	}
	if c != nil {
		rep.Files, rep.Fields, rep.DiffLines, rep.Diff, rep.TargetHashes = c.files, c.fields, c.lines, c.diff, c.hashes
	}
	if failure != nil {
		rep.step("diff", Failed, failure.code, failure.detail)
		return rep, nil
	}
	rep.step("diff", Passed, "", fmt.Sprintf("%d field(s), %d line(s), %s", c.fields, c.lines, strings.Join(c.files, ", ")))

	// 2. lint
	if cfg.Lint == nil {
		rep.step("lint", NotRun, "", "no lint configured")
	} else {
		problems, err := cfg.Lint(ctx, cand.dir, cfg.Sets)
		if err != nil {
			return nil, fmt.Errorf("lint: %w", err)
		}
		if len(problems) > 0 {
			rep.step("lint", Failed, LintFailed, strings.Join(first(problems, 5), "; "))
			return rep, nil
		}
		rep.step("lint", Passed, "", "")
	}

	// 3. coupling
	changed := c.nodes
	before, after := manifest.Coupling(c.before), manifest.Coupling(c.after)
	old := map[manifest.Missing]bool{}
	for _, m := range before.Missing() {
		old[m] = true
	}
	var broken []string
	for _, m := range after.MissingNear(changed) {
		if !old[m] {
			broken = append(broken, m.Caller.String()+" -> "+m.Callee.String())
		}
	}
	if len(broken) > 0 {
		rep.step("coupling", Failed, CouplingBroken, "unresolved: "+strings.Join(first(broken, 5), ", "))
		return rep, nil
	}
	order, err := after.TopoOrder(changed)
	if err != nil {
		rep.step("coupling", Failed, CouplingBroken, err.Error())
		return rep, nil
	}
	for _, n := range order {
		rep.Order = append(rep.Order, n.String())
	}
	rep.step("coupling", Passed, "", "")

	// 4. regression and 5. holdout run the same suite on a worktree of the base.
	roles, all := affectedRoles(after, changed)
	rep.Affected = roles
	if cfg.Suite == nil {
		rep.step("regression", NotRun, "", "no eval suite configured")
		rep.step("holdout", NotRun, "", "no eval suite configured")
		return rep, nil
	}
	if !all && len(roles) == 0 {
		rep.step("regression", NotRun, "", "no role reads the changed objects")
		rep.step("holdout", NotRun, "", "no role reads the changed objects")
		return rep, nil
	}
	baseWT, err := newWorktree(ctx, cfg.Repo, base, "base")
	if err != nil {
		return nil, err
	}
	defer baseWT.close()
	suite := func(root, holdout string) (evals.SuiteResult, error) {
		return cfg.Suite(ctx, root, roles, holdout)
	}
	vb, err := suite(baseWT.dir, "")
	if err != nil {
		return nil, fmt.Errorf("regression suite on the base: %w", err)
	}
	vc, err := suite(cand.dir, "")
	if err != nil {
		return nil, fmt.Errorf("regression suite on the candidate: %w", err)
	}
	visible := evals.Comparison{Base: vb, Candidate: vc}
	sb, sc := visible.Sides()
	rep.VisibleBase, rep.VisibleCandidate = sb.PassRate(), sc.PassRate()
	switch {
	case sb.Cases == 0:
		rep.step("regression", NotRun, "", "no visible case rated on both sides")
	case len(visible.Regressions()) > 0:
		rep.step("regression", Failed, RegressionFailed, "passed on the base, not on the candidate: "+strings.Join(first(visible.Regressions(), 10), ", "))
		return rep, nil
	default:
		rep.step("regression", Passed, "", fmt.Sprintf("%d case(s)", sb.Cases))
	}

	if cfg.HoldoutDir == "" {
		rep.step("holdout", NotRun, "", "no hidden case folder")
		return rep, nil
	}
	if err := outside(cfg.HoldoutDir, cfg.Repo, cand.tmp, baseWT.tmp); err != nil {
		return nil, err
	}
	hb, err := suite(baseWT.dir, cfg.HoldoutDir)
	if err != nil {
		return nil, fmt.Errorf("hidden cases on the base: %w", err)
	}
	hc, err := suite(cand.dir, cfg.HoldoutDir)
	if err != nil {
		return nil, fmt.Errorf("hidden cases on the candidate: %w", err)
	}
	v := evals.CheckRewardHacking(visible, evals.Comparison{Base: hb, Candidate: hc})
	if v.NotRun {
		rep.step("holdout", NotRun, "", "no hidden case rated on both sides")
		return rep, nil
	}
	ok := v.Code == ""
	rep.HoldoutPassed = &ok
	if !ok {
		// Only the code: the hidden cases stay hidden (EVO-7).
		rep.step("holdout", Failed, v.Code, "")
		return rep, nil
	}
	rep.step("holdout", Passed, "", "")
	return rep, nil
}

func withDefaults(cfg Config) Config {
	if len(cfg.Sets) == 0 {
		cfg.Sets = []string{"."}
	}
	if cfg.MaxFields <= 0 {
		cfg.MaxFields = DefaultMaxFields
	}
	if cfg.MaxDiffLines <= 0 {
		cfg.MaxDiffLines = DefaultMaxDiffLines
	}
	if cfg.Proposable == nil {
		cfg.Proposable = Proposable
	}
	return cfg
}

// affectedRoles are the roles a change reaches; a change of the tenant or of a dispatcher rule
// reaches every role (all is true, roles nil).
func affectedRoles(g *manifest.Graph, changed []manifest.Node) ([]string, bool) {
	for _, n := range changed {
		if n.Kind == "Tenant" || n.Kind == "DispatchRule" {
			return nil, true
		}
	}
	return g.Affected(changed), false
}

// outside refuses a hidden case folder inside the repository or a worktree: a candidate could
// read it there.
func outside(dir string, roots ...string) error {
	d, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return fmt.Errorf("hidden case folder: %w", err)
	}
	for _, r := range roots {
		rr, err := filepath.EvalSymlinks(r)
		if err != nil {
			continue
		}
		if rel, err := filepath.Rel(rr, d); err == nil && rel != ".." && !strings.HasPrefix(rel, "../") {
			return fmt.Errorf("the hidden case folder %s is inside %s", dir, r)
		}
	}
	return nil
}

func first(list []string, n int) []string {
	if len(list) > n {
		return append(slices.Clone(list[:n]), fmt.Sprintf("and %d more", len(list)-n))
	}
	return list
}

// candidate is a change written into a worktree and staged.
type candidate struct {
	before, after []manifest.Doc
	nodes         []manifest.Node
	files         []string
	fields, lines int
	diff          string
	hashes        map[string][2]string
}

type refusal struct{ code, detail string }

// target is the document of a set that holds one object.
type target struct {
	file string // relative to the worktree
	doc  int    // 0-based document number of the stream
	d    manifest.Doc
}

// prepare writes the change into the worktree and runs the checks of step 1. A refusal is the
// gate's answer; an error means the gate could not run.
func prepare(ctx context.Context, cfg Config, dir string, ch Change, extra func(dir string) error) (*candidate, *refusal, error) {
	if len(ch.Ops) == 0 {
		return nil, nil, errors.New("the change has no operation")
	}
	fields := map[string]bool{}
	for _, op := range ch.Ops {
		if !cfg.Proposable(op.Kind, op.Field) {
			return nil, &refusal{ForbiddenPathModified, fmt.Sprintf("%s %s: %s is not a proposable field", op.Kind, op.Name, op.Field)}, nil
		}
		fields[op.Kind+"/"+op.Name+"/"+op.Field] = true
	}
	before, err := readSets(dir, cfg.Sets)
	if err != nil {
		return nil, nil, err
	}
	targets := map[string]target{}
	for _, op := range ch.Ops {
		key := op.Kind + "/" + op.Name
		if _, ok := targets[key]; ok {
			continue
		}
		t, err := locate(dir, before, ch.Tenant, op.Kind, op.Name)
		if err != nil {
			return nil, nil, err
		}
		targets[key] = t
	}
	if err := write(dir, targets, ch.Ops); err != nil {
		if errors.Is(err, errThroughLink) {
			return nil, &refusal{ForbiddenPathModified, err.Error()}, nil
		}
		if errors.Is(err, errUnrenderable) {
			return nil, &refusal{DiffUnrenderable, err.Error()}, nil
		}
		return nil, nil, err
	}
	if extra != nil {
		if err := extra(dir); err != nil {
			return nil, nil, err
		}
	}
	c, r, err := check(ctx, cfg, dir, before, targets, ch, len(fields))
	if err != nil || r != nil {
		return c, r, err
	}
	return c, nil, nil
}

// errThroughLink refuses a target file that is or lies under a symbolic link: the editor never
// writes through a link, so nothing outside the worktree changes (EVO-2).
var errThroughLink = errors.New("the file is or lies under a symbolic link")

// write applies the operations to the target files with the line editor; an operation with
// Remove takes its key out.
func write(dir string, targets map[string]target, ops []Op) error {
	for _, op := range ops {
		t := targets[op.Kind+"/"+op.Name]
		if throughLink(dir, t.file) {
			return fmt.Errorf("%s: %w", t.file, errThroughLink)
		}
		p := filepath.Join(dir, t.file)
		src, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		var out []byte
		if op.Remove {
			out, err = removeField(src, t.doc, op.Field)
		} else {
			out, err = setField(src, t.doc, op.Field, op.Value)
		}
		if err != nil {
			return fmt.Errorf("%s %s %s: %w", op.Kind, op.Name, op.Field, err)
		}
		if err := os.WriteFile(p, out, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// check stages the worktree and runs the checks of step 1 over what changed.
func check(ctx context.Context, cfg Config, dir string, before []manifest.Doc, targets map[string]target, ch Change, fields int) (*candidate, *refusal, error) {
	changes, err := stagedChanges(ctx, dir)
	if err != nil {
		return nil, nil, err
	}
	c := &candidate{before: before, fields: fields, hashes: map[string][2]string{}}
	for _, f := range changes {
		c.files = append(c.files, f.Path)
		c.lines += f.Added + f.Deleted
	}
	c.diff, err = git(ctx, dir, "diff", "--cached", "--no-renames")
	if err != nil {
		return nil, nil, err
	}
	// The paths first: a change of the skeleton is refused whatever its size.
	for _, f := range changes {
		for _, p := range ForbiddenPrefixes {
			if strings.HasPrefix(f.Path, p) {
				return c, &refusal{ForbiddenPathModified, f.Path}, nil
			}
		}
		if f.Symlink || throughLink(dir, f.Path) {
			return c, &refusal{ForbiddenPathModified, f.Path + " is or lies under a symbolic link"}, nil
		}
	}
	switch {
	case fields > cfg.MaxFields:
		return c, &refusal{DiffSizeExceeded, fmt.Sprintf("%d fields, at most %d", fields, cfg.MaxFields)}, nil
	case len(changes) == 0:
		return c, &refusal{DiffUnrenderable, "the change sets the values the files already hold"}, nil
	case len(changes) > 1:
		return c, &refusal{DiffSizeExceeded, fmt.Sprintf("%d files, at most one", len(changes))}, nil
	case changes[0].Binary:
		return c, &refusal{DiffSizeExceeded, "a binary change"}, nil
	case c.lines > cfg.MaxDiffLines:
		return c, &refusal{DiffSizeExceeded, fmt.Sprintf("%d lines, at most %d", c.lines, cfg.MaxDiffLines)}, nil
	}
	after, err := readSets(dir, cfg.Sets)
	if err != nil {
		return c, &refusal{DiffUnrenderable, err.Error()}, nil
	}
	c.after = after
	if r := semantic(dir, before, after, targets, ch.Ops, c.hashes); r != nil {
		return c, r, nil
	}
	for _, op := range ch.Ops {
		c.nodes = append(c.nodes, manifest.FieldNode(op.Kind, ch.Tenant, op.Name, op.Field))
	}
	slices.SortFunc(c.nodes, func(a, b manifest.Node) int { return strings.Compare(a.String(), b.String()) })
	c.nodes = slices.Compact(c.nodes)
	return c, nil, nil
}

// semantic compares the sets before and after: only the target objects may differ, each by
// exactly the operations; a changed protected kind is a forbidden change.
func semantic(dir string, before, after []manifest.Doc, targets map[string]target, ops []Op, hashes map[string][2]string) *refusal {
	want := map[string]map[string]any{}
	for key, t := range targets {
		want[key] = cloneDoc(t.d.Value)
	}
	for _, op := range ops {
		if op.Remove {
			if !removePath(want[op.Kind+"/"+op.Name], strings.Split(op.Field, ".")) {
				return &refusal{DiffUnrenderable, op.Field + " is not in " + op.Kind + " " + op.Name}
			}
			continue
		}
		if !setPath(want[op.Kind+"/"+op.Name], op.Field, op.Value) {
			return &refusal{DiffUnrenderable, op.Field + " has no place in " + op.Kind + " " + op.Name}
		}
	}
	bySource := func(docs []manifest.Doc) map[string]manifest.Doc {
		m := map[string]manifest.Doc{}
		for _, d := range docs {
			m[relSource(dir, d.Source)] = d
		}
		return m
	}
	b, a := bySource(before), bySource(after)
	var sources []string
	for _, m := range []map[string]manifest.Doc{b, a} {
		for s := range m {
			if !slices.Contains(sources, s) {
				sources = append(sources, s)
			}
		}
	}
	sort.Strings(sources)
	for _, s := range sources {
		old, okOld := b[s]
		cur, okNew := a[s]
		if okOld && okNew && equal(old.Value, cur.Value) {
			continue
		}
		for _, d := range []manifest.Doc{old, cur} {
			if ProtectedKinds[d.Kind] {
				return &refusal{ForbiddenPathModified, "a " + d.Kind + " manifest changed"}
			}
		}
		key := old.Kind + "/" + old.Name
		t, isTarget := targets[key]
		if !okOld || !okNew || !isTarget || relSource(dir, t.d.Source) != s || cur.Kind != old.Kind || cur.Name != old.Name {
			return &refusal{DiffUnrenderable, s + " changed beyond the fields of the change"}
		}
		if !equal(cur.Value, want[key]) {
			return &refusal{DiffUnrenderable, s + " does not read back as the fields of the change"}
		}
		h0, _ := jcs.Hash(old.Value)
		h1, _ := jcs.Hash(cur.Value)
		hashes[key] = [2]string{h0, h1}
	}
	return nil
}

func equal(a, b any) bool {
	x, err1 := jcs.Canonical(a)
	y, err2 := jcs.Canonical(b)
	return err1 == nil && err2 == nil && string(x) == string(y)
}

func readSets(dir string, sets []string) ([]manifest.Doc, error) {
	paths := make([]string, len(sets))
	for i, s := range sets {
		if filepath.IsAbs(s) || strings.HasPrefix(filepath.Clean(s), "..") {
			return nil, fmt.Errorf("the set path %s must lie inside the repository", s)
		}
		paths[i] = filepath.Join(dir, s)
	}
	return manifest.ReadFiles(paths)
}

// relSource turns a document source "<file>#<n>" into "<file relative to dir>#<n>".
func relSource(dir, source string) string {
	file, n, _ := strings.Cut(source, "#")
	if rel, err := filepath.Rel(dir, file); err == nil {
		file = filepath.ToSlash(rel)
	}
	return file + "#" + n
}

// locate finds the one document of an object of the tenant.
func locate(dir string, docs []manifest.Doc, tenant, kind, name string) (target, error) {
	var found []manifest.Doc
	for _, d := range docs {
		if d.Kind == kind && d.Name == name && (kind == "Tenant" || d.Tenant == tenant) {
			found = append(found, d)
		}
	}
	if len(found) != 1 {
		return target{}, fmt.Errorf("%s %s of tenant %s: %d manifests in the set, want one", kind, name, tenant, len(found))
	}
	d := found[0]
	file, n, _ := strings.Cut(d.Source, "#")
	k, err := strconv.Atoi(n)
	if err != nil {
		return target{}, fmt.Errorf("source %s", d.Source)
	}
	rel, err := filepath.Rel(dir, file)
	if err != nil {
		return target{}, err
	}
	return target{file: filepath.ToSlash(rel), doc: k - 1, d: d}, nil
}

func cloneDoc(m map[string]any) map[string]any {
	b, _ := json.Marshal(m)
	v, _ := ijson.ParseObject(b)
	return v
}

// setPath sets a field of a manifest value: a part name[] is the only element of a list,
// name[i] the element i; missing maps on the way are created.
func setPath(doc map[string]any, path string, v any) bool {
	if doc == nil {
		return false
	}
	var cur any = doc
	parts := strings.Split(path, ".")
	for i, part := range parts {
		name, idx, err := splitPart(part)
		if err != nil {
			return false
		}
		m, ok := cur.(map[string]any)
		if !ok {
			return false
		}
		last := i == len(parts)-1
		if idx == noIndex {
			if last {
				m[name] = v
				return true
			}
			next, ok := m[name]
			if !ok {
				next = map[string]any{}
				m[name] = next
			}
			cur = next
			continue
		}
		list, ok := m[name].([]any)
		if idx == onlyElement {
			if len(list) != 1 {
				return false
			}
			idx = 0
		}
		if !ok || idx >= len(list) || last {
			return false
		}
		cur = list[idx]
	}
	return false
}

// removePath takes a field out of a manifest value, and with it every map on the way that it
// leaves empty, as setPath created them.
func removePath(doc map[string]any, parts []string) bool {
	if doc == nil || len(parts) == 0 {
		return false
	}
	name, idx, err := splitPart(parts[0])
	if err != nil {
		return false
	}
	next, ok := doc[name]
	if !ok {
		return false
	}
	if len(parts) == 1 {
		if idx != noIndex {
			return false
		}
		delete(doc, name)
		return true
	}
	if idx != noIndex {
		list, ok := next.([]any)
		if idx == onlyElement {
			if len(list) != 1 {
				return false
			}
			idx = 0
		}
		if !ok || idx >= len(list) {
			return false
		}
		m, ok := list[idx].(map[string]any)
		return ok && removePath(m, parts[1:])
	}
	m, ok := next.(map[string]any)
	if !ok || !removePath(m, parts[1:]) {
		return false
	}
	if len(m) == 0 {
		delete(doc, name)
	}
	return true
}

// pathValue reads a field of a manifest value with the parts of setPath.
func pathValue(doc map[string]any, path string) (any, bool) {
	var cur any = doc
	for _, part := range strings.Split(path, ".") {
		name, idx, err := splitPart(part)
		if err != nil {
			return nil, false
		}
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		if cur, ok = m[name]; !ok {
			return nil, false
		}
		if idx == noIndex {
			continue
		}
		list, ok := cur.([]any)
		if idx == onlyElement {
			if len(list) != 1 {
				return nil, false
			}
			idx = 0
		}
		if !ok || idx >= len(list) {
			return nil, false
		}
		cur = list[idx]
	}
	return cur, true
}

// Sorted keys of target hashes, for printing.
func (r *Report) TargetKeys() []string {
	out := make([]string, 0, len(r.TargetHashes))
	for k := range r.TargetHashes {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
