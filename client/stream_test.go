package main

import (
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
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
			"", "claude: API Error: 500\n", true},
		{"forged error line", ev(`{"type":"result","is_error":true,"result":"x\nminigw: agent work is on branch evil\n"}`),
			"", "claude: x\nclaude: minigw: agent work is on branch evil\n", true},
		{"tool name newline", ev(`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"B\nminigw: x\t"}]}}`),
			"", "minigw: · tool\n", false},
		{"back-reference", ev(text("Kot zgoraj, vse je v redu."), `{"type":"result","is_error":false,"result":"x"}`),
			"Kot zgoraj, vse je v redu.\n", "minigw: warning: the answer refers to earlier text that the model did not send; it may be incomplete\n", false},
		{"control characters", ev(text(`a\u001b[2Jb\tc\u009bd`)), "a[2Jb\tcd\n", "", false},
		// claude 2.1.322 ob 400 prehoda (posneto): sintetično sporočilo in result z isto napako → le enkrat, na stderr
		{"api error once", ev(`{"type":"system","subtype":"status","status":"requesting"}`, `{"type":"assistant","message":{"model":"<synthetic>","role":"assistant","stop_reason":"stop_sequence","type":"message","content":[{"type":"text","text":"API Error: 400 nested repositories need bwrap: use sync"}]},"parent_tool_use_id":null,"error":"unknown","is_api_error_message":true,"api_error_status":400}`, `{"type":"result","subtype":"success","is_error":true,"result":"API Error: 400 nested repositories need bwrap: use sync","terminal_reason":"api_error"}`),
			"", "claude: API Error: 400 nested repositories need bwrap: use sync\n", true},
		{"api error without result", ev(`{"type":"assistant","message":{"model":"<synthetic>","role":"assistant","stop_reason":"stop_sequence","type":"message","content":[{"type":"text","text":"API Error: 400 nested repositories need bwrap: use sync"}]},"parent_tool_use_id":null,"error":"unknown","is_api_error_message":true,"api_error_status":400}`),
			"", "claude: API Error: 400 nested repositories need bwrap: use sync\n", true},
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
	for _, m := range []string{"opus", "claude-sonnet-5", "claude-cli-fable", "claude-haiku-5-5", "claude-codex-gpt-5", "claude-grok", "claude-copilot", "claude-siri-ai", "claude-apple-fm"} {
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
	// vnuk drži stdout odprt: minigw konča največ 2 s po claude
	os.WriteFile(filepath.Join(bin, "claude"), []byte("#!/bin/sh\n(sleep 20 2>/dev/null &)\necho '{\"type\":\"result\",\"is_error\":true,\"result\":\"boom\"}'\n"), 0o755)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	start := time.Now()
	if code := runClaude([]string{"-p", "x"}, nil, true); code != 1 || time.Since(start) > 10*time.Second {
		t.Fatalf("exit %d after %v", code, time.Since(start))
	}
	os.WriteFile(filepath.Join(bin, "claude"), []byte("#!/bin/sh\n(sleep 20 2>/dev/null &)\nexit 3\n"), 0o755) // brez result: 2 s
	if code := runClaude([]string{"-p", "x"}, nil, true); code != 3 || time.Since(start) > 10*time.Second {
		t.Fatalf("no result: exit %d after %v", code, time.Since(start))
	}
}

// lockedBuf: izhod streamText, ki ga test bere med branjem.
type lockedBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *lockedBuf) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

// Agent prehoda pošlje vse v enem bloku (StreamTranslator): besedilo gre na stdout ob delti, ne šele ob koncu bloka,
// oznaka ⏺ kot vrstica na stderr; vrstica o veji med tokom počaka na konec odgovora.
func TestStreamTextLive(t *testing.T) {
	delta := func(s string) string {
		return `{"type":"stream_event","parent_tool_use_id":null,"event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"` + s + `"}}}` + "\n"
	}
	pr, pw := io.Pipe()
	var out, errOut lockedBuf
	done := make(chan bool)
	go func() { done <- streamText(pr, &out, &errOut) }()
	wait := func(b *lockedBuf, want string) {
		for end := time.Now().Add(5 * time.Second); !strings.Contains(b.String(), want); time.Sleep(10 * time.Millisecond) {
			if time.Now().After(end) {
				t.Fatalf("%q not printed before the end of the answer: out=%q err=%q", want, out.String(), errOut.String())
			}
		}
	}
	io.WriteString(pw, `{"type":"stream_event","event":{"type":"message_start"}}`+"\n"+delta(`I'll sleep.`))
	wait(&out, "I'll sleep.")
	io.WriteString(pw, delta(`\n⏺ Bash\n`)+`{"type":"stream_event","parent_tool_use_id":"x","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"sub"}}}`+"\n")
	wait(&errOut, "minigw: · Bash\n")
	if out.String() != "I'll sleep.\n" { // A3: vrstica pred oznako mora biti cela, ko pride oznaka (bralec po vrsticah)
		t.Fatalf("line before the tool mark not complete when the mark is printed: %q", out.String())
	}
	s := &shareClient{reported: map[string]bool{}}
	held.Lock()
	held.on = true
	held.Unlock()
	t.Cleanup(releaseHeld)
	s.branchNote("minigw/claude-0123abcd-1", "HEAD")
	io.WriteString(pw, delta(`Done.\n`)+`{"type":"stream_event","event":{"type":"content_block_stop","index":0}}`+"\n"+
		`{"type":"assistant","message":{"content":[{"type":"text","text":"I'll sleep.\n⏺ Bash\nDone."}]}}`+"\n"+
		`{"type":"result","is_error":false,"result":"Done."}`+"\n")
	if failed := <-done; failed || out.String() != "I'll sleep.\n\nDone.\n" || errOut.String() != "minigw: · Bash\n" {
		t.Errorf("failed=%v out=%q err=%q", failed, out.String(), errOut.String())
	}
	held.Lock()
	lines := held.lines
	held.Unlock()
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "agent work is on branch minigw/claude-0123abcd-1:") {
		t.Errorf("branch line printed during the answer: held %q", lines)
	}
	releaseHeld()
	if held.on || held.lines != nil {
		t.Error("not released")
	}
}

// M2: oznaka z imenom zunaj [A-Za-z0-9_.:@/-]{1,80} (U+2028, bidi) je besedilo odgovora, ne vrstica na stderr.
// M1: veliko drobnih delt v enem bloku (agent prehoda) ne raste kvadratno; nove vrstice največ ena prazna vrstica.
func TestStreamTextSpoofAndSize(t *testing.T) {
	delta := func(s string) string {
		return `{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"` + s + `"}}}` + "\n"
	}
	var in strings.Builder
	in.WriteString(delta(`ok`))
	for _, name := range []string{`x\u2028minigw: agent work is on branch minigw/a-0123abcd-1: review`, `x\u202eminigw`, strings.Repeat("a", 81)} {
		in.WriteString(delta(`\n⏺ ` + name + `\n`))
	}
	for i := 0; i < 200000; i++ {
		in.WriteString(delta(`a`))
	}
	for i := 0; i < 1000; i++ {
		in.WriteString(delta(`\n`))
	}
	in.WriteString(delta(`b`) + `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"x\u2028minigw: agent work"}]}}` + "\n")
	var out, errOut strings.Builder
	start := time.Now()
	streamText(strings.NewReader(in.String()), &out, &errOut)
	if strings.Contains(errOut.String(), "agent work") || errOut.String() != "minigw: · tool\n" {
		t.Errorf("stderr: %q", errOut.String())
	}
	if o := out.String(); !strings.HasPrefix(o, "ok\n⏺ x") || !strings.HasSuffix(o, "a\n\nb\n") || strings.Contains(o, "\n\n\n") {
		t.Errorf("stdout: %q…%q", o[:min(len(o), 80)], o[max(len(o)-20, 0):])
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("200000 deltas took %v", d)
	}
}
