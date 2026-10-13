// SPDX-License-Identifier: AGPL-3.0-only

package jcs

import (
	"testing"

	"github.com/HunterEwok/botaru/impl/internal/ijson"
)

func TestHashingExample(t *testing.T) {
	// The example of spec/hashing.md: key order and whitespace do not change the hash.
	a, _ := ijson.Parse([]byte(`{"source": "nginx-access", "lines": 200}`))
	b, _ := ijson.Parse([]byte(`{"lines":200,"source":"nginx-access"}`))
	ca, err := Canonical(a)
	if err != nil {
		t.Fatal(err)
	}
	if string(ca) != `{"lines":200,"source":"nginx-access"}` {
		t.Fatalf("canonical form %s", ca)
	}
	ha, _ := Hash(a)
	hb, _ := Hash(b)
	if ha != hb || len(ha) != 71 {
		t.Fatalf("hashes %s %s", ha, hb)
	}
}

func TestRFC8785Numbers(t *testing.T) {
	cases := map[float64]string{
		0:                      "0",
		1:                      "1",
		-1:                     "-1",
		1.5:                    "1.5",
		1e21:                   "1e+21",
		1e20:                   "100000000000000000000",
		1e-7:                   "1e-7",
		0.000001:               "0.000001",
		123456789012345680000:  "123456789012345680000",
		9007199254740991:       "9007199254740991",
		-5e-324:                "-5e-324",
		1.7976931348623157e308: "1.7976931348623157e+308",
	}
	a, b := 0.1, 0.2
	cases[a+b] = "0.30000000000000004"
	for f, want := range cases {
		got, err := FormatFloat(f)
		if err != nil || got != want {
			t.Errorf("FormatFloat(%v) = %q, want %q", f, got, want)
		}
	}
}

func TestKeyOrderUTF16(t *testing.T) {
	// RFC 8785 section 3.2.3 example: keys sorted by UTF-16 code units.
	v := map[string]any{"\u20ac": "Euro Sign", "\r": "Carriage Return", "\ufb33": "Hebrew Letter Dalet With Dagesh",
		"1": "One", "\U0001f600": "Emoji: Grinning Face", "\u0080": "Control", "\u00f6": "Latin Small Letter O With Diaeresis"}
	got, err := Canonical(v)
	if err != nil {
		t.Fatal(err)
	}
	want := "{\"\\r\":\"Carriage Return\",\"1\":\"One\",\"\u0080\":\"Control\",\"\u00f6\":\"Latin Small Letter O With Diaeresis\",\"\u20ac\":\"Euro Sign\",\"\U0001f600\":\"Emoji: Grinning Face\",\"\ufb33\":\"Hebrew Letter Dalet With Dagesh\"}"
	if string(got) != want {
		t.Fatalf("got %s\nwant %s", got, want)
	}
}

func TestEscapes(t *testing.T) {
	got, _ := Canonical("a\u0001\"\\\n/")
	if string(got) != `"a\u0001\"\\\n/"` {
		t.Fatalf("got %s", got)
	}
}
