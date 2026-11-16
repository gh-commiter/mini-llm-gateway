import Foundation

extension AdminCommands {
    func teamID(_ ref: String) throws -> Int64 {
        let n = Int64(ref) ?? 0
        for t in try request("GET", "/teams")["teams"]?.array ?? [] {
            let d = t.dict
            if n > 0 && (d["id"]?.int ?? 0) == n || n <= 0 && (d["name"]?.string ?? "").caseInsensitiveCompare(ref) == .orderedSame { return d["id"]?.int ?? 0 }
        }
        throw fail("no team " + GoText.quote(ref) + " (minigw gw team list)")
    }

    func team(_ pos: [String]) throws {
        let usage = fail(Self.help.trimmingCharacters(in: .whitespacesAndNewlines))
        guard pos.count >= 2 else { throw usage }
        switch (pos[1], pos.count) {
        case ("access", _):
            guard pos.count >= 4 else { throw usage }
            let path = "/teams/\(try teamID(pos[3]))/assignments"
            if pos[2] == "list" && pos.count == 4 {
                for a in try request("GET", path)["assignments"]?.array ?? [] {
                    let d = a.dict
                    say("project \(d["project"]?.int ?? 0)  user \(d["user"]?.int ?? 0)  \(d["role"]?.string ?? "")")
                }
            } else if pos[2] == "set" && pos.count == 7 || pos[2] == "remove" && pos.count == 6 {
                var b: [String: Any] = ["project": try projectID(pos[4]), "user": try userID(pos[5])]
                if pos[2] == "set" { b["role"] = pos[6] }
                try request(pos[2] == "set" ? "PUT" : "DELETE", path, b)
            } else { throw usage }
        case ("list", 2):
            for t in try request("GET", "/teams")["teams"]?.array ?? [] { say(ProjectEnv.teamLine(t.dict)) }
        case ("add", 3):
            try request("POST", "/teams", ["name": pos[2]])
            say("team " + clean(pos[2]) + " added")
        case ("rename", 4):
            try request("PATCH", "/teams/\(try teamID(pos[2]))", ["name": pos[3]])
        case ("archive", 3):
            try request("PATCH", "/teams/\(try teamID(pos[2]))", ["archived": true])
        case ("member", 5), ("project", 5):
            guard pos[2] == "add" || pos[2] == "remove" else { throw usage }
            let tid = try teamID(pos[3])
            let id = pos[1] == "member" ? try userID(pos[4]) : try projectID(pos[4])
            try request(pos[2] == "add" ? "PUT" : "DELETE", "/teams/\(tid)/\(pos[1] == "member" ? "members" : "projects")/\(id)")
        default: throw usage
        }
    }
}

extension ProjectEnv {
    public static let teamHelp = """
        minigw team list [--archived] [--json]  your teams (directory only; no project access)
        minigw team show <team> [--json]        team members and projects you can already read
        """

    static func teamLine(_ d: JDict) -> String {
        ProjectText.clean("\(d["id"]?.int ?? 0)  \(d["name"]?.string ?? "")\(d["archived"]?.bool == true ? "  (archived)" : "")")
    }

    static let malformed = ProjectError("the gateway sent an answer minigw cannot read")

    public func team(_ args: [String]) throws {
        if agent { throw ProjectError(Self.agentRefusal) }
        let usage = ProjectError("usage:\n" + Self.teamHelp)
        guard let fl = try? GoFlags.parse([("json", .bool), ("archived", .bool)], args) else { throw usage }
        let pos = fl.positional
        guard pos.count == 1 && pos[0] == "list" || pos.count == 2 && pos[0] == "show" else { throw usage }
        let (c, _) = connect()
        var path = "/minigw/v1/teams"
        if pos[0] == "list" && fl.bool("archived") { path += "?archived=1" }
        if pos[0] == "show" {
            var ref = pos[1]
            if (Int64(ref) ?? 0) <= 0 {
                let r = send(c, "GET", path + "?archived=1", [:], nil)
                guard r.status == 200 else { throw Self.gatewayError(r.status, r.data, r.retryAfter) }
                guard case .arr(let teams)? = JSONValue.parse(r.data) else { throw Self.malformed }
                guard let t = teams.first(where: { ($0.dict["name"]?.string ?? "").caseInsensitiveCompare(ref) == .orderedSame }) else { throw Self.gatewayError(404, Data(), nil) }
                ref = String(t.dict["id"]?.int ?? 0)
            }
            path += "/" + ProjectText.pathEscape(ref)
        }
        let r = send(c, "GET", path, [:], nil)
        guard r.status == 200 else { throw Self.gatewayError(r.status, r.data, r.retryAfter) }
        if fl.bool("json") { say(ProjectText.trim(String(decoding: r.data, as: UTF8.self))); return }
        if pos[0] == "list" {
            guard case .arr(let teams)? = JSONValue.parse(r.data) else { throw Self.malformed }
            for t in teams { say(Self.teamLine(t.dict)) }
            if teams.isEmpty { say("no teams: ask the gateway owner to add you to one") }
        } else {
            guard case .obj? = JSONValue.parse(r.data) else { throw Self.malformed }
            let d = JSONValue.parse(r.data)?.dict ?? JDict()
            say(Self.teamLine(d))
            for m in d["members"]?.array ?? [] {
                say(ProjectText.clean("member \(m.dict["user"]?.int ?? 0)  \(m.dict["name"]?.string ?? "")"))
            }
            for p in d["projects"]?.array ?? [] {
                let v = p.dict
                say(ProjectText.clean("project \(v["id"]?.int ?? 0)  \(v["slug"]?.string ?? "")  \(v["name"]?.string ?? "")\(v["archived"]?.bool == true ? "  (archived)" : "")"))
            }
        }
    }
}

extension ProjectEnv {
    static func originText(_ origins: [JSONValue], roles: Bool) -> String {
        origins.map { o in
            let d = o.dict
            var label = d["kind"]?.string ?? ""
            if label == "team" { label = "team " + (d["name"]?.string ?? "") }
            if roles { label += " " + (d["role"]?.string ?? "") }
            return label
        }.joined(separator: ", ")
    }
}
extension AdminCommands {
    func memberFeedback(_ d: JDict, direct: String) {
        let role = d["role"]?.string ?? ""
        if !role.isEmpty && role != direct {
            say(clean("effective role remains " + role + " (from " + ProjectEnv.originText(d["origins"]?.array ?? [], roles: true) + ")"))
        }
    }
}
