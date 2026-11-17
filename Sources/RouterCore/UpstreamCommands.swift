import Foundation

// minigw gw upstream add|list|remove|link|unlink|status|push: a port of client/upstream_cmd.go (same arguments, output, requests and exit
// codes; client/testdata/upstream-commands.json is asserted here and in client/admin_parity_test.go). Thin calls of the owner's routes
// /upstreams and /upstream-push, which the app forwards to the helper; the helper asks the app for the network and the Keychain.

extension AdminCommands {
    public static let upstreamHelp = """
        minigw gw upstream add <name> <address>              another gateway to send knowledge to (host[:port]); its Copy Token on stdin
          minigw gw upstream list | remove <name>
          minigw gw upstream link <project> <name> [--remote-project <slug>] [--class structure,data] [--with-private]
          minigw gw upstream link --team <team> <name> [--remote-team <name>]   a team's own notes
          minigw gw upstream unlink <project> <name> | --team <team> <name>
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
            let r = try request("PUT", "/upstreams/" + esc(pos[2]), ["address": pos[3], "token": tok])
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
            try request("PUT", path, ["team": isTeam, "remote": remote, "classes": o.classes, "withPrivate": o.withPrivate])
            say(clean("linked \(local) to \(name) as \(remote): nothing is sent until minigw gw upstream push"))
        case ("status", 2...3):
            var q = ""
            if !team.isEmpty && pos.count == 2 { q = "?team=\(try teamID(team))" }
            else if team.isEmpty && pos.count == 3 { q = "?project=\(try projectID(pos[2]))" }
            else if pos.count != 2 { throw usage }
            let links = try request("GET", "/upstreams" + q)["links"]?.array ?? []
            for l in links {
                let d = l.dict
                say(clean(Self.upstreamLinkLine(d, zone)))
                if let e = d["lastError"]?.string, !e.isEmpty { say(clean("  last refusal: " + e)) }
            }
            if links.isEmpty { say("no upstream links (minigw gw upstream link <project> <name>)") }
        case ("push", 2...3):
            var body: [String: Any] = ["dryRun": o.dryRun]
            if !team.isEmpty && pos.count == 2 { body["team"] = try teamID(team) }
            else if team.isEmpty && pos.count == 3 { body["project"] = try projectID(pos[2]) }
            else if pos.count != 2 { throw usage }
            // The helper answers a call within 30 seconds: a long push comes back with "more" (and how long to wait): ask again.
            var calls = 0
            while true {
                calls += 1
                let r = try request("POST", "/upstream-push", body)
                var more = false, wait: Int64 = 0
                for p in r["pushes"]?.array ?? [] {
                    let d = p.dict
                    say(clean("\(d["label"]?.string ?? "") -> \(d["upstream"]?.string ?? "") (\(d["remote"]?.string ?? ""))"))
                    for l in d["lines"]?.array ?? [] { say(clean("  " + (l.string ?? ""))) }
                    let verb = d["dryRun"]?.bool == true ? "would be " : ""
                    say("  \(verb)created \(d["created"]?.int ?? 0), replaced \(d["replaced"]?.int ?? 0), skipped \(d["skipped"]?.int ?? 0), refused \(d["refused"]?.int ?? 0)")
                    if let s = d["stopped"]?.string, !s.isEmpty { say(clean("  stopped: " + s)) }
                    if d["more"]?.bool == true { more = true; wait = max(wait, d["wait"]?.int ?? 0) }
                }
                if !more || calls >= 1000 { break }
                if wait > 0 { pause(Double(wait)) }
            }
        default: throw usage
        }
    }

    static func upstreamLinkLine(_ d: JDict, _ zone: TimeZone) -> String {
        var last = "never pushed"
        if let t = d["lastPush"]?.string.flatMap(ProjectText.parseTime) { last = "last push " + ProjectText.stamp(t, zone) }
        let cls = (d["classes"]?.array ?? []).compactMap { $0.string }.joined(separator: ",")
        return "\(d["label"]?.string ?? "") -> \(d["upstream"]?.string ?? "") (\(d["remote"]?.string ?? "")): \(d["pending"]?.int ?? 0) of \(d["total"]?.int ?? 0) notes would go, \(last), classes \(cls)"
    }
}
