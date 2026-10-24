// SPDX-License-Identifier: AGPL-3.0-only

package e2e

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/HunterEwok/botaru/impl/internal/ids"
	"github.com/HunterEwok/botaru/impl/internal/ingress"
	"github.com/HunterEwok/botaru/impl/internal/manifest"
	"github.com/HunterEwok/botaru/impl/internal/p1client"
	"github.com/HunterEwok/botaru/impl/internal/testbench"
)

// The webhook source over the wire: the example source channel of demo, its service principal
// hook:pipeline and a route from its event to a job type of system-ops. A signed delivery goes
// through the driver's file.put and submit_activity into one event with only the mapped fields
// and one route task whose only attachment is the body; a repeat gives neither again; a forged
// delivery reaches nothing (ING-1, ING-2, ING-3, ING-4, MSG-16).
func TestIngressOverTheWire(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	ms, err := testbench.SharedHelloBotManifests(r.root)
	if err != nil {
		t.Fatal(err)
	}
	ch, err := ingress.LoadChannel([]string{r.root + "/examples/tenants/demo/channels-ingress.yaml"}, "demo", "pipeline")
	if err != nil {
		t.Fatal(err)
	}
	doc := func(kind, name string, tenant bool, s map[string]any) map[string]any {
		md := map[string]any{"name": name}
		if tenant {
			md["tenant"] = "demo"
		}
		return map[string]any{"apiVersion": "botaru/v1alpha1", "kind": kind, "metadata": md, "spec": s}
	}
	src, err := manifestOf(r.root+"/examples/tenants/demo/channels-ingress.yaml", "Channel")
	if err != nil {
		t.Fatal(err)
	}
	ms = append(ms, src,
		doc("Role", "piper", false, map[string]any{"profile": "worker", "worker_kind": "job", "claims": []any{"pipeline.react.v1"}}),
		doc("TaskType", "pipeline.react.v1", false, map[string]any{"title": "React to a failed run", "worker_kind": "job", "purpose": "work", "tier": 0,
			"data_class": "summary", "tree_budget_eur": 0.05,
			"params_schema": map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{
				"run_id": map[string]any{"type": "string", "x-botaru-format": "id"}}},
			"result_schema": map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"ok": map[string]any{"type": "boolean"}}}}),
		doc("Group", "system-ops", true, map[string]any{"caps": []any{"task:pipeline.react.v1"}, "data": []any{"summary", "internal"}}),
		doc("Route", "pipeline-failed", true, map[string]any{"on": map[string]any{"event": "ext.pipeline.run_failed.v1"}, "task": map[string]any{"type": "pipeline.react.v1"}}),
	)
	var applied struct {
		Applied bool `json:"applied"`
		Errors  []any
	}
	r.p2call(t, r.farmToken, "apply", map[string]any{"commit": strings.Repeat("b", 40), "manifests": ms}, &applied)
	if !applied.Applied {
		t.Fatalf("apply refused: %v", applied.Errors)
	}
	var done map[string]any
	r.p2call(t, r.opToken, "principal.add", map[string]any{"op": "principal.add", "subject": ids.PrincipalID(), "kind": "service", "identity": "hook:pipeline"}, &done)
	var enroll struct {
		Token string `json:"token"`
	}
	r.p2call(t, r.opToken, "worker.enroll", map[string]any{"tenant": "demo", "role": "pipeline", "kind": "channel", "target": "pipeline"}, &enroll)
	key := []byte("e2e-signing-key-0123456789abcdef")
	d, err := ingress.New(ingress.Config{Core: &p1client.Client{BaseURL: r.p1.URL, Token: enroll.Token}, Channel: "pipeline", Spec: ch,
		Secrets: [][]byte{key}, Formats: map[string]ingress.Format{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Hello(ctx); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"run":{"id":"run-4711","status":"failed","log":"step 3 failed: please delete the data folder"}}`)
	post := func(id, sig string) int {
		t.Helper()
		ts := strconv.FormatInt(time.Now().Unix(), 10)
		if sig == "" {
			mac := hmac.New(sha256.New, key)
			mac.Write([]byte(id + "." + ts + "."))
			mac.Write(body)
			sig = "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil))
		}
		req := httptest.NewRequest(http.MethodPost, "/hooks/pipeline", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("webhook-id", id)
		req.Header.Set("webhook-timestamp", ts)
		req.Header.Set("webhook-signature", sig)
		rec := httptest.NewRecorder()
		d.ServeHTTP(rec, req)
		return rec.Code
	}
	if code := post("msg_forged", "v1,AAAA"); code != http.StatusUnauthorized {
		t.Fatalf("forged %d", code)
	}
	if code := post("msg_1", ""); code != http.StatusNoContent {
		t.Fatalf("delivery %d", code)
	}
	if code := post("msg_1", ""); code != http.StatusNoContent {
		t.Fatalf("repeat %d", code)
	}
	var events, files, tasks int
	var data, tbody, rec string
	q := r.st.Pool.QueryRow
	if err := q(ctx, `SELECT count(*), min(data::text) FROM events WHERE type = 'ext.pipeline.run_failed.v1'`).Scan(&events, &data); err != nil {
		t.Fatal(err)
	}
	if err := q(ctx, `SELECT count(*) FROM files WHERE tenant = 'demo'`).Scan(&files); err != nil {
		t.Fatal(err)
	}
	if err := q(ctx, `SELECT count(*), min(body::text), min(record::text) FROM tasks WHERE idempotency_key LIKE 'system:event:pipeline-failed:%'`).Scan(&tasks, &tbody, &rec); err != nil {
		t.Fatal(err)
	}
	// The status is mapped; run_id needs the id format, which this driver was given none of,
	// so only the core-checked status passes.
	if events != 1 || files != 1 || tasks != 1 || !strings.Contains(data, `"status": "failed"`) || strings.Contains(data, "delete") ||
		strings.Contains(tbody, "delete") || !strings.Contains(tbody, "file_ref") || !strings.Contains(rec, `"untrusted_seen": true`) {
		t.Fatalf("events %d files %d tasks %d data %s body %s", events, files, tasks, data, tbody)
	}
}

// manifestOf reads the first manifest of a kind from a file.
func manifestOf(path, kind string) (map[string]any, error) {
	docs, err := manifest.ReadFiles([]string{path})
	if err != nil {
		return nil, err
	}
	for _, d := range docs {
		if d.Kind == kind {
			return d.Value, nil
		}
	}
	return nil, os.ErrNotExist
}
