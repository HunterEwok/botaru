// SPDX-License-Identifier: AGPL-3.0-only

package gate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/HunterEwok/botaru/impl/internal/evals"
)

// RevertReasons are the reasons of a Botaru-Revert line (PRP-20).
var RevertReasons = []string{"harm_observed", "superseded", "other"}

// RevertField is one field of a realized change: the value the change set and the value before.
type RevertField struct {
	Kind     string `json:"kind"`
	Name     string `json:"name"`
	Field    string `json:"field"`
	Accepted any    `json:"accepted"`
	Previous any    `json:"previous"`
	// Absent marks a field the change added: the revert takes it out.
	Absent bool `json:"absent,omitempty"`
}

// RevertRequest is the revert of one realized proposal.
type RevertRequest struct {
	Repo     string
	Sets     []string
	Tenant   string
	Proposal string
	Reason   string
	Fields   []RevertField
}

// RevertResult is the revert commit, already on the person's branch.
type RevertResult struct {
	Commit string `json:"commit"`
	Branch string `json:"branch"`
	File   string `json:"file"`
	// TargetHashes are the JCS hashes of the target objects before and after the revert.
	TargetHashes map[string][2]string `json:"target_hashes"`
}

// Revert writes the previous values of a realized change into the manifests in a temporary
// worktree of the current branch's head, checks that only those fields changed, commits with
// the line `Botaru-Revert: <id> <reason>` and moves the branch to that commit with a
// fast-forward (EVO-10, PRP-20). It refuses a checkout with uncommitted changes or without a
// branch, and a field that no longer holds the value the change set. The core never writes git
// (PRP-23): this runs on the person's command, and the person pushes.
func Revert(ctx context.Context, r RevertRequest) (*RevertResult, error) {
	if !contains(RevertReasons, r.Reason) {
		return nil, fmt.Errorf("the reason %q is not one of %s", r.Reason, strings.Join(RevertReasons, ", "))
	}
	if len(r.Fields) == 0 {
		return nil, errors.New("the proposal has no reverse edit")
	}
	if err := clean(ctx, r.Repo); err != nil {
		return nil, err
	}
	branch, err := git(ctx, r.Repo, "symbolic-ref", "--short", "HEAD")
	if err != nil {
		return nil, err
	}
	head, err := resolve(ctx, r.Repo, "HEAD")
	if err != nil {
		return nil, err
	}
	w, err := newWorktree(ctx, r.Repo, head, "revert")
	if err != nil {
		return nil, err
	}
	defer w.close()
	cfg := withDefaults(Config{Repo: r.Repo, Sets: r.Sets, MaxFields: len(r.Fields), MaxDiffLines: 400})
	before, err := readSets(w.dir, cfg.Sets)
	if err != nil {
		return nil, err
	}
	targets := map[string]target{}
	var ops []Op
	for _, f := range r.Fields {
		key := f.Kind + "/" + f.Name
		t, ok := targets[key]
		if !ok {
			if t, err = locate(w.dir, before, r.Tenant, f.Kind, f.Name); err != nil {
				return nil, err
			}
			targets[key] = t
		}
		cur, ok := pathValue(t.d.Value, f.Field)
		if !ok || !equal(cur, f.Accepted) {
			return nil, fmt.Errorf("%s %s %s holds %v, not the accepted %v: the change was edited since; revert it by hand", f.Kind, f.Name, f.Field, cur, f.Accepted)
		}
		ops = append(ops, Op{Kind: f.Kind, Name: f.Name, Field: f.Field, Value: f.Previous, Remove: f.Absent})
	}
	if err := write(w.dir, targets, ops); err != nil {
		return nil, err
	}
	// The editor writes no link, and the staged paths are checked again below.
	changes, err := stagedChanges(ctx, w.dir)
	if err != nil {
		return nil, err
	}
	for _, f := range changes {
		for _, p := range ForbiddenPrefixes {
			if strings.HasPrefix(f.Path, p) {
				return nil, fmt.Errorf("the revert would change %s", f.Path)
			}
		}
		if f.Symlink || throughLink(w.dir, f.Path) {
			return nil, fmt.Errorf("%s is or lies under a symbolic link", f.Path)
		}
	}
	if len(changes) != 1 {
		return nil, fmt.Errorf("the revert changes %d files, want one", len(changes))
	}
	after, err := readSets(w.dir, cfg.Sets)
	if err != nil {
		return nil, err
	}
	res := &RevertResult{Branch: branch, File: changes[0].Path, TargetHashes: map[string][2]string{}}
	if ref := semantic(w.dir, before, after, targets, ops, res.TargetHashes); ref != nil {
		return nil, fmt.Errorf("%s: %s", ref.code, ref.detail)
	}
	msg := fmt.Sprintf("Revert proposal %s (%s)\n\nBotaru-Revert: %s %s\n", r.Proposal, r.Reason, r.Proposal, r.Reason)
	if _, err := git(ctx, w.dir, "commit", "--no-verify", "-q", "-m", msg); err != nil {
		return nil, err
	}
	if res.Commit, err = resolve(ctx, w.dir, "HEAD"); err != nil {
		return nil, err
	}
	// The person's branch moves only forward, and only when nothing moved it meanwhile.
	if _, err := git(ctx, r.Repo, "merge", "--ff-only", "-q", res.Commit); err != nil {
		return nil, err
	}
	return res, nil
}

// Patch writes the change of an accepted proposal as a commit on the base in a temporary
// worktree and returns it as a mail patch with the line `Botaru-Proposal: <id>`, for the
// executor to apply with git am (PRP-19). The checks of the diff step run first.
func Patch(ctx context.Context, cfg Config, ch Change, title string) ([]byte, error) {
	cfg = withDefaults(cfg)
	base, err := resolve(ctx, cfg.Repo, cfg.Base)
	if err != nil {
		return nil, err
	}
	w, err := newWorktree(ctx, cfg.Repo, base, "patch")
	if err != nil {
		return nil, err
	}
	defer w.close()
	_, ref, err := prepare(ctx, cfg, w.dir, ch, nil)
	if err != nil {
		return nil, err
	}
	if ref != nil {
		return nil, fmt.Errorf("%s: %s", ref.code, ref.detail)
	}
	if title == "" {
		title = "Apply proposal " + ch.Proposal
	}
	msg := fmt.Sprintf("%s\n\nBotaru-Proposal: %s\n", strings.ReplaceAll(title, "\n", " "), ch.Proposal)
	if _, err := git(ctx, w.dir, "commit", "--no-verify", "-q", "-m", msg); err != nil {
		return nil, err
	}
	out, err := git(ctx, w.dir, "format-patch", "-1", "--stdout", "HEAD")
	if err != nil {
		return nil, err
	}
	return []byte(out + "\n"), nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// EvalRunner runs botaru-eval as a child process for the suite steps: with the worktree as
// the skeleton root, a fresh output folder and an environment that holds only the path, the
// home and the temporary folder, so a candidate reads no secret of the operator. The mock model
// is the default; a real model needs the key file and a spend cap.
type EvalRunner struct {
	Bin     string
	DSN     string
	Real    bool
	KeyFile string
	MaxEUR  float64
	Timeout time.Duration
}

// Suite is the gate's Suite over botaru-eval. A root without evals/cases runs no visible case.
func (e EvalRunner) Suite() Suite {
	return func(ctx context.Context, root string, roles []string, holdout string) (evals.SuiteResult, error) {
		if holdout == "" {
			if st, err := os.Stat(filepath.Join(root, "evals/cases")); err != nil || !st.IsDir() {
				return evals.SuiteResult{}, nil
			}
		}
		out, err := os.MkdirTemp("", "botaru-gate-eval-*")
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(out)
		args := []string{"-root", root, "-out", out}
		if len(roles) > 0 {
			args = append(args, "-role", strings.Join(roles, ","))
		}
		if holdout != "" {
			args = append(args, "-holdout-dir", holdout)
		}
		if e.Real {
			args = append(args, "-real", "-key-file", e.KeyFile, "-max-eur", fmt.Sprint(e.MaxEUR))
		}
		timeout := e.Timeout
		if timeout <= 0 {
			timeout = 30 * time.Minute
		}
		cctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		cmd := exec.CommandContext(cctx, e.Bin, args...)
		cmd.Dir = root
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "TMPDIR=" + os.TempDir()}
		if e.DSN != "" {
			// In the environment, not on the command line another user can read.
			cmd.Env = append(cmd.Env, "BOTARU_EVAL_DSN="+e.DSN)
		}
		var errb bytes.Buffer
		cmd.Stdout, cmd.Stderr = &errb, &errb
		runErr := cmd.Run()
		f, err := os.Open(filepath.Join(out, "results.jsonl"))
		if err != nil {
			// No results: the run itself failed. The output of a run over the hidden set may
			// name its cases, so it stays out of the error (EVO-7).
			msg := strings.TrimSpace(errb.String())
			if holdout != "" {
				msg = "the output of a run over the hidden set is not shown"
			} else if len(msg) > 2000 {
				msg = msg[len(msg)-2000:]
			}
			return nil, fmt.Errorf("botaru-eval: %v: %s", runErr, msg)
		}
		defer f.Close()
		return evals.ReadResults(f)
	}
}

// Checkout writes the manifest file of a variant: the file that holds the target object at the
// base commit, with the variant's fields set by the line editor, under out at its path in the
// repository. It returns that path.
func Checkout(ctx context.Context, cfg Config, ch Change, out string) (string, error) {
	cfg = withDefaults(cfg)
	cfg.MaxFields, cfg.MaxDiffLines = 10, 100000
	base, err := resolve(ctx, cfg.Repo, cfg.Base)
	if err != nil {
		return "", err
	}
	w, err := newWorktree(ctx, cfg.Repo, base, "checkout")
	if err != nil {
		return "", err
	}
	defer w.close()
	c, ref, err := prepare(ctx, cfg, w.dir, ch, nil)
	if err != nil {
		return "", err
	}
	if ref != nil {
		return "", fmt.Errorf("%s: %s", ref.code, ref.detail)
	}
	b, err := os.ReadFile(filepath.Join(w.dir, c.files[0]))
	if err != nil {
		return "", err
	}
	dst := filepath.Join(out, filepath.FromSlash(c.files[0]))
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", err
	}
	return dst, os.WriteFile(dst, b, 0o644)
}
