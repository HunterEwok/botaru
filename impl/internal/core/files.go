// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"mime"
	"net/http"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/store"
)

// MaxFileBytes is the largest file the core stores; the deployment may set a lower body limit.
const MaxFileBytes = 32 << 20

// defaultAttachmentClass is the lowest mark of an uploaded file (ART-3).
const defaultAttachmentClass = "internal"

// FilePutRequest is p1-file-put-request.
type FilePutRequest struct {
	Filename  string `json:"filename,omitempty"`
	MediaType string `json:"media_type"`
	DataMark  string `json:"data_mark,omitempty"`
}

// FilePutResult is p1-file-put-response.
type FilePutResult struct {
	FileRef string `json:"file_ref"`
}

// FileGetRequest is p1-file-get-request.
type FileGetRequest struct {
	FileRef string `json:"file_ref"`
}

// FileGetResult is p1-file-get-response with the bytes, which the binding sends as the body.
type FileGetResult struct {
	FileRef   string `json:"file_ref"`
	MediaType string `json:"media_type"`
	Size      int64  `json:"size"`
	DataMark  string `json:"data_mark"`
	Content   []byte `json:"-"`
}

func newFileRef() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return "file:" + base64.RawURLEncoding.EncodeToString(b)
}

// sniffType is the media type of the bytes, without parameters: the declared type is only for
// reference (ART-1).
func sniffType(content []byte) string {
	mt, _, err := mime.ParseMediaType(http.DetectContentType(content))
	if err != nil {
		return "application/octet-stream"
	}
	return mt
}

// fileExpiry is the smaller of the tenant file period and the type's result_ttl (ART-1).
func fileExpiry(now time.Time, tenantFiles, resultTTL string) any {
	var out time.Duration
	for _, s := range []string{tenantFiles, resultTTL} {
		if d, ok := parseDuration(s); ok && d > 0 && (out == 0 || d < out) {
			out = d
		}
	}
	if out == 0 {
		return nil
	}
	return now.Add(out)
}

// FilePut stores a file and its record (ART-1, ART-3, ART-6). A channel puts a person's
// attachment without a lease, on behalf of the person: the file is an upload, marked at least
// the attachment class and at most what the person may see. A worker puts a file it produced
// under the lease of an attempt or a tool call: the mark is not below the task's, and the core
// writes the provenance block (PRV-2).
func (c *Core) FilePut(ctx context.Context, w *Worker, lease string, ob *OnBehalf, req FilePutRequest, content []byte) (*FilePutResult, error) {
	if len(content) > MaxFileBytes {
		return nil, refuse("over_capacity", "the file is larger than the core takes")
	}
	tv, cat, err := c.requireTenant(ctx, w.Tenant)
	if err != nil {
		return nil, err
	}
	if req.DataMark != "" {
		if _, ok := c.markOrder(req.DataMark); !ok {
			return nil, refuse("input_schema_invalid", "unknown data class %s", req.DataMark)
		}
	}
	sum := sha256.Sum256(content)
	ref := newFileRef()
	var out FilePutResult
	err = c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		var origin, producer, mark, personal string
		var provenance any
		expires := fileExpiry(now, tv.Spec.Retention.Files, "")
		switch {
		case lease == "" && w.Kind == "channel":
			if ob == nil {
				return refuse("forbidden", "a channel puts a file on behalf of a person")
			}
			principal, err := principalByIdentity(ctx, tx, w.Tenant, ob.Identity)
			if errors.Is(err, pgx.ErrNoRows) {
				return refuse("forbidden", "the person is not a principal of the tenant")
			}
			if err != nil {
				return err
			}
			p, err := loadPrincipal(ctx, tx, now, w.Tenant, principal)
			if err != nil {
				return err
			}
			if p.State != "active" {
				return refuse("person_inactive", "the person is not active")
			}
			groups := groupClosure(tv, p.Groups)
			mark = c.higherMark(defaultAttachmentClass, req.DataMark)
			if !hasData(tv, groups, mark) {
				return refuse("data_class_exceeded", "the file mark is above what the person may see")
			}
			origin, producer = "upload", principal
			var declared *bool
			if err := tx.QueryRow(ctx, `SELECT uploads_personal FROM principals WHERE tenant = $1 AND id = $2`, w.Tenant, principal).Scan(&declared); err != nil {
				return err
			}
			if p.Kind == "service" && ((declared != nil && *declared) || (declared == nil && tv.Spec.Kind == "production")) {
				personal = "declared"
			}
		case lease != "":
			l, _, err := c.heldLease(ctx, tx, w, lease, "file.put", nil)
			if err != nil {
				return err
			}
			if !l.Until.After(now) || l.State != "active" {
				return refuse("fenced", "the lease is no longer active")
			}
			task, toolcall := l.WorkID, ""
			switch l.Kind {
			case "attempt":
			case "toolcall":
				toolcall = l.WorkID
				if err := tx.QueryRow(ctx, `SELECT task FROM toolcalls WHERE id = $1`, l.WorkID).Scan(&task); err != nil {
					return err
				}
			default:
				return refuse("forbidden", "a file is produced under an attempt or a tool call")
			}
			var typ string
			var rec []byte
			if err := tx.QueryRow(ctx, `SELECT type, record FROM tasks WHERE id = $1`, task).Scan(&typ, &rec); err != nil {
				return err
			}
			var r struct {
				DataMark string `json:"data_mark"`
			}
			_ = json.Unmarshal(rec, &r)
			mark = c.higherMark(r.DataMark, req.DataMark)
			origin, producer = "produced", task
			expires = fileExpiry(now, tv.Spec.Retention.Files, cat.TaskTypes[typ].ResultTTL)
			if provenance, err = c.fileProvenance(ctx, tx, now, task, toolcall); err != nil {
				return err
			}
		default:
			return refuse("fenced", "no lease token")
		}
		var prov []byte
		if provenance != nil {
			prov, _ = json.Marshal(provenance)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO files (file_ref, tenant, origin, producer, media_type, size, sha256, data_mark, personal_seen, state, provenance, content, created_at, expires_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 'stored', $10, $11, $12, $13)`,
			ref, w.Tenant, origin, producer, sniffType(content), len(content), hex.EncodeToString(sum[:]), mark, nullable(personal), prov, content, now, expires); err != nil {
			return err
		}
		out = FilePutResult{FileRef: ref}
		return c.appendEvent(ctx, tx, now, w.Tenant, Event{Type: "file.stored", Subject: ref, Data: map[string]any{
			"file_ref": ref, "origin": origin, "data_mark": mark,
		}})
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// fileProvenance is the provenance block of a produced file, from the records only (PRV-2):
// the task, its fingerprint when the attempt has one, the files attached to the task, the tool
// calls of the task so far, and the time.
func (c *Core) fileProvenance(ctx context.Context, tx pgx.Tx, now time.Time, task, toolcall string) (map[string]any, error) {
	var body []byte
	var fingerprint *string
	if err := tx.QueryRow(ctx, `SELECT body, (SELECT attempt->>'fingerprint' FROM leases WHERE work_id = $1 AND kind = 'attempt' ORDER BY lease_no DESC LIMIT 1)
		FROM tasks WHERE id = $1`, task).Scan(&body, &fingerprint); err != nil {
		return nil, err
	}
	inputs := []any{}
	var b struct {
		Attachments []struct {
			FileRef string `json:"file_ref"`
		} `json:"attachments"`
	}
	_ = json.Unmarshal(body, &b)
	for _, a := range b.Attachments {
		if a.FileRef != "" {
			inputs = append(inputs, map[string]any{"kind": "file", "id": a.FileRef})
		}
	}
	calls := []string{}
	rows, err := tx.Query(ctx, `SELECT id FROM toolcalls WHERE task = $1 ORDER BY created_at, id`, task)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		calls = append(calls, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if toolcall != "" && !slices.Contains(calls, toolcall) {
		calls = append(calls, toolcall)
	}
	p := map[string]any{"task": task, "inputs": inputs, "toolcalls": calls, "at": fmtTime(now)}
	if fingerprint != nil {
		p["fingerprint"] = *fingerprint
	}
	return p, nil
}

// fileRow is a file record as the handout reads it.
type fileRow struct {
	Ref, Tenant, Origin, Producer, MediaType, Mark, State string
	Size                                                  int64
	Expires                                               *time.Time
}

func loadFile(ctx context.Context, q store.Querier, tenant, ref string) (*fileRow, error) {
	var f fileRow
	err := q.QueryRow(ctx, `SELECT file_ref, tenant, origin, producer, media_type, data_mark, state, size, expires_at FROM files WHERE file_ref = $1 AND tenant = $2`,
		ref, tenant).Scan(&f.Ref, &f.Tenant, &f.Origin, &f.Producer, &f.MediaType, &f.Mark, &f.State, &f.Size, &f.Expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &f, err
}

// FileGet hands out the bytes of a file under the lease of an attempt, a tool call or a
// delivery (ART-3, PRM-9, TEN-7). A file of another tenant, an unknown one or one whose mark
// is above the recipient's right is not_found; an expired or purged one is gone. An attempt
// that reads a file is untrusted from then on and its task takes the file's mark (ART-2).
func (c *Core) FileGet(ctx context.Context, w *Worker, lease string, req FileGetRequest) (*FileGetResult, error) {
	tv, _, err := c.requireTenant(ctx, w.Tenant)
	if err != nil {
		return nil, err
	}
	var out FileGetResult
	err = c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		l, _, err := c.heldLease(ctx, tx, w, lease, "file.get", nil)
		if err != nil {
			return err
		}
		if !l.Until.After(now) || l.State != "active" {
			return refuse("fenced", "the lease is no longer active")
		}
		f, err := loadFile(ctx, tx, w.Tenant, req.FileRef)
		if err != nil {
			return err
		}
		if f == nil {
			return refuse("not_found", "no such file")
		}
		if f.State != "stored" || (f.Expires != nil && !f.Expires.After(now)) {
			return refuse("gone", "the file is no longer kept")
		}
		task := ""
		switch l.Kind {
		case "attempt":
			task = l.WorkID
		case "toolcall":
			if err := tx.QueryRow(ctx, `SELECT task FROM toolcalls WHERE id = $1`, l.WorkID).Scan(&task); err != nil {
				return err
			}
		case "delivery":
			var recipient, dmark *string
			var execKind, execName string
			if err := tx.QueryRow(ctx, `SELECT recipient, data_mark, exec_kind, exec_name FROM deliveries WHERE id = $1`, l.WorkID).Scan(&recipient, &dmark, &execKind, &execName); err != nil {
				return err
			}
			// The delivery hands out a file no higher than its own mark, which the claim
			// checked against the recipient and the ceiling.
			if deref(dmark) == "" || !c.markWithin(f.Mark, *dmark) {
				return refuse("not_found", "no such file")
			}
			if recipient != nil {
				reason, err := c.accessLost(ctx, tx, now, tv, *recipient, "", f.Mark)
				if err != nil {
					return err
				}
				if reason != "" {
					return refuse("not_found", "no such file")
				}
			}
			if ceiling := executorCeiling(tv, execKind, execName); ceiling != "" && !c.markWithin(f.Mark, ceiling) {
				return refuse("not_found", "no such file")
			}
		default:
			return refuse("forbidden", "no file under this kind of work")
		}
		if task != "" {
			if err := c.taintByFile(ctx, tx, now, tv, task, f); err != nil {
				return err
			}
		}
		var content []byte
		if err := tx.QueryRow(ctx, `SELECT content FROM files WHERE file_ref = $1`, f.Ref).Scan(&content); err != nil {
			return err
		}
		out = FileGetResult{FileRef: f.Ref, MediaType: f.MediaType, Size: f.Size, DataMark: f.Mark, Content: content}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// taintByFile checks a file read for a task: the initiator may see its mark (a person's task
// only), and the task becomes untrusted and takes the higher mark (ART-2, ART-3, INJ-13).
func (c *Core) taintByFile(ctx context.Context, tx pgx.Tx, now time.Time, tv *TenantView, task string, f *fileRow) error {
	var initiator string
	var rec []byte
	if err := tx.QueryRow(ctx, `SELECT initiator, record FROM tasks WHERE id = $1 FOR UPDATE`, task).Scan(&initiator, &rec); err != nil {
		return err
	}
	if initiator != "system" {
		p, err := loadPrincipal(ctx, tx, now, tv.Name, initiator)
		if err != nil {
			return err
		}
		if !hasData(tv, groupClosure(tv, p.Groups), f.Mark) {
			return refuse("not_found", "no such file")
		}
	} else if !hasData(tv, groupClosure(tv, []string{"system-ops"}), f.Mark) {
		return refuse("not_found", "no such file")
	}
	var r map[string]any
	if err := json.Unmarshal(rec, &r); err != nil {
		return err
	}
	mark, _ := r["data_mark"].(string)
	newMark := c.higherMark(mark, f.Mark)
	if r["untrusted_seen"] == true && newMark == mark {
		return nil
	}
	_, err := tx.Exec(ctx, `UPDATE tasks SET record = record || jsonb_build_object('untrusted_seen', true, 'untrusted_source', coalesce(record->>'untrusted_source', 'file'), 'data_mark', $2::text), updated_at = $3 WHERE id = $1`,
		task, newMark, now)
	return err
}
