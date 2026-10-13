// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/ids"
	"github.com/HunterEwok/botaru/impl/internal/manifest"
	"github.com/HunterEwok/botaru/impl/internal/money"
)

// ModelGrant is what a live model handle opens on P4: the attempt it belongs to and the model
// profile of the role slot (GW-3, GW-7).
type ModelGrant struct {
	Tenant       string
	Task         string
	Root         string
	Initiator    string
	Role         string
	Worker       string
	Slot         string
	ProfileName  string
	Profile      manifest.ModelProfile
	Subprocessor manifest.Subprocessor
	Line         string
	// TreeBudget is the type's tree budget in micro-euros; 0 means none.
	TreeBudget int64
}

// ResolveModelHandle checks a model handle: it exists, its lease is alive and of the current
// epoch (SEC-3, FEN-4), and its profile is allowed for the tenant (GW-7, SEC-8, GW-15).
func (c *Core) ResolveModelHandle(ctx context.Context, handle string) (*ModelGrant, error) {
	if handle == "" {
		return nil, refuse("handle_invalid", "no model handle")
	}
	cat, err := c.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	var g ModelGrant
	var epoch int64
	var leaseNo int
	var state string
	var until, now time.Time
	var slot, profile *string
	err = c.st.Pool.QueryRow(ctx, `SELECT h.tenant, h.work_id, h.lease_no, h.epoch, h.worker_id, h.slot, h.profile, l.state, l.lease_until, botaru_now()
		FROM handles h JOIN leases l ON l.work_id = h.work_id AND l.lease_no = h.lease_no
		WHERE h.hash = $1 AND h.kind = 'model'`, c.hashSecret(handle)).
		Scan(&g.Tenant, &g.Task, &leaseNo, &epoch, &g.Worker, &slot, &profile, &state, &until, &now)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, refuse("handle_invalid", "unknown model handle")
	}
	if err != nil {
		return nil, err
	}
	if epoch != c.epoch || state != "active" || !until.After(now) {
		return nil, refuse("fenced", "the lease of the handle is not alive")
	}
	if slot != nil {
		g.Slot = *slot
	}
	if profile != nil {
		g.ProfileName = *profile
	}
	var typ string
	var rec []byte
	if err := c.st.Pool.QueryRow(ctx, `SELECT type, initiator, record FROM tasks WHERE id = $1`, g.Task).Scan(&typ, &g.Initiator, &rec); err != nil {
		return nil, err
	}
	var record struct {
		Root string `json:"root"`
	}
	_ = json.Unmarshal(rec, &record)
	g.Root = record.Root
	if g.Root == "" {
		g.Root = g.Task
	}
	var role string
	if err := c.st.Pool.QueryRow(ctx, `SELECT role FROM workers WHERE id = $1`, g.Worker).Scan(&role); err != nil {
		return nil, err
	}
	g.Role = role
	tt := cat.TaskTypes[typ]
	if tt.TreeBudgetEUR > 0 {
		g.TreeBudget = money.FromEUR(tt.TreeBudgetEUR)
	}
	g.Line = "work"
	tv := cat.Tenants[g.Tenant]
	if tv == nil {
		return nil, refuse("not_ready", "tenant is not applied")
	}
	p, ok := cat.Profiles[g.ProfileName]
	if !ok {
		return nil, refuse("provider_config", "the model profile of the slot is not applied")
	}
	g.Profile = p
	if l := tv.Spec.ModelsAllowed; len(l) > 0 && !slices.Contains(l, g.ProfileName) {
		return nil, refuse("provider_config", "the profile is not in the tenant's models_allowed")
	}
	if l := tv.Spec.ProvidersAllowed; len(l) > 0 && !slices.Contains(l, p.Provider) {
		return nil, refuse("provider_config", "the provider is not in the tenant's providers_allowed")
	}
	found := false
	for _, s := range tv.Spec.Privacy.Subprocessors {
		if s.Provider == p.Provider {
			g.Subprocessor, found = s, true
		}
	}
	if !found {
		return nil, refuse("provider_config", "the provider is not a subprocessor of the tenant (SEC-8)")
	}
	if p.ProviderTraining == nil || p.ProviderTraining.Value {
		return nil, refuse("provider_config", "the profile does not declare provider_training: false")
	}
	if r := g.Subprocessor.Retention; r.Mode == "zero" || r.MaxDays != nil {
		allowed := 0
		if r.Mode != "zero" {
			allowed = *r.MaxDays
		}
		if p.ProviderRetentionDays == nil || p.ProviderRetentionDays.Value > allowed {
			return nil, refuse("provider_config", "the provider retention is above what the tenant allows (GW-15)")
		}
	}
	return &g, nil
}

// Reserve writes the worst-case reserve of a call before it goes out (GW-2, BUD-2). It
// refuses when the tenant windows, the person's day limit or the tree budget would be passed.
func (c *Core) Reserve(ctx context.Context, g *ModelGrant, micros int64) (string, error) {
	cat, err := c.Catalog(ctx)
	if err != nil {
		return "", err
	}
	id := ids.New(ids.Spend)
	err = c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		tv := cat.Tenants[g.Tenant]
		var groups []string
		if p, err := loadPrincipal(ctx, tx, now, g.Tenant, g.Initiator); err == nil {
			groups = groupClosure(tv, p.Groups)
		}
		if p := c.checkBudget(ctx, tx, now, tv, g.Initiator, groups); p != nil {
			return p
		}
		if g.TreeBudget > 0 {
			var used int64
			if err := tx.QueryRow(ctx, `SELECT coalesce(sum(eur_micros), 0) FROM spend WHERE root = $1`, g.Root).Scan(&used); err != nil {
				return err
			}
			if used+micros > g.TreeBudget {
				return refuse("budget_exhausted", "the call reserve does not fit the tree budget")
			}
		}
		_, err := tx.Exec(ctx, `INSERT INTO spend (id, tenant, task, root, subject, role, worker, line, unit, profile, state, eur_micros, time)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'tokens', $9, 'reserved', $10, $11)`,
			id, g.Tenant, g.Task, g.Root, g.Initiator, g.Role, g.Worker, g.Line, g.ProfileName, micros, now)
		return err
	})
	if err != nil {
		return "", err
	}
	return id, nil
}

// Tokens are the usage of one call by token kind.
type Tokens struct {
	Input      int64 `json:"input"`
	Output     int64 `json:"output"`
	CacheWrite int64 `json:"cache_write,omitempty"`
	CacheRead  int64 `json:"cache_read,omitempty"`
}

// Charge replaces a reserve by the actual usage line (BUD-3). Without an answer the caller
// charges the reserve itself with estimated set (BUD-11).
func (c *Core) Charge(ctx context.Context, spendID string, micros int64, tokens *Tokens, requestID string, estimated bool) error {
	state := "committed"
	if estimated {
		state = "estimated"
	}
	var tok []byte
	if tokens != nil {
		tok, _ = json.Marshal(tokens)
	}
	return c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		if estimated {
			_, err := tx.Exec(ctx, `UPDATE spend SET state = $2, request_id = $3 WHERE id = $1 AND state = 'reserved'`, spendID, state, nullable(requestID))
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE spend SET state = $2, eur_micros = $3, tokens = $4, request_id = $5 WHERE id = $1 AND state = 'reserved'`,
			spendID, state, micros, tok, nullable(requestID))
		return err
	})
}

// DropReserve removes a reserve of a call that never went out.
func (c *Core) DropReserve(ctx context.Context, spendID string) error {
	_, err := c.st.Pool.Exec(ctx, `DELETE FROM spend WHERE id = $1 AND state = 'reserved'`, spendID)
	return err
}
