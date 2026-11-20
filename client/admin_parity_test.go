package main

// Parity of the owner commands (minigw gw user|bind|project|member|grant|core|usage-events|quota) with the Mac client
// (Sources/RouterCore/AdminCommands.swift): client/testdata/admin-commands.json holds command -> requests -> output for
// fixed fake admin answers, asserted here and in Tests/RouterCoreTests/AdminCommandsTests.swift. MGW_UPDATE_FIXTURES=1
// rewrites the expected values from this implementation: review the diff before committing it.

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type admCase struct {
	Name string   `json:"name"`
	Args []string `json:"args"`
	Opts struct {
		Revision    *int64 `json:"revision,omitempty"`
		Current     *int64 `json:"current,omitempty"`
		Reason      string `json:"reason,omitempty"`
		Version     string `json:"version,omitempty"`
		TTY         bool   `json:"tty,omitempty"`
		Days        string `json:"days,omitempty"`
		User        string `json:"user,omitempty"`
		Project     string `json:"project,omitempty"`
		Model       string `json:"model,omitempty"`
		ClaudeProbe string `json:"claudeProbe,omitempty"`
		Provider    string `json:"provider,omitempty"`
		Device      string `json:"device,omitempty"`
		Folder      string `json:"folder,omitempty"`
		JSON        bool   `json:"json,omitempty"`
		Explain     bool   `json:"explain,omitempty"`
		CSV         bool   `json:"csv,omitempty"`
		Sort        string `json:"sort,omitempty"`
		Team        string `json:"team,omitempty"`
		By          string `json:"by,omitempty"`
		Phase       string `json:"phase,omitempty"`
		Activity    string `json:"activity,omitempty"`
		Task        string `json:"task,omitempty"`
		LabelSource string `json:"labelSource,omitempty"`
		// upstream commands
		DeviceName    string `json:"deviceName,omitempty"`
		RemoteProject string `json:"remoteProject,omitempty"`
		RemoteTeam    string `json:"remoteTeam,omitempty"`
		Class         string `json:"class,omitempty"`
		WithPrivate   bool   `json:"withPrivate,omitempty"`
		DryRun        bool   `json:"dryRun,omitempty"`
		Auto          bool   `json:"auto,omitempty"`
		Manual        bool   `json:"manual,omitempty"`
		Theirs        bool   `json:"theirs,omitempty"`
		Ours          bool   `json:"ours,omitempty"`
		AfterRestore  bool   `json:"afterRestore,omitempty"`
		Yes           bool   `json:"yes,omitempty"`
		Mode          string `json:"mode,omitempty"`
		Upstream      string `json:"upstream,omitempty"`
		Read          bool   `json:"read,omitempty"`
		NoRead        bool   `json:"noRead,omitempty"`
		ReadClass     string `json:"readClass,omitempty"`
		ReadSources   string `json:"readSources,omitempty"`
		AcceptReach   bool   `json:"acceptReach,omitempty"`
		PullEvery     string `json:"pullEvery,omitempty"`
		ShowErrors    bool   `json:"showErrors,omitempty"` // labels eval
	} `json:"opts"`
	File      string    `json:"file,omitempty"` // labels eval: the content of the examples file (written to a temp file by the test)
	path      string    // where the test wrote it
	Stdin     string    `json:"stdin,omitempty"` // what the command reads (the upstream's token)
	Responses []cmdResp `json:"responses"`
	Want      *admWant  `json:"want,omitempty"`
}

type admWant struct {
	Out      string     `json:"out"`
	Err      string     `json:"err"`
	Exit     int        `json:"exit"`
	Requests [][]string `json:"requests"` // method, path (with query), body
}

type exitPanic int

// captured runs f with stdout and stderr captured and fail() turned into a panic.
func captured(f func()) (out, errText string, exit int) {
	so, se, savedExit := os.Stdout, os.Stderr, exitFn
	ro, wo, _ := os.Pipe()
	re, we, _ := os.Pipe()
	os.Stdout, os.Stderr = wo, we
	exitFn = func(c int) { panic(exitPanic(c)) }
	var bo, be bytes.Buffer
	done := make(chan struct{}, 2)
	go func() { io.Copy(&bo, ro); done <- struct{}{} }()
	go func() { io.Copy(&be, re); done <- struct{}{} }()
	func() {
		defer func() {
			if r := recover(); r != nil {
				c, ok := r.(exitPanic)
				if !ok {
					panic(r)
				}
				exit = int(c)
			}
		}()
		f()
	}()
	wo.Close()
	we.Close()
	<-done
	<-done
	os.Stdout, os.Stderr, exitFn = so, se, savedExit
	return bo.String(), be.String(), exit
}

func runAdminCase(c *admCase) *admWant {
	var reqs [][]string
	a := &adminClient{fake: func(method, path string, body []byte) (int, []byte) {
		reqs = append(reqs, []string{method, path, string(body)})
		for _, r := range c.Responses {
			if r.Match == method+" "+path {
				return r.Status, []byte(r.Body)
			}
		}
		return 599, []byte(`{"error":{"message":"no such response in the fixture"}}`)
	}}
	o := coreOpts{days: "7", user: c.Opts.User, project: c.Opts.Project, model: c.Opts.Model, claudeProbe: c.Opts.ClaudeProbe,
		provider: c.Opts.Provider, device: c.Opts.Device, folder: c.Opts.Folder, sort: c.Opts.Sort, json: c.Opts.JSON, csv: c.Opts.CSV, explain: c.Opts.Explain, reason: c.Opts.Reason, version: c.Opts.Version, team: c.Opts.Team, by: c.Opts.By, labelSource: c.Opts.LabelSource, phase: c.Opts.Phase, activity: c.Opts.Activity, task: c.Opts.Task,
		in: strings.NewReader(c.Stdin), remoteProject: c.Opts.RemoteProject, remoteTeam: c.Opts.RemoteTeam, class: c.Opts.Class, deviceName: c.Opts.DeviceName, auto: c.Opts.Auto, manual: c.Opts.Manual, theirs: c.Opts.Theirs, ours: c.Opts.Ours, mode: c.Opts.Mode, upstream: c.Opts.Upstream, withPrivate: c.Opts.WithPrivate, dryRun: c.Opts.DryRun, afterRestore: c.Opts.AfterRestore, yes: c.Opts.Yes, read: c.Opts.Read, noRead: c.Opts.NoRead, readClass: c.Opts.ReadClass, readSources: c.Opts.ReadSources, pullEvery: c.Opts.PullEvery, acceptReach: c.Opts.AcceptReach, file: c.path, showErrors: c.Opts.ShowErrors}
	if c.Opts.Revision != nil {
		o.revision = *c.Opts.Revision
		o.hasRevision = true
	}
	if c.Opts.Current != nil {
		o.current = *c.Opts.Current
		o.hasCurrent = true
	}
	if c.Opts.Days != "" {
		o.days = c.Opts.Days
	}
	saved := adminTerminal
	adminTerminal = func() bool { return c.Opts.TTY }
	defer func() { adminTerminal = saved }()
	out, errText, exit := captured(func() { coreCommand(a, c.Args, o) })
	if reqs == nil {
		reqs = [][]string{}
	}
	return &admWant{Out: out, Err: errText, Exit: exit, Requests: reqs}
}

func TestAdminParityCommands(t *testing.T) {
	var fx struct {
		Cases []*admCase `json:"cases"`
	}
	readFixture(t, "admin-commands.json", &fx)
	saved := time.Local
	time.Local = time.UTC // times are printed in local time; the fixture is in UTC
	defer func() { time.Local = saved }()
	if len(fx.Cases) < 60 {
		t.Fatalf("only %d cases", len(fx.Cases))
	}
	for _, c := range fx.Cases {
		t.Run(c.Name, func(t *testing.T) {
			if c.File != "" {
				c.path = filepath.Join(t.TempDir(), "examples.jsonl")
				if err := os.WriteFile(c.path, []byte(c.File), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			got := runAdminCase(c)
			if updateFixtures() {
				c.Want = got
				return
			}
			if c.Want == nil {
				t.Fatal("no expected values in the fixture")
			}
			gj, _ := json.Marshal(got)
			wj, _ := json.Marshal(c.Want)
			if !bytes.Equal(gj, wj) {
				t.Fatalf("\n got %s\nwant %s", gj, wj)
			}
		})
	}
	if updateFixtures() {
		writeFixture(t, "admin-commands.json", fx)
	}
}

// The provider/model report (minigw gw usage): client/testdata/usage-report.json, asserted here and in
// Tests/RouterCoreTests/UsageReportTests.swift.
func TestUsageReportParity(t *testing.T) {
	var fx struct {
		Cases []*admCase `json:"cases"`
	}
	readFixture(t, "usage-report.json", &fx)
	if len(fx.Cases) < 10 {
		t.Fatalf("only %d cases", len(fx.Cases))
	}
	for _, c := range fx.Cases {
		t.Run(c.Name, func(t *testing.T) {
			got := runAdminCase(c)
			if updateFixtures() {
				c.Want = got
				return
			}
			if c.Want == nil {
				t.Fatal("no expected values in the fixture")
			}
			gj, _ := json.Marshal(got)
			wj, _ := json.Marshal(c.Want)
			if !bytes.Equal(gj, wj) {
				t.Fatalf("\n got %s\nwant %s", gj, wj)
			}
		})
	}
	if updateFixtures() {
		writeFixture(t, "usage-report.json", fx)
	}
}

// The upstream commands (minigw gw upstream ...): client/testdata/upstream-commands.json. The Go gateway only for now (the Mac app's
// gateway does not support upstreams yet).
func TestUpstreamCommandParity(t *testing.T) {
	var fx struct {
		Cases []*admCase `json:"cases"`
	}
	readFixture(t, "upstream-commands.json", &fx)
	saved := time.Local
	time.Local = time.UTC
	defer func() { time.Local = saved }()
	if len(fx.Cases) < 15 {
		t.Fatalf("only %d cases", len(fx.Cases))
	}
	for _, c := range fx.Cases {
		t.Run(c.Name, func(t *testing.T) {
			got := runAdminCase(c)
			if updateFixtures() {
				c.Want = got
				return
			}
			if c.Want == nil {
				t.Fatal("no expected values in the fixture")
			}
			gj, _ := json.Marshal(got)
			wj, _ := json.Marshal(c.Want)
			if !bytes.Equal(gj, wj) {
				t.Fatalf("\n got %s\nwant %s", gj, wj)
			}
		})
	}
	if updateFixtures() {
		writeFixture(t, "upstream-commands.json", fx)
	}
}

func TestNoteHistoryOwnerParity(t *testing.T) {
	var fx struct {
		Gateway string          `json:"gateway"`
		Cases   json.RawMessage `json:"cases"`
		Owner   []*admCase      `json:"owner"`
	}
	readFixture(t, "note-history.json", &fx)
	saved := time.Local
	time.Local = time.UTC
	defer func() { time.Local = saved }()
	for _, c := range fx.Owner {
		t.Run(c.Name, func(t *testing.T) {
			got := runAdminCase(c)
			if updateFixtures() {
				c.Want = got
				return
			}
			gj, _ := json.Marshal(got)
			wj, _ := json.Marshal(c.Want)
			if !bytes.Equal(gj, wj) {
				t.Fatalf("got %s want %s", gj, wj)
			}
		})
	}
	if updateFixtures() {
		writeFixture(t, "note-history.json", fx)
	}
}

func TestNoteHistoryOwnerRejectsScope(t *testing.T) {
	for _, scope := range []string{"team", "project"} {
		c := &admCase{Args: []string{"kb", "history", "7"}}
		if scope == "team" {
			c.Opts.Team = "garden"
		} else {
			c.Opts.Project = "orchard"
		}
		got := runAdminCase(c)
		if got.Exit != 1 || len(got.Requests) != 0 || !strings.Contains(got.Err, "usage:") {
			t.Fatal(scope, got)
		}
	}
}
