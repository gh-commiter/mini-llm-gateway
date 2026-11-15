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

// toolsFor: orodja modela (model brez pripone profila): "" = brez omejitve. --minigw-ro pri modelu ponudnika pomeni read,
// razen z izrecnim --minigw-tools; model agenta je omejen na prehodu (--minigw-ro), zato zanj --minigw-tools ni.
func toolsFor(model, flag string, readOnly bool) (tools, errMsg string) {
	if flag != "" && isAgentModel(model) {
		return "", "--minigw-tools is not available for " + model + ": agents are limited on the gateway, use --minigw-ro"
	}
	if flag == "" && readOnly && model != "" && !isAgentModel(model) {
		return "read", ""
	}
	return flag, ""
}

// toolsArgs: argumenti claude za orodja; --disable-slash-commands le, če ga args še nimajo.
func toolsArgs(tools string, args []string) []string {
	list := ""
	if tools == "read" {
		list = toolsRead
	}
	out := []string{"--tools=" + list, "--strict-mcp-config"}
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
