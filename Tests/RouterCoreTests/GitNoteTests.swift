import Foundation
import Testing
@testable import RouterCore

// #112: the git sentence of the agent's note comes from the share guard's rules; client/testdata/git-notes.json is asserted here and in client/gitnote_test.go.
private struct GitNoteCases: Decodable {
    struct Case: Decodable { let Mode: String; let Shell: Bool; let ReadOnly: Bool; let Note: String }
    let cases: [Case]
    let Hints: [[String]]
}

private func load() throws -> GitNoteCases {
    let root = URL(fileURLWithPath: #filePath).deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent()
    return try JSONDecoder().decode(GitNoteCases.self, from: Data(contentsOf: root.appendingPathComponent("client/testdata/git-notes.json")))
}

@Test func gitNoteMatchesTheSharedTable() throws {
    let c = try load()
    #expect(!c.cases.isEmpty)
    for k in c.cases {
        #expect(GitNote.text(mode: k.Mode, shell: k.Shell, readOnly: k.ReadOnly) == k.Note, "\(k.Mode) shell \(k.Shell) ro \(k.ReadOnly)")
        #expect(k.Note.count < 700)
    }
    for h in c.Hints { #expect((GitNote.refusalHint(h[0]) ?? "") == h[1], "\(h[0])") }
}

/// A command the note says works must not write a path the rules refuse, and one it says is not possible must not be writable.
@Test func gitNoteAgreesWithTheRules() {
    let note = GitNote.text(mode: "direct", shell: true, readOnly: false)
    let afterWorks = note.components(separatedBy: "commit in this folder: ")[1].components(separatedBy: ". Not possible in this folder: ")
    let works = afterWorks[0].components(separatedBy: ", "), refused = afterWorks[1].components(separatedBy: ". Use merge")[0].components(separatedBy: ", ")
    for op in GitNote.ops {
        let allowed = op.paths.filter { !Sync.isProtected(".git/" + $0, git: true) }.count
        if works.contains(op.name) { #expect(allowed == op.paths.count, "\(op.name) is said to work") }
        if refused.contains(op.name) { #expect(allowed == 0, "\(op.name) is said to be refused") }
    }
}

// Every agent kind that gets a note gets the sentence: mountNote is the one place (Claude, Codex, Grok, Copilot).
@Test func mountNoteCarriesTheGitSentence() {
    let dir = URL(fileURLWithPath: "/m")
    #expect(Router.mountNote("/c", dir, readOnly: false).contains("Git: you can commit in this folder"))
    #expect(Router.mountNote("/c", dir, readOnly: false, shell: false).contains("Git: you have no shell here"))
    #expect(Router.mountNote("/c", dir, readOnly: true).contains("Git: the folder is read-only"))
    #expect(Router.mountNote("/c", dir, readOnly: false, synced: true).contains("there is no .git in this copy"))
    #expect(!Router.mountNote("/c", dir, readOnly: false, branch: "minigw/x", gits: ["/m/.git"]).contains("Git:"))  // worktree: its note says how commits work
}

@Test func theRefusalLineNamesTheAlternative() {
    var log = WebDAV.RefusalLog()
    #expect(log.line(".git/rebase-merge/x") == "minigw: refused agent write to \".git/rebase-merge/x\" (protected path: rebase is not available in direct mode; use merge, or worktree mode)\n")
    #expect(log.line(".git/hooks/x") == "minigw: refused agent write to \".git/hooks/x\" (protected path)\n")
}
