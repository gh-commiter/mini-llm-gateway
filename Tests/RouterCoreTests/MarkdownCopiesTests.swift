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
        // the export names its gateway and project (origin): a re-import into the same project knows its own file
        let originNote = try JSONSerialization.data(withJSONObject: fixture["originNote"]!)
        #expect(String(decoding: ProjectEnv.markdown(JSONValue.parse(originNote)!.dict), as: UTF8.self) == fixture["originMarkdown"] as! String)
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

    @Test func rateLimitedKBFilesAreBounded() throws {
        // A gateway that always answers 429 must end the command, not retry for ever: 30 attempts
        // at most, and the waits are recorded (the tests inject sleep).
        var waits: [TimeInterval] = []
        let env = ProjectEnv(connect: { (ProjectConn(url: "", token: ""), "") }, state: "", cwd: "", stdin: { Data() }, out: { _ in }, err: { _ in }, agent: false, send: { _, _, _, _, _ in
            ProjectReply(status: 429, retryAfter: "1")
        })
        env.sleep = { waits.append($0) }
        do {
            try env.kb(["import", "demo", "/tmp/note.md"])
            Issue.record("expected a bounded failure")
        } catch {
            #expect(String(describing: error) == "the gateway keeps answering 429: try again later")
        }
        #expect(waits.count == 29 && waits.allSatisfy { $0 == 1 })
    }

    private func exportEnv(_ cwd: String) -> ProjectEnv {
        ProjectEnv(connect: { (ProjectConn(url: "", token: ""), "") }, state: "", cwd: cwd, stdin: { Data() }, out: { _ in }, err: { _ in }, agent: false, send: { _, _, path, _, _ in
            if path == "/minigw/v1/projects/demo" { return ProjectReply(status: 200, data: Data(#"{"id":1}"#.utf8)) }
            if path.contains("/notes?") { return ProjectReply(status: 200, data: Data(#"{"notes":[{"id":5}]}"#.utf8)) }
            return ProjectReply(status: 200, data: Data(#"{"id":5,"title":"T","body":"secret"}"#.utf8))
        })
    }

    @Test func exportSurvivesDirectorySwapAndRefusesLinkDestination() throws {
        let fm = FileManager.default
        let tmp = fm.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        let out = tmp.appendingPathComponent("out"), moved = tmp.appendingPathComponent("out-moved"), victim = tmp.appendingPathComponent("victim")
        try fm.createDirectory(at: victim, withIntermediateDirectories: true)
        defer { try? fm.removeItem(at: tmp) }
        let swapping = exportEnv(tmp.path)
        swapping.exportHook = {
            precondition(rename(out.path, moved.path) == 0)
            precondition(symlink(victim.path, out.path) == 0)
        }
        try swapping.kb(["export", "demo", out.path])
        swapping.exportHook = nil
        #expect(try fm.contentsOfDirectory(atPath: victim.path).isEmpty)
        #expect(fm.fileExists(atPath: moved.appendingPathComponent("5.md").path))
        #expect(fm.fileExists(atPath: moved.appendingPathComponent("INDEX.md").path))
        // `out` is now a link: refused, nothing written to the target.
        #expect(throws: ProjectError.self) { try exportEnv(tmp.path).kb(["export", "demo", out.path]) }
        #expect(try fm.contentsOfDirectory(atPath: victim.path).isEmpty)
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
        try cmd.bundle(["import", "/tmp/team.zip", "--source", "remotesrc", "--dry-run", "--map", "ana=Ana", "--map=bor=Bor", "--adopt", "project:shop,user:alice", "--adopt=team:acme"])
        #expect(routes == ["/export", "/import"])
        #expect(bodies[0]["projects"] as? [String] == ["one", "two"])
        #expect(bodies[0]["with_private"] as? Bool == true)
        #expect(bodies[1]["source"] as? String == "remotesrc")
        #expect(bodies[1]["map"] as? [String: String] == ["ana": "Ana", "bor": "Bor"])
        #expect(bodies[1]["dry_run"] as? Bool == true)
        #expect(bodies[1]["adopt"] as? [String] == ["project:shop", "user:alice", "team:acme"])
        #expect(output.contains("create team Team"))
        for args in [["export", "x.zip"], ["export", "--team", "T", "x.zip", "--source", "s"], ["export", "--team", "T", "x.zip", "--adopt", "user:a"], ["import", "x.zip"], ["import", "x.zip", "--with-private"], ["import", "x.zip", "--source", "s", "--map", "bad"], ["import", "x.zip", "--source", "s", "--map", "a=b", "--map", "a=c"]] {
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

    @Test func memberImportDescriptorReads() throws {
        let fm = FileManager.default
        let dir = fm.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try fm.createDirectory(at: dir, withIntermediateDirectories: true)
        defer { try? fm.removeItem(at: dir) }
        let env = ProjectEnv(connect: { (ProjectConn(url: "", token: ""), "") }, state: "", cwd: dir.path, stdin: { Data() }, out: { _ in }, err: { _ in }, agent: false, send: { _, _, _, _, _ in ProjectReply(status: 500) })
        let child = dir.appendingPathComponent("child")
        try fm.createDirectory(at: child, withIntermediateDirectories: true)
        try Data("note".utf8).write(to: child.appendingPathComponent("note.md"))
        let fd = open(dir.path, O_RDONLY | O_DIRECTORY | O_NOFOLLOW)
        #expect(fd >= 0)
        defer { close(fd) }
        #expect(try env.readNoteFile("child/note.md", root: fd) == Data("note".utf8))
        try fm.removeItem(at: child)
        let outside = fm.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try fm.createDirectory(at: outside, withIntermediateDirectories: true)
        defer { try? fm.removeItem(at: outside) }
        try Data("secret".utf8).write(to: outside.appendingPathComponent("note.md"))
        try fm.createSymbolicLink(at: child, withDestinationURL: outside)
        #expect(throws: ProjectError.self) { try env.readNoteFile("child/note.md", root: fd) }
        let fifo = dir.appendingPathComponent("fifo.md")
        #expect(mkfifo(fifo.path, 0o600) == 0)
        #expect(throws: ProjectError.self) { try env.readNoteFile(fifo.path) }
        let big = dir.appendingPathComponent("big.md")
        try Data(repeating: 0, count: 96 << 10).write(to: big)
        #expect(throws: ProjectError.self) { try env.readNoteFile(big.path) }
    }

    @Test func memberImportBoundsEmptyDirectories() throws {
        let fm = FileManager.default
        let dir = fm.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        var child = dir
        for _ in 0..<33 { child.appendPathComponent("d") }
        try fm.createDirectory(at: child, withIntermediateDirectories: true)
        defer { try? fm.removeItem(at: dir) }
        var posts = 0
        let env = ProjectEnv(connect: { (ProjectConn(url: "", token: ""), "") }, state: "", cwd: dir.path, stdin: { Data() }, out: { _ in }, err: { _ in }, agent: false, send: { _, method, _, _, _ in
            if method == "POST" { posts += 1 }
            return ProjectReply(status: 200, data: Data(#"{"id":1}"#.utf8))
        })
        #expect(throws: ProjectError.self) { try env.kb(["import", "demo", dir.path]) }
        #expect(posts == 0)
    }
}
