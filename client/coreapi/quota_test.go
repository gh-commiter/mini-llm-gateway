package coreapi

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"minigw/core"
)

func probeSample() core.QuotaProbe {
	now := time.Now().UTC()
	return core.QuotaProbe{ID: "qp_recorded", Provider: "openrouter", AccountScope: "qs_opaque", ProbedAt: now, Status: "ok", Values: []core.QuotaValue{{Name: "credits", Unit: "usd", Value: 2, ObservedAt: now, Source: "openrouter-api"}}}
}

func TestQuotaRecorderSpoolReplayAndOwnerHistory(t *testing.T) {
	dir := t.TempDir()
	st, err := core.Open(dir, core.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	available := false
	a := New(Options{Store: func() *core.Store {
		if available {
			return st
		}
		return nil
	}, Dir: dir})
	a.StartRecorder()
	defer a.StopRecorder()
	p := probeSample()
	if err = a.Call(bg, "quota-probe", map[string]any{"probe": p}, nil); err != nil {
		t.Fatal(err)
	}
	a.FlushRecorder()
	probes, _ := newSpool(filepath.Join(dir, "quota.spool")).loadProbes()
	if len(probes) != 1 {
		t.Fatal(probes)
	}
	available = true
	// Simulate a replay interrupted before the spool is cleared, then replay again.
	saved := SpoolAfterReplay
	defer func() { SpoolAfterReplay = saved }()
	SpoolAfterReplay = func() bool { return true }
	a.rec.replay()
	SpoolAfterReplay = nil
	a.rec.replay()
	p.Values[0].Value = 999
	a.QuotaProbe(p)
	a.FlushRecorder()
	r := a.Handle(bg, Context{Kind: KindAdmin}, Request{Method: "GET", Path: "/quota/history", RawQuery: "days=7&provider=openrouter"})
	if r.Status != 200 || strings.Contains(string(r.Body), "accountScope") || strings.Contains(string(r.Body), "source") || strings.Contains(string(r.Body), "observedAt") {
		t.Fatal(r.Status, string(r.Body))
	}
	var body struct {
		Probes []struct {
			Values []struct{ Value float64 }
		}
		Gaps []any
	}
	if json.Unmarshal(r.Body, &body) != nil || len(body.Probes) != 1 || body.Gaps == nil {
		t.Fatal(string(r.Body))
	}
	// the replayed probe keeps its first value (the timestamp may end in .999Z: look at the decoded field, not the text)
	if v := body.Probes[0].Values; len(v) != 1 || v[0].Value == 999 {
		t.Fatal(string(r.Body))
	}
	for _, cx := range []Context{{}, {Kind: KindMember, DeviceID: "dev", Approved: true}, {Kind: KindRun}} {
		r = a.Handle(bg, cx, Request{Method: "GET", Path: "/quota/history"})
		if r.Status < 400 {
			t.Fatal(cx, r.Status)
		}
	}
	for _, query := range []string{"days=no", "days=0", "days=401", "from=2026-11-01"} {
		r = a.Handle(bg, Context{Kind: KindAdmin}, Request{Method: "GET", Path: "/quota/history", RawQuery: query})
		if r.Status != 400 {
			t.Fatal(query, r.Status)
		}
	}
	p.ErrorKind = "secret raw error"
	p.Status = "error"
	p.Values = nil
	if err = a.Call(bg, "quota-probe", map[string]any{"probe": p}, nil); err == nil {
		t.Fatal("accepted raw error")
	}
}

func TestQuotaQueueAndSpoolLossAreSeparateFromUsage(t *testing.T) {
	st, err := core.Open(t.TempDir(), core.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	a := New(Options{Store: func() *core.Store { return st }})
	u := &Recorder{api: a, ch: make(chan recMsg, 1), spool: newSpool(filepath.Join(t.TempDir(), SpoolName)), quotaSpool: newSpool(filepath.Join(t.TempDir(), "quota.spool"))}
	for i := 0; i < 4; i++ {
		p := probeSample()
		p.ID = fmt.Sprintf("qp_queue%d", i)
		u.enqueue(recMsg{kind: evQuota, probe: p})
	}
	u.takeLost()
	var n int
	st.DB().QueryRow(`SELECT sum(count) FROM quota_gaps`).Scan(&n)
	if n != 3 {
		t.Fatal(n)
	}
	rep, err := st.AdminUsage(bg, 7, core.UsageFilter{})
	if err != nil || len(rep.Gaps) != 0 {
		t.Fatal(rep, err)
	}
	old := SpoolMaxEvents
	SpoolMaxEvents = 3
	defer func() { SpoolMaxEvents = old }()
	for i := 0; i < 12; i++ {
		p := probeSample()
		p.ID = fmt.Sprintf("qp_spool%d", i)
		if err = u.quotaSpool.addProbe(p); err != nil {
			t.Fatal(err)
		}
	}
	probes, gaps := u.quotaSpool.loadProbes()
	var lost int64
	for _, g := range gaps {
		lost += g.Count
	}
	if len(probes) > 3 || int64(len(probes))+lost != 12 {
		t.Fatal(probes, gaps)
	}
	u.replay()
	r, _, err := st.QuotaHistory(bg, 7, "")
	if err != nil || len(r["gaps"].([]any)) != 2 {
		t.Fatal(r, err)
	}
}

func TestQuotaHistoryCapStatus(t *testing.T) {
	f := newFixture(t)
	many := []core.QuotaProbe{}
	for i := 0; i <= core.QuotaHistoryLimit; i++ {
		p := probeSample()
		p.ID = fmt.Sprintf("qp_cap_%04d", i)
		many = append(many, p)
	}
	if err := f.st.ReplayQuota(bg, many, nil); err != nil {
		t.Fatal(err)
	}
	r := f.api.Handle(bg, Context{Kind: KindAdmin}, Request{Method: "GET", Path: "/quota/history"})
	if r.Status != 206 {
		t.Fatal(r.Status, string(r.Body))
	}
}

func TestInvalidQuotaSpoolBecomesOneDurableGap(t *testing.T) {
	f := newFixture(t)
	f.api.StartRecorder()
	defer f.api.StopRecorder()
	f.api.FlushRecorder()
	p := probeSample()
	p.ErrorKind = "not an allowed kind"
	if err := f.api.rec.quotaSpool.addProbe(p); err != nil {
		t.Fatal(err)
	}
	p.ID = "qp_healthy"
	p.ErrorKind = ""
	if err := f.api.rec.quotaSpool.addProbe(p); err != nil {
		t.Fatal(err)
	}
	saved := SpoolAfterReplay
	defer func() { SpoolAfterReplay = saved }()
	SpoolAfterReplay = func() bool { return true }
	f.api.rec.replay()
	SpoolAfterReplay = nil
	f.api.rec.replay()
	rep, _, err := f.st.QuotaHistory(bg, 7, "")
	if err != nil || len(rep["probes"].([]any)) != 1 || len(rep["gaps"].([]any)) != 1 {
		t.Fatal(rep, err)
	}
}

func TestInvalidInProcessQuotaRecordsLoss(t *testing.T) {
	f := newFixture(t)
	f.api.StartRecorder()
	defer f.api.StopRecorder()
	p := probeSample()
	p.Provider = strings.Repeat("p", 257)
	f.api.QuotaProbe(p)
	f.api.FlushRecorder()
	rep, _, err := f.st.QuotaHistory(bg, 7, "")
	if err != nil || len(rep["probes"].([]any)) != 0 || len(rep["gaps"].([]any)) != 1 {
		t.Fatal(rep, err)
	}
}
