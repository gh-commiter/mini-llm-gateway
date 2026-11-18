package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

type mcpFixture struct {
	Initialize, Initialized string
	Tools                   []any
	Search                  string
	Cases                   []struct {
		Name, Frame, Call, Query, ID string
		Code                         int
		Tools, Silent                bool
		Result                       map[string]any
	}
}

func mcpFX(t *testing.T) mcpFixture {
	t.Helper()
	var f mcpFixture
	readFixture(t, "kb-mcp.json", &f)
	return f
}
func mcpInput(t *testing.T, frames ...string) string {
	f := mcpFX(t)
	return f.Initialize + "\n" + f.Initialized + "\n" + strings.Join(frames, "\n") + "\n"
}
func mcpMessages(t *testing.T, b []byte) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range bytes.Split(bytes.TrimSpace(b), []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		var v map[string]any
		if json.Unmarshal(line, &v) != nil || v["jsonrpc"] != "2.0" {
			t.Fatalf("contaminated stdout: %q", line)
		}
		out = append(out, v)
	}
	return out
}
func TestMCPSharedProtocol(t *testing.T) {
	f := mcpFX(t)
	for _, c := range f.Cases {
		t.Run(c.Name, func(t *testing.T) {
			var out bytes.Buffer
			calls := 0
			err := mcpLoop(strings.NewReader(mcpInput(t, c.Frame)), &out, func(name string, args map[string]json.RawMessage) (any, error) {
				calls++
				if name != c.Call {
					t.Fatal(name)
				}
				if c.ID != "" && string(args["id"]) != c.ID {
					t.Fatal(string(args["id"]))
				}
				if c.Query != "" {
					var q string
					json.Unmarshal(args["query"], &q)
					if q != c.Query {
						t.Fatal(q)
					}
				}
				return map[string]any{"ok": true}, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			replies := mcpMessages(t, out.Bytes())
			count := 2
			if c.Silent {
				count = 1
			}
			if len(replies) != count {
				t.Fatal(out.String())
			}
			init := replies[0]["result"].(map[string]any)
			if init["protocolVersion"] != mcpVersion {
				t.Fatal(init)
			}
			expected := 0
			if c.Call != "" {
				expected = 1
			}
			if calls != expected {
				t.Fatal("calls", calls)
			}
			if c.Silent {
				return
			}
			last := replies[1]
			if c.Code != 0 {
				if last["error"].(map[string]any)["code"] != float64(c.Code) {
					t.Fatal(last)
				}
				return
			}
			result := last["result"].(map[string]any)
			if c.Result != nil && !reflect.DeepEqual(result, c.Result) {
				t.Fatal(result)
			}
			if c.Tools {
				if !reflect.DeepEqual(result["tools"], f.Tools) {
					t.Fatal(result)
				}
				if len(result["tools"].([]any)) != 2 {
					t.Fatal(result)
				}
			}
			if c.Call != "" && result["isError"] != false {
				t.Fatal(result)
			}
		})
	}
}
func TestMCPBoundsEOFAndErrors(t *testing.T) {
	for _, input := range []string{"", `{"jsonrpc":"2.0","id":1,"method":"ping"}`, strings.Repeat("x", mcpFrame+1) + "\n"} {
		var out bytes.Buffer
		if err := mcpLoop(strings.NewReader(input), &out, nil); err != nil {
			t.Fatal(err)
		}
		msgs := mcpMessages(t, out.Bytes())
		if input == "" && len(msgs) != 0 || input != "" && len(msgs) != 1 {
			t.Fatal(out.String())
		}
	}
	for _, huge := range []bool{false, true} {
		var out bytes.Buffer
		input := mcpInput(t, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"note_read","arguments":{"id":1}}}`)
		err := mcpLoop(strings.NewReader(input), &out, func(string, map[string]json.RawMessage) (any, error) {
			if huge {
				return map[string]any{"title": strings.Repeat("\x00", mcpReply)}, nil
			}
			return nil, fmt.Errorf("denied\x1b\u202e\nreference")
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range bytes.Split(bytes.TrimSpace(out.Bytes()), []byte{'\n'}) {
			if len(line) > mcpReply {
				t.Fatal("oversize reply")
			}
		}
		msgs := mcpMessages(t, out.Bytes())
		if !huge {
			text := msgs[1]["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
			if strings.ContainsAny(text, "\x1b\u202e") {
				t.Fatal(text)
			}
		}
	}
}
func TestMCPFrozenRoutesAndSanitization(t *testing.T) {
	f := mcpFX(t)
	for _, run := range []bool{false, true} {
		t.Run(fmt.Sprint(run), func(t *testing.T) {
			var out bytes.Buffer
			connections, calls := 0, 0
			env := &projectEnv{in: strings.NewReader(mcpInput(t, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"note_read","arguments":{"id":9007199254740993}}}`, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"kb_search","arguments":{"query":"x &project=2"}}}`)), out: &out, agent: run,
				connect: func() (connection, string) { connections++; return connection{URL: "frozen"}, "" },
				do: func(c connection, m, p string, h map[string]string, b []byte) (int, []byte, http.Header) {
					if c.URL != "frozen" || m != "GET" || len(h) != 0 || len(b) != 0 {
						t.Fatal(c, m, p)
					}
					if p == apiProjects+"/slug" {
						return 200, []byte(`{"id":7}`), nil
					}
					calls++
					base := "/minigw/v1"
					if !run {
						base = apiProjects + "/7"
					}
					if calls == 1 {
						if p != base+"/notes/9007199254740993" {
							t.Fatal(p)
						}
						v, _ := json.Marshal(map[string]any{"class": "data", "title": "a\x1b\u202eb", "author": "x\u200by", "body": strings.Repeat("界", 6000), "provenance": map[string]any{"device": "d\x1b"}})
						return 200, v, nil
					}
					if p != base+"/kb/search?q=x+%26project%3D2" {
						t.Fatal(p)
					}
					return 200, []byte(f.Search), nil
				}}
			args := []string{"--project", "slug"}
			if run {
				args = nil
				env.runConn = &connection{URL: "frozen"}
			}
			if err := env.kbMCP(args); err != nil {
				t.Fatal(err)
			}
			if connections != map[bool]int{true: 0, false: 1}[run] || calls != 2 {
				t.Fatal(connections, calls)
			}
			msgs := mcpMessages(t, out.Bytes())
			text := msgs[1]["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
			var note map[string]any
			if json.Unmarshal([]byte(strings.TrimPrefix(text, mcpReference+"\n")), &note) != nil {
				t.Fatal(text)
			}
			body := note["body"].(string)
			if !utf8.ValidString(body) || len(body) > 16<<10 || !strings.HasSuffix(body, "\n[truncated]") || note["truncated"] != true || note["title"] != "ab" || note["author"] != "xy" {
				t.Fatal(note["title"], len(body), note["truncated"])
			}
			if strings.Contains(out.String(), "secret") {
				t.Fatal("private search hit")
			}
			search := msgs[2]["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
			if !strings.Contains(search, `"id":9007199254740993`) {
				t.Fatal(search)
			}
		})
	}
}
func TestMCPRunFailClosedAndConfig(t *testing.T) {
	for _, vars := range []map[string]string{{"MINIGW_AGENT_URL": ""}, {"MINIGW_AGENT_AUTH_FILE": "missing"}, {"MINIGW_AGENT_URL": "http://example.com", "MINIGW_AGENT_AUTH_FILE": "missing"}} {
		t.Run(fmt.Sprint(vars), func(t *testing.T) {
			for k, v := range vars {
				t.Setenv(k, v)
			}
			var out bytes.Buffer
			env := &projectEnv{out: &out, agent: runEnvironment(), connect: func() (connection, string) { t.Fatal("member fallback"); return connection{}, "" }}
			if err := env.kbMCP(nil); err == nil {
				t.Fatal("accepted credentials")
			}
			if err := env.kbMCP([]string{"--project", "same"}); err == nil {
				t.Fatal("accepted project")
			}
			if err := env.kbMCP([]string{"--print-config"}); err != nil {
				t.Fatal(err)
			}
			var v map[string]any
			if json.Unmarshal(out.Bytes(), &v) != nil || strings.Contains(out.String(), "MINIGW_AGENT") {
				t.Fatal(out.String())
			}
		})
	}
	var out bytes.Buffer
	env := &projectEnv{out: &out, connect: func() (connection, string) { t.Fatal("config connected"); return connection{}, "" }}
	if err := env.kbMCP([]string{"--print-config", "--project", "a\"\\b"}); err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		MCPServers map[string]struct {
			Command string
			Args    []string
		}
	}
	if json.Unmarshal(out.Bytes(), &cfg) != nil {
		t.Fatal(out.String())
	}
	server := cfg.MCPServers["minigw-kb"]
	if !filepath.IsAbs(server.Command) || server.Args[3] != "a\"\\b" {
		t.Fatal(server)
	}
}

func TestMCPExpiredRunNeverConnectsMember(t *testing.T) {
	dir := t.TempDir()
	auth := filepath.Join(dir, "auth")
	if err := os.WriteFile(auth, []byte("expired"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MINIGW_AGENT_URL", "http://127.0.0.1:1")
	t.Setenv("MINIGW_AGENT_AUTH_FILE", auth)
	c := agentConn()
	if c == nil {
		t.Fatal("valid credential shape")
	}
	env := &projectEnv{agent: true, runConn: c, in: strings.NewReader(mcpInput(t, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"note_read","arguments":{"id":1}}}`)), out: io.Discard, connect: func() (connection, string) { t.Fatal("fallback"); return connection{}, "" }, do: func(connection, string, string, map[string]string, []byte) (int, []byte, http.Header) {
		return 401, nil, nil
	}}
	if err := env.kbMCP(nil); err != nil {
		t.Fatal(err)
	}
}

func TestMCPMemberEncryptedGatewayAndRevocation(t *testing.T) {
	c := newClientFx(t)
	dir := t.TempDir()
	shared, _ := c.f.create("ana", "fakture", "structure")
	private, _ := c.f.create("ana", "fakture", "private")
	env, out, _ := c.env("ana", dir, "")
	if err := env.project([]string{"link", "fakture"}); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	read := func(id int64) string {
		return fmt.Sprintf(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"note_read","arguments":{"id":%d}}}`, id)
	}
	env.in = strings.NewReader(mcpInput(t, read(shared), read(private), read(shared)))
	calls := 0
	original := env.do
	env.do = func(conn connection, m, path string, h map[string]string, b []byte) (int, []byte, http.Header) {
		calls++
		if calls == 3 {
			if err := c.f.s.core.RemoveBinding(t0, devID(c.f.dev["ana"])); err != nil {
				t.Fatal(err)
			}
		}
		return original(conn, m, path, h, b)
	}
	if err := env.kbMCP(nil); err != nil {
		t.Fatal(err)
	}
	replies := mcpMessages(t, out.Bytes())
	for i, want := range []bool{false, true, true} {
		if replies[i+1]["result"].(map[string]any)["isError"] != want {
			t.Fatal(out.String())
		}
	}
}

// C3: a hub hit and a hub note reach the model with their origin (from, upstream, asOf) and the marker line at the start of the text,
// in both modes (client/testdata/kb-search-upstream.json, shared with the Swift client).
func TestMCPCarriesTheUpstreamMarker(t *testing.T) {
	var f struct{ Marker, End, Search, Note, Text string }
	readFixture(t, "kb-search-upstream.json", &f)
	for _, run := range []bool{false, true} {
		var out bytes.Buffer
		env := &projectEnv{in: strings.NewReader(mcpInput(t, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"kb_search","arguments":{"query":"orbit"}}}`, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"note_read","arguments":{"id":1125899906842625}}}`)), out: &out, agent: run,
			connect: func() (connection, string) { return connection{URL: "x"}, "" },
			do: func(_ connection, m, p string, _ map[string]string, _ []byte) (int, []byte, http.Header) {
				switch {
				case p == apiProjects+"/slug":
					return 200, []byte(`{"id":1}`), nil
				case strings.Contains(p, "/kb/search?"):
					return 200, []byte(f.Search), nil
				case strings.HasSuffix(p, "/notes/1125899906842625"):
					return 200, []byte(f.Note), nil
				}
				t.Fatal(p)
				return 0, nil, nil
			}}
		args := []string{"--project", "slug"}
		if run {
			args, env.runConn = nil, &connection{URL: "x"}
		}
		if err := env.kbMCP(args); err != nil {
			t.Fatal(err)
		}
		msgs := mcpMessages(t, out.Bytes())
		text := func(i int) map[string]any {
			s := msgs[i]["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
			var v map[string]any
			if !strings.HasPrefix(s, mcpReference+"\n") || json.Unmarshal([]byte(strings.TrimPrefix(s, mcpReference+"\n")), &v) != nil {
				t.Fatal(s)
			}
			return v
		}
		hits := text(1)["hits"].([]any)
		for _, h := range hits[1:] {
			d := h.(map[string]any)
			if !strings.HasPrefix(d["from"].(string), "hub:hub ") || d["upstream"] != "hub" || d["asOf"] != "2026-11-18T10:00:00Z" {
				t.Fatal(d)
			}
		}
		if _, ok := hits[2].(map[string]any)["project"]; ok || hits[2].(map[string]any)["team"] != "acme" {
			t.Fatal("a team hit kept its hidden id", hits[2])
		}
		n := text(2)
		body := n["body"].(string)
		if !strings.HasPrefix(body, f.Marker+"\n") || !strings.HasSuffix(body, "\n"+f.End) || n["text"] != f.Text || n["from"] != "hub:hub (there: project:shop)" || n["source"] != "project:shop" || n["upstream"] != "hub" || n["asOf"] == nil {
			t.Fatal(n)
		}
		lines := strings.Split(body, "\n")
		for _, l := range lines[1 : len(lines)-1] { // the text content: nothing of the hub's starts a line
			if l != ">" && !strings.HasPrefix(l, "> ") {
				t.Fatalf("an unquoted line %q", l)
			}
		}
	}
}
