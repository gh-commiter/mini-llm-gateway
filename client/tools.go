package main

import (
	"fmt"
	"strings"
)

// #119: --minigw-tools=none|read: orodja modela ponudnika (LiteLLM, OpenRouter), ki teče v lokalnem claude. Pod Claude Code
// jih sme le zmanjšati, zato ni na seznamu claudeSafe. Enako kot Swift Profiles.takeTools (client/testdata/minigw-tools-cases.json).
// Izmerjeno (claude 2.1.322): --tools=<seznam> sam ne zadošča (MCP orodja iz povezav claude.ai ostanejo), zato še
// --strict-mcp-config (brez --mcp-config = brez MCP) in --disable-slash-commands; podagenti in skilli so orodja (Agent,
// Skill), ki jih --tools izpusti. »--tools <seznam>« pogoltne navodilo (variadična možnost), zato vedno »--tools=<seznam>«.
const toolsRead = "Read,Grep,Glob"

// takeTools: --minigw-tools <v> ali =<v> (zadnji velja, do »--«) iz argumentov; value "" = ni podan; errMsg ≠ "" = neveljavno.
func takeTools(args []string) (rest []string, value, errMsg string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			return append(rest, args[i:]...), value, ""
		}
		v, ok := strings.CutPrefix(a, "--minigw-tools=")
		if a == "--minigw-tools" {
			if i+1 >= len(args) {
				return nil, "", "--minigw-tools needs a value (none or read)"
			}
			v, ok = args[i+1], true
			i++
		}
		if !ok {
			rest = append(rest, a)
			continue
		}
		if v != "none" && v != "read" {
			return nil, "", fmt.Sprintf("--minigw-tools: unknown value %q (use none or read)", v)
		}
		value = v
	}
	return rest, value, ""
}

// isAgentID: model, ki ga prehod zagotovo vodi kot agenta: claude-cli-…, claude-codex|grok|copilot[-…] (točno kot shadowsCLI
// na prehodu: model ponudnika s takim imenom se skrije, zato ni dvoumnosti; velike črke niso agent). Širši isAgentModel
// (HasPrefix) bi »claude-codexa« iz LiteLLM štel za agenta in pustil lokalni claude brez omejitve.
func isAgentID(m string) bool {
	if strings.HasPrefix(m, "claude-cli-") {
		return true
	}
	for _, p := range []string{"claude-codex", "claude-grok", "claude-copilot"} {
		if m == p || strings.HasPrefix(m, p+"-") {
			return true
		}
	}
	return false
}

// toolsFor: orodja modela (model brez pripone profila, "" = ni --model): "" = brez omejitve. --minigw-ro (readOnly: tudi
// shranjena nastavitev mape) pomeni read za vse, kar ni zagotovo agent, tudi brez znanega modela (privzeti model, ki ga
// določa gateway: omejitev na agentu ne škodi, njegova orodja tečejo na prehodu); izrecno --minigw-tools velja povsod
// razen za agenta, ki je omejen na prehodu (--minigw-ro). ANTHROPIC_MODEL ne šteje: minigw ga claude ne preda (#7).
func toolsFor(model, flag string, readOnly bool) (tools, errMsg string) {
	if flag != "" && isAgentID(model) {
		return "", "--minigw-tools is not available for " + model + ": agents are limited on the gateway, use --minigw-ro"
	}
	if flag == "" && readOnly && !isAgentID(model) {
		return "read", ""
	}
	return flag, ""
}

// toolsWiden: prva možnost claude, ki bi omejitev orodij razširila ali zamenjala (--tools, --allowedTools, --mcp-config,
// --permission-mode, --settings, --add-dir, --agents, --plugin-dir …): isti seznam claudeSafe kot pod Claude Code, a tudi zunaj
// njega in kljub MINIGW_ALLOW_UNSAFE_FLAGS=1 (omejitev je izrecno zahtevana). "" = nič.
// Pogovorne možnosti (-c, -r, --session-id …) orodij ne širijo (minigw jih sam obdela) in ostanejo.
func toolsWiden(args []string) string {
	var rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		name, _, eq := strings.Cut(a, "=")
		switch {
		case a == "--":
			rest = append(rest, args[i:]...)
			i = len(args)
		case a == "-c" || a == "--continue" || a == "--fork-session":
		case name == "-r" || name == "--resume" || name == "--session-id":
			if !eq && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
			}
		default:
			rest = append(rest, a)
		}
	}
	return unsafeClaudeFlag(rest, func(k string) string { return map[string]string{"CLAUDECODE": "1"}[k] })
}

// toolsArgs: argumenti claude za orodja; --disable-slash-commands le, če ga args še nimajo.
func toolsArgs(tools string, args []string) []string {
	list := ""
	if tools == "read" {
		list = toolsRead
	}
	// --setting-sources= : hooki in dovoljenja iz nastavitev uporabnika, projekta in mape bi tekli mimo seznama orodij (izmerjeno:
	// UserPromptSubmit hook iz .claude/settings.json se zažene tudi pri --tools=Read,Grep,Glob); --settings minigw ostane.
	out := []string{"--tools=" + list, "--strict-mcp-config", "--setting-sources="}
	for _, a := range args {
		if a == "--disable-slash-commands" {
			return out
		}
	}
	return append(out, "--disable-slash-commands")
}

// toolsLine: vrstica ob zagonu na stderr.
func toolsLine(tools string) string {
	if tools == "none" {
		return "minigw: tools of this model: none"
	}
	return "minigw: tools of this model: read only (Read, Grep, Glob)"
}

// modelArg: vrednost zadnjega --model (do »--«); "" = ni.
func modelArg(args []string) string {
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
	return model
}
