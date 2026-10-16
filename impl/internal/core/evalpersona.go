// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"slices"

	"github.com/HunterEwok/botaru/impl/internal/jcs"
)

// positionPersona is the persona part of the static fingerprint of a role at a position of a
// tenant charter (PRV-3, spec/hashing.md). The seat role of the commission (commission.seat_role)
// of the reviewer profile is the seat holder: its attempts run at the commission position with
// the persona of the lens of the seat appended, so its part covers the commission persona and
// the lens personas of the charter's seats. Without a pinned lens persona the part is the
// position persona alone, as for any other role.
func (x evalIndex) positionPersona(tenant, role, pos string) personaRef {
	gov := x.governance(tenant)
	com := mmap(gov, "commission")
	// Only a seat role of the reviewer profile is the seat holder here; any other role keeps
	// its position persona, so its fingerprint does not move.
	if role == "" || mstr(com, "seat_role") != role || mstr(x.spec("Role", "", role), "profile") != "reviewer" {
		return personaOf(mmap(mmap(mmap(gov, "positions"), pos), "persona"))
	}
	ref := personaOf(mmap(mmap(mmap(gov, "positions"), "commission"), "persona"))
	lenses := map[string]any{}
	for _, s := range asList(com["seats"]) {
		sm, _ := s.(map[string]any)
		lens := mstr(sm, "lens")
		if p := personaOf(mmap(mmap(mmap(com, "lenses"), lens), "persona")); lens != "" && p.SHA256 != "" {
			lenses[lens] = p.SHA256
		}
	}
	if len(lenses) == 0 {
		return ref
	}
	names := slices.Sorted(mapKeys(lenses))
	list := make([]any, 0, len(names))
	for _, n := range names {
		list = append(list, map[string]any{"lens": n, "sha256": lenses[n]})
	}
	h, err := jcs.Hash(map[string]any{"persona": nullable(ref.SHA256), "lenses": list})
	if err != nil {
		return ref
	}
	return personaRef{Ref: ref.Ref, SHA256: h}
}
