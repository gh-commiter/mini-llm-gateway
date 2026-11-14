import Foundation

/// #82/#107 (kot Go os.Root, client/rootresolve.go + mutation.go): odprta mapa deljenja. Vsako branje in pisanje gre
/// relativno na njen deskriptor (openat, fstatat, mkdirat, unlinkat, renameat), nikoli po absolutnem imenu poti, zato
/// starševska mapa, zamenjana s povezavo med preverjanjem in operacijo, ne vodi iz mape ali v zaščiteno pot.
/// Poti so relativne, z »/«, »." = koren. Operacije so stroge: povezava kjerkoli v poti (O_NOFOLLOW_ANY) = napaka;
/// povezave, ki ostanejo v mapi, razreši `resolve` pred operacijo, ki nato dobi pot brez povezav.
final class ShareRoot: @unchecked Sendable {
    /// Isto besedilo kot Go `linkRefusal`.
    static let linkRefusal = "links that lead outside the shared folder are not followed"
    static let maxLinkRewrites = 40

    struct Refused: Error { let message: String }

    let fd: Int32
    /// Prava pot korena (F_GETPATH): sklic za pot nadrejene mape, ki jo operacija pripne.
    let path: String
    /// Imena korena (kot odprto, prava pot): tako je lahko napisana absolutna povezava, ki kaže v mapo.
    private let bases: [[String]]

    init?(_ url: URL) {
        guard let real = Sync.real(url) else { return nil }
        let fd = Darwin.open(real.path, O_RDONLY | O_DIRECTORY | O_CLOEXEC)
        guard fd >= 0 else { return nil }
        self.fd = fd
        path = Self.path(of: fd) ?? real.path
        bases = Set([url.standardizedFileURL.path, real.path, path]).map(Self.lexical)
    }
    deinit { close(fd) }

    static func path(of fd: Int32) -> String? {
        var buf = [CChar](repeating: 0, count: Int(MAXPATHLEN))
        return fcntl(fd, F_GETPATH, &buf) == 0 ? String(cString: buf) : nil
    }

    // MARK: poti

    /// Komponente brez praznih in ».«.
    static func comps(_ rel: String) -> [String] { rel.split(separator: "/").map(String.init).filter { $0 != "." } }
    static func join(_ c: [String]) -> String { c.isEmpty ? "." : c.joined(separator: "/") }
    /// Leksikalno čiščenje (»..« odstrani prejšnjo komponento).
    static func lexical(_ p: String) -> [String] {
        var out: [String] = []
        for c in p.split(separator: "/") where c != "." { if c == ".." { _ = out.popLast() } else { out.append(String(c)) } }
        return out
    }

    // MARK: razreševanje povezav (kot Go rootResolve)

    /// Pot brez povezav. Povezava (relativna ali absolutna), katere cilj je v mapi, se prepiše; povezava, ki mapo zapusti,
    /// je zavrnjena (`Refused`). `followLast` = false: zadnja komponenta ostane, kot je (DELETE, MOVE, ustvarjanje).
    /// Razreševanje le izbere pot; operacija še vedno gre prek deskriptorja, zato povezava, ki se spremeni po njem, ne vodi ven.
    /// Zaščiteno pot je treba preveriti na vrnjeni poti, ne na vhodu.
    func resolve(_ rel: String, followLast: Bool) throws -> String {
        var comps = Self.comps(rel)
        var rewrites = 0
        while !comps.isEmpty {
            if rewrites > Self.maxLinkRewrites { throw Refused(message: "too many links: \(Self.join(comps))") }
            var cur: [String] = []
            var again = false
            for (i, name) in comps.enumerated() {
                let next = cur + [name], rest = Array(comps[(i + 1)...])
                if i == comps.count - 1 && !followLast { return Self.join(next) }
                var e: Int32 = 0
                guard let st = lstat(Self.join(next), &e) else { return Self.join(next + rest) }  // je ni: operacija sporoči
                guard st.st_mode & S_IFMT == S_IFLNK, let target = readlink(Self.join(next)), !target.isEmpty else {
                    cur = next; continue
                }
                let refusal = Refused(message: "\(Self.linkRefusal): \(Self.join(next))")
                let np: [String]
                if target.hasPrefix("/") {
                    guard let n = inside(target) ?? inside(realTarget(target)) else { throw refusal }  // druga imena mape
                    np = n
                } else {
                    var c = cur
                    for t in target.split(separator: "/") where t != "." {  // cur nima povezav, zato je stikanje točno
                        if t == ".." { guard c.popLast() != nil else { throw refusal } } else { c.append(String(t)) }
                    }
                    np = c
                }
                comps = np + rest; rewrites += 1; again = true
                break
            }
            if !again { return Self.join(comps) }
        }
        return "."
    }

    /// Absolutni cilj (leksikalno) relativno na ime korena, v katerem leži; nil, če je zunaj. APFS ne loči velikosti črk.
    private func inside(_ target: String) -> [String]? {
        let t = Self.lexical(target), tl = t.map { $0.lowercased() }
        for b in bases where t.count >= b.count && tl[..<b.count].elementsEqual(b.map { $0.lowercased() }) {
            return Array(t[b.count...])
        }
        return nil
    }

    /// Cilj z razrešenim obstoječim delom (povezava /var/x, mapa odprta kot /private/var/x). Le izbere pot za poskus.
    private func realTarget(_ target: String) -> String {
        var p = "/" + Self.lexical(target).joined(separator: "/"), rest: [String] = []
        while true {
            if let r = realpath(p, nil) {
                defer { free(r) }
                return ([String(cString: r)] + rest).joined(separator: "/")
            }
            if p == "/" { return target }
            rest.insert((p as NSString).lastPathComponent, at: 0)
            p = (p as NSString).deletingLastPathComponent
        }
    }

    // MARK: branje

    func lstat(_ rel: String, _ err: inout Int32) -> stat? {
        var st = stat()
        if fstatat(fd, rel, &st, AT_SYMLINK_NOFOLLOW_ANY) == 0 { return st }
        err = errno
        return nil
    }
    func lstat(_ rel: String) -> stat? { var e: Int32 = 0; return lstat(rel, &e) }

    func isDir(_ rel: String) -> Bool { (lstat(rel)?.st_mode ?? 0) & S_IFMT == S_IFDIR }

    func readlink(_ rel: String) -> String? {
        var buf = [CChar](repeating: 0, count: Int(PATH_MAX) + 1)
        let n = readlinkat(fd, rel, &buf, Int(PATH_MAX))
        return n >= 0 ? String(decoding: buf[..<n].map { UInt8(bitPattern: $0) }, as: UTF8.self) : nil
    }

    /// Navadna datoteka za branje (brez čakanja na FIFO); povezava kjerkoli v poti = nil.
    func openRead(_ rel: String) -> Int32? {
        let f = openat(fd, rel, O_RDONLY | O_NONBLOCK | O_NOFOLLOW_ANY | O_CLOEXEC)
        guard f >= 0 else { return nil }
        var st = stat()
        guard fstat(f, &st) == 0, st.st_mode & S_IFMT == S_IFREG else { close(f); return nil }
        return f
    }

    func list(_ rel: String) -> [String]? {
        let d = openat(fd, rel, O_RDONLY | O_DIRECTORY | O_NOFOLLOW_ANY | O_CLOEXEC)
        guard d >= 0 else { return nil }
        return Self.names(d)
    }

    /// Imena v mapi (zapre `d`).
    private static func names(_ d: Int32) -> [String]? {
        guard let dir = fdopendir(d) else { close(d); return nil }
        defer { closedir(dir) }
        var out: [String] = []
        while let e = readdir(dir) {
            let n = withUnsafeBytes(of: e.pointee.d_name) { String(decoding: $0.prefix(Int(e.pointee.d_namlen)), as: UTF8.self) }
            if n != "." && n != ".." { out.append(n) }
        }
        return out
    }

    /// `count` bajtov od odmika `start`; nil = ni navadna datoteka ali napaka.
    func readRange(_ rel: String, start: Int, count: Int) -> Data? {
        guard let f = openRead(rel) else { return nil }
        defer { close(f) }
        var out = Data(count: count), got = 0
        while got < count {
            let r = out.withUnsafeMutableBytes { pread(f, $0.baseAddress! + got, count - got, off_t(start + got)) }
            if r < 0 { return nil }
            if r == 0 { break }
            got += r
        }
        return out.prefix(got)
    }

    // MARK: pisanje (vse prek pripete nadrejene mape)

    /// Nadrejena mapa `rel`, odprta brez povezav, in ime. Preveri še pot odprte mape glede na koren: zaščitena pot
    /// (.git, .claude/settings*.json, …) ali mapa zunaj korena = nil (kot Go mutationParent). Klicatelj zapre deskriptor.
    func parent(_ rel: String) -> (fd: Int32, name: String)? {
        let c = Self.comps(rel)
        guard let name = c.last, !c.contains(".."), !Sync.isProtected(c.joined(separator: "/")) else { return nil }
        let p = openat(fd, Self.join(Array(c.dropLast())), O_RDONLY | O_DIRECTORY | O_NOFOLLOW_ANY | O_CLOEXEC)
        guard p >= 0 else { return nil }
        guard let pp = Self.path(of: p), pp == path || pp.hasPrefix(path + "/"),
              !Sync.isProtected(pp == path ? name : String(pp.dropFirst(path.count + 1)) + "/" + name)
        else { close(p); return nil }
        return (p, name)
    }

    /// 0 = uspelo, sicer errno.
    func mkdir(_ rel: String, _ mode: mode_t = 0o755) -> Int32 {
        guard let (p, name) = parent(rel) else { return EACCES }
        defer { close(p) }
        return mkdirat(p, name, mode) == 0 ? 0 : errno
    }

    func mkdirAll(_ rel: String) -> Bool {
        var cur: [String] = []
        for c in Self.comps(rel) {
            cur.append(c)
            let e = mkdir(cur.joined(separator: "/"))
            if e != 0 && !(e == EEXIST && isDir(cur.joined(separator: "/"))) { return false }
        }
        return true
    }

    /// Datoteka ali prazna mapa (povezava sama, ne cilj).
    @discardableResult func remove(_ rel: String) -> Bool {
        guard let (p, name) = parent(rel) else { return false }
        defer { close(p) }
        return Self.unlink(p, name, dir: nil)
    }

    private static func unlink(_ p: Int32, _ name: String, dir: Bool?) -> Bool {
        var st = stat()
        let isDir = dir ?? (fstatat(p, name, &st, AT_SYMLINK_NOFOLLOW) == 0 && st.st_mode & S_IFMT == S_IFDIR)
        return unlinkat(p, name, isDir ? AT_REMOVEDIR : 0) == 0
    }

    /// Drevo (povezav ne sledi, odstrani jih same); zaščiteno drevo = false.
    func removeAll(_ rel: String) -> Bool {
        guard let (p, name) = parent(rel), !Self.treeProtected(p, name, [name]) else { return false }
        defer { close(p) }
        return Self.removeTree(p, name)
    }

    private static func removeTree(_ p: Int32, _ name: String) -> Bool {
        var st = stat()
        guard fstatat(p, name, &st, AT_SYMLINK_NOFOLLOW) == 0 else { return errno == ENOENT }
        if st.st_mode & S_IFMT == S_IFDIR {
            let d = openat(p, name, O_RDONLY | O_DIRECTORY | O_NOFOLLOW | O_CLOEXEC)
            guard d >= 0, let kids = names(d) else { return false }  // names zapre d
            let sub = openat(p, name, O_RDONLY | O_DIRECTORY | O_NOFOLLOW | O_CLOEXEC)
            guard sub >= 0 else { return false }
            defer { close(sub) }
            for k in kids where !removeTree(sub, k) { return false }
            return unlinkat(p, name, AT_REMOVEDIR) == 0
        }
        return unlinkat(p, name, 0) == 0
    }

    /// Ali je katera koli pot v drevesu `name` (pod `p`; z imenom `base`) zaščitena; povezavi (ni mapa) se ne sledi,
    /// nečitljivo = zaščiteno (kot Go rootTreeProtected).
    private static func treeProtected(_ p: Int32, _ name: String, _ base: [String]) -> Bool {
        var st = stat()
        guard fstatat(p, name, &st, AT_SYMLINK_NOFOLLOW) == 0 else { return false }
        let b = base.joined(separator: "/")
        if st.st_mode & S_IFMT != S_IFDIR { return Sync.isProtected(b) }
        if Sync.isProtected(b) { return true }
        let d = openat(p, name, O_RDONLY | O_DIRECTORY | O_NOFOLLOW | O_CLOEXEC)
        guard d >= 0 else { return true }
        defer { close(d) }
        guard let kids = names(dup(d)) else { return true }
        for k in kids where treeProtected(d, k, base + [k]) { return true }
        return false
    }

    /// Kot Go rootTreeProtected(rooted, rel, base): `rel` v mapi, `base` = ime(na), pod katerim bi drevo ležalo.
    func treeProtected(_ rel: String, base: [String]) -> Bool {
        let c = Self.comps(rel)
        let p = openat(fd, Self.join(Array(c.dropLast())), O_RDONLY | O_DIRECTORY | O_NOFOLLOW_ANY | O_CLOEXEC)
        guard p >= 0 else { return true }
        defer { close(p) }
        return Self.treeProtected(p, c.last ?? ".", base)
    }

    /// Preimenuje (zaščiteno drevo ni dovoljeno ne kot vir ne kot cilj).
    func rename(_ src: String, _ dst: String) -> Bool {
        guard let (sp, sn) = parent(src) else { return false }
        defer { close(sp) }
        guard let (dp, dn) = parent(dst) else { return false }
        defer { close(dp) }
        guard !Self.treeProtected(sp, sn, [sn]), !Self.treeProtected(sp, sn, [dn]) else { return false }
        return renameat(sp, sn, dp, dn) == 0
    }

    /// Nova datoteka (O_EXCL) ali odprtje za pisanje prek pripete nadrejene mape; končna povezava ne preusmeri pisanja.
    func create(_ rel: String, flags: Int32 = O_CREAT | O_EXCL | O_WRONLY, mode: mode_t = 0o644) -> Int32? {
        guard let (p, name) = parent(rel) else { return nil }
        defer { close(p) }
        let f = openat(p, name, flags | O_NOFOLLOW | O_CLOEXEC, mode)
        return f >= 0 ? f : nil
    }

    /// Atomarno zapiše datoteko prek začasnega imena v pripeti nadrejeni mapi. `mode` nil = pravice ostanejo 0600;
    /// `mtime` (ms) nil = čas zapisa. Prazna mapa na mestu datoteke se odstrani, polna ostane in pisanje ne uspe.
    /// 0 = uspelo, sicer errno.
    func writeAtomic(_ rel: String, _ data: Data, mode: mode_t?, mtime: Int64?) -> Int32 {
        guard let (p, name) = parent(rel) else { return EACCES }
        defer { close(p) }
        var st = stat()
        if fstatat(p, name, &st, AT_SYMLINK_NOFOLLOW) == 0, st.st_mode & S_IFMT == S_IFDIR { _ = unlinkat(p, name, AT_REMOVEDIR) }
        let tmp = ".minigw-" + UUID().uuidString
        let f = openat(p, tmp, O_CREAT | O_EXCL | O_WRONLY | O_NOFOLLOW | O_CLOEXEC, 0o600)
        guard f >= 0 else { return errno }
        var err: Int32 = 0
        data.withUnsafeBytes { buf in
            var off = 0
            while off < buf.count, err == 0 {
                let w = write(f, buf.baseAddress! + off, buf.count - off)
                if w < 0 { err = errno } else { off += w }
            }
        }
        if err == 0, let mode, fchmod(f, mode) != 0 { err = errno }
        if err == 0, let mtime {
            let ts = timespec(tv_sec: Int(mtime / 1000), tv_nsec: Int(mtime % 1000) * 1_000_000)
            if futimens(f, [ts, ts]) != 0 { err = errno }
        }
        if close(f) != 0, err == 0 { err = errno }
        if err == 0, renameat(p, tmp, p, name) != 0 { err = errno }
        if err != 0 { unlinkat(p, tmp, 0) }
        return err
    }

    /// Spremeni pravice navadne datoteke (najprej odpre, nato fchmod; kot Go rootChmod).
    @discardableResult func chmod(_ rel: String, _ mode: mode_t) -> Bool {
        guard let (p, name) = parent(rel) else { return false }
        defer { close(p) }
        let f = openat(p, name, O_RDONLY | O_NONBLOCK | O_NOFOLLOW | O_CLOEXEC)
        guard f >= 0 else { return false }
        defer { close(f) }
        var st = stat()
        return fstat(f, &st) == 0 && st.st_mode & S_IFMT == S_IFREG && fchmod(f, mode) == 0
    }

    /// COPY: navadne datoteke in mape; povezave (tudi ugnezdene) in drugo izpusti (kot Go rootCopyTree).
    func copyTree(_ src: String, _ dst: String) -> Bool {
        guard let st = lstat(src) else { return false }
        switch st.st_mode & S_IFMT {
        case S_IFREG:
            guard let i = openRead(src) else { return false }
            defer { close(i) }
            guard let o = create(dst, mode: st.st_mode & 0o777) else { return false }
            defer { close(o) }
            var buf = [UInt8](repeating: 0, count: 1 << 16)
            while true {
                let r = Darwin.read(i, &buf, buf.count)
                if r < 0 { return false }
                if r == 0 { return true }
                var off = 0
                while off < r {
                    let w = buf.withUnsafeBytes { Darwin.write(o, $0.baseAddress! + off, r - off) }
                    if w < 0 { return false }
                    off += w
                }
            }
        case S_IFDIR:
            guard mkdir(dst, st.st_mode & 0o777) == 0, let kids = list(src) else { return false }
            for k in kids where !copyTree(src + "/" + k, dst + "/" + k) { return false }
            return true
        default:
            return true
        }
    }
}
