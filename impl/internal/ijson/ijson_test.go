// SPDX-License-Identifier: AGPL-3.0-only

package ijson

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseAccepts(t *testing.T) {
	cases := map[string]any{
		`{"a":1,"b":[true,false,null],"c":"x\u00e9\ud83d\ude00"}`: nil,
		`  [ ]  `:            nil,
		`9007199254740991`:   nil,
		`-9007199254740991`:  nil,
		`0.5`:                nil,
		`{"k":{"n":-1.5e3}}`: nil,
		`"\/\b\f\n\r\t\"\\"`: nil,
	}
	for in := range cases {
		if _, err := Parse([]byte(in)); err != nil {
			t.Errorf("Parse(%s): %v", in, err)
		}
	}
}

func TestParseRejects(t *testing.T) {
	bad := []string{
		`{"a":1,"a":2}`,       // repeated key
		`{"a":{"b":1,"b":1}}`, // repeated nested key
		`"\u0000"`,            // U+0000, which PostgreSQL cannot store
		`{"a\u0000": 1}`,      // the same in a key
		`"\ud800"`,            // lone high surrogate
		`"\udc00"`,            // lone low surrogate
		`"\ud800\u0041"`,      // high surrogate followed by a non-surrogate
		`9007199254740992`,    // above 2^53 - 1
		`-9007199254740992`,   // below -(2^53 - 1)
		`1e300`,               // fraction outside the range
		"\"\xff\"",            // invalid UTF-8
		`{"a":1} x`,           // trailing data
		`01`,                  // leading zero
		`[1,]`,                // trailing comma
		"\"a\x01\"",           // raw control character
	}
	for _, in := range bad {
		if _, err := Parse([]byte(in)); err == nil {
			t.Errorf("Parse(%q) accepted", in)
		}
	}
}

func TestDepthLimit(t *testing.T) {
	deep := strings.Repeat("[", 70) + strings.Repeat("]", 70)
	if _, err := Parse([]byte(deep)); err == nil {
		t.Fatal("deep document accepted")
	}
}

func TestValues(t *testing.T) {
	v, err := ParseObject([]byte(`{"n":200,"s":"x","f":1.25}`))
	if err != nil {
		t.Fatal(err)
	}
	if v["n"] != json.Number("200") || v["s"] != "x" || v["f"] != json.Number("1.25") {
		t.Fatalf("unexpected values %#v", v)
	}
}

func TestNormalize(t *testing.T) {
	v, err := Normalize(map[string]any{"a": 1, "b": []any{2.0, 2.5, "x"}, "c": map[any]any{"d": true}})
	if err != nil {
		t.Fatal(err)
	}
	m := v.(map[string]any)
	if m["a"] != json.Number("1") || m["b"].([]any)[0] != json.Number("2") || m["b"].([]any)[1] != json.Number("2.5") {
		t.Fatalf("unexpected %#v", v)
	}
	if m["c"].(map[string]any)["d"] != true {
		t.Fatalf("unexpected %#v", v)
	}
}
