import Foundation
import Testing
@testable import RouterCore

@Suite struct SyncGitignoreNotice {
    @Test func namesWhatOnlyTheRootGitignoreLeavesOut() throws {
        let root = FileManager.default.temporaryDirectory.appendingPathComponent("sync-gi-\(UUID().uuidString)")
        let fm = FileManager.default
        try fm.createDirectory(at: root, withIntermediateDirectories: true)
        defer { try? fm.removeItem(at: root) }
        for d in [".omc", "build", "src", ".git"] { try fm.createDirectory(at: root.appendingPathComponent(d), withIntermediateDirectories: true) }
        try "x".write(to: root.appendingPathComponent("secret.env"), atomically: true, encoding: .utf8)
        try "x".write(to: root.appendingPathComponent("a.go"), atomically: true, encoding: .utf8)
        try ".omc/\nbuild/\n*.env\n".write(to: root.appendingPathComponent(".gitignore"), atomically: true, encoding: .utf8)
        let hidden = Sync.gitignoreHidden(root)
        #expect(hidden == [".omc", "secret.env"], "\(hidden)")  // build and .git belong to the built-in list, src is not ignored
        #expect(Sync.gitignoreNotice(hidden) == "sync: not copied because .gitignore lists them: .omc, secret.env (use direct mode or put what the agent needs in the prompt)")
        #expect(Sync.gitignoreNotice([]) == nil)
        let many = Sync.gitignoreNotice(["a", "b", "c", "d", "e", "f"]) ?? ""
        #expect(many.contains("a, b, c, d, e, … (use direct") && !many.contains("f,") && !many.contains(" f "))
        try "build/\n".write(to: root.appendingPathComponent(".gitignore"), atomically: true, encoding: .utf8)
        #expect(Sync.gitignoreHidden(root).isEmpty)
    }
}
