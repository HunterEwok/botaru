// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/ids"
)

// Invitations and the verification code of privileged groups (PRM-7, PRM-13, PRM-16, IDN-14).
//
// An invitation is single-use, has an expiry and grants the groups it names. Its link code
// carries 128 random bits and is stored only as a keyed hash; the person opens the link, and the
// channel passes /start <code> from the person's private chat. On use the core checks again that
// the creator is active and may grant these groups (PRM-7). An ordinary or delegated group is
// active at once; a privileged one stays pending until the operator checks the person's
// verification code with invite.confirm, or with grant --code for an existing person (PRM-13).
// The verification code is derived from the token key, the tenant and the principal, so the
// store keeps no copy of it, and the person reads it from /me or from the reply to the link.

const (
	// inviteCodePrefix starts the link code, so the channel's /start payload is recognized.
	inviteCodePrefix = "iv1_"
	// inviteTTL is the expiry of an invitation without ttl; maxInviteTTL its ceiling.
	inviteTTL    = 48 * time.Hour
	maxInviteTTL = 30 * 24 * time.Hour
)

// verificationCodeWith is the 8-digit verification code of a principal under a token key.
func verificationCodeWith(key []byte, tenant, principal string) string {
	h := hashSecretWith(key, "botaru.verify.v1\x00"+tenant+"\x00"+principal)
	raw, _ := hex.DecodeString(strings.TrimPrefix(h, "hmac-sha256:"))
	return fmt.Sprintf("%08d", binary.BigEndian.Uint64(raw[:8])%100_000_000)
}

// verificationCode is the person's verification code under the current token key.
func (c *Core) verificationCode(tenant, principal string) string {
	return verificationCodeWith(c.cfg.TokenKey, tenant, principal)
}

// codeMatches checks a verification code under the current key and the keys of the rotation
// window (SEC-5).
func (c *Core) codeMatches(tenant, principal, code string) bool {
	if code == c.verificationCode(tenant, principal) {
		return true
	}
	for _, id := range slices.Sorted(maps.Keys(c.cfg.PreviousKeys)) {
		if code == verificationCodeWith(c.cfg.PreviousKeys[id], tenant, principal) {
			return true
		}
	}
	return false
}

// privilegedNeedsCode says whether a group is granted only with the verification code: a
// privileged group outside the tenant's delegated_groups (PRM-13, PRM-16).
func (c *Core) privilegedNeedsCode(tv *TenantView, cat *Catalog, group string) bool {
	if slices.Contains(tv.Spec.DelegatedGroups, group) && c.delegable(tv, cat, group) {
		return false
	}
	return privilegedGroup(tv, cat, group, map[string]bool{})
}

// requestedByCheck applies the written request of PRM-13 to a privileged grant on a production
// tenant: another active person of the tenant with admin:people, unless the tenant has no active
// principal with admin:people yet (the first admin). It returns the refusal, or nil.
func (c *Core) requestedByCheck(ctx context.Context, tx pgx.Tx, now time.Time, tv *TenantView, op Operator, requestedBy string) (*Problem, error) {
	if tv.Spec.Kind != "production" {
		return nil, nil
	}
	admins, err := c.adminChats(ctx, tx, now, tv, "", "admin:people")
	if err != nil {
		return nil, err
	}
	var anyAdmin bool
	if len(admins) == 0 {
		// An admin without a reachable chat is still an admin: look at the groups.
		rows, err := tx.Query(ctx, `SELECT id FROM principals WHERE tenant = $1 AND state = 'active'`, tv.Name)
		if err != nil {
			return nil, err
		}
		var ps []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, err
			}
			ps = append(ps, id)
		}
		rows.Close()
		for _, id := range ps {
			gs, err := principalGroups(ctx, tx, now, tv, id)
			if err != nil {
				return nil, err
			}
			if hasCap(tv, gs, "admin:people") {
				anyAdmin = true
				break
			}
		}
	} else {
		anyAdmin = true
	}
	if !anyAdmin {
		return nil, nil
	}
	if requestedBy == "" || requestedBy == op.Principal {
		return refuse("forbidden", "a production tenant needs the written request of another admin (--requested-by)"), nil
	}
	p, err := loadPrincipal(ctx, tx, now, tv.Name, requestedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return refuse("forbidden", "requested_by is not an admin of the tenant"), nil
	}
	if err != nil {
		return nil, err
	}
	if p.State != "active" || p.Kind != "human" || !hasCap(tv, groupClosure(tv, p.Groups), "admin:people") {
		return refuse("forbidden", "requested_by is not an active admin of the tenant"), nil
	}
	return nil, nil
}

// notifyAdmins tells every admin of the tenant privately of a grant or a removal of a privileged
// or delegated group (PRM-13, PRM-16).
func (c *Core) notifyAdmins(ctx context.Context, tx pgx.Tx, now time.Time, tv *TenantView, key, subject, group, by string) error {
	chats, err := c.adminChats(ctx, tx, now, tv, "", "admin:people")
	if err != nil {
		return err
	}
	for _, a := range chats {
		if _, err := c.planDelivery(ctx, tx, now, plannedDelivery{
			Tenant: tv.Name, Kind: "notice", Addr: "admins", TargetKind: "chat",
			TargetID: Chat{Kind: "direct", ExternalID: a.ExternalID}.ID(), ExecKind: "channel", ExecName: a.Channel,
			Recipient: a.Principal, DataOrigin: "code",
			Body: map[string]any{"form": "text_code", "text_key": key, "params": map[string]any{"subject": subject, "group": group, "by": by}},
		}); err != nil {
			return err
		}
	}
	return nil
}

// InviteRequest is p2-invite-request.
type InviteRequest struct {
	Op          string   `json:"op"`
	Groups      []string `json:"groups,omitempty"`
	TTL         string   `json:"ttl,omitempty"`
	Invite      string   `json:"invite,omitempty"`
	Code        string   `json:"code,omitempty"`
	RequestedBy string   `json:"requested_by,omitempty"`
}

// InviteResult is p2-invite-response.
type InviteResult struct {
	Invite    string `json:"invite"`
	Code      string `json:"code,omitempty"`
	ExpiresAt string `json:"expires_at,omitempty"`
	State     string `json:"state"`
	refusal   *Problem
}

// inviteRow is a stored invitation.
type inviteRow struct {
	id, tenant, createdBy, state, requestedBy, usedBy string
	groups, privileged                                []string
	expires                                           time.Time
}

func loadInvite(ctx context.Context, tx pgx.Tx, where string, args ...any) (*inviteRow, error) {
	var r inviteRow
	var gs, ps []byte
	var req, used *string
	err := tx.QueryRow(ctx, `SELECT id, tenant, created_by, state, requested_by, used_by, groups, privileged, expires_at FROM invitations WHERE `+where+` FOR UPDATE`, args...).
		Scan(&r.id, &r.tenant, &r.createdBy, &r.state, &req, &used, &gs, &ps, &r.expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r.requestedBy, r.usedBy = deref(req), deref(used)
	_ = json.Unmarshal(gs, &r.groups)
	_ = json.Unmarshal(ps, &r.privileged)
	return &r, nil
}

// Invite runs invite.create, invite.confirm and invite.revoke. In this build P2 has only the
// operator's credentials, so the creator is the operator of the tenant (PRM-14); a privileged
// group needs on a production tenant the written request of another admin (PRM-13).
func (c *Core) Invite(ctx context.Context, op Operator, req InviteRequest) (*InviteResult, error) {
	tv, cat, err := c.requireTenant(ctx, op.Tenant)
	if err != nil {
		return nil, err
	}
	var out *InviteResult
	err = c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		actor := Actor{Principal: op.Principal, Via: "farmctl"}
		switch req.Op {
		case "invite.create":
			if len(req.Groups) == 0 {
				return refuse("input_schema_invalid", "groups are required")
			}
			var privileged []string
			for _, g := range req.Groups {
				if _, ok := tv.Groups[g]; !ok {
					return refuse("not_found", "no such group in the tenant")
				}
				if c.privilegedNeedsCode(tv, cat, g) {
					privileged = append(privileged, g)
				}
			}
			if len(privileged) > 0 {
				if p, err := c.requestedByCheck(ctx, tx, now, tv, op, req.RequestedBy); err != nil || p != nil {
					if err != nil {
						return err
					}
					return p
				}
			}
			ttl := inviteTTL
			if req.TTL != "" {
				d, ok := parseDuration(req.TTL)
				if !ok || d > maxInviteTTL {
					return refuse("input_schema_invalid", "ttl is at most 30d")
				}
				ttl = d
			}
			id, code := ids.New(ids.Invite), inviteCodePrefix+ids.Secret(16)
			gs, _ := json.Marshal(req.Groups)
			ps, _ := json.Marshal(nonNil(privileged))
			expires := now.Add(ttl)
			if _, err := tx.Exec(ctx, `INSERT INTO invitations (id, tenant, created_by, groups, privileged, requested_by, code_hash, state, expires_at, created_at)
				VALUES ($1, $2, $3, $4, $5, $6, $7, 'open', $8, $9)`, id, tv.Name, op.Principal, gs, ps, nullable(req.RequestedBy), c.hashSecret(code), expires, now); err != nil {
				return err
			}
			if _, err := c.appendAudit(ctx, tx, now, Audit{
				Tenant: tv.Name, Kind: "command", Actor: actor, Command: "invite.create", Target: &Target{Kind: "invitation", ID: id}, OK: true,
				After: map[string]any{"groups": req.Groups, "privileged": nonNil(privileged), "expires_at": fmtTime(expires)},
			}); err != nil {
				return err
			}
			out = &InviteResult{Invite: id, Code: code, ExpiresAt: fmtTime(expires), State: "open"}
			return nil
		case "invite.revoke", "invite.confirm":
			r, err := loadInvite(ctx, tx, `id = $1 AND tenant = $2`, req.Invite, tv.Name)
			if err != nil {
				return err
			}
			if r == nil {
				return refuse("not_found", "no such invitation")
			}
			out = &InviteResult{Invite: r.id, ExpiresAt: fmtTime(r.expires), State: r.state}
			if req.Op == "invite.revoke" {
				if err := c.revokeInvite(ctx, tx, now, r, actor); err != nil {
					return err
				}
				out.State = "revoked"
				return nil
			}
			return c.confirmInvite(ctx, tx, now, tv, r, req.Code, actor, out)
		}
		return refuse("input_schema_invalid", "unknown op")
	})
	if err != nil {
		return nil, err
	}
	if out.refusal != nil {
		return nil, out.refusal
	}
	return out, nil
}

// revokeInvite ends an unused invitation; a repeat gives the same answer (IDM-3), a used one
// is already_terminal.
func (c *Core) revokeInvite(ctx context.Context, tx pgx.Tx, now time.Time, r *inviteRow, actor Actor) error {
	switch r.state {
	case "revoked":
		return nil
	case "open":
	default:
		return refuse("already_terminal", "the invitation is used")
	}
	if _, err := tx.Exec(ctx, `UPDATE invitations SET state = 'revoked' WHERE id = $1`, r.id); err != nil {
		return err
	}
	_, err := c.appendAudit(ctx, tx, now, Audit{
		Tenant: r.tenant, Kind: "command", Actor: actor, Command: "invite.revoke", Target: &Target{Kind: "invitation", ID: r.id}, OK: true,
	})
	return err
}

// confirmInvite checks the verification code the person told the operator and makes the pending
// memberships of the invitation active (PRM-13). A wrong code changes nothing and is audited.
func (c *Core) confirmInvite(ctx context.Context, tx pgx.Tx, now time.Time, tv *TenantView, r *inviteRow, code string, actor Actor, out *InviteResult) error {
	switch r.state {
	case "confirmed":
		return nil
	case "used":
	default:
		return refuse("not_found", "the invitation was not used")
	}
	if code == "" || !c.codeMatches(tv.Name, r.usedBy, code) {
		_, err := c.appendAudit(ctx, tx, now, Audit{
			Tenant: tv.Name, Kind: "command", Actor: actor, Command: "invite.confirm", Target: &Target{Kind: "invitation", ID: r.id},
			OK: false, Reason: "forbidden",
		})
		// The refusal keeps its audit record: the caller commits and then refuses.
		out.refusal = refuse("forbidden", "the verification code does not match")
		return err
	}
	rows, err := tx.Query(ctx, `UPDATE memberships SET state = 'active' WHERE tenant = $1 AND principal = $2 AND invite = $3 AND state = 'pending' RETURNING grp`,
		tv.Name, r.usedBy, r.id)
	if err != nil {
		return err
	}
	var groups []string
	for rows.Next() {
		var g string
		if err := rows.Scan(&g); err != nil {
			rows.Close()
			return err
		}
		groups = append(groups, g)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE invitations SET state = 'confirmed' WHERE id = $1`, r.id); err != nil {
		return err
	}
	if _, err := c.appendAudit(ctx, tx, now, Audit{
		Tenant: tv.Name, Kind: "command", Actor: actor, Command: "invite.confirm", Target: &Target{Kind: "invitation", ID: r.id}, OK: true,
		After: map[string]any{"subject": r.usedBy, "groups": nonNil(groups)},
	}); err != nil {
		return err
	}
	for _, g := range groups {
		if err := c.notifyAdmins(ctx, tx, now, tv, "notice.privileged_grant", r.usedBy, g, actor.Principal); err != nil {
			return err
		}
	}
	out.State = "confirmed"
	return nil
}

// inviteCode reads the link code of /start <code> in a private chat, or "".
func inviteCode(a Activity) string {
	word, rest, ok := commandWord(a)
	if !ok || word != "/start" || !strings.HasPrefix(rest, inviteCodePrefix) || strings.ContainsAny(rest, " \n") || len(rest) > 64 {
		return ""
	}
	return rest
}

// useInvitation uses an invitation from the private chat of a verified sender: an unknown
// sender becomes a new person with the sender's identity, a known active person gets the
// groups. A missing, used, revoked or expired invitation, an inactive creator and a group the
// creator may no longer grant all answer with the same text and change nothing (PRM-7).
func (c *Core) useInvitation(ctx context.Context, tx pgx.Tx, now time.Time, tv *TenantView, cat *Catalog, a Activity, code, principal string) (*ActivityResult, error) {
	locale := c.pickLocale("", tv.Spec.Locale)
	invalid := &ActivityResult{Result: "reply", Text: c.texts.Render(locale, "cmd.invite.invalid", nil)}
	r, err := loadInvite(ctx, tx, `tenant = $1 AND code_hash = ANY($2)`, tv.Name, c.secretHashes(code))
	if err != nil || r == nil {
		return invalid, err
	}
	if r.state != "open" || !r.expires.After(now) {
		return invalid, nil
	}
	// The creator is checked again: still active if it is a person of the tenant, and every
	// group still exists; a group that became privileged on a production tenant needs the
	// request the invitation does not carry (PRM-7, PRM-13).
	var creatorState string
	switch err := tx.QueryRow(ctx, `SELECT state FROM principals WHERE tenant = $1 AND id = $2`, tv.Name, r.createdBy).Scan(&creatorState); {
	case err == nil && creatorState != "active":
		return invalid, nil
	case err != nil && !errors.Is(err, pgx.ErrNoRows):
		return nil, err
	}
	var pending []string
	for _, g := range r.groups {
		if _, ok := tv.Groups[g]; !ok {
			return invalid, nil
		}
		if c.privilegedNeedsCode(tv, cat, g) {
			if !slices.Contains(r.privileged, g) && tv.Spec.Kind == "production" && r.requestedBy == "" {
				return invalid, nil
			}
			pending = append(pending, g)
		}
	}
	if principal == "" {
		principal = ids.PrincipalID()
		if _, err := tx.Exec(ctx, `INSERT INTO principals (tenant, id, kind, state, created_by, created_at) VALUES ($1, $2, 'human', 'active', $3, $4)`,
			tv.Name, principal, r.createdBy, now); err != nil {
			return nil, err
		}
		if err := c.setIdentity(ctx, tx, tv.Name, principal, a.Sender.Identity); err != nil {
			return nil, err
		}
	} else {
		var state, kind string
		if err := tx.QueryRow(ctx, `SELECT state, kind FROM principals WHERE tenant = $1 AND id = $2 FOR UPDATE`, tv.Name, principal).Scan(&state, &kind); err != nil {
			return nil, err
		}
		if state != "active" || kind != "human" {
			return invalid, nil
		}
	}
	for _, g := range r.groups {
		state := "active"
		if slices.Contains(pending, g) {
			state = "pending"
		}
		// An active membership stays active; a pending one waits for the code of this invitation.
		if _, err := tx.Exec(ctx, `INSERT INTO memberships (tenant, principal, grp, state, created_at, granted_by, requested_by, invite)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			ON CONFLICT (tenant, principal, grp) DO UPDATE SET invite = CASE WHEN memberships.state = 'active' THEN memberships.invite ELSE EXCLUDED.invite END,
			state = CASE WHEN memberships.state = 'active' THEN 'active' ELSE EXCLUDED.state END`,
			tv.Name, principal, g, state, now, r.createdBy, nullable(r.requestedBy), r.id); err != nil {
			return nil, err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE invitations SET state = 'used', used_by = $2, used_at = $3 WHERE id = $1`, r.id, principal, now); err != nil {
		return nil, err
	}
	if _, err := c.appendAudit(ctx, tx, now, Audit{
		Tenant: tv.Name, Kind: "command", Actor: Actor{Principal: principal, Via: "channel"}, Command: "invite.use",
		Target: &Target{Kind: "invitation", ID: r.id}, OK: true, After: map[string]any{"groups": r.groups, "pending": nonNil(pending)},
	}); err != nil {
		return nil, err
	}
	params := map[string]any{"groups": strings.Join(r.groups, ", ")}
	if len(pending) == 0 {
		// No privileged group waits, so the invitation needs no confirmation.
		if _, err := tx.Exec(ctx, `UPDATE invitations SET state = 'confirmed' WHERE id = $1`, r.id); err != nil {
			return nil, err
		}
		return &ActivityResult{Result: "reply", Text: c.texts.Render(locale, "cmd.invite.done", params)}, nil
	}
	params["code"] = c.verificationCode(tv.Name, principal)
	return &ActivityResult{Result: "reply", Text: c.texts.Render(locale, "cmd.invite.pending", params)}, nil
}

// revokeInvitationsOf revokes the unused invitations a person issued, in the transaction of the
// offboarding (IDN-14).
func revokeInvitationsOf(ctx context.Context, tx pgx.Tx, tenant, subject string) error {
	_, err := tx.Exec(ctx, `UPDATE invitations SET state = 'revoked' WHERE tenant = $1 AND created_by = $2 AND state = 'open'`, tenant, subject)
	return err
}

// onceReply runs a code command of an activity once: a retried update of the same activity
// gets the reply of the first and changes nothing again (DLV-5, IDM-3).
func onceReply(ctx context.Context, tx pgx.Tx, now time.Time, tenant, key string, run func() (*ActivityResult, error)) (*ActivityResult, error) {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 7300105))`, tenant+"/"+key); err != nil {
		return nil, err
	}
	var raw []byte
	err := tx.QueryRow(ctx, `SELECT reply FROM activity_outcomes WHERE tenant = $1 AND key = $2`, tenant, key).Scan(&raw)
	if err == nil {
		var out ActivityResult
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, err
		}
		return &out, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	out, err := run()
	if err != nil {
		return nil, err
	}
	if raw, err = json.Marshal(out); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO activity_outcomes (tenant, key, reply, created_at) VALUES ($1, $2, $3, $4) ON CONFLICT DO NOTHING`,
		tenant, key, raw, now); err != nil {
		return nil, err
	}
	return out, nil
}

// privilegedGrantCheck applies PRM-13 to P2 grant of a privileged group: the subject is an
// active person, a production tenant has the written request of another admin, and a code, if
// given, is the subject's verification code; a wrong code is audited and changes nothing.
func (c *Core) privilegedGrantCheck(ctx context.Context, tx pgx.Tx, now time.Time, tv *TenantView, op Operator, req GrantRequest) (*Problem, error) {
	var state, kind string
	if err := tx.QueryRow(ctx, `SELECT state, kind FROM principals WHERE tenant = $1 AND id = $2`, tv.Name, req.Subject).Scan(&state, &kind); err != nil {
		return nil, err
	}
	if state != "active" || kind != "human" {
		return refuse("person_inactive", "a privileged group is granted to an active person"), nil
	}
	if p, err := c.requestedByCheck(ctx, tx, now, tv, op, req.RequestedBy); err != nil || p != nil {
		return p, err
	}
	if req.Code != "" && !c.codeMatches(tv.Name, req.Subject, req.Code) {
		if _, err := c.appendAudit(ctx, tx, now, Audit{
			Tenant: tv.Name, Kind: "command", Actor: Actor{Principal: op.Principal, Via: "farmctl"}, Command: "grant",
			Target: &Target{Kind: "membership", ID: req.Subject + "/" + req.Group}, OK: false, Reason: "forbidden",
		}); err != nil {
			return nil, err
		}
		return refuse("forbidden", "the verification code does not match"), nil
	}
	return nil, nil
}
