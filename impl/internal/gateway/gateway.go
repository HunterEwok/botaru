// SPDX-License-Identifier: AGPL-3.0-only

// Package gateway is P4, the model gateway, with the Anthropic Messages binding
// (spec/bindings/model-providers/anthropic.md). It is not a transparent proxy: it checks the
// request against the allowlist, sets the model of the handle's slot, the output limit, the
// region and the cache point, reserves the worst case before the call and charges the actual
// usage after it. Only P4 holds the provider key.
package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/HunterEwok/botaru/impl/internal/core"
	"github.com/HunterEwok/botaru/impl/internal/ijson"
	"github.com/HunterEwok/botaru/impl/internal/manifest"
)

// AnthropicURL is the only provider address of the binding outside the test bench.
const AnthropicURL = "https://api.anthropic.com"

// Provider is a provider name of the model profiles mapped to a binding, an address and the
// key of each tenant's workspace.
type Provider struct {
	Binding string            `yaml:"binding"`
	BaseURL string            `yaml:"base_url"`
	Keys    map[string]string `yaml:"-"` // tenant -> key, filled by the deployment from secrets
}

// Config is the P4 part of the deployment settings.
type Config struct {
	Providers map[string]Provider
	TestBench bool
	Client    *http.Client
	Logger    *slog.Logger
}

// Gateway serves /v1/p4/<binding>/<path>.
type Gateway struct {
	c   *core.Core
	cfg Config
	log *slog.Logger
}

// New checks the provider addresses: another address than the binding's own is allowed only
// on the test bench (GW-3).
func New(c *core.Core, cfg Config) (*Gateway, error) {
	for name, p := range cfg.Providers {
		if p.Binding != "anthropic" {
			return nil, errors.New("gateway: provider " + name + ": unknown binding " + p.Binding)
		}
		if p.BaseURL == "" {
			p.BaseURL = AnthropicURL
			cfg.Providers[name] = p
		}
		if strings.TrimRight(p.BaseURL, "/") != AnthropicURL && !cfg.TestBench {
			return nil, errors.New("gateway: provider " + name + ": an address outside the binding list is allowed only on the test bench")
		}
	}
	if cfg.Client == nil {
		cfg.Client = &http.Client{}
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Gateway{c: c, cfg: cfg, log: cfg.Logger}, nil
}

func problem(w http.ResponseWriter, reason, detail string, retryAfter int) {
	status := map[string]int{
		"input_schema_invalid": 400, "handle_invalid": 401, "not_found": 404, "fenced": 409, "rank_exceeded": 403,
		"budget_exhausted": 422, "model_error": 422, "provider_rate_limited": 429, "provider_quota": 429,
		"provider_unavailable": 503, "not_ready": 503, "store_unavailable": 503,
		"provider_config": 502, "provider_spend_cap": 502,
	}[reason]
	if status == 0 {
		status = 422
	}
	body := map[string]any{"type": "urn:botaru:reason:" + reason, "title": reason, "status": status, "reason": reason}
	if detail != "" {
		body["detail"] = detail
	}
	if retryAfter > 0 {
		body["retry_after_s"] = retryAfter
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
	}
	b, _ := json.Marshal(body)
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

func (g *Gateway) refuse(w http.ResponseWriter, err error) {
	if p, ok := core.AsProblem(err); ok {
		problem(w, p.Reason, p.Detail, p.RetryAfterS)
		return
	}
	g.log.Error("p4 failed", "err", err)
	problem(w, "store_unavailable", "internal error", 5)
}

// ServeHTTP is the P4 entry on the P1 listener.
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// The binding opens one path and method; anything else is refused before the provider.
	if r.URL.Path != "/v1/p4/anthropic/v1/messages" || r.Method != http.MethodPost {
		problem(w, "not_found", "the binding does not open this path", 0)
		return
	}
	ctx := r.Context()
	handle, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	grant, err := g.c.ResolveModelHandle(ctx, strings.TrimSpace(handle))
	if err != nil {
		g.refuse(w, err)
		return
	}
	prov, ok := g.cfg.Providers[grant.Profile.Provider]
	if !ok || prov.Binding != "anthropic" {
		problem(w, "provider_config", "the provider of the profile has no anthropic binding in this deployment", 0)
		return
	}
	key := prov.Keys[grant.Tenant]
	if key == "" {
		problem(w, "provider_config", "no provider key for the tenant's workspace", 0)
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 32<<20))
	if err != nil {
		problem(w, "input_schema_invalid", "the body could not be read", 0)
		return
	}
	body, err := ijson.ParseObject(raw)
	if err != nil {
		problem(w, "input_schema_invalid", err.Error(), 0)
		return
	}
	if err := g.c.Spec().Validate("p4-anthropic-messages-request", body); err != nil {
		problem(w, "input_schema_invalid", "the request is outside the allowlist", 0)
		return
	}
	out, maxTokens, err := g.providerBody(body, grant)
	if err != nil {
		g.refuse(w, err)
		return
	}
	payload, err := json.Marshal(out)
	if err != nil {
		g.refuse(w, err)
		return
	}
	reserve := Reserve(grant.Profile, len(payload), maxTokens)
	spendID, err := g.c.Reserve(ctx, grant, reserve)
	if err != nil {
		g.refuse(w, err)
		return
	}
	// The provider call does not depend on the runtime connection: a dropped runtime does not
	// break reading the answer, so the usage is still charged (GW-2, BUD-11).
	callCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), deadline(grant.Profile))
	defer cancel()
	resp, sent, err := g.send(callCtx, prov, key, payload)
	if err != nil {
		if !sent {
			_ = g.c.DropReserve(context.WithoutCancel(ctx), spendID)
		} else {
			_ = g.c.Charge(context.WithoutCancel(ctx), spendID, reserve, nil, "", true)
		}
		problem(w, "provider_unavailable", "the provider did not answer", 5)
		return
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	requestID := resp.Header.Get("request-id")
	if err != nil {
		_ = g.c.Charge(context.WithoutCancel(ctx), spendID, reserve, nil, requestID, true)
		problem(w, "provider_unavailable", "the provider answer broke off", 5)
		return
	}
	if resp.StatusCode != http.StatusOK {
		_ = g.c.Charge(context.WithoutCancel(ctx), spendID, 0, &core.Tokens{}, requestID, false)
		reason, retry := mapError(resp)
		problem(w, reason, "provider answered "+strconv.Itoa(resp.StatusCode), retry)
		return
	}
	var parsed struct {
		Usage *Usage `json:"usage"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil || parsed.Usage == nil {
		_ = g.c.Charge(context.WithoutCancel(ctx), spendID, reserve, nil, requestID, true)
		problem(w, "model_error", "the provider answer has no usage", 0)
		return
	}
	cost, tokens := Cost(grant.Profile.Price, *parsed.Usage)
	if err := g.c.Charge(context.WithoutCancel(ctx), spendID, cost, tokens, requestID, false); err != nil {
		g.log.Error("charging a model call failed", "err", err)
	}
	// The request-id stays in the usage line, not in the answer to the runtime.
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(respBody)
}

// providerBody builds the body of the provider call from a checked request: metadata dropped,
// the model of the slot profile, the output limit, the region and the cache point set by P4.
func (g *Gateway) providerBody(req map[string]any, grant *core.ModelGrant) (map[string]any, int, error) {
	p := grant.Profile
	out := map[string]any{}
	for k, v := range req {
		if k == "metadata" {
			continue // accepted and dropped (D-87 (4))
		}
		out[k] = v
	}
	if s, _ := out["stream"].(bool); s {
		return nil, 0, &core.Problem{Reason: "input_schema_invalid", Detail: "streaming is not served by this build"}
	}
	delete(out, "stream")
	// Tools must equal the P3 catalog of the handle; v0.1 attempts of this build have none.
	if tools, ok := out["tools"].([]any); ok && len(tools) > 0 {
		return nil, 0, &core.Problem{Reason: "input_schema_invalid", Detail: "tools differ from the P3 catalog of the handle"}
	}
	delete(out, "tools")
	out["model"] = p.Model
	maxTokens := int(toInt(req["max_tokens"]))
	if p.MaxOutputTokens > 0 && maxTokens > p.MaxOutputTokens {
		maxTokens = p.MaxOutputTokens
	}
	out["max_tokens"] = maxTokens
	if th, ok := out["thinking"].(map[string]any); ok {
		if b := toInt(th["budget_tokens"]); b > 0 && int(b) >= maxTokens {
			return nil, 0, &core.Problem{Reason: "input_schema_invalid", Detail: "thinking.budget_tokens is not below max_tokens"}
		}
	}
	// Region: only P4 sets it, from the tenant's subprocessor record (GW-14).
	switch region := grant.Subprocessor.Region; region {
	case "us", "global":
		out["inference_geo"] = region
	default:
		if !g.cfg.TestBench {
			return nil, 0, &core.Problem{Reason: "provider_config", Detail: "the subprocessor region is not in the binding's table"}
		}
	}
	// Cache: a paid cache only; P4 sets the point on the last system block (GW-16, BUD-9).
	points := countCache(out)
	cached := p.Price.CacheWrite != nil && p.Price.CacheRead != nil
	if !cached && points > 0 {
		return nil, 0, &core.Problem{Reason: "input_schema_invalid", Detail: "the profile has no cache prices"}
	}
	if cached {
		if sys := systemBlocks(out["system"]); len(sys) > 0 {
			last := sys[len(sys)-1].(map[string]any)
			if _, has := last["cache_control"]; !has {
				last["cache_control"] = map[string]any{"type": "ephemeral"}
				points++
			}
			out["system"] = sys
		}
		if points > 4 {
			return nil, 0, &core.Problem{Reason: "input_schema_invalid", Detail: "more than four cache points"}
		}
	}
	return out, maxTokens, nil
}

func systemBlocks(v any) []any {
	switch s := v.(type) {
	case string:
		if s == "" {
			return nil
		}
		return []any{map[string]any{"type": "text", "text": s}}
	case []any:
		return s
	}
	return nil
}

func countCache(v any) int {
	n := 0
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			if k == "cache_control" {
				n++
				continue
			}
			n += countCache(e)
		}
	case []any:
		for _, e := range x {
			n += countCache(e)
		}
	}
	return n
}

func toInt(v any) int64 {
	switch n := v.(type) {
	case json.Number:
		i, _ := n.Int64()
		return i
	case float64:
		return int64(n)
	case int:
		return int64(n)
	case int64:
		return n
	}
	return 0
}

// send calls the provider once, and a second time only if the request did not go out (GW-12).
func (g *Gateway) send(ctx context.Context, p Provider, key string, payload []byte) (*http.Response, bool, error) {
	for try := 0; try < 2; try++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(p.BaseURL, "/")+"/v1/messages", bytes.NewReader(payload))
		if err != nil {
			return nil, false, err
		}
		// The closed list of headers that P4 sets towards the provider (GW-3).
		req.Header.Set("x-api-key", key)
		req.Header.Set("anthropic-version", "2023-06-01")
		req.Header.Set("content-type", "application/json")
		req.Header.Set("accept", "application/json")
		resp, err := g.cfg.Client.Do(req)
		if err == nil {
			return resp, true, nil
		}
		var op *net.OpError
		if !(errors.As(err, &op) && op.Op == "dial") {
			return nil, true, err
		}
	}
	return nil, false, errors.New("the provider is unreachable")
}

// mapError maps a provider error answer to a reason code (binding table "Errors").
func mapError(resp *http.Response) (string, int) {
	switch resp.StatusCode {
	case 401, 403, 404:
		return "provider_config", 0
	case 402:
		return "provider_spend_cap", 0
	case 429:
		ra, _ := strconv.Atoi(resp.Header.Get("retry-after"))
		return "provider_rate_limited", ra
	case 400, 413:
		return "model_error", 0
	}
	if resp.StatusCode >= 500 {
		return "provider_unavailable", 5
	}
	return "model_error", 0
}

// deadline is max_output_tokens / tokens_per_s times two, at least 60 seconds (GW-12).
func deadline(p manifest.ModelProfile) time.Duration {
	d := 60 * time.Second
	if p.TokensPerS > 0 && p.MaxOutputTokens > 0 {
		if x := time.Duration(2*p.MaxOutputTokens/p.TokensPerS) * time.Second; x > d {
			d = x
		}
	}
	return d
}

// Usage is the usage field of a provider answer.
type Usage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	Iterations               []struct {
		InputTokens              int64 `json:"input_tokens"`
		OutputTokens             int64 `json:"output_tokens"`
		CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	} `json:"iterations"`
}

// Cost is the price of a call in micro-euros: tokens times the profile price per million
// tokens, summed over the lines, a fraction of a micro-euro rounded up (BUD-9).
func Cost(price manifest.Price, u Usage) (int64, *core.Tokens) {
	t := &core.Tokens{Input: u.InputTokens, Output: u.OutputTokens, CacheWrite: u.CacheCreationInputTokens, CacheRead: u.CacheReadInputTokens}
	if len(u.Iterations) > 0 {
		t = &core.Tokens{}
		for _, it := range u.Iterations {
			t.Input += it.InputTokens
			t.Output += it.OutputTokens
			t.CacheWrite += it.CacheCreationInputTokens
			t.CacheRead += it.CacheReadInputTokens
		}
	}
	sum := float64(t.Input)*price.Input + float64(t.Output)*price.Output
	if price.CacheWrite != nil {
		sum += float64(t.CacheWrite) * *price.CacheWrite
	}
	if price.CacheRead != nil {
		sum += float64(t.CacheRead) * *price.CacheRead
	}
	return int64(math.Ceil(sum - 1e-9)), t
}

// Reserve is the worst case of a call: the input estimated from above by the request bytes
// plus 1024 tokens, at the higher of the input and cache write prices, and the output limit at
// the output price (GW-2).
func Reserve(p manifest.ModelProfile, bodyBytes, maxTokens int) int64 {
	in := p.Price.Input
	if p.Price.CacheWrite != nil && *p.Price.CacheWrite > in {
		in = *p.Price.CacheWrite
	}
	sum := float64(bodyBytes+1024)*in + float64(maxTokens)*p.Price.Output
	return int64(math.Ceil(sum))
}
