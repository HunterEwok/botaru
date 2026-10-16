// SPDX-License-Identifier: AGPL-3.0-only

// Package ids makes core object ids: a kind prefix, an underscore and a ULID
// (urn:botaru:schema:common:1#/$defs/object_id), and random secrets for tokens, handles and
// nonces.
package ids

import (
	"crypto/rand"
	"encoding/base64"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"
)

// Prefixes of core objects.
const (
	Task      = "t"
	Worker    = "wk"
	Delivery  = "d"
	Principal = "p"
	Event     = "ev"
	Spend     = "sp"
	Bot       = "bot"
	Call      = "c"
	Approval  = "a"
	Hold      = "h"
	Erasure   = "er"
	Grant     = "sg"
	Session   = "s"
	Memory    = "m"
	// Message shares the prefix m with memory items: the message schema fixes m_<ulid>.
	Message    = "m"
	Subject    = "sr"
	Step       = "es"
	Proposal   = "prp"
	Fact       = "fct"
	FactOffer  = "fo"
	Incident   = "inc"
	Control    = "ctl"
	Finding    = "fd"
	Card       = "cc"
	Read       = "rd"
	Campaign   = "cmp"
	Script     = "scr"
	ScriptGate = "scg"
)

var (
	mu      sync.Mutex
	entropy = ulid.Monotonic(rand.Reader, 0)
)

// New returns <prefix>_<ULID> for the current time.
func New(prefix string) string {
	mu.Lock()
	defer mu.Unlock()
	return prefix + "_" + ulid.MustNew(ulid.Timestamp(time.Now()), entropy).String()
}

// Untimed returns a new id of the form prefix_<ULID> whose time part is zero and whose random
// part is drawn afresh, so it says nothing about when or in which order it was made (SEC-14).
func Untimed(prefix string) string {
	return prefix + "_" + ulid.MustNew(0, rand.Reader).String()
}

// PrincipalID returns a new opaque person id human:p_<ULID>.
func PrincipalID() string { return "human:" + New(Principal) }

// Secret returns n random bytes as base64url without padding.
func Secret(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand does not fail on supported platforms
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
