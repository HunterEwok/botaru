// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// Fallback profiles (GW-5). P4 switches a call to a fallback profile only for a reason of the
// closed list spec/model/model-fallback-reasons.yaml that the slot profile names in its
// fallback_on, only to a profile of the slot profile's fallback list, in its order, and only to
// one the tenant admits like a slot profile (models_allowed, providers_allowed and the
// subprocessors with their terms, SEC-8), whose rank is not above the position ceiling of the
// acting identity at this call (GOV-3; apply checks the role ceiling), and that carries a pass
// attestation of the role's eval on it: an EvalReports entry of the role for its static
// fingerprint with the fallback in the slot and without security failures (PRV-3), or for the
// seat slot an mgr-inj entry of the seat on the fallback (MGR-23). The granted slot has no
// fallback: a person chose its profile (MGR-18). A call on a fallback writes the actual profile
// and the fallback marker into its usage line and the event model.fallback.v1.

// fallbackReasons is the closed list of model-fallback-reasons.yaml.
var fallbackReasons = []string{"overloaded", "unavailable", "retired"}

// SwitchesOn says whether a call of g goes to a fallback profile for reason: the reason is in
// the closed list and in the fallback_on of the slot profile, and the slot is not granted.
func SwitchesOn(g *ModelGrant, reason string) bool {
	return slices.Contains(fallbackReasons, reason) && slices.Contains(g.Profile.FallbackOn, reason) && g.Slot != grantedSlot
}

// Fallbacks gives the grants of the fallback profiles a call of g may switch to, in the order
// of the slot profile's fallback list; the caller sets FallbackReason. The reserve of the first
// call is at the most expensive of them and the slot profile (GW-6).
func (c *Core) Fallbacks(ctx context.Context, g *ModelGrant) ([]*ModelGrant, error) {
	if len(g.Profile.FallbackOn) == 0 || g.Slot == grantedSlot || g.FallbackOf != "" {
		return nil, nil
	}
	return c.fallbackGrants(ctx, g)
}

func (c *Core) fallbackGrants(ctx context.Context, g *ModelGrant) ([]*ModelGrant, error) {
	if len(g.Profile.Fallback) == 0 {
		return nil, nil
	}
	cat, err := c.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	tv := cat.Tenants[g.Tenant]
	if tv == nil {
		return nil, nil
	}
	attestedOn, err := c.fallbackAttested(ctx, cat.SetHash)
	if err != nil {
		return nil, err
	}
	reports := catalogEvalReports(cat)
	var out []*ModelGrant
	for _, name := range g.Profile.Fallback {
		if name == g.ProfileName {
			continue
		}
		p, ok := cat.Profile(g.Tenant, name)
		if !ok {
			continue
		}
		sub, err := profileAdmitted(tv, name, p)
		if _, refused := AsProblem(err); refused {
			continue
		} else if err != nil {
			return nil, err
		}
		fb := *g
		fb.ProfileName, fb.Profile, fb.Subprocessor = name, p, sub
		fb.FallbackOf = g.ProfileName
		if ok, err := c.rankAllowed(ctx, cat, &fb); err != nil {
			return nil, err
		} else if !ok {
			continue
		}
		attested := false
		switch g.Slot {
		case "base", "analysis":
			if attested, err = attestedOn(g.Role, g.Tenant, g.Slot, name); err != nil {
				return nil, err
			}
		case "seat":
			fp, err := seatFingerprint(cat, name)
			if err != nil {
				return nil, err
			}
			attested = fp != "" && seatEvalPassed(reports, name, fp)
		}
		if attested {
			out = append(out, &fb)
		}
	}
	return out, nil
}

// fallbackCache keeps, for one manifest set, the eval index and whether the role's eval passed
// on a fallback profile in a slot, so a model call does not read the manifests again: the set
// hash changes with every apply.
type fallbackCache struct {
	mu       sync.Mutex
	hash     string
	index    evalIndex
	attested map[[4]string]bool
}

// fallbackAttested gives the check of a pass attestation of the role's eval on a fallback in a
// base or analysis slot for the manifest set of the hash.
func (c *Core) fallbackAttested(ctx context.Context, hash string) (func(role, tenant, slot, profile string) (bool, error), error) {
	fc := &c.fallbacks
	fc.mu.Lock()
	defer fc.mu.Unlock()
	if fc.index == nil || fc.hash != hash {
		docs, loaded, err := loadManifests(ctx, c.st.Pool)
		if err != nil {
			return nil, err
		}
		fc.hash, fc.index, fc.attested = loaded, newEvalIndex(docs), map[[4]string]bool{}
	}
	x, memo := fc.index, fc.attested
	return func(role, tenant, slot, profile string) (bool, error) {
		key := [4]string{role, tenant, slot, profile}
		fc.mu.Lock()
		ok, seen := memo[key]
		fc.mu.Unlock()
		if seen {
			return ok, nil
		}
		fp, found, err := x.fingerprintOn(role, tenant, slot, profile)
		if err != nil {
			return false, err
		}
		ok = found && x.reported(role, fp)
		fc.mu.Lock()
		memo[key] = ok
		fc.mu.Unlock()
		return ok, nil
	}, nil
}

// recordFallback marks the usage line of a call on a fallback profile and writes the event
// model.fallback.v1 (GW-5).
func (c *Core) recordFallback(ctx context.Context, tx pgx.Tx, now time.Time, g *ModelGrant, spendID string) error {
	if _, err := tx.Exec(ctx, `UPDATE spend SET fallback = true, slot_profile = $2 WHERE id = $1`, spendID, g.FallbackOf); err != nil {
		return err
	}
	data := map[string]any{"task": g.Task, "profile": g.FallbackOf, "fallback_profile": g.ProfileName, "reason": g.FallbackReason}
	if g.Slot != "" {
		data["slot"] = g.Slot
	}
	return c.appendEvent(ctx, tx, now, g.Tenant, Event{Type: "model.fallback.v1", Subject: g.Task, Data: data})
}

// FallbackFingerprint is the static fingerprint the pass attestation of a role's eval on a
// fallback profile carries in the EvalReports index: the role's static fingerprint in the
// tenant with the profile in the slot (base or analysis) instead of the role's own (GW-5,
// spec/hashing.md). An eval run writes it into the report of such a run.
func (c *Core) FallbackFingerprint(ctx context.Context, role, tenant, slot, profile string) (string, error) {
	docs, _, err := loadManifests(ctx, c.st.Pool)
	if err != nil {
		return "", err
	}
	fp, ok, err := newEvalIndex(docs).fingerprintOn(role, tenant, slot, profile)
	if err != nil || !ok {
		return "", err
	}
	return fp, nil
}
