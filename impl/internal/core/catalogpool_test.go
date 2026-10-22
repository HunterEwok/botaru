// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestCatalogFullPool: the catalog is read while every connection of the main pool is taken,
// as when each request holding one waits on the catalog inside its transaction; the read
// must not wait for a main connection.
func TestCatalogFullPool(t *testing.T) {
	b := newBench(t)
	ctx := context.Background()
	pool := b.c.st.Pool
	var held []*pgxpool.Conn
	defer func() {
		for _, c := range held {
			c.Release()
		}
	}()
	for range pool.Config().MaxConns {
		c, err := pool.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, c)
	}
	b.c.cat.reset()
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cat, err := b.c.Catalog(rctx)
	if err != nil {
		t.Fatalf("catalog with the main pool full: %v", err)
	}
	if cat.SetHash == "" {
		t.Fatal("the catalog has no set hash")
	}
}
