// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/hpke"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/ids"
	"github.com/HunterEwok/botaru/impl/internal/manifest"
)

// Export of a tenant or a principal (LCY-2, SEC-10). P2 tenant.export and principal.export, and
// a confirmed /mydata export request, queue a core export task (a row of exports, not a task
// a worker claims, so a hold on the tenant does not stop it: only export is allowed while the
// tenant is offboarding). The erasure loop runs it: one JSON Lines file, each line naming the
// $id of its spec/model schema, encrypted for the one recipient, stored as a file of origin
// export and handed only to the export_only sink by a delivery of kind export. Nothing goes to
// a chat or a model, and declared objects (manifests) are not exported: they are in git.
//
// Encryption is HPKE (RFC 9180) from the standard library, base mode, single shot:
// DHKEM(X25519, HKDF-SHA256), HKDF-SHA256 and ChaCha20-Poly1305, with the info string
// exportInfo. The file is the encapsulated key followed by the ciphertext. A public key is
// written x25519:<base64 of the 32 bytes>.

const exportInfo = "botaru export v1"

// ExportRequest is p2-export-request.
type ExportRequest struct {
	Op      string `json:"op"`
	Tenant  string `json:"tenant"`
	Subject string `json:"subject"`
	Sink    string `json:"sink"`
}

// exportPublicKey parses an X25519 public key: x25519:<base64>, or the bare base64.
func exportPublicKey(s string) (hpke.PublicKey, error) {
	s = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "x25519:"))
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		if raw, err = base64.RawStdEncoding.DecodeString(s); err != nil {
			return nil, errors.New("the key is not base64")
		}
	}
	pub, err := ecdh.X25519().NewPublicKey(raw)
	if err != nil {
		return nil, errors.New("the key is not an X25519 public key")
	}
	return hpke.NewDHKEMPublicKey(pub)
}

// sealExport encrypts an export for its recipient.
func sealExport(key string, plain []byte) ([]byte, error) {
	pk, err := exportPublicKey(key)
	if err != nil {
		return nil, err
	}
	return hpke.Seal(pk, hpke.HKDFSHA256(), hpke.ChaCha20Poly1305(), []byte(exportInfo), plain)
}

// exportSink is the export_only block of a sink of the tenant: the recipient and the key_ref.
func exportSink(cat *Catalog, tenant, name string) (recipient, keyRef string, err error) {
	raw := cat.RawGlobals[manifest.Key{Kind: "Sink", Tenant: tenant, Name: name}]
	if raw == nil {
		return "", "", refuse("not_found", "no sink %s in the tenant", name)
	}
	spec, _ := raw["spec"].(map[string]any)
	eo, ok := spec["export_only"].(map[string]any)
	if !ok {
		return "", "", refuse("sink_not_export", "the sink %s has no export_only", name)
	}
	recipient = mstr(eo, "recipient_ref")
	if recipient == "" {
		// The deprecated form: admin, or a recipient that is not a person reference.
		recipient = mstr(eo, "recipient")
	}
	return recipient, mstr(eo, "key_ref"), nil
}

// checkRecipient checks the one recipient of an export (SEC-10): a person's copy goes only to
// that person (self or their own human:<id>), admin only takes a tenant export.
func checkRecipient(kind, subject, recipient string) error {
	switch kind {
	case "principal":
		if recipient != "self" && recipient != subject {
			return refuse("export_recipient_not_subject", "a copy of a person's data goes only to that person")
		}
	default:
		if recipient != "admin" && !strings.HasPrefix(recipient, "human:") {
			return refuse("export_recipient_not_subject", "a tenant export goes to admin or a named person")
		}
	}
	return nil
}

// Export is P2 tenant.export and principal.export (LCY-2, SEC-10, PRM-14).
func (c *Core) Export(ctx context.Context, op Operator, req ExportRequest) (*Done, error) {
	if op.Tenant != farmTenant && op.Tenant != req.Tenant {
		return nil, refuse("forbidden", "the operator credential is for another tenant")
	}
	kind := strings.TrimSuffix(req.Op, ".export")
	if kind != "tenant" && kind != "principal" {
		return nil, refuse("input_schema_invalid", "unknown op %s", req.Op)
	}
	if (kind == "principal") != (req.Subject != "") {
		return nil, refuse("input_schema_invalid", "subject is given with principal.export and only with it")
	}
	tv, cat, err := c.requireTenant(ctx, req.Tenant)
	if err != nil {
		return nil, err
	}
	recipient, keyRef, err := exportSink(cat, tv.Name, req.Sink)
	if err != nil {
		return nil, err
	}
	if err := checkRecipient(kind, req.Subject, recipient); err != nil {
		return nil, err
	}
	key := c.cfg.ExportKeys[keyRef]
	if key == "" {
		return nil, refuse("not_ready", "no public key for key_ref %s in the core settings: an export is always encrypted", keyRef)
	}
	if _, err := exportPublicKey(key); err != nil {
		return nil, refuse("not_ready", "the public key for key_ref %s: %v", keyRef, err)
	}
	var out Done
	err = c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		if state, _, err := tenantState(ctx, tx, tv.Name); err != nil {
			return err
		} else if state == "deleted" {
			return refuse("gone", "the tenant is deleted")
		}
		if kind == "principal" {
			var exists bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM principals WHERE tenant = $1 AND id = $2)`, tv.Name, req.Subject).Scan(&exists); err != nil {
				return err
			}
			if !exists {
				return refuse("not_found", "no such principal")
			}
		}
		id, err := c.queueExport(ctx, tx, now, tv.Name, kind, req.Subject, req.Sink, recipient, key, "", op.Principal)
		if err != nil {
			return err
		}
		seq, err := c.appendAudit(ctx, tx, now, Audit{
			Tenant: tv.Name, Kind: "command", Actor: Actor{Principal: op.Principal, Via: "farmctl"},
			Command: req.Op, Target: &Target{Kind: "export", ID: id}, OK: true,
			After: map[string]any{"sink": req.Sink, "recipient_ref": recipient, "subject": nullable(req.Subject)},
		})
		out = Done{OK: true, ID: id, State: "queued", AuditSeq: seq}
		return err
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// queueExport records an export task.
func (c *Core) queueExport(ctx context.Context, tx pgx.Tx, now time.Time, tenant, kind, subject, sink, recipient, key, request, author string) (string, error) {
	id := ids.New("exp")
	_, err := tx.Exec(ctx, `INSERT INTO exports (id, tenant, kind, subject, sink, recipient_ref, recipient_key, subject_request, author, state, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 'queued', $10, $10)`,
		id, tenant, kind, nullable(subject), sink, recipient, key, nullable(request), author, now)
	if err == nil {
		_ = notifyWork(ctx, tx)
	}
	return id, err
}

// exportWriter collects the lines of an export and the highest data mark among them.
type exportWriter struct {
	c     *Core
	buf   bytes.Buffer
	lines int
	mark  string
}

func (w *exportWriter) add(schema string, obj map[string]any, extra map[string]any) error {
	line := map[string]any{"schema": "urn:botaru:schema:" + schema + ":1", "object": obj}
	for k, v := range extra {
		line[k] = v
	}
	b, err := json.Marshal(line)
	if err != nil {
		return err
	}
	w.buf.Write(b)
	w.buf.WriteByte('\n')
	w.lines++
	return nil
}

func (w *exportWriter) raise(mark string) {
	if mark != "" {
		w.mark = w.c.higherMark(w.mark, mark)
	}
}

// exportContent writes the objects of an export (LCY-2). A principal export holds only what the
// person initiated or owns and leaves out every object whose personal_subjects name another
// principal (AUD-5); a tenant export leaves out the sessions of private chats and every object
// with the personal_seen source connection (SEC-7). This build keeps no connection records.
func (c *Core) exportContent(ctx context.Context, tx pgx.Tx, tv *TenantView, kind, subject string) (*exportWriter, error) {
	tn := tv.Name
	w := &exportWriter{c: c}
	one := kind == "principal"
	// Principals with their groups. The identity links are left out: the record keeps no
	// strength of a link, and messenger ids stay out of the records (D-85).
	{
		q := `SELECT p.id, p.kind, p.state, coalesce(p.locale, ''), p.created_by,
			coalesce((SELECT array_agg(m.grp ORDER BY m.grp) FROM memberships m WHERE m.tenant = p.tenant AND m.principal = p.id AND m.state = 'active'), '{}')
			FROM principals p WHERE p.tenant = $1 AND ($2 = '' OR p.id = $2) ORDER BY p.id`
		rows, err := tx.Query(ctx, q, tn, subject)
		if err != nil {
			return nil, err
		}
		type prin struct {
			id, kind, state, locale string
			by                      *string
			groups                  []string
		}
		var ps []prin
		for rows.Next() {
			var p prin
			if err := rows.Scan(&p.id, &p.kind, &p.state, &p.locale, &p.by, &p.groups); err != nil {
				rows.Close()
				return nil, err
			}
			ps = append(ps, p)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		for _, p := range ps {
			obj := map[string]any{"id": p.id, "tenant": tv.Name, "kind": p.kind, "state": p.state}
			if p.locale != "" {
				obj["locale"] = p.locale
			}
			if p.by != nil {
				obj["created_by"] = *p.by
			}
			ms := []any{}
			for _, g := range p.groups {
				ms = append(ms, map[string]any{"subject": p.id, "group": g, "state": "active"})
			}
			obj["memberships"] = ms
			if err := w.add("principal", obj, nil); err != nil {
				return nil, err
			}
		}
	}
	// Tasks with their body and result.
	var taskIDs []string
	{
		filter := `NOT coalesce(record->'personal_seen', '[]'::jsonb) ? 'connection'`
		if one {
			filter = `initiator = $2 AND NOT EXISTS (SELECT 1 FROM jsonb_array_elements_text(coalesce(record->'personal_subjects', '[]'::jsonb)) s WHERE s <> $2)`
		}
		q := `SELECT id, type, state, coalesce(reason, ''), initiator, priority_class, idempotency_key, lease_no, attempts_spent, runs_counted, body, result, record
			FROM tasks WHERE tenant = $1 AND ` + filter + ` ORDER BY created_at, id`
		args := []any{tn}
		if one {
			args = append(args, subject)
		}
		rows, err := tx.Query(ctx, q, args...)
		if err != nil {
			return nil, err
		}
		var objs []map[string]any
		for rows.Next() {
			var id, typ, state, reason, initiator, idem string
			var prio, leaseNo, spent, runs int
			var body, result, record []byte
			if err := rows.Scan(&id, &typ, &state, &reason, &initiator, &prio, &idem, &leaseNo, &spent, &runs, &body, &result, &record); err != nil {
				rows.Close()
				return nil, err
			}
			obj := map[string]any{}
			_ = json.Unmarshal(record, &obj)
			for k, v := range map[string]any{"id": id, "tenant": tv.Name, "type": typ, "state": state, "initiator": initiator,
				"priority_class": prio, "idempotency_key": idem, "lease_no": leaseNo, "attempts_spent": spent, "runs_counted": runs} {
				obj[k] = v
			}
			if reason != "" {
				obj["reason"] = reason
			}
			for k, raw := range map[string][]byte{"body": body, "result": result} {
				if len(raw) > 0 {
					var v any
					if json.Unmarshal(raw, &v) == nil {
						obj[k] = v
					}
				}
			}
			objs = append(objs, obj)
			taskIDs = append(taskIDs, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		for _, obj := range objs {
			if m, ok := obj["data_mark"].(string); ok {
				w.raise(m)
			}
			if err := w.add("task-record", obj, nil); err != nil {
				return nil, err
			}
		}
	}
	// Approvals the person initiated, or all of the tenant.
	{
		q := `SELECT ` + approvalColumns + ` FROM approvals WHERE tenant = $1 AND ($2 = '' OR initiator = $2) ORDER BY id`
		rows, err := tx.Query(ctx, q, tn, subject)
		if err != nil {
			return nil, err
		}
		var as []*approvalRow
		for rows.Next() {
			a, err := scanApproval(rows)
			if err != nil {
				rows.Close()
				return nil, err
			}
			as = append(as, a)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		for _, a := range as {
			if err := w.add("approval", a.Record(), nil); err != nil {
				return nil, err
			}
		}
	}
	// Memory records about the person, or all of the tenant.
	{
		rows, err := tx.Query(ctx, `SELECT id, scope, subject, role, key, content, state, personal_seen, created_at, decide_by, decided_at
			FROM memory_items WHERE tenant = $1 AND ($2 = '' OR subject = $2) AND state IN ('proposed', 'active') ORDER BY created_at, id`, tn, subject)
		if err != nil {
			return nil, err
		}
		var objs []map[string]any
		for rows.Next() {
			var id, scope, subj, role, key, content, state string
			var seen bool
			var created time.Time
			var decideBy, decidedAt *time.Time
			if err := rows.Scan(&id, &scope, &subj, &role, &key, &content, &state, &seen, &created, &decideBy, &decidedAt); err != nil {
				rows.Close()
				return nil, err
			}
			obj := map[string]any{"id": id, "tenant": tv.Name, "scope": scope, "subject": subj, "role": role, "key": key,
				"content": content, "state": state, "personal_seen": seen, "created_at": fmtTime(created)}
			if decideBy != nil {
				obj["decide_by"] = fmtTime(*decideBy)
			}
			if decidedAt != nil {
				obj["decided_at"] = fmtTime(*decidedAt)
			}
			objs = append(objs, obj)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		for _, obj := range objs {
			if err := w.add("memory-item", obj, nil); err != nil {
				return nil, err
			}
		}
	}
	// Session entries: for a person only the sessions of their own conversations with nobody
	// else in them; for a tenant every session except those of private chats.
	{
		filter := `s.conversation NOT LIKE 'c:%:direct:%'`
		if one {
			filter = `s.subjects = ARRAY[$2]::text[]`
		}
		q := `SELECT s.id, coalesce(s.data_mark, ''), e.uuid, e.summary, e.content FROM sessions s JOIN session_entries e ON e.session = s.id
			WHERE s.tenant = $1 AND ` + filter + ` ORDER BY s.id, e.seq`
		args := []any{tn}
		if one {
			args = append(args, subject)
		}
		rows, err := tx.Query(ctx, q, args...)
		if err != nil {
			return nil, err
		}
		type entry struct {
			session, mark, uuid string
			summary             bool
			content             []byte
		}
		var es []entry
		for rows.Next() {
			var e entry
			if err := rows.Scan(&e.session, &e.mark, &e.uuid, &e.summary, &e.content); err != nil {
				rows.Close()
				return nil, err
			}
			es = append(es, e)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		for _, e := range es {
			var content any
			_ = json.Unmarshal(e.content, &content)
			w.raise(e.mark)
			obj := map[string]any{"uuid": e.uuid, "content": content}
			if e.summary {
				obj["summary"] = true
			}
			if err := w.add("session-entry", obj, map[string]any{"session": e.session}); err != nil {
				return nil, err
			}
		}
	}
	// File records (not their content) of the exported tasks and of the person's uploads.
	{
		q := `SELECT file_ref, origin, producer, media_type, size, data_mark, coalesce(personal_seen, ''), state, created_at, expires_at
			FROM files WHERE tenant = $1 AND origin <> 'export' AND (producer = ANY($2) OR ($3 = '' AND origin <> 'produced') OR producer = $3) ORDER BY created_at, file_ref`
		rows, err := tx.Query(ctx, q, tn, taskIDs, subject)
		if err != nil {
			return nil, err
		}
		var objs []map[string]any
		for rows.Next() {
			var ref, origin, producer, mt, mark, seen, state string
			var size int64
			var created time.Time
			var expires *time.Time
			if err := rows.Scan(&ref, &origin, &producer, &mt, &size, &mark, &seen, &state, &created, &expires); err != nil {
				rows.Close()
				return nil, err
			}
			obj := map[string]any{"file_ref": ref, "tenant": tv.Name, "origin": origin, "producer": producer, "media_type": mt,
				"size": size, "data_mark": mark, "untrusted": origin != "produced", "state": state, "created_at": fmtTime(created)}
			if seen != "" {
				obj["personal_seen"] = seen
			}
			if expires != nil {
				obj["expires_at"] = fmtTime(*expires)
			}
			w.raise(mark)
			objs = append(objs, obj)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		for _, obj := range objs {
			if err := w.add("file-record", obj, nil); err != nil {
				return nil, err
			}
		}
	}
	return w, nil
}

// runExports runs the queued export tasks, one transaction each (LCY-2). It runs in the
// erasure loop, before the erasure work of the same pass.
func (c *Core) runExports(ctx context.Context) error {
	cat, err := c.Catalog(ctx)
	if err != nil {
		return err
	}
	for {
		done := true
		err := c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
			var id, tenant, kind, sink, recipient, key string
			var subject *string
			err := tx.QueryRow(ctx, `SELECT id, tenant, kind, subject, sink, recipient_ref, recipient_key FROM exports
				WHERE state = 'queued' ORDER BY created_at, id LIMIT 1 FOR UPDATE SKIP LOCKED`).Scan(&id, &tenant, &kind, &subject, &sink, &recipient, &key)
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			done = false
			fail := func(reason string) error {
				_, err := tx.Exec(ctx, `UPDATE exports SET state = 'failed', reason = $2, updated_at = $3 WHERE id = $1`, id, reason, now)
				return err
			}
			tv := cat.Tenants[tenant]
			if tv == nil {
				return fail("not_found")
			}
			if state, _, err := tenantState(ctx, tx, tenant); err != nil {
				return err
			} else if state == "deleted" {
				return fail("gone")
			}
			// The sink is checked again: an apply may have changed it since the request.
			rec, _, err := exportSink(cat, tenant, sink)
			if err != nil {
				p, _ := AsProblem(err)
				if p == nil {
					return err
				}
				return fail(p.Reason)
			}
			if err := checkRecipient(kind, deref(subject), rec); err != nil || rec != recipient {
				return fail("export_recipient_not_subject")
			}
			w, err := c.exportContent(ctx, tx, tv, kind, deref(subject))
			if err != nil {
				return err
			}
			mark := w.mark
			if mark == "" {
				mark = defaultAttachmentClass
			}
			// The sink takes nothing above its ceiling (SEC-9).
			raw := cat.RawGlobals[manifest.Key{Kind: "Sink", Tenant: tenant, Name: sink}]
			spec, _ := raw["spec"].(map[string]any)
			if ceiling := mstr(spec, "ceiling"); ceiling != "" && !c.markWithin(mark, ceiling) {
				return fail("data_class_exceeded")
			}
			sealed, err := sealExport(key, w.buf.Bytes())
			if err != nil {
				return fail("input_schema_invalid")
			}
			sum := sha256.Sum256(sealed)
			ref := newFileRef()
			if _, err := tx.Exec(ctx, `INSERT INTO files (file_ref, tenant, origin, producer, media_type, size, sha256, data_mark, state, content, created_at, expires_at, filename)
				VALUES ($1, $2, 'export', $3, 'application/octet-stream', $4, $5, $6, 'stored', $7, $8, $9, $10)`,
				ref, tenant, id, len(sealed), hex.EncodeToString(sum[:]), mark, sealed, now, fileExpiry(now, tv.Spec.Retention.Files, ""), id+".jsonl.hpke"); err != nil {
				return err
			}
			body := map[string]any{"form": "export", "export": id, "kind": kind, "file_ref": ref, "lines": w.lines,
				"encryption": "hpke-base-x25519-sha256-chacha20poly1305"}
			if subject != nil {
				body["subject"] = *subject
			}
			if _, err := c.planDelivery(ctx, tx, now, plannedDelivery{
				Tenant: tenant, Kind: "export", Addr: "sink:" + sink, TargetKind: "sink", TargetID: sink,
				ExecKind: "sink", ExecName: sink, Body: body, DataMark: mark, DataOrigin: "code",
			}); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE exports SET state = 'done', file_ref = $2, lines = $3, updated_at = $4 WHERE id = $1`, id, ref, w.lines, now); err != nil {
				return err
			}
			_, err = c.appendAudit(ctx, tx, now, Audit{
				Tenant: tenant, Kind: "command", Actor: Actor{Principal: "system", Via: "core"},
				Command: "export.run", Target: &Target{Kind: "export", ID: id}, OK: true,
				After: map[string]any{"file_ref": ref, "lines": w.lines, "sink": sink},
			})
			return err
		})
		if err != nil {
			return fmt.Errorf("export: %w", err)
		}
		if done {
			return nil
		}
	}
}
