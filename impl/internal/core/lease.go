// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"bytes"
	"context"
	"crypto/hmac"
	"encoding/base64"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/ijson"
	"github.com/HunterEwok/botaru/impl/internal/jcs"
)

// LeaseToken is the lease token of urn:botaru:schema:lease:1#/$defs/token. The core signs it
// with the token key; the holder returns it as received in the Botaru-Lease header.
type LeaseToken struct {
	Epoch    int64  `json:"epoch"`
	WorkID   string `json:"work_id"`
	LeaseNo  int    `json:"lease_no"`
	WorkerID string `json:"worker_id"`
	KeyID    string `json:"key_id,omitempty"`
	Sig      string `json:"sig,omitempty"`
}

func (t LeaseToken) fields() map[string]any {
	m := map[string]any{"epoch": t.Epoch, "work_id": t.WorkID, "lease_no": t.LeaseNo, "worker_id": t.WorkerID}
	if t.KeyID != "" {
		m["key_id"] = t.KeyID
	}
	return m
}

// MarshalJSON writes the token in its canonical form (RFC 8785), so that a holder that
// compacts the received JSON gets exactly the header value.
func (t LeaseToken) MarshalJSON() ([]byte, error) {
	m := t.fields()
	if t.Sig != "" {
		m["sig"] = t.Sig
	}
	return jcs.Canonical(m)
}

// issueToken signs a token: the signature covers the canonical form of every field but sig
// (spec/hashing.md, "lease token signature").
func (c *Core) issueToken(epoch int64, workID string, leaseNo int, workerID string) (LeaseToken, error) {
	t := LeaseToken{Epoch: epoch, WorkID: workID, LeaseNo: leaseNo, WorkerID: workerID, KeyID: c.cfg.KeyID}
	b, err := jcs.Canonical(t.fields())
	if err != nil {
		return t, err
	}
	t.Sig = c.sign(b)
	return t, nil
}

// EncodeToken writes the header value: base64url without padding of the RFC 8785 form.
func EncodeToken(t LeaseToken) (string, error) {
	b, err := t.MarshalJSON()
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// decodeToken reads a header value. Any malformed token is a lease_mismatch: it can only be a
// changed token, not an old one.
func (c *Core) decodeToken(header string) (LeaseToken, error) {
	var t LeaseToken
	mismatch := refuse("lease_mismatch", "the lease token is not valid")
	raw, err := base64.RawURLEncoding.DecodeString(header)
	if err != nil {
		return t, mismatch
	}
	// The strict parser rejects repeated keys and unsafe numbers; the decoder rejects fields
	// outside the token definition.
	if _, err := ijson.ParseObject(raw); err != nil {
		return t, mismatch
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&t); err != nil {
		return t, mismatch
	}
	if t.WorkID == "" || t.WorkerID == "" || t.LeaseNo < 1 {
		return t, mismatch
	}
	// This core signs every token it issues, so a token without the signature is not one of
	// its tokens (FEN-2, D-87 (5)).
	b, err := jcs.Canonical(t.fields())
	key := c.signingKey(t.KeyID)
	if err != nil || t.Sig == "" || key == nil || !hmac.Equal([]byte(signWith(key, b)), []byte(t.Sig)) {
		return t, mismatch
	}
	return t, nil
}

// leaseRow is a lease record with the facts the checks need.
type leaseRow struct {
	WorkID   string
	LeaseNo  int
	Kind     string
	Tenant   string
	WorkerID string
	Epoch    int64
	State    string
	Until    time.Time
	Outcome  []byte
}

// heldLease checks a call under a lease (FEN-2, spec/bindings/http/README.md) and locks the lease
// row. A repeat of an accepted call on a closed lease returns the stored answer (IDM-3): replay
// is then non-nil and the caller returns it as it is.
func (c *Core) heldLease(ctx context.Context, tx pgx.Tx, w *Worker, header, op string, args any) (*leaseRow, json.RawMessage, error) {
	if header == "" {
		return nil, nil, refuse("fenced", "no lease token")
	}
	t, err := c.decodeToken(header)
	if err != nil {
		return nil, nil, err
	}
	if t.WorkerID != w.ID {
		return nil, nil, refuse("lease_mismatch", "the lease token is of another worker")
	}
	if t.Epoch != c.epoch {
		return nil, nil, refuse("fenced", "the lease token is of another store epoch")
	}
	var l leaseRow
	err = tx.QueryRow(ctx, `SELECT work_id, lease_no, kind, tenant, worker_id, epoch, state, lease_until, outcome FROM leases
		WHERE work_id = $1 AND lease_no = $2 FOR UPDATE`, t.WorkID, t.LeaseNo).
		Scan(&l.WorkID, &l.LeaseNo, &l.Kind, &l.Tenant, &l.WorkerID, &l.Epoch, &l.State, &l.Until, &l.Outcome)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, refuse("lease_mismatch", "no such lease")
	}
	if err != nil {
		return nil, nil, err
	}
	if l.WorkerID != w.ID || l.Tenant != w.Tenant {
		return nil, nil, refuse("lease_mismatch", "the lease is held by another worker")
	}
	if l.State != "active" {
		if len(l.Outcome) > 0 {
			var o struct {
				Op       string          `json:"op"`
				Args     string          `json:"args"`
				Response json.RawMessage `json:"response"`
				Problem  *struct {
					Reason string `json:"reason"`
					Detail string `json:"detail"`
				} `json:"problem"`
			}
			h, _ := jcs.Hash(argsValue(args))
			if json.Unmarshal(l.Outcome, &o) == nil && o.Op == op && o.Args == h {
				if o.Problem != nil { // a stored refusal is repeated as it was (IDM-3)
					return nil, nil, &Problem{Reason: o.Problem.Reason, Detail: o.Problem.Detail}
				}
				return &l, o.Response, nil
			}
		}
		return nil, nil, refuse("fenced", "the lease is no longer active")
	}
	return &l, nil, nil
}

// closeLease ends a lease and stores the accepted answer for repeats (IDM-3).
func closeLease(ctx context.Context, tx pgx.Tx, now time.Time, l *leaseRow, state, reason, op string, args, response any) error {
	h, err := jcs.Hash(argsValue(args))
	if err != nil {
		return err
	}
	resp, err := json.Marshal(response)
	if err != nil {
		return err
	}
	outcome, err := json.Marshal(map[string]any{"op": op, "args": h, "response": json.RawMessage(resp)})
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE leases SET state = $3, reason = $4, outcome = $5, closed_at = $6 WHERE work_id = $1 AND lease_no = $2`,
		l.WorkID, l.LeaseNo, state, nullable(reason), outcome, now); err != nil {
		return err
	}
	// Handles end together with the lease (SEC-3).
	_, err = tx.Exec(ctx, `DELETE FROM handles WHERE work_id = $1 AND lease_no = $2`, l.WorkID, l.LeaseNo)
	return err
}

// argsValue turns request arguments into a JSON value for hashing.
func argsValue(args any) any {
	if args == nil {
		return map[string]any{}
	}
	b, err := json.Marshal(args)
	if err != nil {
		return map[string]any{}
	}
	v, err := ijson.Parse(b)
	if err != nil {
		return map[string]any{}
	}
	return v
}
