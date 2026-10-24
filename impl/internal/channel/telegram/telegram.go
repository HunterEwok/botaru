// SPDX-License-Identifier: AGPL-3.0-only

// Package telegram is the E1 channel driver for the Telegram Bot API
// (spec/bindings/channels/telegram.md): getUpdates polling into submit_activity, and
// deliveries into sendMessage, cards with buttons and their presses (callback_query) into P1
// decide, card.details and card.remind. Files and group membership come later.
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/HunterEwok/botaru/impl/internal/p1client"
	"github.com/HunterEwok/botaru/impl/internal/texts"
	"github.com/HunterEwok/botaru/impl/internal/version"
)

// Config is the channel process settings. The bot token comes from a deployment secret.
type Config struct {
	Core          *p1client.Client
	Channel       string // the farm channel name, for example telegram
	BotAPI        string // the Bot API server, https://api.telegram.org by default
	BotToken      string
	Texts         *texts.Catalog
	LinkAllowlist []string
	PollTimeoutS  int
	HTTP          *http.Client
	Logger        *slog.Logger
}

// Channel runs the driver.
type Channel struct {
	cfg    Config
	offset int64
	log    *slog.Logger
	// reported is when the channel last reported a successful poll to the core (OBS-10).
	reported time.Time
}

// upstreamEvery is how often the channel reports the time of its last successful poll.
const upstreamEvery = time.Minute

// New returns a channel driver.
func New(cfg Config) *Channel {
	if cfg.BotAPI == "" {
		cfg.BotAPI = "https://api.telegram.org"
	}
	if cfg.PollTimeoutS == 0 {
		cfg.PollTimeoutS = 30
	}
	if cfg.HTTP == nil {
		cfg.HTTP = &http.Client{Timeout: time.Duration(cfg.PollTimeoutS+30) * time.Second}
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Channel{cfg: cfg, log: cfg.Logger}
}

// Hello reports the channel and its declared capabilities to the core.
func (c *Channel) Hello(ctx context.Context) error {
	return c.hello(ctx, time.Time{})
}

// hello reports the channel; a non-zero polled is the time of the last successful getUpdates,
// sent as the health report of the channel's upstream (OBS-10).
func (c *Channel) hello(ctx context.Context, polled time.Time) error {
	req := map[string]any{
		"kind": "channel", "implementation": "botaru-telegram " + version.Version, "capacity": 1, "protocol_versions": []string{"v1"},
		"driver": map[string]any{"ai_disclosure": "first_contact", "provenance": "visible_label", "stores_content": "none"},
	}
	if !polled.IsZero() {
		req["health"] = []any{map[string]any{"component": "channel_upstream", "name": c.cfg.Channel, "state": "ok",
			"time": polled.UTC().Format("2006-01-02T15:04:05.000Z")}}
	}
	return c.cfg.Core.Call(ctx, "hello", "", req, nil)
}

// reportPoll sends the time of a successful poll at most once a minute; a failed report is
// logged and tried again after the next poll.
func (c *Channel) reportPoll(ctx context.Context, polled time.Time) {
	if !c.reported.IsZero() && polled.Sub(c.reported) < upstreamEvery {
		return
	}
	if err := c.hello(ctx, polled); err != nil {
		c.log.Warn("reporting the last poll failed", "err", err)
		return
	}
	c.reported = polled
}

// Run says hello, then polls updates and serves deliveries until ctx ends.
func (c *Channel) Run(ctx context.Context) error {
	for {
		err := c.Hello(ctx)
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if r := p1client.Reason(err); r == "blocked" || r == "unauthenticated" || r == "version_unsupported" {
			return err
		}
		c.log.Warn("hello failed", "err", err)
		sleep(ctx, 5*time.Second)
	}
	errc := make(chan error, 1)
	go func() { errc <- c.deliveries(ctx) }()
	for ctx.Err() == nil {
		if err := c.PollOnce(ctx); err != nil && ctx.Err() == nil {
			c.log.Warn("polling updates", "err", err)
			sleep(ctx, 3*time.Second)
		} else if err == nil {
			c.reportPoll(ctx, time.Now())
		}
	}
	<-errc
	return ctx.Err()
}

// botCall calls a Bot API method. It returns the result field, or an *APIError.
func (c *Channel) botCall(ctx context.Context, method string, params any) (json.RawMessage, error) {
	body, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	u := strings.TrimRight(c.cfg.BotAPI, "/") + "/bot" + c.cfg.BotToken + "/" + method
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.cfg.HTTP.Do(req)
	if err != nil {
		// The error text carries the address with the bot token; it does not go to logs.
		var ue *url.Error
		if errors.As(err, &ue) {
			return nil, fmt.Errorf("bot api %s: %w", method, ue.Err)
		}
		return nil, fmt.Errorf("bot api %s: request failed", method)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	var out struct {
		OK          bool            `json:"ok"`
		Result      json.RawMessage `json:"result"`
		ErrorCode   int             `json:"error_code"`
		Description string          `json:"description"`
		Parameters  struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, &APIError{Status: resp.StatusCode, Description: "unreadable answer"}
	}
	if !out.OK {
		code := out.ErrorCode
		if code == 0 {
			code = resp.StatusCode
		}
		return nil, &APIError{Status: code, Description: out.Description, RetryAfter: out.Parameters.RetryAfter}
	}
	return out.Result, nil
}

// APIError is a Bot API refusal.
type APIError struct {
	Status      int
	Description string
	RetryAfter  int
}

func (e *APIError) Error() string { return fmt.Sprintf("bot api %d: %s", e.Status, e.Description) }

type update struct {
	UpdateID      int64          `json:"update_id"`
	Message       *message       `json:"message"`
	CallbackQuery *callbackQuery `json:"callback_query"`
}

// callbackQuery is a press of an inline button.
type callbackQuery struct {
	ID   string `json:"id"`
	From struct {
		ID    int64 `json:"id"`
		IsBot bool  `json:"is_bot"`
	} `json:"from"`
	Message *struct {
		MessageID int64 `json:"message_id"`
		Chat      struct {
			ID   int64  `json:"id"`
			Type string `json:"type"`
		} `json:"chat"`
	} `json:"message"`
	Data string `json:"data"`
}

type message struct {
	MessageID int64 `json:"message_id"`
	From      *struct {
		ID    int64 `json:"id"`
		IsBot bool  `json:"is_bot"`
	} `json:"from"`
	SenderChat         *struct{} `json:"sender_chat"`
	SenderBusinessBot  *struct{} `json:"sender_business_bot"`
	ViaBot             *struct{} `json:"via_bot"`
	ForwardOrigin      *origin   `json:"forward_origin"`
	IsAutomaticForward bool      `json:"is_automatic_forward"`
	Quote              *struct{} `json:"quote"`
	ExternalReply      *struct {
		Origin *origin `json:"origin"`
	} `json:"external_reply"`
	Chat struct {
		ID   int64  `json:"id"`
		Type string `json:"type"`
	} `json:"chat"`
	Text           string `json:"text"`
	Caption        string `json:"caption"`
	ReplyToMessage *struct {
		MessageID int64 `json:"message_id"`
		From      *struct {
			ID    int64 `json:"id"`
			IsBot bool  `json:"is_bot"`
		} `json:"from"`
	} `json:"reply_to_message"`
}

// origin is the MessageOrigin of the Bot API: the author of forwarded or quoted text.
type origin struct {
	Type       string `json:"type"`
	SenderUser *struct {
		ID    int64 `json:"id"`
		IsBot bool  `json:"is_bot"`
	} `json:"sender_user"`
}

// person is the identity of an origin that is a visible person, or "".
func (o *origin) person() string {
	if o == nil || o.Type != "user" || o.SenderUser == nil || o.SenderUser.IsBot {
		return ""
	}
	return "telegram:" + strconv.FormatInt(o.SenderUser.ID, 10)
}

// PollOnce reads one batch of updates and submits them. The offset moves past an update only
// after the core answered for it (DLV-5).
func (c *Channel) PollOnce(ctx context.Context) error {
	params := map[string]any{"timeout": c.cfg.PollTimeoutS, "allowed_updates": []string{"message", "callback_query"}}
	if c.offset > 0 {
		params["offset"] = c.offset
	}
	res, err := c.botCall(ctx, "getUpdates", params)
	if err != nil {
		return err
	}
	var ups []update
	if err := json.Unmarshal(res, &ups); err != nil {
		return err
	}
	for _, u := range ups {
		if err := c.submit(ctx, u); err != nil {
			return err
		}
		c.offset = u.UpdateID + 1
	}
	return nil
}

// Activity turns a Telegram message into an activity (binding table "Input"). It returns nil
// for a message the driver does not submit.
func Activity(channel string, u update) map[string]any {
	m := u.Message
	if m == nil || m.Chat.Type == "channel" || m.From == nil && m.SenderChat == nil {
		return nil
	}
	kind := "direct"
	if m.Chat.Type == "group" || m.Chat.Type == "supergroup" {
		kind = "group"
	} else if m.Chat.Type != "private" {
		return nil
	}
	a := map[string]any{
		"kind": "message", "channel": channel, "update_id": strconv.FormatInt(u.UpdateID, 10),
		"chat":        map[string]any{"kind": kind, "external_id": strconv.FormatInt(m.Chat.ID, 10)},
		"received_at": time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
	}
	verified := m.From != nil && m.SenderChat == nil && !m.From.IsBot
	identity := ""
	if m.From != nil {
		identity = "telegram:" + strconv.FormatInt(m.From.ID, 10)
	}
	if identity == "" {
		return nil
	}
	a["sender"] = map[string]any{"identity": identity, "verified": verified}
	text := m.Text
	if text == "" {
		text = m.Caption
	}
	if text != "" {
		if r := []rune(text); len(r) > 4096 {
			text = string(r[:4096])
		}
		a["text"] = text
	}
	if m.ReplyToMessage != nil {
		a["reply_to"] = strconv.FormatInt(m.ReplyToMessage.MessageID, 10)
	}
	if m.ForwardOrigin != nil || m.ViaBot != nil || m.IsAutomaticForward || m.SenderBusinessBot != nil {
		a["forwarded"] = true
	}
	// The author of someone else's text, when the messenger shows a person, so the core can
	// find their data in this task (AUD-5).
	if id := m.ForwardOrigin.person(); id != "" {
		a["forwarded_from"] = id
	}
	if m.Quote != nil || m.ExternalReply != nil {
		a["quoted"] = true
	}
	switch {
	case m.Quote != nil && m.ReplyToMessage != nil && m.ReplyToMessage.From != nil && !m.ReplyToMessage.From.IsBot:
		a["quoted_from"] = "telegram:" + strconv.FormatInt(m.ReplyToMessage.From.ID, 10)
	case m.ExternalReply != nil:
		if id := m.ExternalReply.Origin.person(); id != "" {
			a["quoted_from"] = id
		}
	}
	return a
}

func (c *Channel) submit(ctx context.Context, u update) error {
	if u.CallbackQuery != nil {
		return c.press(ctx, u.CallbackQuery)
	}
	a := Activity(c.cfg.Channel, u)
	if a == nil {
		return nil
	}
	var out struct {
		Result string `json:"result"`
		Text   string `json:"text"`
		Reason string `json:"reason"`
	}
	err := c.cfg.Core.Call(ctx, "submit_activity", "", a, &out)
	if err != nil {
		var pe *p1client.Error
		if errors.As(err, &pe) && pe.Status < 500 && pe.Status != 429 {
			// A refusal of this update; a repeat would get the same answer.
			c.log.Warn("activity refused", "update", u.UpdateID, "reason", pe.Reason)
			return nil
		}
		return err
	}
	if out.Result == "reply" && out.Text != "" {
		chat := u.Message.Chat.ID
		if _, err := c.send(ctx, chat, out.Text); err != nil {
			c.log.Warn("sending a code reply failed", "err", err)
		}
	}
	return nil
}

func (c *Channel) send(ctx context.Context, chatID int64, text string) (int64, error) {
	return c.sendWith(ctx, chatID, text, nil)
}

func cutText(text string) string {
	if r := []rune(text); len(r) > 4096 {
		return string(r[:4095]) + "…"
	}
	return text
}

// sendWith sends a message, with the inline keyboard of a card when markup is set.
func (c *Channel) sendWith(ctx context.Context, chatID int64, text string, markup any) (int64, error) {
	params := map[string]any{"chat_id": chatID, "text": cutText(text), "link_preview_options": map[string]any{"is_disabled": true}}
	if markup != nil {
		params["reply_markup"] = markup
	}
	res, err := c.botCall(ctx, "sendMessage", params)
	if err != nil {
		return 0, err
	}
	var m struct {
		MessageID int64 `json:"message_id"`
	}
	err = json.Unmarshal(res, &m)
	return m.MessageID, err
}

type deliveryPayload struct {
	Locale   string `json:"locale"`
	Delivery struct {
		ID        string `json:"id"`
		Kind      string `json:"kind"`
		Recipient struct {
			Chat struct {
				Kind       string `json:"kind"`
				ExternalID string `json:"external_id"`
			} `json:"chat"`
		} `json:"recipient"`
		Body struct {
			Form    string         `json:"form"`
			Text    string         `json:"text"`
			TextKey string         `json:"text_key"`
			Params  map[string]any `json:"params"`
			Card    *card          `json:"card"`
			// The digest form (REP-2).
			Window  string      `json:"window"`
			Rows    []digestRow `json:"rows"`
			Hidden  int         `json:"hidden"`
			Sources []string    `json:"sources"`
			Labeled bool        `json:"labeled"`
		} `json:"body"`
		ReplacesExternalID string `json:"replaces_external_id"`
	} `json:"delivery"`
}

func (c *Channel) deliveries(ctx context.Context) error {
	for ctx.Err() == nil {
		if _, err := c.DeliverOnce(ctx, 30); err != nil && ctx.Err() == nil {
			c.log.Warn("delivery loop", "err", err)
			sleep(ctx, 2*time.Second)
		}
	}
	return ctx.Err()
}

// DeliverOnce claims one delivery and sends it. It reports whether there was work.
func (c *Channel) DeliverOnce(ctx context.Context, waitS int) (bool, error) {
	w, err := c.cfg.Core.Claim(ctx, []string{"delivery"}, waitS)
	if err != nil || w == nil {
		return false, err
	}
	lease := w.Lease()
	var p deliveryPayload
	if err := json.Unmarshal(w.Payload, &p); err != nil {
		return true, c.fail(ctx, lease, "input_schema_invalid", false, 0)
	}
	text, ok := c.render(p)
	if !ok {
		return true, c.fail(ctx, lease, "input_schema_invalid", false, 0)
	}
	chat, err := strconv.ParseInt(p.Delivery.Recipient.Chat.ExternalID, 10, 64)
	if err != nil {
		return true, c.fail(ctx, lease, "address_invalid", false, 0)
	}
	var markup any
	if cd := p.Delivery.Body.Card; cd != nil {
		markup = c.keyboard(p.Locale, cd)
	}
	var id int64
	if p.Delivery.Kind == "card_update" || p.Delivery.ReplacesExternalID != "" {
		// A card update, and any delivery that names the message it replaces (the
		// manager_notice of an incident, MGR-39), edits that message; no new message is sent
		// (FEN-7).
		var msg int64
		msg, err = strconv.ParseInt(p.Delivery.ReplacesExternalID, 10, 64)
		if err != nil {
			return true, c.fail(ctx, lease, "input_schema_invalid", false, 0)
		}
		id, err = msg, c.edit(ctx, chat, msg, text, markup)
	} else {
		id, err = c.sendWith(ctx, chat, text, markup)
	}
	if err != nil {
		var ae *APIError
		switch {
		case errors.As(err, &ae) && ae.Status == 429:
			return true, c.fail(ctx, lease, "provider_rate_limited", true, ae.RetryAfter)
		case errors.As(err, &ae) && ae.Status == 403:
			return true, c.fail(ctx, lease, "recipient_inactive", false, 0)
		case errors.As(err, &ae) && ae.Status >= 400 && ae.Status < 500:
			return true, c.fail(ctx, lease, "input_schema_invalid", false, 0)
		default:
			return true, c.fail(ctx, lease, "provider_unavailable", true, 0)
		}
	}
	return true, c.cfg.Core.Call(ctx, "complete", lease, map[string]any{"receipt": map[string]any{"external_id": strconv.FormatInt(id, 10)}}, nil)
}

func (c *Channel) fail(ctx context.Context, lease, reason string, retryable bool, retryAfter int) error {
	req := map[string]any{"reason": reason, "retryable": retryable}
	if retryAfter > 0 {
		req["retry_after_s"] = retryAfter
	}
	return c.cfg.Core.Call(ctx, "fail", lease, req, nil)
}

// render builds the message text. Model text (form result) gets the visible provenance line
// and loses links outside the tenant's list (INJ-11); code texts come from the catalog.
func (c *Channel) render(p deliveryPayload) (string, bool) {
	b := p.Delivery.Body
	switch b.Form {
	case "result":
		label := c.cfg.Texts.Render(p.Locale, "provenance.model", nil)
		return label + "\n" + c.cutLinks(b.Text), true
	case "text_code":
		if b.TextKey == "" {
			return "", false
		}
		return c.cfg.Texts.Render(p.Locale, b.TextKey, b.Params), true
	case "card":
		if b.Card == nil {
			return "", false
		}
		return c.renderCard(p.Locale, b.Card), true
	case "digest":
		return c.renderDigest(p.Locale, b.Window, b.Rows, b.Hidden, b.Text, b.Sources, b.Labeled), true
	}
	return "", false
}

// digestRow is a line of the code table of a digest.
type digestRow struct {
	Title string `json:"title"`
	State string `json:"state"`
	Count int    `json:"count"`
}

// renderDigest draws a digest: the code table and the hidden count from the catalog texts, then
// the model paragraph as a separate part under the provenance line, with its sources when the
// attempt saw untrusted text (REP-2). A group chat bound with model_text: labeled gets the
// paragraph of such an attempt under the code line "bot text, check the numbers against the
// table" before the sources, and still after the table.
func (c *Channel) renderDigest(locale, window string, rows []digestRow, hidden int, text string, sources []string, labeled bool) string {
	lines := []string{c.cfg.Texts.Render(locale, "digest.title", map[string]any{"window": window})}
	for _, r := range rows {
		lines = append(lines, c.cfg.Texts.Render(locale, "digest.row", map[string]any{"title": r.Title, "state": r.State, "count": r.Count}))
	}
	if len(rows) == 0 {
		lines = append(lines, c.cfg.Texts.Render(locale, "digest.empty", nil))
	}
	if hidden > 0 {
		lines = append(lines, c.cfg.Texts.Render(locale, "digest.hidden", map[string]any{"count": hidden}))
	}
	if text != "" {
		label := c.cfg.Texts.Render(locale, "provenance.model", nil)
		if sources != nil {
			label = c.cfg.Texts.Render(locale, "digest.sources", map[string]any{"sources": c.cutLinks(strings.Join(sources, ", "))})
		}
		lines = append(lines, "")
		if labeled {
			lines = append(lines, c.cfg.Texts.Render(locale, "digest.labeled", nil))
		}
		lines = append(lines, label, c.cutLinks(text))
	}
	return strings.Join(lines, "\n")
}

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
