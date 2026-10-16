// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/manifest"
	"github.com/HunterEwok/botaru/impl/internal/spec"
)

// privacyRequired are the fields of the privacy block a production tenant may not leave empty
// (SEC-8, docs/operations.md): subject_request_days has a default and export_sink is optional,
// and dpia_ref may be an explicit none. subprocessors is asked only of a tenant with
// providers_allowed, since without one P4 calls no provider for it, and channels only of a
// tenant with a channel: an empty list is then the true state, not an empty field. The same
// list is in ci/check_manifests.py.
var privacyRequired = []string{"purposes", "controller", "subprocessors", "recipients", "notice_ref", "notice_version", "channels", "dpia_ref", "ropa_ref"}

// noticeFields are the fields of the privacy block whose change needs a higher notice_version
// (SEC-13).
var noticeFields = []string{"controller", "purposes", "subprocessors", "recipients"}

// privacyGaps are the JSON pointers below /privacy of the empty fields of a block.
func privacyGaps(priv map[string]any, providers, channels bool) []string {
	var out []string
	for _, k := range privacyRequired {
		if k == "subprocessors" && !providers || k == "channels" && !channels {
			continue
		}
		if emptyValue(priv[k]) {
			out = append(out, "/"+k)
		}
	}
	if ctl := mmap(priv, "controller"); len(ctl) > 0 {
		for _, k := range []string{"name", "contact", "agreement_ref"} {
			if emptyValue(ctl[k]) {
				out = append(out, "/controller/"+k)
			}
		}
	}
	for i, s := range mlist(priv, "subprocessors") {
		sm, _ := s.(map[string]any)
		for _, k := range []string{"agreement_ref", "region", "retention"} {
			if emptyValue(sm[k]) {
				out = append(out, fmt.Sprintf("/subprocessors/%d/%s", i, k))
			}
		}
	}
	return out
}

func emptyValue(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return x == ""
	case []any:
		return len(x) == 0
	case map[string]any:
		return len(x) == 0
	}
	return false
}

// lintPrivacy checks the privacy block of every tenant (SEC-8, SEC-13, GW-14) and refuses a
// role with person memory while the personal assistant is deferred (MEM-2):
//   - a production tenant has no empty field of the block (privacy_incomplete);
//   - every provider of providers_allowed, in any tenant, is a subprocessor, so a fallback model
//     cannot add one without a decision (subprocessor_undeclared);
//   - a change of controller, purposes, subprocessors or recipients against the stored tenant
//     needs a higher notice_version (notice_version_not_raised);
//   - the subprocessor region of an allowed profile's provider is a row of the region table of
//     its provider binding (region_unsupported).
func (c *Core) lintPrivacy(S *lintSet, f *lintFindings, prior []manifest.Doc) {
	withChannel := map[string]bool{}
	for _, o := range S.each("Channel") {
		withChannel[o.tenant] = true
	}
	stored := map[string]map[string]any{}
	for _, d := range prior {
		if d.Kind == "Tenant" {
			s, _ := d.Value["spec"].(map[string]any)
			stored[d.Name] = mmap(s, "privacy")
		}
	}
	regions := map[string]map[string]bool{}
	for _, o := range S.each("Tenant") {
		ts := o.spec
		priv := mmap(ts, "privacy")
		subs := map[string]map[string]any{}
		for _, s := range mlist(priv, "subprocessors") {
			if sm, ok := s.(map[string]any); ok {
				subs[mstr(sm, "provider")] = sm
			}
		}
		providers := mstrs(ts, "providers_allowed")
		if k := mstr(ts, "kind"); k == "" || k == "production" {
			for _, p := range privacyGaps(priv, len(providers) > 0, withChannel[o.name]) {
				f.add(o, "privacy_incomplete", "/privacy"+p)
			}
		}
		for j, p := range providers {
			if subs[p] == nil {
				f.add(o, "subprocessor_undeclared", fmt.Sprintf("/providers_allowed/%d", j))
			}
		}
		if old, ok := stored[o.name]; ok && o.i >= 0 {
			if changed := privacyChangedFields(old, priv, noticeFields); len(changed) > 0 {
				was, _ := mnum(old, "notice_version")
				now, _ := mnum(priv, "notice_version")
				if now <= was {
					f.addDetail(o, "notice_version_not_raised", "/privacy/notice_version", strings.Join(changed, ","))
				}
			}
		}
		for j, mp := range mstrs(ts, "models_allowed") {
			prof := S.get("ModelProfile", mp, o.name)
			if prof == nil {
				continue
			}
			p := mstr(prof, "provider")
			sub := subs[p]
			region := mstr(sub, "region")
			if region == "" {
				continue
			}
			table, ok := regions[p]
			if !ok {
				table = c.bindingRegions(p)
				regions[p] = table
			}
			if table != nil && !table[region] {
				f.addDetail(o, "region_unsupported", fmt.Sprintf("/models_allowed/%d", j), region)
			}
		}
	}
	// From v0.2, while the personal assistant is deferred, no role holds person memory
	// (MEM-2, DSP-10, D-114).
	if !spec.VersionLater("v0.2", spec.Effective()) {
		for _, o := range S.each("Role") {
			if mstr(o.spec, "memory") == "person" {
				f.add(o, "person_memory_deferred", "/memory")
			}
		}
	}
}

// bindingRegions are the regions of the "Region" table of the provider binding note
// spec/bindings/model-providers/<provider>.md (GW-14), as ci/check_manifests.py reads it. A
// provider without a note or without the section, such as the test providers of the examples
// and the mock provider, has no table: nil. The provider name is checked against
// reProviderName before it becomes a path.
func (c *Core) bindingRegions(provider string) map[string]bool {
	if !reProviderName.MatchString(provider) || provider == "README" || provider == "mock" {
		return nil
	}
	raw, err := os.ReadFile(filepath.Join(c.spec.Root, "spec", "bindings", "model-providers", provider+".md"))
	if err != nil {
		return nil
	}
	var out map[string]bool
	inside := false
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "## ") {
			inside = strings.TrimSpace(line) == "## Region"
			if inside && out == nil {
				out = map[string]bool{}
			}
			continue
		}
		if inside && strings.HasPrefix(line, "| `") {
			out[strings.Split(line, "`")[1]] = true
		}
	}
	return out
}

// privacyChangedFields are the fields of the list whose value differs between two privacy
// blocks, in the order of the list.
func privacyChangedFields(old, cur map[string]any, fields []string) []string {
	var out []string
	for _, k := range fields {
		if !reflect.DeepEqual(normJSON(old[k]), normJSON(cur[k])) {
			out = append(out, k)
		}
	}
	return out
}

// normJSON gives a value the form it has after a JSON round trip, so a stored block and one of
// the set compare by content.
func normJSON(v any) any {
	raw, err := json.Marshal(v)
	if err != nil {
		return v
	}
	var out any
	_ = json.Unmarshal(raw, &out)
	return out
}

// privacyChange is a tenant whose privacy block an apply changes.
type privacyChange struct {
	tenant        string
	fields        []string
	noticeVersion any
}

// privacyChangeOf compares the stored privacy block of a tenant with the one an apply writes and
// returns the changed fields of the block, in its schema order (SEC-8).
func privacyChangeOf(ctx context.Context, tx pgx.Tx, name string, doc map[string]any) (*privacyChange, error) {
	var raw []byte
	if err := tx.QueryRow(ctx, `SELECT doc FROM manifests WHERE kind = 'Tenant' AND name = $1`, name).Scan(&raw); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	var old map[string]any
	if err := json.Unmarshal(raw, &old); err != nil {
		return nil, err
	}
	before := mmap(mmap(old, "spec"), "privacy")
	after := mmap(mmap(doc, "spec"), "privacy")
	keys := map[string]bool{}
	for k := range before {
		keys[k] = true
	}
	for k := range after {
		keys[k] = true
	}
	order := append(slices.Clone(privacyRequired), "subject_request_days", "export_sink")
	for k := range keys {
		if !slices.Contains(order, k) {
			order = append(order, k)
		}
	}
	var fields []string
	for _, k := range order {
		if keys[k] && !reflect.DeepEqual(normJSON(before[k]), normJSON(after[k])) {
			fields = append(fields, k)
		}
	}
	if len(fields) == 0 {
		return nil, nil
	}
	return &privacyChange{name, fields, after["notice_version"]}, nil
}

// privacyNotices gives each tenant whose privacy block changed the event
// tenant.privacy_changed and a code notice to its admins (admin:people) in their private chats,
// in the apply transaction (SEC-8). Both carry only the names of the changed fields and the
// notice version.
func (c *Core) privacyNotices(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, changes []privacyChange) error {
	for _, ch := range changes {
		data := map[string]any{"tenant": ch.tenant, "fields": ch.fields}
		if ch.noticeVersion != nil {
			data["notice_version"] = ch.noticeVersion
		}
		if err := c.appendEvent(ctx, tx, now, ch.tenant, Event{Type: "tenant.privacy_changed", Subject: ch.tenant, Data: data}); err != nil {
			return err
		}
		tv := cat.Tenants[ch.tenant]
		if tv == nil {
			continue
		}
		chats, err := c.adminChats(ctx, tx, now, tv, "", "admin:people")
		if err != nil {
			return err
		}
		version := ""
		if ch.noticeVersion != nil {
			version = fmt.Sprint(ch.noticeVersion)
		}
		for _, a := range chats {
			if _, err := c.planDelivery(ctx, tx, now, plannedDelivery{
				Tenant: tv.Name, Kind: "notice", Addr: "admins", TargetKind: "chat",
				TargetID: Chat{Kind: "direct", ExternalID: a.ExternalID}.ID(), ExecKind: "channel", ExecName: a.Channel,
				Recipient: a.Principal, DataOrigin: "code",
				Body: map[string]any{"form": "text_code", "text_key": "notice.privacy_changed",
					"params": map[string]any{"tenant": tv.Name, "fields": strings.Join(ch.fields, ", "), "notice_version": version}},
			}); err != nil {
				return err
			}
		}
	}
	return nil
}
