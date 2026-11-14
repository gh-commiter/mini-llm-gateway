import Foundation

/// Git limits of a run, told to the agent in its system note (#112; client/gitnote.go says the same, client/testdata/git-notes.json is asserted by both).
/// The lists are computed from the share guard's rules (Sync.isProtected), not written by hand: a command works when every path it writes in `.git` is
/// writable in direct mode, and is not possible when every one is refused.
enum GitNote {
    struct Op { let name: String; let paths: [String] }  // paths inside .git the command writes

    static let ops: [Op] = [
        Op(name: "add", paths: ["index.lock", "index", "objects/ab/cdef"]),
        Op(name: "commit", paths: ["index.lock", "COMMIT_EDITMSG", "refs/heads/x", "logs/HEAD", "objects/ab/cdef"]),
        Op(name: "commit --amend", paths: ["index.lock", "COMMIT_EDITMSG", "refs/heads/x", "logs/HEAD", "ORIG_HEAD"]),
        Op(name: "branch", paths: ["refs/heads/x", "logs/refs/heads/x"]),
        Op(name: "tag", paths: ["refs/tags/x", "TAG_EDITMSG"]),
        Op(name: "checkout", paths: ["HEAD", "index.lock", "logs/HEAD"]),
        Op(name: "stash", paths: ["refs/stash", "index.stash.123", "logs/refs/stash"]),
        Op(name: "merge", paths: ["MERGE_HEAD", "MERGE_MSG", "MERGE_MODE", "ORIG_HEAD", "AUTO_MERGE"]),
        Op(name: "single cherry-pick/revert", paths: ["CHERRY_PICK_HEAD", "REVERT_HEAD", "AUTO_MERGE", "MERGE_MSG"]),
        Op(name: "rebase", paths: ["rebase-merge/head-name", "rebase-apply/patch"]),
        Op(name: "multi-commit cherry-pick/revert", paths: ["sequencer/todo"]),
        Op(name: "git worktree add", paths: ["worktrees/x/HEAD", "worktrees/x/gitdir"]),
        Op(name: "changing .git/config, hooks, info/attributes or remotes", paths: ["config.lock", "hooks/pre-commit", "info/attributes", "remotes/origin"]),
    ]

    static func directLists() -> (works: [String], refused: [String]) {
        var works: [String] = [], refused: [String] = []
        for op in ops {
            let writable = op.paths.map { !Sync.isProtected(".git/" + $0, git: true) }
            if writable.allSatisfy({ $0 }) { works.append(op.name) } else if !writable.contains(true) { refused.append(op.name) }
        }
        return (works, refused)
    }

    /// The git sentence of the agent's note. mode: "direct", "sync" or "worktree" ("" for worktree: its note already says how commits work);
    /// shell: the agent can run commands; readOnly: --minigw-ro or a cap.
    static func text(mode: String, shell: Bool, readOnly: Bool) -> String {
        if !shell { return "Git: you have no shell here, so no git commands, and the file tools cannot touch .git. If the task needs a commit, say so; the user commits your changes." }
        if readOnly { return "Git: the folder is read-only for you: no changes and no commits." }
        switch mode {
        case "sync":
            return "Git: there is no .git in this copy (git data is not copied), so git commands do not see the user's repository and nothing you commit goes back. Do not run git init or git commit here; the changed files are copied back and the user commits them."
        case "direct":
            let (works, refused) = directLists()
            return "Git: you can commit in this folder: " + works.joined(separator: ", ") + ". Not possible in this folder: " + refused.joined(separator: ", ")
                + ". Use merge instead of rebase. If the task needs one of these, say so and tell the user to run it themselves or to use worktree mode (minigw --minigw-work=worktree)."
        default: return ""
        }
    }

    /// Where a refused git path has an alternative, the refusal names it (the client's stderr line and the 403 text).
    static func refusalHint(_ rel: String) -> String? {
        let c = rel.lowercased().split(separator: "/").map(String.init)
        guard let i = c.firstIndex(of: ".git"), i + 1 < c.count else { return nil }
        switch c[i + 1] {
        case "rebase-merge", "rebase-apply": return "rebase is not available in direct mode; use merge, or worktree mode"
        case "sequencer": return "a cherry-pick or revert of several commits is not available in direct mode; do them one by one, or use worktree mode"
        case "worktrees": return "git worktree is not available in direct mode; use worktree mode (minigw --minigw-work=worktree)"
        default: return nil
        }
    }
}
