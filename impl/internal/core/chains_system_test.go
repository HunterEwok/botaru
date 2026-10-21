// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"slices"
	"testing"

	"github.com/HunterEwok/botaru/impl/internal/manifest"
)

// A task of system, such as a scheduled sensor check, may enqueue the children its group
// system-ops lists, as enqueue checks them, and no others; system has no memberships (PRM-10,
// PRM-6, flow 05).
func TestSystemChildTypes(t *testing.T) {
	t.Parallel()
	cat := &Catalog{
		Roles:     map[string]manifest.Role{"data-job": {MayEnqueue: []string{"report.sensor.v1", "report.other.v1"}}},
		TaskTypes: map[string]manifest.TaskType{"report.sensor.v1": {}, "report.other.v1": {}},
		Tenants: map[string]*TenantView{"demo": {Groups: map[string]manifest.Group{
			"system-ops": {Caps: []string{"task:data.sensor_check.v1", "task:report.sensor.v1"}},
			"staff":      {Caps: []string{"task:report.other.v1"}},
		}}},
	}
	g := &toolsGrant{Tenant: "demo", Role: "data-job", Initiator: "system"}
	if got := mayEnqueueTypes(cat, g); !slices.Equal(got, []string{"report.sensor.v1"}) {
		t.Fatalf("system may enqueue %v, want [report.sensor.v1]", got)
	}
	// A person keeps the groups of their memberships.
	g = &toolsGrant{Tenant: "demo", Role: "data-job", Initiator: "p_bob", InitiatorGroups: []string{"staff"}}
	if got := mayEnqueueTypes(cat, g); !slices.Equal(got, []string{"report.other.v1"}) {
		t.Fatalf("a person may enqueue %v, want [report.other.v1]", got)
	}
}

// A tool call hands the toolhost only the task's attachments that its arguments name, whole
// or as a list item; another string, even a file_ref of another task, is not a file (TEN-7).
func TestNamedFiles(t *testing.T) {
	t.Parallel()
	attached := []string{"file:A", "file:B", "file:C"}
	got := namedFiles(attached, map[string]any{"file": "file:B", "more": []any{"file:A", "file:Z", 3}, "note": "file:B text", "n": 1})
	if !slices.Equal(got, []string{"file:A", "file:B"}) {
		t.Fatalf("named %v", got)
	}
	if got := namedFiles(nil, map[string]any{"file": "file:A"}); got != nil {
		t.Fatalf("a task without attachments named %v", got)
	}
}
