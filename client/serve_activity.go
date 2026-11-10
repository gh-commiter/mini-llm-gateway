package main

// Activity (GET /activity): tekoče in zadnje zahteve modelom, kot okno Activity na Macu. Le metapodatki, nikoli
// vsebina; v pomnilniku (ob ponovnem zagonu prazno).

import (
	"encoding/json"
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

// start: tekoča zahteva do vrnjene funkcije (kot Activity.started na Macu: le /v1/messages z modelom).
func (a *activityLog) start(r *req) func() {
	var b struct{ Model string }
	if r.Method != "POST" || r.URL.Path != "/v1/messages" || json.Unmarshal(r.body, &b) != nil || b.Model == "" {
		return func() {}
	}
	a.mu.Lock()
	a.runs = slices.Insert(a.runs, 0, activityRun{ID: r.rid, Time: time.Now().UTC().Format(time.RFC3339), Model: b.Model})
	a.mu.Unlock()
	return func() {
		a.mu.Lock()
		a.runs = slices.DeleteFunc(a.runs, func(x activityRun) bool { return x.ID == r.rid })
		a.mu.Unlock()
	}
}

func (a *activityLog) step(rid, text string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if i := slices.IndexFunc(a.runs, func(x activityRun) bool { return x.ID == rid }); i >= 0 {
		a.runs[i].Step = text
	}
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
	x := activityReq{Time: start.UTC().Format(time.RFC3339), Method: r.Method, Path: r.URL.Path, Model: model, Status: status,
		MS: time.Since(start).Milliseconds(), Bytes: len(r.body)}
	if d := r.conn.device; d != nil && (!r.conn.loopback() || s.conf().listed(d.ID)) {
		x.Device, x.DeviceName = d.ID, d.Name
		if i := slices.IndexFunc(s.conf().Devices, func(c serveDevice) bool { return c.ID == d.ID }); i >= 0 && s.conf().Devices[i].Name != "" {
			x.DeviceName = s.conf().Devices[i].Name
		}
	}
	s.activity.done(x)
}
