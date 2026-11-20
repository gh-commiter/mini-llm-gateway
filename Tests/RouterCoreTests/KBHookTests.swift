import Foundation
import Testing
@testable import RouterCore

// The opt-in kb hooks (Sources/RouterCore/KBHook.swift). The shared fixture client/testdata/kb-hook.json is asserted here and in
// client/kb_hook_test.go. Everything is fake: a temp config folder and project folder, git and the gateway injected per instance
// (never the real ~/Library/Application Support/minigw or ~/.claude).

private let repoRoot = URL(fileURLWithPath: #filePath).deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent()

private final class HookWorld: @unchecked Sendable {
    let root: String, cfg: String, work: String
    var out = "", err = "", calls = 0
    var env: ProjectEnv!

    init(stdin: Data = Data()) throws {
        root = try Projects.realFolder(NSTemporaryDirectory()) + "/kbhook-" + UUID().uuidString
        cfg = root + "/cfg/minigw"
        work = root + "/work"
        try FileManager.default.createDirectory(atPath: work, withIntermediateDirectories: true)
        env = ProjectEnv(connect: { [unowned self] in self.calls += 1; return (ProjectConn(url: "", token: ""), "gw1") }, state: cfg + "/projects.json", cwd: work,
                         stdin: { stdin }, out: { [unowned self] in self.out += $0 }, err: { [unowned self] in self.err += $0 }, agent: false,
                         send: { [unowned self] _, _, _, _, _ in self.calls += 1; return ProjectReply(status: 599) }, sleep: { _ in })
        env.gateway = { "gw1" }
        env.now = { Date(timeIntervalSince1970: 1_000_000) }
    }

    deinit { try? FileManager.default.removeItem(atPath: root) }

    func link(_ gw: String, _ project: Int64, _ slug: String, path: String? = nil) throws {
        var st = (try? Projects.load(env.state)) ?? ProjectState()
        st.links.append(ProjectLink(gateway: gw, project: project, slug: slug, path: path ?? work))
        try Projects.save(env.state, st)
    }

    func write(_ rel: String, _ v: Any) throws {
        let p = cfg + "/" + rel
        try FileManager.default.createDirectory(atPath: (p as NSString).deletingLastPathComponent, withIntermediateDirectories: true)
        FileManager.default.createFile(atPath: p, contents: try JSONSerialization.data(withJSONObject: v), attributes: [.posixPermissions: 0o600])
    }

    func files() -> [String] {
        let e = FileManager.default.enumerator(atPath: root)
        var out: [String] = []
        while let f = e?.nextObject() as? String {
            var isDir: ObjCBool = false
            if FileManager.default.fileExists(atPath: root + "/" + f, isDirectory: &isDir), !isDir.boolValue { out.append(f) }
        }
        return out.sorted()
    }
}

private func mode(_ p: String) -> mode_t { var s = stat(); return stat(p, &s) == 0 ? s.st_mode & 0o777 : 0 }

@Suite struct KBHookTests {
    @Test func sharedFixture() throws {
        let f = try JSONSerialization.jsonObject(with: Data(contentsOf: repoRoot.appendingPathComponent("client/testdata/kb-hook.json"))) as! [String: Any]
        let gateway = f["gateway"] as! String, project = (f["project"] as! NSNumber).int64Value, slug = f["slug"] as! String, now = (f["now"] as! NSNumber).doubleValue
        let cases = f["cases"] as! [[String: Any]]
        #expect(cases.count > 30)
        for c in cases {
            let name = c["name"] as! String
            let want = c["want"] as! [String: Any]
            let w = try HookWorld()
            w.env.now = { Date(timeIntervalSince1970: now) }
            let git = c["git"] as! [String: String]
            w.env.gitState = { dir in
                #expect(dir == w.work, "\(name): git asked in the wrong folder")
                return (git["head"]!, git["status"]!)
            }
            w.env.agent = c["agent"] as! Bool
            if c["linked"] as! Bool { try w.link(gateway, project, c["linkSlug"] as? String ?? slug) }
            if let hint = c["hint"] as? String { try Data((hint + "\n").utf8).write(to: URL(fileURLWithPath: w.work + "/.minigw-project")) }
            if c["enabled"] as! Bool { try w.write("kb-hooks.json", ["version": 1, "enabled": [["gateway": gateway, "project": project]]]) }
            let session = want["session"] as? String ?? "s1"
            if let m = c["marker"] as? [String: Any] { try w.write("kb-hook/\(session).json", m) }
            if let t = c["submitted"] as? Int, t != 0 { try w.write("kb-submitted.json", ["version": 1, "submitted": [["gateway": gateway, "project": project, "time": t]]]) }
            var input: Data
            if let raw = c["stdinRaw"] as? String { input = Data(raw.utf8) } else {
                var s = c["stdin"] as! [String: Any]
                s["cwd"] = (s["cwd"] as! String).replacingOccurrences(of: "$CWD", with: w.work)
                if let pad = c["stdinPad"] as? Int { s["pad"] = String(repeating: "x", count: pad) }
                input = try JSONSerialization.data(withJSONObject: s)
            }
            w.env.stdin = { input }
            let started = Date()
            try w.env.kb(["hook", c["event"] as! String])
            #expect(Date().timeIntervalSince(started) < 2, "\(name): slower than 2 s")
            #expect(w.out == want["stdout"] as! String, "\(name): stdout")
            #expect(w.err.isEmpty && w.calls == 0, "\(name): stderr or a gateway call")
            let file = w.cfg + "/kb-hook/\(session).json"
            let got = FileManager.default.contents(atPath: file).flatMap(ProjectEnv.HookMarker.init)
            let wantMarker = try (want["marker"] as? [String: Any]).map { try ProjectEnv.HookMarker(JSONSerialization.data(withJSONObject: $0))! }
            #expect(got == wantMarker, "\(name): marker")
            if got != nil { #expect(mode(file) == 0o600, "\(name): marker mode") }
            for rel in w.files() {  // nothing but the state files and markers inside kb-hook/; the project folder stays empty
                let ok = rel == "cfg/minigw/projects.json" || rel == "cfg/minigw/kb-hooks.json" || rel == "cfg/minigw/kb-submitted.json" || rel == "work/.minigw-project"
                    || rel.hasPrefix("cfg/minigw/kb-hook/") && !rel.dropFirst("cfg/minigw/kb-hook/".count).contains("/")
                #expect(ok, "\(name): unexpected file \(rel)")
            }
        }
    }

    @Test func reviewShowsATabWhereItIs() throws {
        let full = try #require(JSONValue.parse(Data(#"{"id":1,"project":1,"state":"pending","class":"data","title":"T","body":"a\tb\u200bc","reviewHash":"h"}"#.utf8))).dict
        var lines: [String] = []
        ProjectEnv.printReview({ lines.append($0) }, full, [:])
        #expect(lines.last == "a\tbc")  // the tab stays in place, the invisible character goes
    }

    /// The review printers of both clients on the shared fixture (client/testdata/review-text.json); an emoji sequence comes out as it went in.
    @Test func reviewTextFixture() throws {
        let repo = URL(fileURLWithPath: #filePath).deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent()
        let cases = try JSONSerialization.jsonObject(with: Data(contentsOf: repo.appendingPathComponent("client/testdata/review-text.json"))) as! [[String: String]]
        #expect(!cases.isEmpty)
        for c in cases { #expect(ProjectText.reviewText(c["in"]!) == c["out"]!, "\(c["name"]!)") }
        var lines: [String] = []
        let full = try #require(JSONValue.parse(Data(#"{"id":1,"project":1,"state":"pending","class":"data","title":"T","body":"\ud83d\udc69\u200d\ud83d\udcbb \u26a0\ufe0f 1\ufe0f\u20e3\ta\u202eb","reviewHash":"h"}"#.utf8))).dict
        ProjectEnv.printReview({ lines.append($0) }, full, [:])
        #expect(lines.last == "\u{1F469}\u{200D}\u{1F4BB} \u{26A0}\u{FE0F} 1\u{FE0F}\u{20E3}\tab")
    }

    @Test func olderGatewayNamesTheVersionForKBAndTeam() throws {
        func send(_ version: String, _ path: String) -> String {
            let s = ProjectEnv.olderRouteHint { _, _, p, _, _ in
                p == "/health/ready" ? ProjectReply(status: 200, data: Data(#"{"version":"\#(version)"}"#.utf8)) : ProjectReply(status: 404, data: Data(#"{"error":{"message":"not found"}}"#.utf8))
            }
            let r = s(ProjectConn(url: "", token: ""), "GET", path, [:], nil)
            return ProjectEnv.gatewayError(r.status, r.data, nil).message
        }
        #expect(send("0.7.5", "/minigw/v1/projects/7/kb/search?q=x") == "this gateway is 0.7.5; kb needs 0.7.10 or newer: update the gateway (/minigw:gw update there)")
        #expect(send("0.7.0", "/minigw/v1/teams") == "this gateway is 0.7.0; team needs 0.7.1 or newer: update the gateway (/minigw:gw update there)")
        #expect(send("0.7.10", "/minigw/v1/projects/7/kb/search?q=x") == "not found or not visible")
    }

    /// The rule over ALL code points against the compact range list the Go test checks too (client/testdata/invisible-ranges.json).
    @Test func invisibleRangesFixture() throws {
        let repo = URL(fileURLWithPath: #filePath).deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent()
        let want = try JSONSerialization.jsonObject(with: Data(contentsOf: repo.appendingPathComponent("client/testdata/invisible-ranges.json"))) as! [[Int]]
        var got: [[Int]] = []
        for v in 0...0x10FFFF where !(0xD800...0xDFFF).contains(v) && ProjectText.invisibleRune(UInt32(v)) {
            if let last = got.last, last[1] == v - 1 { got[got.count - 1][1] = v } else { got.append([v, v]) }
        }
        #expect(got == want)
    }

    @Test(.blocking) func pruneKeepsFreshAndTheNewest200() throws {
        let w = try HookWorld(stdin: Data())
        try w.link("gw1", 7, "acme-shop")
        try w.write("kb-hooks.json", ["version": 1, "enabled": [["gateway": "gw1", "project": 7]]])
        w.env.gitState = { _ in ("h", "s") }
        let day = 24 * 3600, now = 5_000_000
        w.env.now = { Date(timeIntervalSince1970: Double(now)) }
        func marker(_ started: Int) -> [String: Any] { ["gateway": "gw1", "project": 7, "head": "", "status": "", "asked": false, "compact": false, "started": started] }
        try w.write("kb-hook/old.json", marker(now - 15 * day))
        try w.write("kb-hook/fresh.json", marker(now - day))
        try Data("{".utf8).write(to: URL(fileURLWithPath: w.cfg + "/kb-hook/junk.json"))
        for i in 0..<230 { try w.write(String(format: "kb-hook/m%03d.json", i), marker(4_000_000 + i)) }
        w.env.stdin = { Data(#"{"session_id":"new","cwd":"\#(w.work)"}"#.utf8) }
        try w.env.kb(["hook", "start"])
        let names = Set(try FileManager.default.contentsOfDirectory(atPath: w.cfg + "/kb-hook"))
        #expect(!names.contains("old.json") && !names.contains("junk.json") && names.contains("fresh.json") && names.contains("new.json"))
        #expect(names.count == ProjectEnv.hookKeepMax)  // 200 markers, the new one among them
        #expect(!names.contains("m000.json") && !names.contains("m031.json") && names.contains("m032.json") && names.contains("m229.json"))
    }

    @Test func hooksOnOffStatus() throws {
        let w = try HookWorld()
        func run(_ a: String...) throws -> String { w.out = ""; try w.env.kb(["hooks"] + a); return w.out }
        do { _ = try run("status"); Issue.record("not linked") } catch { #expect(String(describing: error).hasPrefix("this folder is not linked to a project")) }
        do { _ = try run("on"); Issue.record("not linked") } catch { #expect(String(describing: error).hasPrefix("this folder is not linked to a project")) }
        #expect(!FileManager.default.fileExists(atPath: w.cfg + "/kb-hooks.json"))
        do { _ = try run("on", "acme-shop"); Issue.record("not linked") } catch { #expect(String(describing: error).contains("acme-shop")) }
        try w.link("gw1", 7, "acme-shop")
        try w.link("gw1", 8, "web", path: w.root + "/elsewhere")
        #expect(try run("status") == "kb hooks: off for project acme-shop\n")
        let before = try FileManager.default.contentsOfDirectory(atPath: w.work)
        #expect(try run("on") == "kb hooks on for project acme-shop\n")
        #expect(try run("on") == "kb hooks on for project acme-shop\n")
        #expect(try run("status") == "kb hooks: on for project acme-shop\n")
        #expect(try run("status", "web") == "kb hooks: off for project web\n")  // one project does not switch another
        #expect(try run("on", "web") == "kb hooks on for project web\n")
        #expect(try run("off", "acme-shop") == "kb hooks off for project acme-shop\n")
        #expect(try run("status", "web") == "kb hooks: on for project web\n")
        #expect(try run("status") == "kb hooks: off for project acme-shop\n")
        #expect(w.env.hookEnabled("gw1", 8) && !w.env.hookEnabled("gw1", 7) && w.env.loadHookSettings().count == 1)
        #expect(mode(w.cfg + "/kb-hooks.json") == 0o600)
        #expect(try FileManager.default.contentsOfDirectory(atPath: w.work) == before)
        #expect(w.calls == 0)
        for bad in [[], ["maybe"], ["on", "a", "b"]] {
            do { try w.env.kb(["hooks"] + bad); Issue.record("expected usage") } catch { #expect(String(describing: error).hasPrefix("usage:")) }
        }
        w.env.gateway = { "" }
        let shared = try JSONSerialization.jsonObject(with: Data(contentsOf: repoRoot.appendingPathComponent("client/testdata/kb-hook.json"))) as! [String: Any]
        do { _ = try run("status"); Issue.record("not connected") } catch { #expect(String(describing: error) == shared["notConnected"] as? String) }
        w.env.gateway = { "gw1" }
        w.env.agent = true
        do { _ = try run("status"); Issue.record("agent") } catch { #expect(String(describing: error) == ProjectEnv.agentRefusal) }
    }

    @Test func submitRecordsOnlyA201() throws {
        for (status, record) in [(201, true), (400, false), (403, false), (413, false), (0, false)] {
            let w = try HookWorld(stdin: Data("text\n".utf8))
            w.env.send = { _, method, _, _, _ in
                method == "GET" ? ProjectReply(status: 200, data: Data(#"{"id":7}"#.utf8))
                    : ProjectReply(status: status, data: Data(#"{"id":1,"project":7,"state":"pending","class":"data","title":"t"}"#.utf8))
            }
            try? w.env.kb(["submit", "acme-shop", "--title", "t", "-"])
            let file = w.cfg + "/kb-submitted.json"
            #expect(FileManager.default.fileExists(atPath: file) == record, "status \(status)")
            if record {
                let o = try JSONSerialization.jsonObject(with: Data(contentsOf: URL(fileURLWithPath: file))) as! [String: Any]
                let list = o["submitted"] as! [[String: Any]]
                #expect(list.count == 1 && list[0]["gateway"] as? String == "gw1" && list[0]["project"] as? Int == 7 && list[0]["time"] as? Int == 1_000_000)
                #expect(mode(file) == 0o600)
            }
        }
    }

    // MARK: git

    private func git(_ dir: String, _ args: String...) throws -> String {
        let p = Process()
        p.executableURL = URL(fileURLWithPath: "/usr/bin/git")
        p.arguments = ["-c", "user.name=n", "-c", "user.email=a@b", "-c", "commit.gpgsign=false"] + args
        p.currentDirectoryURL = URL(fileURLWithPath: dir)
        p.environment = ["GIT_CONFIG_GLOBAL": "/dev/null", "GIT_CONFIG_NOSYSTEM": "1", "PATH": "/usr/bin:/bin"]
        let pipe = Pipe()
        p.standardOutput = pipe
        try p.run()
        let data = pipe.fileHandleForReading.readDataToEndOfFile()
        p.waitUntilExit()
        #expect(p.terminationStatus == 0, "git \(args)")
        return String(decoding: data, as: UTF8.self)
    }

    private func repo() throws -> (HookWorld, String) {
        let w = try HookWorld()
        let dir = w.root + "/repo"
        try FileManager.default.createDirectory(atPath: dir, withIntermediateDirectories: true)
        _ = try git(dir, "init", "-q")
        try Data("a".utf8).write(to: URL(fileURLWithPath: dir + "/a.txt"))
        _ = try git(dir, "add", "a.txt")
        _ = try git(dir, "commit", "-qm", "one")
        return (w, dir)
    }

    @Test(.blocking) func gitStateOfARepositoryAndOfAPlainFolder() throws {
        let (w, dir) = try repo()
        let (head, status) = ProjectEnv.hookGitState(dir, timeout: 60)
        #expect(head == ProjectText.trim(try git(dir, "rev-parse", "HEAD")) && status.count == 64)
        #expect(ProjectEnv.hookGitState(dir, timeout: 60) == (head, status))
        try Data("b".utf8).write(to: URL(fileURLWithPath: dir + "/b.txt"))
        let (h2, s2) = ProjectEnv.hookGitState(dir, timeout: 60)
        #expect(h2 == head && s2 != status)  // an untracked file changes the status, not HEAD
        _ = try git(dir, "add", "b.txt")
        _ = try git(dir, "commit", "-qm", "two")
        #expect(ProjectEnv.hookGitState(dir, timeout: 60).head != head)
        let plain = ProjectEnv.hookGitState(w.work)
        #expect(plain.head == "" && plain.status == "")
    }

    @Test(.blocking) func gitStateDoesNotRunTheCheckoutsCode() throws {
        let (w, dir) = try repo()
        let proof = w.root + "/ran", script = w.root + "/fsmonitor.sh"
        try Data("#!/bin/sh\necho ran >> '\(proof)'\nprintf '\\0'\n".utf8).write(to: URL(fileURLWithPath: script))
        chmod(script, 0o755)
        _ = try git(dir, "config", "core.fsmonitor", script)
        // positive control: plain git does run it, so the check below proves the guard and not a git that never calls it
        _ = try git(dir, "status")
        #expect(FileManager.default.fileExists(atPath: proof), "the control did not run the fsmonitor script: the test proves nothing")
        unlink(proof)
        _ = ProjectEnv.hookGitState(dir, timeout: 60)
        #expect(!FileManager.default.fileExists(atPath: proof), "the checkout's core.fsmonitor program ran")
    }

    @Test func gitComesOnlyFromAnAbsolutePathEntry() throws {
        let w = try HookWorld()
        let bin = w.root + "/bin"
        try FileManager.default.createDirectory(atPath: bin, withIntermediateDirectories: true)
        let fake = bin + "/git"
        try Data("#!/bin/sh\necho ran >> '\(w.root)/fake-ran'\nexit 1\n".utf8).write(to: URL(fileURLWithPath: fake))
        chmod(fake, 0o755)
        // a relative entry (bin, .) is never used: the process folder is global state, so it is the lookup that is asserted
        #expect(Profiles.executable("git", path: "bin:.:/usr/bin") == "/usr/bin/git")
        // L4: no git on an absolute PATH entry means no state at all (not Apple's /usr/bin/git stub)
        let none = ProjectEnv.hookGitState(w.work, path: "bin:.", timeout: 60)
        #expect(none.head == "" && none.status == "")
        _ = ProjectEnv.hookGitState(w.work, path: "bin:.:/usr/bin", timeout: 60)
        #expect(!FileManager.default.fileExists(atPath: w.root + "/fake-ran"))
    }

    /// Another process that was started at the same moment (a parallel test's child) can hold the write end of git's output pipe, so EOF comes late. git is
    /// done and its output is complete: the state must not wait for that process (three such waits on the 3 pool threads of a small runner stopped a whole run).
    @Test func aStrangerHoldingTheOutputPipeDoesNotDelayTheState() throws {
        let w = try HookWorld()
        let bin = w.root + "/bin"
        try FileManager.default.createDirectory(atPath: bin, withIntermediateDirectories: true)
        // git prints and exits at once, but a background process keeps its stdout open for 10 s
        try Data("#!/bin/sh\necho deadbeef\n(sleep 10 &) \nexit 0\n".utf8).write(to: URL(fileURLWithPath: bin + "/git"))
        chmod(bin + "/git", 0o755)
        let started = Date()
        let r = ProjectEnv.hookGitState(w.work, path: bin, timeout: 30)
        #expect(Date().timeIntervalSince(started) < 5, "waited for the stranger: \(Date().timeIntervalSince(started)) s")
        #expect(r.head == "deadbeef")
    }

    @Test func aSlowGitIsGivenUpOn() throws {
        let w = try HookWorld()
        let bin = w.root + "/bin"
        try FileManager.default.createDirectory(atPath: bin, withIntermediateDirectories: true)
        try Data("#!/bin/sh\nsleep 20\n".utf8).write(to: URL(fileURLWithPath: bin + "/git"))
        chmod(bin + "/git", 0o755)
        let started = Date()
        let r = ProjectEnv.hookGitState(w.work, path: bin, timeout: 0.3)
        #expect(r.head == "" && r.status == "")
        #expect(Date().timeIntervalSince(started) < 5)
    }

    @Test(.blocking) func contentFiltersOfTheCheckoutDoNotRun() throws {
        let (w, dir) = try repo()
        let proof = w.root + "/ran"
        try Data("* filter=x\n".utf8).write(to: URL(fileURLWithPath: dir + "/.git/info/attributes"))
        _ = try git(dir, "config", "filter.x.clean", "touch '\(proof)'; cat")
        // a tracked file whose stat differs from the index, content the same: git compares content, through the filter
        var times = [timeval(tv_sec: Int(Date().timeIntervalSince1970) + 3600, tv_usec: 0), timeval(tv_sec: Int(Date().timeIntervalSince1970) + 3600, tv_usec: 0)]
        utimes(dir + "/a.txt", &times)
        _ = try git(dir, "status")  // positive control: plain status runs the filter
        #expect(FileManager.default.fileExists(atPath: proof), "the control did not run the filter: the test proves nothing")
        rename(proof, proof + ".1")
        let before = ProjectEnv.hookGitState(dir, timeout: 60)
        #expect(!FileManager.default.fileExists(atPath: proof), "the checkout's clean filter ran")
        try Data("changed again, longer".utf8).write(to: URL(fileURLWithPath: dir + "/a.txt"))
        let after = ProjectEnv.hookGitState(dir, timeout: 60)
        #expect(after.status != before.status && after.status.count == 64, "an edit of a tracked file is not seen")
        _ = try git(dir, "add", "a.txt")
        #expect(ProjectEnv.hookGitState(dir, timeout: 60).status != before.status)
    }

    @Test func nothingIsReadWhenNotEnabled() throws {
        for settings in [nil, ["version": 1, "enabled": []] as [String: Any]] {
            let w = try HookWorld()
            try w.link("gw1", 7, "acme-shop")
            if let settings { try w.write("kb-hooks.json", settings) }
            var asked = 0
            w.env.gateway = { asked += 1; return "gw1" }
            w.env.stdin = { Data(#"{"session_id":"s1","cwd":"\#(w.work)"}"#.utf8) }
            try w.env.kb(["hook", "start"])
            #expect(asked == 0 && w.out.isEmpty)
        }
    }

    /// The request puts nothing taken from the repository on a command line: no double-quoted placeholder, no --title.
    @Test func theRequestHasNoShellExpandablePlaceholder() throws {
        let text = ProjectEnv.hookRequestText
        #expect(!text.contains("\"<") && !text.contains("--title\u{20}\""))
        let re = try NSRegularExpression(pattern: "`(minigw kb submit [^`]*)`")
        for m in re.matches(in: text, range: NSRange(text.startIndex..., in: text)) {
            let cmd = String(text[Range(m.range(at: 1), in: text)!])
            #expect(!cmd.contains("--title") && !cmd.contains("\""))
        }
    }

    /// The command in the hand-in request, parsed and run through the real argument parser with the text on stdin.
    @Test func theCommandsOfTheRequestWork() throws {
        let text = ProjectEnv.hookRequestText.replacingOccurrences(of: "<slug>", with: "acme-shop")
        let re = try NSRegularExpression(pattern: "`(minigw kb submit [^`]*)`")
        let cmds = re.matches(in: text, range: NSRange(text.startIndex..., in: text)).map { String(text[Range($0.range(at: 1), in: text)!]) }
        #expect(cmds.count == 2)
        for (cmd, kind) in zip(cmds, ["finding", "faq"]) {
            let tok = try NSRegularExpression(pattern: #""([^"]*)"|(\S+)"#)
            let args = tok.matches(in: cmd, range: NSRange(cmd.startIndex..., in: cmd)).map { m -> String in
                String(cmd[Range(m.range(at: m.range(at: 1).location != NSNotFound ? 1 : 2), in: cmd)!])
            }
            #expect(args[0] == "minigw" && args[1] == "kb")
            let w = try HookWorld(stdin: Data("the text on stdin\n".utf8))
            var sent: [String: Any] = [:]
            w.env.send = { _, method, _, _, body in
                if method == "GET" { return ProjectReply(status: 200, data: Data(#"{"id":7}"#.utf8)) }
                sent = (try? JSONSerialization.jsonObject(with: body ?? Data())) as? [String: Any] ?? [:]
                return ProjectReply(status: 201, data: Data(#"{"id":1,"project":7,"state":"pending","class":"data","title":"t"}"#.utf8))
            }
            try w.env.kb(Array(args.dropFirst(2)))
            #expect(sent["entryKind"] as? String == kind && sent["body"] as? String == "the text on stdin\n" && !(sent["title"] as? String ?? "").isEmpty)
            #expect(w.out == "submission 1 pending\n")
        }
    }
}

// What a reviewer sees of waiting submissions (project show, the session-start line). The shared fixture client/testdata/kb-waiting.json
// is asserted here and in client/kb_waiting_test.go.
@Suite struct KBWaitingTests {
    private func fixture() throws -> [String: Any] {
        try JSONSerialization.jsonObject(with: Data(contentsOf: repoRoot.appendingPathComponent("client/testdata/kb-waiting.json"))) as! [String: Any]
    }

    @Test func sharedFixture() async throws {
        let f = try fixture()
        for l in f["lines"] as! [[String: Any]] {
            #expect(ProjectEnv.waitingLine(l["n"] as! Int, l["slug"] as! String) == l["want"] as! String)
        }
        for l in f["teamLines"] as! [[String: Any]] {
            #expect(ProjectEnv.teamWaitingLine(l["n"] as! Int, l["team"] as! String) == l["want"] as! String)
        }
        for a in f["teamAnswers"] as! [[String: Any]] {
            let w = try HookWorld()
            let body = Data((a["body"] as! String).utf8)
            let box = AskedBox()
            w.env.send = { _, _, path, _, _ in box.path = path; return ProjectReply(status: 200, data: body) }
            let env = w.env!
            let r = await offPool { env.reviewWaitingTeams(ProjectConn(url: "", token: ""), 7, limit: 1, teams: true) }
            #expect(r.n == a["wantN"] as! Int, "\(a["name"] as! String)")
            #expect(r.teams.map { "\($0.name):\($0.waiting)" } == a["want"] as! [String], "\(a["name"] as! String)")
            #expect(box.path.hasPrefix("/minigw/v1/projects/7/kb/review?after=") && box.path.hasSuffix("&teams=1"))
        }
        for a in f["answers"] as! [[String: Any]] {
            let w = try HookWorld()
            let status = a["status"] as! Int, body = Data((a["body"] as! String).utf8)
            let box = AskedBox()
            w.env.send = { _, _, path, _, _ in box.path = path; return ProjectReply(status: status, data: body) }
            let env = w.env!
            let n = await offPool { env.reviewWaiting(ProjectConn(url: "", token: ""), 7, limit: 1) }
            #expect(n == a["want"] as! Int, "\(a["name"] as! String)")
            #expect(box.path.hasPrefix("/minigw/v1/projects/7/kb/review?after="))
        }
    }

    private final class AskedBox: @unchecked Sendable { var path = "" }

    /// The blocking calls (a gateway request with its wait) go on a thread of their own, never on one of the concurrency pool (offPool).
    private func start(_ w: HookWorld) async { let e = w.env!; await offPool { e.kbHook(["start"]) } }
    private func show(_ w: HookWorld) async throws { let e = w.env!; try await offPool { try e.project(["show"]) } }

    /// slow: the gateway's answer is held until `gate` is signalled (no sleep, no timing assertion: that the hook returns at all shows the deadline works).
    private func world(waiting: Int?, optIn: Bool, gate: DispatchSemaphore? = nil) throws -> HookWorld {
        let w = try HookWorld(stdin: Data(#"{"session_id":"s1","cwd":"\#(try Projects.realFolder(NSTemporaryDirectory()))"}"#.utf8))
        try w.link("gw1", 7, "acme-shop")
        let input = Data(#"{"session_id":"s1","cwd":"\#(w.work)"}"#.utf8)
        w.env.stdin = { input }
        w.env.gitState = { _ in ("h", "s") }
        if optIn { try w.write("kb-hooks.json", ["version": 1, "enabled": [["gateway": "gw1", "project": 7]]]) }
        w.env.hookConn = { ProjectConn(url: "", token: "") }
        w.env.waitLimit = 0.1
        w.env.send = { _, _, _, _, _ in
            gate?.wait()
            guard let n = waiting else { return ProjectReply(status: 404, data: Data(#"{"error":{"message":"not found"}}"#.utf8)) }
            return ProjectReply(status: 200, data: Data(#"{"submissions":[],"next":0,"waiting":\#(n)}"#.utf8))
        }
        return w
    }

    @Test(.blocking) func hookLineOnlyWhenOptedInAndOnlyCountAndProject() async throws {
        let line = "2 submissions wait for your review in acme-shop: minigw kb review acme-shop\n"
        var w = try world(waiting: 2, optIn: true)
        await start(w)
        #expect(w.out.hasPrefix("Mini Gateway: this folder is linked to project acme-shop"))
        #expect(w.out.hasSuffix(line))
        w = try world(waiting: 2, optIn: false)
        await start(w)
        #expect(w.out.isEmpty)
        w = try world(waiting: nil, optIn: true)  // not a reviewer: the gateway's 404
        await start(w)
        #expect(!w.out.contains("wait") && w.out.hasPrefix("Mini Gateway:"))
        w = try world(waiting: 0, optIn: true)
        await start(w)
        #expect(!w.out.contains("wait"))
    }

    @Test(.blocking) func hookTimeoutPrintsNothing() async throws {
        let gate = DispatchSemaphore(value: 0)
        let w = try world(waiting: 5, optIn: true, gate: gate)
        await start(w)  // returns after waitLimit (0.1 s) although the gateway has not answered
        gate.signal()
        #expect(!w.out.contains("wait") && w.out.hasPrefix("Mini Gateway:"))
    }

    @Test(.blocking) func showLineAppearsAndDisappears() async throws {
        for (waiting, want) in [(1, "1 submission waits for your review in acme-shop: minigw kb review acme-shop\ngateway: gw1\n"), (0, "gateway: gw1\n")] {
            let w = try world(waiting: waiting, optIn: false)
            try await show(w)
            #expect(w.out.hasSuffix(want), "\(waiting)")
            #expect(w.out.contains("project:  acme-shop (id 7)"))
        }
        let w = try world(waiting: nil, optIn: false)  // a developer: nothing
        try await show(w)
        #expect(!w.out.contains("wait"))
    }
}
