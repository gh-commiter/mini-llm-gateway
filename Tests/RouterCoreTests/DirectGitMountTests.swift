import Foundation
import Synchronization
import Testing
@testable import RouterCore

// 0.6.0 (#112) end to end on this Mac, no network beyond loopback: the client folder is served by WebDAV.handle (the client's side of the share, with its
// guard) through a loopback HTTP server, mounted by the system's mount_webdav as the gateway does, and a process that stands for the agent runs git in the
// mount. A commit appears in the client's repository; what can run code (hooks, config, attributes, alternates, a rebase todo) is refused with the client's
// refusal line.

private final class DAVLoopback: @unchecked Sendable {
    let root: URL, prefix: String
    private var listener: Int32 = -1
    private(set) var port: UInt16 = 0
    private let seen = Mutex<[String]>([])  // requests that reached the client side, newest last (for the failure message)
    var writes: [String] { seen.withLock { $0.filter { !$0.hasPrefix("PROPFIND") && !$0.hasPrefix("GET")  && !$0.hasPrefix("HEAD") && !$0.hasPrefix("OPTIONS") } } }
    var summary: String { seen.withLock { "\($0.count) requests reached the client side, last: " + $0.suffix(6).joined(separator: "; ") } }

    init?(root: URL, prefix: String) {
        self.root = root; self.prefix = prefix
        listener = socket(AF_INET, SOCK_STREAM, 0)
        var yes: Int32 = 1
        setsockopt(listener, SOL_SOCKET, SO_REUSEADDR, &yes, socklen_t(MemoryLayout<Int32>.size))
        var addr = sockaddr_in()
        addr.sin_family = sa_family_t(AF_INET); addr.sin_len = UInt8(MemoryLayout<sockaddr_in>.size)
        addr.sin_addr.s_addr = inet_addr("127.0.0.1"); addr.sin_port = 0
        let ok = withUnsafePointer(to: &addr) { $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { bind(listener, $0, socklen_t(MemoryLayout<sockaddr_in>.size)) } }
        guard ok == 0, listen(listener, 16) == 0 else { return nil }
        var len = socklen_t(MemoryLayout<sockaddr_in>.size)
        _ = withUnsafeMutablePointer(to: &addr) { $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { getsockname(listener, $0, &len) } }
        port = UInt16(bigEndian: addr.sin_port)
        Thread.detachNewThread { [self] in
            while true {
                let c = accept(listener, nil, nil)
                if c < 0 { return }
                Thread.detachNewThread { self.serve(c) }
            }
        }
    }

    func stop() { close(listener) }

    /// One request per connection, as the router's server (HTTPParser has no keep-alive).
    private func serve(_ c: Int32) {
        defer { close(c) }
        var buf = Data(), chunk = [UInt8](repeating: 0, count: 65536)
        while true {
            switch HTTPParser.parse(buf) {
            case .needMore:
                let n = read(c, &chunk, chunk.count)
                if n <= 0 { return }
                buf.append(contentsOf: chunk[0..<n])
            case .error(let s, _):
                send(c, HTTPResponse.head(status: s, headers: [], contentLength: 0)); return
            case .request(let req):
                let r = WebDAV.handle(method: req.method, path: req.path, headers: req.headers, body: req.body, root: root, prefix: prefix)
                seen.withLock { $0.append(req.method + " " + req.path + (req.method == "PROPFIND" || req.method == "GET" ? "" : " -> \(r.status)")) }
                var headers = r.headers.map { ($0.key, $0.value) }
                let length = r.headers["Content-Length"] == nil ? r.body.count : nil
                if length == nil { headers = headers.filter { $0.0 != "Content-Length" } + [("Content-Length", r.headers["Content-Length"]!)] }
                send(c, HTTPResponse.head(status: r.status, headers: headers, contentLength: length) + r.body)
                return
            }
        }
    }

    private func send(_ c: Int32, _ data: Data) {
        data.withUnsafeBytes { p in
            var off = 0
            while off < data.count { let n = write(c, p.baseAddress! + off, data.count - off); if n <= 0 { return }; off += n }
        }
    }
}

private func sh(_ cmd: String, cwd: String, timeout: Double = 90) -> (status: Int32, out: String) {
    let p = Process(), pipe = Pipe()
    p.executableURL = URL(fileURLWithPath: "/bin/sh")
    p.arguments = ["-c", "export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1 GIT_TERMINAL_PROMPT=0 GIT_AUTHOR_NAME=n GIT_AUTHOR_EMAIL=a@b GIT_COMMITTER_NAME=n GIT_COMMITTER_EMAIL=a@b; " + cmd]
    p.currentDirectoryURL = URL(fileURLWithPath: cwd)
    p.standardOutput = pipe; p.standardError = pipe
    guard (try? p.run()) != nil else { return (-1, "cannot run") }
    var data = Data()
    let reader = Thread { data = pipe.fileHandleForReading.readDataToEndOfFile() }
    reader.start()
    let deadline = Date().addingTimeInterval(timeout)
    while p.isRunning && Date() < deadline { usleep(20_000) }
    if p.isRunning { p.terminate() }
    p.waitUntilExit()
    // The reader ends at EOF, and EOF comes only when EVERY holder of the pipe has closed it: a process the command left behind (a
    // daemon git started, a mount helper) keeps it open after the shell is gone, and a full test run once waited here for an hour.
    // Give it a moment, then close our end: the read fails and the thread ends with what it has.
    let patience = Date().addingTimeInterval(5)
    while reader.isExecuting && Date() < patience { usleep(5_000) }
    if reader.isExecuting { try? pipe.fileHandleForReading.close() }
    let last = Date().addingTimeInterval(5)
    while reader.isExecuting && Date() < last { usleep(5_000) }
    return (p.terminationStatus, String(decoding: data, as: UTF8.self))
}

/// For failure messages when a mount dies: the system's mount table for this mount point and the webdavfs processes (a dead agent = the OS dropped the mount).
private func webdavState(_ mp: URL) -> String {
    let table = sh("mount | grep -F '\(mp.path)' || echo 'not in the mount table'", cwd: "/", timeout: 10).out
    let procs = sh("ps -axo pid,etime,command | grep -i '[w]ebdavfs' || echo 'no webdavfs process'", cwd: "/", timeout: 10).out
    let load = sh("uptime", cwd: "/", timeout: 10).out
    return "mount table: \(table.trimmingCharacters(in: .whitespacesAndNewlines)); webdavfs: \(procs.trimmingCharacters(in: .whitespacesAndNewlines)); \(load.trimmingCharacters(in: .whitespacesAndNewlines))"
}

/// webdavfs keeps the data of a write the client refused and retries it, so a plain umount can wait for ever: force it, and never wait longer than 20 s.
private func forceUnmount(_ mp: URL) {
    for (exe, args) in [("/sbin/umount", ["-f", mp.path]), ("/usr/sbin/diskutil", ["unmount", "force", mp.path])] {
        let p = Process()
        p.executableURL = URL(fileURLWithPath: exe); p.arguments = args
        p.standardOutput = FileHandle.nullDevice; p.standardError = FileHandle.nullDevice
        guard (try? p.run()) != nil else { continue }
        let deadline = Date().addingTimeInterval(20)
        while p.isRunning && Date() < deadline { usleep(50_000) }
        if p.isRunning { kill(p.processIdentifier, SIGKILL) }
        p.waitUntilExit()
        if !ShareHub.isMounted(mp) { break }
    }
    rmdir(mp.path)
}

@Test func anAgentInADirectModeMountCommitsAndCannotPlantWhatRunsCode() throws {
    let fm = FileManager.default
    let base = try #require(Sync.real(fm.temporaryDirectory)).appendingPathComponent("direct-mount-" + UUID().uuidString, isDirectory: true)
    let client = base.appendingPathComponent("proj"), mp = base.appendingPathComponent("mnt")
    for d in [client, mp] { try fm.createDirectory(at: d, withIntermediateDirectories: true) }
    defer { try? fm.removeItem(at: base) }
    #expect(sh("git init -q && echo hi > a.txt && git add a.txt && git commit -qm init && git branch x", cwd: client.path).status == 0)
    let server = try #require(DAVLoopback(root: client, prefix: "/dav/e2e"))
    defer { server.stop() }  // runs after the unmount below: webdavfs still wants its server while it writes back
    let mounted = ShareHub.run("/sbin/mount_webdav", ["-S", "-v", "proj", "http://127.0.0.1:\(server.port)/dav/e2e/", mp.path]) == 0
    defer { forceUnmount(mp) }
    try #require(mounted, "mount_webdav to the loopback server")

    // the agent: edit, add, commit, branch, tag, switch, stash, merge a local branch
    for cmd in ["echo more >> a.txt && git add a.txt && git commit -qm agent", "git branch feature", "git tag v1", "git checkout -q -b topic", "echo again >> a.txt && git stash -q",
                "git checkout -q - && git merge -q --no-edit x", "git log --oneline | wc -l | grep -q 2", "git gc -q", "git repack -a -d -q && git prune --expire=now && git count-objects -v | grep -q '^count: 0'", "git gc --aggressive -q"] {
        let r = sh(cmd, cwd: mp.path)
        #expect(r.status == 0, "\(cmd): \(r) | mounted \(ShareHub.isMounted(mp)); \(server.summary)\(r.status == 0 ? "" : " | " + webdavState(mp))")
    }
    // the commit is in the client's own repository, which git itself finds sound
    let log = sh("git log --oneline", cwd: client.path)
    #expect(log.out.contains("agent") && log.out.contains("init"), "\(log)")
    #expect(sh("git branch --list feature topic && git tag --list v1 && git fsck --strict 2>&1 | grep -c -i -E 'error|fatal|missing'", cwd: client.path).out.hasSuffix("0\n"))

    // what runs code is refused with the client's refusal line (stderr of this process), and nothing appears in the client's folder
    // (the log of refusals is one line per path and process: start from an empty one; webdavfs writes back after close, so wait for the lines)
    let errFile = base.appendingPathComponent("stderr.txt")
    let saved = dup(2), fd = open(errFile.path, O_WRONLY | O_CREAT | O_TRUNC, 0o600)
    dup2(fd, 2)
    // the log is process-wide and a line is written once per path: reset it only now that stderr is ours, else another test's refusal of the
    // same path between the reset and the redirect would use the line up and send it to the real stderr
    WebDAV.refused.withLock { $0 = WebDAV.RefusalLog() }
    let denied = ["echo '# t' >> .git/config", "echo noop > .git/hooks/pre-commit", "echo noop > .git/info/attributes", "echo /x > .git/objects/info/alternates",
                  "mkdir .git/rebase-merge && echo noop > .git/rebase-merge/git-rebase-todo", "echo x > .git/shallow", "git config user.name evil"]
    for cmd in denied { _ = sh(cmd, cwd: mp.path) }
    var refusals = ""
    for _ in 0..<150 {
        refusals = (try? String(contentsOf: errFile, encoding: .utf8)) ?? ""
        if [".git/hooks/pre-commit\"", ".git/config", ".git/info/attributes\"", ".git/objects/info/alternates\""].allSatisfy(refusals.contains) { break }
        usleep(200_000)
    }
    dup2(saved, 2); close(saved); close(fd)
    // (the shell may report success for a write that webdavfs writes back after close and the client then refuses: the checks are the refusal line and the client's folder)
    let state = "mounted \(ShareHub.isMounted(mp)); \(server.summary)"  // no line = the writes never reached the client (a stalled mount) or the log was used up
    #expect(refusals.contains("minigw: refused agent write to \".git/hooks/pre-commit\" (protected path)"), "\(refusals) | \(state)")
    #expect(refusals.contains("minigw: refused agent write to \".git/config"), "\(refusals) | \(state)")
    for p in ["hooks/pre-commit", "info/attributes", "objects/info/alternates", "rebase-merge", "shallow", "config.lock"] {
        #expect(!fm.fileExists(atPath: client.appendingPathComponent(".git/" + p).path), "\(p)")
    }
    #expect(!sh("cat .git/config", cwd: client.path).out.contains("evil") && !sh("cat .git/config", cwd: client.path).out.contains("# t"))
}

/// 0.7.0: the REAL claude, launched with the gateway's own arguments (`agentArgs(directGit: true)`) and environment, in a real direct-mode mount (mount_webdav of a folder served by the
/// client's WebDAV code, `WebDAV.handle`, with its guard). Gated (the owner's subscription): MG_REAL_CLAUDE_GIT=1.
@Test(.enabled(if: ProcessInfo.processInfo.environment["MG_REAL_CLAUDE_GIT"] == "1"))
func aRealClaudeAgentCommitsInARealDirectModeMount() async throws {
    guard let exe = await ClaudeCLI.executable("") else { try Test.cancel("claude is not installed") }
    let fm = FileManager.default
    let base = try #require(Sync.real(fm.temporaryDirectory)).appendingPathComponent("direct-mount-real-" + UUID().uuidString, isDirectory: true)
    let client = base.appendingPathComponent("proj"), mp = base.appendingPathComponent("mnt")
    for d in [client, mp] { try fm.createDirectory(at: d, withIntermediateDirectories: true) }
    defer { try? fm.removeItem(at: base) }
    #expect(sh("git init -q && git config user.name Owner && git config user.email o@example.com && echo hi > a.txt && printf '#!/bin/sh\\n' > exec.sh && chmod 755 exec.sh && git add a.txt exec.sh && git commit -qm init", cwd: client.path).status == 0)
    let server = try #require(DAVLoopback(root: client, prefix: "/dav/e2e"))
    defer { server.stop() }
    let mounted = ShareHub.run("/sbin/mount_webdav", ["-S", "-v", "proj", "http://127.0.0.1:\(server.port)/dav/e2e/", mp.path]) == 0
    defer { forceUnmount(mp) }
    try #require(mounted, "mount_webdav to the loopback server")
    var st = stat()
    print("[mount-real] lstat .git: \(lstat(mp.path + "/.git", &st)) isDir=\(st.st_mode & S_IFMT == S_IFDIR) mp=\(mp.path) real=\(Sync.real(mp)?.path ?? "nil")")
    var cfg = ClaudeCLIConfig(); cfg.allowLocalTools = true
    var args = cfg.agentArgs(cwd: mp, allTools: ["Bash"], folders: AllowedFolders(), checkedPaths: [], directGit: true)
    args += ["-p", "--output-format", "json", "--model", "haiku", "--strict-mcp-config", "--setting-sources", ""]
    let settings = args[args.firstIndex(of: "--settings")! + 1]
    let dw = ((((try? JSONSerialization.jsonObject(with: Data(settings.utf8))) as? [String: Any])?["sandbox"] as? [String: Any])?["filesystem"] as? [String: Any])?["denyWrite"] as? [String] ?? []
    print("[mount-real] denyWrite git entries: \(dw.filter { $0.hasSuffix("/.git") || $0.hasSuffix("/.git/index.lock") })")
    var env = ProcessInfo.processInfo.environment
    for k in env.keys where k.hasPrefix("ANTHROPIC_") || k.hasPrefix("CLAUDE_CODE_") || k == "CLAUDECODE" || k.hasPrefix("GIT_") { env[k] = nil }
    env["PATH"] = (env["PATH"] ?? "") + ":/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin"
    env.merge(ClaudeCLIConfig.sandboxGitEnv(mount: true)) { $1 }
    let watch = try #require(GitWatch.begin(client.path, stateDir: base.path + "-state"))  // the client's end-of-run git watch
    let errFile = base.appendingPathComponent("stderr.txt")
    let saved = dup(2), fd = open(errFile.path, O_WRONLY | O_CREAT | O_TRUNC, 0o600)
    dup2(fd, 2)
    WebDAV.refused.withLock { $0 = WebDAV.RefusalLog() }
    let ask = "Run these as separate Bash calls, one simple command each (no chaining, no echo): 1) printf hi > b.txt  2) git add b.txt  3) git commit -m 'add b'  4) printf x > .git/hooks/pre-commit  5) printf x > .git/COMMIT_EDITMSG  6) mv .git/COMMIT_EDITMSG .git/config  7) printf '# edit' >> exec.sh  8) git add exec.sh  9) git commit -m 'edit exec'. Report the exact output of each."
    let r = ClaudeSeatbelt.capture(exe, args, env: env, cwd: mp.path, input: ask, timeout: 300, log: base)
    for _ in 0..<50 { usleep(200_000) }  // webdavfs writes back after close
    dup2(saved, 2); close(saved); close(fd)
    let refusals = (try? String(contentsOf: errFile, encoding: .utf8)) ?? ""
    print("[mount-real] claude said: \((r.out.components(separatedBy: "\"result\":").last ?? "").prefix(1200))")
    print("[mount-real] client stderr: \(refusals)")
    print("[mount-real] requests: \(server.summary)\n[mount-real] writes: \(server.writes)")
    let log = sh("git log --format=%s", cwd: client.path).out.components(separatedBy: "\n").dropFirst().first ?? ""  // the second newest: "add b"
    let report = watch.finish()
    print("[mount-real] git watch report: \(report)")
    #expect(log.hasPrefix("add b"), "no commit in the client's repository: \(log)")
    #expect(report.joined().contains("2 new commit(s)"), "\(report)")
    // 0.7.1: the mount shows every file as -rwx------: the new file is recorded as 100644, the executable file keeps 100755 after the agent's edit
    let tree = sh("git ls-tree HEAD", cwd: client.path).out
    #expect(tree.range(of: #"100644 blob \w+\tb\.txt"#, options: .regularExpression) != nil, Comment(rawValue: tree))
    #expect(tree.range(of: #"100755 blob \w+\texec\.sh"#, options: .regularExpression) != nil, Comment(rawValue: tree))
    #expect(sh("git log --format=%s", cwd: client.path).out.hasPrefix("edit exec"))
    #expect(!refusals.contains("._"), "AppleDouble files are not reported: \(refusals)")
    #expect(!fm.fileExists(atPath: client.appendingPathComponent(".git/hooks/pre-commit").path))
    #expect(!sh("cat .git/config", cwd: client.path).out.contains("COMMIT_EDITMSG") && !sh("cat .git/config", cwd: client.path).out.hasPrefix("x"))
}

/// 0.7.1: the mount shows every file as -rwx------. With the environment of a sandboxed run on a mount (core.fileMode false) a new file is committed as 100644 and an executable
/// file keeps 100755; what a tracked symlink looks like on the mount is measured here.
@Test func modesOnADirectModeMountAreNotTheMountsModes() throws {
    let fm = FileManager.default
    let base = try #require(Sync.real(fm.temporaryDirectory)).appendingPathComponent("direct-mount-mode-" + UUID().uuidString, isDirectory: true)
    let client = base.appendingPathComponent("proj"), mp = base.appendingPathComponent("mnt")
    for d in [client, mp] { try fm.createDirectory(at: d, withIntermediateDirectories: true) }
    defer { try? fm.removeItem(at: base) }
    #expect(sh("git init -q && echo '#!/bin/sh' > exec.sh && chmod 755 exec.sh && echo p > plain.txt && ln -s plain.txt link && git add -A && git commit -qm init", cwd: client.path).status == 0)
    let server = try #require(DAVLoopback(root: client, prefix: "/dav/e2e"))
    defer { server.stop() }
    let mounted = ShareHub.run("/sbin/mount_webdav", ["-S", "-v", "proj", "http://127.0.0.1:\(server.port)/dav/e2e/", mp.path]) == 0
    defer { forceUnmount(mp) }
    try #require(mounted, "mount_webdav to the loopback server")
    let env = ClaudeCLIConfig.sandboxGitEnv(mount: true).map { "export \($0.key)='\($0.value)'" }.joined(separator: "; ")
    print("[mode] ls -l on the mount: \(sh("ls -l exec.sh plain.txt link", cwd: mp.path).out)")
    print("[mode] status on the mount, default config: \(sh("git status --short; git config core.symlinks; git ls-files -s", cwd: mp.path).out)")
    print("[mode] status with the run's environment: \(sh(env + "; git status --short; git diff --raw", cwd: mp.path).out)")
    let r = sh(env + "; echo new > new.txt && echo '# edit' >> exec.sh && git add new.txt exec.sh && git commit -qm agent", cwd: mp.path)
    #expect(r.status == 0, "\(r)")
    let tree = sh("git ls-tree HEAD", cwd: client.path).out
    print("[mode] tree after the agent's commit: \(tree)")
    #expect(tree.range(of: #"100644 blob \w+\tnew\.txt"#, options: .regularExpression) != nil, Comment(rawValue: tree))
    #expect(tree.range(of: #"100755 blob \w+\texec\.sh"#, options: .regularExpression) != nil, Comment(rawValue: tree))
    print("[mode] client diff --raw after: \(sh("git status --short; git diff --raw HEAD", cwd: client.path).out)")
}

/// 0.7.1: the gateway reads a missing flag as closed: the client decides (it sees the real folder) and says so at share time.
@Test func commitsThroughAMountNeedTheClientsFlag() async {
    let hub = ShareHub()
    _ = await hub.register(session: "s1", cwd: "/c/p", key: nil, device: "d")
    let direct1 = await hub.isDirect(session: "s1"), open1 = await hub.commitsAllowed(session: "s1")
    #expect(direct1 && !open1)
    let k = await hub.register(session: "s2", cwd: "/c/q", key: nil, device: "d", gitWrites: true)
    let open2 = await hub.commitsAllowed(session: "s2")
    #expect(open2)
    _ = await hub.register(session: "s2", cwd: "/c/q", key: k, device: "d")  // the next registration without the flag closes it again
    let open3 = await hub.commitsAllowed(session: "s2")
    #expect(!open3)
}

/// 0.7.1: a repository that tracks a symbolic link gets no commit opening (the client sends no flag): the real claude on the real mount stages everything and cannot commit; the link stays a link.
@Test(.enabled(if: ProcessInfo.processInfo.environment["MG_REAL_CLAUDE_GIT"] == "1"))
func aRealClaudeAgentCannotCommitThroughAMountOfARepositoryWithASymlink() async throws {
    guard let exe = await ClaudeCLI.executable("") else { try Test.cancel("claude is not installed") }
    let fm = FileManager.default
    let base = try #require(Sync.real(fm.temporaryDirectory)).appendingPathComponent("direct-mount-link-" + UUID().uuidString, isDirectory: true)
    let client = base.appendingPathComponent("proj"), mp = base.appendingPathComponent("mnt")
    for d in [client, mp] { try fm.createDirectory(at: d, withIntermediateDirectories: true) }
    defer { try? fm.removeItem(at: base) }
    #expect(sh("git init -q && git config user.name Owner && git config user.email o@example.com && echo hi > a.txt && ln -s a.txt link && git add -A && git commit -qm init", cwd: client.path).status == 0)
    let hub = ShareHub()
    _ = await hub.register(session: "s", cwd: client.path, key: nil, device: "d")  // no flag: the client found a tracked symbolic link
    let open = await hub.commitsAllowed(session: "s")
    #expect(!open)
    let server = try #require(DAVLoopback(root: client, prefix: "/dav/e2e"))
    defer { server.stop() }
    let mounted = ShareHub.run("/sbin/mount_webdav", ["-S", "-v", "proj", "http://127.0.0.1:\(server.port)/dav/e2e/", mp.path]) == 0
    defer { forceUnmount(mp) }
    try #require(mounted, "mount_webdav to the loopback server")
    var cfg = ClaudeCLIConfig(); cfg.allowLocalTools = true
    let args = cfg.agentArgs(cwd: mp, allTools: ["Bash"], folders: AllowedFolders(), checkedPaths: [], directGit: open)
        + ["-p", "--output-format", "json", "--model", "haiku", "--strict-mcp-config", "--setting-sources", ""]
    var env = ProcessInfo.processInfo.environment
    for k in env.keys where k.hasPrefix("ANTHROPIC_") || k.hasPrefix("CLAUDE_CODE_") || k == "CLAUDECODE" || k.hasPrefix("GIT_") { env[k] = nil }
    env["PATH"] = (env["PATH"] ?? "") + ":/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin"
    env.merge(ClaudeCLIConfig.sandboxGitEnv(mount: open)) { $1 }
    let ask = "Run these as separate Bash calls, one simple command each (no chaining, no echo): 1) printf x > c.txt  2) git add -A  3) git commit -m sneaky. Report the exact output of each."
    let r = ClaudeSeatbelt.capture(exe, args, env: env, cwd: mp.path, input: ask, timeout: 300, log: base)
    for _ in 0..<25 { usleep(200_000) }
    print("[mount-link] claude said: \((r.out.components(separatedBy: "\"result\":").last ?? "").prefix(700))")
    #expect(sh("git log --format=%s", cwd: client.path).out == "init\n", "a commit was made")
    #expect(sh("git ls-tree HEAD link", cwd: client.path).out.hasPrefix("120000"), "the link is no longer a link")
}
