package core

import (
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
