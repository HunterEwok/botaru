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
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/ids"
	"github.com/HunterEwok/botaru/impl/internal/testbench"
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
	var applied []map[string]any
	var root string
	b := newBenchWith(t, func(r string, ms []map[string]any) []map[string]any {
		root = r
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
		applied = append(ms, map[string]any{
			"apiVersion": "botaru/v1alpha1", "kind": "Group",
			"metadata": map[string]any{"name": "admins", "tenant": "demo"},
			"spec":     map[string]any{"caps": []any{"admin:people"}, "data": []any{"summary"}},
		}, sink("plain", nil), sink("copies", map[string]any{"recipient_ref": "self", "key_ref": "EXPORT_SELF"}),
			sink("office", map[string]any{"recipient_ref": "admin", "key_ref": "EXPORT_ADMIN"}))
		return applied
	})
	ctx := context.Background()
	// A sink for alice by name, which takes an operator's principal.export of her data.
	personas, err := testbench.Personas(root)
	if err != nil {
		t.Fatal(err)
	}
	ms := append(applied, sink("mine", map[string]any{"recipient_ref": b.alice, "key_ref": "EXPORT_ALICE"}))
	if res, err := b.c.Apply(ctx, b.op, ApplyRequest{Commit: "1111111111111111111111111111111111111111", Manifests: ms, Personas: personas}); err != nil || !res.Applied {
		t.Fatalf("apply %+v %v", res, err)
	}
	bob := ids.PrincipalID()
	if _, err := b.c.Principal(ctx, b.op, PrincipalRequest{Op: "principal.add", Subject: bob, Identity: "telegram:1002"}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.c.st.Pool.Exec(ctx, `INSERT INTO memberships (tenant, principal, grp, state, created_at) VALUES ('demo', $1, 'admins', 'active', now())`, bob); err != nil {
		t.Fatal(err)
	}
	self, selfPriv := exportKey(t)
	admin, adminPriv := exportKey(t)
	mine, minePriv := exportKey(t)
	b.c.cfg.ExportKeys = map[string]string{"EXPORT_SELF": self, "EXPORT_ADMIN": admin, "EXPORT_ALICE": mine}
	return &actionBench{bench: b, bob: bob}, map[string]*ecdh.PrivateKey{"copies": selfPriv, "office": adminPriv, "mine": minePriv}
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
	if _, err := b.c.Complete(ctx, sw, header(t, item), CompleteRequest{Receipt: &Receipt{Location: "/srv/" + sink + "/" + ref}}); err != nil {
		t.Fatal(err)
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
	// The shared task came from a group chat; the others from alice's private chat.
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE tasks SET record = record || jsonb_build_object('conversation', 'c:telegram:group:-500:' || initiator,
		'origin', '{"channel": "telegram", "chat": {"kind": "group", "external_id": "-500"}}'::jsonb) WHERE id = $1`, shared); err != nil {
		t.Fatal(err)
	}
	// A memory item from a private-chat task and one from the group chat.
	mem := map[string]string{own: ids.New("mem"), shared: ids.New("mem")}
	for i, src := range []string{own, shared} {
		if _, err := b.c.st.Pool.Exec(ctx, `INSERT INTO memory_items (id, tenant, scope, subject, role, key, content, content_hash, state, nonce, source_task, created_at, updated_at)
			VALUES ($1, 'demo', 'person', $2, 'hello', 'k', $3, $3, 'active', 'n', $4, now(), now())`, mem[src], b.alice, "fact "+string(rune('a'+i)), src); err != nil {
			t.Fatal(err)
		}
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
	// A self sink takes only the person's own /mydata export: the operator's key_ref would
	// open it.
	try("principal.export", b.alice, "copies", "export_recipient_not_subject")
	try("principal.export", b.bob, "mine", "export_recipient_not_subject")

	d, err := b.c.Export(ctx, b.op, ExportRequest{Op: "principal.export", Tenant: "demo", Subject: b.alice, Sink: "mine"})
	if err != nil || d.State != "queued" {
		t.Fatalf("principal.export %+v %v", d, err)
	}
	// The key of the sink's key_ref changes while the export waits: the export is sealed with
	// the key of the settings when it is written.
	rotated, rotatedPriv := exportKey(t)
	b.c.cfg.ExportKeys["EXPORT_ALICE"] = rotated
	lines := exported(t, b, "mine", rotatedPriv)
	tasks := lines["urn:botaru:schema:task-record:1"]
	// The person's export leaves out the task and the memory item that name bob.
	mems := lines["urn:botaru:schema:memory-item:1"]
	if !hasID(tasks, own) || hasID(tasks, shared) || len(lines["urn:botaru:schema:principal:1"]) != 1 || !hasID(mems, mem[own]) || hasID(mems, mem[shared]) {
		t.Fatalf("principal export %v", lines)
	}
	// A key removed from the settings fails a waiting export with not_ready.
	if _, err := b.c.Export(ctx, b.op, ExportRequest{Op: "principal.export", Tenant: "demo", Subject: b.alice, Sink: "mine"}); err != nil {
		t.Fatal(err)
	}
	delete(b.c.cfg.ExportKeys, "EXPORT_ALICE")
	if err := b.c.tickLoop(ctx, "erasure"); err != nil {
		t.Fatal(err)
	}
	var failed string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT reason FROM exports WHERE state = 'failed' AND sink = 'mine'`).Scan(&failed); err != nil || failed != "not_ready" {
		t.Fatalf("export without its key %q %v", failed, err)
	}
	_ = keys["mine"]
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
	// The tenant export leaves out the private-chat tasks and their memory items (LCY-2).
	mems = lines["urn:botaru:schema:memory-item:1"]
	if hasID(tasks, own) || !hasID(tasks, shared) || hasID(tasks, conn) || len(lines["urn:botaru:schema:session-entry:1"]) != 1 ||
		len(lines["urn:botaru:schema:principal:1"]) != 2 || hasID(mems, mem[own]) || !hasID(mems, mem[shared]) {
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
	// A low-order key would give a zero shared secret: it is refused when given.
	low := "x25519:" + base64.StdEncoding.EncodeToString(make([]byte, 32))
	if r := b.say(t, "4", "telegram:1001", "1001", "/mydata export "+low); r.Text != b.c.texts.Render("en", "cmd.mydata.export_key", nil) {
		t.Fatalf("low-order key: %+v", r)
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
	// Once the sink took the copy the person is told where it is, and the request is closed.
	var state string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT state FROM subject_requests WHERE id = $1`, req.ID).Scan(&state); err != nil || state != "done" {
		t.Fatalf("request %s %v", state, err)
	}
	var target, key, location string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT target_id, body->>'text_key', body->'params'->>'location' FROM deliveries
		WHERE kind = 'notice' AND body->'params'->>'request' = $1`, req.ID).Scan(&target, &key, &location); err != nil ||
		target != "direct:1001" || key != "notice.export_ready" || !strings.HasPrefix(location, "/srv/copies/") {
		t.Fatalf("notice %s %s %s %v", target, key, location, err)
	}
	// The closed request does not withhold it: the channel gets the text with the location.
	item := b.claim(t, b.channel, "delivery")
	if item == nil {
		t.Fatal("the export_ready notice is not handed to the channel")
	}
	dl, _ := item.Payload["delivery"].(map[string]any)
	db, _ := dl["body"].(map[string]any)
	params, _ := db["params"].(map[string]any)
	if text := b.c.texts.Render("en", "notice.export_ready", params); dl["kind"] != "notice" || db["text_key"] != "notice.export_ready" || !strings.Contains(text, location) {
		t.Fatalf("export_ready delivery %+v", item.Payload)
	}
}

// Holds at delivery (HLD-1, HLD-3, LCY-1, SEC-6): a suspension plans the admins' notice, which
// passes the suspension like an export does; a sink hold stops an export; an offboarding tenant
// enrolls only its export sinks; the export window is at least a day.
func TestExportHolds(t *testing.T) {
	ctx := context.Background()
	b, keys := newExportBench(t)
	if _, err := b.c.TenantLifecycle(ctx, b.op, TenantLifecycleRequest{Op: "tenant.offboard", Tenant: "demo", ExportWindow: "1h"}); !hasReason(err, "input_schema_invalid") {
		t.Fatalf("export window below a day: %v", err)
	}
	s, err := b.c.TenantLifecycle(ctx, b.op, TenantLifecycleRequest{Op: "tenant.suspend", Tenant: "demo"})
	if err != nil {
		t.Fatal(err)
	}
	item := b.claim(t, b.channel, "delivery")
	var dl map[string]any
	if item != nil {
		dl, _ = item.Payload["delivery"].(map[string]any)
	}
	body, _ := dl["body"].(map[string]any)
	rcpt, _ := dl["recipient"].(map[string]any)
	chat, _ := rcpt["chat"].(map[string]any)
	if dl["kind"] != "notice" || chat["external_id"] != "1002" || body["about_hold"] != s.ID {
		t.Fatalf("the admins' notice of the suspension %+v", item)
	}
	if _, err := b.c.Export(ctx, b.op, ExportRequest{Op: "tenant.export", Tenant: "demo", Sink: "office"}); err != nil {
		t.Fatal(err)
	}
	exported(t, b, "office", keys["office"])

	// The sink's credential is compromised: its sink hold stops the next export (SEC-6).
	b.c.cfg.SecretUses = map[string][]HoldScope{"office-key": {{Kind: "sink", Tenant: "demo", Name: "office"}}}
	if _, err := b.c.SecretRevoke(ctx, b.op, SecretRevokeRequest{Secret: "office-key", Compromised: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.c.Export(ctx, b.op, ExportRequest{Op: "tenant.export", Tenant: "demo", Sink: "office"}); err != nil {
		t.Fatal(err)
	}
	if err := b.c.tickLoop(ctx, "erasure"); err != nil {
		t.Fatal(err)
	}
	sw := b.enroll(t, EnrollRequest{Tenant: "demo", Kind: "sink", Target: "office"})
	if item := b.claim(t, sw, "delivery"); item != nil {
		t.Fatalf("an export through a held sink %+v", item.Payload)
	}

	// A tenant-hold notice passes only the suspension or offboarding it names: one naming the
	// sink's compromised hold is withheld by the suspension.
	var sinkHold string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT id FROM holds WHERE scope_kind = 'sink' AND reason = 'compromised'`).Scan(&sinkHold); err != nil {
		t.Fatal(err)
	}
	var stray string
	err = b.c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		var err error
		stray, err = b.c.planDelivery(ctx, tx, now, plannedDelivery{
			Tenant: "demo", Kind: "notice", Addr: "admins", TargetKind: "chat", TargetID: "direct:1002", ExecKind: "channel", ExecName: "telegram",
			Recipient: b.bob, DataOrigin: "code",
			Body: map[string]any{"form": "text_code", "text_key": "notice.tenant_held", "about_hold": sinkHold, "params": map[string]any{"tenant": "demo", "state": "suspended", "hold": sinkHold}},
		})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if item := b.claim(t, b.channel, "delivery"); item != nil {
		t.Fatalf("a notice about another hold passed the suspension %+v", item.Payload)
	}
	var strayState string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT state FROM deliveries WHERE id = $1`, stray).Scan(&strayState); err != nil || strayState != "withheld" {
		t.Fatalf("stray notice %s %v", strayState, err)
	}

	if _, err := b.c.HoldRelease(ctx, OperatorActor(b.op), HoldReleaseRequest{ID: s.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.c.TenantLifecycle(ctx, b.op, TenantLifecycleRequest{Op: "tenant.offboard", Tenant: "demo", ExportWindow: "1d"}); err != nil {
		t.Fatal(err)
	}
	// The channels are revoked: no notice is planned, and the audit record says why.
	var why string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT record->'after'->>'admins_notice' FROM audit WHERE record->>'command' = 'tenant.offboard'`).Scan(&why); err != nil || why != "channels_revoked" {
		t.Fatalf("offboarding audit %q %v", why, err)
	}
	if _, err := b.c.Enroll(ctx, b.op, EnrollRequest{Tenant: "demo", Role: "hello", Kind: "agent"}); !hasReason(err, "forbidden") {
		t.Fatalf("an agent enrolled while offboarding: %v", err)
	}
	if _, err := b.c.Enroll(ctx, b.op, EnrollRequest{Tenant: "demo", Kind: "sink", Target: "plain"}); !hasReason(err, "forbidden") {
		t.Fatalf("a sink without export_only enrolled while offboarding: %v", err)
	}
	if _, err := b.c.Enroll(ctx, b.op, EnrollRequest{Tenant: "demo", Kind: "sink", Target: "office"}); err != nil {
		t.Fatalf("the export sink while offboarding: %v", err)
	}
}
