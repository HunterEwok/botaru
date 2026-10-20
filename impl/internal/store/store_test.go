// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"errors"
	"testing"

	"github.com/HunterEwok/botaru/impl/internal/pgtest"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

// TestMigrateAndInit: the migrations are idempotent, the test bench flag of the store never
// changes after the first start (APR-9), and the audit is append-only: an UPDATE or a DELETE of
// an audit record fails even with direct store access (AUD-3).
func TestMigrateAndInit(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, pgtest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(ctx); err != nil { // idempotent
		t.Fatal(err)
	}
	m, err := s.Init(ctx, false)
	if err != nil || m.Epoch != 1 || m.TestBench {
		t.Fatalf("init: %+v %v", m, err)
	}
	// A production store never becomes a test bench (APR-9).
	if _, err := s.Init(ctx, true); !errors.Is(err, ErrTestBenchMismatch) {
		t.Fatalf("want mismatch, got %v", err)
	}
	// And a test bench store never runs a core without the test bench key.
	if _, err := s.Pool.Exec(ctx, `UPDATE farm_meta SET test_bench = true`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Init(ctx, false); !errors.Is(err, ErrTestBenchMismatch) {
		t.Fatalf("want mismatch on a test bench store, got %v", err)
	}
	if _, err := s.Pool.Exec(ctx, `UPDATE farm_meta SET test_bench = false`); err != nil {
		t.Fatal(err)
	}
	// The audit is append-only (AUD-3).
	if _, err := s.Pool.Exec(ctx, `INSERT INTO audit (tenant, epoch, seq, at, record, hash) VALUES ('demo', 1, 1, now(), '{}', 'x')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(ctx, `UPDATE audit SET hash = 'y'`); err == nil {
		t.Fatal("audit row changed")
	}
	if _, err := s.Pool.Exec(ctx, `DELETE FROM audit`); err == nil {
		t.Fatal("audit row deleted")
	}
	now, err := Now(ctx, s.Pool)
	if err != nil || now.IsZero() {
		t.Fatal(err)
	}
}

// TestMigrateUpToDate: on a store with every migration applied Migrate applies nothing and
// changes neither the migration list nor the schema, which the test templates of
// pgtest.TemplateDSN rely on.
func TestMigrateUpToDate(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, pgtest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	snapshot := func() string {
		var out string
		err := s.Pool.QueryRow(ctx, `SELECT
			(SELECT string_agg(name || '@' || applied_at::text, ',' ORDER BY name) FROM schema_migrations) || '|' ||
			(SELECT string_agg(oid::text || ':' || relname, ',' ORDER BY oid) FROM pg_class WHERE relnamespace = 'public'::regnamespace)`).Scan(&out)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	before := snapshot()
	names, err := migrationNames()
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&n); err != nil || n != len(names) {
		t.Fatalf("applied %d of %d migrations: %v", n, len(names), err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if after := snapshot(); after != before {
		t.Fatalf("a second Migrate changed the store:\nbefore %s\nafter  %s", before, after)
	}
	if k := MigrationsKey(); len(k) != 64 || k != MigrationsKey() {
		t.Fatalf("migrations key %q", k)
	}
}
