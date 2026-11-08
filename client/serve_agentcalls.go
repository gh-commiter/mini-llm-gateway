package main

// Agent vpraša drug model istega prehoda (docs/plans/plan-agent-calls.md). Za vsako izvajanje agenta s stikalom
// »May ask other models« prehod odpre svoja vrata na 127.0.0.1 in žeton, veljavna le med izvajanjem
// (MINIGW_AGENT_URL, žeton v datoteki MINIGW_AGENT_AUTH_FILE). Zahteve tam dobijo napravo, sejo in mapo klicatelja; veriga agentov je le v
// kontekstu (ne v glavi), globina največ 2 in brez ponovitve vrste agenta.

import (
	"bufio"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"time"
)

// agentRun: eno izvajanje agenta z dostopom do drugih modelov.
type agentRun struct {
	kind               string   // claude, codex, grok, copilot
	chain              []string // vrste agentov od zahteve odjemalca do tega izvajanja (vključno s tem)
	device             *serveDevice
	session, cwd       string // seja in mapa odjemalca (glave izvirne zahteve)
	role               string // vloga klicatelja (--minigw-role); vprašani agent je ne more spremeniti
	dir                string // mapa izvajanja: kopija sync, klon worktree ali mapa seje
	readOnly           bool   // mapa le za branje (--minigw-ro): tudi za vprašanega agenta
	wt, maven, objects string // worktree: klon, Maven in objekti zrcala klicatelja ("" = ni worktree)
	shared             bool   // mapa je prišla od odjemalca (sync, worktree); ne privzeta mapa prehoda (L1)
	token, url         string
	authFile, ownDir   string // M3: žeton v datoteki 0600 v začasni mapi izvajanja (ne v okolju); ownDir: mapa, ki jo izbriše stop
	srv                *http.Server
	slots              chan struct{} // M1: največ maxNested hkratnih vprašanj tega izvajanja
	root               atomic.Int64  // M2: PID procesa agenta (0 = še ne teče); odjemalec mora biti v njegovem drevesu
}

// maxNested (M1): hkratna vprašanja drugim modelom na izvajanje; vsa vgnezdena izvajanja štejejo tudi v s.procs.
const maxNested = 2

// started: proces agenta teče (M2: žeton velja le za njegovo drevo procesov).
func (run *agentRun) started(pid int) {
	if run != nil {
		run.root.Store(int64(pid))
	}
}

// via: veriga za dnevnik (L3), npr. »claude→codex«.
func (run *agentRun) via() string { return strings.Join(run.chain, "→") }

type agentRunKey struct{}

// runOf: izvajanje, iz katerega pride zahteva (vrata agenta), ali nil (navadna zahteva).
func runOf(r *req) *agentRun { p, _ := r.Context().Value(agentRunKey{}).(*agentRun); return p }

// agentChainHeader: zunanja zahteva z verigo je zavrnjena (veriga ni nikoli v glavi).
const agentChainHeader = "X-Minigw-Agent-Chain"

// askOthers: stikalo »May ask other models« za vrsto agenta (Claude CLI le z »Allow local tools«: brez Bash ne more klicati minigw).
func (c serveConfig) askOthers(kind string) bool {
	switch kind {
	case "claude":
		return c.ClaudeAskOthers && c.AllowLocalTools
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

// startRun: vrata in žeton za izvajanje, če ima agent stikalo; nil, ko ga nima. Okolje gre agentu. `secretDir`: začasna
// mapa izvajanja, ki jo agent vidi (sandbox), za datoteko z žetonom; "" = nova v Scratch/tmp.
func (s *server) startRun(r *req, kind, dir string, readOnly, shared bool, wt, maven, objects, secretDir string) (*agentRun, []string) {
	if !s.conf().askOthers(kind) {
		return nil, nil
	}
	ownDir := ""
	if secretDir == "" {
		d, err := os.MkdirTemp(s.conf().scratch("tmp"), "run-*")
		if err != nil {
			s.log.Printf("%s agent calls: %v", r.rid, err)
			return nil, nil
		}
		secretDir, ownDir = d, d
	}
	token := randomToken()
	authFile := filepath.Join(secretDir, "minigw-agent-auth")
	if err := os.WriteFile(authFile, []byte(token), 0o600); err != nil {
		s.log.Printf("%s agent calls: %v", r.rid, err)
		os.RemoveAll(ownDir)
		return nil, nil
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		s.log.Printf("%s agent calls: %v", r.rid, err)
		os.Remove(authFile)
		os.RemoveAll(ownDir)
		return nil, nil
	}
	run := &agentRun{kind: kind, device: r.conn.device, session: r.h("x-claude-mini-session"), cwd: r.h("x-claude-mini-cwd"), role: reqRole(r),
		dir: dir, readOnly: readOnly, shared: shared, wt: wt, maven: maven, objects: objects, token: token, url: "http://" + ln.Addr().String(),
		authFile: authFile, ownDir: ownDir, slots: make(chan struct{}, maxNested)}
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
	env := []string{"MINIGW_AGENT_URL=" + run.url, "MINIGW_AGENT_AUTH_FILE=" + run.authFile}
	if s.conf().askOnPath(dir) { // askNote: »minigw« je ta minigw.exe
		env = append(env, "PATH="+filepath.Dir(minigwExe())+";"+os.Getenv("PATH"))
	}
	return run, env
}

// stop: konec izvajanja: vrata zaprta, žeton neveljaven in izbrisan (tudi tekoče vgnezdene zahteve se končajo).
func (run *agentRun) stop() {
	if run != nil {
		run.srv.Close()
		os.Remove(run.authFile)
		if run.ownDir != "" {
			os.RemoveAll(run.ownDir)
		}
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
		// M2: žeton velja le za procese tega izvajanja (agent, njegovi ukazi, posrednik sandboxa Claude Code v njem).
		c, _ := hr.Context().Value(connKey{}).(*serveConn)
		ours, ok1 := c.LocalAddr().(*net.TCPAddr)
		peer, ok2 := c.RemoteAddr().(*net.TCPAddr)
		if root := int(run.root.Load()); root == 0 || !ok1 || !ok2 || !peerInTree(ours, peer, root) {
			writeError(w, "", 403, "Not a process of this agent run")
			return
		}
		select { // M1
		case run.slots <- struct{}{}:
			defer func() { <-run.slots }()
		default:
			writeError(w, "", 429, fmt.Sprintf("This agent already has %d requests to other models running", maxNested))
			return
		}
		// Identiteta klicatelja: žeton prehoda (za overitev), seja in mapa izvirne zahteve; brez mape za deljenje.
		hr.Header.Set("Authorization", "Bearer "+s.conf().Token)
		hr.Header.Del("X-Api-Key")
		hr.Header.Del(agentChainHeader)
		hr.Header.Del("X-Claude-Mini-Share")
		hr.Header.Set("X-Claude-Mini-Session", run.session)
		hr.Header.Set("X-Claude-Mini-Cwd", run.cwd)
		hr.Header.Del("X-Claude-Mini-Role") // vloga iz izvajanja, ne od vprašanega agenta
		if run.role != "" {
			hr.Header.Set("X-Claude-Mini-Role", run.role)
		}
		s.ServeHTTP(w, hr.WithContext(context.WithValue(hr.Context(), agentRunKey{}, run)))
	})
}

// minigwExe: ta program (z razrešenimi povezavami): agent z njim vpraša druge modele, sandbox ga mora videti.
func minigwExe() string {
	exe, err := os.Executable()
	if err != nil {
		return "minigw"
	}
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	if isTrayExe(exe) { // pladenj nima konzole in z -p ne bi bil minigw: minigw.exe ob njem
		exe = filepath.Join(filepath.Dir(exe), "minigw.exe")
	}
	return exe
}

// askOnPath: Windows, mapa minigw.exe gre na začetek PATH izvajanja in ukaz »minigw« je vnaprej dovoljen (askRules), da
// se navodilo ujema s pravilom (brez peskovnika Claude Code tam Bash sicer potrebuje odobritev, ki je v -p ni). Ne, če
// je mapa v dovoljeni mapi, mapi seje ali Scratch (L1): agent bi tja podtaknil »minigw« ali git.exe, ki bi tekla brez
// odobritve; takrat navodilo z absolutno potjo in brez pravila.
func (c serveConfig) askOnPath(dir string) bool {
	return runtime.GOOS == "windows" && !agentWritable(filepath.Dir(minigwExe()), append(slices.Clone(c.AllowedFolders), dir, c.Scratch))
}

// agentWritable: mapa d je v eni od map (kanonično), ki jih agent lahko spreminja.
func agentWritable(d string, folders []string) bool {
	return slices.ContainsFunc(folders, func(f string) bool { return f != "" && under(realPath(d), realPath(f)) })
}

// askNote: navodilo agentu z vrati izvajanja; onPath (askOnPath): »minigw« iz PATH izvajanja, sicer absolutna pot.
func askNote(exe string, onPath bool) string {
	cmd := "\"" + exe + "\""
	if onPath {
		cmd = "minigw"
	}
	return "You may ask another model of this gateway: `" + cmd + " -p --model <id> \"…\"`; `" + cmd + " models` lists them."
}

// askRules: pravila, ki agentu Claude na Windows dovolijo le ukaz iz askNote (le z askOnPath); Claude Code na Windows
// ukaze praviloma poganja z orodjem PowerShell, ki ima svoja pravila (Bash(...) zanj ne velja).
const askRules = "Bash(minigw -p:*),Bash(minigw models:*),PowerShell(minigw -p *),PowerShell(minigw models *)"

// sandboxAllowRead: sandbox Claude Code (--settings) sme brati še te poti.
func sandboxAllowRead(args []string, paths ...string) []string {
	for i := 0; i+1 < len(args); i++ {
		var st map[string]any
		if args[i] != "--settings" || json.Unmarshal([]byte(args[i+1]), &st) != nil {
			continue
		}
		if sb, _ := st["sandbox"].(map[string]any); sb != nil {
			if fs, _ := sb["filesystem"].(map[string]any); fs != nil {
				fs["allowRead"] = append(toStrings(fs["allowRead"]), paths...)
			}
		}
		b, _ := json.Marshal(st)
		args[i+1] = string(b)
	}
	return args
}

// nestedNote: navodilo vprašanemu agentu (mapa klicatelja).
func nestedNote(p *agentRun) string {
	return "Another agent on this gateway (" + strings.Join(p.chain, " → ") + ") asks you this. You work in its folder " + p.dir +
		", which is the user's folder " + p.cwd + " on this gateway: map paths under " + p.cwd + " to it. Answer the question; " +
		"change files only if asked."
}
