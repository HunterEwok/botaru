// SPDX-License-Identifier: AGPL-3.0-only

// Package plugins holds the own plugins of this implementation: cmd (named commands without a
// shell, INJ-14), logs (the tail of catalog log sources) and checks (the latest site check
// results). Each is an MCP server on the standard streams of a child process of the toolhost
// (spec/bindings/mcp/README.md, plugin profile), without secrets and without network (DEP-7).
// Its tools/list is derived from its own manifest, so the toolhost's fingerprint matches the
// manifest the core applied (INJ-2).
package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"

	"github.com/HunterEwok/botaru/impl/internal/mcp"
	"github.com/HunterEwok/botaru/impl/internal/toolspec"
	"github.com/HunterEwok/botaru/impl/internal/version"
)

// maxMessage bounds one request a plugin reads.
const maxMessage = 1 << 20

// Handler runs one tool call.
type Handler func(ctx context.Context, tool string, args map[string]any, meta *mcp.Meta) (*mcp.CallResult, error)

// Serve runs the MCP server of a plugin on r and w until r ends. Calls run one at a time; a
// notifications/cancelled for the running call cancels its context.
func Serve(ctx context.Context, p toolspec.Plugin, r io.Reader, w io.Writer, h Handler) error {
	conn := mcp.NewConn(r, w)
	var (
		mu      sync.Mutex
		running string
		cancel  context.CancelFunc
		wg      sync.WaitGroup
	)
	defer wg.Wait()
	for {
		m, err := conn.Receive(maxMessage)
		if errors.Is(err, io.EOF) {
			mu.Lock()
			if cancel != nil {
				cancel()
			}
			mu.Unlock()
			return nil
		}
		if err != nil {
			_ = conn.Send(mcp.ErrorResponse(nil, mcp.ParseError, "the message could not be read"))
			continue
		}
		if len(m.ID) == 0 {
			if m.Method == "notifications/cancelled" {
				var cp mcp.CancelParams
				_ = json.Unmarshal(m.Params, &cp)
				mu.Lock()
				if cancel != nil && string(cp.RequestID) == running {
					cancel()
				}
				mu.Unlock()
			}
			continue
		}
		switch m.Method {
		case "server/discover", "initialize":
			_ = conn.Send(mcp.Response(m.ID, mcp.DiscoverResult{
				ProtocolVersion: mcp.Revision, Capabilities: map[string]any{"tools": map[string]any{}},
				ServerInfo: map[string]any{"name": "botaru-plugin-" + p.Name, "version": version.Version},
			}))
		case "tools/list":
			_ = conn.Send(mcp.Response(m.ID, mcp.ListResult{Tools: p.LiveTools()}))
		case "tools/call":
			var cp mcp.CallParams
			if err := json.Unmarshal(m.Params, &cp); err != nil {
				_ = conn.Send(mcp.ErrorResponse(m.ID, mcp.InvalidParams, "bad params"))
				continue
			}
			t, ok := p.Tool(cp.Name)
			if !ok || !t.On() {
				_ = conn.Send(mcp.ErrorResponse(m.ID, mcp.InvalidParams, "no such tool"))
				continue
			}
			callCtx, c := context.WithCancel(ctx)
			mu.Lock()
			running, cancel = string(m.ID), c
			mu.Unlock()
			wg.Add(1)
			go func(id json.RawMessage) {
				defer wg.Done()
				defer c()
				res, err := h(callCtx, cp.Name, cp.Arguments, cp.Meta)
				if err != nil {
					res = &mcp.CallResult{IsError: true, Content: []mcp.Content{{Type: "text", Text: err.Error()}}}
				}
				_ = conn.Send(mcp.Response(id, res))
				mu.Lock()
				if running == string(id) {
					running, cancel = "", nil
				}
				mu.Unlock()
			}(m.ID)
		default:
			_ = conn.Send(mcp.ErrorResponse(m.ID, mcp.MethodNotFound, "not served by this plugin"))
		}
	}
}

// Text returns a result of one text block.
func Text(s string) *mcp.CallResult {
	return &mcp.CallResult{Content: []mcp.Content{{Type: "text", Text: s}}}
}

func argString(args map[string]any, name string) (string, error) {
	v, ok := args[name].(string)
	if !ok || v == "" {
		return "", errors.New("missing argument " + name)
	}
	return v, nil
}
