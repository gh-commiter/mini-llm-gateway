import Foundation
import Testing
@testable import RouterCore

private let mcpRepo = URL(fileURLWithPath: #filePath).deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent()
private func mcpFixture() throws -> [String: Any] {
    try JSONSerialization.jsonObject(with: Data(contentsOf: mcpRepo.appendingPathComponent("client/testdata/kb-mcp.json"))) as! [String: Any]
}
private func mcpInput(_ frames: [String]) throws -> String {
    let f = try mcpFixture()
    return ([f["initialize"] as! String, f["initialized"] as! String] + frames).joined(separator: "\n") + "\n"
}
private func mcpRun(_ input: String, call: (String, [String: Any]) throws -> Any = { _, _ in [:] }) throws -> [[String: Any]] {
    var bytes = Array(input.utf8), out = Data()
    try KBMCP.loop(read: {
        if bytes.isEmpty { return nil }
        let chunk = Data(bytes.prefix(37)); bytes.removeFirst(min(37, bytes.count)); return chunk
    }, write: { out.append($0) }, call: call)
    return try out.split(separator: 10).map {
        #expect($0.count <= KBMCP.replyLimit)
        let v = try JSONSerialization.jsonObject(with: Data($0)) as! [String: Any]
        #expect(v["jsonrpc"] as? String == "2.0"); return v
    }
}

@Suite struct KBMCPTests {
    @Test func sharedProtocol() throws {
        for c in try mcpFixture()["cases"] as! [[String: Any]] {
            var calls = 0
            let replies = try mcpRun(mcpInput([c["frame"] as! String])) { name, args in
                calls += 1; #expect(name == c["call"] as? String)
                if let id = c["id"] as? String { #expect(String(args["id"] as! Int64) == id) }
                if let query = c["query"] as? String { #expect(args["query"] as? String == query) }
                return ["ok": true]
            }
            #expect(replies.count == (c["silent"] as? Bool == true ? 1 : 2), "\(c["name"]!)")
            #expect((replies[0]["result"] as? [String: Any])?["protocolVersion"] as? String == KBMCP.version)
            #expect(calls == (c["call"] == nil ? 0 : 1))
            if c["silent"] as? Bool == true { continue }
            let reply = replies[1]
            if let code = c["code"] as? Int {
                #expect((reply["error"] as? [String: Any])?["code"] as? Int == code); continue
            }
            let result = reply["result"] as! [String: Any]
            if let expected = c["result"] as? [String: Any] { #expect(NSDictionary(dictionary: result).isEqual(to: expected)) }
            if c["tools"] as? Bool == true {
                let tools = result["tools"] as! [[String: Any]]
                #expect(tools.count == 2)
                #expect(NSDictionary(dictionary: ["tools": tools]).isEqual(to: ["tools": try mcpFixture()["tools"]!]))
                #expect(tools.map { $0["name"] as! String } == ["kb_search", "note_read"])
            }
            if c["call"] != nil { #expect(result["isError"] as? Bool == false) }
        }
    }

    @Test func boundsAndEOF() throws {
        #expect(try mcpRun("").isEmpty)
        #expect(try mcpRun("{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"ping\"}").count == 1)
        let large = try mcpRun(String(repeating: "x", count: KBMCP.frameLimit + 1) + "\n")
        #expect((large[0]["error"] as? [String: Any])?["code"] as? Int == -32600)
        let input = try mcpInput(["{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"tools/call\",\"params\":{\"name\":\"note_read\",\"arguments\":{\"id\":1}}}"])
        let replies = try mcpRun(input) { _, _ in ["title": String(repeating: "\u{0}", count: KBMCP.replyLimit)] }
        #expect((replies[1]["result"] as? [String: Any])?["isError"] as? Bool == true)
        let errors = try mcpRun(input) { _, _ in throw ProjectError("denied\u{1b}\u{202e}\nreference") }
        let content = (errors[1]["result"] as! [String: Any])["content"] as! [[String: Any]]
        #expect(content[0]["text"] as? String == KBMCP.reference + "\ndenied\nreference")
    }

    @Test func frozenRoutesAndSanitization() throws {
        for run in [false, true] {
            var connects = 0, calls = 0, out = Data()
            var input = Data(try mcpInput([
                "{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"tools/call\",\"params\":{\"name\":\"note_read\",\"arguments\":{\"id\":9007199254740993}}}",
                "{\"jsonrpc\":\"2.0\",\"id\":3,\"method\":\"tools/call\",\"params\":{\"name\":\"kb_search\",\"arguments\":{\"query\":\"x &project=2\"}}}"
            ]).utf8)
            let env = ProjectEnv(connect: { connects += 1; return (ProjectConn(url: "frozen", token: "secret"), "") }, state: "", cwd: "", stdin: { Data() }, out: { out.append(Data($0.utf8)) }, err: { _ in }, agent: run,
                                 runConn: run ? ProjectConn(url: "frozen", token: "secret") : nil, send: { c, method, path, headers, body in
                #expect(c.url == "frozen" && method == "GET" && headers.isEmpty && body == nil)
                if path == "/minigw/v1/projects/slug" { return ProjectReply(status: 200, data: Data("{\"id\":7}".utf8)) }
                calls += 1
                let base = run ? "/minigw/v1" : "/minigw/v1/projects/7"
                if calls == 1 {
                    #expect(path == base + "/notes/9007199254740993")
                    return ProjectReply(status: 200, data: try! JSONSerialization.data(withJSONObject: ["class": "data", "title": "a\u{1b}\u{202e}b", "author": "x\u{200b}y", "body": String(repeating: "界", count: 6000), "provenance": ["device": "d\u{1b}"]]))
                }
                #expect(path == base + "/kb/search?q=x+%26project%3D2")
                return ProjectReply(status: 200, data: Data("{\"hits\":[{\"class\":\"private\",\"title\":\"secret\"},{\"class\":\"data\",\"title\":\"ok\"}]}".utf8))
            })
            try env.kbMCP(run ? [] : ["--project", "slug"], executable: "/minigw", read: { if input.isEmpty { return nil }; let d = input; input = Data(); return d }, write: { out.append($0) })
            #expect(connects == (run ? 0 : 1) && calls == 2)
            let replies = try out.split(separator: 10).map { try JSONSerialization.jsonObject(with: Data($0)) as! [String: Any] }
            let result = replies[1]["result"] as! [String: Any]
            let text = (result["content"] as! [[String: Any]])[0]["text"] as! String
            let note = try JSONSerialization.jsonObject(with: Data(text.dropFirst(KBMCP.reference.count + 1).utf8)) as! [String: Any]
            #expect(note["title"] as? String == "ab" && note["author"] as? String == "xy")
            #expect(note["truncated"] as? Bool == true)
            #expect((note["body"] as! String).utf8.count <= 16 << 10 && (note["body"] as! String).hasSuffix("\n[truncated]"))
            #expect(!(String(decoding: out, as: UTF8.self).contains("secret")))
        }
    }

    @Test func failClosedAndConfig() throws {
        var output = ""
        let env = ProjectEnv(connect: { Issue.record("member fallback"); return (ProjectConn(url: "", token: ""), "") }, state: "", cwd: "", stdin: { Data() }, out: { output += $0 }, err: { _ in }, agent: true, send: { _, _, _, _, _ in Issue.record("network"); return ProjectReply(status: 0) })
        #expect(throws: ProjectError.self) { try env.kbMCP([], executable: "/minigw", read: { nil }, write: { _ in }) }
        #expect(throws: ProjectError.self) { try env.kbMCP(["--project", "same"], executable: "/minigw", read: { nil }, write: { _ in }) }
        try env.kbMCP(["--print-config"], executable: "/a\"\\b/minigw", read: { nil }, write: { _ in })
        let config = try JSONSerialization.jsonObject(with: Data(output.utf8)) as! [String: Any]
        let servers = config["mcpServers"] as! [String: Any], server = servers["minigw-kb"] as! [String: Any]
        #expect(server["command"] as? String == "/a\"\\b/minigw" && server["args"] as? [String] == ["kb", "mcp"])
        #expect(!output.contains("token") && !output.contains("AUTH"))
    }
}
