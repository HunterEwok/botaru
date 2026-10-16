// SPDX-License-Identifier: AGPL-3.0-only

package plugins

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/HunterEwok/botaru/impl/internal/mcp"
)

// INJ-14: a command runs with a cleared environment (nothing from the toolhost reaches it),
// and a value with shell syntax stays one whole argv element after --, never a command.
func TestBackfillCmdEnvAndArgv(t *testing.T) {
	if _, err := os.Stat("/usr/bin/printenv"); err != nil {
		t.Fatal("the test needs /usr/bin/printenv")
	}
	t.Setenv("BOTARU_TOOLHOST_SECRET", "canary-7f3a")
	c, err := NewCmd(cmdPlugin("/usr/bin/printenv"), "", map[string]bool{"sh": true, "env": true})
	if err != nil {
		t.Fatal(err)
	}
	r, err := c.Call(context.Background(), "run", map[string]any{"command": "say"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(r.Content[0].Text), "\n")
	slices.Sort(lines)
	want := slices.Clone(CmdEnv)
	slices.Sort(want)
	if !slices.Equal(lines, want) {
		t.Fatalf("INJ-14: the environment of a command is %v, want only %v", lines, want)
	}
	e, err := NewCmd(cmdPlugin("/bin/echo", "--", "{service.unit}"), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	meta := &mcp.Meta{Resolved: map[string]string{"service.unit": "x;id$(id)"}}
	r, err = e.Call(context.Background(), "run", map[string]any{"command": "say", "service": "nginx"}, meta)
	if err != nil || r.Content[0].Text != "-- x;id$(id)\n" {
		t.Fatalf("INJ-14: a value with shell syntax: %+v %v", r, err)
	}
}
