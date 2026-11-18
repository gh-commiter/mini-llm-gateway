package main

import (
	"fmt"
	"strings"
	"testing"

	"minigw/core"
)

// loopGw: one real gateway of this binary on loopback with project shop, for the tests of notes that come from another gateway.
type loopGw struct {
	t    *testing.T
	s    *server
	adm  *adminClient
	shop int64
}

func newLoopGw(t *testing.T) *loopGw {
	t.Helper()
	g := &loopGw{t: t}
	g.s, _, _ = coreLiveServer(t)
	g.adm = &adminClient{off: g.s}
	p, err := g.s.core.CreateProject(t0, "shop", "Shop")
	if err != nil {
		t.Fatal(err)
	}
	g.shop = p.ID
	return g
}

func (g *loopGw) cmd(o coreOpts, args ...string) string {
	g.t.Helper()
	out, errText, exit := captured(func() { coreCommand(g.adm, append([]string{"upstream"}, args...), o) })
	if exit != 0 {
		g.t.Fatalf("upstream %v: %s %s", args, out, errText)
	}
	return out
}

// member: a user bound to device dev with role developer in shop; returns its principal.
func (g *loopGw) member(name, dev string) core.Principal {
	g.t.Helper()
	c := g.s.core
	u, err := c.CreateUser(t0, name)
	if err != nil {
		g.t.Fatal(err)
	}
	if err = c.SetBinding(t0, dev, u.ID); err != nil {
		g.t.Fatal(err)
	}
	if err = c.SetMember(t0, g.shop, u.ID, core.Developer); err != nil {
		g.t.Fatal(err)
	}
	p, err := c.Principal(t0, dev, true)
	if err != nil {
		g.t.Fatal(err)
	}
	return p
}

// linkTo: g adds `to` as upstream `name` and links its shop to to's shop in `mode`; to's owner allows g's device and binds it to a
// developer of shop called user.
func (g *loopGw) linkTo(to *loopGw, name, user, mode string) {
	g.t.Helper()
	addr := fmt.Sprintf("127.0.0.1:%d", to.s.cfg.Port)
	g.cmd(coreOpts{in: strings.NewReader(connectToken(to.s.cfg) + "\n")}, "add", name, addr)
	dev := upstreamHost{g.s}.Info(name).Device
	if err := to.s.update(func(c *serveConfig) error {
		c.Devices = append(c.Devices, serveDevice{ID: dev, Name: name})
		return nil
	}); err != nil {
		g.t.Fatal(err)
	}
	to.member(user, dev)
	g.cmd(coreOpts{mode: mode}, "link", "shop", name)
}

// notes: the notes of shop, and how many of them carry the text of the note titled "Invoice numbering" written on the first gateway.
func (g *loopGw) notes() (n, copies int) {
	g.t.Helper()
	if err := g.s.core.DB().QueryRow(`SELECT count(*), coalesce(sum(body LIKE 'text of Invoice numbering%'),0) FROM notes WHERE project_id=?`, g.shop).Scan(&n, &copies); err != nil {
		g.t.Fatal(err)
	}
	return
}

// acceptAll: the owner accepts every pending submission (submit mode: what a reviewer would do).
func (g *loopGw) acceptAll() {
	g.t.Helper()
	rows, err := g.s.core.DB().Query(`SELECT id FROM kb_submissions WHERE state='pending'`)
	if err != nil {
		g.t.Fatal(err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		if _, err := g.s.core.ReviewSubmission(t0, id, true, "", seenHash(g.s.core, id)); err != nil {
			g.t.Fatal(err)
		}
	}
}

// M1: two gateways that push to each other (both publish, or both submit with every submission accepted) do not copy each other's
// notes back and forth: after several rounds of push on both sides each side has the original and one copy, stable.
func TestUpstreamMutualLinksDoNotLoop(t *testing.T) {
	for _, mode := range []string{"publish", "submit"} {
		t.Run(mode, func(t *testing.T) {
			fakeKeys(t)
			a, b := newLoopGw(t), newLoopGw(t)
			ana := a.member("ana", "dev-ana")
			a.linkTo(b, "to-b", "gw-a", mode)
			b.linkTo(a, "to-a", "gw-b", mode)
			if _, _, err := a.s.core.CreateNote(t0, ana, a.shop, "n1", core.NoteInput{Class: core.Structure, Title: "Invoice numbering", Body: "text of Invoice numbering"}); err != nil {
				t.Fatal(err)
			}
			var counts []string
			for round := 1; round <= 4; round++ {
				a.cmd(coreOpts{}, "push", "shop")
				b.acceptAll()
				b.cmd(coreOpts{}, "push", "shop")
				a.acceptAll()
				na, _ := a.notes()
				nb, _ := b.notes()
				counts = append(counts, fmt.Sprintf("round %d: A %d, B %d", round, na, nb))
			}
			t.Log(strings.Join(counts, "; "))
			if na, ca := a.notes(); na != 1 || ca != 1 {
				t.Fatalf("A: %d notes (%d with the text): %s", na, ca, strings.Join(counts, "; "))
			}
			if nb, cb := b.notes(); nb != 1 || cb != 1 {
				t.Fatalf("B: %d notes (%d with the text): %s", nb, cb, strings.Join(counts, "; "))
			}
			// the owner sees why the copy on B is not sent: in status and in the push report
			if out := b.cmd(coreOpts{}, "status", "shop"); !strings.Contains(out, "0 of 0 notes would go") || !strings.Contains(out, "1 note not sent: arrived from another gateway") {
				t.Fatal(out)
			}
			if out := b.cmd(coreOpts{}, "push", "shop"); !strings.Contains(out, "arrived from another gateway, never pushed onward") {
				t.Fatal(out)
			}
		})
	}
}

// M1, the relay: A pushes to B, B pushes to C: A's note (and its author's name) never reaches C, in either mode; a note of B's own
// member does.
func TestUpstreamNoteFromAnotherGatewayIsNotRelayed(t *testing.T) {
	for _, mode := range []string{"publish", "submit"} {
		t.Run(mode, func(t *testing.T) {
			fakeKeys(t)
			a, b, c := newLoopGw(t), newLoopGw(t), newLoopGw(t)
			ana := a.member("ana", "dev-ana")
			bob := b.member("bob", "dev-bob")
			a.linkTo(b, "to-b", "gw-a", mode)
			b.linkTo(c, "to-c", "gw-b", mode)
			if _, _, err := a.s.core.CreateNote(t0, ana, a.shop, "n1", core.NoteInput{Class: core.Structure, Title: "Invoice numbering", Body: "text of Invoice numbering"}); err != nil {
				t.Fatal(err)
			}
			if _, _, err := b.s.core.CreateNote(t0, bob, b.shop, "n1", core.NoteInput{Class: core.Structure, Title: "Bob's note", Body: "bob's own text"}); err != nil {
				t.Fatal(err)
			}
			for round := 0; round < 3; round++ {
				a.cmd(coreOpts{}, "push", "shop")
				b.acceptAll()
				b.cmd(coreOpts{}, "push", "shop")
				c.acceptAll()
			}
			if _, cb := b.notes(); cb != 1 {
				t.Fatalf("B has %d copies", cb)
			}
			var n, relayed, subs int
			if err := c.s.core.DB().QueryRow(`SELECT count(*), coalesce(sum(body LIKE '%Invoice numbering%' OR body LIKE '%author ana%'),0) FROM notes`).Scan(&n, &relayed); err != nil {
				t.Fatal(err)
			}
			if err := c.s.core.DB().QueryRow(`SELECT count(*) FROM kb_submissions WHERE body LIKE '%Invoice numbering%' OR body LIKE '%author ana%'`).Scan(&subs); err != nil {
				t.Fatal(err)
			}
			if n != 1 || relayed != 0 || subs != 0 {
				t.Fatalf("C: %d notes, %d relayed, %d relayed submissions", n, relayed, subs)
			}
		})
	}
}

// A local member's edit of a copy that arrived from another gateway: while the copy still ends with the other gateway's trailer it
// stays unsent; an edit that leaves the trailer out is local work and goes, to the gateway it came from too (a NEW note there, which
// ends with our trailer and so never comes back). Counts stay stable.
func TestUpstreamEditedCopyFromAnotherGateway(t *testing.T) {
	fakeKeys(t)
	a, b := newLoopGw(t), newLoopGw(t)
	ana := a.member("ana", "dev-ana")
	bob := b.member("bob", "dev-bob")
	if err := b.s.core.SetMember(t0, b.shop, bob.UserID, core.Admin); err != nil { // an admin may change another member's note
		t.Fatal(err)
	}
	a.linkTo(b, "to-b", "gw-a", "publish")
	b.linkTo(a, "to-a", "gw-b", "publish")
	if _, _, err := a.s.core.CreateNote(t0, ana, a.shop, "n1", core.NoteInput{Class: core.Structure, Title: "Invoice numbering", Body: "text of Invoice numbering"}); err != nil {
		t.Fatal(err)
	}
	a.cmd(coreOpts{}, "push", "shop")
	var id, rev int64
	var body string
	if err := b.s.core.DB().QueryRow(`SELECT id, revision, body FROM notes WHERE project_id=?`, b.shop).Scan(&id, &rev, &body); err != nil {
		t.Fatal(err)
	}
	// bob changes the first line and keeps the trailer: still the other gateway's text
	cur, err := b.s.core.UpdateNote(t0, bob, b.shop, id, rev, core.NoteInput{Class: core.Structure, Title: "Invoice numbering", Body: "text of Invoice numbering, bob's addition" + body[len("text of Invoice numbering"):]})
	if err != nil {
		t.Fatal(err)
	}
	if out := b.cmd(coreOpts{}, "push", "shop"); !strings.Contains(out, "created 0") {
		t.Fatal(out)
	}
	// bob removes the trailer: his text now, it goes (to A, as a new note there)
	if _, err = b.s.core.UpdateNote(t0, bob, b.shop, id, cur.Revision, core.NoteInput{Class: core.Structure, Title: "Invoice numbering", Body: "text of Invoice numbering, bob's version"}); err != nil {
		t.Fatal(err)
	}
	for round := 0; round < 3; round++ {
		b.cmd(coreOpts{}, "push", "shop")
		a.cmd(coreOpts{}, "push", "shop")
	}
	if na, _ := a.notes(); na != 2 {
		t.Fatalf("A: %d notes", na)
	}
	if nb, _ := b.notes(); nb != 1 {
		t.Fatalf("B: %d notes", nb)
	}
}
