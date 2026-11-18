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
          minigw gw upstream unlink <project> <name> | --team <team> <name>
          minigw gw upstream resolve <project> <note> --theirs|--ours   a publish link: a note changed on the upstream (--theirs: keep theirs, forget our change; --ours: send ours as a submission)
          minigw gw upstream resolve --team <team> <note> --theirs|--ours
          minigw gw upstream status [<project>|--team <team>]  per link: what would go, when it last went, the last refusal
          minigw gw upstream push [<project>|--team <team>] [--dry-run]   send the published notes as submissions (nothing without a link)
        """

    /// Go's readKey on the text it read: the secret must be printable ASCII without spaces (it goes into an HTTP header).
    public static func secretLine(_ raw: String, what: String) throws -> String {
        let key = raw.trimmingCharacters(in: .whitespacesAndNewlines)
        if key.isEmpty { throw AdminCLIError("no \(what) (pipe it on stdin or type it at the prompt)") }
        if key.unicodeScalars.contains(where: { $0.value < 0x21 || $0.value > 0x7e }) { throw AdminCLIError("the \(what) must be printable ASCII without spaces") }
        return key
    }

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
            if remote.isEmpty { remote = local }
            if o.auto && o.manual { throw fail("--auto and --manual together: choose one") }
            var req: [String: Any] = ["team": isTeam, "remote": remote, "classes": o.classes, "withPrivate": o.withPrivate]
            if o.auto { req["auto"] = true } else if o.manual { req["auto"] = false }
            if !o.mode.isEmpty { req["mode"] = o.mode }
            try request("PUT", path, req)
            if o.auto { say(clean("linked \(local) to \(name) as \(remote): changes are sent automatically (a minute after the last one; minigw gw upstream status shows it)")) }
            else { say(clean("linked \(local) to \(name) as \(remote): nothing is sent until minigw gw upstream push")) }
            if o.withPrivate {
                say(clean("also the private notes of ALL members of the " + (isTeam ? "team" : "project") + " are sent"))
            }
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
                if let e = d["lastError"]?.string, !e.isEmpty { say(clean("  last refusal: " + e)) }
                if let s = Self.upstreamNotPropagated(d) { say(clean("  " + s)) }
                if let s = Self.upstreamConflicts(d) { say(clean("  " + s)) }
            }
            if links.isEmpty { say("no upstream links (minigw gw upstream link <project> <name>)") }
        case ("resolve", 4) where team.isEmpty, ("resolve", 3) where !team.isEmpty:
            if o.theirs == o.ours { throw fail("resolve: choose --theirs or --ours") }
            guard let note = Int64(pos[pos.count - 1]), note > 0 else { throw fail("resolve: the note is its number") }
            let body: [String: Any] = ["note": note, "ours": o.ours, team.isEmpty ? "project" : "team": team.isEmpty ? try projectID(pos[2]) : try teamID(team)]
            try request("POST", "/upstream-resolve", body)
            if o.ours { say("note \(note): our version goes to the upstream as a submission at the next push") }
            else { say("note \(note): the upstream's version stays, our change is forgotten for it") }
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
                        say("  \(verb)created \(d["created"]?.int ?? 0), replaced \(d["replaced"]?.int ?? 0), skipped \(d["skipped"]?.int ?? 0), refused \(d["refused"]?.int ?? 0)" + ((d["conflicts"]?.int ?? 0) > 0 ? ", conflicts \(d["conflicts"]?.int ?? 0)" : ""))
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
        let parts = (d["conflicts"]?.array ?? []).map { c in "note \(c.dict["note"]?.int ?? 0) (" + (c.dict["gone"]?.bool == true ? "deleted there" : "changed there") + ")" }
        return parts.isEmpty ? nil : "CONFLICT: " + parts.joined(separator: ", ") + ": not overwritten until minigw gw upstream resolve <project> <note> --theirs|--ours"
    }

    /// Sent notes that were deleted or made private here (as Go's upstreamNotPropagated).
    static func upstreamNotPropagated(_ d: JDict) -> String? {
        var by: [String: [String]] = [:]
        for n in d["notPropagated"]?.array ?? [] { by[n.dict["why"]?.string ?? "", default: []].append(String(n.dict["note"]?.int ?? 0)) }
        let parts = ["deleted here", "now private here"].compactMap { why in by[why].map { why + ": note " + $0.joined(separator: ", ") } }
        return parts.isEmpty ? nil : "not propagated: " + parts.joined(separator: "; ") + " (the copy stays on the upstream until you remove it there)"
    }
}
