package main

import (
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

var copilotForbidden = []string{"--allow-all-tools", "--allow-all-paths", "--allow-all-urls", "--allow-all", "--yolo", "--sandbox", "--trust-folder"}

func copilotRules(a []string) (allow, deny []string) {
	for i := 0; i+1 < len(a); i++ {
		switch a[i] {
		case "--allow-tool":
			allow = append(allow, a[i+1])
		case "--deny-tool":
			deny = append(deny, a[i+1])
		}
	}
	return
}

func TestCopilotArgs(t *testing.T) {
	ro := copilotArgs(true, "", false, false, "s1")
	allow, deny := copilotRules(ro)
	if slices.ContainsFunc(ro, func(x string) bool { return slices.Contains(copilotForbidden, x) || x == "--add-dir" }) { // M3
		t.Fatalf("forbidden flag: %q", ro)
	}
	if !slices.Equal(allow, []string{"read"}) || !slices.Equal(deny, []string{"memory", "write", "shell", "shell(gh:*)", "shell(security:*)", "url"}) {
		t.Fatalf("read-only: allow %q deny %q", allow, deny)
	}
	for _, f := range []string{"--disallow-temp-dir", "--disable-builtin-mcps", "--no-ask-user", "--no-auto-update",
		"--disable-client-telemetry", "--secret-env-vars=COPILOT_GITHUB_TOKEN,GH_TOKEN,GITHUB_TOKEN"} {
		if !slices.Contains(ro, f) {
			t.Errorf("missing %s", f)
		}
	}
	if i := slices.Index(ro, "--session-id"); i < 0 || ro[i+1] != "s1" || slices.Contains(ro, "-p") { // navodilo na stdin
		t.Fatalf("session/prompt: %q", ro)
	}
	w := copilotArgs(true, "gpt-5.2", true, true, "s")
	allow, deny = copilotRules(w)
	if !slices.Equal(allow, []string{"read", "write", "url"}) || !slices.Contains(deny, "shell") { // #81 H1: lupine nikoli
		t.Fatalf("write: allow %q deny %q", allow, deny)
	}
	if w[slices.Index(w, "--model")+1] != "gpt-5.2" {
		t.Fatalf("model: %q", w)
	}
	chat := copilotArgs(false, "", true, true, "s")
	if allow, deny = copilotRules(chat); len(allow) != 0 || !slices.Contains(deny, "read") {
		t.Fatalf("chat: allow %q deny %q", allow, deny)
	}
	if txt, _ := copilotEvent([]byte(`{"type":"assistant.message","data":{"content":"Hi!"}}`)); txt != "Hi!" {
		t.Fatal("assistant.message")
	}
	if _, st := copilotEvent([]byte(`{"type":"tool.execution_start","data":{"toolName":"view"}}`)); st != "view" {
		t.Fatal("tool step")
	}
	env := agentEnv([]string{"COPILOT_ALLOW_ALL=true", "COPILOT_ALLOW_ALL_TOOLS=1", "COPILOT_ASSISTED_APPROVAL=1", "COPILOT_HOME=/x", "PATH=/bin",
		"COPILOT_PROVIDER_API_KEY_COMMAND=touch /tmp/x", "COPILOT_CLI_DIST_DIR=/evil", "GH_TOKEN=u", "GITHUB_TOKEN=u", "COPILOT_GITHUB_TOKEN=u"})
	if !slices.Equal(env, []string{"PATH=/bin"}) {
		t.Fatalf("env: %q", env)
	}
	c := agentCLI{Models: []string{"gpt-5.2"}}
	if !c.allowsModel(copilotKind, "gpt-5.2") || c.allowsModel(copilotKind, "other") || (agentCLI{}).allowsModel(copilotKind, "x") ||
		(agentCLI{}).canWrite(copilotKind) || !(agentCLI{}).canWrite(codexKind) {
		t.Fatal("models/write defaults")
	}
}

// copilotTestHome (#81): začasni HOME s pravo obliko ~/.copilot/config.json (vrstice »//«, kazalec na prijavo in
// ključi, ki se ne smejo kopirati); nikoli prava prijava uporabnika.
func copilotTestHome(t *testing.T) string {
	home := t.TempDir()
	t.Setenv("HOME", home)
	os.MkdirAll(filepath.Join(home, ".copilot"), 0o700)
	os.WriteFile(filepath.Join(home, ".copilot", "config.json"), []byte(`// This file is managed automatically.
// User settings belong in settings.json.
{
  "loggedInUsers": [{"host": "https://github.com", "login": "octo-cat"}, {"host": "corp.ghe.com", "login": "octo_corp", "kind": "user"}],
  "lastLoggedInUser": {"host": "https://github.com", "login": "octo-cat"},
  "trustedFolders": ["/"], "installedPlugins": [{"name": "evil", "cachePath": "/tmp/evil"}], "storeTokenPlaintext": false
}
`), 0o600)
	return home
}

// #81: le loggedInUsers in lastLoggedInUser, preverjena oblika; žeton v navadnem besedilu ali tuja oblika = razlog.
func TestCopilotLoginPointer(t *testing.T) {
	home := copilotTestHome(t)
	real := filepath.Join(home, ".copilot")
	conf, why := copilotLoginPointer(real)
	if why != "" || string(conf) != `{"lastLoggedInUser":{"host":"https://github.com","login":"octo-cat"},`+
		`"loggedInUsers":[{"host":"https://github.com","login":"octo-cat"},{"host":"corp.ghe.com","kind":"user","login":"octo_corp"}]}` {
		t.Fatalf("pointer: %s / %s", conf, why)
	}
	write := func(body string) string {
		os.WriteFile(filepath.Join(real, "config.json"), []byte(body), 0o600)
		c, why := copilotLoginPointer(real)
		if why != "" && c != nil {
			t.Fatalf("both: %s", c)
		}
		return why
	}
	user := `{"host":"github.com","login":"u"}`
	for body, want := range map[string]string{
		`{"loggedInUsers":[` + user + `]}`:                                                                     "",
		`{"loggedInUsers":[` + user + `],"authTokens":{"github.com:u":"gho_x"}}`:                               "plain text",
		`{"loggedInUsers":[` + user + `],"copilotTokens":{"x":"y"}}`:                                           "plain text",
		`{"loggedInUsers":[]}`:                                                                                 "not signed in",
		`{"trustedFolders":["/"]}`:                                                                             "not signed in",
		`not json`:                                                                                             "expected form",
		`{"loggedInUsers":[{"host":"github.com","login":"u","token":"x"}]}`:                                    "expected form", // tuji ključ
		`{"loggedInUsers":[{"host":"github.com/../x","login":"u"}]}`:                                           "expected form",
		`{"loggedInUsers":[{"host":"file:///etc","login":"u"}]}`:                                               "expected form",
		`{"loggedInUsers":[{"host":"github.com","login":"-u"}]}`:                                               "expected form",
		`{"loggedInUsers":[{"host":"github.com","login":"u;rm"}]}`:                                             "expected form",
		`{"loggedInUsers":[{"host":"github.com"}]}`:                                                            "expected form",
		`{"loggedInUsers":[{"host":"github.com","login":"u","kind":7}]}`:                                       "expected form",
		`{"loggedInUsers":[{"host":"github.com","login":"u","kind":"-user"}]}`:                                 "expected form", // L4: [A-Za-z][A-Za-z0-9-]
		`{"loggedInUsers":[{"host":"https://github.com","login":"u","kind":"githubDotCom"}]}`:                  "",              // copilot 1.0.1xx
		`{"loggedInUsers":[{"host":"github.com","login":"u","derivedFrom":"github.com:u"}]}`:                   "expected form",
		`{"loggedInUsers":[{"host":"github.com","login":"u","kind":"enterprise-user","derivedFrom":"oauth"}]}`: "",
		`{"loggedInUsers":[{"host":"github.com","login":"u","kind":"a
b"}]}`: "expected form",
		`{"loggedInUsers":[` + user + `],"lastLoggedInUser":{"host":"","login":"u"}}`: "expected form",
		`{"loggedInUsers":"u"}`: "not signed in",
	} {
		if got := write(body); want == "" && got != "" || want != "" && !strings.Contains(got, want) {
			t.Errorf("%s: %q, want %q", body, got, want)
		}
	}
	os.Remove(filepath.Join(real, "config.json"))
	if runtime.GOOS != "windows" {
		os.WriteFile(filepath.Join(home, "elsewhere.json"), []byte(`{"loggedInUsers":[`+user+`]}`), 0o600)
		os.Symlink(filepath.Join(home, "elsewhere.json"), filepath.Join(real, "config.json"))
		if _, why := copilotLoginPointer(real); !strings.Contains(why, "not signed in") { // povezava: ne sledi
			t.Fatalf("followed a link: %q", why)
		}
	}
	if _, why := copilotLoginPointer(filepath.Join(home, "none")); !strings.Contains(why, "copilot login") {
		t.Fatalf("no login: %q", why)
	}
}

// #81 (Linux): Secret Service na seji D-Bus (lažna busctl in dbus-send) in xdg-dbus-proxy; brez njih razlog.
func TestCopilotKeyringDetection(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Secret Service over D-Bus (fake busctl, dbus-send, xdg-dbus-proxy): Linux semantics")
	}
	if !secretServiceListed(`as 3 ":1.0" "org.freedesktop.DBus" "org.freedesktop.secrets"`) ||
		!secretServiceListed("method return\n   array [\n      string \"org.freedesktop.secrets\"\n   ]") ||
		secretServiceListed(`as 2 "org.freedesktop.secrets.x" "org.freedesktop.DBus"`) {
		t.Fatal("listing")
	}
	// M1: git (ovoja exec in PATH) nikoli ne dobi posrednika shrambe ključev
	if !strings.Contains(wrapScript("/g"), "\nunset DBUS_SESSION_BUS_ADDRESS\n") || !strings.Contains(pathWrapScript("/g"), "\nunset DBUS_SESSION_BUS_ADDRESS\n") {
		t.Fatal("git wrappers keep DBUS_SESSION_BUS_ADDRESS")
	}
	if got := dbusEscape("/run/user/1 x/bus"); got != "/run/user/1%20x/bus" {
		t.Fatalf("escape %q", got)
	}
	bin := t.TempDir()
	t.Setenv("PATH", bin)
	t.Setenv("XDG_RUNTIME_DIR", "")
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path=/run/user/1/bus")
	script := func(name, body string) {
		os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755)
	}
	if why := copilotKeyring(); !strings.Contains(why, "xdg-dbus-proxy") {
		t.Fatalf("no proxy: %q", why)
	}
	script("xdg-dbus-proxy", "exit 0")
	if why := copilotKeyring(); !strings.Contains(why, "Secret Service") { // ne busctl ne dbus-send
		t.Fatalf("no bus tools: %q", why)
	}
	script("dbus-send", `[ "$DBUS_SESSION_BUS_ADDRESS" = unix:path=/run/user/1/bus ] || exit 1
case "$*" in *ListActivatableNames*) echo '   string "org.freedesktop.secrets"';; *) echo '   string "org.freedesktop.DBus"';; esac`)
	if why := copilotKeyring(); why != "" { // le za zagon ob klicu (gnome-keyring prek aktivacije)
		t.Fatalf("activatable: %q", why)
	}
	script("busctl", `echo 'as 1 "org.freedesktop.DBus"'`) // busctl ima prednost: tu ni Secret Service
	if why := copilotKeyring(); !strings.Contains(why, "headless") {
		t.Fatalf("headless: %q", why)
	}
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:abstract=/tmp/dbus-x,guid=1") // M1: dosegljiva iz bwrap
	if why := copilotKeyring(); !strings.Contains(why, "unix:abstract=") {
		t.Fatalf("abstract: %q", why)
	}
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "") // brez seje D-Bus
	os.Remove(filepath.Join(bin, "busctl"))
	if why := copilotKeyring(); !strings.Contains(why, "Secret Service") {
		t.Fatalf("no session: %q", why)
	}
	// posrednik: vtičnica dir/bus, le org.freedesktop.secrets
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path=/real/bus")
	script("xdg-dbus-proxy", `echo "$@" > "`+bin+`/proxy.args"; exec sleep 30`)
	t.Setenv("PATH", bin+":/usr/bin:/bin")
	dir, err := os.MkdirTemp("/tmp", "cbp") // kratka pot: vtičnice unix do 104 znakov
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	go func() { // vtičnico posrednika naredi test (lažni posrednik le čaka)
		for range 200 {
			if _, err := os.Stat(filepath.Join(bin, "proxy.args")); err == nil {
				if ln, err := net.Listen("unix", filepath.Join(dir, "bus")); err == nil {
					t.Cleanup(func() { ln.Close() })
				}
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	addr, stop, err := copilotBusProxy(dir)
	if err != nil {
		t.Fatal(err)
	}
	stop()
	args, _ := os.ReadFile(filepath.Join(bin, "proxy.args"))
	if addr != "unix:path="+dir+"/bus" || strings.TrimSpace(string(args)) != "unix:path=/real/bus "+dir+"/bus --filter --talk=org.freedesktop.secrets" {
		t.Fatalf("proxy: %q %q", addr, args)
	}
}

func TestCopilotThroughRouter(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Copilot is not supported on Windows gateways (#81); its fake uses POSIX links and $COPILOT_HOME paths")
	}
	bin := t.TempDir()
	argsFile := filepath.Join(bin, "args")
	copilot := filepath.Join(bin, "copilot")
	os.WriteFile(copilot, []byte(`#!/bin/sh
if [ "$1" = --version ]; then echo "token=${COPILOT_GITHUB_TOKEN:-none}" >> "`+argsFile+`.version"; exit 0; fi
id=""; prev=""; for a in "$@"; do [ "$prev" = --session-id ] && id="$a"; prev="$a"; done
printf '%s\n' "$@" "gitdir=$GIT_DIR" "pager=$GIT_PAGER" "fsmon=$GIT_CONFIG_KEY_0" "path=$PATH" "dbus=$DBUS_SESSION_BUS_ADDRESS" "config=$(cat "$COPILOT_HOME/config.json")" "home=$COPILOT_HOME" "token=$COPILOT_GITHUB_TOKEN$GH_TOKEN$GITHUB_TOKEN" > "`+argsFile+`"
cat > "`+argsFile+`.stdin"
st="$COPILOT_HOME/session-state/$id"; mkdir -p "$st"
n=$(cat "$st/events.jsonl" 2>/dev/null || echo 0); echo $((n + 1)) > "$st/events.jsonl"
hooks=no; [ -e "$COPILOT_HOME/hooks.json" ] && hooks=yes; [ -e "$st/checkpoints" ] && hooks=checkpoints
link=no; [ -L "$st/link" ] && link=yes
echo '{}' > "$COPILOT_HOME/hooks.json"; ln -s /etc "$st/link" 2>/dev/null; mkdir -p "$st/checkpoints"
echo '{"type":"session.start","data":{"sessionId":"x"}}'
echo '{"type":"assistant.message","data":{"content":"first"}}'
echo '{"type":"assistant.message","data":{"content":"copilot says hi turn='$((n + 1))' hooks='$hooks' link='$link'"}}'
`), 0o755)
	for _, v := range copilotTokenVars { // #81: žetoni iz okolja prehoda nikoli do copilota
		t.Setenv(v, "leak")
	}
	home := copilotTestHome(t)
	defer func(k func() string, p func(string) (string, func(), error)) { copilotKeyring, copilotBusProxy = k, p }(copilotKeyring, copilotBusProxy)
	copilotKeyring = func() string { return "" } // Linux: Secret Service je (preverjanje v TestCopilotKeyringDetection)
	proxied := ""
	copilotBusProxy = func(dir string) (string, func(), error) {
		proxied = dir
		return "unix:path=" + dir + "/bus", func() {}, nil
	}
	s, _, base := liveServer(t, func(c *serveConfig) {
		c.AllowUnsandboxedBash = true // Linux brez bwrap (Docker)
		t.Setenv("COPILOT_HOME", t.TempDir())
		c.Copilot = agentCLI{Enabled: true, Executable: copilot, Models: []string{"gpt-5.2"}}
	})
	host := strings.TrimPrefix(base, "http://")
	if _, models := adminReq(t, base, host, "tok", "GET", "/v1/models", ""); !strings.Contains(models, `"claude-copilot"`) ||
		!strings.Contains(models, `"claude-copilot-gpt-5.2"`) {
		t.Fatalf("models: %s", models)
	}
	c := startLoopback(t, s)
	dir := t.TempDir()
	sc := &shareClient{c: c, session: "sidp", root: dir, sync: true, synced: map[string]syncEntry{}, reported: map[string]bool{}}
	if !sc.register() {
		t.Fatal("register failed: " + sc.refusal)
	}
	sc.start(2)
	defer sc.unregister()
	h := map[string]string{"x-claude-mini-session": "sidp", "x-claude-mini-cwd": dir, "content-type": "application/json"}
	ask := func(model string, history ...string) (int, string) {
		msgs := `{"role":"user","content":"PROMPT_MARKER"}`
		if len(history) == 1 {
			msgs += `,{"role":"assistant","content":"` + history[0] + `"},{"role":"user","content":"next"}`
		}
		st, data := call(c, "POST", "/v1/messages", h, []byte(`{"model":"`+model+`","tools":[{"name":"x"}],"messages":[`+msgs+`]}`), 30*time.Second)
		return st, string(data)
	}
	args := func() []string {
		b, _ := os.ReadFile(argsFile)
		return strings.Split(strings.TrimSpace(string(b)), "\n")
	}
	st, b := ask("claude-copilot-gpt-5.2")
	if st != 200 || !strings.Contains(b, "copilot says hi turn=1 hooks=no") || strings.Contains(b, "first") {
		t.Fatalf("copilot: %d %s", st, b)
	}
	a := args()
	field := func(a []string, k string) string {
		for _, x := range a {
			if v, ok := strings.CutPrefix(x, k+"="); ok {
				return v
			}
		}
		return "?"
	}
	reqHome := field(a, "home")
	allow, deny := copilotRules(a)
	if !slices.Equal(allow, []string{"read"}) || !slices.Contains(deny, "write") || !slices.Contains(deny, "shell") ||
		slices.ContainsFunc(a, func(x string) bool { return slices.Contains(copilotForbidden, x) }) ||
		!strings.HasPrefix(reqHome, s.conf().Scratch+"/tmp/req-") {
		t.Fatalf("copilot args: %q", a)
	}
	if _, err := os.Stat(reqHome); err == nil {
		t.Fatal("per-request COPILOT_HOME not removed")
	}
	// Navodilo na stdin, ne v argumentih (ps, /proc/<pid>/cmdline).
	if in, _ := os.ReadFile(argsFile + ".stdin"); !strings.Contains(string(in), "PROMPT_MARKER") ||
		slices.ContainsFunc(a, func(x string) bool { return strings.Contains(x, "PROMPT_MARKER") }) {
		t.Fatalf("prompt not on stdin: %q / %q", in, a)
	}
	// #81: brez žetona; svež config.json le s kazalcem na prijavo (ne trustedFolders, installedPlugins)
	if field(a, "token") != "" || field(a, "config") != `{"lastLoggedInUser":{"host":"https://github.com","login":"octo-cat"},`+
		`"loggedInUsers":[{"host":"https://github.com","login":"octo-cat"},{"host":"corp.ghe.com","kind":"user","login":"octo_corp"}]}` {
		t.Fatalf("login handoff: %q", a)
	}
	// #81 N1: tudi zunaj worktree git copilota brez .git/config odjemalca; Linux: ovoj git (brez D-Bus) na PATH
	if field(a, "fsmon") != "core.fsmonitor" || field(a, "gitdir") != os.DevNull || field(a, "pager") != "cat" || // N3: brez repozitorija
		runtime.GOOS == "linux" && lookPath("git") != "" &&
			!strings.HasPrefix(field(a, "path"), filepath.Join(s.conf().Scratch, "gitexec", "bin")+":") {
		t.Fatalf("git guard: %q", a)
	}
	if want := map[bool]string{true: "unix:path=" + proxied + "/bus", false: ""}[runtime.GOOS == "linux"]; field(a, "dbus") != want ||
		runtime.GOOS == "linux" && filepath.Dir(reqHome) != proxied { // Linux: le posrednik v mapi zahteve
		t.Fatalf("dbus: %q (proxy %q)", field(a, "dbus"), proxied)
	}
	// R7: na Linuxu enkrat »--version« brez bwrap in brez žetona (razširitev paketa).
	if v, _ := os.ReadFile(argsFile + ".version"); runtime.GOOS == "linux" && !strings.HasPrefix(string(v), "token=none") {
		t.Fatalf("prepare: %q", v)
	}
	// C2, R6: naslednji zavoj: le events.jsonl in workspace.yaml ostaneta (brez povezave, checkpoints), hooks.json ne.
	if st, b := ask("claude-copilot-gpt-5.2", "copilot says hi turn=1 hooks=no link=no"); st != 200 || !strings.Contains(b, "turn=2 hooks=no link=no") {
		t.Fatalf("second turn: %d %s", st, b)
	}
	if st, _ := ask("claude-copilot-unknown"); st != 404 {
		t.Fatalf("unlisted model: %d", st)
	}
	if st, b := adminNoise(t, s, base, s.admin, "GET", adminPrefix+"/agents/copilot", ""); st != 200 || !strings.Contains(b, `"write":false`) ||
		!strings.Contains(b, `"commands":false`) || !strings.Contains(b, `"network":false`) {
		t.Fatalf("copilot API defaults: %d %s", st, b)
	}
	// H1: ukazov Copilot ni nikjer: PATCH 400 z razlogom, shranjen true ne velja (GET commands:false + razlog)
	if st, b := adminNoise(t, s, base, s.admin, "PATCH", adminPrefix+"/agents/copilot", `{"write":true,"commands":true}`); st != 400 ||
		!strings.Contains(b, copilotNoCommands) {
		t.Fatalf("commands: %d %s", st, b)
	}
	s.update(func(c *serveConfig) error { c.Copilot.Commands = true; return nil }) // starejša nastavitev
	if st, b := adminNoise(t, s, base, s.admin, "PATCH", adminPrefix+"/agents/copilot", `{"write":true}`); st != 200 || !strings.Contains(b, `"write":true`) ||
		!strings.Contains(b, `"commands":false`) || !strings.Contains(b, `"commandsUnavailable":"`+copilotNoCommands) {
		t.Fatalf("copilot API: %d %s", st, b)
	}
	if st, _ := adminNoise(t, s, base, s.admin, "PATCH", adminPrefix+"/agents/codex", `{"commands":true}`); st != 400 {
		t.Fatal("codex commands accepted")
	}
	if st, b := adminNoise(t, s, base, s.admin, "PATCH", adminPrefix+"/agents/copilot", `{"token":"github_pat_cmd1"}`); st != 400 ||
		strings.Contains(b, "github_pat_cmd1") { // D1: brez žetona
		t.Fatalf("token: %d %s", st, b)
	}
	if st, b := ask("claude-copilot"); st != 200 || field(args(), "token") != "" {
		t.Fatalf("with writing: %d %s %q", st, b, args())
	}
	allow, deny = copilotRules(args())
	if !slices.Contains(allow, "write") || slices.Contains(allow, "shell") || !slices.Contains(deny, "shell") {
		t.Fatalf("after PATCH: allow %q deny %q", allow, deny)
	}
	// Omejitev: stderr ostane v dnevniku, odjemalec dobi stalno besedilo (Mac: očiščen konec stderr)
	os.WriteFile(copilot, []byte("#!/bin/sh\n[ \"$1\" = --version ] && exit 0\necho \"You've hit your 5-hour limit STDERR_TAIL\" >&2\nexit 1\n"), 0o755)
	if st, b := ask("claude-copilot"); st != 429 || !strings.Contains(b, "copilot: usage or rate limit reached; details are in the gateway log") ||
		strings.Contains(b, "STDERR_TAIL") {
		t.Fatalf("limit: %d %s", st, b)
	}
	// D1: brez uporabne prijave copilot login Copilot ni na voljo, z razlogom (zahteva, API, status)
	os.WriteFile(filepath.Join(home, ".copilot", "config.json"), []byte(`{"loggedInUsers":[{"host":"github.com","login":"u"}],"authTokens":{"x":"y"}}`), 0o600)
	if st, b := ask("claude-copilot"); st != 503 || !strings.Contains(b, "plain text") || strings.Contains(b, `"y"`) {
		t.Fatalf("plaintext: %d %s", st, b)
	}
	os.Remove(filepath.Join(home, ".copilot", "config.json"))
	if st, b := ask("claude-copilot"); st != 503 || !strings.Contains(b, "copilot login") {
		t.Fatalf("no login: %d %s", st, b)
	}
	if st, b := adminNoise(t, s, base, s.admin, "GET", adminPrefix+"/agents/copilot", ""); st != 200 || !strings.Contains(b, `"loginProblem":"Copilot is not signed in`) ||
		!strings.Contains(b, `not available: Copilot is not signed in`) {
		t.Fatalf("GET reason: %d %s", st, b)
	}
	if st, b := adminNoise(t, s, base, s.admin, "GET", adminPrefix+"/status", ""); st != 200 || !strings.Contains(b, `"copilot":"Copilot is not signed in`) {
		t.Fatalf("status reason: %d %s", st, b)
	}
}

// R4, R6: le events.jsonl in workspace.yaml, le navadne datoteke iz prave mape; povezava (datoteka ali mapa) ne.
func TestCopySession(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges")
	}
	d := t.TempDir()
	src, secret := filepath.Join(d, "s"), filepath.Join(d, "secret")
	os.MkdirAll(filepath.Join(src, "checkpoints"), 0o700)
	os.WriteFile(secret, []byte("s"), 0o600)
	os.WriteFile(filepath.Join(src, "events.jsonl"), []byte("e"), 0o600)
	os.WriteFile(filepath.Join(src, "checkpoints", "x"), []byte("c"), 0o600)
	os.Symlink(secret, filepath.Join(src, "workspace.yaml"))
	if err := copySession(src, filepath.Join(d, "o")); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadDir(filepath.Join(d, "o"))
	if len(got) != 1 || got[0].Name() != "events.jsonl" {
		t.Fatalf("copied %v", got)
	}
	os.Symlink(src, filepath.Join(d, "link"))
	if copySession(filepath.Join(d, "link"), filepath.Join(d, "o2")) == nil {
		t.Fatal("followed a linked session folder")
	}
}

// #26: dovoljena mapa, ki vsebuje paket Copilot ali je v njem, je zavrnjena (kot Scratch).
func TestCopilotCacheFoldersRefused(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	if runtime.GOOS == "windows" {
		t.Setenv("LocalAppData", filepath.Join(home, "AppData", "Local"))
	}
	cache := copilotCacheDir()
	os.MkdirAll(filepath.Join(cache, "pkg"), 0o700)
	other := filepath.Join(home, "projects")
	os.MkdirAll(other, 0o700)
	if len(copilotCacheOverlaps([]string{filepath.Dir(cache)})) != 1 || len(copilotCacheOverlaps([]string{filepath.Join(cache, "pkg")})) != 1 ||
		len(copilotCacheOverlaps([]string{other})) != 0 {
		t.Fatal("overlaps")
	}
	cfgDir := filepath.Join(home, "cfg")
	os.MkdirAll(cfgDir, 0o700)
	c := serveConfig{AllowedFolders: []string{filepath.Dir(cache)}, Scratch: filepath.Join(home, "scratch")}
	if _, err := checkAgent(&c, cfgDir, nil); err == nil || !strings.Contains(err.Error(), "Copilot's package cache") {
		t.Fatalf("checkAgent: %v", err)
	}
}

// 0.5.24 (lastnik): Copilot na prehodu Windows ni podprt: ni na seznamu modelov, vklop in zahteve zavrnjeni z razlogom.
func TestCopilotUnsupportedOnWindows(t *testing.T) {
	defer func(v bool) { copilotUnsupported = v }(copilotUnsupported)
	copilotUnsupported = true
	exe, _ := os.Executable()
	s, _, base := liveServer(t, func(c *serveConfig) {
		c.AllowUnsandboxedBash = true
		c.Copilot = agentCLI{Enabled: true, Executable: exe}
	})
	host := strings.TrimPrefix(base, "http://")
	if _, models := adminReq(t, base, host, "tok", "GET", "/v1/models", ""); strings.Contains(models, "claude-copilot") {
		t.Fatalf("listed: %s", models)
	}
	if st, b := adminNoise(t, s, base, s.admin, "GET", adminPrefix+"/agents/copilot", ""); st != 200 || !strings.Contains(b, `"unsupported":true`) ||
		!strings.Contains(b, `"enabled":false`) || !strings.Contains(b, copilotUnsupportedMsg) {
		t.Fatalf("GET: %d %s", st, b)
	}
	if st, b := adminNoise(t, s, base, s.admin, "PATCH", adminPrefix+"/agents/copilot", `{"enabled":true}`); st != 400 || !strings.Contains(b, copilotUnsupportedMsg) {
		t.Fatalf("enable: %d %s", st, b)
	}
	if st, b := adminReq(t, base, host, "tok", "POST", "/v1/messages", `{"model":"claude-copilot","messages":[{"role":"user","content":"hi"}]}`); st != 400 ||
		!strings.Contains(b, copilotUnsupportedMsg) {
		t.Fatalf("run: %d %s", st, b)
	}
	// Copilot izklopljen (privzeto): še vedno razlog, ne 404 »Unknown model« (Claude Code bi rekel »model may not exist«).
	_, _, base2 := liveServer(t, nil)
	host2 := strings.TrimPrefix(base2, "http://")
	if st, b := adminReq(t, base2, host2, "tok", "POST", "/v1/messages", `{"model":"claude-copilot","messages":[{"role":"user","content":"hi"}]}`); st != 400 ||
		!strings.Contains(b, copilotUnsupportedMsg) {
		t.Fatalf("disabled run: %d %s", st, b)
	}
	req, _ := http.NewRequest("POST", base2+"/v1/messages", strings.NewReader(`{"model":"claude-copilot","messages":[]}`))
	req.Host = host2
	req.Header.Set("authorization", "Bearer tok")
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.Header.Get("x-should-retry") != "false" {
		t.Fatalf("retry header: %v %v", err, resp)
	} else {
		resp.Body.Close()
	}
}
