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

// Leftovers of an offboarded person in bound group chats (IDN-13, IDN-14, REP-7). The farm does
// not remove a person from an external chat. The core does not see group membership either, so
// a person seen writing in a bound chat counts as its member; at offboarding these chats become
// leftovers: the principal_left notice names them and repeats once a day for at most 7 days,
// and the core counts the person as a participant without permissions, so the views for such a
// chat carry nothing. Binding the chat again clears its leftovers: the operator checked the
// members, as for an unknown member (DLV-7).

// principalLeftRepeat and principalLeftDays are the repeat of the principal_left notice.
const (
	principalLeftRepeat = 24 * time.Hour
	principalLeftDays   = 7
)

// noteChatSeen records that a principal wrote in a bound chat.
func noteChatSeen(ctx context.Context, tx pgx.Tx, now time.Time, tenant, binding, principal string) error {
	_, err := tx.Exec(ctx, `INSERT INTO chat_seen (tenant, binding, principal, last_at) VALUES ($1, $2, $3, $4)
		ON CONFLICT (tenant, binding, principal) DO UPDATE SET last_at = EXCLUDED.last_at`, tenant, binding, principal, now)
	return err
}

// keepLeftovers turns the chats where an offboarded person was seen into leftovers.
func keepLeftovers(ctx context.Context, tx pgx.Tx, now time.Time, tenant, principal string) error {
	if _, err := tx.Exec(ctx, `INSERT INTO chat_leftovers (tenant, binding, principal, since, next_notice_at)
		SELECT s.tenant, s.binding, s.principal, $3, $4 FROM chat_seen s JOIN chat_bindings b ON b.tenant = s.tenant AND b.name = s.binding
		WHERE s.tenant = $1 AND s.principal = $2 ON CONFLICT DO NOTHING`, tenant, principal, now, now.Add(principalLeftRepeat)); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `DELETE FROM chat_seen WHERE tenant = $1 AND principal = $2`, tenant, principal)
	return err
}

// memberChats are the names of the bound chats a principal is seen in or left behind in.
func memberChats(ctx context.Context, tx pgx.Tx, tenant, principal string) ([]string, error) {
	rows, err := tx.Query(ctx, `SELECT binding FROM chat_seen WHERE tenant = $1 AND principal = $2
		UNION SELECT binding FROM chat_leftovers WHERE tenant = $1 AND principal = $2`, tenant, principal)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	sort.Strings(out)
	return out, rows.Err()
}

// chatLeftovers are the offboarded persons still counted as members of a bound chat.
func chatLeftovers(ctx context.Context, tx pgx.Tx, tenant, binding string) ([]string, error) {
	rows, err := tx.Query(ctx, `SELECT principal FROM chat_leftovers WHERE tenant = $1 AND binding = $2 ORDER BY principal`, tenant, binding)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// chatsParam is the chats parameter of principal_left: the chat names, or the text for none.
func (c *Core) chatsParam(locale string, chats []string) string {
	if len(chats) == 0 {
		return c.texts.Render(locale, "notice.principal_left.no_chats", nil)
	}
	return strings.Join(chats, ", ")
}

// repeatPrincipalLeft repeats the principal_left notice of an offboarded person who is still
// counted as a member of a bound chat: once a day, for at most 7 days from the offboarding
// (IDN-14). It runs in the deadline loop.
func (c *Core) repeatPrincipalLeft(ctx context.Context) error {
	cat, err := c.Catalog(ctx)
	if err != nil {
		return err
	}
	return c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		// The due rows move to the next day at once (a core that was down sends one notice,
		// not one for each missed day); the update locks them against another core copy.
		rows, err := tx.Query(ctx, `UPDATE chat_leftovers SET next_notice_at = $1 + make_interval(secs => $3)
			WHERE next_notice_at <= $1 AND since + make_interval(days => $2) > $1 RETURNING tenant, principal`, now, principalLeftDays, principalLeftRepeat.Seconds())
		if err != nil {
			return err
		}
		type left struct{ tenant, principal string }
		var due []left
		for rows.Next() {
			var l left
			if err := rows.Scan(&l.tenant, &l.principal); err != nil {
				rows.Close()
				return err
			}
			if !slices.Contains(due, l) {
				due = append(due, l)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		sort.Slice(due, func(i, j int) bool { return due[i].tenant+" "+due[i].principal < due[j].tenant+" "+due[j].principal })
		for _, l := range due {
			if tv := cat.Tenants[l.tenant]; tv != nil {
				if _, err := c.planPrincipalLeft(ctx, tx, now, tv, l.principal, "offboarding"); err != nil {
					return err
				}
			}
		}
		return nil
	})
}
