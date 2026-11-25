//go:build !linux && !darwin

package main

// Free space is unknown here, so backups are not limited by it.
func diskSpace(string) (free, total uint64, ok bool) { return 0, 0, false }
