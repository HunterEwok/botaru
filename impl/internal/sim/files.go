// SPDX-License-Identifier: AGPL-3.0-only

package sim

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/HunterEwok/botaru/impl/internal/p1client"
	"github.com/HunterEwok/botaru/impl/internal/version"
)

// The files people send. The Telegram channel of the reference takes no files yet, so a message
// with a file goes in through a channel worker of the stand's own in the person's tenant: it puts
// the file on the person's behalf (P1 file.put) and submits the message with the file attached,
// as the channel would for a document. It never claims a delivery, so the bot's answer goes out
// through the Telegram channel as usual.

// fileUpdates numbers the updates of the stand's file channel apart from the fake Bot API's.
var fileUpdates atomic.Int64

const fileUpdateBase = 900_000_000

// fileChannel is the stand's file channel worker of a tenant, enrolled on first use.
func (s *Stand) fileChannel(ctx context.Context, tenant string) (*p1client.Client, error) {
	s.mu.Lock()
	c := s.fileChans[tenant]
	s.mu.Unlock()
	if c != nil {
		return c, nil
	}
	tok, err := s.Bench.Enroll(ctx, map[string]any{"tenant": tenant, "role": "telegram", "kind": "channel", "target": "telegram"})
	if err != nil {
		return nil, err
	}
	c = &p1client.Client{BaseURL: s.Bench.P1(), Token: tok, HTTP: &http.Client{Timeout: time.Minute}}
	if err := c.Call(ctx, "hello", "", map[string]any{"kind": "channel", "implementation": "botaru-sim " + version.Version, "capacity": 1,
		"protocol_versions": []string{"v1"}, "driver": map[string]any{"ai_disclosure": "first_contact", "provenance": "visible_label", "stores_content": "none"}}, nil); err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.fileChans[tenant] = c
	s.mu.Unlock()
	return c, nil
}

// SayWithFile sends a person's message in a chat with a data file of their tenant's world
// attached (a CTD profile such as ctd-st-05.csv) and returns the idempotency key of the task it
// makes.
func (s *Stand) SayWithFile(ctx context.Context, who, chat, text, file string) (string, error) {
	p, ch := s.seed.Person(who), s.seed.Chat(chat)
	if p == nil || ch == nil || p.Identity == "" {
		return "", fmt.Errorf("no seed person %s with an identity or chat %s", who, chat)
	}
	if file == "" || strings.ContainsAny(file, `/\`) {
		return "", fmt.Errorf("bad file name %q", file)
	}
	content, err := os.ReadFile(filepath.Join(dataDir(s.cfg.Work, p.Tenant), file))
	if err != nil {
		return "", err
	}
	c, err := s.fileChannel(ctx, p.Tenant)
	if err != nil {
		return "", err
	}
	headers := map[string]string{"Botaru-On-Behalf": p.Identity, "Botaru-Chat": ch.Kind + ":" + ch.ExternalID}
	ref, err := c.FilePut(ctx, headers, map[string]any{"filename": file, "media_type": "text/plain"}, content)
	if err != nil {
		return "", fmt.Errorf("file.put: %w", err)
	}
	id := strconv.FormatInt(fileUpdateBase+fileUpdates.Add(1), 10)
	a := map[string]any{"kind": "message", "channel": "telegram", "update_id": id,
		"chat":        map[string]any{"kind": ch.Kind, "external_id": ch.ExternalID},
		"received_at": time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
		"sender":      map[string]any{"identity": p.Identity, "verified": true}, "text": text, "attachments": []string{ref}}
	s.log.Add(KindChatIn, fmt.Sprintf("%s in %s: %s [file %s]", who, chat, text, file), map[string]any{"person": who, "chat": chat, "text": text, "file": file})
	if err := c.Call(ctx, "submit_activity", "", a, nil); err != nil {
		return "", fmt.Errorf("submit_activity: %w", err)
	}
	return "system:activity:telegram:" + id, nil
}

// UploadKnowledge uploads a document to an ingested corpus of a tenant as its decider, a staff
// member with kb.write (P2 kb.upload, KNW-4), and returns the document id; a kb.ingest.v1 job
// of data-job then cuts it into fragments.
func (s *Stand) UploadKnowledge(ctx context.Context, tenant, corpus, filename string, content []byte) (string, error) {
	var out struct {
		Document string `json:"document"`
	}
	err := s.Bench.P2As(ctx, s.PersonIn(tenant, Decider), "kb.upload", map[string]any{"tenant": tenant, "corpus": corpus, "filename": filename,
		"content": base64.StdEncoding.EncodeToString(content)}, &out)
	return out.Document, err
}

// deciders are the P2 persons of the bench: the decider of every tenant of the farm.
func deciders(f *Farm) []string {
	var out []string
	for _, t := range f.Tenants {
		if t.Name == f.Home().Name {
			out = append(out, Decider)
		} else {
			out = append(out, t.Name+"-"+Decider)
		}
	}
	return out
}
