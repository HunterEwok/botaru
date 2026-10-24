// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/HunterEwok/botaru/impl/internal/p1client"
)

// The conversation of a chat lives in the core's session of the task (MEM-1): the agent reads
// the latest exchanges before the first model call and appends the goal and its answer before
// complete, through P1 session.load and session.append with the session handle of the lease.
// The runtime keeps nothing of it after the attempt (RT-1).
const (
	// maxConversationEntries is how many of the latest entries the model reads.
	maxConversationEntries = 20
	// maxEntryText bounds the text of one entry, read or written: the conversation goes into
	// the first message, which every call of the run sends again.
	maxEntryText = 2000
	// loadLast is how many of the latest entries one load reads, so that after the earlier
	// answers of a task are dropped maxConversationEntries are left.
	loadLast = 60
)

// sessionClient calls the session operations of P1 with the session handle as the bearer.
type sessionClient struct {
	c *p1client.Client
}

func newSessionClient(baseURL, handle string, hc *http.Client) *sessionClient {
	return &sessionClient{c: &p1client.Client{BaseURL: baseURL, Token: handle, HTTP: hc}}
}

// conversationEntry is the content of an entry this runtime writes: who spoke, the text, and
// the task the exchange belongs to.
type conversationEntry struct {
	Role string `json:"role"`
	Text string `json:"text"`
	Task string `json:"task,omitempty"`
}

// load reads the latest entries of the session, oldest first; the core leaves out what an
// attempt appended whose result it did not accept (MEM-11). Of the answers of one task only the
// last counts.
func (s *sessionClient) load(ctx context.Context) ([]conversationEntry, error) {
	var out struct {
		Entries []struct {
			Content json.RawMessage `json:"content"`
		} `json:"entries"`
	}
	if err := s.c.Call(ctx, "session.load", "", map[string]any{"last": loadLast}, &out); err != nil {
		return nil, err
	}
	var all []conversationEntry
	for _, e := range out.Entries {
		var c conversationEntry
		if json.Unmarshal(e.Content, &c) == nil && (c.Role == "user" || c.Role == "assistant") && c.Text != "" {
			c.Text = clipText(c.Text, maxEntryText)
			all = append(all, c)
		}
	}
	// The last answer of a task replaces its earlier ones.
	last := map[string]int{}
	for i, c := range all {
		if c.Role == "assistant" && c.Task != "" {
			last[c.Task] = i
		}
	}
	var kept []conversationEntry
	for i, c := range all {
		if c.Role == "assistant" && c.Task != "" && last[c.Task] != i {
			continue
		}
		kept = append(kept, c)
	}
	if len(kept) > maxConversationEntries {
		kept = kept[len(kept)-maxConversationEntries:]
	}
	return kept, nil
}

// appendExchange writes the goal of the task and the answer of this attempt. The goal entry is
// keyed by the task, so a retry does not repeat it; the answer by the task and the lease.
func (s *sessionClient) appendExchange(ctx context.Context, task string, leaseNo int, goal string, result map[string]any) error {
	// The answer text the person reads, when the result has one; the whole result otherwise.
	answer, _ := result["answer"].(string)
	if answer == "" {
		raw, err := json.Marshal(result)
		if err != nil {
			return err
		}
		answer = string(raw)
	}
	var entries []map[string]any
	if goal != "" {
		entries = append(entries, map[string]any{"uuid": entryUUID(task, "goal"), "content": conversationEntry{Role: "user", Text: clipText(goal, maxEntryText), Task: task}})
	}
	entries = append(entries, map[string]any{"uuid": entryUUID(task, fmt.Sprintf("answer-%d", leaseNo)), "content": conversationEntry{Role: "assistant", Text: clipText(answer, maxEntryText), Task: task}})
	return s.c.Call(ctx, "session.append", "", map[string]any{"entries": entries}, nil)
}

// entryUUID is a stable entry id in the uuid form the schema asks for, from the task and the
// part of the exchange.
func entryUUID(task, part string) string {
	h := sha256.Sum256([]byte(task + "\x00" + part))
	x := hex.EncodeToString(h[:16])
	return x[0:8] + "-" + x[8:12] + "-" + x[12:16] + "-" + x[16:20] + "-" + x[20:32]
}

// clipText cuts a text to n characters.
func clipText(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}
