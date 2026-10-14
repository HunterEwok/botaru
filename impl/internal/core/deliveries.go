// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/ids"
)

// plannedDelivery is a delivery the core creates in the transaction of the transition that
// produces it (DLV-2). The address comes only from the closed list (DLV-6).
type plannedDelivery struct {
	Tenant        string
	Task          string
	Kind          string
	Addr          string // origin, group:<g>, chat:<binding>, sink:<sink>, approval_initiator, monitor
	TargetKind    string // chat, subject, sink, monitor
	TargetID      string
	ExecKind      string // channel, sink, monitor
	ExecName      string
	Recipient     string
	Body          map[string]any
	DataMark      string
	DataOrigin    string // model or code (DLV-13)
	NoticeVersion string
}

func (c *Core) planDelivery(ctx context.Context, tx pgx.Tx, now time.Time, d plannedDelivery) (string, error) {
	id := ids.New(ids.Delivery)
	body, err := json.Marshal(d.Body)
	if err != nil {
		return "", err
	}
	_, err = tx.Exec(ctx, `INSERT INTO deliveries (id, tenant, task, kind, addr, target_kind, target_id, exec_kind, exec_name, recipient, state, body, data_mark, dataorigin, notice_version, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, 'planned', $11, $12, $13, $14, $15, $15)`,
		id, d.Tenant, nullable(d.Task), d.Kind, d.Addr, d.TargetKind, d.TargetID, d.ExecKind, d.ExecName, nullable(d.Recipient),
		body, nullable(d.DataMark), d.DataOrigin, nullable(d.NoticeVersion), now)
	if err != nil {
		return "", err
	}
	if err := c.appendEvent(ctx, tx, now, d.Tenant, Event{Type: "delivery.planned", Subject: id, Data: map[string]any{"delivery": id, "kind": d.Kind, "task": d.Task}}); err != nil {
		return "", err
	}
	return id, notifyWork(ctx, tx)
}

// firstContact plans ai_notice and privacy_notice on a principal's first contact with a channel
// (DLV-12). Model text for this recipient waits until ai_notice is delivered (DLV-13).
func (c *Core) firstContact(ctx context.Context, tx pgx.Tx, now time.Time, tv *TenantView, channel, principal string, chat Chat) error {
	tag, err := tx.Exec(ctx, `INSERT INTO contacts (tenant, channel, principal, created_at) VALUES ($1, $2, $3, $4) ON CONFLICT DO NOTHING`,
		tv.Name, channel, principal, now)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		// An ai_notice that never arrived is planned again; model text waits for it meanwhile.
		var dead bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM contacts ct JOIN deliveries d ON d.id = ct.ai_notice_delivery
			WHERE ct.tenant = $1 AND ct.channel = $2 AND ct.principal = $3 AND ct.ai_notice_at IS NULL AND d.state = 'dead')`,
			tv.Name, channel, principal).Scan(&dead); err != nil {
			return err
		}
		if dead {
			if err := c.planAINotice(ctx, tx, now, tv, channel, principal, chat); err != nil {
				return err
			}
		}
		return c.privacyVersionChange(ctx, tx, now, tv, channel, principal, chat)
	}
	if tv.Channels[channel].AIDisclosure == "native" {
		// The messenger itself shows that the account is a bot (DLV-12).
		return c.planPrivacyNotice(ctx, tx, now, tv, channel, principal, chat)
	}
	if err := c.planAINotice(ctx, tx, now, tv, channel, principal, chat); err != nil {
		return err
	}
	return c.planPrivacyNotice(ctx, tx, now, tv, channel, principal, chat)
}

func (c *Core) planAINotice(ctx context.Context, tx pgx.Tx, now time.Time, tv *TenantView, channel, principal string, chat Chat) error {
	aiID, err := c.planDelivery(ctx, tx, now, plannedDelivery{
		Tenant: tv.Name, Kind: "ai_notice", Addr: "origin", TargetKind: "chat", TargetID: chat.ID(),
		ExecKind: "channel", ExecName: channel, Recipient: principal, DataOrigin: "code",
		Body: map[string]any{"form": "text_code", "text_key": "notice.ai", "params": c.aiNoticeParams(tv)},
	})
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE contacts SET ai_notice_delivery = $4 WHERE tenant = $1 AND channel = $2 AND principal = $3`,
		tv.Name, channel, principal, aiID)
	return err
}

func (c *Core) planPrivacyNotice(ctx context.Context, tx pgx.Tx, now time.Time, tv *TenantView, channel, principal string, chat Chat) error {
	version := tv.Spec.Privacy.NoticeVersionString()
	params, err := c.privacyNoticeParams(tv)
	if err != nil {
		return err
	}
	body := map[string]any{"form": "text_code", "text_key": "notice.privacy", "params": params}
	if _, err := c.planDelivery(ctx, tx, now, plannedDelivery{
		Tenant: tv.Name, Kind: "privacy_notice", Addr: "origin", TargetKind: "chat", TargetID: chat.ID(),
		ExecKind: "channel", ExecName: channel, Recipient: principal, DataOrigin: "code", Body: body, NoticeVersion: version,
	}); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE contacts SET privacy_version = $4 WHERE tenant = $1 AND channel = $2 AND principal = $3`,
		tv.Name, channel, principal, version)
	return err
}

// operatorFallback names the operator in notice.ai when the deployment sets no operator;
// the core warns about it at start.
const operatorFallback = "the operator of this farm"

// noValue stands for a value of a notice that the tenant does not set, so a person never sees
// a raw placeholder.
const noValue = "-"

// aiNoticeParams are the values of notice.ai: the tenant and the operator of the deployment.
func (c *Core) aiNoticeParams(tv *TenantView) map[string]any {
	op := c.cfg.Operator
	if op == "" {
		op = operatorFallback
	}
	return map[string]any{"tenant": tv.Name, "operator": cut500(op)}
}

// privacyNoticeParams are the values of notice.privacy, filled by code from the tenant's
// privacy block (SEC-8, SEC-13) and the default periods of spec/model/retention-classes.yaml;
// lists are joined with commas. Every value is set: the controller defaults to the tenant
// name, the contact to the notice link, and anything else the tenant leaves out to "-"
// (noticeGaps lists these for the warning at start and at apply).
func (c *Core) privacyNoticeParams(tv *TenantView) (map[string]any, error) {
	pr := tv.Spec.Privacy
	params := map[string]any{}
	set := func(k string, vs ...string) {
		for _, v := range vs {
			if v != "" {
				params[k] = cut500(v)
				return
			}
		}
		params[k] = noValue
	}
	set("notice_version", pr.NoticeVersionString())
	set("notice_ref", pr.NoticeRef)
	set("controller", pr.Controller.Name, tv.Name)
	set("contact", pr.Controller.Contact, pr.NoticeRef)
	var purposes, subs []string
	for _, p := range pr.Purposes {
		purposes = append(purposes, p.Purpose)
	}
	for _, s := range pr.Subprocessors {
		if s.Region != "" {
			subs = append(subs, s.Provider+" ("+s.Region+")")
		} else {
			subs = append(subs, s.Provider)
		}
	}
	set("purposes", strings.Join(purposes, ", "))
	set("recipients", strings.Join(pr.Recipients, ", "))
	set("subprocessors", strings.Join(subs, ", "))
	reg, err := c.spec.Registry("spec/model/retention-classes.yaml")
	if err != nil {
		return nil, err
	}
	var periods []string
	for _, class := range []string{"content", "metadata"} {
		e, _ := reg.Entry(class)
		def, _ := e["default"].(map[string]any)
		if days, ok := def["days"].(int); ok {
			periods = append(periods, fmt.Sprintf("%s %d d", class, days))
		}
	}
	set("retention", strings.Join(periods, ", "))
	return params, nil
}

// noticeGaps lists the notice values that fall back to a default for a tenant.
func noticeGaps(tv *TenantView) []string {
	pr := tv.Spec.Privacy
	var gaps []string
	add := func(name string, empty bool) {
		if empty {
			gaps = append(gaps, name)
		}
	}
	add("privacy.controller.name", pr.Controller.Name == "")
	add("privacy.controller.contact", pr.Controller.Contact == "")
	add("privacy.notice_ref", pr.NoticeRef == "")
	add("privacy.notice_version", pr.NoticeVersionString() == "")
	add("privacy.purposes", len(pr.Purposes) == 0)
	add("privacy.recipients", len(pr.Recipients) == 0)
	add("privacy.subprocessors", len(pr.Subprocessors) == 0)
	return gaps
}

// warnNoticeFallbacks logs, at start and after apply, the notice values that use a fallback,
// so the operator can set them.
func (c *Core) warnNoticeFallbacks(cat *Catalog) {
	if c.cfg.Operator == "" {
		c.log.Warn("the setting operator is empty; notice.ai names the operator as a fallback", "fallback", operatorFallback)
	}
	names := make([]string, 0, len(cat.Tenants))
	for n := range cat.Tenants {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		tv := cat.Tenants[n]
		if tv.Spec.Kind == "" {
			continue
		}
		if gaps := noticeGaps(tv); len(gaps) > 0 {
			c.log.Warn("the privacy notice of a tenant uses fallbacks for fields it does not set", "tenant", n, "fields", strings.Join(gaps, ","))
		}
	}
}

// fillNoticeParams completes the parameters of notices planned before an upgrade: a notice
// of the first version had none or fewer, and the store migration only renamed its key. A
// value already present stays.
func (c *Core) fillNoticeParams(ctx context.Context, cat *Catalog) error {
	rows, err := c.st.Pool.Query(ctx, `SELECT id, tenant, body FROM deliveries
		WHERE state IN ('planned', 'sending') AND body->>'text_key' IN ('notice.ai', 'notice.privacy')`)
	if err != nil {
		return err
	}
	type row struct {
		id, tenant string
		body       map[string]any
	}
	var todo []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.tenant, &r.body); err != nil {
			rows.Close()
			return err
		}
		todo = append(todo, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, r := range todo {
		tv := cat.Tenants[r.tenant]
		if tv == nil || tv.Spec.Kind == "" {
			continue
		}
		var want map[string]any
		if r.body["text_key"] == "notice.ai" {
			want = c.aiNoticeParams(tv)
		} else if want, err = c.privacyNoticeParams(tv); err != nil {
			return err
		}
		have, _ := r.body["params"].(map[string]any)
		if have == nil {
			have = map[string]any{}
		}
		changed := false
		for k, v := range want {
			if _, ok := have[k]; !ok {
				have[k] = v
				changed = true
			}
		}
		if !changed {
			continue
		}
		r.body["params"] = have
		if _, err := c.st.Pool.Exec(ctx, `UPDATE deliveries SET body = $2 WHERE id = $1`, r.id, r.body); err != nil {
			return err
		}
		c.log.Info("a notice planned before the upgrade got its new parameters", "delivery", r.id)
	}
	return nil
}

// cut500 bounds a parameter value to the 500 characters of the delivery body schema.
func cut500(s string) string {
	if r := []rune(s); len(r) > 500 {
		return string(r[:500])
	}
	return s
}

// privacyVersionChange plans a new privacy_notice when the tenant changed its notice version.
func (c *Core) privacyVersionChange(ctx context.Context, tx pgx.Tx, now time.Time, tv *TenantView, channel, principal string, chat Chat) error {
	var seen *string
	if err := tx.QueryRow(ctx, `SELECT privacy_version FROM contacts WHERE tenant = $1 AND channel = $2 AND principal = $3 FOR UPDATE`,
		tv.Name, channel, principal).Scan(&seen); err != nil {
		return err
	}
	if v := tv.Spec.Privacy.NoticeVersionString(); seen == nil || v != "" && *seen != v {
		return c.planPrivacyNotice(ctx, tx, now, tv, channel, principal, chat)
	}
	return nil
}

// resultText assembles the text of a task_result delivery from the result by code: the
// summary_fields of the type in order, else the single string field, else every scalar field
// as "name: value" lines (the delivery body form result).
func resultText(result map[string]any, summaryFields []string) (string, map[string]string) {
	fields := map[string]string{}
	keys := summaryFields
	if len(keys) == 0 {
		for k := range result {
			keys = append(keys, k)
		}
		sort.Strings(keys)
	}
	var lines []string
	for _, k := range keys {
		v, ok := result[k]
		if !ok {
			continue
		}
		var s string
		switch x := v.(type) {
		case string:
			s = x
		case nil:
			continue
		case map[string]any, []any:
			b, _ := json.Marshal(x)
			s = string(b)
		default:
			s = fmt.Sprint(x)
		}
		if len(s) > 2000 {
			s = s[:2000]
		}
		fields[k] = s
		lines = append(lines, k+": "+s)
	}
	if len(fields) == 1 {
		for _, v := range fields {
			return v, fields
		}
	}
	return strings.Join(lines, "\n"), fields
}
