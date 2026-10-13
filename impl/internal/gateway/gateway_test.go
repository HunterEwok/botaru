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
		"tools":  {"model": "b", "max_tokens": 10, "tools": []any{map[string]any{"name": "x"}}},
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
}

func TestNewRefusesForeignAddressOutsideTestBench(t *testing.T) {
	_, err := New(nil, Config{Providers: map[string]Provider{"a": {Binding: "anthropic", BaseURL: "http://127.0.0.1:9"}}})
	if err == nil {
		t.Fatal("a mock address passed outside the test bench")
	}
}
