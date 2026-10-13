// SPDX-License-Identifier: AGPL-3.0-only

// Package telegram is the E1 channel driver for the Telegram Bot API
// (spec/bindings/channels/telegram.md): getUpdates polling into submit_activity, and
// deliveries into sendMessage. v0.1 of this build serves text messages; buttons, files and
// group membership come later.
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
	"regexp"
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
}

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
	return c.cfg.Core.Call(ctx, "hello", "", map[string]any{
		"kind": "channel", "implementation": "botaru-telegram " + version.Version, "capacity": 1, "protocol_versions": []string{"v1"},
		"driver": map[string]any{"ai_disclosure": "first_contact", "provenance": "visible_label", "stores_content": "none"},
	}, nil)
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
	UpdateID int64    `json:"update_id"`
	Message  *message `json:"message"`
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
	ForwardOrigin      *struct{} `json:"forward_origin"`
	IsAutomaticForward bool      `json:"is_automatic_forward"`
	Quote              *struct{} `json:"quote"`
	ExternalReply      *struct{} `json:"external_reply"`
	Chat               struct {
		ID   int64  `json:"id"`
		Type string `json:"type"`
	} `json:"chat"`
	Text           string `json:"text"`
	Caption        string `json:"caption"`
	ReplyToMessage *struct {
		MessageID int64 `json:"message_id"`
	} `json:"reply_to_message"`
}

// PollOnce reads one batch of updates and submits them. The offset moves past an update only
// after the core answered for it (DLV-5).
func (c *Channel) PollOnce(ctx context.Context) error {
	params := map[string]any{"timeout": c.cfg.PollTimeoutS, "allowed_updates": []string{"message"}}
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
	if m.Quote != nil || m.ExternalReply != nil {
		a["quoted"] = true
	}
	return a
}

func (c *Channel) submit(ctx context.Context, u update) error {
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
	if r := []rune(text); len(r) > 4096 {
		text = string(r[:4095]) + "…"
	}
	res, err := c.botCall(ctx, "sendMessage", map[string]any{
		"chat_id": chatID, "text": text, "link_preview_options": map[string]any{"is_disabled": true},
	})
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
		} `json:"body"`
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
	id, err := c.send(ctx, chat, text)
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
	}
	return "", false
}

var linkRE = regexp.MustCompile(`(?i)\b(?:https?://|www\.)[^\s<>()]+`)

func (c *Channel) cutLinks(s string) string {
	return linkRE.ReplaceAllStringFunc(s, func(link string) string {
		u := link
		if !strings.Contains(strings.ToLower(u), "://") {
			u = "http://" + u
		}
		parsed, err := url.Parse(u)
		if err == nil {
			host := strings.ToLower(parsed.Hostname())
			for _, a := range c.cfg.LinkAllowlist {
				if host == strings.ToLower(a) {
					return link
				}
			}
		}
		return "[link removed]"
	})
}

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
