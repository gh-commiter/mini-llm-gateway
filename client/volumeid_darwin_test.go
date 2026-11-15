package main

import "testing"

func TestVolumeIDUsesLocalUUID(t *testing.T) {
	dir := t.TempDir()
	first := volumeIDOf(dir)
	if first == 0 {
		t.Fatal("local volume UUID was not available")
	}
	if got := volumeIDOf(dir); got != first {
		t.Fatalf("volume identity changed: %x != %x", got, first)
	}
}
