// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/jcs"
	"github.com/HunterEwok/botaru/impl/internal/manifest"
	"github.com/HunterEwok/botaru/impl/internal/store"
	"github.com/HunterEwok/botaru/impl/internal/toolspec"
)

// Catalog is the applied manifest set as typed views. It is rebuilt from the manifests table
// after every apply and is read-only afterwards.
type Catalog struct {
	SetHash   string
	Tenants   map[string]*TenantView
	Roles     map[string]manifest.Role
	TaskTypes map[string]manifest.TaskType
	// Profiles are keyed by tenant and name; a farm-wide profile has the tenant "".
	Profiles map[[2]string]manifest.ModelProfile
	// Plugins are the ToolPlugin manifests by name (global, D-92).
	Plugins    map[string]toolspec.Plugin
	RawGlobals map[manifest.Key]map[string]any
	// Hashes are the hashes of the applied manifests (spec/hashing.md), for the parts of the
	// run fingerprint (PRV-1) and the catalog version of an attempt (OUT-1).
	Hashes map[manifest.Key]string
}

// TenantView holds a tenant manifest and the objects of that tenant.
type TenantView struct {
	Name     string
	Spec     manifest.Tenant
	Groups   map[string]manifest.Group
	Channels map[string]manifest.Channel
}

// Profile resolves a model profile for a tenant: the tenant's own profile of that name, else
// the farm-wide one (ModelProfile has scope either).
func (c *Catalog) Profile(tenant, name string) (manifest.ModelProfile, bool) {
	if p, ok := c.Profiles[[2]string{tenant, name}]; ok {
		return p, true
	}
	p, ok := c.Profiles[[2]string{"", name}]
	return p, ok
}

func newCatalog() *Catalog {
	return &Catalog{
		Tenants:    map[string]*TenantView{},
		Roles:      map[string]manifest.Role{},
		TaskTypes:  map[string]manifest.TaskType{},
		Profiles:   map[[2]string]manifest.ModelProfile{},
		Plugins:    map[string]toolspec.Plugin{},
		RawGlobals: map[manifest.Key]map[string]any{},
		Hashes:     map[manifest.Key]string{},
	}
}

func (c *Catalog) tenant(name string) *TenantView {
	t, ok := c.Tenants[name]
	if !ok {
		t = &TenantView{Name: name, Groups: map[string]manifest.Group{}, Channels: map[string]manifest.Channel{}}
		c.Tenants[name] = t
	}
	return t
}

// buildCatalog decodes a manifest set. The documents have already passed the schemas.
func buildCatalog(docs []manifest.Doc) (*Catalog, error) {
	c := newCatalog()
	for _, d := range docs {
		h, err := jcs.Hash(d.Value)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", d.Source, err)
		}
		c.Hashes[d.Key()] = h
		switch d.Kind {
		case "Tenant":
			var t manifest.Tenant
			if t, err = manifest.Decode[manifest.Tenant](d); err == nil {
				c.tenant(d.Name).Spec = t
			}
		case "Group":
			var g manifest.Group
			if g, err = manifest.Decode[manifest.Group](d); err == nil {
				c.tenant(d.Tenant).Groups[d.Name] = g
			}
		case "Channel":
			var ch manifest.Channel
			if ch, err = manifest.Decode[manifest.Channel](d); err == nil {
				c.tenant(d.Tenant).Channels[d.Name] = ch
			}
		case "Role":
			var r manifest.Role
			if r, err = manifest.Decode[manifest.Role](d); err == nil {
				c.Roles[d.Name] = r
			}
		case "TaskType":
			var tt manifest.TaskType
			if tt, err = manifest.Decode[manifest.TaskType](d); err == nil {
				c.TaskTypes[d.Name] = tt
			}
		case "ModelProfile":
			var p manifest.ModelProfile
			if p, err = manifest.Decode[manifest.ModelProfile](d); err == nil {
				c.Profiles[[2]string{d.Tenant, d.Name}] = p
			}
		case "ToolPlugin":
			spec, _ := d.Value["spec"].(map[string]any)
			var p toolspec.Plugin
			if p, err = toolspec.Decode(d.Name, spec); err == nil {
				c.Plugins[d.Name] = p
			}
			c.RawGlobals[d.Key()] = d.Value
		default:
			c.RawGlobals[d.Key()] = d.Value
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", d.Source, err)
		}
	}
	return c, nil
}

// catalogCache keeps the current catalog and reloads it when the applied set changes.
type catalogCache struct {
	mu   sync.RWMutex
	cur  *Catalog
	hash string
}

func (cc *catalogCache) get() *Catalog {
	cc.mu.RLock()
	defer cc.mu.RUnlock()
	return cc.cur
}

func (cc *catalogCache) set(c *Catalog) {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	cc.cur = c
	cc.hash = c.SetHash
}

func loadManifests(ctx context.Context, q store.Querier) ([]manifest.Doc, string, error) {
	var setHash string
	err := q.QueryRow(ctx, `SELECT set_hash FROM apply_status WHERE id = 1`).Scan(&setHash)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, "", err
	}
	rows, err := q.Query(ctx, `SELECT doc FROM manifests ORDER BY kind, tenant, name`)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	var docs []manifest.Doc
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, "", err
		}
		var v map[string]any
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, "", err
		}
		docs = append(docs, manifest.FromValue(v, "store"))
	}
	return docs, setHash, rows.Err()
}

// Catalog returns the current catalog, reloading it if another core copy applied a new set.
func (c *Core) Catalog(ctx context.Context) (*Catalog, error) {
	var setHash string
	err := c.st.Pool.QueryRow(ctx, `SELECT coalesce((SELECT set_hash FROM apply_status WHERE id = 1), '')`).Scan(&setHash)
	if err != nil {
		return nil, err
	}
	if cur := c.cat.get(); cur != nil && cur.SetHash == setHash {
		return cur, nil
	}
	docs, h, err := loadManifests(ctx, c.st.Pool)
	if err != nil {
		return nil, err
	}
	cat, err := buildCatalog(docs)
	if err != nil {
		return nil, err
	}
	cat.SetHash = h
	c.cat.set(cat)
	return cat, nil
}
