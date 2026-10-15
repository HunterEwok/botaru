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

	"github.com/HunterEwok/botaru/impl/internal/ids"
	"github.com/HunterEwok/botaru/impl/internal/manifest"
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
	Purpose      string
	// TreeBudget is the type's tree budget in micro-euros; 0 means none.
	TreeBudget int64
	// Fingerprint is the run fingerprint of the attempt, written on every usage line (PRV-1).
	Fingerprint string
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
	var slot, profile, fp *string
	err = c.st.Pool.QueryRow(ctx, `SELECT h.tenant, h.work_id, h.lease_no, h.epoch, h.worker_id, h.slot, h.profile, l.state, l.lease_until, l.fingerprint, botaru_now()
		FROM handles h JOIN leases l ON l.work_id = h.work_id AND l.lease_no = h.lease_no
		WHERE h.hash = ANY($1) AND h.kind = 'model'`, c.secretHashes(handle)).
		Scan(&g.Tenant, &g.Task, &leaseNo, &epoch, &g.Worker, &slot, &profile, &state, &until, &fp, &now)
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
	if fp != nil {
		g.Fingerprint = *fp
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
	g.TreeBudget = taskTreeBudget(rec, tt)
	g.Line, g.Purpose = c.taskLine(rec, tt)
	tv := cat.Tenants[g.Tenant]
	if tv == nil {
		return nil, refuse("not_ready", "tenant is not applied")
	}
	// The initiator's rights are checked again at every call: a membership that expired or a
	// right removed by apply after the claim stops the task data from reaching the model (PRM-8).
	if reason, err := c.accessLost(ctx, c.st.Pool, now, tv, g.Initiator, "task:"+typ, c.taskMark(rec, tt.DataClass)); err != nil {
		return nil, err
	} else if reason != "" {
		return nil, refuse(reason, "the initiator no longer has access to this task")
	}
	p, ok := cat.Profile(g.Tenant, g.ProfileName)
	if !ok {
		return nil, refuse("provider_config", "the model profile of the slot is not applied")
	}
	g.Profile = p
	// The rank of the slot's profile against the ceiling of the acting identity's position
	// (GOV-3).
	if ok, err := c.rankAllowed(ctx, cat, &g); err != nil {
		return nil, err
	} else if !ok {
		return nil, refuse("rank_exceeded", "the model profile is above the rank ceiling of the acting position")
	}
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
// refuses when the tenant windows, the budget line, the person's day limit or the tree budget
// would be passed (BUD-7).
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
		return c.reserveSpend(ctx, tx, now, tv, spendRow{
			ID: id, Tenant: g.Tenant, Task: g.Task, Root: g.Root, Subject: g.Initiator, Role: g.Role, Worker: g.Worker,
			Line: g.Line, Purpose: g.Purpose, Unit: "tokens", Profile: g.ProfileName, Provider: strings.ToLower(g.Profile.Provider), Fingerprint: g.Fingerprint, Micros: micros,
		}, groups, g.TreeBudget)
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
			_, err := tx.Exec(ctx, `UPDATE spend SET state = $2, request_id = $3, settled_at = $4 WHERE id = $1 AND state = 'reserved'`, spendID, state, nullable(requestID), now)
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE spend SET state = $2, eur_micros = $3, tokens = $4, request_id = $5, settled_at = $6 WHERE id = $1 AND state = 'reserved'`,
			spendID, state, micros, tok, nullable(requestID), now)
		return err
	})
}

// DropReserve removes a reserve of a call that never went out.
func (c *Core) DropReserve(ctx context.Context, spendID string) error {
	_, err := c.st.Pool.Exec(ctx, `DELETE FROM spend WHERE id = $1 AND state = 'reserved'`, spendID)
	return err
}
