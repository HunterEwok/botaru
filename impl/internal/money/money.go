// SPDX-License-Identifier: AGPL-3.0-only

// Package money converts between euros in manifests and the integer units of the records:
// micro-euros eur_micros for usage and reserves, euro cents for limits and reports (BUD-9,
// D-87 (1)). There is no floating-point money in records.
package money

import "math"

// Micros is an amount in micro-euros; 1 EUR = 1 000 000.
type Micros = int64

// FromEUR converts a euro amount of a manifest to micro-euros, rounding to the nearest unit.
func FromEUR(eur float64) Micros { return int64(math.Round(eur * 1e6)) }

// TokensCost is tokens multiplied by a price in euros per million tokens, that is micro-euros
// per token; a fraction of a micro-euro is rounded up (anthropic binding, "Usage").
func TokensCost(tokens int64, eurPerMTok float64) Micros {
	if tokens <= 0 || eurPerMTok <= 0 {
		return 0
	}
	return int64(math.Ceil(float64(tokens) * eurPerMTok))
}

// CentsUp rounds a window sum up to a whole euro cent (D-87 (1)).
func CentsUp(m Micros) int64 { return (m + 9999) / 10000 }
