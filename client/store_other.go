//go:build !windows

package main

// Povezava v datoteki uporabnika z dovoljenji 0600 (Linux, in macOS pri preizkusu; macOS minigw v aplikaciji
// uporablja Keychain). ponytail: na Linuxu brez Secret Service; dodaj, če bo potreben namizni keyring.

import (
	"errors"
	"os"
	"path/filepath"
)

var errNotFound = errors.New("not found")

func storePath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "minigw", "connection.json")
}

func storeLoad() ([]byte, error) {
	data, err := os.ReadFile(storePath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, errNotFound
	}
	return data, err
}

func storeSave(data []byte) error {
	if err := os.MkdirAll(filepath.Dir(storePath()), 0o700); err != nil {
		return err
	}
	return os.WriteFile(storePath(), data, 0o600)
}

func storeDelete() { os.Remove(storePath()) }

const storeName = "a private file"
