package main

// minigw serve: mape odjemalcev le v načinu sync (kot ShareHub v RouterCore/Shares.swift). minigw sprašuje po poslih
// (long poll); SYNC-PUSH pred agentom pošlje manifest in datoteke v kopijo, SYNC-PULL po agentu prevzame spremembe.
// Kopija je v scratch/sync/<lastnik>/<mapa>; do nje pride le lastnik, ki je mapo registriral (naprava ali »local«).

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

type serveShare struct {
	session, key, cwd, copy, owner string
	readOnly                       bool
	jobs                           chan davJob
	replies                        map[string]chan davResponse
	done                           chan struct{} // zaprt ob odjavi: zbudi čakajoče
	lastSeen                       time.Time
	state                          map[string]syncEntry  // kopija, kot jo ima odjemalec; nil = še ni prenosa
	incoming                       map[string]syncEntry  // zadnji manifest odjemalca (preverjanje prejetih datotek)
	pending                        map[string]*syncEntry // poslane spremembe agenta (nil = izbris) do potrditve
	big                            map[string]bool       // že javljene prevelike datoteke
	execKnown                      bool                  // odjemalec pozna bit x (ne Windows)
	tree                           *shareTree            // worktree: zrcalo in kloni agentov (serve_worktree.go); nil = sync
	capNoted                       map[string]bool       // L4: agenti, ki jim je opomba o omejitvi #45 že poslana
}

// firstCapNote (L4): opomba o omejitvi agenta enkrat na sejo (mapo) in agenta.
func (h *shareHub) firstCapNote(session, kind string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.bySession[session]
	if s == nil || s.capNoted[kind] {
		return false
	}
	if s.capNoted == nil {
		s.capNoted = map[string]bool{}
	}
	s.capNoted[kind] = true
	return true
}

type shareHub struct {
	mu        sync.Mutex
	bySession map[string]*serveShare
	busy      map[string]chan struct{} // ena zahteva agenta naenkrat na kopijo
	ops       map[string]int           // Clean(copy) → operations working in that copy outside h.mu; kept after unregister (ifCopyFree)
	refused   map[string]bool          // seje, ki so želele način direct
	reaper    sync.Once
	expired   time.Time            // zadnje čiščenje starih kopij (#1, #9)
	logf      func(string, ...any) // dnevnik prehoda (worktree: zakaj veje ni)
	// nestedBlocker (N1): zakaj agent s temi ugnezdenimi repozitoriji tu ne sme začeti ("" = sme); nil = vedno sme.
	nestedBlocker func(agent string) string
	// cloneMaxFiles: files a returned clone may hold (0 = maxSyncFiles); only a test sets it, to reach the limit without writing 100000 files.
	cloneMaxFiles int
}

// #1, #9: meje kopije (manifest odjemalca) in življenjska doba neuporabljenih kopij.
const (
	maxSyncFiles = 100_000
	maxSyncBytes = int64(8) << 30
	copyLifetime = 14 * 24 * time.Hour
	maxCopies    = 32 // M2: kopij na napravo (vsaka do maxSyncBytes)
)

// tooManyCopies: nova kopija (sync/<naprava>/<mapa>) bi presegla maxCopies; obstoječa sme vedno naprej.
func tooManyCopies(copyDir string) bool {
	if _, err := os.Lstat(copyDir); err == nil {
		return false
	}
	ents, _ := os.ReadDir(filepath.Dir(copyDir))
	n := 0
	for _, e := range ents {
		if e.IsDir() && validSyncCopyID(e.Name()) { // a leftover "<id>.removing-<ns>" (a removal that crashed) is not a copy
			n++
		}
	}
	return n >= maxCopies
}

// syncOverQuota: "" = v mejah, sicer sporočilo za odjemalca.
func syncOverQuota(files []syncEntry) string {
	var bytes int64
	for _, f := range files {
		bytes += max(0, f.Size)
	}
	if len(files) <= maxSyncFiles && bytes <= maxSyncBytes {
		return ""
	}
	return fmt.Sprintf("The folder is too large to sync (%d files, %d MB; the limit is %d files and %d GB): "+
		"add build output and large data to .gitignore, or use a smaller folder", len(files), bytes>>20, maxSyncFiles, maxSyncBytes>>30)
}

// touchUsed: oznaka rabe <kopija>.used ob kopiji (v kopiji bi se sinhronizirala nazaj).
func touchUsed(copyDir string) {
	m := copyDir + ".used"
	if f, err := os.Create(m); err == nil {
		f.Close()
	}
}

// expireCopies: največ enkrat na dan izbriše kopije (sync/<naprava>/<mapa>), ki jih 14 dni ni uporabila nobena seja.
func (h *shareHub) expireCopies(root string, now time.Time) {
	h.mu.Lock()
	if now.Sub(h.expired) < 24*time.Hour {
		h.mu.Unlock()
		return
	}
	h.expired = now
	active := map[string]bool{}
	for _, s := range h.bySession {
		active[filepath.Clean(s.copy)] = true
	}
	h.mu.Unlock()
	removeOldCopies(root, active, now)
	pruneMirrors(filepath.Dir(root), now) // worktree: zrcala, ki jih 30 dni ni uporabil noben agent
}

func removeOldCopies(root string, active map[string]bool, now time.Time) {
	rooted, err := os.OpenRoot(root)
	if err != nil {
		return
	}
	defer rooted.Close()
	removeOldCopiesAt(rooted, root, active, now)
}

func removeOldCopiesAt(rooted *os.Root, root string, active map[string]bool, now time.Time) {
	owners, _ := rootReadDir(rooted, ".")
	for _, o := range owners {
		if !o.IsDir() || o.Name() == "runs" {
			continue
		}
		copies, _ := rootReadDir(rooted, o.Name())
		for _, c := range copies {
			rel := filepath.Join(o.Name(), c.Name())
			p := filepath.Join(root, rel)
			if !c.IsDir() || active[filepath.Clean(p)] { // c.IsDir: povezava ni mapa
				continue
			}
			used := now
			if st, err := rooted.Stat(rel + ".used"); err == nil {
				used = st.ModTime()
			} else if st, err := rooted.Lstat(rel); err == nil {
				used = st.ModTime()
			}
			if now.Sub(used) > copyLifetime {
				rooted.RemoveAll(rel)
				rooted.Remove(rel + ".used")
			}
		}
	}
}

func newShareHub() *shareHub {
	return &shareHub{bySession: map[string]*serveShare{}, busy: map[string]chan struct{}{}, ops: map[string]int{}, refused: map[string]bool{}, logf: func(string, ...any) {}}
}

func newKey() string {
	b := make([]byte, 16)
	rand.Read(b)
	return fmt.Sprintf("%x", b)
}

func (h *shareHub) byKey(key string) *serveShare {
	for _, s := range h.bySession {
		if s.key == key && key != "" {
			return s
		}
	}
	return nil
}

// share: le za lastnika, ki je mapo registriral (drug z istim ključem je ne doseže; "" nikoli). Klicati pod h.mu.
func (h *shareHub) share(key, owner string) *serveShare {
	if s := h.byKey(key); s != nil && owner != "" && s.owner == owner {
		return s
	}
	return nil
}

func (h *shareHub) refuse(session string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.refused) > 2000 { // ponytail: grobo čiščenje
		h.refused = map[string]bool{}
	}
	h.refused[session] = true
}

func (h *shareHub) isRefused(session string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.refused[session]
}

// register: false, če ima seja že mapo drugega lastnika (brez njegovega ključa je ni mogoče prevzeti).
// count: registrirane seje (premik Scratch le brez njih, kot na Macu).
func (h *shareHub) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.bySession)
}

func (h *shareHub) register(session, cwd, key string, readOnly bool, copy, owner string) (string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if s := h.bySession[session]; s != nil {
		if s.key != key || s.owner != owner {
			return "", false
		}
		if s.cwd == cwd && s.readOnly == readOnly && s.copy == copy {
			s.lastSeen = time.Now()
			return s.key, true
		}
		h.close(s)
	}
	delete(h.refused, session)
	s := &serveShare{session: session, key: newKey(), cwd: cwd, copy: copy, owner: owner, readOnly: readOnly,
		jobs: make(chan davJob, 64), replies: map[string]chan davResponse{}, done: make(chan struct{}),
		lastSeen: time.Now(), big: map[string]bool{}, execKnown: true}
	h.bySession[session] = s
	h.reaper.Do(func() {
		go func() {
			for range time.Tick(15 * time.Second) {
				h.reap(60 * time.Second)
			}
		}()
	})
	return s.key, true
}

// reap: minigw ni več spraševal (zaprt prenosnik, prekinjen VPN) → odjava.
func (h *shareHub) reap(after time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, s := range h.bySession {
		if time.Since(s.lastSeen) > after {
			h.close(s)
		}
	}
}

// registerTree: kot register, a mapa seje je repozitorij git (worktree); copy je mapa klonov (zaklep na repozitorij).
func (h *shareHub) registerTree(session, cwd, key string, t *shareTree, owner string) (string, bool) {
	k, ok := h.register(session, cwd, key, false, t.worktrees, owner)
	if ok {
		h.mu.Lock()
		if s := h.bySession[session]; s != nil && s.tree == nil {
			s.tree = t
		}
		h.mu.Unlock()
	}
	return k, ok
}

// close: pod h.mu.
func (h *shareHub) close(s *serveShare) {
	if s.tree != nil {
		dropClones(s.tree, h.logf)
	}
	delete(h.bySession, s.session)
	close(s.done)
}

func (h *shareHub) unregister(key, owner string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if s := h.share(key, owner); s != nil {
		h.close(s)
	}
}

// poll: naslednji posel ali nil po 25 s; false, če mape ni (→ 404, minigw se registrira znova).
func (h *shareHub) poll(ctx context.Context, key, owner string) (*davJob, bool) {
	h.mu.Lock()
	s := h.share(key, owner)
	if s != nil {
		s.lastSeen = time.Now()
	}
	h.mu.Unlock()
	if s == nil {
		return nil, false
	}
	defer func() { h.mu.Lock(); s.lastSeen = time.Now(); h.mu.Unlock() }()
	select {
	case j := <-s.jobs:
		return &j, true
	case <-s.done:
	case <-ctx.Done():
	case <-time.After(25 * time.Second):
	}
	return nil, true
}

func (h *shareHub) reply(key, owner string, r davReply) bool {
	h.mu.Lock()
	s := h.share(key, owner)
	var ch chan davResponse
	if s != nil {
		s.lastSeen = time.Now()
		ch = s.replies[r.ID]
		delete(s.replies, r.ID)
	}
	h.mu.Unlock()
	if ch != nil {
		ch <- r.Response
	}
	return s != nil
}

// send: posel za minigw (naslednji long poll); nil, če ne odgovori v timeout.
func (h *shareHub) send(s *serveShare, method string, timeout time.Duration) *davResponse {
	return h.sendJob(s, method, []byte{}, timeout) // macOS minigw ne sprejme null
}

func (h *shareHub) sendJob(s *serveShare, method string, body []byte, timeout time.Duration) *davResponse {
	job := davJob{ID: newKey(), Method: method, Headers: map[string]string{}, Body: body}
	ch := make(chan davResponse, 1)
	h.mu.Lock()
	s.replies[job.ID] = ch
	h.mu.Unlock()
	defer func() { h.mu.Lock(); delete(s.replies, job.ID); h.mu.Unlock() }()
	select {
	case s.jobs <- job:
	default:
		return nil
	}
	select {
	case r := <-ch:
		return &r
	case <-s.done:
	case <-time.After(timeout):
	}
	return nil
}

// waitShare: mapa seje (le lastnika, ki jo je registriral). wait: po ponovnem zagonu routerja počaka do 30 s,
// da jo minigw znova registrira; ne čaka na seje, zavrnjene zaradi načina direct.
func (h *shareHub) waitShare(ctx context.Context, session, owner string, wait bool) *serveShare {
	for i := 0; ; i++ {
		h.mu.Lock()
		s, refused := h.bySession[session], h.refused[session]
		h.mu.Unlock()
		if s != nil {
			if owner == "" || s.owner != owner {
				return nil
			}
			return s
		}
		if !wait || refused || session == "" || i >= 60 || ctx.Err() != nil {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// lock: ena zahteva agenta naenkrat na kopijo (prenos → agent → prevzem); "" = seja nima kopije. Napaka = prekinjeno.
func (h *shareHub) lock(ctx context.Context, session, owner string, wait bool) (string, error) {
	s := h.waitShare(ctx, session, owner, wait)
	if s == nil {
		return "", ctx.Err()
	}
	h.mu.Lock()
	sem := h.busy[s.copy]
	if sem == nil {
		sem = make(chan struct{}, 1)
		h.busy[s.copy] = sem
	}
	h.mu.Unlock()
	select {
	case sem <- struct{}{}:
		return s.copy, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (h *shareHub) unlock(copy string) {
	h.mu.Lock()
	sem := h.busy[copy]
	h.mu.Unlock()
	<-sem
}

// push: minigw naj posodobi kopijo (SYNC-PUSH); vrne kopijo ali "" (refused: worktreeMount). Prvi prenos velike mape traja.
// worktree: klon agenta (nov iz posnetka odjemalca ali od prejšnje zahteve te seje).
func (h *shareHub) push(s *serveShare, agent string) (dir, refused string) {
	if s.tree != nil {
		return h.worktreeMount(s, agent)
	}
	if r := h.send(s, "SYNC-PUSH", 900*time.Second); r != nil && r.Status == 200 {
		return s.copy, ""
	}
	return "", ""
}

// syncBack: po agentu minigw prevzame spremembe kopije (ne pri --minigw-ro), pred koncem odgovora. Vrne, zakaj dela
// agenta ni dobil (#82: zahteva se konča z napako, Claude Code ga ne javi kot dostavljenega); "" = dobil ali nič.
func (h *shareHub) syncBack(session, agent string) string {
	h.mu.Lock()
	s := h.bySession[session]
	h.mu.Unlock()
	if s != nil && s.tree != nil {
		return h.worktreeBack(s, agent)
	} else if s != nil && !s.readOnly {
		if why := pullFailure(h.send(s, "SYNC-PULL", 900*time.Second)); why != "" {
			h.logf("sync (%s): the agent's changes were not copied back: %s", s.session, why)
			return "the agent's changes were not copied back to your folder: " + why +
				"; they are kept on the gateway and only the next agent request in this folder copies them back"
		}
	}
	return ""
}

// pullFailure (#82): zakaj minigw dela agenta ni prevzel ("" = prevzel).
func pullFailure(r *davResponse) string {
	switch {
	case r == nil:
		return "minigw did not answer in time"
	case r.Status == 503:
		return "the shared folder is busy: another minigw session on this folder is using it"
	case r.Status != 200:
		return fmt.Sprintf("minigw could not take them (HTTP %d; minigw printed why)", r.Status)
	}
	return ""
}

// isGitPath: ».git« v kateri koli obliki (».GIT«, ».git.«, »git~1«): nikoli v kopiji ali iz nje.
func isGitPath(rel string) bool {
	for _, c := range strings.Split(rel, "/") {
		if normName(c) == ".git" {
			return true
		}
	}
	return false
}

// syncDiff: kaj je agent spremenil v kopiji (cur) glede na stanje odjemalca (old). Izbris le, če na poti ni ničesar:
// na novo prezrta ali prevelika datoteka ni izbrisana.
func syncDiff(old, cur map[string]syncEntry, root string) syncChanges {
	ch := syncChanges{Changed: []syncEntry{}, Deleted: []string{}, Skipped: []string{}}
	rooted, err := os.OpenRoot(root)
	if err == nil {
		defer rooted.Close()
	}
	for p, e := range cur {
		if o, ok := old[p]; !ok || o.SHA256 != e.SHA256 || o.Exec != e.Exec {
			ch.Changed = append(ch.Changed, e)
		}
	}
	for p := range old {
		if _, ok := cur[p]; ok {
			continue
		}
		if _, ok := syncResolve(p, root); ok && rooted != nil {
			if _, err := rooted.Lstat(filepath.FromSlash(p)); err == nil {
				continue
			}
		}
		ch.Deleted = append(ch.Deleted, p)
	}
	sort.Slice(ch.Changed, func(i, j int) bool { return ch.Changed[i].Path < ch.Changed[j].Path })
	sort.Strings(ch.Deleted)
	return ch
}

// opStart counts an operation that touches the files of s's copy outside h.mu; call with h.mu held, in the same locked section
// that found the session. The hold outlives an unregister of the session; the returned func (takes h.mu) ends it.
func (h *shareHub) opStart(s *serveShare) func() {
	c := filepath.Clean(s.copy)
	h.ops[c]++
	return func() {
		h.mu.Lock()
		if h.ops[c]--; h.ops[c] <= 0 {
			delete(h.ops, c)
		}
		h.mu.Unlock()
	}
}

// syncManifest: manifest odjemalca → poti, ki jih kopija potrebuje; datoteke, ki jih odjemalec nima več, izbriše.
// Še nepotrjenih sprememb agenta (kept) ne prepiše in ne izbriše; gredo z naslednjim prevzemom.
func (h *shareHub) syncManifest(key string, files []syncEntry, execKnown bool, owner string) (need, kept []string, ok bool) {
	h.mu.Lock()
	s := h.share(key, owner)
	if s == nil {
		h.mu.Unlock()
		return nil, nil, false
	}
	s.lastSeen = time.Now()
	s.execKnown = execKnown
	touchUsed(s.copy)
	prev := s.state
	end := h.opStart(s)
	h.mu.Unlock()
	defer end()

	if err := os.MkdirAll(s.copy, 0o700); err != nil {
		return nil, nil, false
	}
	rooted, err := os.OpenRoot(s.copy)
	if err != nil {
		return nil, nil, false
	}
	defer rooted.Close()
	current, _ := syncScan(s.copy, prev, false) // zunaj zaklepa: velika mapa traja
	// Prvi prenos te registracije: stanje ni znano → kopija velja za usklajeno. Pri --minigw-ro je kopija zavrženo delo.
	base := prev
	if s.readOnly || base == nil {
		base = current
	}
	agent := syncDiff(base, current, s.copy)
	keep := map[string]bool{}
	for _, e := range agent.Changed {
		keep[e.Path] = true
	}
	for _, p := range agent.Deleted {
		keep[p] = true
	}
	incoming := map[string]syncEntry{}
	for _, e := range files {
		if _, ok := syncResolve(e.Path, s.copy); ok && !isGitPath(e.Path) {
			incoming[e.Path] = e
		}
	}
	need, kept = []string{}, []string{}
	for p, e := range incoming {
		if c, ok := current[p]; (!ok || c.SHA256 != e.SHA256) && !keep[p] {
			need = append(need, p)
		}
	}
	state := current
	for p := range keep {
		kept = append(kept, p)
		if e, ok := base[p]; ok {
			state[p] = e
		} else {
			delete(state, p)
		}
	}
	for p := range current {
		if _, listed := incoming[p]; !listed && !keep[p] {
			if _, ok := syncResolve(p, s.copy); ok {
				rooted.Remove(filepath.FromSlash(p))
			}
			delete(state, p)
		}
	}
	sort.Strings(need)
	sort.Strings(kept)
	h.mu.Lock()
	s.state, s.incoming = state, incoming
	h.mu.Unlock()
	return need, kept, true
}

// syncPut: datoteka iz manifesta; vsebina se mora ujemati z njim. Status HTTP.
var syncPutWrite = syncWriteCopy // testi: počasno pisanje

// Pisanje (do 64 MB) teče brez h.mu (#21): počasen prenos ne ustavi pollov in odgovorov drugih map.
func (h *shareHub) syncPut(key, path string, data []byte, owner string) int {
	h.mu.Lock()
	s := h.share(key, owner)
	if s == nil {
		h.mu.Unlock()
		return 404
	}
	s.lastSeen = time.Now()
	e, listed := s.incoming[path]
	copyDir, execKnown := s.copy, s.execKnown
	end := h.opStart(s)
	h.mu.Unlock()
	defer end()
	_, ok := syncResolve(path, copyDir)
	if !listed || !ok {
		return 403
	}
	want := e.SHA256
	if int64(len(data)) != e.Size || sha256Hex(data) != want { // M2: kvota šteje velikost iz manifesta
		return 409 // spremenjena med prenosom
	}
	rooted, err := os.OpenRoot(copyDir)
	if err != nil {
		return 500
	}
	defer rooted.Close()
	rel, err := rootResolve(rooted, filepath.FromSlash(path), false)
	if err != nil {
		return 403
	}
	if syncPutWrite(rooted, rel, data, e.Mtime, e.Exec, !execKnown) != nil {
		return 500
	}
	if !execKnown { // Windows: dejanski način kopije
		if a, ok := syncEntryOf(rooted, rel, path, nil); ok {
			e = a
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.share(key, owner) != s { // med pisanjem odjavljena ali zamenjana
		return 404
	}
	if cur, still := s.incoming[path]; !still || cur.SHA256 != want {
		return 409 // nov manifest med pisanjem: odjemalec pošlje znova
	}
	if s.state != nil {
		s.state[path] = e
	}
	return 204
}

// syncChanges: spremembe agenta od zadnjega prenosa; stanje se premakne šele s potrditvijo (syncAck).
func (h *shareHub) syncChanges(key, owner string) (syncChanges, bool) {
	h.mu.Lock()
	s := h.share(key, owner)
	if s == nil || s.readOnly || s.state == nil {
		h.mu.Unlock()
		return syncChanges{}, false
	}
	s.lastSeen = time.Now()
	prev := s.state
	end := h.opStart(s)
	h.mu.Unlock()
	defer end()

	current, skipped := syncScan(s.copy, prev, false)
	ch := syncDiff(prev, current, s.copy)
	h.mu.Lock()
	defer h.mu.Unlock()
	big := map[string]bool{}
	for _, p := range skipped {
		if !s.big[p] {
			ch.Skipped = append(ch.Skipped, p)
		}
		big[p] = true
	}
	s.big = big
	s.pending = map[string]*syncEntry{}
	for i := range ch.Changed {
		s.pending[ch.Changed[i].Path] = &ch.Changed[i]
	}
	for _, p := range ch.Deleted {
		s.pending[p] = nil
	}
	return ch, true
}

// syncAck: minigw potrdi poti, ki jih je obdelal (zapisal, izbrisal ali shranil kot spor).
func (h *shareHub) syncAck(key string, paths []string, owner string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.share(key, owner)
	if s == nil || s.state == nil {
		return false
	}
	for _, p := range paths {
		e, ok := s.pending[p]
		if !ok {
			continue
		}
		delete(s.pending, p)
		if e == nil {
			delete(s.state, p)
		} else {
			s.state[p] = *e
		}
	}
	return true
}

// syncGet: datoteka kopije za minigw (le poslane spremembe, nikoli prezrte ali zunaj kopije).
func (h *shareHub) syncGet(key, path, owner string) ([]byte, bool) {
	h.mu.Lock()
	s := h.share(key, owner)
	if s == nil || s.readOnly || s.pending[path] == nil {
		h.mu.Unlock()
		return nil, false
	}
	s.lastSeen = time.Now()
	end := h.opStart(s)
	h.mu.Unlock()
	defer end()
	if _, ok := syncResolve(path, s.copy); !ok {
		return nil, false
	}
	return readRegular(s.copy, path, maxSyncFile, false) // agent ne podtakne povezave ven iz kopije
}

// syncCopyInUse is the 409 text of removing a copy that a session or agent uses (the same text in Shares.swift; testdata/sync_copy_in_use.txt).
const syncCopyInUse = "sync: a minigw session or agent is using this copy; retry when it stops"

// ifCopyFree runs act (a fast step: the rename that takes the copy away) only if no session is registered on `copy` and no
// request of an agent holds it; true = in use. h.mu is also taken by register, so no session can register this copy between the
// check and act: one that registers later starts a fresh copy.
func (h *shareHub) ifCopyFree(copy string, act func() error) (bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	c := filepath.Clean(copy)
	for _, s := range h.bySession {
		if filepath.Clean(s.copy) == c {
			return true, nil
		}
	}
	if h.ops[c] > 0 { // an operation of a session that was unregistered meanwhile still works in the copy
		return true, nil
	}
	for k, sem := range h.busy {
		if filepath.Clean(k) == c && len(sem) > 0 {
			return true, nil
		}
	}
	return false, act()
}

// removingName is the name a copy gets while it is deleted: <id>.removing-<ns>. A copy ID is 16 hex plus an optional "-ro" and the
// gateway computes it itself (sha256 of the folder), so no client can create a folder with a "." in its name here; the pattern is exact.
var removingName = regexp.MustCompile(`^[0-9a-f]{16}(-ro)?\.removing-[0-9]+$`)

var leftoverRun sync.Mutex // one cleanup at a time; a second request skips

// removeLeftoverCopies deletes the leftovers of removals that did not finish, in one device folder (device "" = all of them), inside
// an opened os.Root: only directories (a link is not one, and RemoveAll does not follow links) whose name matches removingName.
// The copy marker of a copy that is gone goes with it.
func removeLeftoverCopies(root, device string) {
	if leftoverRun.TryLock() {
		defer leftoverRun.Unlock()
		cleanLeftovers(root, device)
	}
}

func cleanLeftovers(root, device string) {
	r, err := os.OpenRoot(root)
	if err != nil {
		return
	}
	defer r.Close()
	owners := []string{device}
	if device == "" {
		ents, _ := rootReadDir(r, ".")
		owners = owners[:0]
		for _, e := range ents {
			if e.IsDir() && e.Name() != "runs" {
				owners = append(owners, e.Name())
			}
		}
	}
	for _, o := range owners {
		ents, err := rootReadDir(r, o)
		if err != nil {
			continue
		}
		for _, e := range ents {
			if !e.IsDir() || !removingName.MatchString(e.Name()) {
				continue
			}
			rel := filepath.Join(o, e.Name())
			if r.RemoveAll(rel) != nil {
				continue
			}
			base := filepath.Join(o, e.Name()[:strings.Index(e.Name(), ".removing-")])
			if _, err := r.Lstat(base); os.IsNotExist(err) {
				r.Remove(base + ".used")
			}
		}
	}
}

// syncCopies uses an opened root so a copy cannot redirect removal outside Scratch. `free` (nil = always free): see ifCopyFree.
func syncCopies(root, device, remove string, free func(copy string, act func() error) (bool, error)) ([]string, int, string) {
	copies := []string{}
	if remove != "" && !validSyncCopyID(remove) {
		return nil, 400, "sync: invalid copy ID"
	}
	r, err := os.OpenRoot(root)
	if os.IsNotExist(err) && remove == "" {
		return copies, 200, ""
	}
	if err != nil {
		return nil, 404, "sync: copy not found"
	}
	defer r.Close()
	st, err := r.Lstat(device)
	if err != nil {
		if remove == "" {
			return copies, 200, ""
		}
		return nil, 404, "sync: copy not found"
	}
	if !st.IsDir() {
		return nil, 400, "sync: invalid copy directory"
	}
	go removeLeftoverCopies(root, device) // own root, runs after this call's folder listing, skipped if another cleanup runs
	entries, err := rootReadDir(r, device)
	if err != nil {
		return nil, 500, "sync: cannot read copies"
	}
	for _, e := range entries {
		if e.IsDir() && validSyncCopyID(e.Name()) {
			copies = append(copies, e.Name())
		}
	}
	sort.Strings(copies)
	if remove == "" {
		return copies, 200, ""
	}
	found := false
	for _, id := range copies {
		if id == remove {
			found = true
		}
	}
	if !found {
		return nil, 404, "sync: copy not found"
	}
	rel := filepath.Join(device, remove)
	// The copy leaves its name in one rename under the hub lock (no registration of it in between); the delete itself runs after,
	// outside the lock, on the renamed folder (not listed, not reachable by a session).
	gone := fmt.Sprintf("%s.removing-%d", rel, time.Now().UnixNano())
	rename := func() error { return r.Rename(rel, gone) }
	if free == nil {
		free = func(_ string, act func() error) (bool, error) { return false, act() }
	}
	inUse, err := free(filepath.Join(root, device, remove), rename)
	if inUse {
		return nil, 409, syncCopyInUse
	}
	if err != nil {
		return nil, 500, "sync: cannot remove copy"
	}
	if err := r.RemoveAll(gone); err != nil {
		return nil, 500, "sync: cannot remove copy"
	}
	if err := r.Remove(rel + ".used"); err != nil && !os.IsNotExist(err) {
		return nil, 500, "sync: cannot remove copy marker"
	}
	return copies, 200, ""
}

func validSyncCopyID(id string) bool {
	id = strings.TrimSuffix(id, "-ro")
	if len(id) != 16 {
		return false
	}
	for _, c := range id {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
