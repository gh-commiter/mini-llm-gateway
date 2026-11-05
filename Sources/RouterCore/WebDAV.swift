import Foundation

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
    public static func handle(method: String, path: String, headers: [String: String], body: Data,
                              root: URL, prefix: String) -> DAVResponse {
        let root = root.resolvingSymlinksInPath().standardizedFileURL
        guard let (url, rel) = resolve(path, prefix: prefix, root: root) else { return DAVResponse(403) }
        let fm = FileManager.default
        let junk = isJunk(url)  // ._* (xattri) in .DS_Store ne pišemo v mapo odjemalca
        var isDir: ObjCBool = false
        let exists = !junk && fm.fileExists(atPath: url.path, isDirectory: &isDir)

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
            guard let data = fm.contents(atPath: url.path) else { return DAVResponse(403) }
            var h = ["Content-Type": "application/octet-stream", "Last-Modified": httpDate(modified(url)), "Accept-Ranges": "bytes"]
            var part = data, status = 200
            if let r = headers["range"], let (a, b) = range(r, size: data.count) {
                part = data.subdata(in: a..<b + 1)
                h["Content-Range"] = "bytes \(a)-\(b)/\(data.count)"
                status = 206
            }
            if method == "HEAD" { h["Content-Length"] = "\(part.count)"; part = Data() }
            return DAVResponse(status, h, part)

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
            do { try fm.removeItem(at: url) } catch { return DAVResponse(403) }
            return DAVResponse(204)

        case "MOVE", "COPY":
            guard exists, !rel.isEmpty else { return DAVResponse(junk ? 204 : 404) }
            guard let raw = headers["destination"],
                  let (dest, destRel) = resolve(URL(string: raw)?.path(percentEncoded: true) ?? raw, prefix: prefix, root: root),
                  !destRel.isEmpty
            else { return DAVResponse(403) }
            if isJunk(dest) { return DAVResponse(201) }
            guard parentExists(dest) else { return DAVResponse(409) }
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
