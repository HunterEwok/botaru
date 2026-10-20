// SPDX-License-Identifier: AGPL-3.0-only

package testbench

import (
	"context"
	"testing"

	"github.com/HunterEwok/botaru/impl/internal/pgtest"
	"github.com/HunterEwok/botaru/impl/internal/store"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

// Two databases of MigratedDSN carry every migration and are separate copies: a write to one
// is not seen in the other.
func TestMigratedDSN(t *testing.T) {
	ctx := context.Background()
	open := func() *store.Store {
		st, err := store.Open(ctx, MigratedDSN(t))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(st.Close)
		if err := st.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
		return st
	}
	a, b := open(), open()
	fresh, err := store.Open(ctx, pgtest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	if err := fresh.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	count := func(st *store.Store) int {
		var n int
		if err := st.Pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if want := count(fresh); count(a) != want || count(b) != want {
		t.Fatalf("migrations: copies %d and %d, fresh store %d", count(a), count(b), want)
	}
	if _, err := a.Init(ctx, true); err != nil {
		t.Fatal(err)
	}
	// The seeded farm manager seat is vacant since the copy, not since the template was built.
	var age float64
	if err := b.Pool.QueryRow(ctx, `SELECT extract(epoch FROM now() - unheld_since) FROM seats WHERE id = 'farm-manager'`).Scan(&age); err != nil || age < 0 || age > 60 {
		t.Fatalf("the farm manager seat is vacant for %v s: %v", age, err)
	}
	var n int
	if err := b.Pool.QueryRow(ctx, `SELECT count(*) FROM farm_meta`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("a write to one copy reached the other: %d %v", n, err)
	}
}
