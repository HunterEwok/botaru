// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/HunterEwok/botaru/impl/internal/ids"
)

// lockedBuffer is a log sink safe for the loops that log while the test runs.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// OBS-1: the process log of the core holds no content: a canary in the task body, in a
// refused and in an accepted model answer does not appear in it, through the whole path to the
// delivery and the loops.
func TestBackfillLogWithoutContent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newBench(t)
	var out lockedBuffer
	b.c.log = slog.New(slog.NewTextHandler(&out, &slog.HandlerOptions{Level: slog.LevelDebug}))
	canary := "CANARY-" + ids.Secret(6)
	a := Activity{Kind: "message", Channel: "telegram", UpdateID: "1", Chat: Chat{Kind: "direct", ExternalID: "1001"}, Text: "/hello " + canary}
	a.Sender.Identity, a.Sender.Verified = "telegram:1001", true
	if res, err := b.c.SubmitActivity(ctx, b.channel, a); err != nil || res.Result != "task" {
		t.Fatalf("submit %+v %v", res, err)
	}
	att := b.claim(t, b.agent, "attempt")
	// A result outside the schema ends the attempt; the next one gives a valid result.
	if _, err := b.c.Complete(ctx, b.agent, header(t, att), CompleteRequest{Result: map[string]any{"answer": canary, canary: canary}}); err == nil {
		t.Fatal("a result outside the schema passed")
	}
	b.advance(t, 600)
	if att = b.claim(t, b.agent, "attempt"); att == nil {
		t.Fatal("no second attempt")
	}
	if _, err := b.c.Complete(ctx, b.agent, header(t, att), CompleteRequest{Result: map[string]any{"answer": "Hello " + canary}}); err != nil {
		t.Fatal(err)
	}
	if err := b.c.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if s := b.drain(t); len(s) == 0 {
		t.Fatal("no delivery")
	}
	if err := b.c.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	// The positive control: the sink caught the core's own lines of this path.
	if strings.Count(out.String(), "\n") == 0 {
		t.Fatal("OBS-1: the log sink caught no line of the core")
	}
	if strings.Contains(out.String(), canary) {
		t.Fatalf("OBS-1: the process log holds content:\n%s", out.String())
	}
}
