// SPDX-License-Identifier: AGPL-3.0-only

package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/HunterEwok/botaru/impl/internal/p1client"
)

// card is the card schema as far as this driver draws it (APR-3): every line comes from a code
// text of the catalog and a field of the record; there is no model text on a card.
type card struct {
	Card             string         `json:"card"`
	SubjectKind      string         `json:"subject_kind"`
	ID               string         `json:"id"`
	Nonce            string         `json:"nonce"`
	Title            string         `json:"title"`
	Args             map[string]any `json:"args"`
	RequestedBy      map[string]any `json:"requested_by"`
	InitiatorDisplay string         `json:"initiator_display"`
	Context          map[string]any `json:"context"`
	UntrustedSeen    bool           `json:"untrusted_seen"`
	UntrustedSource  string         `json:"untrusted_source"`
	Risk             string         `json:"risk"`
	ExpiresAt        string         `json:"expires_at"`
	State            string         `json:"state"`
	Reminder         bool           `json:"reminder"`
	Campaign         *struct {
		Count     int      `json:"count"`
		First     []string `json:"first"`
		Rollout   string   `json:"rollout"`
		Threshold int      `json:"threshold"`
	} `json:"campaign"`
	Buttons []struct {
		Code        string `json:"code"`
		SubjectKind string `json:"subject_kind"`
		ID          string `json:"id"`
		Nonce       string `json:"nonce"`
	} `json:"buttons"`
	Footer struct {
		WhyYou    string `json:"why_you"`
		Until     string `json:"until"`
		OnSilence string `json:"on_silence"`
	} `json:"footer"`
}

// buttonCodes are the two-character codes of callback_data (spec/bindings/channels/telegram.md).
var buttonCodes = map[string]string{
	"approve": "ap", "reject": "rj", "defer": "df", "exclude": "ex", "details": "dt", "remind": "rm", "accept": "ac",
	"yes": "ye", "no": "no", "unknown": "un", "withdraw": "wd", "override_veto": "ov", "would_accept": "wa",
	"would_not_accept": "wn", "confirm": "cf", "count": "ct", "full_list": "fl", "pause": "ps", "cancel": "cn",
}

// openStates are the states of a subject whose card still waits for its person: a pending
// approval, a presented proposal, a change being carried out.
var openStates = []string{"pending", "presented", "realizing"}

// subjectPrefixes give the subject kind of a card by the prefix of its id.
var subjectPrefixes = map[string]string{"a": "approval", "prp": "proposal", "cmp": "campaign", "scr": "script", "scg": "script_gate"}

// buttonLabels are the text keys of the button labels this driver draws.
var buttonLabels = map[string]string{
	"approve": "card.button.approve", "reject": "card.button.reject", "details": "card.button.details", "remind": "card.copy.remind_button",
	"accept": "card.button.accept", "defer": "card.button.defer", "pause": "card.button.pause", "cancel": "card.button.cancel",
}

// titleKeys are the title text keys of the card kinds that are not an approval card.
var titleKeys = map[string]string{"proposal_card": "card.proposal.title", "realization_card": "card.realization.title", "rollback_card": "card.rollback.title",
	"campaign_approval": "card.campaign.title", "campaign_progress": "card.campaign.progress_title",
	"script_confirm": "card.script.title", "script_gate_card": "card.script.gate_title"}

// flat writes a map of closed fields as key=value pairs in key order.
func flat(m map[string]any) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		v := m[k]
		if b, err := json.Marshal(v); err == nil {
			if s, ok := v.(string); ok {
				parts = append(parts, k+"="+s)
				continue
			}
			parts = append(parts, k+"="+string(b))
		}
	}
	return strings.Join(parts, ", ")
}

// renderCard draws the card text: the reminder line, the title, risk, arguments, who asked,
// the context line, the untrusted flag, the state, the deadline and the bottom line (APR-3,
// APR-12, APR-19).
func (c *Channel) renderCard(locale string, cd *card) string {
	t := c.cfg.Texts
	var lines []string
	if cd.Reminder {
		lines = append(lines, t.Render(locale, "card.remind", map[string]any{"title": cd.Title, "deadline": cd.ExpiresAt, "initiator": cd.InitiatorDisplay}))
	}
	titleKey := "card.title"
	if k, ok := titleKeys[cd.Card]; ok {
		titleKey = k
	}
	lines = append(lines, t.Render(locale, titleKey, map[string]any{"title": cd.Title}))
	if cd.Risk != "" {
		lines = append(lines, t.Render(locale, "card.risk", map[string]any{"risk": cd.Risk}))
	}
	if len(cd.Args) > 0 {
		lines = append(lines, t.Render(locale, "card.args", map[string]any{"args": flat(cd.Args)}))
	}
	if cp := cd.Campaign; cp != nil {
		lines = append(lines, t.Render(locale, "card.campaign.items", map[string]any{"count": cp.Count, "first": strings.Join(cp.First, ", ")}),
			t.Render(locale, "card.campaign.rollout", map[string]any{"rollout": cp.Rollout}))
	}
	source, _ := cd.RequestedBy["source"].(string)
	if role, ok := cd.RequestedBy["role"].(string); ok {
		source += ", " + role
	}
	lines = append(lines, t.Render(locale, "card.requested_by", map[string]any{"initiator": cd.InitiatorDisplay, "source": source}))
	if len(cd.Context) > 0 {
		lines = append(lines, t.Render(locale, "card.context", map[string]any{"context": flat(cd.Context)}))
	}
	if cd.UntrustedSeen {
		src := cd.UntrustedSource
		if src == "" {
			src = "-"
		}
		lines = append(lines, t.Render(locale, "card.untrusted", map[string]any{"source": src}))
	}
	// A card still waiting for its person shows the deadline, a closed one its state.
	switch {
	case cd.State != "" && !slices.Contains(openStates, cd.State):
		lines = append(lines, t.Render(locale, "card.state", map[string]any{"state": cd.State}))
	case cd.ExpiresAt != "":
		lines = append(lines, t.Render(locale, "card.deadline", map[string]any{"deadline": cd.ExpiresAt}))
	}
	footer := "card.footer"
	switch {
	case cd.Card == "realization_card" && cd.Footer.Until != "":
		footer = "card.realization.footer"
	case cd.Card == "realization_card" || cd.Card == "rollback_card":
		footer = "card.executor.footer"
	case cd.Card == "campaign_progress":
		footer = "card.campaign.footer"
	case cd.Card == "script_confirm" || cd.Card == "script_gate_card":
		footer = "card.script.footer"
	}
	lines = append(lines, t.Render(locale, footer, map[string]any{"why_you": cd.Footer.WhyYou, "until": cd.Footer.Until, "on_silence": cd.Footer.OnSilence}))
	return strings.Join(lines, "\n")
}

// keyboard is the inline keyboard of a card: callback_data <id>:<nonce>:<code> (APR-3). A
// card without buttons gets an empty keyboard, so an edit removes the old buttons.
func (c *Channel) keyboard(locale string, cd *card) any {
	row := []any{}
	for _, b := range cd.Buttons {
		code, ok := buttonCodes[b.Code]
		label, labeled := buttonLabels[b.Code]
		if !ok || !labeled {
			continue
		}
		data := b.ID + ":" + b.Nonce + ":" + code
		if len(data) > 64 {
			continue
		}
		row = append(row, map[string]any{"text": c.cfg.Texts.Render(locale, label, nil), "callback_data": data})
	}
	if len(row) == 0 {
		return map[string]any{"inline_keyboard": []any{}}
	}
	return map[string]any{"inline_keyboard": []any{row}}
}

// edit replaces the text of a card message; "message is not modified" is not an error.
func (c *Channel) edit(ctx context.Context, chatID, messageID int64, text string, markup any) error {
	params := map[string]any{"chat_id": chatID, "message_id": messageID, "text": cutText(text), "link_preview_options": map[string]any{"is_disabled": true}}
	if markup != nil {
		params["reply_markup"] = markup
	}
	_, err := c.botCall(ctx, "editMessageText", params)
	var ae *APIError
	if errors.As(err, &ae) && ae.Status == 400 && strings.Contains(ae.Description, "not modified") {
		return nil
	}
	return err
}

// parseData reads callback_data <id>:<nonce>:<code>; the subject kind comes from the id prefix.
func parseData(data string) (kind, id, nonce, code string, ok bool) {
	parts := strings.Split(data, ":")
	if len(parts) != 3 {
		return "", "", "", "", false
	}
	for full, short := range buttonCodes {
		if short == parts[2] {
			code = full
		}
	}
	prefix, _, _ := strings.Cut(parts[0], "_")
	kind = subjectPrefixes[prefix]
	if code == "" || kind == "" || parts[1] == "" {
		return "", "", "", "", false
	}
	return kind, parts[0], parts[1], code, true
}

// press turns a button press into P1 decide, card.details or card.remind on behalf of the
// person who pressed, and answers the press with a short code text (APR-4).
func (c *Channel) press(ctx context.Context, q *callbackQuery) error {
	answer := func(key string, params map[string]any) {
		if _, err := c.botCall(ctx, "answerCallbackQuery", map[string]any{"callback_query_id": q.ID, "text": c.cfg.Texts.Render("", key, params)}); err != nil {
			c.log.Warn("answering a press failed", "err", err)
		}
	}
	kind, id, nonce, code, ok := parseData(q.Data)
	if !ok || q.Message == nil || q.From.IsBot {
		answer("card.answer.forbidden", nil)
		return nil
	}
	chatKind := "direct"
	if q.Message.Chat.Type == "group" || q.Message.Chat.Type == "supergroup" {
		chatKind = "group"
	}
	headers := map[string]string{
		"Botaru-On-Behalf": "telegram:" + strconv.FormatInt(q.From.ID, 10),
		"Botaru-Chat":      chatKind + ":" + strconv.FormatInt(q.Message.Chat.ID, 10),
		"Botaru-Message":   strconv.FormatInt(q.Message.MessageID, 10),
	}
	var err error
	switch code {
	case "details":
		var out struct {
			Template string         `json:"template"`
			Fields   map[string]any `json:"fields"`
		}
		err = c.cfg.Core.CallAs(ctx, "card.details", headers, map[string]any{"op": "card.details", "subject_kind": kind, "id": id, "nonce": nonce}, &out)
		if err == nil {
			answer("card.answer.accepted", nil)
			lines := []string{c.cfg.Texts.Render("", "card.details.header", map[string]any{"title": out.Fields["title"]})}
			keys := make([]string, 0, len(out.Fields))
			for k := range out.Fields {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				v := out.Fields[k]
				if m, ok := v.(map[string]any); ok {
					v = flat(m)
				}
				lines = append(lines, c.cfg.Texts.Render("", "card.details.row", map[string]any{"field": k, "value": fmt.Sprint(v)}))
			}
			if _, err := c.send(ctx, q.Message.Chat.ID, strings.Join(lines, "\n")); err != nil {
				c.log.Warn("sending card details failed", "err", err)
			}
			return nil
		}
	case "remind":
		err = c.cfg.Core.CallAs(ctx, "card.remind", headers, map[string]any{"op": "card.remind", "subject_kind": kind, "id": id, "nonce": nonce}, nil)
	case "pause", "cancel":
		// Campaign and script operations go as P1 campaign.control and script.control (e1-channel).
		op := "campaign.control"
		if kind == "script" {
			op = "script.control"
		}
		err = c.cfg.Core.CallAs(ctx, op, headers, map[string]any{"id": id, "command": code, "nonce": nonce}, nil)
	default:
		err = c.cfg.Core.CallAs(ctx, "decide", headers, map[string]any{"subject_kind": kind, "id": id, "nonce": nonce, "decision": code}, nil)
	}
	switch r := p1client.Reason(err); {
	case err == nil:
		answer("card.answer.accepted", nil)
	case r == "expired":
		answer("card.answer.expired", nil)
	case r == "already_decided":
		answer("card.answer.already_decided", nil)
	case r == "service_rate_limited":
		answer("card.remind.rate_limited", map[string]any{"minutes": 5})
	case r == "already_terminal":
		answer("card.answer.already_decided", nil)
	case r == "forbidden" || r == "not_found" || r == "held" || r == "estimate_exceeded":
		answer("card.answer.forbidden", nil)
	default:
		var pe *p1client.Error
		if errors.As(err, &pe) && pe.Status < 500 && pe.Status != 429 {
			answer("card.answer.failed", nil)
			return nil
		}
		// The core did not answer: the update is submitted again on the next poll (DLV-5).
		return err
	}
	return nil
}
