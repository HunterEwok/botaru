// SPDX-License-Identifier: AGPL-3.0-only

package testbench

import (
	"context"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/pgtest"
	"github.com/HunterEwok/botaru/impl/internal/store"
)

// MigratedDSN returns a new database for this test with every store migration applied: a copy
// of a template that the test process migrates once (pgtest.TemplateDSN), named by a hash of
// the migrations. A bench still runs store.Migrate on it, which then applies nothing. The
// tests of the migrations themselves take an empty database from pgtest.DSN.
//
// The rows a migration seeds with now() carry the time the template was built; MigratedDSN sets
// them to the time of the copy, as a migration run for this test would have: the vacant farm
// manager seat (0032) raises alarm.manager.absent five minutes after unheld_since.
func MigratedDSN(t testing.TB) string {
	t.Helper()
	dsn := pgtest.TemplateDSN(t, migrationsKey(), func(ctx context.Context, dsn string) error {
		st, err := store.Open(ctx, dsn)
		if err != nil {
			return err
		}
		// Closing the pool closes its connections: PostgreSQL copies no database that has one.
		defer st.Close()
		return st.Migrate(ctx)
	})
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("testbench: %v", err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, `UPDATE seats SET updated_at = now(), unheld_since = now() WHERE id = 'farm-manager' AND state = 'vacant'`); err != nil {
		t.Fatalf("testbench: %v", err)
	}
	return dsn
}

var migrationsKey = sync.OnceValue(store.MigrationsKey)
