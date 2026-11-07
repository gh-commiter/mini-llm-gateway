package main

// Stran z nastavitvami (/minigw/settings): ena statična vgrajena datoteka HTML brez skrivnosti. Pladenj ali
// minigw router settings odpre /minigw/settings#t=<enkratni žeton>: žeton je v fragmentu, zato ga strežnik nikoli ne
// vidi v vrstici zahteve. Stran ga pošlje s POST /minigw/settings/login in dobi ID seje, ki ga hrani v sessionStorage
// (vezan na shemo, gostitelja in vrata) in pošilja v glavi X-Minigw-Session. Brez piškotkov: piškotek na 127.0.0.1
// bi dobil vsak lokalni strežnik na drugih vratih; glave po meri tuja stran brez CORS ne more poslati.

import (
	"crypto/rand"
	"crypto/subtle"
	_ "embed"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed settings.html
var settingsPage string

const (
	settingsHeader = "X-Minigw-Session"
	sessionIdle    = 30 * time.Minute
)

// pickFunc: okno za izbiro mape (kind "folder") ali programa ("exe") na tem računalniku; "" = preklic.
type pickFunc func(kind string) (string, error)

type settingsState struct {
	mu       sync.Mutex
	logins   map[string]time.Time // enkratni žeton → rok
	sessions map[string]time.Time // ID seje → zadnja uporaba
}

func randomToken() string {
	b := make([]byte, 32)
	rand.Read(b)
	return base64url(b)
}

// purge: potekli žetoni in seje ven; največ 50 vsakega (ponytail: najstarejši ni nujno prvi, dovolj za en računalnik).
func (st *settingsState) purge(now time.Time) {
	if st.logins == nil {
		st.logins, st.sessions = map[string]time.Time{}, map[string]time.Time{}
	}
	for k, exp := range st.logins {
		if now.After(exp) || len(st.logins) > 50 {
			delete(st.logins, k)
		}
	}
	for k, used := range st.sessions {
		if now.Sub(used) > sessionIdle || len(st.sessions) > 50 {
			delete(st.sessions, k)
		}
	}
}

func (st *settingsState) newLogin() string {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.purge(time.Now())
	t := randomToken()
	st.logins[t] = time.Now().Add(60 * time.Second)
	return t
}

// login: enkratni žeton (60 s, le enkrat) → nova seja.
func (st *settingsState) login(token string) (string, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.purge(time.Now())
	exp, ok := st.logins[token]
	if token == "" || !ok {
		return "", false
	}
	delete(st.logins, token)
	if time.Now().After(exp) {
		return "", false
	}
	id := randomToken()
	st.sessions[id] = time.Now()
	return id, true
}

// valid: seja iz glave; uporaba podaljša rok mirovanja (30 min).
func (st *settingsState) valid(id string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.purge(time.Now())
	for k := range st.sessions { // primerjava v konstantnem času, ne iskanje po ključu
		if subtle.ConstantTimeCompare([]byte(k), []byte(id)) == 1 {
			st.sessions[k] = time.Now()
			return true
		}
	}
	return false
}

func (st *settingsState) logout(id string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	delete(st.sessions, id)
}

// clear: ob novem žetonu routerja se končajo vse seje.
func (st *settingsState) clear() {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.sessions = map[string]time.Time{}
	st.logins = map[string]time.Time{}
}

func (s *server) settingsLoginURL() string {
	return "http://127.0.0.1:" + strconv.Itoa(s.conf().Port) + "/minigw/settings#t=" + s.settings.newLogin()
}

func (s *server) settingsHTTP(w http.ResponseWriter, r *req) int {
	if st := localOnly(w, r); st != 0 {
		return st
	}
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("X-Frame-Options", "DENY")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	switch r.Method + " " + r.URL.Path {
	case "GET /minigw/settings": // statična, brez skrivnosti
		nonce := randomToken()
		h.Set("Content-Security-Policy", "default-src 'none'; script-src 'nonce-"+nonce+"'; style-src 'nonce-"+nonce+
			"'; connect-src 'self'; img-src 'self' data:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
		page := strings.NewReplacer("{{NONCE}}", nonce, "{{PICKER}}", strconv.FormatBool(s.pick.Load() != nil)).Replace(settingsPage)
		return writeStatus(w, 200, "text/html; charset=utf-8", []byte(page))
	case "POST /minigw/settings/login":
		if st := s.refuseAgentPeer(w, r); st != 0 { // R4
			return st
		}
		if !readBody(w, r, 4<<10) {
			return writeError(w, r.rid, 413, "Request body too large or too slow")
		}
		var b struct {
			T string `json:"t"`
		}
		if err := decodeStrict(r.body, &b); err != nil {
			return writeError(w, r.rid, 400, err.Error())
		}
		id, ok := s.settings.login(b.T)
		if !ok {
			return writeError(w, r.rid, 401, "This settings link has expired or was already used. Open Settings… again.")
		}
		return writeJSON(w, r.rid, 200, map[string]string{"session": id})
	case "POST /minigw/settings/pick": // Izberi… / Prebrskaj…: okno pladnja, pot nazaj na stran
		if st := s.refuseAgentPeer(w, r); st != 0 {
			return st
		}
		if !s.settings.valid(r.h(settingsHeader)) {
			return writeError(w, r.rid, 401, "Sign in to Settings again (Settings… in the tray menu)")
		}
		if !readBody(w, r, 4<<10) {
			return writeError(w, r.rid, 413, "Request body too large or too slow")
		}
		var b struct {
			Kind string `json:"kind"`
		}
		if err := decodeStrict(r.body, &b); err != nil || (b.Kind != "folder" && b.Kind != "exe") {
			return writeError(w, r.rid, 400, `kind must be "folder" or "exe"`)
		}
		pick := s.pick.Load()
		if pick == nil {
			return writeError(w, r.rid, 501, "No file dialog here: type the path")
		}
		if !s.picking.CompareAndSwap(false, true) {
			return writeError(w, r.rid, 409, "A file dialog is already open on the gateway computer")
		}
		defer s.picking.Store(false)
		path, err := (*pick)(b.Kind)
		if err != nil {
			return writeError(w, r.rid, 500, "File dialog: "+err.Error())
		}
		return writeJSON(w, r.rid, 200, map[string]string{"path": path})
	case "POST /minigw/settings/logout":
		s.settings.logout(r.h(settingsHeader))
		return writeJSON(w, r.rid, 200, map[string]bool{"ok": true})
	}
	return writeError(w, r.rid, 405, "Not allowed: "+r.Method+" "+r.URL.Path)
}
