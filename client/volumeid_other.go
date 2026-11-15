//go:build !linux && !windows && !darwin

package main

// volumeIDOf (B1): no stable volume id or mount-source fallback on these platforms; only the inode is compared.
func volumeIDOf(string) uint64 { return 0 }
