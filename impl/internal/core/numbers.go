// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"
)

// The number check of a model paragraph to people (REP-4): all numbers of a digest come from the
// code table, so the paragraph the model writes holds none. Code reads it by tokens and rejects a
// token with a digit and a number word of the digest locale (number.words of the message
// catalog). A token with a digit (any Unicode number, so also superscripts and fractions) passes
// only as a whole name of the table that also holds letters, as an exact name of the tenant
// catalog of the same digest, or as a date or time equal to the snapshot time or a window bound.
// A word of a multi-word name is no name of its own.

// numberCheck is what the check needs from one digest: the names it may mention, the instants
// its dates and times may name, and the time zone of the tenant.
type numberCheck struct {
	// names are whole names of the table rows; one passes only when it holds letters too.
	names map[string]bool
	// exact are the exact names of the tenant catalog.
	exact map[string]bool
	times []time.Time
	loc   *time.Location
}

func hasNumber(s string) bool { return strings.IndexFunc(s, unicode.IsNumber) >= 0 }
func hasLetter(s string) bool { return strings.IndexFunc(s, unicode.IsLetter) >= 0 }

// nameAllowed says whether a token is a name the digest may mention.
func (n numberCheck) nameAllowed(low string) bool {
	return n.exact[low] || n.names[low] && hasLetter(low)
}

// trimToken strips the punctuation around a token of running text.
func trimToken(s string) string {
	return strings.TrimFunc(s, func(r rune) bool { return unicode.IsPunct(r) && r != '%' || unicode.IsSymbol(r) })
}

// paragraphNumbers reads a paragraph and returns the tokens it checked (tokens with a digit and
// number words) and the ones it rejected.
func (n numberCheck) paragraphNumbers(text string, words []string) (checked, rejected int) {
	isWord := map[string]bool{}
	for _, w := range words {
		isWord[w] = true
	}
	for _, raw := range strings.Fields(text) {
		tok := trimToken(raw)
		if tok == "" {
			continue
		}
		low := strings.ToLower(tok)
		if hasNumber(tok) {
			checked++
			if !n.nameAllowed(low) && !n.timeAllowed(tok) {
				rejected++
			}
			continue
		}
		// A number word, alone or as a part of a compound such as twenty-one or one/two.
		for _, part := range strings.FieldsFunc(low, func(r rune) bool { return unicode.IsPunct(r) || unicode.IsSymbol(r) }) {
			if isWord[part] {
				checked++
				rejected++
				break
			}
		}
	}
	return checked, rejected
}

// timeAllowed says whether a token is a date, a time of day or an instant equal to the snapshot
// time or a window bound, in UTC or in the tenant's zone.
func (n numberCheck) timeAllowed(tok string) bool {
	for _, t := range n.times {
		for _, loc := range []*time.Location{time.UTC, n.loc} {
			if loc == nil {
				continue
			}
			lt := t.In(loc)
			if tok == lt.Format("2006-01-02") || tok == lt.Format("15:04") || tok == lt.Format("2006-01-02T15:04") {
				return true
			}
		}
		if at, err := time.Parse(time.RFC3339Nano, tok); err == nil && at.Truncate(time.Second).Equal(t.Truncate(time.Second)) {
			return true
		}
	}
	return false
}

// digestNumberCheck builds the check of a digest: the names of its table rows (types, titles and
// states) and of the tenant catalog (the ids and names of its lists), and the snapshot time and
// the window start.
func digestNumberCheck(cat *Catalog, tenant string, snap *viewSnapshot) numberCheck {
	n := numberCheck{names: map[string]bool{}, exact: map[string]bool{}, loc: time.UTC}
	whole := func(s string) string { return strings.ToLower(trimToken(strings.TrimSpace(s))) }
	addName := func(s string) {
		if s = whole(s); s != "" && !strings.ContainsFunc(s, unicode.IsSpace) {
			n.names[s] = true
		}
	}
	addExact := func(s string) {
		if s = whole(s); s != "" {
			n.exact[s] = true
		}
	}
	for _, r := range snap.Counts {
		for _, k := range []string{"type", "title", "state"} {
			if s, ok := r[k].(string); ok {
				addName(s)
			}
		}
	}
	for _, v := range tenantSpecOf(cat, "Catalog", tenant) {
		list, _ := v.([]any)
		for _, x := range list {
			m := asMap(x)
			for _, k := range []string{"id", "name", "host", "volume"} {
				if s, ok := m[k].(string); ok {
					addExact(s)
				}
			}
			if h, v := mstr(m, "host"), mstr(m, "volume"); h != "" && v != "" {
				addExact(h + ":" + v)
				addExact("res:" + h + ":" + v)
			}
		}
	}
	if tv := cat.Tenants[tenant]; tv != nil && tv.Spec.Timezone != "" {
		if l, err := time.LoadLocation(tv.Spec.Timezone); err == nil {
			n.loc = l
		}
	}
	if _, at, ok := strings.Cut(snap.ID, ":"); ok {
		if t, err := time.Parse(time.RFC3339Nano, at); err == nil {
			n.times = append(n.times, t)
		}
	}
	if t, err := time.Parse(time.RFC3339Nano, snap.From); err == nil {
		n.times = append(n.times, t)
	}
	return n
}

// recipientLocale is the locale of a digest's recipient: the person's own, else the tenant's
// default, else the fallback of the catalog.
func recipientLocale(ctx context.Context, tx pgx.Tx, tv *TenantView, recipient string) (string, error) {
	if recipient != "" {
		var locale *string
		err := tx.QueryRow(ctx, `SELECT locale FROM principals WHERE tenant = $1 AND id = $2`, tv.Name, recipient).Scan(&locale)
		if err != nil && err != pgx.ErrNoRows {
			return "", err
		}
		if locale != nil && *locale != "" {
			return *locale, nil
		}
	}
	if l := mstr(mmap(tv.Raw, "spec"), "locale"); l != "" {
		return l, nil
	}
	return "en", nil
}
