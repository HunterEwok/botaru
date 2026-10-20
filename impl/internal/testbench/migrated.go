// SPDX-License-Identifier: AGPL-3.0-only

package testbench

import (
	"context"
	"testing"

	"github.com/HunterEwok/botaru/impl/internal/pgtest"
	"github.com/HunterEwok/botaru/impl/internal/store"
)

// MigratedDSN returns a new database for this test with every store migration applied: a copy
// of a template that the test process migrates once (pgtest.TemplateDSN), named by a hash of
// the migrations. A bench still runs store.Migrate on it, which then applies nothing. The
// tests of the migrations themselves take an empty database from pgtest.DSN.
func MigratedDSN(t testing.TB) string {
	t.Helper()
	return pgtest.TemplateDSN(t, store.MigrationsKey(), func(ctx context.Context, dsn string) error {
		st, err := store.Open(ctx, dsn)
		if err != nil {
			return err
		}
		// Closing the pool closes its connections: PostgreSQL copies no database that has one.
		defer st.Close()
		return st.Migrate(ctx)
	})
}
