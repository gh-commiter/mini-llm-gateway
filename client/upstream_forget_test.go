package main

import (
	"errors"
	"strings"
	"testing"
)

// A secret store that fails to delete leaves the token and the device key in place: Forget must say so, naming the item and never a value.
func TestUpstreamForgetReportsAFailingStore(t *testing.T) {
	keys := fakeKeys(t)
	keys["upstream:hub"] = "SECRET-VALUE"
	old := keyDel
	keyDel = func(_, _ string) error { return errors.New("store locked") }
	t.Cleanup(func() { keyDel = old })
	err := upstreamHost{&server{}}.Forget("hub")
	if err == nil {
		t.Fatal("Forget said the credentials are gone while the store still holds them")
	}
	if m := err.Error(); !strings.Contains(m, "upstream:hub") || !strings.Contains(m, "store locked") || strings.Contains(m, "SECRET-VALUE") {
		t.Fatalf("the error must name the item and the cause, never a value: %q", m)
	}
}

func TestUpstreamForgetSucceedsWhenTheStoreDeletes(t *testing.T) {
	keys := fakeKeys(t)
	keys["upstream:hub"] = "v"
	if err := (upstreamHost{&server{}}).Forget("hub"); err != nil {
		t.Fatal(err)
	}
}
