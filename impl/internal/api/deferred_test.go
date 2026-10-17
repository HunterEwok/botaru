// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// D-114, MAN-11: an operation of a deferred rule, or a deferred field of a served operation's
// body, is refused with version_unsupported and the path; a served body passes.
func TestDeferredOp(t *testing.T) {
	cases := []struct {
		name   string
		body   map[string]any
		refuse bool
		detail string
	}{
		{"P2.task.replay", nil, true, "P2.task.replay is of the deferred rule APR-8"},
		{"P1.obs.append", nil, true, "OBV-8"},
		{"P2.control.apply", map[string]any{"op": "pin_task"}, true, "/op of P2.control.apply is of the deferred rule MGR-12"},
		{"P2.control.apply", map[string]any{"op": "hold"}, false, ""},
		{"P1.campaign.propose", map[string]any{"rollout": map[string]any{"waves": []any{}}}, true, "/rollout/waves"},
		{"P1.campaign.propose", map[string]any{"rollout": map[string]any{"max_failures": 1}}, false, ""},
		{"P1.enqueue", map[string]any{"mode": "x"}, true, "PRM-11"},
		{"P1.enqueue", nil, false, ""},
		{"P1.hello", map[string]any{"runtime": map[string]any{"skills": "farm"}}, true, "/runtime/skills of P1.hello is of the deferred rule SKL-4"},
		{"P1.hello", map[string]any{"runtime": map[string]any{"skills": "runtime_files"}}, false, ""},
		{"P2.chat.bind", map[string]any{"assistant": true}, true, "DSP-10"},
		{"P2.chat.bind", map[string]any{"assistant": false}, false, ""},
	}
	for _, c := range cases {
		w := httptest.NewRecorder()
		got := deferredOp(w, c.name, c.body)
		if got != c.refuse {
			t.Errorf("%s %v: refused %v", c.name, c.body, got)
			continue
		}
		if !got {
			continue
		}
		var p map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
			t.Fatal(err)
		}
		if p["reason"] != "version_unsupported" || w.Code != 400 || !strings.Contains(p["detail"].(string), c.detail) {
			t.Errorf("%s: %d %v", c.name, w.Code, p)
		}
	}
}
