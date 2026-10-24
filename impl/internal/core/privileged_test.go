// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/HunterEwok/botaru/impl/internal/manifest"
	"github.com/HunterEwok/botaru/impl/internal/spec"
	"github.com/HunterEwok/botaru/impl/internal/testbench"
)

func TestPrivilegedGroup(t *testing.T) {
	t.Parallel()
	cat := newCatalog()
	cat.TaskTypes["greet"] = manifest.TaskType{Tier: 0}
	cat.TaskTypes["read"] = manifest.TaskType{Tier: 1}
	tv := &TenantView{Groups: map[string]manifest.Group{
		"hello":    {Caps: []string{"task:greet", "msg.send"}, Data: []string{"summary"}},
		"reader":   {Caps: []string{"task:read"}},
		"logs":     {Caps: []string{"task:greet"}, Data: []string{"raw_logs"}},
		"admin":    {Caps: []string{"admin:people"}},
		"unknown":  {Caps: []string{"task:missing"}},
		"heir":     {Caps: []string{"team.view"}, Inherits: []string{"admin"}},
		"loop":     {Inherits: []string{"loop2"}},
		"loop2":    {Inherits: []string{"loop"}},
		"inherits": {Inherits: []string{"hello"}},
		"viewer":   {Caps: []string{"team.view:hello", "task.read:greet"}},
		"wildcard": {Caps: []string{"task:greet.*"}},
	}}
	for name, want := range map[string]bool{
		"hello": false, "reader": true, "logs": true, "admin": true, "unknown": true,
		"heir": true, "loop": false, "inherits": false, "absent": true, "viewer": false, "wildcard": true,
	} {
		if got := privilegedGroup(tv, cat, name, map[string]bool{}); got != want {
			t.Errorf("%s: privileged %v, want %v", name, got, want)
		}
	}
}

func TestCapCovers(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		have, want string
		ok         bool
	}{
		{"task:report.*", "task:report.daily.v1", true},
		{"task:report.*", "task:reports.daily.v1", false},
		{"task:report.daily.v1", "task:report.daily.v1", true},
		{"action.*", "action.restart", false},
		{"task:*", "task:x", false},
	} {
		if got := capCovers(c.have, c.want); got != c.ok {
			t.Errorf("%s covers %s: %v, want %v", c.have, c.want, got, c.ok)
		}
	}
}

// PRM-13, PRM-7: a delete and a second apply do not make a group with members privileged. A
// group is deleted only without members, and a group of the store's memberships counts as an
// ordinary group even when the stored set lacks it.
func TestApplyPrivilegeRaiseAfterDelete(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newBench(t)
	root, err := spec.FindRoot("")
	if err != nil {
		t.Fatal(err)
	}
	ms, err := testbench.SharedHelloBotManifests(root)
	if err != nil {
		t.Fatal(err)
	}
	var rest []map[string]any
	for _, m := range ms {
		if m["kind"] != "Group" {
			rest = append(rest, m)
		}
	}
	_, err = b.c.Apply(ctx, farmOf(b.op), ApplyRequest{Commit: "3333333333333333333333333333333333333333", Manifests: rest,
		Delete: []ApplyDelete{{Kind: "Group", Tenant: "demo", Name: "hello"}}})
	var p *Problem
	if !errors.As(err, &p) || p.Reason != "forbidden" {
		t.Fatalf("delete of a group with members: %v", err)
	}
	// A plan reports the refusal too.
	_, err = b.c.Apply(ctx, farmOf(b.op), ApplyRequest{Commit: "3333333333333333333333333333333333333333", Manifests: rest, DryRun: true,
		Delete: []ApplyDelete{{Kind: "Group", Tenant: "demo", Name: "hello"}}})
	if !errors.As(err, &p) || p.Reason != "forbidden" {
		t.Fatalf("dry run of the delete of a group with members: %v", err)
	}
	// A group the stored set lacks (deleted by an earlier build) while its members stay.
	if _, err := b.c.st.Pool.Exec(ctx, `DELETE FROM manifests WHERE kind = 'Group' AND tenant = 'demo' AND name = 'hello'`); err != nil {
		t.Fatal(err)
	}
	// A grant reads the group from the stored set, not from the core's copy.
	if _, err := b.c.Grant(ctx, b.op, GrantRequest{Op: "grant", Subject: b.alice, Group: "hello"}); !errors.As(err, &p) || p.Reason != "not_found" {
		t.Fatalf("a grant of a group the stored set lacks: %v", err)
	}
	for _, m := range ms {
		if m["kind"] == "Group" {
			m["spec"].(map[string]any)["data"] = []any{"summary", "raw_logs"}
		}
	}
	res, err := b.c.Apply(ctx, farmOf(b.op), ApplyRequest{Commit: "4444444444444444444444444444444444444444", Manifests: ms})
	if err != nil || res.Applied || !hasCode(res, "privileged_group") {
		t.Fatalf("a privileged group of the old members: %+v %v", res, err)
	}
	// Without members the group is new and is granted under PRM-13.
	if _, err := b.c.Grant(ctx, b.op, GrantRequest{Op: "ungrant", Subject: b.alice, Group: "hello"}); err != nil {
		t.Fatal(err)
	}
	if res, err := b.c.Apply(ctx, farmOf(b.op), ApplyRequest{Commit: "4444444444444444444444444444444444444444", Manifests: ms}); err != nil || !res.Applied {
		t.Fatalf("a new privileged group without members: %+v %v", res, err)
	}
}

// PRM-13: a grant takes the apply lock in shared mode, so it waits while an apply holds it.
func TestGrantWaitsForApply(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newBench(t)
	tx, err := b.c.st.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(7300111)`); err != nil {
		t.Fatal(err)
	}
	short, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	_, err = b.c.Grant(short, b.op, GrantRequest{Op: "grant", Subject: b.alice, Group: "hello"})
	cancel()
	if err == nil {
		t.Fatal("a grant ran while an apply held the lock")
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := b.c.Grant(ctx, b.op, GrantRequest{Op: "grant", Subject: b.alice, Group: "hello"}); err != nil {
		t.Fatalf("a grant after the apply: %v", err)
	}
}
