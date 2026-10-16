// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
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
	// The external identities go into the erasure request before their links are deleted, so
	// the edge stores can match by them (RET-4).
	needles, err := needlesOf(ctx, tx, tv.Name, subject)
	if err != nil {
		return "", 0, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM identities WHERE tenant = $1 AND principal = $2`, tv.Name, subject); err != nil {
		return "", 0, err
	}
	if err := c.cancelTasksOf(ctx, tx, now, tv.Name, subject, "person_inactive"); err != nil {
		return "", 0, err
	}
	// From v0.2 the person leaves every group in the same transaction; a returning person gets
	// a new principal without them (IDN-14, D-100). The bound chats where they were seen stay
	// as leftovers (IDN-13, REP-7).
	if _, err := tx.Exec(ctx, `DELETE FROM memberships WHERE tenant = $1 AND principal = $2`, tv.Name, subject); err != nil {
		return "", 0, err
	}
	// The unused invitations they issued go with them (IDN-14).
	if err := revokeInvitationsOf(ctx, tx, tv.Name, subject); err != nil {
		return "", 0, err
	}
	if err := keepLeftovers(ctx, tx, now, tv.Name, subject); err != nil {
		return "", 0, err
	}
	// Deliveries still planned for them are withheld for good: the chat id they carry may later
	// belong to a new principal of the freed identity (DLV-6).
	if err := c.withholdDeliveriesTo(ctx, tx, now, tv.Name, subject, "person_inactive"); err != nil {
		return "", 0, err
	}
	erasure, err := c.requestErasure(ctx, tx, now, tv, erasureSpec{
		Scope: ErasureScope{Tenant: tv.Name, Subject: subject}, Reason: "offboarding", Author: author, Needles: needles,
	})
	if err != nil {
		return "", 0, err
	}
	// The event names only the opaque principal id (LCY-3).
	if err := c.appendEvent(ctx, tx, now, tv.Name, Event{Type: "principal.offboarded.v1", Subject: subject,
		Data: map[string]any{"subject": subject, "erasure": erasure}}); err != nil {
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
	if err := c.cancelApprovalsOf(ctx, tx, now, tenant, principal, reason); err != nil {
		return err
	}
	// An action task is not canceled: its write call may already run (APR-7).
	rows, err := tx.Query(ctx, `SELECT id FROM tasks WHERE tenant = $1 AND initiator = $2 AND state IN ('submitted', 'working') AND type NOT LIKE 'action.%' ORDER BY id FOR UPDATE`, tenant, principal)
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
	return c.cancelTaskIDs(ctx, tx, now, tenant, tasks, reason)
}

// planPrincipalLeft sends the code notice principal_left to the direct chats of the active
// admins of the tenant with admin:people (IDN-13, REP-5), never to a group. It names the bound
// chats where the person was seen and may still be a member (IDN-14). It returns the number of
// planned notices.
func (c *Core) planPrincipalLeft(ctx context.Context, tx pgx.Tx, now time.Time, tv *TenantView, subject, reason string) (int, error) {
	chats, err := c.adminChats(ctx, tx, now, tv, subject, "admin:people")
	if err != nil {
		return 0, err
	}
	member, err := memberChats(ctx, tx, tv.Name, subject)
	if err != nil {
		return 0, err
	}
	for _, a := range chats {
		params := map[string]any{"subject": subject, "reason": reason, "chats": c.chatsParam(a.Locale, member)}
		if _, err := c.planDelivery(ctx, tx, now, plannedDelivery{
			Tenant: tv.Name, Kind: "principal_left", Addr: "admins", TargetKind: "chat",
			TargetID: Chat{Kind: "direct", ExternalID: a.ExternalID}.ID(), ExecKind: "channel", ExecName: a.Channel,
			Recipient: a.Principal, DataOrigin: "code",
			Body: map[string]any{"form": "text_code", "text_key": "notice.principal_left", "params": params},
		}); err != nil {
			return 0, err
		}
	}
	return len(chats), nil
}

// adminChat is the direct chat of an admin with one channel of the tenant.
type adminChat struct {
	Principal, Locale, Channel, ExternalID string
}

// adminChats lists the direct chats of the active persons of the tenant that hold one of the
// capabilities, except the principal exclude. A direct chat is reached through each channel of
// the tenant that sends and whose identity kind matches an identity of the admin; the chat id
// of a direct chat is the external user id.
func (c *Core) adminChats(ctx context.Context, tx pgx.Tx, now time.Time, tv *TenantView, exclude string, caps ...string) ([]adminChat, error) {
	return c.principalChats(ctx, tx, now, tv, exclude, func(groups []string) bool {
		return slices.ContainsFunc(caps, func(cp string) bool { return hasCap(tv, groups, cp) })
	})
}

// principalChats are the direct chats of the active people of the tenant whose groups keep
// accepts, in every outgoing channel of their identities.
func (c *Core) principalChats(ctx context.Context, tx pgx.Tx, now time.Time, tv *TenantView, exclude string, keep func(groups []string) bool) ([]adminChat, error) {
	rows, err := tx.Query(ctx, `SELECT p.id, i.identity FROM principals p JOIN identities i ON i.tenant = p.tenant AND i.principal = p.id
		WHERE p.tenant = $1 AND p.id <> $2 AND p.state = 'active' AND p.kind = 'human' ORDER BY p.id, i.identity`, tv.Name, exclude)
	if err != nil {
		return nil, err
	}
	type link struct{ principal, identity string }
	var links []link
	for rows.Next() {
		var l link
		if err := rows.Scan(&l.principal, &l.identity); err != nil {
			rows.Close()
			return nil, err
		}
		links = append(links, l)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	channels := make([]string, 0, len(tv.Channels))
	for name := range tv.Channels {
		channels = append(channels, name)
	}
	sort.Strings(channels)
	admins := map[string]*principalInfo{}
	var out []adminChat
	for _, l := range links {
		p, seen := admins[l.principal]
		if !seen {
			var err error
			if p, err = loadPrincipal(ctx, tx, now, tv.Name, l.principal); err != nil {
				return nil, err
			}
			groups := groupClosure(tv, p.Groups)
			if !keep(groups) {
				p = nil
			}
			admins[l.principal] = p
		}
		if p == nil {
			continue
		}
		kind, ext, found := strings.Cut(l.identity, ":")
		if !found {
			continue
		}
		for _, name := range channels {
			ch := tv.Channels[name]
			if ch.IdentityKind != kind || ch.Direction == "in" {
				continue
			}
			out = append(out, adminChat{Principal: l.principal, Locale: p.Locale, Channel: name, ExternalID: ext})
		}
	}
	return out, nil
}
