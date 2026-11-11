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

// streamArgs (#78): claude -p brez izbranega --output-format teče s stream-json in deltami; minigw sproti izpiše vsa
// besedila modela (ne le zadnjega sporočila), tudi agenta prehoda, ki vse pošlje v enem bloku. Izbran --output-format gre naprej nespremenjen.
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
	return append([]string{"--output-format", "stream-json", "--verbose", "--include-partial-messages"}, args...), true
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

// toolMark: oznaka orodja agenta prehoda v besedilu (StreamTranslator, translator.feed: »⏺ Bash«) kot cel delta; ime le
// toolName, sicer je delta besedilo odgovora (model ne ponaredi vrstice minigw: z U+2028, bidi ali dolgim imenom).
var toolMark = regexp.MustCompile(`^\n?⏺ ([A-Za-z0-9_.:@/-]{1,80})\n$`)

// toolName: ime orodja, ki gre v vrstico minigw: · (drugačno je »tool«).
var toolName = regexp.MustCompile(`^[A-Za-z0-9_.:@/-]{1,80}$`)

// lastMax: besedilo bloka za opozorilo backRef (agent prehoda pošlje vse v enem bloku).
const lastMax = 64 << 10

// streamText (#78): dogodki claude --output-format stream-json → besedilo modela na out sproti (delte
// --include-partial-messages; brez njih celi bloki), prazna vrstica med bloki; uporaba orodja (tudi oznaka ⏺ agenta
// prehoda) kot kratka vrstica na errOut; končni result ni izpisan dvakrat in konča branje. failed = result z napako.
func streamText(in io.Reader, out, errOut io.Writer) (failed bool) {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 64<<10), streamLineMax)
	n, last := 0, ""              // izpisani bloki, besedilo zadnjega
	partial, open := false, false // partial: delte namesto celih blokov; open: blok se izpisuje
	// pend: nove vrstice, ki jih besedilo želi na koncu (največ ena prazna vrstica); nl: že izpisane. Prva gre takoj,
	// da je vrstica cela pred oznako orodja (bralec po vrsticah, npr. časovni žigi, sicer dobi stavek šele z naslednjim).
	pend, nl := 0, 0
	lineEnd := func() {
		if pend > 0 && nl == 0 {
			fmt.Fprint(out, "\n")
			nl = 1
		}
	}
	text := func(t string) {
		t = printable(t)
		if !open {
			if strings.TrimSpace(t) == "" {
				return
			}
			if n > 0 {
				pend = 2
			}
			open, n, last = true, n+1, ""
		}
		if len(last) < lastMax {
			last += t
		}
		body := strings.TrimRight(t, "\n")
		if body != "" {
			fmt.Fprint(out, strings.Repeat("\n", max(pend-nl, 0))+body)
			pend, nl = 0, 0
		}
		pend = min(pend+len(t)-len(body), 2)
		lineEnd()
	}
	tool := func(name string) {
		if !toolName.MatchString(name) {
			name = "tool"
		}
		fmt.Fprintln(errOut, "minigw: · "+name)
	}
	end := func() { open, pend = false, 0 }
	for sc.Scan() {
		var ev struct {
			Type    string  `json:"type"`
			IsError bool    `json:"is_error"`
			Result  string  `json:"result"`
			Parent  *string `json:"parent_tool_use_id"`
			Event   struct {
				Type  string
				Delta struct{ Type, Text string }
			} `json:"event"`
			Message struct {
				Content []struct{ Type, Text, Name string } `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		switch ev.Type {
		case "stream_event":
			partial = true
			if ev.Parent != nil { // notranji podagent
				break
			}
			switch {
			case ev.Event.Type == "content_block_delta" && ev.Event.Delta.Type == "text_delta":
				if m := toolMark.FindStringSubmatch(ev.Event.Delta.Text); m != nil {
					if open { // vrstica pred oznako je cela, preden gre oznaka na stderr
						pend = 2
						lineEnd()
					}
					tool(m[1])
				} else {
					text(ev.Event.Delta.Text)
				}
			case ev.Event.Type == "content_block_stop":
				end()
			}
		case "assistant":
			for _, c := range ev.Message.Content {
				if c.Type == "text" && !partial {
					text(c.Text)
					end()
				} else if c.Type == "tool_use" {
					tool(c.Name)
				}
			}
		case "result":
			if ev.IsError {
				failed = true
				// vsaka vrstica s predpono claude: — model ne more ponarediti vrstice minigw:, ki ji skill zaupa
				for _, l := range strings.Split(printable(strings.TrimRight(ev.Result, "\n")), "\n") {
					fmt.Fprintln(errOut, "claude: "+l)
				}
			} else if n == 0 {
				text(ev.Result)
			}
		}
		if ev.Type == "result" { // zadnji dogodek claude -p
			break
		}
	}
	if n > 0 && nl == 0 {
		fmt.Fprintln(out)
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
