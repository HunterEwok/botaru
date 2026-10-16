// SPDX-License-Identifier: AGPL-3.0-only

package zabbix

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"time"
)

// forbiddenActions are the role actions no adapter account may hold: acknowledging, closing,
// suppressing problems, changing their severity and editing maintenance windows (MON-4).
var forbiddenActions = []string{"acknowledge_problems", "close_problems", "suppress_problems", "change_severity", "edit_maintenance"}

// forbiddenUI are the frontend sections that change notification actions and maintenance
// windows; only a role of type Admin or higher has them at all.
var forbiddenUI = []string{"alerts.actions", "data_collection.maintenance"}

// maxVisible is the most hosts a report names; an account that sees more is reported as
// truncated, which is wider than any tenant needs.
const maxVisible = 1000

// NodeHash is how a report names a host: the SHA-256 of its Zabbix host name, so no Zabbix
// string reaches the core, which compares it with the hashes of the catalog nodes (MON-4).
func NodeHash(node string) string {
	sum := sha256.Sum256([]byte(node))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// AccountReport reads the adapter's own user and role through the API (user.checkAuthentication, role.get)
// and what the user sees (host.get, script.getscriptsbyhosts), as the data of
// ext.monitor.account.v1 (MON-4, OBS-11): the role rights that are wider than the rule allows
// (findings: super_admin, a forbidden action or frontend section by its rule name), the hashes
// of every host the user sees (visible) and of every such host with a script the user may run
// (scripts). The core decides what of it is wider: a node of another tenant or of the farm, a
// node outside the catalog, a script on a farm node. Host, script and role names stay here.
func (a *Adapter) AccountReport(ctx context.Context) (map[string]any, error) {
	// The adapter's own user: the one its API token belongs to, by user.checkAuthentication
	// (Zabbix 7.0), never the first row of a user list.
	var self struct {
		UserID string `json:"userid"`
		RoleID string `json:"roleid"`
	}
	if err := a.callAnon(ctx, "user.checkAuthentication", map[string]any{"token": a.cfg.APIToken}, &self); err != nil {
		return nil, err
	}
	if self.UserID != "" && self.RoleID == "" {
		var users []struct {
			UserID string `json:"userid"`
			RoleID string `json:"roleid"`
		}
		if err := a.call(ctx, "user.get", map[string]any{"output": []string{"userid", "roleid"}, "userids": []string{self.UserID}}, &users); err != nil {
			return nil, err
		}
		for _, u := range users {
			if u.UserID == self.UserID {
				self.RoleID = u.RoleID
			}
		}
	}
	findings := []string{}
	if self.UserID == "" || self.RoleID == "" {
		findings = append(findings, "role_unreadable")
	} else {
		var roles []struct {
			Type  string `json:"type"`
			Rules struct {
				UI                   []rule `json:"ui"`
				UIDefaultAccess      string `json:"ui.default_access"`
				Actions              []rule `json:"actions"`
				ActionsDefaultAccess string `json:"actions.default_access"`
			} `json:"rules"`
		}
		if err := a.call(ctx, "role.get", map[string]any{"output": []string{"roleid", "type"}, "roleids": []string{self.RoleID}, "selectRules": "extend"}, &roles); err != nil {
			return nil, err
		}
		if len(roles) == 0 {
			findings = append(findings, "role_unreadable")
		} else {
			r := roles[0]
			if r.Type == "3" {
				findings = append(findings, "super_admin")
			}
			for _, name := range forbiddenActions {
				if allowed(r.Rules.Actions, r.Rules.ActionsDefaultAccess, name) {
					findings = append(findings, name)
				}
			}
			// A role of type User has no frontend section of the configuration at all.
			if r.Type == "2" || r.Type == "3" {
				for _, name := range forbiddenUI {
					if allowed(r.Rules.UI, r.Rules.UIDefaultAccess, name) {
						findings = append(findings, name)
					}
				}
			}
		}
	}
	var hosts []struct {
		HostID string `json:"hostid"`
		Host   string `json:"host"`
	}
	if err := a.call(ctx, "host.get", map[string]any{"output": []string{"hostid", "host"}, "sortfield": "hostid", "limit": maxVisible + 1}, &hosts); err != nil {
		return nil, err
	}
	truncated := len(hosts) > maxVisible
	if truncated {
		hosts = hosts[:maxVisible]
	}
	visible := make([]string, 0, len(hosts))
	byID := map[string]string{}
	req := make([]map[string]string, 0, len(hosts))
	for _, h := range hosts {
		visible = append(visible, NodeHash(h.Host))
		byID[h.HostID] = h.Host
		req = append(req, map[string]string{"hostid": h.HostID})
	}
	scripts := []string{}
	if len(req) > 0 {
		var res map[string][]struct {
			ScriptID string `json:"scriptid"`
		}
		if err := a.call(ctx, "script.getscriptsbyhosts", req, &res); err != nil {
			return nil, err
		}
		for id, list := range res {
			if node, ok := byID[id]; ok && len(list) > 0 {
				scripts = append(scripts, NodeHash(node))
			}
		}
	}
	sort.Strings(visible)
	sort.Strings(scripts)
	return map[string]any{"findings": findings, "visible": visible, "scripts": scripts, "truncated": truncated}, nil
}

// SyncAccount sends the account report to the core (MON-4, OBS-11). It runs with the script
// lists, every 10 minutes by default; the core keeps the newest report and its time.
func (a *Adapter) SyncAccount(ctx context.Context) error {
	data, err := a.AccountReport(ctx)
	if err != nil {
		return err
	}
	_, err = a.signal(ctx, "ext.monitor.account.v1", "account:"+time.Now().UTC().Format("20060102T150405"), data)
	return err
}

type rule struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

// allowed says whether a role rule is on: its own entry, or the default access of its kind.
func allowed(rules []rule, def, name string) bool {
	for _, r := range rules {
		if r.Name == name {
			return r.Status == "1"
		}
	}
	return def == "1"
}
