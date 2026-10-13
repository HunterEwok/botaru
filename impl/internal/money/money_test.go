// SPDX-License-Identifier: AGPL-3.0-only

package money

import "testing"

func TestMoney(t *testing.T) {
	if FromEUR(0.05) != 50000 || FromEUR(300) != 300000000 {
		t.Fatal("FromEUR")
	}
	if TokensCost(180, 1.0) != 180 || TokensCost(20, 4.0) != 80 || TokensCost(3, 0.3) != 1 {
		t.Fatal("TokensCost")
	}
	if CentsUp(1) != 1 || CentsUp(10000) != 1 || CentsUp(10001) != 2 || CentsUp(0) != 0 {
		t.Fatal("CentsUp")
	}
}
