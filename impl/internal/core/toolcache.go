// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"slices"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/jcs"
	"github.com/HunterEwok/botaru/impl/internal/manifest"
	"github.com/HunterEwok/botaru/impl/internal/toolspec"
)

// The tool output cache (PRM-12). Only a plugin tool with access: read and cache_s above 0 is
// cached, never one with a per-principal credential (SEC-7) and never a built-in tool. An
// entry is keyed by the tenant, the plugin, the tool, the arguments and the catalog version:
// the hash of the tenant's Catalog manifest and of the tool definition the call was checked
// against, so an apply that changes either misses. A hit is looked up only after the full
// check of the current call (PRM-3) and applies the flags of the original call again.

// cacheable says whether the output of a tool may be cached.
func cacheable(t toolspec.Tool) bool {
	return t.Access == "read" && t.CacheS > 0 && t.Credential["per_principal"] == nil
}

// catalogVersion is the hash of the tenant's Catalog manifest, "" without one.
func catalogVersion(cat *Catalog, tenant string) string {
	var keys []manifest.Key
	for k := range cat.Hashes {
		if k.Kind == "Catalog" && k.Tenant == tenant {
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].Name < keys[j].Name })
	var hs []string
	for _, k := range keys {
		hs = append(hs, cat.Hashes[k])
	}
	if len(hs) == 1 {
		return hs[0]
	}
	h, _ := jcs.Hash(hs)
	return h
}

// toolCacheKey is the key of a call's entry.
func toolCacheKey(cat *Catalog, tenant, right, defHash string, args map[string]any) (string, error) {
	plugin, tool, _ := cutTool(right)
	return jcs.Hash(map[string]any{"tenant": tenant, "plugin": plugin, "tool": tool, "args": args,
		"catalog": catalogVersion(cat, tenant), "definition": defHash})
}

// cachedOutput is an entry of the cache with the flags of the call that filled it.
type cachedOutput struct {
	Output           []byte
	PersonalSeen     []string
	PersonalSubjects []string
}

// lookupToolCache returns the live entry of a key, or nil.
func lookupToolCache(ctx context.Context, tx pgx.Tx, now time.Time, tenant, key string) (*cachedOutput, error) {
	var out cachedOutput
	var seen, subjects []byte
	err := tx.QueryRow(ctx, `SELECT output, personal_seen, personal_subjects FROM tool_cache WHERE tenant = $1 AND key = $2 AND expires_at > $3`,
		tenant, key, now).Scan(&out.Output, &seen, &subjects)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal(seen, &out.PersonalSeen)
	_ = json.Unmarshal(subjects, &out.PersonalSubjects)
	return &out, nil
}

// storeToolCache keeps the output of a finished call with the flags it gave its attempt: the
// personal_seen sources and the personal_subjects (AUD-5). Expired entries of the tenant go at
// the same time.
func storeToolCache(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, t toolspec.Tool, call string, output []byte, personalSeen []string) error {
	var tenant, right, defHash string
	var argsRaw []byte
	if err := tx.QueryRow(ctx, `SELECT tenant, tool, coalesce(tool_def_hash, ''), args FROM toolcalls WHERE id = $1`, call).Scan(&tenant, &right, &defHash, &argsRaw); err != nil {
		return err
	}
	var args map[string]any
	if err := json.Unmarshal(argsRaw, &args); err != nil {
		return err
	}
	key, err := toolCacheKey(cat, tenant, right, defHash, args)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM tool_cache WHERE tenant = $1 AND expires_at <= $2`, tenant, now); err != nil {
		return err
	}
	if personalSeen == nil {
		personalSeen = []string{}
	}
	seen, _ := json.Marshal(personalSeen)
	plugin, _, _ := cutTool(right)
	_, err = tx.Exec(ctx, `INSERT INTO tool_cache (tenant, key, plugin, tool, output, personal_seen, personal_subjects, source_call, created_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, '[]', $7, $8, $9)
		ON CONFLICT (tenant, key) DO UPDATE SET output = EXCLUDED.output, personal_seen = EXCLUDED.personal_seen,
			personal_subjects = EXCLUDED.personal_subjects, source_call = EXCLUDED.source_call, created_at = EXCLUDED.created_at, expires_at = EXCLUDED.expires_at`,
		tenant, key, plugin, right, output, seen, call, now, now.Add(time.Duration(t.CacheS)*time.Second))
	return err
}

// serveCachedCall ends a checked call from the cache: the call record is done with the cached
// output and the flag cached, the attempt gets untrusted_seen and the flags of the original
// call, and the audit and usage lines carry cached (PRM-12).
func (c *Core) serveCachedCall(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, g *toolsGrant, row toolCallRow, hit *cachedOutput) error {
	row.State = "queued"
	if err := c.insertToolCall(ctx, tx, now, row); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE toolcalls SET cached = true WHERE id = $1`, row.ID); err != nil {
		return err
	}
	// Output from the cache stays untrusted (INJ-13).
	if _, err := tx.Exec(ctx, `UPDATE tasks SET record = record || jsonb_build_object('untrusted_seen', true,
		'untrusted_source', coalesce(record->>'untrusted_source', $2::text)) WHERE id = $1`, g.Task, untrustedSource(row.Tool)); err != nil {
		return err
	}
	for _, s := range hit.PersonalSeen {
		if err := addPersonalSeen(ctx, tx, g.Task, s); err != nil {
			return err
		}
	}
	for _, p := range hit.PersonalSubjects {
		if _, err := tx.Exec(ctx, `UPDATE tasks SET record = jsonb_set(record, '{personal_subjects}',
			coalesce(record->'personal_subjects', '[]'::jsonb) || to_jsonb($2::text)) WHERE id = $1
			AND NOT coalesce(record->'personal_subjects', '[]'::jsonb) ? $2`, g.Task, p); err != nil {
			return err
		}
	}
	// A hit costs nothing at a toolhost: its usage line has no amount and the flag.
	if err := c.reserveToolCall(ctx, tx, now, cat, g, row.ID, 0); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE spend SET cached = true, state = 'committed' WHERE toolcall = $1`, row.ID); err != nil {
		return err
	}
	a := toolAudit(g, row.ID, row.Tool, true, "")
	a.After["cached"] = true
	if _, err := c.appendAudit(ctx, tx, now, a); err != nil {
		return err
	}
	return c.endToolCall(ctx, tx, now, row.ID, "done", "", hit.Output)
}

// purgeToolCache drops the entries a no_tool hold covers (PRM-12): every entry for the farm, the
// entries of the plugin for a plugin hold, and the tenant's entries for any other scope, as an
// entry does not record the role, type, worker or target it was filled for.
func purgeToolCache(ctx context.Context, tx pgx.Tx, s HoldScope) error {
	var err error
	switch {
	case s.Kind == "farm":
		_, err = tx.Exec(ctx, `DELETE FROM tool_cache`)
	case s.Kind == "plugin":
		_, err = tx.Exec(ctx, `DELETE FROM tool_cache WHERE tenant = $1 AND plugin = $2`, s.Tenant, s.Name)
	case s.Tenant != "":
		_, err = tx.Exec(ctx, `DELETE FROM tool_cache WHERE tenant = $1`, s.Tenant)
	}
	return err
}

// holdPurgesCache says whether a new hold drops cached output.
func holdPurgesCache(effects []string) bool { return slices.Contains(effects, "no_tool") }
