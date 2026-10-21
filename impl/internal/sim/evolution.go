// SPDX-License-Identifier: AGPL-3.0-only

package sim

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/HunterEwok/botaru/impl/internal/evals"
	"github.com/HunterEwok/botaru/impl/internal/gate"
	"github.com/HunterEwok/botaru/impl/internal/manifest"
)

// The commands of the safe scaffold evolution on the stand (D-120, EVO-1..EVO-10), as an
// operator runs them with farmctl: the merge gate of a manifest change, the patch of an
// accepted change applied from the farm git, the revert in one step and the reads of the
// variant archive and the review rules. The person is bob (group deciders), who holds
// proposal.gate and proposal.decide of the category reliability.

// Decider is the person of the seed who runs the gate and reads the archive.
const Decider = "bob"

// applyRepo applies the head of the farm git with its message.
func (s *Stand) applyRepo(ctx context.Context) error {
	s.repo.mu.Lock()
	commit, msg, err := s.repo.head(ctx)
	var set []map[string]any
	if err == nil {
		set, err = s.repo.readSet()
	}
	s.repo.mu.Unlock()
	if err != nil {
		return err
	}
	if err := s.Bench.ApplySet(ctx, set, commit, msg); err != nil {
		return err
	}
	s.log.Add(KindGit, fmt.Sprintf("applied %s: %s", commit[:12], firstLine(msg)), map[string]any{"commit": commit, "message": msg})
	return nil
}

// applyOrDrop applies the head commit of the farm git and drops that commit when the core
// refuses it, so a later apply does not carry the change without its Botaru line. After an
// error of the transport the core may have applied it, so the commit stays.
func (s *Stand) applyOrDrop(ctx context.Context, commit string) error {
	err := s.applyRepo(ctx)
	var refused *evals.RefusedError
	if err == nil || !errors.As(err, &refused) {
		return err
	}
	s.repo.mu.Lock()
	defer s.repo.mu.Unlock()
	if head, _, herr := s.repo.head(ctx); herr != nil || head != commit {
		return err
	}
	if _, rerr := s.repo.git(context.WithoutCancel(ctx), "reset", "-q", "--hard", commit+"~1"); rerr != nil {
		return errors.Join(err, rerr)
	}
	s.log.Addf(KindGit, "dropped %s: the core refused it", commit[:12])
	return err
}

func firstLine(s string) string {
	l, _, _ := strings.Cut(s, "\n")
	return l
}

// Repo is the farm git of the stand.
func (s *Stand) Repo() string { return s.repo.dir }

// Holdout is the folder of the hidden cases of the stand's gate.
func (s *Stand) Holdout() string { return s.holdout }

// planLint is the lint step of the gate: the apply plan of the candidate set, as farmctl runs it.
func (s *Stand) planLint(ctx context.Context, dir string, sets []string) ([]string, error) {
	var paths []string
	for _, p := range sets {
		paths = append(paths, filepath.Join(dir, p))
	}
	docs, err := manifest.ReadFiles(paths)
	if err != nil {
		return []string{err.Error()}, nil
	}
	ms := make([]map[string]any, len(docs))
	for i, d := range docs {
		ms[i] = d.Value
	}
	commit, err := (&farmRepo{dir: dir}).git(ctx, "rev-parse", "HEAD")
	if err != nil {
		return nil, err
	}
	return s.Bench.PlanSet(ctx, ms, commit)
}

// gateRead is what P2 proposal.gate returns without a result.
type gateRead struct {
	Tenant  string           `json:"tenant"`
	State   string           `json:"state"`
	Ops     []map[string]any `json:"ops"`
	Applied *string          `json:"applied_commit"`
	Limits  struct {
		MaxFields    int `json:"max_fields"`
		MaxDiffLines int `json:"max_diff_lines"`
	} `json:"limits"`
}

func opsChange(id, tenant string, ops []map[string]any) gate.Change {
	ch := gate.Change{Proposal: id, Tenant: tenant}
	for _, op := range ops {
		tg := mapOf(op["target"])
		k, _ := tg["kind"].(string)
		n, _ := tg["name"].(string)
		f, _ := op["field"].(string)
		ch.Ops = append(ch.Ops, gate.Op{Kind: k, Name: n, Field: f, Value: op["value"]})
	}
	return ch
}

// GateProposal runs the merge gate of a manifest change on the farm git at the applied commit,
// with the stand's lint, suite and hidden cases, and records the verdict through P2
// proposal.gate as the decider (EVO-9). It returns the report and the proposal state after.
func (s *Stand) GateProposal(ctx context.Context, id string) (*gate.Report, string, error) {
	var read gateRead
	if err := s.Bench.P2As(ctx, Decider, "proposal.gate", map[string]any{"proposal": id}, &read); err != nil {
		return nil, "", err
	}
	if read.Applied == nil {
		return nil, "", errors.New("the core has applied no commit yet")
	}
	cfg := gate.Config{Repo: s.repo.dir, Base: *read.Applied, Sets: []string{farmSet}, MaxFields: read.Limits.MaxFields,
		MaxDiffLines: read.Limits.MaxDiffLines, Lint: s.planLint, Suite: standSuite(), HoldoutDir: s.holdout}
	s.repo.mu.Lock()
	rep, err := gate.Run(ctx, cfg, opsChange(id, read.Tenant, read.Ops))
	s.repo.mu.Unlock()
	if err != nil {
		return nil, "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "merge gate of %s at %s: %s", id, rep.BaseSHA[:12], rep.Verdict)
	if rep.Code != "" {
		fmt.Fprintf(&b, " (%s)", rep.Code)
	}
	for _, st := range rep.Steps {
		fmt.Fprintf(&b, "\n  %-10s %s", st.Name, st.Status)
		if st.Code != "" {
			b.WriteString(" " + st.Code)
		}
		if st.Detail != "" {
			b.WriteString(": " + st.Detail)
		}
	}
	s.log.Add(KindGit, b.String(), map[string]any{"proposal": id, "verdict": rep.Verdict, "code": rep.Code, "base_sha": rep.BaseSHA})
	res := map[string]any{"verdict": rep.Verdict, "code": rep.Code, "base_sha": rep.BaseSHA, "holdout_passed": rep.HoldoutPassed,
		"regression_pass_rate": nil, "fields": rep.Fields, "diff_lines": rep.DiffLines}
	if rep.VisibleCandidate >= 0 {
		res["regression_pass_rate"] = rep.VisibleCandidate
	}
	var steps []map[string]any
	for _, st := range rep.Steps {
		steps = append(steps, map[string]any{"step": st.Name, "status": st.Status, "code": st.Code})
	}
	res["steps"] = steps
	var out struct {
		State string `json:"state"`
	}
	if err := s.Bench.P2As(ctx, Decider, "proposal.gate", map[string]any{"proposal": id, "result": res}, &out); err != nil {
		return rep, "", err
	}
	return rep, out.State, nil
}

// proposalRecord reads a proposal through P2 proposal.show as the decider.
func (s *Stand) proposalRecord(ctx context.Context, id string) (map[string]any, error) {
	var rec map[string]any
	err := s.Bench.P2As(ctx, Decider, "proposal.show", map[string]any{"proposal": id}, &rec)
	return rec, err
}

// recordChange is the manifest change of a proposal record.
func recordChange(id string, rec map[string]any) (gate.Change, error) {
	mc := mapOf(mapOf(mapOf(rec["body"])["change"])["manifest_change"])
	var ops []map[string]any
	for _, o := range asSlice(mc["ops"]) {
		ops = append(ops, mapOf(o))
	}
	if len(ops) == 0 {
		return gate.Change{}, fmt.Errorf("%s is not a manifest change", id)
	}
	tenant, _ := rec["tenant"].(string)
	return opsChange(id, tenant, ops), nil
}

func asSlice(v any) []any {
	l, _ := v.([]any)
	return l
}

// Realize applies an accepted manifest change as its executor does (PRP-19): the patch with the
// line Botaru-Proposal is committed to the farm git with git am, and the set is applied, which
// moves the proposal to realized. It returns the commit.
func (s *Stand) Realize(ctx context.Context, id string) (string, error) {
	rec, err := s.proposalRecord(ctx, id)
	if err != nil {
		return "", err
	}
	if rec["state"] != "realizing" {
		return "", fmt.Errorf("%s is %v; an accepted change (realizing) is applied", id, rec["state"])
	}
	ch, err := recordChange(id, rec)
	if err != nil {
		return "", err
	}
	title := "Apply proposal " + id
	if code, _ := mapOf(mapOf(rec["body"])["problem"])["code"].(string); code != "" {
		title += ": " + code
	}
	s.repo.mu.Lock()
	commit, err := func() (string, error) {
		patch, err := gate.Patch(ctx, gate.Config{Repo: s.repo.dir, Sets: []string{farmSet}, MaxFields: 10, MaxDiffLines: 400}, ch, title)
		if err != nil {
			return "", err
		}
		f, err := os.CreateTemp("", "botaru-sim-patch-*.patch")
		if err != nil {
			return "", err
		}
		defer os.Remove(f.Name())
		if _, err := f.Write(patch); err != nil {
			f.Close()
			return "", err
		}
		f.Close()
		if _, err := s.repo.git(ctx, "am", "-q", f.Name()); err != nil {
			_, _ = s.repo.git(context.WithoutCancel(ctx), "am", "--abort")
			return "", err
		}
		return s.repo.git(ctx, "rev-parse", "HEAD")
	}()
	s.repo.mu.Unlock()
	if err != nil {
		return "", err
	}
	s.log.Addf(KindGit, "committed the accepted change of %s as %s (Botaru-Proposal)", id, commit[:12])
	return commit, s.applyOrDrop(ctx, commit)
}

// RevertProposal reverts a realized manifest change in one step (EVO-10, PRP-20): the reverse
// edit as a commit with the line Botaru-Revert on the farm git, then the apply, which moves the
// proposal to reverted, ends its trial and records the variant outcome and the review rule.
func (s *Stand) RevertProposal(ctx context.Context, id, reason string) (*gate.RevertResult, error) {
	rec, err := s.proposalRecord(ctx, id)
	if err != nil {
		return nil, err
	}
	if rec["state"] != "realized" {
		return nil, fmt.Errorf("%s is %v; only a realized change is reverted", id, rec["state"])
	}
	ch, err := recordChange(id, rec)
	if err != nil {
		return nil, err
	}
	real := mapOf(rec["realization"])
	patch := asSlice(real["revert_patch"])
	for _, e := range asSlice(real["revert_absent"]) {
		patch = append(patch, map[string]any{"target": mapOf(e)["target"], "field": mapOf(e)["field"], "absent": true})
	}
	if len(patch) == 0 {
		return nil, fmt.Errorf("%s has no reverse edit", id)
	}
	accepted := map[string]any{}
	for _, op := range ch.Ops {
		accepted[op.Kind+"/"+op.Name+"/"+op.Field] = op.Value
	}
	req := gate.RevertRequest{Repo: s.repo.dir, Sets: []string{farmSet}, Tenant: ch.Tenant, Proposal: id, Reason: reason}
	for _, e := range patch {
		em := mapOf(e)
		tg := mapOf(em["target"])
		f := gate.RevertField{Field: fmt.Sprint(em["field"]), Previous: em["previous"], Absent: em["absent"] == true}
		f.Kind, _ = tg["kind"].(string)
		f.Name, _ = tg["name"].(string)
		f.Accepted = accepted[f.Kind+"/"+f.Name+"/"+f.Field]
		req.Fields = append(req.Fields, f)
	}
	s.repo.mu.Lock()
	res, err := gate.Revert(ctx, req)
	s.repo.mu.Unlock()
	if err != nil {
		return nil, err
	}
	s.log.Addf(KindGit, "reverted %s in %s as %s (Botaru-Revert: %s)", id, res.File, res.Commit[:12], reason)
	return res, s.applyOrDrop(ctx, res.Commit)
}

// Variants are the variants of the scaffold archive of demo (P2 variant.list, EVO-6).
func (s *Stand) Variants(ctx context.Context) ([]map[string]any, error) {
	return s.items(ctx, "variant.list")
}

// ReviewRules are the live review rules of demo (P2 review_rule.list, EVO-3).
func (s *Stand) ReviewRules(ctx context.Context) ([]map[string]any, error) {
	return s.items(ctx, "review_rule.list")
}

func (s *Stand) items(ctx context.Context, op string) ([]map[string]any, error) {
	var out struct {
		Items []map[string]any `json:"items"`
	}
	if err := s.Bench.P2As(ctx, Decider, op, map[string]any{}, &out); err != nil {
		return nil, err
	}
	return out.Items, nil
}

// evolveTask is the instruction of ops.insight.v1 in the evolution mode, the load check: a real
// model hands in a manifest change only with a target it was told it may change and the series
// that back it, so the stand writes this into the trusted instruction in the farm git, in place of
// the usual one, as an operator who wants the check would; the mock model reads the flag instead.
// No paragraph has a word of the act group of the unawareness lint beside one of the outcome
// group (GOV-29, TestEvolveInstruction).
var evolveTask = evolveTaskFor("demo")

// evolveTaskFor is evolveTask for a home tenant.
func evolveTaskFor(home string) string {
	return fmt.Sprintf(`Load check of this stand, which its operator asked for. Read farm.task.attempts, farm.attempt.duration
and farm.task.outcome of task_type:ops.investigate.v1 with observations_query.

The investigations have an attempt deadline of %[1]s in the single dispatcher rule of tenant %[3]s
(spec.dispatch.rules[].timeouts.attempt), and their task deadline is 30m. An attempt cut at that
deadline starts again from the beginning.

When farm.attempt.duration reaches the deadline of 300 seconds (its rollup window.p95) and
farm.task.attempts shows tasks that needed more than one attempt, the first entry of proposals (proposals[0]) is this change, and nothing_found is not
an answer: change.kind manifest_change with the single op {"op": "set", "target": {"kind": "Tenant",
"name": "%[3]s"}, "field": "spec.dispatch.rules[].timeouts.attempt", "value": "%[2]s"}, with
benefit.metric farm.task.attempts, benefit.subject task_type:ops.investigate.v1 and benefit.direction
down, a cost, a risk, and the values you read as its evidence.

The operator tries the change on this stand and measures it there; a change of the kind none in its
place is not an answer.
`, InvestigateAttempt, EvolveAttempt, home)
}

// SetEvolve turns the evolution mode on or off: the mock model proposes the manifest change,
// and the instruction of ops.insight.v1 in the farm git asks a real model for the load check
// (applied as a commit of the farm git); off restores the instruction it had.
func (s *Stand) SetEvolve(ctx context.Context, on bool) error {
	commit, changed, err := s.writeEvolve(ctx, on)
	if err != nil {
		return err
	}
	s.Brain.SetEvolve(on)
	if changed && commit != "" {
		return s.applyOrDrop(ctx, commit)
	}
	return nil
}

// writeEvolve commits the instruction of the mode to the farm git: the load check on, the
// instruction it replaced off. It says whether the mode changed.
func (s *Stand) writeEvolve(ctx context.Context, on bool) (string, bool, error) {
	s.mu.Lock()
	prev, active := s.insightInstruction, s.evolveActive
	s.mu.Unlock()
	if on == active {
		return "", false, nil
	}
	value, msg := prev, "Restore the analysis instruction of ops-reader"
	if on {
		cur, err := s.taskInstruction(insightType)
		if err != nil {
			return "", false, err
		}
		prev, value, msg = cur, evolveTaskFor(s.Home()), "Ask the analysis of ops-reader for the load check"
	}
	commit, err := s.repo.setField(ctx, "TaskType", "", insightType, "spec.instruction", value, msg)
	if err != nil {
		return "", false, err
	}
	// The farm git holds the new instruction from here on: the mode follows it even when the
	// apply fails, so that turning the mode off restores the text.
	s.mu.Lock()
	s.insightInstruction, s.evolveActive = prev, on
	s.mu.Unlock()
	return commit, true, nil
}

// insightType is the analysis type of ops-reader.
const insightType = "ops.insight.v1"

// taskInstruction reads the instruction of a task type from the farm git.
func (s *Stand) taskInstruction(name string) (string, error) {
	s.repo.mu.Lock()
	set, err := s.repo.readSet()
	s.repo.mu.Unlock()
	if err != nil {
		return "", err
	}
	for _, m := range set {
		if m["kind"] == "TaskType" && mapOf(m["metadata"])["name"] == name {
			if v, ok := mapOf(m["spec"])["instruction"].(string); ok {
				return v, nil
			}
		}
	}
	return "", fmt.Errorf("no task type %s in the farm git", name)
}
