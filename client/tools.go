package main

import (
	"fmt"
	"maps"
	"slices"
	"strings"
)

// #119: --minigw-tools=none|read|edit: orodja modela ponudnika (LiteLLM, OpenRouter), ki teče v lokalnem claude. Pod Claude Code
// jih sme le zmanjšati, zato ni na seznamu claudeSafe. Enako kot Swift Profiles.takeTools (client/testdata/minigw-tools-cases.json).
// Izmerjeno (claude 2.1.322): --tools=<seznam> sam ne zadošča (MCP orodja iz povezav claude.ai ostanejo), zato še
// --strict-mcp-config (brez --mcp-config = brez MCP) in --disable-slash-commands; podagenti in skilli so orodja (Agent,
// Skill), ki jih --tools izpusti. »--tools <seznam>« pogoltne navodilo (variadična možnost), zato vedno »--tools=<seznam>«.
const toolsRead = "Read,Grep,Glob"

// toolsEdit: level edit: Read, Grep, Glob, Edit, Write, no Bash. The CLI matches path rules for Read and Edit only (Edit covers Write):
// files only inside the current folder (./**), the paths below stay refused even inside it (the list of protectedPath in sync.go).
const toolsEdit = "Read,Grep,Glob,Edit,Write"

// editProtected: the protected paths as globs (below the folder or any subfolder). Only the plain names of macOS and Linux; Windows name forms
// (trailing dot or space, 8.3 short names) are not covered, so edit is refused on Windows (editWindowsRefusal).
var editProtected = []string{".git/**", ".git", ".claude/settings*.json", ".claude/hooks/**", ".claude/commands/**", ".claude/agents/**",
	".claude/skills/**", ".husky/**", ".githooks/**", ".vscode/tasks.json", ".envrc"}

// editWindowsRefusal: why edit cannot run on this system, "" = it may.
func editWindowsRefusal(goos string) string {
	if goos == "windows" {
		return "--minigw-tools=edit is not available on Windows yet: its path rules cannot cover Windows name forms (trailing dots and spaces, 8.3 short names)"
	}
	return ""
}

// editRules: --allowedTools and --disallowedTools of the level edit. A rule matches the folder itself and any subfolder (./ and ./**/),
// every letter of a protected name in both cases (a case-insensitive file system).
func editRules() (allow, deny string) {
	return "Read(./**),Grep(./**),Glob(./**),Edit(./**)", strings.Join(editDenyRules(), ",")
}

// editDenyRules: the Edit(...) deny rules, generated from editProtected (the test TestEditRulesCoverProtectedPaths checks them against protectedPath).
func editDenyRules() []string {
	var d []string
	for _, base := range []string{"./", "./**/"} {
		for _, p := range editProtected {
			parts := strings.Split(p, "/")
			for i, part := range parts {
				if part != "**" {
					parts[i] = caseClass(part)
				}
			}
			d = append(d, "Edit("+base+strings.Join(parts, "/")+")")
		}
	}
	return d
}

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
				return nil, "", "--minigw-tools needs a value (none, read or edit)"
			}
			v, ok = args[i+1], true
			i++
		}
		if !ok {
			rest = append(rest, a)
			continue
		}
		if v != "none" && v != "read" && v != "edit" {
			return nil, "", fmt.Sprintf("--minigw-tools: unknown value %q (use none, read or edit)", v)
		}
		value = v
	}
	return rest, value, ""
}

// isAgentID: the name rule for a model the gateway runs as an agent: claude-cli-…, claude-codex|grok|copilot[-…]; capital letters are no agent.
// The Go gateway hides a provider model with such a name (shadowsCLI), so it is never listed as a provider model and the name decides. The Mac
// gateway does not hide it (it serves a provider's "claude-grok-4.7", the Grok agent is not supported there), so toolsFor lets the gateway's
// /v1/models list overrule this rule. The wider isAgentModel (HasPrefix) would count LiteLLM's "claude-codexa" as an agent.
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

// toolsRank: the tool levels from the lowest. The owner of a gateway sets the highest level a provider's models may get (minigw gw provider
// tools); the client reads it from /v1/models (minigw_tools) and refuses a higher level, and sends the level it runs with in the header
// x-claude-mini-tools, which the gateway checks on every request. The tools run in the client's Claude Code: the gateway can only refuse to
// serve, and a modified client can send a lower level than it runs; the real protection of the folder is the client's own permission rules.
var toolsRank = map[string]int{"none": 0, "read": 1, "edit": 2}

// toolsLevelRefusal: "" = the level is within the cap; the capped provider's name goes into the owner's command.
func toolsLevelRefusal(level, cap, provider, model string) string {
	r, ok := toolsRank[level]
	if !ok || r <= toolsRank[cap] {
		if !ok {
			return "unknown tools level " + level
		}
		return ""
	}
	return "--minigw-tools=" + level + " is not allowed for " + model + ": the gateway allows " + cap + " at most. The owner of the gateway can allow it with: minigw gw provider tools " + provider + " " + level + " [--model " + model + "]"
}

// toolsFor: orodja modela (model brez pripone profila, "" = ni --model): "" = brez omejitve. listed(model) = the gateway lists the model as a
// provider model (minigw_provider in /v1/models); it is asked only when the name looks like an agent, and then a provider model is no agent. --minigw-ro (readOnly: tudi
// shranjena nastavitev mape) pomeni read za vse, kar ni zagotovo agent, tudi brez znanega modela (privzeti model, ki ga
// določa gateway: omejitev na agentu ne škodi, njegova orodja tečejo na prehodu); izrecno --minigw-tools velja povsod
// razen za agenta, ki je omejen na prehodu (--minigw-ro). ANTHROPIC_MODEL ne šteje: minigw ga claude ne preda (#7).
func toolsFor(model, flag string, readOnly bool, listed func(string) bool) (tools, errMsg string) {
	if flag == "" && !readOnly {
		return "", ""
	}
	agent := isAgentID(model) && !listed(model)
	if flag != "" && agent {
		return "", "--minigw-tools is not available for " + model + ": agents are limited on the gateway, use --minigw-ro"
	}
	if flag == "edit" && readOnly {
		return "", "--minigw-tools=edit cannot be combined with --minigw-ro (the folder is read-only)"
	}
	if flag == "" && readOnly && !agent {
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
	var out []string
	if tools == "edit" {
		allow, deny := editRules()
		list = toolsEdit
		// dontAsk: what is not allowed is refused, no prompt; the rules go with "=" (a variadic option would swallow the prompt)
		out = []string{"--allowedTools=" + allow, "--disallowedTools=" + deny, "--permission-mode=dontAsk"}
	}
	// --setting-sources= : hooki in dovoljenja iz nastavitev uporabnika, projekta in mape bi tekli mimo seznama orodij (izmerjeno:
	// UserPromptSubmit hook iz .claude/settings.json se zažene tudi pri --tools=Read,Grep,Glob); --settings minigw ostane.
	out = append([]string{"--tools=" + list, "--strict-mcp-config", "--setting-sources="}, out...)
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
	if tools == "edit" {
		return "minigw: tools of this model: read and edit files in this folder (Read, Grep, Glob, Edit, Write; no commands)"
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

// providerToolsLines: the answer of minigw gw provider tools (same text on the Mac client, testdata/provider-tools.json). tools: the provider's
// level, models: the exceptions per model; model != "" = that one model.
func providerToolsLines(name, tools string, models map[string]string, model string) []string {
	if tools == "" {
		tools = "read"
	}
	if model != "" {
		m := strings.TrimPrefix(model, "claude-")
		if v, ok := models[m]; ok {
			return []string{name + "  " + m + "  tools: " + v}
		}
		return []string{name + "  " + m + "  tools: " + tools + " (the provider's)"}
	}
	out := []string{name + "  tools: " + tools}
	for _, m := range slices.Sorted(maps.Keys(models)) {
		out = append(out, "  "+m+"  tools: "+models[m])
	}
	return out
}

// toolsCapOf: the tool cap and the provider of a model from the /v1/models answer (fields minigw_tools and minigw_provider); a gateway
// without the field (older, or the model is not a provider model) counts as read. found = the model is in the list.
func toolsCapOf(list map[string]any, model string) (cap, provider string, found bool) {
	data, _ := list["data"].([]any)
	for _, e := range data {
		m, _ := e.(map[string]any)
		if id, _ := m["id"].(string); id == model || id == "claude-"+model {
			cap, _ = m["minigw_tools"].(string)
			provider, _ = m["minigw_provider"].(string)
			if _, ok := toolsRank[cap]; !ok {
				cap = "read"
			}
			return cap, provider, true
		}
	}
	return "", "", false
}

// editRefusal: why --minigw-tools=edit cannot run, "" = it may. list: the /v1/models answer (nil = the gateway did not answer).
func editRefusal(list map[string]any, model string) string {
	if model == "" {
		return "--minigw-tools=edit needs --model: the gateway sets the highest tool level per model"
	}
	if list == nil {
		return "--minigw-tools=edit: cannot read from the gateway which tools " + model + " may get"
	}
	cap, provider, found := toolsCapOf(list, model)
	if !found {
		return "--minigw-tools=edit: the gateway does not list the model " + model
	}
	if provider == "" {
		provider = "<provider>"
	}
	return toolsLevelRefusal("edit", cap, provider, model)
}

// providerListed: the gateway's /v1/models answer lists the model with a provider (minigw_provider), so it is a provider model whatever its name.
func providerListed(list map[string]any, model string) bool {
	_, provider, found := toolsCapOf(list, model)
	return found && provider != ""
}
