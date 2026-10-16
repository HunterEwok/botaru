// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/hpke"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/HunterEwok/botaru/impl/internal/ids"
)

// exportKey is a recipient key pair in the form the core reads.
func exportKey(t *testing.T) (string, *ecdh.PrivateKey) {
	t.Helper()
	priv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return "x25519:" + base64.StdEncoding.EncodeToString(priv.PublicKey().Bytes()), priv
}

// newExportBench is the hello bench with bob in admins (admin:people), a sink without
// export_only, an export sink for the person (self) and one for the admin, and the tenant's
// privacy.export_sink.
func newExportBench(t *testing.T) (*actionBench, map[string]*ecdh.PrivateKey) {
	t.Helper()
	sink := func(name string, eo map[string]any) map[string]any {
		spec := map[string]any{"driver": "disk", "base": "/srv/" + name, "ceiling": "raw_logs"}
		if eo != nil {
			spec["export_only"] = eo
		}
		return map[string]any{"apiVersion": "botaru/v1alpha1", "kind": "Sink", "metadata": map[string]any{"name": name, "tenant": "demo"}, "spec": spec}
	}
	b := newBenchWith(t, func(root string, ms []map[string]any) []map[string]any {
		for _, m := range ms {
			if m["kind"] == "Tenant" {
				spec := m["spec"].(map[string]any)
				p, _ := spec["privacy"].(map[string]any)
				if p == nil {
					p = map[string]any{}
					spec["privacy"] = p
				}
				p["export_sink"] = "copies"
			}
		}
		return append(ms, map[string]any{
			"apiVersion": "botaru/v1alpha1", "kind": "Group",
			"metadata": map[string]any{"name": "admins", "tenant": "demo"},
			"spec":     map[string]any{"caps": []any{"admin:people"}, "data": []any{"summary"}},
		}, sink("plain", nil), sink("copies", map[string]any{"recipient_ref": "self", "key_ref": "EXPORT_SELF"}),
			sink("office", map[string]any{"recipient_ref": "admin", "key_ref": "EXPORT_ADMIN"}))
	})
	ctx := context.Background()
	bob := ids.PrincipalID()
	if _, err := b.c.Principal(ctx, b.op, PrincipalRequest{Op: "principal.add", Subject: bob, Identity: "telegram:1002"}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.c.st.Pool.Exec(ctx, `INSERT INTO memberships (tenant, principal, grp, state, created_at) VALUES ('demo', $1, 'admins', 'active', now())`, bob); err != nil {
		t.Fatal(err)
	}
	self, selfPriv := exportKey(t)
	admin, adminPriv := exportKey(t)
	b.c.cfg.ExportKeys = map[string]string{"EXPORT_SELF": self, "EXPORT_ADMIN": admin}
	return &actionBench{bench: b, bob: bob}, map[string]*ecdh.PrivateKey{"copies": selfPriv, "office": adminPriv}
}

// exported runs the export loop and returns the lines the sink gets, decrypted with the
// recipient's key, by schema id.
func exported(t *testing.T, b *actionBench, sink string, priv *ecdh.PrivateKey) map[string][]map[string]any {
	t.Helper()
	ctx := context.Background()
	if err := b.c.tickLoop(ctx, "erasure"); err != nil {
		t.Fatal(err)
	}
	var state, reason string
	_ = b.c.st.Pool.QueryRow(ctx, `SELECT state, coalesce(reason, '') FROM exports ORDER BY created_at DESC LIMIT 1`).Scan(&state, &reason)
	if state != "done" {
		t.Fatalf("export %s %s", state, reason)
	}
	sw := b.enroll(t, EnrollRequest{Tenant: "demo", Kind: "sink", Target: sink})
	item := b.claim(t, sw, "delivery")
	var dl map[string]any
	if item != nil {
		dl, _ = item.Payload["delivery"].(map[string]any)
	}
	if dl["kind"] != "export" {
		t.Fatalf("no export delivery to %s: %+v", sink, item)
	}
	body, _ := dl["body"].(map[string]any)
	ref, _ := body["file_ref"].(string)
	f, err := b.c.FileGet(ctx, sw, header(t, item), FileGetRequest{FileRef: ref})
	if err != nil {
		t.Fatalf("file %s: %v (%+v)", ref, err, item.Payload)
	}
	k, err := hpke.NewDHKEMPrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := hpke.Open(k, hpke.HKDFSHA256(), hpke.ChaCha20Poly1305(), []byte(exportInfo), f.Content)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	out := map[string][]map[string]any{}
	sc := bufio.NewScanner(bytes.NewReader(plain))
	sc.Buffer(nil, 1<<24)
	for sc.Scan() {
		var line struct {
			Schema string         `json:"schema"`
			Object map[string]any `json:"object"`
		}
		if err := json.Unmarshal(sc.Bytes(), &line); err != nil || !strings.HasPrefix(line.Schema, "urn:botaru:schema:") {
			t.Fatalf("line %s: %v", sc.Text(), err)
		}
		name := strings.TrimSuffix(strings.TrimPrefix(line.Schema, "urn:botaru:schema:"), ":1")
		if err := b.c.spec.Validate(name, line.Object); err != nil {
			t.Errorf("a %s line does not match its schema: %v", name, err)
		}
		out[line.Schema] = append(out[line.Schema], line.Object)
	}
	return out
}

func hasID(objs []map[string]any, id string) bool {
	for _, o := range objs {
		if o["id"] == id {
			return true
		}
	}
	return false
}

// Export (LCY-2, SEC-10, blackbox:core/export-jsonl, blackbox:core/export-sink-not-export): a
// sink without export_only gets sink_not_export, a person's copy only goes to that person, a
// tenant export not to self; the principal export holds the person's tasks but not one whose
// personal_subjects name another person, and no manifests; the tenant export leaves out the
// sessions of private chats and objects seen through a connection. Each line names its schema
// and the file is encrypted for the one recipient.
func TestExportLines(t *testing.T) {
	ctx := context.Background()
	b, keys := newExportBench(t)
	own := b.hello(t, "41")
	shared := b.hello(t, "42")
	conn := b.hello(t, "43")
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE tasks SET record = record || jsonb_build_object('personal_subjects', jsonb_build_array($2::text)) WHERE id = $1`, shared, b.bob); err != nil {
		t.Fatal(err)
	}
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE tasks SET record = record || '{"personal_seen": ["connection"]}' WHERE id = $1`, conn); err != nil {
		t.Fatal(err)
	}
	for conv, sid := range map[string]string{"c:telegram:direct:1001": "00000000-0000-4000-8000-000000000001", "c:telegram:group:-500": "00000000-0000-4000-8000-000000000002"} {
		if _, err := b.c.st.Pool.Exec(ctx, `INSERT INTO sessions (id, tenant, role, conversation, subjects, created_at, updated_at) VALUES ($1, 'demo', 'hello', $2, ARRAY[$3], now(), now());
			`, sid, conv, b.alice); err != nil {
			t.Fatal(err)
		}
		if _, err := b.c.st.Pool.Exec(ctx, `INSERT INTO session_entries (session, uuid, content, created_at) VALUES ($1, $1, '{"text": "hi"}', now())`, sid); err != nil {
			t.Fatal(err)
		}
	}
	try := func(op, subject, sink, reason string) {
		t.Helper()
		_, err := b.c.Export(ctx, b.op, ExportRequest{Op: op, Tenant: "demo", Subject: subject, Sink: sink})
		if !hasReason(err, reason) {
			t.Fatalf("%s to %s: %v, want %s", op, sink, err, reason)
		}
	}
	try("tenant.export", "", "plain", "sink_not_export")
	try("principal.export", b.alice, "office", "export_recipient_not_subject")
	try("tenant.export", "", "copies", "export_recipient_not_subject")

	d, err := b.c.Export(ctx, b.op, ExportRequest{Op: "principal.export", Tenant: "demo", Subject: b.alice, Sink: "copies"})
	if err != nil || d.State != "queued" {
		t.Fatalf("principal.export %+v %v", d, err)
	}
	lines := exported(t, b, "copies", keys["copies"])
	tasks := lines["urn:botaru:schema:task-record:1"]
	if !hasID(tasks, own) || hasID(tasks, shared) || len(lines["urn:botaru:schema:principal:1"]) != 1 {
		t.Fatalf("principal export %v", lines)
	}
	for schema := range lines {
		if strings.Contains(schema, "manifest") {
			t.Fatalf("a manifest in the export: %s", schema)
		}
	}
	if len(lines["urn:botaru:schema:session-entry:1"]) != 2 {
		t.Fatalf("the person's own sessions %v", lines["urn:botaru:schema:session-entry:1"])
	}

	if _, err := b.c.Export(ctx, b.op, ExportRequest{Op: "tenant.export", Tenant: "demo", Sink: "office"}); err != nil {
		t.Fatal(err)
	}
	lines = exported(t, b, "office", keys["office"])
	tasks = lines["urn:botaru:schema:task-record:1"]
	if !hasID(tasks, own) || !hasID(tasks, shared) || hasID(tasks, conn) || len(lines["urn:botaru:schema:session-entry:1"]) != 1 ||
		len(lines["urn:botaru:schema:principal:1"]) != 2 {
		t.Fatalf("tenant export %v", lines)
	}
	// The export file is handed out only with its export delivery.
	var ref string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT file_ref FROM exports WHERE kind = 'tenant'`).Scan(&ref); err != nil {
		t.Fatal(err)
	}
	var origin string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT origin FROM files WHERE file_ref = $1`, ref).Scan(&origin); err != nil || origin != "export" {
		t.Fatalf("export file origin %q %v", origin, err)
	}
}

// /mydata export (SEC-10, SEC-11, blackbox:core/mydata-export): without a key the person gets
// the usage; with a key a request of kind export goes on a card to bob (admin:people); his
// approval queues one principal export to the tenant's export sink with recipient_ref self,
// encrypted with the person's key; an admin cannot ask for another person's copy.
func TestMydataExport(t *testing.T) {
	ctx := context.Background()
	b, _ := newExportBench(t)
	if _, err := b.c.PrincipalDataRequest(ctx, b.op, DataRequest{Kind: "export", Subject: b.alice}); !hasReason(err, "export_recipient_not_subject") {
		t.Fatalf("a copy asked by another: %v", err)
	}
	if r := b.say(t, "2", "telegram:1001", "1001", "/mydata export"); r.Text != b.c.texts.Render("en", "cmd.mydata.export_key", nil) {
		t.Fatalf("no key: %+v", r)
	}
	pub, priv := exportKey(t)
	if r := b.say(t, "3", "telegram:1001", "1001", "/mydata export "+pub); !strings.Contains(r.Text, "recorded") {
		t.Fatalf("mydata export %+v", r)
	}
	s := b.drain(t)
	card := cardOf(t, s, "subject_request_export", "direct:1002")
	cd := card.body["card"].(map[string]any)
	validOut(t, b.bench, "card", cd)
	req := DecideRequest{SubjectKind: "subject_request", ID: cd["id"].(string), Nonce: cd["nonce"].(string), Decision: "approve"}
	if r, err := b.c.Decide(ctx, b.channel, press("telegram:1002", "direct:1002", card.external), req); err != nil || r.State != "approved" {
		t.Fatalf("decide %+v %v", r, err)
	}
	var sink, recipient, subject string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT sink, recipient_ref, subject FROM exports WHERE subject_request = $1`, req.ID).Scan(&sink, &recipient, &subject); err != nil ||
		sink != "copies" || recipient != "self" || subject != b.alice {
		t.Fatalf("export %s %s %s %v", sink, recipient, subject, err)
	}
	lines := exported(t, b, "copies", priv)
	if len(lines["urn:botaru:schema:principal:1"]) != 1 || lines["urn:botaru:schema:principal:1"][0]["id"] != b.alice {
		t.Fatalf("copy %v", lines)
	}
}
