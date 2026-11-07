package main

// Agent vpraša drug model istega prehoda (docs/plans/plan-agent-calls.md). Za vsako izvajanje agenta s stikalom
// »May ask other models« prehod odpre svoja vrata na 127.0.0.1 in žeton, veljavna le med izvajanjem
// (MINIGW_AGENT_URL, MINIGW_AGENT_TOKEN). Zahteve tam dobijo napravo, sejo in mapo klicatelja; veriga agentov je le v
// kontekstu (ne v glavi), globina največ 2 in brez ponovitve vrste agenta.

import (
	"bufio"
	"context"
	"crypto/subtle"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"
)

// agentRun: eno izvajanje agenta z dostopom do drugih modelov.
type agentRun struct {
	kind               string   // claude, codex, grok, copilot
	chain              []string // vrste agentov od zahteve odjemalca do tega izvajanja (vključno s tem)
	device             *serveDevice
	session, cwd       string // seja in mapa odjemalca (glave izvirne zahteve)
	dir                string // mapa izvajanja: kopija sync, klon worktree ali mapa seje
	readOnly           bool   // mapa le za branje (--minigw-ro): tudi za vprašanega agenta
	wt, maven, objects string // worktree: klon, Maven in objekti zrcala klicatelja ("" = ni worktree)
	token, url         string
	srv                *http.Server
}

type agentRunKey struct{}

// runOf: izvajanje, iz katerega pride zahteva (vrata agenta), ali nil (navadna zahteva).
func runOf(r *req) *agentRun { p, _ := r.Context().Value(agentRunKey{}).(*agentRun); return p }

// agentChainHeader: zunanja zahteva z verigo je zavrnjena (veriga ni nikoli v glavi).
const agentChainHeader = "X-Minigw-Agent-Chain"

// askOthers: stikalo »May ask other models« za vrsto agenta.
func (c serveConfig) askOthers(kind string) bool {
	switch kind {
	case "claude":
		return c.ClaudeAskOthers
	case "codex":
		return c.Codex.AskOthers
	case "grok":
		return c.Grok.AskOthers
	case "copilot":
		return c.Copilot.AskOthers
	}
	return false
}

// chainRefusal: zahteva iz izvajanja za agenta vrste kind: "" = dovoljeno, sicer razlog (403).
func chainRefusal(r *req, kind string) string {
	p := runOf(r)
	switch {
	case p == nil:
		return ""
	case len(p.chain) >= 2:
		return "an agent asked by another agent cannot ask further models (" + strings.Join(p.chain, " → ") + ")"
	case slices.Contains(p.chain, kind):
		return kind + " is already working on this request (" + strings.Join(p.chain, " → ") + "): an agent cannot ask its own kind"
	}
	return ""
}

// startRun: vrata in žeton za izvajanje, če ima agent stikalo; nil, ko ga nima. Okolje gre agentu.
func (s *server) startRun(r *req, kind, dir string, readOnly bool, wt, maven, objects string) (*agentRun, []string) {
	if !s.conf().askOthers(kind) {
		return nil, nil
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		s.log.Printf("%s agent calls: %v", r.rid, err)
		return nil, nil
	}
	run := &agentRun{kind: kind, device: r.conn.device, session: r.h("x-claude-mini-session"), cwd: r.h("x-claude-mini-cwd"),
		dir: dir, readOnly: readOnly, wt: wt, maven: maven, objects: objects, token: randomToken(), url: "http://" + ln.Addr().String()}
	if p := runOf(r); p != nil {
		run.chain = append(slices.Clone(p.chain), kind)
	} else {
		run.chain = []string{kind}
	}
	peer := r.conn.peer // naslov izvirne zahteve: ista pravila naprave in loopback kot za klicatelja
	run.srv = &http.Server{Handler: s.runHandler(run), ReadHeaderTimeout: 30 * time.Second, MaxHeaderBytes: 64 << 10,
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			return context.WithValue(ctx, connKey{}, &serveConn{Conn: c, br: bufio.NewReader(c), peer: peer, device: run.device})
		}}
	go run.srv.Serve(ln)
	return run, []string{"MINIGW_AGENT_URL=" + run.url, "MINIGW_AGENT_TOKEN=" + run.token}
}

// stop: konec izvajanja: vrata zaprta, žeton neveljaven (tudi tekoče vgnezdene zahteve se končajo).
func (run *agentRun) stop() {
	if run != nil {
		run.srv.Close()
	}
}

// runAllowed: na vratih agenta le modeli in sporočila (ne skrbniški API, mape, nastavitve).
var runAllowed = map[string]bool{"GET /v1/models": true, "POST /v1/messages": true, "POST /v1/messages/count_tokens": true}

func (s *server) runHandler(run *agentRun) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, hr *http.Request) {
		got := strings.TrimPrefix(hr.Header.Get("Authorization"), "Bearer ")
		if hr.Header.Get("Authorization") == "" {
			got = hr.Header.Get("X-Api-Key")
		}
		if subtle.ConstantTimeCompare([]byte(got), []byte(run.token)) != 1 {
			writeError(w, "", 401, "Invalid token for this agent run")
			return
		}
		if !runAllowed[hr.Method+" "+hr.URL.Path] {
			writeError(w, "", 404, "Not available to agents: "+hr.Method+" "+hr.URL.Path)
			return
		}
		// Identiteta klicatelja: žeton prehoda (za overitev), seja in mapa izvirne zahteve; brez mape za deljenje.
		hr.Header.Set("Authorization", "Bearer "+s.conf().Token)
		hr.Header.Del("X-Api-Key")
		hr.Header.Del(agentChainHeader)
		hr.Header.Del("X-Claude-Mini-Share")
		hr.Header.Set("X-Claude-Mini-Session", run.session)
		hr.Header.Set("X-Claude-Mini-Cwd", run.cwd)
		s.ServeHTTP(w, hr.WithContext(context.WithValue(hr.Context(), agentRunKey{}, run)))
	})
}

// nestedNote: navodilo vprašanemu agentu (mapa klicatelja).
func nestedNote(p *agentRun) string {
	return "Another agent on this gateway (" + strings.Join(p.chain, " → ") + ") asks you this. You work in its folder " + p.dir +
		", which is the user's folder " + p.cwd + " on this gateway: map paths under " + p.cwd + " to it. Answer the question; " +
		"change files only if asked."
}
