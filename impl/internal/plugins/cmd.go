// SPDX-License-Identifier: AGPL-3.0-only

package plugins

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/HunterEwok/botaru/impl/internal/mcp"
	"github.com/HunterEwok/botaru/impl/internal/toolspec"
)

// CmdEnv is the whole environment of a command: a fixed search path and the C locale, nothing
// from the toolhost (INJ-14).
var CmdEnv = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LC_ALL=C"}

// Cmd is the program launch plugin: a named command of the manifest runs through execve
// without a shell, with an absolute path, a cleared environment and every parameter as a
// whole argv element after -- (INJ-14). The manifest is checked again at start: a command
// with a forbidden program or a parameter inside an element does not run.
type Cmd struct {
	Plugin toolspec.Plugin
	// Host is the catalog machine this plugin runs on; a call for another machine is refused.
	Host string
	// MaxOutput bounds what a command may print; the toolhost cuts further by the tool limit.
	MaxOutput int
}

// NewCmd checks the manifest of the plugin with the forbidden programs.
func NewCmd(p toolspec.Plugin, host string, forbidden map[string]bool) (*Cmd, error) {
	if issues := toolspec.Check(p, forbidden); len(issues) > 0 {
		return nil, fmt.Errorf("the cmd manifest does not pass the checks: %v", issues)
	}
	return &Cmd{Plugin: p, Host: host, MaxOutput: 1 << 20}, nil
}

// Call runs one command.
func (c *Cmd) Call(ctx context.Context, tool string, args map[string]any, meta *mcp.Meta) (*mcp.CallResult, error) {
	t, _ := c.Plugin.Tool(tool)
	if h, ok := args["host"].(string); ok && c.Host != "" && h != c.Host {
		return nil, errors.New("the call is for another machine")
	}
	name, err := argString(args, "command")
	if err != nil {
		return nil, err
	}
	cmd, ok := t.Commands[name]
	if !ok {
		return nil, errors.New("unknown command")
	}
	argv, err := expand(cmd, args, meta)
	if err != nil {
		return nil, err
	}
	timeout := 20 * time.Second
	if t.TimeoutS > 0 {
		timeout = time.Duration(t.TimeoutS) * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	x := exec.CommandContext(ctx, argv[0], argv[1:]...)
	x.Env = CmdEnv
	x.Dir = "/"
	out := &limitBuffer{max: c.MaxOutput}
	x.Stdout, x.Stderr = out, out
	runErr := x.Run()
	if ctx.Err() != nil {
		return nil, errors.New("the command did not finish in time")
	}
	text := out.String()
	var ee *exec.ExitError
	switch {
	case errors.As(runErr, &ee):
		// A nonzero exit is an answer, not a call error: systemctl status of a stopped service
		// exits 3.
		text += fmt.Sprintf("\n[exit status %d]", ee.ExitCode())
	case runErr != nil:
		return nil, errors.New("the command could not start")
	}
	if out.cut {
		text += "\n[output cut]"
	}
	return Text(text), nil
}

// expand builds the argv of a command: a placeholder element {param} takes the argument value,
// {param.field} the catalog field the core resolved for the argument (impl/README.md). An
// element is replaced whole, never inside a string, so a value cannot become an option or a
// second argument.
func expand(cmd toolspec.Command, args map[string]any, meta *mcp.Meta) ([]string, error) {
	argv := make([]string, len(cmd.Argv))
	for i, a := range cmd.Argv {
		if !strings.Contains(a, "{") {
			argv[i] = a
			continue
		}
		if !toolspec.IsPlaceholder(a) {
			return nil, errors.New("a parameter inside an argv element")
		}
		ref := strings.Trim(a, "{}")
		var v string
		if strings.Contains(ref, ".") {
			if meta != nil {
				v = meta.Resolved[ref]
			}
		} else {
			s, ok := args[ref].(string)
			if !ok {
				return nil, errors.New("missing argument " + ref)
			}
			v = s
		}
		if v == "" || strings.HasPrefix(v, "-") || strings.ContainsAny(v, "\x00\n") {
			return nil, errors.New("the value of " + ref + " cannot be an argument")
		}
		argv[i] = v
	}
	return argv, nil
}

// limitBuffer keeps at most max bytes and drops the rest.
type limitBuffer struct {
	bytes.Buffer
	max int
	cut bool
}

func (b *limitBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if room := b.max - b.Len(); room < len(p) {
		if room > 0 {
			b.Buffer.Write(p[:room])
		}
		b.cut = true
		return n, nil
	}
	b.Buffer.Write(p)
	return n, nil
}
