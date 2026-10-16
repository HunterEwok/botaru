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
	"path"
	"slices"
	"strings"
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
// reference (ART-1). http.DetectContentType follows the WHATWG MIME Sniffing standard; inside
// text/*, where the bytes cannot tell a table from plain text, the file name's extension
// refines text/plain to one of a few fixed text types, never to an active one (ART-5).
func sniffType(content []byte, filename string) string {
	mt, _, err := mime.ParseMediaType(http.DetectContentType(content))
	if err != nil {
		return "application/octet-stream"
	}
	if mt == "text/plain" {
		if refined, ok := textByExtension[strings.ToLower(path.Ext(filename))]; ok {
			return refined
		}
	}
	return mt
}

// baseName is the last element of a file name, without folders; "" for none.
func baseName(name string) string {
	if name == "" {
		return ""
	}
	return path.Base(strings.ReplaceAll(name, "\\", "/"))
}

// textByExtension are the text types an extension refines text/plain to (ART-5).
var textByExtension = map[string]string{
	".csv": "text/csv", ".tsv": "text/tab-separated-values", ".md": "text/markdown", ".markdown": "text/markdown",
}

// activeMediaType says whether a media type is an active format that is never handed out and
// cannot be declared in result_media_types (ART-5): HTML, XHTML, SVG, JavaScript and every XML
// type, as a browser may run what XML carries.
func activeMediaType(mt string) bool {
	switch mt {
	case "text/html", "application/xhtml+xml", "image/svg+xml", "text/javascript", "application/javascript",
		"application/x-javascript", "text/ecmascript", "application/ecmascript", "text/xml", "application/xml":
		return true
	}
	return strings.HasSuffix(mt, "+xml")
}

// FilesConfig holds the farm ceilings of file sizes from the deployment settings (ART-4). A
// tenant's limits only lower them.
type FilesConfig struct {
	// MaxFileBytes is the ceiling of one file; MaxFileBytes (32 MiB, the store's ceiling) by
	// default and at most.
	MaxFileBytes int64 `yaml:"max_file_bytes"`
	// MaxTaskFilesBytes is the ceiling of all files of one task; 256 MiB by default.
	MaxTaskFilesBytes int64 `yaml:"max_task_files_bytes"`
}

func (f *FilesConfig) defaults() {
	if f.MaxFileBytes <= 0 || f.MaxFileBytes > MaxFileBytes {
		f.MaxFileBytes = MaxFileBytes
	}
	if f.MaxTaskFilesBytes <= 0 {
		f.MaxTaskFilesBytes = 256 << 20
	}
}

// fileLimits are the limits of one file and of the files of one task in a tenant: the farm
// ceiling, lowered by the tenant's limits (ART-4).
func (c *Core) fileLimits(tv *TenantView) (file, task int64) {
	file, task = c.cfg.Files.MaxFileBytes, c.cfg.Files.MaxTaskFilesBytes
	if l := tv.Spec.Limits.MaxFileBytes; l > 0 && l < file {
		file = l
	}
	if l := tv.Spec.Limits.MaxTaskFilesBytes; l > 0 && l < task {
		task = l
	}
	return file, task
}

// contentDays is the default period of the retention class content
// (spec/model/retention-classes.yaml, RET-1).
const contentDays = 30

// fileExpiry is the smaller of the tenant file period and the type's result_ttl (ART-1), else the
// default period of class content.
func fileExpiry(now time.Time, tenantFiles, resultTTL string) any {
	var out time.Duration
	for _, s := range []string{tenantFiles, resultTTL} {
		if d, ok := parseDuration(s); ok && d > 0 && (out == 0 || d < out) {
			out = d
		}
	}
	if out == 0 {
		// Without a tenant period a file keeps the default period of class content (RET-2).
		out = contentDays * 24 * time.Hour
	}
	return now.Add(out)
}

// FilePut stores a file and its record (ART-1, ART-3, ART-6). A channel puts a person's
// attachment without a lease, on behalf of the person: the file is an upload, marked at least
// the attachment class and at most what the person may see. A worker puts a file it produced
// under the lease of an attempt or a tool call: the mark is not below the task's, and the core
// writes the provenance block (PRV-2).
func (c *Core) FilePut(ctx context.Context, w *Worker, lease string, ob *OnBehalf, req FilePutRequest, content []byte) (*FilePutResult, error) {
	tv, cat, err := c.requireTenant(ctx, w.Tenant)
	if err != nil {
		return nil, err
	}
	// A file above the limit is refused whole: no record and nothing truncated (ART-4).
	maxFile, maxTask := c.fileLimits(tv)
	if int64(len(content)) > maxFile {
		return nil, refuse("file_too_large", "the file is larger than %d bytes", maxFile)
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
			// The same checks as an activity of the channel: an accepted hello, a channel of
			// the tenant and an identity of the channel's kind (IDN-2).
			if w.State != "active" && w.State != "draining" {
				return refuse("not_ready", "send hello first")
			}
			ch, ok := tv.Channels[w.Target]
			if !ok || ch.IdentityKind == "" || !strings.HasPrefix(ob.Identity, ch.IdentityKind+":") {
				return refuse("forbidden", "the identity kind does not match the channel driver")
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
				// A job produces files; a model runtime does not (P1 file.put).
				if w.Kind != "job" {
					return refuse("forbidden", "only a job worker produces a file under an attempt")
				}
			case "toolcall":
				toolcall = l.WorkID
				var placement string
				if err := tx.QueryRow(ctx, `SELECT task, target_placement FROM toolcalls WHERE id = $1`, l.WorkID).Scan(&task, &placement); err != nil {
					return err
				}
				// Only a sandbox toolhost produces a file under a tool call (P1 file.put).
				if w.Placement != "sandbox" && placement != "sandbox" {
					return refuse("forbidden", "only a sandbox toolhost produces a file under a tool call")
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
			// A file an ingest job brings in is ingested, not produced (ART-6). The catalog
			// resources and their personal mark are not in this version, so the core cannot
			// declare personal data for an ingested file yet.
			if typ == "kb.ingest.v1" || typ == "data.arrived.v1" {
				origin = "ingested"
			}
			expires = fileExpiry(now, tv.Spec.Retention.Files, cat.TaskTypes[typ].ResultTTL)
			// The files of one task together stay within the task limit, under a task lock so
			// two puts do not both pass (ART-4).
			if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(7300113, hashtext($1))`, task); err != nil {
				return err
			}
			var taskUsed int64
			if err := tx.QueryRow(ctx, `SELECT coalesce(sum(size), 0) FROM files WHERE tenant = $1 AND producer = $2 AND state <> 'purged'`, w.Tenant, task).Scan(&taskUsed); err != nil {
				return err
			}
			if taskUsed+int64(len(content)) > maxTask {
				return refuse("file_too_large", "the files of the task would pass %d bytes", maxTask)
			}
			if provenance, err = c.fileProvenance(ctx, tx, now, task, toolcall); err != nil {
				return err
			}
		default:
			return refuse("fenced", "no lease token")
		}
		// The storage ceiling of the tenant's files, under a tenant lock so two puts do not both
		// pass (CAP-1).
		if max := tv.Spec.Limits.MaxStorageBytes; max > 0 {
			if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(7300112, hashtext($1))`, w.Tenant); err != nil {
				return err
			}
			var used int64
			if err := tx.QueryRow(ctx, `SELECT coalesce(sum(size), 0) FROM files WHERE tenant = $1 AND content IS NOT NULL`, w.Tenant).Scan(&used); err != nil {
				return err
			}
			if used+int64(len(content)) > max {
				return refuse("over_capacity", "the tenant storage ceiling is reached")
			}
		}
		var prov []byte
		if provenance != nil {
			prov, _ = json.Marshal(provenance)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO files (file_ref, tenant, origin, producer, media_type, size, sha256, data_mark, personal_seen, state, provenance, content, created_at, expires_at, filename)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 'stored', $10, $11, $12, $13, $14)`,
			ref, w.Tenant, origin, producer, sniffType(content, req.Filename), len(content), hex.EncodeToString(sum[:]), mark, nullable(personal), prov, content, now, expires, nullable(baseName(req.Filename))); err != nil {
			return err
		}
		out = FilePutResult{FileRef: ref}
		return c.appendEvent(ctx, tx, now, w.Tenant, Event{Type: "file.stored", Subject: ref, Data: map[string]any{
			"file_ref": ref, "origin": origin, "data_mark": mark, "worker": w.ID,
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
	Ref, Tenant, Origin, Producer, MediaType, Mark, State, PersonalSeen string
	Size                                                                int64
	Expires                                                             *time.Time
}

func loadFile(ctx context.Context, q store.Querier, tenant, ref string) (*fileRow, error) {
	var f fileRow
	err := q.QueryRow(ctx, `SELECT file_ref, tenant, origin, producer, media_type, data_mark, state, coalesce(personal_seen, ''), size, expires_at FROM files WHERE file_ref = $1 AND tenant = $2`,
		ref, tenant).Scan(&f.Ref, &f.Tenant, &f.Origin, &f.Producer, &f.MediaType, &f.Mark, &f.State, &f.PersonalSeen, &f.Size, &f.Expires)
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
	tv, cat, err := c.requireTenant(ctx, w.Tenant)
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
		// An export file exists only for its export delivery: an attempt or a tool call never
		// sees it (LCY-2, SEC-10).
		if f == nil || f.Origin == "export" && l.Kind != "delivery" {
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
			var recipient, dmark, dtask *string
			var execKind, execName, dkind string
			var named bool
			// A delivery hands out only the files its body names.
			if err := tx.QueryRow(ctx, `SELECT recipient, data_mark, exec_kind, exec_name, position($2 in coalesce(body::text, '')) > 0, task, kind FROM deliveries WHERE id = $1`,
				l.WorkID, `"`+f.Ref+`"`).Scan(&recipient, &dmark, &execKind, &execName, &named, &dtask, &dkind); err != nil {
				return err
			}
			if !named {
				return refuse("not_found", "no such file")
			}
			if err := c.mediaAllowed(ctx, tx, cat, dkind, deref(dtask), f); err != nil {
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
		if task != "" && l.Kind == "attempt" {
			// A seat attempt: MGR-25, MGR-30 and the read log (MGR-28).
			if err := c.seatFileRead(ctx, tx, now, w.Tenant, task, l.LeaseNo, f); err != nil {
				return err
			}
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

// mediaAllowed checks a file a delivery hands out to a person or a sink (ART-5): an export
// delivery hands out only the export file, any other only a result file of its own task or of
// a task of its chain (an ancestor, a finished descendant, an earlier task of the tree with the
// same initiator) whose sniffed type the producing task's type lists in
// result_media_types and that is not an active format.
// Another file is refused with media_type_not_allowed; the task outcome does not change.
func (c *Core) mediaAllowed(ctx context.Context, tx pgx.Tx, cat *Catalog, kind, task string, f *fileRow) error {
	if kind == "export" || f.Origin == "export" {
		if kind == "export" && f.Origin == "export" {
			return nil
		}
		return refuse("media_type_not_allowed", "an export file goes only with its export delivery")
	}
	if task == "" || f.Origin != "produced" || activeMediaType(f.MediaType) {
		return refuse("media_type_not_allowed", "the file type %s is not handed out", f.MediaType)
	}
	// A result file of the delivery's own task, of one of its ancestors, of a finished
	// descendant, or of an earlier task of its tree with the same initiator, such as a chain's
	// child whose outcome a continuation reports; the producer's type lists it. A task of the
	// tree another person started is not handed out.
	var typ string
	err := tx.QueryRow(ctx, `WITH RECURSIVE up(id, n) AS (
			SELECT record->>'parent', 1 FROM tasks WHERE id = $2
			UNION ALL SELECT t.record->>'parent', up.n + 1 FROM tasks t JOIN up ON t.id = up.id WHERE up.n < 16),
		pup(id, n) AS (
			SELECT record->>'parent', 1 FROM tasks WHERE id = $1
			UNION ALL SELECT t.record->>'parent', pup.n + 1 FROM tasks t JOIN pup ON t.id = pup.id WHERE pup.n < 16)
		SELECT p.type FROM tasks p JOIN tasks d ON d.id = $2 AND d.tenant = p.tenant
		WHERE p.id = $1 AND (p.id = d.id
			OR p.id IN (SELECT id FROM up WHERE id IS NOT NULL)
			OR d.id IN (SELECT id FROM pup WHERE id IS NOT NULL) AND p.state IN ('completed', 'failed', 'canceled')
			OR coalesce(p.record->>'root', p.id) = coalesce(d.record->>'root', d.id) AND p.initiator = d.initiator AND p.created_at <= d.created_at)`,
		f.Producer, task).Scan(&typ)
	if errors.Is(err, pgx.ErrNoRows) {
		return refuse("media_type_not_allowed", "the file is not a result of this task's chain")
	}
	if err != nil {
		return err
	}
	if !slices.Contains(cat.TaskTypes[typ].ResultMediaTypes, f.MediaType) {
		return refuse("media_type_not_allowed", "the file type %s is not in the result types of %s", f.MediaType, typ)
	}
	return nil
}

// taintByFile checks a file read for a task: the initiator may see its mark (a person's task
// only), and the task becomes untrusted and takes the higher mark (ART-2, ART-3, INJ-13). A
// person's upload marks the task personal_seen human_file, a file with the flag inherited
// (AUD-5).
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
	if f.Origin == "upload" {
		var kind string
		if err := tx.QueryRow(ctx, `SELECT kind FROM principals WHERE tenant = $1 AND id = $2`, tv.Name, f.Producer).Scan(&kind); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if kind == "human" {
			if err := addPersonalSeen(ctx, tx, task, "human_file"); err != nil {
				return err
			}
		}
	}
	if f.PersonalSeen != "" {
		if err := addPersonalSeen(ctx, tx, task, "inherited"); err != nil {
			return err
		}
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
