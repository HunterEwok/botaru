// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// newFileBench is the hello bench whose group may see internal data.
func newFileBench(t *testing.T) *bench {
	t.Helper()
	return newBenchWith(t, func(root string, ms []map[string]any) []map[string]any {
		for _, m := range ms {
			if m["kind"] == "Group" {
				m["spec"].(map[string]any)["data"] = []any{"summary", "internal"}
			}
		}
		return ms
	})
}

// A person's upload is marked at least internal and at most their right; only their own upload
// attaches to their task, which starts untrusted with the file's mark; a produced file has
// the task's mark and provenance from the records; handout follows the lease and the rights;
// a result with provenance is refused (ART-1, ART-2, ART-3, ART-6, PRV-2).
func TestFiles(t *testing.T) {
	ctx := context.Background()
	b := newFileBench(t)
	alice := &OnBehalf{Identity: "telegram:1001", Chat: Chat{Kind: "direct", ExternalID: "1001"}}
	csv := []byte("station,depth\nst-03,12\n")
	if _, err := b.c.FilePut(ctx, b.channel, "", alice, FilePutRequest{MediaType: "text/csv", DataMark: "restricted"}, csv); err == nil {
		t.Fatal("a mark above the uploader's right passed")
	}
	up, err := b.c.FilePut(ctx, b.channel, "", alice, FilePutRequest{MediaType: "image/png"}, csv)
	if err != nil {
		t.Fatal(err)
	}
	var mt, mark, origin string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT media_type, data_mark, origin FROM files WHERE file_ref = $1`, up.FileRef).Scan(&mt, &mark, &origin); err != nil {
		t.Fatal(err)
	}
	if mt != "text/plain" || mark != "internal" || origin != "upload" {
		t.Fatalf("record %s %s %s", mt, mark, origin)
	}
	// Someone else's upload does not attach.
	bob := "p_bob"
	if _, err := b.c.Principal(ctx, b.op, PrincipalRequest{Op: "principal.add", Subject: bob, Identity: "telegram:1002"}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.c.Grant(ctx, b.op, GrantRequest{Op: "grant", Subject: bob, Group: "hello"}); err != nil {
		t.Fatal(err)
	}
	a := Activity{Kind: "message", Channel: "telegram", UpdateID: "1", Chat: Chat{Kind: "direct", ExternalID: "1002"}, Text: "/hello look", Attachments: []string{up.FileRef}}
	a.Sender.Identity, a.Sender.Verified = "telegram:1002", true
	if r, err := b.c.SubmitActivity(ctx, b.channel, a); err != nil || r.Result != "reply" || !strings.Contains(r.Text, "could not be used") {
		t.Fatalf("someone else's file %+v", r)
	}
	a.UpdateID, a.Chat.ExternalID, a.Sender.Identity = "2", "1001", "telegram:1001"
	r, err := b.c.SubmitActivity(ctx, b.channel, a)
	if err != nil || r.Result != "task" {
		t.Fatalf("own file %+v %v", r, err)
	}
	var rec []byte
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT record FROM tasks WHERE id = $1`, r.Task).Scan(&rec); err != nil {
		t.Fatal(err)
	}
	var tr map[string]any
	_ = json.Unmarshal(rec, &tr)
	if tr["untrusted_seen"] != true || tr["data_mark"] != "internal" || tr["untrusted_source"] != "attachment" {
		t.Fatalf("task record %v", tr)
	}
	att := b.claim(t, b.agent, "attempt")
	h := header(t, att)
	got, err := b.c.FileGet(ctx, b.agent, h, FileGetRequest{FileRef: up.FileRef})
	if err != nil || string(got.Content) != string(csv) {
		t.Fatalf("file.get %+v %v", got, err)
	}
	if _, err := b.c.FileGet(ctx, b.agent, h, FileGetRequest{FileRef: "file:AAAAAAAAAAAAAAAAAAAAAA"}); err == nil {
		t.Fatal("an unknown file was handed out")
	}
	if _, err := b.c.FilePut(ctx, b.agent, h, nil, FilePutRequest{MediaType: "text/plain"}, []byte("report")); err == nil {
		t.Fatal("a model runtime produced a file")
	}
	// The same lease seen as a job worker's: only a job produces a file under an attempt.
	job := *b.agent
	job.Kind = "job"
	prod, err := b.c.FilePut(ctx, &job, h, nil, FilePutRequest{MediaType: "text/plain", DataMark: "summary"}, []byte("report"))
	if err != nil {
		t.Fatal(err)
	}
	var prov []byte
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT data_mark, origin, provenance FROM files WHERE file_ref = $1`, prod.FileRef).Scan(&mark, &origin, &prov); err != nil {
		t.Fatal(err)
	}
	if mark != "internal" || origin != "produced" || !strings.Contains(string(prov), up.FileRef) || !strings.Contains(string(prov), r.Task) {
		t.Fatalf("produced %s %s %s", mark, origin, prov)
	}
	if _, err := b.c.Complete(ctx, b.agent, h, CompleteRequest{Result: map[string]any{"answer": "x", "provenance": map[string]any{}}}); err == nil || !strings.Contains(err.Error(), "provenance") {
		t.Fatalf("a result with provenance: %v", err)
	}
	// A service principal declares the personal data of its uploads.
	if _, err := b.c.Principal(ctx, b.op, PrincipalRequest{Op: "principal.add", Subject: "p_svc", Kind: "service", Identity: "telegram:1003"}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE principals SET uploads_personal = true WHERE id = 'p_svc'`); err != nil {
		t.Fatal(err)
	}
	if _, err := b.c.st.Pool.Exec(ctx, `INSERT INTO memberships (tenant, principal, grp, state, created_at) VALUES ('demo', 'p_svc', 'hello', 'active', now())`); err != nil {
		t.Fatal(err)
	}
	svc, err := b.c.FilePut(ctx, b.channel, "", &OnBehalf{Identity: "telegram:1003"}, FilePutRequest{MediaType: "text/csv"}, csv)
	if err != nil {
		t.Fatal(err)
	}
	var personal *string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT personal_seen FROM files WHERE file_ref = $1`, svc.FileRef).Scan(&personal); err != nil || deref(personal) != "declared" {
		t.Fatalf("declared personal %v %v", personal, err)
	}
}

// The storage ceiling refuses a put beyond max_storage_bytes (CAP-1); principal.add writes
// uploads_personal (ART-6); reading a person's upload marks the task personal_seen human_file
// (AUD-5); an ingest job's file is ingested (ART-6); a lost data class revokes a task by the
// mark it took, not only its type's class (PRM-8); an expired file is purged (RET-2).
func TestFileCeilingAndMarks(t *testing.T) {
	ctx := context.Background()
	b := newBenchWith(t, func(root string, ms []map[string]any) []map[string]any {
		var extra []map[string]any
		for _, m := range ms {
			switch m["kind"] {
			case "Tenant":
				m["spec"].(map[string]any)["limits"] = map[string]any{"max_storage_bytes": 50}
			case "Group":
				raw, _ := json.Marshal(m)
				var g map[string]any
				_ = json.Unmarshal(raw, &g)
				g["metadata"].(map[string]any)["name"] = "files"
				g["spec"].(map[string]any)["data"] = []any{"summary", "internal"}
				extra = append(extra, g)
			}
		}
		return append(ms, extra...)
	})
	if _, err := b.c.Grant(ctx, b.op, GrantRequest{Op: "grant", Subject: b.alice, Group: "files"}); err != nil {
		t.Fatal(err)
	}
	yes := true
	if _, err := b.c.Principal(ctx, b.op, PrincipalRequest{Op: "principal.add", Subject: "p_svc", Kind: "service", Identity: "telegram:1003", UploadsPersonal: &yes}); err != nil {
		t.Fatal(err)
	}
	var declared *bool
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT uploads_personal FROM principals WHERE id = 'p_svc'`).Scan(&declared); err != nil || declared == nil || !*declared {
		t.Fatalf("uploads_personal %v %v", declared, err)
	}
	alice := &OnBehalf{Identity: "telegram:1001", Chat: Chat{Kind: "direct", ExternalID: "1001"}}
	up, err := b.c.FilePut(ctx, b.channel, "", alice, FilePutRequest{MediaType: "text/plain"}, make([]byte, 30))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.c.FilePut(ctx, b.channel, "", alice, FilePutRequest{MediaType: "text/plain"}, make([]byte, 30)); err == nil || !strings.Contains(err.Error(), "over_capacity") {
		t.Fatalf("a put past the ceiling: %v", err)
	}
	a := Activity{Kind: "message", Channel: "telegram", UpdateID: "1", Chat: Chat{Kind: "direct", ExternalID: "1001"}, Text: "/hello look", Attachments: []string{up.FileRef}}
	a.Sender.Identity, a.Sender.Verified = "telegram:1001", true
	r, err := b.c.SubmitActivity(ctx, b.channel, a)
	if err != nil || r.Result != "task" {
		t.Fatalf("own file %+v %v", r, err)
	}
	att := b.claim(t, b.agent, "attempt")
	h := header(t, att)
	if _, err := b.c.FileGet(ctx, b.agent, h, FileGetRequest{FileRef: up.FileRef}); err != nil {
		t.Fatal(err)
	}
	var seen, typ string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT coalesce(record->>'personal_seen', ''), type FROM tasks WHERE id = $1`, r.Task).Scan(&seen, &typ); err != nil || seen != `["human_file"]` {
		t.Fatalf("personal_seen %q %v", seen, err)
	}
	// The same lease seen as an ingest job's.
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE tasks SET type = 'kb.ingest.v1' WHERE id = $1`, r.Task); err != nil {
		t.Fatal(err)
	}
	job := *b.agent
	job.Kind = "job"
	in, err := b.c.FilePut(ctx, &job, h, nil, FilePutRequest{MediaType: "text/plain"}, []byte("doc"))
	if err != nil {
		t.Fatal(err)
	}
	var origin string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT origin FROM files WHERE file_ref = $1`, in.FileRef).Scan(&origin); err != nil || origin != "ingested" {
		t.Fatalf("origin %q %v", origin, err)
	}
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE tasks SET type = $2 WHERE id = $1`, r.Task, typ); err != nil {
		t.Fatal(err)
	}
	// The task took the internal mark of the file; without the group that sees internal the
	// task stops, though its type's class is summary.
	if _, err := b.c.Grant(ctx, b.op, GrantRequest{Op: "ungrant", Subject: b.alice, Group: "files"}); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT state FROM tasks WHERE id = $1`, r.Task).Scan(&state); err != nil || state != "failed" {
		t.Fatalf("task after the data class was lost: %q %v", state, err)
	}
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE files SET expires_at = botaru_now() - interval '1 second' WHERE file_ref = $1`, up.FileRef); err != nil {
		t.Fatal(err)
	}
	if err := b.c.tickLoop(ctx, "retention"); err != nil {
		t.Fatal(err)
	}
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT state FROM files WHERE file_ref = $1`, up.FileRef).Scan(&state); err != nil || state != "purged" {
		t.Fatalf("an expired file: %q %v", state, err)
	}
}
