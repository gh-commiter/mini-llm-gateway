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
        l.withBranch(cwd: "x", linked: { _ in true }, branch: { _ in "feature/149-usage" })
        #expect(l.task == "feature/149-usage" && l.taskSource == "branch")
        var given = RunLabels(task: "#1", taskSource: "given")
        given.withBranch(cwd: "x", linked: { _ in true }, branch: { _ in "main" })
        #expect(given.task == "#1" && given.taskSource == "given")
        var none = RunLabels()
        none.withBranch(cwd: "x", linked: { _ in false }, branch: { _ in "main" })
        none.withBranch(cwd: "x", linked: { _ in true }, branch: { _ in "" })
        none.withBranch(cwd: "x", linked: { _ in true }, branch: { _ in "bad branch name" })
        #expect(none.task.isEmpty && none.taskSource.isEmpty)
        #expect(RunLabels(phase: "plan", task: "feature/x", taskSource: "branch").headers == "\nx-claude-mini-phase: plan\nx-claude-mini-task: feature/x\nx-claude-mini-task-source: branch")
    }

    @Test func gitBranchReadsFilesOnly() throws {
        let dir = NSTemporaryDirectory() + "labels-" + UUID().uuidString
        let fm = FileManager.default
        try fm.createDirectory(atPath: dir + "/repo/.git", withIntermediateDirectories: true)
        try fm.createDirectory(atPath: dir + "/repo/sub/deep", withIntermediateDirectories: true)
        try "ref: refs/heads/feature/x\n".write(toFile: dir + "/repo/.git/HEAD", atomically: true, encoding: .utf8)
        #expect(RunLabels.gitBranch(dir + "/repo/sub/deep") == "feature/x")
        try "0123456789012345678901234567890123456789\n".write(toFile: dir + "/repo/.git/HEAD", atomically: true, encoding: .utf8)
        #expect(RunLabels.gitBranch(dir + "/repo") == "")
        try fm.createDirectory(atPath: dir + "/repo/.git/worktrees/wt", withIntermediateDirectories: true)
        try "ref: refs/heads/topic\n".write(toFile: dir + "/repo/.git/worktrees/wt/HEAD", atomically: true, encoding: .utf8)
        try fm.createDirectory(atPath: dir + "/wt", withIntermediateDirectories: true)
        try ("gitdir: " + dir + "/repo/.git/worktrees/wt\n").write(toFile: dir + "/wt/.git", atomically: true, encoding: .utf8)
        #expect(RunLabels.gitBranch(dir + "/wt") == "topic")
    }
}
