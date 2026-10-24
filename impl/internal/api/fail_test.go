// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// A value the store refuses gets 400, so a channel does not submit it again and again; any other
// store failure stays not_ready, which the caller retries (DLV-5, OBS-8).
func TestFailStoreRefusal(t *testing.T) {
	s := &Server{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	for _, tc := range []struct {
		err  error
		want int
	}{{&pgconn.PgError{Code: "22P05"}, 400}, {&pgconn.PgError{Code: "22021"}, 400}, {&pgconn.PgError{Code: "40001"}, 503}, {errors.New("conn reset"), 503}} {
		w := httptest.NewRecorder()
		s.fail(w, tc.err)
		if w.Code != tc.want {
			t.Errorf("%v: %d, want %d", tc.err, w.Code, tc.want)
		}
	}
}
