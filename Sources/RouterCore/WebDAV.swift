import Foundation
import Synchronization

public struct DAVResponse: Codable, Sendable {
    public var status: Int
    public var headers: [String: String]
    public var body: Data

    public init(_ status: Int, _ headers: [String: String] = [:], _ body: Data = Data()) {
        self.status = status; self.headers = headers; self.body = body
    }
}

/// Minimalni WebDAV (razred 1 + navidezni LOCK, ki ga macOS webdavfs zahteva za pisanje) nad eno mapo.
/// Teče v minigw na odjemalcu; router mu posreduje zahteve priklopa na Mac miniju.
/// Nič zunaj `root`: brez `..`, simbolne povezave se razrešijo in preverijo.
public enum WebDAV {
    /// Največji kos datoteke v enem odgovoru (router sprejme odgovor do 64 MB, base64 doda tretjino).
    public static let maxServe = 32 << 20

    /// Zavrnjene poti, ki smo jih že izpisali. Največ `max` različnih (in toliko si jih zapomnimo): agent z milijoni
    /// imen ne zasiplje terminala in ne veča pomnilnika (M1).
    struct RefusalLog {
        static let max = 50
        var paths: Set<String> = []
        var more = false

        /// Vrstica za stderr ali nil (že izpisano ali čez mejo). Pot v narekovajih z ubežnimi znaki (kot %q v Go),
        /// da ime iz agenta ne krmili terminala (M1).
        mutating func line(_ p: String) -> String? {
            if paths.contains(p) { return nil }
            if paths.count < Self.max {
                paths.insert(p)
                return "minigw: refused agent write to \(Noise.quoted(p)) (protected path)\n"
            }
            if more { return nil }
            more = true
            return "minigw: further refused agent writes not shown\n"
        }
    }
    private static let refused = Mutex(RefusalLog())

    /// Ena vrstica na stderr na zavrnjeno pot (brez ponovitev): agent vidi le »Operation not permitted«, tako da je
    /// razvidno, da je zavrnila ta zaščita in ne peskovnik agenta. Le relativna pot v deljeni mapi.
    static func refuse(_ rel: [String]) -> DAVResponse {
        let p = rel.joined(separator: "/")
        if let line = refused.withLock({ $0.line(p) }) { try? FileHandle.standardError.write(contentsOf: Data(line.utf8)) }
        return forbidden()
    }

    /// Test: lokalna zamenjava po začetnih preverjanjih, tik pred spremembo (kot Go davBeforeMutation); argument = pot korena.
    nonisolated(unsafe) static var beforeMutation: ((String) -> Void)?

    /// 403 z istim besedilom kot Go `dav()`; `reason` = razlog zavrnitve povezave.
    static func forbidden(_ reason: String? = nil) -> DAVResponse {
        DAVResponse(403, [:], Data(("Access refused. " + (reason ?? ShareRoot.linkRefusal + ". Protected paths cannot be modified.")).utf8))
    }

    public static func handle(method: String, path: String, headers: [String: String], body: Data,
                              root: URL, prefix: String) -> DAVResponse {
        let root = root.resolvingSymlinksInPath().standardizedFileURL
        guard let rt = ShareRoot(root), let (_, href0) = resolve(path, prefix: prefix, root: root) else { return forbidden() }
        var p = ShareRoot.join(href0)
        let junk = isJunk(p)  // ._* (xattri) in .DS_Store ne pišemo v mapo odjemalca
        // Povezave v mapi (tudi absolutne) se razrešijo tu; zaščita poti velja za rezultat. DELETE in MOVE delata na povezavi sami.
        let follow = ["GET", "HEAD", "PROPFIND", "PUT", "COPY"].contains(method)
        do { p = try rt.resolve(p, followLast: follow) } catch let e as ShareRoot.Refused { return forbidden(e.message) } catch { return forbidden() }
        var rel = ShareRoot.comps(p)
        var serr: Int32 = 0
        let info = rt.lstat(p, &serr)
        if info == nil, serr != ENOENT, serr != ENOTDIR { return forbidden() }
        let exists = !junk && info != nil
        let isDir = exists && info!.st_mode & S_IFMT == S_IFDIR
        // Zaščitene poti (kot sync): pisanje v .git, .claude/settings*.json, .vscode/tasks.json, .envrc nikoli, tudi
        // prek povezave; branje ostane.
        func protected(_ r: String, _ c: [String]) -> Bool {
            Sync.isProtected(c.joined(separator: "/")) || Sync.protectedCanonical(URL(fileURLWithPath: root.path).appendingPathComponent(r), root: root)
        }
        if ["PUT", "MKCOL", "DELETE", "MOVE", "PROPPATCH"].contains(method) || (method == "LOCK" && !exists), protected(p, rel) {
            return refuse(rel)
        }
        func parentExists(_ r: String) -> Bool { rt.isDir(ShareRoot.join(Array(ShareRoot.comps(r).dropLast()))) }

        switch method {
        case "OPTIONS":
            return DAVResponse(200, ["DAV": "1, 2", "MS-Author-Via": "DAV",
                                     "Allow": "OPTIONS, GET, HEAD, PUT, DELETE, MKCOL, MOVE, COPY, PROPFIND, PROPPATCH, LOCK, UNLOCK"])

        case "PROPFIND":
            guard exists else { return DAVResponse(404) }
            var items = [(p, href0)]
            if isDir, headers["depth"] != "0" {
                for n in (rt.list(p) ?? []).filter({ !isJunk($0) }).sorted() {
                    var child = p == "." ? n : p + "/" + n
                    if let c = try? rt.resolve(child, followLast: true) { child = c }  // povezava v mapi: vrsta in velikost cilja
                    items.append((child, href0 + [n]))
                }
            }
            let xml = items.map { propResponse(rt, $0.0, href: href(prefix, $0.1)) }.joined()
            return DAVResponse(207, ["Content-Type": "application/xml; charset=utf-8"],
                               Data(("<?xml version=\"1.0\" encoding=\"utf-8\"?>\n<D:multistatus xmlns:D=\"DAV:\">" + xml + "</D:multistatus>").utf8))

        case "PROPPATCH":
            guard exists else { return DAVResponse(404) }
            return DAVResponse(207, ["Content-Type": "application/xml; charset=utf-8"], Data((
                "<?xml version=\"1.0\" encoding=\"utf-8\"?>\n<D:multistatus xmlns:D=\"DAV:\"><D:response><D:href>\(href(prefix, href0))</D:href>"
                + "<D:propstat><D:prop/><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response></D:multistatus>").utf8))

        case "GET", "HEAD":
            guard exists, let info else { return DAVResponse(404) }
            if isDir { return DAVResponse(200, ["Content-Type": "text/plain"]) }
            guard info.st_mode & S_IFMT == S_IFREG else { return forbidden() }  // L3: FIFO, naprava ali povezava
            // Le zahtevani del (HEAD nič): odgovor gre v enem kosu prek routerja (do 64 MB, base64).
            let size = Int(info.st_size)
            var h = ["Content-Type": "application/octet-stream", "Last-Modified": httpDate(date(info.st_mtimespec)), "Accept-Ranges": "bytes"]
            var start = 0, n = size, status = 200
            if let r = headers["range"], let (a, b) = range(r, size: size) {
                (start, n, status) = (a, b - a + 1, 206)
                h["Content-Range"] = "bytes \(a)-\(b)/\(size)"
            }
            if method == "HEAD" { h["Content-Length"] = "\(n)"; return DAVResponse(status, h) }
            guard n <= maxServe else { return DAVResponse(413) }  // takoj, namesto da router čaka na odgovor
            guard let part = rt.readRange(p, start: start, count: n) else { return forbidden() }
            return DAVResponse(status, h, part)

        case "PUT":
            if junk { return DAVResponse(201) }
            if isDir { return DAVResponse(405) }
            guard parentExists(p) else { return DAVResponse(409) }
            beforeMutation?(rt.path ?? "")
            // #9: obstoječa datoteka obdrži pravice (0755 build.sh ostane izvršljiv); nova ostane 0600.
            let perm = info.map { $0.st_mode & S_IFMT == S_IFREG ? $0.st_mode & 0o7777 : nil } ?? nil
            guard rt.writeAtomic(p, body, mode: perm, mtime: nil) == 0 else { return forbidden() }
            return DAVResponse(exists ? 204 : 201)

        case "MKCOL":
            if info != nil { return DAVResponse(405) }
            guard parentExists(p) else { return DAVResponse(409) }
            beforeMutation?(rt.path ?? "")
            guard rt.mkdir(p) == 0 else { return forbidden() }
            return DAVResponse(201)

        case "DELETE":
            if junk { return DAVResponse(204) }
            guard exists else { return DAVResponse(404) }
            guard !rel.isEmpty else { return forbidden() }
            guard !rt.treeProtected(p, base: rel) else { return refuse(rel) }
            beforeMutation?(rt.path ?? "")
            guard rt.removeAll(p) else { return forbidden() }
            return DAVResponse(204)

        case "MOVE", "COPY":
            guard exists, !rel.isEmpty else { return DAVResponse(junk ? 204 : 404) }
            guard let raw = headers["destination"],
                  let (_, destRel0) = resolve(URL(string: raw)?.path(percentEncoded: true) ?? raw, prefix: prefix, root: root),
                  !destRel0.isEmpty
            else { return forbidden() }
            // Tudi vsebina mape: nič zaščitenega na cilju (stage/settings.json → .claude), nič zaščitenega ne izgine
            // (MOVE .claude stran, prepis obstoječe .claude). Cilj: zadnja komponenta ostane, kot je.
            let dest: String
            do { dest = try rt.resolve(ShareRoot.join(destRel0), followLast: false) } catch let e as ShareRoot.Refused { return forbidden(e.message) } catch { return forbidden() }
            let destRel = ShareRoot.comps(dest)
            if protected(dest, destRel) || rt.treeProtected(p, base: destRel) || (rt.lstat(dest) != nil && rt.treeProtected(dest, base: destRel)) {
                return refuse(destRel)
            }
            if method == "MOVE", rt.treeProtected(p, base: rel) { return refuse(rel) }
            if isJunk(dest) { return DAVResponse(201) }
            guard parentExists(dest) else { return DAVResponse(409) }
            // Mapa v svojo podmapo in vir v cilju (MOVE /a/b → /a bi z brisanjem cilja izbrisal vir): nikoli
            // (poti sta brez povezav; macOS: velikost črk ni pomembna).
            let src = p.lowercased() + "/", dst = dest.lowercased() + "/"
            if dst != src, dst.hasPrefix(src) || src.hasPrefix(dst) { return forbidden() }
            beforeMutation?(rt.path ?? "")
            // Ista datoteka (»a.txt« → »A.txt« na APFS, trda povezava): cilja ne brišemo, ker je to vir.
            // MOVE preimenuje prek začasnega imena (tudi datotečni sistemi, ki sami ne preimenujejo le velikosti črk).
            if let s = rt.lstat(p), let d = rt.lstat(dest), s.st_dev == d.st_dev, s.st_ino == d.st_ino {
                if method == "COPY" { return forbidden() }
                let tmp = ShareRoot.join(Array(ShareRoot.comps(p).dropLast()) + [".minigw-mv-" + UUID().uuidString])
                guard rt.rename(p, tmp) else { return forbidden() }
                guard rt.rename(tmp, dest) else { _ = rt.rename(tmp, p); return forbidden() }
                rt.remove(tmp)  // trda povezava: rename je bil brez učinka, tmp je le še ena povezava cilja
                return DAVResponse(201)
            }
            let replaced = rt.lstat(dest) != nil  // povezava na cilju je sama zamenjana
            if replaced {
                if headers["overwrite"]?.uppercased() == "F" { return DAVResponse(412) }
                guard rt.removeAll(dest) else { return forbidden() }
            }
            guard method == "MOVE" ? rt.rename(p, dest) : rt.copyTree(p, dest) else { return forbidden() }
            return DAVResponse(replaced ? 204 : 201)

        case "LOCK":
            // Navidezen izključni zaklep: en agent na sejo, macOS ga potrebuje le za pisanje.
            var created = false
            if !exists, !junk {
                guard parentExists(p) else { return DAVResponse(409) }
                beforeMutation?(rt.path ?? "")
                guard let f = rt.create(p) else { return forbidden() }
                close(f)
                created = true
            }
            let token = headers["if"].flatMap { h in h.range(of: "opaquelocktoken:[^>]+", options: .regularExpression).map { String(h[$0]) } }
                ?? "opaquelocktoken:" + UUID().uuidString.lowercased()
            let xml = "<?xml version=\"1.0\" encoding=\"utf-8\"?>\n<D:prop xmlns:D=\"DAV:\"><D:lockdiscovery><D:activelock>"
                + "<D:locktype><D:write/></D:locktype><D:lockscope><D:exclusive/></D:lockscope><D:depth>0</D:depth>"
                + "<D:timeout>Second-3600</D:timeout><D:locktoken><D:href>\(token)</D:href></D:locktoken>"
                + "</D:activelock></D:lockdiscovery></D:prop>"
            return DAVResponse(created ? 201 : 200, ["Lock-Token": "<\(token)>", "Content-Type": "application/xml; charset=utf-8"], Data(xml.utf8))

        case "UNLOCK":
            return DAVResponse(204)

        default:
            return DAVResponse(405)
        }
    }

    /// Pot zahteve (z `prefix`, kodirana) → datoteka pod `root` in relativne komponente; nil, če bi ušla iz `root`.
    static func resolve(_ path: String, prefix: String, root: URL) -> (URL, [String])? {
        guard path.hasPrefix(prefix) else { return nil }
        var comps: [String] = []
        for c in path.dropFirst(prefix.count).split(separator: "/") {
            guard let name = String(c).removingPercentEncoding, name != ".", name != "..", !name.contains("/"), !name.contains("\0")
            else { return nil }
            comps.append(name)
        }
        let url = comps.reduce(root) { $0.appendingPathComponent($1) }
        // Simbolne povezave: obstoječi del poti mora tudi po razrešitvi ostati v root.
        var existing = url
        while !FileManager.default.fileExists(atPath: existing.path), existing.path != root.path { existing.deleteLastPathComponent() }
        let real = existing.resolvingSymlinksInPath().standardizedFileURL.path
        guard real == root.path || real.hasPrefix(root.path + "/") else { return nil }
        return (url, comps)
    }

    static func isJunk(_ path: String) -> Bool {
        let n = (path as NSString).lastPathComponent
        return n.hasPrefix("._") || n == ".DS_Store"
    }

    static func href(_ prefix: String, _ comps: [String]) -> String {
        let allowed = CharacterSet.urlPathAllowed.subtracting(CharacterSet(charactersIn: "/?#[]@!$&'()*+,;="))
        return prefix + "/" + comps.map { $0.addingPercentEncoding(withAllowedCharacters: allowed) ?? $0 }.joined(separator: "/")
    }

    /// Povezava, ki je `resolve` ni prepisal (zapusti mapo, ostane povezava) ali česa ni: ne vrsta ne velikost povezave
    /// (kot Go rootPropResponse: datoteka velikosti 0).
    static func propResponse(_ rt: ShareRoot, _ rel: String, href: String) -> String {
        let st = rt.lstat(rel).flatMap { [S_IFREG, S_IFDIR].contains($0.st_mode & S_IFMT) ? $0 : nil }
        let dir = st.map { $0.st_mode & S_IFMT == S_IFDIR } ?? false
        let size = st.map { Int($0.st_size) } ?? 0
        let mod = st.map { date($0.st_mtimespec) } ?? Date()
        let created = st.map { date($0.st_birthtimespec) } ?? mod
        var props = "<D:resourcetype>\(dir ? "<D:collection/>" : "")</D:resourcetype>"
            + "<D:getlastmodified>\(httpDate(mod))</D:getlastmodified>"
            + "<D:creationdate>\(ISO8601DateFormatter().string(from: created))</D:creationdate>"
        if !dir { props += "<D:getcontentlength>\(size)</D:getcontentlength><D:getcontenttype>application/octet-stream</D:getcontenttype>" }
        return "<D:response><D:href>\(href + (dir && !href.hasSuffix("/") ? "/" : ""))</D:href><D:propstat><D:prop>\(props)</D:prop>"
            + "<D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>"
    }

    static func date(_ t: timespec) -> Date { Date(timeIntervalSince1970: Double(t.tv_sec) + Double(t.tv_nsec) / 1e9) }

    static func httpDate(_ d: Date) -> String {
        let f = DateFormatter()
        f.locale = Locale(identifier: "en_US_POSIX")
        f.timeZone = TimeZone(identifier: "GMT")
        f.dateFormat = "EEE, dd MMM yyyy HH:mm:ss 'GMT'"
        return f.string(from: d)
    }

    /// »bytes=a-b« / »bytes=a-« / »bytes=-n« → zaprt interval; nil = celotna datoteka.
    static func range(_ h: String, size: Int) -> (Int, Int)? {
        guard size > 0, h.hasPrefix("bytes="), !h.contains(",") else { return nil }
        let p = h.dropFirst(6).split(separator: "-", omittingEmptySubsequences: false).map(String.init)
        guard p.count == 2 else { return nil }
        if p[0].isEmpty, let n = Int(p[1]), n > 0 { return (max(0, size - n), size - 1) }
        guard let a = Int(p[0]), a < size else { return nil }
        let b = min(Int(p[1]) ?? size - 1, size - 1)
        return a <= b ? (a, b) : nil
    }
}
