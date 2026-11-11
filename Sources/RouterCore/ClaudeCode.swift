import Foundation

/// #79 (kot Go claudecode.go): minigw teče v Claude Code, ko je CLAUDECODE nastavljen ali je eden od prednikov pravi
/// claude (naravni program ali node s cli.js Claude Code). Samo CLAUDECODE bi agent obšel z »env -u CLAUDECODE minigw -p
/// --continue …«. Človek v terminalu in skripte uporabnika zunaj Claude Code ostanejo brez omejitev (D2). Varovalo, ne
/// meja: proces, ki se odcepi od drevesa (dvojni fork), ali drug harness ni prepoznan; MINIGW_ALLOW_UNSAFE_FLAGS=1 omejitve izklopi.
extension Profiles {
    /// Starš, program in argumenti procesa; nil = ni berljivo (veriga se ne da preveriti).
    public typealias ProcInfo = (ppid: pid_t, exe: String, args: [String])

    /// Največ toliko prednikov (zanka ali zelo globoko drevo = preverjanje ni uspelo).
    static let maxAncestors = 64

    /// V verigi prednikov od ppid je claude. Ko veriga ni berljiva, šteje kot Claude Code, razen s terminalom na stdin.
    public static func claudeCaller(ppid: pid_t, info: (pid_t) -> ProcInfo?, terminal: Bool) -> Bool {
        var pid = ppid
        for _ in 0..<maxAncestors {
            guard let p = info(pid) else { return !terminal }
            if isClaudeProc(exe: p.exe, args: p.args) { return true }
            if p.ppid <= 0 || p.ppid == pid { return false }
            pid = p.ppid
        }
        return !terminal
    }

    /// Program ali argv[0] je claude (claude.exe), naravni namestitveni program (…/claude/versions/<različica>) ali
    /// node/bun z …/claude-code/cli.js.
    public static func isClaudeProc(exe: String, args: [String]) -> Bool {
        func norm(_ s: String) -> String { s.replacingOccurrences(of: "\\", with: "/").lowercased() }
        func name(_ s: String) -> String {
            let b = norm(s).split(separator: "/", omittingEmptySubsequences: false).last.map(String.init) ?? ""
            return b.hasSuffix(".exe") ? String(b.dropLast(4)) : b
        }
        if name(exe) == "claude" || args.first.map(name) == "claude" || norm(exe).contains("/claude/versions/") { return true }
        return ["node", "bun"].contains(name(exe)) && args.dropFirst().contains { norm($0).hasSuffix("/claude-code/cli.js") }
    }

    /// macOS: starš in ime iz sysctl KERN_PROC_PID (tudi procesi drugih uporabnikov), pot in argumenti iz KERN_PROCARGS2
    /// (le procesi tega uporabnika; sicer ime).
    public static func procInfo(_ pid: pid_t) -> ProcInfo? {
        var kp = kinfo_proc(), size = MemoryLayout<kinfo_proc>.stride
        var mib: [Int32] = [CTL_KERN, KERN_PROC, KERN_PROC_PID, pid]
        guard sysctl(&mib, 4, &kp, &size, nil, 0) == 0, size == MemoryLayout<kinfo_proc>.stride else { return nil }
        var exe = withUnsafeBytes(of: kp.kp_proc.p_comm) { String(decoding: $0.prefix { $0 != 0 }, as: UTF8.self) }
        var args: [String] = []
        var amib: [Int32] = [CTL_KERN, KERN_PROCARGS2, pid], asize = 0
        if sysctl(&amib, 3, nil, &asize, nil, 0) == 0, asize > 4 {
            var buf = [UInt8](repeating: 0, count: asize)
            if sysctl(&amib, 3, &buf, &asize, nil, 0) == 0, asize > 4 {
                let argc = buf.withUnsafeBytes { Int($0.loadUnaligned(as: Int32.self)) }
                // pot programa, ničle za poravnavo, argv, okolje
                let parts = buf[4..<asize].split(separator: 0, omittingEmptySubsequences: true).map { String(decoding: $0, as: UTF8.self) }
                if let first = parts.first { exe = first; args = Array(parts.dropFirst().prefix(argc)) }
            }
        }
        return (kp.kp_eproc.e_ppid, exe, args)
    }

    /// Claude med predniki tega procesa (procInfo; stdin terminal = isatty).
    public static func claudeAncestor() -> Bool { claudeCaller(ppid: getppid(), info: procInfo, terminal: isatty(0) == 1) }

    /// ID seje claude (UUID); drugo v datoteki se ne preda claude.
    static func validSession(_ s: String) -> Bool { s.wholeMatch(of: #/[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}/#) != nil }

    /// #79: -c/--continue → --resume <zadnja seja minigw v tej mapi>. claude -c bi nadaljeval najnovejši pogovor v mapi iz
    /// ~/.claude, tudi tistega brez minigw, in njegov prepis poslal prehodu. Brez zapisa (last "") se možnost izpusti:
    /// nov pogovor. Po »--« so le argumenti (kot Go continueSession).
    public static func continueSession(_ args: [String], last: String) -> (args: [String], dropped: Bool) {
        var out: [String] = [], dropped = false
        for (i, a) in args.enumerated() {
            if a == "--" { return (out + args[i...], dropped) }
            if a == "-c" || a == "--continue" {
                if last.isEmpty { dropped = true } else { out += ["--resume", last] }
            } else {
                out.append(a)
            }
        }
        return (out, dropped)
    }

    /// Zadnja seja minigw v mapi cwd ("" = ni); file: JSON {mapa: ID}, ista datoteka kot Go na macOS.
    public static func lastSession(file: URL, cwd: String) -> String {
        let m = (try? JSONSerialization.jsonObject(with: Data(contentsOf: file))) as? [String: Any]
        guard let s = m?[cwd] as? String, validSession(s) else { return "" }
        return s
    }

    /// sid je zadnja seja minigw v mapi cwd.
    /// ponytail: dva hkratna zagona v različnih mapah lahko izgubita en zapis (zadnji zapiše); tedaj -c začne nov pogovor.
    public static func saveLastSession(file: URL, cwd: String, sid: String) {
        guard validSession(sid) else { return }
        var m = ((try? JSONSerialization.jsonObject(with: Data(contentsOf: file))) as? [String: Any]) ?? [:]
        guard m[cwd] as? String != sid else { return }
        m[cwd] = sid
        try? FileManager.default.createDirectory(at: file.deletingLastPathComponent(), withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
        guard let d = try? JSONSerialization.data(withJSONObject: m) else { return }
        try? d.write(to: file, options: .atomic)
        try? FileManager.default.setAttributes([.posixPermissions: 0o600], ofItemAtPath: file.path)
    }
}
