// SPDX-License-Identifier: AGPL-3.0-only

package gateway

import (
	"testing"

	"github.com/HunterEwok/botaru/impl/internal/core"
	"github.com/HunterEwok/botaru/impl/internal/manifest"
)

func TestCostAndReserve(t *testing.T) {
	p := manifest.ModelProfile{MaxOutputTokens: 8000, Price: manifest.Price{Input: 1, Output: 4}}
	if c, tok := Cost(p.Price, Usage{InputTokens: 180, OutputTokens: 20}); c != 260 || tok.Input != 180 {
		t.Fatalf("cost %d %+v", c, tok)
	}
	// A fraction of a micro-euro of the call sum is rounded up.
	if c, _ := Cost(manifest.Price{Input: 0.25}, Usage{InputTokens: 3}); c != 1 {
		t.Fatalf("rounding %d", c)
	}
	// Iterations are summed.
	u := Usage{InputTokens: 1, OutputTokens: 1}
	u.Iterations = append(u.Iterations, struct {
		InputTokens              int64 `json:"input_tokens"`
		OutputTokens             int64 `json:"output_tokens"`
		CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	}{InputTokens: 10, OutputTokens: 10}, struct {
		InputTokens              int64 `json:"input_tokens"`
		OutputTokens             int64 `json:"output_tokens"`
		CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	}{InputTokens: 10, OutputTokens: 10})
	if c, _ := Cost(p.Price, u); c != 100 {
		t.Fatalf("iterations %d", c)
	}
	// The hello-bot reserve: (body + 1024) at the input price plus the output limit.
	if r := Reserve(p, 300, 8000); r != 1324+32000 {
		t.Fatalf("reserve %d", r)
	}
}

func TestProviderBody(t *testing.T) {
	g := &Gateway{cfg: Config{TestBench: false}}
	grant := &core.ModelGrant{Profile: manifest.ModelProfile{Model: "m-1", MaxOutputTokens: 500}}
	grant.Subprocessor.Region = "us"
	req := map[string]any{"model": "base", "max_tokens": 1000, "messages": []any{}, "metadata": map[string]any{"user_id": "x"}, "system": "s"}
	out, max, err := g.providerBody(req, grant)
	if err != nil {
		t.Fatal(err)
	}
	if out["model"] != "m-1" || max != 500 || out["inference_geo"] != "us" {
		t.Fatalf("body %v", out)
	}
	if _, ok := out["metadata"]; ok {
		t.Fatal("metadata reached the provider")
	}
	if _, ok := out["system"].(string); !ok {
		t.Fatal("a profile without cache prices got a cache point")
	}
	for name, bad := range map[string]map[string]any{
		"stream": {"model": "b", "max_tokens": 10, "stream": true},
		"cache":  {"model": "b", "max_tokens": 10, "system": []any{map[string]any{"type": "text", "text": "s", "cache_control": map[string]any{"type": "ephemeral"}}}},
	} {
		if _, _, err := g.providerBody(bad, grant); err == nil {
			t.Errorf("%s passed", name)
		}
	}
	grant.Subprocessor.Region = "region-a"
	if _, _, err := g.providerBody(req, grant); err == nil {
		t.Error("a region outside the binding table passed outside the test bench")
	}
	cw, cr := 1.25, 0.1
	grant.Profile.Price = manifest.Price{CacheWrite: &cw, CacheRead: &cr}
	grant.Subprocessor.Region = "global"
	out, _, err = g.providerBody(req, grant)
	if err != nil {
		t.Fatal(err)
	}
	blocks := out["system"].([]any)
	if blocks[0].(map[string]any)["cache_control"] == nil {
		t.Fatal("no cache point on the last system block")
	}
	if _, ok := out["output_config"]; ok {
		t.Fatal("a profile without effort set one")
	}
	// From the second call of a run on, the last block of the last message gets a point too.
	loop := map[string]any{"model": "base", "max_tokens": 1000, "system": "s", "messages": []any{
		map[string]any{"role": "user", "content": "goal"},
		map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": "t1", "name": "x", "input": map[string]any{}}}},
		map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "t1", "content": "r"}}},
	}, "output_config": map[string]any{"effort": "max"}}
	grant.Profile.Effort = "low"
	out, _, err = g.providerBody(loop, grant)
	if err != nil {
		t.Fatal(err)
	}
	msgs := out["messages"].([]any)
	last := msgs[2].(map[string]any)["content"].([]any)[0].(map[string]any)
	if last["cache_control"] == nil || countCache(out) != 2 {
		t.Fatalf("cache points %d, last block %v", countCache(out), last)
	}
	if msgs[0].(map[string]any)["content"] != "goal" {
		t.Fatal("an earlier message was changed")
	}
	if e := out["output_config"].(map[string]any)["effort"]; e != "low" {
		t.Fatalf("effort %v: the profile's effort wins", e)
	}
	// The first call of a run gets no message point: nothing reads it.
	first := map[string]any{"model": "base", "max_tokens": 1000, "system": "s", "messages": []any{map[string]any{"role": "user", "content": "goal"}}}
	if out, _, err = g.providerBody(first, grant); err != nil || countCache(out) != 1 {
		t.Fatalf("first call: points %d, %v", countCache(out), err)
	}
}

func TestPriceTier(t *testing.T) {
	cw, cr := 0.125, 0.01
	p := manifest.ModelProfile{MaxOutputTokens: 1000, Price: manifest.Price{Input: 0.1, Output: 0.5, CacheWrite: &cw, CacheRead: &cr,
		Above: &manifest.PriceTier{InputTokens: 100000, Input: 0.5, Output: 2.5}}}
	// At the threshold the base price holds; above it every token of the call is at the higher price.
	if c, _ := Cost(p.Price, Usage{InputTokens: 100000, OutputTokens: 1000}); c != 10500 {
		t.Fatalf("base tier %d", c)
	}
	if c, _ := Cost(p.Price, Usage{InputTokens: 1, CacheReadInputTokens: 100000, OutputTokens: 1000}); c != 1+5000+2500 {
		t.Fatalf("upper tier %d: cache read at 0.01*5", c)
	}
	// Each compaction iteration takes its own tier: two iterations at the threshold stay at the base.
	two := Usage{}
	two.Iterations = append(two.Iterations, struct {
		InputTokens              int64 `json:"input_tokens"`
		OutputTokens             int64 `json:"output_tokens"`
		CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	}{InputTokens: 100000}, struct {
		InputTokens              int64 `json:"input_tokens"`
		OutputTokens             int64 `json:"output_tokens"`
		CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	}{InputTokens: 100000})
	if c, tk := Cost(p.Price, two); c != 20000 || tk.Input != 200000 {
		t.Fatalf("iterations %d %+v", c, tk)
	}
	// A tier cheaper than the base counts at the base.
	cheap := manifest.Price{Input: 0.1, Output: 0.5, Above: &manifest.PriceTier{InputTokens: 10, Input: 0.05, Output: 0.1}}
	if c, _ := Cost(cheap, Usage{InputTokens: 1000, OutputTokens: 1000}); c != 600 {
		t.Fatalf("cheap tier %d", c)
	}
	// The reserve takes the tier of its own input estimate.
	if r := Reserve(p, 1000, 1000); r != 253+500 {
		t.Fatalf("small reserve %d", r)
	}
	if r := Reserve(p, 200000, 1000); r != 125640+2500 {
		t.Fatalf("large reserve %d", r)
	}
}

func TestNewRefusesForeignAddressOutsideTestBench(t *testing.T) {
	_, err := New(nil, Config{Providers: map[string]Provider{"a": {Binding: "anthropic", BaseURL: "http://127.0.0.1:9"}}})
	if err == nil {
		t.Fatal("a mock address passed outside the test bench")
	}
}

// The cache points and the effort of one profile never reach the body of a fallback call: the
// request body is not changed in place (GW-18, GW-19).
func TestProviderBodyLeavesRequest(t *testing.T) {
	g := &Gateway{cfg: Config{TestBench: true}}
	cw, cr := 1.25, 0.1
	cachedGrant := &core.ModelGrant{Profile: manifest.ModelProfile{Model: "m-1", Effort: "low", Price: manifest.Price{CacheWrite: &cw, CacheRead: &cr}}}
	plain := &core.ModelGrant{Profile: manifest.ModelProfile{Model: "m-2"}}
	req := map[string]any{"model": "base", "max_tokens": 100, "system": []any{map[string]any{"type": "text", "text": "s"}},
		"output_config": map[string]any{"format": map[string]any{"type": "json_schema"}},
		"messages": []any{map[string]any{"role": "user", "content": "q"}, map[string]any{"role": "assistant", "content": "a"},
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "more"}}}}}
	out, _, err := g.providerBody(req, cachedGrant)
	if err != nil {
		t.Fatal(err)
	}
	msgs := out["messages"].([]any)
	if msgs[2].(map[string]any)["content"].([]any)[0].(map[string]any)["cache_control"] == nil || out["output_config"].(map[string]any)["effort"] != "low" {
		t.Fatalf("cached body %v", out)
	}
	if countCache(req) != 0 || req["output_config"].(map[string]any)["effort"] != nil {
		t.Fatalf("the request was changed: %v", req)
	}
	out, _, err = g.providerBody(req, plain)
	if err != nil {
		t.Fatalf("the fallback call: %v", err)
	}
	if countCache(out) != 0 || out["output_config"].(map[string]any)["effort"] != nil {
		t.Fatalf("fallback body %v", out)
	}
}
