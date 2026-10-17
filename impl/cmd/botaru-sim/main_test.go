// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestOptions(t *testing.T) {
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"-real", "-max-eur", "1"}, "-real needs -model"},
		{[]string{"-real", "-model", "claude-haiku-4-5-20251001"}, "-real needs -max-eur"},
		{[]string{"-real", "-model", "some-model", "-max-eur", "1"}, "no list price of some-model"},
		{[]string{"-mock", "-real"}, "exclude each other"},
		{[]string{"-auto-approve", "yes"}, "-auto-approve is one of"},
		{[]string{"-scenario", "nope"}, "no scenario nope"},
		{[]string{"-max-eur", "-1"}, "-max-eur must be a positive amount"},
		{[]string{"-dsn", ""}, "-dsn"},
		{[]string{"-max-eur", "0.0000001"}, "-max-eur must be between"},
		{[]string{"-max-eur", "1e13"}, "-max-eur must be between"},
	} {
		var out, errb bytes.Buffer
		code := run(c.args, strings.NewReader(""), &out, &errb)
		if code != 2 || !strings.Contains(errb.String(), c.want) {
			t.Errorf("%v: code %d, %q", c.args, code, errb.String())
		}
	}
}

func TestPrices(t *testing.T) {
	o := options{real: true, model: "claude-sonnet-4-20250514", maxEUR: 1.5, approve: "safe", dsn: "x", step: 5e9, timeout: 60e9, faultEvery: 60e9}
	if _, err := o.check(); err != nil {
		t.Fatal(err)
	}
	if o.priceIn != 3 || o.priceOut != 15 || o.scenario != "all" {
		t.Fatalf("defaults: %+v", o)
	}
	o = options{real: true, model: "claude-haiku-4-5-20251001", maxEUR: 2, priceIn: 0.5, approve: "safe", dsn: "x", step: 5e9, timeout: 60e9, faultEvery: 60e9, interactive: true}
	if _, err := o.check(); err != nil {
		t.Fatal(err)
	}
	if o.priceIn != 0.5 || o.priceOut != 5 || o.scenario != "" {
		t.Fatalf("an override: %+v", o)
	}
}
