// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/ids"
	"github.com/HunterEwok/botaru/impl/internal/store"
)

// Bot identities (IDN-8..IDN-12, spec/lifecycles/identity.md). An identity is a core record
// with a permanent id, a tenant, a role and a binding to a worker or a seat with an epoch.
// Only the core creates, binds and retires identities; bots have no operation on them. An
// identity grants no permissions: authorize never reads it (IDN-9).

const (
	// The charter defaults (GOV-33): an unbound identity leaves after unbound_ttl, a retired
	// one keeps its sealed data for seal_ttl.
	defaultUnboundTTL = 30 * 24 * time.Hour
	defaultSealTTL    = 30 * 24 * time.Hour
)

// botRow is a bot identity as the store keeps it.
type botRow struct {
	ID, Tenant, Role, State, Mode, Worker, Seat string
	Epoch                                       int
	Record, Retired                             map[string]any
	UnboundAt, SealedUntil                      *time.Time
	CreatedAt                                   time.Time
}

const botCols = `id, tenant, role, state, mode, coalesce(worker_id, ''), coalesce(seat, ''), epoch, record, retired, unbound_at, sealed_until, created_at`

func scanBot(row pgx.Row) (*botRow, error) {
	var b botRow
	var rec, ret []byte
	if err := row.Scan(&b.ID, &b.Tenant, &b.Role, &b.State, &b.Mode, &b.Worker, &b.Seat, &b.Epoch, &rec, &ret, &b.UnboundAt, &b.SealedUntil, &b.CreatedAt); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(rec, &b.Record); err != nil {
		return nil, err
	}
	if len(ret) > 0 {
		if err := json.Unmarshal(ret, &b.Retired); err != nil {
			return nil, err
		}
	}
	return &b, nil
}

func loadBot(ctx context.Context, q store.Querier, id string, lock bool) (*botRow, error) {
	sql := `SELECT ` + botCols + ` FROM bots WHERE id = $1`
	if lock {
		sql += ` FOR UPDATE`
	}
	b, err := scanBot(q.QueryRow(ctx, sql, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return b, err
}

// view is the bot record of spec/model/bot.schema.json.
func (b *botRow) view() map[string]any {
	binding := map[string]any{"mode": b.Mode, "worker": nil, "seat": nil, "epoch": b.Epoch}
	if b.Worker != "" && b.Mode == "worker" && (b.State == "active" || b.State == "suspended") {
		binding["worker"] = b.Worker
	}
	if b.Seat != "" {
		binding["seat"] = b.Seat
	}
	out := map[string]any{"id": b.ID, "tenant": b.Tenant, "role": b.Role, "state": b.State, "binding": binding,
		"created_at": fmtTime(b.CreatedAt), "record": b.Record, "retired": nil}
	if b.Retired != nil {
		out["retired"] = b.Retired
	}
	if b.SealedUntil != nil {
		out["sealed_until"] = fmtTime(*b.SealedUntil)
	}
	return out
}

// hasModel says whether workers of a role get a bot identity: a role with a model slot.
func hasModel(cat *Catalog, role string) bool {
	r, ok := cat.Roles[role]
	return ok && len(r.Models) > 0
}

// insertBot creates an identity with epoch 1: bound to a worker, or to a seat.
func (c *Core) insertBot(ctx context.Context, tx pgx.Tx, now time.Time, tenant, role, mode, worker, seat, cause string) (string, error) {
	id := ids.New(ids.Bot)
	if _, err := tx.Exec(ctx, `INSERT INTO bots (id, tenant, role, state, mode, worker_id, seat, epoch, created_at, updated_at)
		VALUES ($1, $2, $3, 'active', $4, $5, $6, 1, $7, $7)`, id, tenant, role, mode, nullable(worker), nullable(seat), now); err != nil {
		return "", err
	}
	_, err := c.appendAudit(ctx, tx, now, Audit{
		Tenant: tenant, Kind: "command", Actor: Actor{Principal: "system", Via: "core"},
		Command: "bot.create", Target: &Target{Kind: "bot", ID: id}, OK: true,
		After: map[string]any{"role": role, "mode": mode, "cause": cause},
	})
	return id, err
}

// workerBot is the identity of a worker: the bound one, or a new one for a live worker of a
// role with a model that has none (a worker enrolled before identities, or one whose
// identity retired). A worker without a model role has no identity: nil. The row is read FOR
// SHARE, so a retirement waits for the claim and its fence then finds the new lease.
func (c *Core) workerBot(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, w *Worker) (*botRow, error) {
	b, err := scanBot(tx.QueryRow(ctx, `SELECT `+botCols+` FROM bots WHERE worker_id = $1 AND mode = 'worker' AND state IN ('active', 'suspended') FOR SHARE`, w.ID))
	if err == nil {
		return b, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	if (w.Kind != "agent" && w.Kind != "job") || !hasModel(cat, w.Role) || w.State == "revoked" {
		return nil, nil
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(7300130, hashtext($1))`, w.ID); err != nil {
		return nil, err
	}
	// A second claim of the same worker may have created it meanwhile.
	b, err = scanBot(tx.QueryRow(ctx, `SELECT `+botCols+` FROM bots WHERE worker_id = $1 AND mode = 'worker' AND state IN ('active', 'suspended') FOR SHARE`, w.ID))
	if err == nil {
		return b, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	id, err := c.insertBot(ctx, tx, now, w.Tenant, w.Role, "worker", w.ID, "", "worker.enroll")
	if err != nil {
		return nil, err
	}
	return loadBot(ctx, tx, id, true)
}

// enrollBot binds the identity of a newly enrolled worker (IDN-8): a new one, or with the bot
// parameter an unbound identity of the same tenant and role, with the epoch + 1; it is
// suspended when a hold by a person on the bot scope stands.
func (c *Core) enrollBot(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, w Worker, botID string) (string, error) {
	if botID == "" {
		if (w.Kind != "agent" && w.Kind != "job") || !hasModel(cat, w.Role) {
			return "", nil
		}
		id, err := c.insertBot(ctx, tx, now, w.Tenant, w.Role, "worker", w.ID, "", "worker.enroll")
		if err != nil {
			return "", err
		}
		// The first identity people enroll in a role takes the waiting fact offers (MEM-7).
		return id, c.pickUpOffers(ctx, tx, now, cat, w.Tenant, w.Role, "", id)
	}
	b, err := loadBot(ctx, tx, botID, true)
	if err != nil {
		return "", err
	}
	if b == nil || b.Tenant != w.Tenant {
		return "", refuse("not_found", "no such bot identity in the tenant")
	}
	if b.Role != w.Role || b.State != "unbound" {
		return "", refuse("forbidden", "only an unbound identity of the same role is bound again")
	}
	state := "active"
	if held, err := botHeld(ctx, tx, now, b); err != nil {
		return "", err
	} else if held {
		state = "suspended"
	}
	if _, err := tx.Exec(ctx, `UPDATE bots SET state = $2, worker_id = $3, epoch = epoch + 1, unbound_at = NULL, updated_at = $4 WHERE id = $1`,
		b.ID, state, w.ID, now); err != nil {
		return "", err
	}
	_, err = c.appendAudit(ctx, tx, now, Audit{
		Tenant: b.Tenant, Kind: "command", Actor: Actor{Principal: "system", Via: "core"},
		Command: "bot.bind", Target: &Target{Kind: "bot", ID: b.ID}, OK: true,
		Before: map[string]any{"state": b.State, "epoch": b.Epoch}, After: map[string]any{"state": state, "epoch": b.Epoch + 1, "worker": w.ID},
	})
	return b.ID, err
}

// botHeld says whether a hold by a person stands on the bot scope of an identity: only such a
// hold suspends it (HLD-1); a hold of the farm manager seat holder only delays its tasks.
func botHeld(ctx context.Context, q store.Querier, now time.Time, b *botRow) (bool, error) {
	var held bool
	err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM holds WHERE state = 'active' AND (until IS NULL OR until > $4)
		AND scope_kind = 'bot' AND scope_tenant = $1 AND scope_name = $2 AND author_kind = ANY($3))`,
		b.Tenant, b.ID, []string{"operator", "tenant_admin"}, now).Scan(&held)
	return held, err
}

// fenceAttempts ends the running attempts that match without spending them: the leases are
// revoked with the reason, the tasks go back to the queue, so the next call of the holder is
// fenced (IDN-10, IDN-11).
func (c *Core) fenceAttempts(ctx context.Context, tx pgx.Tx, now time.Time, reason, where string, args ...any) error {
	rows, err := tx.Query(ctx, `SELECT work_id, lease_no, kind, tenant, worker_id, epoch, state, lease_until FROM leases
		WHERE state = 'active' AND kind = 'attempt' AND `+where+` ORDER BY work_id FOR UPDATE`, args...)
	if err != nil {
		return err
	}
	var ls []leaseRow
	for rows.Next() {
		var l leaseRow
		if err := rows.Scan(&l.WorkID, &l.LeaseNo, &l.Kind, &l.Tenant, &l.WorkerID, &l.Epoch, &l.State, &l.Until); err != nil {
			rows.Close()
			return err
		}
		ls = append(ls, l)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for i := range ls {
		l := &ls[i]
		res := &HeartbeatResult{CancelRequested: true}
		if err := closeLease(ctx, tx, now, l, "revoked", reason, "heartbeat", nil, res); err != nil {
			return err
		}
		if err := c.requeue(ctx, tx, now, l, now); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE tasks SET reason = $2 WHERE id = $1`, l.WorkID, reason); err != nil {
			return err
		}
		if _, err := c.appendAudit(ctx, tx, now, Audit{
			Tenant: l.Tenant, Kind: "command", Actor: Actor{Principal: "system", Via: "system", Task: l.WorkID},
			Command: "attempt.cancel", Target: &Target{Kind: "task", ID: l.WorkID}, OK: true, Reason: reason,
			After: map[string]any{"lease_no": l.LeaseNo, "spent": false},
		}); err != nil {
			return err
		}
	}
	return nil
}

// charterDuration is a duration field of the tenant charter, or the default.
func charterDuration(cat *Catalog, tenant, block, field string, def time.Duration) time.Duration {
	gov := tenantSpecOf(cat, "Governance", tenant)
	m, _ := gov[block].(map[string]any)
	return durationOr(asString(m[field]), def)
}

// retireBot retires an identity for a reason from bot-retire-reasons.yaml (IDN-11): the state
// is terminal, its running attempts are fenced, its data is sealed for seal_ttl, and a live
// worker of a worker binding gets a new identity of the same role, which is the successor; a
// seat holder's successor is the next holder (successor). The tombstone keeps the reason,
// the time and the successor.
func (c *Core) retireBot(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, b *botRow, reason, successor, actor string) (string, error) {
	if b.State == "retired" || b.State == "erased" {
		return "", nil
	}
	if b.Mode == "worker" && b.Worker != "" && b.State != "unbound" && successor == "" {
		var wstate string
		err := tx.QueryRow(ctx, `SELECT state FROM workers WHERE id = $1`, b.Worker).Scan(&wstate)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return "", err
		}
		if err == nil && wstate != "revoked" && hasModel(cat, b.Role) && reason != "role_removed" {
			// The worker keeps working under a new identity of the same role.
			if _, err := tx.Exec(ctx, `UPDATE bots SET worker_id = NULL WHERE id = $1`, b.ID); err != nil {
				return "", err
			}
			id, err := c.insertBot(ctx, tx, now, b.Tenant, b.Role, "worker", b.Worker, "", "bot.retire")
			if err != nil {
				return "", err
			}
			successor = id
		}
	}
	retired := map[string]any{"reason": reason, "at": fmtTime(now), "successor": nil}
	if successor != "" {
		retired["successor"] = successor
	}
	raw, err := json.Marshal(retired)
	if err != nil {
		return "", err
	}
	sealed := now.Add(charterDuration(cat, b.Tenant, "memory", "seal_ttl", defaultSealTTL))
	if _, err := tx.Exec(ctx, `UPDATE bots SET state = 'retired', retired = $2, sealed_until = $3, updated_at = $4 WHERE id = $1`, b.ID, raw, sealed, now); err != nil {
		return "", err
	}
	if err := c.fenceAttempts(ctx, tx, now, "bot_retired", `acting_bot = $1`, b.ID); err != nil {
		return "", err
	}
	// The facts go to the successor: the new identity of the worker, the next holder of the
	// seat, or the first identity enrolled in the role before the seal ends; a role that left
	// the catalog has none. Then they are sealed (MEM-4, MEM-7).
	if reason != "role_removed" && reason != "tenant_offboarded" {
		seat := ""
		if b.Mode == "seat" && successor == "" {
			seat = b.Seat
		}
		if err := c.offerFacts(ctx, tx, now, cat, b, successor, seat, sealed); err != nil {
			return "", err
		}
	}
	if err := sealFacts(ctx, tx, b); err != nil {
		return "", err
	}
	// A holder that leaves other than by a seat operation frees its seat.
	if reason != "replaced_by_human" {
		if err := c.seatVacated(ctx, tx, now, b, "bot.retire"); err != nil {
			return "", err
		}
	}
	if _, err := c.appendAudit(ctx, tx, now, Audit{
		Tenant: b.Tenant, Kind: "command", Actor: Actor{Principal: principalOr(actor), Via: "core"},
		Command: "bot.retire", Target: &Target{Kind: "bot", ID: b.ID}, OK: true, Reason: reason,
		Before: map[string]any{"state": b.State}, After: map[string]any{"state": "retired", "successor": retired["successor"], "sealed_until": fmtTime(sealed)},
	}); err != nil {
		return "", err
	}
	b.State, b.Retired, b.SealedUntil = "retired", retired, &sealed
	return successor, nil
}

// WorkerRequest is p2-worker-request.
type WorkerRequest struct {
	Op     string `json:"op"`
	Worker string `json:"worker"`
}

// WorkerRevoke is P2 worker.revoke (IDN-4, IDN-8): the token stops working, the running
// attempts go back to the queue without spending, and the worker's identity becomes unbound
// with its memory intact; it leaves as worker_gone after unbound_ttl unless enroll binds it
// again.
func (c *Core) WorkerRevoke(ctx context.Context, op Operator, req WorkerRequest) (*Done, error) {
	var out Done
	err := c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		var tenant, state string
		err := tx.QueryRow(ctx, `SELECT tenant, state FROM workers WHERE id = $1 FOR UPDATE`, req.Worker).Scan(&tenant, &state)
		if errors.Is(err, pgx.ErrNoRows) || err == nil && tenant != op.Tenant {
			return refuse("not_found", "no such worker in the tenant")
		}
		if err != nil {
			return err
		}
		out = Done{OK: true, ID: req.Worker, State: "revoked"}
		if state == "revoked" {
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE workers SET state = 'revoked' WHERE id = $1`, req.Worker); err != nil {
			return err
		}
		if err := c.fenceAttempts(ctx, tx, now, "worker_revoke", `worker_id = $1`, req.Worker); err != nil {
			return err
		}
		b, err := scanBot(tx.QueryRow(ctx, `SELECT `+botCols+` FROM bots WHERE worker_id = $1 AND mode = 'worker' AND state IN ('active', 'suspended') FOR UPDATE`, req.Worker))
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		after := map[string]any{"state": "revoked"}
		if b != nil {
			if _, err := tx.Exec(ctx, `UPDATE bots SET state = 'unbound', unbound_at = $2, epoch = epoch + 1, updated_at = $2 WHERE id = $1`, b.ID, now); err != nil {
				return err
			}
			after["bot"], after["bot_state"] = b.ID, "unbound"
		}
		seq, err := c.appendAudit(ctx, tx, now, Audit{
			Tenant: tenant, Kind: "command", Actor: Actor{Principal: op.Principal, Via: "farmctl"},
			Command: "worker.revoke", Target: &Target{Kind: "worker", ID: req.Worker}, OK: true,
			Before: map[string]any{"state": state}, After: after,
		})
		out.AuditSeq = seq
		return err
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// BotRetireRequest is p2-bot-retire-request.
type BotRetireRequest struct {
	Bot string `json:"bot"`
}

// BotRetire is P2 bot.retire with the reason human_retire (IDN-11).
func (c *Core) BotRetire(ctx context.Context, op Operator, req BotRetireRequest) (*Done, error) {
	cat, err := c.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	var out Done
	err = c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		b, err := loadBot(ctx, tx, req.Bot, true)
		if err != nil {
			return err
		}
		if b == nil || b.Tenant != op.Tenant {
			return refuse("not_found", "no such bot identity in the tenant")
		}
		out = Done{OK: true, ID: b.ID, State: b.State}
		if b.State == "retired" || b.State == "erased" {
			return nil
		}
		if _, err := c.retireBot(ctx, tx, now, cat, b, "human_retire", "", op.Principal); err != nil {
			return err
		}
		out.State = "retired"
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// BotList is P2 bot.list: the identities of the tenant with id, state and time.
func (c *Core) BotList(ctx context.Context, op Operator) (map[string]any, error) {
	rows, err := c.st.Pool.Query(ctx, `SELECT id, state, updated_at FROM bots WHERE tenant = $1 ORDER BY created_at, id LIMIT 1000`, op.Tenant)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []any{}
	for rows.Next() {
		var id, state string
		var at time.Time
		if err := rows.Scan(&id, &state, &at); err != nil {
			return nil, err
		}
		items = append(items, map[string]any{"id": id, "state": state, "at": fmtTime(at)})
	}
	return map[string]any{"items": items}, rows.Err()
}

// BotShow is P2 bot.show: the bot record; the track record holds only numbers and codes.
func (c *Core) BotShow(ctx context.Context, op Operator, id string) (map[string]any, error) {
	b, err := loadBot(ctx, c.st.Pool, id, false)
	if err != nil {
		return nil, err
	}
	if b == nil || b.Tenant != op.Tenant {
		return nil, refuse("not_found", "no such bot identity in the tenant")
	}
	return b.view(), nil
}

// identityLoop runs the identity deadlines (spec/ports/background-loops.md): the suspended
// state follows the holds of people on the bot scope, an identity whose role is no longer
// applied leaves as role_removed, an unbound one past unbound_ttl as worker_gone, and at the
// end of seal_ttl a retired identity gets a bot_retired erasure request (IDN-11, MEM-9).
func (c *Core) identityLoop(ctx context.Context) error {
	cat, err := c.Catalog(ctx)
	if err != nil {
		return err
	}
	if err := c.tx(ctx, func(tx pgx.Tx, now time.Time) error { return expireOffers(ctx, tx, now) }); err != nil {
		return err
	}
	rows, err := c.st.Pool.Query(ctx, `SELECT id FROM bots WHERE state IN ('active', 'suspended', 'unbound')
		OR state = 'retired' AND sealed_until IS NOT NULL ORDER BY id`)
	if err != nil {
		return err
	}
	list, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	var errs []error
	for _, id := range list {
		if err := c.tx(ctx, func(tx pgx.Tx, now time.Time) error { return c.identityStep(ctx, tx, now, cat, id) }); err != nil {
			errs = append(errs, fmt.Errorf("identity %s: %w", id, err))
		}
	}
	return errors.Join(errs...)
}

func (c *Core) identityStep(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, id string) error {
	b, err := loadBot(ctx, tx, id, true)
	if err != nil || b == nil {
		return err
	}
	if _, ok := cat.Tenants[b.Tenant]; !ok {
		// A tenant no longer in the catalog is offboarded or deleted; its identities leave
		// with it (tenant_offboarded arrives in v0.2).
		return nil
	}
	switch b.State {
	case "active", "suspended", "unbound":
		if _, ok := cat.Roles[b.Role]; !ok {
			_, err := c.retireBot(ctx, tx, now, cat, b, "role_removed", "", "")
			return err
		}
		if b.State == "unbound" {
			if b.UnboundAt != nil && now.Sub(*b.UnboundAt) >= charterDuration(cat, b.Tenant, "identity", "unbound_ttl", defaultUnboundTTL) {
				_, err := c.retireBot(ctx, tx, now, cat, b, "worker_gone", "", "")
				return err
			}
			return nil
		}
		held, err := botHeld(ctx, tx, now, b)
		if err != nil {
			return err
		}
		next := map[bool]string{true: "suspended", false: "active"}[held]
		if next != b.State {
			if _, err := tx.Exec(ctx, `UPDATE bots SET state = $2, updated_at = $3 WHERE id = $1`, b.ID, next, now); err != nil {
				return err
			}
			if _, err := c.appendAudit(ctx, tx, now, Audit{
				Tenant: b.Tenant, Kind: "command", Actor: Actor{Principal: "system", Via: "core"},
				Command: "bot.hold", Target: &Target{Kind: "bot", ID: b.ID}, OK: true,
				Before: map[string]any{"state": b.State}, After: map[string]any{"state": next},
			}); err != nil {
				return err
			}
		}
	case "retired":
		if b.SealedUntil == nil || now.Before(*b.SealedUntil) || b.Retired["erasure"] != nil {
			return nil
		}
		tv := cat.Tenants[b.Tenant]
		req, err := c.requestErasure(ctx, tx, now, tv, erasureSpec{Scope: ErasureScope{Tenant: b.Tenant, Bot: b.ID}, Reason: "bot_retired"})
		if err != nil {
			return err
		}
		b.Retired["erasure"] = req
		raw, err := json.Marshal(b.Retired)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE bots SET retired = $2, updated_at = $3 WHERE id = $1`, b.ID, raw, now); err != nil {
			return err
		}
	}
	return nil
}

// botErased ends a retired identity whose bot_retired request closed: only the tombstone
// remains (id, tenant, role, time, reason and successor).
func botErased(ctx context.Context, tx pgx.Tx, now time.Time, id string) error {
	if _, err := tx.Exec(ctx, `UPDATE bots SET state = 'erased', record = '{}', retired = retired - 'erasure', updated_at = $2 WHERE id = $1 AND state = 'retired'`, id, now); err != nil {
		return err
	}
	return eraseBotFacts(ctx, tx, now, id)
}

// actingOf is the identity a lease runs on behalf of, with its epochs.
type actingOf struct {
	Bot       string
	BotEpoch  int
	Seat      string
	SeatEpoch int
}

// leaseActing reads the acting identity of the last lease of a work item by a worker; empty
// when the work runs without an identity.
func leaseActing(ctx context.Context, q store.Querier, workID, worker string) (actingOf, error) {
	var a actingOf
	var bot, seat *string
	var be, se *int
	err := q.QueryRow(ctx, `SELECT acting_bot, bot_epoch, seat, seat_epoch FROM leases WHERE work_id = $1 AND ($2 = '' OR worker_id = $2)
		ORDER BY lease_no DESC LIMIT 1`, workID, worker).Scan(&bot, &be, &seat, &se)
	if errors.Is(err, pgx.ErrNoRows) {
		return a, nil
	}
	if err != nil {
		return a, err
	}
	a.Bot, a.Seat = deref(bot), deref(seat)
	if be != nil {
		a.BotEpoch = *be
	}
	if se != nil {
		a.SeatEpoch = *se
	}
	return a, nil
}

// actingFenced says whether a lease's identity or seat moved on since the lease was issued: a
// write with an old binding or seat epoch, or from a retired identity, gets fenced (IDN-8,
// IDN-10, IDN-11).
func actingFenced(ctx context.Context, q store.Querier, l *leaseRow) (bool, error) {
	if l.Acting.Bot == "" {
		return false, nil
	}
	var state string
	var epoch int
	err := q.QueryRow(ctx, `SELECT state, epoch FROM bots WHERE id = $1`, l.Acting.Bot).Scan(&state, &epoch)
	if errors.Is(err, pgx.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if !slices.Contains([]string{"active", "suspended"}, state) || epoch != l.Acting.BotEpoch {
		return true, nil
	}
	if l.Acting.Seat == "" {
		return false, nil
	}
	var sstate string
	var sepoch int
	err = q.QueryRow(ctx, `SELECT state, epoch FROM seats WHERE tenant = $1 AND id = $2`, l.Tenant, l.Acting.Seat).Scan(&sstate, &sepoch)
	if errors.Is(err, pgx.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return sstate != "held" || sepoch != l.Acting.SeatEpoch, nil
}
