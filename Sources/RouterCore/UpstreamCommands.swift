import Foundation

// minigw gw upstream add|list|remove|link|unlink|status|push: a port of client/upstream_cmd.go (same arguments, output, requests and exit
// codes; client/testdata/upstream-commands.json is asserted here and in client/admin_parity_test.go). Thin calls of the owner's routes
// /upstreams and /upstream-push, which the app forwards to the helper; the helper asks the app for the network and the Keychain.

extension AdminCommands {
    public static let upstreamHelp = """
        minigw gw upstream add <name> <address> [--device-name <text>]   another gateway to send knowledge to (host[:port]); its Copy Token on stdin
          minigw gw upstream list | remove <name>
          minigw gw upstream link <project> <name> [--remote-project <slug>] [--class structure,data] [--with-private] [--auto|--manual] [--mode submit|publish]
          minigw gw upstream link --with-private   also the private notes of ALL members of the project (needs private in --class)
          minigw gw upstream link --team <team> <name> [--remote-team <name>] [--auto|--manual] [--mode ..]   a team's own notes
          minigw gw upstream link <project> <name> --read [--read-class structure[,data]] [--read-sources <label,...>|none] --accept-reach   read the upstream's notes into a local mirror (look first: pull --dry-run; no --read-sources keeps the ones it reads now, none clears them)
          minigw gw upstream link <project> <name> --pull-every <minutes>   how often a link with reading on is pulled by itself (5 to 1440, default 15; --team <team> works the same)
          minigw gw upstream link <project> <name> --no-read   stop reading and delete the mirror (--team <team> works the same)
          minigw gw upstream unlink <project> <name> | --team <team> <name>
          minigw gw upstream resolve <project> <note> --theirs|--ours [--upstream <name>]   a note the push did not send: changed on the upstream (publish link), deleted or no longer visible there, or its earlier submission cannot be read (--theirs: our text stays unsent, the upstream's text is not fetched; --ours: send ours as a submission, a NEW note when the old one is gone from view)
          minigw gw upstream resolve --team <team> <note> --theirs|--ours [--upstream <name>]
          minigw gw upstream status [<project>|--team <team>]  per link: what would go, when it last went, the last refusal
          minigw gw upstream push [<project>|--team <team>] [--dry-run]   send the published notes as submissions (nothing without a link)
          minigw gw upstream pull [<project>|--team <team>] [--dry-run]   read the linked project's notes into the mirror (links with --read), or with --dry-run only ask what could be read (nothing is stored)
        """

    /// Go's readKey on the text it read: the secret must be printable ASCII without spaces (it goes into an HTTP header).
    public static func secretLine(_ raw: String, what: String) throws -> String {
        let key = raw.trimmingCharacters(in: .whitespacesAndNewlines)
        if key.isEmpty { throw AdminCLIError("no \(what) (pipe it on stdin or type it at the prompt)") }
        if key.unicodeScalars.contains(where: { $0.value < 0x21 || $0.value > 0x7e }) { throw AdminCLIError("the \(what) must be printable ASCII without spaces") }
        return key
    }

    /// Runs of one pull command for one link (25 runs of 200 notes are the 5000 notes a link's mirror may hold); the same number in Go.
    static let maxPullRuns = 25

    func upstream(_ pos: [String], _ o: AdminOpts) throws {
        let usage = fail(Self.upstreamHelp.trimmingCharacters(in: .whitespacesAndNewlines))
        guard pos.count >= 2 else { throw usage }
        let team = o.team
        func esc(_ s: String) -> String { ProjectText.pathEscape(s) }
        switch (pos[1], pos.count) {
        case ("add", 4) where team.isEmpty:
            let tok = try Self.secretLine(try o.tokenInput?() ?? "", what: "Copy Token of the upstream")
            var req: [String: Any] = ["address": pos[3], "token": tok]
            if !o.deviceName.isEmpty { req["deviceName"] = o.deviceName }
            let r = try request("PUT", "/upstreams/" + esc(pos[2]), req)
            say(clean("upstream \(r["name"]?.string ?? ""): \(r["address"]?.string ?? "")"))
            say(clean("device \(r["device"]?.string ?? ""): the upstream's owner must allow this device and bind it to a user there, and give that user a role in the project"))
            say(clean("credentials stored in " + (r["where"]?.string ?? "")))
            say(clean(r["probe"]?.string ?? ""))
        case ("list", 2) where team.isEmpty:
            let ups = try request("GET", "/upstreams")["upstreams"]?.array ?? []
            for x in ups {
                let d = x.dict
                say(clean("\(d["name"]?.string ?? "")  \(d["address"]?.string ?? "")  device \(d["device"]?.string ?? "")"))
            }
            if ups.isEmpty { say("no upstreams (add one with: minigw gw upstream add <name> <address>)") }
        case ("remove", 3) where team.isEmpty:
            try request("DELETE", "/upstreams/" + esc(pos[2]))
            say("removed upstream " + clean(pos[2]) + ", its links and its credentials (what was sent stays there)")
        case ("link", 4) where team.isEmpty, ("link", 3) where !team.isEmpty, ("unlink", 4) where team.isEmpty, ("unlink", 3) where !team.isEmpty:
            let name = pos[pos.count - 1]
            let isTeam = !team.isEmpty
            var remote = o.remoteProject, local = "", id: Int64 = 0
            if isTeam {
                id = try teamID(team); local = team; remote = o.remoteTeam
                if !o.remoteProject.isEmpty { throw fail("--remote-project is for a project; a team link takes --remote-team") }
            } else {
                id = try projectID(pos[2]); local = pos[2]
                if !o.remoteTeam.isEmpty { throw fail("--remote-team needs --team") }
            }
            let path = "/upstreams/\(esc(name))/links/\(id)"
            if pos[1] == "unlink" {
                try request("DELETE", path, ["team": isTeam])
                say(clean("unlinked \(local) from \(name)"))
                return
            }
            if o.auto && o.manual { throw fail("--auto and --manual together: choose one") }
            if o.read && o.noRead { throw fail("--read and --no-read together: choose one") }
            var req: [String: Any] = ["team": isTeam, "classes": o.classes, "withPrivate": o.withPrivate]
            if !remote.isEmpty { req["remote"] = remote }  // not told: the gateway keeps the destination of an existing link, or uses the same name there for a new one
            if o.auto { req["auto"] = true } else if o.manual { req["auto"] = false }
            if !o.mode.isEmpty { req["mode"] = o.mode }
            let reading = o.read || o.noRead || !o.readClass.isEmpty || !o.readSources.isEmpty
            let pulling = !o.pullEvery.isEmpty
            if pulling {
                guard let n = Int(o.pullEvery), n >= 5, n <= 1440 else { throw fail("--pull-every: minutes, 5 to 1440") }
                req["pullEvery"] = n
            }
            let pushFlags = !o.classes.isEmpty || o.withPrivate || o.auto || o.manual || !o.mode.isEmpty || !o.remoteProject.isEmpty || !o.remoteTeam.isEmpty
            if reading {
                req["read"] = !o.noRead
                req["readClasses"] = o.readClass; req["acceptReach"] = o.acceptReach
                if !o.readSources.isEmpty { req["readSources"] = o.readSources == "none" ? "" : o.readSources }  // omitted keeps; "none" = no extra sources
            }
            if reading || pulling { req["readOnly"] = !pushFlags }  // an existing link keeps its push settings
            let linked = try request("PUT", path, req)
            if let r = linked["remote"]?.string, !r.isEmpty { remote = r } else if remote.isEmpty { remote = local }
            defer { if linked["readOff"]?.bool == true { say(clean("reading was turned off and the mirror deleted: the link now reaches another project or team than the one you accepted; repeat with --read --accept-reach to accept the new reach")) } }
            if pulling && !reading && !pushFlags {
                say(clean("\(local) -> \(name): a link with reading on is pulled every \(o.pullEvery) minutes"))
                return
            }
            if reading && !pushFlags {
                if o.noRead { say(clean("\(local) -> \(name): reading is off, the mirror was deleted")) }
                else { say(clean("\(local) -> \(name): reading is on; minigw gw upstream pull fills the mirror, which the members then see in kb search, kb faq and note read (marked from hub)")) }
                return
            }
            if o.auto { say(clean("linked \(local) to \(name) as \(remote): changes are sent automatically (a minute after the last one; minigw gw upstream status shows it)")) }
            else { say(clean("linked \(local) to \(name) as \(remote): nothing is sent until minigw gw upstream push")) }
            if o.withPrivate {
                say(clean("also the private notes of ALL members of the " + (isTeam ? "team" : "project") + " are sent"))
            }
            if reading { say(o.noRead ? "reading is off, the mirror was deleted" : "reading is on; minigw gw upstream pull fills the mirror, which the members then see in kb search, kb faq and note read (marked from hub)") }
        case ("status", 2...3):
            var q = ""
            if !team.isEmpty && pos.count == 2 { q = "?team=\(try teamID(team))" }
            else if team.isEmpty && pos.count == 3 { q = "?project=\(try projectID(pos[2]))" }
            else if pos.count != 2 { throw usage }
            let links = try request("GET", "/upstreams" + q)["links"]?.array ?? []
            for l in links {
                let d = l.dict
                say(clean(Self.upstreamLinkLine(d, zone)))
                if let s = Self.upstreamSyncLine(d, zone) { say(clean("  " + s)) }
                say(clean("  " + Self.upstreamReadLine(d, zone)))
                if d["read"]?.bool == true, let e = d["pullError"]?.string, !e.isEmpty { say(clean("  last pull error: " + e)) }
                if let e = d["lastError"]?.string, !e.isEmpty { say(clean("  last refusal: " + e)) }
                if let s = Self.upstreamNotPropagated(d) { say(clean("  " + s)) }
                if let s = Self.upstreamConflicts(d) { say(clean("  " + s)) }
                if let w = d["waiting"]?.int, w > 0 { say("  \(w) notes waited for a review upstream at the last push; they are checked at the next push (not a failure)") }
                if let s = Self.upstreamForeign(d) { say(clean("  " + s)) }
                if let s = Self.upstreamSkipped(d) { say(clean("  " + s)) }
                if let s = Self.upstreamBackwards(d) { say(clean("  " + s)) }
            }
            if links.isEmpty { say("no upstream links (minigw gw upstream link <project> <name>)") }
        case ("resolve", 4) where team.isEmpty, ("resolve", 3) where !team.isEmpty:
            if o.theirs == o.ours { throw fail("resolve: choose --theirs or --ours") }
            guard let note = Int64(pos[pos.count - 1]), note > 0 else { throw fail("resolve: the note is its number") }
            var body: [String: Any] = ["note": note, "ours": o.ours, team.isEmpty ? "project" : "team": team.isEmpty ? try projectID(pos[2]) : try teamID(team)]
            if !o.upstream.isEmpty { body["upstream"] = o.upstream }
            try request("POST", "/upstream-resolve", body)
            if o.ours { say("note \(note): our version goes to the upstream as a submission at the next push; the push report says whether it replaces the upstream's note or becomes a new one (a new one when the upstream's note is deleted or no longer visible, or its earlier submission cannot be read: a second note if the old one still exists out of sight)") }
            else { say("note \(note): our text stays unsent (the upstream's text is not fetched here); a later change here is looked at again") }
        case ("push", 2...3):
            var body: [String: Any] = ["dryRun": o.dryRun]
            if !team.isEmpty && pos.count == 2 { body["team"] = try teamID(team) }
            else if team.isEmpty && pos.count == 3 { body["project"] = try projectID(pos[2]) }
            else if pos.count != 2 { throw usage }
            // A helper answers a call within 30 seconds and a run sends a bounded number of notes: a push that is not through comes back with
            // "more" and where it stopped; ask again for that link, as long as it gets further (or only waits for the upstream's write
            // limit, a bounded number of times).
            var targets: [[String: Any]] = [body]
            var lastNext: [String: Int64] = [:], stalls: [String: Int] = [:]
            while !targets.isEmpty {
                var next: [[String: Any]] = []
                var wait: Int64 = 0
                for tgt in targets {
                    let r = try request("POST", "/upstream-push", tgt)
                    for p in r["pushes"]?.array ?? [] {
                        let d = p.dict
                        say(clean("\(d["label"]?.string ?? "") -> \(d["upstream"]?.string ?? "") (\(d["remote"]?.string ?? ""))"))
                        for l in d["lines"]?.array ?? [] { say(clean("  " + (l.string ?? ""))) }
                        let verb = d["dryRun"]?.bool == true ? "would be " : ""
                        say("  \(verb)created \(d["created"]?.int ?? 0), replaced \(d["replaced"]?.int ?? 0), skipped \(d["skipped"]?.int ?? 0), refused \(d["refused"]?.int ?? 0)" + ((d["conflicts"]?.int ?? 0) > 0 ? ", conflicts \(d["conflicts"]?.int ?? 0)" : "") + ((d["waiting"]?.int ?? 0) > 0 ? ", waiting for a review \(d["waiting"]?.int ?? 0)" : ""))
                        if let s = d["stopped"]?.string, !s.isEmpty { say(clean("  stopped: " + s)) }
                    }
                    for p in r["pushes"]?.array ?? [] {
                        let d = p.dict
                        guard d["more"]?.bool == true else { continue }
                        let project = d["project"]?.int ?? 0, upstream = d["upstream"]?.string ?? "", nextID = d["next"]?.int ?? 0, w = d["wait"]?.int ?? 0
                        let key = "\(project)|\(upstream)"
                        if nextID > lastNext[key] ?? 0 { lastNext[key] = nextID; stalls[key] = 0 }
                        else if w > 0 && (stalls[key] ?? 0) < 30 { stalls[key] = (stalls[key] ?? 0) + 1 }
                        else { say(clean("  \(d["label"]?.string ?? "") -> \(upstream): no progress: run the push again later")); continue }
                        var nt: [String: Any] = ["upstream": upstream, "after": nextID, "dryRun": o.dryRun]
                        if d["team"]?.bool == true { nt["team"] = project - ProjectEnv.segmentBase } else { nt["project"] = project }
                        next.append(nt)
                        wait = max(wait, w)
                    }
                }
                targets = next
                if wait > 0 && !targets.isEmpty { pause(Double(wait)) }
            }
        case ("pull", 2...3):
            var body: [String: Any] = ["dryRun": o.dryRun]
            if !team.isEmpty && pos.count == 2 { body["team"] = try teamID(team) }
            else if team.isEmpty && pos.count == 3 { body["project"] = try projectID(pos[2]) }
            else if pos.count != 2 { throw usage }
            if o.dryRun {
                for p in try request("POST", "/upstream-pull", body)["pulls"]?.array ?? [] {
                    let d = p.dict
                    say(clean("\(d["label"]?.string ?? "") -> \(d["upstream"]?.string ?? "") (\(d["remote"]?.string ?? ""))"))
                    for l in d["lines"]?.array ?? [] { say(clean("  " + (l.string ?? ""))) }
                    if let s = d["stopped"]?.string, !s.isEmpty { say(clean("  stopped: " + s)) }
                    else { say("  dry run: \(d["total"]?.int ?? 0) notes could be read, nothing was stored") }
                }
                return
            }
            // A run reads a bounded number of notes (and a helper answers a call within 30 seconds): a pull that is not through comes back with
            // "more" and its cursor; ask again for that link, as long as it gets further (or only waits for the upstream's read limit, a
            // bounded number of times).
            var targets: [[String: Any]] = [body]
            // The loop is bounded: at most maxPullRuns runs (the per-link cap of notes) and pullWall seconds per command, whatever an upstream offers.
            var lastNext: [String: Int64] = [:], stalls: [String: Int] = [:], runs: [String: Int] = [:]
            let deadline = Date().addingTimeInterval(pullWall)
            while !targets.isEmpty {
                var next: [[String: Any]] = []
                var wait: Int64 = 0
                for tgt in targets {
                    for p in try request("POST", "/upstream-pull", tgt)["pulls"]?.array ?? [] {
                        let d = p.dict
                        say(clean("\(d["label"]?.string ?? "") -> \(d["upstream"]?.string ?? "") (\(d["remote"]?.string ?? ""))"))
                        for l in d["lines"]?.array ?? [] { say(clean("  " + (l.string ?? ""))) }
                        say("  added \(d["added"]?.int ?? 0), updated \(d["updated"]?.int ?? 0), removed \(d["removed"]?.int ?? 0), skipped \(d["skipped"]?.int ?? 0), mirror \(d["mirror"]?.int ?? 0) notes")
                        if d["purged"]?.bool == true { say("  the upstream withdrew the right to read: the mirror of this link was deleted") }
                        if let s = d["stopped"]?.string, !s.isEmpty { say(clean("  stopped: " + s)) }
                        guard d["more"]?.bool == true else { continue }
                        let project = d["project"]?.int ?? 0, upstream = d["upstream"]?.string ?? "", nextID = d["next"]?.int ?? 0, w = d["wait"]?.int ?? 0
                        let key = "\(project)|\(upstream)"
                        runs[key, default: 0] += 1
                        if runs[key]! >= Self.maxPullRuns || Date() > deadline { say(clean("  \(d["label"]?.string ?? "") -> \(upstream): more remains: run the pull again")); continue }
                        if nextID > lastNext[key] ?? 0 { lastNext[key] = nextID; stalls[key] = 0 }
                        else if w > 0 && (stalls[key] ?? 0) < 30 { stalls[key] = (stalls[key] ?? 0) + 1 }
                        else { say(clean("  \(d["label"]?.string ?? "") -> \(upstream): no progress: run the pull again later")); continue }
                        var nt: [String: Any] = ["upstream": upstream, "dryRun": false]
                        if d["team"]?.bool == true { nt["team"] = project - ProjectEnv.segmentBase } else { nt["project"] = project }
                        next.append(nt)
                        wait = max(wait, w)
                    }
                }
                targets = next
                if wait > 0 && !targets.isEmpty { pause(Double(wait)) }
            }
        default: throw usage
        }
    }

    static func upstreamLinkLine(_ d: JDict, _ zone: TimeZone) -> String {
        var last = "never pushed"
        if let t = d["lastPush"]?.string.flatMap(ProjectText.parseTime) { last = "last push " + ProjectText.stamp(t, zone) }
        let cls = (d["classes"]?.array ?? []).compactMap { $0.string }.joined(separator: ",")
        var how = d["auto"]?.bool == true ? "auto" : "manual"
        if d["mode"]?.string == "publish" { how += ", publish" }
        return "\(d["label"]?.string ?? "") -> \(d["upstream"]?.string ?? "") (\(d["remote"]?.string ?? "")): \(d["pending"]?.int ?? 0) of \(d["total"]?.int ?? 0) notes would go, \(last), classes \(cls), \(how)"
    }

    /// Whether the link reads, the reach the owner accepted, the mirror and how current it is (as Go's upstreamReadLine).
    static func upstreamReadLine(_ d: JDict, _ zone: TimeZone) -> String {
        if d["read"]?.bool != true { return "read: off" }
        let cls = (d["readClasses"]?.array ?? []).compactMap { $0.string }.joined(separator: ",")
        let srcs = (d["readSources"]?.array ?? []).compactMap { $0.string }
        func at(_ key: String) -> Date? { d[key]?.string.flatMap(ProjectText.parseTime) }
        let acc = at("readAccepted").map { ", accepted " + ProjectText.stamp($0, zone) + " for " + (d["remote"]?.string ?? "") } ?? ""
        var pulled = ", never pulled"
        if let ok = at("lastPullOk") { pulled = ", last pull " + ProjectText.stamp(ok, zone) + " (" + upstreamAge(d["lastPullAge"]?.int ?? 0) + " old)" }
        if let t = at("lastPull"), at("lastPullOk").map({ t > $0 }) ?? true { pulled += ", last try " + ProjectText.stamp(t, zone) }
        if d["stale"]?.bool == true { pulled += ", stale: members do not see the mirror until a pull completes (none in the last 7 days)" }
        return "read: on, classes \(cls), sources \(srcs.isEmpty ? "none" : srcs.joined(separator: ",") )\(acc), mirror \(d["mirror"]?.int ?? 0) notes (\(upstreamSize(Int64(d["mirrorBytes"]?.int ?? 0))))\(pulled)\(upstreamTimerLine(d, zone))"
    }

    /// The automatic pull of a link that is read: how often, when next, the failures in a row, or that a refusal switched it off (as Go's upstreamTimerLine).
    static func upstreamTimerLine(_ d: JDict, _ zone: TimeZone) -> String {
        let every = d["pullEvery"]?.int ?? 0
        if every == 0 { return "" }
        if d["pullTimerOff"]?.bool == true { return ", automatic pull off (the upstream refused it; accept the reach again with --read --accept-reach to switch it on)" }
        var out = ", pulled every \(every / 60) min"
        if let t = d["pullNext"]?.string.flatMap(ProjectText.parseTime) { out += ", next pull " + ProjectText.stamp(t, zone) }
        if (d["pullFailCount"]?.int ?? 0) > 0 { out += " (\(d["pullFailCount"]?.int ?? 0) failures in a row)" }
        return out
    }

    static func upstreamSize(_ b: Int64) -> String {
        if b < 1024 { return "\(b) B" }
        if b < 1 << 20 { return "\(b / 1024).\(b % 1024 * 10 / 1024) KiB" }
        return "\(b >> 20).\(b % (1 << 20) * 10 / (1 << 20)) MiB"
    }

    static func upstreamAge(_ sec: Int64) -> String {
        if sec < 60 { return "under a minute" }
        if sec < 3600 { return "\(sec / 60) min" }
        if sec < 86400 { return "\(sec / 3600) h" }
        return "\(sec / 86400) d"
    }

    /// The state of the automatic push, when there is any to tell (as Go's upstreamSyncLine).
    static func upstreamSyncLine(_ d: JDict, _ zone: TimeZone) -> String? {
        var parts: [String] = []
        func at(_ label: String, _ key: String) {
            if let t = d[key]?.string.flatMap(ProjectText.parseTime) { parts.append(label + " " + ProjectText.stamp(t, zone)) }
        }
        at("dirty since", "dirtySince"); at("last success", "lastOk"); at("last failure", "lastFail")
        if (d["failCount"]?.int ?? 0) > 1, !parts.isEmpty, d["lastFail"]?.string != nil { parts[parts.count - 1] += " (\(d["failCount"]?.int ?? 0) in a row)" }
        at("next attempt", "nextAttempt")
        return parts.isEmpty ? nil : "sync: " + parts.joined(separator: "; ")
    }

    /// Publish notes that changed on the upstream and were not overwritten (as Go's upstreamConflicts).
    static func upstreamConflicts(_ d: JDict) -> String? {
        let parts = (d["conflicts"]?.array ?? []).map { c in
            let what = c.dict["gone"]?.bool == true ? "deleted or no longer visible there"
                : c.dict["submission"]?.bool == true ? "the earlier submission can no longer be read there: check on the hub whether it became a note" : "changed there"
            return "note \(c.dict["note"]?.int ?? 0) (" + what + ")"
        }
        return parts.isEmpty ? nil : "CONFLICT: " + parts.joined(separator: ", ") + ": not overwritten until minigw gw upstream resolve <project> <note> --theirs|--ours"
    }

    /// Published notes that arrived from another gateway: never pushed onward (as Go's upstreamForeign).
    static func upstreamForeign(_ d: JDict) -> String? {
        let n = d["foreign"]?.int ?? 0
        if n == 0 { return nil }
        return (n == 1 ? "1 note" : "\(n) notes") + " not sent: arrived from another gateway, never pushed onward"
    }

    /// Notes the upstream answered about definitively in the last traversal (as Go's upstreamSkipped).
    static func upstreamSkipped(_ d: JDict) -> String? {
        let n = d["skippedNotes"]?.int ?? 0
        if n == 0 { return nil }
        let items = (d["skippedList"]?.array ?? []).compactMap { $0.string }
        var list = items.joined(separator: ", ")
        if n > items.count { list += ", ... (\(n) in all)" }
        return "\(n) notes could not be read (upstream note:reason): \(list); retried at the next pull"
    }

    /// Published notes whose copy on the upstream has a lower revision than our push recorded (as Go's upstreamBackwards).
    static func upstreamBackwards(_ d: JDict) -> String? {
        let ids = (d["revisionBackwards"]?.array ?? []).map { String($0.int ?? 0) }
        return ids.isEmpty ? nil : "revision went backwards on the upstream: note " + ids.joined(separator: ", ") + " (its copy there was replaced by an older one; members see it when its text differs from what we sent)"
    }

    /// Sent notes that were deleted or made private here (as Go's upstreamNotPropagated).
    static func upstreamNotPropagated(_ d: JDict) -> String? {
        var by: [String: [String]] = [:]
        for n in d["notPropagated"]?.array ?? [] { by[n.dict["why"]?.string ?? "", default: []].append(String(n.dict["note"]?.int ?? 0)) }
        let parts = ["deleted here", "now private here"].compactMap { why in by[why].map { why + ": note " + $0.joined(separator: ", ") } }
        return parts.isEmpty ? nil : "not propagated: " + parts.joined(separator: "; ") + " (the copy stays on the upstream until you remove it there)"
    }
}
