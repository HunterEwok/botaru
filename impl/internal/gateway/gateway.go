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
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/HunterEwok/botaru/impl/internal/core"
	"github.com/HunterEwok/botaru/impl/internal/ijson"
	"github.com/HunterEwok/botaru/impl/internal/jcs"
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
		// A redirect is not followed: it would send the key and the prompt to another address.
		cfg.Client = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
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
	if err := g.checkTools(ctx, body, grant); err != nil {
		g.refuse(w, err)
		return
	}
	// The fallback profiles the call may switch to; the reserve of the first call is at the
	// most expensive model P4 may use in it, the slot profile or one of them (GW-6).
	fallbacks, err := g.c.Fallbacks(ctx, grant)
	if err != nil {
		g.refuse(w, err)
		return
	}
	var candidates []manifest.ModelProfile
	for _, f := range fallbacks {
		candidates = append(candidates, f.Profile)
	}
	// The call goes to the slot profile; on a reason of its fallback_on it goes once to each
	// fallback profile in order (GW-5).
	tried := 0
	switches := func(reason string) bool { return core.SwitchesOn(grant, reason) && tried < len(fallbacks) }
	for call := grant; ; {
		reason := g.serveCall(ctx, w, body, call, call == grant, candidates, switches)
		if reason == "" {
			return
		}
		next := *fallbacks[tried]
		next.FallbackReason = reason
		tried++
		call = &next
	}
}

// serveCall makes one provider call for a checked request on the profile of call and writes
// the answer, unless the call failed for a reason on which switches says the call goes to the
// next fallback profile (GW-5): then it writes nothing and returns the reason.
func (g *Gateway) serveCall(ctx context.Context, w http.ResponseWriter, body map[string]any, call *core.ModelGrant, first bool, candidates []manifest.ModelProfile, switches func(string) bool) string {
	// The pair (workspace, model pool) answers at once while its circuit is open or it waits
	// for the provider (GW-10, GW-11, GW-13).
	probe, err := g.c.ProviderGateProbe(ctx, call)
	if err != nil {
		// A fallback pair whose circuit is open or that waits passes to the next fallback.
		if !first && switches(call.FallbackReason) {
			return call.FallbackReason
		}
		// The open circuit of the slot's pair is the model being unavailable (GW-11): the call
		// goes to a fallback whose pair answers, as after a 500 (GW-5). A pair that waits for a
		// rate limit or a configuration fix is no fallback reason.
		if p, ok := core.AsProblem(err); ok && first && p.Reason == "provider_unavailable" && switches("unavailable") {
			return "unavailable"
		}
		g.refuse(w, err)
		return ""
	}
	// The probe that ends before the provider is called gives the probe back (GW-11).
	sentToProvider := false
	defer func() {
		if probe != nil && !sentToProvider {
			if err := g.c.ProviderProbeAbandoned(context.WithoutCancel(ctx), call, *probe); err != nil {
				g.log.Warn("giving back the probe failed", "err", err)
			}
		}
	}()
	prov, ok := g.cfg.Providers[call.Profile.Provider]
	if !ok || prov.Binding != "anthropic" {
		g.outcome(ctx, call, "provider_config", 0)
		problem(w, "provider_config", "the provider of the profile has no anthropic binding in this deployment", 0)
		return ""
	}
	key := prov.Keys[call.Tenant]
	if key == "" {
		g.outcome(ctx, call, "provider_config", 0)
		problem(w, "provider_config", "no provider key for the tenant's workspace", 0)
		return ""
	}
	out, maxTokens, err := g.providerBody(body, call)
	if err != nil {
		g.refuse(w, err)
		return ""
	}
	payload, err := json.Marshal(out)
	if err != nil {
		g.refuse(w, err)
		return ""
	}
	// own is the worst case of this call's own model: a call without an answer charges it
	// (BUD-11), the call was on this model only.
	own := Reserve(call.Profile, len(payload), maxTokens)
	reserve := own
	if first {
		for _, p := range candidates {
			reserve = max(reserve, Reserve(p, len(payload), maxTokens))
		}
	}
	spendID, err := g.c.Reserve(ctx, call, reserve)
	if err != nil {
		g.refuse(w, err)
		return ""
	}
	// The provider call does not depend on the runtime connection: a dropped runtime does not
	// break reading the answer, so the usage is still charged (GW-2, BUD-11).
	callCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), deadline(call.Profile))
	defer cancel()
	resp, sent, err := g.send(callCtx, prov, key, payload)
	sentToProvider = sent
	if err != nil {
		if !sent {
			_ = g.c.DropReserve(context.WithoutCancel(ctx), spendID)
		} else {
			_ = g.c.Charge(context.WithoutCancel(ctx), spendID, own, nil, "", true)
		}
		g.outcome(ctx, call, "provider_unavailable", 0)
		// A dropped connection is the reason unavailable (binding table "Errors").
		if switches("unavailable") {
			return "unavailable"
		}
		problem(w, "provider_unavailable", "the provider did not answer", 5)
		return ""
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	requestID := resp.Header.Get("request-id")
	if err != nil {
		_ = g.c.Charge(context.WithoutCancel(ctx), spendID, own, nil, requestID, true)
		g.outcome(ctx, call, "provider_unavailable", 0)
		problem(w, "provider_unavailable", "the provider answer broke off", 5)
		return ""
	}
	if resp.StatusCode != http.StatusOK {
		_ = g.c.Charge(context.WithoutCancel(ctx), spendID, 0, nil, requestID, false)
		reason, retry := mapError(resp)
		g.outcome(ctx, call, reason, retry)
		if fr := fallbackReason(resp.StatusCode, respBody); fr != "" && switches(fr) {
			return fr
		}
		problem(w, reason, "provider answered "+strconv.Itoa(resp.StatusCode), retry)
		return ""
	}
	var parsed struct {
		Usage *Usage `json:"usage"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil || parsed.Usage == nil {
		_ = g.c.Charge(context.WithoutCancel(ctx), spendID, own, nil, requestID, true)
		problem(w, "model_error", "the provider answer has no usage", 0)
		return ""
	}
	g.outcome(ctx, call, "", 0)
	cost, tokens := Cost(call.Profile.Price, *parsed.Usage)
	// The answer goes out only once its usage is recorded (BUD-3). If the store stays away, the
	// reserve stays in place, so the limit still counts the worst case of the call (BUD-2).
	if err := g.charge(context.WithoutCancel(ctx), spendID, cost, tokens, requestID); err != nil {
		g.log.Error("charging a model call failed", "err", err, "spend", spendID)
		problem(w, "store_unavailable", "the usage of the call could not be recorded", 5)
		return ""
	}
	// The request-id stays in the usage line, not in the answer to the runtime.
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(respBody)
	return ""
}

// fallbackReason is the fallback reason of a provider answer (binding, GW-5): 529 overloaded,
// 500 unavailable, a 404 not_found_error on the model retired; "" for any other answer.
func fallbackReason(status int, body []byte) string {
	switch status {
	case 529:
		return "overloaded"
	case 500:
		return "unavailable"
	case 404:
		var e struct {
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(body, &e) == nil && e.Error.Type == "not_found_error" && strings.Contains(strings.ToLower(e.Error.Message), "model") {
			return "retired"
		}
	}
	return ""
}

// outcome reports the outcome of a provider call for the circuit of its pair; a failure to
// record it is logged, since the answer to the runtime does not depend on it.
func (g *Gateway) outcome(ctx context.Context, grant *core.ModelGrant, reason string, retryAfterS int) {
	if err := g.c.ProviderOutcome(context.WithoutCancel(ctx), grant, reason, retryAfterS); err != nil {
		g.log.Warn("recording the provider outcome failed", "err", err, "reason", reason)
	}
}

// checkTools compares the tools of a request with the P3 catalog of the handle's attempt by
// name and by the P4 tool hash of spec/hashing.md (GW-1): the request carries exactly the
// catalog in its order, so a tool the catalog does not have, one with another description or
// schema, a missing one and another order are refused before the provider.
func (g *Gateway) checkTools(ctx context.Context, body map[string]any, grant *core.ModelGrant) error {
	tools, _ := body["tools"].([]any)
	want, err := g.c.CatalogHashes(ctx, grant)
	if err != nil {
		return err
	}
	differ := &core.Problem{Reason: "input_schema_invalid", Detail: "tools differ from the P3 catalog of the handle"}
	if len(tools) != len(want) {
		return differ
	}
	for i, x := range tools {
		t, _ := x.(map[string]any)
		name, _ := t["name"].(string)
		m := map[string]any{}
		for _, k := range []string{"name", "description", "input_schema"} {
			if v, ok := t[k]; ok {
				m[k] = v
			}
		}
		h, err := jcs.Hash(m)
		if err != nil || name != want[i].Name || h != want[i].Hash {
			return differ
		}
	}
	return nil
}

// charge records the usage of a call, retrying while the store is briefly away.
func (g *Gateway) charge(ctx context.Context, spendID string, cost int64, tokens *core.Tokens, requestID string) error {
	var err error
	for i, pause := range []time.Duration{0, 200 * time.Millisecond, time.Second, 3 * time.Second} {
		if i > 0 {
			time.Sleep(pause)
		}
		if err = g.c.Charge(ctx, spendID, cost, tokens, requestID, false); err == nil {
			return nil
		}
	}
	return err
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
	if tools, ok := out["tools"].([]any); !ok || len(tools) == 0 {
		delete(out, "tools")
	}
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
	// Effort: the profile's, as the output limit is the profile's (GW-7).
	if p.Effort != "" {
		// A copy: the request body serves every fallback call, so it is never changed in place.
		oc := shallow(out["output_config"])
		oc["effort"] = p.Effort
		out["output_config"] = oc
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
	// Cache: a paid cache only; P4 sets the point on the last system block and, from the
	// second call of a run on, on the last block of the last message, so the next call reads
	// the conversation so far from the cache (GW-16, BUD-9).
	points := countCache(out)
	cached := p.Price.CacheWrite != nil && p.Price.CacheRead != nil
	if !cached && points > 0 {
		return nil, 0, &core.Problem{Reason: "input_schema_invalid", Detail: "the profile has no cache prices"}
	}
	if cached {
		// The points go on copies of the blocks they change: the request body serves every
		// fallback call, whose profile may have no cache prices.
		if sys := systemBlocks(out["system"]); len(sys) > 0 {
			if last, ok := sys[len(sys)-1].(map[string]any); ok {
				if _, has := last["cache_control"]; !has {
					sys = slices.Clone(sys)
					last = shallow(last)
					last["cache_control"] = map[string]any{"type": "ephemeral"}
					sys[len(sys)-1] = last
					points++
				}
			}
			out["system"] = sys
		}
		if msgs, _ := out["messages"].([]any); len(msgs) > 1 && points < 4 {
			if m, ok := msgs[len(msgs)-1].(map[string]any); ok {
				blocks := contentBlocks(m["content"])
				if n := len(blocks); n > 0 {
					if last, ok := blocks[n-1].(map[string]any); ok && cacheable(last) {
						if _, has := last["cache_control"]; !has {
							blocks = slices.Clone(blocks)
							last = shallow(last)
							last["cache_control"] = map[string]any{"type": "ephemeral"}
							blocks[n-1] = last
							m = shallow(m)
							m["content"] = blocks
							msgs = slices.Clone(msgs)
							msgs[len(msgs)-1] = m
							out["messages"] = msgs
							points++
						}
					}
				}
			}
		}
		if points > 4 {
			return nil, 0, &core.Problem{Reason: "input_schema_invalid", Detail: "more than four cache points"}
		}
	}
	return out, maxTokens, nil
}

// shallow is a one-level copy of a JSON object; anything else gives an empty object.
func shallow(v any) map[string]any {
	m, _ := v.(map[string]any)
	out := make(map[string]any, len(m))
	for k, x := range m {
		out[k] = x
	}
	return out
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

// contentBlocks is the content of a message as blocks: a string becomes one text block.
func contentBlocks(v any) []any {
	switch c := v.(type) {
	case string:
		if c == "" {
			return nil
		}
		return []any{map[string]any{"type": "text", "text": c}}
	case []any:
		return c
	}
	return nil
}

// cacheable says whether a message block may carry a cache point: reasoning blocks may not.
func cacheable(b map[string]any) bool {
	switch b["type"] {
	case "thinking", "redacted_thinking":
		return false
	}
	return true
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
// tokens, summed over the lines, a fraction of a micro-euro rounded up (BUD-9). Each compaction
// iteration is a request of its own and takes the price tier of its own input (BUD-17).
func Cost(price manifest.Price, u Usage) (int64, *core.Tokens) {
	lines := []core.Tokens{{Input: u.InputTokens, Output: u.OutputTokens, CacheWrite: u.CacheCreationInputTokens, CacheRead: u.CacheReadInputTokens}}
	if len(u.Iterations) > 0 {
		lines = lines[:0]
		for _, it := range u.Iterations {
			lines = append(lines, core.Tokens{Input: it.InputTokens, Output: it.OutputTokens, CacheWrite: it.CacheCreationInputTokens, CacheRead: it.CacheReadInputTokens})
		}
	}
	t := &core.Tokens{}
	var sum float64
	for _, l := range lines {
		t.Input += l.Input
		t.Output += l.Output
		t.CacheWrite += l.CacheWrite
		t.CacheRead += l.CacheRead
		pr := price.Tier(l.Input + l.CacheWrite + l.CacheRead)
		sum += float64(l.Input)*pr.Input + float64(l.Output)*pr.Output
		if pr.CacheWrite != nil {
			sum += float64(l.CacheWrite) * *pr.CacheWrite
		}
		if pr.CacheRead != nil {
			sum += float64(l.CacheRead) * *pr.CacheRead
		}
	}
	return int64(math.Ceil(sum - 1e-9)), t
}

// Reserve is the worst case of a call: the input estimated from above by the request bytes
// plus 1024 tokens, at the higher of the input and cache write prices of the price tier of that
// input, and the output limit at the tier's output price (GW-2).
func Reserve(p manifest.ModelProfile, bodyBytes, maxTokens int) int64 {
	tier := p.Price.Tier(int64(bodyBytes + 1024))
	sum := float64(bodyBytes+1024)*tier.InputCeiling() + float64(maxTokens)*tier.Output
	return int64(math.Ceil(sum))
}
