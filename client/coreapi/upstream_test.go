package coreapi

import (
	"context"
	"strings"
	"testing"
	"time"

	"minigw/core"
)

type noHost struct{}

func (noHost) Do(context.Context, string, string, string, map[string]string, []byte) (int, []byte, time.Duration, error) {
	return 0, nil, 0, nil
}
func (noHost) Connect(string, string, string) (UpstreamInfo, error) { return UpstreamInfo{}, nil }
func (noHost) Info(string) UpstreamInfo                             { return UpstreamInfo{} }
func (noHost) Forget(string)                                        {}
func (noHost) Available() bool                                      { return true }

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
		{"GET", "/upstreams", ``, 200},
		{"DELETE", "/upstreams/hub", ``, 200},
		{"DELETE", "/upstreams/hub", ``, 404},
	} {
		if r := api.Handle(bg, admin, Request{Method: c.method, Path: c.path, Body: []byte(c.body)}); r.Status != c.want {
			t.Errorf("%s %s %s: %d %s, want %d", c.method, c.path, c.body, r.Status, r.Body, c.want)
		}
	}
}
