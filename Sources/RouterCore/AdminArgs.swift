import Foundation

/// The argument parser of the Mac `minigw gw ...` commands (the function RouterCommand.swift calls), kept here so that a test can drive
/// it: commands and options in any order (`--x v` or `--x=v`), and the option check of `gw usage`.
public enum AdminArgs {
    /// Options that take a value (`--x v`); any other `--x` is a switch.
    public static let valued: Set<String> = ["name", "enable", "disable", "path", "tools", "folders", "allow-local-tools", "ask-others", "work", "folder",
                                             "allow-unsandboxed-bash", "config", "device", "days", "user", "project", "model", "claude-probe", "provider",
                                             "sort", "reason", "team", "by", "phase", "activity", "task", "label-source", "remote-project", "remote-team", "class", "device-name", "mode", "upstream", "version", "read-class", "read-sources", "pull-every", "file", "revision", "current"]

    /// The options `gw usage` accepts (the report and --legacy); every option its help text names must be here.
    public static let usageFlags: Set<String> = ["days", "provider", "model", "sort", "json", "csv", "legacy", "device", "folder", "user", "project",
                                                 "by", "phase", "activity", "task", "label-source"]

    public static func parse(_ argv: [String]) -> (pos: [String], flags: [String: String]) {
        var pos: [String] = [], flags: [String: String] = [:]
        var i = 0
        while i < argv.count {
            let a = argv[i]
            // A device ID (43 base64url characters) may start with "--": not an option.
            if a.hasPrefix("--"), !(a.count == 43 && a.allSatisfy { $0.isASCII && ($0.isLetter || $0.isNumber || $0 == "-" || $0 == "_") }) {
                let kv = a.dropFirst(2).split(separator: "=", maxSplits: 1).map(String.init)
                if kv.count == 2 { flags[kv[0]] = kv[1] } else if valued.contains(kv[0]), i + 1 < argv.count { flags[kv[0]] = argv[i + 1]; i += 1 } else { flags[kv[0]] = "true" }
            } else {
                pos.append(a)
            }
            i += 1
        }
        return (pos, flags)
    }

    /// The options of the `upstream` commands, from the parsed flags: the one place the Mac executable maps them (so a test can parse a real argv).
    public static func applyUpstream(_ flags: [String: String], to opts: inout AdminOpts) {
        opts.remoteProject = flags["remote-project"] ?? ""; opts.remoteTeam = flags["remote-team"] ?? ""; opts.classes = flags["class"] ?? ""; opts.deviceName = flags["device-name"] ?? ""; opts.mode = flags["mode"] ?? ""; opts.auto = flags["auto"] == "true"; opts.manual = flags["manual"] == "true"; opts.theirs = flags["theirs"] == "true"; opts.ours = flags["ours"] == "true"
        opts.upstream = flags["upstream"] ?? ""
        opts.withPrivate = flags["with-private"] == "true"; opts.dryRun = flags["dry-run"] == "true"
        opts.afterRestore = flags["after-restore"] == "true"; opts.yes = flags["yes"] != nil
        opts.read = flags["read"] == "true"; opts.noRead = flags["no-read"] == "true"; opts.acceptReach = flags["accept-reach"] == "true"; opts.readClass = flags["read-class"] ?? ""; opts.readSources = flags["read-sources"] ?? ""; opts.pullEvery = flags["pull-every"] ?? ""
    }

    /// The first option `gw usage` does not know, as the refusal text; nil when all are known.
    public static func usageError(_ pos: [String], _ flags: [String: String]) -> String? {
        guard pos.first == "usage", let bad = flags.keys.sorted().first(where: { !usageFlags.contains($0) }) else { return nil }
        return "minigw gw usage: unknown option --\(bad)\n" + UsageReport.help
    }

}

extension AdminCommands {
    /// The end of a provider's status line (as Go gatewayReadyText): still loading is not a failure; a failure points at the log.
    public static func gatewayReadyText(ready: Bool, state: String, reason: String) -> String {
        if ready || state == "ready" { return ", ready" }
        if state.isEmpty { return ", models not loaded (see the gateway log)" }  // an older gateway that sends no state
        if state == "loading" { return ", models loading (not loaded yet)" }
        return reason.isEmpty ? ", models failed to load (see the gateway log)" : ", models failed to load: " + reason + " (see the gateway log)"
    }
}
