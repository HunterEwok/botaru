// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/manifest"
	"github.com/HunterEwok/botaru/impl/internal/spec"
)

// Source channels of external events (profile ingress, spec/bindings/channels/webhook.md): the
// driver checks the authenticity of a delivery by the means of the external system, saves the
// whole body as a file on behalf of the service principal hook:<source> and submits one signal.
// The core takes the event type, the source and the key from the manifest of the submitting
// channel, never from the activity (ING-1 to ING-4).

// ingressOn says whether the build claims the ingress profile; until it does, apply refuses a
// source channel (MAN-11) and these paths are not reached.
func ingressOn() bool { return spec.Claims("ingress") }

// reUpdateID is the delivery id form of update_id in spec/model/activity.schema.json.
var reUpdateID = regexp.MustCompile(`^[A-Za-z0-9_:.-]{1,128}$`)

// sourceChannel returns the manifest of the worker's channel when it is a source channel.
func sourceChannel(tv *TenantView, w *Worker) (manifest.Channel, bool) {
	ch, ok := tv.Channels[w.Target]
	return ch, ok && ch.Direction == "in"
}

// hookPrincipal is the active service principal hook:<source> of a source channel, or a
// refusal (ING-1): the operator creates it, and only its identity submits the channel's
// deliveries.
func hookPrincipal(ctx context.Context, tx pgx.Tx, now time.Time, tenant, source string) (string, error) {
	id, err := principalByIdentity(ctx, tx, tenant, "hook:"+source)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", refuse("identity_unknown", "no service principal hook:%s in the tenant", source)
	}
	if err != nil {
		return "", err
	}
	p, err := loadPrincipal(ctx, tx, now, tenant, id)
	if err != nil {
		return "", err
	}
	if p.Kind != "service" || p.State != "active" {
		return "", refuse("identity_unknown", "hook:%s is not an active service principal", source)
	}
	return id, nil
}

// submitIngress turns a signal of a source channel into the event ingress.event: once per
// delivery key system:ingress:<source>:<delivery id> (ING-3), with only the mapped fields of
// closed kinds that pass their schemas (ING-2), in the ext.* namespace and never an alarm
// (ING-4). The body file stays an attachment of the tasks the routes on the event enqueue.
func (c *Core) submitIngress(ctx context.Context, w *Worker, tv *TenantView, cat *Catalog, ch manifest.Channel, a Activity) (*ActivityResult, error) {
	if !ingressOn() {
		return nil, refuse("not_ready", "the ingress profile is off")
	}
	ing := ch.Ingress
	source := ing.Source()
	if ing == nil || source == "" {
		return nil, refuse("not_ready", "the source channel has no ingress block")
	}
	// The type and the sender are those of the channel's manifest, or the signal is refused: a
	// source channel cannot submit another source's events or the monitoring adapter's (ING-4).
	if a.Kind != "signal" || a.Event == nil || a.Event.Type != ing.Event || a.Sender.Identity != "hook:"+source || !a.Sender.Verified ||
		a.Chat.Kind != "direct" || a.Chat.ExternalID != source || !reUpdateID.MatchString(a.UpdateID) ||
		a.Text != "" || a.ReplyTo != "" || a.Forwarded || a.Quoted || len(a.Attachments) > 1 {
		return nil, refuse("input_schema_invalid", "a source channel submits only signals of ingress.event from hook:%s", source)
	}
	// Only the fields of the mapping, and of each only a value that passes its closed schema
	// again; a field the mapping does not name is refused (ING-2).
	data := map[string]any{}
	for k, v := range a.Event.Data {
		f, ok := ing.Mapping[k]
		if !ok {
			return nil, refuse("input_schema_invalid", "the event data holds a field outside ingress.mapping")
		}
		ok, err := c.mappedValueOK(cat, w.Tenant, f, v)
		if err != nil {
			return nil, err
		}
		if ok {
			data[k] = v
		}
	}
	key := "system:ingress:" + source + ":" + a.UpdateID
	out := ActivityResult{Result: "ignored"}
	err := c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		hook, err := hookPrincipal(ctx, tx, now, w.Tenant, source)
		if err != nil {
			return err
		}
		// The body file is one the driver put for this delivery on behalf of hook:<source>.
		var file string
		for _, ref := range a.Attachments {
			f, err := loadFile(ctx, tx, w.Tenant, ref)
			if err != nil {
				return err
			}
			if f == nil || f.Origin != "upload" || f.Producer != hook || f.State != "stored" {
				return refuse("input_schema_invalid", "the attachment is not a body file of the source")
			}
			file = ref
		}
		tag, err := tx.Exec(ctx, `INSERT INTO ingress_deliveries (tenant, key, channel, event, file_ref, created_at) VALUES ($1, $2, $3, '', $4, $5)
			ON CONFLICT DO NOTHING`, w.Tenant, key, w.Target, nullable(file), now)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			// A repeat of the delivery: the same answer, no second event (ING-3, IDM-2).
			return nil
		}
		src := "botaru:ingress/" + w.Tenant + "/" + source
		id, err := c.appendEventFrom(ctx, tx, now, w.Tenant, src, Event{Type: ing.Event, Subject: key, Data: data})
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE ingress_deliveries SET event = $3 WHERE tenant = $1 AND key = $2`, w.Tenant, key, id); err != nil {
			return err
		}
		return c.eventRoutes(ctx, tx, now, cat, w.Tenant, routedEvent{
			Type: ing.Event, ID: id, Source: src, Subject: key, Data: data, File: file, TokenSource: ing.Auth == "tls_token",
		})
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// mappedValueOK says whether a value of an ingress.mapping field passes its closed schema: the
// leaves of INJ-1, with the catalog values of an enum_from list and the patterns of the
// formats (ING-2). A schema that is not closed passes nothing.
func (c *Core) mappedValueOK(cat *Catalog, tenant string, f manifest.IngressField, v any) (bool, error) {
	if !closedSchema(f.Schema) {
		return false, nil
	}
	names := map[string]bool{}
	enumFromNames(f.Schema, names)
	full, err := c.fullSchema(f.Schema, c.resolveEnumFrom(cat, tenant, defaultAttachmentClass, names))
	if err != nil {
		return false, err
	}
	return c.rs.validate(full, v) == nil, nil
}

// sourceFilePut checks a file.put of a source channel: on behalf of its hook:<source> only, and
// not above the source's body limit (ING-1, ING-3). It returns the producer.
func (c *Core) sourceFilePut(ctx context.Context, tx pgx.Tx, now time.Time, tenant string, ch manifest.Channel, ob *OnBehalf, size int) (string, error) {
	if !ingressOn() || ch.Ingress == nil {
		return "", refuse("not_ready", "the source channel has no ingress block")
	}
	source := ch.Ingress.Source()
	if ob == nil || ob.Identity != "hook:"+source {
		return "", refuse("forbidden", "a source channel puts a file only on behalf of hook:%s", source)
	}
	if lim := ch.Ingress.Limits.MaxBodyBytes; lim > 0 && int64(size) > lim {
		return "", refuse("file_too_large", "the body is larger than the source's max_body_bytes")
	}
	return hookPrincipal(ctx, tx, now, tenant, source)
}

// systemOpsLists says whether the group system-ops lists task:<type> itself, without a
// wildcard: a route from ext.* enqueues only such a type (PRM-10, ING-4).
func systemOpsLists(tv *TenantView, typ string) bool {
	for _, g := range groupClosure(tv, []string{"system-ops"}) {
		if slices.Contains(tv.Groups[g].Caps, "task:"+typ) {
			return true
		}
	}
	return false
}
