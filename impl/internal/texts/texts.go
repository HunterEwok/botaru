// SPDX-License-Identifier: AGPL-3.0-only

// Package texts holds the code text catalog: the ready texts of notices, refusals and labels
// that core and channel code show to people without a model (DLV-9).
package texts

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/HunterEwok/botaru/impl/internal/yamlx"
)

//go:embed en.yaml
var builtinEN []byte

// Fallback is the locale used when the wanted one has no text.
const Fallback = "en"

// Catalog maps locale -> key -> text.
type Catalog struct {
	byLocale map[string]map[string]string
}

type file struct {
	Locale string            `yaml:"locale"`
	Texts  map[string]string `yaml:"texts"`
}

// Load returns the built-in en catalog plus every <locale>.yaml file in dir (dir may be empty).
// A file of the installation replaces the built-in texts key by key.
func Load(dir string) (*Catalog, error) {
	c := &Catalog{byLocale: map[string]map[string]string{}}
	if err := c.add(builtinEN, "builtin en.yaml"); err != nil {
		return nil, err
	}
	if dir == "" {
		return c, nil
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		if err := c.add(b, f); err != nil {
			return nil, err
		}
	}
	return c, nil
}

func (c *Catalog) add(b []byte, src string) error {
	var f file
	if err := yamlx.Unmarshal(b, &f); err != nil {
		return fmt.Errorf("texts: %s: %w", src, err)
	}
	if f.Locale == "" {
		return fmt.Errorf("texts: %s: no locale", src)
	}
	m := c.byLocale[f.Locale]
	if m == nil {
		m = map[string]string{}
		c.byLocale[f.Locale] = m
	}
	for k, v := range f.Texts {
		m[k] = strings.TrimSpace(v)
	}
	return nil
}

// Has reports whether the fallback locale has the key.
func (c *Catalog) Has(key string) bool { _, ok := c.byLocale[Fallback][key]; return ok }

// Locales lists the loaded locales.
func (c *Catalog) Locales() []string {
	out := make([]string, 0, len(c.byLocale))
	for l := range c.byLocale {
		out = append(out, l)
	}
	sort.Strings(out)
	return out
}

// Render returns the text of key in locale (or the fallback) with {name} parameters filled in.
// An unknown key renders as the key itself, so a missing text is visible but harmless.
func (c *Catalog) Render(locale, key string, params map[string]any) string {
	t, ok := c.byLocale[locale][key]
	if !ok {
		t, ok = c.byLocale[Fallback][key]
	}
	if !ok {
		return key
	}
	for k, v := range params {
		t = strings.ReplaceAll(t, "{"+k+"}", fmt.Sprint(v))
	}
	return t
}
