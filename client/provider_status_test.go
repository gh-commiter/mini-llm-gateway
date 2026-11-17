package main

import (
	"context"
	"log"
	"strings"
	"testing"
	"time"
)

func TestGatewayReadyText(t *testing.T) {
	for _, c := range []struct {
		ready               bool
		state, reason, want string
	}{
		{true, "ready", "", ", ready"},
		{false, "loading", "", ", models loading (not loaded yet)"},
		{false, "failed", "cannot connect: dial tcp 10.0.0.9:4000: connect: connection refused", ", models failed to load: cannot connect: dial tcp 10.0.0.9:4000: connect: connection refused (see the gateway log)"},
		{false, "failed", "", ", models failed to load (see the gateway log)"},
		{false, "", "", ", models not loaded (see the gateway log)"},
	} {
		if got := gatewayReadyText(c.ready, c.state, c.reason); got != c.want {
			t.Errorf("%v %q: %q", c.ready, c.state, got)
		}
	}
}

// Right after the start nothing was asked for yet, then a load runs, then it ends: loading, loading, ready or failed.
func TestGatewayCatalogState(t *testing.T) {
	var c gwCatalog
	if st, _ := c.state("p"); st != "loading" {
		t.Fatal("an entry that was never asked for is not a failure:", st)
	}
	c.m = map[string]*gwEntry{"p": {loading: make(chan struct{})}}
	if st, _ := c.state("p"); st != "loading" {
		t.Fatal(st)
	}
	c.m["p"] = &gwEntry{failed: time.Now(), reason: "HTTP 401 from /v1/models: check the API key"}
	if st, why := c.state("p"); st != "failed" || !strings.Contains(why, "401") {
		t.Fatal(st, why)
	}
	c.m["p"] = &gwEntry{ok: true}
	if st, _ := c.state("p"); st != "ready" {
		t.Fatal(st)
	}
}

// The reason shown and logged never carries the address with its credentials.
func TestGatewayFetchErrorHasNoURL(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := gwFetch(ctx, serveGateway{BaseURL: "http://user:secret-pass@127.0.0.1:1/api", Insecure: true}, "sk-key", "/v1/models")
	if err == nil {
		t.Fatal("expected a connection error")
	}
	for _, bad := range []string{"secret-pass", "user:", "/api", "sk-key", "http://"} {
		if strings.Contains(err.Error(), bad) {
			t.Fatalf("%q in %q", bad, err)
		}
	}
	if !strings.HasPrefix(err.Error(), "cannot connect: ") {
		t.Fatal(err)
	}
}

// A provider whose list cannot be loaded is logged once, with its name and the reason (no address, no key), and the status says
// "failed" with that reason; before the load ends it said "loading".
func TestFailedModelLoadIsLoggedAndShown(t *testing.T) {
	keys := fakeKeys(t)
	keys["gw"] = gwKey
	var logs strings.Builder
	s := &server{log: log.New(&logs, "", 0), cfgDir: t.TempDir()}
	g := serveGateway{Name: "gw", BaseURL: "http://user:secret-pass@127.0.0.1:1/api", Insecure: true}
	s.gatewayModels(g, true) // waits for the load, which fails (nothing listens)
	st, why := s.gateways.state("gw")
	if st != "failed" || why == "" || strings.Contains(why, "secret-pass") {
		t.Fatal(st, why)
	}
	line := logs.String()
	if strings.Count(line, "\n") != 1 || !strings.Contains(line, "provider gw: models not loaded: cannot connect") || strings.Contains(line, "secret-pass") || strings.Contains(line, gwKey) {
		t.Fatalf("%q", line)
	}
}

// A provider switched off in the settings says "disabled" in the status (never "loading", never "see the log") and is marked in the
// list; one that is on keeps its load state. The Mac commands print the same words (RouterCommand.swift).
func TestDisabledProviderIsShownAsDisabled(t *testing.T) {
	off := apiGateway{Name: "x", Enabled: false, State: "loading"}
	if got := providerStatusEnd(off, true); got != ", disabled" {
		t.Fatal(got)
	}
	if got := providerStatusEnd(off, false); got != ", disabled" {
		t.Fatal(got)
	}
	on := apiGateway{Name: "x", Enabled: true, State: "loading"}
	if got := providerStatusEnd(on, true); got != ", models loading (not loaded yet)" {
		t.Fatal(got)
	}
	if got := providerStatusEnd(on, false); got != "" {
		t.Fatal(got)
	}
	if providerListMark(off) != "  (disabled)" || providerListMark(on) != "" {
		t.Fatal(providerListMark(off), providerListMark(on))
	}
}
