package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"minigw/core"
)

// A deterministic fake agent for the run tests: it performs the steps of a JSON plan against the run port
// (MINIGW_AGENT_URL, token from MINIGW_AGENT_AUTH_FILE) and appends one JSON line per request to <plan>.out.
// Steps: {"path","method","headers","body"} | {"signal": file} | {"wait": file}. It also dumps its arguments, its
// system prompt note and whether it got a run port, so that tests can see what the gateway gave it.
type runStep struct {
	Method  string            `json:"method,omitempty"`
	Path    string            `json:"path,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    string            `json:"body,omitempty"`
	Signal  string            `json:"signal,omitempty"`
	Wait    string            `json:"wait,omitempty"`
}

type runResult struct {
	Step   int    `json:"step"`
	Status int    `json:"status"`
	Body   string `json:"body"`
}

func fakeRunPlan(plan string) {
	if plan == "" {
		return
	}
	var steps []runStep
	if b, err := os.ReadFile(plan); err != nil || json.Unmarshal(b, &steps) != nil {
		return
	}
	tok, _ := os.ReadFile(os.Getenv("MINIGW_AGENT_AUTH_FILE"))
	client := &http.Client{Transport: &http.Transport{Proxy: agentProxy}, Timeout: 20 * time.Second}
	for i, st := range steps {
		switch {
		case st.Signal != "":
			os.WriteFile(st.Signal, []byte("x"), 0o644)
		case st.Wait != "":
			for n := 0; n < 600 && !fakeExists(st.Wait); n++ {
				time.Sleep(50 * time.Millisecond)
			}
		default:
			m := st.Method
			if m == "" {
				m = "GET"
			}
			req, _ := http.NewRequest(m, os.Getenv("MINIGW_AGENT_URL")+st.Path, strings.NewReader(st.Body))
			req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(tok)))
			req.Header.Set("content-type", "application/json")
			for k, v := range st.Headers {
				req.Header.Set(k, v)
			}
			res := runResult{Step: i}
			if resp, err := client.Do(req); err == nil {
				b, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				res.Status, res.Body = resp.StatusCode, string(b)
			}
			line, _ := json.Marshal(res)
			fakeAppend(plan+".out", string(line)+"\n")
		}
	}
}

func fakeDump(dump string, args []string) {
	if dump == "" {
		return
	}
	fakeArgs(filepath.Join(dump, "args.txt"), args)
	note := ""
	if f := fakeFlag(args, "--append-system-prompt-file"); f != "" {
		b, _ := os.ReadFile(f)
		note = string(b)
	}
	os.WriteFile(filepath.Join(dump, "note.txt"), []byte(note), 0o644)
	os.WriteFile(filepath.Join(dump, "port.txt"), []byte(os.Getenv("MINIGW_AGENT_URL")), 0o644)
}

var fakeClaudeRun = fake("claude-run", func(p fakeParams, args []string) {
	fakeGuard(args)
	fakeStdin()
	fakeDump(p["dump"], args)
	fakeRunPlan(p["plan"])
	// a nested ask of a second agent, if the plan file for it exists
	if m := p["ask"]; m != "" {
		fakeAsk(m)
	}
	fakeClaudeDone(3)
})

var fakeCodexRun = fake("codex-run", func(p fakeParams, args []string) {
	if len(args) > 2 && args[2] == "debug" {
		fmt.Println(`{"models":[]}`)
		return
	}
	out := fakeFlag(args, "-o")
	fakeStdin()
	fakeRunPlan(p["plan"])
	fmt.Println(`{"type":"thread.started","thread_id":"t1"}`)
	fakeWrite(out, "codex answer\n")
})

type runFx struct {
	t      *testing.T
	f      *coreFx
	tmp    string
	plan   string
	dump   string
	proxy  map[string]connection
	nps    map[string]*noiseProxy
	addr   string
	shares map[string]*shareClient
	model  string // the model of agent(); default claude-cli-haiku
}

// newRunFx: the gateway with the core fixture, a fake claude (and codex when withCodex) and a real TCP listener,
// so members come through Noise tunnels with their device keys.
func newRunFx(t *testing.T, ask, withCodex bool) *runFx {
	t.Helper()
	x := &runFx{t: t, tmp: t.TempDir(), proxy: map[string]connection{}, nps: map[string]*noiseProxy{}, shares: map[string]*shareClient{}}
	x.plan, x.dump = filepath.Join(x.tmp, "plan.json"), filepath.Join(x.tmp, "dump")
	os.MkdirAll(x.dump, 0o755)
	params := fakeParams{"plan": x.plan, "dump": x.dump}
	claude := fakeCLI(t, x.tmp, "claude", fakeClaudeRun, params)
	f := &coreFx{t: t, dev: map[string]*device{}, uid: map[string]int64{}, pid: map[string]int64{}}
	var list []serveDevice
	for _, n := range []string{"ana", "ana2", "bor", "fani", "eva", "cene", "dana"} {
		f.dev[n] = newDevice(n)
		list = append(list, serveDevice{ID: devID(f.dev[n]), Name: n})
	}
	f.s, f.key = testServer(t, func(c *serveConfig) {
		c.Devices = list
		c.Claude = claude
		c.AllowUnsandboxedBash = runtime.GOOS != "linux" || bwrapReady(t.TempDir()) == ""
		if ask {
			c.ClaudeAskOthers, c.AllowLocalTools = true, true
		}
		if withCodex {
			t.Setenv("CODEX_HOME", t.TempDir())
			c.Codex = agentCLI{Enabled: true, Executable: fakeCLI(t, x.tmp, "codex", fakeCodexRun, fakeParams{"plan": x.plan + ".codex"}), AskOthers: true}
		}
	})
	f.s.openCore()
	t.Cleanup(f.s.closeCore)
	if f.s.core == nil {
		t.Fatal(f.s.coreErr)
	}
	f.setup()
	x.f = f
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	x.addr = "http://" + ln.Addr().String()
	go f.s.serve(ln)
	return x
}

func (x *runFx) setPlan(steps ...runStep) {
	b, _ := json.Marshal(steps)
	os.WriteFile(x.plan, b, 0o644)
	os.Remove(x.plan + ".out")
}

func (x *runFx) results() []runResult {
	var out []runResult
	b, _ := os.ReadFile(x.plan + ".out")
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var r runResult
		if json.Unmarshal([]byte(l), &r) == nil && l != "" {
			out = append(out, r)
		}
	}
	return out
}

func (x *runFx) conn(user string) connection {
	if c, ok := x.proxy[user]; ok {
		return c
	}
	np, err := newNoiseProxy(x.addr, x.f.s.key.PublicKey().Bytes(), x.f.dev[user])
	if err == nil {
		err = np.start()
	}
	if err != nil {
		x.t.Fatal(err)
	}
	x.t.Cleanup(np.stop)
	x.nps[user] = np
	c := connection{URL: np.localURL(), Token: "tok"}
	x.proxy[user] = c
	return c
}

// agent sends one agent request (claude-cli with tools) as user from a folder, project 0 = no header.
func (x *runFx) agent(user, dir, sid string, project int64, history ...string) (int, string) {
	x.t.Helper()
	c := x.conn(user)
	key := user + "|" + sid
	if x.shares[key] == nil {
		sc := &shareClient{c: c, session: sid, root: dir, sync: true, synced: map[string]syncEntry{}, reported: map[string]bool{}}
		if !sc.register() {
			x.t.Fatal("register: " + sc.refusal)
		}
		sc.start(2)
		x.t.Cleanup(sc.unregister)
		x.shares[key] = sc
	}
	var msgs []string
	roles := []string{"user", "assistant"}
	for i, h := range history {
		msgs = append(msgs, fmt.Sprintf(`{"role":%q,"content":%q}`, roles[i%2], h))
	}
	model := x.model
	if model == "" {
		model = "claude-cli-haiku"
	}
	body := `{"model":"` + model + `","tools":[{"name":"x"}],"messages":[` + strings.Join(msgs, ",") + `]}`
	h := map[string]string{"x-claude-mini-session": sid, "x-claude-mini-cwd": dir, "content-type": "application/json"}
	x.nps[user].project = "" // minigw's proxy sets the project header (L1); a header from the client would be dropped
	if project != 0 {
		x.nps[user].project = fmt.Sprint(project)
	}
	st, data := call(c, "POST", "/v1/messages", h, []byte(body), 60*time.Second)
	return st, string(data)
}

func (x *runFx) dumped(name string) string {
	b, _ := os.ReadFile(filepath.Join(x.dump, name))
	return string(b)
}

func noteIDs(t *testing.T, body string) map[int64]bool {
	t.Helper()
	var l struct{ Notes []struct{ ID int64 } }
	if err := json.Unmarshal([]byte(body), &l); err != nil {
		t.Fatalf("not a note list: %q", body)
	}
	m := map[int64]bool{}
	for _, n := range l.Notes {
		m[n.ID] = true
	}
	return m
}

// --- case 12 and 13: what a run reads, forged project ids, writes, ask-others off ---

func TestRunReadsOnlyItsProjectAndGrants(t *testing.T) {
	x := newRunFx(t, false, false) // "May ask other models" is off: the notes capability exists anyway
	f := x.f
	f.ok(f.s.core.SetMember(t0, f.pid["eureko"], f.uid["cene"], core.Developer))
	f.project2("side", "cene", core.Developer)
	own, _ := f.create("cene", "eureko", "data")
	priv, _ := f.create("cene", "eureko", "private")
	fs, _ := f.create("ana", "fakture", "structure")
	fd, _ := f.create("ana", "fakture", "data")
	sideNote, _ := f.create("cene", "side", "data")
	forged := map[string]string{projectHeader: fmt.Sprint(f.pid["side"])}
	x.setPlan(
		runStep{Path: "/minigw/v1/notes"},                                                                                                // 0 the run's list
		runStep{Path: fmt.Sprintf("/minigw/v1/notes/%d", own)},                                                                           // 1 own project note
		runStep{Path: fmt.Sprintf("/minigw/v1/notes/%d", fs)},                                                                            // 2 fakture structure through the grant
		runStep{Path: fmt.Sprintf("/minigw/v1/notes/%d", fd)},                                                                            // 3 fakture data: no
		runStep{Path: fmt.Sprintf("/minigw/v1/notes/%d", priv)},                                                                          // 4 cene's private note: no
		runStep{Path: fmt.Sprintf("/minigw/v1/notes/%d", sideNote)},                                                                      // 5 a project cene is also a member of: no
		runStep{Path: fmt.Sprintf("/minigw/v1/projects/%d/notes", f.pid["side"])},                                                        // 6 project id in the path
		runStep{Path: fmt.Sprintf("/minigw/v1/notes?project=%d&project_id=%d", f.pid["side"], f.pid["side"])},                            // 7 in the query
		runStep{Path: "/minigw/v1/notes", Headers: forged},                                                                               // 8 in a header
		runStep{Path: "/minigw/v1/notes", Method: "POST", Body: fmt.Sprintf(`{"project":%d,"class":"data","title":"x"}`, f.pid["side"])}, // 9 a write with a body
		runStep{Path: "/minigw/v1/notes", Method: "GET", Body: fmt.Sprintf(`{"project":%d}`, f.pid["side"])},                             // 10 in a body
		runStep{Path: fmt.Sprintf("/minigw/v1/notes/%d", own), Method: "PUT", Body: `{"revision":1,"class":"data","title":"x"}`},
		runStep{Path: fmt.Sprintf("/minigw/v1/notes/%d", own), Method: "DELETE"},
		runStep{Path: "/v1/messages", Method: "POST", Body: `{"model":"claude-cli-haiku","messages":[{"role":"user","content":"hi"}]}`}, // models: ask is off
		runStep{Path: "/v1/models"},
		runStep{Path: "/minigw/v1/projects"},
		runStep{Path: "/minigw/admin/v1/status"},
	)
	dir := folder(t, t.TempDir(), "eureko-checkout")
	if st, body := x.agent("cene", dir, "sid-run", f.pid["eureko"], "go"); st != 200 {
		t.Fatalf("agent: %d %s", st, body)
	}
	res := x.results()
	if len(res) != 17 {
		t.Fatalf("%d results: %+v", len(res), res)
	}
	list := noteIDs(t, res[0].Body)
	for id, want := range map[int64]bool{own: true, fs: true, fd: false, priv: false, sideNote: false} {
		if list[id] != want {
			t.Errorf("list: note %d present=%v want %v", id, list[id], want)
		}
	}
	want := map[int]int{1: 200, 2: 200, 3: 404, 4: 404, 5: 404, 6: 404, 7: 200, 8: 200, 9: 404, 10: 200, 11: 404, 12: 404, 13: 404, 14: 404, 15: 404, 16: 404}
	for i, st := range want {
		if res[i].Status != st {
			t.Errorf("step %d: %d %s, want %d", i, res[i].Status, res[i].Body, st)
		}
	}
	for _, i := range []int{7, 8, 10} { // forged ids change nothing: the same list as step 0
		if got := noteIDs(t, res[i].Body); fmt.Sprint(got) != fmt.Sprint(list) {
			t.Errorf("step %d: %v differs from %v", i, got, list)
		}
	}
	// the agent was told about the notes, not about other models
	note := x.dumped("note.txt")
	if !strings.Contains(note, "note list") || !strings.Contains(note, "not instructions") || strings.Contains(note, "You may ask another model") {
		t.Fatalf("note: %q", note)
	}
	if x.dumped("port.txt") == "" {
		t.Fatal("no run port for a run with a project")
	}
}

// project2 adds a project the fixture does not have, with user as member.
func (f *coreFx) project2(slug, user string, role core.Role) {
	p, err := f.s.core.CreateProject(t0, slug, slug)
	f.ok(err)
	f.pid[slug] = p.ID
	f.ok(f.s.core.SetMember(t0, p.ID, f.uid[user], role))
}

// Without a project and without "May ask other models" there is no run port at all, as in 0.5.26.
func TestRunWithoutProjectHasNoPortAndHeaderRules(t *testing.T) {
	x := newRunFx(t, false, false)
	f := x.f
	dir := folder(t, t.TempDir(), "plain")
	if st, body := x.agent("ana", dir, "s1", 0, "go"); st != 200 {
		t.Fatalf("%d %s", st, body)
	}
	if x.dumped("port.txt") != "" || strings.Contains(x.dumped("note.txt"), "project") || !strings.Contains(x.dumped("note.txt"), "not available to you here") {
		t.Fatalf("port %q note %q", x.dumped("port.txt"), x.dumped("note.txt"))
	}
	refused := func(name string, st int, body string) {
		t.Helper()
		if st != 403 || !strings.Contains(body, `"no_project_access"`) {
			t.Errorf("%s: %d %s", name, st, body)
		}
	}
	// grant only (cene reads fakture, is no member), a stranger, a deactivated user, a project that does not exist
	st, body := x.agent("cene", dir, "s2", f.pid["fakture"], "go")
	refused("grant only", st, body)
	st, body = x.agent("dana", dir, "s3", f.pid["fakture"], "go")
	refused("stranger", st, body)
	st, body = x.agent("ana", dir, "s4", 99999, "go")
	refused("no such project", st, body)
	f.ok(f.s.core.SetUserActive(t0, f.uid["eva"], false))
	st, body = x.agent("eva", dir, "s5", f.pid["fakture"], "go")
	refused("deactivated", st, body)
	// a device without a binding
	stranger := newDevice("stranger")
	f.ok(f.s.update(func(c *serveConfig) error {
		c.Devices = append(c.Devices, serveDevice{ID: devID(stranger), Name: "stranger"})
		return nil
	}))
	f.dev["stranger"] = stranger
	st, body = x.agent("stranger", dir, "s6", f.pid["fakture"], "go")
	refused("unbound", st, body)
	// a device that left the approved list
	f.s.adminDo("DELETE", "/devices/"+devID(f.dev["bor"]), nil)
	x.conn("bor")
	x.nps["bor"].project = fmt.Sprint(f.pid["fakture"])
	r, _ := call(x.conn("bor"), "POST", "/v1/messages", map[string]string{"x-claude-mini-session": "s7", "x-claude-mini-cwd": dir}, []byte(`{"model":"claude-cli-haiku","tools":[{"name":"x"}],"messages":[{"role":"user","content":"go"}]}`), 30*time.Second)
	if r != 403 && r != 0 {
		t.Errorf("removed device: %d", r)
	}
	// a member of the project: runs, with a port
	if st, body := x.agent("ana", dir, "s8", f.pid["fakture"], "go"); st != 200 {
		t.Fatalf("member: %d %s", st, body)
	}
	if x.dumped("port.txt") == "" {
		t.Fatal("member run without a port")
	}
}

func TestRunProjectRefusedWithoutDeviceProofAndHeaderOnModelsIgnored(t *testing.T) {
	x := newRunFx(t, false, false)
	f := x.f
	c := startLoopback(t, f.s) // plain loopback: no device key, so no principal
	st, data := call(c, "POST", "/v1/messages", map[string]string{projectHeader: fmt.Sprint(f.pid["fakture"]), "x-claude-mini-session": "p", "x-claude-mini-cwd": "/x"},
		[]byte(`{"model":"claude-cli-haiku","tools":[{"name":"x"}],"messages":[{"role":"user","content":"go"}]}`), 20*time.Second)
	if st != 403 || !strings.Contains(string(data), `"no_project_access"`) {
		t.Fatalf("%d %s", st, data)
	}
	// the model list is not an agent request: the header changes nothing
	if st, _ := call(c, "GET", "/v1/models", map[string]string{projectHeader: "1"}, nil, 5*time.Second); st != 200 {
		t.Fatalf("models: %d", st)
	}
	// the core is down: refused with 503, never run without the project
	x.f.s.stopUsage() // the usage writer reads s.core: stop it before the test takes the core away
	x.f.s.core = nil
	st2, body := x.agent("ana", folder(t, t.TempDir(), "d"), "s", f.pid["fakture"], "go")
	if st2 != 503 || !strings.Contains(body, "core_unavailable") {
		t.Fatalf("%d %s", st2, body)
	}
}

// --- case 10 (run part), 11, 14: revocation between two reads of one run ---

func TestRunEpochChangeBetweenReads(t *testing.T) {
	changes := map[string]func(x *runFx){
		"epoch bumped":       func(x *runFx) { x.f.s.core.BumpEpoch(t0, "test") },
		"device removed":     func(x *runFx) { x.f.s.adminDo("DELETE", "/devices/"+devID(x.f.dev["ana"]), nil) },
		"binding changed":    func(x *runFx) { x.f.s.core.SetBinding(t0, devID(x.f.dev["ana"]), x.f.uid["dana"]) },
		"membership removed": func(x *runFx) { x.f.s.core.RemoveMember(t0, x.f.pid["fakture"], x.f.uid["ana"]) },
		"user deactivated":   func(x *runFx) { x.f.s.core.SetUserActive(t0, x.f.uid["ana"], false) },
		"role lowered":       func(x *runFx) { x.f.s.core.SetMember(t0, x.f.pid["fakture"], x.f.uid["ana"], core.Reader) },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			x := newRunFx(t, false, false)
			f := x.f
			n, _ := f.create("bor", "fakture", "structure")
			ready, gate := filepath.Join(x.tmp, "ready"), filepath.Join(x.tmp, "gate")
			read := runStep{Path: fmt.Sprintf("/minigw/v1/notes/%d", n)}
			x.setPlan(read, runStep{Signal: ready}, runStep{Wait: gate}, read, read)
			dir := folder(t, t.TempDir(), "w")
			done := make(chan [2]any, 1)
			go func() { st, b := x.agent("ana", dir, "sid-e", f.pid["fakture"], "go"); done <- [2]any{st, b} }()
			for i := 0; i < 400 && !fakeExists(ready); i++ {
				time.Sleep(25 * time.Millisecond)
			}
			if !fakeExists(ready) {
				t.Fatal("the agent never got to its first read")
			}
			change(x)
			os.WriteFile(gate, []byte("x"), 0o644)
			select {
			case r := <-done:
				if r[0].(int) != 200 { // the run itself goes on
					t.Fatalf("run: %v %v", r[0], r[1])
				}
			case <-time.After(30 * time.Second):
				t.Fatal("timeout")
			}
			res := x.results()
			if len(res) != 3 || res[0].Status != 200 {
				t.Fatalf("%+v", res)
			}
			for _, r := range res[1:] { // the next reads of the old run fail, one fixed body
				if r.Status != 403 || !strings.Contains(r.Body, `"no_project_access"`) {
					t.Fatalf("after the change: %d %s", r.Status, r.Body)
				}
			}
		})
	}
}

// case 11: remove and re-approve a device, re-create a binding: an earlier run stays dead, a new run works.
func TestRunNoResurrection(t *testing.T) {
	x := newRunFx(t, false, false)
	f := x.f
	n, _ := f.create("bor", "fakture", "structure")
	ready, gate := filepath.Join(x.tmp, "ready"), filepath.Join(x.tmp, "gate")
	read := runStep{Path: fmt.Sprintf("/minigw/v1/notes/%d", n)}
	x.setPlan(read, runStep{Signal: ready}, runStep{Wait: gate}, read)
	dir := folder(t, t.TempDir(), "w")
	done := make(chan int, 1)
	go func() { st, _ := x.agent("ana", dir, "sid-r", f.pid["fakture"], "go"); done <- st }()
	for i := 0; i < 400 && !fakeExists(ready); i++ {
		time.Sleep(25 * time.Millisecond)
	}
	id := devID(f.dev["ana"])
	f.s.adminDo("DELETE", "/devices/"+id, nil) // removed ...
	f.s.adminDo("POST", "/devices", []byte(fmt.Sprintf(`{"id":%q,"name":"ana"}`, id)))
	f.ok(f.s.core.RemoveBinding(t0, id)) // ... re-approved, binding re-created
	f.ok(f.s.core.SetBinding(t0, id, f.uid["ana"]))
	os.WriteFile(gate, []byte("x"), 0o644)
	<-done
	if res := x.results(); len(res) != 2 || res[0].Status != 200 || res[1].Status != 403 {
		t.Fatalf("old run: %+v", res)
	}
	x.setPlan(read) // a new run works
	if st, b := x.agent("ana", dir, "sid-r2", f.pid["fakture"], "go"); st != 200 {
		t.Fatalf("%d %s", st, b)
	}
	if res := x.results(); len(res) != 1 || res[0].Status != 200 {
		t.Fatalf("new run: %+v", res)
	}
}

// --- case 13: a nested run inherits the capability and nothing from its own headers ---

func TestNestedRunInheritsTheCapability(t *testing.T) {
	x := newRunFx(t, true, true) // ask-others on: claude asks codex, which reads notes through its own run port
	f := x.f
	f.project2("side", "ana", core.Developer)
	fk, _ := f.create("bor", "fakture", "structure")
	sd, _ := f.create("ana", "side", "data")
	x.setPlan() // the claude run itself reads nothing
	codexPlan := x.plan + ".codex"
	forged := map[string]string{projectHeader: fmt.Sprint(f.pid["side"])}
	b, _ := json.Marshal([]runStep{{Path: "/minigw/v1/notes"}, {Path: "/minigw/v1/notes", Headers: forged}, {Path: fmt.Sprintf("/minigw/v1/notes/%d", sd)}})
	os.WriteFile(codexPlan, b, 0o644)
	os.Remove(codexPlan + ".out")
	// claude asks codex over the run port, and sends a forged project header with the nested request
	x.setPlan(runStep{Path: "/v1/messages", Method: "POST", Headers: forged,
		Body: `{"model":"claude-codex","max_tokens":10,"tools":[{"name":"x"}],"messages":[{"role":"user","content":"hi"}]}`})
	dir := folder(t, t.TempDir(), "w")
	if st, body := x.agent("ana", dir, "sid-n", f.pid["fakture"], "go"); st != 200 {
		t.Fatalf("%d %s", st, body)
	}
	if res := x.results(); len(res) != 1 || res[0].Status != 200 {
		t.Fatalf("nested ask: %+v", res)
	}
	var codex []runResult
	data, _ := os.ReadFile(codexPlan + ".out")
	for _, l := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var r runResult
		if json.Unmarshal([]byte(l), &r) == nil && l != "" {
			codex = append(codex, r)
		}
	}
	if len(codex) != 3 {
		t.Fatalf("codex results: %q", data)
	}
	for _, i := range []int{0, 1} { // the nested run reads FAKTURE (the root's project), also with the forged header
		ids := noteIDs(t, codex[i].Body)
		if codex[i].Status != 200 || !ids[fk] || ids[sd] {
			t.Errorf("codex step %d: %d %s", i, codex[i].Status, codex[i].Body)
		}
	}
	if codex[2].Status != 404 { // the other project's note, which only the forged header named
		t.Errorf("codex read of the side project's note: %d", codex[2].Status)
	}
}

// --- case 16 and 17: conversation isolation ---

func TestConversationScopeWithoutProjectIsTheOldKey(t *testing.T) { // case 17: byte for byte
	for _, c := range []struct{ kind, owner, cwd string }{{"codex", "devA", "/home/x/proj"}, {"grok", "local", ""}, {"copilot", "d", "C:\\w\\p"}} {
		if got, want := agentScope(c.kind, c.owner, c.cwd, nil), c.kind+"|"+c.owner+"|"+c.cwd; got != want {
			t.Errorf("agentScope: %q, want %q", got, want)
		}
	}
	if got, want := cliScope("sid1", "/gw/copy", nil), "sid1|/gw/copy"; got != want {
		t.Errorf("cliScope: %q, want %q", got, want)
	}
	if transcriptKey(nil, cliScope("s", "/d", nil)) != transcriptKey(nil, "s|/d") {
		t.Error("transcript key changed for a request without a project")
	}
}

// Review A1: no combination of a project and a hostile folder or session id equals a key of another namespace or
// of another project scope. Every keying site (agentScope for Codex, Grok, Copilot; cliScope for Claude CLI).
func TestConversationScopeCannotCollide(t *testing.T) {
	p := &projectCtx{user: 1, project: 2, epoch: 3}
	sites := map[string]func(a, b string, p *projectCtx) string{
		"agentScope": func(a, b string, p *projectCtx) string { return agentScope("codex", a, b, p) },
		"cliScope":   func(a, b string, p *projectCtx) string { return cliScope(a, b, p) },
	}
	hostile := []string{"/w|u1|p2|e3", "/w", "|u1|p2|e3", "\x00project\x00", "/w\x00", `["agent"]`, ""}
	for name, f := range sites {
		seen := map[string]string{}
		add := func(label, key string) {
			if prev, ok := seen[key]; ok {
				t.Errorf("%s: %s and %s give the same key %q", name, prev, label, key)
			}
			seen[key] = label
		}
		for _, a := range []string{"owner", "own|er", "owner|", ""} {
			for _, b := range hostile {
				add(fmt.Sprintf("no project (%q, %q)", a, b), f(a, b, nil))
				add(fmt.Sprintf("project (%q, %q)", a, b), f(a, b, p))
				add(fmt.Sprintf("project 2 (%q, %q)", a, b), f(a, b, &projectCtx{user: 1, project: 3, epoch: 3}))
			}
		}
		// the exact collision of the review
		if f("owner", "/w", p) == f("owner", "/w|u1|p2|e3", nil) {
			t.Errorf("%s: the review's collision", name)
		}
		if (&projectCtx{user: 1, project: 23, epoch: 4}).namespace("cli", "a") == (&projectCtx{user: 12, project: 3, epoch: 4}).namespace("cli", "a") {
			t.Error("numbers run together")
		}
	}
	// and the old concatenation really was ambiguous (the test is not vacuous)
	if "owner|/w"+"|u1|p2|e3" != "owner|"+"/w|u1|p2|e3" {
		t.Fatal("the old key was injective")
	}
}

func TestConversationsAreNotResumedAcrossUserProjectOrEpoch(t *testing.T) {
	x := newRunFx(t, false, false)
	f := x.f
	f.project2("labs", "ana", core.Developer)
	x.setPlan()
	dir := folder(t, t.TempDir(), "w")
	resumed := func() bool { return strings.Contains(x.dumped("args.txt"), "--resume") }
	turn := func(user string, project int64) bool { // the same client session id and history every time
		st, body := x.agent(user, dir, "same-sid", project, "go", "done", "again")
		if st != 200 {
			t.Fatalf("%s/%d: %d %s", user, project, st, body)
		}
		return resumed()
	}
	first := func(user string, project int64) { // the turn that stores the conversation (history "go")
		if st, body := x.agent(user, dir, "same-sid", project, "go"); st != 200 {
			t.Fatalf("%d %s", st, body)
		}
	}
	// case 17: no project: resumed as in 0.5.26
	first("ana", 0)
	if !turn("ana", 0) {
		t.Fatal("a request without a project must resume its conversation")
	}
	// a project makes a new conversation, then that one resumes for itself
	if turn("ana", f.pid["fakture"]) {
		t.Fatal("a conversation without a project was resumed under a project")
	}
	first("ana", f.pid["fakture"])
	if !turn("ana", f.pid["fakture"]) {
		t.Fatal("same user, project and epoch must resume")
	}
	// the project changed (the explicit old session id is the same)
	if turn("ana", f.pid["labs"]) {
		t.Fatal("resumed across projects")
	}
	// the user changed through a rebinding of the same device
	f.ok(f.s.core.SetBinding(t0, devID(f.dev["ana"]), f.uid["bor"]))
	if turn("ana", f.pid["fakture"]) {
		t.Fatal("resumed across users")
	}
	f.ok(f.s.core.SetBinding(t0, devID(f.dev["ana"]), f.uid["ana"]))
	// the epoch changed
	first("ana", f.pid["fakture"])
	if !turn("ana", f.pid["fakture"]) {
		t.Fatal("control: same epoch resumes")
	}
	f.s.core.BumpEpoch(t0, "test")
	if turn("ana", f.pid["fakture"]) {
		t.Fatal("resumed across an epoch change")
	}
	// the project-less conversation is still there for a request without a project
	if !turn("ana", 0) {
		t.Fatal("the 0.5.26 conversation was lost")
	}
}

// The client: the project header of a request (section 3) and the read commands inside a run.
func TestClientSendsTheLinkedProjectHeader(t *testing.T) {
	state := filepath.Join(t.TempDir(), "projects.json")
	dir := folder(t, t.TempDir(), "w")
	sub := folder(t, dir, "src")
	c := connection{URL: "http://x", Token: "t"}
	key := "gwkey"
	c.Key = &key
	if h, err := projectHeaderFor(sub, c, state); h != "" || err != nil {
		t.Fatalf("no link: %q %v", h, err)
	}
	hint(t, dir, "fakture") // a hint alone never links: the request goes without a project
	if h, err := projectHeaderFor(sub, c, state); h != "" || err != nil {
		t.Fatalf("hint only: %q %v", h, err)
	}
	saveProjectState(state, projectState{Links: []projectLink{{Gateway: key, Project: 42, Slug: "fakture", Path: dir}}})
	if h, err := projectHeaderFor(sub, c, state); h != "x-minigw-project: 42" || err != nil {
		t.Fatalf("linked: %q %v", h, err)
	}
	other := "other-gateway"
	if h, _ := projectHeaderFor(sub, connection{Key: &other}, state); h != "" {
		t.Fatalf("another gateway's link applies: %q", h)
	}
	hint(t, dir, "eureko") // link + hint with another slug: the request is not sent
	if h, err := projectHeaderFor(sub, c, state); h != "" || err == nil || !strings.Contains(err.Error(), "linked to fakture") {
		t.Fatalf("hint differs: %q %v", h, err)
	}
	t.Setenv("MINIGW_AGENT_URL", "http://127.0.0.1:1") // inside a run the gateway inherits the project: nothing is sent
	af := filepath.Join(t.TempDir(), "auth")
	os.WriteFile(af, []byte("t\n"), 0o600)
	t.Setenv("MINIGW_AGENT_AUTH_FILE", af)
	if h, err := projectHeaderFor(sub, c, state); h != "" || err != nil {
		t.Fatalf("in a run: %q %v", h, err)
	}
}

func TestNoteReadCommandsInsideARun(t *testing.T) {
	x := newRunFx(t, false, false)
	f := x.f
	n, _ := f.create("ana", "fakture", "structure")
	priv, _ := f.create("ana", "fakture", "private")
	// a run with the notes capability, as the gateway makes it for a request with a project
	p, _ := f.s.core.Principal(t0, devID(f.dev["ana"]), true)
	epoch, _ := f.s.core.Epoch(t0)
	hr, _ := http.NewRequest("POST", "/v1/messages", nil)
	r := &req{Request: hr, conn: &serveConn{device: &serveDevice{ID: devID(f.dev["ana"])}, peer: "127.0.0.1"},
		project: &projectCtx{runID: "run1", user: p.UserID, device: devID(f.dev["ana"]), project: f.pid["fakture"], epoch: epoch}}
	run, _ := f.s.startRun(r, "claude", t.TempDir(), false, true, "", "", "", "")
	if run == nil {
		t.Fatal("no run for a project")
	}
	defer run.stop()
	run.started(os.Getpid())
	conn := connection{URL: run.url, Token: run.token}
	env := func() (*projectEnv, *out, *out) {
		o, e := &out{}, &out{}
		return &projectEnv{connect: func() (connection, string) { return conn, "" }, cwd: t.TempDir(), in: strings.NewReader(""), out: o, err: e,
			agent: true, runConn: &conn, do: realDo, sleep: func(time.Duration) {}}, o, e
	}
	e, o, _ := env()
	if err := e.note([]string{"list"}); err != nil || !strings.Contains(o.String(), "structure") || strings.Contains(o.String(), "private") {
		t.Fatalf("list: %v %q", err, o.String())
	}
	e, o, _ = env()
	if err := e.note([]string{"read", fmt.Sprint(n)}); err != nil || !strings.Contains(o.String(), "by ana") {
		t.Fatalf("read: %v %q", err, o.String())
	}
	e, _, _ = env()
	wantErr(t, e.note([]string{"read", fmt.Sprint(priv)}), "not found or not visible") // never a private note
	for _, args := range [][]string{{"push", "-"}, {"edit", "1", "-"}, {"rm", "1"}, {"list", "--project", "side"}} {
		e, _, _ = env()
		if err := e.note(args); err == nil {
			t.Errorf("note %v worked inside a run", args)
		}
	}
	e, _, _ = env()
	wantErr(t, e.project([]string{"list"}), "only minigw note list and note read work")
	// the epoch moves: the next read says so
	f.s.core.BumpEpoch(t0, "test")
	e, _, _ = env()
	wantErr(t, e.note([]string{"list"}), "no project access")
}

// Review L1: through minigw's proxy a header from the agent CLI's own settings never chooses the project.
func TestProxySetsTheProjectAndIgnoresAForgedHeader(t *testing.T) {
	x := newRunFx(t, false, false)
	f := x.f
	x.setPlan()
	dir := folder(t, t.TempDir(), "w")
	x.conn("ana")
	send := func(forged int64) {
		body := `{"model":"claude-cli-haiku","tools":[{"name":"x"}],"messages":[{"role":"user","content":"go"}]}`
		sc := &shareClient{c: x.conn("ana"), session: fmt.Sprint("p", forged, x.nps["ana"].project), root: dir, sync: true, synced: map[string]syncEntry{}, reported: map[string]bool{}}
		if !sc.register() {
			t.Fatal(sc.refusal)
		}
		sc.start(2)
		defer sc.unregister()
		h := map[string]string{"x-claude-mini-session": sc.session, "x-claude-mini-cwd": dir, "content-type": "application/json", "X-MINIGW-PROJECT": fmt.Sprint(forged)}
		if st, b := call(x.conn("ana"), "POST", "/v1/messages", h, []byte(body), 30*time.Second); st != 200 {
			t.Fatalf("%d %s", st, b)
		}
	}
	x.nps["ana"].project = fmt.Sprint(f.pid["fakture"])
	send(f.pid["import"]) // ana has no role in import: if the forged header got through, the request would be refused
	x.nps["ana"].project = ""
	send(f.pid["fakture"]) // ana does have a role in fakture: if it got through, the event would carry the project
	ev := f.events()
	if len(ev) != 2 || ev[0].Project != f.pid["fakture"] || ev[1].Project != 0 {
		t.Fatalf("%+v", ev)
	}
}
