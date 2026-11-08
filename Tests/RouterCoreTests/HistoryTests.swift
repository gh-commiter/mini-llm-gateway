import Foundation
import Testing
@testable import RouterCore

/// Priklop = prva zahteva po 10 minutah tišine; največ 20; odstranjena naprava izgubi zgodovino, njene seje ostanejo.
@Test func deviceHistoryAndSessions() throws {
    let dir = FileManager.default.temporaryDirectory.appendingPathComponent("hist-" + UUID().uuidString)
    defer { try? FileManager.default.removeItem(at: dir) }
    let file = dir.appendingPathComponent("state.json")
    let g = GatewayHistory()
    g.load(file)
    let t0 = Date(timeIntervalSince1970: 1_793_000_000)
    g.seen("A", address: "192.0.2.1", client: "Linux, minigw 0.5.18", now: t0)
    g.seen("A", address: "192.0.2.1", client: "", now: t0 + 60)
    g.seen("A", address: "192.0.2.2", client: "", now: t0 + 720)
    let h = g.history("A")
    #expect(h.count == 2 && h[0].address == "192.0.2.2" && h[0].client == "Linux, minigw 0.5.18" && g.lastSeen("A") == t0 + 720)
    for i in 0..<30 { g.seen("A", address: "192.0.2.3", client: "", now: t0 + Double(i + 2) * 3600) }
    #expect(g.history("A").count == GatewayHistory.historyPerDevice)

    func use(_ session: String, _ device: String, _ model: String, _ status: Int, _ at: Date) {
        var u = GatewayHistory.Use()
        u.session = session; u.device = device; u.deviceName = "PC"; u.folder = "/Users/u/p"; u.mode = "sync"; u.model = model
        u.status = status; u.took = 2; u.input = 10; u.output = 5
        g.record(u, now: at)
    }
    use("s1", "A", "claude-codex", 200, t0)
    use("s1", "A", "claude-codex", 500, t0 + 60)
    use("s1", "A", "claude-sonnet", 403, t0 + 120)
    use("", "A", "claude-haiku", 200, t0)
    use("", "A", "claude-haiku", 200, t0 + 300)   // isto okno
    use("", "A", "claude-haiku", 200, t0 + 1800)  // novo okno
    #expect(g.sessions().count == 3)
    let s1 = try #require(g.sessions(device: "A", folder: "/Users/u").first { $0.id == "s1" })
    let m = try #require(useOf(s1, "claude-codex"))
    #expect(m.requests == 2 && m.errors == 1 && m.seconds == 4 && m.inputTokens == 20 && m.outputTokens == 10)
    #expect(useOf(s1, "claude-sonnet")?.refused == 1 && s1.last.timeIntervalSince(s1.start) == 120 && s1.mode == "sync")
    #expect(g.sessions(device: "B").isEmpty && g.sessions(folder: "/Users/u/other").isEmpty)
    let json = GatewayHistory.json(s1)
    #expect(json["duration"] as? Double == 120 && json["deviceName"] as? String == "PC" && json["branch"] == nil)

    g.prune(allowed: ["B"])
    #expect(g.history("A").isEmpty && g.lastSeen("A") == nil && g.sessions(device: "A").allSatisfy(\.deviceRemoved))
    for i in 0..<(GatewayHistory.maxSessions + 5) { use("x\(i)", "B", "m", 200, t0) }
    #expect(g.sessions(device: "B").count == GatewayHistory.maxDeviceSessions && g.sessions().count == 53)  // 3 naprave A + 50 naprave B
    g.save()
    let perm = try FileManager.default.attributesOfItem(atPath: file.path)[.posixPermissions] as? Int
    #expect(perm == 0o600)
    let g2 = GatewayHistory()
    g2.load(file)
    #expect(g2.sessions().count == 53)
    g.clearSessions()
    #expect(g.sessions().isEmpty)
}

/// Žetoni iz odgovora (SSE), tudi ko meja zapisa razdeli število.
@Test func usageWriterCountsTokens() async throws {
    let u = UsageWriter(FakeWriter())
    for p in [#"data: {"type":"message_start","message":{"usage":{"input_tokens":12"#, #"34,"cache_read_input_tokens":999,"output_tokens":1}}}"#,
              "\n\ndata: {\"usage\":{\"output_tok", #"ens": 56}}"#] {
        try await u.write(Data(p.utf8))
    }
    #expect(u.usage.input == 1234 && u.usage.output == 56)
}

/// Skrbniški API: zgodovina dovoljene naprave, seje, brisanje (oblika kot v minigw serve).
@Test func sessionsAdminAPI() async throws {
    let id = String(repeating: "A", count: 43)
    var c = RouterConfig()
    c.devices = [DeviceConfig(id: id, name: "PC")]
    let r = FakeApp(c).router()
    Router.history.seen(id, address: "192.0.2.7", client: "Windows, minigw 0.5.18")
    let session = "hist-" + UUID().uuidString
    var u = GatewayHistory.Use()
    u.session = session; u.device = id; u.deviceName = "PC"; u.folder = #"C:\p"#; u.mode = "worktree"; u.branch = "minigw/claude-1"
    u.model = "claude-sonnet"; u.input = 3; u.output = 4
    Router.history.record(u)
    let h = await admin(r, "GET", "/devices/\(id)/history")
    #expect(h.status == 200 && h.text.contains(#""address":"192.0.2.7""#) && h.text.contains(#""client":"Windows, minigw 0.5.18""#), "\(h.text)")
    #expect(await admin(r, "GET", "/devices/nope/history").status == 404)
    let one = await admin(r, "GET", "/sessions/\(session)")
    #expect(one.status == 200 && one.text.contains(#""branch":"minigw/claude-1""#) && one.text.contains(#""inputTokens":3"#), "\(one.text)")
    let list = await admin(r, "GET", "/sessions?device=\(id)&folder=C%3A%5Cp")
    #expect(list.status == 200 && list.text.contains(session))
}

func useOf(_ s: GatewayHistory.Session, _ model: String, role: String = "") -> GatewayHistory.ModelUse? {
    s.models.first { $0.model == model && $0.role == role }
}

/// Mapa, vloga, omejitve in ID seje; enaki primeri kot TestFolderRoleAndRestrictions v Go.
@Test func folderRoleAndRestrictions() throws {
    for (i, want) in [#"C:\Users\u\p\"#: "C:/Users/u/p", "/Users/u/p/./a/..": "/Users/u/p", "/": "/", "rel//x/": "rel/x", "": "",
                      #"\\srv\share\x\"#: "//srv/share/x", #"/home/u/a\b/"#: #"/home/u/a\b"#, "c:": "c:"] {
        #expect(GatewayHistory.folder(i) == want, "\(i)")
    }
    for (r, ok) in ["dev": true, "sec.review-1_x": true, "": false, "a b": false, "x\nx-evil: 1": false, String(repeating: "a", count: 33): false, "dév": false] {
        #expect(GatewayHistory.validRole(r) == ok, "\(r)")
    }
    let g = GatewayHistory()
    for role in ["security", "dev", "dev", ""] {
        var u = GatewayHistory.Use()
        u.session = "s"; u.folder = #"C:\Proj\"#; u.os = "Windows"; u.model = "claude-codex"; u.role = role; u.tools = "write"; u.readOnly = true
        g.record(u)
    }
    let s = try #require(g.sessions(folder: "c:/proj").first)  // Windows: brez razlike v velikih črkah
    #expect(g.sessions(folder: "c:/proj").count == 1 && s.folder == "C:/Proj" && s.role == "dev")
    #expect(useOf(s, "claude-codex", role: "dev")?.requests == 2 && useOf(s, "claude-codex", role: "security")?.requests == 1
            && useOf(s, "claude-codex")?.requests == 1 && useOf(s, "claude-codex", role: "dev")?.readOnly == true && useOf(s, "claude-codex", role: "dev")?.tools == "write")
    let json = GatewayHistory.json(s)
    #expect(json["role"] as? String == "dev" && (json["models"] as? [[String: Any]])?.first?["model"] as? String == "claude-codex")
    var l = GatewayHistory.Use()
    l.session = "l"; l.folder = "/home/U"; l.os = "Linux"; l.model = "m"
    g.record(l)
    #expect(g.sessions(folder: "/home/u").isEmpty && g.sessions(folder: "/home/U").count == 1)  // Linux: z velikimi črkami
    for bad in [String(repeating: "x", count: 129), "a\u{1b}[31m"] {  // M2: brez zapisa
        var b = GatewayHistory.Use()
        b.session = bad; b.model = "m"
        g.record(b)
    }
    #expect(g.sessions().count == 2)
    var c = RouterConfig()
    c.claudeCLI.tools = "Read,Grep"; c.codex.write = true; c.codex.network = false; c.grok.write = true; c.copilot.network = true
    for (model, want) in ["claude-cli": "Read,Grep", "claude-cli-opus": "Read,Grep", "claude-codex": "write", "claude-codex-gpt-5": "write",
                          "claude-grok": "write", "claude-copilot": "network", "claude-sonnet": ""] {
        #expect(GatewayHistory.restrictions(model, c) == want, "\(model)")
    }
    c.claudeCLI.allowLocalTools = true
    #expect(GatewayHistory.restrictions("claude-cli", c) == "local")
}

/// Ugnezdeni klic (agent-calls): v seji klicatelja pod isto napravo, z »via« = veriga izvajanja in vlogo klicatelja
/// (glavi vprašanega agenta ne veljata).
@Test func nestedCallLandsInCallersSession() async throws {
    let id = String(repeating: "N", count: 43), sid = "nested-" + UUID().uuidString
    var c = RouterConfig()
    c.claudeCLI.askOthers = true; c.claudeCLI.allowLocalTools = true
    c.devices = [DeviceConfig(id: id, name: "PC")]
    let router = Router(config: c, token: "tok", secrets: FakeSecrets(values: [:]))
    var caller = request("POST", "/v1/messages", headers: ["x-claude-mini-session": sid, "x-claude-mini-cwd": "/client/dir", "x-claude-mini-role": "dev"])
    caller.peer = "10.0.0.9"
    caller.device = Noise.Device(id: id, name: "PC")
    let run = try #require(await router.startAgentRun(caller, kind: "claude", dir: nil))
    defer { run.stop() }
    run.started(getpid())
    var r = URLRequest(url: URL(string: "http://127.0.0.1:\(run.port)/v1/messages")!)
    r.httpMethod = "POST"
    r.httpBody = Data(#"{"model":"claude-nope","max_tokens":5,"messages":[{"role":"user","content":"hi"}]}"#.utf8)
    for (k, v) in ["Authorization": "Bearer " + run.token, "x-claude-mini-session": "forged", "x-claude-mini-role": "security",
                   "x-minigw-agent-chain": "codex"] { r.setValue(v, forHTTPHeaderField: k) }
    _ = try? await URLSession(configuration: .ephemeral).data(for: r)
    let s = try #require(Router.history.sessions(device: id).first { $0.id == sid })
    #expect(s.role == "dev" && s.folder == "/client/dir" && s.models.count == 1)
    #expect(s.models.first?.via == "claude" && s.models.first?.role == "dev" && s.models.first?.model == "unknown")
    #expect(Router.history.sessions().allSatisfy { $0.id != "forged" })
}

/// --minigw-role: oblike, prednost pred MINIGW_ROLE, neveljavno ime; enaki primeri kot TestTakeRole v Go.
@Test func takeRoleParsing() {
    let cases: [([String], String?, String, String, Bool)] = [
        (["-p", "x"], nil, "", "-p x", true),
        (["--minigw-role", "dev", "-p", "x"], nil, "dev", "-p x", true),
        (["-p", "--minigw-role=security"], "dev", "security", "-p", true),
        (["-p"], "dev", "dev", "-p", true),
        (["--minigw-role", "a b"], nil, "a b", "", false),
        (["-p", "--minigw-role"], nil, "", "-p --minigw-role", true),  // brez vrednosti: gre naprej v claude
    ]
    for (args, env, role, rest, ok) in cases {
        let r = GatewayHistory.takeRole(args, env: env)
        #expect(r.role == role && r.rest.joined(separator: " ") == rest && r.ok == ok, "\(args)")
    }
}

/// M1: najslabši primer ostane pod mejo in se prebere; M2: naprava ima največ 50 sej in ne izrine drugih; L3, L4.
@Test func stateLimits() throws {
    let dir = FileManager.default.temporaryDirectory.appendingPathComponent("limits-" + UUID().uuidString)
    defer { try? FileManager.default.removeItem(at: dir) }
    try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
    let file = dir.appendingPathComponent("state.json")
    let g = GatewayHistory()
    g.load(file)
    func rec(_ session: String, _ device: String, model: String = "m", folder: String = "", role: String = "", via: String = "", branch: String = "",
             tools: String = "") {
        var u = GatewayHistory.Use()
        u.session = session; u.device = device; u.model = model; u.folder = folder; u.role = role; u.via = via; u.branch = branch; u.tools = tools
        g.record(u)
    }
    rec("keep", "B")
    for i in 0..<120 { rec("x\(i)", "D0") }
    #expect(g.sessions(device: "D0").count == GatewayHistory.maxDeviceSessions && g.sessions(device: "B").count == 1)
    let wide = String(repeating: "/", count: 600)  // JSONEncoder zapiše »\/«: dvojna dolžina
    for i in 0..<200 {
        for j in 0..<(GatewayHistory.maxSessionModels + 2) {
            rec("s\(i)", "D\(i % 4)", model: "\(j)" + wide, folder: String(repeating: "/a", count: 600), role: String(repeating: "r", count: 40),
                via: wide, branch: wide, tools: wide)
        }
    }
    #expect(g.sessions().count == GatewayHistory.maxSessions)
    let x = try #require(g.sessions(device: "D1").first)
    #expect(x.folder.utf8.count <= 1024 && x.models[0].model.utf8.count <= 64 && x.models[0].role.utf8.count <= 32 && x.branch.utf8.count <= 255)
    g.seen("D1", address: "a", client: "c")  // označi spremembo
    g.save()
    let size = try FileManager.default.attributesOfItem(atPath: file.path)[.size] as? Int ?? 0
    #expect(size > 0 && size <= GatewayHistory.maxFile)
    #expect(g.sessions().count < GatewayHistory.maxSessions)  // brez obrezovanja bi bila datoteka večja od 4 MB
    let g2 = GatewayHistory()
    g2.load(file)
    #expect(!g2.sessions().isEmpty)
    #expect(GatewayHistory.clip("a\u{85}b\u{202e}c\u{2066}d\u{9b}e", 64) == "abcde" && GatewayHistory.clip("žžž", 5) == "žž")
    // L3: povezava namesto datoteke se ne bere
    try FileManager.default.removeItem(at: file)
    try FileManager.default.createSymbolicLink(atPath: file.path, withDestinationPath: "/dev/zero")
    let g3 = GatewayHistory()
    g3.load(file)
    #expect(g3.sessions().isEmpty)
}
