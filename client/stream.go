package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"
)

// streamArgs (#78): claude -p brez izbranega --output-format teče s stream-json; minigw sproti izpiše vsa besedila modela
// (ne le zadnjega sporočila). Izbran --output-format gre naprej nespremenjen.
func streamArgs(args []string) ([]string, bool) {
	print := false
	for _, a := range args {
		if a == "--" {
			break
		}
		if a == "--output-format" || strings.HasPrefix(a, "--output-format=") {
			return args, false
		}
		print = print || a == "-p" || a == "--print"
	}
	if !print {
		return args, false
	}
	return append([]string{"--output-format", "stream-json", "--verbose"}, args...), true
}

// helperModelEnv (#78): model ponudnika (ne opus/sonnet/haiku/fable): pomožni klici Claude Code (naslov, povzetki,
// podagent Explore) gredo k istemu modelu; agenti (Codex, Grok, Copilot, Siri AI, Apple FM) in modeli Claude ne, ne k claude-haiku-…, ki ga prehod morda nima (404). Uporabnikove vrednosti ostanejo.
func helperModelEnv(args []string, getenv func(string) string) []string {
	model := ""
	for i, a := range args {
		if a == "--" {
			break
		}
		if a == "--model" && i+1 < len(args) {
			model = args[i+1]
		} else if v, ok := strings.CutPrefix(a, "--model="); ok {
			model = v
		}
	}
	agent := isAgentModel(model) || slices.Contains([]string{"claude-apple-fm", "apple-fm", "claude-siri-ai", "siri-ai"}, model)
	if _, _, claude := cliMatch(strings.ToLower(model)); model == "" || claude || agent {
		return nil
	}
	var env []string
	for _, k := range []string{"ANTHROPIC_DEFAULT_HAIKU_MODEL", "ANTHROPIC_SMALL_FAST_MODEL"} {
		v := getenv(k)
		if v == "" {
			v = model
		}
		env = append(env, k+"="+v)
	}
	return env
}

// streamLineMax: najdaljša vrstica stream-json (enako v Swift Profiles.StreamText.lineMax).
const streamLineMax = 64 << 20

// printable: besedilo modela brez nadzornih znakov terminala (C0 razen \n in \t, DEL, C1).
func printable(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 && r != '\n' && r != '\t' || r >= 0x7f && r <= 0x9f {
			return -1
		}
		return r
	}, s)
}

// backRef: končno sporočilo se sklicuje na prejšnje besedilo (#78).
var backRef = regexp.MustCompile(`(?i)\babove\b|\bsee (the )?previous\b|zgoraj`)

// streamText (#78): dogodki claude --output-format stream-json → vsak blok besedila modela na out (prazna vrstica med
// njimi), uporaba orodja kot kratka vrstica na errOut; končni result ni izpisan dvakrat in konča branje. failed = result z napako.
func streamText(in io.Reader, out, errOut io.Writer) (failed bool) {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 64<<10), streamLineMax)
	n, last := 0, ""
	emit := func(t string) {
		if n > 0 {
			fmt.Fprintln(out)
		}
		fmt.Fprintln(out, printable(strings.TrimRight(t, "\n")))
		n, last = n+1, t
	}
	for sc.Scan() {
		var ev struct {
			Type    string `json:"type"`
			IsError bool   `json:"is_error"`
			Result  string `json:"result"`
			Message struct {
				Content []struct{ Type, Text, Name string } `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		switch ev.Type {
		case "assistant":
			for _, c := range ev.Message.Content {
				if c.Type == "text" && strings.TrimSpace(c.Text) != "" {
					emit(c.Text)
				} else if c.Type == "tool_use" {
					fmt.Fprintln(errOut, "minigw: · "+strings.NewReplacer("\n", "", "\t", "").Replace(printable(c.Name)))
				}
			}
		case "result":
			if ev.IsError {
				failed = true
				// vsaka vrstica s predpono claude: — model ne more ponarediti vrstice minigw:, ki ji skill zaupa
				for _, l := range strings.Split(printable(strings.TrimRight(ev.Result, "\n")), "\n") {
					fmt.Fprintln(errOut, "claude: "+l)
				}
			} else if n == 0 && strings.TrimSpace(ev.Result) != "" {
				emit(ev.Result)
			}
		}
		if ev.Type == "result" { // zadnji dogodek claude -p
			break
		}
	}
	if err := sc.Err(); err != nil && !errors.Is(err, os.ErrClosed) {
		msg := err.Error()
		if errors.Is(err, bufio.ErrTooLong) {
			msg = "line longer than 64 MB"
		}
		fmt.Fprintln(errOut, "minigw: cannot read claude's output: "+msg)
		failed = true
	}
	if n == 1 && backRef.MatchString(last) {
		fmt.Fprintln(errOut, "minigw: warning: the answer refers to earlier text that the model did not send; it may be incomplete")
	}
	return failed
}
