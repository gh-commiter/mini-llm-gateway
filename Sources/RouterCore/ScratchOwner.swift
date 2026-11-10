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

    public static func owner(_ dir: URL) -> String {
        ((try? String(contentsOf: dir.appendingPathComponent(ownerFile), encoding: .utf8)) ?? "").trimmingCharacters(in: .whitespacesAndNewlines)
    }

    /// "" = mapa te instance (ali še brez lastnika), sicer inUse (drug prehod teče) ali stale.
    public static func state(_ dir: URL, id: String) -> String {
        let o = owner(dir)
        if o.isEmpty || o == id { return "" }
        return locked(dir) ? inUse : stale
    }

    /// Zaklep mape drži drug proces (aplikacija ali minigw serve, ki teče).
    static func locked(_ dir: URL) -> Bool {
        let fd = open(dir.appendingPathComponent(lockFile).path, O_RDONLY | O_NOFOLLOW | O_CLOEXEC)
        guard fd >= 0 else { return false }
        defer { close(fd) }
        if flock(fd, LOCK_EX | LOCK_NB) == 0 { flock(fd, LOCK_UN); return false }
        return true
    }

    /// Ob zagonu in ob vsaki spremembi nastavitev: prva raba zapiše lastnika (O_EXCL: od dveh hkratnih zagonov zmaga
    /// eden), mapa te instance dobi zaklep, dokler aplikacija teče. Vrne state(); nastavi blocked.
    @discardableResult public static func claim(_ dir: URL, id: String) -> String {
        held.withLock { if $0 >= 0 { close($0); $0 = -1 } }  // flock velja za odprto datoteko: tudi naša bi zaklep zavrnila
        try? FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
        let fd = open(dir.appendingPathComponent(ownerFile).path, O_WRONLY | O_CREAT | O_EXCL | O_NOFOLLOW | O_CLOEXEC, 0o600)
        if fd >= 0 {
            let line = Array((id + "\n").utf8)
            let ok = write(fd, line, line.count) == line.count
            close(fd)
            if !ok { unlink(dir.appendingPathComponent(ownerFile).path) }
        }
        var st = state(dir, id: id)
        if st.isEmpty, owner(dir) == id {
            let lfd = open(dir.appendingPathComponent(lockFile).path, O_RDWR | O_CREAT | O_NOFOLLOW | O_CLOEXEC, 0o600)
            if lfd >= 0, flock(lfd, LOCK_EX | LOCK_NB) == 0 {
                held.withLock { $0 = lfd }
            } else if lfd >= 0 {
                close(lfd)
                st = inUse
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

    /// Odločitev 2 (prevzem = preimenovanje, nikoli združevanje): stara mapa druge instance postane <osnova>/<id> z ID-jem
    /// te instance. Mapa te instance, ki je že tam: brez pogovorov izbrisana, s pogovori ob strani kot <id>.old-<datum>.
    /// Zaklep drugega prehoda: napaka. Zapisano v dnevnik.
    @discardableResult public static func takeOver(_ old: URL, id: String, now: Date = Date()) throws -> (folder: URL, aside: URL?) {
        guard !id.isEmpty else { throw error("this gateway has no instance ID yet") }
        guard !locked(old) else { throw error("the folder is used by another gateway that is running: stop it first") }
        let fm = FileManager.default
        let dst = old.deletingLastPathComponent().appendingPathComponent(id, isDirectory: true)
        var aside: URL?
        if old.lastPathComponent != id {
            if (try? fm.attributesOfItem(atPath: dst.path)) != nil {
                if hasConversations(dst) {
                    let f = DateFormatter()
                    f.locale = Locale(identifier: "en_US_POSIX")
                    f.dateFormat = "yyyy-MM-dd"
                    var a = dst.path + ".old-" + f.string(from: now)
                    if fm.fileExists(atPath: a) { f.dateFormat = "-HHmmss"; a += f.string(from: now) }
                    try fm.moveItem(atPath: dst.path, toPath: a)
                    aside = URL(fileURLWithPath: a, isDirectory: true)
                } else {
                    try fm.removeItem(at: dst)
                }
            }
            do { try fm.moveItem(at: old, to: dst) } catch {
                if let aside { try? fm.moveItem(at: aside, to: dst) }
                throw error
            }
        }
        try Data((id + "\n").utf8).write(to: dst.appendingPathComponent(ownerFile), options: .atomic)
        log.notice("scratch folder \(old.path, privacy: .public) taken over from another gateway: now \(dst.path, privacy: .public)\(aside.map { "; this gateway's previous folder is kept as " + $0.path } ?? "", privacy: .public)")
        return (dst, aside)
    }

    static func error(_ s: String) -> Error { CocoaError(.fileWriteNoPermission, userInfo: [NSLocalizedDescriptionKey: s]) }
}
