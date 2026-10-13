// SPDX-License-Identifier: AGPL-3.0-only

// Package spec reads the skeleton specification that the core implements: the JSON schemas of
// spec/model and spec/manifests and the closed registries. The implementation does not copy
// these files (D-92): the core reads them from the skeleton tree that is deployed next to the
// binary, and the tests read them from the repository.
package spec

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dlclark/regexp2"
	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/HunterEwok/botaru/impl/internal/ijson"
	"github.com/HunterEwok/botaru/impl/internal/yamlx"
)

// Version is the skeleton version this implementation claims (MAN-8).
const Version = "v0.1"

// EnvRoot names the environment variable with the path of the skeleton root.
const EnvRoot = "BOTARU_SPEC_ROOT"

// FindRoot returns the skeleton root: the explicit path if it is not empty, else $BOTARU_SPEC_ROOT,
// else the nearest parent of the working directory or of the executable that holds
// spec/VERSION.
func FindRoot(explicit string) (string, error) {
	if explicit != "" {
		return checkRoot(explicit)
	}
	if env := os.Getenv(EnvRoot); env != "" {
		return checkRoot(env)
	}
	var starts []string
	if wd, err := os.Getwd(); err == nil {
		starts = append(starts, wd)
	}
	if exe, err := os.Executable(); err == nil {
		starts = append(starts, filepath.Dir(exe))
	}
	for _, s := range starts {
		for dir := s; ; dir = filepath.Dir(dir) {
			if _, err := os.Stat(filepath.Join(dir, "spec", "VERSION")); err == nil {
				return dir, nil
			}
			if filepath.Dir(dir) == dir {
				break
			}
		}
	}
	return "", errors.New("spec: skeleton root with spec/VERSION not found; set " + EnvRoot)
}

func checkRoot(dir string) (string, error) {
	if _, err := os.Stat(filepath.Join(dir, "spec", "VERSION")); err != nil {
		return "", fmt.Errorf("spec: %s is not a skeleton root: %w", dir, err)
	}
	return filepath.Abs(dir)
}

// Spec holds the compiled schemas and the registries of one skeleton tree.
type Spec struct {
	Root string

	mu       sync.Mutex
	compiler *jsonschema.Compiler
	schemas  map[string]*jsonschema.Schema
	ids      map[string]string // short name (p1-claim-request) -> $id
	reg      map[string]*Registry
}

type ecmaRegexp struct{ re *regexp2.Regexp }

func (r ecmaRegexp) MatchString(s string) bool {
	ok, err := r.re.MatchString(s)
	return err == nil && ok
}
func (r ecmaRegexp) String() string { return r.re.String() }

// Load reads every schema under spec/model and spec/manifests of the root.
func Load(root string) (*Spec, error) {
	s := &Spec{Root: root, schemas: map[string]*jsonschema.Schema{}, ids: map[string]string{}, reg: map[string]*Registry{}}
	c := jsonschema.NewCompiler()
	c.UseRegexpEngine(func(expr string) (jsonschema.Regexp, error) {
		re, err := regexp2.Compile(expr, regexp2.ECMAScript)
		if err != nil {
			return nil, err
		}
		// A bound against a runaway match, not a tight one: a zero timeout makes the deadline
		// pass at once, and a match then fails at random depending on the clock.
		re.MatchTimeout = time.Second
		return ecmaRegexp{re}, nil
	})
	dirs := []string{"spec/model", "spec/model/event-data", "spec/manifests", "spec/manifests/drivers"}
	for _, d := range dirs {
		files, err := filepath.Glob(filepath.Join(root, d, "*.schema.json"))
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			b, err := os.ReadFile(f)
			if err != nil {
				return nil, err
			}
			doc, err := jsonschema.UnmarshalJSON(strings.NewReader(string(b)))
			if err != nil {
				return nil, fmt.Errorf("spec: %s: %w", f, err)
			}
			m, ok := doc.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("spec: %s is not an object", f)
			}
			id, _ := m["$id"].(string)
			if id == "" {
				return nil, fmt.Errorf("spec: %s has no $id", f)
			}
			if err := c.AddResource(id, doc); err != nil {
				return nil, fmt.Errorf("spec: %s: %w", f, err)
			}
			short := strings.TrimSuffix(filepath.Base(f), ".schema.json")
			if d == "spec/manifests" || d == "spec/manifests/drivers" {
				short = "manifest:" + short
			}
			s.ids[short] = id
		}
	}
	s.compiler = c
	return s, nil
}

// ID returns the $id of a schema by its short name: the file name without .schema.json for
// spec/model (p1-claim-request) and "manifest:<name>" for spec/manifests.
func (s *Spec) ID(short string) (string, bool) {
	id, ok := s.ids[short]
	return id, ok
}

// Schema compiles (once) and returns a schema by short name or by $id.
func (s *Spec) Schema(name string) (*jsonschema.Schema, error) {
	id := name
	if v, ok := s.ids[name]; ok {
		id = v
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if sch, ok := s.schemas[id]; ok {
		return sch, nil
	}
	sch, err := s.compiler.Compile(id)
	if err != nil {
		return nil, fmt.Errorf("spec: compile %s: %w", name, err)
	}
	s.schemas[id] = sch
	return sch, nil
}

// CompileAll compiles every schema; a startup check that the tree is consistent.
func (s *Spec) CompileAll() error {
	names := make([]string, 0, len(s.ids))
	for n := range s.ids {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if _, err := s.Schema(n); err != nil {
			return err
		}
	}
	return nil
}

// Violation is one schema error: the JSON pointer of the value and a message.
type Violation struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

// ValidationError lists the violations of one validation.
type ValidationError struct {
	Schema     string
	Violations []Violation
}

func (e *ValidationError) Error() string {
	parts := make([]string, 0, len(e.Violations))
	for _, v := range e.Violations {
		parts = append(parts, v.Path+": "+v.Message)
	}
	return "schema " + e.Schema + ": " + strings.Join(parts, "; ")
}

// Validate checks v (shapes of package ijson) against a schema by short name or $id.
func (s *Spec) Validate(name string, v any) error {
	sch, err := s.Schema(name)
	if err != nil {
		return err
	}
	n, err := ijson.Normalize(v)
	if err != nil {
		return err
	}
	if err := sch.Validate(n); err != nil {
		var ve *jsonschema.ValidationError
		if errors.As(err, &ve) {
			out := &ValidationError{Schema: name}
			collect(ve, &out.Violations)
			if len(out.Violations) == 0 {
				out.Violations = append(out.Violations, Violation{Path: "", Message: ve.Error()})
			}
			return out
		}
		return err
	}
	return nil
}

func collect(ve *jsonschema.ValidationError, out *[]Violation) {
	if len(ve.Causes) == 0 {
		ptr := ""
		for _, t := range ve.InstanceLocation {
			ptr += "/" + strings.ReplaceAll(strings.ReplaceAll(t, "~", "~0"), "/", "~1")
		}
		msg := fmt.Sprint(ve.ErrorKind)
		*out = append(*out, Violation{Path: ptr, Message: msg})
		return
	}
	for _, c := range ve.Causes {
		collect(c, out)
	}
}

// Registry is a closed registry file: registry, version, entries with an id each.
type Registry struct {
	Name    string
	Version int
	Entries []map[string]any
	byID    map[string]map[string]any
}

// Entry returns the entry with this id.
func (r *Registry) Entry(id string) (map[string]any, bool) {
	e, ok := r.byID[id]
	return e, ok
}

// Has reports whether the registry has an entry with this id.
func (r *Registry) Has(id string) bool { _, ok := r.byID[id]; return ok }

// Registry loads (once) a registry file relative to the root, for example
// "spec/model/reasons.yaml".
func (s *Spec) Registry(rel string) (*Registry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.reg[rel]; ok {
		return r, nil
	}
	v, err := ReadYAMLFile(filepath.Join(s.Root, rel))
	if err != nil {
		return nil, err
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("spec: %s is not a mapping", rel)
	}
	r := &Registry{byID: map[string]map[string]any{}}
	r.Name, _ = m["registry"].(string)
	if n, ok := m["version"].(int); ok {
		r.Version = n
	}
	list, _ := m["entries"].([]any)
	for _, e := range list {
		em, ok := e.(map[string]any)
		if !ok {
			continue
		}
		r.Entries = append(r.Entries, em)
		if id, ok := em["id"].(string); ok {
			r.byID[id] = em
		}
	}
	s.reg[rel] = r
	return r, nil
}

// ReadYAMLFile reads one YAML document.
func ReadYAMLFile(path string) (any, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var v any
	if err := yamlx.Unmarshal(b, &v); err != nil {
		return nil, fmt.Errorf("spec: %s: %w", path, err)
	}
	return v, nil
}
