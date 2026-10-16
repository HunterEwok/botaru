// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"

	"github.com/HunterEwok/botaru/impl/internal/core"
)

// kbIndexPut is P1 kb.index_put (KNW-4, KNW-5): the ingest job writes the fragments of the
// document of its leased kb.ingest.v1 task. The body was checked against its schema.
func (s *Server) kbIndexPut(ctx context.Context, wk *core.Worker, lease string, m map[string]any) (any, error) {
	req, err := decode[core.KBIndexPutRequest](m)
	if err != nil {
		return nil, err
	}
	return s.c.KBIndexPut(ctx, wk, lease, req)
}
