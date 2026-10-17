// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"slices"
	"testing"
)

const seatRoleYAML = `apiVersion: botaru/v1alpha1
kind: Role
metadata: {name: farm-manager}
spec:
  profile: farm-manager
  worker_kind: agent
  claims: [manager.incident.respond.v1]
  models: {base: manager-std}
`

// The lint keeps the built-in tools of the seat profiles to their list (MGR-10) and the
// operation targets and the forbidden list of proposals clear of MGR-16's targets.
func TestSeatProfileLint(t *testing.T) {
	c := lintCore(t)
	codes := lintCodes(t, c, seatRoleYAML)
	if slices.Contains(codes, "role_profile_forbidden") || slices.Contains(codes, "manager_self_change") {
		t.Fatalf("the skeleton registries fail the seat lint: %v", codes)
	}
	// A built-in tool of the registry outside the seat list, as a broken registry would give.
	saved := seatBuiltins["farm-manager"]
	seatBuiltins["farm-manager"] = slices.DeleteFunc(slices.Clone(saved), func(s string) bool { return s == "service/control" })
	codes = lintCodes(t, c, seatRoleYAML)
	seatBuiltins["farm-manager"] = saved
	if !slices.Contains(codes, "role_profile_forbidden") {
		t.Fatalf("codes %v", codes)
	}
	// An operation target among MGR-16's targets.
	savedTargets := selfChangeTargets
	selfChangeTargets = append(slices.Clone(savedTargets), "role")
	codes = lintCodes(t, c, seatRoleYAML)
	selfChangeTargets = savedTargets
	if !slices.Contains(codes, "manager_self_change") {
		t.Fatalf("codes %v", codes)
	}
}

// An incident catalog without store_low fails; a store_low with stops_manager passes at the
// v0.3 claim, which does not use the field and only warns of it (MGR-38, MGR-55,
// fx-mgr-store-low).
func TestStoreLowLint(t *testing.T) {
	c := lintCore(t)
	disk := `apiVersion: botaru/v1alpha1
kind: IncidentClass
metadata: {name: disk_filling}
spec:
  title: Disk filling
  severity: sev2
  detect: {alarm: alarm.disk.free.low.v1}
  subject: {kind: resource}
`
	if codes := lintCodes(t, c, disk); !slices.Contains(codes, "incident_store_low") {
		t.Fatalf("a catalog without store_low: %v", codes)
	}
	stop := `apiVersion: botaru/v1alpha1
kind: IncidentClass
metadata: {name: store_low}
spec:
  title: Low space for the database
  severity: sev1
  stops_manager: true
  detect: {alarm: alarm.store.free.low.v1}
  subject: {kind: farm}
`
	if codes := lintCodes(t, c, stop); slices.Contains(codes, "incident_store_low") {
		t.Fatalf("store_low with stops_manager: %v", codes)
	}
	if codes := lintCodes(t, c, disk+"---\n"+stop); slices.Contains(codes, "incident_store_low") {
		t.Fatalf("codes %v", codes)
	}
}
