// SPDX-License-Identifier: AGPL-3.0-only

// Package toolhost is the E3 tool execution adapter of this implementation: a worker of kind
// toolhost that takes tool calls through P1 claim(toolcall), runs each call in a fresh child
// process of its plugin over MCP on the standard streams, masks and cuts the output and hands
// it in. It has no incoming port and no path to the store (DEP-6, DEP-8). It measures the call
// lease by its own monotonic clock and stops the plugin when the lease is lost (FEN-6).
package toolhost

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/HunterEwok/botaru/impl/internal/jcs"
	"github.com/HunterEwok/botaru/impl/internal/mcp"
	"github.com/HunterEwok/botaru/impl/internal/p1client"
	"github.com/HunterEwok/botaru/impl/internal/redact"
	"github.com/HunterEwok/botaru/impl/internal/toolspec"
	"github.com/HunterEwok/botaru/impl/internal/version"
)

// Plugin is how the toolhost starts one plugin: the program and its arguments, run without a
// shell and with only the listed environment.
type Plugin struct {
	Name    string   `yaml:"name"`
	Command []string `yaml:"command"`
	Env     []string `yaml:"env"`
}

// Config is the toolhost's settings.
type Config struct {
	Core      *p1client.Client
	Placement string
	Plugins   []Plugin
	// KillAfter is how long a plugin has after a cancellation before its process is ended.
	KillAfter time.Duration
	// StopMargin is how long before the end of the lease the toolhost stops a call; the plugin
	// is cancelled StopMargin plus KillAfter before it, so it is gone before the lease ends
	// (FEN-6).
	StopMargin time.Duration
	Logger     *slog.Logger
}

// Host runs calls one at a time.
type Host struct {
	cfg        Config
	heartbeatS int
	log        *slog.Logger
	byName     map[string]Plugin
	// mu guards fps and down: the fingerprint of each plugin's last answered tools/list and
	// the plugins whose last check failed, which hello and heartbeat report (OBS-10).
	mu   sync.Mutex
	fps  map[string]string
	down map[string]bool
}

// New returns a toolhost.
func New(cfg Config) (*Host, error) {
	if cfg.KillAfter < 0 || cfg.StopMargin < 0 {
		return nil, errors.New("toolhost: kill_after and the stop margin cannot be negative")
	}
	if cfg.KillAfter == 0 {
		cfg.KillAfter = 10 * time.Second
	}
	if cfg.StopMargin == 0 {
		cfg.StopMargin = 10 * time.Second
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	by := map[string]Plugin{}
	for _, p := range cfg.Plugins {
		if p.Name == "" || len(p.Command) == 0 {
			return nil, errors.New("toolhost: a plugin needs a name and a command")
		}
		if _, dup := by[p.Name]; dup {
			return nil, fmt.Errorf("toolhost: plugin %s twice", p.Name)
		}
		by[p.Name] = p
	}
	return &Host{cfg: cfg, heartbeatS: 20, log: cfg.Logger, byName: by, fps: map[string]string{}, down: map[string]bool{}}, nil
}

// maxMessage bounds one message from a plugin.
const maxMessage = 8 << 20

// Fingerprints asks every plugin for its live tools/list and returns the fingerprint of each
// (spec/hashing.md) and the combined one that hello carries.
func (h *Host) Fingerprints(ctx context.Context) (map[string]string, string, error) {
	by := map[string]string{}
	for name, p := range h.byName {
		fp, err := h.fingerprint(ctx, p)
		if err != nil {
			return nil, "", fmt.Errorf("plugin %s: %w", name, err)
		}
		by[name] = fp
	}
	all, err := toolspec.CombinedFingerprint(by)
	return by, all, err
}

// fingerprint is the fingerprint of one plugin's live tools/list.
func (h *Host) fingerprint(ctx context.Context, p Plugin) (string, error) {
	var res mcp.ListResult
	err := h.session(ctx, p, func(s *session) error {
		raw, err := s.request(ctx, "tools/list", mcp.ListParams{Meta: mcp.ClientMeta("botaru-toolhost", version.Version)})
		if err != nil {
			return err
		}
		return json.Unmarshal(raw, &res)
	})
	if err != nil {
		return "", err
	}
	return toolspec.Fingerprint(res.Tools)
}

// check asks every plugin for its live tools/list: a plugin that answers is up with its
// fingerprint, one that does not is down and keeps the fingerprint of its last answer. The
// combined fingerprint needs every plugin to have answered once.
// probeTimeout bounds one plugin's answer to tools/list in a health check.
const probeTimeout = 20 * time.Second

func (h *Host) check(ctx context.Context) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for name, p := range h.byName {
		// A plugin that never answers is down after the probe timeout, so one stuck plugin
		// does not stop the hello and the work of the others (OBS-10).
		pctx, cancel := context.WithTimeout(ctx, probeTimeout)
		fp, err := h.fingerprint(pctx, p)
		cancel()
		if err != nil {
			if _, seen := h.fps[name]; !seen {
				return "", fmt.Errorf("plugin %s: %w", name, err)
			}
			h.down[name] = true
			continue
		}
		h.fps[name], h.down[name] = fp, false
	}
	return toolspec.CombinedFingerprint(h.fps)
}

// health is the health report of every plugin by its last check (OBS-10).
func (h *Host) health() []map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	at := time.Now().UTC().Format("2006-01-02T15:04:05.000000Z")
	out := make([]map[string]any, 0, len(h.down))
	for name, down := range h.down {
		state := "ok"
		if down {
			state = "down"
		}
		out = append(out, map[string]any{"component": "plugin", "name": name, "state": state, "time": at})
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["name"].(string) < out[j]["name"].(string) })
	return out
}

// Hello reports the toolhost with its plugins, placement, the live fingerprint (INJ-2) and the
// health of each plugin (OBS-10). Run says hello again every heartbeat interval, so a plugin
// that stops or comes back narrows or widens dispatch.
func (h *Host) Hello(ctx context.Context) error {
	fp, err := h.check(ctx)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(h.byName))
	for n := range h.byName {
		names = append(names, n)
	}
	sort.Strings(names)
	var out struct {
		HeartbeatS int `json:"heartbeat_s"`
	}
	err = h.cfg.Core.Call(ctx, "hello", "", map[string]any{
		"kind": "toolhost", "implementation": "botaru-toolhost " + version.Version, "capacity": 1, "protocol_versions": []string{"v1"},
		"plugins": names, "placement": h.cfg.Placement, "tools_fingerprint": fp, "health": h.health(),
	}, &out)
	if err == nil && out.HeartbeatS > 0 {
		h.heartbeatS = out.HeartbeatS
	}
	return err
}

// Run says hello and then claims and runs calls until ctx ends.
func (h *Host) Run(ctx context.Context) error {
	for {
		err := h.Hello(ctx)
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		h.log.Warn("hello failed", "err", err)
		if r := p1client.Reason(err); r == "blocked" || r == "unauthenticated" || r == "version_unsupported" {
			return err
		}
		sleep(ctx, 5*time.Second)
	}
	hello := time.Now()
	for ctx.Err() == nil {
		if every := time.Duration(h.heartbeatS) * time.Second; time.Since(hello) >= every {
			if err := h.Hello(ctx); err != nil && ctx.Err() == nil {
				h.log.Warn("hello failed", "err", err)
			}
			hello = time.Now()
		}
		if _, err := h.Once(ctx, min(30, h.heartbeatS)); err != nil && ctx.Err() == nil {
			h.log.Warn("call loop", "err", err)
			sleep(ctx, 2*time.Second)
		}
	}
	return ctx.Err()
}

type callPayload struct {
	Target struct {
		Plugin string `json:"plugin"`
	} `json:"target"`
	Tool     string            `json:"tool"`
	Args     map[string]any    `json:"args"`
	Resolved map[string]string `json:"resolved"`
	// ApprovalID, GrantID and ArgsHash are set on the write call of an action (APR-7).
	ApprovalID string `json:"approval_id"`
	GrantID    string `json:"grant_id"`
	ArgsHash   string `json:"args_hash"`
	// CampaignItem is the item number of a campaign write call: the plugin key is
	// <approval_id>:<n> (ORC-3).
	CampaignItem int `json:"campaign_item"`
	Limits       struct {
		Redact           []string `json:"redact"`
		OutputLimitBytes int      `json:"output_limit_bytes"`
		TimeoutS         int      `json:"timeout_s"`
	} `json:"limits"`
}

// Once claims one call and runs it. It reports whether there was work.
func (h *Host) Once(ctx context.Context, waitS int) (bool, error) {
	w, err := h.cfg.Core.Claim(ctx, []string{"toolcall"}, waitS)
	if err != nil || w == nil {
		return false, err
	}
	// The lease is counted by this process's monotonic clock from the remaining seconds, not
	// from the absolute time (FEN-6, TIM-1).
	until := time.Now().Add(time.Duration(w.LeaseRemainingS) * time.Second)
	lease := w.Lease()
	var p callPayload
	if err := json.Unmarshal(w.Payload, &p); err != nil {
		return true, h.fail(ctx, lease, "input_schema_invalid")
	}
	plugin, ok := h.byName[p.Target.Plugin]
	if !ok {
		return true, h.fail(ctx, lease, "plugin_unavailable")
	}
	// A write call runs only with its approval or grant and with the arguments that were
	// approved: their hash must equal the approved one (APR-7).
	if p.ArgsHash != "" || p.ApprovalID != "" || p.GrantID != "" {
		got, err := jcs.Hash(p.Args)
		if p.ArgsHash == "" || p.ApprovalID == "" && p.GrantID == "" || err != nil || got != p.ArgsHash {
			h.log.Error("a write call without a matching approval was refused", "work", w.WorkID)
			return true, h.fail(ctx, lease, "exec_failed")
		}
	}
	// A lease no longer than the stop margin plus kill_after leaves the call no time at all: it
	// fails at once instead of being cancelled at start and left to expire.
	if margin := h.cfg.StopMargin + h.cfg.KillAfter; time.Duration(w.LeaseRemainingS)*time.Second <= margin {
		h.log.Error("the lease is too short for the stop margin and kill_after", "lease_s", w.LeaseRemainingS, "margin", margin)
		return true, h.fail(ctx, lease, "exec_failed")
	}
	timeout := 20 * time.Second
	if p.Limits.TimeoutS > 0 {
		timeout = time.Duration(p.Limits.TimeoutS) * time.Second
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	lost := make(chan struct{})
	go h.keepLease(callCtx, cancel, lease, until, lost)

	res, err := h.call(callCtx, plugin, w.WorkID, p)
	select {
	case <-lost:
		// The lease is gone: nothing more is written under it (FEN-6).
		h.log.Warn("call stopped: the lease was lost", "work", w.WorkID)
		return true, nil
	default:
	}
	if err != nil {
		// A plugin's error text is its own output: it is masked like stderr before the log.
		h.log.Warn("call failed", "work", w.WorkID, "plugin", plugin.Name,
			"err", redact.Text(err.Error(), []string{redact.IP, redact.Email, redact.Secret}))
		return true, h.fail(ctx, lease, "exec_failed")
	}
	out := clean(res, p.Limits.Redact, p.Limits.OutputLimitBytes)
	var m map[string]any
	b, _ := json.Marshal(out)
	_ = json.Unmarshal(b, &m)
	return true, h.cfg.Core.Call(ctx, "complete", lease, map[string]any{"output": m}, nil)
}

func (h *Host) fail(ctx context.Context, lease, reason string) error {
	return h.cfg.Core.Call(ctx, "fail", lease, map[string]any{"reason": reason, "retryable": false}, nil)
}

// keepLease renews the call lease; when the core answers that the call is canceled, or the
// lease ran out by this process's clock without a renewal, it cancels the call (FEN-4, FEN-6).
func (h *Host) keepLease(ctx context.Context, cancel context.CancelFunc, lease string, until time.Time, lost chan struct{}) {
	every := time.Duration(h.heartbeatS) * time.Second
	margin := h.cfg.StopMargin + h.cfg.KillAfter
	for {
		cutoff := until.Add(-margin)
		wait := min(every, time.Until(cutoff))
		t := time.NewTimer(max(wait, 0))
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		if !time.Now().Before(cutoff) {
			close(lost)
			cancel()
			return
		}
		var out struct {
			LeaseRemainingS int  `json:"lease_remaining_s"`
			CancelRequested bool `json:"cancel_requested"`
		}
		sent := time.Now()
		// The heartbeat ends at the local cutoff: a stalled request never keeps the call
		// running past it.
		hctx, hcancel := context.WithDeadline(ctx, cutoff)
		err := h.cfg.Core.Call(hctx, "heartbeat", lease, map[string]any{"health": h.health()}, &out)
		hcancel()
		if r := p1client.Reason(err); r == "fenced" || r == "lease_mismatch" || out.CancelRequested {
			close(lost)
			cancel()
			return
		}
		if err == nil {
			until = sent.Add(time.Duration(out.LeaseRemainingS) * time.Second)
		}
	}
}

// clean masks the output by the tool's redaction kinds and cuts it at the tool limit (INJ-4).
func clean(res *mcp.CallResult, kinds []string, limit int) *mcp.CallResult {
	if limit <= 0 {
		limit = 32 << 10
	}
	out := &mcp.CallResult{IsError: res.IsError, InputRequired: res.InputRequired, Content: []mcp.Content{}}
	// The text blocks are joined before masking, so nothing split across blocks escapes it.
	var joined strings.Builder
	for _, c := range res.Content {
		if c.Type == "text" {
			joined.WriteString(c.Text)
		}
	}
	if text, _ := redact.Cut(redact.Text(joined.String(), kinds), limit); text != "" {
		out.Content = append(out.Content, mcp.Content{Type: "text", Text: text})
	}
	if res.StructuredContent != nil {
		sc, _ := redact.Value(res.StructuredContent, kinds).(map[string]any)
		out.StructuredContent = sc
	}
	return out
}

// callWorkID is the work id a plugin recognizes a repeated write by, shared by all attempts:
// the approval id, <approval_id>:<n> for a campaign item, the service_grant record of a
// service or script call, otherwise the id of the call (APR-7, FEN-7).
func callWorkID(workID string, c callPayload) string {
	if c.ApprovalID != "" {
		if c.CampaignItem > 0 {
			return c.ApprovalID + ":" + strconv.Itoa(c.CampaignItem)
		}
		return c.ApprovalID
	}
	if c.GrantID != "" {
		return c.GrantID
	}
	return workID
}

// call runs one tool call in a new plugin process.
func (h *Host) call(ctx context.Context, p Plugin, workID string, c callPayload) (*mcp.CallResult, error) {
	var res mcp.CallResult
	err := h.session(ctx, p, func(s *session) error {
		meta := mcp.ClientMeta("botaru-toolhost", version.Version)
		meta.Resolved = c.Resolved
		meta.WorkID = callWorkID(workID, c)
		raw, err := s.request(ctx, "tools/call", mcp.CallParams{Name: c.Tool, Arguments: c.Args, Meta: meta})
		if err != nil {
			return err
		}
		return json.Unmarshal(raw, &res)
	})
	if err != nil {
		return nil, err
	}
	if res.InputRequired {
		return nil, errors.New("the plugin asked for input (INJ-5)")
	}
	return &res, nil
}

// session is one plugin process with its MCP connection.
type session struct {
	conn   *mcp.Conn
	nextID atomic.Int64
	log    *slog.Logger
	msgs   chan mcp.Message
	errs   chan error
}

// session starts a plugin process, runs fn and ends the process. When ctx ends first, the
// plugin gets notifications/cancelled and, after KillAfter, its process is killed (FEN-6).
func (h *Host) session(ctx context.Context, p Plugin, fn func(*session) error) error {
	cmd := exec.Command(p.Command[0], p.Command[1:]...)
	cmd.Env = p.Env
	if cmd.Env == nil {
		cmd.Env = []string{}
	}
	cmd.Dir = "/"
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &limited{w: &stderr, n: 64 << 10}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start: %w", err)
	}
	s := &session{conn: mcp.NewConn(stdout, stdin), log: h.log, msgs: make(chan mcp.Message, 4), errs: make(chan error, 1)}
	go s.read()
	done := make(chan error, 1)
	go func() { done <- fn(s) }()
	var ferr error
	select {
	case ferr = <-done:
	case <-ctx.Done():
		_ = s.conn.Send(mcp.Message{Method: "notifications/cancelled", Params: mustJSON(mcp.CancelParams{RequestID: json.RawMessage(fmt.Sprint(s.nextID.Load())), Reason: "lease or deadline"})})
		select {
		case ferr = <-done:
		case <-time.After(h.cfg.KillAfter):
			ferr = ctx.Err()
		}
		if ferr == nil {
			ferr = ctx.Err()
		}
	}
	_ = stdin.Close()
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	select {
	case <-exited:
	case <-time.After(h.cfg.KillAfter):
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-exited
	}
	// Nothing a plugin started outlives its call: the rest of its process group is killed
	// even after the plugin itself exited on time.
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	if ferr != nil && stderr.Len() > 0 {
		h.log.Warn("plugin stderr", "plugin", p.Name, "text", redact.Text(stderr.String(), []string{redact.IP, redact.Email, redact.Secret}))
	}
	return ferr
}

func (s *session) read() {
	for {
		m, err := s.conn.Receive(maxMessage)
		if err != nil {
			s.errs <- err
			close(s.msgs)
			return
		}
		s.msgs <- m
	}
}

// request sends a request and waits for its answer. A request from the plugin (sampling,
// elicitation, roots) is refused, and the call ends with an error (INJ-5).
func (s *session) request(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := json.RawMessage(fmt.Sprint(s.nextID.Add(1)))
	if err := s.conn.Send(mcp.Message{ID: id, Method: method, Params: mustJSON(params)}); err != nil {
		return nil, err
	}
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case m, ok := <-s.msgs:
			if !ok {
				err := <-s.errs
				if errors.Is(err, io.EOF) {
					err = errors.New("the plugin closed its output")
				}
				return nil, err
			}
			if m.Method != "" {
				if len(m.ID) > 0 {
					_ = s.conn.Send(mcp.ErrorResponse(m.ID, mcp.MethodNotFound, "the toolhost declares no client capabilities"))
					return nil, fmt.Errorf("the plugin sent the request %q (INJ-5)", m.Method)
				}
				continue // notifications from the plugin are ignored
			}
			if string(m.ID) != string(id) {
				continue
			}
			if m.Error != nil {
				return nil, m.Error
			}
			return m.Result, nil
		}
	}
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

type limited struct {
	w  io.Writer
	n  int
	mu sync.Mutex
}

func (l *limited) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := len(p)
	if l.n <= 0 {
		return n, nil
	}
	if len(p) > l.n {
		p = p[:l.n]
	}
	l.n -= len(p)
	_, err := l.w.Write(p)
	return n, err
}

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
