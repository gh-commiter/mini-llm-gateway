package main

// Stran z nastavitvami (/minigw/settings): ena vgrajena datoteka HTML, ki kliče skrbniški API s piškotkom seje in
// glavo X-CSRF. Prijava le z enkratnim žetonom (60 s), ki ga da pladenj ali minigw router settings.

import (
	"crypto/rand"
	_ "embed"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed settings.html
var settingsPage string

const settingsCookie = "minigw_settings"

type settingsSession struct {
	csrf string
	exp  time.Time
}

type settingsState struct {
	mu       sync.Mutex
	logins   map[string]time.Time // enkratni žeton → rok
	sessions map[string]settingsSession
}

func randomToken() string {
	b := make([]byte, 32)
	rand.Read(b)
	return base64url(b)
}

// purge: potekli žetoni in seje ven; največ 50 vsakega (ponytail: najstarejši ni nujno prvi, dovolj za en računalnik).
func (st *settingsState) purge(now time.Time) {
	for k, exp := range st.logins {
		if now.After(exp) || len(st.logins) > 50 {
			delete(st.logins, k)
		}
	}
	for k, s := range st.sessions {
		if now.After(s.exp) || len(st.sessions) > 50 {
			delete(st.sessions, k)
		}
	}
}

func (st *settingsState) newLogin() string {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.logins == nil {
		st.logins, st.sessions = map[string]time.Time{}, map[string]settingsSession{}
	}
	st.purge(time.Now())
	t := randomToken()
	st.logins[t] = time.Now().Add(60 * time.Second)
	return t
}

// login: enkratni žeton → nova seja (ID za piškotek); žeton velja le enkrat.
func (st *settingsState) login(token string) (string, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	exp, ok := st.logins[token]
	if token == "" || !ok {
		return "", false
	}
	delete(st.logins, token)
	if time.Now().After(exp) {
		return "", false
	}
	id := randomToken()
	st.sessions[id] = settingsSession{csrf: randomToken(), exp: time.Now().Add(12 * time.Hour)}
	return id, true
}

func (st *settingsState) session(r *req) (settingsSession, bool) {
	c, err := r.Cookie(settingsCookie)
	if err != nil {
		return settingsSession{}, false
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	s, ok := st.sessions[c.Value]
	if !ok || time.Now().After(s.exp) {
		return settingsSession{}, false
	}
	return s, true
}

func (s *server) settingsLoginURL() string {
	return "http://127.0.0.1:" + strconv.Itoa(s.conf().Port) + "/minigw/settings/login?t=" + s.settings.newLogin()
}

func (s *server) settingsHTTP(w http.ResponseWriter, r *req) int {
	if st := localOnly(w, r); st != 0 {
		return st
	}
	if r.Method != "GET" {
		return writeError(w, r.rid, 405, "GET only")
	}
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("X-Frame-Options", "DENY")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer") // enkratni žeton ne gre nikamor
	if r.URL.Path == "/minigw/settings/login" {
		id, ok := s.settings.login(r.URL.Query().Get("t"))
		if !ok {
			return settingsMessage(w, 401, "This settings link has expired or was already used. Open Settings… again.")
		}
		http.SetCookie(w, &http.Cookie{Name: settingsCookie, Value: id, Path: "/minigw/", HttpOnly: true,
			SameSite: http.SameSiteStrictMode, MaxAge: 12 * 3600})
		h.Set("Location", "/minigw/settings")
		w.WriteHeader(303)
		return 303
	}
	sess, ok := s.settings.session(r)
	if !ok {
		return settingsMessage(w, 401, "Not signed in. Open Settings… from the tray icon, or run: minigw router settings")
	}
	nonce := randomToken()
	h.Set("Content-Security-Policy", "default-src 'none'; script-src 'nonce-"+nonce+"'; style-src 'nonce-"+nonce+
		"'; connect-src 'self'; img-src 'self' data:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	page := strings.NewReplacer("{{NONCE}}", nonce, "{{CSRF}}", sess.csrf).Replace(settingsPage)
	return writeStatus(w, 200, "text/html; charset=utf-8", []byte(page))
}

func settingsMessage(w http.ResponseWriter, status int, msg string) int {
	w.Header().Set("Content-Security-Policy", "default-src 'none'")
	return writeStatus(w, status, "text/plain; charset=utf-8", []byte(msg+"\n"))
}
