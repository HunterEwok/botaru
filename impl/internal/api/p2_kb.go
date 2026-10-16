// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"encoding/base64"

	"github.com/HunterEwok/botaru/impl/internal/core"
)

// kbUpload is P2 kb.upload (KNW-4, ART-4). The skeleton has no schema for it yet, so the body
// is checked here: tenant, corpus, filename and content (the document bytes in standard
// base64), and nothing else.
func (s *Server) kbUpload(ctx context.Context, oper core.Operator, m map[string]any) (any, error) {
	bad := &core.Problem{Reason: "input_schema_invalid", Detail: "kb.upload takes tenant, corpus, filename and base64 content"}
	for k := range m {
		switch k {
		case "op", "tenant", "corpus", "filename", "content":
		default:
			return nil, bad
		}
	}
	str := func(k string, max int) (string, bool) {
		v, ok := m[k].(string)
		return v, ok && v != "" && len(v) <= max
	}
	tenant, ok1 := str("tenant", 64)
	corpus, ok2 := str("corpus", 64)
	name, ok3 := str("filename", 255)
	content, ok4 := m["content"].(string)
	if !ok1 || !ok2 || !ok3 || !ok4 {
		return nil, bad
	}
	data, err := base64.StdEncoding.DecodeString(content)
	if err != nil {
		return nil, bad
	}
	return s.c.KBUpload(ctx, oper, core.KBUploadRequest{Tenant: tenant, Corpus: corpus, Filename: name, Content: data})
}
