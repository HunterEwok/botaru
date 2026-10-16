// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/ids"
)

// Revocation on compromise (SEC-6, HLD-2). secret.revoke names a credential the core issued or
// a named deployment secret. A worker token (named by its worker id) and the P2 secret of a
// channel driver (named by the driver) stop at the next call without an overlap window. For
// any secret the core sets a hold without expiry with reason compromised on every scope that
// uses it: the worker of a token, the channels of a driver, the plugins whose manifest names
// the secret, the scopes the core settings list for a deployment secret (secret_uses), the
// tenants whose provider key it is and the scopes of the processes that the deployment
// description gives the secret (DEP-10). A secret only the core, the store, the watchdog or the
// backup holds is revoked with no hold; a secret no use is known of is not_found; a described
// use outside the credential's reach is forbidden, and a process of tenant farm is the farm
// operator's.
// The hold's author is the operator, so only the operator releases it.

// SecretRevokeRequest is p2-secret-revoke-request.
type SecretRevokeRequest struct {
	Secret      string         `json:"secret"`
	Compromised bool           `json:"compromised"`
	Connections map[string]any `json:"connections"`
}

// compromisedEffects stop every path of a scope that uses a revoked secret.
var compromisedEffects = []string{"no_enqueue", "no_claim", "no_tool", "no_action", "no_delivery"}

// SecretRevoke is P2 secret.revoke (SEC-6).
func (c *Core) SecretRevoke(ctx context.Context, op Operator, req SecretRevokeRequest) (*Done, error) {
	if req.Connections != nil {
		// People's connections (SEC-7) belong to the connections profile, which this build
		// does not claim.
		return nil, refuse("not_found", "connections are not built in this version")
	}
	if !req.Compromised || req.Secret == "" {
		return nil, refuse("input_schema_invalid", "secret.revoke needs secret and compromised")
	}
	cat, err := c.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	reach := func(tenant string) bool {
		return tenant != farmTenant && (op.Tenant == farmTenant || op.Tenant == tenant)
	}
	var out Done
	err = c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		var scopes []HoldScope
		add := func(s HoldScope) {
			if !slices.Contains(scopes, s) {
				scopes = append(scopes, s)
			}
		}
		kind := "deployment"
		// A worker token: the worker is revoked at once; its leases and handles end with it.
		var tenant, state string
		err := tx.QueryRow(ctx, `SELECT tenant, state FROM workers WHERE id = $1 FOR UPDATE`, req.Secret).Scan(&tenant, &state)
		if err != nil && err != pgx.ErrNoRows {
			return err
		}
		if err == nil && reach(tenant) {
			kind = "worker_token"
			// The token hash is replaced as well, so no window keeps the old token (SEC-5).
			if _, err := tx.Exec(ctx, `UPDATE workers SET token_hash = $2 WHERE id = $1`, req.Secret, c.hashSecret("revoked_"+ids.Secret(32))); err != nil {
				return err
			}
			if state != "revoked" {
				if _, err := c.revokeWorker(ctx, tx, now, tenant, req.Secret, state, op.Principal, "worker.revoke", "compromised"); err != nil {
					return err
				}
			}
			add(HoldScope{Kind: "worker", Tenant: tenant, Name: strings.ToLower(req.Secret)})
		}
		// The P2 secret of a channel driver: every value of it ends now, the rotation window too.
		rows, err := tx.Query(ctx, `DELETE FROM driver_secrets WHERE driver = $1 RETURNING tenant`, req.Secret)
		if err != nil {
			return err
		}
		var driverTenants []string
		for rows.Next() {
			var t string
			if err := rows.Scan(&t); err != nil {
				rows.Close()
				return err
			}
			driverTenants = append(driverTenants, t)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, t := range driverTenants {
			if !reach(t) {
				return refuse("forbidden", "the driver secret is used by another tenant")
			}
			kind = "driver_secret"
			if tv := cat.Tenants[t]; tv != nil {
				for name, ch := range tv.Channels {
					if ch.Driver == req.Secret {
						add(HoldScope{Kind: "channel", Tenant: t, Name: name})
					}
				}
			}
		}
		// A plugin secret by its manifest name: the plugin in every tenant the operator reaches.
		var plugins []string
		for name, p := range cat.Plugins {
			if slices.Contains(p.Secrets, req.Secret) {
				plugins = append(plugins, name)
			}
		}
		sort.Strings(plugins)
		var tenants []string
		for t := range cat.Tenants {
			if reach(t) {
				tenants = append(tenants, t)
			}
		}
		sort.Strings(tenants)
		for _, p := range plugins {
			for _, t := range tenants {
				add(HoldScope{Kind: "plugin", Tenant: t, Name: p})
			}
		}
		// The uses of a deployment secret from the core settings.
		for _, s := range c.cfg.SecretUses[req.Secret] {
			if !slices.Contains([]string{"channel", "plugin", "worker", "sink", "tenant"}, s.Kind) || !reach(s.Tenant) {
				continue
			}
			if s.Kind == "tenant" {
				s.Name = ""
			}
			add(s)
		}
		// The described uses of a deployment secret: the tenants whose provider key it is and
		// the scopes the deployment description gives it (SEC-6, DEP-10). Every described scope
		// must be in the credential's reach, so a secret two tenants share is revoked only by a
		// credential that holds both; a process of tenant farm is the farm operator's (its hold
		// is the tenant farm's scope). A secret of the farm's own processes only gets no hold; one
		// held by a process whose tenant the description does not give holds every tenant.
		reachDescribed := func(t string) bool { return reach(t) || t == farmTenant && op.Tenant == farmTenant }
		var described []HoldScope
		for _, t := range c.cfg.ProviderKeySecrets[req.Secret] {
			described = append(described, HoldScope{Kind: "tenant", Tenant: t})
		}
		farmOnly := false
		if c.deploy != nil {
			channel := func(t, name string) bool { return cat.Tenants[t].channel(name) }
			use := c.deploy.secretScopes(req.Secret, channel, c.cfg.ProviderKeySecrets[req.Secret])
			if use.Unresolved {
				for _, t := range slices.Sorted(mapKeys(cat.Tenants)) {
					described = append(described, HoldScope{Kind: "tenant", Tenant: t})
				}
			}
			described = append(described, use.Scopes...)
			farmOnly = use.Farm && len(described) == 0
		}
		for _, s := range described {
			if !reachDescribed(s.Tenant) {
				return refuse("forbidden", "the secret is used by a tenant the credential does not reach: only a credential that reaches every tenant of its uses revokes it")
			}
			add(s)
		}
		if len(scopes) == 0 && kind == "deployment" && !farmOnly {
			return refuse("not_found", "no credential or use of the secret is known in the tenants of the credential")
		}
		var holds []any
		for _, s := range scopes {
			id, _, err := c.insertHold(ctx, tx, now, OperatorActor(op), s, compromisedEffects, "compromised", nil)
			if err != nil {
				return err
			}
			holds = append(holds, id)
		}
		seq, err := c.appendAudit(ctx, tx, now, Audit{
			Tenant: op.Tenant, Kind: "command", Actor: Actor{Principal: op.Principal, Via: "farmctl"},
			Command: "secret.revoke", Target: &Target{Kind: "secret"}, OK: true, Reason: "compromised",
			After: map[string]any{"secret": req.Secret, "kind": kind, "holds": holds},
		})
		out = Done{OK: true, State: "revoked", AuditSeq: seq}
		if kind == "worker_token" {
			out.ID = req.Secret
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}
