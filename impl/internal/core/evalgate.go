// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/jcs"
	"github.com/HunterEwok/botaru/impl/internal/manifest"
	"github.com/HunterEwok/botaru/impl/internal/version"
)

// The quality gate on a static fingerprint change (PRV-3). The static fingerprint of a role in a
// tenant is the row of spec/hashing.md: the role manifest, its claimed types, its skills, the
// persona of its position, the base and analysis model profiles, the runtime driver with the
// major and minor digits of its version, and the sandbox digest. A role is enabled in every
// tenant, as roles are global; apply checks a role of tier 1 or higher in each tenant where it
// is new or where its static fingerprint changed, so a role that is already enabled stays
// enabled after a core upgrade without apply.

// evalIndex is a manifest set by kind, tenant and name.
type evalIndex map[manifest.Key]manifest.Doc

func newEvalIndex(docs []manifest.Doc) evalIndex {
	out := evalIndex{}
	for _, d := range docs {
		k := d.Key()
		if d.Kind == "Role" || d.Kind == "TaskType" || d.Kind == "EvalReports" || d.Kind == "Tenant" {
			k.Tenant = ""
		}
		out[k] = d
	}
	return out
}

func (x evalIndex) spec(kind, tenant, name string) map[string]any {
	d, ok := x[manifest.Key{Kind: kind, Tenant: tenant, Name: name}]
	if !ok {
		return nil
	}
	s, _ := d.Value["spec"].(map[string]any)
	return s
}

// governance is the charter spec of a tenant, nil without one.
func (x evalIndex) governance(tenant string) map[string]any {
	var names []string
	for k := range x {
		if k.Kind == "Governance" && k.Tenant == tenant {
			names = append(names, k.Name)
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		return nil
	}
	return x.spec("Governance", tenant, names[0])
}

// majorMinor is the major and minor digits of a version ("v0.2.7" gives "0.2"); a version
// without them stays as it is (D-103).
func majorMinor(v string) string {
	parts := strings.Split(strings.TrimPrefix(v, "v"), ".")
	if len(parts) < 2 {
		return v
	}
	return parts[0] + "." + parts[1]
}

// roleTier is the highest tier of the types a role claims.
func (x evalIndex) roleTier(role map[string]any) int {
	tier := 0
	for _, tn := range mstrs(role, "claims") {
		if n, ok := mnum(x.spec("TaskType", "", tn), "tier"); ok && int(n) > tier {
			tier = int(n)
		}
	}
	return tier
}

// staticFingerprint is the static fingerprint of a role in a tenant (spec/hashing.md, PRV-3);
// ok is false when the set has no such role.
func (x evalIndex) staticFingerprint(role, tenant string) (string, bool, error) {
	d, ok := x[manifest.Key{Kind: "Role", Name: role}]
	if !ok {
		return "", false, nil
	}
	rs, _ := d.Value["spec"].(map[string]any)
	roleHash, err := jcs.Hash(d.Value)
	if err != nil {
		return "", false, err
	}
	claims := slices.Clone(mstrs(rs, "claims"))
	sort.Strings(claims)
	types := []any{}
	for _, tn := range claims {
		if td, ok := x[manifest.Key{Kind: "TaskType", Name: tn}]; ok {
			h, err := jcs.Hash(td.Value)
			if err != nil {
				return "", false, err
			}
			types = append(types, h)
		}
	}
	// The skills part as in the run fingerprint: this build gives a run no skills.
	skills, err := jcs.Hash([]any{})
	if err != nil {
		return "", false, err
	}
	var persona any
	var ref personaRef
	switch pos := profilePosition(mstr(rs, "profile")); pos {
	case "farm_manager":
		ref = personaOf(mmap(mmap(x.governance(farmTenant), "farm_manager"), "persona"))
	default:
		ref = personaOf(mmap(mmap(mmap(x.governance(tenant), "positions"), pos), "persona"))
	}
	if ref.SHA256 != "" {
		if persona, err = jcs.Hash([]any{ref.SHA256}); err != nil {
			return "", false, err
		}
	}
	models := mmap(rs, "models")
	driver := mstr(mmap(rs, "runtime"), "driver")
	if driver == "" {
		driver = "core" // the core agent loop (D-93)
	}
	fp, err := jcs.Hash(map[string]any{
		"role": roleHash, "types": types, "skills": skills, "persona": persona,
		"models":  map[string]any{"base": nullable(mstr(models, "base")), "analysis": nullable(mstr(models, "analysis"))},
		"runtime": map[string]any{"driver": driver, "version": majorMinor(version.Version)},
		"sandbox": nil,
	})
	return fp, true, err
}

// profilePosition is the position of a role without a seat by its profile, as positionOf.
func profilePosition(profile string) string {
	switch profile {
	case "reviewer":
		return "commission"
	case "translator":
		return "translator"
	case "farm-manager", "farm-manager-reader":
		return "farm_manager"
	}
	return "functional"
}

// reported says whether the EvalReports index has an entry for the role with the fingerprint
// and zero security failures.
func (x evalIndex) reported(role, fp string) bool {
	for k, d := range x {
		if k.Kind != "EvalReports" {
			continue
		}
		s, _ := d.Value["spec"].(map[string]any)
		for _, r := range mlist(s, "reports") {
			rm, _ := r.(map[string]any)
			sub := mmap(rm, "subject")
			if mstr(sub, "kind") == "role" && mstr(sub, "name") == role && mstr(rm, "fingerprint") == fp {
				if n, ok := mnum(rm, "security_failures"); ok && n == 0 {
					return true
				}
			}
		}
	}
	return false
}

// tenantKind is the kind of a tenant manifest; production without the field (APR-9).
func tenantKind(s map[string]any) string {
	if k := mstr(s, "kind"); k != "" {
		return k
	}
	return "production"
}

// evalGate checks the set after an apply against the one before it (PRV-3): a role of tier 1
// or higher that is new or whose static fingerprint changed in a tenant needs an EvalReports
// entry for the new fingerprint with zero security failures. In a production tenant the role
// is not enabled (an error eval_report_missing); in a test tenant the apply passes with a
// warning. index gives the place of each object of after in the request (-1 for the store).
func (c *Core) evalGate(after []manifest.Doc, index []int, before []manifest.Doc) (errs, warns []ApplyError, err error) {
	cur, old := newEvalIndex(after), newEvalIndex(before)
	at := map[string]int{}
	for i, d := range after {
		if d.Kind == "Role" {
			at[d.Name] = index[i]
		}
	}
	var tenants []string
	for k := range cur {
		if k.Kind == "Tenant" {
			tenants = append(tenants, k.Name)
		}
	}
	sort.Strings(tenants)
	roles := slices.Sorted(func(yield func(string) bool) {
		for k := range cur {
			if k.Kind == "Role" && !yield(k.Name) {
				return
			}
		}
	})
	for _, role := range roles {
		if cur.roleTier(cur.spec("Role", "", role)) < 1 {
			continue
		}
		warned := false
		for _, tenant := range tenants {
			fp, _, err := cur.staticFingerprint(role, tenant)
			if err != nil {
				return nil, nil, err
			}
			if _, had := old[manifest.Key{Kind: "Tenant", Name: tenant}]; had {
				if prev, ok, err := old.staticFingerprint(role, tenant); err != nil {
					return nil, nil, err
				} else if ok && prev == fp {
					continue
				}
			}
			if cur.reported(role, fp) {
				continue
			}
			e := ApplyError{Kind: "Role", Name: role, Code: "eval_report_missing", Path: "/store/spec"}
			if i := at[role]; i >= 0 {
				e.Path = fmt.Sprintf("/manifests/%d/spec", i)
			}
			// Any production tenant refuses the role; a test tenant only warns, once per role,
			// and the production tenants after it are still checked.
			if tenantKind(cur.spec("Tenant", "", tenant)) == "production" {
				errs = append(errs, e)
				break
			}
			if !warned {
				warns, warned = append(warns, e), true
			}
		}
	}
	return errs, warns, nil
}

// doctorEval warns eval_report_stale for each role of tier 1 or higher whose current static
// fingerprint in a production tenant has no EvalReports entry, as after a core minor upgrade
// without apply (PRV-3).
func (c *Core) doctorEval(ctx context.Context, tx pgx.Tx, _ time.Time, tenant string, add func(DoctorCheck)) error {
	docs, _, err := loadManifests(ctx, tx)
	if err != nil {
		return err
	}
	x := newEvalIndex(docs)
	ts := x.spec("Tenant", "", tenant)
	if ts == nil || tenantKind(ts) != "production" {
		return nil
	}
	stale := []string{}
	for k := range x {
		if k.Kind != "Role" || x.roleTier(x.spec("Role", "", k.Name)) < 1 {
			continue
		}
		fp, _, err := x.staticFingerprint(k.Name, tenant)
		if err != nil {
			return err
		}
		if !x.reported(k.Name, fp) {
			stale = append(stale, k.Name)
		}
	}
	sort.Strings(stale)
	ch := DoctorCheck{ID: "eval_report_stale", OK: len(stale) == 0, Severity: "info", Value: stale, Tenant: tenant}
	if len(stale) > 0 {
		ch.Severity = "warn"
	}
	add(ch)
	return nil
}
