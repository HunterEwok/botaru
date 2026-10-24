// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/dlclark/regexp2"
	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/HunterEwok/botaru/impl/internal/ijson"
	"github.com/HunterEwok/botaru/impl/internal/jcs"
)

// resultSchemas compiles the schemas that manifests embed (params_schema, result_schema) once
// per content hash.
type resultSchemas struct {
	mu sync.Mutex
	m  map[string]*jsonschema.Schema
}

func newResultSchemas() *resultSchemas { return &resultSchemas{m: map[string]*jsonschema.Schema{}} }

type re2 struct{ re *regexp2.Regexp }

func (r re2) MatchString(s string) bool { ok, err := r.re.MatchString(s); return err == nil && ok }
func (r re2) String() string            { return r.re.String() }

func (rs *resultSchemas) compile(schema map[string]any) (*jsonschema.Schema, error) {
	h, err := jcs.Hash(schema)
	if err != nil {
		return nil, err
	}
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if s, ok := rs.m[h]; ok {
		return s, nil
	}
	n, err := ijson.Normalize(schema)
	if err != nil {
		return nil, err
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	c.UseRegexpEngine(func(expr string) (jsonschema.Regexp, error) {
		re, err := regexp2.Compile(expr, regexp2.ECMAScript)
		if err != nil {
			return nil, err
		}
		// A bound against a runaway match of a pattern a manifest or a plugin wrote, as in
		// spec.Load; a match past it fails.
		re.MatchTimeout = time.Second
		return re2{re}, nil
	})
	url := "urn:botaru:embedded:" + h
	if err := c.AddResource(url, n); err != nil {
		return nil, err
	}
	s, err := c.Compile(url)
	if err != nil {
		return nil, err
	}
	rs.m[h] = s
	return s, nil
}

// validate checks v against an embedded schema.
func (rs *resultSchemas) validate(schema map[string]any, v any) error {
	s, err := rs.compile(schema)
	if err != nil {
		return fmt.Errorf("embedded schema: %w", err)
	}
	n, err := ijson.Normalize(v)
	if err != nil {
		return err
	}
	if err := s.Validate(n); err != nil {
		var ve *jsonschema.ValidationError
		if errors.As(err, &ve) {
			return errors.New("value does not match the schema")
		}
		return err
	}
	return nil
}
