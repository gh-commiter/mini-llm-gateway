import Foundation
import Testing
@testable import RouterCore

@Suite struct RunLabelsParity {
    @Test func flagsAndEnvironment() {
        nonisolated(unsafe) var warns: [String] = []
        let a = RunLabels.take(["-p", "--minigw-phase", "Plan", "--minigw-activity=bugfix", "--minigw-task", "#149", "hello"], env: [:]) { warns.append($0) }
        #expect(a.labels == RunLabels(phase: "plan", activity: "bugfix", task: "#149", taskSource: "given") && a.rest == ["-p", "hello"] && warns.isEmpty)
        let b = RunLabels.take(["--minigw-task=feature/x"], env: ["MINIGW_PHASE": "act", "MINIGW_ACTIVITY": "Triage", "MINIGW_TASK": "old"]) { warns.append($0) }
        #expect(b.labels == RunLabels(phase: "act", activity: "triage", task: "feature/x", taskSource: "given"))
    }

    @Test func aValueThatIsNotATokenIsDroppedAndTheRestIsUntouched() {
        nonisolated(unsafe) var warns: [String] = []
        let r = RunLabels.take(["--minigw-phase", "not a token!", "--minigw-activity=", "--minigw-task=" + String(repeating: "x", count: 65), "--minigw-phase", "!!", "x"], env: [:]) { warns.append($0) }
        #expect(r.labels == RunLabels() && r.rest == ["x"] && warns.count == 2)  // the last phase wins: one warning per final value
        warns = []
        let t = RunLabels.take(["a", "--minigw-task"], env: [:]) { warns.append($0) }
        #expect(t.labels.task.isEmpty && t.rest == ["a"] && warns.count == 1)
    }

    @Test func branchFillsOnlyAnEmptyTaskOfALinkedFolder() {
        var l = RunLabels()
        l.withBranch(cwd: "x", linkRoot: { _ in "root" }, branch: { _, _ in "feature/149-usage" })
        #expect(l.task == "feature/149-usage" && l.taskSource == "branch")
        var given = RunLabels(task: "#1", taskSource: "given")
        given.withBranch(cwd: "x", linkRoot: { _ in "root" }, branch: { _, _ in "main" })
        #expect(given.task == "#1" && given.taskSource == "given")
        var none = RunLabels()
        none.withBranch(cwd: "x", linkRoot: { _ in "" }, branch: { _, _ in "main" })
        none.withBranch(cwd: "x", linkRoot: { _ in "root" }, branch: { _, _ in "" })
        none.withBranch(cwd: "x", linkRoot: { _ in "root" }, branch: { _, _ in "bad branch name" })
        #expect(none.task.isEmpty && none.taskSource.isEmpty)
        #expect(RunLabels(phase: "plan", task: "feature/x", taskSource: "branch").headers == "\nx-claude-mini-phase: plan\nx-claude-mini-task: feature/x\nx-claude-mini-task-source: branch")
    }

    @Test func gitBranchReadsFilesOnly() throws {
        let dir = NSTemporaryDirectory() + "labels-" + UUID().uuidString
        let fm = FileManager.default
        try fm.createDirectory(atPath: dir + "/repo/.git", withIntermediateDirectories: true)
        try fm.createDirectory(atPath: dir + "/repo/sub/deep", withIntermediateDirectories: true)
        try "ref: refs/heads/feature/x\n".write(toFile: dir + "/repo/.git/HEAD", atomically: true, encoding: .utf8)
        #expect(RunLabels.gitBranch(dir + "/repo/sub/deep", root: dir + "/repo") == "feature/x")
        try "0123456789012345678901234567890123456789\n".write(toFile: dir + "/repo/.git/HEAD", atomically: true, encoding: .utf8)
        #expect(RunLabels.gitBranch(dir + "/repo", root: dir + "/repo") == "")
        try fm.createDirectory(atPath: dir + "/repo/.git/worktrees/wt", withIntermediateDirectories: true)
        try "ref: refs/heads/topic\n".write(toFile: dir + "/repo/.git/worktrees/wt/HEAD", atomically: true, encoding: .utf8)
        try fm.createDirectory(atPath: dir + "/wt", withIntermediateDirectories: true)
        try ("gitdir: " + dir + "/repo/.git/worktrees/wt\n").write(toFile: dir + "/wt/.git", atomically: true, encoding: .utf8)
        #expect(RunLabels.gitBranch(dir + "/wt", root: dir + "/wt") == "topic")
        // A repository ABOVE the linked folder gives no branch for a project it is not part of.
        try "ref: refs/heads/outer\n".write(toFile: dir + "/repo/.git/HEAD", atomically: true, encoding: .utf8)
        try fm.createDirectory(atPath: dir + "/repo/linked/deeper", withIntermediateDirectories: true)
        #expect(RunLabels.gitBranch(dir + "/repo/linked/deeper", root: dir + "/repo/linked") == "")
        #expect(RunLabels.gitBranch(dir + "/repo/linked/deeper", root: dir + "/repo") == "outer")
    }

    @Test func aValueIsOneTokenOrNothingNeverReparsed() {
        nonisolated(unsafe) var warns = 0
        for v in ["x --minigw-work=direct", "x; rm", "$(id)", "x --minigw-tools=edit"] {
            let r = RunLabels.take(["--minigw-task", v, "--minigw-phase=" + v, "-p", "hi"], env: [:]) { _ in warns += 1 }
            #expect(r.rest == ["-p", "hi"] && (r.labels.task.isEmpty || r.labels.task == v) && !r.labels.phase.contains(" ") && !r.labels.phase.contains(";"))
            #expect(!r.rest.contains { $0.contains("--minigw-") })
        }
        #expect(warns > 0)
    }

    /// The shared cases (client/testdata/labels.json) are asserted by the Go client too (labels_test.go).
    @Test func sharedCases() throws {
        let repo = URL(fileURLWithPath: #filePath).deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent()
        let f = try JSONSerialization.jsonObject(with: Data(contentsOf: repo.appendingPathComponent("client/testdata/labels.json"))) as! [String: Any]
        for c in f["cases"] as! [[String: Any]] {
            nonisolated(unsafe) var warns = 0
            let r = RunLabels.take(c["args"] as! [String], env: c["env"] as! [String: String]) { _ in warns += 1 }
            let want = RunLabels(phase: c["phase"] as! String, activity: c["activity"] as! String, task: c["task"] as! String, taskSource: c["taskSource"] as! String)
            #expect(r.labels == want && r.rest == (c["rest"] as! [String]) && warns == (c["warnings"] as! Int), "\(c["name"] as! String)")
        }
    }

    /// A FIFO, a link to a FIFO, a huge file or a directory in place of HEAD (also behind a gitdir: file) is "no branch", at once.
    @Test(.blocking) func oddFilesNeverBlockTheBranchRead() throws {
        let fm = FileManager.default
        let dir = NSTemporaryDirectory() + "labels-odd-" + UUID().uuidString
        func repo(_ name: String) throws -> String {
            let r = dir + "/" + name
            try fm.createDirectory(atPath: r + "/.git", withIntermediateDirectories: true)
            return r
        }
        func within(_ what: String, _ path: String) {
            nonisolated(unsafe) var got: String?
            let done = DispatchSemaphore(value: 0)
            // Its own thread, not the shared dispatch pool: in a full parallel run the pool can take seconds to hand out a thread, and the
            // test then reported "blocked" for a read that had not even started. The read itself must return at once; the deadline only
            // has to tell "returns" from "never returns".
            let t = Thread { got = RunLabels.gitBranch(path, root: path); done.signal() }
            t.stackSize = 1 << 20; t.start()
            if done.wait(timeout: .now() + 20) == .timedOut { Issue.record("\(what): blocked"); return }
            #expect(got == "", "\(what)")
        }
        var r = try repo("fifo")
        #expect(mkfifo(r + "/.git/HEAD", 0o600) == 0)
        within("FIFO as HEAD", r)
        r = try repo("gitdir")
        let target = dir + "/target"
        try fm.createDirectory(atPath: target, withIntermediateDirectories: true)
        #expect(mkfifo(target + "/HEAD", 0o600) == 0)
        try fm.removeItem(atPath: r + "/.git")
        try ("gitdir: " + target + "\n").write(toFile: r + "/.git", atomically: true, encoding: .utf8)
        within("FIFO as the gitdir target's HEAD", r)
        r = try repo("link")
        #expect(mkfifo(dir + "/f", 0o600) == 0)
        try fm.createSymbolicLink(atPath: r + "/.git/HEAD", withDestinationPath: dir + "/f")
        within("a link to a FIFO", r)
        r = try repo("huge")
        try ("ref: refs/heads/" + String(repeating: "x", count: 6000)).write(toFile: r + "/.git/HEAD", atomically: true, encoding: .utf8)
        within("a huge file", r)
        r = try repo("dirhead")
        try fm.createDirectory(atPath: r + "/.git/HEAD", withIntermediateDirectories: true)
        within("a directory named HEAD", r)
    }
}

