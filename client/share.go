package main

// Trenutna mapa za agente na Mac miniju (kot ShareClient v MiniGW/main.swift): router jo priklopi
// (mount_webdav), minigw streže datoteke. Mac mini odjemalca ne doseže, zato vse teče po povezavah,
// ki jih odpre minigw (long poll).

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type davJob struct {
	ID      string            `json:"id"`
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Prefix  string            `json:"prefix"`
	Headers map[string]string `json:"headers"`
	Body    []byte            `json:"body"`
}

type davReply struct {
	ID       string      `json:"id"`
	Response davResponse `json:"response"`
}

type shareClient struct {
	c        connection
	session  string
	root     string
	readOnly bool                // pisanje zavrne minigw sam (router mapo priklopi tudi rdonly)
	sync     bool                // --minigw-work=sync: router dobi kopijo namesto priklopa (sync.go)
	native   bool                // --minigw-work=native: router dela v svoji mapi (native.go); posla routerja ni
	inPlace  bool                // native-direct: agenti v mapi sami (tudi v repozitoriju git), ne na svojih vejah
	branches bool                // native v repozitoriju git: agenti na svojih vejah minigw/… (odgovor prehoda)
	lock     bool                // native v mapi sami: en pisoči agent naenkrat (odgovor prehoda; starejši prehod ga ne pošlje)
	capped   []string            // #60: agenti, ki zaradi omejitve prehoda le berejo (odgovor prehoda)
	worktree bool                // --minigw-work=worktree: posnetek kot bundle, veja agenta nazaj kot ref (worktree.go)
	top      string              // worktree: koren repozitorija git
	snaps    []string            // worktree: posnetki, poslani prehodu (začetek dela agenta za vrstico o veji)
	nested   map[string][]string // worktree: ugnezdeni repozitoriji (pot) → njihovi poslani posnetki
	conv     wtConv              // worktree (#66): pogovor (wtConversation); prazen = brez nadaljevanja
	chain    map[string]wtRecord // worktree (#66): poslani posnetek ali vrh → začetek verige pogovora (nil = še ni odločeno)
	contTip  string              // worktree (#66): vrh, iz katerega začnejo vsi agenti zagona ("" = iz mape)
	contRec  wtRecord            // worktree (#66): veriga vrha contTip
	gateway  string              // worktree (#66): prehod (naslov profila); pogovor nadaljuje le na istem
	mu       sync.Mutex
	key      string
	syncMu   sync.Mutex
	synced   map[string]syncEntry       // manifest ob zadnjem prenosu (zgoščevanje, spori)
	sent     map[string]map[string]bool // pot → različice (sha256), ki jih je router dobil od tega odjemalca (#20)
	reported map[string]bool            // že javljene prevelike datoteke
	unpulled bool                       // spremembe agenta, ki niso bile prevzete in potrjene
	refusal  string                     // sporočilo routerja ob zavrnjeni registraciji (npr. le način sync)
	checked  *fileID                    // #84: istovetnost, ki jo je preveril shareWanted (nil: preizkusi brez preverbe)
	rootID   fileID                     // L6 (#84): istovetnost korena ob start; ničelna brez start (preizkusi)
	rootErr  error                      // L6: ob start ni znana ali ni preverjena → vsi posli zavrnjeni
	fsMu     sync.RWMutex               // #82: lockFolder v tem procesu (branje deljeno, pisanje izključno)
	fsWant   atomic.Int32               // #82 N1: čakajoča pisanja; branje se jim umakne (do svojega roka)
}

var writeMethods = map[string]bool{"PUT": true, "DELETE": true, "MKCOL": true, "MOVE": true, "COPY": true, "PROPPATCH": true, "LOCK": true}

func (s *shareClient) mode() string {
	if s.readOnly {
		return "ro"
	}
	return "rw"
}

func (s *shareClient) currentKey() string { s.mu.Lock(); defer s.mu.Unlock(); return s.key }

func (s *shareClient) register() bool {
	work := "direct"
	if s.sync {
		work = "sync"
	}
	h := map[string]string{
		"x-claude-mini-session": s.session, "x-claude-mini-cwd": s.root, "x-share-key": s.currentKey(), "x-share-mode": s.mode(),
		"x-claude-mini-work": work}
	if s.worktree {
		h["x-claude-mini-work"], h["x-worktree-top"] = "worktree", filepath.ToSlash(s.top)
	}
	if s.native { // vsaka registracija (tudi po ponovnem zagonu routerja) z novim dokazom
		proof, why := nativeProof(s.c, s.root)
		if proof == nil {
			s.mu.Lock()
			s.refusal = why
			s.mu.Unlock()
			return false
		}
		for k, v := range proof {
			h[k] = v
		}
		h["x-claude-mini-work"] = "native"
		if s.inPlace {
			h["x-native-inplace"] = "1" // starejši prehod to že dela
		}
	}
	st, data := call(s.c, "POST", "/minigw/share", h, nil, 10*time.Second)
	var r struct {
		Key      string   `json:"key"`
		Branches bool     `json:"branches"`
		Lock     bool     `json:"lock"`
		Capped   []string `json:"capped"` // #60: agenti, ki zaradi omejitve prehoda le berejo (starejši prehod: nič)
		Error    struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	err := json.Unmarshal(data, &r)
	if st != 200 || err != nil || r.Key == "" {
		s.mu.Lock() // ponovna registracija teče iz več delavcev hkrati
		s.refusal = r.Error.Message
		s.mu.Unlock()
		return false
	}
	s.mu.Lock()
	s.key, s.branches, s.lock, s.capped = r.Key, r.Branches, r.Lock, r.Capped
	s.mu.Unlock()
	return true
}

func (s *shareClient) unregister() {
	if k := s.currentKey(); k != "" {
		call(s.c, "DELETE", "/minigw/share", map[string]string{"x-share-key": k}, nil, 3*time.Second)
	}
}

func (s *shareClient) start(workers int) {
	s.rootID, s.rootErr = fileIDOf(s.root)
	if s.rootErr == nil && s.checked != nil && *s.checked != s.rootID {
		s.rootErr = errors.New("the folder changed after it was checked")
		fmt.Fprintln(os.Stderr, "minigw: not sharing "+s.root+" (the folder changed after it was checked)")
	}
	for i := 0; i < workers; i++ {
		go s.work()
	}
}

func (s *shareClient) work() {
	for {
		k := s.currentKey()
		st, data := call(s.c, "GET", "/minigw/share/next", map[string]string{"x-share-key": k}, nil, 40*time.Second)
		switch st {
		case 200:
			var job davJob
			if json.Unmarshal(data, &job) != nil {
				continue
			}
			body, err := json.Marshal(davReply{ID: job.ID, Response: s.handle(job, k)})
			if err != nil {
				continue
			}
			call(s.c, "POST", "/minigw/share/reply", map[string]string{"x-share-key": k, "content-type": "application/json"}, body, 120*time.Second)
		case 204:
		case 404:
			s.syncMu.Lock()
			if s.sync && s.unpulled { // nova registracija: router ne pozna več nepotrjenih sprememb
				s.note("the gateway restarted; the agent's changes that were not copied back may be overwritten by the next sync")
			}
			s.syncMu.Unlock()
			s.register() // router se je znova zagnal
			time.Sleep(time.Second)
		default:
			time.Sleep(2 * time.Second) // router nedosegljiv: poskusi znova
		}
	}
}

// handle: posel routerja. Router ni zaupanja vreden: SYNC-* le v načinu sync, SYNC-PULL (piše v mapo) nikoli pri
// --minigw-ro, WebDAV le v načinu direct (pisanje ne pri --minigw-ro).
func (s *shareClient) handle(job davJob, k string) davResponse {
	if !s.sameRoot() { // L6 (#84): vsi posli (WebDAV, SYNC-*, WORKTREE-*), ne le WebDAV
		return dav(403, nil, nil)
	}
	if job.Method != "WORKTREE-NOTE" { // #82: sporočilo mape ne bere in ne piše
		// SYNC-PUSH mapo le bere; sync in worktree čakajo do roka posla prehoda (sicer bi se delo agenta izgubilo)
		exclusive := writeMethods[job.Method] || job.Method == "SYNC-PULL" || strings.HasPrefix(job.Method, "WORKTREE-")
		unlock := s.lockFolder(exclusive, folderLockWaitFor(job.Method))
		if unlock == nil {
			return dav(503, map[string]string{"content-type": "text/plain", "retry-after": "5"}, []byte(folderBusyText+"\n"))
		}
		defer unlock()
		if !s.sameRoot() { // I2: znova pod zaklepom
			return dav(403, nil, nil)
		}
	}
	switch {
	case s.worktree && job.Method == "WORKTREE-PUSH":
		s.syncMu.Lock()
		defer s.syncMu.Unlock()
		return s.wtPush(k, job)
	case job.Method == "WORKTREE-NOTE": // sporočilo prehoda (native: dostavljena ali nedostavljena veja; #45 tudi sync: agent le bere)
		var n struct{ Message, Branch, Base, Nested string }
		json.Unmarshal(job.Body, &n)
		s.syncMu.Lock()
		defer s.syncMu.Unlock()
		top := gitToplevel(s.root)
		if s.native && n.Nested == "" && deliveredBranch(s.root, n.Branch, n.Base) { // native z git (worktree: odločajo lastni posnetki)
			s.branchNote(n.Branch, n.Base)
		} else if s.native && n.Nested != "" && validNestedPath(n.Nested) && top != "" && nestedInside(top, n.Nested) &&
			deliveredBranch(filepath.Join(top, filepath.FromSlash(n.Nested)), n.Branch, n.Base) { // A: ugnezden v native
			s.branchNote(n.Branch, n.Base, n.Nested)
		} else {
			fmt.Fprintln(os.Stderr, gatewayNote(n.Message))
		}
		return dav(200, nil, nil)
	case s.worktree && job.Method == "WORKTREE-PULL":
		s.syncMu.Lock()
		defer s.syncMu.Unlock()
		return s.wtPull(k, job)
	case s.native, s.worktree, strings.HasPrefix(job.Method, "WORKTREE-"):
		return dav(403, nil, nil)
	case strings.HasPrefix(job.Method, "SYNC-"):
		if !s.sync || (job.Method == "SYNC-PULL" && s.readOnly) || (job.Method != "SYNC-PUSH" && job.Method != "SYNC-PULL") {
			return dav(403, nil, nil)
		}
		s.syncMu.Lock()
		defer s.syncMu.Unlock()
		if (job.Method == "SYNC-PUSH" && s.syncPush(k)) || (job.Method == "SYNC-PULL" && s.syncPull(k)) {
			return dav(200, nil, nil)
		}
		return dav(500, nil, nil)
	case s.sync || (s.readOnly && writeMethods[job.Method]):
		return dav(403, nil, nil)
	}
	return davHandle(job.Method, job.Path, job.Headers, job.Body, s.root, job.Prefix)
}

// folderLockWait (#82, vmesno): toliko posel WebDAV čaka zaklep mape, nato 503 (nikoli ne obvisi).
var folderLockWait = 5 * time.Second

// folderSyncWait, folderWorktreePullWait: sync in worktree čakajo skoraj do roka posla prehoda (WORKTREE-PULL 300 s,
// drugi 900 s), da prehod odgovor (tudi 503) dobi pred svojim rokom; 5 s bi delo agenta pustilo na prehodu.
var folderSyncWait, folderWorktreePullWait = 880 * time.Second, 280 * time.Second

func folderLockWaitFor(method string) time.Duration {
	switch {
	case method == "WORKTREE-PULL":
		return folderWorktreePullWait
	case strings.HasPrefix(method, "SYNC-"), strings.HasPrefix(method, "WORKTREE-"):
		return folderSyncWait
	}
	return folderLockWait
}

// folderLockDir: mapa zaklepov ("" = <mapa nastavitev>/minigw/locks; preizkusi začasno mapo).
var folderLockDir string

const folderBusyText = "minigw: the shared folder is busy (another minigw session on this folder is using it); try again"

var (
	folderLockOnce sync.Once
	folderBusyMu   sync.Mutex
	folderBusyLast time.Time
)

// folderLockName: ime datoteke zaklepa; st_dev na macOS kot uint32 (kot SharedFolders.fileID v Swift), da oba odjemalca
// zaklepata isto datoteko. Le za ime: shranjene istovetnosti #84 ostanejo.
func folderLockName(id fileID, goos string) string {
	if goos == "darwin" {
		id.dev = uint64(uint32(id.dev))
	}
	return fmt.Sprintf("%x-%x.lock", id.dev, id.ino)
}

// lockFolder (#82, vmesno): branje (GET, HEAD, PROPFIND, SYNC-PUSH) deljeno, pisanje, SYNC-PULL in worktree izključno —
// v procesu (fsMu) in med procesi (flock / LockFileEx na datoteki, imenovani po istovetnosti korena, v mapi nastavitev,
// ne v projektu; ob sesutju ga sprosti jedro). Tako branje ne dirka z MOVE nadrejene mape med preverbo poti in branjem in
// dve seji minigw na isti mapi ne pišeta hkrati. nil = zaklepa ni bilo v roku wait (503).
// ponytail: preveri-nato-uporabi ostane za lokalni proces brez zaklepa; popolno (operacije prek odprtega korena) 0.5.26.
func (s *shareClient) lockFolder(exclusive bool, wait time.Duration) func() {
	try, unlock := s.fsMu.TryRLock, s.fsMu.RUnlock
	if exclusive {
		try, unlock = s.fsMu.TryLock, s.fsMu.Unlock
	}
	deadline := time.Now().Add(wait)
	busy := func() func() {
		folderBusyMu.Lock()
		if time.Since(folderBusyLast) >= 30*time.Second { // vsaka nova zasedenost, največ enkrat na 30 s
			fmt.Fprintln(os.Stderr, folderBusyText)
		}
		folderBusyLast = time.Now()
		folderBusyMu.Unlock()
		return nil
	}
	if exclusive { // N1: prednost pisanju (TryLock je nima): novo branje čaka, dokler pisanje čaka
		s.fsWant.Add(1)
		defer s.fsWant.Add(-1)
	}
	for (!exclusive && s.fsWant.Load() > 0) || !try() { // L3: tudi v procesu le do roka
		if time.Now().After(deadline) {
			return busy()
		}
		time.Sleep(5 * time.Millisecond)
	}
	id := s.rootID
	if id == (fileID{}) {
		id, _ = fileIDOf(s.root) // brez start (preizkusi)
	}
	dir := folderLockDir
	if dir == "" {
		if d, err := os.UserConfigDir(); err == nil {
			dir = filepath.Join(d, "minigw", "locks")
		}
	}
	var f *os.File
	err := os.MkdirAll(dir, 0o700)
	if err == nil && !(ownedByMe(dir) && safeDir(dir)) { // L1: mapa zaklepov le naša
		err = errors.New(dir + " is not a private folder of this user")
	}
	if err == nil {
		f, err = openLockFile(filepath.Join(dir, folderLockName(id, runtime.GOOS)))
	}
	for ; err == nil; time.Sleep(10 * time.Millisecond) {
		busyLock, lerr := tryLockFile(f, exclusive)
		if lerr == nil {
			return func() { unlockFile(f); f.Close(); unlock() } // odklep pred Close (kot lockScratch)
		} else if !busyLock {
			err = lerr
		} else if time.Now().After(deadline) {
			f.Close()
			unlock()
			return busy()
		}
	}
	if f != nil {
		f.Close()
	}
	// Zaklep ni mogoč (npr. mapa nastavitev na disku brez flock, tuja mapa): kot 0.5.24, le znotraj procesa.
	folderLockOnce.Do(func() {
		fmt.Fprintln(os.Stderr, "minigw: cannot lock the shared folder against other minigw sessions ("+err.Error()+")")
	})
	return unlock
}

// sameRoot (L6, #84): koren je še ista mapa kot ob start (mapo med sejo ni zamenjala povezava ali druga mapa).
func (s *shareClient) sameRoot() bool {
	if s.rootID == (fileID{}) && s.rootErr == nil {
		return true // brez start (preizkusi)
	}
	id, err := fileIDOf(s.root)
	return s.rootErr == nil && err == nil && id == s.rootID
}

// gatewayNote (M2): besedilo prehoda kot »minigw: gateway: …« v eni vrstici; vrstico o veji izpiše le minigw sam iz
// preverjenih podatkov (worktree: lastni posnetki), zato besedilo prehoda nikoli ni videti kot ona.
func gatewayNote(msg string) string {
	m := shortLine(msg, 500)
	if strings.Contains(strings.ToLower(m), "agent work is on branch") {
		m = "reported a branch that minigw could not verify; ignored"
	}
	return "minigw: gateway: " + m
}

// maxReply: največji odgovor routerja v pomnilniku (datoteka do 64 MB + glave).
const maxReply = 80 << 20

// call: poljubna zahteva do routerja (prek lokalnega proxyja); 0 = ni povezave.
// noRedirect (L2): zahteva z žetonom prehoda nikoli ne sledi preusmeritvi (npr. na proxy drugega profila na 127.0.0.1).
func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

func call(c connection, method, path string, headers map[string]string, body []byte, timeout time.Duration) (int, []byte) {
	req, err := http.NewRequest(method, c.URL+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil
	}
	req.Header.Set("authorization", "Bearer "+c.Token)
	req.Header.Set("X-Minigw-Client", clientInfo()) // OS in različica za vrstico čakajoče naprave (starejši prehodi jo prezrejo)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := (&http.Client{Timeout: timeout, CheckRedirect: noRedirect}).Do(req)
	if err != nil {
		return 0, nil
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, maxReply))
	return resp.StatusCode, data
}
