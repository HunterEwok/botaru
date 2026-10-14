// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"encoding/json"
	"net/http"

	"github.com/HunterEwok/botaru/impl/internal/core"
	"github.com/HunterEwok/botaru/impl/internal/mcp"
)

// p3 serves /v1/p3/mcp: sessionless MCP over HTTP, one JSON-RPC 2.0 message in the body
// (spec/bindings/mcp/README.md, gateway profile). The tool handle is in the authorization
// header of every request (SEC-3); a refused handle is a problem details answer of the HTTP
// binding, everything else a JSON-RPC answer.
func (s *Server) p3(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v1/p3/mcp" || r.Method != http.MethodPost {
		Problem(w, &core.Problem{Reason: "not_found", Detail: "the binding opens POST /v1/p3/mcp"})
		return
	}
	m, ok := s.body(w, r, "")
	if !ok {
		return
	}
	msg, err := decode[mcp.Message](m)
	if err != nil || msg.Method == "" {
		writeJSON(w, mcp.ErrorResponse(nil, mcp.InvalidRequest, "not a JSON-RPC request"))
		return
	}
	ctx := r.Context()
	handle := bearer(r)
	if len(msg.ID) == 0 {
		// A notification: cancellation stops nothing but the runtime's own wait, which ends with
		// its connection (P3, FEN-4); the handle is still checked.
		if _, err := s.c.P3Discover(ctx, handle); err != nil {
			s.fail(w, err)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		return
	}
	var result any
	switch msg.Method {
	case "server/discover":
		result, err = s.c.P3Discover(ctx, handle)
	case "tools/list":
		result, err = s.c.P3ToolsList(ctx, handle)
	case "tools/call":
		var p mcp.CallParams
		if len(msg.Params) == 0 || json.Unmarshal(msg.Params, &p) != nil || p.Name == "" {
			if _, err := s.c.P3Discover(ctx, handle); err != nil {
				s.fail(w, err)
				return
			}
			writeJSON(w, mcp.ErrorResponse(msg.ID, mcp.InvalidParams, "tools/call needs a name and arguments"))
			return
		}
		result, err = s.c.P3ToolsCall(ctx, handle, p)
	default:
		if _, err := s.c.P3Discover(ctx, handle); err != nil {
			s.fail(w, err)
			return
		}
		writeJSON(w, mcp.ErrorResponse(msg.ID, mcp.MethodNotFound, "the gateway profile has server/discover, tools/list and tools/call"))
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, mcp.Response(msg.ID, result))
}
