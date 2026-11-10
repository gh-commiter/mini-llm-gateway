import Foundation
import os

/// N4, odločitvi 2 in 3 (0.5.24): vsaka instanca prehoda ima svoj Scratch <osnova>/<instanca>. Datoteka lastnika v njem
/// hrani ID instance (RouterConfig.instanceID); mape druge instance prehod ne uporablja (brez agentov, brez čiščenja),
/// dokler je lastnik izrecno ne prevzame (Take over). Aplikacija, ki teče, drži flock na lockFile. Ista pravila kot
/// serve_scratchowner.go v minigw serve.
public enum ScratchOwner {
    public static let ownerFile = ".minigw-owner", lockFile = ".minigw-lock"
    public static let inUse = "in-use", stale = "stale"
    /// Ne štejejo kot pogovori, ko je na poti stara mapa te instance (odločitev 2).
    static let notConversations: Set<String> = [ownerFile, lockFile, "tmp", "mxc", "gitexec", "prompts", "cache", "caches", "deriveddata"]
    private static let held = OSAllocatedUnfairLock<Int32>(initialState: -1)
    /// Scratch je mapa drugega prehoda: Router zavrne zahteve, AppState ne čisti.
    public static let blocked = OSAllocatedUnfairLock(initialState: false)
    static let log = Logger(subsystem: "dev.trojar.minigateway", category: "scratch")

    public static func newID() -> String {
        "gw-" + (0..<6).map { _ in String(format: "%02x", UInt8.random(in: 0...255)) }.joined()
    }

    /// "" = ni datoteke lastnika. Neberljiva, ne navadna datoteka ali povezava: unknown (tuja, zaprto). O_NONBLOCK:
    /// podtaknjena cev ne ustavi GET /general in Settings.
    public static func owner(_ dir: URL) -> String {
        let fd = open(dir.appendingPathComponent(ownerFile).path, O_RDONLY | O_NONBLOCK | O_NOFOLLOW | O_CLOEXEC)
        if fd < 0 { return errno == ENOENT ? "" : unknown }
        defer { close(fd) }
        var st = stat()
        guard fstat(fd, &st) == 0, st.st_mode & S_IFMT == S_IFREG else { return unknown }
        var buf = [UInt8](repeating: 0, count: 256)
        let n = read(fd, &buf, buf.count)
        guard n >= 0 else { return unknown }
        return String(decoding: buf[0..<n], as: UTF8.self).trimmingCharacters(in: .whitespacesAndNewlines)
    }
    static let unknown = "?"

    /// Prava mapa (ne povezava) tega uporabnika.
    static func mine(_ dir: URL) -> Bool {
        var st = stat()
        return lstat(dir.path, &st) == 0 && st.st_mode & S_IFMT == S_IFDIR && st.st_uid == getuid()
    }

    /// "" = mapa te instance (ali še brez lastnika), sicer inUse (drug prehod teče) ali stale.
    public static func state(_ dir: URL, id: String) -> String {
        let o = owner(dir)
        if o.isEmpty || o == id { return "" }
        return locked(dir) ? inUse : stale
    }

    /// Zaklep mape drži drug proces (aplikacija ali minigw serve, ki teče).
    static func locked(_ dir: URL) -> Bool {
        let fd = open(dir.appendingPathComponent(lockFile).path, O_RDONLY | O_NONBLOCK | O_NOFOLLOW | O_CLOEXEC)
        guard fd >= 0 else { return errno != ENOENT }  // zaprto: ni mogoče preveriti
        defer { close(fd) }
        var st = stat()
        guard fstat(fd, &st) == 0, st.st_mode & S_IFMT == S_IFREG else { return true }
        if flock(fd, LOCK_EX | LOCK_NB) == 0 { flock(fd, LOCK_UN); return false }
        return true
    }

    /// Ob zagonu in ob vsaki spremembi nastavitev: prva raba zapiše lastnika (O_EXCL: od dveh hkratnih zagonov zmaga
    /// eden), mapa te instance dobi zaklep, dokler aplikacija teče. Vrne state(); nastavi blocked.
    @discardableResult public static func claim(_ dir: URL, id: String) -> String {
        held.withLock { if $0 >= 0 { close($0); $0 = -1 } }  // flock velja za odprto datoteko: tudi naša bi zaklep zavrnila
        try? FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
        // prva raba (tudi obstoječa mapa pred 0.5.24): le prava mapa tega uporabnika, ki je ne drži drug proces
        let fresh = owner(dir).isEmpty && mine(dir) && !locked(dir)
        let fd = fresh ? open(dir.appendingPathComponent(ownerFile).path, O_WRONLY | O_CREAT | O_EXCL | O_NOFOLLOW | O_CLOEXEC, 0o600) : -1
        if fd >= 0 {
            let line = Array((id + "\n").utf8)
            let ok = write(fd, line, line.count) == line.count
            close(fd)
            if !ok { unlink(dir.appendingPathComponent(ownerFile).path) }
        }
        var st = state(dir, id: id)
        if st.isEmpty, owner(dir) != id, (try? FileManager.default.attributesOfItem(atPath: dir.path)) != nil {
            st = locked(dir) ? inUse : stale  // brez lastnika in ni naša ali jo drži drug proces
        }
        if st.isEmpty, owner(dir) == id {
            let lfd = open(dir.appendingPathComponent(lockFile).path, O_RDWR | O_CREAT | O_NONBLOCK | O_NOFOLLOW | O_CLOEXEC, 0o600)
            var fs = stat()
            if lfd >= 0, fstat(lfd, &fs) == 0, fs.st_mode & S_IFMT == S_IFREG, flock(lfd, LOCK_EX | LOCK_NB) == 0 {
                held.withLock { $0 = lfd }
            } else {
                let e = errno
                if lfd >= 0 { close(lfd) }
                if e == EWOULDBLOCK { st = inUse } else {  // npr. omrežni disk brez flock: brez zaklepa, opozorilo
                    log.warning("scratch folder \(dir.path, privacy: .public): no lock (\(String(cString: strerror(e)), privacy: .public)): a Scratch base shared by more than one computer is not supported")
                }
            }
        }
        let foreign = !st.isEmpty
        blocked.withLock { $0 = foreign }
        if !st.isEmpty { log.warning("\(message(dir, st), privacy: .public)") }
        return st
    }

    public static func message(_ dir: URL, _ st: String) -> String {
        let who = st == inUse ? "another gateway that is running" : "another gateway that is not running (stale owner)"
        return "the Scratch folder \(dir.path) belongs to \(who): agents are off and nothing in it is cleaned up. "
            + "Choose a Scratch folder of this gateway (Settings → General → Scratch folder → Choose…), or, if that gateway is gone, Take over"
    }

    /// Kaj drugega kot notConversations z vsaj eno datoteko (napaka branja šteje: raje ob strani kot izbrisano).
    public static func hasConversations(_ dir: URL) -> Bool {
        let fm = FileManager.default
        guard let names = try? fm.contentsOfDirectory(atPath: dir.path) else { return true }
        return names.contains { n in
            guard !notConversations.contains(n.lowercased()) else { return false }
            let p = dir.appendingPathComponent(n).path
            guard let t = try? fm.attributesOfItem(atPath: p)[.type] as? FileAttributeType else { return true }
            guard t == .typeDirectory, let e = fm.enumerator(atPath: p) else { return t != .typeDirectory }
            while e.nextObject() != nil { if e.fileAttributes?[.type] as? FileAttributeType != .typeDirectory { return true } }
            return false
        }
    }

    /// Odločitev 2 (prevzem = preimenovanje, nikoli združevanje): stara mapa druge instance (old) postane <osnova>/<id> z
    /// ID-jem te instance. Trenutna mapa te instance (current, tudi sosednja) in mapa <osnova>/<id>, ki je na poti: brez
    /// pogovorov izbrisani, s pogovori ob strani kot <id>.old-<datum>. Zaklep drugega prehoda: napaka. Zapisano v dnevnik.
    @discardableResult public static func takeOver(_ old: URL, current: URL? = nil, id: String, now: Date = Date()) throws -> (folder: URL, aside: URL?) {
        guard !id.isEmpty else { throw error("this gateway has no instance ID yet") }
        guard !locked(old) else { throw error("the folder is used by another gateway that is running: stop it first") }
        guard mine(old) else { throw error("the folder is not a real folder of this user") }
        let fm = FileManager.default
        let dst = old.deletingLastPathComponent().appendingPathComponent(id, isDirectory: true)
        var aside: URL?, moved: [(URL, URL)] = []
        func undo(_ e: Error) -> Error {
            for (from, to) in moved.reversed() { try? fm.moveItem(at: to, to: from) }
            return e
        }
        for p in [current, dst].compactMap({ $0 }) where p.standardizedFileURL.path != old.standardizedFileURL.path {
            guard (try? fm.attributesOfItem(atPath: p.path)) != nil else { continue }
            do {
                if !hasConversations(p) { try fm.removeItem(at: p); continue }
                let f = DateFormatter()
                f.locale = Locale(identifier: "en_US_POSIX")
                f.dateFormat = "yyyy-MM-dd"
                var a = dst.path + ".old-" + f.string(from: now)
                if fm.fileExists(atPath: a) { f.dateFormat = "-HHmmss"; a += f.string(from: now) }
                try fm.moveItem(atPath: p.path, toPath: a)
                aside = URL(fileURLWithPath: a, isDirectory: true)
                moved.append((p, aside!))
            } catch { throw undo(error) }
        }
        if old.lastPathComponent != id {
            do { try fm.moveItem(at: old, to: dst); moved.append((old, dst)) } catch { throw undo(error) }
        }
        do {  // zaščita (0700) in lastnik; napaka: vse nazaj
            guard chmod(dst.path, 0o700) == 0, mine(dst) else { throw error("\(dst.path): cannot make it private to this user") }
            try Data((id + "\n").utf8).write(to: dst.appendingPathComponent(ownerFile), options: .atomic)
        } catch { throw undo(error) }
        log.notice("scratch folder \(old.path, privacy: .public) taken over from another gateway: now \(dst.path, privacy: .public)\(aside.map { "; this gateway's previous folder is kept as " + $0.path } ?? "", privacy: .public)")
        return (dst, aside)
    }

    /// Prave mape v osnovi trenutnega Scratch z datoteko lastnika druge instance (brez *.old-*, brez trenutne mape):
    /// (ime, inUse ali stale). Mape brez datoteke lastnika ne štejejo.
    public static func siblings(_ current: URL, id: String) -> [(name: String, state: String)] {
        let fm = FileManager.default, base = current.deletingLastPathComponent()
        return ((try? fm.contentsOfDirectory(atPath: base.path)) ?? []).sorted().compactMap { n in
            let p = base.appendingPathComponent(n, isDirectory: true)
            guard n != current.lastPathComponent, !n.contains(".old-"), mine(p), !owner(p).isEmpty else { return nil }
            let st = state(p, id: id)
            return st.isEmpty ? nil : (n, st)
        }
    }

    public struct TakeError: Error { public let status: Int; public let message: String }

    /// Take over (skrbniški API in Settings, znotraj iste zaporedne spremembe nastavitev): name = sosednja mapa v osnovi,
    /// nil = mapa, ki jo imenuje nastavitev. Vrne mapo ob strani (nil = ni je).
    public static func take(_ c: inout RouterConfig, name: String?) throws -> URL? {
        let cur = c.scratchURL
        var old = cur, st = ""
        if let name, !name.isEmpty {  // le neposredna podmapa osnove, prava mapa druge instance
            guard !name.contains("/"), !name.contains(":"), name != ".", name != ".." else {
                throw TakeError(status: 400, message: "name: a folder name in the Scratch base, without path separators")
            }
            guard !c.instanceID.isEmpty, let s = siblings(cur, id: c.instanceID).first(where: { $0.name == name }) else {
                throw TakeError(status: 400, message: "name: \(name) is not another gateway's Scratch folder in \(cur.deletingLastPathComponent().path)")
            }
            old = cur.deletingLastPathComponent().appendingPathComponent(name, isDirectory: true)
            st = s.state
        } else {
            st = c.instanceID.isEmpty ? "" : state(cur, id: c.instanceID)
        }
        if st.isEmpty { throw TakeError(status: 409, message: "the Scratch folder \(old.path) is this gateway's own: nothing to take over") }
        if st == inUse { throw TakeError(status: 409, message: "the Scratch folder \(old.path) is used by another gateway that is running: stop it first, or choose another folder") }
        do {
            let r = try takeOver(old, current: old == cur ? nil : cur, id: c.instanceID)
            c.scratchName = c.instanceID
            return r.aside
        } catch {
            throw TakeError(status: 500, message: "scratch folder not taken over (it stays \(old.path)): \(error.localizedDescription)")
        }
    }

    static func error(_ s: String) -> Error { CocoaError(.fileWriteNoPermission, userInfo: [NSLocalizedDescriptionKey: s]) }
}
