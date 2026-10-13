// SPDX-License-Identifier: AGPL-3.0-only

// Package core is the farm core of the reference implementation: the operations of P1 and P2
// over the C1 store, with the C2 permission check in the same transaction as the transition.
// The HTTP binding lives in package api; this package knows nothing about HTTP.
package core

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/spec"
	"github.com/HunterEwok/botaru/impl/internal/store"
	"github.com/HunterEwok/botaru/impl/internal/texts"
)

// Config is what the core needs from the deployment settings.
type Config struct {
	// TokenKey keys the hashes of tokens, handles and secrets and signs lease tokens
	// (spec/hashing.md: a secret hash is keyed).
	TokenKey []byte
	// KeyID names TokenKey in signed lease tokens.
	KeyID string
	// TestBench is true when the deployment set the test bench key (APR-9).
	TestBench bool
	// Lease and heartbeat periods: the farm defaults and ceilings (FEN-3).
	LeaseS, HeartbeatS int
	// DeliveryMaxAttempts is the farm ceiling of delivery attempts (DLV-3).
	DeliveryMaxAttempts int
	Logger              *slog.Logger
}

func (c *Config) defaults() {
	if c.LeaseS == 0 {
		c.LeaseS = 60
	}
	if c.HeartbeatS == 0 {
		c.HeartbeatS = 20
	}
	if c.DeliveryMaxAttempts == 0 {
		c.DeliveryMaxAttempts = 5
	}
	if c.KeyID == "" {
		c.KeyID = "k1"
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
}

// Core holds the store, the specification and the caches.
type Core struct {
	cfg   Config
	st    *store.Store
	spec  *spec.Spec
	texts *texts.Catalog
	cat   catalogCache
	wake  *waker
	epoch int64
	log   *slog.Logger
	rs    *resultSchemas
}

// New opens the core over a migrated store.
func New(ctx context.Context, cfg Config, st *store.Store, sp *spec.Spec, tx *texts.Catalog) (*Core, error) {
	cfg.defaults()
	if len(cfg.TokenKey) < 32 {
		return nil, errors.New("core: the token key must be at least 32 bytes")
	}
	meta, err := st.Init(ctx, cfg.TestBench)
	if err != nil {
		return nil, err
	}
	c := &Core{cfg: cfg, st: st, spec: sp, texts: tx, epoch: meta.Epoch, log: cfg.Logger, rs: newResultSchemas()}
	c.wake = newWaker()
	if _, err := c.Catalog(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

// Spec returns the loaded specification.
func (c *Core) Spec() *spec.Spec { return c.spec }

// Store returns the store.
func (c *Core) Store() *store.Store { return c.st }

// Epoch returns the store epoch.
func (c *Core) Epoch() int64 { return c.epoch }

// Problem is a refusal with a reason code from spec/model/reasons.yaml. The HTTP binding turns
// it into problem details (RFC 9457).
type Problem struct {
	Reason      string
	Detail      string
	RetryAfterS int
	Needs       []string
}

func (p *Problem) Error() string {
	if p.Detail != "" {
		return p.Reason + ": " + p.Detail
	}
	return p.Reason
}

// refuse returns a problem with a reason and a detail without tenant content.
func refuse(reason, format string, args ...any) *Problem {
	return &Problem{Reason: reason, Detail: fmt.Sprintf(format, args...)}
}

// AsProblem returns the problem inside err, if any.
func AsProblem(err error) (*Problem, bool) {
	var p *Problem
	if errors.As(err, &p) {
		return p, true
	}
	return nil, false
}

// hashSecret is the keyed hash of a token, handle or code (spec/hashing.md, "Hashes of secrets").
func (c *Core) hashSecret(secret string) string {
	m := hmac.New(sha256.New, c.cfg.TokenKey)
	m.Write([]byte("botaru.secret.v1\x00"))
	m.Write([]byte(secret))
	return "hmac-sha256:" + hex.EncodeToString(m.Sum(nil))
}

func (c *Core) sign(b []byte) string {
	m := hmac.New(sha256.New, c.cfg.TokenKey)
	m.Write([]byte("botaru.lease.v1\x00"))
	m.Write(b)
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// tx runs fn in a store transaction and gives it the store clock.
func (c *Core) tx(ctx context.Context, fn func(tx pgx.Tx, now time.Time) error) error {
	return c.st.Tx(ctx, func(tx pgx.Tx) error {
		now, err := store.Now(ctx, tx)
		if err != nil {
			return err
		}
		return fn(tx, now)
	})
}

// fmtTime writes a UTC instant in the datetime form of the schemas.
func fmtTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000000Z") }

// Texts returns the code text catalog.
func (c *Core) Texts() *texts.Catalog { return c.texts }
