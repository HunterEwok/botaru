// SPDX-License-Identifier: AGPL-3.0-only

package sim

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"sync"
	"time"
)

// Kinds of the simulator's event lines.
const (
	KindWorld    = "WORLD"
	KindChatIn   = "CHAT -> BOT"
	KindChatOut  = "BOT -> CHAT"
	KindTool     = "P3 TOOL"
	KindModel    = "P4 MODEL"
	KindCard     = "CARD"
	KindMonitor  = "MONITOR"
	KindDirector = "DIRECTOR"
	KindCheck    = "CHECK"
	KindFuse     = "FUSE"
)

// Line is one event of a run: printed to the console and written as a JSON line to
// sim-events.jsonl.
type Line struct {
	Time time.Time      `json:"time"`
	Kind string         `json:"kind"`
	Text string         `json:"text"`
	Data map[string]any `json:"data,omitempty"`
}

// Log writes the event lines of a run. It is safe for concurrent use.
type Log struct {
	mu      sync.Mutex
	console io.Writer
	file    *os.File
	enc     *json.Encoder
	// quiet hides the kinds it names from the console; the file keeps every line.
	quiet map[string]bool
	// subs see each line, for the checks of the director.
	subs []sub
	next int
}

// NewLog writes to the console and to the file at path ("" for none).
func NewLog(console io.Writer, path string) (*Log, error) {
	l := &Log{console: console, quiet: map[string]bool{}}
	if path != "" {
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
		if err != nil {
			return nil, err
		}
		l.file, l.enc = f, json.NewEncoder(f)
		l.enc.SetEscapeHTML(false)
	}
	return l, nil
}

// Quiet hides a kind from the console.
func (l *Log) Quiet(kind string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.quiet[kind] = true
}

// Subscribe adds a function that sees each later line; the returned function removes it.
func (l *Log) Subscribe(f func(Line)) func() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.next++
	id := l.next
	l.subs = append(l.subs, sub{id, f})
	return func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.subs = slices.DeleteFunc(l.subs, func(s sub) bool { return s.id == id })
	}
}

type sub struct {
	id int
	f  func(Line)
}

// Add writes a line.
func (l *Log) Add(kind, text string, data map[string]any) {
	ln := Line{Time: time.Now().UTC(), Kind: kind, Text: text, Data: data}
	l.mu.Lock()
	subs := slices.Clone(l.subs)
	if l.console != nil && !l.quiet[kind] {
		prefix := fmt.Sprintf("%s [%s] ", ln.Time.Local().Format("15:04:05"), kind)
		pad := strings.Repeat(" ", len(prefix))
		fmt.Fprintln(l.console, prefix+strings.ReplaceAll(strings.TrimRight(text, "\n"), "\n", "\n"+pad))
	}
	if l.enc != nil {
		_ = l.enc.Encode(ln)
	}
	l.mu.Unlock()
	for _, sb := range subs {
		f := sb.f
		f(ln)
	}
}

// Addf writes a formatted line without data.
func (l *Log) Addf(kind, format string, args ...any) { l.Add(kind, fmt.Sprintf(format, args...), nil) }

// Close closes the file.
func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil
	}
	err := l.file.Close()
	l.file, l.enc = nil, nil
	return err
}

// clip shortens a text for the console.
func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}
