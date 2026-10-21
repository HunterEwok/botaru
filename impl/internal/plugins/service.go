// SPDX-License-Identifier: AGPL-3.0-only

package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/HunterEwok/botaru/impl/internal/mcp"
)

// unitName is an exact service unit name without a template or a mask (MON-8, DEP-4).
var unitName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9:_.-]{0,239}\.service$`)

// Service is the write plugin service: the unit mechanism of the systemd binding. It runs
// systemctl <verb> <unit> without a shell, where the core resolved the exact unit from the
// catalog, then reads the unit state by systemctl is-active (spec/bindings/os/systemd.md).
// The output of systemctl does not reach the result: only the state does.
type Service struct {
	// Systemctl is the path of systemctl; /usr/bin/systemctl when empty.
	Systemctl string
	// Wait bounds the wait for the state after the verb.
	Wait time.Duration
	// run runs a program and returns its output and exit code; tests replace it.
	run func(ctx context.Context, name string, args ...string) (string, int, error)
}

func (s *Service) runner() func(ctx context.Context, name string, args ...string) (string, int, error) {
	if s.run != nil {
		return s.run
	}
	return func(ctx context.Context, name string, args ...string) (string, int, error) {
		x := exec.CommandContext(ctx, name, args...)
		x.Env, x.Dir = CmdEnv, "/"
		out := &limitBuffer{max: 4096}
		x.Stdout, x.Stderr = out, out
		err := x.Run()
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return out.String(), ee.ExitCode(), nil
		}
		return out.String(), 0, err
	}
}

// Call runs service/control {service, verb}.
func (s *Service) Call(ctx context.Context, tool string, args map[string]any, meta *mcp.Meta) (*mcp.CallResult, error) {
	if tool != "control" {
		return nil, errors.New("unknown tool")
	}
	verb, _ := args["verb"].(string)
	if !slices.Contains([]string{"start", "stop", "restart"}, verb) {
		return nil, errors.New("unknown verb")
	}
	unit := ""
	if meta != nil {
		unit = meta.Resolved["service.unit"]
	}
	if !unitName.MatchString(unit) {
		return nil, errors.New("the core resolved no exact unit")
	}
	bin := s.Systemctl
	if bin == "" {
		bin = "/usr/bin/systemctl"
	}
	run := s.runner()
	if _, code, err := run(ctx, bin, verb, "--", unit); err != nil || code != 0 {
		return nil, fmt.Errorf("systemctl %s failed", verb)
	}
	want := "active"
	if verb == "stop" {
		want = "inactive"
	}
	wait := s.Wait
	if wait <= 0 {
		wait = 30 * time.Second
	}
	deadline := time.Now().Add(wait)
	state := "unknown"
	for {
		out, _, err := run(ctx, bin, "is-active", "--", unit)
		if err == nil {
			state = unitState(out)
		}
		if state == want || state == "failed" || !time.Now().Before(deadline) || ctx.Err() != nil {
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
	}
	return &mcp.CallResult{Content: []mcp.Content{}, StructuredContent: map[string]any{"state": state}}, nil
}

// unitState maps the answer of systemctl is-active to the closed states of the result.
func unitState(out string) string {
	switch s := strings.TrimSpace(out); s {
	case "active", "inactive", "failed":
		return s
	case "activating", "reloading", "refreshing":
		return "active"
	case "deactivating":
		return "inactive"
	}
	return "unknown"
}

// Queue is the action plugin of the oneshot mechanism: it puts the run request of a write call
// as a file into the tenant queue, where the path unit of the tenant starts the root request
// parser (spec/bindings/os/systemd.md). The request holds only the closed arguments, the tool
// and the approval or grant number; a repeat of the same number finds its file and is not
// queued twice (APR-7, FEN-7). The number of a campaign item is <campaign id>:<n>; its file is
// named <campaign id>-<n>.
type Queue struct {
	Dir string
}

var workID = regexp.MustCompile(`^[a-z]{1,8}_[0-9A-HJKMNP-TV-Z]{26}(:[1-9][0-9]{0,5})?$`)

// Call queues one run request.
func (q *Queue) Call(ctx context.Context, tool string, args map[string]any, meta *mcp.Meta) (*mcp.CallResult, error) {
	if meta == nil || !workID.MatchString(meta.WorkID) {
		return nil, errors.New("a write without an approval or grant number")
	}
	path := filepath.Join(q.Dir, strings.Replace(meta.WorkID, ":", "-", 1)+".json")
	accepted := &mcp.CallResult{Content: []mcp.Content{}, StructuredContent: map[string]any{"code": "accepted", "run": meta.WorkID}}
	if _, err := os.Stat(path); err == nil {
		return accepted, nil
	}
	raw, err := json.Marshal(map[string]any{"id": meta.WorkID, "tool": tool, "args": args, "at": time.Now().UTC().Format(time.RFC3339)})
	if err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(q.Dir, ".req-*")
	if err != nil {
		return nil, err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(raw); err != nil {
		f.Close()
		return nil, err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	// A link fails when the file exists, so two runs of one number never both queue it.
	if err := os.Link(tmp, path); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	return accepted, nil
}

// PolkitRule is the polkit file that lets the OS user of a tenant's action toolhost run exactly
// the catalog verbs on the exact units of one machine and nothing else (DEP-4,
// spec/bindings/os/systemd.md). units maps a unit to its verbs.
func PolkitRule(source, user string, units map[string][]string) (string, error) {
	if !regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`).MatchString(user) {
		return "", errors.New("the OS user name is not a plain name")
	}
	var b strings.Builder
	fmt.Fprintf(&b, "// generated by botaru-plugin polkit from %s; do not edit\n", strings.ReplaceAll(source, "\n", " "))
	b.WriteString("polkit.addRule(function(action, subject) {\n")
	b.WriteString("  if (action.id !== \"org.freedesktop.systemd1.manage-units\") return polkit.Result.NOT_HANDLED;\n")
	fmt.Fprintf(&b, "  if (subject.user !== %q) return polkit.Result.NOT_HANDLED;\n", user)
	b.WriteString("  var unit = action.lookup(\"unit\");\n  var verb = action.lookup(\"verb\");\n")
	names := make([]string, 0, len(units))
	for u := range units {
		names = append(names, u)
	}
	sort.Strings(names)
	for _, u := range names {
		if !unitName.MatchString(u) {
			return "", fmt.Errorf("%q is not an exact service unit", u)
		}
		var conds []string
		verbs := slices.Clone(units[u])
		sort.Strings(verbs)
		for _, v := range slices.Compact(verbs) {
			if !slices.Contains([]string{"start", "stop", "restart"}, v) {
				return "", fmt.Errorf("verb %q is not start, stop or restart", v)
			}
			conds = append(conds, fmt.Sprintf("verb === %q", v))
		}
		if len(conds) == 0 {
			continue
		}
		fmt.Fprintf(&b, "  if (unit === %q && (%s))\n    return polkit.Result.YES;\n", u, strings.Join(conds, " || "))
	}
	b.WriteString("  return polkit.Result.NO;\n});\n")
	return b.String(), nil
}
