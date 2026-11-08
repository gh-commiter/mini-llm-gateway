//go:build !windows

package main

import (
	"context"
	"os"
	"runtime"
	"strings"
	"time"
)

// computerName: macOS ime računalnika (Sharing), Linux »hostnamectl --pretty« ali /etc/hostname; "" = os.Hostname.
func computerName() string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	switch runtime.GOOS {
	case "darwin":
		if out, err := commandContext(ctx, "scutil", "--get", "ComputerName").Output(); err == nil {
			return strings.TrimSpace(string(out))
		}
	case "linux":
		if out, err := commandContext(ctx, "hostnamectl", "--pretty").Output(); err == nil && strings.TrimSpace(string(out)) != "" {
			return strings.TrimSpace(string(out))
		}
		if b, err := os.ReadFile("/etc/hostname"); err == nil {
			return strings.TrimSpace(string(b))
		}
	}
	return ""
}
