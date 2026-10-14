// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Activity is the activity schema: what a channel passes to submit_activity.
type Activity struct {
	Kind     string `json:"kind"`
	Channel  string `json:"channel"`
	UpdateID string `json:"update_id"`
	Sender   struct {
		Identity string `json:"identity"`
		Verified bool   `json:"verified"`
	} `json:"sender"`
	Chat      Chat   `json:"chat"`
	Text      string `json:"text,omitempty"`
	ReplyTo   string `json:"reply_to,omitempty"`
	Forwarded bool   `json:"forwarded,omitempty"`
	Quoted    bool   `json:"quoted,omitempty"`
	// ForwardedFrom and QuotedFrom name the author of someone else's text when the messenger
	// shows a person (AUD-5).
	ForwardedFrom string `json:"forwarded_from,omitempty"`
	QuotedFrom    string `json:"quoted_from,omitempty"`
}

// ActivityResult is p1-submit-activity-response.
type ActivityResult struct {
	Result   string `json:"result"`
	Task     string `json:"task,omitempty"`
	Existing bool   `json:"existing,omitempty"`
	Reason   string `json:"reason,omitempty"`
	Text     string `json:"text,omitempty"`
}

// SubmitActivity turns a channel activity into a task by code (IDN-6): a command of the first
// word in a direct chat by the tenant's commands, else a default type of the chat kind. The text
// never chooses anything else; forwarded and quoted text is never a command (IDN-7).
func (c *Core) SubmitActivity(ctx context.Context, w *Worker, a Activity) (*ActivityResult, error) {
	if w.Kind != "channel" {
		return nil, refuse("forbidden", "only a channel submits activity")
	}
	if a.Channel != w.Target {
		return nil, refuse("forbidden", "the activity is for another channel")
	}
	if w.State != "active" && w.State != "draining" {
		return nil, refuse("blocked", "the channel has no accepted hello")
	}
	tv, cat, err := c.requireTenant(ctx, w.Tenant)
	if err != nil {
		return nil, err
	}
	ch, ok := tv.Channels[a.Channel]
	if !ok {
		return nil, refuse("not_found", "no such channel in the tenant")
	}
	// The manifest may have changed to native disclosure after hello: the driver must have
	// reported it, or no ai_notice would be sent (DLV-12).
	if ch.AIDisclosure == "native" && (w.Hello == nil || w.Hello.Driver == nil || w.Hello.Driver.AIDisclosure != "native") {
		return nil, refuse("blocked", "the channel driver did not report native AI disclosure")
	}
	if kind := ch.IdentityKind; kind != "" && !strings.HasPrefix(a.Sender.Identity, kind+":") {
		return nil, refuse("forbidden", "the identity kind does not match the channel driver")
	}
	// A group chat is served only after chat.bind and its membership check, which this version
	// does not have: an answer there would reach members nobody admitted.
	if a.Chat.Kind != "direct" {
		return &ActivityResult{Result: "ignored", Reason: "not_subject"}, nil
	}
	key := "system:activity:" + a.Channel + ":" + a.UpdateID

	var out ActivityResult
	err = c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		// A repeat of the same update returns the same task (DLV-5).
		var existing string
		err := tx.QueryRow(ctx, `SELECT id FROM tasks WHERE tenant = $1 AND idempotency_key = $2`, w.Tenant, key).Scan(&existing)
		if err == nil {
			out = ActivityResult{Result: "task", Task: existing, Existing: true}
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		principal, err := principalByIdentity(ctx, tx, w.Tenant, a.Sender.Identity)
		if errors.Is(err, pgx.ErrNoRows) {
			out = ActivityResult{Result: "ignored", Reason: "not_subject"}
			return nil
		}
		if err != nil {
			return err
		}
		p, err := loadPrincipal(ctx, tx, now, w.Tenant, principal)
		if err != nil {
			return err
		}
		// The recipient's locale, then en (DLV-9); the tenant's locale is a field of v0.2.
		locale := p.Locale
		reply := func(key string) {
			out = ActivityResult{Result: "reply", Text: c.texts.Render(locale, key, nil)}
		}
		if p.State != "active" || p.Kind != "human" {
			out = ActivityResult{Result: "ignored", Reason: "person_inactive"}
			return nil
		}
		if !a.Sender.Verified {
			reply("activity.not_verified")
			return nil
		}
		groups := groupClosure(tv, p.Groups)
		// The commands the core runs by code, without a model and without a task type.
		if word, rest, ok := commandWord(a); ok && (word == "/service" || word == "/run" || codeCommands[word]) {
			// A channel command closes the follow-up window (CNT-5).
			if err := closeFollowups(ctx, tx, w.Tenant, "c:"+a.Channel+":"+a.Chat.ID(), principal); err != nil {
				return err
			}
			if err := c.firstContact(ctx, tx, now, tv, a.Channel, principal, a.Chat); err != nil {
				return err
			}
			pc := personCommand{tv: tv, cat: cat, person: principal, groups: groups, locale: locale,
				channel: a.Channel, chat: a.Chat, key: key}
			var r *ActivityResult
			switch word {
			case "/service":
				r, err = c.serviceCommand(ctx, tx, now, pc, rest)
			case "/run":
				r, err = c.runCommand(ctx, tx, now, pc, rest)
			default:
				r, err = c.codeCommand(ctx, tx, now, pc, word, rest)
			}
			if err != nil {
				return err
			}
			out = *r
			return nil
		}
		typ, goal, ok := c.chooseType(cat, tv, a)
		_, _, command := commandWord(a)
		prev, err := c.continuesTo(ctx, tx, now, tv, a, principal, groups, command && ok)
		if err != nil {
			return err
		}
		// A reply or an answer in the window continues the earlier task with its type, unless
		// the message is a command (CNT-5).
		if prev != nil && !command && strings.TrimSpace(a.Text) != "" {
			typ, goal, ok = prev.Type, strings.TrimSpace(a.Text), true
		}
		if !ok {
			reply("activity.not_command")
			return nil
		}
		if _, known := cat.TaskTypes[typ]; !known {
			reply("activity.unknown_command")
			return nil
		}
		origin := &Origin{Channel: a.Channel, Chat: a.Chat}
		if err := c.firstContact(ctx, tx, now, tv, a.Channel, principal, a.Chat); err != nil {
			return err
		}
		body := map[string]any{"type": typ, "goal": goal}
		in := enqueueInput{
			Tenant: w.Tenant, Type: typ, Body: body, IdempotencyKey: key, Initiator: principal,
			EnqueuedBy: "channel:" + a.Channel, Origin: origin, Source: "interactive", Cause: key,
			Forwarded: a.Forwarded, Quoted: a.Quoted,
		}
		if prev != nil {
			// The outcome of the earlier task is an attachment block of the new one (CNT-5).
			body["attachments"] = []any{map[string]any{"data": prev.Data}}
			in.Continues, in.Untrusted, in.ContinuesMark = prev.ID, prev.Untrusted, prev.Mark
		}
		if in.Subjects, err = c.textAuthors(ctx, tx, w.Tenant, principal, a); err != nil {
			return err
		}
		res, err := c.enqueue(ctx, tx, now, cat, in)
		if err != nil {
			if p, ok := AsProblem(err); ok {
				out = ActivityResult{Result: "reply", Text: c.rejectText(locale, p.Reason)}
				return nil
			}
			return err
		}
		if res.Rejected != nil {
			out = ActivityResult{Result: "reply", Text: c.rejectText(locale, res.Rejected.Reason)}
			return nil
		}
		out = ActivityResult{Result: "task", Task: res.TaskID, Existing: res.Existing}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Core) rejectText(locale, reason string) string {
	if c.texts.Has("task.rejected." + reason) {
		return c.texts.Render(locale, "task.rejected."+reason, nil)
	}
	return c.texts.Render(locale, "task.rejected", map[string]any{"reason": reason})
}

// chooseType picks the task type of an activity and the goal text.
func (c *Core) chooseType(cat *Catalog, tv *TenantView, a Activity) (string, string, bool) {
	text := strings.TrimSpace(a.Text)
	if a.Chat.Kind == "direct" && !a.Forwarded && !a.Quoted && strings.HasPrefix(text, "/") {
		word, rest, _ := strings.Cut(text, " ")
		if i := strings.IndexByte(word, '@'); i >= 0 {
			word = word[:i]
		}
		typ, ok := tv.Spec.Commands[word]
		if !ok {
			return "", "", false
		}
		// The goal is required (task-body): a bare command is its own goal.
		goal := strings.TrimSpace(rest)
		if goal == "" {
			goal = word
		}
		return typ, goal, true
	}
	if text == "" {
		return "", "", false
	}
	for _, d := range tv.Spec.DefaultTypes {
		if len(d.Chats) > 0 && !containsStr(d.Chats, a.Chat.Kind) {
			continue
		}
		if _, ok := cat.TaskTypes[d.Type]; !ok {
			continue
		}
		return d.Type, text, true
	}
	return "", "", false
}

func containsStr(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// textAuthors are the principals of the tenant whose forwarded or quoted text the activity
// carries, other than the sender, for personal_subjects (AUD-5). An author the tenant does not
// know has no principal id to record.
func (c *Core) textAuthors(ctx context.Context, tx pgx.Tx, tenant, sender string, a Activity) ([]string, error) {
	var ids []string
	if a.Forwarded && a.ForwardedFrom != "" {
		ids = append(ids, a.ForwardedFrom)
	}
	if a.Quoted && a.QuotedFrom != "" {
		ids = append(ids, a.QuotedFrom)
	}
	var out []string
	for _, id := range ids {
		p, err := principalByIdentity(ctx, tx, tenant, id)
		if errors.Is(err, pgx.ErrNoRows) || p == sender || slices.Contains(out, p) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}
