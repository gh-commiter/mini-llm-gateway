import Foundation
import Testing
@testable import RouterCore

/// #125: the allowlist of agent runs. client/testdata/agent-env.json is asserted by Go too (client/agentenv_test.go).
@Suite struct AgentEnvTests {
    struct Fixture: Decodable {
        struct Case: Decodable { var names: [String]; var want: [String]?; var error: String }
        struct Unset: Decodable { var env: [String: String]; var names, want: [String] }
        var baseNames, basePrefixes, denyNames, denyPrefixes, denySuffixes: [String]
        var maxNames: Int
        var validate: [Case]
        var tooMany: String
        var notBase: [String]
        var unset: [Unset]
    }
    static let root = URL(fileURLWithPath: #filePath).deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent()
    static func fixture() throws -> Fixture {
        try JSONDecoder().decode(Fixture.self, from: Data(contentsOf: root.appendingPathComponent("client/testdata/agent-env.json")))
    }
    /// The gateway's environment with secrets.
    static let sample = ["MGTEST_SECRET": "1", "AWS_SECRET_ACCESS_KEY": "x", "FOO_API_KEY": "x", "PATH": "/usr/bin:/bin", "HOME": "/h",
                         "HTTPS_PROXY": "http://127.0.0.1:9", "https_proxy": "http://127.0.0.1:9", "LC_ALL": "C", "ANTHROPIC_BASE_URL": "x", "GH_TOKEN": "x"]

    @Test func listsMatchTheFixture() throws {
        let f = try Self.fixture()
        #expect(AgentEnv.baseNames == f.baseNames)
        #expect(AgentEnv.basePrefixes == f.basePrefixes)
        #expect(AgentEnv.denyNames == f.denyNames)
        #expect(AgentEnv.denyPrefixes == f.denyPrefixes)
        #expect(AgentEnv.denySuffixes == f.denySuffixes)
        #expect(AgentEnv.maxNames == f.maxNames)
    }

    @Test func validation() throws {
        let f = try Self.fixture()
        for c in f.validate {
            let v = AgentEnv.validate(c.names)
            #expect((v.error ?? "") == c.error, "\(c.names)")
            if c.error.isEmpty { #expect(v.names == c.want, "\(c.names)") }
        }
        let many = (0...f.maxNames).map { "V\($0)" }
        #expect(AgentEnv.validate(Array(many.prefix(f.maxNames))).error == nil)
        #expect(AgentEnv.validate(many).error == f.tooMany)
        #expect(AgentEnv.names(" A, B  C,,") == ["A", "B", "C"])
    }

    /// D: the owner's names the gateway's environment does not hold (GET agents/env "unset"), what the base list must not hold, and what Settings stores.
    @Test func unsetNamesAndBaseLimits() throws {
        let f = try Self.fixture()
        for c in f.unset { #expect(AgentEnv.unset(c.names, env: c.env) == c.want, "\(c.names)") }
        for n in f.notBase { #expect(AgentEnv.environment([n: "1"], owner: []).isEmpty, "\(n)") }
        for n in ["GOPRIVATE", "GONOSUMDB", "GONOPROXY", "GRADLE_USER_HOME", "NVM_HOME", "NVM_SYMLINK"] { #expect(AgentEnv.environment([n: "1"], owner: []).count == 1, "\(n)") }
        #expect(AgentEnv.saved("A;B", stored: "OLD") == "OLD")  // Settings: an invalid entry is not saved
        #expect(AgentEnv.saved("A, B", stored: "OLD") == "A, B")
        #expect(AgentEnv.saved("FOO_API_KEY", stored: "") == "")
    }

    /// Claude CLI, Codex, Copilot and minigw run all start from AgentEnv.environment (everyStartUsesTheAllowlist).
    @Test func environmentForEveryKind() {
        let base: Set = ["PATH", "HOME", "HTTPS_PROXY", "https_proxy", "LC_ALL"]
        #expect(Set(AgentEnv.environment(Self.sample, owner: []).keys) == base)
        #expect(Set(AgentEnv.environment(Self.sample, owner: ["MGTEST_SECRET"]).keys) == base.union(["MGTEST_SECRET"]))
        #expect(Set(AgentEnv.environment(Self.sample, owner: ["mgtest_secret", "FOO_API_KEY", "GH_TOKEN"]).keys) == base.union(["MGTEST_SECRET"]))
        // Copilot filters on top (its own denylist) and sets its home; minigw run sets its temporary folders: neither brings a secret back
        let copilot = Copilot.environment(AgentEnv.environment(Self.sample, owner: ["MGTEST_SECRET"]), home: URL(fileURLWithPath: "/tmp/h"))
        #expect(copilot["MGTEST_SECRET"] == "1" && copilot["AWS_SECRET_ACCESS_KEY"] == nil && copilot["PATH"] != nil)
        let run = Native.runEnv(AgentEnv.environment(Self.sample, owner: []), runTmp: URL(fileURLWithPath: "/tmp/r"))
        #expect(run["MGTEST_SECRET"] == nil && run["AWS_SECRET_ACCESS_KEY"] == nil && run["HTTPS_PROXY"] != nil)
    }

    /// #189: a hand-edited config must not pass these names either: they are dropped at run time, whatever the case.
    @Test func deniedOwnerNamesAreDroppedAtRunTime() {
        let env = ["MINIGW_SHARE": "x", "minigw_other": "x", "GH_ENTERPRISE_TOKEN": "x", "GITHUB_ENTERPRISE_TOKEN": "x", "CLAUDE_ENV_FILE": "x",
                   "BASH_ENV": "x", "ENV": "x", "PATH": "/usr/bin"]
        let owner = ["MINIGW_SHARE", "MINIGW_OTHER", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN", "CLAUDE_ENV_FILE", "BASH_ENV", "ENV"]
        #expect(Set(AgentEnv.environment(env, owner: owner).keys) == ["PATH"])
    }

    /// The real start of Codex (Codex.run): the process gets the filtered environment and the run's own variables after it.
    @Test func codexRunGetsTheFilteredEnvironment() async throws {
        let dir = try #require(Sync.real(FileManager.default.temporaryDirectory)).appendingPathComponent("ae-" + UUID().uuidString)
        try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: dir) }
        let out = dir.appendingPathComponent("out")
        // names only, never values
        let script = "for n in MGTEST_SECRET AWS_SECRET_ACCESS_KEY FOO_API_KEY HTTPS_PROXY MGTEST_RUN; do /usr/bin/printenv $n >/dev/null && echo $n; done > '\(out.path)'"
        for (owner, want) in [([String](), "HTTPS_PROXY MGTEST_RUN"), (["MGTEST_SECRET", "FOO_API_KEY"], "MGTEST_SECRET HTTPS_PROXY MGTEST_RUN")] {
            let (text, _) = try await Codex.run("/bin/sh", args: ["-c", script], cwd: dir, prompt: "", output: out, env: ["MGTEST_RUN": "1"],
                                                owner: owner, base: Self.sample, dirs: [dir.path])
            #expect(text.split(separator: "\n").joined(separator: " ") == want, "owner \(owner)")
        }
    }

    /// Every agent start on the Mac builds its environment with AgentEnv.environment; the gateway's whole environment
    /// (ProcessInfo.processInfo.environment) as a process environment there fails here.
    @Test func everyStartUsesTheAllowlist() throws {
        func src(_ p: String) throws -> String { try String(contentsOf: Self.root.appendingPathComponent("Sources/RouterCore/" + p), encoding: .utf8) }
        let starts = ["ClaudeCLI.swift": "var env = AgentEnv.environment(owner: AgentEnv.names(config.agentEnv))",
                      "Codex.swift": "p.environment = AgentEnv.environment(base, owner: owner)",
                      "Copilot.swift": "let base = AgentEnv.environment(owner: AgentEnv.names(config.agentEnv))",
                      "Native.swift": "Native.runEnv(AgentEnv.environment(owner: AgentEnv.names(config.agentEnv))"]
        for (file, line) in starts { #expect(try src(file).contains(line), "\(file)") }
        #expect(try src("Codex.swift").contains("owner: AgentEnv.names(config.agentEnv)"))  // the handler passes the owner's names
        #expect(try !src("Codex.swift").contains("p.environment = ProcessInfo.processInfo.environment"))
        #expect(try !src("Native.swift").contains("Native.runEnv(ProcessInfo.processInfo.environment"))
        #expect(try !src("Copilot.swift").contains("let base = ProcessInfo.processInfo.environment"))
        #expect(try !src("ClaudeCLI.swift").contains("var env = ProcessInfo.processInfo.environment"))
    }

    /// The setting through the admin API (gw agents env): add, remove, replace and validation.
    @Test func settingRoundTrip() async {
        let app = FakeApp()
        let r = app.router()
        for (method, body, status, want) in [
            ("GET", "", 200, #""names":[]"#),
            ("PATCH", #"{"add":"MGTEST_SECRET"}"#, 200, #""names":["MGTEST_SECRET"]"#),
            ("PATCH", #"{"add":"OTHER"}"#, 200, #""names":["MGTEST_SECRET","OTHER"]"#),
            ("PATCH", #"{"add":"MGTEST_SECRET"}"#, 200, #""names":["MGTEST_SECRET","OTHER"]"#),
            ("PATCH", #"{"add":"FOO_API_KEY"}"#, 400, "is never passed to agents"),
            ("PATCH", #"{"add":"1X"}"#, 400, "is not a variable name"),
            ("PATCH", #"{"remove":"NOPE"}"#, 400, "agentEnv: NOPE is not in the list"),
            ("PATCH", #"{"remove":"OTHER"}"#, 200, #""names":["MGTEST_SECRET"]"#),
            ("PATCH", #"{"add":"A","remove":"B"}"#, 400, "bad JSON body"),
            ("PATCH", "{}", 400, "bad JSON body"),
            ("PATCH", #"{"names":["X","Y"]}"#, 200, #""names":["X","Y"]"#),
            ("PATCH", #"{"names":["Z"]}"#, 200, #""names":["Z"]"#),
            ("PATCH", #"{"names":["PATH","Z"]}"#, 200, #""unset":["Z"]"#),  // D: PATH is set, Z is not
            ("PATCH", #"{"names":["Z"]}"#, 200, #""names":["Z"]"#),
        ] {
            let w = await admin(r, method, "/agents/env", body: body)
            #expect(w.status == status && w.text.contains(want), "\(method) \(body): \(w.text)")
        }
        #expect(app.lock.withLock { app.config.agentEnv } == "Z")
    }
}
