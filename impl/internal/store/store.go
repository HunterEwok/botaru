// SPDX-License-Identifier: AGPL-3.0-only

// Package store is the PostgreSQL adapter of the C1 farm store (D-03): the connection pool,
// the schema migrations, transactions and the store clock. Only the core holds the store
// credentials (C1 sheet, "Guarantees").
package store

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Store is the open farm store.
type Store struct {
	Pool *pgxpool.Pool
}

// Open connects to the store; it does not migrate.
func Open(ctx context.Context, dsn string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	cfg.ConnConfig.RuntimeParams["timezone"] = "UTC"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	return &Store{Pool: pool}, nil
}

// Close closes the pool.
func (s *Store) Close() { s.Pool.Close() }

// Ping reports whether the store answers.
func (s *Store) Ping(ctx context.Context) error { return s.Pool.Ping(ctx) }

// Migrate applies the embedded migrations that are not applied yet, under an advisory lock, so
// that two core copies starting at once do not apply a migration twice.
func (s *Store) Migrate(ctx context.Context) error {
	conn, err := s.Pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(7300101)`); err != nil {
		return err
	}
	defer func() { _, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock(7300101)`) }()
	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (name text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT clock_timestamp())`); err != nil {
		return err
	}
	names, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(names)
	for _, n := range names {
		var done bool
		if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE name = $1)`, n).Scan(&done); err != nil {
			return err
		}
		if done {
			continue
		}
		sql, err := migrations.ReadFile(n)
		if err != nil {
			return err
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("store: migration %s: %w", n, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (name) VALUES ($1)`, n); err != nil {
			_ = tx.Rollback(ctx)
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
	}
	return nil
}

// Meta is the farm_meta row.
type Meta struct {
	Epoch     int64
	TestBench bool
	CreatedAt time.Time
}

// ErrTestBenchMismatch means the test bench key and the store flag disagree (APR-9).
var ErrTestBenchMismatch = errors.New("store: test bench setting does not match the store")

// Init writes the farm_meta row at the first start and checks it at every later start: the
// test bench flag is set only at the first start and never changes (test-control sheet).
func (s *Store) Init(ctx context.Context, testBench bool) (Meta, error) {
	var m Meta
	err := s.Tx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO farm_meta (id, epoch, test_bench) VALUES (1, 1, $1) ON CONFLICT (id) DO NOTHING`, testBench)
		if err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT epoch, test_bench, created_at FROM farm_meta WHERE id = 1`).Scan(&m.Epoch, &m.TestBench, &m.CreatedAt)
	})
	if err != nil {
		return m, err
	}
	if testBench && !m.TestBench {
		return m, fmt.Errorf("%w: the core has a test bench key, but the store was not created as a test bench store", ErrTestBenchMismatch)
	}
	if testBench {
		var n int
		if err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM tenants WHERE kind <> 'test'`).Scan(&n); err != nil {
			return m, err
		}
		if n > 0 {
			return m, fmt.Errorf("%w: the store has a tenant that is not kind: test", ErrTestBenchMismatch)
		}
	}
	return m, nil
}

// Tx runs fn in a READ COMMITTED transaction. Work is serialized by row locks taken inside fn;
// a serialization or deadlock failure is retried a few times.
func (s *Store) Tx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	var err error
	for attempt := 0; attempt < 5; attempt++ {
		err = pgx.BeginFunc(ctx, s.Pool, fn)
		if !retryable(err) {
			return err
		}
	}
	return err
}

func retryable(err error) bool {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code == "40001" || pe.Code == "40P01"
	}
	return false
}

// IsUniqueViolation reports a unique constraint failure, optionally of a named constraint.
func IsUniqueViolation(err error, constraint string) bool {
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "23505" {
		return constraint == "" || strings.Contains(pe.ConstraintName, constraint)
	}
	return false
}

// Now returns the store clock inside a transaction (TIM-1), truncated to microseconds.
func Now(ctx context.Context, q Querier) (time.Time, error) {
	var t time.Time
	if err := q.QueryRow(ctx, `SELECT botaru_now()`).Scan(&t); err != nil {
		return t, err
	}
	return t.UTC(), nil
}

// Querier is a connection, a pool or a transaction.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}
