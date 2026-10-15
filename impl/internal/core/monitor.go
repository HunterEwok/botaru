// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/jcs"
)

// MonitorConfig is the monitor profile of the installation (D-88): with it the monitoring
// system is the one path of alarms to people (MON-1, MON-12) and the external witness of the
// audit head (MON-3); without it the core notices the admins itself (MON-13).
type MonitorConfig struct {
	Enabled bool `yaml:"enabled"`
	// Target is the target name the monitoring adapters enroll with; default zabbix.
	Target string `yaml:"target"`
}

// exportStalledAfter is the age of the last successful export after which the core raises
// alarm.monitor.export_stalled and messages the admins (MON-11, MON-12).
const exportStalledAfter = 15 * time.Minute

// stalledNoticeEvery is the least pause between two monitor_export_stalled messages.
const stalledNoticeEvery = 6 * time.Hour

// headEvery is how often the audit-head loop puts the signed head of a chain (MON-3).
const headEvery = 24 * time.Hour

func (c *Core) monitorOn() bool { return c.cfg.Monitor.Enabled }

func (c *Core) monitorTarget() string {
	if c.cfg.Monitor.Target != "" {
		return c.cfg.Monitor.Target
	}
	return "zabbix"
}

var exportSubject = regexp.MustCompile(`^[a-z][a-z0-9_.:-]*$`)

// exportEpisode puts a change of an alarm episode as one monitor_alarm delivery for the adapter
// of its tenant, in the transaction of the change (MON-12). A repeated firing gives none: the
// caller only calls it for a new episode or its resolution. The value carries only numbers,
// enum values, tags and catalog ids (MON-1).
func (c *Core) exportEpisode(ctx context.Context, tx pgx.Tx, now time.Time, ep *episode, origin string, values map[string]any) error {
	if !c.monitorOn() {
		return nil
	}
	name := strings.TrimSuffix(strings.TrimPrefix(ep.Alarm, "alarm."), ".v1")
	subject := strings.ToLower(ep.Subject.ID)
	if !exportSubject.MatchString(subject) {
		subject = ep.Subject.Kind
	}
	scope := "tenant"
	if ep.Tenant == "farm" {
		scope = "farm"
	}
	if origin == "" {
		origin = "code"
	}
	body := map[string]any{
		"seq": ep.Seq, "state": ep.State, "tenant": ep.Tenant, "alarm": name, "subject": subject,
		"severity": ep.Severity, "scope": scope, "origin": origin, "key": ep.Tenant + ":" + name + ":" + subject,
	}
	if len(values) > 0 {
		body["values"] = values
	}
	if err := c.spec.Validate("monitor-alarm", body); err != nil {
		// A value outside the closed form never leaves: the tags are enough to wake people.
		delete(body, "values")
		if err := c.spec.Validate("monitor-alarm", body); err != nil {
			return err
		}
	}
	if err := ensureMonitorRow(ctx, tx, ep.Tenant); err != nil {
		return err
	}
	_, err := c.planDelivery(ctx, tx, now, plannedDelivery{
		Tenant: ep.Tenant, Kind: "monitor_alarm", Addr: "monitor", TargetKind: "monitor", TargetID: c.monitorTarget(),
		ExecKind: "monitor", ExecName: c.monitorTarget(), DataOrigin: "code", Alarmkey: ep.Tenant + "/" + ep.Alarmkey, Episode: ep.ID, Body: body,
	})
	return err
}

func ensureMonitorRow(ctx context.Context, tx pgx.Tx, tenant string) error {
	_, err := tx.Exec(ctx, `INSERT INTO monitor_exports (tenant) VALUES ($1) ON CONFLICT DO NOTHING`, tenant)
	return err
}

// headKey is the Ed25519 key of audit heads for a key id: derived from the token key of that
// id, so it rotates with it and needs no secret of its own (SEC-5, spec/hashing.md).
func (c *Core) headKey(id string) ed25519.PrivateKey {
	k := c.signingKey(id)
	if k == nil {
		return nil
	}
	m := hmac.New(sha256.New, k)
	m.Write([]byte("botaru.audit-head.key.v1"))
	return ed25519.NewKeyFromSeed(m.Sum(nil))
}

// AuditHead is the signed head of an audit chain (monitor-audit-head).
type AuditHead struct {
	Tenant string `json:"tenant"`
	Epoch  int64  `json:"epoch"`
	Seq    int64  `json:"seq"`
	Hash   string `json:"hash"`
	Sig    string `json:"sig"`
	KeyID  string `json:"key_id"`
}

func (h AuditHead) signed() ([]byte, error) {
	return jcs.Canonical(map[string]any{"type": "botaru.audit-head.v1", "tenant": h.Tenant, "epoch": h.Epoch, "seq": h.Seq, "hash": h.Hash, "key_id": h.KeyID})
}

func (c *Core) signHead(h AuditHead) (AuditHead, error) {
	h.KeyID = c.cfg.KeyID
	b, err := h.signed()
	if err != nil {
		return h, err
	}
	h.Sig = base64.StdEncoding.EncodeToString(ed25519.Sign(c.headKey(h.KeyID), b))
	return h, nil
}

// headSignatureOK checks the signature of a head with the key its key id names: the current
// key or one of the rotation window.
func (c *Core) headSignatureOK(h AuditHead) bool {
	key := c.headKey(h.KeyID)
	sig, err := base64.StdEncoding.DecodeString(h.Sig)
	if key == nil || err != nil {
		return false
	}
	b, err := h.signed()
	return err == nil && ed25519.Verify(key.Public().(ed25519.PublicKey), b, sig)
}

// planHead puts the signed head of a tenant chain as a monitor_audit_head delivery; an older
// head not handed out yet is replaced, since only the newest one matters (MON-3, MON-11).
func (c *Core) planHead(ctx context.Context, tx pgx.Tx, now time.Time, tenant string) error {
	h := AuditHead{Tenant: tenant, Epoch: c.epoch}
	err := tx.QueryRow(ctx, `SELECT seq, hash FROM audit_heads WHERE tenant = $1 AND epoch = $2`, tenant, c.epoch).Scan(&h.Seq, &h.Hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if h, err = c.signHead(h); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE deliveries SET state = 'withheld', reason = 'superseded', updated_at = $2
		WHERE tenant = $1 AND kind = 'monitor_audit_head' AND state = 'planned'`, tenant, now); err != nil {
		return err
	}
	if err := ensureMonitorRow(ctx, tx, tenant); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE monitor_exports SET head_planned_at = $2 WHERE tenant = $1`, tenant, now); err != nil {
		return err
	}
	_, err = c.planDelivery(ctx, tx, now, plannedDelivery{
		Tenant: tenant, Kind: "monitor_audit_head", Addr: "monitor", TargetKind: "monitor", TargetID: c.monitorTarget(),
		ExecKind: "monitor", ExecName: c.monitorTarget(), DataOrigin: "code",
		Body: map[string]any{"tenant": h.Tenant, "epoch": h.Epoch, "seq": h.Seq, "hash": h.Hash, "sig": h.Sig, "key_id": h.KeyID},
	})
	return err
}

// auditHeadLoop puts the signed head of each chain of the current epoch at least once a day
// (MON-3).
func (c *Core) auditHeadLoop(ctx context.Context) error {
	if !c.monitorOn() {
		return nil
	}
	return c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		rows, err := tx.Query(ctx, `SELECT h.tenant FROM audit_heads h LEFT JOIN monitor_exports m ON m.tenant = h.tenant
			WHERE h.epoch = $1 AND (m.head_planned_at IS NULL OR m.head_planned_at <= $2) ORDER BY h.tenant`, c.epoch, now.Add(-headEvery))
		if err != nil {
			return err
		}
		tenants, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		for _, t := range tenants {
			if err := c.planHead(ctx, tx, now, t); err != nil {
				return err
			}
		}
		return nil
	})
}

// monitorExported records a successful E5 export. The first success after a failure exports
// again every open episode and the head of the tenant, so a gap during the failure leaves
// neither a problem without an episode nor an episode without a problem (MON-11).
func (c *Core) monitorExported(ctx context.Context, tx pgx.Tx, now time.Time, tenant, kind string) error {
	if err := ensureMonitorRow(ctx, tx, tenant); err != nil {
		return err
	}
	var failing *time.Time
	if err := tx.QueryRow(ctx, `SELECT failing_since FROM monitor_exports WHERE tenant = $1 FOR UPDATE`, tenant).Scan(&failing); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE monitor_exports SET last_ok_at = $2, failing_since = NULL,
		head_exported_at = CASE WHEN $3 THEN $2 ELSE head_exported_at END WHERE tenant = $1`, tenant, now, kind == "monitor_audit_head"); err != nil {
		return err
	}
	if failing == nil {
		return nil
	}
	rows, err := tx.Query(ctx, `SELECT e.id, e.alarmkey, e.alarm, e.severity, e.subject, e.vals, e.origin FROM alarm_episodes e
		WHERE e.tenant = $1 AND e.state = 'firing' AND NOT EXISTS (SELECT 1 FROM deliveries d WHERE d.episode = e.id
			AND d.kind = 'monitor_alarm' AND d.state IN ('planned', 'sending')) ORDER BY e.id`, tenant)
	if err != nil {
		return err
	}
	type open struct {
		ep     episode
		vals   map[string]any
		origin string
	}
	var eps []open
	for rows.Next() {
		var o open
		var subj []byte
		if err := rows.Scan(&o.ep.ID, &o.ep.Alarmkey, &o.ep.Alarm, &o.ep.Severity, &subj, &o.vals, &o.origin); err != nil {
			rows.Close()
			return err
		}
		if err := json.Unmarshal(subj, &o.ep.Subject); err != nil {
			rows.Close()
			return err
		}
		o.ep.Tenant, o.ep.State, o.ep.Seq = tenant, "firing", 1
		eps = append(eps, o)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, o := range eps {
		if err := c.exportEpisode(ctx, tx, now, &o.ep, o.origin, o.vals); err != nil {
			return err
		}
	}
	if kind == "monitor_audit_head" {
		return nil
	}
	return c.planHead(ctx, tx, now, tenant)
}

// monitorFailed marks the start of a failure of the E5 export of a tenant.
func monitorFailed(ctx context.Context, tx pgx.Tx, now time.Time, tenant string) error {
	if err := ensureMonitorRow(ctx, tx, tenant); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE monitor_exports SET failing_since = coalesce(failing_since, $2) WHERE tenant = $1`, tenant, now)
	return err
}

// exportAge is the age of the last successful E5 export of each tenant that has E5 work: a
// tenant that never exported counts from its oldest open delivery (MON-11).
func exportAges(ctx context.Context, tx pgx.Tx, now time.Time) (map[string]int64, map[string]bool, error) {
	rows, err := tx.Query(ctx, `SELECT m.tenant, m.last_ok_at,
		(SELECT min(d.created_at) FROM deliveries d WHERE d.tenant = m.tenant AND d.exec_kind = 'monitor' AND d.state IN ('planned', 'sending'))
		FROM monitor_exports m ORDER BY m.tenant`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	ages, pending := map[string]int64{}, map[string]bool{}
	for rows.Next() {
		var tenant string
		var ok, oldest *time.Time
		if err := rows.Scan(&tenant, &ok, &oldest); err != nil {
			return nil, nil, err
		}
		pending[tenant] = oldest != nil
		switch {
		case ok != nil:
			ages[tenant] = max(int64(now.Sub(*ok).Seconds()), 0)
		case oldest != nil:
			ages[tenant] = max(int64(now.Sub(*oldest).Seconds()), 0)
		}
	}
	return ages, pending, rows.Err()
}

// monitorLoop watches the E5 export of each tenant: work waiting for longer than 15 minutes
// with no success in that time raises alarm.monitor.export_stalled and sends the admins of
// the tenant and the operator the code message monitor_export_stalled at most once in 6
// hours; the export coming back resolves the alarm (MON-11, MON-12).
func (c *Core) monitorLoop(ctx context.Context) error {
	if !c.monitorOn() {
		return nil
	}
	return c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		rows, err := tx.Query(ctx, `SELECT m.tenant, m.stalled_notice_at,
			EXISTS (SELECT 1 FROM deliveries d WHERE d.tenant = m.tenant AND d.exec_kind = 'monitor' AND d.state IN ('planned', 'sending') AND d.created_at <= $1)
			AND (m.last_ok_at IS NULL OR m.last_ok_at <= $1)
			FROM monitor_exports m ORDER BY m.tenant`, now.Add(-exportStalledAfter))
		if err != nil {
			return err
		}
		type row struct {
			tenant  string
			noticed *time.Time
			stalled bool
		}
		rs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
			var x row
			return x, r.Scan(&x.tenant, &x.noticed, &x.stalled)
		})
		if err != nil {
			return err
		}
		ages, _, err := exportAges(ctx, tx, now)
		if err != nil {
			return err
		}
		for _, r := range rs {
			a := Alarm{Tenant: r.tenant, Name: "monitor.export_stalled", Subject: AlarmSubject{Kind: "tenant", ID: r.tenant}, Severity: "warn"}
			if !r.stalled {
				if err := c.resolveAlarm(ctx, tx, now, a); err != nil {
					return err
				}
				continue
			}
			if err := c.fireAlarm(ctx, tx, now, a); err != nil {
				return err
			}
			if r.noticed != nil && now.Sub(*r.noticed) < stalledNoticeEvery {
				continue
			}
			if _, err := tx.Exec(ctx, `UPDATE monitor_exports SET stalled_notice_at = $2 WHERE tenant = $1`, r.tenant, now); err != nil {
				return err
			}
			if err := c.noticeStalled(ctx, tx, now, r.tenant, ages[r.tenant]); err != nil {
				return err
			}
		}
		return nil
	})
}

// noticeStalled sends the code message monitor_export_stalled to the admins of the tenant and
// to the operator (the admins of farm) in their private chats: only the tenant and the age of
// the last successful export, no model text. This is not an alarm delivery, but a message
// that alarms do not get through (MON-12).
func (c *Core) noticeStalled(ctx context.Context, tx pgx.Tx, now time.Time, tenant string, age int64) error {
	cat, err := c.Catalog(ctx)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	sent := 0
	for _, t := range []string{tenant, "farm"} {
		tv, ok := cat.Tenants[t]
		if !ok || tv.Spec.Kind == "" || (t == "farm" && tenant == "farm" && sent > 0) {
			continue
		}
		chats, err := c.adminChats(ctx, tx, now, tv, "", "admin:people", "hold.release")
		if err != nil {
			return err
		}
		for _, a := range chats {
			key := t + "/" + a.Channel + "/" + a.ExternalID
			if seen[key] {
				continue
			}
			seen[key] = true
			if _, err := c.planDelivery(ctx, tx, now, plannedDelivery{
				Tenant: t, Kind: "monitor_export_stalled", Addr: "admins", TargetKind: "chat",
				TargetID: Chat{Kind: "direct", ExternalID: a.ExternalID}.ID(), ExecKind: "channel", ExecName: a.Channel,
				Recipient: a.Principal, DataOrigin: "code",
				Body: map[string]any{"form": "text_code", "text_key": "notice.monitor_export_stalled", "params": map[string]any{
					"tenant": tenant, "age_min": age / 60,
				}},
			}); err != nil {
				return err
			}
			sent++
		}
	}
	if sent == 0 {
		c.log.Warn("monitor_export_stalled has no recipient: no active admin of the tenant or of farm has a private chat", "tenant", tenant)
	}
	return nil
}

// verifyHeads checks the heads a witness kept, in the order of its history: the signature by
// the key the head names, seq growing within an epoch and falling only with a growing epoch
// (RET-6), and each head of the current epoch matching the record it names (MON-3, AUD-6).
// It returns the reason and the seq of the first bad head, or "".
func (c *Core) verifyHeads(ctx context.Context, tenant string, heads []AuditHead) (string, int64, error) {
	var prev *AuditHead
	for i := range heads {
		h := heads[i]
		if h.Tenant != tenant || !c.headSignatureOK(h) {
			return "head_signature", h.Seq, nil
		}
		if prev != nil && (h.Epoch < prev.Epoch || h.Epoch == prev.Epoch && h.Seq < prev.Seq) {
			return "head_seq", h.Seq, nil
		}
		if h.Epoch == c.epoch {
			var stored string
			err := c.st.Pool.QueryRow(ctx, `SELECT hash FROM audit WHERE tenant = $1 AND epoch = $2 AND seq = $3`, tenant, h.Epoch, h.Seq).Scan(&stored)
			if errors.Is(err, pgx.ErrNoRows) || err == nil && stored != h.Hash {
				return "head_mismatch", h.Seq, nil
			}
			if err != nil {
				return "", 0, err
			}
		}
		prev = &heads[i]
	}
	return "", 0, nil
}

// doctorMonitor adds the lines of the alarm path to the monitoring system per tenant (OBS-11,
// MON-3, MON-11): the export age, E5 deliveries planned for longer than 15 minutes, and the
// age of the last exported audit head.
func (c *Core) doctorMonitor(ctx context.Context, tx pgx.Tx, now time.Time, tenant string, add func(DoctorCheck)) error {
	if !c.monitorOn() {
		return nil
	}
	ages, pending, err := exportAges(ctx, tx, now)
	if err != nil {
		return err
	}
	mine := func(t string) bool { return tenant == "farm" || t == tenant }
	for _, t := range slices.Sorted(mapKeys(ages)) {
		if !mine(t) {
			continue
		}
		ok := !pending[t] || ages[t] <= int64(exportStalledAfter.Seconds())
		add(DoctorCheck{ID: "monitor.export_age_s", Tenant: t, OK: ok, Severity: sevOf(ok, "fail"), Value: ages[t]})
	}
	rows, err := tx.Query(ctx, `SELECT m.tenant, (SELECT count(*) FROM deliveries d WHERE d.tenant = m.tenant AND d.exec_kind = 'monitor' AND d.state = 'planned' AND d.created_at <= $1),
		m.head_exported_at FROM monitor_exports m ORDER BY m.tenant`, now.Add(-exportStalledAfter))
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var t string
		var old int64
		var head *time.Time
		if err := rows.Scan(&t, &old, &head); err != nil {
			return err
		}
		if !mine(t) {
			continue
		}
		add(DoctorCheck{ID: "monitor.planned_old", Tenant: t, OK: old == 0, Severity: sevOf(old == 0, "fail"), Value: old})
		age := int64(-1)
		if head != nil {
			age = max(int64(now.Sub(*head).Seconds()), 0)
		}
		ok := head != nil && now.Sub(*head) <= headEvery+time.Hour
		add(DoctorCheck{ID: "monitor.audit_head_age_s", Tenant: t, OK: ok, Severity: sevOf(ok, "fail"), Value: age})
	}
	return rows.Err()
}

func sevOf(ok bool, bad string) string {
	if ok {
		return "info"
	}
	return bad
}

// profiles are the conformance profiles this installation claims (MAN-8, D-88).
func (c *Core) profiles() []string {
	if c.monitorOn() {
		return []string{"core", "monitor"}
	}
	return []string{"core"}
}
