package main

import (
	"encoding/json"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

// #79: minigw teče v Claude Code, ko je CLAUDECODE nastavljen ali je eden od prednikov pravi claude (naravni program
// ali node s cli.js Claude Code). Samo CLAUDECODE bi agent obšel z »env -u CLAUDECODE minigw -p --continue …«. Človek v
// terminalu in skripte uporabnika zunaj Claude Code ostanejo brez omejitev (D2). Varovalo, ne meja: proces, ki se
// odcepi od drevesa (dvojni fork, setsid), ali drug harness ni prepoznan; MINIGW_ALLOW_UNSAFE_FLAGS=1 omejitve izklopi.

// procFunc: starš, program in argumenti procesa pid; err = ni berljivo (veriga se ne da preveriti).
type procFunc func(pid int) (ppid int, exe string, args []string, err error)

// maxAncestors: največ toliko prednikov (zanka v podatkih ali zelo globoko drevo = preverjanje ni uspelo).
const maxAncestors = 64

// claudeCaller: v verigi prednikov od ppid je claude. Ko veriga ni berljiva, šteje kot Claude Code, razen s terminalom
// na stdin (fail safe: človek tipka, agent ga nima).
func claudeCaller(ppid int, info procFunc, terminal bool) bool {
	pid := ppid
	for range maxAncestors {
		parent, exe, args, err := info(pid)
		if err != nil {
			return !terminal
		}
		if isClaudeProc(exe, args) {
			return true
		}
		if parent <= 0 || parent == pid {
			return false
		}
		pid = parent
	}
	return !terminal
}

// isClaudeProc: program ali argv[0] je claude (claude.exe), naravni namestitveni program (…/claude/versions/<različica>)
// ali node/bun z …/claude-code/cli.js.
func isClaudeProc(exe string, args []string) bool {
	norm := func(s string) string { return strings.ToLower(strings.ReplaceAll(s, `\`, "/")) }
	name := func(s string) string { return strings.TrimSuffix(path.Base(norm(s)), ".exe") }
	if name(exe) == "claude" || len(args) > 0 && name(args[0]) == "claude" || strings.Contains(norm(exe), "/claude/versions/") {
		return true
	}
	if n := name(exe); (n == "node" || n == "bun") && len(args) > 1 {
		for _, a := range args[1:] {
			if strings.HasSuffix(norm(a), "/claude-code/cli.js") {
				return true
			}
		}
	}
	return false
}

// markClaudeCode: claude med predniki → CLAUDECODE=1 v tem procesu, zato vsa varovala (checkClaudeArgs, promptGuard,
// unsafeWork) veljajo kot ob spremenljivki; gnezdeni claude ga ne dobi (claudeEnv).
func markClaudeCode() {
	if os.Getenv("CLAUDECODE") == "" && claudeCaller(os.Getppid(), procLookup(), stdinTerminal()) {
		os.Setenv("CLAUDECODE", "1")
	}
}

// sessionRe: ID seje claude (UUID); drugo v datoteki se ne preda claude.
var sessionRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// continueSession (#79): -c/--continue → --resume <zadnja seja minigw v tej mapi>. claude -c bi nadaljeval najnovejši
// pogovor v mapi iz ~/.claude, tudi tistega brez minigw, in njegov prepis poslal prehodu. Brez zapisa (last "") se
// možnost izpusti: nov pogovor. Po »--« so le argumenti.
func continueSession(args []string, last string) (out []string, dropped bool) {
	for i, a := range args {
		switch {
		case a == "--":
			return append(out, args[i:]...), dropped
		case a == "-c" || a == "--continue":
			if last == "" {
				dropped = true
			} else {
				out = append(out, "--resume", last)
			}
		default:
			out = append(out, a)
		}
	}
	return out, dropped
}

// lastSessionFile: zadnja seja minigw po mapah (JSON {mapa: ID}); ista datoteka kot minigw v aplikaciji na macOS.
func lastSessionFile() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "minigw", "last-session.json")
}

func loadLastSessions() map[string]string {
	m := map[string]string{}
	if f := lastSessionFile(); f != "" {
		b, _ := os.ReadFile(f)
		json.Unmarshal(b, &m)
	}
	return m
}

// lastSession: zadnja seja minigw v mapi cwd ("" = ni).
func lastSession(cwd string) string {
	if s := loadLastSessions()[cwd]; sessionRe.MatchString(s) {
		return s
	}
	return ""
}

// saveLastSession: sid je zadnja seja minigw v mapi cwd.
// ponytail: dva hkratna zagona v različnih mapah lahko izgubita en zapis (zadnji zapiše); tedaj -c začne nov pogovor.
func saveLastSession(cwd, sid string) {
	f := lastSessionFile()
	if f == "" || !sessionRe.MatchString(sid) {
		return
	}
	m := loadLastSessions()
	if m[cwd] == sid {
		return
	}
	m[cwd] = sid
	b, _ := json.Marshal(m)
	os.MkdirAll(filepath.Dir(f), 0o700)
	tmp := f + ".tmp" + newUUID()[:8]
	if os.WriteFile(tmp, b, 0o600) == nil && os.Rename(tmp, f) != nil {
		os.Remove(tmp)
	}
}
