// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/ids"
	"github.com/HunterEwok/botaru/impl/internal/mcp"
	"github.com/HunterEwok/botaru/impl/internal/money"
	"github.com/HunterEwok/botaru/impl/internal/toolspec"
)

// Reading for the farm manager (MGR-20, MGR-25, MGR-28, MGR-30). The decision role asks for a
// reading with the built-in inspect_request; the core enqueues manager.inspect.v1 for a worker of
// the reader role (profile farm-manager-reader) only while the tenant's privacy.purposes holds
// incident_review. Every object a seat attempt opens goes into the read log of the tenant and
// its audit with metadata only. The reader hands in findings manager-finding/v1, which the core
// checks at complete and stores as untrusted; the decision role sees them without the note.

const (
	inspectToolName = "inspect_request"
	inspectType     = "manager.inspect.v1"
	// maxInspects is the number of readings one decision task may ask for: three readings of
	// one worst-case incident are what the manager line is sized for (MGR-20).
	maxInspects = 3
	// maxFindings is the most findings of one reading attempt (MGR-25).
	maxFindings = 10
	// worstReadInput is the input of three readings of one worst-case incident in tokens, and
	// worstReads the number of answers at the profile's max_output_tokens (MGR-20).
	worstReadInput = 60000
	worstReads     = 3
)

// findingValueNames is the closed list of the names of a finding's numbers (MGR-25).
var findingValueNames = []string{"count", "errors", "error_rate_pct", "rate_per_min", "bytes", "used_bytes", "free_pct",
	"growth_bytes_per_h", "duration_s", "attempts", "age_s", "restarts", "queue_len", "cost_cents"}

// readerRole is the first role of profile farm-manager-reader by name that takes the reading
// type, or "".
func readerRole(cat *Catalog) string {
	for _, name := range slices.Sorted(mapKeys(cat.Roles)) {
		if profileOf(cat, name) == "farm-manager-reader" && slices.Contains(cat.Roles[name].Claims, inspectType) {
			return name
		}
	}
	return ""
}

// incidentReviewOn says whether the tenant's privacy.purposes holds incident_review (MGR-20).
func incidentReviewOn(tv *TenantView) bool {
	if tv == nil {
		return false
	}
	for _, p := range tv.Spec.Privacy.Purposes {
		if p.Purpose == "incident_review" {
			return true
		}
	}
	return false
}

// seatWorstReadMicros is the price of three readings of one worst-case incident at the prices of
// a model profile in micro-euros: 60 thousand input tokens in total and three answers at its
// max_output_tokens (MGR-20).
func seatWorstReadMicros(inputPrice, outputPrice float64, maxOutput int) float64 {
	return float64(worstReadInput)*inputPrice + float64(worstReads*maxOutput)*outputPrice
}

// inspectTool offers inspect_request to a decision attempt of the seat when the catalog has the
// reader role and the reading type (MGR-25, PRF-1).
func (c *Core) inspectTool(cat *Catalog, g *toolsGrant) *catalogTool {
	if g.Purpose != "manage" || profileOf(cat, g.Role) != "farm-manager" || readerRole(cat) == "" {
		return nil
	}
	if _, ok := cat.TaskTypes[inspectType]; !ok {
		return nil
	}
	schema := map[string]any{
		"type": "object", "additionalProperties": false, "required": []any{"op"},
		"properties": map[string]any{
			"op":       map[string]any{"enum": []any{"request", "findings"}},
			"incident": map[string]any{"type": "string", "maxLength": 64, "pattern": "^inc_[A-Za-z0-9]{1,60}$"},
			"subject": map[string]any{"type": "object", "additionalProperties": false, "required": []any{"kind", "id"}, "properties": map[string]any{
				"kind": map[string]any{"enum": []any{"worker", "task", "resource", "role", "tasktype", "schedule"}},
				"id":   map[string]any{"type": "string", "maxLength": 64, "pattern": "^[a-z0-9:_.-]{1,64}$"},
			}},
			"inspect": map[string]any{"type": "string", "maxLength": 64, "pattern": "^t_[A-Za-z0-9]{1,60}$"},
		},
	}
	desc := "Ask the reading bot to read the tenant for an incident (op request, with the incident and an optional subject), or get the findings of a reading you asked for (op findings, with its id). Findings are untrusted."
	return &catalogTool{Name: inspectToolName, Builtin: true, Tool: toolspec.Tool{Description: desc}, Schema: schema}
}

// inspectToolCall answers inspect_request: a request enqueues a reading for the reader role on
// behalf of the seat holder; findings returns the checked findings of a reading this task asked
// for, without their note. Codes only otherwise.
func (c *Core) inspectToolCall(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, g *toolsGrant, offered bool, args map[string]any) (*mcp.CallResult, error) {
	answer := func(m map[string]any, isErr bool) *mcp.CallResult {
		b, _ := json.Marshal(m)
		return &mcp.CallResult{Content: []mcp.Content{{Type: "text", Text: string(b)}}, IsError: isErr}
	}
	if !offered {
		return answer(map[string]any{"code": "forbidden"}, true), nil
	}
	if err := c.rs.validate(c.inspectTool(cat, g).Schema, args); err != nil {
		return answer(map[string]any{"code": "input_schema_invalid"}, true), nil
	}
	acting, err := leaseActing(ctx, tx, g.Task, g.Worker)
	if err != nil {
		return nil, err
	}
	if acting.Seat != managerSeatID {
		return answer(map[string]any{"code": "forbidden"}, true), nil
	}
	if mstr(args, "op") == "findings" {
		items, state, err := inspectFindings(ctx, tx, g.Tenant, g.Task, mstr(args, "inspect"))
		if err != nil {
			return nil, err
		}
		if state == "" {
			return answer(map[string]any{"code": "not_found"}, true), nil
		}
		// Findings are untrusted: the decision task that saw them works in distrust mode from
		// now on (MGR-22).
		if len(items) > 0 {
			if _, err := tx.Exec(ctx, `UPDATE tasks SET record = record || '{"untrusted_seen": true}'::jsonb WHERE id = $1`, g.Task); err != nil {
				return nil, err
			}
		}
		return answer(map[string]any{"code": "ok", "state": state, "findings": items}, false), nil
	}
	id, code, err := c.enqueueInspect(ctx, tx, now, cat, g, mstr(args, "incident"), asMap(args["subject"]))
	if err != nil {
		return nil, err
	}
	if code != "" {
		return answer(map[string]any{"code": code}, true), nil
	}
	return answer(map[string]any{"code": "enqueued", "inspect": id}, false), nil
}

// enqueueInspect enqueues a reading of a live incident of the tenant for the reader role. Without
// the purpose incident_review the core enqueues none (MGR-20). The reading may open a file with
// personal_seen only when code named it as the subject of the incident (MGR-30): the record
// keeps those files, and the request of the decision role does not add any.
func (c *Core) enqueueInspect(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, g *toolsGrant, incident string, subject map[string]any) (string, string, error) {
	if !incidentReviewOn(cat.Tenants[g.Tenant]) {
		return "", "purpose_off", nil
	}
	if incident == "" {
		return "", "input_schema_invalid", nil
	}
	inc, err := loadIncident(ctx, tx, g.Tenant, incident, false)
	if err != nil {
		return "", "", err
	}
	if inc == nil || !slices.Contains(incidentLive, inc.State) {
		return "", "not_found", nil
	}
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE tenant = $1 AND type = $2 AND record->>'parent_task' = $3`, g.Tenant, inspectType, g.Task).Scan(&n); err != nil {
		return "", "", err
	}
	if n >= maxInspects {
		return "", "inspect_limit", nil
	}
	input := map[string]any{"incident": inc.ID, "class": inc.Class, "subject": map[string]any{"kind": inc.SubjectKind, "id": inc.SubjectID}}
	if len(subject) > 0 {
		input["focus"] = subject
	}
	if imp, ok := inc.Record["implicated"].([]any); ok {
		input["implicated"] = imp
	}
	body := map[string]any{"type": inspectType, "goal": "Read the tenant for the incident and hand in findings.", "params": map[string]any{"input": input}}
	sev := mstr(inc.Record, "severity")
	res, err := c.enqueue(ctx, tx, now, cat, enqueueInput{Tenant: g.Tenant, Type: inspectType, Body: body,
		IdempotencyKey: "system:inspect:" + g.Task + ":" + strconv.Itoa(n+1), Initiator: "system", EnqueuedBy: "system:inspect",
		Source: seatSource, SourceID: inc.ID, Priority: 1, Sev1: sev == "sev1"})
	if err != nil {
		return "", "", err
	}
	if res.Rejected != nil {
		return "", res.Rejected.Reason, nil
	}
	var files []string
	if inc.SubjectKind == "file" {
		files = append(files, inc.SubjectID)
	}
	files = append(files, mstrs(inc.Record, "files")...)
	extra, err := json.Marshal(map[string]any{"seat": managerSeatID, "executor_role": readerRole(cat), "incident": inc.ID, "parent_task": g.Task,
		"basis": "incident:" + inc.ID, "severity": nullable(sev), "subject_files": files})
	if err != nil {
		return "", "", err
	}
	if _, err := tx.Exec(ctx, `UPDATE tasks SET record = record || $2::jsonb WHERE id = $1`, res.TaskID, extra); err != nil {
		return "", "", err
	}
	return res.TaskID, "", nil
}

// inspectFindings are the findings of a reading the decision task asked for, without the note,
// and the reading's state; "" when there is no such reading.
func inspectFindings(ctx context.Context, tx pgx.Tx, tenant, parent, inspect string) ([]any, string, error) {
	var state string
	err := tx.QueryRow(ctx, `SELECT state FROM tasks WHERE id = $1 AND tenant = $2 AND record->>'parent_task' = $3`, inspect, tenant, parent).Scan(&state)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", err
	}
	rows, err := tx.Query(ctx, `SELECT id, body, personal_seen, in_code_targets FROM findings WHERE task = $1 ORDER BY id`, inspect)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	items := []any{}
	for rows.Next() {
		var id string
		var raw []byte
		var personal, inCode bool
		if err := rows.Scan(&id, &raw, &personal, &inCode); err != nil {
			return nil, "", err
		}
		var f map[string]any
		if err := json.Unmarshal(raw, &f); err != nil {
			return nil, "", err
		}
		delete(f, "note")
		f["id"], f["untrusted"], f["personal_seen"], f["subject_in_code_targets"] = id, true, personal, inCode
		items = append(items, f)
	}
	return items, state, rows.Err()
}

// seatRead is one object a seat attempt opened.
type seatRead struct {
	Kind, Object, Hash, Mark string
	Size                     int64
	Personal                 bool
}

// logSeatRead writes a read of a seat attempt into the tenant's read log and its audit: the
// attempt, acting_bot, the epoch, the basis and the object's kind, id, size, hash and mark
// (MGR-28). The content stays where it was.
func (c *Core) logSeatRead(ctx context.Context, tx pgx.Tx, now time.Time, tenant, task string, leaseNo int, rec []byte, r seatRead) error {
	acting, err := leaseActing(ctx, tx, task, "")
	if err != nil {
		return err
	}
	basis := readBasis(task, rec)
	var epoch any
	if acting.Seat != "" {
		epoch = acting.SeatEpoch
	}
	if _, err := tx.Exec(ctx, `INSERT INTO manager_reads (id, tenant, task, lease_no, acting_bot, epoch, basis, kind, object, size, hash, data_mark, personal, at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`,
		ids.New(ids.Read), tenant, task, leaseNo, nullable(acting.Bot), epoch, basis, r.Kind, r.Object, nullableSize(r.Size), nullable(r.Hash), nullable(r.Mark), r.Personal, now); err != nil {
		return err
	}
	_, err = c.appendAudit(ctx, tx, now, Audit{
		Tenant: tenant, Kind: "read", Actor: Actor{Principal: "system", Via: "seat", Role: managerSeatID, Task: task}, Command: "manager.read",
		Target: &Target{Kind: r.Kind, ID: cut(r.Object, 200)}, OK: true,
		After: map[string]any{"attempt": task + ":" + strconv.Itoa(leaseNo), "acting_bot": nullable(acting.Bot), "epoch": epoch, "basis": basis,
			"size": nullableSize(r.Size), "hash": nullable(r.Hash), "mark": nullable(r.Mark), "personal_seen": r.Personal},
	})
	return err
}

func nullableSize(n int64) any {
	if n <= 0 {
		return nil
	}
	return n
}

// readBasis is the basis of a seat task: its incident, directive, review day or reading.
func readBasis(task string, rec []byte) string {
	var r struct {
		Basis     string     `json:"basis"`
		Incident  string     `json:"incident"`
		Directive *directive `json:"directive"`
		Review    string     `json:"review_day"`
	}
	_ = json.Unmarshal(rec, &r)
	switch {
	case r.Basis != "":
		return r.Basis
	case r.Incident != "":
		return "incident:" + r.Incident
	case r.Directive != nil:
		return "directive:" + task
	case r.Review != "":
		return "review:" + r.Review
	}
	return "task:" + task
}

// seatToolRead logs a plugin tool call of a seat attempt: the tool and each string argument,
// which names a catalog object (INJ-1), so a finding can cite it as evidence (MGR-25, MGR-28).
func (c *Core) seatToolRead(ctx context.Context, tx pgx.Tx, now time.Time, g *toolsGrant, right string, args map[string]any) error {
	if g.Purpose != "manage" && g.Purpose != "inspect" {
		return nil
	}
	var rec []byte
	if err := tx.QueryRow(ctx, `SELECT record FROM tasks WHERE id = $1`, g.Task).Scan(&rec); err != nil {
		return err
	}
	if err := c.logSeatRead(ctx, tx, now, g.Tenant, g.Task, g.LeaseNo, rec, seatRead{Kind: "tool", Object: right}); err != nil {
		return err
	}
	n := 0
	for _, k := range slices.Sorted(mapKeys(args)) {
		s, ok := args[k].(string)
		if !ok || s == "" || n >= 5 {
			continue
		}
		n++
		if err := c.logSeatRead(ctx, tx, now, g.Tenant, g.Task, g.LeaseNo, rec, seatRead{Kind: "tool_arg", Object: cut(s, 200)}); err != nil {
			return err
		}
	}
	return nil
}

// seatFileRead applies MGR-25 and MGR-30 to a file an attempt of a seat task opens: the decision
// role reads no tenant text; a reading opens a file with personal_seen only when code named it
// as the subject of its incident, and every file it opens goes into the read log. Another
// file with the flag is not_found, the same answer as for a file that does not exist.
func (c *Core) seatFileRead(ctx context.Context, tx pgx.Tx, now time.Time, tenant, task string, leaseNo int, f *fileRow) error {
	var rec []byte
	if err := tx.QueryRow(ctx, `SELECT record FROM tasks WHERE id = $1`, task).Scan(&rec); err != nil {
		return err
	}
	var r struct {
		Purpose string   `json:"purpose"`
		Files   []string `json:"subject_files"`
	}
	_ = json.Unmarshal(rec, &r)
	switch r.Purpose {
	case "manage":
		return refuse("not_found", "no such file")
	case "inspect":
	default:
		return nil
	}
	var declared *string
	var producerKind string
	var sum string
	if err := tx.QueryRow(ctx, `SELECT f.personal_seen, coalesce(p.kind, ''), f.sha256 FROM files f
		LEFT JOIN principals p ON f.origin = 'upload' AND p.tenant = f.tenant AND p.id = f.producer WHERE f.file_ref = $1`, f.Ref).Scan(&declared, &producerKind, &sum); err != nil {
		return err
	}
	// A file a task of an excluded purpose or profile produced is not the reader's to open, the
	// same answer as for a file that does not exist (MGR-9, PRF-1).
	var purpose, role string
	if err := tx.QueryRow(ctx, `SELECT coalesce(t.record->>'purpose', ''), coalesce(t.record->>'executor_role', '') FROM files f
		JOIN tasks t ON f.origin = 'produced' AND t.id = f.producer WHERE f.file_ref = $1`, f.Ref).Scan(&purpose, &role); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	cat, err := c.Catalog(ctx)
	if err != nil {
		return err
	}
	if slices.Contains(excludedPurposes, purpose) || role != "" && slices.Contains(excludedProfiles, profileOf(cat, role)) {
		return refuse("not_found", "no such file")
	}
	personal := producerKind == "human" || deref(declared) == "declared"
	if personal && !slices.Contains(r.Files, f.Ref) {
		return refuse("not_found", "no such file")
	}
	if personal {
		if _, err := tx.Exec(ctx, `UPDATE tasks SET record = jsonb_set(record, '{personal_seen}', coalesce(record->'personal_seen', '[]'::jsonb) || '["file"]'::jsonb)
			WHERE id = $1 AND NOT coalesce(record->'personal_seen', '[]'::jsonb) ? 'file'`, task); err != nil {
			return err
		}
	}
	return c.logSeatRead(ctx, tx, now, tenant, task, leaseNo, rec, seatRead{Kind: "file", Object: f.Ref, Size: f.Size, Hash: "sha256:" + sum, Mark: f.Mark, Personal: personal})
}

// takeFindings checks the findings a reading hands in and stores them (MGR-25, MGR-30): at most
// 10, each a manager-finding/v1 whose subject is an object of the attempt's tenant, whose
// evidence is in this attempt's read log and whose numbers are from the closed list. Each is
// untrusted; it gets personal_seen when the attempt opened a file with the flag; its note sits
// in data class content, and the audit keeps only its hash and length.
func (c *Core) takeFindings(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, tenant string, l *leaseRow, rec []byte, result map[string]any) error {
	list, ok := result["findings"].([]any)
	if !ok || len(list) > maxFindings || len(result) != 1 {
		return refuse("output_schema_invalid", "a reading hands in {findings: [...]} with at most 10 findings")
	}
	rows, err := tx.Query(ctx, `SELECT lower(object), personal FROM manager_reads WHERE task = $1 AND lease_no = $2`, l.WorkID, l.LeaseNo)
	if err != nil {
		return err
	}
	read := map[string]bool{}
	personal := false
	for rows.Next() {
		var o string
		var p bool
		if err := rows.Scan(&o, &p); err != nil {
			rows.Close()
			return err
		}
		read[o] = true
		personal = personal || p
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	var r struct {
		Incident string `json:"incident"`
	}
	_ = json.Unmarshal(rec, &r)
	var inc *incidentRow
	if r.Incident != "" {
		if inc, err = loadIncident(ctx, tx, tenant, r.Incident, false); err != nil {
			return err
		}
	}
	for _, v := range list {
		fm, ok := v.(map[string]any)
		if !ok || c.spec.Validate("urn:botaru:schema:manager-finding:1", fm) != nil {
			return refuse("output_schema_invalid", "a finding is not a manager-finding/v1")
		}
		for name := range asMap(fm["values"]) {
			if !slices.Contains(findingValueNames, name) {
				return refuse("output_schema_invalid", "a finding names a number outside the closed list")
			}
		}
		for _, e := range mstrs(fm, "evidence") {
			if !read[strings.ToLower(e)] {
				return refuse("output_schema_invalid", "the evidence of a finding is not in the read log of this attempt")
			}
		}
		subj := asMap(fm["subject"])
		t := ControlTarget{Kind: mstr(subj, "kind"), ID: mstr(subj, "id")}
		if ok, err := c.tenantObject(ctx, tx, cat, tenant, t); err != nil {
			return err
		} else if !ok {
			return refuse("output_schema_invalid", "the subject of a finding is not an object of the tenant")
		}
		id := ids.New(ids.Finding)
		note := mstr(fm, "note")
		body, err := json.Marshal(fm)
		if err != nil {
			return err
		}
		inCode := inc != nil && implicatedTarget(inc, t)
		if _, err := tx.Exec(ctx, `INSERT INTO findings (id, tenant, task, incident, kind, subject_kind, subject_id, body, note, untrusted, personal_seen, in_code_targets, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, true, $10, $11, $12)`,
			id, tenant, l.WorkID, nullable(r.Incident), mstr(fm, "kind"), t.Kind, t.ID, body, nullable(note), personal, inCode, now); err != nil {
			return err
		}
		after := map[string]any{"kind": mstr(fm, "kind"), "subject": map[string]any{"kind": t.Kind, "id": t.ID}, "untrusted": true,
			"personal_seen": personal, "subject_in_code_targets": inCode, "incident": nullable(r.Incident)}
		if note != "" {
			h := sha256.Sum256([]byte(note))
			after["note_hash"], after["note_len"] = "sha256:"+hex.EncodeToString(h[:]), len([]rune(note))
		}
		if _, err := c.appendAudit(ctx, tx, now, Audit{
			Tenant: tenant, Kind: "command", Actor: Actor{Principal: "system", Via: "seat", Role: managerSeatID, Task: l.WorkID}, Command: "manager.finding",
			Target: &Target{Kind: "finding", ID: id}, OK: true, After: after,
		}); err != nil {
			return err
		}
	}
	return nil
}

// tenantObject says whether a finding's subject is an object of the tenant: a worker or a task
// of the tenant, a resource, machine or volume of its catalog, a role with a worker in it, a type a group of the
// tenant grants, or one of its schedules. Another tenant's object is the same as none.
func (c *Core) tenantObject(ctx context.Context, tx pgx.Tx, cat *Catalog, tenant string, t ControlTarget) (bool, error) {
	tv := cat.Tenants[tenant]
	if tv == nil {
		return false, nil
	}
	var ok bool
	switch t.Kind {
	case "worker":
		return ok, tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM workers WHERE tenant = $1 AND lower(id) = lower($2))`, tenant, t.ID).Scan(&ok)
	case "task":
		return ok, tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM tasks WHERE tenant = $1 AND lower(id) = lower($2))`, tenant, t.ID).Scan(&ok)
	case "role":
		return ok, tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM workers WHERE tenant = $1 AND role = $2 AND state <> 'revoked')`, tenant, t.ID).Scan(&ok)
	case "resource":
		// A machine of the catalog is a resource too, as an incident's subject or a worker's host.
		return slices.Contains(catalogResources(cat, tenant), t.ID) || slices.Contains(catalogHosts(cat, tenant), t.ID) ||
			volumeOf(cat, tenant, t.ID) != nil, nil
	case "tasktype":
		for _, g := range tv.Groups {
			if slices.ContainsFunc(g.Caps, func(cp string) bool { return capCovers(cp, "task:"+t.ID) }) {
				return true, nil
			}
		}
		return false, nil
	case "schedule":
		_, ok := tv.Schedules[t.ID]
		return ok, nil
	}
	return false, nil
}

// inspectPurposeOff applies a tenant's privacy.purposes without incident_review in the apply
// transaction: the core cancels its readings, running and waiting (MGR-20).
func (c *Core) inspectPurposeOff(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog) error {
	for _, t := range slices.Sorted(mapKeys(cat.Tenants)) {
		if incidentReviewOn(cat.Tenants[t]) {
			continue
		}
		rows, err := tx.Query(ctx, `SELECT id FROM tasks WHERE tenant = $1 AND type = $2 AND state IN ('submitted', 'working') ORDER BY id FOR UPDATE`, t, inspectType)
		if err != nil {
			return err
		}
		list, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		if err := c.cancelTaskIDs(ctx, tx, now, t, list, "rights_revoked"); err != nil {
			return err
		}
	}
	return nil
}

// doctorManager adds the checks of the farm manager in a tenant: the usage of the daily review
// over 30 days as a line of its own (MGR-41, BUD-15), and a warning when the purpose
// incident_review is on and no worker of the reader role is enrolled (MGR-20).
func (c *Core) doctorManager(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, tenant string, add func(DoctorCheck)) error {
	tv := cat.Tenants[tenant]
	if tv == nil || !managerEnabled(cat, tenant) {
		return nil
	}
	var review int64
	if err := tx.QueryRow(ctx, `SELECT coalesce(sum(s.eur_micros + coalesce(s.correction_eur_micros, 0)), 0) FROM spend s JOIN tasks t ON t.id = s.task
		WHERE s.tenant = $1 AND s.line = 'manager' AND s.unit <> 'task' AND s.time >= $2 AND t.type = $3`, tenant, now.Add(-30*24*time.Hour), reviewType).Scan(&review); err != nil {
		return err
	}
	add(DoctorCheck{ID: "budget.used_30d_eur_micros.manager_review", OK: true, Severity: "info", Value: review, Tenant: tenant})
	if err := doctorManagerMonth(ctx, tx, now, tv, add); err != nil {
		return err
	}
	if err := c.doctorRestricted(ctx, tx, tenant, add); err != nil {
		return err
	}
	if !incidentReviewOn(tv) {
		return nil
	}
	ch := DoctorCheck{ID: "manager.reader_workers", OK: true, Severity: "info", Tenant: tenant}
	var n int64
	if role := readerRole(cat); role != "" {
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM workers WHERE tenant = $1 AND role = $2 AND state <> 'revoked'`, tenant, role).Scan(&n); err != nil {
			return err
		}
	}
	ch.Value = n
	if n == 0 {
		ch.OK, ch.Severity = false, "warn"
	}
	add(ch)
	return nil
}

// ManagerReadsRequest is p2-manager-reads-request.
type ManagerReadsRequest struct {
	Tenant string `json:"tenant"`
	From   string `json:"from,omitempty"`
	To     string `json:"to,omitempty"`
	Limit  int    `json:"limit,omitempty"`
	After  string `json:"after,omitempty"`
}

// ManagerReads is P2 manager.reads: the read log of the farm manager in the tenant, metadata
// only, oldest first, with the basis of each read (MGR-28, MGR-30, AUD-7).
func (c *Core) ManagerReads(ctx context.Context, op Operator, req ManagerReadsRequest) (map[string]any, error) {
	tenant, err := viewTenant(op, req.Tenant)
	if err != nil {
		return nil, err
	}
	from, to := time.Time{}, time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
	if req.From != "" {
		if from, err = time.Parse(time.RFC3339Nano, req.From); err != nil {
			return nil, refuse("input_schema_invalid", "from")
		}
	}
	if req.To != "" {
		if to, err = time.Parse(time.RFC3339Nano, req.To); err != nil {
			return nil, refuse("input_schema_invalid", "to")
		}
	}
	limit := req.Limit
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	rows, err := c.st.Pool.Query(ctx, `SELECT id, at, kind, object, basis, task, lease_no, coalesce(size, 0), coalesce(data_mark, ''), personal FROM manager_reads
		WHERE tenant = $1 AND at >= $2 AND at < $3 AND id > $4 ORDER BY id LIMIT $5`, tenant, from, to, req.After, limit+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []any{}
	next := ""
	for rows.Next() {
		var id, kind, object, basis, task, mark string
		var at time.Time
		var leaseNo int
		var size int64
		var personal bool
		if err := rows.Scan(&id, &at, &kind, &object, &basis, &task, &leaseNo, &size, &mark, &personal); err != nil {
			return nil, err
		}
		if len(items) == limit {
			next = items[len(items)-1].(map[string]any)["id"].(string)
			break
		}
		it := map[string]any{"id": id, "at": fmtTime(at), "kind": kind, "object": object, "basis": basis,
			"attempt": task + ":" + strconv.Itoa(leaseNo), "personal_seen": personal}
		if size > 0 {
			it["size"] = size
		}
		if mark != "" {
			it["data_mark"] = mark
		}
		items = append(items, it)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := map[string]any{"items": items}
	if next != "" {
		out["next"] = next
	}
	// A reading of the read log is itself a read in the tenant's audit, like the audit query (AUD-7).
	err = c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		_, err := c.appendAudit(ctx, tx, now, Audit{
			Tenant: tenant, Kind: "read", Actor: Actor{Principal: op.Principal, Via: "farmctl"},
			Command: "manager.reads", Target: &Target{Kind: "manager_reads"}, OK: true, After: map[string]any{"records": len(items)},
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// doctorManagerMonth shows the usage of the manager line in the month with the analysis and
// takeover rows, and the date the month's line runs out at the spend of the last 7 days
// (MGR-49, BUD-15).
func doctorManagerMonth(ctx context.Context, tx pgx.Tx, now time.Time, tv *TenantView, add func(DoctorCheck)) error {
	day := tenantDay(tv, now)
	month := time.Date(day.Year(), day.Month(), 1, 0, 0, 0, 0, day.Location())
	var used, postmortem, takeover, week int64
	if err := tx.QueryRow(ctx, `SELECT coalesce(sum(s.eur_micros + coalesce(s.correction_eur_micros, 0)), 0),
		coalesce(sum(s.eur_micros + coalesce(s.correction_eur_micros, 0)) FILTER (WHERE t.type = $3), 0),
		coalesce(sum(s.eur_micros + coalesce(s.correction_eur_micros, 0)) FILTER (WHERE t.record ? 'takeover_of'), 0),
		coalesce(sum(s.eur_micros + coalesce(s.correction_eur_micros, 0)) FILTER (WHERE s.time >= $4), 0)
		FROM spend s LEFT JOIN tasks t ON t.id = s.task WHERE s.tenant = $1 AND s.line = 'manager' AND s.unit <> 'task' AND s.time >= least($2, $4)`,
		tv.Name, month, postmortemType, now.Add(-7*24*time.Hour)).Scan(&used, &postmortem, &takeover, &week); err != nil {
		return err
	}
	add(DoctorCheck{ID: "budget.used_month_eur_micros.manager", OK: true, Severity: "info", Value: used, Tenant: tv.Name})
	add(DoctorCheck{ID: "budget.used_month_eur_micros.manager_postmortem", OK: true, Severity: "info", Value: postmortem, Tenant: tv.Name})
	add(DoctorCheck{ID: "budget.used_month_eur_micros.manager_takeover", OK: true, Severity: "info", Value: takeover, Tenant: tv.Name})
	lb := tv.Spec.Budgets.Lines["manager"]
	if lb.MonthEUR == nil {
		return nil
	}
	ch := DoctorCheck{ID: "manager.line_runs_out", OK: true, Severity: "info", Tenant: tv.Name, Value: "-"}
	if left := money.FromEUR(*lb.MonthEUR) - used; left <= 0 {
		ch.OK, ch.Severity, ch.Value = false, "warn", day.Format("2006-01-02")
	} else if week > 0 {
		out := now.Add(time.Duration(float64(left) / (float64(week) / 7) * float64(24*time.Hour)))
		ch.Value = out.In(day.Location()).Format("2006-01-02")
		if out.Before(month.AddDate(0, 1, 0)) {
			ch.OK, ch.Severity = false, "warn"
		}
	}
	add(ch)
	return nil
}
