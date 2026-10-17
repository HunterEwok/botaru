// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
)

// The approval loop: expiry, reminders, closing and the recheck of the initiator (APR-6, APR-16).

// approvalLoop is the approval-expiry loop: the cards of an approval go out once the attempt
// that asked has ended (the attempt does not wait for the decision, D-41), with the context
// line of its result; a pending approval under a no_action hold is canceled, an overdue one
// expires, and at half of the deadline the approvers get one reminder (APR-12, APR-19, HLD-3).
func (c *Core) approvalLoop(ctx context.Context) error {
	cat, err := c.Catalog(ctx)
	if err != nil {
		return err
	}
	// A memory record card that expired without a decision discards its record (MEM-3).
	if err := c.tx(ctx, func(tx pgx.Tx, now time.Time) error { return c.expireMemory(ctx, tx, now) }); err != nil {
		return err
	}
	// The approvals that wait for a busy resource run once it is free (ORC-12).
	if err := c.releaseWaitingApprovals(ctx, cat); err != nil {
		return err
	}
	var list []struct{ id, tenant string }
	err = c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		list = list[:0]
		rows, err := tx.Query(ctx, `SELECT id, tenant FROM approvals WHERE state = 'pending' ORDER BY created_at, id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var x struct{ id, tenant string }
			if err := rows.Scan(&x.id, &x.tenant); err != nil {
				return err
			}
			list = append(list, x)
		}
		return rows.Err()
	})
	if err != nil {
		return err
	}
	for _, x := range list {
		if err := c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
			a, err := loadApproval(ctx, tx, x.tenant, x.id, true)
			if err != nil {
				return err
			}
			if a.State != "pending" {
				return nil
			}
			tv := cat.Tenants[a.Tenant]
			if tv == nil {
				return nil
			}
			typ := ""
			if a.SourceTask != "" {
				_ = tx.QueryRow(ctx, `SELECT type FROM tasks WHERE id = $1`, a.SourceTask).Scan(&typ)
			}
			scopes, err := c.approvalHoldScopes(ctx, tx, cat, a)
			if err != nil {
				return err
			}
			// no_action wins over pause_action (HLD-8).
			if held, err := heldBy(ctx, tx, now, []string{"no_action"}, scopes...); err != nil {
				return err
			} else if held != "" {
				return c.closeApproval(ctx, tx, now, a, "canceled", "held")
			}
			// A paused approval does not expire and gets no reminder, unless it has been pending
			// under pause_action for 7 days in total (HLD-8).
			if paused, err := c.syncPause(ctx, tx, now, a, scopes); err != nil {
				return err
			} else if paused {
				if a.pausedTooLong(now) {
					return c.closeApproval(ctx, tx, now, a, "expired", "expired")
				}
				return nil
			}
			if !a.ExpiresAt.After(now) {
				return c.closeApproval(ctx, tx, now, a, "expired", "expired")
			}
			if a.CardsAt == nil {
				return c.cardsWhenAttemptEnds(ctx, tx, now, cat, tv, a, typ)
			}
			half := a.CardsAt.Add(a.ExpiresAt.Sub(*a.CardsAt) / 2)
			if a.RemindedAt == nil && !now.Before(half) {
				if _, err := tx.Exec(ctx, `UPDATE approvals SET reminded_at = $2, updated_at = $2 WHERE id = $1`, a.ID, now); err != nil {
					return err
				}
				_, err := c.remindApprovers(ctx, tx, now, tv, a)
				return err
			}
			return nil
		}); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// One bad row does not stop the expiry and reminders of the others.
			c.log.Error("an approval pass failed", "tenant", x.tenant, "approval", x.id, "error", err)
		}
	}
	return nil
}

// cardsWhenAttemptEnds plans the cards once the source attempt has no active lease, with the
// context line of its result when it completed.
func (c *Core) cardsWhenAttemptEnds(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, tv *TenantView, a *approvalRow, typ string) error {
	if a.SourceTask != "" {
		var active bool
		var state string
		var result []byte
		if err := tx.QueryRow(ctx, `SELECT state, result, EXISTS (SELECT 1 FROM leases WHERE work_id = t.id AND kind = 'attempt' AND state = 'active')
			FROM tasks t WHERE id = $1`, a.SourceTask).Scan(&state, &result, &active); err != nil {
			return err
		}
		if active {
			return nil
		}
		if state == "completed" && len(result) > 0 {
			var res map[string]any
			if json.Unmarshal(result, &res) == nil {
				if ctxLine := approvalContext(cat.TaskTypes[typ], res); len(ctxLine) > 0 {
					a.Context = ctxLine
					if _, err := tx.Exec(ctx, `UPDATE approvals SET record = jsonb_set(record, '{context}', $2) WHERE id = $1`, a.ID, mustJSON(ctxLine)); err != nil {
						return err
					}
				}
			}
		}
	}
	return c.planCards(ctx, tx, now, tv, a)
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

// remindApprovers sends the approvers who have not decided a new card with the reminder line
// (APR-19); it returns how many got one.
func (c *Core) remindApprovers(ctx context.Context, tx pgx.Tx, now time.Time, tv *TenantView, a *approvalRow) (int, error) {
	people, err := approvers(ctx, tx, now, tv, a)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, p := range people {
		// One who already voted on a two approver request is not reminded (APR-16).
		if slices.ContainsFunc(a.Votes, func(v approvalVote) bool { return v.By == p }) {
			continue
		}
		var had bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM deliveries WHERE subject_kind = 'approval' AND subject_id = $1 AND kind = 'approval_card' AND recipient = $2)`,
			a.ID, p).Scan(&had); err != nil {
			return 0, err
		}
		if !had {
			continue
		}
		if err := c.planCard(ctx, tx, now, tv, a, approvalCard, p, true); err != nil {
			return 0, err
		}
		n++
	}
	return n, nil
}

// closeApproval ends a pending approval without execution: rejected, expired or canceled.
func (c *Core) closeApproval(ctx context.Context, tx pgx.Tx, now time.Time, a *approvalRow, state, reason string) error {
	if _, err := tx.Exec(ctx, `UPDATE approvals SET state = $2, reason = $3, updated_at = $4 WHERE id = $1`, a.ID, state, nullable(reason), now); err != nil {
		return err
	}
	a.State, a.Reason = state, reason
	if err := c.appendEvent(ctx, tx, now, a.Tenant, Event{Type: "approval." + state, Subject: a.ID,
		Data: map[string]any{"approval": a.ID, "action": a.Action, "reason": reason}}); err != nil {
		return err
	}
	if err := c.planCardUpdates(ctx, tx, now, a); err != nil {
		return err
	}
	return c.scriptApprovalMoved(ctx, tx, now, a)
}

// cancelApprovalsOf cancels the pending approvals of an initiator, as P2 principal.off or a
// lost right does (APR-6).
func (c *Core) cancelApprovalsOf(ctx context.Context, tx pgx.Tx, now time.Time, tenant, initiator, reason string) error {
	rows, err := tx.Query(ctx, `SELECT `+approvalColumns+` FROM approvals WHERE tenant = $1 AND initiator = $2 AND state IN ('pending', 'approved') ORDER BY id FOR UPDATE`, tenant, initiator)
	if err != nil {
		return err
	}
	var as []*approvalRow
	for rows.Next() {
		a, err := scanApproval(rows)
		if err != nil {
			rows.Close()
			return err
		}
		as = append(as, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, a := range as {
		if err := c.closeApproval(ctx, tx, now, a, "canceled", reason); err != nil {
			return err
		}
	}
	return nil
}

// recheckApprovalsOf cancels the pending approvals of an initiator who is no longer active
// (person_inactive) or lost action.request for the action (rights_revoked), in the
// transaction of the change (APR-6).
func (c *Core) recheckApprovalsOf(ctx context.Context, tx pgx.Tx, now time.Time, tv *TenantView, principal string) error {
	rows, err := tx.Query(ctx, `SELECT `+approvalColumns+` FROM approvals WHERE tenant = $1 AND initiator = $2 AND state IN ('pending', 'approved') ORDER BY id FOR UPDATE`, tv.Name, principal)
	if err != nil {
		return err
	}
	var as []*approvalRow
	for rows.Next() {
		a, err := scanApproval(rows)
		if err != nil {
			rows.Close()
			return err
		}
		as = append(as, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(as) == 0 {
		return err
	}
	p, err := loadPrincipal(ctx, tx, now, tv.Name, principal)
	if errors.Is(err, pgx.ErrNoRows) {
		p, err = nil, nil
	}
	if err != nil {
		return err
	}
	for _, a := range as {
		switch {
		case p == nil || p.State != "active":
			err = c.closeApproval(ctx, tx, now, a, "canceled", "person_inactive")
		case !hasCap(tv, groupClosure(tv, p.Groups), requestCap(a.Action, a.Args)):
			err = c.closeApproval(ctx, tx, now, a, "canceled", "rights_revoked")
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// initiatorLost is the cancel reason when the initiator of an approval is no longer active
// (person_inactive) or lost action.request (rights_revoked), or "" (APR-6, APR-16).
func (c *Core) initiatorLost(ctx context.Context, tx pgx.Tx, now time.Time, tv *TenantView, a *approvalRow) (string, error) {
	if a.Initiator == "" || a.Initiator == "system" {
		return "", nil
	}
	p, err := loadPrincipal(ctx, tx, now, a.Tenant, a.Initiator)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && p.State != "active" {
		return "person_inactive", nil
	}
	if err != nil {
		return "", err
	}
	if !hasCap(tv, groupClosure(tv, p.Groups), requestCap(a.Action, a.Args)) {
		return "rights_revoked", nil
	}
	return "", nil
}
