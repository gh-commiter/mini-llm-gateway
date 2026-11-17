import Foundation

/// Labels of a run for the gateway's usage reports: --minigw-phase (MINIGW_PHASE), --minigw-activity (MINIGW_ACTIVITY),
/// --minigw-task (MINIGW_TASK). Data, never a reason to refuse or delay a run: a missing label is empty, an unusual value that is
/// a safe token goes as given, a value that is not one is dropped with one warning line. A port of client/labels.go.
public struct RunLabels: Equatable, Sendable {
    public var phase = "", activity = "", task = "", taskSource = ""  // taskSource: given, branch or ""
    public init(phase: String = "", activity: String = "", task: String = "", taskSource: String = "") {
        self.phase = phase; self.activity = activity; self.task = task; self.taskSource = taskSource
    }

    private static func ok(_ s: String, max: Int, extra: String) -> Bool {
        guard (1...max).contains(s.utf8.count) else { return false }
        return s.unicodeScalars.allSatisfy { ($0.isASCII) && (CharacterSet.alphanumerics.contains($0) || ".-_".unicodeScalars.contains($0) || extra.unicodeScalars.contains($0)) }
    }
    /// phase and activity: lower case, ^[a-z0-9._-]{1,32}$ ("" if not).
    public static func safeWord(_ s: String) -> String {
        let v = s.trimmingCharacters(in: .whitespacesAndNewlines).lowercased()
        return ok(v, max: 32, extra: "") ? v : ""
    }
    /// task: ^[A-Za-z0-9._#/-]{1,64}$ ("" if not).
    public static func safeTask(_ s: String) -> String {
        let v = s.trimmingCharacters(in: .whitespacesAndNewlines)
        return ok(v, max: 64, extra: "#/") ? v : ""
    }

    /// Takes the label flags out of args (the last one wins; a flag beats the environment) and validates them.
    public static func take(_ args: [String], env: [String: String], warn: (String) -> Void) -> (labels: RunLabels, rest: [String]) {
        var vals = ["phase": env["MINIGW_PHASE"] ?? "", "activity": env["MINIGW_ACTIVITY"] ?? "", "task": env["MINIGW_TASK"] ?? ""]
        var rest: [String] = []
        var i = 0
        while i < args.count {
            var matched = false
            for n in ["phase", "activity", "task"] {
                let flag = "--minigw-" + n
                if args[i] == flag {
                    matched = true
                    if i + 1 < args.count && !args[i + 1].hasPrefix("-") { vals[n] = args[i + 1]; i += 1 }  // a following option is not the value: it stays
                    else { warn("--minigw-\(n) has no value: ignored") }
                } else if args[i].hasPrefix(flag + "=") {
                    matched = true
                    vals[n] = String(args[i].dropFirst(flag.count + 1))
                }
                if matched { break }
            }
            if !matched { rest.append(args[i]) }
            i += 1
        }
        func check(_ n: String, _ safe: (String) -> String) -> String {
            guard let v = vals[n], !v.isEmpty else { return "" }
            let s = safe(v)
            if s.isEmpty { warn("--minigw-\(n) ignored: \(n.prefix(1).uppercased() + n.dropFirst()) must be a short token (letters, digits and . _ -\(n == "task" ? " # /" : ""))") }
            return s
        }
        var l = RunLabels(phase: check("phase", safeWord), activity: check("activity", safeWord))
        l.task = check("task", safeTask)
        if !l.task.isEmpty { l.taskSource = "given" }
        return (l, rest)
    }

    /// An empty task is filled with the git branch of the folder when it is linked to a project; nothing for a detached HEAD or no repository.
    public mutating func withBranch(cwd: String, linked: (String) -> Bool, branch: (String) -> String) {
        guard task.isEmpty, linked(cwd) else { return }
        let b = Self.safeTask(branch(cwd))
        if !b.isEmpty { task = b; taskSource = "branch" }
    }

    /// The request header lines that carry the labels (each only when set), each starting with a newline.
    public var headers: String {
        var h = ""
        for (n, v) in [("phase", phase), ("activity", activity), ("task", task), ("task-source", taskSource)] where !v.isEmpty { h += "\nx-claude-mini-\(n): \(v)" }
        return h
    }

    /// The folder or a parent is a linked project folder (on any gateway).
    public static func folderLinked(_ cwd: String, statePath: String) -> Bool {
        guard let st = try? Projects.load(statePath), var dir = try? Projects.realFolder(cwd) else { return false }
        while true {
            if st.links.contains(where: { Projects.samePath(dir, $0.path) }) { return true }
            let up = Projects.parent(dir)
            if up == dir { return false }
            dir = up
        }
    }

    /// The current branch from the repository's HEAD file ("" for a detached HEAD, no repository or anything unexpected); a worktree's
    /// .git file is followed one step. Files only: no git process, no code of the checkout runs.
    public static func gitBranch(_ start: String) -> String {
        var dir = start
        while true {
            let g = dir + "/.git"
            var st = stat()
            if lstat(g, &st) == 0, (st.st_mode & S_IFMT) != S_IFLNK {
                var gitDir = g
                if (st.st_mode & S_IFMT) == S_IFREG {
                    let text = small(g).trimmingCharacters(in: .whitespacesAndNewlines)
                    guard text.hasPrefix("gitdir:") else { return "" }
                    gitDir = String(text.dropFirst("gitdir:".count)).trimmingCharacters(in: .whitespaces)
                    if !gitDir.hasPrefix("/") { gitDir = dir + "/" + gitDir }
                } else if (st.st_mode & S_IFMT) != S_IFDIR { return "" }
                // Only <gitDir>/HEAD is ever read from there, as a small regular file (never a FIFO, a link or a directory).
                let head = small(gitDir + "/HEAD").trimmingCharacters(in: .whitespacesAndNewlines)
                guard head.hasPrefix("ref: refs/heads/") else { return "" }
                return String(head.dropFirst("ref: refs/heads/".count))
            }
            let up = (dir as NSString).deletingLastPathComponent
            if up.isEmpty || up == dir { return "" }
            dir = up
        }
    }

    /// A small file of a repository: opened once without following a link and without waiting for a FIFO, checked on the descriptor
    /// (a regular file of at most 4096 bytes), else "".
    static func small(_ path: String) -> String {
        let fd = open(path, O_RDONLY | O_NONBLOCK | O_NOFOLLOW)
        guard fd >= 0 else { return "" }
        defer { close(fd) }
        var st = stat()
        guard fstat(fd, &st) == 0, (st.st_mode & S_IFMT) == S_IFREG, st.st_size <= 4096 else { return "" }
        var buf = [UInt8](repeating: 0, count: 4096)
        let n = read(fd, &buf, buf.count)
        return n > 0 ? String(decoding: buf[0..<n], as: UTF8.self) : ""
    }
}
