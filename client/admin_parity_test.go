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
	"testing"
	"time"
)

type admCase struct {
	Name string   `json:"name"`
	Args []string `json:"args"`
	Opts struct {
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
	} `json:"opts"`
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
		provider: c.Opts.Provider, device: c.Opts.Device, folder: c.Opts.Folder, sort: c.Opts.Sort, json: c.Opts.JSON, csv: c.Opts.CSV, explain: c.Opts.Explain}
	if c.Opts.Days != "" {
		o.days = c.Opts.Days
	}
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
