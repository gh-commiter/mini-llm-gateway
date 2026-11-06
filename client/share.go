package main

// Trenutna mapa za agente na Mac miniju (kot ShareClient v MiniGW/main.swift): router jo priklopi
// (mount_webdav), minigw streže datoteke. Mac mini odjemalca ne doseže, zato vse teče po povezavah,
// ki jih odpre minigw (long poll).

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
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
	c       connection
	session string
	root    string
	mu      sync.Mutex
	key     string
}

func (s *shareClient) currentKey() string { s.mu.Lock(); defer s.mu.Unlock(); return s.key }

func (s *shareClient) register() bool {
	st, data := call(s.c, "POST", "/minigw/share", map[string]string{
		"x-claude-mini-session": s.session, "x-claude-mini-cwd": s.root, "x-share-key": s.currentKey()}, nil, 10*time.Second)
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
			r := davHandle(job.Method, job.Path, job.Headers, job.Body, s.root, job.Prefix)
			body, err := json.Marshal(davReply{ID: job.ID, Response: r})
			if err != nil {
				continue
			}
			call(s.c, "POST", "/minigw/share/reply", map[string]string{"x-share-key": k, "content-type": "application/json"}, body, 120*time.Second)
		case 204:
		case 404:
			s.register() // router se je znova zagnal
			time.Sleep(time.Second)
		default:
			time.Sleep(2 * time.Second) // router nedosegljiv: poskusi znova
		}
	}
}

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
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data
}
