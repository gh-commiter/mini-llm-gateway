package main

import (
	"context"
	"encoding/json"
	"fmt"
	"minigw/core"
	"minigw/coreapi"
	"os"
	"strings"
	"testing"
	"time"
)

func TestProjectSessionCommandsAndNames(t *testing.T) {
	c := newClientFx(t)
	dir := t.TempDir()
	f := c.f
	pid := f.pid["fakture"]
	// More than one API page, with equal timestamps; every root must appear once.
	at := time.Now().UTC()
	for i := 0; i < 101; i++ {
		f.ok(f.s.core.FinishEvent(context.Background(), core.UsageEvent{ID: fmt.Sprintf("root-%03d", i), Start: at, End: at.Add(time.Second), ProjectID: pid, UserID: f.uid["eva"], DeviceID: devID(f.dev["eva"]), Model: "m", Outcome: "ok", TokensIn: 0, TokensOut: -1, TokenSource: "reported"}))
	}
	if _, err := c.run("eva", dir, "", "project", "sessions"); err == nil {
		t.Fatal("unlinked project defaulted")
	}
	text, err := c.run("eva", dir, "", "project", "sessions", "fakture", "--json")
	f.ok(err)
	var page core.SessionPage
	f.ok(json.Unmarshal([]byte(text), &page))
	if len(page.Sessions) != 101 || page.Sessions[0].DeviceName != "eva" {
		t.Fatalf("%d %s", len(page.Sessions), text)
	}
	_, err = c.run("eva", dir, "", "project", "link", "fakture")
	f.ok(err)
	text, err = c.run("eva", dir, "", "project", "sessions", "--model", "m", "--device", devID(f.dev["eva"]))
	f.ok(err)
	if strings.Count(text, " requests ") != 101 || !strings.Contains(text, "unknown out") {
		t.Fatal(text)
	}
	f.s.update(func(cfg *serveConfig) error {
		for i := range cfg.Devices {
			if cfg.Devices[i].ID == devID(f.dev["eva"]) {
				cfg.Devices[i].Name = "new name"
			}
		}
		return nil
	})
	text, err = c.run("eva", dir, "", "project", "sessions", "fakture")
	f.ok(err)
	if !strings.Contains(text, "new name") {
		t.Fatal(text)
	}
	st, obj := f.s.adminDo("GET", fmt.Sprintf("/usage/%%73essions/?project=%d", pid), nil)
	if st != 200 {
		t.Fatalf("%d %+v", st, obj)
	}
	// Missing names fall back to IDs, without changing frozen user identity.
	body, _ := json.Marshal(core.SessionPage{Sessions: []core.SessionRow{{DeviceID: "deleted", UserID: 99}}})
	r := f.s.sessionNames(coreapi.Response{Status: 200, Body: body})
	f.ok(json.Unmarshal(r.Body, &page))
	if page.Sessions[0].DeviceName != "deleted" || page.Sessions[0].UserID != 99 {
		t.Fatal(page)
	}
}

func TestSessionTextSharedFixture(t *testing.T) {
	b, err := os.ReadFile("testdata/project-sessions.json")
	if err != nil {
		t.Fatal(err)
	}
	var page core.SessionPage
	if err = json.Unmarshal(b, &page); err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile("testdata/project-sessions.txt")
	if err != nil {
		t.Fatal(err)
	}
	if got := sessionText(page); got != string(want) {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestOwnerSessionCommandFollowsPages(t *testing.T) {
	b, err := os.ReadFile("testdata/project-sessions.json")
	if err != nil {
		t.Fatal(err)
	}
	var page core.SessionPage
	if err = json.Unmarshal(b, &page); err != nil {
		t.Fatal(err)
	}
	page.Next = "cursor"
	first, _ := json.Marshal(page)
	c := &admCase{Args: []string{"project-sessions"}, Responses: []cmdResp{
		{Match: "GET /projects", Status: 200, Body: `{"projects":[{"id":7,"slug":"p"}]}`},
		{Match: "GET /usage/sessions?days=7&device=d&model=a&project=7", Status: 200, Body: string(first)},
		{Match: "GET /usage/sessions?after=cursor&days=7&device=d&model=a&project=7", Status: 200, Body: `{"sessions":[]}`},
	}}
	c.Opts.Project = "p"
	c.Opts.Device = "d"
	c.Opts.Model = "a"
	got := runAdminCase(c)
	want, _ := os.ReadFile("testdata/project-sessions.txt")
	if got.Exit != 0 || got.Out != string(want) || len(got.Requests) != 3 {
		t.Fatalf("%+v", got)
	}
	c.Opts.JSON = true
	got = runAdminCase(c)
	if got.Exit != 0 || !strings.Contains(got.Out, `"costUsd":"1.234567891"`) || strings.Contains(got.Out, `"next"`) {
		t.Fatalf("%+v", got)
	}
	c.Opts.Project = ""
	got = runAdminCase(c)
	if got.Exit != 1 || !strings.Contains(got.Err, "--project required") {
		t.Fatalf("%+v", got)
	}
}
