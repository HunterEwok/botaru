// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
)

// The language of code texts (DLV-18): the core takes, for each delivery, the first value of
// the order for which the installation has a catalog: the recipient's own locale (the
// principal's for a person or a private chat, the chat binding's for a group chat); for a
// delivery of a schedule task to the schedule's deliver_to, the schedule's locale; the
// tenant's locale; en. A key missing in the chosen catalog renders its en text (texts.Render).

// pickLocale is the first of the candidates with a loaded catalog, or en.
func (c *Core) pickLocale(cands ...string) string {
	have := c.texts.Locales()
	for _, l := range cands {
		if l != "" && slices.Contains(have, l) {
			return l
		}
	}
	return "en"
}

// bindingLocale is the locale of the bound group chat of a channel, or "".
func bindingLocale(ctx context.Context, q pgx.Tx, tenant, channel, chat string) (string, error) {
	var l *string
	err := q.QueryRow(ctx, `SELECT locale FROM chat_bindings WHERE tenant = $1 AND channel = $2 AND chat = $3`, tenant, channel, chat).Scan(&l)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return deref(l), err
}

// principalLocale is a principal's own locale, or "".
func principalLocale(ctx context.Context, q pgx.Tx, tenant, id string) (string, error) {
	var l *string
	err := q.QueryRow(ctx, `SELECT locale FROM principals WHERE tenant = $1 AND id = $2`, tenant, id).Scan(&l)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return deref(l), err
}

// taskSchedule is the schedule of the tree of a task, or nil.
func taskSchedule(ctx context.Context, tx pgx.Tx, cat *Catalog, tenant, task string) (*scheduleRef, error) {
	if task == "" {
		return nil, nil
	}
	var rec []byte
	err := tx.QueryRow(ctx, `SELECT record FROM tasks WHERE id = $1`, task).Scan(&rec)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var r treeRecord
	if err := json.Unmarshal(rec, &r); err != nil {
		return nil, err
	}
	name, err := scheduleOf(ctx, tx, r, task)
	if err != nil || name == "" {
		return nil, err
	}
	tv := cat.Tenants[tenant]
	if tv == nil {
		return nil, nil
	}
	sc, ok := tv.Schedules[name]
	if !ok {
		return nil, nil
	}
	return &scheduleRef{locale: sc.Locale, deliverTo: sc.DeliverTo}, nil
}

type scheduleRef struct {
	locale    string
	deliverTo []any
}

// deliveryLocale is the language of a delivery by the order of DLV-18.
func (c *Core) deliveryLocale(ctx context.Context, tx pgx.Tx, cat *Catalog, tenant, channel, targetKind, targetID, recipient, task, addr string) (string, error) {
	var own string
	var err error
	switch {
	case targetKind == "chat" && strings.HasPrefix(targetID, "group:"):
		own, err = bindingLocale(ctx, tx, tenant, channel, targetID)
	case recipient != "":
		own, err = principalLocale(ctx, tx, tenant, recipient)
	}
	if err != nil {
		return "", err
	}
	sched := ""
	if s, err := taskSchedule(ctx, tx, cat, tenant, task); err != nil {
		return "", err
	} else if s != nil && slices.Contains(s.deliverTo, any(addr)) {
		sched = s.locale
	}
	return c.pickLocale(own, sched, tenantLocale(cat, tenant)), nil
}

// tenantLocale is Tenant.spec.locale, or "".
func tenantLocale(cat *Catalog, tenant string) string {
	if tv := cat.Tenants[tenant]; tv != nil {
		return tv.Spec.Locale
	}
	return ""
}

// replyLocale is the reply_locale of an attempt (DLV-18): the same order for the recipient of
// the task result: for a task of a schedule its addresses (the locale of a bound chat among
// them) and then the schedule's locale; otherwise the bound chat of a request from a group chat
// or the initiator; then the tenant's, en. The model does not choose it.
func (c *Core) replyLocale(ctx context.Context, tx pgx.Tx, cat *Catalog, tenant, task, initiator string, r treeRecord) (string, error) {
	tl := tenantLocale(cat, tenant)
	// A task of a schedule answers its addresses: the locale of the first bound chat among
	// them that has one, then the schedule's.
	if s, err := taskSchedule(ctx, tx, cat, tenant, task); err != nil {
		return "", err
	} else if s != nil {
		own := ""
		for _, x := range s.deliverTo {
			addr, _ := x.(string)
			kind, name, _ := strings.Cut(addr, ":")
			if kind != "chat" {
				continue
			}
			b, err := bindingByName(ctx, tx, tenant, name)
			if err != nil {
				return "", err
			}
			if b != nil && b.Locale != "" && slices.Contains(c.texts.Locales(), b.Locale) {
				own = b.Locale
				break
			}
		}
		return c.pickLocale(own, s.locale, tl), nil
	}
	own := ""
	var err error
	if o := r.Origin; o != nil && o.Chat.Kind == "group" {
		own, err = bindingLocale(ctx, tx, tenant, o.Channel, o.Chat.ID())
	} else if initiator != "" {
		own, err = principalLocale(ctx, tx, tenant, initiator)
	}
	if err != nil {
		return "", err
	}
	return c.pickLocale(own, tl), nil
}
