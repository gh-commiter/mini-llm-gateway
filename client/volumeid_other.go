//go:build !linux && !windows

package main

// volumeIDOf (B1): no stable volume id here (macOS f_fsid is the device number, which changes with the mount order):
// only the inode is compared. The Mac app uses the volume UUID instead.
func volumeIDOf(string) uint64 { return 0 }
