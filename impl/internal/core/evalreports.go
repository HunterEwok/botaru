// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/jcs"
	"github.com/HunterEwok/botaru/impl/internal/manifest"
	"github.com/HunterEwok/botaru/impl/internal/version"
)

// The eval report index (spec/manifests/eval-reports.schema.json): the EvalReports manifests of
// the installation hold only run metadata, the static fingerprint, the subject, the set, the date
// and the failure counts. The core reads it for the translator gate at apply (TRN-6), the seat
// check before seat work (MGR-23) and the grant sign of a type's history (MGR-17). This file is a
// small reader of its own; the quality gate of PRV-3 reads the same index.

const (
	// suiteManagerInjection is the eval set of the farm manager seat (MGR-23).
	suiteManagerInjection = "mgr-inj"
	// suiteTranslator and suiteTranslatorInjection are the translator's correctness and
	// injection sets (TRN-6).
	suiteTranslator          = "trn"
	suiteTranslatorInjection = "trn-inj"
	// translatorMinCases is the smallest correctness set of a translator (TRN-6).
	translatorMinCases = 30
	// defaultStructuralMatch is evals.translator_structural_match when the tenant sets none.
	defaultStructuralMatch = 0.9
)

// evalReport is one entry of the index.
type evalReport struct {
	Fingerprint                       string
	SubjectKind, SubjectName          string
	Suite, Date                       string
	Cases, Failures, SecurityFailures int
	StructuralMatch, BaseFailShare    *float64
}

// evalReportsOf reads the entries of EvalReports specs.
func evalReportsOf(specs []map[string]any) []evalReport {
	var out []evalReport
	for _, s := range specs {
		for _, x := range mlist(s, "reports") {
			m := asMap(x)
			sub := mmap(m, "subject")
			r := evalReport{Fingerprint: mstr(m, "fingerprint"), SubjectKind: mstr(sub, "kind"), SubjectName: mstr(sub, "name"),
				Suite: mstr(m, "suite"), Date: mstr(m, "date")}
			n := func(k string) int { v, _ := mnum(m, k); return int(v) }
			r.Cases, r.Failures, r.SecurityFailures = n("cases"), n("failures"), n("security_failures")
			if v, ok := mnum(m, "structural_match"); ok {
				r.StructuralMatch = &v
			}
			if v, ok := mnum(m, "base_fail_share"); ok {
				r.BaseFailShare = &v
			}
			out = append(out, r)
		}
	}
	return out
}

// catalogEvalReports are the entries of every applied EvalReports manifest.
func catalogEvalReports(cat *Catalog) []evalReport {
	var specs []map[string]any
	keys := make([]manifest.Key, 0)
	for k := range cat.RawGlobals {
		if k.Kind == "EvalReports" {
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].Name < keys[j].Name })
	for _, k := range keys {
		specs = append(specs, mmap(cat.RawGlobals[k], "spec"))
	}
	return evalReportsOf(specs)
}

// seatEvalPassed says whether the index has an mgr-inj entry for the seat model profile, with the
// current seat fingerprint, and no security failure (MGR-23).
func seatEvalPassed(reports []evalReport, profile, fingerprint string) bool {
	return profile != "" && slices.ContainsFunc(reports, func(r evalReport) bool {
		return r.SubjectKind == "seat" && r.SubjectName == profile && r.Fingerprint == fingerprint &&
			r.Suite == suiteManagerInjection && r.SecurityFailures == 0
	})
}

// seatFingerprint is the fingerprint an mgr-inj entry of the seat carries (eval-reports.schema:
// "for a seat, of the seat profile"): the hash of the profile name, the hash of the whole
// ModelProfile manifest as applied (the farm's own, else the farm-wide one) and the persona pin
// of the farm manager position, so any change of the profile or the manager persona asks for a
// new report (MGR-23, GOV-4).
func seatFingerprint(cat *Catalog, profile string) (string, error) {
	h := cat.Hashes[manifest.Key{Kind: "ModelProfile", Tenant: farmTenant, Name: profile}]
	if h == "" {
		h = cat.Hashes[manifest.Key{Kind: "ModelProfile", Name: profile}]
	}
	if h == "" {
		return "", nil
	}
	parts := map[string]any{"profile": profile, "manifest": h, "persona": nil}
	if pr := personaOf(mmap(managerCharter(cat), "persona")); pr.pinned() {
		parts["persona"] = pr.SHA256
	}
	return jcs.Hash(parts)
}

// translatorEvalPassed says whether a translator role's static fingerprint has both sets: the
// correctness set of at least 30 cases with the structural match not below the threshold, and
// the injection set with no security failure (TRN-6).
func translatorEvalPassed(reports []evalReport, role, fingerprint string, threshold float64) bool {
	var correct, inject bool
	for _, r := range reports {
		if r.SubjectKind != "role" || r.SubjectName != role || r.Fingerprint != fingerprint {
			continue
		}
		switch r.Suite {
		case suiteTranslator:
			if r.Cases >= translatorMinCases && r.StructuralMatch != nil && *r.StructuralMatch >= threshold && r.SecurityFailures == 0 {
				correct = true
			}
		case suiteTranslatorInjection:
			if r.Cases > 0 && r.SecurityFailures == 0 {
				inject = true
			}
		}
	}
	return correct && inject
}

// typeBaseFailShare is the failure share of the newest run of a type's grant_eval_set on the
// base model, or nil when the index has none (MGR-17).
func typeBaseFailShare(reports []evalReport, typ, set string) *float64 {
	var best *evalReport
	for i, r := range reports {
		if r.SubjectKind != "task_type" || r.SubjectName != typ || r.Suite != set || r.BaseFailShare == nil {
			continue
		}
		if best == nil || r.Date > best.Date {
			best = &reports[i]
		}
	}
	if best == nil {
		return nil
	}
	return best.BaseFailShare
}

var reMajorMinor = regexp.MustCompile(`^v?(\d+)\.(\d+)`)

// runtimeMajorMinor is the version part of the static fingerprint's runtime: only the major and
// minor digits, so a patch release does not change it (D-103).
func runtimeMajorMinor(v string) string {
	if m := reMajorMinor.FindStringSubmatch(v); m != nil {
		return m[1] + "." + m[2]
	}
	return v
}

// staticFingerprint is the static fingerprint of spec/hashing.md from its parts: the hash of the
// role manifest, the hashes of its claimed type manifests by type name, the persona pin hash of
// its position ("" for none), its base and analysis profiles and its runtime driver ("" for the
// core loop). Skills are not built, so their part is the hash of the empty list as in the run
// fingerprint; there is no sandbox (PRV-1, PRV-3).
func staticFingerprint(roleHash string, typeHashes map[string]string, persona, base, analysis, driver string) (string, error) {
	types := []any{}
	for _, name := range slices.Sorted(mapKeys(typeHashes)) {
		types = append(types, typeHashes[name])
	}
	typesHash, err := jcs.Hash(types)
	if err != nil {
		return "", err
	}
	skills, err := jcs.Hash([]any{})
	if err != nil {
		return "", err
	}
	parts := map[string]any{"role": nullable(roleHash), "types": typesHash, "skills": skills, "persona": nil, "sandbox": nil}
	if persona != "" {
		if parts["persona"], err = jcs.Hash([]any{persona}); err != nil {
			return "", err
		}
	}
	if parts["models"], err = jcs.Hash(map[string]any{"base": nullable(base), "analysis": nullable(analysis)}); err != nil {
		return "", err
	}
	if driver == "" {
		driver = "core"
	}
	if parts["runtime"], err = jcs.Hash(map[string]any{"driver": driver, "version": runtimeMajorMinor(version.Version)}); err != nil {
		return "", err
	}
	return jcs.Hash(parts)
}

// trnState is the translator gate of one translator role in one production tenant of a set: its
// static fingerprint there and whether the index of the set has both passing sets for it.
type trnState struct {
	tenant, role lintObj
	gov          *lintObj
	fp           string
	passed       bool
	types        []string
}

// translatorStates computes the gate of every translator role in every production tenant of a
// set (a tenant without kind is one, APR-9). The fingerprint takes the persona the tenant's
// charter pins for the translator position, and the threshold is the tenant's
// evals.translator_structural_match (TRN-6).
func translatorStates(docs []manifest.Doc, S *lintSet) (map[[2]string]*trnState, error) {
	var specs []map[string]any
	for _, o := range S.each("EvalReports") {
		specs = append(specs, o.spec)
	}
	reports := evalReportsOf(specs)
	hashes := map[manifest.Key]string{}
	for _, d := range docs {
		if d.Kind == "Role" || d.Kind == "TaskType" {
			h, err := jcs.Hash(d.Value)
			if err != nil {
				return nil, err
			}
			hashes[manifest.Key{Kind: d.Kind, Name: d.Name}] = h
		}
	}
	out := map[[2]string]*trnState{}
	for _, t := range S.each("Tenant") {
		if mstr(t.spec, "kind") == "test" {
			continue
		}
		threshold := defaultStructuralMatch
		if v, ok := mnum(mmap(t.spec, "evals"), "translator_structural_match"); ok {
			threshold = v
		}
		var gov map[string]any
		var govObj *lintObj
		for _, g := range S.each("Governance") {
			if g.tenant == t.name {
				gov, govObj = g.spec, &g
				break
			}
		}
		pin := ""
		if persona := personaOf(mmap(mmap(mmap(gov, "positions"), "translator"), "persona")); persona.pinned() {
			pin = persona.SHA256
		}
		for _, r := range S.each("Role") {
			if mstr(r.spec, "profile") != "translator" {
				continue
			}
			types := map[string]string{}
			for _, typ := range mstrs(r.spec, "claims") {
				types[typ] = hashes[manifest.Key{Kind: "TaskType", Name: typ}]
			}
			models := mmap(r.spec, "models")
			fp, err := staticFingerprint(hashes[manifest.Key{Kind: "Role", Name: r.name}], types, pin, mstr(models, "base"), mstr(models, "analysis"),
				mstr(mmap(r.spec, "runtime"), "driver"))
			if err != nil {
				return nil, err
			}
			st := &trnState{role: r, gov: govObj, fp: fp, passed: translatorEvalPassed(reports, r.name, fp, threshold), types: slices.Sorted(mapKeys(types))}
			st.tenant = t
			out[[2]string{t.name, r.name}] = st
		}
	}
	return out, nil
}

// lintTranslatorEvals refuses a translator role of a production tenant whose static fingerprint
// has no passing reports of both translator sets in the index of the set (TRN-6,
// eval_report_missing), as PRV-3 does: only when the apply enables the role in the tenant (a new
// role, or a tenant that becomes production), moves one of its parts (the role, a claimed type,
// the persona pin; a new fingerprint), or takes its passing entry away (an EvalReports change or a
// higher threshold). A role already enabled that went stale with none of these, as after a core
// minor upgrade, stays enabled, and doctor warns eval_report_stale. The finding goes on the object
// of the apply that moved it (the role, a claimed type, the Governance, the tenant, the EvalReports
// manifest that held the role's entries, in this order) and names the tenant and the expected
// fingerprint. prior is the stored set before the apply.
func lintTranslatorEvals(docs []manifest.Doc, S *lintSet, prior []manifest.Doc, f *lintFindings) error {
	now, err := translatorStates(docs, S)
	if err != nil {
		return err
	}
	pindex := make([]int, len(prior))
	for i := range pindex {
		pindex[i] = -1
	}
	P := newLintSet(prior, pindex)
	before, err := translatorStates(prior, P)
	if err != nil {
		return err
	}
	submitted := func(kind, name, tenant string) (lintObj, bool) {
		for _, o := range S.each(kind) {
			if o.i >= 0 && o.name == name && (tenant == "" || o.tenant == tenant) {
				return o, true
			}
		}
		return lintObj{}, false
	}
	// The submitted EvalReports manifest whose stored version held entries of the role.
	heldEntries := func(role string) (lintObj, bool) {
		var first *lintObj
		for _, o := range S.each("EvalReports") {
			if o.i < 0 {
				continue
			}
			if first == nil {
				first = &o
			}
			if old := P.get("EvalReports", o.name, ""); old != nil {
				for _, r := range evalReportsOf([]map[string]any{old}) {
					if r.SubjectKind == "role" && r.SubjectName == role {
						return o, true
					}
				}
			}
		}
		if first != nil {
			return *first, true
		}
		return lintObj{}, false
	}
	keys := slices.SortedFunc(mapKeys(now), func(a, b [2]string) int { return strings.Compare(a[0]+"/"+a[1], b[0]+"/"+b[1]) })
	for _, k := range keys {
		st := now[k]
		if st.passed || f.refuses(st.role) {
			continue
		}
		if old, ok := before[k]; ok && old.fp == st.fp && !old.passed {
			continue // stale before this apply: doctor warns eval_report_stale
		}
		on, found := st.role, st.role.i >= 0
		for _, typ := range st.types {
			if found {
				break
			}
			on, found = submitted("TaskType", typ, "")
		}
		if !found && st.gov != nil && st.gov.i >= 0 {
			on, found = *st.gov, true
		}
		if !found && st.tenant.i >= 0 {
			on, found = st.tenant, true
		}
		if !found {
			if o, ok := heldEntries(st.role.name); ok {
				on, found = o, true
			}
		}
		if !found {
			on = st.role
		}
		f.addDetail(on, "eval_report_missing", "", fmt.Sprintf("role %s in tenant %s needs reports for static fingerprint %s", st.role.name, st.tenant.name, st.fp))
	}
	return nil
}

// staleTranslatorRoles are the translator roles of a production tenant of the stored set whose
// current static fingerprint has no passing reports: roles that stay enabled until an apply moves
// them, which doctor warns as eval_report_stale (PRV-3, TRN-6).
func staleTranslatorRoles(stored []manifest.Doc, kinds map[string]kindInfo, tenant string) ([]string, error) {
	docs := make([]manifest.Doc, len(stored))
	index := make([]int, len(stored))
	for i, d := range stored {
		if kinds[d.Kind].scope == "global" {
			d.Tenant = ""
		}
		docs[i], index[i] = d, -1
	}
	states, err := translatorStates(docs, newLintSet(docs, index))
	if err != nil {
		return nil, err
	}
	var out []string
	for k, st := range states {
		if k[0] == tenant && !st.passed {
			out = append(out, k[1])
		}
	}
	slices.Sort(out)
	return out, nil
}

// doctorEvals warns eval_report_stale, one check per translator role of a production tenant that
// stays enabled while its current static fingerprint has no passing reports (PRV-3, TRN-6).
func (c *Core) doctorEvals(ctx context.Context, tx pgx.Tx, tenant string, add func(DoctorCheck)) error {
	stored, _, err := loadManifests(ctx, tx)
	if err != nil {
		return err
	}
	kinds, err := c.manifestKinds()
	if err != nil {
		return err
	}
	roles, err := staleTranslatorRoles(stored, kinds, tenant)
	if err != nil {
		return err
	}
	for _, r := range roles {
		add(DoctorCheck{ID: "eval_report_stale", OK: false, Severity: "warn", Value: r, Tenant: tenant})
	}
	return nil
}

// productionTenant says whether a tenant counts as production: kind production, or no kind
// (APR-9).
func productionTenant(tv *TenantView) bool {
	return tv != nil && strings.TrimSpace(tv.Spec.Kind) != "test"
}

// seatProfile is the model profile the farm manager seat runs on: the farm charter's
// model_profile, else the main slot of the seat's role (MGR-1).
func seatProfile(cat *Catalog, role manifest.Role) string {
	if mp := mstr(managerCharter(cat), "model_profile"); mp != "" {
		return mp
	}
	_, p := mainSlot(role)
	return p
}

// seatEvalWait says whether a seat task of a production tenant must wait for the eval of the
// seat model profile, and sends the farm operators with admin:governance the notice
// seat_eval_missing_notice once a day per profile (MGR-23).
func (c *Core) seatEvalWait(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, waiting string, role manifest.Role) (bool, error) {
	profile := seatProfile(cat, role)
	fp, err := seatFingerprint(cat, profile)
	if err != nil {
		return false, err
	}
	if fp != "" && seatEvalPassed(catalogEvalReports(cat), profile, fp) {
		return false, nil
	}
	first, err := markOnce(ctx, tx, now, farmTenant, "seat_eval_missing:"+profile+":"+fp, 24*time.Hour)
	if err != nil || !first {
		return true, err
	}
	params := map[string]any{"profile": profile, "fingerprint": fp}
	params["tenant"] = waiting
	return true, c.noticeAdmins(ctx, tx, now, cat, []string{farmTenant}, "seat_eval_missing_notice", "notice.seat_eval_missing", params, "admin:governance")
}
