package main

// Activity (GET /activity): tekoče in zadnje zahteve modelom, kot okno Activity na Macu. Le metapodatki, nikoli
// vsebina (korak agenta je orodje ali ukaz); v pomnilniku (ob ponovnem zagonu prazno).

import (
	"bytes"
	"slices"
	"sync"
	"time"
)

const activityMax = 200

// activityRun: tekoča zahteva (POST /v1/messages) z zadnjim korakom agenta.
type activityRun struct {
	ID    string `json:"id"`
	Time  string `json:"time"`
	Model string `json:"model"`
	Step  string `json:"step,omitempty"`
	Steps int    `json:"steps,omitempty"`
	// for GET /minigw/v1/running (never in the admin list): the device that sent the request, its folder, the time of the last
	// step, and the stream's step hook (replyWhenDone)
	owner, folder   string
	started, stepAt time.Time
	sink            func(string)
}

// activityReq: končana zahteva, ki je prišla do modela.
type activityReq struct {
	Time       string `json:"time"`
	Method     string `json:"method"`
	Path       string `json:"path"`
	Model      string `json:"model"`
	Status     int    `json:"status"`
	MS         int64  `json:"ms"`
	Bytes      int    `json:"bytes"`
	Device     string `json:"device,omitempty"`
	DeviceName string `json:"deviceName,omitempty"`
}

type activityLog struct {
	mu   sync.Mutex
	runs []activityRun // najnovejša prva
	reqs []activityReq // najnovejša prva, največ activityMax
}

// activityModel: niz odjemalca le, če je kratek; neznan model (404) ali predolg niz je »unknown« (kot M1 v sejah).
func activityModel(model string, status int) string {
	if status == 404 || len(model) > 200 {
		return "unknown"
	}
	return model
}

// codexMessage: dogodek Codex z besedilom agenta (ne v Activity). ponytail: iskanje niza, preskoči kvečjemu korak več.
func codexMessage(line []byte) bool { return bytes.Contains(line, []byte(`"agent_message"`)) }

// start: tekoča zahteva do vrnjene funkcije (kot Activity.started na Macu: le /v1/messages).
func (a *activityLog) start(r *req, model string) func() {
	if r.URL.Path != "/v1/messages" {
		return func() {}
	}
	a.mu.Lock()
	now := time.Now()
	a.runs = slices.Insert(a.runs, 0, activityRun{ID: r.rid, Time: now.UTC().Format(time.RFC3339), Model: activityModel(model, 0),
		owner: r.owner(), folder: shortLine(r.h("x-claude-mini-cwd"), 300), started: now})
	a.mu.Unlock()
	return func() {
		a.mu.Lock()
		a.runs = slices.DeleteFunc(a.runs, func(x activityRun) bool { return x.ID == r.rid })
		a.mu.Unlock()
	}
}

// step: the agent's newest step (also to the response stream when replyWhenDone registered a hook).
func (a *activityLog) step(rid, text string) {
	a.mu.Lock()
	var sink func(string)
	if i := slices.IndexFunc(a.runs, func(x activityRun) bool { return x.ID == rid }); i >= 0 {
		a.runs[i].Step, a.runs[i].stepAt, sink = text, time.Now(), a.runs[i].sink
		a.runs[i].Steps++
	}
	a.mu.Unlock()
	if sink != nil {
		sink(text)
	}
}

// onStep: the hook that sends each step of this request to its response stream (nil removes it).
func (a *activityLog) onStep(rid string, f func(string)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if i := slices.IndexFunc(a.runs, func(x activityRun) bool { return x.ID == rid }); i >= 0 {
		a.runs[i].sink = f
	}
}

// runningRun: one running request of the asking device (GET /minigw/v1/running); no prompt, no other device's requests.
type runningRun struct {
	Model      string `json:"model"`
	Folder     string `json:"folder,omitempty"`
	ElapsedSec int64  `json:"elapsedSec"`
	Steps      int    `json:"steps"`
	LastStep   string `json:"lastStep,omitempty"`
	StepAgoSec int64  `json:"stepAgoSec,omitempty"`
}

// running: the requests of one owner (device id or "local") that run now; an empty owner sees nothing.
func (a *activityLog) running(owner string) []runningRun {
	out := []runningRun{}
	if owner == "" {
		return out
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	for _, x := range a.runs {
		if x.owner != owner {
			continue
		}
		y := runningRun{Model: x.Model, Folder: x.folder, ElapsedSec: int64(now.Sub(x.started).Seconds()), Steps: x.Steps, LastStep: x.Step}
		if !x.stepAt.IsZero() {
			y.StepAgoSec = int64(now.Sub(x.stepAt).Seconds())
		}
		out = append(out, y)
	}
	return out
}

func (a *activityLog) done(x activityReq) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.reqs = slices.Insert(a.reqs, 0, x)
	if len(a.reqs) > activityMax {
		a.reqs = a.reqs[:activityMax]
	}
}

func (a *activityLog) list() map[string]any {
	a.mu.Lock()
	defer a.mu.Unlock()
	return map[string]any{"inflight": append([]activityRun{}, a.runs...), "requests": append([]activityReq{}, a.reqs...)}
}

// recordActivity: končana zahteva z modelom; naprava kot v zgodovini sej (ime s seznama dovoljenih).
func (s *server) recordActivity(r *req, model string, status int, start time.Time) {
	x := activityReq{Time: start.UTC().Format(time.RFC3339), Method: r.Method, Path: r.URL.Path, Model: activityModel(model, status), Status: status,
		MS: time.Since(start).Milliseconds(), Bytes: len(r.body)}
	if d := r.conn.device; d != nil && (!r.conn.loopback() || s.conf().listed(d.ID)) {
		x.Device, x.DeviceName = d.ID, d.Name
		if i := slices.IndexFunc(s.conf().Devices, func(c serveDevice) bool { return c.ID == d.ID }); i >= 0 && s.conf().Devices[i].Name != "" {
			x.DeviceName = s.conf().Devices[i].Name
		}
	}
	s.activity.done(x)
}
