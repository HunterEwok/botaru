// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"testing"
)

// TestKeyRotation: in the rotation window a token key moved to the previous keys still
// verifies the lease tokens it signed and finds the worker tokens it hashed; a worker token is
// rehashed under the new key at its first use, so it outlives the window (SEC-5).
func TestKeyRotation(t *testing.T) {
	ctx := context.Background()
	b := newBench(t)
	r, err := b.c.Enroll(ctx, b.op, EnrollRequest{Tenant: "demo", Role: "hello", Kind: "agent"})
	if err != nil {
		t.Fatal(err)
	}
	old := b.c.cfg.TokenKey
	tok, err := b.c.issueToken(b.c.Epoch(), "tsk_x", 1, "wrk_x")
	if err != nil {
		t.Fatal(err)
	}
	h, err := EncodeToken(tok)
	if err != nil {
		t.Fatal(err)
	}
	newKey := []byte("fedcba9876543210fedcba9876543210")
	rotated, err := New(ctx, Config{TokenKey: newKey, KeyID: "k2", PreviousKeys: map[string][]byte{"k1": old}, TestBench: true}, b.c.st, b.c.spec, b.c.texts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rotated.decodeToken(h); err != nil {
		t.Fatalf("a token of the previous key in the window: %v", err)
	}
	if _, err := rotated.AuthenticateWorker(ctx, r.Token); err != nil {
		t.Fatalf("a worker token of the previous key in the window: %v", err)
	}
	after, err := New(ctx, Config{TokenKey: newKey, KeyID: "k2", TestBench: true}, b.c.st, b.c.spec, b.c.texts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := after.decodeToken(h); err == nil {
		t.Fatal("a token of a removed key still verifies")
	}
	if _, err := after.AuthenticateWorker(ctx, r.Token); err != nil {
		t.Fatalf("a worker token rehashed in the window: %v", err)
	}
	if _, err := New(ctx, Config{TokenKey: newKey, KeyID: "k2", PreviousKeys: map[string][]byte{"k2": old}, TestBench: true}, b.c.st, b.c.spec, b.c.texts); err == nil {
		t.Fatal("a previous key with the current key id passed")
	}
}
