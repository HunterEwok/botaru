// SPDX-License-Identifier: AGPL-3.0-only

// Package httpbound bounds how long a client may take to send its request. http.Server's
// ReadTimeout would also end a long poll of claim or a model call through P4 while the
// handler still runs, so the deadline covers the request body only: it is cleared once the
// handler has read the body to its end.
package httpbound

import (
	"errors"
	"io"
	"net/http"
	"time"
)

// Server is an http.Server whose clients cannot hold a connection without sending: a bounded
// header, a body that must arrive within body, and an idle connection closed after idle.
func Server(addr string, h http.Handler, body, idle time.Duration) *http.Server {
	return &http.Server{Addr: addr, Handler: Body(h, body), ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout: idle, MaxHeaderBytes: 64 << 10}
}

// Body gives every request of h until d to deliver its whole body.
func Body(h http.Handler, d time.Duration) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rc := http.NewResponseController(w)
		if err := rc.SetReadDeadline(time.Now().Add(d)); err == nil {
			r.Body = &body{ReadCloser: r.Body, rc: rc}
		}
		h.ServeHTTP(w, r)
	})
}

type body struct {
	io.ReadCloser
	rc   *http.ResponseController
	done bool
}

func (b *body) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if errors.Is(err, io.EOF) && !b.done {
		b.done = true
		_ = b.rc.SetReadDeadline(time.Time{})
	}
	return n, err
}
