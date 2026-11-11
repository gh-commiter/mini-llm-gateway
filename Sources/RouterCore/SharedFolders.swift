import Foundation

/// Seznam »minigw share add« (datoteka minigw/shared-folders, ista za minigw v aplikaciji in Go na macOS):
/// mapa na vrstico, za tabulatorjem neobvezno privzet način te mape in njenih podmap, npr. `/Volumes/Java/p\t--work=native`.
/// Starejše datoteke (le poti) veljajo naprej.
public enum SharedFolders {
    public struct Entry: Equatable, Sendable {
        public var path: String
        public var work: String?     // direct, sync, native, worktree ali auto; nil = privzeto
        public var readOnly = false
        public init(path: String, work: String? = nil, readOnly: Bool = false) { self.path = path; self.work = work; self.readOnly = readOnly }
    }

    /// native-direct: kot native, a agenti delajo v mapi sami (tudi v repozitoriju git, en pisoči agent naenkrat).
    public static let modes = ["direct", "sync", "native", "native-direct", "worktree", "auto"]

    public static func parse(_ text: String) -> [Entry] {
        text.split(whereSeparator: \.isNewline).compactMap { line in
            let parts = line.split(separator: "\t", maxSplits: 1).map(String.init)
            let path = parts[0].trimmingCharacters(in: .whitespaces)
            guard !path.isEmpty else { return nil }
            var e = Entry(path: path)
            for o in (parts.count > 1 ? parts[1] : "").split(separator: " ") {
                if o.hasPrefix("--work="), modes.contains(String(o.dropFirst(7))) { e.work = String(o.dropFirst(7)) }
                if o == "--ro" { e.readOnly = true }
            }
            return e
        }
    }

    public static func format(_ entries: [Entry]) -> String {
        entries.map { e in
            let o = (e.work.map { ["--work=" + $0] } ?? []) + (e.readOnly ? ["--ro"] : [])
            return e.path + (o.isEmpty ? "" : "\t" + o.joined(separator: " "))
        }.joined(separator: "\n") + "\n"
    }

    /// Projektna mapa kjer koli (tudi /Volumes/Disk/projekt), razen korena diska ali priklopa, domače mape same,
    /// sistemskih map, domačih map drugih ter skritih map in ~/Library (kot autoShare v minigw Go). Podmapa začasne mape
    /// (/tmp/projekt, mktemp -d) je projektna mapa (privzeti auto jo kopira: worktree ali sync), začasna mapa sama ne,
    /// tudi ne skrita (/tmp/.X11-unix), tuja (prva raven pod začasno mapo drugega uporabnika) ali zgornji ravni začasne
    /// mape Claude Code (claude-<uid>, claude-<uid>/<projekt>, …/<seja>; globlje da) (L1, L2).
    public static func autoShare(_ path: String, home: String = NSHomeDirectory(), temp: String = NSTemporaryDirectory()) -> Bool {
        let p = URL(fileURLWithPath: path).standardizedFileURL.path
        guard p != "/", p != home else { return false }
        let temps = [URL(fileURLWithPath: temp).standardizedFileURL.path, "/tmp", "/var/tmp"].flatMap { [$0, "/private" + $0] }
        for t in temps where t != "/" && t != "/private/" && !(home + "/").hasPrefix(t + "/") && p.hasPrefix(t + "/") {
            let parts = p.dropFirst(t.count + 1).split(separator: "/").map(String.init)
            guard let first = parts.first, !first.hasPrefix(".") else { return false }
            let a = try? FileManager.default.attributesOfItem(atPath: t + "/" + first)  // lstat: ne povezava
            guard a?[.type] as? FileAttributeType == .typeDirectory, (a?[.ownerAccountID] as? NSNumber)?.uint32Value == getuid() else { return false }
            return first != "claude-\(getuid())" || parts.count > 3
        }
        if p.hasPrefix(home + "/") {
            let top = String(p.dropFirst(home.count + 1).split(separator: "/").first ?? "")
            return !top.hasPrefix(".") && top != "Library"
        }
        let parts = p.split(separator: "/").map { $0.lowercased() }
        let system: Set<String> = ["system", "library", "usr", "bin", "sbin", "etc", "private", "var", "dev", "opt", "applications",
                                   "users", "home", "root", "proc", "sys", "boot", "tmp", "run", "snap", "nix", "cores"]
        guard let top = parts.first, !top.hasPrefix("."), !system.contains(top) else { return false }
        return !["volumes", "mnt", "media"].contains(top) || parts.count >= 3
    }

    /// direct ni izbran izrecno (privzeto, auto ali samo za branje namesto worktree/native): prehod brez priklopa → sync.
    public static func implicitWork(asked: String?, work: String) -> Bool {
        asked == nil || asked == "auto" || asked != work
    }

    /// Najbolj natančna mapa s seznama, v kateri je cwd.
    public static func lookup(_ entries: [Entry], cwd: String) -> Entry? {
        entries.filter { cwd == $0.path || cwd.hasPrefix($0.path.hasSuffix("/") ? $0.path : $0.path + "/") }
            .max { $0.path.count < $1.path.count }
    }

    /// Način za en zagon: --minigw-work= / MINIGW_WORK > shranjen za mapo > auto. Shranjen --ro velja vedno; native
    /// samo za branje ni mogoč → direct samo za branje (tudi lažen router, ki native zavrne, ne odpre pisanja).
    /// Worktree samo za branje prav tako ne (agent vrne vejo) → direct samo za branje; auto se razreši v `candidates`.
    public static func mode(flag: String?, saved: Entry?, readOnly: Bool) -> (work: String, readOnly: Bool) {
        let ro = readOnly || saved?.readOnly == true, work = flag ?? saved?.work ?? "auto"
        return (["native", "native-direct", "worktree"].contains(work) && ro ? "direct" : work, ro)
    }

    /// #60: imena omejenih agentov iz odgovora prehoda (le znani, brez ponovitev) in ali so omejeni vsi (kot cappedNames v Go).
    static func cappedNames(_ capped: [String]) -> (names: [String], all: Bool) {
        var seen: Set<String> = [], names: [String] = []
        for k in capped { if let n = Router.agentNames[k], seen.insert(k).inserted { names.append(n) } }
        return (names, seen.count == Router.agentNames.count)
    }

    /// #60: način v začetni vrstici; capped = agenti, ki zaradi omejitve prehoda (work) le berejo (kot cappedMode v Go).
    public static func cappedMode(_ capped: [String]) -> String {
        let (names, all) = cappedNames(capped)
        if all { return "read-only (capped by the gateway)" }
        return names.isEmpty ? "read-write" : "read-write (read-only for \(names.joined(separator: ", ")): capped by the gateway)"
    }

    /// #60: dodatek vrstici native (v mapi sami); "" brez omejitve (kot cappedNative v Go).
    public static func cappedNative(_ capped: [String]) -> String {
        let (names, all) = cappedNames(capped)
        if all { return ", read-only (capped by the gateway)" }
        return names.isEmpty ? "" : ", read-only for \(names.joined(separator: ", ")) (capped by the gateway)"
    }

    /// Načini, ki jih minigw poskusi po vrsti (naslednji, če router prejšnjega ne sprejme).
    /// auto: samo za branje → direct; mapa na shemi SMB routerja → native; repozitorij git → worktree; sicer sync.
    /// Repozitorij git nikoli v direct za pisanje (agenti tam ne morejo commitati, .git samo za branje): worktree pade na sync. Izrecen native pade na direct.
    public static func candidates(work: String, readOnly: Bool, smb: Bool, git: Bool) -> [String] {
        switch work {
        case "auto" where readOnly: return ["direct"]
        case "auto": return (smb ? ["native"] : []) + (git ? ["worktree", "sync"] : ["sync", "direct"])
        case "native", "native-direct": return [work, "direct"]
        case "worktree": return git ? ["worktree", "sync"] : ["direct"]
        default: return [work]
        }
    }

    /// M1: worktree pri auto le, če je koren repozitorija ta mapa; nadrejeni (dotfiles v domači mapi, monorepo) bi poslal
    /// tudi datoteke zunaj nje.
    public static func repoRoot(_ top: String?, _ cwd: String) -> Bool {
        let real = { URL(fileURLWithPath: $0).resolvingSymlinksInPath().standardizedFileURL.path }
        return top.map { real($0) == real(cwd) } == true
    }

    /// #84: istovetnost mape (st_dev, st_ino; sledi povezavam), nil = ni je mogoče ugotoviti (errno).
    public static func fileID(_ p: String) -> [UInt64]? {
        var st = stat()
        return stat(p, &st) == 0 ? [UInt64(UInt32(bitPattern: st.st_dev)), st.st_ino] : nil
    }

    /// #84: zakaj mape ni dovoljeno deliti, nil = dovoljeno (kot shareRefusal v Go). Nikoli koren diska ali koren nosilca
    /// z domom ali sistemom (statfs f_mntonname in f_fsid: /System/Volumes/Data da, /Volumes/<Disk> ne) ali mapa, ki je domača mapa ali jo vsebuje (/Users). Primerja
    /// istovetnost (st_dev+st_ino), ne nizov: /System/Volumes/Data/Users/<ime> ali povezava je ista mapa. Ko istovetnosti
    /// ni mogoče ugotoviti, zavrne. Velja ob share add in ob vsaki uporabi mape s seznama;
    /// shareCheck vrne tudi preverjeno istovetnost (ShareClient.start jo primerja s korenom).
    public static func shareCheck(_ dir: String, home: String = NSHomeDirectory()) -> (id: [UInt64]?, why: String?) {
        let all = " would expose everything"
        let real = { (p: String) in Sync.real(URL(fileURLWithPath: p))?.path ?? p }
        let id = fileID
        let d = real(dir)
        guard let did = id(d) else { return (nil, "cannot tell which folder this is: \(String(cString: strerror(errno)))") }
        if d == "/" { return (nil, "a disk root" + all) }
        var fs = statfs()
        guard statfs(d, &fs) == 0 else { return (nil, "cannot tell whether this is a volume root: \(String(cString: strerror(errno)))") }
        let mnt = withUnsafeBytes(of: fs.f_mntonname) { String(decoding: $0.prefix { $0 != 0 }, as: UTF8.self) }
        if id(mnt) == did {  // koren nosilca: zavrnjen le nosilec z domom ali sistemom (/System/Volumes/Data; /Volumes/<Disk> sme)
            for p in [home, "/"] {
                var o = statfs()
                guard statfs(p, &o) == 0 else { return (nil, "cannot tell whether this is a volume root: \(String(cString: strerror(errno)))") }
                if o.f_fsid.val.0 == fs.f_fsid.val.0 && o.f_fsid.val.1 == fs.f_fsid.val.1 {
                    return (nil, "the root of the volume with your home folder or the system" + all)
                }
            }
        }
        for h in [home, real(home)] {
            var p = h
            while true {
                guard let hid = id(p) else { return (nil, "cannot check your home folder: \(String(cString: strerror(errno)))") }
                if hid == did || p == d { return (nil, "your home folder or a folder that contains it" + all) }
                if p == "/" { break }
                p = (p as NSString).deletingLastPathComponent
            }
        }
        return (did, nil)
    }

    public static func shareRefusal(_ dir: String, home: String = NSHomeDirectory()) -> String? { shareCheck(dir, home: home).why }

    /// #84: lookup ob uporabi; mapa, ki je shareRefusal zdaj zavrne (dodana pred 0.5.25), ni deljena (refused = vrstica za stderr).
    public static func lookupAllowed(_ entries: [Entry], cwd: String, home: String = NSHomeDirectory()) -> (entry: Entry?, refused: String?) {
        guard let e = lookup(entries, cwd: cwd) else { return (nil, nil) }
        guard let why = shareRefusal(e.path, home: home) else { return (e, nil) }
        return (nil, "minigw: not sharing \(e.path) (\(why)); remove it with: minigw share remove \"\(e.path)\"")
    }

    /// Pot za seznam: ena vrstica, tabulator loči možnosti.
    public static func validPath(_ p: String) -> Bool { !p.contains { $0 == "\t" || $0 == "\n" || $0 == "\r" } }
}
