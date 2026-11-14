import Foundation
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
    while reader.isExecuting { usleep(5_000) }
    return (p.terminationStatus, String(decoding: data, as: UTF8.self))
}

@Test func anAgentInADirectModeMountCommitsAndCannotPlantWhatRunsCode() throws {
    let fm = FileManager.default
    let base = try #require(Sync.real(fm.temporaryDirectory)).appendingPathComponent("direct-mount-" + UUID().uuidString, isDirectory: true)
    let client = base.appendingPathComponent("proj"), mp = base.appendingPathComponent("mnt")
    for d in [client, mp] { try fm.createDirectory(at: d, withIntermediateDirectories: true) }
    defer { ShareHub.unmount(mp); try? fm.removeItem(at: base) }
    #expect(sh("git init -q && echo hi > a.txt && git add a.txt && git commit -qm init && git branch x", cwd: client.path).status == 0)
    let server = try #require(DAVLoopback(root: client, prefix: "/dav/e2e"))
    defer { server.stop() }
    let mounted = ShareHub.run("/sbin/mount_webdav", ["-S", "-v", "proj", "http://127.0.0.1:\(server.port)/dav/e2e/", mp.path]) == 0
    try #require(mounted, "mount_webdav to the loopback server")

    // the agent: edit, add, commit, branch, tag, switch, stash, merge a local branch
    for cmd in ["echo more >> a.txt && git add a.txt && git commit -qm agent", "git branch feature", "git tag v1", "git checkout -q -b topic", "echo again >> a.txt && git stash -q",
                "git checkout -q - && git merge -q --no-edit x", "git log --oneline | wc -l | grep -q 2"] {
        let r = sh(cmd, cwd: mp.path)
        #expect(r.status == 0, "\(cmd): \(r)")
    }
    // the commit is in the client's own repository, which git itself finds sound
    let log = sh("git log --oneline", cwd: client.path)
    #expect(log.out.contains("agent") && log.out.contains("init"), "\(log)")
    #expect(sh("git branch --list feature topic && git tag --list v1 && git fsck --strict 2>&1 | grep -c -i -E 'error|fatal|missing'", cwd: client.path).out.hasSuffix("0\n"))

    // what runs code is refused with the client's refusal line (stderr of this process), and nothing appears in the client's folder
    let errFile = base.appendingPathComponent("stderr.txt")
    let saved = dup(2), fd = open(errFile.path, O_WRONLY | O_CREAT | O_TRUNC, 0o600)
    dup2(fd, 2)
    let denied = ["echo '# t' >> .git/config", "echo noop > .git/hooks/pre-commit", "echo noop > .git/info/attributes", "echo /x > .git/objects/info/alternates",
                  "mkdir .git/rebase-merge && echo noop > .git/rebase-merge/git-rebase-todo", "echo x > .git/shallow", "git config user.name evil"]
    for cmd in denied { _ = sh(cmd, cwd: mp.path) }
    dup2(saved, 2); close(saved); close(fd)
    // (webdavfs writes back after close: the shell may report success for a write the client then refuses, so the checks are the refusal line and the client's folder)
    let refusals = (try? String(contentsOf: errFile, encoding: .utf8)) ?? ""
    #expect(refusals.contains("minigw: refused agent write to \".git/hooks/pre-commit\" (protected path)"), "\(refusals)")
    #expect(refusals.contains("minigw: refused agent write to \".git/config"), "\(refusals)")
    for p in ["hooks/pre-commit", "info/attributes", "objects/info/alternates", "rebase-merge", "shallow", "config.lock"] {
        #expect(!fm.fileExists(atPath: client.appendingPathComponent(".git/" + p).path), "\(p)")
    }
    #expect(!sh("cat .git/config", cwd: client.path).out.contains("evil") && !sh("cat .git/config", cwd: client.path).out.contains("# t"))
}
