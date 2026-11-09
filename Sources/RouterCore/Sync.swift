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
        /// sha256 različice, iz katere je agent začel, za vsako spremenjeno in izbrisano pot (ni ključa = datoteke ni bilo).
        /// Odjemalec piše ali briše le, če ima še to različico. nil = starejši router → stanje odjemalca ob prenosu.
        public var base: [String: String]?
        public init(changed: [Entry] = [], deleted: [String] = [], skipped: [String] = [], base: [String: String]? = nil) {
            self.changed = changed; self.deleted = deleted; self.skipped = skipped; self.base = base
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

        /// Le navadna datoteka do 1 MB (ne FIFO ali naprava); sicer kot brez .gitignore. `follow`: mapa uporabnika,
        /// .gitignore je lahko povezava (dotfiles); router (kopija, klon) povezavi ne sledi.
        public init(root: URL, follow: Bool) {
            self.init(gitignore: Sync.read(root.appendingPathComponent(".gitignore"), max: 1 << 20, flags: follow ? 0 : O_NOFOLLOW)
                .map { String(decoding: $0, as: UTF8.self) } ?? "")
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

    /// Pot, ki je prevzem nikoli ne zapiše ali izbriše: git, hooki gita (.husky, .githooks), .claude/{hooks,commands,
    /// agents,skills} in nastavitve, ki zaganjajo ukaze (.claude/settings*.json, .vscode/tasks.json, .envrc).
    /// Isti seznam velja za WebDAV (M1, kot Go protectedPath).
    public static func isProtected(_ rel: String) -> Bool {
        let c = rel.split(separator: "/").map { normName(String($0)) }
        let last = c.last ?? "", parent = c.count > 1 ? c[c.count - 2] : ""
        let run: Set<String> = ["hooks", "commands", "agents", "skills"]
        if c.contains(where: { $0 == ".husky" || $0 == ".githooks" }) || zip(c, c.dropFirst()).contains(where: { $0 == ".claude" && run.contains($1) }) {
            return true
        }
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
    /// `trusted`: mapa uporabnika (odjemalec); sicer kopija ali klon routerja, kjer ne sledi povezavam nikjer v poti.
    public static func scan(_ root: URL, previous: [String: Entry] = [:], trusted: Bool = false) -> (files: [String: Entry], skipped: [String]) {
        let root = trusted ? root : real(root) ?? root
        let ignore = Ignore(root: root, follow: trusted)
        var files: [String: Entry] = [:], skipped: [String] = []
        func walk(_ dir: URL, _ rel: String) {
            let names = trusted ? (try? FileManager.default.contentsOfDirectory(atPath: dir.path)) : list(dir)
            for name in (names ?? []).sorted() {
                // #3: ime v manifestu je NFC (Windows, Linux); datoteka ostane pod svojim imenom (APFS najde obe obliki).
                let key = name.precomposedStringWithCanonicalMapping
                let r = rel.isEmpty ? key : rel + "/" + key, url = dir.appendingPathComponent(name)
                if normName(name) == ".git" { continue }  // tudi ».GIT«
                guard let a = try? FileManager.default.attributesOfItem(atPath: url.path) else { continue }  // ne sledi povezavam
                switch a[.type] as? FileAttributeType {
                case .typeDirectory?:
                    if !ignore.ignored(r, isDir: true) { walk(url, r) }
                case .typeRegular?:
                    guard !ignore.ignored(r, isDir: false) else { continue }
                    if (a[.size] as? NSNumber)?.int64Value ?? 0 > maxFile { skipped.append(r); continue }
                    if let e = entry(url, path: r, previous: previous[r], flags: trusted ? O_NOFOLLOW : O_NOFOLLOW_ANY) { files[r] = e }
                default:
                    continue
                }
            }
        }
        walk(root, "")
        return (files, skipped)
    }

    /// #1, #9: največ datotek in bajtov na kopijo (manifest odjemalca); nil = v mejah, sicer sporočilo.
    static let maxFiles = 100_000, maxBytes: Int64 = 8 << 30
    static func overQuota(_ files: [Entry]) -> String? {
        let bytes = files.reduce(Int64(0)) { $0 + max(0, $1.size) }
        guard files.count > maxFiles || bytes > maxBytes else { return nil }
        return "The folder is too large to sync (\(files.count) files, \(bytes >> 20) MB; the limit is \(maxFiles) files and \(maxBytes >> 30) GB): "
            + "add build output and large data to .gitignore, or use a smaller folder"
    }

    /// M2: nova kopija (sync/<naprava>/<mapa>) bi presegla maxCopies; obstoječa sme vedno naprej.
    static let maxCopies = 32
    static func tooManyCopies(_ copy: URL) -> Bool {
        let fm = FileManager.default
        if (try? fm.attributesOfItem(atPath: copy.path)) != nil { return false }
        let items = (try? fm.contentsOfDirectory(at: copy.deletingLastPathComponent(), includingPropertiesForKeys: [.isDirectoryKey])) ?? []
        return items.filter { (try? $0.resourceValues(forKeys: [.isDirectoryKey]).isDirectory) == true }.count >= maxCopies
    }

    /// #2: poti, ki se razlikujejo le v velikih črkah (A.txt in a.txt): na routerju brez razlikovanja črk bi bile ena
    /// datoteka, zato ne gredo nikamor.
    public static func caseCollisions(_ paths: Set<String>) -> [String] {
        Dictionary(grouping: paths, by: { $0.lowercased() }).values.filter { $0.count > 1 }.flatMap { $0 }.sorted()
    }

    /// #23: imena v mapi kopije ali klona, odprte z O_NOFOLLOW_ANY: mapa, ki jo ostanek agenta po razvrstitvi zamenja s
    /// povezavo (npr. na domačo mapo), se ne odpre, zato se ne izpiše drevo zunaj korena.
    static func list(_ dir: URL) -> [String]? {
        let fd = open(dir.path, O_RDONLY | O_DIRECTORY | O_NOFOLLOW_ANY | O_CLOEXEC)
        guard fd >= 0 else { return nil }
        guard let d = fdopendir(fd) else { close(fd); return nil }
        defer { closedir(d) }
        var out: [String] = []
        while let e = readdir(d) {
            let name = withUnsafeBytes(of: e.pointee.d_name) { String(decoding: $0.prefix(Int(e.pointee.d_namlen)), as: UTF8.self) }
            if name != "." && name != ".." { out.append(name) }
        }
        return out
    }

    /// Ena datoteka; nil, če ne obstaja ali ni navadna datoteka.
    static func entry(_ url: URL, path: String, previous: Entry?, flags: Int32 = O_NOFOLLOW) -> Entry? {
        guard let a = try? FileManager.default.attributesOfItem(atPath: url.path), a[.type] as? FileAttributeType == .typeRegular
        else { return nil }
        let size = (a[.size] as? NSNumber)?.int64Value ?? 0
        let mtime = Int64((((a[.modificationDate] as? Date)?.timeIntervalSince1970 ?? 0) * 1000).rounded())
        let exec = ((a[.posixPermissions] as? NSNumber)?.intValue ?? 0) & 0o100 != 0
        if var p = previous, p.size == size, p.mtime == mtime { p.path = path; p.exec = exec; return p }
        guard let f = openRegular(url, flags: flags) else { return nil }
        var h = SHA256()
        while let d = try? f.read(upToCount: 1 << 20), !d.isEmpty { h.update(data: d) }
        return Entry(path: path, size: size, mtime: mtime, sha256: hex(h.finalize()), exec: exec)
    }

    /// Navadna datoteka, odprta brez čakanja na FIFO; `flags`: O_NOFOLLOW (zadnji del), O_NOFOLLOW_ANY (nikjer v poti,
    /// pot mora biti kanonična) ali 0 (sledi).
    static func openRegular(_ url: URL, flags: Int32 = O_NOFOLLOW) -> FileHandle? {
        let fd = Darwin.open(url.path, O_RDONLY | O_NONBLOCK | flags)
        guard fd >= 0 else { return nil }
        let f = FileHandle(fileDescriptor: fd, closeOnDealloc: true)
        var st = stat()
        return fstat(fd, &st) == 0 && st.st_mode & S_IFMT == S_IFREG ? f : nil
    }

    /// Kanonična pot (realpath); resolvingSymlinksInPath odreže »/private« in pusti povezavo »/var«.
    public static func real(_ url: URL) -> URL? {
        guard let p = realpath(url.path, nil) else { return nil }
        defer { free(p) }
        return URL(fileURLWithPath: String(cString: p))
    }

    /// Vsebina navadne datoteke (glej openRegular); nil, če je večja od `max`.
    static func read(_ url: URL, max: Int, flags: Int32 = O_NOFOLLOW) -> Data? {
        guard let f = openRegular(url, flags: flags), let d = try? f.read(upToCount: max + 1) ?? Data(), d.count <= max else { return nil }
        return d
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

    /// #11: zaščita tudi za kanonično pot (povezava hooks → .git/hooks): kanonična pot najglobljega obstoječega prednika
    /// z manjkajočim koncem, relativno na kanonični koren; nečitljiva pot ali pot zunaj korena = zaščitena.
    static func protectedCanonical(_ url: URL, root: URL) -> Bool {
        guard let r = real(root)?.path else { return true }
        var p = url.path, rest: [String] = []
        while true {
            if let c = real(URL(fileURLWithPath: p))?.path {
                let full = ([c] + rest).joined(separator: "/")
                guard full == r || full.hasPrefix(r + "/") else { return true }
                let rel = String(full.dropFirst(r.count)).trimmingCharacters(in: CharacterSet(charactersIn: "/"))
                return !rel.isEmpty && isProtected(rel)
            }
            let parent = (p as NSString).deletingLastPathComponent
            guard parent != p, !parent.isEmpty else { return true }
            rest.insert((p as NSString).lastPathComponent, at: 0)
            p = parent
        }
    }

    /// Atomarno zapiše datoteko (ustvari mape), nastavi čas spremembe druge strani. Način obstoječe (sicer 0644) ostane,
    /// biti x pa sledijo `exec`; nil = druga stran bita x ne pozna (Windows) → ostanejo.
    public static func write(_ data: Data, to url: URL, mtime: Int64, exec: Bool?) throws {
        let fm = FileManager.default
        let old = ((try? fm.attributesOfItem(atPath: url.path))?[.posixPermissions] as? NSNumber)?.intValue ?? 0o644
        try fm.createDirectory(at: url.deletingLastPathComponent(), withIntermediateDirectories: true)
        rmdir(url.path)  // prazna mapa na mestu datoteke (mapa → datoteka); polna ostane in pisanje ne uspe
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
    /// Izbris le, če na poti ni datoteke ali povezave: na novo prezrta ali prevelika datoteka ni izbrisana; mapa na njenem
    /// mestu (datoteka »a« → »a/b«) pa je izbris.
    public static func diff(old: [String: Entry], new: [String: Entry], root: URL) -> Changes {
        Changes(changed: new.values.filter { old[$0.path]?.sha256 != $0.sha256 || old[$0.path]?.exec != $0.exec }
                    .sorted { $0.path < $1.path },
                deleted: old.keys.filter { p in
                    let t = (try? FileManager.default.attributesOfItem(atPath: root.appendingPathComponent(p).path))?[.type] as? FileAttributeType
                    return new[p] == nil && (t == nil || t == .typeDirectory)
                }.sorted())
    }

    /// Odjemalec po agentu: zapiše in izbriše, kar je agent spremenil. Datoteke, ki ni več različica, iz katere je agent
    /// začel (`ch.base`; spremenil jo je uporabnik ali drug agent), ne prepiše: različico routerja zapiše kot
    /// `<ime>.minigw-conflict`. Poti, prezrte pri odjemalcu, preskoči.
    /// `state` = manifest ob prenosu. Vrne sporočila za stderr in obdelane poti (potrditev routerju).
    public static func apply(_ ch: Changes, root: URL, state: inout [String: Entry], known: ((String, String) -> Bool)? = nil,
                             fetch: (String) -> Data?) -> (notes: [String], done: [String]) {
        let ignore = Ignore(root: root, follow: true)  // odjemalec: mapa uporabnika
        var notes: [String] = [], done: [String] = []
        let before = state
        // #20: osnovo pošlje router; velja le različica, ki jo je ta odjemalec poslal. Sicer osnova, ki je nobena datoteka
        // nima: uporabnikova sprememba med izvajanjem ostane, agentova gre v .minigw-conflict.
        let base = { (p: String) -> String? in
            guard let b = ch.base else { return before[p]?.sha256 }
            if let v = b[p], v != before[p]?.sha256, let known, !known(p, v) { return "\u{0}unknown base" }
            return b[p]
        }
        for p in ch.deleted {  // pred spremembami: datoteka »a« → mapa »a/…«
            if isProtected(p) { notes.append("not deleted \(p): protected path"); done.append(p); continue }
            if ignore.covers(p) { done.append(p); continue }
            done.append(p)
            guard let url = resolve(p, root: root) else { continue }
            if protectedCanonical(url, root: root) { notes.append("not deleted \(p): protected path (through a link)"); continue }
            guard let local = entry(url, path: p, previous: state[p]) else { state[p] = nil; continue }
            if local.sha256 != base(p) { notes.append("conflict: \(p) was deleted on the gateway but changed here; kept"); continue }
            try? FileManager.default.removeItem(at: url)
            state[p] = nil
        }
        for e in ch.changed {
            if isProtected(e.path) { notes.append("skipped \(e.path): protected path"); done.append(e.path); continue }
            if ignore.covers(e.path) { notes.append("skipped \(e.path): ignored here"); done.append(e.path); continue }
            guard let url = resolve(e.path, root: root) else {  // potrjena, sicer bi jo router pošiljal v nedogled
                notes.append("skipped \(e.path): invalid path here"); done.append(e.path); continue
            }
            if protectedCanonical(url, root: root) { notes.append("skipped \(e.path): protected path (through a link)"); done.append(e.path); continue }
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
            if local?.sha256 != base(e.path) {  // spremenjena (ali izbrisana) tudi tukaj
                guard let c = resolve(e.path + ".minigw-conflict", root: root),
                      (try? write(data, to: c, mtime: e.mtime, exec: e.exec)) != nil else {
                    notes.append("conflict: \(e.path) changed here and on the gateway; the gateway's version could not be saved"); continue
                }
                notes.append("conflict: \(e.path) changed here and on the gateway; kept yours, the gateway's version is in \(e.path).minigw-conflict")
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
        notes += ch.skipped.map { "not copied back (larger than 64 MB): \($0)" }
        return (notes, done)
    }
}
