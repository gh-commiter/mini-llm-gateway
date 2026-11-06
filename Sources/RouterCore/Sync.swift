import CryptoKit
import Foundation

/// Sinhronizacija mape (minigw --minigw-work=sync): agent dela v kopiji na routerju namesto v priklopu.
/// Pred agentom minigw pošlje manifest in datoteke, ki jih router nima; po agentu prevzame spremembe kopije.
/// Isto kodo uporabljata router in minigw za macOS; odjemalec Go (client/sync.go) dela enako.
public enum Sync {
    public struct Entry: Codable, Sendable, Equatable {
        public var path: String    // relativna, z »/«
        public var size: Int64
        public var mtime: Int64    // ms; obe strani nastavita enak čas → ni ponovnega zgoščevanja
        public var sha256: String
        public var exec: Bool      // izvršljiva (bit x lastnika)
        public init(path: String, size: Int64, mtime: Int64, sha256: String, exec: Bool = false) {
            self.path = path; self.size = size; self.mtime = mtime; self.sha256 = sha256; self.exec = exec
        }
    }

    /// Spremembe kopije po agentu (router → odjemalec).
    public struct Changes: Codable, Sendable {
        public var changed: [Entry] = []
        public var deleted: [String] = []
        public var skipped: [String] = []  // večje od maxFile
        public init(changed: [Entry] = [], deleted: [String] = [], skipped: [String] = []) {
            self.changed = changed; self.deleted = deleted; self.skipped = skipped
        }
    }

    /// Cela datoteka gre v eni zahtevi; router sprejme telo do 64 MB.
    public static let maxFile = Int64(HTTPParser.maxBody)
    /// Nikoli se ne sinhronizira (poleg .gitignore): git, izhod gradnje, smeti macOS, spori.
    public static let defaultIgnore = [".git/", "target/", ".build/", "node_modules/", "build/", "dist/", "DerivedData/",
                                       ".DS_Store", "._*", "*.minigw-conflict"]

    /// Pravila .gitignore v korenu (podmnožica): ime, `dir/`, vzorci fnmatch (`*.ext`), `/` na začetku ali v sredini = od korena.
    /// ponytail: brez `!`, `**` in .gitignore v podmapah; dodaj, ko bo treba.
    public struct Ignore: Sendable {
        struct Rule: Sendable { let pattern: String; let dirOnly: Bool; let anchored: Bool }
        let rules: [Rule]

        public init(gitignore: String) {
            rules = (Sync.defaultIgnore + gitignore.split(whereSeparator: \.isNewline).map(String.init)).compactMap { line in
                var p = line.trimmingCharacters(in: .whitespaces)
                guard !p.isEmpty, !p.hasPrefix("#"), !p.hasPrefix("!") else { return nil }
                let dirOnly = p.hasSuffix("/")
                if dirOnly { p.removeLast() }
                let anchored = p.contains("/")
                if p.hasPrefix("/") { p.removeFirst() }
                return p.isEmpty ? nil : Rule(pattern: p, dirOnly: dirOnly, anchored: anchored)
            }
        }

        public init(root: URL) {
            self.init(gitignore: (try? String(contentsOf: root.appendingPathComponent(".gitignore"), encoding: .utf8)) ?? "")
        }

        public func ignored(_ rel: String, isDir: Bool) -> Bool {
            let name = String(rel.split(separator: "/").last ?? "")
            return rules.contains { r in
                // APFS je brez razlike v velikih črkah → tudi pravila
                (!r.dirOnly || isDir) && fnmatch(r.pattern, r.anchored ? rel : name, (r.anchored ? FNM_PATHNAME : 0) | FNM_CASEFOLD) == 0
            }
        }

        /// Pot ali katera koli njena nadrejena mapa je prezrta.
        public func covers(_ rel: String) -> Bool {
            let comps = rel.split(separator: "/").map(String.init)
            return comps.indices.contains { ignored(comps[...$0].joined(separator: "/"), isDir: $0 < comps.count - 1) }
        }
    }

    /// Ime, kot ga vidi datotečni sistem brez razlike v črkah (Windows odreže končne pike in presledke: ».GIT.« = ».git«).
    static func normName(_ c: String) -> String {
        // upper→lower preslika ſ, ı, K (Kelvin) v ASCII; »git~1« je kratko 8.3 ime ».git« na Windows.
        // ponytail: ime, ne kanonična pot; ob simlinkanem staršu preveri kanonično pot (issue v Gitei).
        var n = c.uppercased().lowercased()
        while n.hasSuffix(".") || n.hasSuffix(" ") { n.removeLast() }
        return n.range(of: #"^git~[0-9]+$"#, options: .regularExpression) != nil ? ".git" : n
    }

    /// Pot vsebuje ».git« v kateri koli obliki: nikoli v kopiji ali iz nje.
    public static func isGitPath(_ rel: String) -> Bool { rel.split(separator: "/").contains { normName(String($0)) == ".git" } }

    /// Pot, ki je prevzem nikoli ne zapiše ali izbriše: git in nastavitve, ki zaganjajo ukaze
    /// (.claude/settings*.json, .vscode/tasks.json, .envrc).
    public static func isProtected(_ rel: String) -> Bool {
        let c = rel.split(separator: "/").map { normName(String($0)) }
        let last = c.last ?? "", parent = c.count > 1 ? c[c.count - 2] : ""
        return isGitPath(rel) || last == ".envrc" || (parent == ".vscode" && last == "tasks.json")
            || (parent == ".claude" && last.hasPrefix("settings") && last.hasSuffix(".json"))
    }

    /// Posel routerja za minigw (router ni zaupanja vreden): SYNC-* le v načinu sync, SYNC-PULL nikoli pri --minigw-ro,
    /// WebDAV le v načinu direct (pisanje ne pri --minigw-ro).
    public static func jobAllowed(_ method: String, sync: Bool, readOnly: Bool, write: Bool) -> Bool {
        if method.hasPrefix("SYNC-") { return sync && (method == "SYNC-PUSH" || (method == "SYNC-PULL" && !readOnly)) }
        return !sync && !(readOnly && write)
    }

    /// Datoteke pod `root` brez prezrtih in simbolnih povezav. Zgoščuje le, kar ima drugo velikost ali čas kot v `previous`.
    public static func scan(_ root: URL, previous: [String: Entry] = [:]) -> (files: [String: Entry], skipped: [String]) {
        let ignore = Ignore(root: root)
        var files: [String: Entry] = [:], skipped: [String] = []
        func walk(_ dir: URL, _ rel: String) {
            for name in ((try? FileManager.default.contentsOfDirectory(atPath: dir.path)) ?? []).sorted() {
                let r = rel.isEmpty ? name : rel + "/" + name, url = dir.appendingPathComponent(name)
                if normName(name) == ".git" { continue }  // tudi ».GIT«
                guard let a = try? FileManager.default.attributesOfItem(atPath: url.path) else { continue }  // ne sledi povezavam
                switch a[.type] as? FileAttributeType {
                case .typeDirectory?:
                    if !ignore.ignored(r, isDir: true) { walk(url, r) }
                case .typeRegular?:
                    guard !ignore.ignored(r, isDir: false) else { continue }
                    if (a[.size] as? NSNumber)?.int64Value ?? 0 > maxFile { skipped.append(r); continue }
                    if let e = entry(url, path: r, previous: previous[r]) { files[r] = e }
                default:
                    continue
                }
            }
        }
        walk(root, "")
        return (files, skipped)
    }

    /// Ena datoteka; nil, če ne obstaja ali ni navadna datoteka.
    static func entry(_ url: URL, path: String, previous: Entry?) -> Entry? {
        guard let a = try? FileManager.default.attributesOfItem(atPath: url.path), a[.type] as? FileAttributeType == .typeRegular
        else { return nil }
        let size = (a[.size] as? NSNumber)?.int64Value ?? 0
        let mtime = Int64((((a[.modificationDate] as? Date)?.timeIntervalSince1970 ?? 0) * 1000).rounded())
        let exec = ((a[.posixPermissions] as? NSNumber)?.intValue ?? 0) & 0o100 != 0
        if var p = previous, p.size == size, p.mtime == mtime { p.path = path; p.exec = exec; return p }
        guard let f = FileHandle(forReadingAtPath: url.path) else { return nil }
        defer { try? f.close() }
        var h = SHA256()
        while let d = try? f.read(upToCount: 1 << 20), !d.isEmpty { h.update(data: d) }
        return Entry(path: path, size: size, mtime: mtime, sha256: hex(h.finalize()), exec: exec)
    }

    public static func sha256(_ d: Data) -> String { hex(SHA256.hash(data: d)) }
    static func hex(_ d: SHA256.Digest) -> String { d.map { String(format: "%02x", $0) }.joined() }

    /// Stabilno ime kopije za mapo odjemalca.
    public static func folderID(_ cwd: String) -> String { String(sha256(Data(cwd.utf8)).prefix(16)) }

    /// Pot v glavi x-sync-path.
    public static func encode(_ path: String) -> String { path.addingPercentEncoding(withAllowedCharacters: .alphanumerics) ?? "" }

    /// Relativna pot druge strani → datoteka pod `root`; nil za »..«, absolutno pot, »\«, prazne dele ali pobeg prek povezave.
    public static func resolve(_ rel: String, root: URL) -> URL? {
        let comps = rel.split(separator: "/", omittingEmptySubsequences: false).map(String.init)
        guard !comps.contains(where: { $0.isEmpty || $0 == "." || $0 == ".." || $0.contains("\\") || $0.contains("\0") })
        else { return nil }
        let root = root.resolvingSymlinksInPath().standardizedFileURL
        return WebDAV.resolve("/" + comps.map(encode).joined(separator: "/"), prefix: "", root: root)?.0
    }

    /// Atomarno zapiše datoteko (ustvari mape), nastavi čas spremembe druge strani. Način obstoječe (sicer 0644) ostane,
    /// biti x pa sledijo `exec`; nil = druga stran bita x ne pozna (Windows) → ostanejo.
    public static func write(_ data: Data, to url: URL, mtime: Int64, exec: Bool?) throws {
        let fm = FileManager.default
        let old = ((try? fm.attributesOfItem(atPath: url.path))?[.posixPermissions] as? NSNumber)?.intValue ?? 0o644
        try fm.createDirectory(at: url.deletingLastPathComponent(), withIntermediateDirectories: true)
        try data.write(to: url, options: .atomic)
        try fm.setAttributes([.modificationDate: Date(timeIntervalSince1970: Double(mtime) / 1000),
                              .posixPermissions: exec.map { withExec(old, $0) } ?? old], ofItemAtPath: url.path)
    }

    /// Biti x tam, kjer je bit r (0644 → 0755); brez exec vsi biti x izklopljeni.
    static func withExec(_ mode: Int, _ exec: Bool) -> Int { mode & ~0o111 | (exec ? (mode & 0o444) >> 2 : 0) }

    /// Router: katere datoteke manifesta odjemalca potrebuje in katere je odjemalec izbrisal; `keep` ostane nedotaknjen.
    public static func plan(client: [Entry], copy: [String: Entry], keep: Set<String> = []) -> (need: [String], gone: [String]) {
        let paths = Set(client.map(\.path))
        return (client.filter { copy[$0.path]?.sha256 != $0.sha256 && !keep.contains($0.path) }.map(\.path).sorted(),
                copy.keys.filter { !paths.contains($0) && !keep.contains($0) }.sorted())
    }

    /// Router: kaj je agent spremenil v kopiji (`new`, pod `root`) glede na stanje odjemalca (`old`).
    /// Izbris le, če na poti ni ničesar: na novo prezrta ali prevelika datoteka ni izbrisana.
    public static func diff(old: [String: Entry], new: [String: Entry], root: URL) -> Changes {
        Changes(changed: new.values.filter { old[$0.path]?.sha256 != $0.sha256 || old[$0.path]?.exec != $0.exec }
                    .sorted { $0.path < $1.path },
                deleted: old.keys.filter { p in
                    new[p] == nil && (try? FileManager.default.attributesOfItem(atPath: root.appendingPathComponent(p).path)) == nil
                }.sorted())
    }

    /// Odjemalec po agentu: zapiše in izbriše, kar je agent spremenil. Datoteke, ki se je pri odjemalcu od prenosa
    /// spremenila, ne prepiše: različico routerja zapiše kot `<ime>.minigw-conflict`. Poti, prezrte pri odjemalcu, preskoči.
    /// `state` = manifest ob prenosu. Vrne sporočila za stderr in obdelane poti (potrditev routerju).
    public static func apply(_ ch: Changes, root: URL, state: inout [String: Entry],
                             fetch: (String) -> Data?) -> (notes: [String], done: [String]) {
        let ignore = Ignore(root: root)
        var notes: [String] = [], done: [String] = []
        for e in ch.changed {
            if isProtected(e.path) { notes.append("skipped \(e.path): protected path"); done.append(e.path); continue }
            if ignore.covers(e.path) { notes.append("skipped \(e.path): ignored here"); done.append(e.path); continue }
            guard let url = resolve(e.path, root: root) else {  // potrjena, sicer bi jo router pošiljal v nedogled
                notes.append("skipped \(e.path): invalid path here"); done.append(e.path); continue
            }
            if let local = entry(url, path: e.path, previous: state[e.path]), local.sha256 == e.sha256 {
                // Le bit x, in le če ga uporabnik od prenosa ni spremenil sam.
                if local.exec != e.exec, local.exec != state[e.path]?.exec {  // stanje ostane: bit ostane uporabnikov
                    notes.append("kept your file mode of \(e.path) (the agent changed it too)")
                    done.append(e.path); continue
                } else if local.exec != e.exec, let m = (try? FileManager.default.attributesOfItem(atPath: url.path))?[.posixPermissions] as? NSNumber {
                    try? FileManager.default.setAttributes([.posixPermissions: withExec(m.intValue, e.exec)], ofItemAtPath: url.path)
                }
                state[e.path] = local; done.append(e.path); continue
            }
            guard let data = fetch(e.path), sha256(data) == e.sha256 else { notes.append("skipped \(e.path): download failed"); continue }
            let local = entry(url, path: e.path, previous: state[e.path])  // znova tik pred pisanjem
            if local?.sha256 != state[e.path]?.sha256 {  // spremenjena (ali izbrisana) tudi tukaj
                guard let c = resolve(e.path + ".minigw-conflict", root: root),
                      (try? write(data, to: c, mtime: e.mtime, exec: e.exec)) != nil else {
                    notes.append("conflict: \(e.path) changed here and on the router; the router's version could not be saved"); continue
                }
                notes.append("conflict: \(e.path) changed here and on the router; kept yours, the router's version is in \(e.path).minigw-conflict")
                done.append(e.path)
                continue
            }
            do {
                // Bit x agenta, razen če ga je uporabnik od prenosa spremenil sam.
                try write(data, to: url, mtime: e.mtime, exec: local != nil && local?.exec != state[e.path]?.exec ? nil : e.exec)
                state[e.path] = e
                done.append(e.path)
            } catch { notes.append("cannot write \(e.path): \(error.localizedDescription)") }
        }
        for p in ch.deleted {
            if isProtected(p) { notes.append("not deleted \(p): protected path"); done.append(p); continue }
            if ignore.covers(p) { done.append(p); continue }
            done.append(p)
            guard let url = resolve(p, root: root) else { continue }
            guard let local = entry(url, path: p, previous: state[p]) else { state[p] = nil; continue }
            if local.sha256 != state[p]?.sha256 { notes.append("conflict: \(p) was deleted on the router but changed here; kept"); continue }
            try? FileManager.default.removeItem(at: url)
            state[p] = nil
        }
        notes += ch.skipped.map { "not copied back (larger than 64 MB): \($0)" }
        return (notes, done)
    }
}
