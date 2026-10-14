// SPDX-License-Identifier: AGPL-3.0-only

package conform

import (
	"reflect"
	"testing"
)

func TestFlattenInherits(t *testing.T) {
	doc := map[string]any{
		"id": "fx", "rules": []any{"A-1"},
		"given": map[string]any{"tenant": "demo", "people": []any{
			map[string]any{"principal": "human:alice", "groups": []any{"user"}},
			map[string]any{"principal": "human:bob", "groups": []any{"staff"}},
		}},
		"steps":  []any{map[string]any{"lint": map[string]any{}}},
		"expect": map[string]any{"result": "ok"},
		"cases": []any{
			map[string]any{"id": "c1"},
			map[string]any{"id": "c2", "rules": []any{"A-2"}, "version": "v0.2",
				"given":  map[string]any{"people": []any{map[string]any{"principal": "human:bob", "groups": []any{"approver"}}}},
				"expect": map[string]any{"result": "rejected"}},
			map[string]any{"steps": []any{}},
		},
	}
	var out []*Case
	flatten(&out, "f.yaml", "fx", nil, doc, &Case{})
	if len(out) != 3 {
		t.Fatalf("%d cases", len(out))
	}
	c1, c2, c3 := out[0], out[1], out[2]
	if c1.ID != "c1" || !reflect.DeepEqual(c1.Rules, []string{"A-1"}) || len(c1.Steps) != 1 || c1.OwnSteps || c1.Expect["result"] != "ok" {
		t.Fatalf("c1 does not inherit: %+v", c1)
	}
	if !reflect.DeepEqual(c2.Rules, []string{"A-2"}) || c2.Version != "v0.2" || c2.Expect["result"] != "rejected" {
		t.Fatalf("c2 does not replace: %+v", c2)
	}
	people := asList(c2.Given["people"])
	if len(people) != 2 || !reflect.DeepEqual(strs(asMap(people[1])["groups"]), []string{"approver"}) {
		t.Fatalf("c2 people: %v", people)
	}
	if c3.ID != "fx#3" || !c3.OwnSteps || len(c3.Steps) != 0 {
		t.Fatalf("c3: %+v", c3)
	}
}

func TestRuleInScope(t *testing.T) {
	claim := Claim{Version: "v0.2", Profiles: []string{"core"}}
	for _, tc := range []struct {
		r    Rule
		want bool
	}{
		{Rule{Status: "active", Version: "v0.1", Profile: "core"}, true},
		{Rule{Status: "active", Version: "v0.3", Profile: "core"}, false},
		{Rule{Status: "active", Version: "v0.1", Profile: "insights"}, false},
		{Rule{Status: "deferred", Version: "v0.1", Profile: "core"}, false},
		{Rule{Status: "active", Version: "v0.1", Until: "v0.2", Profile: "core"}, true},
		{Rule{Status: "active", Version: "v0.1", Until: "v0.1", Profile: "core"}, false},
	} {
		if got := RuleInScope(&tc.r, claim); got != tc.want {
			t.Errorf("%+v: %v", tc.r, got)
		}
	}
	if cmpVersion("v0.10", "v0.9") <= 0 || cmpVersion("0.1.0", "v0.1") != 0 {
		t.Error("cmpVersion")
	}
}
