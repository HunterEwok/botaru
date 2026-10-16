// SPDX-License-Identifier: AGPL-3.0-only

// Package kbjob is the ingest job of the knowledge corpora (kb.ingest.v1, KNW-4, KNW-5, D-97):
// a job worker without a model that reads the document of its leased task with P1 file.get,
// cuts its text into fragments and writes them with P1 kb.index_put. The core takes the corpus
// and the document from the leased task, so the job names only what its work item gives it.
package kbjob

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/HunterEwok/botaru/impl/internal/p1client"
	"github.com/HunterEwok/botaru/impl/internal/version"
)

// The fragment limits of p1-kb-index-put-request: at most 2000 fragments of 4000 characters
// with titles of 200. A fragment aims at targetRunes, so a search hit stays a short read.
const (
	maxFragments = 2000
	maxRunes     = 4000
	maxTitle     = 200
	targetRunes  = 1200
)

// Fragment is one fragment of kb.index_put.
type Fragment struct {
	N     int    `json:"n"`
	Text  string `json:"text"`
	Title string `json:"title,omitempty"`
}

// Cut splits a document into fragments in document order: paragraphs (blocks between blank
// lines) are joined up to about 1200 characters, a longer paragraph is split at a space, and a
// Markdown heading line starts a new fragment and becomes the title of the fragments under it.
// A document that is not UTF-8 text is skipped as unreadable, one without text as empty, and
// one of more than 2000 fragments as too_large; a skipped document gets no fragments.
func Cut(content []byte) ([]Fragment, string) {
	if !utf8.Valid(content) || bytes.IndexByte(content, 0) >= 0 {
		return nil, "unreadable"
	}
	text := strings.ReplaceAll(string(content), "\r\n", "\n")
	if strings.TrimSpace(text) == "" {
		return nil, "empty"
	}
	var out []Fragment
	title := ""
	var cur []string
	size := 0
	flush := func() {
		if len(cur) > 0 {
			out = append(out, Fragment{N: len(out), Text: strings.Join(cur, "\n\n"), Title: title})
		}
		cur, size = nil, 0
	}
	for _, block := range strings.Split(text, "\n\n") {
		var para []string
		for _, line := range strings.Split(block, "\n") {
			if h, ok := heading(line); ok {
				if len(para) > 0 {
					cur, size = appendPara(cur, size, strings.Join(para, "\n"), &out, title, flush)
					para = nil
				}
				flush()
				title = h
				continue
			}
			if strings.TrimSpace(line) != "" {
				para = append(para, strings.TrimRightFunc(line, unicode.IsSpace))
			}
		}
		if len(para) > 0 {
			cur, size = appendPara(cur, size, strings.Join(para, "\n"), &out, title, flush)
		}
		if len(out) > maxFragments {
			return nil, "too_large"
		}
	}
	flush()
	if len(out) > maxFragments {
		return nil, "too_large"
	}
	if len(out) == 0 {
		return nil, "empty"
	}
	return out, ""
}

// appendPara adds a paragraph to the fragment being built, flushing it first when the paragraph
// would take it past the target; a paragraph above the target alone is split into pieces.
func appendPara(cur []string, size int, para string, out *[]Fragment, title string, flush func()) ([]string, int) {
	n := utf8.RuneCountInString(para)
	if size > 0 && size+n > targetRunes {
		flush()
		cur, size = nil, 0
	}
	if n <= targetRunes {
		return append(cur, para), size + n
	}
	for _, piece := range split(para) {
		*out = append(*out, Fragment{N: len(*out), Text: piece, Title: title})
	}
	return nil, 0
}

// split cuts a long paragraph into pieces of about targetRunes characters at a space, or hard
// at maxRunes when there is none.
func split(s string) []string {
	var out []string
	r := []rune(s)
	for len(r) > targetRunes {
		cut := -1
		for i := targetRunes; i > targetRunes/2; i-- {
			if unicode.IsSpace(r[i]) {
				cut = i
				break
			}
		}
		if cut < 0 {
			cut = min(len(r), maxRunes)
		}
		out = append(out, strings.TrimSpace(string(r[:cut])))
		r = []rune(strings.TrimLeftFunc(string(r[cut:]), unicode.IsSpace))
	}
	if len(r) > 0 {
		out = append(out, string(r))
	}
	return out
}

// heading reads a Markdown heading line "# Title" (one to six #), its title cut to 200 characters.
func heading(line string) (string, bool) {
	t := strings.TrimSpace(line)
	level := 0
	for level < len(t) && t[level] == '#' {
		level++
	}
	if level == 0 || level > 6 || level >= len(t) || t[level] != ' ' {
		return "", false
	}
	h := strings.TrimSpace(t[level:])
	if r := []rune(h); len(r) > maxTitle {
		h = string(r[:maxTitle])
	}
	return h, h != ""
}

// Job is one ingest job worker.
type Job struct {
	Core *p1client.Client
	Log  *slog.Logger
}

// Hello reports the worker to the core.
func (j *Job) Hello(ctx context.Context) error {
	return j.Core.Call(ctx, "hello", "", map[string]any{
		"kind": "job", "implementation": "botaru-kbjob " + version.Version, "capacity": 1, "protocol_versions": []string{"v1"},
	}, nil)
}

// Run says hello, then claims and runs ingest attempts until ctx ends.
func (j *Job) Run(ctx context.Context) error {
	for {
		err := j.Hello(ctx)
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if r := p1client.Reason(err); r == "blocked" || r == "unauthenticated" || r == "version_unsupported" {
			return err
		}
		j.log().Warn("hello failed", "err", err)
		wait(ctx, 5*time.Second)
	}
	for ctx.Err() == nil {
		if _, err := j.Once(ctx, 30); err != nil && ctx.Err() == nil {
			j.log().Warn("ingest loop", "err", err)
			wait(ctx, 2*time.Second)
		}
	}
	return ctx.Err()
}

type payload struct {
	Task struct {
		ID   string `json:"id"`
		Type string `json:"type"`
	} `json:"task"`
	// KBDocument is the extension field of the core: the document of the ingest task.
	KBDocument *struct {
		ID      string `json:"id"`
		Corpus  string `json:"corpus"`
		FileRef string `json:"file_ref"`
	} `json:"kb_document"`
}

// Once claims one attempt and runs it; it reports whether there was work. The log lines carry
// ids and counts only, never document text.
func (j *Job) Once(ctx context.Context, waitS int) (bool, error) {
	w, err := j.Core.Claim(ctx, []string{"attempt"}, waitS)
	if err != nil || w == nil {
		return false, err
	}
	lease := w.Lease()
	var p payload
	if err := json.Unmarshal(w.Payload, &p); err != nil || p.Task.Type != "kb.ingest.v1" || p.KBDocument == nil {
		// Not an ingest of a live document: nothing this job can do.
		return true, j.Core.Call(ctx, "fail", lease, map[string]any{"reason": "runtime_error", "retryable": false}, nil)
	}
	var content []byte
	err = j.Core.Call(ctx, "file.get", lease, map[string]any{"file_ref": p.KBDocument.FileRef}, &content)
	if r := p1client.Reason(err); r == "gone" || r == "not_found" {
		return true, j.Core.Call(ctx, "fail", lease, map[string]any{"reason": "runtime_error", "retryable": false}, nil)
	}
	if err != nil {
		return true, j.Core.Call(ctx, "fail", lease, map[string]any{"reason": "runtime_error", "retryable": true}, nil)
	}
	frags, skipped := Cut(content)
	if frags == nil {
		frags = []Fragment{}
	}
	err = j.Core.Call(ctx, "kb.index_put", lease, map[string]any{
		"corpus": p.KBDocument.Corpus, "file": p.KBDocument.ID, "fragments": frags,
	}, nil)
	if err != nil {
		var pe *p1client.Error
		retry := !errors.As(err, &pe) || pe.Reason != "forbidden"
		j.log().Warn("index_put refused", "task", p.Task.ID, "reason", p1client.Reason(err))
		return true, j.Core.Call(ctx, "fail", lease, map[string]any{"reason": "runtime_error", "retryable": retry}, nil)
	}
	result := map[string]any{"fragments": len(frags)}
	if skipped != "" {
		result["skipped"] = skipped
	}
	j.log().Info("document indexed", "task", p.Task.ID, "document", p.KBDocument.ID, "fragments", len(frags))
	return true, j.Core.Call(ctx, "complete", lease, map[string]any{"result": result}, nil)
}

func (j *Job) log() *slog.Logger {
	if j.Log == nil {
		return slog.Default()
	}
	return j.Log
}

func wait(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
