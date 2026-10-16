// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/ids"
)

// rotationWindow is how long the previous worker token stays valid after worker.rotate: the
// binding leaves the value to the implementation, and the core uses the window of the driver's
// P2 secret (IDN-4, docs/operations.md "Rotating secrets").
const rotationWindow = 24 * time.Hour

// RotateResult is p2-worker-rotate-response.
type RotateResult struct {
	Token         string `json:"token"`
	KeyID         string `json:"key_id,omitempty"`
	OldValidUntil string `json:"old_valid_until"`
}

// lockWorker reads the state of a worker of the operator's tenant under its row lock; a worker
// of another tenant is not_found.
func lockWorker(ctx context.Context, tx pgx.Tx, op Operator, id string) (string, error) {
	var tenant, state string
	err := tx.QueryRow(ctx, `SELECT tenant, state FROM workers WHERE id = $1 FOR UPDATE`, id).Scan(&tenant, &state)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && tenant != op.Tenant {
		return "", refuse("not_found", "no such worker in the tenant")
	}
	return state, err
}

// WorkerRotate is P2 worker.rotate (IDN-4): a new P1 token, shown once and stored as a keyed
// hash; the old token stays valid until old_valid_until, and a token rotated before that is
// dropped at once. The worker's state, leases and enroll record do not change.
func (c *Core) WorkerRotate(ctx context.Context, op Operator, req WorkerRequest) (*RotateResult, error) {
	token := "bw1_" + ids.Secret(32)
	var out *RotateResult
	err := c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		state, err := lockWorker(ctx, tx, op, req.Worker)
		if err != nil {
			return err
		}
		if state == "revoked" {
			return refuse("subject_closed", "the worker is revoked")
		}
		until := now.Add(rotationWindow)
		if _, err := tx.Exec(ctx, `UPDATE workers SET prev_token_hash = token_hash, prev_valid_until = $2, token_hash = $3 WHERE id = $1`,
			req.Worker, until, c.hashSecret(token)); err != nil {
			return err
		}
		if _, err := c.appendAudit(ctx, tx, now, Audit{
			Tenant: op.Tenant, Kind: "command", Actor: Actor{Principal: op.Principal, Via: "farmctl"},
			Command: "worker.rotate", Target: &Target{Kind: "worker", ID: req.Worker}, OK: true,
			After: map[string]any{"old_valid_until": fmtTime(until)},
		}); err != nil {
			return err
		}
		out = &RotateResult{Token: token, KeyID: c.cfg.KeyID, OldValidUntil: fmtTime(until)}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// WorkerUnblock is P2 worker.unblock: a worker that hello blocked for a mismatch with its
// enroll record or with the manifests (DEP-2, INJ-2) goes back to enrolled, so its next hello
// is checked again from the start; it does not become active by this operation. A worker that
// is not blocked is left as it is and the answer repeats its state (IDM-3).
func (c *Core) WorkerUnblock(ctx context.Context, op Operator, req WorkerRequest) (*Done, error) {
	var out Done
	err := c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		state, err := lockWorker(ctx, tx, op, req.Worker)
		if err != nil {
			return err
		}
		out = Done{OK: true, ID: req.Worker, State: state}
		if state != "blocked" {
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE workers SET state = 'enrolled' WHERE id = $1`, req.Worker); err != nil {
			return err
		}
		seq, err := c.appendAudit(ctx, tx, now, Audit{
			Tenant: op.Tenant, Kind: "command", Actor: Actor{Principal: op.Principal, Via: "farmctl"},
			Command: "worker.unblock", Target: &Target{Kind: "worker", ID: req.Worker}, OK: true,
			Before: map[string]any{"state": "blocked"}, After: map[string]any{"state": "enrolled"},
		})
		out.State, out.AuditSeq = "enrolled", seq
		return err
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}
