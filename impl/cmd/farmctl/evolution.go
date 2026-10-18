// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/HunterEwok/botaru/impl/internal/gate"
)

// The commands of the safe scaffold evolution (EVO-1..EVO-10): the variant archive, the review
// rules, the merge gate, the patch of an accepted change and the revert in one command. The gate
// and the revert run here, in temporary git worktrees on the operator's machine; the core only
// records their closed results (PRP-23: the core never writes git).

// stringList is a repeatable flag.
type stringList []string

func (l *stringList) String() string     { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }

func variant(c *client, args []string) error {
	switch {
	case len(args) >= 1 && args[0] == "list":
		fs := flag.NewFlagSet("variant list", flag.ExitOnError)
		target := fs.String("target", "", "only the variants of one object, KIND:NAME")
		limit := fs.Int("limit", 0, "at most this many")
		_ = fs.Parse(args[1:])
		return c.print("variant.list", map[string]any{"target": *target, "limit": *limit})
	case len(args) == 2 && args[0] == "show":
		return c.print("variant.show", map[string]any{"variant": args[1]})
	case len(args) >= 2 && args[0] == "checkout":
		return variantCheckout(c, args[1], args[2:])
	}
	return errors.New("usage: farmctl variant list [-target KIND:NAME] [-limit N] | variant show ID | variant checkout ID -out DIR [-repo DIR] [-set PATH]")
}

// variantCheckout writes a variant as variant.json and, with -repo, the manifest file that holds
// its object with the variant's fields, at the variant's base commit when the checkout has it.
func variantCheckout(c *client, id string, args []string) error {
	fs := flag.NewFlagSet("variant checkout", flag.ExitOnError)
	out := fs.String("out", "", "the folder to write into")
	repo := fs.String("repo", "", "the checkout of the farm manifests, for the manifest file")
	var sets stringList
	fs.Var(&sets, "set", "a set path inside -repo (repeatable; default: the whole checkout)")
	_ = fs.Parse(reorder(args))
	if *out == "" {
		return errors.New("variant checkout needs -out")
	}
	var v struct {
		Kind  string `json:"target_kind"`
		Name  string `json:"target_name"`
		Base  string `json:"base_commit"`
		Patch []struct {
			Field string `json:"field"`
			Value any    `json:"value"`
		} `json:"patch"`
		Proposal string `json:"proposal_id"`
		Tenant   string `json:"tenant"`
	}
	var raw map[string]any
	if err := c.call("variant.show", map[string]any{"variant": id}, &raw); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(raw, "", "  ")
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(*out, "variant.json"), append(b, '\n'), 0o644); err != nil {
		return err
	}
	fmt.Println(filepath.Join(*out, "variant.json"))
	if *repo == "" {
		return nil
	}
	ch := gate.Change{Proposal: v.Proposal, Tenant: v.Tenant}
	for _, p := range v.Patch {
		ch.Ops = append(ch.Ops, gate.Op{Kind: v.Kind, Name: v.Name, Field: p.Field, Value: p.Value})
	}
	base := v.Base
	if base == "" || !hasCommit(*repo, base) {
		base = "HEAD"
	}
	path, err := gate.Checkout(context.Background(), gate.Config{Repo: *repo, Base: base, Sets: sets}, ch, *out)
	if err != nil {
		return err
	}
	fmt.Println(path)
	return nil
}

func hasCommit(repo, sha string) bool {
	_, err := gitOut(repo, "rev-parse", "--verify", "-q", sha+"^{commit}")
	return err == nil
}

func gitOut(dir string, args ...string) (string, error) {
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
	return strings.TrimSpace(string(out)), err
}

func reviewRule(c *client, args []string) error {
	switch {
	case len(args) >= 1 && args[0] == "list":
		fs := flag.NewFlagSet("review-rule list", flag.ExitOnError)
		role := fs.String("role", "", "only the rules of one role")
		_ = fs.Parse(args[1:])
		return c.print("review_rule.list", map[string]any{"role": *role})
	case len(args) == 2 && args[0] == "drop":
		n, err := strconv.ParseInt(args[1], 10, 64)
		if err != nil {
			return fmt.Errorf("a rule is a number: %s", args[1])
		}
		return c.print("review_rule.drop", map[string]any{"rule": n})
	}
	return errors.New("usage: farmctl review-rule list [-role ROLE] | review-rule drop ID")
}

// proposalChange reads the manifest change of a proposal through P2 proposal.show.
func proposalChange(c *client, id string) (map[string]any, gate.Change, error) {
	var rec map[string]any
	if err := c.call("proposal.show", map[string]any{"proposal": id}, &rec); err != nil {
		return nil, gate.Change{}, err
	}
	ch := gate.Change{Proposal: id}
	ch.Tenant, _ = rec["tenant"].(string)
	body, _ := rec["body"].(map[string]any)
	change, _ := body["change"].(map[string]any)
	mc, _ := change["manifest_change"].(map[string]any)
	ops, _ := mc["ops"].([]any)
	if len(ops) == 0 {
		return nil, ch, fmt.Errorf("%s is not a manifest change", id)
	}
	for _, o := range ops {
		op, _ := o.(map[string]any)
		tg, _ := op["target"].(map[string]any)
		k, _ := tg["kind"].(string)
		n, _ := tg["name"].(string)
		f, _ := op["field"].(string)
		ch.Ops = append(ch.Ops, gate.Op{Kind: k, Name: n, Field: f, Value: op["value"]})
	}
	return rec, ch, nil
}

func proposalEvolution(c *client, args []string) (bool, error) {
	if len(args) < 2 {
		return false, nil
	}
	switch args[1] {
	case "gate":
		return true, proposalGate(c, args[2:])
	case "patch":
		return true, proposalPatch(c, args[2:])
	case "revert":
		return true, proposalRevert(c, args[2:])
	}
	return false, nil
}

// proposalGate runs the merge gate of manifest changes and posts the closed verdicts (EVO-9).
func proposalGate(c *client, args []string) error {
	fs := flag.NewFlagSet("proposal gate", flag.ExitOnError)
	repo := fs.String("repo", ".", "the checkout of the farm manifests")
	var sets stringList
	fs.Var(&sets, "set", "a set path inside -repo, as farmctl apply takes it (repeatable; default: the whole checkout)")
	base := fs.String("base", "", "the base commit (default: the commit the core applied)")
	holdout := fs.String("holdout-dir", "", "the hidden holdout cases, outside -repo (EVO-7)")
	evalBin := fs.String("eval-bin", "", "the botaru-eval binary for the regression and holdout steps (default: the steps do not run)")
	dsn := fs.String("dsn", "", "the PostgreSQL server of botaru-eval (default: $BOTARU_EVAL_DSN)")
	real := fs.Bool("real", false, "run the eval cases against a real provider (needs -key-file and -max-eur)")
	keyFile := fs.String("key-file", "", "the provider key file, with -real")
	maxEUR := fs.Float64("max-eur", 0, "the spend cap of each eval run in EUR, with -real")
	pending := fs.Bool("pending", false, "gate every recommended or presented manifest change without a gate at the applied commit")
	noPost := fs.Bool("no-post", false, "print the report only, record nothing")
	_ = fs.Parse(reorderBool(args, "-pending", "-no-post", "-real"))
	ids := fs.Args()
	if *pending {
		var l struct {
			Items []struct {
				ID string `json:"id"`
			} `json:"items"`
		}
		if err := c.call("proposal.list", map[string]any{"state": []string{"recommended", "presented"}}, &l); err != nil {
			return err
		}
		for _, it := range l.Items {
			ids = append(ids, it.ID)
		}
	}
	if len(ids) == 0 {
		return errors.New("usage: farmctl proposal gate ID... | -pending [-repo DIR] [-set PATH]... [-base SHA] [-holdout-dir DIR] [-eval-bin BIN [-dsn DSN] [-real -key-file F -max-eur N]] [-no-post]")
	}
	if *real && (*keyFile == "" || *maxEUR <= 0) {
		return errors.New("-real needs -key-file and -max-eur")
	}
	if *dsn == "" {
		*dsn = os.Getenv("BOTARU_EVAL_DSN")
	}
	failed := 0
	for _, id := range ids {
		var read struct {
			Tenant  string           `json:"tenant"`
			State   string           `json:"state"`
			Ops     []map[string]any `json:"ops"`
			Applied *string          `json:"applied_commit"`
			Limits  struct {
				MaxFields    int `json:"max_fields"`
				MaxDiffLines int `json:"max_diff_lines"`
			} `json:"limits"`
			Gate map[string]any `json:"gate"`
		}
		if err := c.call("proposal.gate", map[string]any{"proposal": id}, &read); err != nil {
			if *pending {
				continue // not a manifest change
			}
			return err
		}
		if read.Applied == nil {
			return errors.New("the core has applied no commit yet")
		}
		if *pending && (read.Gate != nil && read.Gate["base_sha"] == *read.Applied || read.State != "recommended" && read.State != "presented") {
			continue
		}
		ch := gate.Change{Proposal: id, Tenant: read.Tenant}
		for _, op := range read.Ops {
			tg, _ := op["target"].(map[string]any)
			k, _ := tg["kind"].(string)
			n, _ := tg["name"].(string)
			f, _ := op["field"].(string)
			ch.Ops = append(ch.Ops, gate.Op{Kind: k, Name: n, Field: f, Value: op["value"]})
		}
		b := *base
		if b == "" {
			b = *read.Applied
		}
		cfg := gate.Config{Repo: *repo, Base: b, Sets: sets, MaxFields: read.Limits.MaxFields, MaxDiffLines: read.Limits.MaxDiffLines,
			HoldoutDir: *holdout, Lint: planLint(c)}
		if *evalBin != "" {
			cfg.Suite = gate.EvalRunner{Bin: *evalBin, DSN: *dsn, Real: *real, KeyFile: *keyFile, MaxEUR: *maxEUR}.Suite()
		}
		rep, err := gate.Run(context.Background(), cfg, ch)
		if err != nil {
			return fmt.Errorf("%s: %w", id, err)
		}
		printGate(rep)
		if !rep.Passed() {
			failed++
		}
		if *noPost {
			continue
		}
		if rep.BaseSHA != *read.Applied {
			fmt.Printf("  not recorded: the gate ran on %s, the core applied %s\n", rep.BaseSHA, *read.Applied)
			continue
		}
		res := map[string]any{"verdict": rep.Verdict, "code": rep.Code, "base_sha": rep.BaseSHA, "holdout_passed": rep.HoldoutPassed,
			"regression_pass_rate": nil, "fields": rep.Fields, "diff_lines": rep.DiffLines}
		if rep.VisibleCandidate >= 0 {
			res["regression_pass_rate"] = rep.VisibleCandidate
		}
		var steps []map[string]any
		for _, s := range rep.Steps {
			steps = append(steps, map[string]any{"step": s.Name, "status": s.Status, "code": s.Code})
		}
		res["steps"] = steps
		var out struct {
			State string `json:"state"`
		}
		if err := c.call("proposal.gate", map[string]any{"proposal": id, "result": res}, &out); err != nil {
			return fmt.Errorf("%s: %w", id, err)
		}
		fmt.Printf("  recorded: the proposal is %s\n", out.State)
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d failed the gate", failed, len(ids))
	}
	return nil
}

func printGate(r *gate.Report) {
	fmt.Printf("proposal %s at %s: %s", r.Proposal, r.BaseSHA[:12], r.Verdict)
	if r.Code != "" {
		fmt.Printf(" (%s)", r.Code)
	}
	fmt.Println()
	for _, s := range r.Steps {
		line := fmt.Sprintf("  %-10s %-7s", s.Name, s.Status)
		if s.Code != "" {
			line += " " + s.Code
		}
		if s.Detail != "" {
			line += ": " + s.Detail
		}
		fmt.Println(line)
	}
	if r.HoldoutPassed != nil {
		fmt.Printf("  holdout_passed: %v\n", *r.HoldoutPassed)
	}
	if r.Diff != "" && r.Code != gate.ForbiddenPathModified {
		fmt.Println(indent(r.Diff, "  | "))
	}
}

func indent(s, prefix string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n")
}

// planLint is the lint step: an apply plan of the candidate set through P2, so the core runs
// every schema and set check it runs at apply (MAN-6, MAN-10, DSP-3, INJ-6, SEC-8).
func planLint(c *client) gate.Lint {
	return func(_ context.Context, dir string, sets []string) ([]string, error) {
		var paths []string
		for _, s := range sets {
			paths = append(paths, filepath.Join(dir, s))
		}
		ms, docs, err := readSet(paths, false)
		if err != nil {
			return []string{err.Error()}, nil
		}
		commit, err := gitOut(dir, "rev-parse", "HEAD")
		if err != nil {
			return nil, err
		}
		var plan struct {
			Errors []struct {
				Kind, Name, Code, Path string
			} `json:"errors"`
		}
		if err := c.call("apply", applyRequest(docs, ms, commit, true), &plan); err != nil {
			return nil, err
		}
		var out []string
		for _, e := range plan.Errors {
			out = append(out, strings.TrimSpace(fmt.Sprintf("%s %s %s %s", e.Code, e.Kind, e.Name, e.Path)))
		}
		return out, nil
	}
}

// proposalPatch writes the change of an accepted proposal as a mail patch with the
// Botaru-Proposal line, after the checks of the diff step (PRP-19).
func proposalPatch(c *client, args []string) error {
	fs := flag.NewFlagSet("proposal patch", flag.ExitOnError)
	repo := fs.String("repo", ".", "the checkout of the farm manifests")
	var sets stringList
	fs.Var(&sets, "set", "a set path inside -repo (repeatable)")
	out := fs.String("out", "", "the patch file (default: standard output)")
	_ = fs.Parse(reorder(args))
	if fs.NArg() != 1 {
		return errors.New("usage: farmctl proposal patch ID [-repo DIR] [-set PATH]... [-out FILE]")
	}
	rec, ch, err := proposalChange(c, fs.Arg(0))
	if err != nil {
		return err
	}
	if rec["state"] != "realizing" {
		return fmt.Errorf("%s is %v; a patch is written for an accepted change (realizing)", ch.Proposal, rec["state"])
	}
	title := "Apply proposal " + ch.Proposal
	if p, ok := rec["body"].(map[string]any)["problem"].(map[string]any); ok {
		if code, _ := p["code"].(string); code != "" {
			title += ": " + code
		}
	}
	patch, err := gate.Patch(context.Background(), gate.Config{Repo: *repo, Sets: sets, MaxFields: 10, MaxDiffLines: 400}, ch, title)
	if err != nil {
		return err
	}
	if *out == "" {
		_, err = os.Stdout.Write(patch)
		return err
	}
	return os.WriteFile(*out, patch, 0o644)
}

// proposalRevert reverts a realized change in one command: the reverse edit as a commit with
// Botaru-Revert on the current branch, then apply of the set, which moves the proposal to
// reverted, ends its trial and records the variant and the review rule (EVO-10, PRP-20).
func proposalRevert(c *client, args []string) error {
	fs := flag.NewFlagSet("proposal revert", flag.ExitOnError)
	repo := fs.String("repo", ".", "the checkout of the farm manifests, on the branch to revert on")
	var sets stringList
	fs.Var(&sets, "set", "a set path inside -repo, as farmctl apply takes it (repeatable; default: the whole checkout)")
	reason := fs.String("reason", "", "harm_observed, superseded or other")
	noApply := fs.Bool("no-apply", false, "commit only; apply later")
	_ = fs.Parse(reorderBool(args, "-no-apply"))
	if fs.NArg() != 1 || *reason == "" {
		return errors.New("usage: farmctl proposal revert ID -reason harm_observed|superseded|other [-repo DIR] [-set PATH]... [-no-apply]")
	}
	rec, ch, err := proposalChange(c, fs.Arg(0))
	if err != nil {
		return err
	}
	if rec["state"] != "realized" {
		return fmt.Errorf("%s is %v; only a realized change is reverted", ch.Proposal, rec["state"])
	}
	real, _ := rec["realization"].(map[string]any)
	patch, _ := real["revert_patch"].([]any)
	if len(patch) == 0 {
		return fmt.Errorf("%s has no reverse edit", ch.Proposal)
	}
	accepted := map[string]any{}
	for _, op := range ch.Ops {
		accepted[op.Kind+"/"+op.Name+"/"+op.Field] = op.Value
	}
	req := gate.RevertRequest{Repo: *repo, Sets: sets, Tenant: ch.Tenant, Proposal: ch.Proposal, Reason: *reason}
	for _, e := range patch {
		em, _ := e.(map[string]any)
		tg, _ := em["target"].(map[string]any)
		f := gate.RevertField{Field: fmt.Sprint(em["field"]), Previous: em["previous"]}
		f.Kind, _ = tg["kind"].(string)
		f.Name, _ = tg["name"].(string)
		f.Accepted = accepted[f.Kind+"/"+f.Name+"/"+f.Field]
		req.Fields = append(req.Fields, f)
	}
	res, err := gate.Revert(context.Background(), req)
	if err != nil {
		return err
	}
	fmt.Printf("reverted %s in %s on %s: commit %s\n", ch.Proposal, res.File, res.Branch, res.Commit)
	if *noApply {
		fmt.Println("apply it with: farmctl apply -commit " + res.Commit + " <set paths>")
		return nil
	}
	paths := []string{}
	for _, s := range sets {
		paths = append(paths, filepath.Join(*repo, s))
	}
	if len(paths) == 0 {
		paths = []string{*repo}
	}
	if err := apply(c, append([]string{"-commit", res.Commit}, paths...)); err != nil {
		return err
	}
	fmt.Println("push the branch to keep the farm git in step: git -C " + *repo + " push")
	return nil
}

// reorderBool is reorder for commands whose boolean flags take no value.
func reorderBool(args []string, bools ...string) []string {
	var flags, pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			pos = append(pos, a)
			continue
		}
		flags = append(flags, a)
		isBool := strings.Contains(a, "=")
		for _, b := range bools {
			if a == b {
				isBool = true
			}
		}
		if !isBool && i+1 < len(args) {
			flags = append(flags, args[i+1])
			i++
		}
	}
	return append(flags, pos...)
}
