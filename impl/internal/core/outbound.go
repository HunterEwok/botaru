// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/redact"
)

// channelCeiling is the highest data mark a channel driver may receive (SEC-9): the ceiling of
// its entry in the tenant's privacy block, internal for a third-party store without one, and
// internal for a driver the block does not list. "" means no ceiling.
func channelCeiling(tv *TenantView, channel string) string {
	driver := tv.Channels[channel].Driver
	if driver == "" {
		driver = channel
	}
	for _, ch := range tv.Spec.Privacy.Channels {
		if ch.Driver != driver {
			continue
		}
		switch {
		case ch.Ceiling != "":
			return ch.Ceiling
		case ch.ThirdPartyStorage:
			return "internal"
		}
		return ""
	}
	return "internal"
}

// markWithin says whether a mark is not above a ceiling; an unknown mark or ceiling is above.
func (c *Core) markWithin(mark, ceiling string) bool {
	m, ok1 := c.markOrder(mark)
	l, ok2 := c.markOrder(ceiling)
	return ok1 && ok2 && m <= l
}

// outgoing is a delivery as the handoff checks it.
type outgoing struct {
	ID, Kind, Addr, TargetKind, TargetID string
	Task, Recipient, DataMark            string
	DataOrigin                           string
	Body                                 []byte
}

// outboundRefusal checks a delivery at handoff against the channel ceiling (SEC-9) and, for
// model text, searches it for secrets with the toolhost's patterns (INJ-11). It returns the
// withheld reason, or "". A delivery above the ceiling gets a code text in its place; a
// secret raises alarm.outbound.secret.detected without content, and the second case of one
// role within an hour stops the deliveries of that role (HLD-6).
func (c *Core) outboundRefusal(ctx context.Context, tx pgx.Tx, now time.Time, tv *TenantView, channel string, d outgoing, role string) (string, error) {
	if d.DataMark != "" {
		if ceiling := channelCeiling(tv, channel); ceiling != "" && !c.markWithin(d.DataMark, ceiling) {
			if err := c.withhold(ctx, tx, now, tv.Name, d, "channel_ceiling"); err != nil {
				return "", err
			}
			// The person learns that the answer exists, without its content.
			_, err := c.planDelivery(ctx, tx, now, plannedDelivery{
				Tenant: tv.Name, Task: d.Task, Kind: d.Kind, Addr: d.Addr, TargetKind: d.TargetKind, TargetID: d.TargetID,
				ExecKind: "channel", ExecName: channel, Recipient: d.Recipient, DataOrigin: "code",
				Body: map[string]any{"form": "text_code", "text_key": "delivery.withheld.channel_ceiling"},
			})
			return "channel_ceiling", err
		}
	}
	if d.DataOrigin != "model" || !redact.HasSecret(outgoingText(d.Body)) {
		return "", nil
	}
	if err := c.withhold(ctx, tx, now, tv.Name, d, "secret_in_outbound"); err != nil {
		return "", err
	}
	subject := AlarmSubject{Kind: "tenant", ID: tv.Name}
	if role != "" {
		subject = AlarmSubject{Kind: "role", ID: role}
	}
	if err := c.fireAlarm(ctx, tx, now, Alarm{Tenant: tv.Name, Name: "outbound.secret.detected", Subject: subject, Severity: "page",
		Values: map[string]any{"reason": "secret_in_outbound"}}); err != nil {
		return "", err
	}
	if role == "" {
		return "secret_in_outbound", nil
	}
	return "secret_in_outbound", c.secretAutostop(ctx, tx, now, tv.Name, role)
}

// secretAutostop sets the hold of HLD-6: a role whose messages were stopped for a secret twice
// within an hour gets a no_delivery hold of contain_auto without expiry, which only a person
// lifts, and the alarm alarm.hold.auto.
func (c *Core) secretAutostop(ctx context.Context, tx pgx.Tx, now time.Time, tenant, role string) error {
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM deliveries d WHERE d.tenant = $1 AND d.state = 'withheld' AND d.reason = 'secret_in_outbound'
		AND d.updated_at > $3 AND $2 = (SELECT w.role FROM leases l JOIN workers w ON w.id = l.worker_id
			WHERE l.work_id = d.task AND l.kind = 'attempt' ORDER BY l.lease_no DESC LIMIT 1)`, tenant, role, now.Add(-time.Hour)).Scan(&n); err != nil {
		return err
	}
	if n < 2 {
		return nil
	}
	var held int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM holds WHERE tenant = $1 AND state = 'active' AND reason = 'secret_in_outbound'
		AND scope_kind = 'role' AND scope_tenant = $1 AND scope_name = $2`, tenant, role).Scan(&held); err != nil {
		return err
	}
	if held > 0 {
		return nil
	}
	actor := HoldActor{Principal: "system", Rank: "contain_auto", Tenant: tenant, Via: "system", Source: "secret_in_outbound_twice"}
	if _, _, err := c.insertHold(ctx, tx, now, actor, HoldScope{Kind: "role", Tenant: tenant, Name: role}, []string{"no_delivery"}, "secret_in_outbound", nil); err != nil {
		return err
	}
	return c.fireAlarm(ctx, tx, now, Alarm{Tenant: tenant, Name: "hold.auto", Subject: AlarmSubject{Kind: "role", ID: role}, Severity: "page",
		Values: map[string]any{"reason": "secret_in_outbound", "cases": n}})
}

// withhold ends a delivery at handoff with a reason and its event, without content.
func (c *Core) withhold(ctx context.Context, tx pgx.Tx, now time.Time, tenant string, d outgoing, reason string) error {
	if _, err := tx.Exec(ctx, `UPDATE deliveries SET state = 'withheld', reason = $2, updated_at = $3 WHERE id = $1`, d.ID, reason, now); err != nil {
		return err
	}
	return c.appendEvent(ctx, tx, now, tenant, Event{Type: "delivery.withheld", Subject: d.ID, Data: map[string]any{"delivery": d.ID, "kind": d.Kind, "reason": reason}})
}

// outgoingText is the text a delivery body would put in a message: the text and the fields
// of a result.
func outgoingText(body []byte) string {
	var b struct {
		Text   string            `json:"text"`
		Fields map[string]string `json:"fields"`
	}
	_ = json.Unmarshal(body, &b)
	var s strings.Builder
	s.WriteString(b.Text)
	for _, v := range b.Fields {
		s.WriteString("\n")
		s.WriteString(v)
	}
	return s.String()
}

// deliveryRole is the role of the worker whose attempt produced the task of a delivery, or "".
func deliveryRole(ctx context.Context, tx pgx.Tx, task string) (string, error) {
	if task == "" {
		return "", nil
	}
	var role string
	err := tx.QueryRow(ctx, `SELECT w.role FROM leases l JOIN workers w ON w.id = l.worker_id
		WHERE l.work_id = $1 AND l.kind = 'attempt' ORDER BY l.lease_no DESC LIMIT 1`, task).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return role, err
}
