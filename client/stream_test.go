package main

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestStreamText(t *testing.T) {
	ev := func(lines ...string) string { return strings.Join(lines, "\n") + "\n" }
	text := func(s string) string {
		return `{"type":"assistant","message":{"content":[{"type":"text","text":"` + s + `"}]}}`
	}
	tool := `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{}}]}}`
	user := `{"type":"user","message":{"content":[{"type":"tool_result","content":"secret output"}]}}`
	for _, c := range []struct {
		name, in, out, errOut string
		failed                bool
	}{
		{"multi-turn", ev(`{"type":"system","subtype":"init"}`, text("First."), tool, user, text("See above."),
			`{"type":"result","subtype":"success","is_error":false,"result":"See above."}`),
			"First.\n\nSee above.\n", "minigw: · Bash\n", false},
		{"error", ev(`{"type":"result","subtype":"error_during_execution","is_error":true,"result":"API Error: 500"}`),
			"", "API Error: 500\n", true},
		{"back-reference", ev(text("Kot zgoraj, vse je v redu."), `{"type":"result","is_error":false,"result":"x"}`),
			"Kot zgoraj, vse je v redu.\n", "minigw: warning: the answer refers to earlier text that the model did not send; it may be incomplete\n", false},
		{"result only", ev(`not json`, `{"type":"result","is_error":false,"result":"Done"}`), "Done\n", "", false},
	} {
		var out, errOut strings.Builder
		if failed := streamText(strings.NewReader(c.in), &out, &errOut); failed != c.failed || out.String() != c.out || errOut.String() != c.errOut {
			t.Errorf("%s: failed=%v out=%q err=%q", c.name, failed, out.String(), errOut.String())
		}
	}
}

func TestStreamArgs(t *testing.T) {
	if a, ok := streamArgs([]string{"--model", "m", "-p", "hi"}); !ok || !slices.Equal(a[:3], []string{"--output-format", "stream-json", "--verbose"}) {
		t.Errorf("print mode: %v %v", a, ok)
	}
	for _, a := range [][]string{{"-p", "--output-format", "json", "hi"}, {"-p", "--output-format=text"}, {"hi"}, {"--", "-p"}} {
		if _, ok := streamArgs(a); ok {
			t.Errorf("%v: streamed", a)
		}
	}
}

func TestHelperModelEnv(t *testing.T) {
	none := func(string) string { return "" }
	if e := helperModelEnv([]string{"--model", "claude-deepseek/deepseek-v4-flash", "-p"}, none); !slices.Equal(e, []string{
		"ANTHROPIC_DEFAULT_HAIKU_MODEL=claude-deepseek/deepseek-v4-flash", "ANTHROPIC_SMALL_FAST_MODEL=claude-deepseek/deepseek-v4-flash"}) {
		t.Errorf("provider model: %v", e)
	}
	for _, m := range []string{"opus", "claude-sonnet-5", "claude-cli-fable", "claude-haiku-5-5"} {
		if e := helperModelEnv([]string{"--model=" + m}, none); e != nil {
			t.Errorf("%s: %v", m, e)
		}
	}
	if e := helperModelEnv([]string{"--model", "x"}, func(k string) string { return "mine" }); e[0] != "ANTHROPIC_DEFAULT_HAIKU_MODEL=mine" {
		t.Errorf("user value: %v", e)
	}
}

// runClaude s tokom: error result → 1, tudi ko claude konča z 0.
func TestRunClaudeStream(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake claude is a shell script")
	}
	bin := t.TempDir()
	os.WriteFile(filepath.Join(bin, "claude"), []byte("#!/bin/sh\necho '{\"type\":\"result\",\"is_error\":true,\"result\":\"boom\"}'\n"), 0o755)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if code := runClaude([]string{"-p", "x"}, nil, true); code != 1 {
		t.Fatalf("exit %d", code)
	}
}
