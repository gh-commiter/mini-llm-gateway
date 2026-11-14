import CryptoKit
import Foundation
import os

/// 0.6.0: the kernel layer under every Claude CLI run without "Allow local tools" (`Native.claudeRestrictedProfile`): the claude process runs in
/// `sandbox-exec`, so the kernel refuses what the Claude Code rules (the first layer, unchanged) would let through if they failed open (0.5.27).
/// This type decides whether the layer can be used: one self-check per (claude binary and version, OS, profile text), kept on disk.
///   1. kernel rows (no model): a plain `cat`/`touch` under the very same profile reads the canary in a temp session folder and cannot read or write outside it;
///   2. one real `claude -p --model haiku` under the profile reads the canary (claude starts, logs in, answers, writes its transcript);
///      if that fails, the same run without the profile tells a broken profile (fall back, say so) from a broken network or login (inconclusive: not cached).
/// A failure never stays silent: one WARNING in the log, `status.claudeSandbox` (minigw gw status) and the Claude CLI pane of Settings show the reason, and the
/// restricted mode runs on the Claude Code rules alone until the next check passes (a new claude version, OS or profile; the app start checks again).
public actor ClaudeSeatbelt {
    public enum State: Equatable, Sendable { case unchecked, on, off(String) }
    enum Outcome: Equatable, Sendable { case pass, fail(String), inconclusive }

    private static let log = Logger(subsystem: "dev.trojar.minigateway", category: "claude-seatbelt")
    /// Tools of a run that write files (the edit class of the restricted branch).
    static let editTools: Set<String> = ["Edit", "Write", "MultiEdit", "NotebookEdit"]

    public private(set) var state: State = .unchecked
    private let home: String
    private var memo: (stamp: String, key: String)?
    private var verdicts: [String: State] = [:]
    private var inflight: [String: Task<Outcome, Never>] = [:]

    public init(home: String = NSHomeDirectory()) { self.home = home }

    /// The reason the layer is off ("" while it is on or not checked yet): `status.claudeSandbox`, Settings.
    public var offReason: String { if case .off(let r) = state { return r }; return "" }

    /// The line shown to the admin when the layer is off (log, status, Settings use this text; the Settings pane translates the prefix).
    public static func offLine(_ reason: String) -> String { "Claude CLI without local tools runs without the system sandbox: \(reason)" }

    /// True: launch the restricted run under the kernel profile. False: the self-check failed, the run uses the Claude Code rules alone (state `off`).
    /// `tmpBase`: Scratch/tmp (the check's temp folder, a hidden root as a real session's can be); `cacheFile`: the passes, kept on disk (Scratch/cache, hidden from agents).
    public func ensure(exe: String, tmpBase: URL, cacheFile: URL) async -> Bool {
        guard let key = await keyFor(exe) else { return true }  // no claude: the launch reports it
        if let v = verdicts[key] { state = v; return v == .on }
        if let disk = Self.stored(cacheFile), disk == key { verdicts[key] = .on; state = .on; return true }
        let task: Task<Outcome, Never>
        if let t = inflight[key] { task = t } else {
            let home = self.home
            task = Task { await Self.check(exe: exe, tmpBase: tmpBase, home: home) }
            inflight[key] = task
        }
        let outcome = await task.value
        inflight[key] = nil
        switch outcome {
        case .pass:
            verdicts[key] = .on; state = .on
            try? Data("{\"key\":\"\(key)\"}".utf8).write(to: cacheFile, options: .atomic)
            return true
        case .fail(let why):
            if state != .off(why) { Self.log.warning("\(Self.offLine(why), privacy: .public)") }
            verdicts[key] = .off(why); state = .off(why)
            return false
        case .inconclusive:
            return true  // the profile was not shown broken (network or login): the run is confined, the next request checks again
        }
    }

    private func keyFor(_ exe: String) async -> String? {
        guard let real = Sync.real(URL(fileURLWithPath: exe))?.path else { return nil }
        var st = stat()
        guard stat(real, &st) == 0 else { return nil }
        let stamp = "\(real)|\(st.st_mtimespec.tv_sec)|\(st.st_size)"
        if let m = memo, m.stamp == stamp { return m.key }
        let version = await offPool { () -> String in
            if case .success(let v) = ClaudeCLI.validate(real) { return v }
            return "?"
        }
        let os = ProcessInfo.processInfo.operatingSystemVersionString
        let template = Native.claudeRestrictedProfile(session: "/s", folders: ["/a"], writable: true, tmp: "/t", exe: "/e", home: "/h") ?? ""
        let hash = SHA256.hash(data: Data("1|\(stamp)|\(version)|\(os)|\(template)".utf8)).map { String(format: "%02x", $0) }.joined()
        memo = (stamp, hash)
        return hash
    }

    private static func stored(_ file: URL) -> String? {
        guard let d = try? Data(contentsOf: file), let j = (try? JSONSerialization.jsonObject(with: d)) as? [String: Any] else { return nil }
        return j["key"] as? String
    }

    // MARK: the grants of a run, from the arguments the restricted branch built

    /// The allowed folders (`--add-dir`) and whether the run may write (an edit tool is among `--tools`; a read-only run has none).
    static func grants(args: [String]) -> (folders: [String], writable: Bool) {
        var folders: [String] = []
        for (i, a) in args.enumerated() where a == "--add-dir" && i + 1 < args.count { folders.append(args[i + 1]) }
        let tools = args.firstIndex(of: "--tools").map { args[$0 + 1] } ?? ""
        return (folders, tools.split(separator: ",").contains { editTools.contains($0.trimmingCharacters(in: .whitespaces)) })
    }

    /// The environment of a confined run: claude's temp folder (`CLAUDE_CODE_TMPDIR`) and TMPDIR are the run's private folder.
    static func confine(_ env: inout [String: String], tmp: String) {
        env["CLAUDE_CODE_TMPDIR"] = tmp
        env["TMPDIR"] = tmp + "/"; env["TMP"] = tmp; env["TEMP"] = tmp
    }

    // MARK: the self-check

    /// What claude needs under the profile, in a temp session folder under `tmpBase` (Scratch: a hidden root, as a real session can be).
    static func check(exe: String, tmpBase: URL, home: String) async -> Outcome {
        await offPool { checkNow(exe: exe, tmpBase: tmpBase, home: home) }
    }

    static func checkNow(exe: String, tmpBase: URL, home: String, modelRun: Bool = true) -> Outcome {
        let fm = FileManager.default
        // a fixed name: claude keeps this session's transcript under ~/.claude/projects/<slug of the path>, one folder for every check, not one per check
        let root = tmpBase.appendingPathComponent("run-seatbelt", isDirectory: true)
        let dirs = ["session", "outside", "tmp"].map { root.appendingPathComponent($0, isDirectory: true) }
        for d in dirs {
            guard (try? fm.createDirectory(at: d, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])) != nil else {
                return .fail("cannot create its temporary folder in Scratch")
            }
        }
        defer { try? fm.removeItem(at: root) }
        guard let session = Sync.real(dirs[0])?.path, let outside = Sync.real(dirs[1])?.path, let tmp = Sync.real(dirs[2])?.path,
              let real = Sync.real(URL(fileURLWithPath: exe))?.path,
              let profile = Native.claudeRestrictedProfile(session: session, folders: [], writable: true, tmp: tmp, exe: real, home: home) else {
            return .fail("cannot build the sandbox profile")
        }
        let canary = "CANARY-" + UUID().uuidString.lowercased()
        try? Data(canary.utf8).write(to: URL(fileURLWithPath: session + "/canary.txt"))
        try? Data("OUTSIDE".utf8).write(to: URL(fileURLWithPath: outside + "/secret.txt"))
        if let why = kernelRows(profile: profile, session: session, outside: outside, canary: canary) { return .fail(why) }
        guard modelRun else { return .pass }

        var env = ProcessInfo.processInfo.environment
        for k in env.keys where k.hasPrefix("ANTHROPIC_") || k.hasPrefix("CLAUDE_CODE_") || k == "CLAUDECODE" { env[k] = nil }
        env["PATH"] = (env["PATH"].map { $0 + ":" } ?? "") + "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"
        env["CLAUDE_CODE_DISABLE_FILE_MENTIONS"] = "1"
        let args = ["-p", "--disable-slash-commands", "--output-format", "json", "--model", "haiku", "--strict-mcp-config", "--tools", "Read",
                    "--allowedTools", "Read(./**)", "--permission-mode", "dontAsk", "--setting-sources", ""]
        let prompt = "Read the file canary.txt in the current folder and reply with only its exact content."
        var boxed = env
        confine(&boxed, tmp: tmp)
        let sandboxed = capture("/usr/bin/sandbox-exec", ["-p", profile, exe] + args, env: boxed, cwd: session, input: prompt, timeout: 120, log: root)
        if sandboxed.out.contains(canary) { return .pass }
        // not shown by the profile alone: the same run without it (network, login, a Claude Code outage are not the profile's fault)
        let control = capture(exe, args, env: env, cwd: session, input: prompt, timeout: 120, log: root)
        if !control.out.contains(canary) { return .inconclusive }
        let why = sandboxed.timedOut ? "claude did not answer in time under the profile"
            : firstLine(sandboxed.err) ?? firstLine(sandboxed.out) ?? "claude exited with \(sandboxed.status) under the profile"
        return .fail("claude does not run under the profile (\(why))")
    }

    /// Plain tools under the same profile, no model: the canary is read, nothing outside the session folder is read or written. nil = in order.
    static func kernelRows(profile: String, session: String, outside: String, canary: String) -> String? {
        func run(_ tool: String, _ args: [String]) -> (status: Int32, out: String)? {
            let p = Process(), out = Pipe()
            p.executableURL = URL(fileURLWithPath: "/usr/bin/sandbox-exec")
            p.arguments = ["-p", profile, tool] + args
            p.standardOutput = out; p.standardError = FileHandle.nullDevice; p.standardInput = FileHandle.nullDevice
            guard (try? p.run()) != nil else { return nil }
            let data = out.fileHandleForReading.readDataToEndOfFile()
            p.waitUntilExit()
            return (p.terminationStatus, String(decoding: data, as: UTF8.self))
        }
        guard let inside = run("/bin/cat", [session + "/canary.txt"]), inside.status == 0, inside.out == canary else {
            return "sandbox-exec does not run a program in the session folder under the profile"
        }
        if let r = run("/bin/cat", [outside + "/secret.txt"]), r.status == 0 || r.out.contains("OUTSIDE") {
            return "the system sandbox (sandbox-exec) does not stop a read outside the session folder"
        }
        if let r = run("/usr/bin/touch", [outside + "/new.txt"]), r.status == 0 || FileManager.default.fileExists(atPath: outside + "/new.txt") {
            return "the system sandbox (sandbox-exec) does not stop a write outside the session folder"
        }
        return nil
    }

    private static func firstLine(_ s: String) -> String? {
        s.split(whereSeparator: \.isNewline).map { $0.trimmingCharacters(in: .whitespaces) }.first { !$0.isEmpty }.map { String($0.prefix(160)) }
    }

    /// Runs a program to its end (or kills its tree after `timeout`); output goes to files in `log` (no pipe to fill).
    static func capture(_ exe: String, _ args: [String], env: [String: String], cwd: String, input: String, timeout: TimeInterval,
                        log: URL) -> (status: Int32, out: String, err: String, timedOut: Bool) {
        let id = UUID().uuidString.prefix(8)
        let outURL = log.appendingPathComponent("out-\(id)"), errURL = log.appendingPathComponent("err-\(id)"), inURL = log.appendingPathComponent("in-\(id)")
        for u in [outURL, errURL] { FileManager.default.createFile(atPath: u.path, contents: nil) }
        try? Data(input.utf8).write(to: inURL)
        let p = Process()
        p.executableURL = URL(fileURLWithPath: exe)
        p.arguments = args
        p.environment = env
        p.currentDirectoryURL = URL(fileURLWithPath: cwd)
        p.standardInput = try? FileHandle(forReadingFrom: inURL)
        p.standardOutput = try? FileHandle(forWritingTo: outURL)
        p.standardError = try? FileHandle(forWritingTo: errURL)
        guard (try? p.run()) != nil else { return (-1, "", "cannot start \(exe)", false) }
        let timedOut = Flag(false)
        let killer = DispatchWorkItem { timedOut.set(); _ = Native.killTree(p.processIdentifier, pipes: []) }
        DispatchQueue.global().asyncAfter(deadline: .now() + timeout, execute: killer)
        p.waitUntilExit()
        killer.cancel()
        _ = Native.killTree(p.processIdentifier, pipes: [])  // grandchildren
        let read = { (u: URL) in String(decoding: (try? Data(contentsOf: u)) ?? Data(), as: UTF8.self) }
        return (p.terminationStatus, read(outURL), read(errURL), timedOut.value)
    }
}

/// A flag set once from another thread.
private final class Flag: @unchecked Sendable {
    private let lock = NSLock()
    private var flag: Bool
    init(_ v: Bool) { flag = v }
    func set() { lock.lock(); flag = true; lock.unlock() }
    var value: Bool { lock.lock(); defer { lock.unlock() }; return flag }
}

extension Router {
    /// One per process: the self-check result the restricted Claude runs, `status` and Settings share (it survives a rebuilt Router).
    public static let claudeSeatbelt = ClaudeSeatbelt()

    /// The self-check outside a request (app start): for a Claude CLI that runs without local tools. The pass is kept on disk (Scratch/cache), so a start is quick.
    public func checkClaudeSandbox() async {
        guard config.claudeCLI.enabled, !config.claudeCLI.allowLocalTools, let exe = await ClaudeCLI.executable(config.claudeCLI.executable) else { return }
        _ = await Self.claudeSeatbelt.ensure(exe: exe, tmpBase: config.scratch("tmp"), cacheFile: config.scratch("cache").appendingPathComponent("claude-seatbelt.json"))
    }

    /// `status.claudeSandbox` ("on" / "off") and `claudeSandboxReason`; nothing while not checked, or when the Claude CLI has local tools (Claude Code's own sandbox).
    func claudeSandboxStatus() async -> [String: Any] {
        guard config.claudeCLI.enabled, !config.claudeCLI.allowLocalTools else { return [:] }
        switch await Self.claudeSeatbelt.state {
        case .unchecked: return [:]
        case .on: return ["claudeSandbox": "on"]
        case .off(let why): return ["claudeSandbox": "off", "claudeSandboxReason": why]
        }
    }
}
