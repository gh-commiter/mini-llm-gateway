package main

// minigw v agentu prehoda (MINIGW_AGENT_URL, MINIGW_AGENT_AUTH_FILE; serve_agentcalls.go): -p pošlje vprašanje naravnost na
// vrata izvajanja in izpiše odgovor, brez vgnezdenega Claude Code.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
)

// agentProxy: v sandboxu Claude Code je loopback dosegljiv le prek njegovega posrednika (HTTP_PROXY), ki ga Go za
// loopback sicer preskoči (NO_PROXY ga tudi našteje); brez posrednika neposredno. I5: le posrednik na loopbacku (posrednik
// sandboxa), nikoli drug (podjetniški, podtaknjen v .envrc): vprašanja ne gredo z računalnika.
func agentProxy(*http.Request) (*url.URL, error) {
	for _, k := range []string{"HTTP_PROXY", "http_proxy"} {
		if v := os.Getenv(k); v != "" {
			u, err := url.Parse(v)
			if err != nil || u.Scheme != "http" {
				return nil, nil
			}
			if h := u.Hostname(); h == "localhost" || (net.ParseIP(h) != nil && net.ParseIP(h).IsLoopback()) {
				return u, nil
			}
			return nil, nil
		}
	}
	return nil, nil
}

// agentBody: telo zahteve; modeli agentov (claude-cli, codex …) delajo v mapi le z orodji, zato eno navidezno.
func agentBody(model, question string) []byte {
	body := map[string]any{"model": model, "max_tokens": 32000, "stream": true,
		"messages": []map[string]any{{"role": "user", "content": question}}}
	if isAgentModel(model) {
		body["tools"] = []map[string]any{{"name": "none", "description": "Not used.", "input_schema": map[string]any{"type": "object"}}}
	}
	b, _ := json.Marshal(body)
	return b
}

// agentAskArgs: model in besede vprašanja iz »-p --model <id> [--] vprašanje«; po »--« je vse vprašanje (N5).
// bad = prva nedovoljena možnost. --minigw-ro, --minigw-work=… in --minigw-role izpusti (kot Swift, ki jih odstrani prej).
func agentAskArgs(args []string) (model string, words []string, bad string) {
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--":
			return model, append(words, args[i+1:]...), ""
		case a == "-p" || a == "--print", a == "--minigw-ro", strings.HasPrefix(a, "--minigw-work="), strings.HasPrefix(a, "--minigw-role="):
		case a == "--minigw-role" && i+1 < len(args):
			i++
		case a == "--model" && i+1 < len(args):
			model, i = args[i+1], i+1
		case strings.HasPrefix(a, "--model="):
			model = strings.TrimPrefix(a, "--model=")
		case strings.HasPrefix(a, "-"):
			return model, words, a
		default:
			words = append(words, a)
		}
	}
	return model, words, ""
}

// agentAsk: minigw -p --model <id> "vprašanje" (ali vprašanje na stdin); besedilo odgovora sproti na stdout.
func agentAsk(c *connection, args []string) {
	model, words, bad := agentAskArgs(args)
	if bad != "" {
		fail("inside a gateway agent, minigw -p takes only --model <id> and the question, not " + bad)
	}
	if model == "" {
		fail(`usage: minigw -p --model <id> "question"   (minigw models lists the models)`)
	}
	q := strings.Join(words, " ")
	if q == "" {
		b, _ := io.ReadAll(io.LimitReader(os.Stdin, 16<<20))
		q = string(b)
	}
	req, _ := http.NewRequest("POST", c.URL+"/v1/messages", bytes.NewReader(agentBody(model, q)))
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fail("cannot reach the gateway: " + err.Error())
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		fail(fmt.Sprintf("HTTP %d: %s", resp.StatusCode, agentError(b)))
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data: ")
		if !ok {
			continue
		}
		var e struct {
			Type  string
			Delta struct{ Type, Text string }
			Error struct{ Message string }
		}
		if json.Unmarshal([]byte(data), &e) != nil {
			continue
		}
		switch {
		case e.Type == "content_block_delta" && e.Delta.Type == "text_delta":
			fmt.Print(e.Delta.Text)
		case e.Type == "error":
			fmt.Println()
			fail(e.Error.Message)
		}
	}
	fmt.Println()
	if sc.Err() != nil {
		fail("the answer was cut off: " + sc.Err().Error())
	}
}

// agentError: sporočilo iz {"error":{"message":…}} ali začetek odgovora.
func agentError(b []byte) string {
	var e struct{ Error struct{ Message string } }
	if json.Unmarshal(b, &e) == nil && e.Error.Message != "" {
		return e.Error.Message
	}
	return strings.TrimSpace(string(b[:min(len(b), 300)]))
}
