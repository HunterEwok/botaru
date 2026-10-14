// SPDX-License-Identifier: AGPL-3.0-only

//go:build !unix

package core

import "errors"

func volumeSpace(string) (uint64, uint64, error) {
	return 0, 0, errors.New("measuring a volume is not supported on this system")
}
