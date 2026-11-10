package main

// The pages of client/settings.html, in rows. Every read is a GET of the admin API (docs/admin-api.md), every change
// the same request as the page sends; the gateway checks it.

import (
	"errors"
	"fmt"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

func str(m map[string]any, k string) string { s, _ := m[k].(string); return s }
func yes(m map[string]any, k string) bool   { b, _ := m[k].(bool); return b }
func num(m map[string]any, k string) int    { f, _ := m[k].(float64); return int(f) }
func objs(m map[string]any, k string) []map[string]any {
	var out []map[string]any
	l, _ := m[k].([]any)
	for _, x := range l {
		if o, ok := x.(map[string]any); ok {
			out = append(out, o)
		}
	}
	return out
}

// strs: a list of names; nil when the field is null or missing (= all).
func strs(m map[string]any, k string) []string {
	l, ok := m[k].([]any)
	if !ok {
		return nil
	}
	out := []string{}
	for _, x := range l {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// csv: "a, b,,c" → [a b c].
func csv(s string) []string { return csvSep(s, ",") }

func csvSep(s, sep string) []string {
	out := []string{}
	for _, x := range strings.Split(s, sep) {
		if x = strings.TrimSpace(x); x != "" {
			out = append(out, x)
		}
	}
	return out
}

func sec(label, value string, f func(string) (string, error)) row {
	return row{kind: secret, label: label, value: value, act: f}
}

func onOff(b bool, on, off string) string {
	if b {
		return on
	}
	return off
}

// setModel: the enabled list after switching one model, as the settings page does: all of them = null (also the
// models that come later).
func setModel(enabled, all []string, name string, on bool) []string {
	list := slices.Clone(enabled)
	if enabled == nil {
		list = slices.Clone(all)
	}
	list = slices.DeleteFunc(list, func(x string) bool { return x == name })
	if on {
		list = append(list, name)
	}
	if len(list) == len(all) && !slices.ContainsFunc(all, func(x string) bool { return !slices.Contains(list, x) }) {
		return nil
	}
	return list
}

func general(u *ui) ([]row, error) {
	st, err := u.call("GET", "/status", nil)
	if err != nil {
		return nil, err
	}
	g, err := u.call("GET", "/general", nil)
	if err != nil {
		return nil, err
	}
	patch := func(body map[string]any) (string, error) { return u.save("PATCH", "/general", body) }
	running := fmt.Sprintf("running on port %d", num(st, "port"))
	if !yes(st, "running") {
		running = "not running (changes go to serve.json)"
	}
	rows := []row{hdr("Gateway"),
		inf("Status", running),
		inf("Version", str(st, "version")),
		inf("Devices", fmt.Sprintf("%d allowed, %d waiting", num(st, "devices"), num(st, "pending"))),
		inf("Providers", strconv.Itoa(len(objs(st, "providers")))),
	}
	if d := str(st, "installDir"); d != "" {
		rows = append(rows, inf("Installed in", d))
	}
	if sb, ok := st["agentSandbox"].(map[string]any); ok { // #68: Windows
		v := "not available (" + clean(str(sb, "reason")) + ")"
		if str(sb, "kind") == "mxc" {
			v = "Windows sandbox (MXC " + str(sb, "version") + ")"
		}
		rows = append(rows, inf("Agent sandbox", v))
		if w := str(sb, "warning"); w != "" {
			rows = append(rows, inf("  "+clean(w), ""))
		}
	}
	switch os := str(st, "os"); os {
	case "linux", "darwin": // darwin: minigw serve for development
		rows = append(rows, inf("Startup", map[string]string{"boot": "starts at boot", "sign-in": "starts at sign-in",
			"": "does not start on its own (minigw serve install)"}[str(st, "autostart")]))
	default: // Windows: the Run value; the Mac ("" here): Open at Login
		rows = append(rows, tog(onOff(os == "windows", "Start at sign-in", "Open at Login"), yes(st, "installed"),
			func(v bool) (string, error) { return patch(map[string]any{"startAtSignIn": v}) }))
	}
	rows = append(rows, hdr("Network"),
		ed("Address (empty = all)", str(g, "address"), func(v string) (string, error) { return patch(map[string]any{"address": v}) }),
		ed("Port", strconv.Itoa(num(g, "port")), func(v string) (string, error) {
			p, err := strconv.Atoi(v)
			if err != nil {
				return "", errors.New("the port is a number (1-65535)")
			}
			return patch(map[string]any{"port": p})
		}),
		hdr("Token"))
	if u.connect { // only after an explicit key press; hidden again when the page changes
		rows = append(rows, inf("On each client run:", ""), inf("  "+clean(str(g, "connect")), ""),
			btn("Hide the connect line", "", func() (string, error) { u.connect = false; return "", nil }))
	} else {
		rows = append(rows, inf("Token", "••••••••••••••••••••••••"),
			btn("Show the connect line (with the token)", "", func() (string, error) { u.connect = true; return "", nil }))
	}
	rows = append(rows,
		btn("New token", "Make a new token? Every client must connect again with the new line", func() (string, error) {
			if _, err := u.call("POST", "/general/token", nil); err != nil {
				return "", err
			}
			return "New token made; the old one no longer works. Show the connect line for the clients.", nil
		}),
		hdr("Apps on this computer"),
		tog("Allow this user's apps without a token", yes(g, "allowLocalApps"), func(v bool) (string, error) {
			return patch(map[string]any{"allowLocalApps": v})
		}),
		inf("  Containers, VMs and tunnels running as you count as this user's apps.", ""),
		hdr("Scratch folder"),
		ed("Folder", str(g, "scratch"), func(v string) (string, error) { return patch(map[string]any{"scratch": v}) }),
		btn("Use the default folder", "", func() (string, error) { return patch(map[string]any{"scratch": ""}) }),
	)
	if _, ok := g["worktreeNetwork"]; ok && str(st, "os") != "" { // #68: minigw serve (the Mac shows it under Mac only)
		rows = append(rows, hdr("Worktree mode"),
			tog("Network for builds", yes(g, "worktreeNetwork"), func(v bool) (string, error) { return patch(map[string]any{"worktreeNetwork": v}) }),
			inf("  Claude CLI's builds in its clone may download; off: no network for its commands (on Windows also no web fetch), a remote git clone is refused.", ""))
		if sb, _ := st["agentSandbox"].(map[string]any); str(st, "os") == "windows" && str(sb, "kind") != "mxc" {
			rows = append(rows, inf("  No effect on this computer: there is no Windows sandbox, so agents' network is not limited.", ""))
		}
	}
	return rows, nil
}

func devices(u *ui) ([]row, error) {
	d, err := u.call("GET", "/devices", nil)
	if err != nil {
		return nil, err
	}
	g, err := u.call("GET", "/general", nil)
	if err != nil {
		return nil, err
	}
	gen := func(body map[string]any) (string, error) { return u.save("PATCH", "/general", body) }
	allowed, pending := objs(d, "devices"), objs(d, "pending")
	rows := []row{hdr("Access")}
	if len(allowed) == 0 {
		rows = append(rows, tog("Allow any device with the token", yes(g, "allowAnyDevice"), func(v bool) (string, error) {
			return gen(map[string]any{"allowAnyDevice": v})
		}))
	} else {
		rows = append(rows, inf("Allow any device with the token", "off (only the devices below)"))
	}
	rows = append(rows,
		ed("Allowed client addresses (empty = all)", str(g, "allowedClients"), func(v string) (string, error) {
			return gen(map[string]any{"allowedClients": v})
		}),
		tog("Allow unencrypted remote clients", yes(g, "allowPlainRemote"), func(v bool) (string, error) {
			return gen(map[string]any{"allowPlainRemote": v})
		}),
		hdr("Waiting for approval (compare the ID with the client's minigw status)"))
	for _, x := range pending {
		id := str(x, "id")
		rows = append(rows, inf(clean(str(x, "name")), ""), inf("  ID "+clean(id), ""),
			inf("  "+clean(strings.Join(slices.DeleteFunc([]string{str(x, "address"), str(x, "time"), str(x, "client")}, func(s string) bool { return s == "" }), " · ")), ""),
			ed("  Allow with the name", clean(str(x, "name")), func(v string) (string, error) {
				return u.save("POST", "/devices", map[string]any{"id": id, "name": v})
			}),
			btn("Reject", "", func() (string, error) { return u.save("DELETE", "/devices/"+url.PathEscape(id), nil) }))
	}
	if len(pending) == 0 {
		rows = append(rows, inf("No device is waiting.", ""))
	}
	rows = append(rows, hdr("Allowed devices"))
	for _, x := range allowed {
		id, name := str(x, "id"), clean(str(x, "name"))
		seen := "not seen yet"
		if s := str(x, "lastSeen"); s != "" {
			seen = "last seen " + s
		}
		rows = append(rows, inf(name, ""), inf("  ID "+clean(id), ""),
			inf("  "+clean(strings.Join(slices.DeleteFunc([]string{seen, str(x, "client")}, func(s string) bool { return s == "" }), " · ")), ""),
			ed("  Name", name, func(v string) (string, error) {
				return u.save("PATCH", "/devices/"+url.PathEscape(id), map[string]any{"name": v})
			}),
			btn(onOff(u.open[id], "Hide history", "History"), "", func() (string, error) { u.open[id] = !u.open[id]; return "", nil }))
		if u.open[id] {
			h, err := u.call("GET", "/devices/"+url.PathEscape(id)+"/history", nil)
			if err != nil {
				return nil, err
			}
			for _, c := range objs(h, "history") {
				rows = append(rows, inf("    "+clean(str(c, "time")+"  "+str(c, "address")+"  "+str(c, "client")), ""))
			}
			if len(objs(h, "history")) == 0 {
				rows = append(rows, inf("    no connections recorded", ""))
			}
		}
		rows = append(rows, btn("Remove", "Remove "+name+"? It can no longer connect", func() (string, error) {
			return u.save("DELETE", "/devices/"+url.PathEscape(id), nil)
		}))
	}
	if len(allowed) == 0 {
		rows = append(rows, inf("No device is allowed yet.", ""))
	}
	return rows, nil
}

var builtin = []string{"litellm", "openrouter"}

func providers(u *ui) ([]row, error) {
	if u.models != "" {
		return providerModels(u)
	}
	r, err := u.call("GET", "/providers", nil)
	if err != nil {
		return nil, err
	}
	rows := []row{}
	for _, p := range objs(r, "providers") {
		name := str(p, "name")
		path := "/providers/" + url.PathEscape(name)
		patch := func(body map[string]any) (string, error) { return u.save("PATCH", path, body) }
		key := "(not set)"
		if yes(p, "hasKey") {
			key = "(stored)"
		}
		rows = append(rows, hdr(clean(name)))
		if yes(p, "hasKey") {
			rows = append(rows, tog("Enabled", yes(p, "enabled"), func(v bool) (string, error) { return patch(map[string]any{"enabled": v}) }))
		} else {
			rows = append(rows, inf("Enabled", "off (enter the API key first)"))
		}
		if !slices.Contains(builtin, strings.ToLower(name)) {
			rows = append(rows, ed("Name", name, func(v string) (string, error) { return patch(map[string]any{"name": v}) }))
		}
		rows = append(rows,
			ed("Base URL", str(p, "baseURL"), func(v string) (string, error) { return patch(map[string]any{"baseURL": v}) }),
			tog("Allow http (local network only)", yes(p, "insecure"), func(v bool) (string, error) { return patch(map[string]any{"insecure": v}) }),
			// One per line in the API ("alias = upstream ID, context window"); here separated by ";".
			ed("Aliases (advanced; a = id, 128k; …)", strings.Join(strings.Split(strings.TrimSpace(str(p, "aliases")), "\n"), "; "), func(v string) (string, error) {
				return patch(map[string]any{"aliases": strings.Join(csvSep(v, ";"), "\n")})
			}),
			sec("API key", key, func(v string) (string, error) {
				if v == "" {
					return "", nil
				}
				return u.save("POST", "/providers", map[string]any{"name": name, "baseURL": str(p, "baseURL"), "key": v, "insecure": yes(p, "insecure")})
			}),
			btn("Models…", "", func() (string, error) { u.models, u.filter = name, ""; return "", nil }))
		if !slices.Contains(builtin, strings.ToLower(name)) {
			rows = append(rows, btn("Remove", "Remove provider "+name+" and its API key?", func() (string, error) {
				_, err := u.call("DELETE", path, nil)
				return "Removed " + name + ".", err
			}))
		}
	}
	dr := &u.draft
	if dr.url == "" {
		dr.url = "https://"
	}
	rows = append(rows, hdr("Add a provider (the key is stored in the system's key store)"),
		ed("Name", dr.name, func(v string) (string, error) { dr.name = v; return "", nil }),
		ed("Base URL (without /v1)", dr.url, func(v string) (string, error) { dr.url = v; return "", nil }),
		tog("Allow http (local network only)", dr.insecure, func(v bool) (string, error) { dr.insecure = v; return "", nil }),
		sec("API key, then add", "", func(v string) (string, error) {
			r, err := u.call("POST", "/providers", map[string]any{"name": dr.name, "baseURL": dr.url, "key": v, "insecure": dr.insecure})
			if err != nil {
				return "", err
			}
			n := 0
			for _, m := range objs(r, "models") {
				if yes(m, "enabled") {
					n++
				}
			}
			msg := fmt.Sprintf("Added %s: %d models, %d enabled.", dr.name, len(objs(r, "models")), n)
			dr.name, dr.url, dr.insecure = "", "", false
			return msg, nil
		}))
	return rows, nil
}

func providerModels(u *ui) ([]row, error) {
	path := "/providers/" + url.PathEscape(u.models) + "/models"
	r, err := u.call("GET", path, nil)
	if err != nil {
		return nil, err
	}
	all := objs(r, "models")
	enabled := 0
	for _, m := range all {
		if yes(m, "enabled") {
			enabled++
		}
	}
	f := strings.ToLower(u.filter)
	hits := slices.DeleteFunc(slices.Clone(all), func(m map[string]any) bool {
		return f != "" && !strings.Contains(strings.ToLower(str(m, "id")), f) && !strings.Contains(strings.ToLower(str(m, "model")), f)
	})
	sort.SliceStable(hits, func(i, j int) bool { return yes(hits[i], "enabled") && !yes(hits[j], "enabled") }) // enabled first
	rows := []row{
		btn("← Providers", "", func() (string, error) { u.models = ""; return "", nil }),
		hdr("Models of " + clean(u.models) + fmt.Sprintf(" (%d of %d enabled)", enabled, len(all))),
		ed("Search", u.filter, func(v string) (string, error) { u.filter = v; return "", nil }),
		btn("Enable all", "", func() (string, error) { return u.save("POST", path, map[string]any{"all": true}) }),
		btn("Disable all", "", func() (string, error) { return u.save("POST", path, map[string]any{"all": false}) }),
	}
	for _, m := range hits {
		id := str(m, "id")
		v := ""
		if str(m, "model") != id {
			v = "/model " + str(m, "model")
		}
		if w := num(m, "window"); w > 0 {
			v += fmt.Sprintf("  %dK", w/1000)
		}
		rows = append(rows, tog(clean(id), yes(m, "enabled"), func(on bool) (string, error) {
			return u.save("POST", path, map[string]any{onOff(on, "enable", "disable"): []string{id}})
		}))
		rows[len(rows)-1].value = clean(v)
	}
	return rows, nil
}

func agents(u *ui) ([]row, error) {
	st, err := u.call("GET", "/status", nil)
	if err != nil {
		return nil, err
	}
	c, err := u.call("GET", "/agents/claude", nil)
	if err != nil {
		return nil, err
	}
	bare := yes(st, "agentsUnsandboxed") && !yes(c, "allowUnsandboxedBash") // Codex, Grok, Copilot need the switch
	patch := func(path string) func(map[string]any) (string, error) {
		return func(body map[string]any) (string, error) { return u.save("PATCH", path, body) }
	}
	pc := patch("/agents/claude")
	rows := []row{hdr("Claude CLI")}
	if _, ok := c["enabled"]; ok {
		rows = append(rows, tog("Enabled", yes(c, "enabled"), func(v bool) (string, error) { return pc(map[string]any{"enabled": v}) }))
	}
	if off := str(c, "off"); off != "" {
		rows = append(rows, inf("  Off: "+clean(off), ""))
	}
	rows = append(rows, inf("Program", found(c)),
		ed("Path (empty = from PATH)", str(c, "path"), func(v string) (string, error) { return pc(map[string]any{"path": v}) }),
		tog("Allow local tools", yes(c, "allowLocalTools"), func(v bool) (string, error) { return pc(map[string]any{"allowLocalTools": v}) }))
	if yes(c, "allowLocalTools") {
		rows = append(rows,
			ed("Tools left out (comma-separated)", strings.Join(strs(c, "disabledTools"), ", "), func(v string) (string, error) {
				return pc(map[string]any{"disabledTools": csv(v)})
			}),
			tog("May ask other models of this gateway", yes(c, "askOthers"), func(v bool) (string, error) { return pc(map[string]any{"askOthers": v}) }))
	} else {
		modes := []string{"dontAsk", "acceptEdits", "auto", "plan"}
		mode := str(c, "permissionMode")
		next := modes[(slices.Index(modes, mode)+1)%len(modes)]
		rows = append(rows,
			ed("Tools (comma-separated)", str(c, "tools"), func(v string) (string, error) { return pc(map[string]any{"tools": strings.Join(csv(v), ",")}) }),
			btn("Permission mode: "+mode, "", func() (string, error) { return pc(map[string]any{"permissionMode": next}) }),
			inf("May ask other models of this gateway", "needs Allow local tools"))
		rows[len(rows)-2].value = "(enter: " + next + ")"
	}
	rows = append(rows, ed("Setting sources (comma-separated)", str(c, "settingSources"), func(v string) (string, error) {
		return pc(map[string]any{"settingSources": v})
	}))
	rows = append(rows, workRow(c, pc)...)
	rows = append(rows, loginRows("claude", c, pc)...)
	if _, ok := c["allowUnsandboxedBash"]; ok && (str(st, "os") == "windows" || yes(st, "agentsUnsandboxed")) {
		if sb, _ := st["agentSandbox"].(map[string]any); str(sb, "kind") == "mxc" { // #68: read-only, as on the settings page
			rows = append(rows, inf("Allow commands without a sandbox", onOff(yes(c, "allowUnsandboxedBash"), "on", "off")),
				inf("  Not needed: agents run in the Windows sandbox.", ""))
		} else {
			rows = append(rows, tog("Allow commands without a sandbox", yes(c, "allowUnsandboxedBash"), func(v bool) (string, error) {
				return pc(map[string]any{"allowUnsandboxedBash": v})
			}), inf("  Agents can then read this computer's files and the gateway's secrets.", ""))
		}
	}

	for _, k := range []struct{ kind, title string }{{"codex", "Codex CLI"}, {"grok", "Grok CLI"}} {
		a, err := u.call("GET", "/agents/"+k.kind, nil)
		if err != nil {
			continue // this gateway has no such agent (404)
		}
		p := patch("/agents/" + k.kind)
		rows = append(rows, hdr(k.title))
		if sb, ok := a["sandboxed"].(bool); ok && !sb && !yes(c, "allowUnsandboxedBash") || !ok && bare { // #68: per agent
			if k.kind == "codex" && str(st, "os") == "windows" {
				rows = append(rows, inf("  No sandbox here: run codex sandbox setup --elevated once (as administrator), or turn on Allow commands without a sandbox (Claude CLI).", ""))
			} else {
				rows = append(rows, inf("  No sandbox here: turn on Allow commands without a sandbox (Claude CLI) first.", ""))
			}
		}
		rows = append(rows, tog("Enabled", yes(a, "enabled"), func(v bool) (string, error) { return p(map[string]any{"enabled": v}) }),
			inf("Program", found(a)),
			ed("Path (empty = from PATH)", str(a, "path"), func(v string) (string, error) { return p(map[string]any{"path": v}) }),
			tog("Allow writing in the session's folder", yes(a, "write"), func(v bool) (string, error) { return p(map[string]any{"write": v}) }))
		if k.kind == "codex" {
			rows = append(rows, tog("Allow network in commands", yes(a, "network"), func(v bool) (string, error) { return p(map[string]any{"network": v}) }))
		}
		rows = append(rows, workRow(a, p)...)
		rows = append(rows, loginRows(k.kind, a, p)...)
		rows = append(rows, tog("May ask other models of this gateway", yes(a, "askOthers"), func(v bool) (string, error) { return p(map[string]any{"askOthers": v}) }))
		m, _ := u.call("GET", "/agents/"+k.kind+"/models", nil)
		rows = append(rows, modelToggles(m, "slug", strs(a, "models"), p)...)
	}

	if a, err := u.call("GET", "/agents/copilot", nil); err == nil {
		p := patch("/agents/copilot")
		rows = append(rows, hdr("Copilot CLI"))
		if bare {
			rows = append(rows, inf("  No sandbox here: turn on Allow commands without a sandbox (Claude CLI) first.", ""))
		}
		rows = append(rows, tog("Enabled", yes(a, "enabled"), func(v bool) (string, error) { return p(map[string]any{"enabled": v}) }),
			inf("Program", found(a)),
			ed("Path (empty = from PATH)", str(a, "path"), func(v string) (string, error) { return p(map[string]any{"path": v}) }),
			ed("Models (comma-separated)", strings.Join(strs(a, "models"), ", "), func(v string) (string, error) { return p(map[string]any{"models": csv(v)}) }),
			tog("Allow writing in the session's folder", yes(a, "write"), func(v bool) (string, error) { return p(map[string]any{"write": v}) }),
			tog("Allow the web tool", yes(a, "network"), func(v bool) (string, error) { return p(map[string]any{"network": v}) }),
			tog("Allow commands (needs writing, token)", yes(a, "commands"), func(v bool) (string, error) { return p(map[string]any{"commands": v}) }))
		if !yes(a, "commands") { // commands are an explicit opt-in: ask before turning them on
			rows[len(rows)-1].confirm = "Copilot may then run commands in the session's folder. Turn on"
		}
		rows = append(rows, workRow(a, p)...)
		rows = append(rows, loginRows("copilot", a, p)...)
		rows = append(rows, tog("May ask other models of this gateway", yes(a, "askOthers"), func(v bool) (string, error) { return p(map[string]any{"askOthers": v}) }))
	}

	if f, err := u.call("GET", "/foundry", nil); err == nil {
		p := patch("/foundry")
		state := "not installed"
		if yes(f, "found") {
			state = onOff(yes(f, "running"), "running", onOff(str(f, "error") != "", clean(str(f, "error")), "stopped"))
		}
		rows = append(rows, hdr("Foundry Local"),
			tog("Enabled", yes(f, "enabled"), func(v bool) (string, error) { return p(map[string]any{"enabled": v}) }),
			inf("Program", state),
			ed("Path (empty = from PATH)", str(f, "path"), func(v string) (string, error) { return p(map[string]any{"path": v}) }),
			ed("Instructions", str(f, "instructions"), func(v string) (string, error) { return p(map[string]any{"instructions": v}) }))
		m, _ := u.call("GET", "/foundry/models", nil)
		rows = append(rows, modelToggles(m, "name", strs(f, "models"), p)...)
	}
	return rows, nil
}

// workModes: the agent's cap on the work mode (#45), most trust first; a session above it runs the agent read-only.
var workModes = []struct{ value, label string }{
	{"direct", "In the folder or a copy (any mode)"},
	{"sync", "Only in a copy or on a branch (sync, worktree)"},
	{"worktree", "Only on a branch (worktree)"},
}

// workRow: "Changes in the client's folder", cycling like the permission mode; none for a gateway without the field.
func workRow(a map[string]any, patch func(map[string]any) (string, error)) []row {
	if _, ok := a["work"]; !ok {
		return nil
	}
	i := max(0, slices.IndexFunc(workModes, func(w struct{ value, label string }) bool { return w.value == str(a, "work") }))
	next := workModes[(i+1)%len(workModes)]
	r := btn("Changes in the client's folder: "+workModes[i].value, "", func() (string, error) { return patch(map[string]any{"work": next.value}) })
	r.value = "(enter: " + next.value + ")"
	return []row{r, inf("  "+workModes[i].label, "")}
}

// modelToggles: an agent's models (Codex, Grok, Foundry Local), with all on and all off.
func modelToggles(m map[string]any, key string, enabled []string, patch func(map[string]any) (string, error)) []row {
	if m == nil || len(objs(m, "models")) == 0 {
		return []row{inf("Models", "none found")}
	}
	var all []string
	for _, x := range objs(m, "models") {
		all = append(all, str(x, key))
	}
	rows := []row{
		btn("Enable all models", "", func() (string, error) { return patch(map[string]any{"models": nil}) }),
		btn("Disable all models", "", func() (string, error) { return patch(map[string]any{"models": []string{}}) }),
	}
	for _, x := range objs(m, "models") {
		name := str(x, key)
		rows = append(rows, tog("  "+clean(onOff(str(x, "name") != "", str(x, "name"), name)), yes(x, "enabled"), func(on bool) (string, error) {
			return patch(map[string]any{"models": setModel(enabled, all, name, on)})
		}))
	}
	return rows
}

func found(a map[string]any) string {
	if !yes(a, "found") {
		return "not found"
	}
	return clean(strings.TrimSpace(onOff(str(a, "exe") != "", str(a, "exe"), "found") + " " + str(a, "version")))
}

func folders(u *ui) ([]row, error) {
	c, err := u.call("GET", "/agents/claude", nil)
	if err != nil {
		return nil, err
	}
	list := csv(str(c, "folders"))
	send := func(l []string) (string, error) {
		return u.save("PATCH", "/agents/claude", map[string]any{"folders": strings.Join(l, ",")})
	}
	rows := []row{hdr("Allowed folders"), inf("Claude CLI and Codex work only in these and in the folders clients share.", "")}
	for _, f := range strs(c, "leftOut") {
		rows = append(rows, inf("! "+clean(f), "left out at startup"))
	}
	for _, f := range list {
		rows = append(rows, btn(clean(f), "Remove "+f+" from the allowed folders", func() (string, error) {
			return send(slices.DeleteFunc(slices.Clone(list), func(x string) bool { return x == f }))
		}))
		rows[len(rows)-1].value = "(enter: remove)"
	}
	if len(list) == 0 {
		rows = append(rows, inf("No folder is allowed.", ""))
	}
	rows = append(rows, ed("Add a folder", "", func(v string) (string, error) {
		if v == "" || slices.Contains(list, v) {
			return "", nil
		}
		return send(append(list, v))
	}))
	return rows, nil
}

func sessions(u *ui) ([]row, error) {
	r, err := u.call("GET", "/sessions", nil)
	if err != nil {
		return nil, err
	}
	list := objs(r, "sessions")
	rows := projects(u)
	rows = append(rows, hdr(fmt.Sprintf("Sessions (%d, newest first; never prompt or answer text)", len(list))))
	if len(list) > 0 {
		rows = append(rows, btn("Clear history", "Clear the session history", func() (string, error) {
			_, err := u.call("DELETE", "/sessions", nil)
			return "Session history cleared.", err
		}))
	} else {
		rows = append(rows, inf("No sessions recorded.", ""))
	}
	for _, x := range list {
		id := str(x, "id")
		who := "this computer"
		if _, ok := x["device"]; ok {
			who = onOff(str(x, "deviceName") != "", str(x, "deviceName"), "device")
		}
		if yes(x, "deviceRemoved") {
			who += " (removed)"
		}
		mode := str(x, "mode") + onOff(yes(x, "readOnly"), ", read-only", "")
		for _, m := range objs(x, "models") {
			if c := str(m, "cap"); c != "" && yes(x, "readOnly") {
				mode += " (cap " + c + ")"
				break
			}
		}
		var models []string
		for _, m := range objs(x, "models") {
			if !slices.Contains(models, str(m, "model")) {
				models = append(models, str(m, "model"))
			}
		}
		open := u.session == id
		rows = append(rows, btn(clean(onOff(open, "▾ ", "▸ ")+str(x, "last")+" · "+who+" · "+mode), "", func() (string, error) {
			u.session = onOff(open, "", id)
			return "", nil
		}))
		rows[len(rows)-1].value = clean(strings.Join(models, ", "))
		if !open {
			continue
		}
		rows = append(rows, inf("    Session", clean(id)), inf("    Folder", clean(str(x, "folder"))),
			inf("    Started", clean(str(x, "start"))+fmt.Sprintf(" (%d min)", num(x, "duration")/60)))
		for _, k := range [][2]string{{"os", "OS"}, {"role", "Role"}, {"branch", "Branch"}} {
			if v := str(x, k[0]); v != "" {
				rows = append(rows, inf("    "+k[1], clean(v)))
			}
		}
		for _, m := range objs(x, "models") {
			extra := []string{}
			for _, k := range []string{"role", "via", "readOnly", "tools"} {
				switch v := str(m, k); {
				case k == "readOnly" && yes(m, k):
					extra = append(extra, "read-only"+onOff(str(m, "cap") != "", " (cap "+str(m, "cap")+")", ""))
				case v != "":
					extra = append(extra, k+" "+v)
				}
			}
			rows = append(rows, inf("    "+clean(str(m, "model")), clean(strings.Join(extra, " · "))),
				inf(fmt.Sprintf("      %d requests, %d errors, %d refused, %ds, %d in / %d out tokens", num(m, "requests"), num(m, "errors"),
					num(m, "refused"), num(m, "seconds"), num(m, "inputTokens"), num(m, "outputTokens")), ""))
		}
	}
	return rows, nil
}

// projects: the counters per folder and device of the last 7 days (GET /usage, all agents together); none for an
// older gateway. They outlive the sessions; Clear history clears them too.
func projects(u *ui) []row {
	r, err := u.call("GET", "/usage?days=7", nil)
	if err != nil {
		return nil
	}
	names := map[string]string{"": "this computer"}
	if d, err := u.call("GET", "/devices", nil); err == nil {
		for _, x := range objs(d, "devices") {
			names[str(x, "id")] = onOff(str(x, "name") != "", str(x, "name"), "device")
		}
	}
	type sum struct {
		folder, device         string
		requests, seconds, tok int
	}
	var all []*sum
	for _, x := range objs(r, "usage") {
		i := slices.IndexFunc(all, func(s *sum) bool { return s.folder == str(x, "folder") && s.device == str(x, "device") })
		if i < 0 {
			all = append(all, &sum{folder: str(x, "folder"), device: str(x, "device")})
			i = len(all) - 1
		}
		all[i].requests += num(x, "requests")
		all[i].seconds += num(x, "seconds")
		all[i].tok += num(x, "inputTokens") + num(x, "outputTokens")
	}
	rows := []row{hdr("Projects (7 days)")}
	if len(all) == 0 {
		rows = append(rows, inf("No usage recorded.", ""))
	}
	for _, p := range all {
		who, ok := names[p.device]
		if !ok {
			who = p.device + " (removed)" // as Sessions
		}
		rows = append(rows, inf(clean(p.folder), clean(who)),
			inf(fmt.Sprintf("    %d requests, %d agent minutes, %d tokens", p.requests, p.seconds/60, p.tok), ""))
	}
	return rows
}

// errOldGateway: the gateway has no GET /activity (404); the page then stops reloading itself.
var errOldGateway = errors.New("Needs gateway 0.5.21 or newer (Mini Gateway app or minigw serve)")

// activity: requests running now at the top, then the recent ones (GET /activity); metadata and the agent's current
// step (tool or command), never prompt or answer text.
func activity(u *ui) ([]row, error) {
	r, err := u.call("GET", "/activity", nil)
	if err != nil && strings.HasPrefix(strings.ToLower(err.Error()), "not found: ") {
		return nil, errOldGateway
	}
	if err != nil {
		return nil, err
	}
	when := func(x map[string]any) string {
		if t, err := time.Parse(time.RFC3339, str(x, "time")); err == nil {
			return t.Local().Format("Jan 02 15:04:05")
		}
		return clean(str(x, "time"))
	}
	toggle := func(key string) func() (string, error) {
		open := u.activity == key
		return func() (string, error) { u.activity = onOff(open, "", key); return "", nil }
	}
	var rows []row
	if runs := objs(r, "inflight"); len(runs) > 0 {
		rows = append(rows, hdr(fmt.Sprintf("Running now (%d)", len(runs))))
		for _, x := range runs {
			key := "run " + str(x, "id")
			took := "?"
			if t, err := time.Parse(time.RFC3339, str(x, "time")); err == nil {
				took = time.Since(t).Round(time.Second).String()
			}
			rows = append(rows, btn(clean(fmt.Sprintf("%s● %-8s %s", onOff(u.activity == key, "▾ ", "▸ "), took, str(x, "model"))), "", toggle(key)))
			rows[len(rows)-1].value = clean(onOff(str(x, "step") != "", str(x, "step"), "working…"))
			if u.activity == key {
				rows = append(rows, inf("    Started", when(x)), inf("    Request", clean(str(x, "id"))))
			}
		}
	}
	reqs := objs(r, "requests")
	rows = append(rows, hdr(fmt.Sprintf("Recent requests (%d, newest first; no prompt or answer text)", len(reqs))))
	if len(reqs) == 0 {
		rows = append(rows, inf("No requests yet.", ""))
	}
	for _, x := range reqs {
		key := fmt.Sprint(str(x, "time"), str(x, "model"), num(x, "ms"), str(x, "device"))
		who := onOff(str(x, "deviceName") != "", str(x, "deviceName"), onOff(str(x, "device") != "", "device", "this computer"))
		took := fmt.Sprintf("%d ms", num(x, "ms"))
		if num(x, "ms") >= 1000 {
			took = fmt.Sprintf("%.1f s", float64(num(x, "ms"))/1000)
		}
		rows = append(rows, btn(clean(fmt.Sprintf("%s%s  %3d  %-8s  %s", onOff(u.activity == key, "▾ ", "▸ "), when(x), num(x, "status"), took,
			str(x, "model"))), "", toggle(key)))
		rows[len(rows)-1].value = clean(who)
		if u.activity != key {
			continue
		}
		rows = append(rows, inf("    Request", clean(str(x, "method")+" "+str(x, "path"))), inf("    Size", fmt.Sprintf("%d bytes", num(x, "bytes"))))
		if d := str(x, "device"); d != "" {
			rows = append(rows, inf("    Device ID", clean(d)))
		}
	}
	return rows, nil
}

// macOnly: what only the Mac app has (Siri AI, Apple Foundation Models, minigw run).
func macOnly(u *ui) ([]row, error) {
	siri, err := u.call("GET", "/siri", nil)
	if err != nil {
		return nil, err
	}
	run, err := u.call("GET", "/run", nil)
	if err != nil {
		return nil, err
	}
	ps := func(body map[string]any) (string, error) { return u.save("PATCH", "/siri", body) }
	pr := func(body map[string]any) (string, error) { return u.save("PATCH", "/run", body) }
	allow := func(on bool, yes, no string) string { return onOff(on, yes, no) }
	rows := []row{hdr("Siri AI (uses this Mac's personal data)"),
		tog("Enabled", yes(siri, "enabled"), func(v bool) (string, error) { return ps(map[string]any{"enabled": v}) }),
		ed("Timeout (seconds, 5-600)", strconv.Itoa(num(siri, "timeout")), func(v string) (string, error) {
			t, err := strconv.Atoi(v)
			if err != nil {
				return "", errors.New("the timeout is a number of seconds (5-600)")
			}
			return ps(map[string]any{"timeout": t})
		})}
	for _, d := range objs(siri, "devices") {
		id := str(d, "id")
		rows = append(rows, tog("  "+clean(str(d, "name")), yes(d, "siri"), func(v bool) (string, error) {
			return ps(map[string]any{allow(v, "allow", "deny"): []string{id}})
		}))
	}
	if len(objs(siri, "devices")) == 0 {
		rows = append(rows, inf("  No device is allowed yet.", ""))
	}
	rows = append(rows, hdr("Apple Foundation Models"))
	if fm, err := u.call("GET", "/appleFM", nil); err == nil {
		pf := func(body map[string]any) (string, error) { return u.save("PATCH", "/appleFM", body) }
		rows = append(rows,
			tog("Enabled", yes(fm, "enabled"), func(v bool) (string, error) { return pf(map[string]any{"enabled": v}) }),
			ed("Instructions", str(fm, "instructions"), func(v string) (string, error) { return pf(map[string]any{"instructions": v}) }))
	} else { // an app older than 0.5.20
		rows = append(rows, inf("Set in the app's Settings window (menu bar icon → Settings…).", ""))
	}
	rows = append(rows, hdr("minigw run (runs commands on this Mac: code execution)"),
		ed("Commands (comma-separated)", str(run, "commands"), func(v string) (string, error) { return pr(map[string]any{"commands": v}) }))
	for _, d := range objs(run, "devices") {
		id, name := str(d, "id"), clean(str(d, "name"))
		rows = append(rows, tog("  "+name+": run", yes(d, "run"), func(v bool) (string, error) {
			return pr(map[string]any{allow(v, "allow", "deny"): []string{id}})
		}))
		if !yes(d, "run") {
			rows[len(rows)-1].confirm = name + " may then run the listed commands on this Mac. Allow"
			continue // network only for a device that may run commands (as in the app)
		}
		rows = append(rows, tog("  "+name+": network in commands", yes(d, "network"), func(v bool) (string, error) {
			return pr(map[string]any{allow(v, "allowNetwork", "denyNetwork"): []string{id}})
		}))
	}
	if g, err := u.call("GET", "/general", nil); err == nil {
		if _, ok := g["worktreeNetwork"]; ok { // an app older than 0.5.20 has no such fields
			pg := func(body map[string]any) (string, error) { return u.save("PATCH", "/general", body) }
			rows = append(rows, hdr("Worktree mode (builds of agents in their clone)"),
				tog("Network for builds", yes(g, "worktreeNetwork"), func(v bool) (string, error) { return pg(map[string]any{"worktreeNetwork": v}) }),
				tog("Network for Codex builds", yes(g, "worktreeCodexNetwork"), func(v bool) (string, error) {
					return pg(map[string]any{"worktreeCodexNetwork": v})
				}),
				inf("  Codex can read all files of this Mac's user: its network is off by default.", ""))
			if !yes(g, "worktreeCodexNetwork") { // vklop le po y/n, kot pri run
				rows[len(rows)-2].confirm = "Codex can read all files of this Mac's user and could then send them out. Turn on network"
			}
		}
	}
	return rows, nil
}

// clean: text from the gateway or the network without control characters (no escape sequences in the terminal).
func clean(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) || r == 0x200e || r == 0x200f || (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069) {
			return -1
		}
		return r
	}, s)
}

// loginRows (#68): the agent's Login row as on the settings page: state (never the value), hint, token (Claude,
// Copilot) and the opt-in API key (Claude, Codex, Grok).
func loginRows(kind string, a map[string]any, p func(map[string]any) (string, error)) []row {
	lg, _ := a["login"].(map[string]any)
	if lg == nil {
		return nil // older gateway
	}
	rows := []row{inf("Login", str(lg, "state")), inf("  "+str(lg, "hint"), "")}
	if kind == "claude" || kind == "copilot" {
		rows = append(rows, sec("Token", map[bool]string{true: "(stored)", false: "(not set)"}[yes(lg, "hasToken")], func(v string) (string, error) {
			if v == "" {
				return "", nil
			}
			return p(map[string]any{"token": v})
		}))
		if yes(lg, "hasToken") {
			rows = append(rows, btn("Remove the token", "Remove the "+kind+" token", func() (string, error) { return p(map[string]any{"token": ""}) }))
		}
	}
	if _, ok := lg["apiKey"]; ok {
		rows = append(rows, sec("Use an API key instead", map[bool]string{true: "(stored)", false: "(not set)"}[yes(lg, "apiKey")], func(v string) (string, error) {
			if v == "" {
				return "", nil
			}
			return p(map[string]any{"apiKey": v})
		}), inf("  "+str(lg, "apiKeyNote"), ""))
		if yes(lg, "apiKey") {
			rows = append(rows, btn("Remove the API key (use the login again)", "Remove the "+kind+" API key", func() (string, error) { return p(map[string]any{"apiKey": ""}) }))
		}
	}
	return rows
}
