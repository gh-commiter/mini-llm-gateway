import Foundation
import Testing

/// Tests never listen or bind outside 127.0.0.1 themselves: the macOS firewall asks for permission for every unsigned program that accepts connections
/// (the test process is one), so those sockets live in ONE signed utility (`mg-test-net`, Sources/MGTestNet, installed by scripts/test-net-install.sh as
/// "Mini Gateway Test Net.app") and the permission binds to it. A test process must never fall back to listening itself.
enum TestNet {
    static let installed = "/Volumes/SAMSUNG990PRO/Mini Gateway Scratch/test-tools/Mini Gateway Test Net.app/Contents/MacOS/mg-test-net"
    static let hint = "mg-test-net is not installed: run scripts/test-net-install.sh once (signs and installs the utility), or set MG_TEST_NET to its path"

    struct Missing: Error, CustomStringConvertible { var description: String { TestNet.hint } }

    /// MG_TEST_NET, else the install location; on CI (GitHub's runner has no such firewall) the package's own unsigned build next to the test bundle.
    static func locate() throws -> String {
        let fm = FileManager.default, env = ProcessInfo.processInfo.environment
        if let p = env["MG_TEST_NET"], fm.isExecutableFile(atPath: p) { return p }
        if fm.isExecutableFile(atPath: installed) { return installed }
        if let ci = env["CI"], !ci.isEmpty {
            for b in Bundle.allBundles where b.bundlePath.hasSuffix(".xctest") {
                let built = b.bundleURL.deletingLastPathComponent().appendingPathComponent("mg-test-net").path
                if fm.isExecutableFile(atPath: built) { return built }
            }
        }
        Issue.record(Comment(rawValue: hint))
        throw Missing()
    }

    /// One run of the utility; its JSON lines are collected. stop() (or deinit) closes its stdin, which ends it.
    final class Run: @unchecked Sendable {
        private let process = Process(), stdin = Pipe(), out = Pipe(), lock = NSLock()
        private var lines: [[String: Any]] = []
        init(_ args: [String]) throws {
            process.executableURL = URL(fileURLWithPath: try TestNet.locate())
            process.arguments = args
            process.standardInput = stdin; process.standardOutput = out; process.standardError = FileHandle.nullDevice
            try process.run()
            let h = out.fileHandleForReading
            Thread { [self] in
                var buf = Data()
                while true {
                    let d = h.availableData
                    if d.isEmpty { return }
                    buf += d
                    while let nl = buf.firstIndex(of: 10) {
                        let line = buf[..<nl]; buf = Data(buf[buf.index(after: nl)...])
                        if let o = (try? JSONSerialization.jsonObject(with: line)) as? [String: Any] { lock.withLock { lines.append(o) } }
                    }
                }
            }.start()
        }
        deinit { stop() }

        var events: [[String: Any]] { lock.withLock { lines } }
        /// The first event with this name, waited for with a deadline.
        func event(_ name: String, timeout: Double = 30) async throws -> [String: Any] {
            let end = Date().addingTimeInterval(timeout)
            while Date() < end {
                if let e = events.first(where: { $0["event"] as? String == name }) { return e }
                if let e = events.first(where: { $0["event"] as? String == "error" }) { Issue.record("mg-test-net: \(e)"); throw Missing() }
                try await Task.sleep(for: .milliseconds(20))
            }
            Issue.record("mg-test-net: no \(name) event within \(timeout) s: \(events)")
            throw Missing()
        }
        func stop() { try? stdin.fileHandleForWriting.close(); if process.isRunning { process.waitUntilExit() } }
    }

    /// Listens on `address` (an ephemeral port by default); returns the run and the port it printed.
    static func listen(_ address: String, port: Int = 0, mode: String = "hold", accept: Int = 0, deadline: Int = 120) async throws -> (run: Run, port: Int) {
        let run = try Run(["listen", address, String(port), "--mode", mode, "--accept", String(accept), "--deadline", String(deadline)])
        let e = try await run.event("listening")
        return (run, e["port"] as? Int ?? 0)
    }

    /// One bind attempt (no listen): the utility reports whether it succeeded and the errno.
    static func tryBind(_ address: String, port: Int, reuseAddr: Bool = false, reusePort: Bool = false) async throws -> (ok: Bool, errno: Int32) {
        let run = try Run(["bind", address, String(port)] + (reuseAddr ? ["--reuseaddr"] : []) + (reusePort ? ["--reuseport"] : []))
        defer { run.stop() }
        let e = try await run.event("bind", timeout: 15)
        return (e["ok"] as? Bool ?? false, Int32(e["errno"] as? Int ?? -1))
    }
}
