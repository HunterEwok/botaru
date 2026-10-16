// SPDX-License-Identifier: AGPL-3.0-only

package p1client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// MAN-3: the core is the main validator and a worker tolerates unknown fields: a claim answer
// with fields added in a later minor version decodes, and the known fields keep their values.
func TestBackfillUnknownFieldsTolerated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"work": {"work_id": "t1", "kind": "attempt", "lease_no": 2, "tenant": "demo",
			"lease_remaining_s": 40, "token": {"v": 1}, "payload": {}, "added_later": {"x": 1}}, "also_added": true}`))
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, Token: "t"}
	w, err := c.Claim(context.Background(), []string{"attempt"}, 0)
	if err != nil {
		t.Fatalf("MAN-3: an answer with unknown fields: %v", err)
	}
	if w == nil || w.WorkID != "t1" || w.LeaseNo != 2 || w.LeaseRemainingS != 40 {
		t.Fatalf("MAN-3: the known fields: %+v", w)
	}
}
