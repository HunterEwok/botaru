// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"testing"

	"github.com/HunterEwok/botaru/impl/internal/manifest"
)

func TestPrivilegedGroup(t *testing.T) {
	cat := newCatalog()
	cat.TaskTypes["greet"] = manifest.TaskType{Tier: 0}
	cat.TaskTypes["read"] = manifest.TaskType{Tier: 1}
	tv := &TenantView{Groups: map[string]manifest.Group{
		"hello":    {Caps: []string{"task:greet", "msg.send"}, Data: []string{"summary"}},
		"reader":   {Caps: []string{"task:read"}},
		"logs":     {Caps: []string{"task:greet"}, Data: []string{"raw_logs"}},
		"admin":    {Caps: []string{"admin:people"}},
		"unknown":  {Caps: []string{"task:missing"}},
		"heir":     {Caps: []string{"team.view"}, Inherits: []string{"admin"}},
		"loop":     {Inherits: []string{"loop2"}},
		"loop2":    {Inherits: []string{"loop"}},
		"inherits": {Inherits: []string{"hello"}},
		"viewer":   {Caps: []string{"team.view:hello", "task.read:greet"}},
		"wildcard": {Caps: []string{"task:greet.*"}},
	}}
	for name, want := range map[string]bool{
		"hello": false, "reader": true, "logs": true, "admin": true, "unknown": true,
		"heir": true, "loop": false, "inherits": false, "absent": true, "viewer": false, "wildcard": true,
	} {
		if got := privilegedGroup(tv, cat, name, map[string]bool{}); got != want {
			t.Errorf("%s: privileged %v, want %v", name, got, want)
		}
	}
}

func TestCapCovers(t *testing.T) {
	for _, c := range []struct {
		have, want string
		ok         bool
	}{
		{"task:report.*", "task:report.daily.v1", true},
		{"task:report.*", "task:reports.daily.v1", false},
		{"task:report.daily.v1", "task:report.daily.v1", true},
		{"action.*", "action.restart", false},
		{"task:*", "task:x", false},
	} {
		if got := capCovers(c.have, c.want); got != c.ok {
			t.Errorf("%s covers %s: %v, want %v", c.have, c.want, got, c.ok)
		}
	}
}
