// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// waker wakes long-polling claims when new work appears. Transactions that create work call
// pg_notify; a listener connection turns notifications into a broadcast. Without the listener
// the waiters still poll on a short timer, so a lost notification only adds latency.
type waker struct {
	mu sync.Mutex
	ch chan struct{}
}

func newWaker() *waker { return &waker{ch: make(chan struct{})} }

func (w *waker) wait() <-chan struct{} {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.ch
}

func (w *waker) broadcast() {
	w.mu.Lock()
	defer w.mu.Unlock()
	close(w.ch)
	w.ch = make(chan struct{})
}

const notifyChannel = "botaru_work"

func notifyWork(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `SELECT pg_notify('`+notifyChannel+`', '')`)
	return err
}

// listen keeps a LISTEN connection until ctx ends, reconnecting after failures. The connection
// is taken out of the pool, because a connection that listened must not serve other queries.
func (c *Core) listen(ctx context.Context) {
	for ctx.Err() == nil {
		pc, err := c.st.Pool.Acquire(ctx)
		if err != nil {
			sleepCtx(ctx, time.Second)
			continue
		}
		conn := pc.Hijack()
		if _, err := conn.Exec(ctx, "LISTEN "+notifyChannel); err == nil {
			// Work may have appeared while there was no listener.
			c.wake.broadcast()
			for {
				if _, err := conn.WaitForNotification(ctx); err != nil {
					break
				}
				c.wake.broadcast()
			}
		}
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = conn.Close(closeCtx)
		cancel()
		sleepCtx(ctx, time.Second)
	}
}

func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
