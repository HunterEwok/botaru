// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"fmt"
	"slices"
	"sort"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/jcs"
	"github.com/HunterEwok/botaru/impl/internal/manifest"
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

// staticFingerprint is the static fingerprint of a role in a tenant (spec/hashing.md, PRV-3)
// from the parts in the set, computed by the one function TRN-6 uses as well; ok is false when
// the set has no such role.
func (x evalIndex) staticFingerprint(role, tenant string) (string, bool, error) {
	return x.fingerprintOn(role, tenant, "", "")
}

// fingerprintOn is the static fingerprint of a role in a tenant with the profile of one model
// slot (base or analysis) taken as the named one, as for a fallback profile (GW-5); an empty
// slot keeps the role's own profiles.
func (x evalIndex) fingerprintOn(role, tenant, slot, profile string) (string, bool, error) {
	d, ok := x[manifest.Key{Kind: "Role", Name: role}]
	if !ok {
		return "", false, nil
	}
	rs, _ := d.Value["spec"].(map[string]any)
	roleHash, err := jcs.Hash(d.Value)
	if err != nil {
		return "", false, err
	}
	types := map[string]string{}
	for _, tn := range mstrs(rs, "claims") {
		if td, ok := x[manifest.Key{Kind: "TaskType", Name: tn}]; ok {
			if types[tn], err = jcs.Hash(td.Value); err != nil {
				return "", false, err
			}
		}
	}
	var ref personaRef
	switch pos := profilePosition(mstr(rs, "profile")); pos {
	case "farm_manager":
		ref = personaOf(mmap(mmap(x.governance(farmTenant), "farm_manager"), "persona"))
	default:
		ref = personaOf(mmap(mmap(mmap(x.governance(tenant), "positions"), pos), "persona"))
	}
	models := mmap(rs, "models")
	base, analysis := mstr(models, "base"), mstr(models, "analysis")
	switch slot {
	case "base":
		base = profile
	case "analysis":
		analysis = profile
	}
	fp, err := staticFingerprint(roleHash, types, ref.SHA256, base, analysis, mstr(mmap(rs, "runtime"), "driver"), skillRefsOf(rs))
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
// and zero security failures; the index is read by the one reader of evalreports.go.
func (x evalIndex) reported(role, fp string) bool {
	return slices.ContainsFunc(x.reports(), func(r evalReport) bool {
		return r.SubjectKind == "role" && r.SubjectName == role && r.Fingerprint == fp && r.SecurityFailures == 0
	})
}

// reports are the entries of the EvalReports manifests of the set.
func (x evalIndex) reports() []evalReport {
	var keys []manifest.Key
	for k := range x {
		if k.Kind == "EvalReports" {
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].Name < keys[j].Name })
	var specs []map[string]any
	for _, k := range keys {
		s, _ := x[k].Value["spec"].(map[string]any)
		specs = append(specs, s)
	}
	return evalReportsOf(specs)
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
				// A translator role's refusal in a production tenant is the stricter gate of
				// TRN-6 in the lint (both translator sets), so it is not given twice.
				if mstr(cur.spec("Role", "", role), "profile") == "translator" {
					continue
				}
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

// doctorEvals gives the one check eval_report_stale of a production tenant: the roles that stay
// enabled while their current static fingerprint has no passing entry, as after a core minor
// upgrade without apply. These are the roles of tier 1 or higher without an EvalReports entry
// with zero security failures (PRV-3) and the translator roles without both passing translator
// sets (TRN-6); the check warns when the list is not empty.
func (c *Core) doctorEvals(ctx context.Context, tx pgx.Tx, tenant string, add func(DoctorCheck)) error {
	docs, _, err := loadManifests(ctx, tx)
	if err != nil {
		return err
	}
	x := newEvalIndex(docs)
	ts := x.spec("Tenant", "", tenant)
	if ts == nil || tenantKind(ts) != "production" {
		return nil
	}
	kinds, err := c.manifestKinds()
	if err != nil {
		return err
	}
	stale, err := staleTranslatorRoles(docs, kinds, tenant)
	if err != nil {
		return err
	}
	for k := range x {
		if k.Kind != "Role" || x.roleTier(x.spec("Role", "", k.Name)) < 1 || slices.Contains(stale, k.Name) {
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
	if stale == nil {
		stale = []string{}
	}
	ch := DoctorCheck{ID: "eval_report_stale", OK: len(stale) == 0, Severity: "info", Value: stale, Tenant: tenant}
	if len(stale) > 0 {
		ch.Severity = "warn"
	}
	add(ch)
	return nil
}

// StaticFingerprint is the static fingerprint of a role in a tenant over the stored manifests
// (spec/hashing.md, PRV-3), the value an EvalReports entry of the role carries; ok is false
// when no such role is stored. The eval runner reads it for the index entries of its run.
func (c *Core) StaticFingerprint(ctx context.Context, tenant, role string) (fp string, ok bool, err error) {
	docs, _, err := loadManifests(ctx, c.st.Pool)
	if err != nil {
		return "", false, err
	}
	return newEvalIndex(docs).staticFingerprint(role, tenant)
}
