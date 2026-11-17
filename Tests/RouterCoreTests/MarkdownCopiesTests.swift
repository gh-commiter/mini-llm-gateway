import Foundation
import Testing
@testable import RouterCore

@Suite struct MarkdownCopies {
    @Test func sharedMarkdownAndMemberFiles() throws {
        let repo = URL(fileURLWithPath: #filePath).deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent()
        let fixture = try JSONSerialization.jsonObject(with: Data(contentsOf: repo.appendingPathComponent("client/testdata/markdown-copy.json"))) as! [String: Any]
        let note = try JSONSerialization.data(withJSONObject: fixture["note"]!)
        let expected = fixture["markdown"] as! String
        #expect(String(decoding: ProjectEnv.markdown(JSONValue.parse(note)!.dict), as: UTF8.self) == expected)
        let dir = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: dir) }
        var calls: [String] = [], output = "", limited = false, limitedImport = false
 var slept: TimeInterval = 0
        let env = ProjectEnv(connect: { (ProjectConn(url: "", token: ""), "") }, state: "", cwd: dir.path, stdin: { Data() }, out: { output += $0 }, err: { _ in }, agent: false, send: { _, method, path, _, body in
            calls.append(method + " " + path)
 if !limited { limited = true; return ProjectReply(status: 429, retryAfter: "1") }
            if path == "/minigw/v1/projects/demo" { return ProjectReply(status: 200, data: Data(#"{"id":1}"#.utf8)) }
            if path.contains("/notes?") { return ProjectReply(status: 200, data: Data(#"{"notes":[{"id":17}]}"#.utf8)) }
            if path.hasSuffix("/notes/17") { return ProjectReply(status: 200, data: note) }
            #expect(path == "/minigw/v1/projects/1/kb/import", "\(method) \(path)")
            if !limitedImport { limitedImport = true; return ProjectReply(status: 429, retryAfter: "1") }
            let input = try! JSONSerialization.jsonObject(with: body!) as! [String: Any]
            #expect(input["markdown"] as? String == expected)
            #expect(input["path"] as? String == "17.md")
            #expect(input["submit"] as? Bool == true)
            return ProjectReply(status: 200, data: Data(#"{"action":"skip","key":"id:17"}"#.utf8))
        })
        env.sleep = { slept += $0 }
        try env.kb(["export", "demo", dir.path])
        #expect(try String(contentsOf: dir.appendingPathComponent("17.md"), encoding: .utf8) == expected)
        #expect(FileManager.default.fileExists(atPath: dir.appendingPathComponent("INDEX.md").path))
        try env.kb(["import", "demo", dir.path, "--submit"])
        #expect(output == "export 1 notes\nskip id:17\n")
        #expect(calls.count == 7 && slept == 2)
        #expect(throws: ProjectError.self) { try env.kb(["export", "demo", dir.path]) }
        let beforeAgentCalls = calls.count
        env.agent = true
        #expect(throws: ProjectError.self) { try env.kb(["export", "demo", dir.path]) }
        #expect(throws: ProjectError.self) { try env.kb(["import", "demo", dir.path]) }
        #expect(calls.count == beforeAgentCalls && slept == 2)
    }

    @Test func ownerBundleArgumentsAndNestedJSON() throws {
        var output = "", bodies: [[String: Any]] = [], routes: [String] = []
        let cmd = AdminCommands(call: { method, path, body in
            #expect(method == "POST")
            routes.append(path)
            bodies.append(try! JSONSerialization.jsonObject(with: body!) as! [String: Any])
            return (200, Data(#"{"lines":["create team Team","skip note"]}"#.utf8))
        }, out: { output += $0 })
        try cmd.bundle(["export", "--team", "Team", "--project", "one", "--project=two", "--with-private", "/tmp/team.zip"])
        try cmd.bundle(["import", "/tmp/team.zip", "--source", "remotesrc", "--dry-run", "--map", "ana=Ana", "--map=bor=Bor"])
        #expect(routes == ["/export", "/import"])
        #expect(bodies[0]["projects"] as? [String] == ["one", "two"])
        #expect(bodies[0]["with_private"] as? Bool == true)
        #expect(bodies[1]["source"] as? String == "remotesrc")
        #expect(bodies[1]["map"] as? [String: String] == ["ana": "Ana", "bor": "Bor"])
        #expect(bodies[1]["dry_run"] as? Bool == true)
        #expect(output.contains("create team Team"))
        for args in [["export", "x.zip"], ["export", "--team", "T", "x.zip", "--source", "s"], ["import", "x.zip"], ["import", "x.zip", "--with-private"], ["import", "x.zip", "--source", "s", "--map", "bad"], ["import", "x.zip", "--source", "s", "--map", "a=b", "--map", "a=c"]] {
            #expect(throws: AdminCLIError.self) { try cmd.bundle(args) }
        }
        #expect(routes.count == 2)
    }

    @Test func memberImportRefusesLinks() throws {
        let fm = FileManager.default
        let dir = fm.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try fm.createDirectory(at: dir, withIntermediateDirectories: true)
        defer { try? fm.removeItem(at: dir) }
        let secret = dir.appendingPathComponent("secret.md")
        try Data("secret".utf8).write(to: secret)
        let link = dir.appendingPathComponent("link.md")
        try fm.createSymbolicLink(at: link, withDestinationURL: secret)
        var posts = 0, output = ""
        let env = ProjectEnv(connect: { (ProjectConn(url: "", token: ""), "") }, state: "", cwd: dir.path, stdin: { Data() }, out: { output += $0 }, err: { _ in }, agent: false, send: { _, method, path, _, _ in
            if method == "GET", path == "/minigw/v1/projects/demo" { return ProjectReply(status: 200, data: Data(#"{"id":1}"#.utf8)) }
            posts += 1
            return ProjectReply(status: 200, data: Data(#"{"action":"create","key":"id:1"}"#.utf8))
        })
        // A folder import refuses a link before posting anything, and a file given through a link is
        // refused: files are opened without following them.
        #expect(throws: ProjectError.self) { try env.kb(["import", "demo", dir.path]) }
        #expect(posts == 0)
        #expect(throws: ProjectError.self) { try env.kb(["import", "demo", link.path]) }
        #expect(posts == 0)
    }
}
