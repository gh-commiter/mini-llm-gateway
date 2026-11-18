package coreapi

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"minigw/core"
)

type noHost struct{}

func TestBusyUpstreamRemovalKeepsCredentials(t *testing.T) {
	f := newFixture(t)
	inside, release := make(chan struct{}), make(chan struct{})
	h := &methodHost{secretHost: secretHost{secrets: map[string]string{}}}
	first := true
	h.answer = func(method, path string) (int, string) {
		if path == "/minigw/v1/projects/shop" {
			return 200, `{"id":7,"slug":"shop"}`
		}
		if method == "POST" {
			if first {
				first = false
				close(inside)
				<-release
			}
			return 201, `{"id":55}`
		}
		return 404, `{}`
	}
	api := New(Options{Store: func() *core.Store { return f.st }, Upstream: h})
	call := func(method, path, body string) Response {
		return api.Handle(bg, Context{Kind: KindAdmin}, Request{Method: method, Path: path, Body: []byte(body)})
	}
	if r := call("PUT", "/upstreams/hub", `{"address":"127.0.0.1:8766","token":"tok#key"}`); r.Status != 200 {
		t.Fatal(r.Status)
	}
	fk := f.pid["fakture"]
	if r := call("PUT", fmt.Sprintf("/upstreams/hub/links/%d", fk), `{"remote":"shop"}`); r.Status != 200 {
		t.Fatal(r.Status)
	}
	done := make(chan Response, 1)
	go func() { done <- call("POST", "/upstream-push", fmt.Sprintf(`{"project":%d}`, fk)) }()
	<-inside
	r := call("DELETE", "/upstreams/hub", "")
	close(release)
	pushed := <-done
	if r.Status != 400 || !strings.Contains(string(r.Body), "is running") || pushed.Status != 200 {
		t.Fatalf("remove: %d %s; push: %d %s", r.Status, r.Body, pushed.Status, pushed.Body)
	}
	if len(h.forgets) != 0 || h.secrets["hub"] == "" {
		t.Fatal("busy removal deleted credentials")
	}
	if r := call("DELETE", "/upstreams/hub", ""); r.Status != 200 {
		t.Fatal(r.Status, string(r.Body))
	}
}

func (noHost) Do(context.Context, string, string, string, map[string]string, []byte) (int, []byte, time.Duration, error) {
	return 0, nil, 0, nil
}
func (noHost) Connect(string, string, string, string) (UpstreamInfo, error) {
	return UpstreamInfo{}, nil
}
func (noHost) Info(string) UpstreamInfo { return UpstreamInfo{} }
func (noHost) Forget(string) error      { return nil }
func (noHost) Available() bool          { return true }

// Only the owner reaches the upstream routes: no member route exists for them, and a host without the feature answers 501.
func TestUpstreamRoutesAreTheOwnersOnly(t *testing.T) {
	f := newFixture(t)
	admin := Context{Kind: KindAdmin}
	for _, c := range []struct{ method, path string }{{"GET", "/upstreams"}, {"PUT", "/upstreams/hub"}, {"DELETE", "/upstreams/hub"}, {"PUT", "/upstreams/hub/links/1"}, {"POST", "/upstream-push"}} {
		if r := f.api.Handle(bg, admin, Request{Method: c.method, Path: c.path, Body: []byte(`{}`)}); r.Status != 501 || !strings.Contains(string(r.Body), "cannot send to an upstream") {
			t.Errorf("%s %s without a host: %d %s", c.method, c.path, r.Status, r.Body)
		}
		for _, mp := range []string{"/minigw/v1" + c.path, "/minigw/v1/projects/1" + c.path} {
			if r := f.api.Handle(bg, member("dev-fani"), Request{Method: c.method, Path: mp, Body: []byte(`{}`)}); r.Status != 404 {
				t.Errorf("member %s %s: %d %s", c.method, mp, r.Status, r.Body)
			}
		}
	}
	// resolving a conflict is the owner's alone too (it needs no host: it only settles what the database says)
	for _, mp := range []string{"/minigw/v1/upstream-resolve", "/minigw/v1/projects/1/upstream-resolve"} {
		if r := f.api.Handle(bg, member("dev-fani"), Request{Method: "POST", Path: mp, Body: []byte(`{"project":1,"note":1,"ours":true}`)}); r.Status != 404 {
			t.Errorf("member %s: %d %s", mp, r.Status, r.Body)
		}
	}
	// with a host: the owner adds, links and removes; a link names a project that exists
	api := New(Options{Store: func() *core.Store { return f.st }, Upstream: noHost{}})
	for _, c := range []struct {
		method, path, body string
		want               int
	}{
		{"PUT", "/upstreams/hub", `{"address":"hub.example","token":"t#k"}`, 200},
		{"PUT", "/upstreams/Bad_Name", `{"address":"hub.example","token":"t#k"}`, 400},
		{"PUT", "/upstreams/x", `{"address":"https://hub.example","token":"t#k"}`, 400},
		{"PUT", "/upstreams/hub", `{"address":"hub.example","token":"t#k"}`, 400}, // exists
		{"PUT", "/upstreams/hub/links/999999", `{"remote":"shop"}`, 404},          // a project nobody named
		{"PUT", "/upstreams/hub/links/1", `{"remote":"shop","x":1}`, 400},
		{"PUT", "/upstreams/hub/links/1", `{"remote":"shop"}`, 200},
		{"PUT", "/upstreams/hub/links/1", `{"remote":"shop","mode":"publish","auto":true}`, 200},
		{"PUT", "/upstreams/hub/links/1", `{"remote":"shop","mode":"overwrite"}`, 400},
		{"PUT", "/upstreams/hub/links/1", `{"remote":"shop","auto":"yes"}`, 400},
		{"POST", "/upstream-resolve", `{"project":1,"note":5,"ours":true}`, 400}, // no conflict on that note
		{"POST", "/upstream-resolve", `{"project":1,"team":2,"note":5}`, 400},
		{"POST", "/upstream-resolve", `{"note":5}`, 400},
		{"GET", "/upstream-resolve", ``, 404},
		{"GET", "/upstreams", ``, 200},
		{"DELETE", "/upstreams/hub", ``, 200},
		{"DELETE", "/upstreams/hub", ``, 404},
	} {
		if r := api.Handle(bg, admin, Request{Method: c.method, Path: c.path, Body: []byte(c.body)}); r.Status != c.want {
			t.Errorf("%s %s %s: %d %s, want %d", c.method, c.path, c.body, r.Status, r.Body, c.want)
		}
	}
}

// A host that keeps its secrets in a map, to see what the routes do to them.
type secretHost struct {
	noHost
	mu       sync.Mutex
	secrets  map[string]string
	connects int
	forgets  []string
	forgetEr error
	delay    time.Duration
}

func (h *secretHost) Connect(name, address, token, device string) (UpstreamInfo, error) {
	time.Sleep(h.delay)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.connects++
	h.secrets[name] = token + "|" + device
	return UpstreamInfo{Device: "DEV", Where: "test"}, nil
}
func (h *secretHost) Forget(name string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.forgets = append(h.forgets, name)
	if h.forgetEr != nil {
		return h.forgetEr
	}
	delete(h.secrets, name)
	return nil
}

func TestUpstreamCredentialsFollowTheRow(t *testing.T) {
	f := newFixture(t)
	h := &secretHost{secrets: map[string]string{}}
	api := New(Options{Store: func() *core.Store { return f.st }, Upstream: h})
	admin := Context{Kind: KindAdmin}
	do := func(method, path, body string) Response {
		return api.Handle(bg, admin, Request{Method: method, Path: path, Body: []byte(body)})
	}
	// two adds of the same name at once: one wins, and the winner's secret stays (the loser must not delete it)
	h.delay = 50 * time.Millisecond
	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i] = do("PUT", "/upstreams/hub", fmt.Sprintf(`{"address":"hub.example","token":"tok-%d#k"}`, i)).Status
		}()
	}
	wg.Wait()
	if !(codes[0] == 200 && codes[1] == 400 || codes[0] == 400 && codes[1] == 200) || h.connects != 1 || len(h.secrets["hub"]) == 0 || len(h.forgets) != 0 {
		t.Fatalf("codes %v connects %d secrets %v forgets %v", codes, h.connects, h.secrets, h.forgets)
	}
	// the default device name is not the computer's name: "gateway <id>"; an owner's choice is checked
	if v := h.secrets["hub"]; !strings.Contains(v, "|gateway ") {
		t.Fatalf("device name: %q", v)
	}
	if r := do("PUT", "/upstreams/x", `{"address":"hub.example","token":"t#k","deviceName":"bad\u0001name"}`); r.Status != 400 {
		t.Fatalf("%d %s", r.Status, r.Body)
	}
	if r := do("PUT", "/upstreams/x", `{"address":"hub.example","token":"t#k","deviceName":"Uros laptop"}`); r.Status != 200 || !strings.HasSuffix(h.secrets["x"], "|Uros laptop") {
		t.Fatalf("%d %s %v", r.Status, r.Body, h.secrets)
	}
	// remove: the secret goes also when the row is already gone, and a secret that cannot be removed is reported
	do("DELETE", "/upstreams/x", "")
	if _, ok := h.secrets["x"]; ok {
		t.Fatal("secret stayed")
	}
	h.secrets["ghost"] = "left over"
	if r := do("DELETE", "/upstreams/ghost", ""); r.Status != 404 || h.secrets["ghost"] != "" {
		t.Fatalf("%d: a secret without a row stayed: %v", r.Status, h.secrets)
	}
	h.forgetEr = errors.New("keychain locked")
	if r := do("DELETE", "/upstreams/hub", ""); r.Status != 500 || !strings.Contains(string(r.Body), "keychain locked") || !strings.Contains(string(r.Body), "remove them by hand") {
		t.Fatalf("%d %s", r.Status, r.Body)
	}
}
