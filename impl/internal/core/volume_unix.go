// SPDX-License-Identifier: AGPL-3.0-only

//go:build unix

package core

import "syscall"

// volumeSpace measures the free space available to the store and the size of the volume of
// path (CAP-5).
func volumeSpace(path string) (free, total uint64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	bs := uint64(st.Bsize) //nolint:unconvert // Bsize differs in type between systems
	return uint64(st.Bavail) * bs, uint64(st.Blocks) * bs, nil
}
