//go:build !windows

package main

import (
	"bytes"
	"errors"
	"os"
	"strings"
)

// osSysStore: Linux: the Secret Service through secret-tool (libsecret) when the user has a session bus and the
// service answers a lookup. macOS: none (the Go minigw serve there is for development; the router is the app, which
// has its own Keychain interface).
func osSysStore(cfgDir string) *sysStore {
	tool := secretTool()
	if tool == "" || os.Getenv("DBUS_SESSION_BUS_ADDRESS") == "" {
		return nil
	}
	s := &sysStore{label: "the Secret Service", legacy: &fileStore{cfgDir: cfgDir}}
	s.get = func(name string) (string, error) {
		var stderr bytes.Buffer
		cmd := command(tool, "lookup", "service", "minigw", "gateway", name)
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil && stderr.Len() > 0 { // "not found" is exit 1 without a message
			return "", errors.New("the Secret Service cannot be read")
		}
		return strings.TrimSpace(string(out)), nil
	}
	// ponytail: the probe is a read of a name nobody has; add a keyring unlock check if a locked keyring needs its own text.
	if _, err := s.get("probe"); err != nil {
		return nil
	}
	// the secret never in argv (secret-tool reads it from stdin)
	s.set = func(name, value string) error {
		cmd := command(tool, "store", "--label=minigw gateway "+name, "service", "minigw", "gateway", name)
		cmd.Stdin = strings.NewReader(value)
		if cmd.Run() != nil {
			return errors.New("the Secret Service refused the secret (it is not stored anywhere else)")
		}
		return nil
	}
	s.del = func(name string) error {
		command(tool, "clear", "service", "minigw", "gateway", name).Run()
		return nil
	}
	return s
}

// clientSecretsWhere: the place of the client's own secrets (profiles' tokens, the device key) for `minigw status`;
// not on the interface yet (step 4 moves them).
func clientSecretsWhere() string { return "file " + storePath() + " (mode 0600)" }
