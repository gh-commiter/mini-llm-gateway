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
        if let line = refused.withLock({ $0.line(p) }) { FileHandle.standardError.write(Data(line.utf8)) }
        return DAVResponse(403)
    }

    public static func handle(method: String, path: String, headers: [String: String], body: Data,
                              root: URL, prefix: String) -> DAVResponse {
        let root = root.resolvingSymlinksInPath().standardizedFileURL
        guard let (url, rel) = resolve(path, prefix: prefix, root: root) else { return DAVResponse(403) }
        let fm = FileManager.default
        let junk = isJunk(url)  // ._* (xattri) in .DS_Store ne pišemo v mapo odjemalca
        var isDir: ObjCBool = false
        let exists = !junk && fm.fileExists(atPath: url.path, isDirectory: &isDir)
        // Zaščitene poti (kot sync): pisanje v .git, .claude/settings*.json, .vscode/tasks.json, .envrc nikoli, tudi
        // prek povezave; branje ostane.
        func protected(_ u: URL, _ r: [String]) -> Bool { Sync.isProtected(r.joined(separator: "/")) || Sync.protectedCanonical(u, root: root) }
        if ["PUT", "MKCOL", "DELETE", "MOVE", "PROPPATCH"].contains(method) || (method == "LOCK" && !exists), protected(url, rel) {
            return refuse(rel)
        }

        switch method {
        case "OPTIONS":
            return DAVResponse(200, ["DAV": "1, 2", "MS-Author-Via": "DAV",
                                     "Allow": "OPTIONS, GET, HEAD, PUT, DELETE, MKCOL, MOVE, COPY, PROPFIND, PROPPATCH, LOCK, UNLOCK"])

        case "PROPFIND":
            guard exists else { return DAVResponse(404) }
            var items = [(url, rel)]
            if isDir.boolValue, headers["depth"] != "0" {
                let names = (try? fm.contentsOfDirectory(atPath: url.path)) ?? []
                items += names.sorted().filter { !isJunk(URL(fileURLWithPath: $0)) }.map {
                    (url.appendingPathComponent($0), rel + [$0])
                }
            }
            let xml = items.map { propResponse($0.0, href: href(prefix, $0.1)) }.joined()
            return DAVResponse(207, ["Content-Type": "application/xml; charset=utf-8"],
                               Data(("<?xml version=\"1.0\" encoding=\"utf-8\"?>\n<D:multistatus xmlns:D=\"DAV:\">" + xml + "</D:multistatus>").utf8))

        case "PROPPATCH":
            guard exists else { return DAVResponse(404) }
            return DAVResponse(207, ["Content-Type": "application/xml; charset=utf-8"], Data((
                "<?xml version=\"1.0\" encoding=\"utf-8\"?>\n<D:multistatus xmlns:D=\"DAV:\"><D:response><D:href>\(href(prefix, rel))</D:href>"
                + "<D:propstat><D:prop/><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response></D:multistatus>").utf8))

        case "GET", "HEAD":
            guard exists else { return DAVResponse(404) }
            if isDir.boolValue { return DAVResponse(200, ["Content-Type": "text/plain"]) }
            var st = stat()  // L3: FIFO ali naprava bi zadržala delavca deljenja
            guard stat(url.path, &st) == 0, st.st_mode & S_IFMT == S_IFREG else { return DAVResponse(403) }
            // Le zahtevani del (HEAD nič): odgovor gre v enem kosu prek routerja (do 64 MB, base64).
            let size = ((try? fm.attributesOfItem(atPath: url.path)[.size]) as? NSNumber)?.intValue ?? 0
            var h = ["Content-Type": "application/octet-stream", "Last-Modified": httpDate(modified(url)), "Accept-Ranges": "bytes"]
            var start = 0, n = size, status = 200
            if let r = headers["range"], let (a, b) = range(r, size: size) {
                (start, n, status) = (a, b - a + 1, 206)
                h["Content-Range"] = "bytes \(a)-\(b)/\(size)"
            }
            if method == "HEAD" { h["Content-Length"] = "\(n)"; return DAVResponse(status, h) }
            guard n <= maxServe else { return DAVResponse(413) }  // takoj, namesto da router čaka na odgovor
            guard let f = FileHandle(forReadingAtPath: url.path) else { return DAVResponse(403) }
            defer { try? f.close() }
            guard (try? f.seek(toOffset: UInt64(start))) != nil else { return DAVResponse(403) }
            return DAVResponse(status, h, n == 0 ? Data() : ((try? f.read(upToCount: n)) ?? Data()))

        case "PUT":
            if junk { return DAVResponse(201) }
            if isDir.boolValue { return DAVResponse(405) }
            guard parentExists(url) else { return DAVResponse(409) }
            do { try body.write(to: url, options: .atomic) } catch { return DAVResponse(403) }
            return DAVResponse(exists ? 204 : 201)

        case "MKCOL":
            if fm.fileExists(atPath: url.path) { return DAVResponse(405) }
            guard parentExists(url) else { return DAVResponse(409) }
            do { try fm.createDirectory(at: url, withIntermediateDirectories: false) } catch { return DAVResponse(403) }
            return DAVResponse(201)

        case "DELETE":
            if junk { return DAVResponse(204) }
            guard exists else { return DAVResponse(404) }
            guard !rel.isEmpty else { return DAVResponse(403) }
            guard !treeProtected(url, rel) else { return refuse(rel) }
            do { try fm.removeItem(at: url) } catch { return DAVResponse(403) }
            return DAVResponse(204)

        case "MOVE", "COPY":
            guard exists, !rel.isEmpty else { return DAVResponse(junk ? 204 : 404) }
            guard let raw = headers["destination"],
                  let (dest, destRel) = resolve(URL(string: raw)?.path(percentEncoded: true) ?? raw, prefix: prefix, root: root),
                  !destRel.isEmpty
            else { return DAVResponse(403) }
            // Tudi vsebina mape: nič zaščitenega na cilju (stage/settings.json → .claude), nič zaščitenega ne izgine
            // (MOVE .claude stran, prepis obstoječe .claude).
            var st = stat()
            if protected(dest, destRel) || treeProtected(url, destRel) || (lstat(dest.path, &st) == 0 && treeProtected(dest, destRel)) {
                return refuse(destRel)
            }
            if method == "MOVE", treeProtected(url, rel) { return refuse(rel) }
            if isJunk(dest) { return DAVResponse(201) }
            guard parentExists(dest) else { return DAVResponse(409) }
            // Mapa v svojo podmapo in vir v cilju (MOVE /a/b → /a bi z brisanjem cilja izbrisal vir): nikoli
            // (kanonični poti, da povezava ne skrije zanke; macOS: velikost črk ni pomembna).
            let src = url.resolvingSymlinksInPath().path.lowercased() + "/"
            let dst = dest.deletingLastPathComponent().resolvingSymlinksInPath().appendingPathComponent(dest.lastPathComponent).path.lowercased() + "/"
            if dst != src, dst.hasPrefix(src) || src.hasPrefix(dst) { return DAVResponse(403) }
            // Ista datoteka (»a.txt« → »A.txt« na APFS, trda povezava): cilja ne brišemo, ker je to vir.
            // MOVE preimenuje prek začasnega imena (tudi datotečni sistemi, ki sami ne preimenujejo le velikosti črk).
            if sameFile(url, dest) {
                if method == "COPY" { return DAVResponse(403) }
                let tmp = url.deletingLastPathComponent().appendingPathComponent(".minigw-mv-" + UUID().uuidString).path
                guard rename(url.path, tmp) == 0 else { return DAVResponse(403) }
                guard rename(tmp, dest.path) == 0 else { _ = rename(tmp, url.path); return DAVResponse(403) }
                _ = unlink(tmp)  // trda povezava: rename je bil brez učinka, tmp je le še ena povezava cilja
                return DAVResponse(201)
            }
            let replaced = fm.fileExists(atPath: dest.path)
            if replaced {
                if headers["overwrite"]?.uppercased() == "F" { return DAVResponse(412) }
                try? fm.removeItem(at: dest)
            }
            do {
                if method == "MOVE" { try fm.moveItem(at: url, to: dest) } else { try fm.copyItem(at: url, to: dest) }
            } catch { return DAVResponse(403) }
            return DAVResponse(replaced ? 204 : 201)

        case "LOCK":
            // Navidezen izključni zaklep: en agent na sejo, macOS ga potrebuje le za pisanje.
            var created = false
            if !exists, !junk {
                guard parentExists(url) else { return DAVResponse(409) }
                created = fm.createFile(atPath: url.path, contents: nil)
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

    /// Ista datoteka (naprava + inode, brez sledenja zadnji povezavi).
    static func sameFile(_ a: URL, _ b: URL) -> Bool {
        var x = stat(), y = stat()
        return lstat(a.path, &x) == 0 && lstat(b.path, &y) == 0 && x.st_dev == y.st_dev && x.st_ino == y.st_ino
    }

    /// Ali je katera koli pot v drevesu `dir` (z imenom `base`) zaščitena.
    /// ponytail: enumerator(atPath:) tiho preskoči nečitljive podmape; ob potrebi enumerator z errorHandler.
    static func treeProtected(_ dir: URL, _ base: [String]) -> Bool {
        let b = base.joined(separator: "/")
        if Sync.isProtected(b) { return true }
        guard let e = FileManager.default.enumerator(atPath: dir.path) else { return false }
        for case let sub as String in e where Sync.isProtected(b.isEmpty ? sub : b + "/" + sub) { return true }
        return false
    }

    static func isJunk(_ url: URL) -> Bool {
        let n = url.lastPathComponent
        return n.hasPrefix("._") || n == ".DS_Store"
    }

    static func parentExists(_ url: URL) -> Bool {
        var d: ObjCBool = false
        return FileManager.default.fileExists(atPath: url.deletingLastPathComponent().path, isDirectory: &d) && d.boolValue
    }

    static func href(_ prefix: String, _ comps: [String]) -> String {
        let allowed = CharacterSet.urlPathAllowed.subtracting(CharacterSet(charactersIn: "/?#[]@!$&'()*+,;="))
        return prefix + "/" + comps.map { $0.addingPercentEncoding(withAllowedCharacters: allowed) ?? $0 }.joined(separator: "/")
    }

    static func propResponse(_ url: URL, href: String) -> String {
        let a = (try? FileManager.default.attributesOfItem(atPath: url.path)) ?? [:]
        let dir = a[.type] as? FileAttributeType == .typeDirectory
        let size = (a[.size] as? NSNumber)?.intValue ?? 0
        let mod = a[.modificationDate] as? Date ?? Date()
        let created = a[.creationDate] as? Date ?? mod
        var props = "<D:resourcetype>\(dir ? "<D:collection/>" : "")</D:resourcetype>"
            + "<D:getlastmodified>\(httpDate(mod))</D:getlastmodified>"
            + "<D:creationdate>\(ISO8601DateFormatter().string(from: created))</D:creationdate>"
        if !dir { props += "<D:getcontentlength>\(size)</D:getcontentlength><D:getcontenttype>application/octet-stream</D:getcontenttype>" }
        return "<D:response><D:href>\(href + (dir && !href.hasSuffix("/") ? "/" : ""))</D:href><D:propstat><D:prop>\(props)</D:prop>"
            + "<D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>"
    }

    static func modified(_ url: URL) -> Date {
        ((try? FileManager.default.attributesOfItem(atPath: url.path))?[.modificationDate] as? Date) ?? Date()
    }

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
