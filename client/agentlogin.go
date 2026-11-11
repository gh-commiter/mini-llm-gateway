package main

// Prijave agentov (#68, odločitev lastnika): vrstica »Login« za vsakega agenta na vseh površinah nastavitev (stran z
// nastavitvami in okno pladnja, minigw-tui, skrbniški API, minigw gw <agent> token [remove]). Ključi so v shrambi
// ključev (Credential Manager, keyring), nikoli v serve.json ali odgovoru API; prikazano je le stanje.
// Privzeto prijave naročnine; ključ API le izrecno (obračun API). Claude: dolgoživ žeton »claude setup-token«
// (CLAUDE_CODE_OAUTH_TOKEN; v škatli MXC obvezen, prijava iz ~\.claude se v škatlo ne kopira). Copilot: žeton za ukaze.
// Codex: teče zunaj škatle s pravo ~/.codex (osveževanje dela samo): le stanje prijave ChatGPT. Grok nima dolgoživega
// žetona (grok login, ni »setup-token«): v škatlo gre kopija ~\.grok\auth.json, nikoli nazaj; po osvežitvi je morda
// potrebna nova prijava. Izbira uporabnika (#68): namesto prijave ključ API (Claude, Codex, Grok; shramba ključev, le
// izrecno nastavljen gre agentu kot ANTHROPIC_API_KEY / CODEX_API_KEY / XAI_API_KEY); podedovani ključi API iz okolja
// prehoda nikoli ne pridejo do agenta (agentEnv), da nihče ni po nesreči obračunan prek API.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// agentLoginKey: ime ključa v shrambi ključev (»:« ime ponudnika ne more imeti).
func agentLoginKey(kind string) string { return "agent:" + kind }

// agentHasToken: vrsta ima shranjen žeton (ostali le stanje prijave CLI).
func agentHasToken(kind string) bool { return kind == "claude" || kind == "copilot" }

// agentAPIKeyEnv: spremenljivka ključa API, ki ga CLI bere (Copilot ga nima).
var agentAPIKeyEnv = map[string]string{"claude": "ANTHROPIC_API_KEY", "codex": "CODEX_API_KEY", "grok": "XAI_API_KEY"}

// apiKeyNote: enako besedilo na vseh površinah.
const apiKeyNote = "Use an API key instead: billed through the provider's API, not your subscription."

// agentAPIKeyName: ključ API v shrambi ključev.
func agentAPIKeyName(kind string) string { return "agent:" + kind + ":apikey" }

// agentAPIKey: izrecno nastavljen ključ API ali "".
func agentAPIKey(cfgDir, kind string) string {
	if agentAPIKeyEnv[kind] == "" {
		return ""
	}
	t, _ := keyGet(cfgDir, agentAPIKeyName(kind))
	return t
}

// setAgentAPIKey: "" izbriše (nazaj na prijavo naročnine).
func setAgentAPIKey(cfgDir, kind, t string) error {
	if t == "" {
		keyDel(cfgDir, agentAPIKeyName(kind))
		return nil
	}
	_, err := keySet(cfgDir, agentAPIKeyName(kind), t)
	return err
}

// apiKeyProblem: "" = videti veljaven (natisljivi ASCII brez presledkov, 20–512 znakov).
func apiKeyProblem(kind, t string) string {
	if agentAPIKeyEnv[kind] == "" {
		return "apiKey: Claude CLI, Codex and Grok only"
	}
	if len(t) < 20 || len(t) > 512 || strings.ContainsFunc(t, func(r rune) bool { return r < 0x21 || r > 0x7e }) {
		return "apiKey: the provider's API key (no spaces)"
	}
	return ""
}

// agentKeyEnv: ključ API agenta za okolje (le izrecno nastavljen), sicer nil.
func agentKeyEnv(cfgDir, kind string) []string {
	if k := agentAPIKey(cfgDir, kind); k != "" {
		return []string{agentAPIKeyEnv[kind] + "=" + k}
	}
	return nil
}

// agentLoginHint: ena vrstica, kako ključ narediti (enako besedilo na vseh površinah).
var agentLoginHint = map[string]string{
	"claude":  "Create it with `claude setup-token` (your Claude subscription, valid one year); needed in the Windows sandbox.",
	"codex":   "Codex uses this user's ChatGPT sign-in (`codex login`); it refreshes itself.",
	"grok":    "Grok uses a copy of this user's sign-in (`grok login`); after Grok refreshes it, log in again with `grok login`.",
	"copilot": "A fine-grained GitHub token with only the Copilot Requests permission; needed for commands, and for every request when the GitHub CLI (gh auth login) is not signed in on the gateway.",
}

// agentTokenProblem: "" = ključ je videti veljaven za to vrsto (le natisljivi ASCII brez presledkov, do 512 znakov).
func agentTokenProblem(kind, t string) string {
	if kind == "copilot" {
		if !validCopilotToken(t) {
			return "token: a fine-grained personal access token (github_pat_…) with only the Copilot Requests permission"
		}
		return ""
	}
	if !strings.HasPrefix(t, "sk-ant-") || len(t) < 20 || len(t) > 512 || strings.ContainsFunc(t, func(r rune) bool { return r < 0x21 || r > 0x7e }) {
		return "token: the token from claude setup-token (sk-ant-…)"
	}
	return ""
}

// setAgentToken: "" izbriše ključ.
func setAgentToken(cfgDir, kind, t string) error {
	if t == "" {
		keyDel(cfgDir, agentLoginKey(kind))
		return nil
	}
	_, err := keySet(cfgDir, agentLoginKey(kind), t)
	return err
}

// agentToken: shranjen ključ ali "".
func agentToken(cfgDir, kind string) string {
	t, _ := keyGet(cfgDir, agentLoginKey(kind))
	return t
}

// agentLogin: vrstica »Login« za API: stanje (nikoli vrednost) in namig.
func agentLogin(cfgDir, kind string) map[string]any {
	o := map[string]any{"hint": agentLoginHint[kind]}
	if agentAPIKeyEnv[kind] != "" {
		o["apiKey"], o["apiKeyNote"] = agentAPIKey(cfgDir, kind) != "", apiKeyNote
	}
	if o["apiKey"] == true {
		o["state"] = "API key (billed through the provider's API)"
		o["hasToken"] = agentHasToken(kind) && agentToken(cfgDir, kind) != ""
		return o
	}
	switch kind {
	case "codex":
		o["state"] = codexLoginState()
	case "grok":
		o["state"] = grokLoginState()
	default:
		has := agentToken(cfgDir, kind) != ""
		o["state"], o["hasToken"] = map[bool]string{true: "set", false: "not set"}[has], has
	}
	return o
}

// grokLoginState: ~/.grok/auth.json je ali ni (vsebine ne bere).
func grokLoginState() string {
	real, _ := agentAuthFiles(grokKind)
	if st, err := os.Lstat(filepath.Join(real, "auth.json")); err == nil && st.Mode().IsRegular() {
		return "logged in (may need to log in again in Grok after it refreshes)"
	}
	return "not logged in (grok login)"
}

// codexLoginState: stanje prijave iz ~/.codex/auth.json (ChatGPT ali ključ API), brez vrednosti.
func codexLoginState() string {
	real, _ := agentAuthFiles(codexKind)
	b, err := os.ReadFile(filepath.Join(real, "auth.json"))
	if err != nil {
		return "not logged in (codex login)"
	}
	var a struct {
		Key    *string         `json:"OPENAI_API_KEY"`
		Tokens json.RawMessage `json:"tokens"`
	}
	if json.Unmarshal(b, &a) != nil {
		return "unknown (auth.json unreadable)"
	}
	switch {
	case len(a.Tokens) > 0 && string(a.Tokens) != "null":
		return "logged in (ChatGPT)"
	case a.Key != nil && *a.Key != "":
		return "logged in (API key)"
	}
	return "not logged in (codex login)"
}
