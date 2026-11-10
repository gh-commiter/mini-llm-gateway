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
    #expect(GatewayHistory.role(request("POST", "/v1/messages", headers: ["x-claude-mini-role": String(repeating: "a", count: 33)])) == ""
            && GatewayHistory.role(request("POST", "/v1/messages", headers: ["x-claude-mini-role": "dev"])) == "dev")
    let g = GatewayHistory()
    for role in ["security", "dev", "dev", ""] {
        var u = GatewayHistory.Use()
        u.session = "s"; u.folder = #"C:\Proj\"#; u.os = "Windows"; u.model = "claude-codex"; u.role = role; u.tools = "write"; u.readOnly = true
        g.record(u)
    }
    let s = try #require(g.sessions(folder: "c:/proj").first)  // Windows: brez razlike v velikih črkah
    #expect(g.sessions(folder: "c:/proj").count == 1 && s.folder == "C:/Proj" && s.role == "")
    #expect(useOf(s, "claude-codex", role: "dev")?.requests == 2 && useOf(s, "claude-codex", role: "security")?.requests == 1
            && useOf(s, "claude-codex")?.requests == 1 && useOf(s, "claude-codex", role: "dev")?.readOnly == true && useOf(s, "claude-codex", role: "dev")?.tools == "write")
    let json = GatewayHistory.json(s)
    #expect(json["role"] == nil && (json["models"] as? [[String: Any]])?.first?["model"] as? String == "claude-codex")
    var l = GatewayHistory.Use()
    l.session = "l"; l.folder = "/home/U"; l.os = "Linux"; l.model = "m"
    g.record(l)
    #expect(g.sessions(folder: "/home/u").isEmpty && g.sessions(folder: "/home/U").count == 1)  // Linux: z velikimi črkami
    for bad in [String(repeating: "x", count: 129), "a\u{1b}[31m"] {  // M2: ID ni zapisan; L5: zahteva šteje v seji »t-…«
        var b = GatewayHistory.Use()
        b.session = bad; b.model = "m"
        g.record(b)
    }
    #expect(g.sessions().count == 3 && g.sessions().contains { $0.id.hasPrefix("t-") && $0.models.first?.requests == 2 })
    var c = RouterConfig()
    c.claudeCLI.tools = "Read,Grep"; c.codex.write = true; c.codex.network = false; c.grok.write = true; c.copilot.network = true
    for (model, want) in ["claude-cli": "Read,Grep", "claude-cli-opus": "Read,Grep", "claude-codex": "write", "claude-codex-gpt-5": "write",
                          "claude-grok": "write", "claude-copilot": "network", "claude-sonnet": ""] {
        #expect(GatewayHistory.restrictions(model, c) == want, "\(model)")
    }
    c.copilot.write = true; c.copilot.commands = true; c.claudeCLI.tools = "Read,Edit,Grep"
    for (model, want) in ["claude-cli": "Read,Grep", "claude-codex": "", "claude-grok": "", "claude-copilot": "network"] {  // --minigw-ro
        #expect(GatewayHistory.restrictions(model, c, readOnly: true) == want, "\(model)")
    }
    c.claudeCLI.allowLocalTools = true
    #expect(GatewayHistory.restrictions("claude-cli", c) == "local")
    #expect(GatewayHistory.restrictions("claude-cli", c, readOnly: true) == "local,read-only")
    c.codex.network = true  // omrežje Codex le s pisanjem; v worktree le z worktreeCodexNetwork
    #expect(GatewayHistory.restrictions("claude-codex", c) == "write,network" && GatewayHistory.restrictions("claude-codex", c, readOnly: true) == "")
    #expect(GatewayHistory.restrictions("claude-codex", c, tree: true) == "write")
    c.worktreeCodexNetwork = true
    #expect(GatewayHistory.restrictions("claude-codex", c, tree: true) == "write,network")
}

/// Ugnezdeni klic (agent-calls): v seji klicatelja pod isto napravo, z »via« = veriga izvajanja in vlogo klicatelja
/// (glavi vprašanega agenta ne veljata).
@Test func nestedCallLandsInCallersSession() async throws {
    let id = String(repeating: "N", count: 43), sid = "nested-" + UUID().uuidString
    var c = RouterConfig()
    c.claudeCLI.askOthers = true; c.claudeCLI.allowLocalTools = true
    c.devices = [DeviceConfig(id: id, name: "PC")]
    let history = GatewayHistory()  // svoja zgodovina: vzporedni testi ne vplivajo na skupno Router.history
    let router = Router(config: c, token: "tok", secrets: FakeSecrets(values: [:]), history: history)
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
    let (body, resp) = try await URLSession(configuration: .ephemeral).data(for: r)
    // Zapis seje sledi odgovoru (odjemalec ima celoten odgovor že pred njim): počakaj nanj največ 5 s.
    for _ in 0..<100 where history.sessions(device: id).isEmpty { try await Task.sleep(for: .milliseconds(50)) }
    let s = try #require(history.sessions(device: id).first { $0.id == sid },
                         "\((resp as? HTTPURLResponse)?.statusCode ?? 0) \(String(decoding: body, as: UTF8.self))")
    #expect(s.role == "dev" && s.folder == "/client/dir" && s.models.count == 1)
    #expect(s.models.first?.via == "claude" && s.models.first?.role == "dev" && s.models.first?.model == "unknown")
    #expect(history.sessions().count == 1)  // ne »forged«
}

/// --minigw-role: oblike, prednost pred MINIGW_ROLE, neveljavno ime; enaki primeri kot TestTakeRole v Go.
@Test func takeRoleParsing() {
    let cases: [([String], String?, String, String, Bool)] = [
        (["-p", "x"], nil, "", "-p x", true),
        (["--minigw-role", "dev", "-p", "x"], nil, "dev", "-p x", true),
        (["-p", "--minigw-role=security"], "dev", "security", "-p", true),
        (["-p"], "dev", "dev", "-p", true),
        (["--minigw-role", "a b"], nil, "a b", "", false),
        (["-p", "--minigw-role"], nil, "", "-p", false),  // brez vrednosti: napaka
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

/// Poročilo Windows #14, #15: naprava na loopbacku brez odobritve je »ta računalnik«; dovoljena ima priklope tudi prek
/// loopbacka. Enako kot TestLoopbackDeviceSessionsAndHistory v Go.
@Test func loopbackDeviceSessionsAndHistory() async throws {
    let listed = String(repeating: "L", count: 43), other = String(repeating: "O", count: 43)
    var c = RouterConfig()
    c.devices = [DeviceConfig(id: listed, name: "PC")]
    let history = GatewayHistory()
    let router = Router(config: c, token: "tok", secrets: FakeSecrets(values: [:]), history: history)
    var sids: [String: String] = [:]
    for id in [listed, other] {
        let sid = "loop-" + UUID().uuidString
        sids[id] = sid
        var r = request("POST", "/v1/messages", headers: ["x-claude-mini-session": sid])
        r.peer = "127.0.0.1"
        r.device = Noise.Device(id: id, name: "x")
        await router.recordSession(r, model: "m", status: 200, took: 0, usage: (0, 0))
    }
    let all = history.sessions()
    #expect(all.first { $0.id == sids[listed] }?.device == listed && all.first { $0.id == sids[other] }?.device == "")
    var r = request("POST", "/v1/messages", headers: ["authorization": "Bearer tok"])
    r.peer = "127.0.0.1"
    r.device = Noise.Device(id: listed, name: "PC")
    await router.handle(r, writer: FakeWriter())
    #expect(!history.history(listed).isEmpty)
}

/// #45: način seje nad omejitvijo agenta (work) → le branje (cap v dejstvih izvajanja); worktree, native, chat in "" ostanejo.
/// Enako kot TestWorkCap v Go, še direct in native-direct z work=sync.
@Test func workCapPerAgent() async throws {
    var c = RouterConfig()
    c.codex.work = "worktree"; c.claudeCLI.work = "sync"; c.grok.work = ""
    let app = FakeApp(c), router = app.router()
    let id = UUID().uuidString, tmp = FileManager.default.temporaryDirectory
    let sessions = ["sync": "cap-s-" + id, "direct": "cap-d-" + id, "native-direct": "cap-n-" + id]
    _ = await Router.shares.register(session: sessions["sync"]!, cwd: "/p", key: nil, copy: tmp.appendingPathComponent(id), device: "dev")
    _ = await Router.shares.register(session: sessions["direct"]!, cwd: "/p", key: nil, device: "dev")
    _ = await Router.shares.register(session: sessions["native-direct"]!, cwd: "/p", key: nil, device: "dev", native: tmp)
    func cap(_ mode: String, _ kind: String) async -> String? {
        var r = request("POST", "/v1/messages", headers: ["x-claude-mini-session": sessions[mode] ?? "cap-none-" + id])
        r.device = Noise.Device(id: "dev", name: "PC")
        let facts = Router.RunBranch()
        let got = await Router.$runBranch.withValue(facts) { await router.capped(r, kind: kind) }
        #expect(facts.cap == (got ?? ""))
        return got
    }
    #expect(await Router.shares.sessionMode(sessions["native-direct"]!, device: "dev").mode == "native-direct")
    let once = "cap-once-" + id  // L4: opomba enkrat na sejo in agenta
    _ = await Router.shares.register(session: once, cwd: "/p", key: nil, device: "dev")
    var notes: [Bool] = []
    for (sid, kind) in [(once, "codex"), (once, "codex"), (once, "claude"), ("none-" + id, "codex")] {
        notes.append(await Router.shares.firstCapNote(session: sid, kind: kind))
    }
    #expect(notes == [true, false, true, false])
    for mode in ["sync", "direct", "native-direct"] { #expect(await cap(mode, "codex") == "worktree", "\(mode)") }
    for mode in ["direct", "native-direct"] { #expect(await cap(mode, "claude") == "sync", "\(mode)") }
    for (mode, kind) in [("sync", "claude"), ("sync", "grok"), ("chat", "codex")] { #expect(await cap(mode, kind) == nil, "\(mode) \(kind)") }
    // Skrbniški API: work na /agents/*; druga vrednost 400 z istim besedilom kot v Go; direct = "".
    for a in ["claude", "codex", "grok", "copilot"] {
        let bad = await admin(router, "PATCH", "/agents/\(a)", body: #"{"work":"readonly"}"#)
        #expect(bad.status == 400 && bad.text.contains("work: direct, sync or worktree"), "\(a) \(bad.text)")
        let ok = await admin(router, "PATCH", "/agents/\(a)", body: #"{"work":"sync"}"#)
        #expect(ok.status == 200 && ok.text.contains(#""work":"sync""#), "\(a) \(ok.text)")
        let d = await admin(router, "PATCH", "/agents/\(a)", body: #"{"work":"direct"}"#)
        #expect(d.status == 200 && d.text.contains(#""work":"direct""#), "\(a) \(d.text)")
    }
    #expect(app.config.codex.work == "" && app.config.claudeCLI.work == "")
}

/// #46a: zgodovina zapiše omejitve, ki jih je izvajanje uporabilo (dejstva izvajanja), ne izračunanih iz seje.
@Test func appliedLimitsInHistory() async throws {
    var c = RouterConfig()
    c.codex.write = true
    let history = GatewayHistory()
    let router = Router(config: c, token: "tok", secrets: FakeSecrets(values: [:]), history: history)
    let sid = "applied-" + UUID().uuidString
    let r = request("POST", "/v1/messages", headers: ["x-claude-mini-session": sid])
    let facts = Router.RunBranch()
    facts.readOnly = true; facts.cap = "worktree"
    await router.recordSession(r, model: "claude-codex", status: 200, took: 1, usage: (0, 0), facts: facts)
    await router.recordSession(r, model: "claude-sonnet", status: 200, took: 1, usage: (0, 0))
    let s = try #require(history.sessions().first { $0.id == sid })
    let m = try #require(useOf(s, "claude-codex"))
    #expect(m.readOnly && m.cap == "worktree" && m.tools == "" && s.readOnly)  // pisanje vklopljeno, a izvajanje je le bralo
    #expect(useOf(s, "claude-sonnet")?.cap == "" && useOf(s, "claude-sonnet")?.readOnly == false)
    let json = GatewayHistory.json(s)
    #expect((json["models"] as? [[String: Any]])?.first?["cap"] as? String == "worktree")
}

/// #46b: števci po projektu; enaki primeri kot TestProjectCounters v Go.
@Test func projectCounters() async throws {
    let g = GatewayHistory()
    let day = ISO8601DateFormatter().date(from: "2026-11-10T12:00:00Z")!
    func use(_ session: String, _ device: String, _ folder: String, _ model: String, _ status: Int, _ at: Date) {
        var u = GatewayHistory.Use()
        u.session = session; u.device = device; u.folder = folder; u.os = "macOS"; u.model = model; u.status = status; u.took = 30; u.input = 10; u.output = 2
        g.record(u, now: at)
    }
    use("a", "D", "/p", "claude-cli-opus", 200, day)
    use("a", "D", "/p/sub", "claude-cli-opus", 500, day)
    use("a", "D", "/p", "claude-codex", 403, day)
    use("b", "D", "/p", "or-qwen", 200, day)
    use("a", "D", "/p", "claude-cli-opus", 200, day.addingTimeInterval(13 * 3600))
    use("c", "D", "", "claude-cli-opus", 200, day)
    let later = day.addingTimeInterval(13 * 3600)
    let rows = g.usage(days: 7, now: later)
    #expect(rows.count == 4 && rows[0].day == "2026-11-11" && rows[1].day == "2026-11-10", "\(rows)")
    let c = rows[1]
    #expect(c.agent == "claude" && c.requests == 2 && c.errors == 1 && c.seconds == 60 && c.inputTokens == 20 && c.outputTokens == 4 && c.os == "macOS")
    #expect(rows[2].agent == "codex" && rows[2].refused == 1 && rows[3].agent == "other")
    #expect(g.usage(folder: "/P", days: 1, now: later).count == 1 && g.usage(device: "X", now: day).isEmpty)
    let d31 = day.addingTimeInterval(31 * 86400)
    use("a", "D", "/p", "claude-cli-opus", 200, d31)  // nova vrstica: starejše od 31 dni odpadejo
    #expect(g.usage(days: 31, now: d31).count == 2)  // 11. 11. ostane, 10. 11. ne
    for i in 0..<(GatewayHistory.maxDeviceProjects + 10) { use("x\(i)", "BIG", "/f\(i)", "m", 200, d31) }
    #expect(g.usage(device: "BIG", days: 31, now: d31).count == GatewayHistory.maxDeviceProjects && g.usage(device: "D", days: 31, now: d31).count == 2)
    g.clearSessions()
    #expect(g.usage(days: 31, now: d31).isEmpty && g.sessions().isEmpty)
    #expect(GatewayHistory.json(rows[3])["device"] as? String == "D" && GatewayHistory.json({ var p = rows[3]; p.device = ""; return p }())["device"] == nil)

    let r = FakeApp().router()
    var u = GatewayHistory.Use()
    u.session = "usage-" + UUID().uuidString; u.folder = "/proj-" + u.session; u.model = "claude-codex"; u.took = 60; u.input = 5; u.output = 6
    Router.history.record(u)
    let list = await admin(r, "GET", "/usage?days=1&folder=" + u.folder)
    #expect(list.status == 200 && list.text.contains(#""agent":"codex""#) && list.text.contains(#""requests":1"#) && !list.text.contains(#""device""#), "\(list.text)")
    let bad = await admin(r, "GET", "/usage?days=32")
    #expect(bad.status == 400 && bad.text.contains("days: 1-31"))
}

/// Parity: isti state.json kot TestStateParitySample v Go (client/testdata) se prebere enako.
@Test func stateParitySample() throws {
    let root = URL(fileURLWithPath: #filePath).deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent()
    let g = GatewayHistory()
    g.load(root.appendingPathComponent("client/testdata/state-0.5.21.json"))
    let s = try #require(g.sessions().first)
    #expect(g.sessions().count == 1 && s.models.first?.cap == "worktree" && s.models.first?.readOnly == true && s.models.first?.role == "")
    #expect(g.lastSeen(String(repeating: "A", count: 43)) != nil && g.history(String(repeating: "A", count: 43)).count == 1)
    let now = ISO8601DateFormatter().date(from: "2026-11-10T18:00:00Z")!
    let u = g.usage(folder: "c:/PROJ", now: now)
    #expect(u.count == 1 && u.first?.agent == "codex" && u.first?.seconds == 42.5 && u.first?.refused == 1)
    let all = g.usage(days: 2, now: now)
    #expect(all.count == 2 && all[0].day == "2026-11-10" && all[1].device == "")
}

/// L1 (#45): tek, ki ga je omejitev naredila le za branje, ne prevzame sprememb kopije k odjemalcu (SYNC-PULL).
@Test func cappedRunDoesNotSyncBack() async throws {
    let hub = ShareHub()
    let copy = FileManager.default.temporaryDirectory.appendingPathComponent("capsync-" + UUID().uuidString)
    defer { try? FileManager.default.removeItem(at: copy) }
    let key = try #require(await hub.register(session: "c", cwd: "/a", key: nil, copy: copy))
    let facts = Router.RunBranch()
    facts.cap = "worktree"
    await Router.$runBranch.withValue(facts) { await hub.syncBack(session: "c") }  // vrne takoj, brez posla
    guard case .some(.none) = await hub.poll(key: key, wait: 0.2) else { Issue.record("capped run queued a job"); return }
    let back = Task { await hub.syncBack(session: "c") }  // brez omejitve: SYNC-PULL
    guard case .some(.some(let job)) = await hub.poll(key: key, wait: 5) else { Issue.record("no SYNC-PULL"); return }
    #expect(job.method == "SYNC-PULL")
    _ = await hub.reply(key: key, DAVReply(id: job.id, response: DAVResponse(200)))
    await back.value
}

/// L2: ključ mape ≤ 256 B; pri meji datoteke najprej odpadejo vrstice naprave z največ vrsticami (kot TestTrimBusiest v Go).
@Test func projectFolderClipAndTrim() {
    let g = GatewayHistory()
    var u = GatewayHistory.Use()
    u.session = "clip"; u.folder = "/" + String(repeating: "ž", count: 300); u.model = "m"
    g.record(u)
    #expect(g.usage().first.map { $0.folder.utf8.count <= 256 } == true)
    func row(_ d: String, _ day: String) -> GatewayHistory.ProjectUse { var p = GatewayHistory.ProjectUse(); p.device = d; p.day = day; return p }
    let got = GatewayHistory.trimBusiest([row("A", "1"), row("B", "1"), row("B", "2"), row("A", "2"), row("B", "3")])
    #expect(got.map { $0.device + $0.day } == ["A1", "B2", "A2", "B3"])
}
