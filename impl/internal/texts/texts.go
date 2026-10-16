// SPDX-License-Identifier: AGPL-3.0-only

// Package texts holds the code text catalog: the ready texts of notices, refusals, labels and
// command answers that core and channel code show to people without a model (DLV-9). The texts
// come from the message catalog of the skeleton, spec/bindings/channels/messages/<locale>.yaml,
// read at run time like the other skeleton files; an installation adds catalogs of other
// locales in its own text folder.
package texts

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/HunterEwok/botaru/impl/internal/yamlx"
)

// Fallback is the locale whose text a key takes when the wanted locale has none (DLV-9).
const Fallback = "en"

// CatalogDir is the folder of the skeleton message catalog, relative to the skeleton root.
const CatalogDir = "spec/bindings/channels/messages"

// Catalog maps locale -> key -> text.
type Catalog struct {
	byLocale map[string]map[string]string
	// lists are the word lists of a catalog by locale and key (number.words, REP-4).
	lists map[string]map[string][]string
}

type file struct {
	Locale string         `yaml:"locale"`
	Texts  map[string]any `yaml:"texts"`
}

// Load reads the message catalogs of the skeleton under root and then every <locale>.yaml
// file in dir (dir may be empty). A file of the installation replaces texts key by key. The
// skeleton must have the en catalog.
func Load(root, dir string) (*Catalog, error) {
	c := &Catalog{byLocale: map[string]map[string]string{}, lists: map[string]map[string][]string{}}
	if err := c.addDir(filepath.Join(root, CatalogDir)); err != nil {
		return nil, err
	}
	if _, ok := c.byLocale[Fallback]; !ok {
		return nil, fmt.Errorf("texts: no %s catalog in %s", Fallback, filepath.Join(root, CatalogDir))
	}
	if dir != "" {
		if err := c.addDir(dir); err != nil {
			return nil, err
		}
	}
	return c, nil
}

func (c *Catalog) addDir(dir string) error {
	files, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return err
	}
	sort.Strings(files)
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		if err := c.add(b, f); err != nil {
			return err
		}
	}
	return nil
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
		// Lists (the number words of REP-4) are not texts; they are kept apart.
		switch x := v.(type) {
		case string:
			m[k] = strings.TrimSpace(x)
		case []any:
			var words []string
			for _, w := range x {
				if s, ok := w.(string); ok {
					words = append(words, strings.ToLower(strings.TrimSpace(s)))
				}
			}
			if c.lists[f.Locale] == nil {
				c.lists[f.Locale] = map[string][]string{}
			}
			c.lists[f.Locale][k] = words
		}
	}
	return nil
}

// Has reports whether the fallback locale has the key.
func (c *Catalog) Has(key string) bool { _, ok := c.byLocale[Fallback][key]; return ok }

// List returns the word list of key in locale, or the fallback list, in lower case.
func (c *Catalog) List(locale, key string) []string {
	if l, ok := c.lists[locale][key]; ok {
		return l
	}
	return c.lists[Fallback][key]
}

// Locales lists the loaded locales.
func (c *Catalog) Locales() []string {
	out := make([]string, 0, len(c.byLocale))
	for l := range c.byLocale {
		out = append(out, l)
	}
	sort.Strings(out)
	return out
}

// placeholder is a {name} parameter of a catalog text.
var placeholder = regexp.MustCompile(`\{[a-z][a-z0-9_]*\}`)

// Render returns the text of key in locale, or the fallback text of the key, with {name}
// parameters filled in. A parameter the caller does not pass becomes "-", so a person never
// sees a raw placeholder. An unknown key renders as the key itself, so a missing text is
// visible but harmless.
func (c *Catalog) Render(locale, key string, params map[string]any) string {
	t, ok := c.byLocale[locale][key]
	if !ok {
		t, ok = c.byLocale[Fallback][key]
	}
	if !ok {
		return key
	}
	return placeholder.ReplaceAllStringFunc(t, func(m string) string {
		if v, ok := params[m[1:len(m)-1]]; ok {
			return fmt.Sprint(v)
		}
		return "-"
	})
}
