//go:build !windows

package main

// isRemoteVolume (W2): only Windows skips the owner check for network volumes.
func isRemoteVolume(string) bool { return false }
