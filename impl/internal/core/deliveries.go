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
		return c.privacyVersionChange(ctx, tx, now, tv, channel, principal, chat)
	}
	if tv.Channels[channel].AIDisclosure == "native" {
		// The messenger itself shows that the account is a bot (DLV-12).
		return c.planPrivacyNotice(ctx, tx, now, tv, channel, principal, chat)
	}
	aiID, err := c.planDelivery(ctx, tx, now, plannedDelivery{
		Tenant: tv.Name, Kind: "ai_notice", Addr: "origin", TargetKind: "chat", TargetID: chat.ID(),
		ExecKind: "channel", ExecName: channel, Recipient: principal, DataOrigin: "code",
		Body: map[string]any{"form": "text_code", "text_key": "ai_notice"},
	})
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE contacts SET ai_notice_delivery = $4 WHERE tenant = $1 AND channel = $2 AND principal = $3`,
		tv.Name, channel, principal, aiID); err != nil {
		return err
	}
	return c.planPrivacyNotice(ctx, tx, now, tv, channel, principal, chat)
}

func (c *Core) planPrivacyNotice(ctx context.Context, tx pgx.Tx, now time.Time, tv *TenantView, channel, principal string, chat Chat) error {
	version := tv.Spec.Privacy.NoticeVersionString()
	params := map[string]any{}
	if version != "" {
		params["notice_version"] = version
	}
	if tv.Spec.Privacy.NoticeRef != "" {
		params["notice_ref"] = tv.Spec.Privacy.NoticeRef
	}
	body := map[string]any{"form": "text_code", "text_key": "privacy_notice"}
	if len(params) > 0 {
		body["params"] = params
	}
	if _, err := c.planDelivery(ctx, tx, now, plannedDelivery{
		Tenant: tv.Name, Kind: "privacy_notice", Addr: "origin", TargetKind: "chat", TargetID: chat.ID(),
		ExecKind: "channel", ExecName: channel, Recipient: principal, DataOrigin: "code", Body: body, NoticeVersion: version,
	}); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE contacts SET privacy_version = $4 WHERE tenant = $1 AND channel = $2 AND principal = $3`,
		tv.Name, channel, principal, version)
	return err
}

// privacyVersionChange plans a new privacy_notice when the tenant changed its notice version.
func (c *Core) privacyVersionChange(ctx context.Context, tx pgx.Tx, now time.Time, tv *TenantView, channel, principal string, chat Chat) error {
	var seen *string
	if err := tx.QueryRow(ctx, `SELECT privacy_version FROM contacts WHERE tenant = $1 AND channel = $2 AND principal = $3 FOR UPDATE`,
		tv.Name, channel, principal).Scan(&seen); err != nil {
		return err
	}
	if v := tv.Spec.Privacy.NoticeVersionString(); v != "" && (seen == nil || *seen != v) {
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
