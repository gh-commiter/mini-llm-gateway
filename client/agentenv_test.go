package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// #125: the allowlist of agent runs; client/testdata/agent-env.json is asserted by Tests/RouterCoreTests/AgentEnvTests.swift too.

type agentEnvFixture struct {
	BaseNames, BasePrefixes, DenyNames, DenyPrefixes, DenySuffixes []string
	MaxNames                                                       int
	Validate                                                       []struct {
		Names, Want []string
		Error       string
	}
	TooMany string
	NotBase []string // D: they load code or carry credentials: only the owner names them
	Unset   []struct {
		Env         map[string]string
		Names, Want []string
	}
}

func loadAgentEnvFixture(t *testing.T) agentEnvFixture {
	var f agentEnvFixture
	if err := json.Unmarshal(srcFile(t, "testdata/agent-env.json"), &f); err != nil || len(f.BaseNames) == 0 {
		t.Fatalf("testdata/agent-env.json: %v", err)
	}
	return f
}

func TestAgentEnvListsMatchTheFixture(t *testing.T) {
	f := loadAgentEnvFixture(t)
	for _, c := range []struct {
		name      string
		got, want []string
	}{{"base", agentEnvBase, f.BaseNames}, {"base prefixes", agentEnvBasePrefixes, f.BasePrefixes}, {"deny", agentEnvDenyNames, f.DenyNames},
		{"deny prefixes", agentEnvDenyPrefixes, f.DenyPrefixes}, {"deny suffixes", agentEnvDenySuffixes, f.DenySuffixes}} {
		if !slices.Equal(c.got, c.want) {
			t.Errorf("%s: Go %q, fixture %q", c.name, c.got, c.want)
		}
	}
	if agentEnvMax != f.MaxNames {
		t.Errorf("max %d, fixture %d", agentEnvMax, f.MaxNames)
	}
}

func TestValidAgentEnv(t *testing.T) {
	f := loadAgentEnvFixture(t)
	for _, c := range f.Validate {
		got, why := validAgentEnv(c.Names)
		if why != c.Error || c.Error == "" && !slices.Equal(got, c.Want) {
			t.Errorf("%q: %q %q, want %q %q", c.Names, got, why, c.Want, c.Error)
		}
	}
	many := make([]string, f.MaxNames+1)
	for i := range many {
		many[i] = fmt.Sprintf("V%d", i)
	}
	if _, why := validAgentEnv(many[:f.MaxNames]); why != "" {
		t.Errorf("64 names: %s", why)
	}
	if _, why := validAgentEnv(many); why != f.TooMany {
		t.Errorf("65 names: %q", why)
	}
}

// The gateway's environment with secrets: only the base list and the owner's names pass, a denied name never.
var agentEnvSample = []string{"MGTEST_SECRET=1", "AWS_SECRET_ACCESS_KEY=x", "FOO_API_KEY=x", "PATH=/usr/bin", "HOME=/h", "HTTPS_PROXY=http://127.0.0.1:9",
	"https_proxy=http://127.0.0.1:9", "LC_ALL=C", "ANTHROPIC_BASE_URL=x", "GH_TOKEN=x"}

func agentEnvNames(env []string) []string {
	var out []string
	for _, e := range env {
		n, _, _ := strings.Cut(e, "=")
		out = append(out, n)
	}
	return out
}

func TestAgentEnvForEveryKind(t *testing.T) {
	// claude (runCLI), codex, grok and copilot (runAgentProcess) all start from agentEnvFor (TestEveryAgentStartUsesTheAllowlist)
	for _, c := range []struct {
		owner, want []string
	}{
		{nil, []string{"PATH", "HOME", "HTTPS_PROXY", "https_proxy", "LC_ALL"}},
		{[]string{"MGTEST_SECRET"}, []string{"MGTEST_SECRET", "PATH", "HOME", "HTTPS_PROXY", "https_proxy", "LC_ALL"}},
		{[]string{"mgtest_secret", "FOO_API_KEY", "GH_TOKEN"}, []string{"MGTEST_SECRET", "PATH", "HOME", "HTTPS_PROXY", "https_proxy", "LC_ALL"}},
	} {
		if got := agentEnvNames(agentEnvFor(agentEnvSample, c.owner)); !slices.Equal(got, c.want) {
			t.Errorf("owner %q: %q, want %q", c.owner, got, c.want)
		}
	}
}

// fakeEnvNames: prints the names (never the values) of the test variables it got.
var fakeEnvNames = fake("envnames", func(p fakeParams, args []string) {
	for _, e := range os.Environ() {
		if n, _, _ := strings.Cut(e, "="); slices.Contains([]string{"MGTEST_SECRET", "AWS_SECRET_ACCESS_KEY", "FOO_API_KEY", "HTTPS_PROXY", "MGTEST_RUN"}, n) {
			fmt.Println(n)
		}
	}
	os.Exit(0)
})

// The real start of codex, grok and copilot (runAgentProcess): the gateway's environment is filtered with the owner's names,
// the run's own variables (m.env) come after the filter.
func TestRunAgentProcessEnvironment(t *testing.T) {
	t.Setenv("MGTEST_SECRET", "1")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "x")
	t.Setenv("FOO_API_KEY", "x")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:9")
	exe := fakeCLI(t, t.TempDir(), "agent", fakeEnvNames, nil)
	for _, c := range []struct {
		owner, want []string
	}{
		{nil, []string{"HTTPS_PROXY", "MGTEST_RUN"}},
		{[]string{"MGTEST_SECRET", "FOO_API_KEY"}, []string{"HTTPS_PROXY", "MGTEST_RUN", "MGTEST_SECRET"}},
	} {
		s, _ := testServer(t, func(sc *serveConfig) { sc.AgentEnv = c.owner })
		var got []string
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		err := s.runAgentProcess(ctx, &req{rid: "req_1"}, exe, nil, agentMounts{cfgDir: t.TempDir(), env: []string{"MGTEST_RUN=1"}}, t.TempDir(), "",
			func(b []byte) { got = append(got, strings.TrimSpace(string(b))) })
		cancel()
		slices.Sort(got)
		if err != nil || !slices.Equal(got, c.want) {
			t.Errorf("owner %q: %q (%v), want %q", c.owner, got, err, c.want)
		}
	}
}

// Every agent start (claude: serve_cli.go, codex/grok/copilot: serve_agents.go, the Windows sandbox probe: mxc.go) builds its
// environment with agentEnvFor; a new os.Environ() in these files, or agentEnv(os.Environ()) anywhere, fails here.
func TestEveryAgentStartUsesTheAllowlist(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	if len(files) == 0 {
		t.Skip("needs the client/ source tree; not in the suite exe")
	}
	starts := map[string][]string{
		"serve_cli.go":    {"agentEnvFor(os.Environ(), s.conf().AgentEnv)"},                                   // claude
		"serve_agents.go": {"agentEnvFor(os.Environ(), s.conf().AgentEnv)", "agentEnvFor(os.Environ(), nil)"}, // runAgentProcess; copilot --version
		"mxc.go":          {"agentEnvFor(os.Environ(), nil)"},                                                 // the sandbox probe
	}
	bare := regexp.MustCompile(`\bagentEnv\(os\.Environ\(\)\)`)
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, _ := os.ReadFile(f)
		src := string(b)
		if bare.MatchString(src) {
			t.Errorf("%s: agentEnv(os.Environ()) is the denylist alone: use agentEnvFor", f)
		}
		want, ok := starts[f]
		if !ok {
			continue
		}
		for _, w := range want {
			if !strings.Contains(src, w) {
				t.Errorf("%s: no %s", f, w)
			}
		}
		if n, allowed := strings.Count(src, "os.Environ()"), strings.Count(src, "agentEnvFor(os.Environ(),"); n != allowed {
			t.Errorf("%s: %d os.Environ() but %d through agentEnvFor", f, n, allowed)
		}
	}
}

// D: the owner's names the gateway's environment does not hold (GET /agents/env "unset"), and what the base list must not hold.
func TestAgentEnvUnset(t *testing.T) {
	f := loadAgentEnvFixture(t)
	for _, c := range f.Unset {
		var env []string
		for k, v := range c.Env {
			env = append(env, k+"="+v)
		}
		if got := agentEnvUnset(c.Names, env); !slices.Equal(got, c.Want) {
			t.Errorf("%q in %q: %q, want %q", c.Names, env, got, c.Want)
		}
	}
	for _, n := range f.NotBase {
		if got := agentEnvFor([]string{n + "=1"}, nil); len(got) != 0 {
			t.Errorf("%s passes without the owner: %q", n, got)
		}
	}
	for _, n := range []string{"GOPRIVATE", "GONOSUMDB", "GONOPROXY", "GRADLE_USER_HOME", "NVM_HOME", "NVM_SYMLINK"} {
		if got := agentEnvFor([]string{n + "=1"}, nil); len(got) != 1 {
			t.Errorf("%s does not pass: %q", n, got)
		}
	}
}

// The setting: add, remove, replace and validation through the admin API (gw agents env, the settings page, the TUI); serve.json keeps it.
func TestAgentEnvSettingRoundTrip(t *testing.T) {
	s, _ := testServer(t, nil)
	do := func(method, body string) (int, string) {
		st, obj := s.adminDo(method, "/agents/env", []byte(body))
		b, _ := json.Marshal(obj)
		return st, string(b)
	}
	for _, c := range []struct {
		method, body string
		status       int
		want         string
	}{
		{"GET", "", 200, `{"names":[],"unset":[]}`},
		{"PATCH", `{"add":"MGTEST_SECRET"}`, 200, `{"names":["MGTEST_SECRET"],"unset":["MGTEST_SECRET"]}`},
		{"PATCH", `{"add":"PATH"}`, 200, `{"names":["MGTEST_SECRET","PATH"],"unset":["MGTEST_SECRET"]}`}, // D: PATH is set, MGTEST_SECRET is not
		{"PATCH", `{"remove":"PATH"}`, 200, `{"names":["MGTEST_SECRET"],"unset":["MGTEST_SECRET"]}`},
		{"PATCH", `{"add":"OTHER"}`, 200, `{"names":["MGTEST_SECRET","OTHER"],"unset":["MGTEST_SECRET","OTHER"]}`},
		{"PATCH", `{"add":"MGTEST_SECRET"}`, 200, `{"names":["MGTEST_SECRET","OTHER"],"unset":["MGTEST_SECRET","OTHER"]}`},
		{"PATCH", `{"add":"FOO_API_KEY"}`, 400, "is never passed to agents"},
		{"PATCH", `{"add":"1X"}`, 400, "is not a variable name"},
		{"PATCH", `{"remove":"NOPE"}`, 400, "agentEnv: NOPE is not in the list"},
		{"PATCH", `{"remove":"OTHER"}`, 200, `{"names":["MGTEST_SECRET"],"unset":["MGTEST_SECRET"]}`},
		{"PATCH", `{"add":"A","remove":"B"}`, 400, "bad JSON body"},
		{"PATCH", `{}`, 400, "bad JSON body"},
		{"PATCH", `{"names":["X","Y"]}`, 200, `{"names":["X","Y"],"unset":["X","Y"]}`},
		{"PATCH", `{"names":[]}`, 200, `{"names":[],"unset":[]}`},
		{"PATCH", `{"names":["Z"]}`, 200, `{"names":["Z"],"unset":["Z"]}`},
	} {
		if st, b := do(c.method, c.body); st != c.status || !strings.Contains(b, c.want) {
			t.Errorf("%s %s: %d %s, want %d %s", c.method, c.body, st, b, c.status, c.want)
		}
	}
	if c, err := loadServeConfig(s.cfgPath); err != nil || !slices.Equal(c.AgentEnv, []string{"Z"}) {
		t.Errorf("serve.json: %q %v", c.AgentEnv, err)
	}
}

// #189: a hand-edited config must not pass these names either: they are dropped at run time, whatever the case.
func TestAgentEnvForDropsDeniedOwnerNames(t *testing.T) {
	env := []string{"MINIGW_SHARE=x", "minigw_other=x", "GH_ENTERPRISE_TOKEN=x", "GITHUB_ENTERPRISE_TOKEN=x", "CLAUDE_ENV_FILE=x", "BASH_ENV=x", "ENV=x", "PATH=/usr/bin"}
	owner := []string{"MINIGW_SHARE", "MINIGW_OTHER", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN", "CLAUDE_ENV_FILE", "BASH_ENV", "ENV"}
	if got := agentEnvNames(agentEnvFor(env, owner)); !slices.Equal(got, []string{"PATH"}) {
		t.Errorf("%q", got)
	}
}
