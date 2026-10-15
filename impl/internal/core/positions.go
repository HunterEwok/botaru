// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"regexp"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/store"
)

// Positions, rank ceilings and personas (GOV-3, GOV-4, IDN-9, docs/bots.md "Positions and
// models"). The core derives a position from the role profile and the binding; it is not a
// manifest. A position changes the persona, the model rank ceiling and admission to its seat's
// reviews, and none of them is part of the permission check.

// positionOf is the position of an identity of a role, acting on its own or on behalf of a
// seat, with the role profile whose ceilings it takes. This version has no owners of workers
// (IDN-5), so a worker profile is always a functionary.
func positionOf(cat *Catalog, role string, seat bool) (position, profile string) {
	if seat {
		return "commission", "reviewer"
	}
	switch p := profileOf(cat, role); p {
	case "reviewer":
		return "commission", p
	case "translator":
		return "translator", p
	case "farm-manager", "farm-manager-reader":
		return "farm_manager", p
	default:
		return "functional", p
	}
}

// rankCeiling is the rank ceiling of a position for a model slot: the role profile ceiling of
// spec/model/role-profiles.yaml, lowered by the charter's positions block (GOV-3). ok is false
// when the profile has no ceiling for the slot.
func (c *Core) rankCeiling(cat *Catalog, tenant, position, profile, slot string) (int, bool) {
	key := "max_rank"
	if slot == "analysis" {
		key = "analysis_max_rank"
	}
	reg, err := c.spec.Registry("spec/model/role-profiles.yaml")
	if err != nil {
		return 0, false
	}
	e, _ := reg.Entry(profile)
	ceil, ok := mnum(e, key)
	if !ok {
		return 0, false
	}
	pos := mmap(mmap(tenantSpecOf(cat, "Governance", tenant), "positions"), position)
	if v, ok := mnum(pos, key); ok && v < ceil {
		ceil = v
	}
	return int(ceil), true
}

// rankAllowed checks a model call against the ceiling of the acting identity's position at
// the attempt's epoch, not of the role of the worker that runs it; without an identity, the
// ceiling of the worker's role. There are no grants of the manager yet (MGR-18), so nothing
// passes above the ceiling (GOV-3).
func (c *Core) rankAllowed(ctx context.Context, cat *Catalog, g *ModelGrant) (bool, error) {
	acting, err := leaseActing(ctx, c.st.Pool, g.Task, g.Worker)
	if err != nil {
		return false, err
	}
	role := g.Role
	if acting.Bot != "" {
		b, err := loadBot(ctx, c.st.Pool, acting.Bot, false)
		if err != nil {
			return false, err
		}
		if b != nil {
			role = b.Role
		}
	}
	position, profile := positionOf(cat, role, acting.Seat != "")
	ceil, ok := c.rankCeiling(cat, g.Tenant, position, profile, g.Slot)
	if !ok {
		return true, nil
	}
	return g.Profile.Rank <= ceil, nil
}

// personaRef is a persona pinned by the charter: a file of git and its hash (GOV-4, COM-19).
type personaRef struct {
	Ref    string
	SHA256 string
}

// personaPinned says whether a persona reference carries a real hash; the schema also lets an
// example write "..." in its place, and such a persona is not inserted.
func (p personaRef) pinned() bool { return len(p.SHA256) == 64 }

func personaOf(m map[string]any) personaRef {
	return personaRef{Ref: mstr(m, "ref"), SHA256: mstr(m, "sha256")}
}

// charterPersonas are the persona references of a charter spec by their path in it: the
// persona of each position and the addendum of each lens.
func charterPersonas(gov map[string]any) map[string]personaRef {
	out := map[string]personaRef{}
	for pos, v := range mmap(gov, "positions") {
		if p, ok := v.(map[string]any); ok {
			if pr := personaOf(mmap(p, "persona")); pr.Ref != "" {
				out["/positions/"+pos+"/persona"] = pr
			}
		}
	}
	for lens, v := range mmap(mmap(gov, "commission"), "lenses") {
		if l, ok := v.(map[string]any); ok {
			if pr := personaOf(mmap(l, "persona")); pr.Ref != "" {
				out["/commission/lenses/"+lens+"/persona"] = pr
			}
		}
	}
	return out
}

// personaHash is the hex SHA-256 of a persona text.
func personaHash(text string) string {
	h := sha256.Sum256([]byte(text))
	return hex.EncodeToString(h[:])
}

// valuePathMark opens the paragraph of a functionary persona that states the path to a
// stronger model (GOV-4, D-31, D-37); the core leaves the mark out of the instruction.
const valuePathMark = "<!-- value-path -->"

var (
	personaLinkRe = regexp.MustCompile(`(?i)(https?://|ftp://|www\.|mailto:|[a-z0-9._%+-]+@[a-z0-9.-]+\.[a-z]{2,})`)
	personaIDRe   = regexp.MustCompile(`(?i)(\bbot_[0-9a-z]{6,}|\bcommission:[0-9]+|\bfarm-manager\b|\bcse_|\bcmsg_)`)
	personaEnvRe  = regexp.MustCompile(`\b[A-Z][A-Z0-9]*_(KEY|TOKEN|SECRET|PASSWORD)\b`)
	paragraphRe   = regexp.MustCompile(`\n[ \t]*\n`)
)

// paragraphs splits a text into its paragraphs.
func paragraphs(text string) []string {
	var out []string
	for _, p := range paragraphRe.Split(strings.ReplaceAll(text, "\r\n", "\n"), -1) {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// unawareTerms is the dictionary of spec/model/unawareness-terms.yaml by group: act, outcome
// and soul stems (GOV-29). The skeleton ships the en stems only.
type unawareTerms map[string][]string

func (c *Core) unawarenessTerms() (unawareTerms, error) {
	reg, err := c.spec.Registry("spec/model/unawareness-terms.yaml")
	if err != nil {
		return nil, err
	}
	out := unawareTerms{}
	for _, e := range reg.Entries {
		g := mstr(e, "group")
		for _, loc := range []string{"en", "ru", "et"} {
			out[g] = append(out[g], mstrs(e, loc)...)
		}
	}
	return out, nil
}

var wordRe = regexp.MustCompile(`[\p{L}][\p{L}\p{N}_-]*`)

// has says whether a text has a word that starts with a stem of the group, ignoring case.
func (u unawareTerms) has(group, text string) bool {
	for _, w := range wordRe.FindAllString(strings.ToLower(text), -1) {
		for _, s := range u[group] {
			if strings.HasPrefix(w, strings.ToLower(s)) {
				return true
			}
		}
	}
	return false
}

// linked says whether a text links an act (scores, reviews, proposals, votes) with an outcome
// (promotion, a seat, the queue, departure) within one paragraph; the value-path paragraph of
// a functionary persona is the only exception (GOV-29).
func (u unawareTerms) linked(text string, valuePath bool) bool {
	for _, p := range paragraphs(text) {
		if valuePath && strings.HasPrefix(p, valuePathMark) {
			continue
		}
		if u.has("act", p) && u.has("outcome", p) {
			return true
		}
	}
	return false
}

// personaFindings are the lint codes of a persona text for a position, or "lens" for a lens
// addendum (GOV-4, COM-19, GOV-29).
func personaFindings(text, position string, u unawareTerms) []string {
	var out []string
	if len(text) > 4096 {
		out = append(out, "persona_size")
	}
	if personaLinkRe.MatchString(text) || personaIDRe.MatchString(text) || personaEnvRe.MatchString(text) {
		out = append(out, "persona_link")
	}
	valuePath := false
	for _, p := range paragraphs(text) {
		if strings.HasPrefix(p, valuePathMark) {
			valuePath = true
		}
	}
	if valuePath != (position == "functional") {
		out = append(out, "persona_value_path")
	}
	if u.linked(text, position == "functional") {
		out = append(out, "unaware_text")
	} else {
		for _, p := range paragraphs(text) {
			if !strings.HasPrefix(p, valuePathMark) && u.has("soul", p) {
				out = append(out, "unaware_text")
				break
			}
		}
	}
	return out
}

// personaText is the stored text of a pinned persona of a tenant, without the value-path mark.
func personaText(ctx context.Context, q store.Querier, tenant string, p personaRef) (string, error) {
	if !p.pinned() {
		return "", nil
	}
	var body string
	err := q.QueryRow(ctx, `SELECT body FROM personas WHERE tenant = $1 AND sha256 = $2`, tenant, p.SHA256).Scan(&body)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(strings.ReplaceAll(body, valuePathMark, "")), nil
}

// attemptPersona is the trusted text the core puts before the role and type instructions of an
// attempt: the persona of the acting identity's position and, for a lens review, the lens
// addendum (GOV-4, COM-19); with the hashes for the run fingerprint (PRV-1). A role without a
// model has no persona.
func attemptPersona(ctx context.Context, q store.Querier, cat *Catalog, tenant, role string, acting actingOf, lens string) (string, []string, error) {
	if !hasModel(cat, role) {
		return "", nil, nil
	}
	actingRole := role
	if acting.Bot != "" {
		b, err := loadBot(ctx, q, acting.Bot, false)
		if err != nil {
			return "", nil, err
		}
		if b != nil {
			actingRole = b.Role
		}
	}
	position, _ := positionOf(cat, actingRole, acting.Seat != "")
	gov := tenantSpecOf(cat, "Governance", tenant)
	refs := []personaRef{personaOf(mmap(mmap(mmap(gov, "positions"), position), "persona"))}
	if lens != "" {
		refs = append(refs, personaOf(mmap(mmap(mmap(mmap(gov, "commission"), "lenses"), lens), "persona")))
	}
	var texts, hashes []string
	for _, r := range refs {
		t, err := personaText(ctx, q, tenant, r)
		if err != nil {
			return "", nil, err
		}
		if t != "" {
			texts = append(texts, t)
			hashes = append(hashes, r.SHA256)
		}
	}
	return strings.Join(texts, "\n\n"), slices.Clip(hashes), nil
}
