// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"path/filepath"
	"slices"

	"github.com/HunterEwok/botaru/impl/internal/spec"
)

// seatBuiltins are the built-in tools a seat profile may have (MGR-10): the control operations
// through control_ops, reading the incident and code requests, and the service and script
// tools within MGR-9; the reading role has only file_read. No other built-in tool of
// spec/model/builtin-tools.yaml may name a seat profile: none gives a shell, another bot's
// context, credentials, a manifest edit, decide on a card, a direct action call or the
// release of people's holds.
var seatBuiltins = map[string][]string{
	"farm-manager": {"control_ops", "inspect_request", "incident_close", "incident_view", "service/control", "monitor/script_run",
		"grant_decide", "escalations_take", "grants_pending", "takeover_request", "link_hint", "attention_notice"},
	"farm-manager-reader": {"file_read"},
}

// selfChangeKinds are the manifest kinds the seat holder never changes, by operations or by
// proposals (MGR-16): the seat, the incident catalog with its involvement rules, the charter
// with the limits, the safeguard and the protected set of the seat, the commission policy and
// the consent groups. The seat roles are proposed only with kind none (Role is none_only), and
// the budget lines are a never field of Tenant.
var selfChangeKinds = []string{"Seat", "IncidentClass", "Governance", "Commission", "Group"}

// selfChangeTargets are the target kinds no control operation may name (MGR-16).
var selfChangeTargets = []string{"seat", "seat_role", "incident_class", "involvement_rule", "control_ops", "control_limits", "breaker",
	"budget_line", "group", "consent_group", "protected_set", "commission", "governance", "holder", "auto_stop"}

// lintSeatProfiles checks, for a set with a role of a seat profile, the registries the seat
// lives by: the built-in tools of the seat profiles (MGR-10), the operation targets and the
// forbidden list of proposals (MGR-16, PRP-3).
func (c *Core) lintSeatProfiles(S *lintSet, f *lintFindings) error {
	var seat []lintObj
	for _, o := range S.each("Role") {
		if _, ok := seatBuiltins[mstr(o.spec, "profile")]; ok && o.i >= 0 {
			seat = append(seat, o)
		}
	}
	if len(seat) == 0 {
		return nil
	}
	tools, err := c.spec.Registry("spec/model/builtin-tools.yaml")
	if err != nil {
		return err
	}
	bad := map[string]bool{}
	for _, e := range tools.Entries {
		for _, p := range strList(e["profiles"]) {
			if allowed, ok := seatBuiltins[p]; ok && !slices.Contains(allowed, mstr(e, "id")) {
				bad[p] = true
			}
		}
	}
	self, err := c.selfChangeProblems()
	if err != nil {
		return err
	}
	for _, o := range seat {
		if bad[mstr(o.spec, "profile")] {
			f.add(o, "role_profile_forbidden", "/profile")
		}
		if self {
			f.add(o, "manager_self_change", "/profile")
		}
	}
	return nil
}

// selfChangeProblems says whether an operation target kind is a target of MGR-16, or the
// forbidden list of proposals misses one (spec/model/proposable-fields.yaml).
func (c *Core) selfChangeProblems() (bool, error) {
	raw, err := c.spec.Raw("urn:botaru:schema:p2-control-apply-request:1")
	if err != nil {
		return false, err
	}
	kinds := mmap(mmap(mmap(raw, "properties"), "target"), "properties")
	for _, k := range strList(mmap(kinds, "kind")["enum"]) {
		if slices.Contains(selfChangeTargets, k) {
			return true, nil
		}
	}
	v, err := spec.ReadYAMLFile(filepath.Join(c.spec.Root, "spec/model/proposable-fields.yaml"))
	if err != nil {
		return false, err
	}
	pf, _ := v.(map[string]any)
	covered := map[string]bool{}
	for _, k := range mstrs(pf, "none_only") {
		covered[k] = true
	}
	budgetLines := false
	for _, n := range mlist(pf, "never") {
		nm := asMap(n)
		for _, k := range mstrs(nm, "kinds") {
			covered[k] = true
		}
		if mstr(nm, "object") == "Tenant" && slices.Contains(mstrs(nm, "fields"), "spec.budgets.lines") {
			budgetLines = true
		}
	}
	for _, e := range mlist(pf, "entries") {
		if slices.Contains(selfChangeKinds, mstr(asMap(e), "object")) || mstr(asMap(e), "object") == "Role" {
			return true, nil
		}
	}
	for _, k := range append(slices.Clone(selfChangeKinds), "Role") {
		if !covered[k] {
			return true, nil
		}
	}
	return !budgetLines, nil
}
