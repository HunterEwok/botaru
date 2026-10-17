// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/HunterEwok/botaru/impl/internal/manifest"
	"github.com/HunterEwok/botaru/impl/internal/money"
	"github.com/HunterEwok/botaru/impl/internal/spec"
)

// Lint checks of privileges, lenses, charters, governance and the manager line.

// lintPrivilegeRaised refuses a set in which an ordinary group became privileged (PRM-13): a
// group of the store that was not privileged is privileged after the set, by its own
// capabilities or by a type it names, whether or not it has members. A group that has
// memberships in the store (held) counts as a group of the store even when the stored set no
// longer has it, and one the stored set lacks counts as not privileged before: otherwise a
// delete and a second apply would hand its members a privilege without the code. Only a group
// new in the set and without members is privileged or not from the start and is granted under
// PRM-13.
func lintPrivilegeRaised(S *lintSet, f *lintFindings, docs, prior []manifest.Doc, held map[string]bool) {
	if len(prior) == 0 && len(held) == 0 {
		return
	}
	before, err := buildCatalog(prior)
	if err != nil {
		return
	}
	after, err := buildCatalog(docs)
	if err != nil {
		return
	}
	for _, o := range S.each("Group") {
		ta := after.Tenants[o.tenant]
		if ta == nil {
			continue
		}
		was, stored := false, false
		if tb := before.Tenants[o.tenant]; tb != nil {
			if _, stored = tb.Groups[o.name]; stored {
				was = grantsPrivilege(tb, before, o.name, map[string]bool{})
			}
		}
		if !stored && !held[o.tenant+"/"+o.name] {
			continue
		}
		if !was && grantsPrivilege(ta, after, o.name, map[string]bool{}) {
			f.add(o, "privileged_group", "/caps")
		}
	}
}

// grantsPrivilege applies the PRM-13 rule to what a group grants in one state of the set: a
// task capability counts by the types it covers there (a wildcard by prefix, capCovers), so a
// capability of a type that is not in the set grants nothing, unlike privilegedGroup, which
// counts anything unknown as privileged for a grant.
func grantsPrivilege(tv *TenantView, cat *Catalog, name string, seen map[string]bool) bool {
	if seen[name] {
		return false
	}
	seen[name] = true
	g, ok := tv.Groups[name]
	if !ok {
		return false
	}
	for _, cp := range g.Caps {
		if unprivilegedCap(cp) {
			continue
		}
		if !strings.HasPrefix(cp, "task:") {
			return true
		}
		for tn, tt := range cat.TaskTypes {
			if tt.Tier >= 1 && capCovers(cp, "task:"+tn) {
				return true
			}
		}
	}
	for _, d := range g.Data {
		if d != "summary" && d != "internal" {
			return true
		}
	}
	for _, in := range g.Inherits {
		if grantsPrivilege(tv, cat, in, seen) {
			return true
		}
	}
	return false
}

// lintLensNames checks the lenses that a charter seat and the commission manifest name: a lens
// of spec/model/lenses.yaml of the claimed version or a tenant lens x_<name> declared with its
// meaning in the tenant's charter, else lens_unknown (COM-19). The weight of a lens that has
// one by rule (farm_manager, from v0.2) is not set by the manifest (COM-8).
func (c *Core) lintLensNames(S *lintSet, f *lintFindings) error {
	reg, err := c.spec.Registry("spec/model/lenses.yaml")
	if err != nil {
		return err
	}
	skeleton := map[string]map[string]any{}
	for _, e := range reg.Entries {
		if !spec.VersionLater(asString(e["since"]), spec.Version) {
			skeleton[asString(e["id"])] = e
		}
	}
	known := func(tenant, lens string) bool {
		if _, ok := skeleton[lens]; ok {
			return true
		}
		if !strings.HasPrefix(lens, "x_") {
			return false
		}
		for _, ch := range S.each("Governance") {
			if ch.tenant == tenant && mstr(mmap(mmap(mmap(ch.spec, "commission"), "lenses"), lens), "meaning") != "" {
				return true
			}
		}
		return false
	}
	for _, o := range S.each("Governance") {
		for i, sv := range mlist(mmap(o.spec, "commission"), "seats") {
			sm, _ := sv.(map[string]any)
			if l := mstr(sm, "lens"); l != "" && !known(o.tenant, l) {
				f.add(o, "lens_unknown", fmt.Sprintf("/commission/seats/%d/lens", i))
			}
		}
	}
	for _, o := range S.each("Commission") {
		for _, k := range []string{"weights", "min"} {
			for _, l := range slices.Sorted(mapKeys(mmap(o.spec, k))) {
				switch {
				case !known(o.tenant, l):
					f.add(o, "lens_unknown", "/"+k+"/"+l)
				case k == "weights" && skeleton[l]["weight"] != nil:
					f.add(o, "lens_weight_fixed", "/weights/"+l)
				}
			}
		}
	}
	return nil
}

// governanceName says whether a name is an event or operation of governance, attribution or
// seats, which no bot reaches (GOV-6, GOV-29); with incidents, also of incident records, which
// no bot creates or changes (GOV-13).
func governanceName(n string, incidents bool) bool {
	for _, p := range []string{"governance.", "attribution.", "seat."} {
		if strings.HasPrefix(n, p) {
			return true
		}
	}
	return incidents && strings.HasPrefix(n, "incident.")
}

// charterFloors are the durations of a tenant charter that only rise from their default, and
// charterCeilings the counts that only fall from it (GOV-33). The schema bounds the counts and
// the audit sample; the durations are strings it cannot compare.
var charterFloors = []struct {
	block, field string
	def          time.Duration
}{
	{"promotion", "probation", govProbation}, {"promotion", "min_tenure", govMinTenure}, {"promotion", "swap_cooldown", govSwapCooldown},
	{"removal", "protect_new", 60 * 24 * time.Hour}, {"attribution", "window", defaultAttributionWindow},
	{"confirmation", "ttl", 7 * 24 * time.Hour}, {"identity", "unbound_ttl", defaultUnboundTTL}, {"memory", "seal_ttl", defaultSealTTL},
}

// isFarmCharter says whether a Governance manifest is the farm charter: the one of tenant farm,
// or one named farm without a tenant.
func isFarmCharter(tenant, name string) bool {
	return tenant == farmTenant || tenant == "" && name == farmTenant
}

// lintCharterBounds checks what the schema of a charter cannot (GOV-33): a seat id declared
// twice, the farm_manager block outside the farm charter (isFarmCharter), a duration below its
// default and the excluded change kinds without none. incident_stop.decide_within is not
// checked: a v0.3 core does not use incident_stop (MGR-55; apply warns, ignoredV03).
func lintCharterBounds(o lintObj, f *lintFindings) {
	seen := map[string]bool{}
	for i, sv := range mlist(mmap(o.spec, "commission"), "seats") {
		sm, _ := sv.(map[string]any)
		// A seat without an id is the schema's finding, not a second declaration.
		if sm["id"] == nil {
			continue
		}
		id := fmt.Sprint(sm["id"])
		if seen[id] {
			f.add(o, "charter_seat_twice", fmt.Sprintf("/commission/seats/%d/id", i))
		}
		seen[id] = true
	}
	if !isFarmCharter(o.tenant, o.name) && o.spec["farm_manager"] != nil {
		f.add(o, "charter_farm_manager", "/farm_manager")
	}
	for _, fl := range charterFloors {
		v := mstr(mmap(o.spec, fl.block), fl.field)
		if d, ok := parseDuration(v); v != "" && (!ok || d < fl.def) {
			f.add(o, "charter_direction", "/"+fl.block+"/"+fl.field)
		}
	}
	if p := mmap(o.spec, "promotion"); p["exclude_change_kinds"] != nil && !slices.Contains(strList(p["exclude_change_kinds"]), "none") {
		f.add(o, "charter_direction", "/promotion/exclude_change_kinds")
	}
}

// lintGovernance checks the personas of the charters (GOV-4, COM-19), the unawareness of the
// texts bots read (GOV-29) and that nothing of a bot refers to governance (GOV-6, GOV-13).
func (c *Core) lintGovernance(S *lintSet, f *lintFindings, personas personaLookup) error {
	u, err := c.unawarenessTerms()
	if err != nil {
		return err
	}
	for _, o := range S.each("Governance") {
		com := mmap(o.spec, "commission")
		for i, sv := range mlist(com, "seats") {
			sm, _ := sv.(map[string]any)
			if l := mstr(sm, "lens"); l != "" && mstr(mmap(mmap(mmap(com, "lenses"), l), "persona"), "ref") == "" {
				f.add(o, "lens_persona_missing", fmt.Sprintf("/commission/seats/%d/lens", i))
			}
		}
		lintCharterBounds(o, f)
		// From v0.3 the size of a persona counts with its addenda (GOV-45).
		refs := charterPersonas(o.spec)
		sizes := map[string]int{}
		for _, path := range slices.Sorted(mapKeys(refs)) {
			r := refs[path]
			if !r.pinned() {
				continue
			}
			text, ok := "", false
			if personas != nil {
				text, ok = personas(o.tenant, r.SHA256)
			}
			if !ok {
				// A charter of an earlier apply whose text the store lacks (pinned before the
				// core kept persona texts) runs without its persona until it is applied again
				// with the file; it does not block an apply of other objects.
				if o.i >= 0 {
					f.add(o, "persona_missing", path)
				}
				continue
			}
			position := "lens"
			if path == "/farm_manager/persona" {
				position = "farm_manager"
			} else if strings.HasPrefix(path, "/positions/") {
				position = strings.Split(path, "/")[2]
			}
			kind := position
			if strings.Contains(path, "/addenda/") {
				kind = "addendum"
			}
			for _, code := range personaFindingsV03(text, kind, u) {
				f.add(o, code, path)
			}
			key := path
			if strings.HasPrefix(path, "/positions/") {
				key = "/positions/" + position
			}
			if sizes[key] += len(text); sizes[key] > 4096 {
				f.add(o, "persona_size", path)
			}
		}
	}
	for _, o := range S.each("TaskType") {
		if u.linked(mstr(o.spec, "instruction"), false) {
			f.add(o, "unaware_text", "/instruction")
		}
		for j, e := range mstrs(o.spec, "events") {
			if governanceName(e, true) {
				f.add(o, "governance_ref", fmt.Sprintf("/events/%d", j))
			}
		}
	}
	for _, o := range S.each("ToolPlugin") {
		for j, tv := range mlist(o.spec, "tools") {
			if tm, ok := tv.(map[string]any); ok && u.linked(mstr(tm, "description"), false) {
				f.add(o, "unaware_text", fmt.Sprintf("/tools/%d/description", j))
			}
		}
	}
	for _, o := range S.each("Role") {
		for j, a := range mstrs(o.spec, "actions_may_request") {
			if governanceName(a, true) {
				f.add(o, "governance_ref", fmt.Sprintf("/actions_may_request/%d", j))
			}
		}
		for j, t := range mstrs(o.spec, "may_enqueue") {
			if governanceName(t, true) {
				f.add(o, "governance_ref", fmt.Sprintf("/may_enqueue/%d", j))
			}
		}
	}
	for _, kind := range []string{"Action", "MessageType", "TaskType"} {
		for _, o := range S.each(kind) {
			if governanceName(o.name, true) {
				f.add(o, "governance_ref", "/metadata/name")
			}
		}
	}
	for _, o := range S.each("Route") {
		if governanceName(mstr(mmap(o.spec, "on"), "event"), false) {
			f.add(o, "governance_ref", "/on/event")
		}
	}
	return nil
}

// groupDefault says whether a default_types entry also serves group chats: a bare type name or
// an entry without chats serves every chat (REP-11).
func groupDefault(dt any) bool {
	m, ok := dt.(map[string]any)
	if !ok {
		return true
	}
	chats := mstrs(m, "chats")
	return len(chats) == 0 || slices.ContainsFunc(chats, func(ch string) bool { return ch == "group" || strings.HasPrefix(ch, "chat:") })
}

// lintTwoApprovers checks the actions of risk high in a production tenant (APR-16): one that a
// group of the tenant grants action.request for, or that a role lists in actions_may_request,
// needs approvals: 2 (high_risk_one_approver); with approvals: 2 and no group of the tenant
// granting action.approve the apply only notes approvers_fewer_than_two in its log, since the
// response has no notes, and doctor counts the people.
func (c *Core) lintTwoApprovers(f *lintFindings, S *lintSet) {
	for _, t := range S.each("Tenant") {
		if k := mstr(t.spec, "kind"); k != "" && k != "production" {
			continue
		}
		grants := func(cp string) bool {
			for _, g := range S.each("Group") {
				if g.tenant == t.name && slices.Contains(mstrs(g.spec, "caps"), cp) {
					return true
				}
			}
			return false
		}
		for _, a := range S.each("Action") {
			if mstr(a.spec, "risk") != "high" {
				continue
			}
			n, ok := mnum(a.spec, "approvals")
			if !ok {
				n = 1
			}
			requested := grants("action.request:" + a.name)
			if n == 1 {
				// A role of the tenant, or one without a tenant, that lists the action.
				for _, r := range S.each("Role") {
					if (r.tenant == "" || r.tenant == t.name) && slices.Contains(mstrs(r.spec, "actions_may_request"), a.name) {
						requested = true
					}
				}
				if requested {
					f.add(a, "high_risk_one_approver", "/approvals")
				}
			} else if !grants("action.approve:" + a.name) {
				// The note does not depend on who may request the action (APR-16).
				c.log.Warn("apply: no group grants action.approve for a high risk action with two approvers", "code", "approvers_fewer_than_two",
					"tenant", t.name, "action", a.name)
			}
		}
	}
}

// managerLineShort are the fields of a tenant's manager line that do not cover what the seat may
// spend: the day holds three worst-case readings of one incident at the seat profile's prices
// (MGR-20), from v0.3 also the daily review and the daily ceiling of the analyses, and the month
// the takeover ceiling (MGR-49).
func managerLineShort(ts, farmManager, seatProfile map[string]any) []string {
	var out []string
	line := mmap(mmap(mmap(ts, "budgets"), "lines"), "manager")
	if d, hasDay := mnum(line, "day_eur"); seatProfile != nil && hasDay {
		in, _ := mnum(mmap(seatProfile, "price"), "input")
		outPrice, _ := mnum(mmap(seatProfile, "price"), "output")
		maxOut, _ := mnum(seatProfile, "max_output_tokens")
		need := seatWorstReadMicros(in, outPrice, int(maxOut))
		need += float64(money.FromEUR(charterReviewMaxEUR(farmManager)) + money.FromEUR(postmortemDayEUR(farmManager)))
		if d*1e6 < need {
			out = append(out, "/budgets/lines/manager/day_eur")
		}
	}
	tm := defaultTakeoverMonthEUR
	if v, ok := mnum(mmap(ts, "takeover"), "manager_month_eur"); ok {
		tm = v
	}
	if mm, ok := mnum(line, "month_eur"); ok && mm < tm {
		out = append(out, "/budgets/lines/manager/month_eur")
	}
	return out
}
