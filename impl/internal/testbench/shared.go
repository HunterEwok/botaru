// SPDX-License-Identifier: AGPL-3.0-only

package testbench

import (
	"maps"
	"sync"

	"github.com/HunterEwok/botaru/impl/internal/spec"
	"github.com/HunterEwok/botaru/impl/internal/texts"
)

// shared is what a test process reads from one skeleton root once: the schemas, the default
// message catalog, the hello-bot manifests and the personas. Tests of a package build hundreds
// of benches from the same files; reading them once saves about half a second a bench under the
// race detector.
type shared struct {
	once     sync.Once
	sp       *spec.Spec
	tx       *texts.Catalog
	ms       []map[string]any
	personas map[string]string
	err      error
}

var (
	sharedMu    sync.Mutex
	sharedRoots = map[string]*shared{}
)

func load(root string) *shared {
	sharedMu.Lock()
	s, ok := sharedRoots[root]
	if !ok {
		s = &shared{}
		sharedRoots[root] = s
	}
	sharedMu.Unlock()
	s.once.Do(func() {
		if s.sp, s.err = spec.Load(root); s.err != nil {
			return
		}
		if s.tx, s.err = texts.Load(root, ""); s.err != nil {
			return
		}
		// HelloBotManifests drops the fields of the profiles the build does not claim; the cache
		// is for every test, so it is read with the claims of the build, not with the profile
		// override a test may have set when the process first gets here.
		if prev := spec.SetProfileOverride(); prev != nil {
			defer spec.SetProfileOverride(prev...)
		}
		if s.ms, s.err = HelloBotManifests(root); s.err != nil {
			return
		}
		s.personas, s.err = Personas(root)
	})
	return s
}

// SharedSpec is spec.Load of the root read once per test process. After loading a spec.Spec is
// only read (its caches of compiled schemas and registries are under its lock), so every bench
// of the process may hold the same object, also from parallel tests.
func SharedSpec(root string) (*spec.Spec, error) {
	s := load(root)
	return s.sp, s.err
}

// SharedTexts is texts.Load of the root with no extra directory, read once per test process;
// the catalog is only read after loading.
func SharedTexts(root string) (*texts.Catalog, error) {
	s := load(root)
	return s.tx, s.err
}

// SharedHelloBotManifests is HelloBotManifests read once per test process; every call returns a
// deep copy, which the caller may change.
func SharedHelloBotManifests(root string) ([]map[string]any, error) {
	s := load(root)
	if s.err != nil {
		return nil, s.err
	}
	out := make([]map[string]any, len(s.ms))
	for i, m := range s.ms {
		out[i] = DeepCopy(m).(map[string]any)
	}
	return out, nil
}

// SharedPersonas is Personas read once per test process; every call returns a copy.
func SharedPersonas(root string) (map[string]string, error) {
	s := load(root)
	if s.err != nil {
		return nil, s.err
	}
	return maps.Clone(s.personas), nil
}

// DeepCopy copies a decoded YAML or JSON value: maps and lists are copied at every level,
// scalars keep their type (an int stays an int, unlike a JSON round trip).
func DeepCopy(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = DeepCopy(e)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = DeepCopy(e)
		}
		return out
	case []map[string]any:
		out := make([]map[string]any, len(x))
		for i, e := range x {
			out[i] = DeepCopy(e).(map[string]any)
		}
		return out
	case []string:
		return append([]string(nil), x...)
	default:
		return v
	}
}
