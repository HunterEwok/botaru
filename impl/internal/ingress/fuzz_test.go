// SPDX-License-Identifier: AGPL-3.0-only

package ingress

import (
	"net/http"
	"regexp"
	"testing"
	"time"
)

var fuzzStatus = map[int]bool{http.StatusNoContent: true, http.StatusBadRequest: true, http.StatusRequestEntityTooLarge: true,
	http.StatusUnsupportedMediaType: true, http.StatusUnprocessableEntity: true, http.StatusTooManyRequests: true}

// Fuzzing a signed body: whatever the sender puts in it, the answer is one of the planned codes
// and the signal carries only mapped fields that pass their closed schema, never free text (ING-2).
func FuzzIngressBody(f *testing.F) {
	f.Add(body)
	for _, s := range []string{`{}`, `[]`, `{"run":{"id":"Run 1","status":"failed ","steps":[{"n":3.5}]}}`, `{"run":{"steps":{"0":{"n":1}}}}`,
		`{"run":{"id":"a","id":"b"}}`, `{"run":{"host":"web1/../x","steps":[{"n":1e400}]}}`} {
		f.Add([]byte(s))
	}
	id := regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	host := regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,119}$`)
	f.Fuzz(func(t *testing.T, b []byte) {
		r := newRig(t, "signature", nil)
		rec := r.post("msg_1", r.now, "", b, nil)
		if !fuzzStatus[rec.Code] {
			t.Fatalf("status %d for %q", rec.Code, b)
		}
		if len(r.core.activities) > 1 || len(r.core.activities) == 1 && rec.Code != http.StatusNoContent {
			t.Fatalf("%d activities, status %d", len(r.core.activities), rec.Code)
		}
		for _, a := range r.core.activities {
			data := a["event"].(map[string]any)["data"].(map[string]any)
			for k, v := range data {
				ok := false
				switch k {
				case "run_id":
					s, isStr := v.(string)
					ok = isStr && id.MatchString(s)
				case "status":
					ok = v == "failed" || v == "cancelled"
				case "host":
					s, isStr := v.(string)
					ok = isStr && host.MatchString(s)
				case "step":
					n, isNum := number(v)
					ok = isNum && n >= 0 && n <= 100 && n == float64(int64(n))
				}
				if !ok {
					t.Fatalf("field %s=%#v passed from %q", k, v, b)
				}
			}
		}
	})
}

// Fuzzing the authenticity headers: a forged delivery never reaches the core.
func FuzzIngressForged(f *testing.F) {
	f.Add("msg_1", "1800000000", "v1,AAAA", body)
	f.Add("", "", "", []byte(`{}`))
	f.Add("msg_1", "1800000000", "v1,"+sign([]byte("wrong-key-0123456789abcdef012345"), "msg_1", 1_800_000_000, body)[3:], body)
	f.Fuzz(func(t *testing.T, msgID, ts, sig string, b []byte) {
		r := newRig(t, "signature", nil)
		if ts == "1800000000" && sig == sign(testKey, msgID, r.now.Unix(), b) {
			return
		}
		rec := r.post("", time.Time{}, "", b, map[string]string{"webhook-id": msgID, "webhook-timestamp": ts, "webhook-signature": sig, "Content-Type": "application/json"})
		if len(r.core.activities) > 0 || len(r.core.files) > 0 {
			t.Fatalf("a forged delivery reached the core, status %d", rec.Code)
		}
	})
}
