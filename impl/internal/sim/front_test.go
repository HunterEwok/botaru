// SPDX-License-Identifier: AGPL-3.0-only

package sim

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestFrontAnswers counts the provider's own answers to model calls only: not other requests,
// and not a provider that cannot be reached.
func TestFrontAnswers(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	p, err := newProviderFront(up.URL, 1)
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(p)
	defer front.Close()
	for _, path := range []string{"/v1/messages", "/v1/messages", "/v1/models"} {
		method := http.MethodPost
		if path == "/v1/models" {
			method = http.MethodGet
		}
		req, _ := http.NewRequest(method, front.URL+path, strings.NewReader("{}"))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	up.Close()
	resp, err := http.Post(front.URL+"/v1/messages", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("an unreachable provider answered %d", resp.StatusCode)
	}
	if got := p.answers(); len(got) != 1 || got[429] != 2 {
		t.Fatalf("answers %v, want 429 twice", got)
	}
}
