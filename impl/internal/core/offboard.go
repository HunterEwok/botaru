// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/ids"
)

// offboard ends a principal for good in the caller's transaction (LCY-8): the state
// offboarded, which no operation leaves; the links of their channel identities are deleted, so
// the same external identity can be given to a new principal; their non-terminal tasks are
// canceled (APR-6, LCY-3); an erasure request by the scope (tenant, subject) with the reason
// offboarding is recorded; and the admins get the principal_left notice (IDN-13). The
// principal record keeps its opaque id, which the other records use (D-85). It returns the id
// of the erasure request and the number of planned notices.
func (c *Core) offboard(ctx context.Context, tx pgx.Tx, now time.Time, tv *TenantView, author, subject string) (string, int, error) {
	if _, err := tx.Exec(ctx, `UPDATE principals SET state = 'offboarded' WHERE tenant = $1 AND id = $2`, tv.Name, subject); err != nil {
		return "", 0, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM identities WHERE tenant = $1 AND principal = $2`, tv.Name, subject); err != nil {
		return "", 0, err
	}
	if err := c.cancelTasksOf(ctx, tx, now, tv.Name, subject, "person_inactive"); err != nil {
		return "", 0, err
	}
	// Deliveries still planned for them are withheld for good: the chat id they carry may later
	// belong to a new principal of the freed identity (DLV-6).
	if err := c.withholdDeliveriesTo(ctx, tx, now, tv.Name, subject, "person_inactive"); err != nil {
		return "", 0, err
	}
	// The request waits for the erasure work, which this version does not run yet.
	erasure := ids.New(ids.Erasure)
	authorRec := map[string]any{"kind": "system"}
	if author != "" {
		authorRec = map[string]any{"kind": "principal", "id": author}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO erasure_requests (id, tenant, subject, reason, author, state, created_at)
		VALUES ($1, $2, $3, 'offboarding', $4, 'requested', $5)`, erasure, tv.Name, subject, authorRec, now); err != nil {
		return "", 0, err
	}
	n, err := c.planPrincipalLeft(ctx, tx, now, tv, subject, "offboarding")
	return erasure, n, err
}

// withholdDeliveriesTo ends the planned deliveries to a principal with the state withheld.
func (c *Core) withholdDeliveriesTo(ctx context.Context, tx pgx.Tx, now time.Time, tenant, principal, reason string) error {
	rows, err := tx.Query(ctx, `UPDATE deliveries SET state = 'withheld', reason = $3, updated_at = $4
		WHERE tenant = $1 AND recipient = $2 AND state = 'planned' RETURNING id, kind`, tenant, principal, reason, now)
	if err != nil {
		return err
	}
	type withheld struct{ id, kind string }
	var ws []withheld
	for rows.Next() {
		var w withheld
		if err := rows.Scan(&w.id, &w.kind); err != nil {
			rows.Close()
			return err
		}
		ws = append(ws, w)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	sort.Slice(ws, func(i, j int) bool { return ws[i].id < ws[j].id })
	for _, w := range ws {
		// A privacy notice that never arrived is planned again at the next contact (DLV-12).
		if w.kind == "privacy_notice" {
			if _, err := tx.Exec(ctx, `UPDATE contacts ct SET privacy_version = NULL FROM deliveries d
				WHERE d.id = $1 AND ct.tenant = d.tenant AND ct.channel = d.exec_name AND ct.principal = d.recipient`, w.id); err != nil {
				return err
			}
		}
		if err := c.appendEvent(ctx, tx, now, tenant, Event{Type: "delivery.withheld", Subject: w.id,
			Data: map[string]any{"delivery": w.id, "kind": w.kind, "reason": reason}}); err != nil {
			return err
		}
	}
	return nil
}

// cancelTasksOf cancels the non-terminal tasks a principal initiated. A running attempt loses
// its lease and handles in the same transaction, so the worker is fenced at its next call.
func (c *Core) cancelTasksOf(ctx context.Context, tx pgx.Tx, now time.Time, tenant, principal, reason string) error {
	rows, err := tx.Query(ctx, `SELECT id FROM tasks WHERE tenant = $1 AND initiator = $2 AND state IN ('submitted', 'working') ORDER BY id FOR UPDATE`, tenant, principal)
	if err != nil {
		return err
	}
	var tasks []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		tasks = append(tasks, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range tasks {
		if _, err := tx.Exec(ctx, `DELETE FROM handles WHERE work_id = $1`, id); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE leases SET state = 'revoked', reason = $3, closed_at = $2 WHERE work_id = $1 AND state = 'active'`, id, now, reason); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE tasks SET state = 'canceled', reason = $2, run_at = NULL, updated_at = $3 WHERE id = $1`, id, reason, now); err != nil {
			return err
		}
		if err := releaseTaskReserve(ctx, tx, id); err != nil {
			return err
		}
		if err := c.appendEvent(ctx, tx, now, tenant, Event{Type: "task.canceled", Subject: id, Data: map[string]any{"task": id, "reason": reason}}); err != nil {
			return err
		}
	}
	return nil
}

// planPrincipalLeft sends the code notice principal_left to the direct chats of the active
// admins of the tenant with admin:people (IDN-13, REP-5), never to a group. This version has no
// chat bindings, so the list of chats where the person remains is always empty and the text
// says so. A direct chat is reached through each channel of the tenant whose identity kind
// matches an identity of the admin; the chat id of a direct chat is the external user id.
// It returns the number of planned notices.
func (c *Core) planPrincipalLeft(ctx context.Context, tx pgx.Tx, now time.Time, tv *TenantView, subject, reason string) (int, error) {
	rows, err := tx.Query(ctx, `SELECT p.id, i.identity FROM principals p JOIN identities i ON i.tenant = p.tenant AND i.principal = p.id
		WHERE p.tenant = $1 AND p.id <> $2 AND p.state = 'active' AND p.kind = 'human' ORDER BY p.id, i.identity`, tv.Name, subject)
	if err != nil {
		return 0, err
	}
	type link struct{ principal, identity string }
	var links []link
	for rows.Next() {
		var l link
		if err := rows.Scan(&l.principal, &l.identity); err != nil {
			rows.Close()
			return 0, err
		}
		links = append(links, l)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	channels := make([]string, 0, len(tv.Channels))
	for name := range tv.Channels {
		channels = append(channels, name)
	}
	sort.Strings(channels)
	admins := map[string]*principalInfo{}
	planned := 0
	for _, l := range links {
		p, seen := admins[l.principal]
		if !seen {
			var err error
			if p, err = loadPrincipal(ctx, tx, now, tv.Name, l.principal); err != nil {
				return 0, err
			}
			if !hasCap(tv, groupClosure(tv, p.Groups), "admin:people") {
				p = nil
			}
			admins[l.principal] = p
		}
		if p == nil {
			continue
		}
		params := map[string]any{"subject": subject, "reason": reason, "chats": c.texts.Render(p.Locale, "notice.principal_left.no_chats", nil)}
		kind, ext, found := strings.Cut(l.identity, ":")
		if !found {
			continue
		}
		for _, name := range channels {
			ch := tv.Channels[name]
			if ch.IdentityKind != kind || ch.Direction == "in" {
				continue
			}
			if _, err := c.planDelivery(ctx, tx, now, plannedDelivery{
				Tenant: tv.Name, Kind: "principal_left", Addr: "admins", TargetKind: "chat",
				TargetID: Chat{Kind: "direct", ExternalID: ext}.ID(), ExecKind: "channel", ExecName: name,
				Recipient: l.principal, DataOrigin: "code",
				Body: map[string]any{"form": "text_code", "text_key": "notice.principal_left", "params": params},
			}); err != nil {
				return 0, err
			}
			planned++
		}
	}
	return planned, nil
}
