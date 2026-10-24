// SPDX-License-Identifier: AGPL-3.0-only

package p1client

import (
	"context"
	"testing"
)

// Plain HTTP reaches the core only on this machine, or on a network the worker declares
// private; another machine takes https, so the worker token is never sent in clear.
func TestCheckCore(t *testing.T) {
	for _, ok := range []string{"http://127.0.0.1:8401", "http://localhost:8401", "http://[::1]:8401", "https://core1.example.org:8401"} {
		if err := CheckCore(ok); err != nil {
			t.Fatalf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"http://core1.example.org:8401", "http://192.0.2.5:8401", "ftp://core1", "core1:8401"} {
		if err := CheckCore(bad); err == nil {
			t.Fatalf("%s passed", bad)
		}
	}
	c := &Client{BaseURL: "http://core1.example.org:8401", Token: "secret"}
	if err := c.Call(context.Background(), "hello", "", nil, nil); err == nil {
		t.Fatal("a call over plain HTTP to another machine was made")
	}
	t.Setenv(PlainPrivateEnv, "1")
	if err := CheckCore("http://core.botaru.example.org:8401"); err != nil {
		t.Fatal(err)
	}
}
