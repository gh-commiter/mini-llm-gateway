import Foundation
import Testing
@testable import RouterCore

@Suite struct KBCommandParity {
    @Test func sharedCommands() throws {
        let repo = URL(fileURLWithPath: #filePath).deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent()
        let f = try JSONSerialization.jsonObject(with: Data(contentsOf: repo.appendingPathComponent("client/testdata/teams-kb-submissions.json"))) as! [String: Any]
        for c in f["admin"] as! [[String: Any]] {
            let responses = c["responses"] as! [[String: Any]]
            var requests: [[String]] = [], out = ""
            let cmd = AdminCommands(call: { m, p, b in
                requests.append([m, p, b.map { String(decoding: $0, as: UTF8.self) } ?? ""])
                let r = responses.first { $0["match"] as? String == m + " " + p }!
                return (r["status"] as! Int, Data((r["body"] as! String).utf8))
            }, out: { out += $0 })
            try cmd.run(c["args"] as! [String], AdminOpts(reason: (c["opts"] as? [String: Any])?["reason"] as? String ?? ""))
            let w = c["want"] as! [String: Any]
            #expect(out == w["out"] as! String)
            #expect(requests == w["requests"] as! [[String]])
        }
        for c in f["member"] as! [[String: Any]] {
            var out = "", requests: [[String]] = []
            let responses = c["responses"] as! [[String: Any]]
            let env = ProjectEnv(connect: { (ProjectConn(url: "", token: ""), "") }, state: "", cwd: "", stdin: { Data((c["input"] as? String ?? "").utf8) }, out: { out += $0 }, err: { _ in }, agent: false, send: { _, m, p, h, b in
                let body = b.flatMap { try? JSONSerialization.jsonObject(with: $0) }
                let canonical = body.flatMap { try? JSONSerialization.data(withJSONObject: $0, options: [.sortedKeys, .withoutEscapingSlashes]) }.map { String(decoding: $0, as: UTF8.self) } ?? ""
                requests.append([m, p, canonical])
                if m == "POST" { #expect(h["Idempotency-Key"] == "fixed") }
                let r = responses.first { $0["match"] as? String == m + " " + p }!
                return ProjectReply(status: r["status"] as! Int, data: Data((r["body"] as! String).utf8))
            })
            env.zone = TimeZone(identifier: "UTC")!
            try env.kb(c["args"] as! [String])
            #expect(out == c["out"] as! String)
            #expect(requests == c["requests"] as! [[String]])
        }
        for c in f["memberErrors"] as! [[String: Any]] {  // refused before any request
            let env = ProjectEnv(connect: { (ProjectConn(url: "", token: ""), "") }, state: "", cwd: "", stdin: { Data() }, out: { _ in }, err: { _ in }, agent: false, send: { _, m, p, _, _ in
                Issue.record("\(c["args"] as! [String]) sent \(m) \(p)")
                return ProjectReply(status: 599)
            })
            do { try env.kb(c["args"] as! [String]); Issue.record("expected refusal") }
            catch { #expect(String(describing: error) == c["error"] as! String) }
        }
        var out = ""
        let body = f["noteJSON"] as! String
        let bodyOut = f["noteJSONOut"] as! String
        let conn = ProjectConn(url: "", token: "")
        let env = ProjectEnv(connect: { (conn, "") }, state: "", cwd: "", stdin: { Data() }, out: { out += $0 }, err: { _ in }, agent: true, runConn: conn, send: { _, m, p, _, _ in
            #expect(m == "GET" && p == "/minigw/v1/notes/7")
            return ProjectReply(status: 200, data: Data(body.utf8))
        })
        try env.note(["read", "7", "--json"])
        #expect(out == bodyOut + "\n")
        #expect(throws: ProjectError.self) { try env.kb(["submissions"]) }
        for c in f["runSearch"] as! [[String: Any]] {
            var out = "", calls = 0
            let env = ProjectEnv(connect: { (conn, "") }, state: "", cwd: "", stdin: { Data() }, out: { out += $0 }, err: { _ in }, agent: true, runConn: conn, send: { _, m, p, _, _ in
                calls += 1
                #expect(m == "GET" && p == c["path"] as! String)
                #expect(Router.notesPath(m, String(p.split(separator: "?")[0])))
                return ProjectReply(status: 200, data: Data((c["body"] as! String).utf8))
            })
            env.zone = TimeZone(identifier: "UTC")!
            if let error = c["error"] as? String, !error.isEmpty {
                do { try env.kb(c["args"] as! [String]); Issue.record("expected refusal") }
                catch { #expect(String(describing: error) == c["error"] as! String) }
                #expect(calls == 0)
            } else {
                try env.kb(c["args"] as! [String])
                #expect(out == c["out"] as! String && calls == 1)
            }
        }
    }

    /// No line-producing function prints the hidden id of a team's own notes as a project: the team name stands there.
    @Test func hiddenIDIsNeverPrinted() throws {
        let seg: Int64 = (1 << 40) + 7
        func clean(_ what: String, _ out: String) {
            #expect(out.range(of: #"\d{13,}"#, options: .regularExpression) == nil && !out.contains("@team-"), "\(what) prints a hidden id: \(out)")
        }
        var out = ""
        let env = ProjectEnv(connect: { (ProjectConn(url: "", token: ""), "") }, state: "", cwd: "", stdin: { Data() }, out: { out += $0 }, err: { _ in }, agent: false, send: { _, m, p, _, _ in
            let body: String
            if p.hasPrefix("/minigw/v1/teams/") { body = #"{"id":7,"name":"acme","segment":\#(seg)}"# }
            else if p.hasSuffix("/notes") { body = #"{"notes":[{"id":5,"project":\#(seg),"team":"acme","class":"data","title":"T","author":"a","updated":"2026-11-17T10:00:00Z","revision":1}]}"# }
            else if p.contains("/notes/") { body = #"{"id":5,"project":\#(seg),"team":"acme","class":"data","title":"T","body":"b","author":"a","updated":"2026-11-17T10:00:00Z","revision":1}"# }
            else if p.hasSuffix("/kb/faq") { body = #"{"entries":[]}"# }
            else if p.contains("/kb/submissions") { body = #"{"submissions":[{"id":8,"project":\#(seg),"class":"data","title":"P","state":"pending"}]}"# }
            else if p.contains("/kb/search") { body = #"{"hits":[{"id":5,"title":"T","project":\#(seg),"class":"data","snippet":"s","updated":"2026-11-17T10:00:00Z","team":"acme"}]}"# }
            else { Issue.record("unexpected \(m) \(p)"); return ProjectReply(status: 599) }
            return ProjectReply(status: 200, data: Data(body.utf8))
        })
        env.zone = TimeZone(identifier: "UTC")!
        for args in [["note", "list", "--team", "acme"], ["note", "read", "5", "--team", "acme"], ["kb", "faq", "--team", "acme"], ["kb", "submissions", "--team", "acme"], ["kb", "search", "--team", "acme", "x"]] {
            out = ""
            if args[0] == "note" { try env.note(Array(args.dropFirst())) } else { try env.kb(Array(args.dropFirst())) }
            clean(args.joined(separator: " "), out)
        }
        #expect(out.contains("team acme"))
        #expect(KBMCP.teamNotID(["project": MCPNumber(raw: String(seg)), "team": "acme"])["project"] == nil)
        #expect(KBMCP.teamNotID(["project": MCPNumber(raw: "3")])["project"] != nil)
    }
}

