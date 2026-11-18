package core

import (
	"database/sql"
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
)

func TestSessionsRolesAndFrozenAttribution(t *testing.T) {
	f := newFx(t)
	pid := f.pid["fakture"]
	filter := SessionFilter{Days: 7}
	for _, u := range []string{"ana", "bor", "eva"} {
		f.event(u, "", f.uid[u], pid, "m", "ok", 0, 1, f.now())
	}
	f.ok(f.SetUserOwner(ctx, f.uid["eva"], true))
	for _, tc := range []struct {
		u string
		n int
	}{{"ana", 1}, {"bor", 1}, {"eva", 1}, {"fani", 3}} {
		page, err := f.Sessions(ctx, f.p(tc.u), pid, filter)
		f.ok(err)
		if len(page.Sessions) != tc.n {
			t.Fatalf("%s: %+v", tc.u, page)
		}
	}
	for _, u := range []string{"cene", "dana"} {
		_, err := f.Sessions(ctx, f.p(u), pid, filter)
		is(t, err, ErrNotFound)
	}
	page, err := f.AdminSessions(ctx, pid, filter)
	f.ok(err)
	if len(page.Sessions) != 3 {
		t.Fatal(page)
	}
	// Current device ownership never changes historical event attribution.
	p := f.p("ana")
	f.ok(f.SetBinding(ctx, p.DeviceID, f.uid["bor"]))
	_, err = f.Sessions(ctx, p, pid, filter)
	is(t, err, ErrNoProjectAccess)
	page, err = f.Sessions(ctx, f.p("bor"), pid, filter)
	f.ok(err)
	if page.Sessions[0].ID != "bor" {
		t.Fatal(page)
	}
	bp := f.p("bor")
	f.ok(f.SetUserActive(ctx, f.uid["bor"], false))
	_, err = f.Sessions(ctx, bp, pid, filter)
	is(t, err, ErrNoProjectAccess)
}

func TestSessionsChainsFiltersAndPagination(t *testing.T) {
	f := newFx(t)
	pid := f.pid["fakture"]
	uid := f.uid["ana"]
	at := f.now()
	f.event("root", "", uid, pid, "z", "ok", 10, 2, at)
	f.event("child", "root", uid, pid, "a", "cancelled", 20, -1, at.Add(24*time.Hour))
	f.event("sibling", "root", uid, pid, "z", "error", 0, 3, at)
	f.event("grandchild", "child", uid, pid, "b", "ok", -1, 0, at)
	f.event("second", "", uid, pid, "z", "ok", 0, 0, at)
	f.event("old", "", uid, pid, "z", "ok", 0, 0, at.AddDate(0, 0, -7))
	f.event("old-child", "old", uid, pid, "a", "ok", 0, 0, at)
	page, err := f.Sessions(ctx, f.p("ana"), pid, SessionFilter{Days: 1, Model: "a"})
	f.ok(err)
	if len(page.Sessions) != 1 {
		t.Fatal(page)
	}
	r := page.Sessions[0]
	if r.ID != "root" || r.Requests != 4 || strings.Join(r.Models, ",") != "a,b,z" || r.Outcome != "failed" || *r.TokensIn.Value != 30 || r.TokensIn.Unknown != 1 || *r.TokensOut.Value != 5 || r.TokensOut.Unknown != 1 || r.DurationMs == nil || *r.DurationMs != 86401000 {
		t.Fatalf("%+v", r)
	}
	if r.Cost != nil || r.UnknownCost != 4 || r.CacheRead.Value != nil || r.CacheRead.Unknown != 4 {
		t.Fatal(r)
	}
	filter := SessionFilter{Days: 1, Limit: 1, Device: "dev"}
	page, err = f.Sessions(ctx, f.p("ana"), pid, filter)
	f.ok(err)
	if len(page.Sessions) != 1 || page.Sessions[0].ID != "second" || page.Next == "" {
		t.Fatal(page)
	}
	filter.After = page.Next
	page, err = f.Sessions(ctx, f.p("ana"), pid, filter)
	f.ok(err)
	if len(page.Sessions) != 1 || page.Sessions[0].ID != "root" || page.Next != "" {
		t.Fatal(page)
	}
	filter.Device = "missing"
	filter.After = ""
	page, err = f.Sessions(ctx, f.p("ana"), pid, filter)
	f.ok(err)
	if len(page.Sessions) != 0 {
		t.Fatal(page)
	}
	for _, filter := range []SessionFilter{{Days: 0}, {Days: 401}, {Days: 1, Limit: 501}, {Days: 1, After: "bad"}} {
		if _, err := f.AdminSessions(ctx, pid, filter); err == nil {
			t.Fatal(filter)
		}
	}
	b, _ := json.Marshal(r)
	for _, field := range []string{"parentId", "prompt", "response", "folder", "errorKind", "transcript"} {
		if strings.Contains(string(b), field) {
			t.Fatal(string(b))
		}
	}
}

func TestSessionsOrphansOpenCyclesAndScope(t *testing.T) {
	f := newFx(t)
	pid := f.pid["fakture"]
	uid := f.uid["ana"]
	at := f.now()
	f.event("foreign", "", uid, f.pid["eureko"], "secret", "ok", 100, 100, at)
	f.event("orphan", "foreign", uid, pid, "m", "ok", 1, 1, at)
	f.event("retained", "gone", uid, pid, "m", "ok", 1, 1, at)
	f.event("nested", "retained", uid, pid, "m", "ok", 1, 1, at)
	f.event("other-user", "retained", f.uid["bor"], pid, "m", "ok", 1, 1, at)
	f.event("other-device", "retained", uid, pid, "m", "ok", 1, 1, at)
	exec(t, f.Store, `UPDATE usage_events SET device_id='other' WHERE id='other-device'`)
	f.event("open-root", "", uid, pid, "m", "ok", 1, 1, at)
	f.ok(f.BeginEvent(ctx, UsageEvent{ID: "open-child", ParentID: "open-root", Start: at, ProjectID: pid, UserID: uid, DeviceID: "dev", Model: "m"}))
	f.event("interrupted", "", uid, pid, "m", "interrupted", -1, -1, at)
	page, err := f.AdminSessions(ctx, pid, SessionFilter{Days: 7})
	f.ok(err)
	if len(page.Sessions) != 5 {
		t.Fatal(page)
	}
	for _, r := range page.Sessions {
		if r.DurationMs != nil || r.ID == "foreign" || r.ID == "open-root" {
			t.Fatal(r)
		}
		if r.ID != "interrupted" && (!r.Incomplete || r.Outcome != "incomplete") {
			t.Fatal(r)
		}
		if r.ID == "retained" && r.Requests != 2 {
			t.Fatal(r)
		}
	}
	f.event("cycle-a", "cycle-b", uid, pid, "m", "ok", 1, 1, at)
	f.event("cycle-b", "cycle-a", uid, pid, "m", "ok", 1, 1, at)
	if _, err = f.AdminSessions(ctx, pid, SessionFilter{Days: 7}); err == nil || !strings.Contains(err.Error(), "cyclic") {
		t.Fatal(err)
	}
}

func TestSessionsCostsCacheAndOverflow(t *testing.T) {
	f := newFx(t)
	pid := f.pid["fakture"]
	uid := f.uid["ana"]
	at := f.now()
	for _, id := range []string{"root", "child", "unknown"} {
		parent := "root"
		if id == "root" {
			parent = ""
		}
		f.event(id, parent, uid, pid, "m", "ok", 0, 0, at)
	}
	exec(t, f.Store, `UPDATE usage_events SET cost_usd_nanos=1234567890,cost_source='reported',cache_read_tokens=7,cache_write_tokens=0 WHERE id='root'`)
	exec(t, f.Store, `UPDATE usage_events SET cost_usd_nanos=1,cost_source='estimated',cache_read_tokens=0 WHERE id='child'`)
	page, err := f.AdminSessions(ctx, pid, SessionFilter{Days: 7})
	f.ok(err)
	r := page.Sessions[0]
	if *r.Cost != "1.234567891" || *r.ReportedCost != "1.234567890" || *r.EstimatedCost != "0.000000001" || r.UnknownCost != 1 || *r.TokensIn.Value != 0 || *r.CacheRead.Value != 7 || r.CacheRead.Unknown != 1 || *r.CacheWrite.Value != 0 || r.CacheWrite.Unknown != 2 {
		t.Fatalf("%+v", r)
	}
	exec(t, f.Store, `UPDATE usage_events SET tokens_in=? WHERE id IN ('root','child')`, int64(math.MaxInt64))
	if _, err = f.AdminSessions(ctx, pid, SessionFilter{Days: 7}); err == nil {
		t.Fatal("token overflow accepted")
	}
	exec(t, f.Store, `UPDATE usage_events SET tokens_in=0,cost_usd_nanos=?`, int64(math.MaxInt64))
	if _, err = f.AdminSessions(ctx, pid, SessionFilter{Days: 7}); err == nil {
		t.Fatal("cost overflow accepted")
	}
	var m SessionMetric
	f.ok(m.add(sql.NullInt64{}))
	if m.Value != nil || m.Unknown != 1 {
		t.Fatal(m)
	}
}

func TestSessionsUTCRetentionAndOutcomes(t *testing.T) {
	f := newFx(t)
	pid := f.pid["fakture"]
	uid := f.uid["ana"]
	midnight := time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC)
	f.event("before", "", uid, pid, "m", "ok", 1, 1, midnight.Add(-time.Millisecond))
	f.event("boundary", "", uid, pid, "m", "cancelled", 1, 1, midnight)
	f.event("overlap", "boundary", uid, pid, "m", "ok", 1, 1, midnight)
	f.event("expired", "", uid, pid, "m", "ok", 100, 100, f.now().AddDate(0, 0, -401))
	f.event("retained-orphan", "expired", uid, pid, "m", "ok", 2, 2, midnight)
	n, err := f.FoldOldUsage(ctx)
	f.ok(err)
	if n != 1 {
		t.Fatal(n)
	}
	page, err := f.Sessions(ctx, f.p("ana"), pid, SessionFilter{Days: 1})
	f.ok(err)
	if len(page.Sessions) != 2 {
		t.Fatal(page)
	}
	for _, r := range page.Sessions {
		if r.ID == "boundary" {
			if r.Outcome != "cancelled" || *r.DurationMs != 1000 || r.Requests != 2 {
				t.Fatal(r)
			}
		}
		if r.ID == "retained-orphan" {
			if !r.Incomplete || r.DurationMs != nil || *r.TokensIn.Value != 2 {
				t.Fatal(r)
			}
		}
	}
}

// Events older than the window are not read: a project with more than the traversal limit of them still answers.
func TestSessionsReadOnlyTheWindow(t *testing.T) {
	f := newFx(t)
	pid := f.pid["fakture"]
	uid := f.uid["ana"]
	at := f.now()
	old := at.AddDate(0, 0, -10)
	exec(t, f.Store, `WITH RECURSIVE seq(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM seq WHERE x<100001)
INSERT INTO usage_events(id,start,end,user_id,device_id,project_id,provider,model,outcome,token_source)
SELECT 'old-'||x, ?, ?, ?, 'dev', ?, 'codex', 'm', 'ok', 'unknown' FROM seq`, ms(old), ms(old.Add(time.Second)), uid, pid)
	f.event("now", "", uid, pid, "m", "ok", 1, 1, at)
	page, err := f.Sessions(ctx, f.p("ana"), pid, SessionFilter{Days: 1})
	f.ok(err)
	if len(page.Sessions) != 1 || page.Sessions[0].ID != "now" {
		t.Fatal(page)
	}
}

// Chains are read whole or not at all: a chain that began just before the window (or older than the one-day margin,
// with a later request inside it) never surfaces as an orphan of its first in-window request.
func TestSessionsChainAcrossTheWindowStart(t *testing.T) {
	f := newFx(t)
	pid := f.pid["fakture"]
	uid := f.uid["ana"]
	at := f.now()
	midnight := time.Date(at.Year(), at.Month(), at.Day(), 0, 0, 0, 0, time.UTC)
	// A chain that began a few hours before the day's window and continued inside it.
	f.event("near", "", uid, pid, "m", "ok", 1, 1, midnight.Add(-3*time.Hour))
	f.event("near-child", "near", uid, pid, "m", "ok", 2, 2, midnight.Add(time.Hour))
	// A chain older than the one-day margin: its later request is in every window, its root is not.
	f.event("far", "", uid, pid, "m", "ok", 4, 4, at.AddDate(0, 0, -3))
	f.event("far-child", "far", uid, pid, "m", "ok", 8, 8, at)

	page, err := f.Sessions(ctx, f.p("ana"), pid, SessionFilter{Days: 1})
	f.ok(err)
	if len(page.Sessions) != 0 { // no chain starts in this window: no child may surface as a session of its own
		t.Fatalf("%+v", page.Sessions)
	}

	page, err = f.Sessions(ctx, f.p("ana"), pid, SessionFilter{Days: 2})
	f.ok(err)
	if len(page.Sessions) != 1 || page.Sessions[0].ID != "near" || page.Sessions[0].Requests != 2 ||
		page.Sessions[0].TokensIn.Value == nil || *page.Sessions[0].TokensIn.Value != 3 {
		t.Fatalf("%+v", page.Sessions) // the near chain is whole; the far one stays out
	}

	page, err = f.Sessions(ctx, f.p("ana"), pid, SessionFilter{Days: 4})
	f.ok(err)
	if len(page.Sessions) != 2 {
		t.Fatalf("%+v", page.Sessions)
	}
	for _, r := range page.Sessions {
		if r.Requests != 2 || r.Incomplete { // both chains are joined to their roots
			t.Fatalf("%+v", r)
		}
	}
}
