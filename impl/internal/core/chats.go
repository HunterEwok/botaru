// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/manifest"
	"github.com/HunterEwok/botaru/impl/internal/store"
)

// ChatBindRequest is p2-chat-bind-request.
type ChatBindRequest struct {
	Op        string `json:"op"`
	Tenant    string `json:"tenant"`
	Channel   string `json:"channel"`
	Chat      Chat   `json:"chat"`
	Ceiling   string `json:"ceiling"`
	Name      string `json:"name"`
	Assistant bool   `json:"assistant"`
	Locale    string `json:"locale"`
	ModelText string `json:"model_text"`
	// NoticeRepeat is how often code repeats the notices to a non-principal (MSG-17).
	NoticeRepeat string `json:"notice_repeat"`
}

// chatBinding is a bound group chat (DLV-7).
type chatBinding struct {
	Tenant, Name, Channel, Chat, Ceiling, ModelText string
	Locale                                          string
	MembersUnknown                                  bool
	// NoticeRepeat is the period of the notices to a non-principal in the chat (MSG-17).
	NoticeRepeat time.Duration
}

// defaultNoticeRepeat is the period of the notices to a non-principal by default (MSG-17).
const defaultNoticeRepeat = 90 * 24 * time.Hour

var bindingNameBad = regexp.MustCompile(`[^a-z0-9-]+`)

// bindingName derives the name of a binding that the request does not name: the channel and
// the external chat id, lowercased, with other characters replaced by a dash.
func bindingName(channel, external string) string {
	n := strings.Trim(bindingNameBad.ReplaceAllString(strings.ToLower(channel+"-"+external), "-"), "-")
	if len(n) > 63 {
		n = strings.TrimRight(n[:63], "-")
	}
	return n
}

// loadBinding is the binding of a group chat of a channel, or nil.
func loadBinding(ctx context.Context, q store.Querier, tenant, channel, chat string) (*chatBinding, error) {
	var b chatBinding
	var locale, repeat *string
	err := q.QueryRow(ctx, `SELECT tenant, name, channel, chat, ceiling, model_text, locale, members_unknown, notice_repeat FROM chat_bindings WHERE tenant = $1 AND channel = $2 AND chat = $3`,
		tenant, channel, chat).Scan(&b.Tenant, &b.Name, &b.Channel, &b.Chat, &b.Ceiling, &b.ModelText, &locale, &b.MembersUnknown, &repeat)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	b.Locale = deref(locale)
	b.NoticeRepeat = min(max(durationOr(deref(repeat), defaultNoticeRepeat), 24*time.Hour), 366*24*time.Hour)
	return &b, nil
}

// bindingByName is the binding named name, or nil.
func bindingByName(ctx context.Context, q store.Querier, tenant, name string) (*chatBinding, error) {
	var channel, chat string
	err := q.QueryRow(ctx, `SELECT channel, chat FROM chat_bindings WHERE tenant = $1 AND name = $2`, tenant, name).Scan(&channel, &chat)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return loadBinding(ctx, q, tenant, channel, chat)
}

// membershipEnforced says whether an active worker of the channel reported at hello that its
// driver sees members joining group chats (DLV-7).
func membershipEnforced(ctx context.Context, q store.Querier, tenant, channel string) (bool, error) {
	var ok bool
	err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM workers WHERE tenant = $1 AND kind = 'channel' AND target = $2 AND state IN ('active', 'draining')
		AND hello->'driver'->>'membership_assurance' = 'enforced')`, tenant, channel).Scan(&ok)
	return ok, err
}

// ChatBind binds a group chat to the tenant with a class ceiling (DLV-7, PRM-14). A ceiling
// above summary needs a channel driver with membership_assurance: enforced. Binding the chat
// again replaces its settings and clears the unknown member mark: the operator checked the
// members.
func (c *Core) ChatBind(ctx context.Context, op Operator, req ChatBindRequest) (*Done, error) {
	if req.Tenant != op.Tenant {
		return nil, refuse("forbidden", "the operator credential is for another tenant")
	}
	if req.Assistant || req.Chat.Kind != "group" {
		return nil, refuse("version_unsupported", "this build binds group chats only; the personal assistant comes in v0.2")
	}
	tv, _, err := c.requireTenant(ctx, op.Tenant)
	if err != nil {
		return nil, err
	}
	if _, ok := tv.Channels[req.Channel]; !ok {
		return nil, refuse("not_found", "no such channel in the tenant")
	}
	if _, ok := c.markOrder(req.Ceiling); !ok {
		return nil, refuse("input_schema_invalid", "unknown data class %s", req.Ceiling)
	}
	name := req.Name
	if name == "" {
		name = bindingName(req.Channel, req.Chat.ExternalID)
	}
	modelText := req.ModelText
	if modelText == "" {
		modelText = "summary_only"
	}
	if modelText != "summary_only" {
		return nil, refuse("version_unsupported", "this build sends only the closed summary to a group (model_text: summary_only)")
	}
	// The notices repeat at most once a day and at least once a year (MSG-17).
	if d, ok := parseDuration(req.NoticeRepeat); req.NoticeRepeat != "" && (!ok || d < 24*time.Hour || d > 366*24*time.Hour) {
		return nil, refuse("input_schema_invalid", "notice_repeat is a duration from 1d to 366d")
	}
	chat := req.Chat.ID()
	var out Done
	err = c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		if !c.markWithin(req.Ceiling, "summary") {
			enforced, err := membershipEnforced(ctx, tx, op.Tenant, req.Channel)
			if err != nil {
				return err
			}
			if !enforced {
				return refuse("forbidden", "a ceiling above summary needs a channel driver with membership_assurance: enforced")
			}
		}
		var other string
		err := tx.QueryRow(ctx, `SELECT name FROM chat_bindings WHERE tenant = $1 AND (name = $2) <> (channel = $3 AND chat = $4) AND (name = $2 OR (channel = $3 AND chat = $4))`,
			op.Tenant, name, req.Channel, chat).Scan(&other)
		if err == nil {
			return refuse("conflict", "the name or the chat is bound already as %s", other)
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		var known bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM chat_bindings WHERE tenant = $1 AND name = $2)`, op.Tenant, name).Scan(&known); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO chat_bindings (tenant, name, channel, chat, ceiling, locale, model_text, created_by, created_at, updated_at, notice_repeat)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $9, $10)
			ON CONFLICT (tenant, name) DO UPDATE SET ceiling = $5, locale = $6, model_text = $7, members_unknown = false, updated_at = $9, notice_repeat = $10`,
			op.Tenant, name, req.Channel, chat, req.Ceiling, nullable(req.Locale), modelText, op.Principal, now, nullable(req.NoticeRepeat)); err != nil {
			return err
		}
		// A newly bound chat on a channel with ai_disclosure: first_contact gets ai_notice and
		// privacy_notice, as a person does at first contact (DLV-12).
		if !known && tv.Channels[req.Channel].AIDisclosure == "first_contact" {
			params, err := c.privacyNoticeParams(tv)
			if err != nil {
				return err
			}
			for _, n := range []plannedDelivery{
				{Kind: "ai_notice", Body: map[string]any{"form": "text_code", "text_key": "notice.ai", "params": c.aiNoticeParams(tv)}},
				{Kind: "privacy_notice", Body: map[string]any{"form": "text_code", "text_key": "notice.privacy", "params": params},
					NoticeVersion: tv.Spec.Privacy.NoticeVersionString()},
			} {
				n.Tenant, n.Addr, n.TargetKind, n.TargetID, n.ExecKind, n.ExecName, n.DataOrigin = op.Tenant, "chat:"+name, "chat", chat, "channel", req.Channel, "code"
				if _, err := c.planDelivery(ctx, tx, now, n); err != nil {
					return err
				}
			}
		}
		out = Done{OK: true, ID: name}
		_, err = c.appendAudit(ctx, tx, now, Audit{
			Tenant: op.Tenant, Kind: "command", Actor: Actor{Principal: op.Principal, Via: "farmctl"},
			Command: "chat.bind", Target: &Target{Kind: "chat_binding", ID: name}, OK: true,
			After: map[string]any{"channel": req.Channel, "chat": chat, "ceiling": req.Ceiling, "model_text": modelText},
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// markMembersUnknown stops the deliveries to a bound chat after a person outside the tenant
// appeared there, until the operator binds it again (DLV-7).
func (c *Core) markMembersUnknown(ctx context.Context, tx pgx.Tx, now time.Time, b *chatBinding) error {
	if b.MembersUnknown {
		return nil
	}
	b.MembersUnknown = true
	if _, err := tx.Exec(ctx, `UPDATE chat_bindings SET members_unknown = true, updated_at = $3 WHERE tenant = $1 AND name = $2`, b.Tenant, b.Name, now); err != nil {
		return err
	}
	return c.appendEvent(ctx, tx, now, b.Tenant, Event{Type: "chat.member_unknown", Subject: b.Name, Data: map[string]any{"chat": b.Name}})
}

// closedSummary is the summary of a result for a group chat: the type's summary_fields whose
// schema closes the value (a number, a boolean, an enum or a const), in the order of
// summary_fields, as code writes them (DLV-15, OUT-1). Free text never goes to a group.
func closedSummary(tt manifest.TaskType, result map[string]any) (string, map[string]string) {
	props, _ := tt.ResultSchema["properties"].(map[string]any)
	fields := map[string]string{}
	var lines []string
	for _, k := range tt.SummaryFields {
		v, ok := result[k]
		if !ok || v == nil {
			continue
		}
		s, _ := props[k].(map[string]any)
		_, isEnum := s["enum"]
		_, isConst := s["const"]
		typ, _ := s["type"].(string)
		if !(isEnum || isConst || typ == "integer" || typ == "number" || typ == "boolean") {
			continue
		}
		// The value must be of the closed kind its schema names; anything else is left out.
		var text string
		switch x := v.(type) {
		case string:
			if !isEnum && !isConst {
				continue
			}
			text = x
		case map[string]any, []any:
			continue
		default:
			b, _ := json.Marshal(x)
			text = string(b)
		}
		if len(text) > 200 {
			continue
		}
		fields[k] = text
		lines = append(lines, fmt.Sprintf("%s: %s", k, text))
	}
	return strings.Join(lines, "\n"), fields
}

// planGroupAnswer splits the answer to a request from a group chat (DLV-15): the chat gets the
// closed summary as code text within the binding ceiling and the text "details sent
// privately"; the full answer goes to the asker's direct chat in the same channel. When the
// asker has no direct chat there, the chat gets a request to open one and nothing else.
func (c *Core) planGroupAnswer(ctx context.Context, tx pgx.Tx, now time.Time, tv *TenantView, task, initiator string, origin *Origin, tt manifest.TaskType, result, body map[string]any, mark string) error {
	tenant := tv.Name
	b, err := loadBinding(ctx, tx, tenant, origin.Channel, origin.Chat.ID())
	if err != nil {
		return err
	}
	group := func(body map[string]any, mark string) error {
		_, err := c.planDelivery(ctx, tx, now, plannedDelivery{
			Tenant: tenant, Task: task, Kind: "task_result", Addr: "origin", TargetKind: "chat", TargetID: origin.Chat.ID(),
			ExecKind: "channel", ExecName: origin.Channel, Body: body, DataMark: mark, DataOrigin: "code",
		})
		return err
	}
	dm, err := directChat(ctx, tx, tv, initiator, origin.Channel)
	if err != nil {
		return err
	}
	if dm != "" {
		// A person whose notice of AI never reached the direct chat cannot get model text
		// there (DLV-12).
		var dead bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM contacts ct JOIN deliveries d ON d.id = ct.ai_notice_delivery
			WHERE ct.tenant = $1 AND ct.channel = $2 AND ct.principal = $3 AND ct.ai_notice_at IS NULL AND d.state = 'dead')`,
			tenant, origin.Channel, initiator).Scan(&dead); err != nil {
			return err
		}
		if dead {
			dm = ""
		}
	}
	if dm == "" {
		return group(map[string]any{"form": "text_code", "text_key": "delivery.open_private_chat"}, "")
	}
	if b != nil && c.markWithin(mark, b.Ceiling) {
		if text, fields := closedSummary(tt, result); text != "" {
			if err := group(map[string]any{"form": "result", "text": text, "fields": fields}, mark); err != nil {
				return err
			}
		}
	}
	if err := group(map[string]any{"form": "text_code", "text_key": "delivery.details_private"}, ""); err != nil {
		return err
	}
	id, err := c.planDelivery(ctx, tx, now, plannedDelivery{
		Tenant: tenant, Task: task, Kind: "task_result", Addr: "origin", TargetKind: "chat", TargetID: dm,
		ExecKind: "channel", ExecName: origin.Channel, Recipient: initiator, Body: body, DataMark: mark, DataOrigin: "model",
	})
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE deliveries SET fallback_chat = $2 WHERE id = $1`, id, origin.Chat.ID())
	return err
}

// directChat is the direct chat id of a principal in a channel of the tenant: the external
// user id of their identity of the channel's identity kind, or "".
func directChat(ctx context.Context, q store.Querier, tv *TenantView, principal, channel string) (string, error) {
	ch, ok := tv.Channels[channel]
	if !ok || ch.IdentityKind == "" || ch.Direction == "in" {
		return "", nil
	}
	var identity string
	err := q.QueryRow(ctx, `SELECT identity FROM identities WHERE tenant = $1 AND principal = $2 AND identity LIKE $3 ORDER BY identity LIMIT 1`,
		tv.Name, principal, ch.IdentityKind+":%").Scan(&identity)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	_, ext, _ := strings.Cut(identity, ":")
	return Chat{Kind: "direct", ExternalID: ext}.ID(), nil
}
