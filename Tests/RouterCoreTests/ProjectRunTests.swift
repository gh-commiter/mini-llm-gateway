import Foundation
import Synchronization
import Testing
@testable import RouterCore

// 0.6.0 adapter duties 6, 7 and 8 on the Mac: project admission of an agent request, the run port's notes (`run` kind), conversation
// scope and agent homes, usage events, the epoch. The REAL minigw-core process (HelperBuild of CoreHelperTests) over a temp store, fake agent
// CLIs (shell scripts) in a temp folder, a temp Scratch. Nothing of the real app support folder, core store, Keychain or settings is touched.
// Globals shared with other suites: Router.shares / Router.sessions registries (keys carry this test's own unique folder, so they do not
// collide) and Router.agentPeer is not used here. The tests of this file run in one serialized suite: each starts a helper process and agents.

private func jsonObject(_ r: CoreResponse) -> [String: Any] { (try? JSONSerialization.jsonObject(with: r.body) as? [String: Any]) ?? [:] }

/// One gateway on a temp folder: a helper with its own store, a Router with fake agents, a client folder.
private final class PFx: @unchecked Sendable {
    let base = FileManager.default.temporaryDirectory.appendingPathComponent("projrun-" + UUID().uuidString, isDirectory: true)
    var client: URL { base.appendingPathComponent("client") }
    var bin: URL { base.appendingPathComponent("bin") }
    var scratch: URL { base.appendingPathComponent("scratch") }
    let h: CoreHelper
    var router: Router!
    var uid: [String: Int] = [:], pid: [String: Int] = [:], nid: [String: Int] = [:]

    init(backoff: [Double] = [0.05, 0.1, 0.2]) async throws {
        let store = base.appendingPathComponent("store/core")
        try CoreHelper.prepareStoreFolder(store)
        h = CoreHelper(binary: try await HelperBuild.shared.binary(), store: store, ownerName: "tester", marks: MemoryMarks(), backoff: backoff, requestTimeout: 10)
        for d in [client, bin, scratch] { try FileManager.default.createDirectory(at: d, withIntermediateDirectories: true) }
        h.start()
        #expect(await h.waitReady())
    }

    /// For failure messages: a 503 core_unavailable means the helper was not answering. pid nil = not running; exit = wait status of the last helper
    /// (9 = killed by SIGKILL from outside, 11/6 = crashed, 0 = quit by itself); restarts > 0 = the supervisor started it again during this test.
    var diag: String {
        let st = h.lastExit.map { $0 & 0x7f == 0 ? "exit code \(($0 >> 8) & 0xff)" : "signal \($0 & 0x7f)" } ?? "none"
        return "helper pid \(h.pid.map(String.init) ?? "nil") ready \(h.isReady) restarts \(h.restarts) last exit \(st) last failure '\(h.lastFailure)' fatal \(String(describing: h.fatalError)) binary \(FileManager.default.isExecutableFile(atPath: h.binaryPath) ? "present" : "MISSING")"
    }

    deinit { h.stop(); try? FileManager.default.removeItem(at: base) }

    func script(_ name: String, _ body: String) throws -> String {
        let u = bin.appendingPathComponent(name)
        try Data(("#!/bin/sh\n" + body).utf8).write(to: u)
        try FileManager.default.setAttributes([.posixPermissions: 0o755], ofItemAtPath: u.path)
        return u.path
    }

    func admin(_ method: String, _ path: String, _ body: String = "", query: String = "") async -> CoreResponse {
        await h.call(context: .admin, request: CoreRequest(method: method, path: path, query: query, body: Data(body.utf8)))
    }

    /// A user bound to a device "dev-<name>" (approved in the Router's config), a member of the projects with the role.
    func user(_ name: String, projects: [String: String] = [:]) async {
        let u = ((jsonObject(await admin("POST", "/users", #"{"name":"\#(name)"}"#))["user"] as? [String: Any])?["id"] as? Int) ?? 0
        uid[name] = u
        _ = await admin("PUT", "/bindings/dev-\(name)", #"{"user":\#(u)}"#)
        for (slug, role) in projects { _ = await admin("PUT", "/projects/\(pid[slug]!)/members/\(u)", #"{"role":"\#(role)"}"#) }
    }

    func project(_ slug: String) async {
        pid[slug] = ((jsonObject(await admin("POST", "/projects", #"{"slug":"\#(slug)","name":"\#(slug)"}"#))["project"] as? [String: Any])?["id"] as? Int) ?? 0
    }

    func note(_ project: String, _ cls: String, by user: String = "ana") async -> Int {
        let r = await h.call(context: CoreContext(kind: .member, device: "dev-\(user)", approved: true),
                             request: CoreRequest(method: "POST", path: "/minigw/v1/projects/\(pid[project]!)/notes", header: ["Idempotency-Key": [UUID().uuidString]], body: Data(#"{"class":"\#(cls)","title":"\#(project)-\#(cls)","body":"text of \#(project) \#(cls)"}"#.utf8)))
        let id = jsonObject(r)["id"] as? Int ?? 0
        #expect(r.status == 201 && id > 0, "creating a \(cls) note of \(project): \(r.status) \(String(decoding: r.body, as: UTF8.self))")
        nid["\(project)-\(cls)"] = id
        return id
    }

    /// The Router with the fake agents; `ask`: "May ask other models" for Claude and Codex.
    func makeRouter(ask: Bool = false, devices: [String] = ["ana"], claude: String? = nil, codex: String? = nil, noCore: Bool = false) {
        var c = RouterConfig()
        c.scratchFolder = scratch.path
        c.devices = devices.map { DeviceConfig(id: "dev-\($0)", name: $0) }
        c.claudeCLI.allowedRoots = client.path  // not base: Scratch is inside it (an allowed folder may not contain Scratch)
        c.claudeCLI.enabled = claude != nil; c.claudeCLI.executable = claude ?? ""
        c.claudeCLI.askOthers = ask; c.claudeCLI.allowLocalTools = true
        c.codex.enabled = codex != nil; c.codex.executable = codex ?? ""; c.codex.askOthers = ask
        router = Router(config: c, token: "tok", secrets: FakeSecrets(values: [:]), core: noCore ? nil : h, history: GatewayHistory())
    }

    /// An agent request of a device (proven in its tunnel) for a model; `project`: the id the client's proxy puts in the header.
    @discardableResult
    func ask(_ model: String = "claude-cli-haiku", project: Int? = nil, device: String? = "ana", session: String = UUID().uuidString,
             messages: String = #"[{"role":"user","content":"go"}]"#, headers: [String: String] = [:]) async -> FakeWriter {
        var h = ["Authorization": "Bearer tok", "x-claude-mini-session": session, "x-claude-mini-cwd": client.path]
        if let project { h["x-minigw-project"] = String(project) }
        var q = request("POST", "/v1/messages", headers: h.merging(headers) { $1 },
                        body: #"{"model":"\#(model)","stream":false,"tools":[{"name":"x"}],"messages":\#(messages)}"#)
        q.device = device.map { Noise.Device(id: "dev-\($0)", name: $0) }
        let w = FakeWriter()
        await router.handle(q, writer: w)
        return w
    }

    func read(_ name: String) -> String? { (try? String(contentsOf: client.appendingPathComponent(name), encoding: .utf8))?.trimmingCharacters(in: .whitespacesAndNewlines) }
    func exists(_ name: String) -> Bool { FileManager.default.fileExists(atPath: client.appendingPathComponent(name).path) }
    func write(_ name: String, _ text: String) { try? Data(text.utf8).write(to: client.appendingPathComponent(name)) }

    func waitFor(_ name: String, seconds: Double = 30) async -> Bool {
        for _ in 0..<Int(seconds * 50) where !exists(name) { try? await Task.sleep(for: .milliseconds(20)) }
        return exists(name)
    }

    /// The usage report of the core (admin, last day): rows by day, model, user and project.
    func usage() async -> [[String: Any]] {
        if router != nil { _ = await router.usageSender.flush() }
        // the helper's recorder writes asynchronously: the rows are read until they stop changing
        var last: [[String: Any]] = [], same = 0
        for _ in 0..<100 where same < 4 {
            let rows = jsonObject(await admin("GET", "/usage/events", query: "days=1"))["days"] as? [[String: Any]] ?? []
            same = NSArray(array: rows).isEqual(to: last) ? same + 1 : 0
            last = rows
            try? await Task.sleep(for: .milliseconds(30))
        }
        return last
    }

    func gaps() async -> Int {
        let g = jsonObject(await admin("GET", "/usage/events", query: "days=1"))["gaps"] as? [[String: Any]] ?? []
        return g.reduce(0) { $0 + ($1["count"] as? Int ?? 0) }
    }

    func events(_ rows: [[String: Any]], model: String, project: Int = 0) -> Int {
        rows.filter { $0["model"] as? String == model && ($0["project"] as? Int ?? 0) == project }.reduce(0) { $0 + ($1["events"] as? Int ?? 0) }
    }
}

/// A fake Claude CLI: leaves `ran.txt` and the run port's address, then does the steps of `plan.txt` (one per line) against the run port with
/// curl and writes `res-<n>.code` and `res-<n>.body`. Lines: `METHOD PATH [NOAUTH | header:value]`, `SIGNAL` (creates `ready`), `WAIT` (until
/// `gate` exists), `ASK <model>` (a nested model request, code only).
private let fakeClaude = """
[ "$1" = --version ] && { echo '2.1.0 (Claude Code)'; exit 0; }
cat > /dev/null
echo ran >> ran.txt
echo "${MINIGW_AGENT_URL:-none}" > port.txt
echo "${MINIGW_AGENT_AUTH_FILE:-none}" > authfile.txt
if [ -f plan.txt ]; then
  set -f; i=0
  while IFS= read -r line; do
    i=$((i+1)); set -- $line
    case "$1" in
      SIGNAL) : > ready; continue ;;
      WAIT) while [ ! -e gate ]; do sleep 0.05; done; continue ;;
    esac
    tok=$(cat "$MINIGW_AGENT_AUTH_FILE" 2>/dev/null)
    if [ "$1" = ASK ]; then
      code=$(curl -s -o res-$i.body -w '%{http_code}' -H "Authorization: Bearer $tok" -H 'content-type: application/json' \\
        -d "{\\"model\\":\\"$2\\",\\"max_tokens\\":10,\\"tools\\":[{\\"name\\":\\"x\\"}],\\"messages\\":[{\\"role\\":\\"user\\",\\"content\\":\\"hi\\"}]}" "$MINIGW_AGENT_URL/v1/messages")
    elif [ "$3" = NOAUTH ]; then
      code=$(curl -s -o res-$i.body -w '%{http_code}' -X "$1" "$MINIGW_AGENT_URL$2")
    elif [ -n "$3" ]; then
      code=$(curl -s -o res-$i.body -w '%{http_code}' -X "$1" -H "Authorization: Bearer $tok" -H "$3" "$MINIGW_AGENT_URL$2")
    else
      code=$(curl -s -o res-$i.body -w '%{http_code}' -X "$1" -H "Authorization: Bearer $tok" "$MINIGW_AGENT_URL$2")
    fi
    echo "$code" > res-$i.code
  done < plan.txt
fi
echo '{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"done"}}}'
echo '{"type":"result","subtype":"success","usage":{"input_tokens":5,"output_tokens":3}}'

"""

/// A fake Codex: one `call-<n>.args` and `call-<n>.env` per run; a new thread id each time; the answer "codex answer".
private let fakeCodex = """
out=""; prev=""
for a in "$@"; do [ "$prev" = "-o" ] && out="$a"; prev="$a"; done
cat > /dev/null
n=$(ls call-*.args 2>/dev/null | wc -l | tr -d ' ')
printf '%s\\n' "$@" > call-$n.args
env > call-$n.env
echo '{"type":"thread.started","thread_id":"T-'$$'"}'
echo "codex answer" > "$out"

"""

@Suite(.serialized) struct ProjectRunTests {
    // MARK: D7 admission

    @Test func agentRequestIsAdmittedOnlyForADirectMemberAndRefusedLocallyWithTheFixedBodies() async throws {
        let f = try await PFx()
        let helper = { "helper ready \(f.h.isReady) restarts \(f.h.restarts) last failure \(f.h.lastFailure)" }  // #109: a 503 or a failed run = the helper was down
        f.makeRouter(claude: try f.script("claude", fakeClaude))
        await f.project("fakture"); await f.project("other"); await f.project("old")
        await f.user("ana", projects: ["fakture": "developer", "old": "developer"])
        _ = await f.admin("PATCH", "/projects/\(f.pid["old"]!)", #"{"archived":true}"#)
        // an archived project is admitted, as in Go (archiving only stops writes; the link-check of the core accepts it): the run reads
        let archived = await f.ask(project: f.pid["old"])
        #expect(archived.status == 200, "archived: \(archived.status) \(archived.text) \(helper())")
        // a member: the run goes
        let ok = await f.ask(project: f.pid["fakture"])
        #expect(ok.status == 200 && f.exists("ran.txt"), "\(ok.text) \(helper())")
        // no header: today's behaviour, no project, no port (the ask switch is off)
        let plain = await f.ask()
        #expect(plain.status == 200 && f.read("port.txt") == "none", "\(plain.status) \(plain.text) port \(f.read("port.txt") ?? "-") \(helper())")
        let ranBefore = f.read("ran.txt")
        // refused: not a member, no such project, a header that is no id, no device in the tunnel; one fixed 403 body, the agent never runs
        for (name, w) in [("non-member", await f.ask(project: f.pid["other"])),
                          ("no such project", await f.ask(project: 9999)),
                          ("no device", await f.ask(project: f.pid["fakture"], device: nil)),
                          ("not an id", await f.ask(project: nil, headers: ["x-minigw-project": "fakture"])),
                          ("unlisted device", await f.ask(project: f.pid["fakture"], device: "stranger"))] {
            #expect(w.status == 403 && w.text.contains(#""type":"no_project_access""#) && w.text.contains(#""message":"No project access""#)
                    && w.text.contains(#""type":"error""#) && w.text.contains("x-should-retry: false"),
                    "\(helper()) | \(name): \(w.status) \(w.text.prefix(400))")  // helper state first: the failure report cuts long messages
        }
        #expect(f.read("ran.txt") == ranBefore, "\(helper())")  // none of them started an agent
        // the fixed refusal is the body asserted by Go (testdata/no_project_access.json) inside the Anthropic shape
        let fixed = String(decoding: try Data(contentsOf: URL(fileURLWithPath: #filePath).deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent()
            .appendingPathComponent("client/coreapi/testdata/no_project_access.json")), as: UTF8.self)
        #expect(fixed.contains("no_project_access"))
        // other agent kinds go through the same admission
        f.makeRouter(claude: try f.script("claude", fakeClaude), codex: try f.script("codex", fakeCodex))
        let codexRefused = await f.ask("claude-codex", project: f.pid["other"])
        #expect(codexRefused.status == 403 && !f.exists("call-0.args"), "\(codexRefused.status) \(codexRefused.text) \(helper())")
    }

    @Test func helperDownIsA503AndNeverARunWithoutTheProject() async throws {
        let f = try await PFx(backoff: [5])
        await f.project("fakture"); await f.user("ana", projects: ["fakture": "developer"])
        f.makeRouter(claude: try f.script("claude", fakeClaude))
        kill(f.h.pid!, SIGKILL)
        for _ in 0..<200 where f.h.isReady { try await Task.sleep(for: .milliseconds(10)) }
        let down = await f.ask(project: f.pid["fakture"])
        #expect(down.status == 503 && down.text.contains(#""type":"core_unavailable""#) && down.text.contains("x-should-retry: false") && !f.exists("ran.txt"), "\(down.text)")
        // models keep working without a project while the helper is down
        let plain = await f.ask()
        #expect(plain.status == 200 && f.read("ran.txt") == "ran")
        // no core at all (a Router without a helper): the same refusal
        f.makeRouter(claude: try f.script("claude", fakeClaude), noCore: true)
        let none = await f.ask(project: f.pid["fakture"])
        #expect(none.status == 503 && none.text.contains("core_unavailable"))
    }

    @Test func approvedIsTheListNowAndANestedCallNeverNamesAProject() async throws {
        let f = try await PFx()
        await f.project("fakture"); await f.user("ana", projects: ["fakture": "developer"])
        f.makeRouter(devices: [], claude: try f.script("claude", fakeClaude))  // the device is bound but not on the approved list
        // this Mac's loopback lets an unlisted device use the models (the 0.5.x exception) but never a project: approved is the list NOW
        let w = await f.ask(project: f.pid["fakture"])
        #expect(w.status == 403 && w.text.contains(#""type":"no_project_access""#) && !f.exists("ran.txt"), "\(w.text)")
        #expect(await f.ask().status == 200 && f.read("ran.txt") == "ran")  // without a project the same device is served as before
        // a nested call never names a project: the run's own is inherited (the header is dropped; see the run port rows)
    }

    // MARK: D6 run port

    private func runFixture() async throws -> (PFx, [String: Int]) {
        let f = try await PFx()
        for p in ["eureko", "fakture", "side"] { await f.project(p) }
        await f.user("ana", projects: ["eureko": "developer", "fakture": "developer", "side": "developer"])
        let own = await f.note("eureko", "data"), priv = await f.note("eureko", "private")
        let fs = await f.note("fakture", "structure"), fd = await f.note("fakture", "data"), side = await f.note("side", "data")
        _ = await f.admin("POST", "/grants", #"{"source":\#(f.pid["fakture"]!),"reader":\#(f.pid["eureko"]!),"class":"structure"}"#)
        return (f, ["own": own, "priv": priv, "fs": fs, "fd": fd, "side": side])
    }

    private func noteIDs(_ body: String?) -> Set<Int> {
        let o = (try? JSONSerialization.jsonObject(with: Data((body ?? "").utf8))) as? [String: Any]
        return Set((o?["notes"] as? [[String: Any]] ?? []).compactMap { $0["id"] as? Int })
    }

    @Test func runPortServesOnlyItsOwnProjectsNotesAndGrantsAndRefusesEverythingElse() async throws {
        let (f, n) = try await runFixture()
        f.makeRouter(claude: try f.script("claude", fakeClaude))  // "May ask other models" is off: the notes capability exists anyway
        let plan = [
            "GET /minigw/v1/notes",                                              // 1 the run's list
            "GET /minigw/v1/notes/\(n["own"]!)",                                 // 2 own project note
            "GET /minigw/v1/notes/\(n["fs"]!)",                                  // 3 fakture structure through the grant
            "GET /minigw/v1/notes/\(n["fd"]!)",                                  // 4 fakture data: no
            "GET /minigw/v1/notes/\(n["priv"]!)",                                // 5 a private note of the run's own project: no
            "GET /minigw/v1/notes/\(n["side"]!)",                                // 6 a project the user also belongs to: no
            "GET /minigw/v1/projects/\(f.pid["side"]!)/notes",                   // 7 the project id in the path
            "GET /minigw/v1/notes?project=\(f.pid["side"]!)&project_id=\(f.pid["side"]!)",  // 8 in the query: ignored
            "GET /minigw/v1/notes x-minigw-project:\(f.pid["side"]!)",           // 9 in a header: ignored
            "POST /minigw/v1/notes",                                             // 10 a write
            "PUT /minigw/v1/notes/\(n["own"]!)",                                 // 11
            "DELETE /minigw/v1/notes/\(n["own"]!)",                              // 12
            "POST /v1/messages",                                                 // 13 models: ask is off
            "GET /v1/models",                                                    // 14
            "GET /minigw/v1/projects",                                           // 15 member routes through the run port
            "GET /minigw/v1/usage",                                              // 16
            "GET /minigw/admin/v1/status",                                       // 17
            "GET /minigw/v1/notes NOAUTH",                                       // 18 no token
            "GET /minigw/v1/notes authorization:Bearer\\ tok",                   // 19 the gateway's token is not the run's
            "HEAD /minigw/v1/notes",                                             // 20 only GET
        ]
        f.write("plan.txt", plan.joined(separator: "\n") + "\n")
        let w = await f.ask(project: f.pid["eureko"])
        #expect(w.status == 200, "\(w.text) | \(f.diag)")
        func code(_ i: Int) -> String? { f.read("res-\(i).code") }
        let want = [1: "200", 2: "200", 3: "200", 4: "404", 5: "404", 6: "404", 7: "404", 8: "200", 9: "200", 10: "404", 11: "404", 12: "404", 13: "404",
                    14: "404", 15: "404", 16: "404", 17: "404", 18: "401", 19: "401"]
        let helperState = f.diag
        for (i, c) in want { #expect(code(i) == c, "step \(i): \(code(i) ?? "-") \(f.read("res-\(i).body") ?? "") want \(c) | \(helperState)") }
        let list = noteIDs(f.read("res-1.body"))
        #expect(list == [n["own"]!, n["fs"]!])  // own project's data and the grant's structure; never private, never the other project's
        for i in [8, 9] { #expect(noteIDs(f.read("res-\(i).body")) == list, "forged ids change nothing (step \(i))") }
        #expect(f.read("res-2.body")?.contains("text of eureko data") == true && f.read("res-3.body")?.contains("text of fakture structure") == true)
        // a refused request is the fixed body, one of the 404 "not available to agents" shapes for the rest
        #expect(f.read("res-4.body")?.contains("not_found") == true)
        // the agent was told about notes, not about other models; the port exists
        #expect(f.read("port.txt")?.hasPrefix("http://127.0.0.1:") == true)
        // the capability ends with the run: the port is closed and the token file is gone
        let port = try #require(f.read("port.txt")), auth = try #require(f.read("authfile.txt"))
        #expect(!FileManager.default.fileExists(atPath: auth))
        var r = URLRequest(url: URL(string: port + "/minigw/v1/notes")!)
        r.timeoutInterval = 3
        let gone = (try? await URLSession(configuration: .ephemeral).data(for: r)) == nil
        #expect(gone)
    }

    @Test func aRunWithAProjectHasThePortAlsoWithoutAskAndTheModelsOnlyWithIt() async throws {
        let (f, _) = try await runFixture()
        f.makeRouter(ask: true, claude: try f.script("claude", fakeClaude))
        f.write("plan.txt", "GET /v1/models\nGET /minigw/v1/notes\nGET /minigw/v1/projects\n")
        #expect(await f.ask(project: f.pid["eureko"]).status == 200)
        #expect(f.read("res-1.code") == "200" && f.read("res-2.code") == "200" && f.read("res-3.code") == "404")  // models with ask, notes always, nothing else
        // no project and no ask: no port at all, as in 0.5.26
        f.makeRouter(claude: try f.script("claude", fakeClaude))
        try FileManager.default.removeItem(at: f.client.appendingPathComponent("port.txt"))
        #expect(await f.ask().status == 200)
        #expect(f.read("port.txt") == "none" && f.read("authfile.txt") == "none")
    }

    @Test func runNotesUseTheRunsCapabilityAndTheApprovalNow() async throws {
        let (f, n) = try await runFixture()
        f.makeRouter()  // dev-ana is on the approved list
        let adm = await f.h.internalCall("admit", ["device": "dev-ana", "approved": true, "project": String(f.pid["eureko"]!)])
        let o = jsonObject(adm)
        let p = ProjectRun(runId: o["runId"] as! String, user: o["user"] as! Int, device: "dev-ana", project: o["project"] as! Int, epoch: o["epoch"] as? Int ?? 0)
        func read(_ router: Router, _ path: String) async -> (Int, String) {
            let w = FakeWriter()
            _ = await router.runNotes(request("GET", path), cap: p.cap, rid: "run_x", writer: w)
            return (w.status, w.text)
        }
        let good = await read(f.router, "/minigw/v1/notes/\(n["own"]!)")
        #expect(good.0 == 200 && good.1.contains("request-id: run_x"))
        // the device is not on the approved list NOW: the core refuses (a Router whose list no longer has it)
        f.makeRouter(devices: [])
        let gone = await read(f.router, "/minigw/v1/notes/\(n["own"]!)")
        #expect(gone.0 == 403 && gone.1.contains("no_project_access"))
        // the core is not reachable: the 503
        f.makeRouter(noCore: true)
        #expect(await read(f.router, "/minigw/v1/notes").0 == 503)
    }

    // MARK: epoch

    @Test(arguments: ["epoch bumped", "device removed", "binding changed", "membership removed", "user deactivated", "role lowered"])
    func aChangeBetweenReadsEndsTheRunsNoteReadsWithOneFixedBody(change: String) async throws {
        let (f, n) = try await runFixture()
        f.makeRouter(claude: try f.script("claude", fakeClaude))
        await f.user("dana", projects: [:])
        let read = "GET /minigw/v1/notes/\(n["own"]!)"
        f.write("plan.txt", [read, "SIGNAL", "WAIT", read, "GET /minigw/v1/notes"].joined(separator: "\n") + "\n")
        let run = Task { await f.ask(project: f.pid["eureko"]) }
        #expect(await f.waitFor("ready"))
        switch change {
        case "epoch bumped": _ = await f.h.internalCall("device-revoked", ["device": "dev-nobody"])
        case "device removed": _ = await f.h.revoke("dev-ana")
        case "binding changed": _ = await f.admin("PUT", "/bindings/dev-ana", #"{"user":\#(f.uid["dana"]!)}"#)
        case "membership removed": _ = await f.admin("DELETE", "/projects/\(f.pid["eureko"]!)/members/\(f.uid["ana"]!)")
        case "user deactivated": _ = await f.admin("PATCH", "/users/\(f.uid["ana"]!)", #"{"active":false}"#)
        default: _ = await f.admin("PUT", "/projects/\(f.pid["eureko"]!)/members/\(f.uid["ana"]!)", #"{"role":"reader"}"#)
        }
        f.write("gate", "x")
        let w = await run.value
        #expect(w.status == 200)  // the run itself goes on
        #expect(f.read("res-1.code") == "200")
        for i in [4, 5] {  // the next reads of the old run fail, one fixed body
            #expect(f.read("res-\(i).code") == "403" && f.read("res-\(i).body")?.contains(#""type":"no_project_access""#) == true, "\(change) step \(i): \(f.read("res-\(i).code") ?? "-")")
        }
    }

    @Test func aRunAfterAnEpochBumpIsAdmittedAgainButTheOldOneStaysDead() async throws {
        let (f, n) = try await runFixture()
        f.makeRouter(claude: try f.script("claude", fakeClaude))
        let read = "GET /minigw/v1/notes/\(n["own"]!)"
        f.write("plan.txt", [read, "SIGNAL", "WAIT", read].joined(separator: "\n") + "\n")
        let old = Task { await f.ask(project: f.pid["eureko"]) }
        #expect(await f.waitFor("ready"))
        _ = await f.h.revoke("dev-ana")  // the device is removed (the epoch goes up) and, as it is on the approved list again here, approved again
        f.write("gate", "x")
        #expect(await old.value.status == 200)
        #expect(f.read("res-1.code") == "200" && f.read("res-4.code") == "403")  // the earlier run stays dead
        // a new run (the agents of one folder run one at a time, so after the first) works at the new epoch
        f.write("plan.txt", read + "\n")
        try FileManager.default.removeItem(at: f.client.appendingPathComponent("res-1.code"))
        let fresh = await f.ask(project: f.pid["eureko"])
        #expect(fresh.status == 200 && f.read("res-1.code") == "200")
    }

    // MARK: scope and homes

    private let conversation = #"[{"role":"user","content":"go"},{"role":"assistant","content":"codex answer"},{"role":"user","content":"more"}]"#

    private func homeOf(_ f: PFx, _ call: Int) -> String? {
        f.read("call-\(call).env")?.split(separator: "\n").first { $0.hasPrefix("CODEX_HOME=") }.map { String($0.dropFirst("CODEX_HOME=".count)) }
    }

    @Test func aConversationIsNotResumedAcrossProjectsEpochsOrWithoutAProjectAndEachRunHasItsOwnHome() async throws {
        let f = try await PFx()
        await f.project("fakture"); await f.project("other")
        await f.user("ana", projects: ["fakture": "developer", "other": "developer"])
        f.makeRouter(claude: try f.script("claude", fakeClaude), codex: try f.script("codex", fakeCodex))
        let session = UUID().uuidString
        let first = #"[{"role":"user","content":"go"}]"#
        func resumed(_ call: Int) -> Bool { f.read("call-\(call).args")?.contains("resume") == true }
        // call 0: a conversation of project fakture; call 1 continues it (same user, project, epoch): resumed, same home
        #expect(await f.ask("claude-codex", project: f.pid["fakture"], session: session, messages: first).status == 200)
        #expect(await f.ask("claude-codex", project: f.pid["fakture"], session: session, messages: conversation).status == 200)
        #expect(!resumed(0) && resumed(1))
        let home = try #require(homeOf(f, 0))
        #expect(homeOf(f, 1) == home)
        #expect(home.contains("/state/") && home.contains("/proj/\(f.uid["ana"]!)-\(f.pid["fakture"]!)-e") && home.hasSuffix("/codex"))
        // the same conversation under another project, or with no project: not resumed, another home (none for no project)
        #expect(await f.ask("claude-codex", project: f.pid["other"], session: session, messages: conversation).status == 200)
        #expect(await f.ask("claude-codex", project: nil, session: session, messages: conversation).status == 200)
        #expect(!resumed(2) && !resumed(3))
        #expect(homeOf(f, 2) != nil && homeOf(f, 2) != home && homeOf(f, 3) == nil)
        // an epoch bump: the same project and conversation is not resumed, a new home, and the dead one is removed
        _ = await f.h.revoke("dev-nobody")
        #expect(await f.ask("claude-codex", project: f.pid["fakture"], session: session, messages: conversation).status == 200)
        #expect(!resumed(4))
        let home2 = try #require(homeOf(f, 4))
        #expect(home2 != home && !FileManager.default.fileExists(atPath: URL(fileURLWithPath: home).deletingLastPathComponent().path))
        #expect(FileManager.default.fileExists(atPath: home2))
        // a project run's files are in no agent's reach: every kind's deny list holds the folder of the homes, Codex got it in its profile
        for kind in ["codex", "grok", "copilot", "claude"] {
            #expect(RouterConfig.agentDenied(kind: kind, scratch: f.scratch).contains(f.scratch.appendingPathComponent("state").standardizedFileURL.path), "\(kind)")
        }
        #expect(f.read("call-4.args")?.contains("/state\"=\"none\"") == true)
    }

    @Test func aClaudeConversationIsKeyedByUserProjectAndEpochToo() async throws {
        let f = try await PFx()
        await f.project("fakture"); await f.project("other")
        await f.user("ana", projects: ["fakture": "developer", "other": "developer"])
        let counter = try f.script("claude", """
        [ "$1" = --version ] && { echo '2.1.0 (Claude Code)'; exit 0; }
        cat > /dev/null
        printf '%s\\n' "$@" | grep -A1 -e '^--resume$' -e '^--session-id$' | tr '\\n' ' ' >> modes.txt; echo >> modes.txt
        echo '{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"done"}}}'
        echo '{"type":"result","subtype":"success","usage":{"output_tokens":3}}'
        """)
        f.makeRouter(claude: counter)
        let session = UUID().uuidString
        let first = #"[{"role":"user","content":"go"}]"#, again = #"[{"role":"user","content":"go"},{"role":"assistant","content":"done"},{"role":"user","content":"more"}]"#
        _ = await f.ask(project: f.pid["fakture"], session: session, messages: first)
        _ = await f.ask(project: f.pid["fakture"], session: session, messages: again)   // resumed
        _ = await f.ask(project: f.pid["other"], session: session, messages: again)     // another project: new session
        _ = await f.ask(project: nil, session: session, messages: again)                 // no project: new session
        _ = await f.h.revoke("dev-nobody")
        _ = await f.ask(project: f.pid["fakture"], session: session, messages: again)   // another epoch: new session
        let modes = (f.read("modes.txt") ?? "").split(separator: "\n").map(String.init)
        try #require(modes.count == 5, "\(modes)")
        #expect(modes[0].contains("--session-id") && modes[1].contains("--resume") && modes[2].contains("--session-id") && modes[3].contains("--session-id") && modes[4].contains("--session-id"), "\(modes)")
    }

    @Test func conversationScopeAndAgentHomeComeFromTheCore() async throws {
        let f = try await PFx()
        f.makeRouter()
        // no project: the 0.5.26 key byte for byte, with no helper call
        #expect(await f.router.conversationScope("agent", ["codex", "dev", "/x"], project: nil) == "codex|dev|/x")
        let p = ProjectRun(runId: "r", user: 2, device: "dev", project: 3, epoch: 4)
        let key = try #require(await f.router.conversationScope("agent", ["codex", "dev", "/x"], project: p))
        #expect(key == "\u{0}project\u{0}" + #"["agent","codex","dev","/x",2,3,4]"#)  // coreapi.ConversationScope (api_test.go asserts it)
        #expect(await f.router.conversationScope("agent", ["codex", "dev", "/x"], project: ProjectRun(runId: "r", user: 2, device: "dev", project: 3, epoch: 5)) != key)
        #expect(await f.router.conversationScope("cli", ["codex", "dev", "/x"], project: p) != key)
        // the home: <Scratch>/state/<hash of the device>/proj/<user>-<project>-e<epoch>/<kind>; an older epoch of the same user and project is removed
        let home = try #require(await f.router.projectHome(kind: "codex", owner: "dev", project: p))
        #expect(home.path.hasSuffix("/proj/2-3-e4/codex") && FileManager.default.fileExists(atPath: home.path))
        #expect((try? FileManager.default.attributesOfItem(atPath: home.path)[.posixPermissions] as? Int) == 0o700)
        let old = try #require(await f.router.projectHome(kind: "codex", owner: "dev", project: ProjectRun(runId: "r", user: 2, device: "dev", project: 3, epoch: 3)))
        Router.releaseProjectHome(old)  // its run is over (F8: a home of a run that is going is not pruned)
        #expect(old.path.hasSuffix("/proj/2-3-e3/codex") && FileManager.default.fileExists(atPath: old.path))  // a lower epoch is created for a stale run, and pruned at the next higher one
        let again = try #require(await f.router.projectHome(kind: "codex", owner: "dev", project: p))
        #expect(again == home && !FileManager.default.fileExists(atPath: old.path))
        // another device: another folder
        let other = try #require(await f.router.projectHome(kind: "codex", owner: "dev2", project: p))
        #expect(other.deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent() != home.deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent())
        // the core cannot answer: nil, the run does not start
        f.makeRouter(noCore: true)
        #expect(await f.router.conversationScope("agent", ["a"], project: p) == nil)
        #expect(await f.router.projectHome(kind: "codex", owner: "dev", project: p) == nil)
    }

    @Test func homesAreBoundedPerDevice() async throws {
        let f = try await PFx()
        f.makeRouter()
        for i in 1...25 { await f.router.projectHome(kind: "codex", owner: "dev", project: ProjectRun(runId: "r", user: i, device: "dev", project: 1, epoch: 1)).map(Router.releaseProjectHome) }
        let state = f.router.config.scratch("state")
        let dev = try #require(try FileManager.default.contentsOfDirectory(atPath: state.path).first)
        let homes = try FileManager.default.contentsOfDirectory(atPath: state.appendingPathComponent(dev + "/proj").path)
        #expect(homes.count <= 20)  // maxHomes of the core's agent-home
    }

    /// Grok is not supported on the Mac gateway: every Grok request is refused (400 invalid_request_error, the client adds "try again" to a 502/503), no retry, with or
    /// without a project core, and Grok is not listed among the models.
    @Test func grokIsRefusedForEveryRun() async throws {
        let f = try await PFx()
        await f.project("fakture"); await f.user("ana", projects: ["fakture": "developer"])
        f.makeRouter()
        for project in [f.pid["fakture"], nil] {
            let w = await f.ask("claude-grok", project: project)
            #expect(w.status == 400 && w.text.contains("invalid_request_error") && w.text.contains("x-should-retry: false") && w.text.contains("Grok is not supported on the Mac gateway"), "\(w.text.prefix(300))")
        }
        f.makeRouter(noCore: true)  // no core: the same
        let w = await f.ask("claude-grok-grok-4.7", project: nil)
        #expect(w.status == 400 && w.text.contains("Use another agent, or Grok through minigw serve on Windows or Linux"), "\(w.text.prefix(300))")
        let list = await f.router.modelList(siri: false)
        #expect(!((list["data"] as? [[String: Any]]) ?? []).contains { ($0["id"] as? String)?.hasPrefix("claude-grok") == true })
    }

    @Test func codexProjectHomeLinksTheLoginAndCarriesAReplacedOneBack() throws {
        let base = FileManager.default.temporaryDirectory.appendingPathComponent("auth-" + UUID().uuidString)
        defer { try? FileManager.default.removeItem(at: base) }
        let real = base.appendingPathComponent("real"), home = base.appendingPathComponent("home")
        for d in [real, home] { try FileManager.default.createDirectory(at: d, withIntermediateDirectories: true) }
        let auth = home.appendingPathComponent("auth.json"), realAuth = real.appendingPathComponent("auth.json")
        try Data(#"{"tokens":"LOGIN1"}"#.utf8).write(to: realAuth)
        Router.linkProjectAuth(home: home, real: real)
        #expect((try? FileManager.default.destinationOfSymbolicLink(atPath: auth.path)) == realAuth.path)
        #expect(!FileManager.default.fileExists(atPath: home.appendingPathComponent("config.toml").path))  // nothing to link: skipped
        // the CLI replaced the link by a new file (a write by rename): it goes back to the real folder, the link is made again
        let start = Date().addingTimeInterval(-1)
        try FileManager.default.removeItem(at: auth)
        try Data(#"{"tokens":"LOGIN2"}"#.utf8).write(to: auth)
        Router.restoreProjectAuth(home: home, real: real, since: start)
        #expect((try? String(contentsOf: realAuth, encoding: .utf8)) == #"{"tokens":"LOGIN2"}"#)
        #expect((try? FileManager.default.destinationOfSymbolicLink(atPath: auth.path)) == realAuth.path)
        #expect((try? FileManager.default.attributesOfItem(atPath: realAuth.path)[.posixPermissions] as? Int) == 0o600)
        #expect(try FileManager.default.contentsOfDirectory(atPath: real.path).sorted() == ["auth.json"])  // no temporary file left
    }

    /// sec-fix F6: what the carry-back refuses (the real login stays as it was, the home gets the link again) and the refresh at run start.
    @Test func carryBackRefusesWhatIsNotAFreshSmallJSONFileOfTheUser() throws {
        let base = FileManager.default.temporaryDirectory.appendingPathComponent("auth-" + UUID().uuidString)
        defer { try? FileManager.default.removeItem(at: base) }
        let fm = FileManager.default
        let real = base.appendingPathComponent("real"), home = base.appendingPathComponent("home"), other = base.appendingPathComponent("other")
        for d in [real, home, other] { try fm.createDirectory(at: d, withIntermediateDirectories: true) }
        let auth = home.appendingPathComponent("auth.json"), realAuth = real.appendingPathComponent("auth.json")
        let good = #"{"tokens":"REAL"}"#
        func reset() throws {  // the real login, linked once (the generation is recorded), then the home's link is gone as if the CLI had replaced it
            try? fm.removeItem(at: realAuth); try Data(good.utf8).write(to: realAuth); unlink(auth.path)
            Router.linkProjectAuth(home: home, real: real, names: ["auth.json"]); unlink(auth.path)
        }
        func realText() -> String? { try? String(contentsOf: realAuth, encoding: .utf8) }
        let start = Date().addingTimeInterval(-1)
        let secret = other.appendingPathComponent("target.json"); try Data(#"{"x":"SECRET"}"#.utf8).write(to: secret)
        // a planted symlink in the home (to a file elsewhere): not read, not carried; the real file is untouched
        try reset(); try fm.createSymbolicLink(at: auth, withDestinationURL: secret)
        Router.restoreProjectAuth(home: home, real: real, since: start)
        #expect(realText() == good)
        // a symlink at the real name is neither followed nor replaced
        try reset(); try fm.removeItem(at: realAuth); try fm.createSymbolicLink(at: realAuth, withDestinationURL: secret)
        try Data(#"{"tokens":"NEW"}"#.utf8).write(to: auth)
        Router.restoreProjectAuth(home: home, real: real, since: start)
        #expect((try? fm.destinationOfSymbolicLink(atPath: realAuth.path)) == secret.path && (try? String(contentsOf: secret, encoding: .utf8)) == #"{"x":"SECRET"}"#)
        // garbage, an array, a huge file, a hard link, a stale file: all refused and dropped (the next run does not use them)
        let huge = Data(#"{"a":""#.utf8) + Data(repeating: 0x61, count: Router.maxAuthBytes + 10) + Data(#""}"#.utf8)
        let linked = other.appendingPathComponent("linked.json"); try Data(good.utf8).write(to: linked)
        let cases: [(String, (URL) throws -> Void)] = [
            ("garbage", { try Data("not json \u{0}\u{1}".utf8).write(to: $0) }), ("array", { try Data("[1,2]".utf8).write(to: $0) }),
            ("huge", { try huge.write(to: $0) }), ("hard link", { try? fm.removeItem(at: linked); try Data(good.utf8).write(to: linked); try fm.linkItem(at: linked, to: $0) }),
            ("stale", { try Data(good.utf8).write(to: $0); try fm.setAttributes([.modificationDate: Date().addingTimeInterval(-3600)], ofItemAtPath: $0.path) })]
        for (name, make) in cases {
            try reset(); try make(auth)
            Router.restoreProjectAuth(home: home, real: real, since: start)
            #expect(realText() == good, "\(name): the real login changed")
            #expect((try? fm.destinationOfSymbolicLink(atPath: auth.path)) == realAuth.path, "\(name): the file was not dropped")
        }
        // run start: a config.toml an earlier run left in the home is refreshed from the real file, and a leftover auth.json is the link again
        try reset()
        try Data("model = \"real\"\n".utf8).write(to: real.appendingPathComponent("config.toml"))
        try Data("model = \"planted\"\n".utf8).write(to: home.appendingPathComponent("config.toml"))
        try Data("not json".utf8).write(to: auth)
        Router.linkProjectAuth(home: home, real: real)
        #expect((try? String(contentsOf: home.appendingPathComponent("config.toml"), encoding: .utf8)) == "model = \"real\"\n")
        #expect((try? fm.destinationOfSymbolicLink(atPath: auth.path)) == realAuth.path)
        #expect(!fm.fileExists(atPath: other.appendingPathComponent("config.toml").path))
    }

    /// R2: two overlapping runs of one user and project share a home. The login the CLI of one run refreshed is carried back, in whichever order
    /// the runs start and end, never lost by the other run's start; and a real login that is newer than the one the link was made from stays.
    @Test func overlappingRunsNeitherLoseARotatedLoginNorOverwriteANewerOne() throws {
        let fm = FileManager.default
        let base = fm.temporaryDirectory.appendingPathComponent("auth-" + UUID().uuidString)
        defer { try? fm.removeItem(at: base) }
        let real = base.appendingPathComponent("real"), home = base.appendingPathComponent("home"), homeY = base.appendingPathComponent("homeY")
        for d in [real, home, homeY] { try fm.createDirectory(at: d, withIntermediateDirectories: true) }
        let auth = home.appendingPathComponent("auth.json"), realAuth = real.appendingPathComponent("auth.json")
        func text(_ u: URL) -> String? { try? String(contentsOf: u, encoding: .utf8) }
        func isLink(_ u: URL) -> Bool { (try? fm.destinationOfSymbolicLink(atPath: u.path)) == realAuth.path }
        func cliReplaces(_ u: URL, _ token: String) throws { try fm.removeItem(at: u); try Data(#"{"tokens":"\#(token)"}"#.utf8).write(to: u) }  // a write by rename
        func fresh() throws { try? fm.removeItem(at: realAuth); try Data(#"{"tokens":"R0"}"#.utf8).write(to: realAuth); unlink(auth.path); unlink(homeY.appendingPathComponent("auth.json").path) }
        // order 1: A's CLI rotates the token, then B starts (its start must carry it back, not unlink it), then A and B end
        try fresh()
        Router.linkProjectAuth(home: home, real: real)  // run A
        Router.linkProjectAuth(home: home, real: real)  // run B starts first (link intact: nothing changes)
        try cliReplaces(auth, "RA")
        Router.linkProjectAuth(home: home, real: real)  // run C starts after A's refresh
        #expect(text(realAuth) == #"{"tokens":"RA"}"# && isLink(auth), "the start unlinked the refreshed login: \(text(realAuth) ?? "")")
        Router.restoreProjectAuth(home: home, real: real, since: Date().addingTimeInterval(-60))  // A ends
        Router.restoreProjectAuth(home: home, real: real, since: Date().addingTimeInterval(-60))  // B ends
        #expect(text(realAuth) == #"{"tokens":"RA"}"# && isLink(auth))
        // order 2: A ends before the next run starts
        try fresh()
        Router.linkProjectAuth(home: home, real: real)
        try cliReplaces(auth, "RA2")
        Router.restoreProjectAuth(home: home, real: real, since: Date().addingTimeInterval(-60))
        Router.linkProjectAuth(home: home, real: real)
        #expect(text(realAuth) == #"{"tokens":"RA2"}"# && isLink(auth))
        // across homes: home X was linked from R0, another home's run then wrote a newer real login; the slower run's file must not overwrite it
        try fresh()
        Router.linkProjectAuth(home: home, real: real)
        try Data(#"{"tokens":"NEWER"}"#.utf8).write(to: realAuth)  // the other home's run (in place, or its own carry-back)
        try cliReplaces(auth, "SLOW")
        Router.restoreProjectAuth(home: home, real: real, since: Date().addingTimeInterval(-60))
        #expect(text(realAuth) == #"{"tokens":"NEWER"}"# && isLink(auth), "a newer real login was overwritten: \(text(realAuth) ?? "")")
        // the same through the next run's start
        try fresh()
        Router.linkProjectAuth(home: home, real: real)
        try Data(#"{"tokens":"NEWER"}"#.utf8).write(to: realAuth)
        try cliReplaces(auth, "SLOW")
        Router.linkProjectAuth(home: home, real: real)
        #expect(text(realAuth) == #"{"tokens":"NEWER"}"# && isLink(auth))
        // real concurrency: the lifecycle of one real file is serialised; many starts and ends never leave the real login damaged
        try fresh()
        Router.linkProjectAuth(home: home, real: real)
        DispatchQueue.concurrentPerform(iterations: 40) { i in
            if i % 2 == 0 { Router.linkProjectAuth(home: home, real: real) } else { Router.restoreProjectAuth(home: home, real: real, since: Date().addingTimeInterval(-60)) }
        }
        #expect(text(realAuth) == #"{"tokens":"R0"}"# && isLink(auth))
    }

    /// sec-fix F7: the core's dirName is one path component.
    @Test func homeNameIsOnePlainPathComponent() {
        for ok in ["2-3-e4", "1-1-e1", "a.b", "..x"] { #expect(Router.isHomeName(ok), "\(ok)") }
        for bad in ["", ".", "..", "a/b", "/abs", "../x", "x/..", "a\u{0}b"] { #expect(!Router.isHomeName(bad), "\(bad.debugDescription)") }
    }

    /// sec-fix F8: past maxHomes the oldest goes first, but never the home of a run that is going (and not a dead epoch's either).
    @Test func aHomeInUseIsNeverPruned() async throws {
        let f = try await PFx()
        f.makeRouter()
        func run(_ user: Int, epoch: Int = 1) -> ProjectRun { ProjectRun(runId: "r", user: user, device: "dev", project: 1, epoch: epoch) }
        let busy = try #require(await f.router.projectHome(kind: "codex", owner: "dev", project: run(1)))  // held: its run is going
        let root = busy.deletingLastPathComponent().deletingLastPathComponent()
        try FileManager.default.setAttributes([.modificationDate: Date().addingTimeInterval(-86400)], ofItemAtPath: busy.deletingLastPathComponent().path)  // the oldest
        for i in 2...45 { await f.router.projectHome(kind: "codex", owner: "dev", project: run(i)).map(Router.releaseProjectHome) }
        #expect(FileManager.default.fileExists(atPath: busy.path), "evicted past maxHomes")
        #expect(try FileManager.default.contentsOfDirectory(atPath: root.path).count <= 21)
        await f.router.projectHome(kind: "codex", owner: "dev", project: run(1, epoch: 2)).map(Router.releaseProjectHome)  // a newer epoch: the old home is dead, but its run goes
        #expect(FileManager.default.fileExists(atPath: busy.path), "dead epoch removed under a running run")
        Router.releaseProjectHome(busy)
        await f.router.projectHome(kind: "codex", owner: "dev", project: run(1, epoch: 3)).map(Router.releaseProjectHome)
        #expect(!FileManager.default.fileExists(atPath: busy.path))  // once its run ended the next prune takes it
    }

    /// R3: the prune picks a folder, another run then holds a home in it, and the deletion must not take it: the hold waits for the deletion
    /// and gets a fresh folder; a hold that came before the pick makes the prune skip the folder.
    @Test func aHoldBetweenThePickAndTheDeletionWaitsAndIsNeverDeleted() throws {
        let fm = FileManager.default
        let root = fm.temporaryDirectory.appendingPathComponent("prune-" + UUID().uuidString)
        defer { try? fm.removeItem(at: root); ProjectHomes.beforeDelete = nil }
        let dead = root.appendingPathComponent("1-1-e1"), home = dead.appendingPathComponent("codex")
        try fm.createDirectory(at: home, withIntermediateDirectories: true)
        try fm.createDirectory(at: root.appendingPathComponent("2-2-e1/codex"), withIntermediateDirectories: true)
        let seen = Mutex<(returned: Bool, existed: Bool?)>((false, nil)), done = DispatchSemaphore(value: 0)
        ProjectHomes.beforeDelete = {  // between the claim and the removeItem
            ProjectHomes.beforeDelete = nil
            Thread {
                ProjectHomes.hold(home.path)  // must wait for the deletion
                seen.withLock { $0 = (true, fm.fileExists(atPath: dead.path)) }
                done.signal()
            }.start()
            Thread.sleep(forTimeInterval: 0.3)  // the other run is certainly waiting now
            seen.withLock { #expect(!$0.returned, "the hold did not wait for the deletion") }
        }
        Router.pruneProjectHomes(root: root, keep: "2-2-e1", prefix: "1-1-e", epoch: 2, maxHomes: 20)
        #expect(done.wait(timeout: .now() + 5) == .success)
        #expect(seen.withLock { $0.existed } == false, "the hold returned before the folder was deleted")
        ProjectHomes.release(home.path)
        // a hold that is already there: the prune skips the folder (and takes it later)
        try fm.createDirectory(at: home, withIntermediateDirectories: true)
        ProjectHomes.hold(home.path)
        Router.pruneProjectHomes(root: root, keep: "2-2-e1", prefix: "1-1-e", epoch: 2, maxHomes: 20)
        #expect(fm.fileExists(atPath: home.path))
        ProjectHomes.release(home.path)
        Router.pruneProjectHomes(root: root, keep: "2-2-e1", prefix: "1-1-e", epoch: 2, maxHomes: 20)
        #expect(!fm.fileExists(atPath: dead.path))
    }

    // MARK: D8 usage events

    @Test func aDispatchedRequestIsOneEventAndALocalRefusalIsNone() async throws {
        let f = try await PFx()
        await f.project("fakture"); await f.project("other")
        await f.user("ana", projects: ["fakture": "developer"])
        f.makeRouter(claude: try f.script("claude", fakeClaude), codex: try f.script("codex", fakeCodex))
        #expect(await f.ask(project: f.pid["fakture"]).status == 200)          // dispatched with its project, tokens reported by the agent
        #expect(await f.ask("claude-codex", project: f.pid["fakture"]).status == 200)  // estimated tokens
        #expect(await f.ask(project: f.pid["other"]).status == 403)            // refused at admission: no event
        #expect(await f.ask("claude-unknown-model", project: nil).status == 404)  // unknown model: no event
        let rows = await f.usage()
        #expect(f.events(rows, model: "claude-cli-haiku", project: f.pid["fakture"]!) == 1, "\(rows)")
        #expect(f.events(rows, model: "claude-codex", project: f.pid["fakture"]!) == 1)
        #expect(f.events(rows, model: "claude-cli-haiku", project: f.pid["other"]!) == 0 && f.events(rows, model: "claude-unknown-model") == 0)
        let claude = try #require(rows.first { $0["model"] as? String == "claude-cli-haiku" })
        #expect(claude["tokensIn"] as? Int == 5 && claude["tokensOut"] as? Int == 3 && claude["user"] as? Int == f.uid["ana"] && claude["failedEvents"] as? Int == 0)
        let codex = try #require(rows.first { $0["model"] as? String == "claude-codex" })
        #expect((codex["tokensOut"] as? Int ?? 0) > 0 && codex["tokensIn"] as? Int == 0)  // estimated: no input count
    }

    @Test func aFailedRequestIsAnErrorEventAndANestedCallIsCountedOnceWithItsParent() async throws {
        let f = try await PFx()
        await f.project("fakture"); await f.user("ana", projects: ["fakture": "developer"])
        let failing = try f.script("claude-fail", """
        [ "$1" = --version ] && { echo '2.1.0 (Claude Code)'; exit 0; }
        cat > /dev/null
        echo boom >&2; exit 3
        """)
        f.makeRouter(ask: true, claude: try f.script("claude", fakeClaude), codex: try f.script("codex", fakeCodex))
        f.write("plan.txt", "ASK claude-codex\n")
        #expect(await f.ask(project: f.pid["fakture"]).status == 200)
        #expect(f.read("res-1.code") == "200")  // the nested call ran
        var rows = await f.usage()
        // the parent and its nested call: two events (not three), the nested one marked as nested, with the root's project and user
        #expect(f.events(rows, model: "claude-cli-haiku", project: f.pid["fakture"]!) == 1 && f.events(rows, model: "claude-codex", project: f.pid["fakture"]!) == 1, "\(rows)")
        let nested = try #require(rows.first { $0["model"] as? String == "claude-codex" })
        #expect(nested["nestedEvents"] as? Int == 1 && nested["user"] as? Int == f.uid["ana"])
        #expect((rows.first { $0["model"] as? String == "claude-cli-haiku" })?["nestedEvents"] as? Int == 0)
        // an agent that fails: an error event (dispatched, so counted)
        f.makeRouter(claude: failing)
        let bad = await f.ask()
        #expect(bad.status != 200)
        rows = await f.usage()
        #expect((rows.first { $0["model"] as? String == "claude-cli-haiku" && ($0["project"] as? Int ?? 0) == 0 })?["failedEvents"] as? Int == 1, "\(rows)")
    }

    @Test func eventsWaitForTheHelperAndAreStoredOnceWhenItIsBack() async throws {
        let f = try await PFx(backoff: [4.0])  // down long enough for the asks below on a quiet Mac
        await f.project("fakture"); await f.user("ana", projects: ["fakture": "developer"])
        f.makeRouter(claude: try f.script("claude", fakeClaude))
        kill(f.h.pid!, SIGKILL)
        for _ in 0..<200 where f.h.isReady { try await Task.sleep(for: .milliseconds(10)) }
        // the helper is down: models without a project keep working, never blocked on usage
        for _ in 0..<3 { #expect(await f.ask().status == 200) }
        // Not a time: the three asks returned 200 above while the helper was (checked next) still down, so they cannot have waited for it.
        // While it is down the events wait in the bounded queue. If a slow Mac let the helper come back first there is nothing to prove here;
        // the end of the test (all three stored once) holds either way. pending is read first, then the state: down at both = down throughout.
        let pending = f.router.usageSender.pending
        if !f.h.isReady { #expect(pending >= 3, "pending \(pending) while the helper is down") }
        #expect(await f.h.waitReady(timeout: 20))
        let rows = await f.usage()
        #expect(f.events(rows, model: "claude-cli-haiku") == 3, "\(rows)")  // all three, each once
        let gaps = await f.gaps()
        #expect(f.router.usageSender.pending == 0 && gaps == 0)
    }

    @Test func anOverflowWhileTheHelperIsDownIsAGapNotASilentLoss() async throws {
        let f = try await PFx(backoff: [1.5])
        let sender = UsageSender(core: f.h, limit: 3, retry: .milliseconds(20))
        kill(f.h.pid!, SIGKILL)
        for _ in 0..<200 where f.h.isReady { try await Task.sleep(for: .milliseconds(10)) }
        for i in 0..<8 {  // the helper is down: the queue holds 3, the other 5 are lost
            var e = UsageEvent(id: "ev_overflow\(i)", start: Date(), model: "m-overflow")
            e.deviceId = "dev-ana"; e.outcome = "ok"; e.end = Date(); e.provider = "p"
            sender.finish(e)
        }
        #expect(sender.pending == 3)
        #expect(await f.h.waitReady(timeout: 20))
        #expect(await sender.flush())
        // every event is stored or counted in a gap, none vanished: 3 stored + a gap of 5 (the recorder writes asynchronously)
        var stored = 0, gap = 0
        for _ in 0..<200 {
            stored = f.events(await f.usage(), model: "m-overflow"); gap = await f.gaps()
            if stored + gap >= 8 { break }
            try await Task.sleep(for: .milliseconds(30))
        }
        #expect(stored == 3 && gap == 5, "stored \(stored) gap \(gap)")
    }

    @Test func theUserOfADeviceIsFrozenFromTheCoreAndFallsBackToTheLastOneSeen() async throws {
        let f = try await PFx()
        await f.user("ana")
        let sender = UsageSender(core: f.h)
        let known = await sender.user(of: "dev-ana"), none = await sender.user(of: ""), unknown = await sender.user(of: "dev-unknown")
        #expect(known == f.uid["ana"] && none == 0 && unknown == 0)
        kill(f.h.pid!, SIGKILL)
        for _ in 0..<200 where f.h.isReady { try await Task.sleep(for: .milliseconds(10)) }
        #expect(await sender.user(of: "dev-ana") == f.uid["ana"])  // the helper is down: the last binding seen (attribution only)
    }
}
