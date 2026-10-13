// SPDX-License-Identifier: AGPL-3.0-only

// Package p1client is the worker side of the HTTP binding v1: POST /v1/p1/<operation> with the
// worker token, and the lease token in the Botaru-Lease header for calls under a lease.
package p1client

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client calls one core.
type Client struct {
	BaseURL string // for example http://127.0.0.1:8401
	Token   string
	HTTP    *http.Client
}

// Error is a refusal of the core: problem details with a reason code.
type Error struct {
	Status      int      `json:"status"`
	Reason      string   `json:"reason"`
	Detail      string   `json:"detail"`
	RetryAfterS int      `json:"retry_after_s"`
	Needs       []string `json:"needs"`
}

func (e *Error) Error() string {
	if e.Detail != "" {
		return fmt.Sprintf("%s (%d): %s", e.Reason, e.Status, e.Detail)
	}
	return fmt.Sprintf("%s (%d)", e.Reason, e.Status)
}

// Reason returns the reason code of a core refusal, or "".
func Reason(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Reason
	}
	return ""
}

// LeaseHeader turns the token of a work item into the header value: base64url without padding
// of the canonical form, as received. The core writes the token in canonical form already, so
// the raw JSON of the token field is passed on unchanged.
func LeaseHeader(token json.RawMessage) string {
	var buf bytes.Buffer
	if err := json.Compact(&buf, token); err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(buf.Bytes())
}

// Call posts a request to /v1/p1/<op> and decodes the answer into out.
func (c *Client) Call(ctx context.Context, op, lease string, req, out any) error {
	return c.post(ctx, "/v1/p1/"+op, lease, req, out)
}

func (c *Client) post(ctx context.Context, path, lease string, req, out any) error {
	if req == nil {
		req = map[string]any{}
	}
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.BaseURL, "/")+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	hr.Header.Set("Content-Type", "application/json")
	hr.Header.Set("Authorization", "Bearer "+c.Token)
	if lease != "" {
		hr.Header.Set("Botaru-Lease", lease)
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 90 * time.Second}
	}
	resp, err := hc.Do(hr)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		e := &Error{Status: resp.StatusCode}
		if json.Unmarshal(b, e) != nil || e.Reason == "" {
			e.Reason = "not_ready"
			e.Detail = strings.TrimSpace(string(b))
		}
		return e
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(b, out)
}

// WorkItem is a work item of claim, with the token kept as received.
type WorkItem struct {
	WorkID          string          `json:"work_id"`
	Kind            string          `json:"kind"`
	LeaseNo         int             `json:"lease_no"`
	Tenant          string          `json:"tenant"`
	LeaseRemainingS int             `json:"lease_remaining_s"`
	Token           json.RawMessage `json:"token"`
	Payload         json.RawMessage `json:"payload"`
}

// Lease returns the Botaru-Lease value of the item.
func (w *WorkItem) Lease() string { return LeaseHeader(w.Token) }

// Claim asks for work of the given kinds with long polling.
func (c *Client) Claim(ctx context.Context, kinds []string, waitS int) (*WorkItem, error) {
	var out struct {
		Work *WorkItem `json:"work"`
	}
	err := c.Call(ctx, "claim", "", map[string]any{"kinds": kinds, "wait_s": waitS}, &out)
	return out.Work, err
}

// Heartbeat extends a lease and reports whether cancellation was requested.
func (c *Client) Heartbeat(ctx context.Context, lease string, req map[string]any) (bool, error) {
	var out struct {
		CancelRequested bool `json:"cancel_requested"`
	}
	err := c.Call(ctx, "heartbeat", lease, req, &out)
	return out.CancelRequested, err
}
