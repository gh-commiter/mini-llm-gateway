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

    /// The shared fixture client/testdata/sync-gitignore.json (asserted by client/sync_gitignore_test.go too): what a sync sends and says.
    @Test func sharedFixtureMatchesGo() throws {
        var repo = URL(fileURLWithPath: #filePath)
        for _ in 0..<3 { repo.deleteLastPathComponent() }
        let fx = try JSONSerialization.jsonObject(with: Data(contentsOf: repo.appendingPathComponent("client/testdata/sync-gitignore.json"))) as! [String: Any]
        let fm = FileManager.default
        for c in fx["cases"] as! [[String: Any]] {
            let name = c["name"] as! String
            let root = fm.temporaryDirectory.appendingPathComponent("sync-gi-fx-\(UUID().uuidString)")
            try fm.createDirectory(at: root, withIntermediateDirectories: true)
            defer { try? fm.removeItem(at: root) }
            for (p, content) in c["tree"] as! [String: String] {
                let url = root.appendingPathComponent(p)
                try fm.createDirectory(at: url.deletingLastPathComponent(), withIntermediateDirectories: true)
                try content.write(to: url, atomically: true, encoding: .utf8)
            }
            let sent = Sync.scan(root, trusted: true).files.keys.sorted()
            #expect(sent == c["synced"] as! [String], "\(name)")
            #expect(Sync.gitignoreNotice(Sync.gitignoreHidden(root)) == c["notice"] as? String, "\(name)")
        }
    }
}
