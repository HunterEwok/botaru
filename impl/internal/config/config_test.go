// SPDX-License-Identifier: AGPL-3.0-only

package config

import (
	"slices"
	"testing"
)

// The provider key of the compose farm is the secret provider-key-demo of tenant demo, so
// secret.revoke of it holds the tenant (SEC-6).
func TestProviderKeySecrets(t *testing.T) {
	c, err := Load("../../deploy/compose/core.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if got := c.ProviderKeySecrets()["provider-key-demo"]; !slices.Equal(got, []string{"demo"}) {
		t.Fatalf("provider key secrets %v", c.ProviderKeySecrets())
	}
}
