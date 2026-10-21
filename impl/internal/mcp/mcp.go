// SPDX-License-Identifier: AGPL-3.0-only

// Package mcp is the part of the Model Context Protocol this implementation uses
// (spec/bindings/mcp/README.md): JSON-RPC 2.0 messages, the sessionless server discovery,
// tools/list, tools/call and cancellation, over HTTP on the north side of P3 and over the
// standard streams of a child process between the toolhost and a plugin. Sampling,
// elicitation and roots are never declared.
package mcp

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// Revision is the protocol revision of the binding.
const Revision = "2026-07-28"

// Message is a JSON-RPC 2.0 request, notification or response.
type Message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// Error is a JSON-RPC error.
type Error struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *Error) Error() string { return fmt.Sprintf("mcp error %d: %s", e.Code, e.Message) }

// JSON-RPC error codes.
const (
	ParseError     = -32700
	InvalidRequest = -32600
	MethodNotFound = -32601
	InvalidParams  = -32602
	InternalError  = -32603
)

// Meta is the _meta of a request: the client version and capabilities and the trace context
// (spec/bindings/mcp/README.md). It decides nothing.
type Meta struct {
	ProtocolVersion string         `json:"protocolVersion,omitempty"`
	ClientInfo      map[string]any `json:"clientInfo,omitempty"`
	Capabilities    map[string]any `json:"capabilities"`
	Traceparent     string         `json:"traceparent,omitempty"`
	// Resolved carries catalog fields of the arguments that a plugin needs, such as the unit
	// of a service (implementation extension, see impl/ARCHITECTURE.md).
	Resolved map[string]string `json:"botaru/resolved,omitempty"`
	// WorkID is the id of the call, by which a plugin recognizes a repeated write (FEN-7).
	WorkID string `json:"botaru/work_id,omitempty"`
	// Files maps each attachment of the task that the arguments name (a file_ref) to the path
	// of a private copy the toolhost made for this call; the copy is removed after the call
	// (implementation extension, see impl/ARCHITECTURE.md).
	Files map[string]string `json:"botaru/files,omitempty"`
}

// CallParams are the params of tools/call.
type CallParams struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
	Meta      *Meta          `json:"_meta,omitempty"`
}

// ListParams are the params of tools/list and server/discover.
type ListParams struct {
	Meta *Meta `json:"_meta,omitempty"`
}

// CancelParams are the params of notifications/cancelled.
type CancelParams struct {
	RequestID json.RawMessage `json:"requestId"`
	Reason    string          `json:"reason,omitempty"`
}

// Content is a content block of a tool result.
type Content struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// CallResult is the result of tools/call.
type CallResult struct {
	Content           []Content      `json:"content"`
	StructuredContent map[string]any `json:"structuredContent,omitempty"`
	IsError           bool           `json:"isError,omitempty"`
	// InputRequired marks an "input needed" answer, which the farm treats as a call error
	// (INJ-5).
	InputRequired bool `json:"inputRequired,omitempty"`
}

// ListResult is the result of tools/list.
type ListResult struct {
	Tools []any `json:"tools"`
}

// DiscoverResult is the result of server/discover.
type DiscoverResult struct {
	ProtocolVersion string         `json:"protocolVersion"`
	Capabilities    map[string]any `json:"capabilities"`
	ServerInfo      map[string]any `json:"serverInfo"`
	Profile         string         `json:"profile,omitempty"`
}

// ClientMeta is the _meta of this implementation's client: no sampling, elicitation or roots.
func ClientMeta(name, version string) *Meta {
	return &Meta{ProtocolVersion: Revision, ClientInfo: map[string]any{"name": name, "version": version}, Capabilities: map[string]any{}}
}

// Response builds a response to a request id.
func Response(id json.RawMessage, result any) Message {
	b, err := json.Marshal(result)
	if err != nil {
		return ErrorResponse(id, InternalError, "result could not be encoded")
	}
	return Message{JSONRPC: "2.0", ID: id, Result: b}
}

// ErrorResponse builds an error response.
func ErrorResponse(id json.RawMessage, code int, msg string) Message {
	return Message{JSONRPC: "2.0", ID: id, Error: &Error{Code: code, Message: msg}}
}

// Conn is a JSON-RPC connection over a stream of newline-delimited messages, as the stdio
// transport of the protocol has it.
type Conn struct {
	r  *bufio.Reader
	w  io.Writer
	mu sync.Mutex
}

// NewConn wraps a stream pair.
func NewConn(r io.Reader, w io.Writer) *Conn {
	return &Conn{r: bufio.NewReaderSize(r, 64<<10), w: w}
}

// Send writes one message.
func (c *Conn) Send(m Message) error {
	m.JSONRPC = "2.0"
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_, err = c.w.Write(append(b, '\n'))
	return err
}

// ErrTooLong is returned for a message longer than the limit.
var ErrTooLong = errors.New("mcp: message too long")

// Receive reads one message of at most limit bytes.
func (c *Conn) Receive(limit int) (Message, error) {
	var m Message
	var line []byte
	for {
		part, isPrefix, err := c.r.ReadLine()
		if err != nil {
			return m, err
		}
		line = append(line, part...)
		if len(line) > limit {
			return m, ErrTooLong
		}
		if !isPrefix {
			break
		}
	}
	if err := json.Unmarshal(line, &m); err != nil {
		return m, fmt.Errorf("mcp: %w", err)
	}
	return m, nil
}
