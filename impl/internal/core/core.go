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
	"maps"
	"slices"
	"sync/atomic"
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
	// Monitor is the monitor profile of the installation (D-88).
	Monitor MonitorConfig
	// PreviousKeys are the token keys of the rotation window by key id: tokens they signed and
	// secrets they hashed stay valid until the operator removes them (SEC-5).
	PreviousKeys map[string][]byte
	// TestBench is true when the deployment set the test bench key (APR-9).
	TestBench bool
	// Lease and heartbeat periods: the farm defaults and ceilings (FEN-3).
	LeaseS, HeartbeatS int
	// DeliveryMaxAttempts is the farm ceiling of delivery attempts (DLV-3).
	DeliveryMaxAttempts int
	// Operator names the farm operator in the ai_notice text (notice.ai).
	Operator string
	Logger   *slog.Logger
	// Offline opens the core for one command of farmctl on the core machine (DEP-8): it
	// changes nothing at start and runs no loops.
	Offline bool
	// FarmHosts are the farm nodes of the deployment description (DEP-11): the machines of the
	// core, the store, farmctl, the channels, the watchdog and the backups. A tenant catalog
	// never names them.
	FarmHosts []string
	// Health holds the thresholds of the health indicators and the store headroom (OBS-9,
	// CAP-5).
	Health HealthConfig
	// Spend holds the reconciliation settings (BUD-10).
	Spend SpendConfig
	// Backup holds the backup locations the core watches (RET-8).
	Backup BackupConfig
	// Files holds the farm ceilings of file sizes (ART-4).
	Files FilesConfig
	// SecretUses are the hold scopes of each named deployment secret, as the deployment
	// description uses it (SEC-6): what secret.revoke stops besides the core's own credentials.
	SecretUses map[string][]HoldScope
	// ExportKeys are the public keys of export sinks by their key_ref (x25519:<base64>), for a
	// tenant export and for a person's copy without a key of its own request (LCY-2, SEC-10).
	ExportKeys map[string]string
	// FreeSpace, when set, measures the store volume instead of Health.StoreVolume: free and
	// total bytes. A test sets a mock measurement here.
	FreeSpace func() (free, total uint64, err error)
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
	c.Health.defaults()
	c.Spend.defaults()
	c.Files.defaults()
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
	// capacity is the state of the store volume by the last measurement (CAP-5).
	capacity atomic.Int32
	// storeDown is set when the store stopped answering and cleared when every loop ticked
	// again after it returned (OBS-8).
	storeDown atomic.Bool
	// budgetAt and retentionAt are the store times of the last slow passes.
	budgetAt, retentionAt atomic.Int64
	// restoring is set while a restore from a backup waits for restore.complete (RET-6).
	restoring atomic.Bool
	// manual is set when the loops tick only by loop.tick on a test bench.
	manual atomic.Bool
	// running is set when Run drives the loops; startedAt is the store time, in Unix
	// nanoseconds, at which Run started (or of the first indicators pass, if the store did not
	// answer then), from which a loop that never ticked counts its age (OBS-4).
	running   atomic.Bool
	startedAt atomic.Int64
}

// New opens the core over a migrated store.
func New(ctx context.Context, cfg Config, st *store.Store, sp *spec.Spec, tx *texts.Catalog) (*Core, error) {
	cfg.defaults()
	if len(cfg.TokenKey) < 32 {
		return nil, errors.New("core: the token key must be at least 32 bytes")
	}
	for id, k := range cfg.PreviousKeys {
		if len(k) < 32 || id == cfg.KeyID || id == "" {
			return nil, fmt.Errorf("core: previous token key %q must be at least 32 bytes and have its own key id", id)
		}
	}
	meta, err := st.Init(ctx, cfg.TestBench)
	if err != nil {
		return nil, err
	}
	c := &Core{cfg: cfg, st: st, spec: sp, texts: tx, epoch: meta.Epoch, log: cfg.Logger, rs: newResultSchemas()}
	c.wake = newWaker()
	cat, err := c.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	if cfg.Offline {
		return c, nil
	}
	if err := c.settleRestore(ctx); err != nil {
		return nil, err
	}
	if err := c.loadRestoring(ctx); err != nil {
		return nil, err
	}
	c.warnNoticeFallbacks(cat)
	if err := c.fillNoticeParams(ctx, cat); err != nil {
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
func (c *Core) hashSecret(secret string) string { return hashSecretWith(c.cfg.TokenKey, secret) }

// secretHashes are the hashes of a secret under the current key and every previous key of the
// rotation window, the current first, for a lookup (SEC-5).
func (c *Core) secretHashes(secret string) []string {
	out := []string{c.hashSecret(secret)}
	for _, id := range slices.Sorted(maps.Keys(c.cfg.PreviousKeys)) {
		out = append(out, hashSecretWith(c.cfg.PreviousKeys[id], secret))
	}
	return out
}

func hashSecretWith(key []byte, secret string) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte("botaru.secret.v1\x00"))
	m.Write([]byte(secret))
	return "hmac-sha256:" + hex.EncodeToString(m.Sum(nil))
}

func (c *Core) sign(b []byte) string { return signWith(c.cfg.TokenKey, b) }

// signingKey is the key a lease token names by its key id: the current or a previous key of
// the rotation window (SEC-5).
func (c *Core) signingKey(id string) []byte {
	if id == c.cfg.KeyID {
		return c.cfg.TokenKey
	}
	return c.cfg.PreviousKeys[id]
}

func signWith(key []byte, b []byte) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte("botaru.lease.v1\x00"))
	m.Write(b)
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// tx runs fn in a store transaction and gives it the store clock. A committed refusal of fn
// commits its writes and is returned as its problem.
func (c *Core) tx(ctx context.Context, fn func(tx pgx.Tx, now time.Time) error) error {
	var cm *committed
	err := c.st.Tx(ctx, func(tx pgx.Tx) error {
		cm = nil
		now, err := store.Now(ctx, tx)
		if err != nil {
			return err
		}
		err = fn(tx, now)
		if errors.As(err, &cm) {
			return nil
		}
		return err
	})
	if err == nil && cm != nil {
		return cm.p
	}
	return err
}

// fmtTime writes a UTC instant in the datetime form of the schemas.
func fmtTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000000Z") }

// Texts returns the code text catalog.
func (c *Core) Texts() *texts.Catalog { return c.texts }
