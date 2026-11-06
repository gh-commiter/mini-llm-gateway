import Foundation

/// WebDAV zahteva priklopa, ki jo minigw izvede v svoji mapi.
public struct DAVJob: Codable, Sendable {
    public var id: String
    public var method: String
    public var path: String
    public var prefix: String
    public var headers: [String: String]
    public var body: Data
}

public struct DAVReply: Codable, Sendable {
    public var id: String
    public var response: DAVResponse
    public init(id: String, response: DAVResponse) { self.id = id; self.response = response }
}

/// Mape odjemalcev (minigw na drugem Macu) za `claude -p`.
/// Mac mini odjemalca ne doseže, zato minigw sam sprašuje po zahtevah (long poll) in vrača odgovore;
/// mapa je na Mac miniju priklopljena z vgrajenim mount_webdav na skrito pot `/dav/<skrivnost>` tega routerja.
public actor ShareHub {
    final class Share {
        let session: String
        let key = UUID().uuidString.lowercased()      // minigw: poll/reply
        let secret = UUID().uuidString.lowercased()   // pot priklopa (brez žetona; le lokalno)
        let dir = UUID().uuidString.lowercased()      // ime mape priklopa (nikoli iz glave odjemalca)
        let cwd: String                               // mapa na odjemalcu
        let readOnly: Bool                            // minigw --minigw-ro: odjemalec zavrne pisanje, priklop rdonly
        var jobs: [DAVJob] = []
        var pollers: [String: CheckedContinuation<DAVJob?, Never>] = [:]
        var replies: [String: CheckedContinuation<DAVResponse?, Never>] = [:]
        var lastSeen = Date()
        var mountPoint: URL?
        var mounting: Task<URL?, Never>?
        init(session: String, cwd: String, readOnly: Bool) { self.session = session; self.cwd = cwd; self.readOnly = readOnly }
    }

    private var bySession: [String: Share] = [:]
    private var reaper: Task<Void, Never>?
    static let mountsDir = FileManager.default.urls(for: .applicationSupportDirectory, in: .userDomainMask)[0]
        .appendingPathComponent("Mini Gateway/mounts", isDirectory: true)

    private func share(key: String) -> Share? { bySession.values.first { $0.key == key } }

    /// nil: seja že ima share drugega lastnika (brez njegovega ključa je ni mogoče prevzeti).
    func register(session: String, cwd: String, key: String?, readOnly: Bool = false) -> String? {
        if let s = bySession[session] {
            guard s.key == key else { return nil }
            if s.cwd == cwd, s.readOnly == readOnly { s.lastSeen = Date(); return s.key }
            bySession[session] = nil
            close(s)
        }
        let s = Share(session: session, cwd: cwd, readOnly: readOnly)
        bySession[session] = s
        if reaper == nil {
            reaper = Task { [weak self] in
                while !Task.isCancelled {
                    try? await Task.sleep(for: .seconds(15))
                    await self?.reap()
                }
            }
        }
        return s.key
    }

    /// Mapa seje je deljena samo za branje.
    func isReadOnly(session: String) -> Bool { bySession[session]?.readOnly ?? false }

    func unregister(key: String) {
        guard let s = share(key: key) else { return }
        bySession[s.session] = nil
        close(s)
    }

    /// minigw ni več spraševal (zaprt prenosnik, prekinjen VPN) → odklopi.
    private func reap() {
        for s in bySession.values where Date().timeIntervalSince(s.lastSeen) > 60 {  // minigw sprašuje vsaj vsakih 25 s
            bySession[s.session] = nil
            close(s)
        }
    }

    private func close(_ s: Share) {
        for (_, k) in s.pollers { k.resume(returning: nil) }
        for (_, k) in s.replies { k.resume(returning: nil) }
        s.pollers = [:]; s.replies = [:]
        if let mp = s.mountPoint { Task.detached { Self.unmount(mp) } }  // zunaj akterja: umount še kliče dav()
    }

    // MARK: minigw

    /// Naslednja zahteva ali nil po `wait` sekundah; nil tudi, če share ne obstaja (→ 404, minigw se registrira znova).
    func poll(key: String, wait: Double = 25) async -> DAVJob?? {
        guard let s = share(key: key) else { return .none }
        s.lastSeen = Date()
        if !s.jobs.isEmpty { return .some(s.jobs.removeFirst()) }
        let id = UUID().uuidString
        let job: DAVJob? = await withCheckedContinuation { k in
            s.pollers[id] = k
            Task { [weak self] in
                try? await Task.sleep(for: .seconds(wait))
                await self?.timeoutPoll(s, id)
            }
        }
        s.lastSeen = Date()
        return .some(job)
    }

    private func timeoutPoll(_ s: Share, _ id: String) { s.pollers.removeValue(forKey: id)?.resume(returning: nil) }

    func reply(key: String, _ r: DAVReply) -> Bool {
        guard let s = share(key: key) else { return false }
        s.lastSeen = Date()
        s.replies.removeValue(forKey: r.id)?.resume(returning: r.response)
        return true
    }

    // MARK: mount_webdav (lokalno)

    func dav(secret: String, method: String, path: String, headers: [String: String], body: Data) async -> DAVResponse? {
        guard let s = bySession.values.first(where: { $0.secret == secret }) else { return nil }
        let job = DAVJob(id: UUID().uuidString, method: method, path: path, prefix: "/dav/" + secret, headers: headers, body: body)
        // Brez odgovora (minigw se je ustavil ali ni odgovoril v 120 s): 504, ne 404 (datoteka morda obstaja).
        let r: DAVResponse? = await withCheckedContinuation { k in
            s.replies[job.id] = k
            if let (pid, p) = s.pollers.first { s.pollers[pid] = nil; p.resume(returning: job) } else { s.jobs.append(job) }
            Task { [weak self] in
                try? await Task.sleep(for: .seconds(120))
                await self?.timeoutReply(s, job.id)
            }
        }
        return r ?? DAVResponse(504)
    }

    private func timeoutReply(_ s: Share, _ id: String) {
        s.jobs.removeAll { $0.id == id }
        s.replies.removeValue(forKey: id)?.resume(returning: nil)
    }

    /// Mapa seje na tem Macu (priklopi ob prvi uporabi); nil, če seja nima mape odjemalca.
    /// `wait`: odjemalec deli mapo (minigw), a je po ponovnem zagonu routerja še ni znova registriral → počakaj do 30 s.
    func mount(session: String, cwd: String?, base: String, wait: Bool = false) async -> URL? {
        if wait, !session.isEmpty {
            for _ in 0..<60 where bySession[session] == nil { try? await Task.sleep(for: .milliseconds(500)) }
        }
        guard let s = bySession[session], cwd == nil || cwd == s.cwd else { return nil }
        if let mp = s.mountPoint, Self.isMounted(mp) { return mp }
        if let t = s.mounting { return await t.value }
        let mp = Self.mountsDir.appendingPathComponent(s.dir, isDirectory: true)
        let url = "\(base)/dav/\(s.secret)/", name = URL(fileURLWithPath: s.cwd).lastPathComponent
        let options = s.readOnly ? ["-o", "rdonly"] : []
        // mount_webdav med priklopom že pošilja zahteve v dav() → ne sme teči v akterju.
        let t = Task.detached { () -> URL? in
            if ShareHub.isMounted(mp) { ShareHub.unmount(mp) }  // ostanek prejšnjega zagona
            try? FileManager.default.createDirectory(at: ShareHub.mountsDir, withIntermediateDirectories: true,
                                                     attributes: [.posixPermissions: 0o700])
            try? FileManager.default.createDirectory(at: mp, withIntermediateDirectories: false)
            guard ShareHub.run("/sbin/mount_webdav", ["-S", "-v", name] + options + [url, mp.path]) == 0 else {
                Router.log.error("mount_webdav failed for session \(session, privacy: .public)")
                rmdir(mp.path)
                return nil
            }
            return mp
        }
        s.mounting = t
        let result = await t.value
        s.mounting = nil
        if bySession[session] === s { s.mountPoint = result } else if let result { Task.detached { Self.unmount(result) } }
        return result
    }

    /// Ob zagonu aplikacije: priklopi prejšnjega zagona kažejo na skrivnosti, ki jih router ne pozna več → odklopi.
    public static func cleanupStale() {
        let dirs = (try? FileManager.default.contentsOfDirectory(at: mountsDir, includingPropertiesForKeys: nil)) ?? []
        for d in dirs { if isMounted(d) { unmount(d) } else { rmdir(d.path) } }
    }

    static func isMounted(_ url: URL) -> Bool {
        var st = statfs()
        return statfs(url.path, &st) == 0 && withUnsafeBytes(of: st.f_mntonname) {
            String(decoding: $0.prefix { $0 != 0 }, as: UTF8.self) } == url.path
    }

    static func unmount(_ url: URL) {
        if run("/sbin/umount", [url.path]) != 0 { _ = run("/usr/sbin/diskutil", ["unmount", "force", url.path]) }
        rmdir(url.path)  // le prazna mapa; nikoli rekurzivno (če odklop ne uspe, je to mapa odjemalca)
    }

    @discardableResult
    static func run(_ exe: String, _ args: [String]) -> Int32 {
        let p = Process()
        p.executableURL = URL(fileURLWithPath: exe)
        p.arguments = args
        p.standardOutput = FileHandle.nullDevice
        p.standardError = FileHandle.nullDevice
        do { try p.run() } catch { return -1 }
        p.waitUntilExit()
        return p.terminationStatus
    }
}
