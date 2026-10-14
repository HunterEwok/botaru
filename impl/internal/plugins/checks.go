// SPDX-License-Identifier: AGPL-3.0-only

package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"

	"github.com/HunterEwok/botaru/impl/internal/mcp"
)

// Checks is the site check plugin: latest gives the counts of the latest results of a catalog
// check from a results file on this machine, written by the checker that runs the checks
// (impl/DEPLOY.md). The output is numbers only, so the tool declares untrusted_output: false,
// and the core checks it against the tool's output_schema.
type Checks struct {
	Results string // path of the results file: {"<check>": {"ok": n, "total": n, "last_status": n, "ms_max": n}}
}

// Call runs latest.
func (c *Checks) Call(ctx context.Context, tool string, args map[string]any, _ *mcp.Meta) (*mcp.CallResult, error) {
	if tool != "latest" {
		return nil, errors.New("unknown tool")
	}
	check, err := argString(args, "check")
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(c.Results)
	if err != nil {
		return nil, errors.New("the results cannot be read")
	}
	var all map[string]map[string]any
	if err := json.Unmarshal(b, &all); err != nil {
		return nil, errors.New("the results are not valid")
	}
	r, ok := all[check]
	if !ok {
		return nil, errors.New("no results for the check yet")
	}
	out := map[string]any{"check": check}
	for _, f := range []struct {
		name string
		max  float64
	}{{"ok", 1000}, {"total", 1000}, {"last_status", 599}, {"ms_max", 60000}} {
		v, ok := r[f.name].(float64)
		if !ok {
			continue
		}
		if v < 0 || v > f.max || v != math.Trunc(v) {
			return nil, errors.New("the results are not valid")
		}
		out[f.name] = int(v)
	}
	if _, ok := out["ok"]; !ok {
		return nil, errors.New("the results are not valid")
	}
	if _, ok := out["total"]; !ok {
		return nil, errors.New("the results are not valid")
	}
	text, _ := json.Marshal(out)
	return &mcp.CallResult{Content: []mcp.Content{{Type: "text", Text: string(text)}}, StructuredContent: out}, nil
}
