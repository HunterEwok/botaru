// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/HunterEwok/botaru/impl/internal/sim"
)

func TestOptions(t *testing.T) {
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"-real", "-model", "claude-haiku-5-5"}, "-real needs -max-eur"},
		{[]string{"-real", "-max-eur", "1", "-price-in", "1"}, "-price-in and -price-out need one -model"},
		{[]string{"-real", "-model", "some-model", "-max-eur", "1"}, "no list price of some-model"},
		{[]string{"-mock", "-real"}, "exclude each other"},
		{[]string{"-auto-approve", "yes"}, "-auto-approve is one of"},
		{[]string{"-scenario", "nope"}, "no scenario nope"},
		{[]string{"-max-eur", "-1"}, "-max-eur must be a positive amount"},
		{[]string{"-dsn", ""}, "-dsn"},
		{[]string{"-max-eur", "0.0000001"}, "-max-eur must be between"},
		{[]string{"-max-eur", "1e13"}, "-max-eur must be between"},
		{[]string{"-halt-on-violation", "maybe"}, "-halt-on-violation is yes, no or auto"},
	} {
		var out, errb bytes.Buffer
		code := run(c.args, strings.NewReader(""), &out, &errb)
		if code != 2 || !strings.Contains(errb.String(), c.want) {
			t.Errorf("%v: code %d, %q", c.args, code, errb.String())
		}
	}
}

func TestPrices(t *testing.T) {
	o := options{real: true, model: "claude-sonnet-5-5", maxEUR: 1.5, approve: "safe", dsn: "x", step: 5e9, timeout: 60e9, faultEvery: 60e9, halt: "auto"}
	if _, err := o.check(); err != nil {
		t.Fatal(err)
	}
	if o.priceIn != 2 || o.priceOut != 10 || o.scenario != "all" {
		t.Fatalf("defaults: %+v", o)
	}
	o = options{real: true, model: "claude-haiku-5-5", maxEUR: 2, priceIn: 0.5, approve: "safe", dsn: "x", step: 5e9, timeout: 60e9, faultEvery: 60e9, interactive: true, halt: "auto"}
	if _, err := o.check(); err != nil {
		t.Fatal(err)
	}
	if o.priceIn != 0.5 || o.priceOut != 2.5 || o.scenario != "" {
		t.Fatalf("an override: %+v", o)
	}
	o = options{real: true, model: "", maxEUR: 1.5, approve: "safe", dsn: "x", step: 5e9, timeout: 60e9, faultEvery: 60e9, halt: "auto"}
	if _, err := o.check(); err != nil || o.model != "rank" {
		t.Fatalf("a rank run: %v %+v", err, o)
	}
}

// TestPriceOf: the list price of every model of the ranks, and an unknown id of a family at the
// highest price the family had.
func TestPriceOf(t *testing.T) {
	for model, want := range map[string][2]float64{
		"claude-haiku-5-5": {0.50, 2.50}, "claude-sonnet-5-5": {2, 10}, "claude-opus-5-5": {4, 20}, "claude-fable-5-1": {10, 50},
		"claude-opus-9-9": {15, 75}, "claude-fable-9": {10, 50},
	} {
		if p, ok := priceOf(model); !ok || p != want {
			t.Errorf("%s: %v %v, want %v", model, p, ok, want)
		}
	}
	if _, ok := priceOf("some-model"); ok {
		t.Error("some-model has a price")
	}
	for r, m := range rankModels {
		if _, ok := prices[m]; !ok {
			t.Errorf("rank %d: no list price of %s", r, m)
		}
	}
}

// TestEvolutionUsage: the console commands of the scaffold evolution say their usage before
// they reach the stand.
func TestEvolutionUsage(t *testing.T) {
	d := &sim.Director{}
	for line, want := range map[string]string{
		"gate":                "gate PROPOSAL",
		"realize":             "realize PROPOSAL",
		"revert":              "revert PROPOSAL",
		"revert a b c":        "revert PROPOSAL",
		"evolve maybe":        "evolve on|off",
		"insights":            "insights on|off",
		"no-such-command now": "unknown command",
	} {
		var out bytes.Buffer
		if quit := command(t.Context(), d, line, &out); quit || !strings.Contains(out.String(), want) {
			t.Errorf("%q: quit %v, %q", line, quit, out.String())
		}
	}
}
