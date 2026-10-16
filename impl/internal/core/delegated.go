// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// coversGroup says whether the groups of a grantor hold every capability and data class of a
// group, with the groups it inherits (PRM-7).
func coversGroup(tv *TenantView, grantor []string, group string) bool {
	for _, g := range groupClosure(tv, []string{group}) {
		spec := tv.Groups[g]
		for _, cp := range spec.Caps {
			if !hasCap(tv, grantor, cp) {
				return false
			}
		}
		for _, d := range spec.Data {
			if !hasData(tv, grantor, d) {
				return false
			}
		}
	}
	return true
}

// grantCommand is /grant and /ungrant <person> <group> from a private chat of a tenant admin
// with admin:people (PRM-16, PRM-7). The person is named by an identity (telegram:1002) or a
// principal id. A delegated group or an ordinary one is granted at once; a privileged group
// outside delegated_groups only by an operator (PRM-13). A grant of a delegated group tells
// every admin of the tenant privately. A retried update of the same activity gets the reply of
// the first and changes nothing again (DLV-5, IDM-3).
func (c *Core) grantCommand(ctx context.Context, tx pgx.Tx, now time.Time, pc personCommand, word, rest string) (*ActivityResult, error) {
	// Two deliveries of the same update wait for each other here, so the second reads the
	// first one's outcome instead of granting again.
	if pc.key != "" {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 7300105))`, pc.tv.Name+"/"+pc.key); err != nil {
			return nil, err
		}
	}
	var raw []byte
	err := tx.QueryRow(ctx, `SELECT reply FROM activity_outcomes WHERE tenant = $1 AND key = $2`, pc.tv.Name, pc.key).Scan(&raw)
	if err == nil {
		var out ActivityResult
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, err
		}
		return &out, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	out, err := c.grantOnce(ctx, tx, now, pc, word, rest)
	if err != nil || pc.key == "" {
		return out, err
	}
	if raw, err = json.Marshal(out); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO activity_outcomes (tenant, key, reply, created_at) VALUES ($1, $2, $3, $4) ON CONFLICT DO NOTHING`,
		pc.tv.Name, pc.key, raw, now); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *Core) grantOnce(ctx context.Context, tx pgx.Tx, now time.Time, pc personCommand, word, rest string) (*ActivityResult, error) {
	f := strings.Fields(rest)
	if len(f) != 2 {
		return pc.reply(c, "cmd.grant.usage", nil), nil
	}
	if !hasCap(pc.tv, pc.groups, "admin:people") {
		return pc.reply(c, "task.rejected.no_group_cap", nil), nil
	}
	who, group := f[0], f[1]
	// The group and its privilege as the stored set has them, under the apply lock (PRM-13).
	stv, scat, err := c.grantView(ctx, tx, pc.tv.Name)
	if err != nil {
		return nil, err
	}
	if _, ok := stv.Groups[group]; !ok {
		return pc.reply(c, "cmd.grant.no_group", map[string]any{"group": group}), nil
	}
	delegated := slices.Contains(stv.Spec.DelegatedGroups, group) && c.delegable(stv, scat, group)
	if !delegated && privilegedGroup(stv, scat, group, map[string]bool{}) {
		return pc.reply(c, "cmd.grant.privileged", map[string]any{"group": group}), nil
	}
	if !coversGroup(pc.tv, pc.groups, group) {
		return pc.reply(c, "cmd.grant.not_covered", map[string]any{"group": group}), nil
	}
	subject := who
	if strings.Contains(who, ":") {
		p, err := principalByIdentity(ctx, tx, pc.tv.Name, who)
		if errors.Is(err, pgx.ErrNoRows) {
			return pc.reply(c, "cmd.grant.no_person", nil), nil
		}
		if err != nil {
			return nil, err
		}
		subject = p
	}
	var state, kind string
	err = tx.QueryRow(ctx, `SELECT state, kind FROM principals WHERE tenant = $1 AND id = $2 FOR UPDATE`, pc.tv.Name, subject).Scan(&state, &kind)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && kind != "human") {
		return pc.reply(c, "cmd.grant.no_person", nil), nil
	}
	if err != nil {
		return nil, err
	}
	op := strings.TrimPrefix(word, "/")
	switch op {
	case "grant":
		if state != "active" {
			return pc.reply(c, "task.rejected.person_inactive", nil), nil
		}
		if _, err := tx.Exec(ctx, `INSERT INTO memberships (tenant, principal, grp, state, created_at) VALUES ($1, $2, $3, 'active', $4)
			ON CONFLICT (tenant, principal, grp) DO UPDATE SET state = 'active',
			until = CASE WHEN memberships.until <= $4 THEN NULL ELSE memberships.until END`, pc.tv.Name, subject, group, now); err != nil {
			return nil, err
		}
	case "ungrant":
		tag, err := tx.Exec(ctx, `DELETE FROM memberships WHERE tenant = $1 AND principal = $2 AND grp = $3`, pc.tv.Name, subject, group)
		if err != nil {
			return nil, err
		}
		if tag.RowsAffected() == 0 {
			return pc.reply(c, "cmd.ungrant.not_member", map[string]any{"subject": subject, "group": group}), nil
		}
		if err := c.revokeWorkOf(ctx, tx, now, pc.tv, subject); err != nil {
			return nil, err
		}
	}
	after := map[string]any{"group": group}
	if delegated {
		after["delegated"] = true
	}
	if _, err := c.appendAudit(ctx, tx, now, Audit{
		Tenant: pc.tv.Name, Kind: "command", Actor: Actor{Principal: pc.person, Via: "channel"},
		Command: op, Target: &Target{Kind: "membership", ID: subject + "/" + group}, OK: true, After: after,
	}); err != nil {
		return nil, err
	}
	// Every admin hears of a grant and a removal of a delegated group (PRM-13, PRM-16).
	if delegated {
		chats, err := c.adminChats(ctx, tx, now, pc.tv, "", "admin:people")
		if err != nil {
			return nil, err
		}
		for _, a := range chats {
			if _, err := c.planDelivery(ctx, tx, now, plannedDelivery{
				Tenant: pc.tv.Name, Kind: "notice", Addr: "admins", TargetKind: "chat",
				TargetID: Chat{Kind: "direct", ExternalID: a.ExternalID}.ID(), ExecKind: "channel", ExecName: a.Channel,
				Recipient: a.Principal, DataOrigin: "code",
				Body: map[string]any{"form": "text_code", "text_key": "notice.delegated_" + op, "params": map[string]any{
					"subject": subject, "group": group, "by": pc.person}},
			}); err != nil {
				return nil, err
			}
		}
	}
	return pc.reply(c, "cmd."+op+".done", map[string]any{"subject": subject, "group": group}), nil
}

// delegable checks the shape PRM-16 allows for a delegated group at the grant itself, not only
// at apply: only task and task.read of named types of tiers up to 2 and classes up to
// internal, without wildcards, and data classes up to internal.
func (c *Core) delegable(tv *TenantView, cat *Catalog, group string) bool {
	for _, g := range groupClosure(tv, []string{group}) {
		spec := tv.Groups[g]
		for _, cp := range spec.Caps {
			base, arg, _ := strings.Cut(cp, ":")
			if base != "task" && base != "task.read" {
				return false
			}
			tt, ok := cat.TaskTypes[arg]
			if !ok || strings.Contains(arg, "*") || strings.HasPrefix(arg, "action.") || tt.Tier >= 3 || !c.markWithin(tt.DataClass, "internal") {
				return false
			}
		}
		for _, d := range spec.Data {
			if !c.markWithin(d, "internal") {
				return false
			}
		}
	}
	return true
}
