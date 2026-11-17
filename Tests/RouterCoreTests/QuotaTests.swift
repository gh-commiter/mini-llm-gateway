import Foundation
import Testing
@testable import RouterCore

// 0.6.0 M5: the quota of the Mac gateway. The parsers are proven against Go through client/testdata/quota-parsers.json; the engine (opt-in,
// cache, stale, guards) with fake CLIs and a fake HTTP; the probe's process with a fake claude script. Nothing runs the real claude, touches the
// real Codex home, the keychain or the network.

private let repo = URL(fileURLWithPath: #filePath).deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent()
private let utc = TimeZone(identifier: "UTC")!

private func fixtureCases() throws -> [[String: Any]] {
    let d = try Data(contentsOf: repo.appendingPathComponent("client/testdata/quota-parsers.json"))
    return (try JSONSerialization.jsonObject(with: d) as! [String: Any])["cases"] as! [[String: Any]]
}

private func tempDir(_ name: String) -> URL {
    let u = FileManager.default.temporaryDirectory.appendingPathComponent("mgquota-\(name)-\(UUID().uuidString.prefix(8))", isDirectory: true).cleanedAtExit()
    try? FileManager.default.createDirectory(at: u, withIntermediateDirectories: true)
    return u
}

@Suite struct QuotaParserParity {
    @Test func everyParserGivesWhatGoGives() throws {
        let cases = try fixtureCases()
        #expect(cases.count >= 60)
        for c in cases {
            let name = c["name"] as! String, kind = c["kind"] as! String, input = c["input"] as! String
            let now = QuotaTime.parse(c["now"] as! String)!
            let got: Any
            do {
                switch kind {
                case "codex": got = QuotaParse.codexRateLimits(Data(input.utf8), observed: now).map(\.json)
                case "claude-usage": got = try QuotaParse.claudeUsage(Data(input.utf8), now: now, zone: utc).map(\.json)
                case "openrouter": got = try QuotaParse.openRouterCredits(Data(input.utf8), now: now).map(\.json)
                case "litellm": got = try QuotaParse.liteLLMKeyInfo(Data(input.utf8), now: now).map(\.json)
                case "claude-reset": got = QuotaParse.claudeReset(input, now: now, zone: utc).map { QuotaTime.format($0) } ?? NSNull()
                case "stale":
                    let vals = (try JSONSerialization.jsonObject(with: Data(input.utf8)) as! [[String: Any]]).map { o in
                        QuotaValue(name: o["name"] as! String, source: o["source"] as! String, observedAt: (o["observedAt"] as? String).flatMap(QuotaTime.parse),
                                   resetsAt: (o["resetsAt"] as? String).flatMap(QuotaTime.parse), stale: o["stale"] as! Bool)
                    }
                    got = quotaMarkStale(vals, now: now).map(\.stale)
                default: Issue.record("\(name): kind \(kind)"); continue
                }
            } catch { got = ["error": (error as? QuotaError)?.description ?? "\(error)"] }
            let want = c["want"]!
            #expect((got as AnyObject).isEqual(want), "\(name):\n got \(got)\nwant \(want)")
        }
    }

    @Test func timeFormatRoundTrips() {
        #expect(QuotaTime.format(Date(timeIntervalSince1970: 1_794_000_000)) == "2026-11-06T21:20:00Z")
        #expect(QuotaTime.format(Date(timeIntervalSince1970: 1_794_000_000.5)) == "2026-11-06T21:20:00.5Z")
        #expect(QuotaTime.format(Date(timeIntervalSince1970: 1_794_000_000.123456)) == "2026-11-06T21:20:00.123456Z")
        #expect(QuotaTime.parse("2026-12-01T00:00:00.123456789Z").map { QuotaTime.format($0) } == "2026-12-01T00:00:00.123457Z")  // microseconds: Go's nanoseconds are rounded
        #expect(QuotaTime.parse("2026-12-01T10:30:00+02:00").map { QuotaTime.format($0) } == "2026-12-01T08:30:00Z")
        #expect(QuotaTime.parse("soon") == nil && QuotaTime.parse("2026-12-01") == nil)
    }
}

// MARK: the engine with fake CLIs and a fake HTTP

private let usageEnvelope = #"{"is_error":false,"duration_api_ms":0,"num_turns":0,"total_cost_usd":0,"usage":{"input_tokens":0,"output_tokens":0},"result":"You are currently using your subscription to power your Claude Code usage\n\nCurrent week (all models): 8% used · resets Nov 19 at 12am (Europe/Ljubljana)\nCurrent week (Fable): 12.5% used · resets Nov 19 at 3:30pm (UTC)\n\nWhat's contributing to your usage?\n"}"#

private final class CLICounter: @unchecked Sendable {
    private let lock = NSLock()
    private var n = 0, reply: Result<Data, Error>
    init(_ r: Result<Data, Error>) { reply = r }
    var calls: Int { lock.withLock { n } }
    func call() throws -> Data { try lock.withLock { n += 1; return try reply.get() } }
    func set(_ r: Result<Data, Error>) { lock.withLock { reply = r } }
}

private func inputs(probe: Bool = false, exe: String? = "/fake/claude", enabled: Bool = true, providers: [QuotaInputs.Provider] = [], key: @escaping (String) -> String? = { _ in nil }) -> QuotaInputs {
    QuotaInputs(claudeProbe: probe, claudeExe: exe, claudeEnabled: enabled, providers: providers, key: key, tmpParent: tempDir("tmp"))
}

@Suite struct QuotaEngineGuards {
    let t0 = QuotaTime.parse("2026-11-14T10:00:00Z")!

    @Test func theClaudeProbeIsOffUnlessTheOwnerTurnsItOn() async {
        let cli = CLICounter(.success(Data(usageEnvelope.utf8)))
        let e = QuotaEngine(runUsage: { _, _ in try cli.call() }, zone: utc)
        let off = await e.report(inputs(probe: false), now: t0)
        #expect(cli.calls == 0)
        #expect(off.map(\.name) == ["codex", "claude"] && off[1].source == "unavailable" && off[1].reason == "probe off" && off[0].note == "no Codex run since this gateway started")
        let noCLI = await e.report(inputs(probe: true, exe: nil), now: t0)
        #expect(noCLI[1].reason == "Claude CLI is off or not installed on this gateway" && cli.calls == 0)
        let disabled = await e.report(inputs(probe: true, enabled: false), now: t0)
        #expect(disabled[1].reason == "Claude CLI is off or not installed on this gateway" && cli.calls == 0)
    }

    @Test func cachedForTenMinutesStaleOnAFailedRefreshAndOffForGoodAfterInference() async {
        let cli = CLICounter(.success(Data(usageEnvelope.utf8)))
        let e = QuotaEngine(runUsage: { _, _ in try cli.call() }, zone: utc)
        let i = inputs(probe: true)
        for k in 0..<3 {
            let v = await e.report(i, now: t0.addingTimeInterval(Double(k) * 60)).filter { $0.name.hasPrefix("claude") }
            #expect(v.count == 2 && !v[0].stale, "\(v)")
        }
        #expect(cli.calls == 1)  // within the cache time
        cli.set(.failure(QuotaError("timeout")))
        let v = await e.report(i, now: t0.addingTimeInterval(11 * 60)).filter { $0.name.hasPrefix("claude") }
        #expect(cli.calls == 2 && v.count == 2 && v[0].stale && v[0].note.contains("timeout"), "\(v)")
        cli.set(.success(Data(usageEnvelope.replacingOccurrences(of: #""num_turns":0"#, with: #""num_turns":2"#).utf8)))
        let off = await e.report(i, now: t0.addingTimeInterval(30 * 60)).filter { $0.name.hasPrefix("claude") }
        #expect(off.count == 1 && off[0].source == "unavailable" && off[0].reason.contains("ran inference"), "\(off)")
        let before = cli.calls
        _ = await e.report(i, now: t0.addingTimeInterval(2 * 3600))
        #expect(cli.calls == before)  // never again
    }

    @Test func aFailureWithoutEarlierValuesIsUnavailableWithTheReason() async {
        let e = QuotaEngine(runUsage: { _, _ in throw QuotaError("claude /usage failed: exit status 1") }, zone: utc)
        let v = await e.report(inputs(probe: true), now: t0).filter { $0.name == "claude" }
        #expect(v.count == 1 && v[0].source == "unavailable" && v[0].stale && v[0].reason == "claude /usage failed: exit status 1")
    }

    @Test func aStuckProbeDoesNotBlockCodexRecordingAndWaitersShareOneRun() async throws {
        let release = DispatchSemaphore(value: 0), started = DispatchSemaphore(value: 0)
        let cli = CLICounter(.success(Data(usageEnvelope.utf8)))
        let e = QuotaEngine(runUsage: { _, _ in _ = try cli.call(); started.signal(); await offPool { release.wait() }; return Data(usageEnvelope.utf8) }, zone: utc)
        let i = inputs(probe: true)
        let a = Task { await e.report(i, now: t0) }
        await offPool { started.wait() }  // the probe runs and is stuck
        let b = Task { await e.report(i, now: t0) }  // a second caller waits for the first one
        // a Codex run finishes meanwhile: its numbers are recorded at once
        let home = tempDir("codex")
        let day = home.appendingPathComponent("sessions/2026/11/14")
        try FileManager.default.createDirectory(at: day, withIntermediateDirectories: true)
        try Data(#"{"payload":{"rate_limits":{"primary":{"used_percent":5,"window_minutes":300,"resets_at":4102444800}}}}"#.utf8 + [0x0a])
            .write(to: day.appendingPathComponent("rollout-2026-11-14T10-00-00-th.jsonl"))
        await e.recordCodex(home: home.path, thread: "th")
        release.signal()
        let ra = await a.value, rb = await b.value
        #expect(cli.calls == 1)
        #expect(ra.filter { $0.name.hasPrefix("claude") }.count == 2 && rb.filter { $0.name.hasPrefix("claude") }.count == 2)
        #expect((await e.report(i, now: t0)).first?.name == "codex.5h")  // recorded while the probe was stuck
    }

    @Test func codexNumbersComeFromThisRunsLogOnly() async throws {
        let home = tempDir("codexhome")
        let day = home.appendingPathComponent("sessions/2026/11/14")
        try FileManager.default.createDirectory(at: day, withIntermediateDirectories: true)
        func rec(_ p: Double, _ w: Double) -> String {
            #"{"timestamp":"t","type":"event_msg","payload":{"type":"token_count","info":null,"rate_limits":{"primary":{"used_percent":\#(p),"window_minutes":300,"resets_at":4102444800},"secondary":{"used_percent":\#(w),"window_minutes":10080,"resets_at":4102444800}}}}"#
        }
        try Data(("not json\n" + rec(10, 20) + "\n{\"type\":\"other\"}\n" + rec(12.5, 21) + "\n{\"type\":\"turn\"}\n").utf8).write(to: day.appendingPathComponent("rollout-2026-11-14T10-00-00-thread-mine.jsonl"))
        try Data((rec(99, 99) + "\n").utf8).write(to: day.appendingPathComponent("rollout-2026-11-14T11-00-00-thread-other.jsonl"))  // newer, another run's
        let e = QuotaEngine(runUsage: { _, _ in Data() }, zone: utc)
        await e.recordCodex(home: home.path, thread: "thread-mine")
        let vals = await e.report(inputs(), now: Date())
        #expect(vals.count >= 2 && vals[0].name == "codex.5h" && vals[0].value == 12.5 && vals[1].name == "codex.week" && vals[1].value == 21)
        #expect(vals[0].source == "codex-session-log" && vals[0].observedAt != nil && vals[0].resetsAt?.timeIntervalSince1970 == 4_102_444_800 && !vals[0].stale)
        // no thread, a thread name that tries a glob or a path, a missing log: nothing is recorded
        let fresh = QuotaEngine(runUsage: { _, _ in Data() }, zone: utc)
        for th in ["", "*", "../x", "a/b", "thread-none", "th[a]"] { await fresh.recordCodex(home: home.path, thread: th) }
        #expect((await fresh.report(inputs(), now: Date())).first?.source == "unavailable")
        // a huge log is read from its end only
        let big = day.appendingPathComponent("rollout-2026-11-14T12-00-00-thread-big.jsonl")
        let filler = (0..<1000).map { _ in #"{"filler":"\#(String(repeating: "x", count: 900))"}"# }.joined(separator: "\n")
        try Data((filler + "\n" + rec(33, 44) + "\n").utf8).write(to: big)
        let v = QuotaParse.lastRateLimits(path: big.path, observed: Date())
        #expect(v.count == 2 && v[0].value == 33)
        // a reading whose window has reset since is stale
        var old = QuotaParse.codexRateLimits(Data(rec(1, 1).utf8), observed: Date().addingTimeInterval(-5 * 3600))
        old[0].resetsAt = Date().addingTimeInterval(-3600)
        #expect(quotaMarkStale(old, now: Date())[0].stale)
    }

    // MARK: OpenRouter and LiteLLM

    private final class Seen: @unchecked Sendable {
        private let lock = NSLock()
        var urls: [String] = [], auth: [String] = []
        func add(_ r: URLRequest) { lock.withLock { urls.append(r.url!.absoluteString); auth.append(r.value(forHTTPHeaderField: "Authorization") ?? "") } }
    }

    @Test func providerCreditAndSpendWithTheKeyTheGatewayHolds() async throws {
        let seen = Seen()
        nonisolated(unsafe) var alive = true
        let e = QuotaEngine(runUsage: { _, _ in Data() }, http: { r in
            seen.add(r)
            guard alive else { return nil }
            if r.url!.path == "/api/v1/credits" { return (200, Data(#"{"data":{"total_credits":50,"total_usage":12.25}}"#.utf8)) }
            return (200, Data(#"{"key":"sk-xxx","info":{"spend":3.5,"max_budget":10,"budget_reset_at":"2026-12-01T00:00:00Z"}}"#.utf8))
        }, zone: utc)
        let providers: [QuotaInputs.Provider] = [.init(id: "openrouter", name: "OpenRouter", baseURL: "https://x.test/api", enabled: true),
                                                 .init(id: "litellm", name: "LiteLLM", baseURL: "http://localhost:4000/", enabled: true),
                                                 .init(id: "other", name: "Other", baseURL: "https://o.test", enabled: true),
                                                 .init(id: "off", name: "litellm-off", baseURL: "https://l.test", enabled: false)]
        let i = inputs(providers: providers, key: { "SECRET-" + $0 })
        let vals = await e.report(i, now: t0)
        let byName = Dictionary(vals.map { ($0.name, $0) }, uniquingKeysWith: { $1 })
        #expect(byName["openrouter.credits-remaining"]?.value == 37.75 && byName["openrouter.credits-remaining"]?.source == "openrouter-api" && byName["openrouter.credits-remaining"]?.stale == false)
        #expect(byName["litellm.spend"]?.value == 3.5 && byName["litellm.spend"]?.resetsAt.map { QuotaTime.format($0) } == "2026-12-01T00:00:00Z" && byName["litellm.max-budget"]?.value == 10)
        #expect(byName["codex"]?.source == "unavailable")
        #expect(Set(seen.urls) == ["https://x.test/api/v1/credits", "http://localhost:4000/key/info"], "\(seen.urls)")  // the others are no known provider, or off
        #expect(Set(seen.auth) == ["Bearer SECRET-openrouter", "Bearer SECRET-litellm"])
        let json = String(decoding: try JSONSerialization.data(withJSONObject: vals.map(\.json)), as: UTF8.self)
        #expect(!json.contains("SECRET"))  // the key never leaves the gateway
        // cached for 10 minutes: no second request
        let n = seen.urls.count
        _ = await e.report(i, now: t0.addingTimeInterval(300))
        #expect(seen.urls.count == n)
        // the provider goes away: the cached values come back marked stale, with a note
        alive = false
        let after = await e.report(i, now: t0.addingTimeInterval(700)).filter { $0.name.hasPrefix("openrouter.") }
        #expect(after.count == 2 && after.allSatisfy { $0.stale && $0.note == "the provider did not answer" }, "\(after)")
    }

    @Test func providerFailuresSayWhy() async {
        func run(_ status: Int, _ body: String, key: String? = "k", tries: Int = 1) async -> [QuotaValue] {
            let e = QuotaEngine(runUsage: { _, _ in Data() }, http: { _ in (status, Data(body.utf8)) }, zone: utc)
            return await e.report(inputs(providers: [.init(id: "openrouter", name: "OpenRouter", baseURL: "https://x.test/api", enabled: true)], key: { _ in key }), now: Date())
                .filter { $0.name == "openrouter" }
        }
        #expect((await run(200, "{}", key: nil)).first?.reason == "no key for OpenRouter")
        #expect((await run(200, "{}", key: "")).first?.reason == "no key for OpenRouter")
        #expect((await run(401, "{}")).first?.reason == "the provider answered HTTP 401")
        #expect((await run(200, "<html>")).first?.reason == "unexpected answer")
    }

    // MARK: the probe's process

    @Test func theEnvironmentIsAnAllowlistPlusAPrivateTempFolder() {
        let env = ClaudeUsageProbe.env(["PATH": "/bin", "HOME": "/h", "ANTHROPIC_BASE_URL": "http://evil", "ANTHROPIC_API_KEY": "k", "CLAUDE_CODE_USE_BEDROCK": "1",
                                        "CLAUDE_CODE_OAUTH_TOKEN": "tok", "CLAUDE_CONFIG_DIR": "/c", "AWS_SECRET_ACCESS_KEY": "s", "HTTPS_PROXY": "http://p",
                                        "MINIGW_ANYTHING": "1", "TZ": "UTC"], tmp: "/probe")
        let got = env.map { "\($0.key)=\($0.value)" }.joined(separator: "\n")
        for bad in ["ANTHROPIC", "BEDROCK", "AWS_", "PROXY", "MINIGW"] { #expect(!got.contains(bad), "\(bad) reached the probe: \(got)") }
        #expect(env["HOME"] == "/h" && env["CLAUDE_CODE_OAUTH_TOKEN"] == "tok" && env["CLAUDE_CONFIG_DIR"] == "/c" && env["TZ"] == "UTC" && env["TMPDIR"] == "/probe" && env["TEMP"] == "/probe")
        #expect(env["PATH"]?.hasPrefix("/bin:") == true)
        for (k, why) in ClaudeUsageProbe.envAllow { #expect(!why.isEmpty, "\(k) has no documented reason") }
    }

    /// A fake claude: records its arguments, working folder and environment next to itself, prints the envelope.
    private func fakeClaude(_ dir: URL, body: String) throws -> String {
        let p = dir.appendingPathComponent("claude")
        try Data("#!/bin/sh\npwd -P > \"\(dir.path)/cwd.txt\"\nprintf '%s\\n' \"$@\" > \"\(dir.path)/args.txt\"\nenv > \"\(dir.path)/env.txt\"\nls -A > \"\(dir.path)/ls.txt\"\n\(body)\n".utf8).write(to: p)
        chmod(p.path, 0o755)
        return p.path
    }

    @Test func theProbeRunsInAnEmptyPrivateFolderWithNoToolsAndAnAllowlistedEnvironment() async throws {
        let dir = tempDir("fake")
        let exe = try fakeClaude(dir, body: "cat <<'EOF'\n\(usageEnvelope)\nEOF")
        let parent = tempDir("parent").appendingPathComponent("quota-tmp")
        let environment = ProcessInfo.processInfo.environment.merging(["ANTHROPIC_API_KEY": "sk-must-not-reach-the-probe", "MINIGW_PROBE_TEST": "1"]) { $1 }
        let out = try await ClaudeUsageProbe.run(exe: exe, parent: parent, limit: 30, drain: 2, environment: environment)
        #expect(String(decoding: out, as: UTF8.self).contains("Current week (all models)"))
        let args = try String(contentsOf: dir.appendingPathComponent("args.txt"), encoding: .utf8).split(separator: "\n", omittingEmptySubsequences: false).map(String.init)
        #expect(args.starts(with: ["-p", "/usage", "--model", "haiku"]) && args.contains("--no-session-persistence") && args.contains("--strict-mcp-config"))
        #expect(zip(args, args.dropFirst()).contains { $0 == "--tools" && $1.isEmpty } && zip(args, args.dropFirst()).contains { $0 == "--setting-sources" && $1.isEmpty })
        #expect(!args.contains("--disable-slash-commands"))  // /usage is a slash command
        let cwd = try String(contentsOf: dir.appendingPathComponent("cwd.txt"), encoding: .utf8).trimmingCharacters(in: .whitespacesAndNewlines)
        #expect(cwd.contains("/quota-tmp/probe-") && !cwd.hasPrefix(FileManager.default.currentDirectoryPath), "cwd \(cwd)")
        #expect(try String(contentsOf: dir.appendingPathComponent("ls.txt"), encoding: .utf8).isEmpty)  // an empty folder
        let env = try String(contentsOf: dir.appendingPathComponent("env.txt"), encoding: .utf8)
        #expect(!env.contains("ANTHROPIC") && !env.contains("MINIGW_PROBE_TEST") && env.contains("TMPDIR="))
        #expect((try FileManager.default.contentsOfDirectory(atPath: parent.path)).isEmpty)  // the working folder is gone
        let mode = try FileManager.default.attributesOfItem(atPath: parent.path)[.posixPermissions] as? Int
        #expect(mode == 0o700)
    }

    @Test func aFailingOrStuckProbeIsAnErrorNotAHang() async throws {
        let dir = tempDir("fake2")
        let bad = try fakeClaude(dir, body: "echo oops >&2\nexit 3")
        do { _ = try await ClaudeUsageProbe.run(exe: bad, parent: tempDir("p")); Issue.record("no error") } catch {
            #expect((error as? QuotaError)?.description == "claude /usage failed: exit status 3")
        }
        let slow = try fakeClaude(tempDir("fake3"), body: "sleep 600")
        let t0 = Date()
        do { _ = try await ClaudeUsageProbe.run(exe: slow, parent: tempDir("p"), limit: 1); Issue.record("no error") } catch {
            #expect((error as? QuotaError)?.description.hasPrefix("claude /usage failed: timed out") == true)
        }
        #expect(Date().timeIntervalSince(t0) < 300)  // ends by its 1 s limit, not at the stub's 600 s
        do { _ = try await ClaudeUsageProbe.run(exe: "/nonexistent/claude", parent: tempDir("p")); Issue.record("no error") } catch {
            #expect((error as? QuotaError)?.description.hasPrefix("claude /usage failed: ") == true)
        }
    }
}

// MARK: the admin route

@Suite(.serialized) struct QuotaAdminRoute {
    @Test func theReportNeedsNoCoreAndTheProbeIsAnOptInThatIsSaved() async throws {
        let cli = CLICounter(.success(Data(usageEnvelope.utf8)))
        let saved = Router.quota
        Router.quota = QuotaEngine(runUsage: { _, _ in try cli.call() }, zone: utc)
        defer { Router.quota = saved }
        var cfg = RouterConfig()
        cfg.claudeCLI.enabled = true
        cfg.claudeCLI.executable = "/bin/sh"  // any executable: the fake runner never starts it
        let app = FakeApp(cfg)
        let r = app.router()
        let off = await admin(r, "GET", "/quota")
        #expect(off.status == 200 && off.text.contains(#""claudeProbe":false"#) && off.text.contains("probe off") && cli.calls == 0, "\(off.text)")
        for bad in ["", "{}", #"{"claudeProbe":1}"#, #"{"claudeProbe":null}"#, #"{"claudeProbe":true,"x":1}"#, "[true]", "nope"] {
            let w = await admin(r, "PATCH", "/quota", body: bad)
            #expect(w.status == 400 && w.text.contains("bad JSON body (claudeProbe)"), "\(bad): \(w.text)")
        }
        #expect(app.config.claudeQuotaProbe == false)
        let on = await admin(r, "PATCH", "/quota", body: #"{"claudeProbe":true}"#)
        #expect(on.status == 200 && on.text.contains(#""claudeProbe":true"#) && cli.calls == 0 && on.text.contains("not probed yet"), "\(on.text)")  // the PATCH runs nothing
        #expect(app.config.claudeQuotaProbe == true)  // saved with the settings
        let got = await admin(app.router(), "GET", "/quota")  // the next GET probes: a restarted router, the cache is the process's
        #expect(got.text.contains("claude.week.all-models") && cli.calls == 1, "\(got.text)")
        let again = await admin(app.router(), "GET", "/quota")
        #expect(again.text.contains(#""claudeProbe":true"#) && cli.calls == 1)
        #expect(await admin(r, "GET", "/quota", auth: nil).status == 401)  // the admin proof is needed
        // the older settings file has no key: off
        let old = try JSONDecoder().decode(RouterConfig.self, from: Data("{}".utf8))
        #expect(old.claudeQuotaProbe == false)
        var c2 = RouterConfig(); c2.claudeQuotaProbe = true
        #expect(try JSONDecoder().decode(RouterConfig.self, from: JSONEncoder().encode(c2)).claudeQuotaProbe == true)
    }
}
