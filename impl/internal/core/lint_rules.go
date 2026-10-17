// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Lint checks of plugins, driver configurations, dispatch rules, time zones and schemas.

// driverConfigForbidden reads the envelope fields that a driver configuration never holds
// (the forbidden list of the driver-config meta-schema, MAN-4).
func (c *Core) driverConfigForbidden() (map[string]bool, error) {
	out := map[string]bool{}
	raw, err := c.spec.Raw("urn:botaru:schema:driver-config:1")
	if err != nil {
		return out, nil
	}
	defs, _ := raw["$defs"].(map[string]any)
	fb, _ := defs["forbidden"].(map[string]any)
	for _, v := range mlist(fb, "enum") {
		if s, ok := v.(string); ok {
			out[s] = true
		}
	}
	return out, nil
}

// claimsProfile says whether this core claims a conformance profile (core, and monitor when
// the settings turn it on); the checks of the manager seat (MGR-20) and of monitoring as a
// recipient (MON-1, D-88) run only under their profiles.
func (c *Core) claimsProfile(name string) bool { return slices.Contains(c.profiles(), name) }

// lintEgress checks a plugin's network list (INJ-6): every address is a machine or a resource
// of a tenant catalog of the set; the address monitor-api of the monitor plugin comes from
// the deployment description.
func (c *Core) lintEgress(f *lintFindings, o lintObj, catalogs map[string]map[string][]string) {
	known := map[string]bool{}
	for _, lists := range catalogs {
		for _, h := range lists["hosts"] {
			known[h] = true
		}
	}
	for j, a := range mstrs(mmap(o.spec, "network"), "egress") {
		if a == "monitor-api" && o.name == "monitor" {
			continue
		}
		if !known[a] {
			f.add(o, "egress_unknown", fmt.Sprintf("/network/egress/%d", j))
		}
	}
}

// lintDispatch checks the dispatcher rules of a tenant beyond the schema, which already closes
// the match to the type name, the type prefix and the source kind (DSP-3, FEN-3): the rule's
// bot group exists and its selector meets the roles that take the type, and the attempt
// settings stay within the type's bounds (an attempt period of at most twice the type's and not
// beyond its deadline, fewer attempts than the type has and at least one).
func (c *Core) lintDispatch(f *lintFindings, o lintObj, types map[string]map[string]any, roles map[string]lintObj, S *lintSet) {
	for j, rv := range mlist(mmap(o.spec, "dispatch"), "rules") {
		rule, _ := rv.(map[string]any)
		path := fmt.Sprintf("/dispatch/rules/%d", j)
		match := mmap(rule, "match")
		var matched []string
		for _, tn := range slices.Sorted(mapKeys(types)) {
			if tn == mstr(match, "type") || (mstr(match, "type_prefix") != "" && strings.HasPrefix(tn, mstr(match, "type_prefix"))) {
				matched = append(matched, tn)
			}
		}
		takers := map[string]bool{}
		for rn, ro := range roles {
			for _, tn := range matched {
				if slices.Contains(mstrs(ro.spec, "claims"), tn) {
					takers[rn] = true
				}
			}
		}
		if g := mstr(rule, "group"); g != "" {
			bg := S.get("BotGroup", g, o.name)
			if bg == nil {
				f.add(o, "ref_missing", path+"/group")
			} else if sel := mstrs(mmap(bg, "select"), "roles"); len(sel) > 0 && len(matched) > 0 {
				meets := false
				for _, rn := range sel {
					if takers[rn] {
						meets = true
					}
				}
				if !meets {
					f.add(o, "dispatch_group_disjoint", path+"/group")
				}
			}
		}
		for _, tn := range matched {
			tt := types[tn]
			if a := durSeconds(mmap(rule, "timeouts")["attempt"]); a >= 0 {
				ta := durSeconds(mmap(tt, "timeouts")["attempt"])
				dl := durSeconds(mmap(tt, "timeouts")["deadline"])
				if (ta >= 0 && a > 2*ta) || (dl >= 0 && a > dl) {
					f.add(o, "dispatch_attempt_bounds", path+"/timeouts/attempt")
				}
			}
			if n, ok := mnum(mmap(rule, "retry"), "max_attempts"); ok {
				tmax, has := mnum(mmap(tt, "retry"), "max_attempts")
				if !has {
					tmax = 2 // the farm default (README, Defaults)
				}
				if n < 1 || n >= tmax {
					f.add(o, "dispatch_attempt_bounds", path+"/retry/max_attempts")
				}
			}
		}
	}
}

// knownZone says whether an IANA time zone name exists in the zone database (TIM-3).
func knownZone(name string) bool {
	if name == "Local" {
		return false
	}
	_, err := time.LoadLocation(name)
	return err == nil
}

// readByCode says whether a schema has a field marked x-botaru-read-by: code.
func readByCode(v any) bool {
	switch x := v.(type) {
	case map[string]any:
		if x["x-botaru-read-by"] == "code" {
			return true
		}
		for _, e := range x {
			if readByCode(e) {
				return true
			}
		}
	case []any:
		for _, e := range x {
			if readByCode(e) {
				return true
			}
		}
	}
	return false
}

var reHumanValue = regexp.MustCompile(`^human(:|$)`)

// namesHuman says whether a schema fixes a value naming a human principal or the kind human
// in a const or an enum (DLV-14).
func namesHuman(v any) bool {
	switch x := v.(type) {
	case map[string]any:
		if s, ok := x["const"].(string); ok && reHumanValue.MatchString(s) {
			return true
		}
		if e, ok := x["enum"].([]any); ok {
			for _, s := range e {
				if str, ok := s.(string); ok && reHumanValue.MatchString(str) {
					return true
				}
			}
		}
		for _, e := range x {
			if namesHuman(e) {
				return true
			}
		}
	case []any:
		for _, e := range x {
			if namesHuman(e) {
				return true
			}
		}
	}
	return false
}
