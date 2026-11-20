package core

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// restoreLinks: a fixture with an automatic submit link (fakture -> hub), a manual publish link (eureko -> hub) and a second upstream
// for the first project (fakture -> hub2).
func restoreLinks(t *testing.T) (*fx, int64, int64) {
	f, fk, _ := autoFx(t)
	ek := f.pid["eureko"]
	f.ok(f.AddUpstream(ctx, "hub2", "hub2.example:8765"))
	f.ok(f.LinkUpstreamWith(ctx, LinkSpec{Project: ek, Upstream: "hub", Remote: "other", Mode: "publish"}))
	f.ok(f.LinkUpstreamWith(ctx, LinkSpec{Project: fk, Upstream: "hub2", Remote: "shop"}))
	return f, fk, ek
}

func (f *fx) reopen() {
	f.t.Helper()
	dir := f.Store.dir
	f.ok(f.Close())
	s, e := Open(dir, Options{})
	f.ok(e)
	f.Store = s
	f.Store.now = func() time.Time { return f.clock }
	f.Store.jitter = func() float64 { return 0.5 }
}

func (f *fx) marked() (n int) {
	f.t.Helper()
	ls, e := f.UpstreamLinks(ctx, 0, 0)
	f.ok(e)
	for _, l := range ls {
		if l.Restored != nil {
			n++
		}
	}
	return
}

func (f *fx) markAll() {
	f.t.Helper()
	f.ok(f.write(ctx, func(tx *sql.Tx) error { return markRestored(tx, f.clock) }))
}

func (f *fx) restoreFrom(bak string) {
	f.t.Helper()
	dir := f.Store.dir
	f.ok(f.Close())
	f.ok(Restore(dir, bak))
	s, e := Open(dir, Options{})
	f.ok(e)
	f.Store = s
	f.Store.now = func() time.Time { return f.clock }
	f.Store.jitter = func() float64 { return 0.5 }
}

func TestStagedRestoreMarksEveryLinkAndItSurvivesARestart(t *testing.T) {
	f, fk, _ := restoreLinks(t)
	f.clock = f.clock.Add(time.Hour)
	f.ok(f.write(ctx, func(tx *sql.Tx) error { return f.markUpstreamDirty(tx, fk) }))
	bak := filepath.Join(t.TempDir(), "b.db")
	f.ok(f.Backup(ctx, bak))
	if f.marked() != 0 {
		t.Fatal("a link is marked before any restore")
	}
	f.restoreFrom(bak)
	if f.marked() != 3 {
		t.Fatalf("%d of 3 links marked after the restore", f.marked())
	}
	for _, l := range func() []UpstreamLinkInfo { ls, _ := f.UpstreamLinks(ctx, 0, 0); return ls }() {
		if l.LastError != "" || l.PullError != "" {
			t.Fatalf("the mark is not a refusal or a pull error: %+v", l)
		}
	}
	if l := f.link(fk); !l.Auto || l.DirtySince == nil {
		t.Fatalf("the automatic setting and the pending work are kept: %+v", l)
	}
	f.reopen()
	if f.marked() != 3 {
		t.Fatal("the mark did not survive a restart")
	}
}

func TestInterruptedRestoreBeforeTheSwapMarksNothing(t *testing.T) {
	f, _, _ := restoreLinks(t)
	bak := filepath.Join(t.TempDir(), "b.db")
	f.ok(f.Backup(ctx, bak))
	dir := f.Store.dir
	f.ok(f.Close())
	restoreHook = func(stage string) error {
		if stage == "prepared" { // the staged copy (marked by stamp) is stored nowhere: the live database is untouched
			return errCrash
		}
		return nil
	}
	defer func() { restoreHook = func(string) error { return nil } }()
	if err := Restore(dir, bak); err == nil {
		t.Fatal("the hook did not stop the restore")
	}
	restoreHook = func(string) error { return nil }
	s, e := Open(dir, Options{})
	f.ok(e)
	f.Store = s
	if f.marked() != 0 {
		t.Fatal("a failed restore left marks")
	}
}

func TestMarkedLinkSendsNothingInAnyMode(t *testing.T) {
	f, fk, ek := restoreLinks(t)
	f.clock = f.clock.Add(time.Hour)
	f.ok(f.write(ctx, func(tx *sql.Tx) error { return f.markUpstreamDirty(tx, fk) }))
	f.markAll()
	before := f.link(fk)
	up := okUp()
	sent := func() (n int) {
		f.ok(f.DB().QueryRow(`SELECT count(*) FROM upstream_sent`).Scan(&n))
		return
	}
	for _, dry := range []bool{false, true} { // a dry run still lists what would go, and says the link is blocked
		for _, project := range []int64{fk, ek} { // submit and publish
			res, err := f.PushUpstream(ctx, up, project, 0, "", 0, dry, noSleep)
			f.ok(err)
			for _, r := range res {
				if !r.Blocked || !dry && (r.Created != 0 || r.Replaced != 0) || len(r.Lines) == 0 || !strings.HasPrefix(r.Lines[0], "blocked: "+RestoredState) {
					t.Fatalf("dry=%v project %d: %+v", dry, project, r)
				}
				if r.DryRun != dry {
					t.Fatalf("dry=%v: %+v", dry, r)
				}
			}
		}
	}
	if up.count("POST") != 0 || sent() != 0 {
		t.Fatalf("something was written: %d POST, %d receipts", up.count("POST"), sent())
	}
	if r, _ := f.PushUpstream(ctx, up, fk, 0, "hub", 0, false, noSleep); !strings.Contains(r[0].Lines[0], "(3 links of this gateway are marked;") {
		t.Fatalf("the blocked line does not say how many links are marked: %v", r[0].Lines)
	}
	if !strings.Contains(restoreBlockedLine(UpstreamLink{Upstream: "hub", Team: true}, "team Eng", 3), "--team Eng --upstream hub --after-restore") {
		t.Fatal("the team command is wrong")
	}
	// automatic: nothing is selected, the next wake-up does not count the link, nothing churns however long we wait
	for i := 0; i < 4; i++ {
		f.clock = f.clock.Add(time.Hour)
		r, err := f.AutoPush(ctx, up, noSleep)
		f.ok(err)
		if r.Ran || r.Next != 0 {
			t.Fatalf("the timer would wake up for a marked link: %+v", r)
		}
	}
	if up.count("POST") != 0 {
		t.Fatal("the automatic push sent something")
	}
	after := f.link(fk)
	if after.FailCount != before.FailCount || after.NextAttempt != nil || after.DirtySince == nil || after.LastPush != nil {
		t.Fatalf("pending work or the back-off changed: before %+v after %+v", before, after)
	}
	// the select/lock race: a push that gets past the selection still stops at the gate under the link lock
	res, err := f.pushLink(ctx, up, after.UpstreamLink, after.Label, 0, false, noSleep, true)
	f.ok(err)
	if !res.Blocked || res.Created != 0 || up.count("POST") != 0 || f.link(fk).FailCount != 0 {
		t.Fatalf("%+v", res)
	}
}

func TestLinkEditsDoNotClearTheMarkButUnlinkAndRemovalDo(t *testing.T) {
	f, fk, ek := restoreLinks(t)
	f.markAll()
	no, yes := false, true
	for _, a := range []*bool{&no, &yes, nil} {
		f.ok(f.LinkUpstreamWith(ctx, LinkSpec{Project: fk, Upstream: "hub", Remote: "shop", Auto: a}))
	}
	f.ok(f.LinkUpstreamWith(ctx, LinkSpec{Project: ek, Upstream: "hub", Remote: "other", Mode: "publish"}))
	if f.marked() != 3 {
		t.Fatalf("an ordinary link edit cleared a mark: %d", f.marked())
	}
	f.ok(f.UnlinkUpstream(ctx, ek, 0, "hub"))
	var keys int
	f.ok(f.DB().QueryRow(`SELECT count(*) FROM meta WHERE key LIKE ?`, restoredPrefix+"%").Scan(&keys))
	if keys != 2 || f.marked() != 2 {
		t.Fatalf("unlink: %d keys, %d marked", keys, f.marked())
	}
	f.ok(f.LinkUpstreamWith(ctx, LinkSpec{Project: ek, Upstream: "hub", Remote: "other"}))
	if l := f.link(ek); l.Restored != nil {
		t.Fatal("a new link of the same name is marked")
	}
	f.ok(f.RemoveUpstream(ctx, "hub2"))
	f.ok(f.DB().QueryRow(`SELECT count(*) FROM meta WHERE key LIKE ?`, restoredPrefix+"%").Scan(&keys))
	if keys != 1 {
		t.Fatalf("removal of the upstream left %d keys", keys)
	}
}

// An epoch gap between the database and core.state is no evidence of a restore (a crash on a healthy gateway can leave one): nothing is marked.
func TestEpochGapAloneMarksNothing(t *testing.T) {
	f, _, _ := restoreLinks(t)
	dir := f.Store.dir
	st, err := readState(dir)
	f.ok(err)
	f.ok(f.Close())
	st.Epoch += 5
	f.ok(writeState(dir, st))
	s, e := Open(dir, Options{})
	f.ok(e)
	f.Store = s
	if f.marked() != 0 {
		t.Fatalf("%d links marked by an epoch gap", f.marked())
	}
}

func TestRestorePendingSentinelMarksThenIsRemoved(t *testing.T) {
	f, _, _ := restoreLinks(t)
	dir := f.Store.dir
	f.ok(f.Close())
	sentinel := filepath.Join(dir, restorePendingFile)
	f.ok(os.WriteFile(sentinel, nil, 0o600))
	// a crash between the mark and the removal: the removal fails, the next start marks again and removes it
	removeFn = func(p string) error {
		if p == sentinel {
			return errCrash
		}
		return os.Remove(p)
	}
	defer func() { removeFn = os.Remove }()
	if _, err := Open(dir, Options{}); err == nil {
		t.Fatal("the failing removal did not stop the start")
	}
	removeFn = os.Remove
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatal("the sentinel is gone although it could not be removed")
	}
	s, e := Open(dir, Options{})
	f.ok(e)
	f.Store = s
	if f.marked() != 3 {
		t.Fatalf("%d marked", f.marked())
	}
	if _, err := os.Stat(sentinel); !os.IsNotExist(err) {
		t.Fatal("the sentinel was not removed")
	}
	f.reopen()
	if f.marked() != 3 {
		t.Fatal("marks lost after the sentinel was consumed")
	}
}

func TestAfterRestoreOverrideClearsOneLinkAndIsAudited(t *testing.T) {
	f, fk, ek := restoreLinks(t)
	f.markAll()
	if err := f.ClearRestoreMark(ctx, fk, 0, ""); err == nil || !strings.Contains(err.Error(), "name one") {
		t.Fatalf("two marked links of a project must need a name: %v", err)
	}
	if err := f.ClearRestoreMark(ctx, 0, 0, "hub"); err == nil {
		t.Fatal("no project named")
	}
	f.ok(f.ClearRestoreMark(ctx, fk, 0, "hub"))
	ls, _ := f.UpstreamLinks(ctx, 0, 0)
	for _, l := range ls {
		want := !(l.Project == fk && l.Upstream == "hub")
		if (l.Restored != nil) != want {
			t.Fatalf("link %d/%s marked=%v, want %v", l.Project, l.Upstream, l.Restored != nil, want)
		}
	}
	if err := f.ClearRestoreMark(ctx, fk, 0, "hub"); err == nil {
		t.Fatal("an unmarked link has nothing to clear")
	}
	var who, subject, detail string
	f.ok(f.DB().QueryRow(`SELECT actor,subject,detail FROM audit_events WHERE action='upstream.after-restore'`).Scan(&who, &subject, &detail))
	if who != adminActor || subject != "hub" || strings.Contains(detail, "Structure") {
		t.Fatalf("audit: %s %s %s", who, subject, detail)
	}
	res, err := f.PushUpstream(ctx, okUp(), fk, 0, "hub", 0, false, noSleep)
	f.ok(err)
	if res[0].Blocked || res[0].Created == 0 {
		t.Fatalf("the cleared link does not push: %+v", res[0])
	}
	if l := f.link(ek); l.Restored == nil {
		t.Fatal("another project's mark was cleared")
	}
}

// A sentinel path that cannot be checked (here: the "folder" is a file, so Lstat answers ENOTDIR) is not "no sentinel": it is an error.
func TestRestorePendingThatCannotBeCheckedFailsClosed(t *testing.T) {
	file := filepath.Join(t.TempDir(), "not-a-folder")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := (&Store{dir: file}).consumeRestorePending(ctx); err == nil {
		t.Fatal("an unreadable sentinel path was taken as none")
	}
	if err := (&Store{dir: t.TempDir()}).consumeRestorePending(ctx); err != nil {
		t.Fatalf("no sentinel is not an error: %v", err)
	}
}
