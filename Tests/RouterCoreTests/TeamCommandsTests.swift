import Foundation
import Testing
@testable import RouterCore

@Suite struct TeamCommandParity {
    @Test func sharedCommands() throws {
        let repo = URL(fileURLWithPath: #filePath).deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent()
        let f = try JSONSerialization.jsonObject(with: Data(contentsOf: repo.appendingPathComponent("client/testdata/teams-kb-commands.json"))) as! [String: Any]
        for c in f["admin"] as! [[String: Any]] {
            let responses = c["responses"] as! [[String: Any]]
            var requests: [[String]] = [], out = "", err = "", exit: Int32 = 0
            let cmd = AdminCommands(call: { method, path, body in
                requests.append([method, path, body.map { String(decoding: $0, as: UTF8.self) } ?? ""])
                guard let r = responses.first(where: { $0["match"] as? String == method + " " + path }) else { return (599, Data()) }
                return (r["status"] as! Int, Data((r["body"] as! String).utf8))
            }, out: { out += $0 })
            do { try cmd.run(c["args"] as! [String], AdminOpts(explain: (c["opts"] as? [String: Any])?["explain"] as? Bool ?? false)) }
            catch let e as AdminCLIError { err = "minigw: " + e.message + "\n"; exit = e.code }
            let w = c["want"] as! [String: Any]
            #expect(out == w["out"] as! String)
            #expect(err == w["err"] as! String)
            #expect(Int(exit) == w["exit"] as! Int)
            #expect(requests == w["requests"] as! [[String]])
        }
        for c in f["projects"] as! [[String: Any]] {
            var out = ""
            let env = ProjectEnv(connect: { (ProjectConn(url: "", token: ""), "") }, state: "", cwd: "", stdin: { Data() }, out: { out += $0 }, err: { _ in }, agent: false, send: { _, method, path, _, _ in
                #expect(method == "GET")
                #expect(path == "/minigw/v1/projects")
                return ProjectReply(status: 200, data: Data((c["body"] as! String).utf8))
            })
            try env.project(["list"])
            #expect(out == c["out"] as! String)
        }
        for c in f["member"] as! [[String: Any]] {
            var out = "", err = "", calls = 0
            let env = ProjectEnv(connect: { (ProjectConn(url: "", token: ""), "") }, state: "", cwd: "", stdin: { Data() }, out: { out += $0 }, err: { _ in }, agent: false, send: { _, method, path, _, _ in
                calls += 1
                #expect(method == "GET")
                if calls == 1, let lookup = c["lookup"] as? String {
                    #expect(path == "/minigw/v1/teams?archived=1")
                    return ProjectReply(status: 200, data: Data(lookup.utf8))
                }
                #expect(path == c["path"] as! String)
                return ProjectReply(status: c["status"] as! Int, data: Data((c["body"] as! String).utf8))
            })
            do { try env.team(c["args"] as! [String]) }
            catch let e as ProjectError { err = e.message }
            #expect(out == c["out"] as! String)
            #expect(err == (c["error"] as? String ?? ""))
            #expect(calls == (c["lookup"] == nil ? 1 : 2))
        }
    }

    @Test func agentAndArguments() throws {
        let env = ProjectEnv(connect: { fatalError("must not connect") }, state: "", cwd: "", stdin: { Data() }, out: { _ in }, err: { _ in }, agent: true, send: { _, _, _, _, _ in fatalError("must not send") })
        #expect(throws: ProjectError.self) { try env.team(["list"]) }
        env.agent = false
        for args in [[], ["show"], ["list", "extra"], ["unknown"], ["list", "--bad"]] {
            #expect(throws: ProjectError.self) { try env.team(args) }
        }
    }
}
