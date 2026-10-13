// SPDX-License-Identifier: AGPL-3.0-only

// Package mockprovider is the mock model provider of the test bench
// (spec/bindings/model-providers/mock.md): it answers POST /v1/messages in the Anthropic format
// by a recorded scenario and records what it received. It decides nothing for the core.
package mockprovider

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/HunterEwok/botaru/impl/internal/yamlx"
)

// Scenario is a mock model scenario file.
type Scenario struct {
	ID          string `yaml:"id"`
	Description string `yaml:"description"`
	Profile     string `yaml:"profile"`
	Turns       []Turn `yaml:"turns"`
}

// Turn is one expected call and its answer.
type Turn struct {
	ExpectRequest struct {
		Slot                string   `yaml:"slot"`
		Tools               []string `yaml:"tools"`
		InstructionContains string   `yaml:"instruction_contains"`
		CachePoints         *int     `yaml:"cache_points"`
	} `yaml:"expect_request"`
	Respond struct {
		StopReason string         `yaml:"stop_reason"`
		Result     map[string]any `yaml:"result"`
		Text       string         `yaml:"text"`
		ToolUse    *struct {
			Name  string         `yaml:"name"`
			Input map[string]any `yaml:"input"`
		} `yaml:"tool_use"`
		Usage map[string]int64 `yaml:"usage"`
	} `yaml:"respond"`
	Fault *struct {
		Kind        string `yaml:"kind"`
		Status      int    `yaml:"status"`
		ErrorType   string `yaml:"error_type"`
		RetryAfterS int    `yaml:"retry_after_s"`
		DelayS      int    `yaml:"delay_s"`
	} `yaml:"fault"`
}

// Load reads a scenario file.
func Load(path string) (*Scenario, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s Scenario
	if err := yamlx.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &s, nil
}

// Call is what the mock provider records of an accepted call; the whole body is not stored.
type Call struct {
	Time        time.Time
	KeyHash     string
	Model       string
	Region      string
	CachePoints int
	Tools       []string
	BodyHash    string
}

// Server plays a scenario. Each call takes the next turn; after the last turn the last turn
// repeats, so that a long-running bench keeps answering.
type Server struct {
	mu    sync.Mutex
	sc    *Scenario
	next  int
	calls []Call
}

// New returns a server for a scenario.
func New(sc *Scenario) *Server { return &Server{sc: sc} }

// Calls returns the recorded calls.
func (s *Server) Calls() []Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Call(nil), s.calls...)
}

func apiError(w http.ResponseWriter, status int, typ, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("request-id", "req_mock_error")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"type": "error", "error": map[string]any{"type": typ, "message": msg}})
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v1/messages" || r.Method != http.MethodPost {
		apiError(w, 404, "not_found_error", "no such path")
		return
	}
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		apiError(w, 400, "invalid_request_error", "body")
		return
	}
	var req struct {
		Model        string            `json:"model"`
		System       json.RawMessage   `json:"system"`
		Tools        []json.RawMessage `json:"tools"`
		InferenceGeo string            `json:"inference_geo"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		apiError(w, 400, "invalid_request_error", "json")
		return
	}
	var tools []string
	for _, t := range req.Tools {
		var n struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(t, &n)
		tools = append(tools, n.Name)
	}
	system := systemText(req.System)
	kh := sha256.Sum256([]byte(r.Header.Get("x-api-key")))
	bh := sha256.Sum256(raw)
	s.mu.Lock()
	if len(s.sc.Turns) == 0 {
		s.mu.Unlock()
		apiError(w, 400, "invalid_request_error", "the scenario has no turns")
		return
	}
	i := min(s.next, len(s.sc.Turns)-1)
	s.next++
	turn := s.sc.Turns[i]
	s.calls = append(s.calls, Call{Time: time.Now(), KeyHash: hex.EncodeToString(kh[:8]), Model: req.Model, Region: req.InferenceGeo,
		CachePoints: strings.Count(string(raw), `"cache_control"`), Tools: tools, BodyHash: "sha256:" + hex.EncodeToString(bh[:])})
	s.mu.Unlock()

	// A mismatch means that P4 let through what it should have rejected or changed.
	if want := turn.ExpectRequest.Tools; want != nil && strings.Join(want, ",") != strings.Join(tools, ",") {
		apiError(w, 400, "invalid_request_error", "tools differ from the scenario")
		return
	}
	if c := turn.ExpectRequest.InstructionContains; c != "" && !strings.Contains(system, c) {
		apiError(w, 400, "invalid_request_error", "system does not contain the expected instruction")
		return
	}
	if f := turn.Fault; f != nil {
		switch f.Kind {
		case "status":
			if f.RetryAfterS > 0 {
				w.Header().Set("retry-after", strconv.Itoa(f.RetryAfterS))
			}
			apiError(w, f.Status, f.ErrorType, "mock fault")
			return
		case "drop_after_body":
			w.Header().Set("request-id", "req_mock_drop")
			if hj, ok := w.(http.Hijacker); ok {
				if conn, _, err := hj.Hijack(); err == nil {
					_ = conn.Close()
					return
				}
			}
			panic(http.ErrAbortHandler)
		case "delay":
			select {
			case <-time.After(time.Duration(f.DelayS) * time.Second):
			case <-r.Context().Done():
				return
			}
		case "spend_cap":
			apiError(w, 402, "billing_error", "workspace spend cap reached")
			return
		}
	}
	var content []map[string]any
	switch {
	case turn.Respond.Result != nil:
		b, _ := json.Marshal(turn.Respond.Result)
		content = append(content, map[string]any{"type": "text", "text": string(b)})
	case turn.Respond.ToolUse != nil:
		content = append(content, map[string]any{"type": "tool_use", "id": "toolu_mock_" + strconv.Itoa(i), "name": turn.Respond.ToolUse.Name, "input": turn.Respond.ToolUse.Input})
	default:
		content = append(content, map[string]any{"type": "text", "text": turn.Respond.Text})
	}
	stop := turn.Respond.StopReason
	if stop == "" {
		stop = "end_turn"
	}
	u := turn.Respond.Usage
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("request-id", "req_mock_"+strconv.Itoa(i))
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id": "msg_mock_" + strconv.Itoa(i), "type": "message", "role": "assistant", "model": req.Model,
		"content": content, "stop_reason": stop, "stop_sequence": nil,
		"usage": map[string]any{
			"input_tokens": u["input_tokens"], "output_tokens": u["output_tokens"],
			"cache_creation_input_tokens": u["cache_creation_input_tokens"], "cache_read_input_tokens": u["cache_read_input_tokens"],
		},
	})
}

func systemText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal(raw, &blocks)
	var sb strings.Builder
	for _, b := range blocks {
		sb.WriteString(b.Text)
		sb.WriteString("\n")
	}
	return sb.String()
}
