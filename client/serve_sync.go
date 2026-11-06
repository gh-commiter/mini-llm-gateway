package main

// minigw serve: mape odjemalcev le v načinu sync (kot ShareHub v RouterCore/Shares.swift). minigw sprašuje po poslih
// (long poll); SYNC-PUSH pred agentom pošlje manifest in datoteke v kopijo, SYNC-PULL po agentu prevzame spremembe.
// Kopija je v scratch/sync/<lastnik>/<mapa>; do nje pride le lastnik, ki je mapo registriral (naprava ali »local«).

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
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
}

type shareHub struct {
	mu        sync.Mutex
	bySession map[string]*serveShare
	busy      map[string]chan struct{} // ena zahteva agenta naenkrat na kopijo
	refused   map[string]bool          // seje, ki so želele način direct
	reaper    sync.Once
}

func newShareHub() *shareHub {
	return &shareHub{bySession: map[string]*serveShare{}, busy: map[string]chan struct{}{}, refused: map[string]bool{}}
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

// close: pod h.mu.
func (h *shareHub) close(s *serveShare) {
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
	job := davJob{ID: newKey(), Method: method, Headers: map[string]string{}, Body: []byte{}} // macOS minigw ne sprejme null
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

// push: minigw naj posodobi kopijo (SYNC-PUSH); vrne kopijo ali "". Prvi prenos velike mape traja.
func (h *shareHub) push(s *serveShare) string {
	if r := h.send(s, "SYNC-PUSH", 900*time.Second); r != nil && r.Status == 200 {
		return s.copy
	}
	return ""
}

// syncBack: po agentu minigw prevzame spremembe kopije (ne pri --minigw-ro), pred koncem odgovora.
func (h *shareHub) syncBack(session string) {
	h.mu.Lock()
	s := h.bySession[session]
	h.mu.Unlock()
	if s != nil && !s.readOnly {
		h.send(s, "SYNC-PULL", 900*time.Second)
	}
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
	for p, e := range cur {
		if o, ok := old[p]; !ok || o.SHA256 != e.SHA256 || o.Exec != e.Exec {
			ch.Changed = append(ch.Changed, e)
		}
	}
	for p := range old {
		if _, ok := cur[p]; ok {
			continue
		}
		if full, ok := syncResolve(p, root); ok {
			if _, err := os.Lstat(full); err == nil {
				continue
			}
		}
		ch.Deleted = append(ch.Deleted, p)
	}
	sort.Slice(ch.Changed, func(i, j int) bool { return ch.Changed[i].Path < ch.Changed[j].Path })
	sort.Strings(ch.Deleted)
	return ch
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
	prev := s.state
	h.mu.Unlock()

	os.MkdirAll(s.copy, 0o700)
	current, _ := syncScan(s.copy, prev) // zunaj zaklepa: velika mapa traja
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
			if full, ok := syncResolve(p, s.copy); ok {
				os.Remove(full)
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
func (h *shareHub) syncPut(key, path string, data []byte, owner string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.share(key, owner)
	if s == nil {
		return 404
	}
	s.lastSeen = time.Now()
	e, listed := s.incoming[path]
	full, ok := syncResolve(path, s.copy)
	if !listed || !ok {
		return 403
	}
	if sha256Hex(data) != e.SHA256 {
		return 409 // spremenjena med prenosom
	}
	if syncWrite(full, data, e.Mtime, e.Exec, !s.execKnown) != nil {
		return 500
	}
	if !s.execKnown { // Windows: dejanski način kopije
		if a, ok := syncEntryOf(full, path, nil); ok {
			e = a
		}
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
	h.mu.Unlock()

	current, skipped := syncScan(s.copy, prev)
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
	h.mu.Unlock()
	full, ok := syncResolve(path, s.copy)
	if !ok {
		return nil, false
	}
	data, err := os.ReadFile(full)
	return data, err == nil
}
