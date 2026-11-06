package main

// Trenutna mapa za agente na Mac miniju (kot ShareClient v MiniGW/main.swift): router jo priklopi
// (mount_webdav), minigw streže datoteke. Mac mini odjemalca ne doseže, zato vse teče po povezavah,
// ki jih odpre minigw (long poll).

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
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
	readOnly bool // pisanje zavrne minigw sam (router mapo priklopi tudi rdonly)
	sync     bool // --minigw-work=sync: router dobi kopijo namesto priklopa (sync.go)
	mu       sync.Mutex
	key      string
	syncMu   sync.Mutex
	synced   map[string]syncEntry // manifest ob zadnjem prenosu (zgoščevanje, spori)
	reported map[string]bool      // že javljene prevelike datoteke
	unpulled bool                 // spremembe agenta, ki niso bile prevzete in potrjene
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
	st, data := call(s.c, "POST", "/minigw/share", map[string]string{
		"x-claude-mini-session": s.session, "x-claude-mini-cwd": s.root, "x-share-key": s.currentKey(), "x-share-mode": s.mode(),
		"x-claude-mini-work": work}, nil, 10*time.Second)
	var r struct {
		Key string `json:"key"`
	}
	if st != 200 || json.Unmarshal(data, &r) != nil || r.Key == "" {
		return false
	}
	s.mu.Lock()
	s.key = r.Key
	s.mu.Unlock()
	return true
}

func (s *shareClient) unregister() {
	if k := s.currentKey(); k != "" {
		call(s.c, "DELETE", "/minigw/share", map[string]string{"x-share-key": k}, nil, 3*time.Second)
	}
}

func (s *shareClient) start(workers int) {
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
				s.note("the router restarted; the agent's changes that were not copied back may be overwritten by the next sync")
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
	switch {
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

// maxReply: največji odgovor routerja v pomnilniku (datoteka do 64 MB + glave).
const maxReply = 80 << 20

// call: poljubna zahteva do routerja (prek lokalnega proxyja); 0 = ni povezave.
func call(c connection, method, path string, headers map[string]string, body []byte, timeout time.Duration) (int, []byte) {
	req, err := http.NewRequest(method, c.URL+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil
	}
	req.Header.Set("authorization", "Bearer "+c.Token)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		return 0, nil
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, maxReply))
	return resp.StatusCode, data
}
