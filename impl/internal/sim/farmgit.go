// SPDX-License-Identifier: AGPL-3.0-only

package sim

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"go.yaml.in/yaml/v3"

	"github.com/HunterEwok/botaru/impl/internal/manifest"
)

// The farm git of the stand (D-120): the stand keeps its manifest set in a git repository of the
// work directory, as an installation keeps it in its own, so the merge gate, the patch of an
// accepted change and the one-command revert of internal/gate run on real commits. Every apply
// of the stand names the commit of that repository, and its message carries the lines
// Botaru-Proposal and Botaru-Revert (PRP-19, PRP-20). The core never writes git (PRP-23): the
// stand writes it in the operator's place.

// farmSet is the folder of the manifests in the farm git, the one set path the gate reads.
const farmSet = "farm"

// farmRepo is the farm git of the stand at <work>/repo.
type farmRepo struct {
	dir string
	// mu keeps one writer at a time: the director and the console both commit.
	mu sync.Mutex
}

// initRepo creates the farm git with the visible eval cases of the stand's suite; the manifests
// come with the first apply.
func initRepo(work string) (*farmRepo, error) {
	r := &farmRepo{dir: filepath.Join(work, "repo")}
	if err := os.MkdirAll(r.dir, 0o755); err != nil {
		return nil, err
	}
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"config", "user.name", "botaru-sim"},
		{"config", "user.email", "botaru-sim@example.org"}, {"config", "commit.gpgsign", "false"}} {
		if _, err := r.git(context.Background(), args...); err != nil {
			return nil, err
		}
	}
	if err := writeCases(filepath.Join(r.dir, "evals", "cases"), visibleCases); err != nil {
		return nil, err
	}
	return r, nil
}

// git runs git in the farm git and returns its trimmed standard output.
func (r *farmRepo) git(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", r.dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_EDITOR=true")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

// manifestPath is the file of a manifest in the farm git: farm/<tenant>/<Kind>/<name>.yaml, with
// _farm for the objects of no tenant.
func manifestPath(m map[string]any) string {
	md := mapOf(m["metadata"])
	name, _ := md["name"].(string)
	tenant, _ := md["tenant"].(string)
	kind, _ := m["kind"].(string)
	if tenant == "" && kind == "Tenant" {
		tenant = name
	}
	if tenant == "" {
		tenant = "_farm"
	}
	clean := func(s string) string { return strings.NewReplacer("/", "_", "\\", "_", "..", "_").Replace(s) }
	return filepath.Join(farmSet, clean(tenant), clean(kind), clean(name)+".yaml")
}

// writeSet writes a set into the farm folder, one manifest per file, removes the files of the
// manifests that left it and commits when anything changed. It returns the commit, its message
// and the set read back from it.
func (r *farmRepo) writeSet(ctx context.Context, ms []map[string]any, message string) (string, string, []map[string]any, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	keep := map[string]bool{}
	for _, m := range ms {
		p := manifestPath(m)
		for i := 2; keep[p]; i++ {
			p = strings.TrimSuffix(manifestPath(m), ".yaml") + fmt.Sprintf("-%d.yaml", i)
		}
		keep[p] = true
		if err := writeYAML(filepath.Join(r.dir, p), m); err != nil {
			return "", "", nil, err
		}
	}
	err := filepath.WalkDir(filepath.Join(r.dir, farmSet), func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(r.dir, path)
		if !keep[rel] {
			return os.Remove(path)
		}
		return nil
	})
	if err != nil {
		return "", "", nil, err
	}
	commit, msg, err := r.commitLocked(ctx, message)
	if err != nil {
		return "", "", nil, err
	}
	set, err := r.readSet()
	return commit, msg, set, err
}

func writeYAML(path string, v any) error {
	b, err := yaml.Marshal(plainValue(v))
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

// plainValue turns the JSON numbers of a manifest read by the manifest package into Go numbers,
// so they are written as YAML numbers, not as strings.
func plainValue(v any) any {
	switch x := v.(type) {
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return i
		}
		f, _ := x.Float64()
		return f
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = plainValue(e)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = plainValue(e)
		}
		return out
	}
	return v
}

// commitLocked commits every change of the work tree with the message; with nothing to commit
// it returns the head. It returns the head commit and its message.
func (r *farmRepo) commitLocked(ctx context.Context, message string) (string, string, error) {
	if _, err := r.git(ctx, "add", "-A"); err != nil {
		return "", "", err
	}
	st, err := r.git(ctx, "status", "--porcelain")
	if err != nil {
		return "", "", err
	}
	if st != "" {
		if _, err := r.git(ctx, "commit", "-q", "--no-verify", "-m", message); err != nil {
			return "", "", err
		}
	}
	return r.head(ctx)
}

// head is the head commit of the farm git and its message.
func (r *farmRepo) head(ctx context.Context) (string, string, error) {
	commit, err := r.git(ctx, "rev-parse", "HEAD")
	if err != nil {
		return "", "", err
	}
	msg, err := r.git(ctx, "log", "-1", "--format=%B", commit)
	return commit, msg, err
}

// readSet reads the manifests of the farm folder of the work tree.
func (r *farmRepo) readSet() ([]map[string]any, error) {
	docs, err := manifest.ReadFiles([]string{filepath.Join(r.dir, farmSet)})
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, len(docs))
	for i, d := range docs {
		out[i] = d.Value
	}
	return out, nil
}

// setField sets a field of one manifest file of the farm folder and commits it with the
// message; a field that already holds the value commits nothing. It returns whether it
// committed.
func (r *farmRepo) setField(ctx context.Context, kind, tenant, name, field string, value any, message string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	md := map[string]any{"name": name}
	if kind != "Tenant" {
		md["tenant"] = tenant
	}
	m := map[string]any{"kind": kind, "metadata": md}
	path := filepath.Join(r.dir, manifestPath(m))
	docs, err := manifest.ReadFiles([]string{path})
	if err != nil {
		return false, err
	}
	if len(docs) != 1 {
		return false, fmt.Errorf("%s holds %d manifests", path, len(docs))
	}
	doc := docs[0].Value
	cur := doc
	parts := strings.Split(field, ".")
	for _, p := range parts[:len(parts)-1] {
		next := mapOf(cur[p])
		if next == nil {
			next = map[string]any{}
			cur[p] = next
		}
		cur = next
	}
	last := parts[len(parts)-1]
	if old, ok := cur[last]; ok && fmt.Sprint(old) == fmt.Sprint(value) {
		return false, nil
	}
	cur[last] = value
	if err := writeYAML(path, doc); err != nil {
		return false, err
	}
	before, _, err := r.head(ctx)
	if err != nil {
		return false, err
	}
	after, _, err := r.commitLocked(ctx, message)
	return err == nil && after != before, err
}

// fieldValue reads a field of a manifest value by its dotted path of map keys.
func fieldValue(doc map[string]any, field string) (any, bool) {
	var cur any = doc
	for _, p := range strings.Split(field, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		if cur, ok = m[p]; !ok {
			return nil, false
		}
	}
	return cur, true
}

// findManifest is the one manifest of a kind and name in a set, of the tenant when the
// manifest has one.
func findManifest(ms []map[string]any, kind, tenant, name string) map[string]any {
	var found []map[string]any
	for _, m := range ms {
		md := mapOf(m["metadata"])
		if m["kind"] != kind || md["name"] != name {
			continue
		}
		if t, ok := md["tenant"].(string); ok && t != tenant {
			continue
		}
		found = append(found, m)
	}
	if len(found) != 1 {
		return nil
	}
	return found[0]
}

// containsValue says whether a list holds the value, or a scalar equals it.
func containsValue(v any, want string) bool {
	if l, ok := v.([]any); ok {
		return slices.ContainsFunc(l, func(x any) bool { return fmt.Sprint(x) == want })
	}
	return fmt.Sprint(v) == want
}
