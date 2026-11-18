package core

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A restore brings back the mirror as it was, also after the hub revoked this gateway: it stays hidden from members (as a stale mirror
// is, nothing deleted) until a pull completes after the restore.
func TestRestoreHidesTheMirrorUntilAPullCompletes(t *testing.T) {
	f, fk := readFx(t)
	h := &hub{notes: []hubNote{structNote(1, "Orbit plan")}}
	pull(f, fk, h.up())
	if len(f.search("ana", fk, "orbit")) != 1 {
		t.Fatal("the mirror is not readable before the restore")
	}
	bak := filepath.Join(t.TempDir(), "b.db")
	f.ok(f.Backup(ctx, bak))
	dir, clock := f.Dir(), f.clock
	f.ok(f.Close())
	if err := Restore(dir, bak); err != nil {
		t.Fatal(err)
	}
	f.Store = openT(t, dir)
	f.Store.now = func() time.Time { return f.clock }
	f.clock = clock
	if ids := f.search("ana", fk, "orbit"); len(ids) != 0 {
		t.Fatal("a restored mirror is readable by members before a pull", ids)
	}
	if l := linkOf(f, fk); !l.Stale || l.Mirror != 1 || !strings.Contains(l.PullError, "restored: waiting for a pull") {
		t.Fatalf("hidden, not deleted, and said so: %+v", l)
	}
	pull(f, fk, h.up())
	if len(f.search("ana", fk, "orbit")) != 1 || linkOf(f, fk).Stale {
		t.Fatal("not readable after a completed pull")
	}
}

// A backup taken in the middle of a sliced pull holds a cursor: after the restore the first pull is a full traversal, so a note the hub changed
// below that cursor is not skipped and the old body is gone before the mirror is shown again.
func TestRestoreResetsTheTraversalCursor(t *testing.T) {
	old := maxPullFetch
	maxPullFetch = 2
	defer func() { maxPullFetch = old }()
	f, fk := readFx(t)
	h := &hub{}
	for i := int64(1); i <= 4; i++ {
		h.notes = append(h.notes, structNote(i, fmt.Sprint("Plan", i)))
	}
	up := h.up()
	if res := pull(f, fk, up); !res.More || res.Next != 2 {
		t.Fatalf("%+v", res)
	}
	bak := filepath.Join(t.TempDir(), "b.db")
	f.ok(f.Backup(ctx, bak))
	h.notes[0].rev, h.notes[0].body = 2, "redacted"
	dir, clock := f.Dir(), f.clock
	f.ok(f.Close())
	if err := Restore(dir, bak); err != nil {
		t.Fatal(err)
	}
	f.Store = openT(t, dir)
	f.Store.now = func() time.Time { return f.clock }
	f.clock = clock
	var cur int64
	f.ok(f.DB().QueryRow(`SELECT pull_cursor FROM upstream_links`).Scan(&cur))
	if cur != 0 {
		t.Fatal("a restored cursor", cur)
	}
	for i := 0; i < 5; i++ {
		res := pull(f, fk, up)
		if res.More {
			if l := linkOf(f, fk); !l.Stale {
				t.Fatal("readable before the traversal completes")
			}
			continue
		}
		break
	}
	var body string
	f.ok(f.DB().QueryRow(`SELECT body FROM upstream_mirror WHERE remote_note=1`).Scan(&body))
	if body != "redacted" {
		t.Fatal("the old body is still in the mirror:", body)
	}
}
