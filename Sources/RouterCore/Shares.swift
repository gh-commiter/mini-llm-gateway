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
        var props: [String: (DAVResponse, Date)] = [:]  // predpomnilnik PROPFIND (kot dir_refresh pri davfs2)
        var writeCount = 0                                // pisanje med čakajočim PROPFIND: odgovora ne shrani
        let copy: URL?                                    // sync: kopija na routerju namesto priklopa
        var state: [String: Sync.Entry]?                  // sync: kopija, kot jo ima odjemalec (prenos + potrjene spremembe)
        var incoming: [String: Sync.Entry] = [:]          // sync: zadnji manifest odjemalca (preverjanje prejetih datotek)
        var pending: [String: Sync.Entry?] = [:]          // sync: poslane spremembe agenta (nil = izbris) do potrditve
        var big: Set<String> = []                         // sync: že javljene prevelike datoteke
        var execKnown = true                              // sync: odjemalec pozna bit x (ne Windows)
        var pulling: Run?                                 // sync: izvajanje, katerega spremembe minigw prav zdaj prevzema
        let device: String?                               // naprava, ki je mapo registrirala (dokaz Noise); nil = lokalno
        init(session: String, cwd: String, readOnly: Bool, copy: URL?, device: String?) {
            self.session = session; self.cwd = cwd; self.readOnly = readOnly; self.copy = copy; self.device = device
        }
    }

    /// sync: eno izvajanje agenta v svojem klonu kopije (vzporedna izvajanja v isti mapi).
    final class Run {
        let share: Share, copy: URL, clone: URL
        let base: [String: Sync.Entry]   // stanje odjemalca ob klonu: od tod so spremembe za prevzem in različice »base«
        let start: [String: Sync.Entry]  // klon ob začetku: od tod so spremembe agenta, ki gredo nazaj v kopijo
        var merged = false
        init(share: Share, copy: URL, clone: URL, base: [String: Sync.Entry], start: [String: Sync.Entry]) {
            self.share = share; self.copy = copy; self.clone = clone; self.base = base; self.start = start
        }
    }

    /// ID izvajanja agenta (Router.syncRun): mount ga poveže s klonom, syncBack in finish ga najdeta.
    @TaskLocal public static var run: String?
    private var runs: [String: Run] = [:]

    private var bySession: [String: Share] = [:]
    private var reaper: Task<Void, Never>?
    static let mountsDir = FileManager.default.urls(for: .applicationSupportDirectory, in: .userDomainMask)[0]
        .appendingPathComponent("Mini Gateway/mounts", isDirectory: true)

    private func share(key: String) -> Share? { bySession.values.first { $0.key == key } }

    /// Share seje, le za napravo, ki ga je registrirala (druga naprava z istim ID seje ali ključem ga ne doseže).
    private func share(session: String, device: String?) -> Share? { bySession[session].flatMap { $0.device == device ? $0 : nil } }
    private func share(key: String, device: String?) -> Share? { share(key: key).flatMap { $0.device == device ? $0 : nil } }

    /// nil: seja že ima share drugega lastnika (brez njegovega ključa je ni mogoče prevzeti).
    /// `copy`: način sync (kopija v scratch/sync), sicer priklop.
    func register(session: String, cwd: String, key: String?, readOnly: Bool = false, copy: URL? = nil, device: String? = nil) -> String? {
        if let s = bySession[session] {
            guard s.key == key else { return nil }
            if s.cwd == cwd, s.readOnly == readOnly, s.copy == copy { s.lastSeen = Date(); return s.key }
            bySession[session] = nil
            close(s)
        }
        let s = Share(session: session, cwd: cwd, readOnly: readOnly, copy: copy, device: device)
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

    /// Seja dela v sinhronizirani kopiji (ne v priklopu).
    func isSync(session: String) -> Bool { bySession[session]?.copy != nil }

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
        // Vsak PROPFIND je pot router → odjemalec → nazaj; agenti (rg, find) jih pošljejo na tisoče.
        // ponytail: 10 s TTL in praznjenje ob pisanju prek priklopa; spremembe mimo priklopa vidne z do 10 s zamude.
        let cacheKey = method == "PROPFIND" ? [path, headers["depth"] ?? "", String(body.hashValue)].joined(separator: "|") : nil
        if Self.writes.contains(method) { s.props.removeAll(); s.writeCount += 1 }
        let writesBefore = s.writeCount
        if let cacheKey, let (r, t) = s.props[cacheKey], Date().timeIntervalSince(t) < Self.propsTTL { return r }
        let job = DAVJob(id: UUID().uuidString, method: method, path: path, prefix: "/dav/" + secret, headers: headers, body: body)
        // Brez odgovora (minigw se je ustavil ali ni odgovoril v 120 s): 504, ne 404 (datoteka morda obstaja).
        let r = await send(s, job, timeout: 120)
        if Self.writes.contains(method) { s.props.removeAll(); s.writeCount += 1 }  // tudi po odgovoru
        if let cacheKey, let r, r.status == 207 || r.status == 404, s.writeCount == writesBefore {
            if s.props.count > 5000 { s.props.removeAll() }
            s.props[cacheKey] = (r, Date())
        }
        return r ?? DAVResponse(504)
    }

    /// Zahteva za minigw (naslednji long poll); nil, če ne odgovori v `timeout` sekundah.
    private func send(_ s: Share, _ job: DAVJob, timeout: Double) async -> DAVResponse? {
        await withCheckedContinuation { k in
            s.replies[job.id] = k
            if let (pid, p) = s.pollers.first { s.pollers[pid] = nil; p.resume(returning: job) } else { s.jobs.append(job) }
            Task { [weak self] in
                try? await Task.sleep(for: .seconds(timeout))
                await self?.timeoutReply(s, job.id)
            }
        }
    }

    static let propsTTL: TimeInterval = 10
    static let writes: Set<String> = ["PUT", "DELETE", "MKCOL", "MOVE", "COPY", "PROPPATCH"]

    private func timeoutReply(_ s: Share, _ id: String) {
        s.jobs.removeAll { $0.id == id }
        s.replies.removeValue(forKey: id)?.resume(returning: nil)
    }

    /// Mapa seje na tem Macu (priklopi ob prvi uporabi); nil, če seja nima mape odjemalca.
    /// `wait`: odjemalec deli mapo (minigw), a je po ponovnem zagonu routerja še ni znova registriral → počakaj do 30 s.
    func mount(session: String, cwd: String?, base: String, wait: Bool = false, device: String? = nil) async -> URL? {
        if wait, !session.isEmpty {
            for _ in 0..<60 where bySession[session] == nil { try? await Task.sleep(for: .milliseconds(500)) }
        }
        guard let s = share(session: session, device: device), cwd == nil || cwd == s.cwd else { return nil }
        if let copy = s.copy {  // zaklep le za prenos; agent nato dela v svojem klonu (brez izvajanja, npr. Siri: v kopiji)
            await acquire(copy)
            var dir: URL?
            if !Task.isCancelled, await syncJob(s, "SYNC-PUSH") {
                if let id = Self.run { dir = await clone(s, copy, id) } else { dir = copy }
            }
            unlock(copy)
            return dir
        }
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

    // MARK: sync (kopija namesto priklopa; glej Sync)

    private var busy: [URL: [CheckedContinuation<Void, Never>]] = [:]

    /// En prenos, prevzem ali združitev naenkrat na kopijo; agenti medtem delajo vzporedno vsak v svojem klonu.
    private func acquire(_ copy: URL) async {
        if busy[copy] == nil { busy[copy] = [] } else { await withCheckedContinuation { busy[copy]!.append($0) } }
    }

    func unlock(_ copy: URL) {
        if var w = busy[copy], !w.isEmpty { let k = w.removeFirst(); busy[copy] = w; k.resume() } else { busy[copy] = nil }
    }

    /// Klon kopije za izvajanje `id` (APFS: takoj, brez dodatnega prostora) v scratch/sync/runs; kliče se pod zaklepom.
    /// Mesto ».<n>« je najnižje prosto: zaporedna izvajanja imajo isto pot (nadaljevanje seje Claude je vezano na mapo).
    private func clone(_ s: Share, _ copy: URL, _ id: String) async -> URL? {
        let dir = copy.deletingLastPathComponent().deletingLastPathComponent().appendingPathComponent("runs", isDirectory: true)
        let name = copy.deletingLastPathComponent().lastPathComponent + "-" + copy.lastPathComponent
        var n = 0
        while runs.values.contains(where: { $0.clone.lastPathComponent == "\(name).\(n)" }) { n += 1 }
        let clone = dir.appendingPathComponent("\(name).\(n)", isDirectory: true)
        let base = s.readOnly ? [:] : s.state ?? [:], readOnly = s.readOnly
        let start = await Task.detached { () -> [String: Sync.Entry]? in
            try? FileManager.default.removeItem(at: clone)  // ostanek prekinjenega zagona
            try? FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
            guard Self.clone(copy, to: clone) else { return nil }
            return readOnly ? [:] : Sync.scan(clone, previous: base).files
        }.value
        guard let start else { return nil }
        runs[id] = Run(share: s, copy: copy, clone: clone, base: base, start: start)
        return clone
    }

    static func clone(_ src: URL, to dst: URL) -> Bool {
        clonefile(src.path, dst.path, 0) == 0 || (try? FileManager.default.copyItem(at: src, to: dst)) != nil
    }

    /// Spremembe agenta v klonu nazaj v kopijo (pod zaklepom), da je naslednji prenos spet le razlika.
    /// Nepotrjene ostanejo v kopiji kot doslej (»kept«) in gredo z naslednjim prevzemom.
    /// ponytail: le sinhronizirane datoteke; prezrti izhod gradnje (.build, node_modules) ostane v klonu in izgine z njim,
    /// zato vsako izvajanje gradi od stanja kopije. Združi tudi te, če bo inkrementalna gradnja prepočasna.
    private func merge(_ r: Run) async {
        guard !r.merged, !r.share.readOnly else { return }
        r.merged = true
        let (start, clone, copy) = (r.start, r.clone, r.copy)
        await Task.detached {
            let fm = FileManager.default
            let ch = Sync.diff(old: start, new: Sync.scan(clone, previous: start).files, root: clone)
            for e in ch.changed {
                guard let src = Sync.resolve(e.path, root: clone), let dst = Sync.resolve(e.path, root: copy) else { continue }
                try? fm.removeItem(at: dst)
                try? fm.createDirectory(at: dst.deletingLastPathComponent(), withIntermediateDirectories: true)
                _ = Self.clone(src, to: dst)
            }
            for p in ch.deleted { if let dst = Sync.resolve(p, root: copy) { try? fm.removeItem(at: dst) } }
        }.value
    }

    /// Konec izvajanja (tudi ob napaki ali prekinitvi): združi, če prevzema ni bilo, in odstrani klon.
    func finish(run id: String) async {
        guard let r = runs[id] else { return }
        if !r.merged, !r.share.readOnly { await acquire(r.copy); await merge(r); unlock(r.copy) }
        await Task.detached { try? FileManager.default.removeItem(at: r.clone) }.value
        runs[id] = nil  // šele po odstranitvi: mesto ».<n>« je spet prosto
    }

    /// Po agentu: minigw prevzame spremembe klona izvajanja (ne pri --minigw-ro), nato gredo v kopijo.
    /// Konča se pred odgovorom, da jih Claude Code že vidi.
    func syncBack(session: String) async {
        guard let s = bySession[session], let copy = s.copy, !s.readOnly else { return }
        let r = Self.run.flatMap { runs[$0] }.flatMap { $0.share === s ? $0 : nil }
        await acquire(copy)  // en prevzem naenkrat: zahteve minigw (changes/file/ack) veljajo za s.pulling
        s.pulling = r
        _ = await syncJob(s, "SYNC-PULL")
        s.pulling = nil
        if let r { await merge(r) }
        unlock(copy)
    }

    /// minigw naj prenese (SYNC-PUSH) ali prevzame (SYNC-PULL); zaporedje zagotavlja zaklep kopije.
    private func syncJob(_ s: Share, _ method: String) async -> Bool {
        let job = DAVJob(id: UUID().uuidString, method: method, path: "", prefix: "", headers: [:], body: Data())
        return await send(s, job, timeout: 900)?.status == 200  // prvi prenos velike mape traja
    }

    /// Manifest odjemalca → poti, ki jih kopija potrebuje; datoteke, ki jih odjemalec nima več, izbriše.
    /// Še nepotrjenih sprememb agenta (`kept`) ne prepiše in ne izbriše; gredo z naslednjim prevzemom.
    /// ponytail: briše le neprezrte datoteke po pravilih kopije; prezrte (izhod gradnje) ostanejo, tudi če jih odjemalec ne prezre več.
    func syncManifest(key: String, _ files: [Sync.Entry], execKnown: Bool = true, device: String? = nil) async -> (need: [String], kept: [String])? {
        guard let s = share(key: key, device: device), let copy = s.copy else { return nil }
        s.lastSeen = Date()
        s.execKnown = execKnown
        try? FileManager.default.createDirectory(at: copy, withIntermediateDirectories: true)
        let prev = s.state ?? [:]
        let current = await Task.detached { Sync.scan(copy, previous: prev).files }.value  // zunaj akterja
        // Prvi prenos te registracije: stanje ni znano → kopija velja za usklajeno. Pri --minigw-ro je kopija zavrženo delo.
        let base = s.readOnly ? current : s.state ?? current
        let agent = Sync.diff(old: base, new: current, root: copy)
        let kept = Set(agent.changed.map(\.path) + agent.deleted)
        let valid = files.filter { Sync.resolve($0.path, root: copy) != nil && !Sync.isGitPath($0.path) }  // ».GIT/hooks« ne
        let (need, gone) = Sync.plan(client: valid, copy: current, keep: kept)
        var state = current
        for p in kept { state[p] = base[p] }
        for p in gone {
            if let url = Sync.resolve(p, root: copy) { try? FileManager.default.removeItem(at: url) }
            state[p] = nil
        }
        s.state = state
        s.incoming = Dictionary(valid.map { ($0.path, $0) }, uniquingKeysWith: { a, _ in a })
        return (need, kept.sorted())
    }

    /// Datoteka iz manifesta; vsebina se mora ujemati z njim. Status HTTP.
    func syncPut(key: String, path: String, data: Data, device: String? = nil) -> Int {
        guard let s = share(key: key, device: device), let copy = s.copy else { return 404 }
        s.lastSeen = Date()
        guard let e = s.incoming[path], let url = Sync.resolve(path, root: copy) else { return 403 }
        guard Sync.sha256(data) == e.sha256 else { return 409 }  // spremenjena med prenosom
        do { try Sync.write(data, to: url, mtime: e.mtime, exec: s.execKnown ? e.exec : nil) } catch { return 500 }
        s.state?[path] = s.execKnown ? e : Sync.entry(url, path: path, previous: nil) ?? e  // Windows: dejanski način kopije
        return 204
    }

    /// Spremembe agenta (v klonu izvajanja, sicer v kopiji) od prenosa; stanje se premakne šele s potrditvijo (syncAck).
    /// nil = ni seje ali samo branje.
    func syncChanges(key: String, device: String? = nil) async -> Sync.Changes? {
        guard let s = share(key: key, device: device), let copy = s.pulling?.clone ?? s.copy, !s.readOnly,
              let prev = s.pulling?.base ?? s.state else { return nil }
        s.lastSeen = Date()
        let (current, skipped) = await Task.detached { Sync.scan(copy, previous: prev) }.value
        var ch = Sync.diff(old: prev, new: current, root: copy)
        ch.base = Dictionary(uniqueKeysWithValues: (ch.changed.map(\.path) + ch.deleted).compactMap { p in prev[p].map { (p, $0.sha256) } })
        ch.skipped = skipped.filter { !s.big.contains($0) }
        s.big = Set(skipped)
        s.pending = Dictionary(uniqueKeysWithValues: ch.changed.map { ($0.path, Optional($0)) } + ch.deleted.map { ($0, nil) })
        return ch
    }

    /// minigw potrdi poti, ki jih je obdelal (zapisal, izbrisal ali shranil kot spor).
    func syncAck(key: String, _ paths: [String], device: String? = nil) -> Bool {
        guard let s = share(key: key, device: device), s.state != nil else { return false }
        for p in paths { if let e = s.pending.removeValue(forKey: p) { s.state?[p] = e } }
        return true
    }

    /// Datoteka kopije za minigw (le poslane spremembe, nikoli prezrte ali zunaj kopije).
    func syncGet(key: String, path: String, device: String? = nil) -> Data? {
        guard let s = share(key: key, device: device), let copy = s.pulling?.clone ?? s.copy, !s.readOnly, (s.pending[path] ?? nil) != nil,
              let url = Sync.resolve(path, root: copy) else { return nil }
        s.lastSeen = Date()
        return try? Data(contentsOf: url)
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
