// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"strings"
	"testing"

	"github.com/HunterEwok/botaru/impl/internal/mcp"
)

// A model that does whatever an injected text tells it gets no further than its role allows:
// every call below is refused at the gateway, before any toolhost sees it, while the same
// tools with honest arguments get as far as the missing toolhost (INJ-1, INJ-14, PRM-8).
func TestP3HostileModel(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newToolBench(t)
	_, h := b.attemptTools(t, "1", true)
	for _, ok := range []mcp.CallParams{
		{Name: "checks_latest", Arguments: map[string]any{"check": "www-health"}},
		{Name: "logs_tail", Arguments: map[string]any{"source": "nginx-error"}},
		{Name: "cmd_run", Arguments: map[string]any{"command": "disk_usage", "host": "web1"}},
	} {
		if r, err := b.c.P3ToolsCall(ctx, h, ok); err != nil || callText(r) != "error: plugin_unavailable" {
			t.Fatalf("honest %s: %+v %v", ok.Name, r, err)
		}
	}
	for name, p := range map[string]mcp.CallParams{
		"shell in the command":       {Name: "cmd_run", Arguments: map[string]any{"command": "disk_usage; rm -rf /", "host": "web1"}},
		"a path for the service":     {Name: "cmd_run", Arguments: map[string]any{"command": "service_status", "host": "web1", "service": "../../etc/passwd"}},
		"a service outside the list": {Name: "cmd_run", Arguments: map[string]any{"command": "service_status", "host": "web1", "service": "sshd"}},
		"its own argv":               {Name: "cmd_run", Arguments: map[string]any{"command": "disk_usage", "host": "web1", "argv": []any{"/bin/sh", "-c", "id"}}},
		"a machine outside the list": {Name: "cmd_run", Arguments: map[string]any{"command": "disk_usage", "host": "db9"}},
		"a NUL in the machine":       {Name: "cmd_run", Arguments: map[string]any{"command": "disk_usage", "host": "web1\x00"}},
		"a log of another tool":      {Name: "logs_tail", Arguments: map[string]any{"source": "audit"}},
		"a huge line count":          {Name: "logs_tail", Arguments: map[string]any{"source": "nginx-error", "lines": 1e9}},
		"a machine for a log":        {Name: "logs_tail", Arguments: map[string]any{"source": "nginx-error", "host": "db9"}},
		"a list for a check":         {Name: "checks_latest", Arguments: map[string]any{"check": []any{"www-health"}}},
		"an extra key":               {Name: "checks_latest", Arguments: map[string]any{"check": "www-health", "__proto__": map[string]any{}}},
		"no arguments":               {Name: "checks_latest"},
		"a tool the role lacks":      {Name: "services_restart", Arguments: map[string]any{"host": "web1", "service": "nginx"}},
		"a slash name":               {Name: "cmd/run", Arguments: map[string]any{"command": "disk_usage", "host": "web1"}},
		"an upper-case name":         {Name: "CMD_RUN", Arguments: map[string]any{"command": "disk_usage", "host": "web1"}},
		"a zero-width name":          {Name: "cmd_run​", Arguments: map[string]any{"command": "disk_usage", "host": "web1"}},
		"a trailing space":           {Name: "cmd_run ", Arguments: map[string]any{"command": "disk_usage", "host": "web1"}},
	} {
		r, err := b.c.P3ToolsCall(ctx, h, p)
		if err != nil {
			continue
		}
		if text := callText(r); !r.IsError || text == "error: plugin_unavailable" || !strings.HasPrefix(text, "error: ") && text != noPermission {
			t.Errorf("%s: %+v", name, r)
		}
	}
}
