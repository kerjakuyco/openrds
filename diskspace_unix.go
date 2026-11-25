//go:build linux || darwin

package main

import "syscall"

// Bytes available to OpenRDS and the size of the filesystem that holds path.
func diskSpace(path string) (free, total uint64, ok bool) {
	var st syscall.Statfs_t
	if syscall.Statfs(path, &st) != nil {
		return 0, 0, false
	}
	return uint64(st.Bavail) * uint64(st.Bsize), uint64(st.Blocks) * uint64(st.Bsize), true
}
