import CryptoKit
import Foundation
import Testing
@testable import RouterCore

// The Mac client's project commands (0.6.0), proven against the Go client: the shared fixtures client/testdata/project-*.json and
// projects.golden.json are asserted here and in client/project_parity_test.go. Nothing here touches the real user config:
// every state file, folder and gateway is temporary.

private let repo = URL(fileURLWithPath: #filePath).deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent()

private func fixtureData(_ name: String) throws -> Data { try Data(contentsOf: repo.appendingPathComponent("client/testdata/" + name)) }
private func fixtureJSON(_ name: String) throws -> [String: Any] { try JSONSerialization.jsonObject(with: fixtureData(name)) as! [String: Any] }

/// A real temporary folder with its canonical path (macOS temp dirs are links).
private func tempRoot() throws -> String {
    let u = FileManager.default.temporaryDirectory.appendingPathComponent("mgproj-" + UUID().uuidString, isDirectory: true).cleanedAtExit()
    try FileManager.default.createDirectory(at: u, withIntermediateDirectories: true)
    return try Projects.realFolder(u.path)
}

private func mkdir(_ p: String) throws { try FileManager.default.createDirectory(atPath: p, withIntermediateDirectories: true) }

private func fileText(_ p: String) -> String? { FileManager.default.contents(atPath: p).map { String(decoding: $0, as: UTF8.self) } }

@Suite struct ProjectParity {
    @Test func linkFileIsTheFileGoWrites() throws {
        let fx = try fixtureJSON("project-state.json")
        let links = (fx["golden_links"] as! [[String: Any]]).map {
            ProjectLink(gateway: $0["gateway"] as! String, project: ($0["project"] as! NSNumber).int64Value, slug: $0["slug"] as! String, path: $0["path"] as! String)
        }
        let dir = try tempRoot()
        let p = dir + "/sub/projects.json"
        try Projects.save(p, ProjectState(links: links))
        let golden = try fixtureData("projects.golden.json")
        #expect(try Data(contentsOf: URL(fileURLWithPath: p)) == golden, "bytes differ from client/testdata/projects.golden.json")
        #expect(try Projects.load(repo.path + "/client/testdata/projects.golden.json").links == links)  // Go's file reads back
        var st = stat()
        #expect(stat(p, &st) == 0 && st.st_mode & 0o777 == 0o600, "link file must be private (0600)")
        try Projects.save(dir + "/empty.json", ProjectState())
        #expect(fileText(dir + "/empty.json") == "{\n  \"version\": 1,\n  \"links\": null\n}\n")
        #expect(try Projects.load(dir + "/missing.json").links.isEmpty)  // no file = no links

        for c in fx["load_ok"] as! [[String: Any]] {
            try Data((c["text"] as! String).utf8).write(to: URL(fileURLWithPath: p))
            #expect(try Projects.load(p).links.count == (c["links"] as! Int), "\(c["name"]!)")
        }
        for c in fx["load_err"] as! [[String: Any]] {
            try Data((c["text"] as! String).utf8).write(to: URL(fileURLWithPath: p))
            #expect(throws: ProjectError.self, "\(c["name"]!): a broken file is an error, not an empty state") { try Projects.load(p) }
            do { _ = try Projects.load(p) } catch { #expect((error as! ProjectError).message.hasPrefix("cannot read " + p + ": ")) }
        }
    }

    @Test func resolutionAndHeaderDecisions() throws {
        let fx = try fixtureJSON("project-header.json")
        for c in fx["resolution"] as! [[String: Any]] {
            let name = c["name"] as! String
            let root = try tempRoot()
            for d in c["dirs"] as! [String] { try mkdir(root + "/" + d) }
            for d in c["git"] as! [String] { try mkdir(root + "/" + d + "/.git") }
            for (d, h) in c["hints"] as! [String: String] { try Data(h.utf8).write(to: URL(fileURLWithPath: root + "/" + d + "/.minigw-project")) }
            for (from, to) in c["symlinks"] as! [String: String] {
                try FileManager.default.createSymbolicLink(atPath: root + "/" + from, withDestinationPath: root + "/" + to)
            }
            let links = (c["links"] as! [[String: Any]]).map {
                ProjectLink(gateway: $0["gateway"] as! String, project: ($0["project"] as! NSNumber).int64Value, slug: $0["slug"] as! String, path: root + "/" + ($0["path"] as! String))
            }
            let statePath = try tempRoot() + "/projects.json"
            try Projects.save(statePath, ProjectState(links: links))
            let cwd = root + "/" + (c["cwd"] as! String), gw = c["gateway"] as! String
            let want = c["want"] as! [String: Any]
            let res = try Projects.resolve(ProjectState(links: links), gateway: gw, cwd: cwd)
            let state = ["none", "ok", "hintDiff"][[Projects.Applies.none, .ok, .hintDiff].firstIndex(of: res.state)!]
            #expect(state == want["state"] as! String, "\(name): state")
            #expect((res.state == .none ? "" : res.link.slug) == want["slug"] as! String, "\(name): slug")
            #expect(res.hint == want["hint"] as! String, "\(name): hint")
            var warned = ""
            do {
                let h = try Projects.headerFor(cwd: cwd, gateway: gw, statePath: statePath, agent: c["agent"] as! Bool, warn: { warned += $0 })
                #expect(h == want["header"] as! String && !(want["error"] as! Bool), "\(name): header \(h)")
            } catch {
                #expect(want["error"] as! Bool && (error as! ProjectError).message.contains("says \(want["hint"]!)"), "\(name): \(error)")
            }
            // a suggestion is printed once for a hint without a link, and never otherwise
            #expect(warned.isEmpty == !(want["state"] as! String == "none" && !(want["hint"] as! String).isEmpty && !(c["agent"] as! Bool)), "\(name): warning \(warned)")
        }
    }

    @Test func proxyHeaderRewriteTable() throws {
        let rows = try fixtureJSON("project-header.json")["rewrite"] as! [[String: Any]]
        #expect(rows.count > 10)
        for r in rows {
            let got = Projects.rewriteHead(Data((r["head"] as! String).utf8), project: r["project"] as! String)
            if r["reject"] as? Bool == true {  // F1: fail closed
                #expect(got == nil, "\(r["name"]!)")
            } else {
                #expect(got.map { String(decoding: $0, as: UTF8.self) } == r["want"] as? String, "\(r["name"]!)")
            }
        }
    }

    @Test func headLimitIsOnTheHeadsLengthWhateverTheChunking() throws {  // R4: the shared fixture's limit rows
        let rows = try fixtureJSON("project-header.json")["limit"] as! [[String: Any]]
        #expect(rows.count >= 6)
        for r in rows {
            let pre = "GET / HTTP/1.1\r\nX-Pad: "
            let req = Array((pre + String(repeating: "a", count: (r["headLen"] as! Int) - pre.utf8.count) + "\r\n\r\n").utf8)  // utf8: "\r\n" is one Character
            let cut = r["cut"] as! Int
            let parts = cut > 0 ? [Data(req[..<cut]), Data(req[cut...])] : [Data(req)]
            let rw = Projects.HeadRewriter(project: "7")
            var ok = true, sent = Data()
            for p in parts { if let o = rw.feed(p) { sent += o } else { ok = false } }
            #expect(ok == (r["accept"] as! Bool), "\(r["name"]!)")
            if ok { #expect(String(decoding: sent.prefix(40), as: UTF8.self).hasPrefix("GET / HTTP/1.1\r\nX-Minigw-Project: 7\r\n")) }
        }
    }

    @Test func headIsRewrittenWhateverTheChunking() {
        let req = "POST /v1/messages HTTP/1.1\r\nHost: x\r\nx-minigw-project: 9\r\nContent-Length: 4\r\n\r\nbody"
        let want = "POST /v1/messages HTTP/1.1\r\nX-Minigw-Project: 42\r\nHost: x\r\nContent-Length: 4\r\n\r\nbody"
        let bytes = Array(req.utf8)
        for step in [1, 3, 7, 1000] {
            let rw = Projects.HeadRewriter(project: "42")
            var sent = Data()
            for i in stride(from: 0, to: bytes.count, by: step) { sent += rw.feed(Data(bytes[i..<min(i + step, bytes.count)]))! }
            #expect(String(decoding: sent, as: UTF8.self) == want, "chunks of \(step)")
        }
        let rw = Projects.HeadRewriter(project: "1")  // after the head, later data passes unchanged (only the first request is rewritten)
        _ = rw.feed(Data("GET / HTTP/1.1\r\n\r\n".utf8))!
        #expect(rw.feed(Data("X-Minigw-Project: 9\r\n".utf8)) == Data("X-Minigw-Project: 9\r\n".utf8))
        // F2: no end of head within 64 KiB: the connection is closed (nil), nothing is passed on; also when it arrives in pieces
        #expect(Projects.HeadRewriter(project: "1").feed(Data(repeating: 0x61, count: 70 << 10)) == nil)
        let slow = Projects.HeadRewriter(project: "1")
        #expect(slow.feed(Data("GET / HTTP/1.1\r\n".utf8)) == Data())
        #expect(slow.feed(Data(repeating: 0x61, count: 40 << 10)) == Data())
        #expect(slow.feed(Data(repeating: 0x61, count: 30 << 10)) == nil)
        // F1: a forged header that only a late chunk completes (obs-fold, space before the colon, bare LF) closes it too
        for late in [" X-Minigw-Project: 7\r\n\r\n", "X-Minigw-Project : 7\r\n\r\n", "X-A: 1\nX-Minigw-Project: 7\r\n\r\n"] {
            let rw = Projects.HeadRewriter(project: "42")
            #expect(rw.feed(Data("GET / HTTP/1.1\r\nHost: x\r\n".utf8)) == Data())
            #expect(rw.feed(Data(late.utf8)) == nil, "\(late.debugDescription)")
        }
    }

    @Test func routerParserRefusesWhatGoReadsDifferently() {
        func parse(_ head: String) -> ParseResult { HTTPParser.parse(Data((head + "\r\n\r\n").utf8)) }
        func status(_ r: ParseResult) -> Int { if case .error(let s, _) = r { return s }; return 0 }
        let line = "POST /v1/messages HTTP/1.1\r\n"
        guard case .request(let ok) = parse(line + "Host: x\r\nX-Minigw-Project: 7") else { Issue.record("valid head refused"); return }
        #expect(ok.header("x-minigw-project") == "7")
        for bad in ["Host: x\r\n X-Minigw-Project: 7", "Host: x\r\n\tX-Minigw-Project: 7", "Host: x\r\nX-Minigw-Project : 7",
                    "Host: x\r\nX-Minigw-Project\u{a0}: 7", "Host: x\r\n\u{a0}X-Minigw-Project: 7", "Host: x\r\nX-A: 1\nX-Minigw-Project: 7",
                    "Host: x\r\nX Y: 1", "Host: x\r\n: 1", "Host: x\r\nX-Minigw-Project: 1\r\nx-minigw-project: 2"] {
            #expect(status(parse(line + bad)) == 400, "\(bad.debugDescription)")
        }
    }

    @Test func slugRuleIsGos() {
        for s in ["ab", "a1", "fakture", "a-b", String(repeating: "a", count: 63)] { #expect(Projects.isSlug(s), "\(s)") }
        for s in ["", "a", "Ab", "-ab", "a b", "a/b", "../x", "ab\n", "č", String(repeating: "a", count: 64)] { #expect(!Projects.isSlug(s), "\(s.debugDescription)") }
    }

    @Test func hintFileIsOnlyAPlainSlug() throws {
        let d = try tempRoot()
        for (content, want) in [("fakture\n", "fakture"), ("  fakture \r\nsecond line", "fakture"), ("", ""), ("Fakture", ""), ("a", ""), ("ab cd", ""),
                                ("../x", ""), (String(repeating: "a", count: 300), "")] {
            try Data(content.utf8).write(to: URL(fileURLWithPath: d + "/.minigw-project"))
            #expect(Projects.readHint(d) == want, "\(content.debugDescription)")
        }
        try FileManager.default.removeItem(atPath: d + "/.minigw-project")
        try FileManager.default.createSymbolicLink(atPath: d + "/.minigw-project", withDestinationPath: "/etc/hostname")
        #expect(Projects.readHint(d) == "", "a symlinked hint is never read")
    }

    @Test func flagParsingFollowsGoFlag() throws {
        let specs: [(String, GoFlags.Kind)] = [("json", .bool), ("class", .string), ("limit", .int)]
        func parse(_ a: [String]) -> GoFlags? { try? GoFlags.parse(specs, a) }
        #expect(parse(["a", "--json", "b", "-class=x", "--limit", "5", "-"])?.positional == ["a", "b", "-"])
        #expect(parse(["--json=false"])?.bool("json") == false && parse(["--json=false"])?.set.contains("json") == true)
        #expect(parse(["--class", "--json"])?.string("class") == "--json")  // a value is taken as it comes
        #expect(parse(["--", "--json"])?.positional == ["--json"] && parse(["--", "--json"])?.bool("json") == false)  // "--" ends the flags ...
        #expect(parse(["--", "a", "--json"])?.positional == ["a"] && parse(["--", "a", "--json"])?.bool("json") == true)  // ... but Go's loop parses on after the first positional
        for bad in [["--nope"], ["-h"], ["--help"], ["--class"], ["--limit", "x"], ["--json=maybe"], ["---json"], ["--=x"]] { #expect(parse(bad) == nil, "\(bad)") }
        #expect(parse(["--limit", "0x10"])?.int("limit") == 16 && parse(["--limit", "-3"])?.int("limit") == -3)
    }
}

// MARK: commands against the fixture's canned answers (output text, errors, requests, link file)

@Suite struct ProjectCommandParity {
    struct Out { var out = "", err = "" }

    @Test func everyCommandMatchesTheGoOutput() throws {
        let fx = try fixtureJSON("project-commands.json")
        let gw = fx["gateway"] as! String
        let cases = fx["cases"] as! [[String: Any]]
        #expect(cases.count > 100)
        for c in cases {
            let name = c["name"] as! String
            let root = try tempRoot()
            let sub: (String) -> String = { $0.replacingOccurrences(of: "{root}", with: root) }
            let unsub: (String) -> String = { $0.replacingOccurrences(of: root, with: "{root}") }
            for d in ["proj", "proj/src"] { try mkdir(root + "/" + d) }
            for (f, body) in (c["files"] as? [String: String]) ?? [:] { try Data(body.utf8).write(to: URL(fileURLWithPath: root + "/" + f)) }
            let statePath = try tempRoot() + "/projects.json"
            if let l = c["link"] as? [String: Any] {
                try Projects.save(statePath, ProjectState(links: [ProjectLink(gateway: gw, project: (l["project"] as! NSNumber).int64Value, slug: l["slug"] as! String, path: root + "/" + (l["rel"] as! String))]))
            }
            if let h = c["hint"] as? String { try Data((h + "\n").utf8).write(to: URL(fileURLWithPath: root + "/proj/.minigw-project")) }
            let responses = c["responses"] as! [[String: Any]]
            nonisolated(unsafe) var requests: [[String]] = []
            nonisolated(unsafe) var outText = "", errText = ""
            let conn = ProjectConn(url: "http://gateway.test", token: "tok")
            let agent = c["agent"] as? String ?? ""
            let env = ProjectEnv(
                connect: { (conn, gw) }, state: statePath, cwd: root + "/" + (c["cwd"] as? String ?? "proj"),
                stdin: { Data(sub(c["stdin"] as? String ?? "").utf8) }, out: { outText += $0 }, err: { errText += $0 },
                agent: !agent.isEmpty, runConn: agent == "run" ? conn : nil,
                send: { _, method, path, headers, body in
                    requests.append([method, path, body.map { String(decoding: $0, as: UTF8.self) } ?? "", headers["Idempotency-Key"] ?? ""])
                    guard let r = responses.first(where: { ($0["match"] as! String) == method + " " + path }) else {
                        return ProjectReply(status: 599, data: Data("no such response in the fixture".utf8))
                    }
                    let h = r["headers"] as? [String: String]
                    return ProjectReply(status: r["status"] as! Int, data: Data((r["body"] as! String).utf8), retryAfter: h?["Retry-After"])
                }, sleep: { _ in })
            env.zone = TimeZone(identifier: "UTC")!
            env.origin = { _ in c["origin"] as? String ?? "" }  // the folder's raw remote.origin.url
            let args = (c["args"] as! [String]).map(sub)
            var failure = ""
            do {
                switch args[0] {
                case "note": try env.note(Array(args.dropFirst()))
                case "usage": try env.usage(Array(args.dropFirst()))
                default: try env.project(Array(args.dropFirst()))
                }
            } catch { failure = (error as? ProjectError)?.message ?? "\(error)" }
            let want = c["want"] as! [String: Any]
            #expect(unsub(outText) == want["out"] as! String, "\(name): stdout")
            #expect(unsub(errText) == want["err"] as! String, "\(name): stderr")
            #expect(unsub(failure) == want["error"] as! String, "\(name): error")
            #expect(requests.map { [$0[0], $0[1], unsub($0[2]), $0[3]] } == want["requests"] as! [[String]], "\(name): requests")
            let links = ((try? Projects.load(statePath))?.links ?? []).map { l in
                "\(l.slug):\(l.project)@" + (l.path.hasPrefix(root + "/") ? String(l.path.dropFirst(root.count + 1)) : l.path)
            }
            #expect(links == want["links"] as! [String], "\(name): link file")
        }
    }
}

// MARK: a fake gateway over real HTTP (the real transport, 403/404/409/429/503, the whole flow)

/// A small in-process HTTP server on 127.0.0.1: one request per connection, answered by `handler`.
final class ProjectFakeGateway: @unchecked Sendable {
    struct Req { var method: String, path: String, headers: [String: String], body: Data }
    struct Resp { var status: Int, headers: [String: String] = [:], body: String = "" }
    let handler: @Sendable (Req) -> Resp
    private var fd: Int32 = -1
    private(set) var port: UInt16 = 0
    private let lock = NSLock()
    private var seen: [Req] = []
    var requests: [Req] { lock.withLock { seen } }

    init(_ handler: @escaping @Sendable (Req) -> Resp) {
        self.handler = handler
        fd = socket(AF_INET, SOCK_STREAM, 0)
        var one: Int32 = 1
        setsockopt(fd, SOL_SOCKET, SO_REUSEADDR, &one, socklen_t(MemoryLayout<Int32>.size))
        var a = sockaddr_in()
        a.sin_family = sa_family_t(AF_INET); a.sin_addr.s_addr = inet_addr("127.0.0.1")
        _ = withUnsafePointer(to: &a) { $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { bind(fd, $0, socklen_t(MemoryLayout<sockaddr_in>.size)) } }
        listen(fd, 16)
        var l = socklen_t(MemoryLayout<sockaddr_in>.size)
        _ = withUnsafeMutablePointer(to: &a) { $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { getsockname(fd, $0, &l) } }
        port = UInt16(bigEndian: a.sin_port)
        let listener = fd
        Thread { [self] in
            while true {
                let c = accept(listener, nil, nil)
                if c < 0 { return }
                Thread { self.serve(c) }.start()
            }
        }.start()
    }

    var url: String { "http://127.0.0.1:\(port)" }
    func stop() { close(fd) }

    private func serve(_ c: Int32) {
        defer { close(c) }
        var buf = Data(), tmp = [UInt8](repeating: 0, count: 65536)
        while true {
            if case .request(let r) = HTTPParser.parse(buf) {
                let q = r.query.map { "?" + $0 } ?? ""
                let req = Req(method: r.method, path: r.path + q, headers: r.headers, body: r.body)
                lock.withLock { seen.append(req) }
                let resp = handler(req)
                var head = "HTTP/1.1 \(resp.status) X\r\nContent-Length: \(resp.body.utf8.count)\r\nConnection: close\r\n"
                for (k, v) in resp.headers { head += "\(k): \(v)\r\n" }
                var out = Data((head + "\r\n" + resp.body).utf8)
                while !out.isEmpty {
                    let n = out.withUnsafeBytes { write(c, $0.baseAddress, $0.count) }
                    if n <= 0 { return }
                    out = out.dropFirst(n)
                }
                return
            }
            let k = read(c, &tmp, tmp.count)
            if k <= 0 { return }
            buf += tmp[0..<k]
        }
    }
}

/// A gateway with one project (fakture, id 7) and its notes: the member's side of contracts-0.6.0 section 6, as far as the client sees it.
final class ProjectStore: @unchecked Sendable {
    struct Note { var id: Int64, cls: String, title: String, body: String, revision: Int64 }
    private let lock = NSLock()
    var notes: [Note] = []
    var next: Int64 = 1
    var mode = "ok"  // ok | noaccess | unavailable | ratelimited | forbidden | archived
    var keys: [String: Int64] = [:]

    func note(_ n: Note) -> String {
        #"{"id":\#(n.id),"project":7,"class":"\#(n.cls)","title":\#(GoJSON.string(n.title)),"body":\#(GoJSON.string(n.body)),"author":"ana","created":"2026-11-01T10:00:00Z","updated":"2026-11-02T08:30:00Z","revision":\#(n.revision)}"#
    }

    func handle(_ r: ProjectFakeGateway.Req) -> ProjectFakeGateway.Resp {
        lock.withLock {
            guard r.headers["authorization"] == "Bearer tok" else { return .init(status: 403, body: #"{"error":{"message":"No project access","type":"no_project_access"}}"#) }
            switch mode {
            case "noaccess": return .init(status: 403, body: #"{"error":{"message":"No project access","type":"no_project_access"}}"#)
            case "unavailable": return .init(status: 503, body: #"{"error":{"message":"The project database is not available","type":"core_unavailable"}}"#)
            case "ratelimited": return .init(status: 429, headers: ["Retry-After": "12"], body: #"{"error":{"type":"rate_limited","message":"slow down"}}"#)
            default: break
            }
            let path = r.path, base = "/minigw/v1/projects/7/notes"
            let json: (Int, String) -> ProjectFakeGateway.Resp = { .init(status: $0, body: $1) }
            let err: (Int, String, String) -> ProjectFakeGateway.Resp = { json($0, #"{"error":{"type":"\#($1)","message":"\#($2)"}}"#) }
            switch (r.method, path.split(separator: "?").first.map(String.init) ?? path) {
            case ("GET", "/minigw/v1/projects"):
                return json(200, #"[{"id":7,"slug":"fakture","name":"Fakture","archived":false,"role":"developer","granted":[]}]"#)
            case ("POST", "/minigw/v1/projects/fakture/link-check"):
                if mode == "forbidden" { return err(403, "forbidden", "no") }
                if mode == "archived" { return err(403, "archived", "no") }
                return json(200, #"{"id":7,"slug":"fakture"}"#)
            case ("POST", "/minigw/v1/projects/nope/link-check"): return err(404, "not_found", "no")
            case ("GET", base): return json(200, #"{"notes":[\#(notes.map { note($0) }.joined(separator: ","))]}"#)
            case ("POST", base):
                let b = (try? JSONSerialization.jsonObject(with: r.body) as? [String: Any]) ?? [:]
                let key = r.headers["idempotency-key"] ?? ""
                if let id = keys[key], let n = notes.first(where: { $0.id == id }) {  // the same key: the gateway replays the answer
                    return n.title == b["title"] as? String ? json(201, note(n)) : err(409, "idempotency_conflict", "x")
                }
                if mode == "forbidden" { return err(403, "forbidden", "no") }
                let n = Note(id: next, cls: b["class"] as? String ?? "", title: b["title"] as? String ?? "", body: b["body"] as? String ?? "", revision: 1)
                next += 1; notes.append(n); keys[key] = n.id
                return json(201, note(n))
            default:
                guard path.hasPrefix(base + "/"), let id = Int64(path.dropFirst(base.count + 1).split(separator: "?").first ?? ""), let i = notes.firstIndex(where: { $0.id == id })
                else { return err(404, "not_found", "x") }
                switch r.method {
                case "GET": return json(200, note(notes[i]))
                case "PUT":
                    let b = (try? JSONSerialization.jsonObject(with: r.body) as? [String: Any]) ?? [:]
                    guard (b["revision"] as? Int64 ?? Int64(b["revision"] as? Int ?? 0)) == notes[i].revision else {
                        return json(409, #"{"error":{"type":"revision_mismatch","message":"x","revision":\#(notes[i].revision)}}"#)
                    }
                    notes[i].cls = b["class"] as? String ?? ""; notes[i].title = b["title"] as? String ?? ""; notes[i].body = b["body"] as? String ?? ""
                    notes[i].revision += 1
                    return json(200, note(notes[i]))
                case "DELETE":
                    guard path.hasSuffix("revision=\(notes[i].revision)") else { return json(409, #"{"error":{"type":"revision_mismatch","message":"x","revision":\#(notes[i].revision)}}"#) }
                    notes.remove(at: i)
                    return json(204, "")
                default: return err(404, "not_found", "x")
                }
            }
        }
    }
}

@Suite(.serialized) struct ProjectAgainstAGateway {
    /// One member's session: a temp folder and link file, the real transport to the fake gateway.
    final class Session: @unchecked Sendable {
        let root: String, state: String, gw: ProjectFakeGateway, store: ProjectStore
        var stdin = ""
        init() throws {
            root = try tempRoot(); state = try tempRoot() + "/projects.json"
            try mkdir(root + "/proj/src")
            let s = ProjectStore(); store = s
            gw = ProjectFakeGateway { s.handle($0) }
        }
        deinit { gw.stop() }

        func run(_ args: [String], cwd: String = "proj", stdin: String = "") -> (out: String, err: String, error: String) {
            nonisolated(unsafe) var out = "", err = ""
            let url = gw.url
            let env = ProjectEnv(connect: { (ProjectConn(url: url, token: "tok"), "gw-test") }, state: state, cwd: root + "/" + cwd,
                                 stdin: { Data(stdin.utf8) }, out: { out += $0 }, err: { err += $0 }, agent: false,
                                 send: { c, m, p, h, b in ProjectHTTP.send(c, m, p, h, b, clientInfo: "macOS, minigw test", timeout: 20) }, sleep: { _ in })
            env.zone = TimeZone(identifier: "UTC")!
            do {
                switch args[0] {
                case "note": try env.note(Array(args.dropFirst()))
                case "usage": try env.usage(Array(args.dropFirst()))
                default: try env.project(Array(args.dropFirst()))
                }
            } catch { return (out, err, (error as? ProjectError)?.message ?? "\(error)") }
            return (out, err, "")
        }
    }

    @Test func linkPushListReadEditRemoveUnlink() throws {
        let s = try Session()
        #expect(s.run(["note", "list"]).error.contains("not linked"))
        #expect(s.run(["project", "list"]).out.contains("fakture") && s.run(["project", "list"]).out.contains("developer"))
        #expect(s.run(["project", "link", "fakture"]).out == "linked \(s.root)/proj to project fakture (id 7)\n")
        #expect(s.run(["project", "link", "fakture"]).error.contains("already linked"))
        #expect(s.run(["project", "show"], cwd: "proj/src").out.contains("fakture (id 7)"))
        #expect(s.run(["project", "list"]).out.contains("(linked here)"))

        let pushed = s.run(["note", "push", "--class", "structure", "-"], cwd: "proj/src", stdin: "# Numbering\nOne counter per year.\n")
        #expect(pushed.out == "note 1 created (structure, revision 1)\n", "\(pushed)")
        #expect(s.run(["note", "list"]).out.contains("Numbering"))
        #expect(s.run(["note", "read", "1"]).out.contains("One counter per year."))
        // the project id of the link is in the path, and the client and the device headers are sent
        #expect(s.gw.requests.contains { $0.path == "/minigw/v1/projects/7/notes" && $0.method == "POST" && $0.headers["x-minigw-client"] == "macOS, minigw test" && $0.headers["idempotency-key"]?.hasPrefix("mgw-") == true })

        let stale = s.run(["note", "edit", "1", "--revision", "9", "--class", "data", "--title", "t", "-"], stdin: "x")
        #expect(stale.error == "the note has changed: its current revision is 1 (read it again, then retry)")  // 409
        #expect(s.run(["note", "edit", "1", "--title", "Numbering v2"]).out == "note 1 updated (structure, revision 2)\n")
        #expect(s.run(["note", "rm", "1", "--revision", "1"]).error.contains("current revision is 2"))
        #expect(s.run(["note", "rm", "1"]).out == "note 1 deleted\n")
        let gone = s.run(["note", "read", "1"]).error  // 404: the server's text first, then the client's hint about other projects and teams
        #expect(gone.hasPrefix("not found or not visible") && gone.contains("--team"))
        #expect(s.run(["project", "unlink"], cwd: "proj/src").out == "unlinked \(s.root)/proj from project fakture\n")
        #expect(s.run(["note", "list"]).error.contains("not linked"))
    }

    @Test func gatewayAnswersBecomeSentences() throws {
        let s = try Session()
        #expect(s.run(["project", "link", "nope"]).error == "not found or not visible")  // 404
        #expect(s.run(["project", "link", "fakture"]).out.contains("linked"))
        for (mode, cmd, want) in [("noaccess", ["note", "list"], "no project access: this device is not approved or not bound to a user on the gateway (ask the gateway owner)"),
                                  ("unavailable", ["note", "list"], "the gateway's project store is not available"),
                                  ("unavailable", ["project", "list"], "the gateway's project store is not available"),
                                  ("ratelimited", ["note", "list"], "too many requests: try again in 12 seconds"),
                                  ("forbidden", ["note", "push", "x.md"], "not allowed: your role in this project does not permit it (a grant alone only lets you read; linking needs a direct role)")] {
            s.store.mode = mode
            try Data("body\n".utf8).write(to: URL(fileURLWithPath: s.root + "/x.md"))
            let r = s.run(cmd.map { $0 == "x.md" ? s.root + "/x.md" : $0 })
            #expect(r.error == want && r.out.isEmpty, "\(mode) \(cmd): \(r)")
        }
        s.store.mode = "archived"
        try FileManager.default.removeItem(atPath: s.state)
        #expect(s.run(["project", "link", "fakture"]).error == "the project is archived: nobody can write to it")
        s.store.mode = "forbidden"
        #expect(s.run(["project", "link", "fakture"]).error.hasPrefix("not allowed:"))
        #expect((try Projects.load(s.state)).links.isEmpty, "a refused link is not saved")
    }

    @Test func noAnswerIsReported() throws {
        let s = try Session()
        let dead = ProjectFakeGateway { _ in .init(status: 200) }
        let url = dead.url
        dead.stop()  // nothing listens here any more
        var out = ""
        let env = ProjectEnv(connect: { (ProjectConn(url: url, token: "tok"), "g") }, state: s.state, cwd: s.root + "/proj", stdin: { Data() }, out: { out += $0 }, err: { _ in },
                             agent: false, send: { c, m, p, h, b in ProjectHTTP.send(c, m, p, h, b, clientInfo: "x", timeout: 5) }, sleep: { _ in })
        #expect(throws: ProjectError.self) { try env.project(["list"]) }
        do { try env.project(["list"]) } catch { #expect((error as! ProjectError).message == "no answer from the gateway") }
    }

    @Test func aLostAnswerIsRetriedWithTheSameKey() throws {
        let s = try Session()
        #expect(s.run(["project", "link", "fakture"]).out.contains("linked"))
        nonisolated(unsafe) var posts = 0
        nonisolated(unsafe) var keys = Set<String>()
        nonisolated(unsafe) var out = ""
        let url = s.gw.url
        let env = ProjectEnv(connect: { (ProjectConn(url: url, token: "tok"), "gw-test") }, state: s.state, cwd: s.root + "/proj", stdin: { Data("lost answer\n".utf8) },
                             out: { out += $0 }, err: { _ in }, agent: false, send: { c, m, p, h, b in
                                 let r = ProjectHTTP.send(c, m, p, h, b, clientInfo: "x", timeout: 20)
                                 guard m == "POST" else { return r }
                                 posts += 1; keys.insert(h["Idempotency-Key"] ?? "")
                                 return posts <= 2 ? ProjectReply(status: 0) : r  // the gateway created the note, the answer never arrived (twice)
                             }, sleep: { _ in })
        try env.note(["push", "-"])
        #expect(posts == 3 && keys.count == 1 && out.contains("created"), "posts \(posts) keys \(keys) out \(out)")
        #expect(s.store.notes.count == 1, "the retry must not create a duplicate")
    }

    @Test func outputOfTheRealTransportFollowsTheReplyAsItComes() throws {
        let s = try Session()
        let r = ProjectHTTP.send(ProjectConn(url: s.gw.url, token: "tok"), "GET", "/minigw/v1/projects", [:], nil, clientInfo: "x")
        #expect(r.status == 200 && String(decoding: r.data, as: UTF8.self).contains("fakture") && r.retryAfter == nil)
        let rr = ProjectHTTP.send(ProjectConn(url: s.gw.url, token: "wrong"), "GET", "/minigw/v1/projects", [:], nil, clientInfo: "x")
        #expect(rr.status == 403)
    }

    @Test func aRedirectIsNeverFollowed() throws {
        let other = ProjectFakeGateway { _ in .init(status: 200, body: "elsewhere") }
        defer { other.stop() }
        let gw = ProjectFakeGateway { _ in .init(status: 302, headers: ["Location": other.url + "/x"]) }
        defer { gw.stop() }
        let r = ProjectHTTP.send(ProjectConn(url: gw.url, token: "tok"), "GET", "/minigw/v1/projects", [:], nil, clientInfo: "x")
        #expect(r.status == 302 && other.requests.isEmpty, "the token must not follow a redirect")
    }

    @Test func usageThroughTheRealTransport() throws {
        let gw = ProjectFakeGateway { r in
            r.path == "/minigw/v1/usage?days=7&project=fakture"
                ? .init(status: 200, body: #"{"days":[{"day":"2026-11-13","model":"m","events":2,"tokensIn":3,"tokensOut":4,"eventsWithoutTokens":0,"nestedEvents":0,"failedEvents":0}],"gaps":[]}"#)
                : .init(status: 404, body: #"{"error":{"type":"not_found","message":"x"}}"#)
        }
        defer { gw.stop() }
        nonisolated(unsafe) var out = ""
        let env = ProjectEnv(connect: { (ProjectConn(url: gw.url, token: "tok"), "g") }, state: try tempRoot() + "/p.json", cwd: try tempRoot(), stdin: { Data() },
                             out: { out += $0 }, err: { _ in }, agent: false, send: { c, m, p, h, b in ProjectHTTP.send(c, m, p, h, b, clientInfo: "x") })
        try env.usage(["--project", "fakture"])
        #expect(out == "project fakture\n2026-11-13  m                         2 events, 3 in, 4 out\ntotal: 2 events, 3 in, 4 out (every event once, nested ones included)\n")
        #expect(throws: ProjectError.self) { try env.usage(["--project", "other"]) }
    }
}

// MARK: the local proxy sets the project header and drops a forged one (through the real NoiseProxy)

private final class Captured: @unchecked Sendable {
    private let lock = NSLock()
    private var d = Data()
    func add(_ x: Data) { lock.withLock { d.append(x) } }
    var text: String { lock.withLock { String(decoding: d, as: UTF8.self) } }
}

@Suite(.serialized) struct ProjectProxyHeader {
    /// A fake router (Noise responder) that reads one request head from the tunnel; the proxy runs for `project`; a request with the
    /// forged header `forged` goes in through the proxy's local port.
    func headSeen(project: String, forged: [String]) async throws -> String {
        let key = Noise.newKey()
        let s = socket(AF_INET, SOCK_STREAM, 0)
        var a = sockaddr_in()
        a.sin_family = sa_family_t(AF_INET); a.sin_addr.s_addr = inet_addr("127.0.0.1")
        _ = withUnsafePointer(to: &a) { $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { bind(s, $0, socklen_t(MemoryLayout<sockaddr_in>.size)) } }
        listen(s, 4)
        var l = socklen_t(MemoryLayout<sockaddr_in>.size)
        _ = withUnsafeMutablePointer(to: &a) { $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { getsockname(s, $0, &l) } }
        let routerPort = UInt16(bigEndian: a.sin_port)
        defer { close(s) }
        let got = Captured()
        Thread {
            let c = accept(s, nil, nil)
            defer { close(c) }
            var buf = [UInt8](repeating: 0, count: 65536)
            var hs = Data()
            while hs.count < 4 + Noise.handshakeLength {
                let k = read(c, &buf, min(buf.count, 4 + Noise.handshakeLength - hs.count))
                if k <= 0 { return }
                hs += buf[0..<k]
            }
            guard let (reply, session) = try? Noise.respond(Data(hs.dropFirst(4)), key: key) else { return }
            _ = reply.withUnsafeBytes { write(c, $0.baseAddress, $0.count) }
            var raw = Data(), plain = Data()
            while !String(decoding: plain, as: UTF8.self).contains("\r\n\r\n") {
                let k = read(c, &buf, buf.count)
                if k <= 0 { break }
                raw += buf[0..<k]
                if let p = try? Noise.unframe(&raw, session.receive) { plain += p }
            }
            got.add(plain)
        }.start()
        let proxy = try #require(NoiseProxy(routerURL: "http://127.0.0.1:\(routerPort)", routerKey: key.publicKey.rawRepresentation))
        proxy.project = project
        try proxy.start()
        defer { proxy.stop() }
        let fd = socket(AF_INET, SOCK_STREAM, 0)
        defer { close(fd) }
        var pa = sockaddr_in()
        pa.sin_family = sa_family_t(AF_INET); pa.sin_addr.s_addr = inet_addr("127.0.0.1"); pa.sin_port = proxy.localPort.bigEndian
        let rc = withUnsafePointer(to: &pa) { $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { connect(fd, $0, socklen_t(MemoryLayout<sockaddr_in>.size)) } }
        try #require(rc == 0)
        let req = "POST /v1/messages HTTP/1.1\r\nHost: x\r\n" + forged.map { $0 + "\r\n" }.joined() + "Content-Length: 2\r\n\r\nhi"
        // in two writes, so that the head arrives in pieces
        let bytes = Array(req.utf8)
        _ = write(fd, bytes, 20)
        try await Task.sleep(for: .milliseconds(50))
        _ = write(fd, Array(bytes.dropFirst(20)), bytes.count - 20)
        for _ in 0..<200 where !got.text.contains("\r\n\r\n") { try await Task.sleep(for: .milliseconds(50)) }
        return (got.text.components(separatedBy: "\r\n\r\n").first ?? "") + "\r\n\r\n"  // the head; the body may come in the same frame
    }

    @Test func projectIsSetAndAForgedHeaderDropped() async throws {
        let seen = try await headSeen(project: "42", forged: ["X-Minigw-Project: 9", "x-MINIGW-project: 8"])
        #expect(seen == "POST /v1/messages HTTP/1.1\r\nX-Minigw-Project: 42\r\nHost: x\r\nContent-Length: 2\r\n\r\n", "\(seen.debugDescription)")
    }

    @Test func aHeadThatCannotBeRewrittenSafelyReachesNobody() async throws {  // F1: the late chunk carries a forged header with a space before the colon
        let seen = try await headSeen(project: "42", forged: ["x-MINIGW-project : 8"])
        #expect(seen == "\r\n\r\n", "\(seen.debugDescription)")
    }

    @Test func noProjectMeansNoHeaderEvenIfTheProgramSendsOne() async throws {
        let seen = try await headSeen(project: "", forged: ["X-Minigw-Project: 9"])
        #expect(seen == "POST /v1/messages HTTP/1.1\r\nHost: x\r\nContent-Length: 2\r\n\r\n", "\(seen.debugDescription)")
    }
}

// MARK: git identity of a project (the cases through the shared fixtures are in ProjectCommandParity and KBCommandParity)

@Suite struct ProjectIdentity {
    @Test func credentialsAreRemovedFromUrlRemotesOnly() {
        for (input, want) in [
            ("https://tok:secret@host.example/a/b.git", "https://host.example/a/b.git"),
            ("ssh://git@host.example:22/a/b", "ssh://host.example:22/a/b"),
            ("git@host.example:a/b.git", "git@host.example:a/b.git"),
            ("https://host.example/a@b/c", "https://host.example/a@b/c"),
            ("  https://u@h.example/a/b \n", "https://h.example/a/b"),
            ("/srv/git/a@b", "/srv/git/a@b"),
            ("https://u:p@ss@host.example/a/b", "https://host.example/a/b"),
        ] { #expect(ProjectEnv.stripUserinfo(input) == want, "\(input)") }
    }

    private func git(_ dir: String, _ args: [String]) throws -> Bool {
        let p = Process()
        p.executableURL = URL(fileURLWithPath: "/usr/bin/env")
        p.arguments = ["git"] + args
        p.currentDirectoryURL = URL(fileURLWithPath: dir)
        var env = ProcessInfo.processInfo.environment
        env["HOME"] = dir; env["GIT_CONFIG_NOSYSTEM"] = "1"
        p.environment = env
        p.standardOutput = FileHandle.nullDevice; p.standardError = FileHandle.nullDevice
        try p.run(); p.waitUntilExit()
        return p.terminationStatus == 0
    }

    @Test func theOriginIsTheRawConfigValue() throws {
        let dir = try tempRoot()
        guard try git(dir, ["init", "-q"]) else { return }  // no git: nothing to prove here
        #expect(ProjectEnv.gitOrigin(dir) == "")
        #expect(try git(dir, ["remote", "add", "origin", "https://tok:secret@Host.example/Eureko/Fakture.git"]))
        #expect(ProjectEnv.gitOrigin(dir) == "https://tok:secret@Host.example/Eureko/Fakture.git")
        // an insteadOf rule does not rewrite what the folder says
        #expect(try git(dir, ["config", "--local", "url.git@other.example:.insteadOf", "https://tok:secret@Host.example/"]))
        #expect(ProjectEnv.gitOrigin(dir) == "https://tok:secret@Host.example/Eureko/Fakture.git")
        try mkdir(dir + "/src")
        #expect(ProjectEnv.gitOrigin(dir + "/src") != "")
        #expect(ProjectEnv.gitOrigin(try tempRoot()) == "")
    }

    @Test func gitIsNeverTakenFromTheFolder() throws {
        let dir = try tempRoot()
        guard try git(dir, ["init", "-q"]), try git(dir, ["remote", "add", "origin", "https://host.example/a/b"]) else { return }
        let marker = dir + "/ran-fake-git"
        try Data("#!/bin/sh\ntouch '\(marker)'\necho evil\n".utf8).write(to: URL(fileURLWithPath: dir + "/git"))
        chmod(dir + "/git", 0o755)
        // A relative entry that would reach the fake git if it were resolved against this process's current directory (which stays untouched).
        let here = FileManager.default.currentDirectoryPath.split(separator: "/"), there = dir.split(separator: "/")
        let common = zip(here, there).prefix { $0 == $1 }.count
        let toDir = (Array(repeating: "..", count: here.count - common) + there.dropFirst(common)).joined(separator: "/")
        // The git that this machine has on its absolute PATH entries (the Xcode shim /usr/bin/git on a runner, Homebrew's on others): nothing here assumes a path
        let realGit = try #require(Profiles.executable("git", path: ProcessInfo.processInfo.environment["PATH"] ?? ""), "no git on an absolute PATH entry")
        let realDir = (realGit as NSString).deletingLastPathComponent
        let path = ".:\(toDir):relative/bin:" + realDir
        #expect(Profiles.executable("git", path: path) == realGit, "a relative PATH entry was resolved")
        // 120 s: a cold git shim on a loaded runner was seen to need more than the default 10 s; what is asserted is which git runs, not how fast
        #expect(ProjectEnv.gitOrigin(dir, path: path, timeout: 120) == "https://host.example/a/b")
        #expect(!FileManager.default.fileExists(atPath: marker), "the folder's own git ran")
        // only relative entries: no git at all is looked up from them (the fallback is the system's), and the folder's own git still never runs
        _ = ProjectEnv.gitOrigin(dir, path: ".:\(toDir):relative/bin", timeout: 120)
        #expect(!FileManager.default.fileExists(atPath: marker), "the folder's own git ran for a PATH of relative entries")
    }

    @Test func kindAndFaqArguments() throws {
        func env(agent: Bool = false) -> ProjectEnv {
            ProjectEnv(connect: { (ProjectConn(url: "", token: ""), "") }, state: "", cwd: "", stdin: { Data() }, out: { _ in }, err: { _ in }, agent: agent, send: { _, _, _, _, _ in ProjectReply(status: 599) })
        }
        #expect(throws: ProjectError.self) { try env().kb(["submit", "fakture", "--kind", "rumor", "-"]) }
        #expect(throws: ProjectError.self) { try env().kb(["faq", "a", "b"]) }
        do { try env(agent: true).kb(["faq"]); Issue.record("agent allowed") } catch { #expect((error as? ProjectError)?.message == ProjectEnv.agentRefusal) }
    }
}
