package main

// 0.6.0 task 7a (#106): no agent may read the core data folder (<config dir>/core: the database, its sidecars,
// backups, the usage spool). The config folder is hidden from every sandboxed agent (bwrap tmpfs, the MXC box's deny
// list, Claude Code's denyRead); an agent that runs without a sandbox (Linux without bwrap, Windows without the box,
// Codex and Copilot on Windows) reads it. Such a gateway opens the core only when the owner has accepted that
// (acceptAgentsCanReadNotes), and each run is checked again at launch, in the places where the config folder's deny is
// applied (runAgent, the Claude launch).

import (
	"minigw/core"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// agentReadsCoreOn: can this agent kind, if it runs, read the core folder? goos, sandboxed (bwrap on Linux, the MXC
// box on Windows: agentSandboxed), allowUnboxed (allowUnsandboxedBash). Pure, so every platform's row is tested everywhere.
func agentReadsCoreOn(goos, kind string, sandboxed, allowUnboxed bool) bool {
	switch {
	case kind == "claude": // Claude Code's own sandbox (denyRead of the config folder); Windows without the box runs Bash only with the switch
		return goos == "windows" && !sandboxed && allowUnboxed
	case goos == "windows" && outsideBox(kind): // Codex, Copilot run beside the box and only with the switch (agentsAllowed)
		return allowUnboxed
	default: // Codex, Grok, Copilot: bwrap hides the folder; without it they run only with the switch (agentsAllowed)
		return !sandboxed && allowUnboxed
	}
}

// agentSandboxedNow: agentSandboxed; a variable for tests (the Linux row needs bwrap, the Windows row the box).
var agentSandboxedNow = agentSandboxed

func (s *server) agentReadsCore(c serveConfig, kind string) bool {
	if kind == "copilot" && copilotUnsupported {
		return false
	}
	return agentReadsCoreOn(runtime.GOOS, kind, agentSandboxedNow(s.cfgDir), c.AllowUnsandboxedBash)
}

// coreExposed: the agent kinds that are on and would read the core folder (names for the message).
func (s *server) coreExposed(c serveConfig) []string {
	var out []string
	if !c.ClaudeDisabled && s.agentReadsCore(c, "claude") {
		out = append(out, "Claude CLI")
	}
	for _, k := range []agentKind{codexKind, grokKind, copilotKind} {
		if k.conf(c).Enabled && s.agentReadsCore(c, k.name) {
			out = append(out, agentNames[k.name])
		}
	}
	return out
}

const acceptHint = "turn off \"Allow commands without a sandbox\", or accept that agents can read all notes: minigw gw claude --accept-agents-read-notes true (setting acceptAgentsCanReadNotes), then restart the gateway"

// coreOpenRefusal: why the core must not be opened ("" = it may). Default: refused; the owner's setting accepts.
func (s *server) coreOpenRefusal(c serveConfig) string {
	if c.AcceptAgentsReadNotes {
		return ""
	}
	if ex := s.coreExposed(c); len(ex) > 0 {
		return strings.Join(ex, ", ") + " run on this gateway without a sandbox and could read the core database (the notes of all projects): " + acceptHint
	}
	return ""
}

// coreDataOnDisk: the core folder holds data (an open core, a refused or failed one, an earlier period): what must stay
// out of reach does not depend on an open handle.
func (s *server) coreDataOnDisk() string {
	dir := filepath.Join(s.cfgDir, coreDirName)
	if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
		return dir
	}
	return ""
}

// coreRefusal: the run of this agent kind is refused at launch ("" = it may run): the kind would read the core folder,
// the folder holds data or the core is open, and the owner has not accepted that.
func (s *server) coreRefusal(c serveConfig, kind string) string {
	exposed := s.agentReadsCore(c, kind)
	if !exposed || !c.AcceptAgentsReadNotes {
		// a hard link to a core file somewhere else bypasses every deny by path (also the sandbox of a sandboxed agent):
		// no run while one exists (a cheap stat of the core folder's files)
		if dir := s.coreDataOnDisk(); dir != "" {
			if err := core.CheckLinks(dir); err != nil {
				return agentNames[kind] + " cannot run on this gateway: " + err.Error()
			}
		}
	}
	if c.AcceptAgentsReadNotes || !exposed {
		return ""
	}
	if dir := s.coreDataOnDisk(); dir != "" && s.core == nil {
		return agentNames[kind] + " cannot run on this gateway: it runs without a sandbox and could read the old project data in " + dir + " (the core is not open). Move that folder away, or " + acceptHint
	}
	if s.core == nil {
		return ""
	}
	return agentNames[kind] + " cannot run on this gateway while projects are enabled: it runs without a sandbox and could read the notes of all projects. To run it, " + acceptHint
}

// warnStaleCore: the core is refused, but its folder holds data of an earlier accepted or sandboxed period: the agents
// that made the refusal necessary can read it. Nothing is deleted; the owner is told (log and status).
func (s *server) warnStaleCore() {
	if dir := s.coreDataOnDisk(); dir != "" {
		s.coreStale = dir
		s.log.Printf("WARNING: old project data is still on disk in %s and agents without a sandbox can read it (the core is not opened): those agents are refused until you move that folder away or accept it with acceptAgentsCanReadNotes", dir)
	}
}

// coreStatus: the line of `minigw gw status` about the projects and the agents' access ("" = nothing to say: offline).
func (s *server) coreStatus(c serveConfig) string {
	switch {
	case s.coreErr != nil && s.coreStale != "":
		return "projects: off (" + s.coreErr.Error() + "); old project data in " + s.coreStale + ": agents that can read it are refused (move it away, or set acceptAgentsCanReadNotes)"
	case s.coreErr != nil:
		return "projects: off (" + s.coreErr.Error() + ")"
	case s.core == nil:
		return ""
	case c.AcceptAgentsReadNotes && len(s.coreExposed(c)) > 0:
		return "projects: on; " + strings.Join(s.coreExposed(c), ", ") + " can read all notes (accepted: acceptAgentsCanReadNotes)"
	case c.AcceptAgentsReadNotes:
		return "projects: on (acceptAgentsCanReadNotes is set; the agents are sandboxed, so it has no effect)"
	}
	return "projects: on"
}

// logCoreAccepted: at start, an accepted exposure is logged.
func (s *server) logCoreAccepted(c serveConfig) {
	if ex := s.coreExposed(c); c.AcceptAgentsReadNotes && len(ex) > 0 {
		s.log.Printf("WARNING: %s run without a sandbox and can read the core database (the notes of all projects): accepted by acceptAgentsCanReadNotes", strings.Join(ex, ", "))
	}
}
