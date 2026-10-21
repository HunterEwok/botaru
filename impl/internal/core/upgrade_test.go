// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"os"
	"strings"
	"testing"
)

// Notices planned by the first version carry the old text keys and fewer parameters. The
// store migration renames the keys of notices not sent yet, and the core fills the missing
// parameters at start, so the channel renders them without a raw placeholder.
func TestUpgradePlannedNotices(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newBench(t)
	b.hello(t, "51")
	pool := b.c.st.Pool
	// The rows as the first version wrote them.
	if _, err := pool.Exec(ctx, `UPDATE deliveries SET body = '{"form": "text_code", "text_key": "ai_notice"}' WHERE kind = 'ai_notice'`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE deliveries SET body = '{"form": "text_code", "text_key": "privacy_notice", "params": {"notice_version": "9"}}' WHERE kind = 'privacy_notice'`); err != nil {
		t.Fatal(err)
	}
	// The key rewrite of migration 0002.
	sql, err := os.ReadFile("../store/migrations/0002_records_v01.sql")
	if err != nil {
		t.Fatal(err)
	}
	ran := 0
	for _, stmt := range strings.Split(string(sql), ";") {
		if i := strings.Index(stmt, "UPDATE deliveries"); i >= 0 {
			if _, err := pool.Exec(ctx, stmt[i:]); err != nil {
				t.Fatal(err)
			}
			ran++
		}
	}
	if ran != 2 {
		t.Fatalf("migration 0002 has %d delivery rewrites", ran)
	}
	cat, err := b.c.Catalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.c.fillNoticeParams(ctx, cat); err != nil {
		t.Fatal(err)
	}
	rows, err := pool.Query(ctx, `SELECT kind, body->>'text_key', body->'params' FROM deliveries WHERE kind IN ('ai_notice', 'privacy_notice') ORDER BY kind`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var kind, key string
		var params map[string]any
		if err := rows.Scan(&kind, &key, &params); err != nil {
			t.Fatal(err)
		}
		n++
		if want := map[string]string{"ai_notice": "notice.ai", "privacy_notice": "notice.privacy"}[kind]; key != want {
			t.Fatalf("%s has key %q", kind, key)
		}
		if kind == "privacy_notice" && params["notice_version"] != "9" {
			t.Fatalf("a stored value changed: %v", params)
		}
		text := b.c.texts.Render("en", key, params)
		if strings.Contains(text, "{") || (kind == "ai_notice" && strings.Contains(text, " - ")) {
			t.Fatalf("%s renders %q", kind, text)
		}
	}
	if n != 2 {
		t.Fatalf("%d notices", n)
	}
}
