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

    /// Najbolj natančna mapa s seznama, v kateri je cwd.
    public static func lookup(_ entries: [Entry], cwd: String) -> Entry? {
        entries.filter { cwd == $0.path || cwd.hasPrefix($0.path.hasSuffix("/") ? $0.path : $0.path + "/") }
            .max { $0.path.count < $1.path.count }
    }

    /// Način za en zagon: --minigw-work= / MINIGW_WORK > shranjen za mapo > direct. Shranjen --ro velja vedno; native
    /// samo za branje ni mogoč → direct samo za branje (tudi lažen router, ki native zavrne, ne odpre pisanja).
    /// Worktree samo za branje prav tako ne (agent vrne vejo) → direct samo za branje; auto se razreši v `candidates`.
    public static func mode(flag: String?, saved: Entry?, readOnly: Bool) -> (work: String, readOnly: Bool) {
        let ro = readOnly || saved?.readOnly == true, work = flag ?? saved?.work ?? "direct"
        return (["native", "native-direct", "worktree"].contains(work) && ro ? "direct" : work, ro)
    }

    /// Načini, ki jih minigw poskusi po vrsti (naslednji, če router prejšnjega ne sprejme).
    /// auto: samo za branje → direct; mapa na shemi SMB routerja → native; repozitorij git → worktree; sicer sync.
    /// Repozitorij git nikoli v direct za pisanje (.git tam ni zaščiten): worktree pade na sync. Izrecen native pade na direct.
    public static func candidates(work: String, readOnly: Bool, smb: Bool, git: Bool) -> [String] {
        switch work {
        case "auto" where readOnly: return ["direct"]
        case "auto": return (smb ? ["native"] : []) + (git ? ["worktree", "sync"] : ["sync", "direct"])
        case "native", "native-direct": return [work, "direct"]
        case "worktree": return git ? ["worktree", "sync"] : ["direct"]
        default: return [work]
        }
    }

    /// Pot za seznam: ena vrstica, tabulator loči možnosti.
    public static func validPath(_ p: String) -> Bool { !p.contains { $0 == "\t" || $0 == "\n" || $0 == "\r" } }
}
