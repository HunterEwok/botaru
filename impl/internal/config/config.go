// SPDX-License-Identifier: AGPL-3.0-only

// Package config reads the deployment settings of the core process. The file lives in the
// installation, never in this repository; secrets are referenced by file path and are read at
// start, so the settings file itself holds no secret.
package config

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/HunterEwok/botaru/impl/internal/api"
	"github.com/HunterEwok/botaru/impl/internal/core"
	"github.com/HunterEwok/botaru/impl/internal/yamlx"
)

// Core is the settings file of botaru-core.
type Core struct {
	// SpecRoot is the skeleton checkout the core reads its schemas and registries from.
	SpecRoot string `yaml:"spec_root"`
	// StoreDSNFile holds the PostgreSQL connection string.
	StoreDSNFile string `yaml:"store_dsn_file"`
	// TokenKeyFile holds at least 32 random bytes: the key of token hashes and lease signatures.
	TokenKeyFile string `yaml:"token_key_file"`
	// TokenKeyID names the current token key in signed tokens (SEC-5); default k1.
	TokenKeyID string `yaml:"token_key_id"`
	// PreviousTokenKeys stay valid in the rotation window of the token key: a new key gets a new
	// id, the old one moves here until the window ends (SEC-5, docs/operations.md).
	PreviousTokenKeys []struct {
		ID   string `yaml:"id"`
		File string `yaml:"file"`
	} `yaml:"previous_token_keys"`
	TestBench bool   `yaml:"test_bench"`
	P1Listen  string `yaml:"p1_listen"`
	P2Listen  string `yaml:"p2_listen"`
	// TestListen is the separate listener of the test-control port (spec/ports/test-control.md);
	// empty means no port. It needs test_bench and the hex SHA-256 of the test bench key.
	TestListen    string `yaml:"test_listen"`
	TestKeySHA256 string `yaml:"test_key_sha256"`
	// TextsDir holds extra locales of the code text catalog (<locale>.yaml).
	TextsDir         string             `yaml:"texts_dir"`
	Operators        []api.OperatorCred `yaml:"operators"`
	HealthKeysSHA256 []string           `yaml:"health_keys_sha256"`
	ReportKeysSHA256 []string           `yaml:"report_keys_sha256"`
	Providers        map[string]struct {
		Binding string `yaml:"binding"`
		BaseURL string `yaml:"base_url"`
		// KeyFiles maps a tenant to the file with the key of its provider workspace.
		KeyFiles map[string]string `yaml:"key_files"`
	} `yaml:"providers"`
	// Operator names the farm operator in the ai_notice text (notice.ai, DLV-12).
	Operator            string `yaml:"operator"`
	LeaseS              int    `yaml:"lease_s"`
	HeartbeatS          int    `yaml:"heartbeat_s"`
	DeliveryMaxAttempts int    `yaml:"delivery_max_attempts"`
	// FarmHosts are the farm nodes of the deployment description (DEP-11), which no tenant
	// catalog may name.
	FarmHosts []string `yaml:"farm_hosts"`
	// Health holds the thresholds of the health indicators and the store volume (OBS-9, CAP-5).
	Health core.HealthConfig `yaml:"health"`
	// Spend holds the reconciliation settings with the provider report (BUD-10).
	Spend core.SpendConfig `yaml:"spend"`
	// Backup holds the backup locations the core watches (RET-8).
	Backup core.BackupConfig `yaml:"backup"`
	// Monitor turns on the monitor profile: alarms go to the monitoring system only (D-88).
	Monitor core.MonitorConfig `yaml:"monitor"`
}

// Load reads and checks a settings file.
func Load(path string) (*Core, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Core
	if err := yamlx.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if c.P1Listen == "" {
		c.P1Listen = "127.0.0.1:8401"
	}
	if c.P2Listen == "" {
		c.P2Listen = "127.0.0.1:8402"
	}
	if c.StoreDSNFile == "" || c.TokenKeyFile == "" {
		return nil, errors.New("config: store_dsn_file and token_key_file are required")
	}
	if c.P1Listen == c.P2Listen {
		return nil, errors.New("config: P2 listens separately from P1 (IDN-7)")
	}
	if c.TestKeySHA256 != "" && !c.TestBench {
		// A test bench key outside a test bench must stop the start (APR-9).
		return nil, errors.New("config: test_key_sha256 is set, but test_bench is not")
	}
	if c.TestListen != "" {
		if c.TestKeySHA256 == "" {
			return nil, errors.New("config: test_listen needs test_key_sha256")
		}
		if c.TestListen == c.P1Listen || c.TestListen == c.P2Listen {
			return nil, errors.New("config: the test-control port listens separately from P1 and P2")
		}
	}
	return &c, nil
}

// TokenKeys reads the current token key and the previous keys of the rotation window (SEC-5).
func (c *Core) TokenKeys() (key []byte, id string, previous map[string][]byte, err error) {
	k, err := Secret(c.TokenKeyFile)
	if err != nil {
		return nil, "", nil, err
	}
	previous = map[string][]byte{}
	for _, p := range c.PreviousTokenKeys {
		pk, err := Secret(p.File)
		if err != nil {
			return nil, "", nil, err
		}
		previous[p.ID] = []byte(pk)
	}
	return []byte(k), c.TokenKeyID, previous, nil
}

// Secret reads a secret file and trims the trailing newline.
func Secret(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(b), "\r\n"), nil
}
