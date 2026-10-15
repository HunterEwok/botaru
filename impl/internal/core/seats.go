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
)

// Commission seats (IDN-10, GOV-33, spec/lifecycles/seat.md). The tenant charter declares the
// seats; apply keeps the seats table in step with it. Only the core writes a holder, in one
// transaction that increments the seat epoch, writes a seat event and an audit record, and
// fences the running attempts of the seat's tasks. The farm manager seat arrives with the
// farm manager (stage 4).

// seatRow is a seat as the store keeps it.
type seatRow struct {
	Tenant, ID, Kind, Lens, Holder, State string
	Epoch                                 int
}

func loadSeat(ctx context.Context, tx pgx.Tx, tenant, id string, lock bool) (*seatRow, error) {
	sql := `SELECT tenant, id, kind, coalesce(lens, ''), coalesce(holder, ''), state, epoch FROM seats WHERE tenant = $1 AND id = $2`
	if lock {
		sql += ` FOR UPDATE`
	}
	var s seatRow
	err := tx.QueryRow(ctx, sql, tenant, id).Scan(&s.Tenant, &s.ID, &s.Kind, &s.Lens, &s.Holder, &s.State, &s.Epoch)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &s, err
}

// charterSeats are the commission seats of a tenant charter by id, with their lens.
func charterSeats(cat *Catalog, tenant string) map[string]string {
	gov := tenantSpecOf(cat, "Governance", tenant)
	com, _ := gov["commission"].(map[string]any)
	out := map[string]string{}
	for _, s := range asList(com["seats"]) {
		sm, _ := s.(map[string]any)
		n, ok := toFloat(sm["id"])
		if !ok {
			continue
		}
		out[fmt.Sprintf("commission:%d", int(n))] = asString(sm["lens"])
	}
	return out
}

// seatRole is the role of the seat tasks of a tenant: commission.seat_role of the charter.
func seatRole(cat *Catalog, tenant string) string {
	gov := tenantSpecOf(cat, "Governance", tenant)
	com, _ := gov["commission"].(map[string]any)
	return asString(com["seat_role"])
}

// syncSeats brings the commission seats of every tenant in line with its applied charter: a
// new seat starts vacant, a seat's lens follows the charter, and a seat the charter dropped is
// removed; a seat with a holder is not removed (GOV-33).
func (c *Core) syncSeats(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog) error {
	rows, err := tx.Query(ctx, `SELECT tenant, id, coalesce(lens, ''), coalesce(holder, ''), state FROM seats WHERE kind = 'commission' AND state <> 'removed' ORDER BY tenant, id`)
	if err != nil {
		return err
	}
	type cur struct{ lens, holder, state string }
	have := map[[2]string]cur{}
	for rows.Next() {
		var t, id string
		var s cur
		if err := rows.Scan(&t, &id, &s.lens, &s.holder, &s.state); err != nil {
			rows.Close()
			return err
		}
		have[[2]string{t, id}] = s
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	want := map[[2]string]string{}
	for _, t := range slices.Sorted(mapKeys(cat.Tenants)) {
		for id, lens := range charterSeats(cat, t) {
			want[[2]string{t, id}] = lens
		}
	}
	for k, s := range have {
		if _, ok := want[k]; ok {
			continue
		}
		if s.holder != "" {
			return refuse("forbidden", "seat %s of tenant %s has a holder: seat.vacate first", k[1], k[0])
		}
		if _, err := tx.Exec(ctx, `UPDATE seats SET state = 'removed', updated_at = $3 WHERE tenant = $1 AND id = $2`, k[0], k[1], now); err != nil {
			return err
		}
	}
	for k, lens := range want {
		if s, ok := have[k]; ok && s.lens == lens {
			continue
		}
		if _, err := tx.Exec(ctx, `INSERT INTO seats (tenant, id, kind, lens, state, epoch, updated_at) VALUES ($1, $2, 'commission', $3, 'vacant', 0, $4)
			ON CONFLICT (tenant, id) DO UPDATE SET lens = EXCLUDED.lens, state = CASE WHEN seats.state = 'removed' THEN 'vacant' ELSE seats.state END, updated_at = EXCLUDED.updated_at`,
			k[0], k[1], lens, now); err != nil {
			return err
		}
	}
	return nil
}

// SeatRequest is p2-seat-assign-request and p2-seat-vacate-request.
type SeatRequest struct {
	Seat string `json:"seat"`
	Bot  string `json:"bot"`
}

// functionaryProfiles are the role profiles of a functionary, who alone may take a commission
// seat (spec/lifecycles/seat.md).
var functionaryProfiles = []string{"worker", "manager", "reporter", "aggregator"}

// SeatAssign is P2 seat.assign of a commission seat (IDN-10): only an active functionary
// identity of the seat's tenant takes it; the previous holder leaves as replaced_by_human with
// the new holder as successor, the new holder's worker gets a new identity, and the seat epoch
// grows.
func (c *Core) SeatAssign(ctx context.Context, op Operator, req SeatRequest) (*Done, error) {
	if req.Seat == "farm-manager" {
		return nil, refuse("not_found", "the farm manager seat arrives with the farm manager")
	}
	if req.Bot == "" {
		return nil, refuse("input_schema_invalid", "a commission seat is assigned to a bot identity")
	}
	cat, err := c.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	var out Done
	err = c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		s, err := loadSeat(ctx, tx, op.Tenant, req.Seat, true)
		if err != nil {
			return err
		}
		if s == nil || s.State == "removed" {
			return refuse("not_found", "no such seat in the tenant")
		}
		out = Done{OK: true, ID: s.ID, State: "held"}
		if s.Holder == req.Bot {
			// A repeat changes nothing (IDM-3).
			return nil
		}
		b, err := loadBot(ctx, tx, req.Bot, true)
		if err != nil {
			return err
		}
		if b == nil || b.Tenant != op.Tenant {
			return refuse("not_found", "no such bot identity in the tenant")
		}
		if b.State != "active" || b.Mode != "worker" || !slices.Contains(functionaryProfiles, profileOf(cat, b.Role)) {
			return refuse("forbidden", "only an active functionary identity of the tenant takes a seat")
		}
		if s.Holder != "" {
			prev, err := loadBot(ctx, tx, s.Holder, true)
			if err != nil {
				return err
			}
			if prev != nil {
				if _, err := c.retireBot(ctx, tx, now, cat, prev, "replaced_by_human", b.ID, op.Principal); err != nil {
					return err
				}
			}
		}
		// The identity moves to the seat; its worker keeps working under a new identity.
		worker := b.Worker
		if _, err := tx.Exec(ctx, `UPDATE bots SET mode = 'seat', seat = $2, worker_id = NULL, epoch = epoch + 1, updated_at = $3 WHERE id = $1`, b.ID, s.ID, now); err != nil {
			return err
		}
		if err := c.fenceAttempts(ctx, tx, now, "seat_changed", `acting_bot = $1`, b.ID); err != nil {
			return err
		}
		if worker != "" {
			if _, err := c.insertBot(ctx, tx, now, b.Tenant, b.Role, "worker", worker, "", "seat.assign"); err != nil {
				return err
			}
		}
		seq, err := c.moveSeat(ctx, tx, now, s, b.ID, "held", "seat.assigned.v1", "seat.assign", op.Principal)
		out.AuditSeq = seq
		return err
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// SeatVacate is P2 seat.vacate: the holder leaves as replaced_by_human and the seat is vacant.
func (c *Core) SeatVacate(ctx context.Context, op Operator, req SeatRequest) (*Done, error) {
	if req.Seat == "farm-manager" {
		return nil, refuse("not_found", "the farm manager seat arrives with the farm manager")
	}
	cat, err := c.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	var out Done
	err = c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		s, err := loadSeat(ctx, tx, op.Tenant, req.Seat, true)
		if err != nil {
			return err
		}
		if s == nil || s.State == "removed" {
			return refuse("not_found", "no such seat in the tenant")
		}
		out = Done{OK: true, ID: s.ID, State: "vacant"}
		if s.Holder == "" {
			return nil
		}
		prev, err := loadBot(ctx, tx, s.Holder, true)
		if err != nil {
			return err
		}
		if prev != nil {
			if _, err := c.retireBot(ctx, tx, now, cat, prev, "replaced_by_human", "", op.Principal); err != nil {
				return err
			}
		}
		seq, err := c.moveSeat(ctx, tx, now, s, "", "vacant", "seat.vacated.v1", "seat.vacate", op.Principal)
		out.AuditSeq = seq
		return err
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// seatVacated frees the seat of a holder that retired for a reason other than a seat
// operation (human_retire, role_removed; spec/lifecycles/seat.md).
func (c *Core) seatVacated(ctx context.Context, tx pgx.Tx, now time.Time, b *botRow, cause string) error {
	if b.Mode != "seat" || b.Seat == "" {
		return nil
	}
	s, err := loadSeat(ctx, tx, b.Tenant, b.Seat, true)
	if err != nil || s == nil || s.Holder != b.ID {
		return err
	}
	_, err = c.moveSeat(ctx, tx, now, s, "", "vacant", "seat.vacated.v1", cause, "system")
	return err
}

// moveSeat writes the new holder and state with the seat epoch + 1, fences the running
// attempts of the seat's tasks (they go back to the queue unspent, seat_changed), and writes
// the seat event and the audit record (IDN-10).
func (c *Core) moveSeat(ctx context.Context, tx pgx.Tx, now time.Time, s *seatRow, holder, state, event, cause, actor string) (int64, error) {
	if _, err := tx.Exec(ctx, `UPDATE seats SET holder = $3, state = $4, epoch = epoch + 1, updated_at = $5 WHERE tenant = $1 AND id = $2`,
		s.Tenant, s.ID, nullable(holder), state, now); err != nil {
		return 0, err
	}
	if err := c.fenceAttempts(ctx, tx, now, "seat_changed", `seat = $1 AND tenant = $2`, s.ID, s.Tenant); err != nil {
		return 0, err
	}
	var h, prev any
	if holder != "" {
		h = holder
	}
	if s.Holder != "" {
		prev = s.Holder
	}
	data := map[string]any{"seat": s.ID, "tenant": s.Tenant, "event": eventName(event), "epoch": s.Epoch + 1, "holder": h, "previous": prev,
		"cause": cause, "at": fmtTime(now)}
	if err := c.spec.Validate("urn:botaru:schema:seat-event:1", data); err != nil {
		return 0, fmt.Errorf("seat event: %w", err)
	}
	if err := c.appendEvent(ctx, tx, now, s.Tenant, Event{Type: event, Subject: s.ID, Data: data}); err != nil {
		return 0, err
	}
	return c.appendAudit(ctx, tx, now, Audit{
		Tenant: s.Tenant, Kind: "command", Actor: Actor{Principal: principalOr(actor), Via: "farmctl"},
		Command: cause, Target: &Target{Kind: "seat", ID: s.ID}, OK: true,
		Before: map[string]any{"holder": prev, "state": s.State, "epoch": s.Epoch},
		After:  map[string]any{"holder": h, "state": state, "epoch": s.Epoch + 1},
	})
}

// eventName drops the version suffix of an event type: seat.assigned.v1 is seat.assigned.
func eventName(t string) string {
	if len(t) > 3 && t[len(t)-3:] == ".v1" {
		return t[:len(t)-3]
	}
	return t
}

// profileOf is the profile of a role: worker when the role names none (PRF-1).
func profileOf(cat *Catalog, role string) string {
	if p := cat.Roles[role].Profile; p != "" {
		return p
	}
	return "worker"
}

// seatExecutor is the seat that reviews a lens of a tenant now: a held seat of that lens whose
// holder is not under a hold of people; nil when the lens goes to its fallback role (COM-7,
// spec/lifecycles/seat.md).
func (c *Core) seatExecutor(ctx context.Context, tx pgx.Tx, now time.Time, tenant, lens string) (*seatRow, error) {
	rows, err := tx.Query(ctx, `SELECT s.id FROM seats s JOIN bots b ON b.id = s.holder
		WHERE s.tenant = $1 AND s.kind = 'commission' AND s.lens = $2 AND s.state = 'held' AND b.state = 'active' ORDER BY s.id`, tenant, lens)
	if err != nil {
		return nil, err
	}
	list, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	for _, id := range list {
		s, err := loadSeat(ctx, tx, tenant, id, false)
		if err != nil {
			return nil, err
		}
		b, err := loadBot(ctx, tx, s.Holder, false)
		if err != nil {
			return nil, err
		}
		if held, err := botHeld(ctx, tx, now, b); err != nil {
			return nil, err
		} else if !held {
			return s, nil
		}
	}
	return nil, nil
}

// seatOfRecord is the seat a task record names, or "".
func seatOfRecord(rec []byte) string {
	var r struct {
		Seat string `json:"seat"`
	}
	if json.Unmarshal(rec, &r) != nil {
		return ""
	}
	return r.Seat
}

// seatActing is the identity a seat task runs on behalf of: the holder of a held seat with its
// binding epoch and the seat epoch; not ok when the seat is vacant or its holder is not active
// or is under a hold of people.
func (c *Core) seatActing(ctx context.Context, tx pgx.Tx, now time.Time, tenant, seat string) (actingOf, bool, error) {
	s, err := loadSeat(ctx, tx, tenant, seat, false)
	if err != nil || s == nil || s.State != "held" || s.Holder == "" {
		return actingOf{}, false, err
	}
	b, err := loadBot(ctx, tx, s.Holder, false)
	if err != nil || b == nil || b.State != "active" {
		return actingOf{}, false, err
	}
	if held, err := botHeld(ctx, tx, now, b); err != nil || held {
		return actingOf{}, false, err
	}
	return actingOf{Bot: b.ID, BotEpoch: b.Epoch, Seat: s.ID, SeatEpoch: s.Epoch}, true, nil
}
