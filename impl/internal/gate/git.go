// SPDX-License-Identifier: AGPL-3.0-only

package gate

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// git runs git in a folder and returns its trimmed standard output.
func git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	// The person's hooks and editors stay out: the gate commits only in its own worktree.
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_EDITOR=true")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	return strings.TrimSpace(out.String()), nil
}

// worktree is a temporary git worktree of one commit, detached, in a folder of its own.
type worktree struct {
	repo, dir, tmp string
}

// newWorktree checks out the commit into os.MkdirTemp("", "botaru-gate-*"); close removes it
// and prunes the repository's worktree list, also after a failure.
func newWorktree(ctx context.Context, repo, commit, name string) (*worktree, error) {
	tmp, err := os.MkdirTemp("", "botaru-gate-*")
	if err != nil {
		return nil, err
	}
	w := &worktree{repo: repo, dir: filepath.Join(tmp, name), tmp: tmp}
	if _, err := git(ctx, repo, "worktree", "add", "--detach", w.dir, commit); err != nil {
		w.close()
		return nil, err
	}
	return w, nil
}

func (w *worktree) close() {
	// A canceled context must not leave the worktree behind.
	ctx := context.Background()
	if _, err := os.Stat(w.dir); err == nil {
		_, _ = git(ctx, w.repo, "worktree", "remove", "--force", w.dir)
	}
	_ = os.RemoveAll(w.tmp)
	_, _ = git(ctx, w.repo, "worktree", "prune")
}

// resolve returns the full commit id of a revision of the repository.
func resolve(ctx context.Context, repo, rev string) (string, error) {
	if rev == "" {
		rev = "HEAD"
	}
	return git(ctx, repo, "rev-parse", "--verify", rev+"^{commit}")
}

// fileChange is one changed path of a staged diff.
type fileChange struct {
	Path           string
	Added, Deleted int
	Binary         bool
	Symlink        bool
}

// stagedChanges stages everything of the worktree and lists the changed paths with their line
// counts and whether a path is or was a symbolic link.
func stagedChanges(ctx context.Context, dir string) ([]fileChange, error) {
	if _, err := git(ctx, dir, "add", "-A"); err != nil {
		return nil, err
	}
	num, err := git(ctx, dir, "diff", "--cached", "--no-renames", "--numstat")
	if err != nil {
		return nil, err
	}
	raw, err := git(ctx, dir, "diff", "--cached", "--no-renames", "--raw")
	if err != nil {
		return nil, err
	}
	links := map[string]bool{}
	for _, l := range strings.Split(raw, "\n") {
		// :<old mode> <new mode> <old sha> <new sha> <status>\t<path>
		meta, path, ok := strings.Cut(l, "\t")
		if !ok {
			continue
		}
		f := strings.Fields(meta)
		if len(f) >= 2 && (strings.TrimPrefix(f[0], ":") == "120000" || f[1] == "120000") {
			links[path] = true
		}
	}
	var out []fileChange
	for _, l := range strings.Split(num, "\n") {
		f := strings.SplitN(l, "\t", 3)
		if len(f) != 3 {
			continue
		}
		c := fileChange{Path: f[2], Symlink: links[f[2]]}
		if f[0] == "-" || f[1] == "-" {
			c.Binary = true
		} else {
			fmt.Sscan(f[0], &c.Added)
			fmt.Sscan(f[1], &c.Deleted)
		}
		out = append(out, c)
	}
	return out, nil
}

// throughLink says whether a path of the worktree is a symbolic link or lies under one.
func throughLink(dir, rel string) bool {
	p := dir
	for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
		p = filepath.Join(p, part)
		st, err := os.Lstat(p)
		if err != nil {
			return false
		}
		if st.Mode()&os.ModeSymlink != 0 {
			return true
		}
	}
	return false
}

// clean says whether a checkout has no change and is on a branch.
func clean(ctx context.Context, repo string) error {
	st, err := git(ctx, repo, "status", "--porcelain", "--untracked-files=no")
	if err != nil {
		return err
	}
	if st != "" {
		return fmt.Errorf("%s has uncommitted changes; commit or stash them first", repo)
	}
	if _, err := git(ctx, repo, "symbolic-ref", "-q", "HEAD"); err != nil {
		return fmt.Errorf("%s is not on a branch", repo)
	}
	return nil
}
