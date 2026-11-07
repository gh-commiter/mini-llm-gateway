//go:build !linux && !windows

package main

// smbMount: macOS ima minigw v aplikaciji (Native.detect); tukaj le MINIGW_NATIVE_MOUNT.
func smbMount(cwd string) (source, mountPoint string, ok bool) { return "", "", false }
