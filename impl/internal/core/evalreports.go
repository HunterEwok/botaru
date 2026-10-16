// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
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

// seatEvalPassed says whether the index has an mgr-inj entry for the seat model profile with no
// security failure (MGR-23).
func seatEvalPassed(reports []evalReport, profile string) bool {
	return profile != "" && slices.ContainsFunc(reports, func(r evalReport) bool {
		return r.SubjectKind == "seat" && r.SubjectName == profile && r.Suite == suiteManagerInjection && r.SecurityFailures == 0
	})
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

// lintTranslatorEvals refuses, when the set holds a production tenant (a tenant without kind is
// one, APR-9), a role of profile translator whose static fingerprint has no passing reports of
// both translator sets in the index of the set (TRN-6, eval_report_missing). The fingerprint is
// taken per production tenant, with the persona its charter pins for the translator position,
// and the threshold is that tenant's evals.translator_structural_match.
func lintTranslatorEvals(docs []manifest.Doc, S *lintSet, f *lintFindings) error {
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
				return err
			}
			hashes[manifest.Key{Kind: d.Kind, Name: d.Name}] = h
		}
	}
	for _, t := range S.each("Tenant") {
		if mstr(t.spec, "kind") == "test" {
			continue
		}
		threshold := defaultStructuralMatch
		if v, ok := mnum(mmap(t.spec, "evals"), "translator_structural_match"); ok {
			threshold = v
		}
		var gov map[string]any
		for _, g := range S.each("Governance") {
			if g.tenant == t.name {
				gov = g.spec
				break
			}
		}
		persona := personaOf(mmap(mmap(mmap(gov, "positions"), "translator"), "persona"))
		pin := ""
		if persona.pinned() {
			pin = persona.SHA256
		}
		for _, r := range S.each("Role") {
			// Only a role of the submitted set is checked (a stored role stays enabled until its
			// parts change), and a role the lint already refuses needs no second refusal.
			if mstr(r.spec, "profile") != "translator" || r.i < 0 || f.refuses(r) {
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
				return err
			}
			if !translatorEvalPassed(reports, r.name, fp, threshold) {
				f.add(r, "eval_report_missing", "")
			}
		}
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
	if seatEvalPassed(catalogEvalReports(cat), profile) {
		return false, nil
	}
	first, err := markOnce(ctx, tx, now, farmTenant, "seat_eval_missing:"+profile, 24*time.Hour)
	if err != nil || !first {
		return true, err
	}
	params := map[string]any{"profile": profile}
	params["tenant"] = waiting
	return true, c.noticeAdmins(ctx, tx, now, cat, []string{farmTenant}, "seat_eval_missing_notice", "notice.seat_eval_missing", params, "admin:governance")
}
