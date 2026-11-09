// minigw plugin install|uninstall|status in minigw uninstall (0.5.20): nameščeni minigw (brew, aplikacija) je edini
// minigw. Enako kot odjemalec Go (client/plugin.go): isti izpis, ista pravila.
import Foundation
import RouterCore
import Security

let home = ProcessInfo.processInfo.environment["HOME"].flatMap { $0.isEmpty ? nil : $0 } ?? NSHomeDirectory()
let claudeDir = home + "/.claude"
let claudeSettings = claudeDir + "/settings.json"
/// CLAUDE_PLUGIN_DATA (v seji Claude Code), sicer mapa, ki jo Claude Code da vtičniku minigw@mini-gateway.
var pluginDataDir: String {
    ProcessInfo.processInfo.environment["CLAUDE_PLUGIN_DATA"].flatMap { $0.isEmpty ? nil : $0 } ?? claudeDir + "/plugins/data/minigw-mini-gateway"
}
var pluginLinkPath: String { pluginDataDir + "/bin/minigw" }
let offNote = "; turned off there (\"\(PluginSetup.id)\": false), left as is"

func sameFile(_ a: String, _ b: String) -> Bool {
    var x = stat(), y = stat()
    return stat(a, &x) == 0 && stat(b, &y) == 0 && x.st_dev == y.st_dev && x.st_ino == y.st_ino
}

/// Ta minigw, kot ga je poklical uporabnik ali brew (/opt/homebrew/bin/minigw, ne različica v Caskroom), da povezava
/// velja tudi po nadgradnji.
func selfPath() -> String {
    let exe = URL(fileURLWithPath: Bundle.main.executablePath ?? CommandLine.arguments[0]).resolvingSymlinksInPath().path
    let a0 = CommandLine.arguments[0]
    let dirs = a0.contains("/") ? [FileManager.default.currentDirectoryPath] : pathDirs
    for d in dirs {
        let p = a0.hasPrefix("/") ? a0 : d + "/" + a0  // brez standardizacije: /private/tmp ostane, kot je
        if a0.contains("/") || FileManager.default.isExecutableFile(atPath: p) { return sameFile(p, exe) ? p : exe }
    }
    return exe
}

/// L4: le absolutne poti iz PATH (».« ali relativna mapa bi našla program v trenutni mapi).
var pathDirs: [String] { (ProcessInfo.processInfo.environment["PATH"] ?? "").split(separator: ":").map(String.init).filter { $0.hasPrefix("/") } }

func findClaude() -> String? {
    let dirs = pathDirs + [home + "/.local/bin", "/opt/homebrew/bin", "/usr/local/bin"]
    return dirs.map { $0 + "/claude" }.first { FileManager.default.isExecutableFile(atPath: $0) }
}

/// Zažene ukaz brez izpisa; true = izhod 0.
func runQuiet(_ exe: String, _ argv: [String]) -> Bool {
    let p = Process()
    p.executableURL = URL(fileURLWithPath: exe)
    p.arguments = argv
    p.standardOutput = FileHandle.nullDevice
    p.standardError = FileHandle.nullDevice
    guard (try? p.run()) != nil else { return false }
    p.waitUntilExit()
    return p.terminationStatus == 0
}

/// Prva beseda izhoda »<exe> --version« brez »minigw «, le številke in pike; sicer "".
func versionOf(_ exe: String) -> String {
    let p = Process(), pipe = Pipe()
    p.executableURL = URL(fileURLWithPath: exe)
    p.arguments = ["--version"]
    p.standardOutput = pipe
    p.standardError = FileHandle.nullDevice
    guard (try? p.run()) != nil else { return "" }
    let line = String(decoding: pipe.fileHandleForReading.readDataToEndOfFile(), as: UTF8.self).split(separator: "\n").first ?? ""
    p.waitUntilExit()
    let v = line.split(separator: " ").map(String.init).first { $0 != "minigw" } ?? ""
    return !v.isEmpty && v.allSatisfy { "0123456789.".contains($0) } ? v : ""
}

/// L3: mapa ali datoteka ni zapisljiva za skupino in druge ter je last tega uporabnika ali root.
/// L1: le <claude>/plugins/data/minigw…, absolutna in počiščena pot.
func pluginDataSafe(_ d: String) -> Bool {
    d.hasPrefix("/") && !d.hasSuffix("/") && !d.contains("//") && !d.contains("/./") && !d.contains("/../")
        && (d as NSString).deletingLastPathComponent == claudeDir + "/plugins/data" && (d as NSString).lastPathComponent.hasPrefix("minigw")
}

/// L5: kot root bi minigw pisal v korenove ~/.claude in povezave.
func refuseRoot(_ cmd: String) { if geteuid() == 0 { fail("run minigw \(cmd) as yourself, not as root") } }

func warn(_ s: String) { err.write(Data(("minigw: " + s + "\n").utf8)) }

/// bin/minigw vtičnika → self; nil = self je že minigw vtičnika (prenesen, brez namestitve).
func pluginLink(_ self: String) throws -> String? {
    let bin = pluginDataDir + "/bin"
    if sameFile((self as NSString).deletingLastPathComponent, bin) { return nil }
    let dir = (self as NSString).deletingLastPathComponent
    guard PluginSetup.safePath(dir), PluginSetup.safePath(self) else { throw PluginSetup.Failure(message: "\(self) or its folder is writable by others or not owned by you or root: move minigw to a safe folder") }
    try FileManager.default.createDirectory(atPath: bin, withIntermediateDirectories: true)
    if (try? FileManager.default.destinationOfSymbolicLink(atPath: pluginLinkPath)) != self {
        unlink(pluginLinkPath)
        try FileManager.default.createSymbolicLink(atPath: pluginLinkPath, withDestinationPath: self)
    }
    return pluginLinkPath
}

/// Odstrani bin/minigw vtičnika, le če je povezava na self.
func pluginUnlink(_ self: String) -> Bool {
    guard let t = try? FileManager.default.destinationOfSymbolicLink(atPath: pluginLinkPath), t == self || sameFile(pluginLinkPath, self) else { return false }
    return unlink(pluginLinkPath) == 0
}

func pluginInstall() -> Bool {
    var ok = true, changed = false, off = false
    do {
        try PluginSetup.edit(claudeSettings) { s in (changed, off) = try PluginSetup.add(&s); return changed }
        say("Claude Code plugin \(PluginSetup.id): " + (changed ? "added to \(claudeSettings)" + (off ? offNote : "; Claude Code installs it the next time claude starts (or /reload-plugins)")
                                                       : "already in \(claudeSettings)" + (off ? offNote : "")))
    } catch {
        warn((error as? PluginSetup.Failure)?.message ?? error.localizedDescription)
        ok = false
    }
    let me = selfPath()
    do {
        if let link = try pluginLink(me) { say("Plugin's minigw: \(link) → \(me)") } else { say("Plugin's minigw: this one (\(me))") }
    } catch {
        warn("the plugin's minigw is not linked (\((error as? PluginSetup.Failure)?.message ?? error.localizedDescription)); a new Claude Code session sets it up")
        ok = false
    }
    return ok
}

/// upgrading (brew upgrade): marketplace in povezava ostaneta, da vtičnik ne izgine med sejo.
func pluginUninstall(upgrading: Bool) -> Bool {
    var had = false, hadOff = false  // pred »marketplace remove«, ki vnose v settings.json odstrani že sam
    try? PluginSetup.edit(claudeSettings) { s in (had, hadOff) = PluginSetup.remove(&s); return false }
    if !upgrading, let c = findClaude() {
        say("Claude Code: marketplace \(PluginSetup.market) " + (runQuiet(c, ["plugin", "marketplace", "remove", PluginSetup.market, "--scope", "user"]) ? "removed" : "not removed (claude plugin marketplace remove failed)"))
    }
    if !upgrading, pluginUnlink(selfPath()) { say("Plugin's minigw: link removed (\(pluginLinkPath))") }
    guard FileManager.default.fileExists(atPath: claudeSettings) else { return true }
    var changed = false, off = false
    do {
        try PluginSetup.edit(claudeSettings) { s in (changed, off) = PluginSetup.remove(&s); return changed }
    } catch {
        warn((error as? PluginSetup.Failure)?.message ?? error.localizedDescription)
        return false
    }
    say("Claude Code plugin \(PluginSetup.id): " + (changed || had ? "removed from " : "nothing to remove in ") + claudeSettings + (off || hadOff ? offNote : ""))
    return true
}

/// Različica nameščenega vtičnika (installed_plugins.json → installPath/.claude-plugin/plugin.json).
func pluginVersion() -> (String, String)? {
    func json(_ p: String) -> [String: Any]? { FileManager.default.contents(atPath: p).flatMap { try? JSONSerialization.jsonObject(with: $0) as? [String: Any] } }
    guard let e = ((json(claudeDir + "/plugins/installed_plugins.json")?["plugins"] as? [String: Any])?[PluginSetup.id] as? [[String: Any]])?.first,
          let path = e["installPath"] as? String else { return nil }
    let v = json(path + "/.claude-plugin/plugin.json")?["version"] as? String ?? e["version"] as? String ?? ""
    return v.isEmpty ? nil : (clean(v), path)
}

func pluginStatus() {
    let me = selfPath()
    say("minigw:     \(minigwVersion) (\(me))")
    var cv = "not found (the agent skills need the claude command)"
    if let c = findClaude() { let v = versionOf(c); cv = (v.isEmpty ? "?" : v) + " (\(c))" }
    say("Claude Code: " + cv)
    if let (v, p) = pluginVersion() { say("Plugin:     \(v) (\(p))") } else { say("Plugin:     not installed in Claude Code (minigw plugin install, then start claude)") }
    var state = "not added (minigw plugin install)"
    if let data = FileManager.default.contents(atPath: claudeSettings) {
        if let s = try? JSONSerialization.jsonObject(with: data) as? [String: Any] {
            if let v = (s["enabledPlugins"] as? [String: Any])?[PluginSetup.id] { state = PluginSetup.isTrue(v) ? "on" : "off (enabledPlugins false)" }
        } else { state = "settings.json is not valid JSON" }
    }
    say("Settings:   \(PluginSetup.id) \(state) (\(claudeSettings))")
    let bin: String
    if let t = try? FileManager.default.destinationOfSymbolicLink(atPath: pluginLinkPath) {
        bin = "\(pluginLinkPath) → \(t)" + (t == me || sameFile(pluginLinkPath, me) ? " (this minigw)" : " (another minigw: minigw plugin install)")
    } else if FileManager.default.fileExists(atPath: pluginLinkPath) {
        let v = versionOf(pluginLinkPath)
        bin = pluginLinkPath + (sameFile(pluginLinkPath, me) ? " (this minigw)" : " (a copy: minigw \(v.isEmpty ? "?" : v))")
    } else { bin = "not set up (minigw plugin install, or a new Claude Code session)" }
    say("Plugin bin: " + bin)
    guard let p = loadProfiles(listing: true).first else { say("Gateway:    not connected (minigw connect)"); return }
    switch routeResult(p) {
    case .failure(let e): say("Gateway:    \(p.conn.url) (\(oneLine(e.message)))")
    case .success(let c):
        let (st, ready) = get(c, "/health/ready")
        guard st == 200, ready?["providers"] != nil else { say("Gateway:    \(p.conn.url) (not connected: minigw status)"); return }
        var v = ready?["version"] as? String ?? ""
        if v.isEmpty || v.contains(where: { !"0123456789.".contains($0) }) { v = "unknown (older than 0.5.20)" }
        say("Gateway:    \(p.conn.url), version \(v)")
    }
}

func pluginCommand(_ a: [String]) -> Never {
    refuseRoot("plugin")
    switch a {
    case ["install"]: exit(pluginInstall() ? 0 : 1)
    case ["uninstall"], ["uninstall", "--upgrading"]: exit(pluginUninstall(upgrading: a.count == 2) ? 0 : 1)
    case ["status"]: pluginStatus(); exit(0)
    default: fail("usage: minigw plugin install | uninstall [--upgrading] | status")
    }
}

// MARK: minigw uninstall

func uninstallCommand(_ a: [String]) -> Never {
    var purge = false, yes = false
    for x in a {
        switch x {
        case "--purge": purge = true
        case "--yes", "-y": yes = true
        default: fail("usage: minigw uninstall [--purge] [--yes]   (--purge also removes settings, profiles and the device key)")
        }
    }
    refuseRoot("uninstall")
    let fm = FileManager.default
    var steps: [(what: String, run: () -> Bool)] = [], kept: [String] = []
    let data = pluginDataDir
    steps.append(("Claude Code plugin \(PluginSetup.id) (settings entries, marketplace, \(data))", {
        let ok = pluginUninstall(upgrading: false)
        if pluginDataSafe(data) { try? fm.removeItem(atPath: data) }  // L1: CLAUDE_PLUGIN_DATA bi lahko kazal drugam
        return ok && !fm.fileExists(atPath: data)
    }))
    let agent = home + "/Library/LaunchAgents/dev.trojar.minigw-serve.plist"  // starejši minigw serve (Go) na Macu
    if fm.fileExists(atPath: agent) {
        steps.append(("LaunchAgent \(agent)", {
            _ = runQuiet("/bin/launchctl", ["bootout", "gui/\(getuid())", agent])
            return (try? fm.removeItem(atPath: agent)) != nil
        }))
    }
    let scratch = fm.urls(for: .cachesDirectory, in: .userDomainMask)[0].appendingPathComponent("minigw").path
    if fm.fileExists(atPath: scratch) { steps.append(("scratch folder \(scratch)", { (try? fm.removeItem(atPath: scratch)) != nil })) }
    let names = profileNames(), settings = sharedFoldersFile.deletingLastPathComponent().path
    let profiles = "profiles \(names.joined(separator: ", ")) (Keychain)"
    if purge {
        if !names.isEmpty {
            steps.append((profiles, { names.allSatisfy { [errSecSuccess, errSecItemNotFound].contains(SecItemDelete(keychainQuery($0) as CFDictionary)) } }))
        }
        if deviceStored() {
            steps.append(("device key (Keychain)", { [errSecSuccess, errSecItemNotFound].contains(SecItemDelete(deviceQuery() as CFDictionary)) }))
        }
        if fm.fileExists(atPath: settings) { steps.append(("settings folder \(settings)", { (try? fm.removeItem(atPath: settings)) != nil })) }
    } else {
        if !names.isEmpty { kept.append(profiles) }
        if deviceStored() { kept.append("device key (Keychain)") }
        if fm.fileExists(atPath: settings) { kept.append("settings folder \(settings)") }
    }
    say("minigw uninstall removes:")
    steps.forEach { say("  - " + $0.what) }
    kept.forEach { say("Kept (--purge removes it): " + $0) }
    let me = URL(fileURLWithPath: selfPath()).resolvingSymlinksInPath().path
    let how = me.contains("/Caskroom/") ? "brew uninstall --cask minigw"
        : me.contains(".app/Contents/") ? "it goes with Mini Gateway.app (brew uninstall --cask mini-gateway, or move the app to the Trash)" : "rm \(me)"
    say("Not removed: minigw itself (\(selfPath())): " + how)
    if !yes {
        guard isatty(0) == 1 else { fail("stdin is not a terminal: confirm with minigw uninstall --yes") }
        FileHandle.standardOutput.write(Data("Remove them? [y/N] ".utf8))
        guard ["y", "yes"].contains((readLine() ?? "").trimmingCharacters(in: .whitespaces).lowercased()) else { say("Nothing removed"); exit(0) }
    }
    var failed = false
    for s in steps where !s.run() { warn("\(s.what): not removed"); failed = true }
    if failed { exit(1) }
    say("Removed.")
    exit(0)
}
