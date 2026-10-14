// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/HunterEwok/botaru/impl/internal/mcp"
	"github.com/HunterEwok/botaru/impl/internal/version"
)

// toolsClient is the runtime side of P3: sessionless MCP over HTTP with the tools handle in the
// authorization header (SEC-3). The handle never reaches the model.
type toolsClient struct {
	url    string
	handle string
	http   *http.Client
	id     atomic.Int64
}

// p3Error is a refusal of the P3 endpoint with its reason code.
type p3Error struct {
	method string
	status int
	reason string
}

func (e *p3Error) Error() string { return fmt.Sprintf("p3 %s: %d %s", e.method, e.status, e.reason) }

// p3Reason returns the reason code of a P3 refusal, or "".
func p3Reason(err error) string {
	var e *p3Error
	if errors.As(err, &e) {
		return e.reason
	}
	return ""
}

func (t *toolsClient) rpc(ctx context.Context, method string, params any) (json.RawMessage, error) {
	p, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	id := json.RawMessage(fmt.Sprint(t.id.Add(1)))
	body, err := json.Marshal(mcp.Message{JSONRPC: "2.0", ID: id, Method: method, Params: p})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+t.handle)
	resp, err := t.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		var prob struct {
			Reason string `json:"reason"`
		}
		_ = json.Unmarshal(b, &prob)
		return nil, &p3Error{method: method, status: resp.StatusCode, reason: prob.Reason}
	}
	var m mcp.Message
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	if m.Error != nil {
		return nil, m.Error
	}
	return m.Result, nil
}

func meta() *mcp.Meta { return mcp.ClientMeta("botaru-agent", version.Version) }

// list reads the P3 catalog and turns it into model tools with the schemas unchanged, so that
// P4 finds the same tool hashes (GW-1).
func (t *toolsClient) list(ctx context.Context) ([]anthropic.ToolUnionParam, error) {
	raw, err := t.rpc(ctx, "tools/list", mcp.ListParams{Meta: meta()})
	if err != nil {
		return nil, err
	}
	var res struct {
		Tools []struct {
			Name        string         `json:"name"`
			Description string         `json:"description"`
			InputSchema map[string]any `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, err
	}
	var out []anthropic.ToolUnionParam
	for _, x := range res.Tools {
		if x.InputSchema["type"] != "object" {
			return nil, errors.New("a P3 tool schema is not an object")
		}
		schema := anthropic.ToolInputSchemaParam{ExtraFields: map[string]any{}}
		for k, v := range x.InputSchema {
			switch k {
			case "type":
			case "properties":
				schema.Properties = v
			case "required":
				for _, r := range v.([]any) {
					s, _ := r.(string)
					schema.Required = append(schema.Required, s)
				}
			default:
				schema.ExtraFields[k] = v
			}
		}
		tp := anthropic.ToolParam{Name: x.Name, InputSchema: schema}
		if x.Description != "" {
			tp.Description = anthropic.String(x.Description)
		}
		out = append(out, anthropic.ToolUnionParam{OfTool: &tp})
	}
	return out, nil
}

// call runs one tool call and returns its text and whether it is an error.
func (t *toolsClient) call(ctx context.Context, name string, args map[string]any) (string, bool, error) {
	if args == nil {
		args = map[string]any{}
	}
	raw, err := t.rpc(ctx, "tools/call", mcp.CallParams{Name: name, Arguments: args, Meta: meta()})
	if err != nil {
		return "", false, err
	}
	var res mcp.CallResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return "", false, err
	}
	var text strings.Builder
	for _, c := range res.Content {
		text.WriteString(c.Text)
	}
	return text.String(), res.IsError, nil
}
