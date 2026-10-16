// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"errors"
	"sync/atomic"

	"github.com/jackc/pgx/v5"
)

// The store faults of the test-control operation store.fault (spec/ports/test-control.md,
// OBS-8): only the core of a test bench sets them, and a production core never does. unavailable
// makes every transaction and Ping fail as a store that does not answer; before_commit makes the
// next count transactions fail after their work, so they roll back as a commit that never
// reached the store. Statements on the pool outside a transaction are not faulted.

// ErrFaultUnavailable is the error of a transaction while the store is faulted unavailable.
var ErrFaultUnavailable = errors.New("store: unavailable (test bench fault)")

// ErrFaultCommit is the error of a transaction that a before_commit fault rolled back.
var ErrFaultCommit = errors.New("store: the commit failed (test bench fault)")

type faults struct {
	unavailable atomic.Bool
	failCommits atomic.Int64
}

// wrap returns fn with the before_commit fault: when fn succeeds and a commit failure is due,
// the transaction rolls back with ErrFaultCommit.
func (f *faults) wrap(fn func(tx pgx.Tx) error) func(tx pgx.Tx) error {
	return func(tx pgx.Tx) error {
		if err := fn(tx); err != nil {
			return err
		}
		for {
			n := f.failCommits.Load()
			if n <= 0 {
				return nil
			}
			if f.failCommits.CompareAndSwap(n, n-1) {
				return ErrFaultCommit
			}
		}
	}
}

// SetUnavailable makes the store answer as unavailable, or answer again.
func (s *Store) SetUnavailable(on bool) { s.faults.unavailable.Store(on) }

// FailCommits makes the next n transactions fail before their commit.
func (s *Store) FailCommits(n int64) { s.faults.failCommits.Store(max(n, 0)) }
