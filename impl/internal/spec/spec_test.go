// SPDX-License-Identifier: AGPL-3.0-only

package spec

import (
	"errors"
	"testing"

	"github.com/HunterEwok/botaru/impl/internal/ijson"
)

func load(t *testing.T) *Spec {
	t.Helper()
	root, err := FindRoot("")
	if err != nil {
		t.Fatal(err)
	}
	s, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestCompileAll(t *testing.T) {
	if err := load(t).CompileAll(); err != nil {
		t.Fatal(err)
	}
}

func TestValidate(t *testing.T) {
	s := load(t)
	ok, _ := ijson.Parse([]byte(`{"kinds":["attempt"],"wait_s":30}`))
	if err := s.Validate("p1-claim-request", ok); err != nil {
		t.Fatal(err)
	}
	bad, _ := ijson.Parse([]byte(`{"kinds":["attempt"],"tenant":"demo"}`))
	err := s.Validate("p1-claim-request", bad)
	var ve *ValidationError
	if !errors.As(err, &ve) || len(ve.Violations) == 0 {
		t.Fatalf("want a validation error, got %v", err)
	}
	// A lookahead pattern (ECMAScript) compiles and works.
	key, _ := ijson.Parse([]byte(`{"body":{"type":"hello.greet.v1","goal":"hi"},"idempotency_key":"system:x"}`))
	if err := s.Validate("enqueue-request", key); err == nil {
		t.Fatal("system: key accepted")
	}
}

func TestRegistry(t *testing.T) {
	r, err := load(t).Registry("spec/model/reasons.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !r.Has("lease_expired") || r.Has("no_such_reason") {
		t.Fatal("reasons registry lookup")
	}
}
