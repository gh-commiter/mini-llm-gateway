package main

import "os"

// smbMount: priklop cifs/smb3 z najdaljšo potjo, pod katero je cwd (/proc/self/mountinfo).
func smbMount(cwd string) (source, mountPoint string, ok bool) {
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return "", "", false
	}
	return smbFromMountinfo(string(data), cwd)
}
