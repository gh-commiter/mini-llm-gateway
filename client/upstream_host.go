package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"minigw/coreapi"
)

// upstreamHost: this gateway as a CLIENT of another gateway (the KB upstream). Its secret, the token line of the upstream and the
// device key this gateway uses there, lives in the same place as the provider keys (the credential store; a private file where there is
// none), under "upstream:<name>": never in the core database (so never in a backup), an argument, a log or an API answer. The connection
// is the client's own encrypted protocol (Noise, the upstream's key pinned), made in memory per request: no listener, nothing for another
// local program to borrow.
type upstreamHost struct{ s *server }

type upstreamSecret struct {
	Name   string `json:"name,omitempty"` // how this gateway is named on the upstream's device list
	Token  string `json:"token"`
	Key    string `json:"key"`    // the upstream's public key (pinned)
	Device string `json:"device"` // this gateway's device key for it (Ed25519 seed)
}

func upstreamKeyName(name string) string { return "upstream:" + name }

func (h upstreamHost) secret(name string) (upstreamSecret, error) {
	v, _ := keyGet(h.s.cfgDir, upstreamKeyName(name))
	var sec upstreamSecret
	if v == "" || json.Unmarshal([]byte(v), &sec) != nil || sec.Token == "" || sec.Key == "" || sec.Device == "" {
		return sec, errors.New("no credentials for upstream " + name + ": remove it and add it again")
	}
	return sec, nil
}

func (h upstreamHost) deviceOf(sec upstreamSecret) (*device, string) {
	seed, err := fromBase64url(sec.Device)
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, ""
	}
	k := ed25519.NewKeyFromSeed(seed)
	n := sec.Name
	if n == "" {
		n = "minigw gateway"
	}
	return &device{key: k, name: n}, base64url(k.Public().(ed25519.PublicKey))
}

// Connect: the token line is the upstream's "Copy Token" (<token>#<gateway key>). A token of this same gateway is refused.
func (h upstreamHost) Connect(name, address, line, deviceName string) (coreapi.UpstreamInfo, error) {
	tok, key, ok := newToken(strings.TrimSpace(line), nil)
	for _, r := range tok { // it goes into an Authorization header: printable ASCII without spaces, as readKey allows
		if r < 0x21 || r > 0x7e {
			return coreapi.UpstreamInfo{}, errors.New("the token must be printable ASCII without spaces")
		}
	}
	if !ok || key == nil {
		return coreapi.UpstreamInfo{}, errors.New("the token line must be the upstream's whole Copy Token (<token>#<key>)")
	}
	kb, err := fromBase64url(*key)
	if err != nil || len(kb) != 32 {
		return coreapi.UpstreamInfo{}, errors.New("the key part of the token is not a gateway key: copy the token again on the upstream")
	}
	if mine, err := h.s.cfg.privateKey(); err == nil && bytes.Equal(mine.PublicKey().Bytes(), kb) {
		return coreapi.UpstreamInfo{}, errors.New("this is the token of this same gateway: an upstream must be another gateway")
	}
	if host, port, err := net.SplitHostPort(address); err == nil && h.s.cfg.Port > 0 && port == strconv.Itoa(h.s.cfg.Port) {
		if ip := net.ParseIP(host); host == "localhost" || ip != nil && ip.IsLoopback() {
			return coreapi.UpstreamInfo{}, errors.New("this address is this same gateway: an upstream must be another gateway")
		}
	}
	sec := upstreamSecret{Name: deviceName, Token: tok, Key: *key, Device: newDeviceKey()}
	data, _ := json.Marshal(sec)
	where, err := keySet(h.s.cfgDir, upstreamKeyName(name), string(data))
	if err != nil {
		return coreapi.UpstreamInfo{}, errors.New("cannot store the credentials: " + err.Error())
	}
	_, id := h.deviceOf(sec)
	return coreapi.UpstreamInfo{Device: id, Where: where}, nil
}

func (h upstreamHost) Info(name string) coreapi.UpstreamInfo {
	sec, err := h.secret(name)
	if err != nil {
		return coreapi.UpstreamInfo{}
	}
	_, id := h.deviceOf(sec)
	return coreapi.UpstreamInfo{Device: id}
}

func (h upstreamHost) Available() bool { return true }

func (h upstreamHost) Forget(name string) error {
	item := upstreamKeyName(name)
	if err := keyDel(h.s.cfgDir, item); err != nil { // the store said no: the token and the device key are still in it
		return errors.New("still in the secret store, item " + item + ": " + err.Error())
	}
	if v, where := keyGet(h.s.cfgDir, item); v != "" {
		return errors.New("still in " + where + ", item " + item)
	}
	return nil
}

// Do: one request to the upstream over a fresh encrypted connection made in memory. Redirects are never followed, an answer is read up to
// 8 MiB, and nothing in it is trusted by this function.
func (h upstreamHost) Do(ctx context.Context, name, method, path string, header map[string]string, body []byte) (int, []byte, time.Duration, error) {
	sec, err := h.secret(name)
	if err != nil {
		return 0, nil, 0, err
	}
	if h.s.core == nil {
		return 0, nil, 0, errors.New("the project database is not available")
	}
	up, err := h.s.core.GetUpstream(ctx, name)
	if err != nil {
		return 0, nil, 0, err
	}
	key, err := fromBase64url(sec.Key)
	if err != nil || len(key) != 32 {
		return 0, nil, 0, errors.New("invalid upstream key")
	}
	dev, _ := h.deviceOf(sec)
	np, err := newNoiseProxy("http://"+up.Address, key, dev)
	if err != nil {
		return 0, nil, 0, err
	}
	hc := &http.Client{Timeout: 60 * time.Second, CheckRedirect: noRedirect, Transport: &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			a, b := net.Pipe()
			go np.serve(b) // the proxy's connection handler, on an in-memory pipe: no listener
			return a, nil
		},
	}}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://upstream"+path, rd)
	if err != nil {
		return 0, nil, 0, err
	}
	req.Header.Set("authorization", "Bearer "+sec.Token)
	req.Header.Set("X-Minigw-Client", "gateway, minigw "+version) // the OS field says what it is: a gateway
	if body != nil {
		req.Header.Set("content-type", "application/json")
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return 0, nil, 0, errors.New("no answer from the upstream")
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, coreapi.MaxCalloutBody)) // 1 MiB and one byte: the core refuses more
	if err != nil {
		return 0, nil, 0, errors.New("no answer from the upstream")
	}
	var wait time.Duration
	if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s > 0 {
		wait = time.Duration(s) * time.Second
	}
	return resp.StatusCode, data, wait, nil
}
