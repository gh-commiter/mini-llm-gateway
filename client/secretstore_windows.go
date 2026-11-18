//go:build windows

package main

import "errors"

// osSysStore: the Credential Manager of the user the process runs as; the target is minigw/gateway/<name>. It
// is used when a read of a name nobody has answers "not found".
func osSysStore(string) *sysStore {
	target := func(name string) string { return "minigw/gateway/" + name }
	s := &sysStore{label: storeName}
	s.get = func(name string) (string, error) {
		b, err := credLoad(target(name))
		if errors.Is(err, errNotFound) {
			return "", nil
		}
		return string(b), err
	}
	if _, err := s.get("probe"); err != nil {
		return nil
	}
	s.set = func(name, value string) error { return credSave(target(name), []byte(value)) }
	s.del = func(name string) error { credDelete(target(name)); return nil }
	return s
}

// clientSecretsWhere: see secretstore_other.go.
func clientSecretsWhere() string { return storeName }
