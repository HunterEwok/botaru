// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"errors"
	"testing"

	"github.com/HunterEwok/botaru/impl/internal/core"
)

// The bodies of the P2 accountability operations take no field their request lacks (GOV-13,
// MGR-8): attribution.list and attribution.create have no schema in the skeleton, and the schema
// of incident.cause allows no other property.
func TestDecodeStrictP2Accountability(t *testing.T) {
	if req, err := decodeStrict[core.AttributionRequest](map[string]any{"op": "attribution.list", "state": "candidate"}); err != nil || req.State != "candidate" {
		t.Fatalf("a known body %+v %v", req, err)
	}
	var p *core.Problem
	if _, err := decodeStrict[core.AttributionRequest](map[string]any{"incident": "inc_1", "stat": "candidate"}); !errors.As(err, &p) || p.Reason != "input_schema_invalid" {
		t.Fatalf("an unknown field of attribution.list: %v", err)
	}
	if _, err := decodeStrict[core.IncidentCauseRequest](map[string]any{"incident": "inc_1", "cause": "attack", "note": "x"}); !errors.As(err, &p) || p.Reason != "input_schema_invalid" {
		t.Fatalf("an unknown field of incident.cause: %v", err)
	}
}
