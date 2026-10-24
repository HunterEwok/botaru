// SPDX-License-Identifier: AGPL-3.0-only

package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The provider key of the compose farm is the secret provider-key-demo of tenant demo, so
// secret.revoke of it holds the tenant; the core's own secrets are those of its settings and
// farm_secrets, the names TestDeployStartAndDoctor starts the compose description with (SEC-6).
func TestProviderKeySecrets(t *testing.T) {
	c, err := Load("../../deploy/compose/core.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if got := c.ProviderKeySecrets()["provider-key-demo"]; !slices.Equal(got, []string{"demo"}) {
		t.Fatalf("provider key secrets %v", c.ProviderKeySecrets())
	}
	if got := c.CoreOwnSecrets(); !slices.Equal(got, []string{"farm-operator", "operator", "store-dsn", "token-key"}) {
		t.Fatalf("core secrets %v", got)
	}
}

// A P1 address other machines can reach needs the TLS files, unless the settings say the
// network is private; the two files go together (spec/bindings/http).
func TestP1Transport(t *testing.T) {
	load := func(extra string) error {
		t.Helper()
		p := filepath.Join(t.TempDir(), "core.yaml")
		base := "store_dsn_file: /x\ntoken_key_file: /y\n"
		if err := os.WriteFile(p, []byte(base+extra), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := Load(p)
		return err
	}
	for _, ok := range []string{"", "p1_listen: 127.0.0.1:8401\n", "p1_listen: \"[::1]:8401\"\n", "p1_listen: localhost:8401\n",
		"p1_listen: 0.0.0.0:8401\np1_plain_private_network: true\n",
		"p1_listen: 0.0.0.0:8401\np1_tls_cert_file: /c\np1_tls_key_file: /k\n"} {
		if err := load(ok); err != nil {
			t.Fatalf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"p1_listen: 0.0.0.0:8401\n", "p1_listen: :8401\n", "p1_listen: 192.0.2.5:8401\n",
		"p1_tls_cert_file: /c\n"} {
		if err := load(bad); err == nil || !strings.Contains(err.Error(), "p1_") {
			t.Fatalf("%q: %v", bad, err)
		}
	}
}
