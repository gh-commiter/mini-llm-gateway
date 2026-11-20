package coreapi

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"minigw/core"
)

func TestReconcileValidatesBeforeAnyWriteAndIsOwnerOnly(t *testing.T) {
	f := newFixture(t)
	calls := 0
	h := &methodHost{secretHost: secretHost{secrets: map[string]string{}}}
	h.answer = func(string, string) (int, string) { calls++; return 500, `{}` }
	a := New(Options{Store: func() *core.Store { return f.st }, Upstream: h})
	for _, body := range []string{
		`{}`, `{"project":1,"team":1}`, `{"project":1,"dryRun":true,"note":1,"new":true}`,
		`{"project":1,"all":true,"new":true}`, `{"project":1,"note":1,"confirm":2,"skip":true}`,
		`{"project":1,"note":-1}`, `{"project":1,"note":1,"new":true,"extra":true}`,
	} {
		r := a.Handle(bg, Context{Kind: KindAdmin}, Request{Method: "POST", Path: "/upstream-reconcile", Body: []byte(body)})
		if r.Status != 400 {
			t.Fatalf("%s: %d %s", body, r.Status, r.Body)
		}
	}
	for _, kind := range []Kind{KindMember, KindRun, KindInternal} {
		r := a.Handle(bg, Context{Kind: kind, DeviceID: "dev-ana", Approved: true}, Request{Method: "POST", Path: "/upstream-reconcile", Body: []byte(fmt.Sprintf(`{"project":%d}`, f.pid["fakture"]))})
		if r.Status == 200 {
			t.Fatalf("%s reached reconcile", kind)
		}
	}
	var audits, keys int
	if e := f.st.DB().QueryRow(`SELECT count(*) FROM audit_events WHERE action LIKE 'upstream.reconcile%'`).Scan(&audits); e != nil {
		t.Fatal(e)
	}
	if e := f.st.DB().QueryRow(`SELECT count(*) FROM upstream_key`).Scan(&keys); e != nil {
		t.Fatal(e)
	}
	if calls != 0 || audits != 0 || keys != 0 {
		t.Fatal("invalid or non-owner request had effects", calls, audits, keys)
	}
}

func TestReconcilePanelDetailNamesCommandAndCounts(t *testing.T) {
	j := toAdminLink(core.UpstreamLinkInfo{UpstreamLink: core.UpstreamLink{Project: 3, Upstream: "hub"}, Label: "shop", Restored: &time.Time{}, ReconcileCounts: &core.ReconcileCounts{Candidates: 2, Unknown: 1}})
	if !j.RestoredBlocked || !strings.Contains(j.ReconcileState, "minigw gw upstream reconcile shop --upstream hub; last listing: candidates 2, unknown 1") {
		t.Fatal(j)
	}
}
