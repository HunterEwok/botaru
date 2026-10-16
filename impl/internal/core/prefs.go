// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// A person's notification preferences (DLV-10): quiet_hours and digest only defer deliveries
// of the kinds marked deferrable in delivery-kinds.yaml; the recipient, the ceiling and every
// other kind (task results, cards, ai_notice, privacy_notice, alarm_notice) stay as they are.

// Prefs is Principal.prefs (DLV-10).
type Prefs struct {
	QuietHours *QuietHours `json:"quiet_hours,omitempty"`
	Digest     string      `json:"digest,omitempty"`
}

// QuietHours is a local interval of the tenant's time zone; from may be later than to.
type QuietHours struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// digestHour is the local hour of the daily digest of a person without quiet hours; with
// quiet hours the digest goes at their end.
const digestHour = 9

var hhmm = regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]$`)

func minutesOf(s string) int {
	t, _ := time.Parse("15:04", s)
	return t.Hour()*60 + t.Minute()
}

// deferrableKinds are the delivery kinds marked deferrable (DLV-10).
func (c *Core) deferrableKinds() []string {
	reg, err := c.spec.Registry("spec/model/delivery-kinds.yaml")
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range reg.Entries {
		if id, _ := e["id"].(string); e["deferrable"] == true {
			out = append(out, id)
		}
	}
	return out
}

// nextLocal is the first local time at minute m of the day at or after now.
func nextLocal(now time.Time, loc *time.Location, m int) time.Time {
	l := now.In(loc)
	t := time.Date(l.Year(), l.Month(), l.Day(), m/60, m%60, 0, 0, loc)
	if t.Before(now) {
		t = time.Date(l.Year(), l.Month(), l.Day()+1, m/60, m%60, 0, 0, loc)
	}
	return t
}

// quietUntil is the end of the person's quiet hours when now falls inside them, or zero.
func quietUntil(q *QuietHours, now time.Time, loc *time.Location) time.Time {
	if q == nil {
		return time.Time{}
	}
	from, to := minutesOf(q.From), minutesOf(q.To)
	l := now.In(loc)
	cur := l.Hour()*60 + l.Minute()
	in := false
	switch {
	case from < to:
		in = cur >= from && cur < to
	case from > to: // over midnight
		in = cur >= from || cur < to
	}
	if !in {
		return time.Time{}
	}
	return nextLocal(now, loc, to)
}

// prefsDefer applies the recipient's preferences at the hand-out of a delivery (DLV-10). A
// deferrable delivery inside quiet hours waits until they end; with digest daily it waits for
// the person's daily digest slot and goes out there with the others collected for that slot.
// It reports whether the delivery waits.
func (c *Core) prefsDefer(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, tenant, id, kind, recipient string) (bool, error) {
	if !slices.Contains(c.deferrableKinds(), kind) {
		return false, nil
	}
	var raw []byte
	var digestOf *string
	err := tx.QueryRow(ctx, `SELECT p.prefs, d.digest_of FROM deliveries d LEFT JOIN principals p ON p.tenant = d.tenant AND p.id = d.recipient
		WHERE d.id = $1`, id).Scan(&raw, &digestOf)
	if errors.Is(err, pgx.ErrNoRows) || len(raw) == 0 {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var p Prefs
	if err := json.Unmarshal(raw, &p); err != nil {
		return false, nil
	}
	loc := tenantLocation(cat, tenant)
	until := quietUntil(p.QuietHours, now, loc)
	if until.IsZero() && p.Digest == "daily" && digestOf == nil {
		m := digestHour * 60
		if p.QuietHours != nil {
			m = minutesOf(p.QuietHours.To)
		}
		until = nextLocal(now, loc, m)
	}
	if until.IsZero() {
		return false, nil
	}
	mark := digestOf
	if p.Digest == "daily" {
		s := "daily:" + until.UTC().Format(time.RFC3339)
		mark = &s
	}
	if _, err := tx.Exec(ctx, `UPDATE deliveries SET run_at = $2, digest_of = $3, paced = false, updated_at = $4 WHERE id = $1`, id, until, mark, now); err != nil {
		return false, err
	}
	return true, nil
}

// prefsCommand is /prefs from a private chat (DLV-10): with no words it shows the person's
// preferences; "quiet HH:MM-HH:MM", "quiet off", "digest daily" and "digest off" set them. A
// person with admin:people sets another person's by naming them first (an identity such as
// telegram:1002, or a principal id). A model, a task result and a task body have no path here.
func (c *Core) prefsCommand(ctx context.Context, tx pgx.Tx, now time.Time, pc personCommand, rest string) (*ActivityResult, error) {
	f := strings.Fields(rest)
	subject := pc.person
	if len(f) > 0 && f[0] != "quiet" && f[0] != "digest" {
		if !hasCap(pc.tv, pc.groups, "admin:people") {
			return pc.reply(c, "task.rejected.no_group_cap", nil), nil
		}
		subject = f[0]
		if strings.Contains(subject, ":") {
			p, err := principalByIdentity(ctx, tx, pc.tv.Name, subject)
			if errors.Is(err, pgx.ErrNoRows) {
				return pc.reply(c, "cmd.grant.no_person", nil), nil
			}
			if err != nil {
				return nil, err
			}
			subject = p
		}
		f = f[1:]
	}
	var raw []byte
	var kind string
	err := tx.QueryRow(ctx, `SELECT prefs, kind FROM principals WHERE tenant = $1 AND id = $2 FOR UPDATE`, pc.tv.Name, subject).Scan(&raw, &kind)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && kind != "human") {
		return pc.reply(c, "cmd.grant.no_person", nil), nil
	}
	if err != nil {
		return nil, err
	}
	var p Prefs
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &p)
	}
	before := p
	switch {
	case len(f) == 0:
	case len(f) == 2 && f[0] == "quiet" && f[1] == "off":
		p.QuietHours = nil
	case len(f) == 2 && f[0] == "quiet":
		from, to, ok := strings.Cut(f[1], "-")
		if !ok || !hhmm.MatchString(from) || !hhmm.MatchString(to) || from == to {
			return pc.reply(c, "cmd.prefs.usage", nil), nil
		}
		p.QuietHours = &QuietHours{From: from, To: to}
	case len(f) == 2 && f[0] == "digest" && (f[1] == "daily" || f[1] == "off"):
		p.Digest = f[1]
	default:
		return pc.reply(c, "cmd.prefs.usage", nil), nil
	}
	if len(f) > 0 {
		b, _ := json.Marshal(p)
		if _, err := tx.Exec(ctx, `UPDATE principals SET prefs = $3 WHERE tenant = $1 AND id = $2`, pc.tv.Name, subject, b); err != nil {
			return nil, err
		}
		if _, err := c.appendAudit(ctx, tx, now, Audit{
			Tenant: pc.tv.Name, Kind: "command", Actor: Actor{Principal: pc.person, Via: "channel"},
			Command: "principal.prefs", Target: &Target{Kind: "principal", ID: subject}, OK: true,
			Before: map[string]any{"prefs": before}, After: map[string]any{"prefs": p},
		}); err != nil {
			return nil, err
		}
	}
	quiet, digest := "-", "off"
	if p.QuietHours != nil {
		quiet = p.QuietHours.From + "-" + p.QuietHours.To
	}
	if p.Digest != "" {
		digest = p.Digest
	}
	return pc.reply(c, "cmd.prefs.show", map[string]any{"quiet": quiet, "digest": digest}), nil
}
