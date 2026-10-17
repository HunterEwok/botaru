// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Tests of secret.revoke and the revocation on compromise (SEC-6).

// secret.revoke reads from the deployment description which tenant processes use a secret: a
// channel's token holds the channel, the provider key the core holds for a tenant holds the
// tenant, a secret of the farm's own processes only is revoked with no hold, a secret of a
// process without a tenant holds every tenant, and uses only in tenants the credential does not
// reach are forbidden (SEC-6, DEP-10).
func TestSecretRevokeByDescription(t *testing.T) {
	ctx := context.Background()
	b := newBench(t)
	holds := func(where string) int {
		return b.count(t, `SELECT count(*) FROM holds WHERE reason = 'compromised' AND `+where)
	}
	revoke := func(doc, secret string) error {
		t.Helper()
		l, err := LintDeploy(b.c.spec, []byte(doc))
		if err != nil {
			t.Fatal(err)
		}
		b.c.deploy = l.Description
		_, err = b.c.SecretRevoke(ctx, b.op, SecretRevokeRequest{Secret: secret, Compromised: true})
		return err
	}
	compose, err := os.ReadFile(filepath.Join(b.c.spec.Root, "impl/deploy/compose/deploy.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	// (a) The shipped compose description: the core's secrets are the farm's, its provider key
	// is tenant demo's, the channel's tokens hold the channel.
	b.c.cfg.ProviderKeySecrets = map[string][]string{"provider-key-demo": {"demo"}}
	if err := revoke(string(compose), "store-dsn"); err != nil || holds("true") != 0 {
		t.Fatalf("a farm secret: %v, holds %d", err, holds("true"))
	}
	if err := revoke(string(compose), "provider-key-demo"); err != nil || holds("scope_kind = 'tenant' AND scope_tenant = 'demo'") != 1 {
		t.Fatalf("the provider key: %v, holds %d", err, holds("scope_kind = 'tenant'"))
	}
	if err := revoke(string(compose), "bot-token"); err != nil || holds("scope_kind = 'channel' AND scope_name = 'telegram'") != 1 {
		t.Fatalf("the channel token: %v", err)
	}
	// (b) A process of no known tenant: every tenant is held.
	b.exec(t, `DELETE FROM holds`)
	if err := revoke(strings.Replace(deployBase, "{name: telegram, kind: channel, tenant: demo, ", "{name: telegram, kind: channel, ", 1), "wt-channel"); err != nil || holds("scope_kind = 'tenant' AND scope_tenant = 'demo'") != 1 {
		t.Fatalf("an unresolved secret: %v", err)
	}
	// Uses only in a tenant the credential does not reach.
	if err := revoke(strings.Replace(deployBase, "tenant: demo, placement: host, user: botaru-read", "tenant: demo2, placement: host, user: botaru-read", 1), "wt-read"); !hasReason(err, "forbidden") {
		t.Fatalf("another tenant's secret: %v", err)
	}
	// A provider key two tenants share: forbidden unless the credential reaches both.
	b.c.cfg.ProviderKeySecrets = map[string][]string{"shared-key": {"demo", "demo2"}}
	if err := revoke(deployBase, "shared-key"); !hasReason(err, "forbidden") {
		t.Fatalf("a shared provider key: %v", err)
	}
	farmRevoke := func(doc, secret string) error {
		t.Helper()
		l, err := LintDeploy(b.c.spec, []byte(doc))
		if err != nil {
			t.Fatal(err)
		}
		b.c.deploy = l.Description
		_, err = b.c.SecretRevoke(ctx, b.farmOp(), SecretRevokeRequest{Secret: secret, Compromised: true})
		return err
	}
	if err := farmRevoke(deployBase, "shared-key"); err != nil || holds("scope_kind = 'tenant' AND scope_tenant = 'demo2'") != 1 {
		t.Fatalf("the farm operator on a shared key: %v", err)
	}
	// A process of tenant farm: the farm operator holds the tenant farm's scope; a tenant
	// operator is forbidden.
	farmDoc := strings.Replace(deployBase, "tenant: demo, placement: sandbox", "tenant: farm, placement: sandbox", 1)
	if err := revoke(farmDoc, "wt-sbx"); !hasReason(err, "forbidden") {
		t.Fatalf("a farm process by a tenant operator: %v", err)
	}
	if err := farmRevoke(farmDoc, "wt-sbx"); err != nil || holds("scope_kind = 'tenant' AND scope_tenant = 'farm'") != 1 {
		t.Fatalf("a farm process by the farm operator: %v", err)
	}
	if err := revoke(deployBase, "nothing-uses-this"); !hasReason(err, "not_found") {
		t.Fatalf("an unknown secret: %v", err)
	}
}

// Revocation on compromise (SEC-6, HLD-2): a worker token stops at the next call with no
// window, the worker scope and the scopes the settings list for a deployment secret get a hold
// without expiry with reason compromised, and only the operator releases it.
func TestSecretRevokeCompromised(t *testing.T) {
	ctx := context.Background()
	b := newBench(t)
	r, err := b.c.Enroll(ctx, b.op, EnrollRequest{Tenant: "demo", Role: "hello", Kind: "agent"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.c.SecretRevoke(ctx, b.op, SecretRevokeRequest{Secret: "unknown-secret", Compromised: true}); !hasReason(err, "not_found") {
		t.Fatalf("unknown secret: %v", err)
	}
	done, err := b.c.SecretRevoke(ctx, b.op, SecretRevokeRequest{Secret: r.Worker.ID, Compromised: true})
	if err != nil || done.ID != r.Worker.ID {
		t.Fatalf("revoke: %+v %v", done, err)
	}
	if _, err := b.c.AuthenticateWorker(ctx, r.Token); !hasReason(err, "unauthenticated") {
		t.Fatalf("the revoked token still works: %v", err)
	}
	b.c.cfg.SecretUses = map[string][]HoldScope{"bot-token-demo": {{Kind: "channel", Tenant: "demo", Name: "telegram"}, {Kind: "channel", Tenant: "other", Name: "x"}}}
	if _, err := b.c.SecretRevoke(ctx, b.op, SecretRevokeRequest{Secret: "bot-token-demo", Compromised: true}); err != nil {
		t.Fatal(err)
	}
	rows, err := b.c.st.Pool.Query(ctx, `SELECT id, scope_kind, scope_name, until IS NULL, author_kind, effects FROM holds WHERE reason = 'compromised' ORDER BY scope_kind`)
	if err != nil {
		t.Fatal(err)
	}
	type hold struct {
		id, kind, name, author string
		forever                bool
		effects                []string
	}
	var hs []hold
	for rows.Next() {
		var h hold
		if err := rows.Scan(&h.id, &h.kind, &h.name, &h.forever, &h.author, &h.effects); err != nil {
			t.Fatal(err)
		}
		hs = append(hs, h)
	}
	rows.Close()
	if len(hs) != 2 || hs[0].kind != "channel" || hs[0].name != "telegram" || hs[1].kind != "worker" || hs[1].name != strings.ToLower(r.Worker.ID) ||
		!hs[0].forever || hs[0].author != "operator" || !slices.Contains(hs[0].effects, "no_delivery") {
		t.Fatalf("holds %+v", hs)
	}
	admin := HoldActor{Principal: b.alice, Rank: "tenant_admin", Tenant: "demo", Via: "channel"}
	if _, err := b.c.HoldRelease(ctx, admin, HoldReleaseRequest{ID: hs[0].id}); !hasReason(err, "forbidden") {
		t.Fatalf("a tenant admin released a compromised hold: %v", err)
	}
	if d, err := b.c.HoldRelease(ctx, OperatorActor(b.op), HoldReleaseRequest{ID: hs[0].id}); err != nil || d.State != "released" {
		t.Fatalf("operator release %+v %v", d, err)
	}
	if _, err := b.c.SecretRevoke(ctx, b.op, SecretRevokeRequest{Connections: map[string]any{"provider": "mail"}, Compromised: true}); err == nil {
		t.Fatal("connections are not built")
	}
}
