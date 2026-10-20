// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"strings"

	"github.com/HunterEwok/botaru/impl/internal/manifest"
)

// coreResultSchema is the result schema of a task type that declares none because the core
// checks the result against its own: the analysis envelope insight-result (INS-1), the
// review-result of a lens review or the necessity output (COM-13, COM-15). It goes into the
// claim payload like a declared result_schema, so that the runtime can ask the model for a
// matching answer; nil for any other type.
func (c *Core) coreResultSchema(tt manifest.TaskType, record map[string]any) map[string]any {
	switch tt.Purpose {
	case "insight", "aggregate":
		return c.inlineSchema("urn:botaru:schema:insight-result:1")
	case "review":
		if record["lens"] == necessityLens {
			return necessitySchema
		}
		return c.inlineSchema(reviewResultSchema(record))
	}
	return nil
}

// inlineSchema is a schema of spec/model with every $ref replaced by its target, for a model
// that reads the schema as text and resolves no references; nil when it cannot be read.
func (c *Core) inlineSchema(id string) map[string]any {
	raw, err := c.spec.Raw(id)
	if err != nil {
		return nil
	}
	out, _ := c.inline(raw, id, 0).(map[string]any)
	if out != nil {
		delete(out, "$schema")
		delete(out, "$id")
	}
	return out
}

func (c *Core) inline(node any, doc string, depth int) any {
	if depth > 32 {
		return map[string]any{}
	}
	switch x := node.(type) {
	case map[string]any:
		if ref, ok := x["$ref"].(string); ok {
			target, tdoc := c.refTarget(ref, doc)
			if target == nil {
				return map[string]any{}
			}
			out, _ := c.inline(target, tdoc, depth+1).(map[string]any)
			if out == nil {
				out = map[string]any{}
			}
			for k, v := range x {
				if k != "$ref" {
					out[k] = c.inline(v, doc, depth+1)
				}
			}
			delete(out, "$schema")
			delete(out, "$id")
			delete(out, "$defs")
			return out
		}
		out := make(map[string]any, len(x))
		for k, v := range x {
			if k == "$defs" {
				continue
			}
			out[k] = c.inline(v, doc, depth+1)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, v := range x {
			out[i] = c.inline(v, doc, depth+1)
		}
		return out
	}
	return node
}

// refTarget is the node a $ref names, as a copy, and the document it lives in.
func (c *Core) refTarget(ref, doc string) (map[string]any, string) {
	id, frag, _ := strings.Cut(ref, "#")
	if id == "" {
		id = doc
	}
	raw, err := c.spec.Raw(id)
	if err != nil {
		return nil, ""
	}
	var node any = raw
	for _, seg := range strings.Split(strings.TrimPrefix(frag, "/"), "/") {
		if seg == "" {
			continue
		}
		m, _ := node.(map[string]any)
		node = m[strings.ReplaceAll(strings.ReplaceAll(seg, "~1", "/"), "~0", "~")]
	}
	m, _ := node.(map[string]any)
	if m == nil {
		return nil, ""
	}
	cp := make(map[string]any, len(m))
	for k, v := range m {
		cp[k] = v
	}
	return cp, id
}
