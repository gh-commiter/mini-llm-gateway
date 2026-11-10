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
        var davPID: pid_t?  // webdavfs_agent, ki je priklopil to mapo (#7): le on sme v /dav/<skrivnost>
        var props: [String: (DAVResponse, Date)] = [:]  // predpomnilnik PROPFIND (kot dir_refresh pri davfs2)
        var writeCount = 0                                // pisanje med čakajočim PROPFIND: odgovora ne shrani
        let copy: URL?                                    // sync: kopija na routerju namesto priklopa
        var state: [String: Sync.Entry]?                  // sync: kopija, kot jo ima odjemalec (prenos + potrjene spremembe)
        var incoming: [String: Sync.Entry] = [:]          // sync: zadnji manifest odjemalca (preverjanje prejetih datotek)
        var pending: [String: Sync.Entry?] = [:]          // sync: poslane spremembe agenta (nil = izbris) do potrditve
        var big: Set<String> = []                         // sync: že javljene prevelike datoteke
        var execKnown = true                              // sync: odjemalec pozna bit x (ne Windows)
        var pulling: Run?                                 // sync: izvajanje, katerega spremembe minigw prav zdaj prevzema
        var keptBase: [String: String?] = [:]             // sync: združeno brez potrditve → različica, iz katere je agent začel
        let device: String?                               // naprava, ki je mapo registrirala (dokaz Noise); nil = lokalno
        let native: URL?                                  // native: mapa tega Maca, ki jo odjemalec vidi prek SMB (dokazano)
        let tree: Tree?                                   // worktree: zrcalo in kloni agentov (glej Worktree)
        var pushing: URL?                                 // worktree: bundle, ki ga minigw prav zdaj nalaga (WORKTREE-PUSH)
        var results: [URL] = []                           // worktree: bundli vej, ki jih minigw prav zdaj prevzema (0 = vrhnji, i = i-ti ugnezdeni v WORKTREE-PULL)
        var capNoted: Set<String> = []                    // L4: agenti, ki jim je opomba o omejitvi #45 že poslana
        init(session: String, cwd: String, readOnly: Bool, copy: URL?, device: String?, native: URL? = nil, tree: Tree? = nil) {
            self.session = session; self.cwd = cwd; self.readOnly = readOnly; self.copy = copy; self.device = device; self.native = native
            self.tree = tree
        }
    }

    /// worktree: golo zrcalo repozitorija odjemalca, mapa klonov in pot cwd v repozitoriju (»« = koren).
    struct Tree: Equatable, Sendable {
        let repo: URL, worktrees: URL, sub: String
        var local: URL? = nil  // native z git: repozitorij na tem Macu (posnetek in prevzem veje tu, brez prenosa)
        /// M1: .git/objects repozitorija uporabnika je še prava mapa (sicer bi allowRead in alternates kazali drugam).
        var localObjectsIntact: Bool {
            guard let local else { return true }
            let o = local.appendingPathComponent(".git/objects")
            return Sync.real(o)?.path == o.path
        }
    }

    /// worktree: klon agenta na svoji veji; zaporedne zahteve iste seje in agenta nadaljujejo v njem (busy = v izvajanju).
    final class Slot {
        let wt: URL, branch: String, base: String
        let nested: [Nested]  // ugnezdeni repozitoriji odjemalca: vsak svoj klon v wt, starš pred otrokom
        var busy = true
        var kept: URL?  // L6: v klonu je delo, ki ni prišlo do odjemalca: ob koncu seje gre klon sem (30 dni)
        private let git: [UInt64]?, cfg: Data?  // H2: .git (naprava, inode) in .git/config ob kloniranju
        init(wt: URL, branch: String, base: String, nested: [Nested] = []) {
            self.wt = wt; self.branch = branch; self.base = base; self.nested = nested
            git = Self.inode(wt.appendingPathComponent(".git")); cfg = Sync.read(wt.appendingPathComponent(".git/config"), max: 1 << 20)
        }
        static func inode(_ u: URL) -> [UInt64]? {
            var st = stat()
            return lstat(u.path, &st) == 0 && st.st_mode & S_IFMT == S_IFDIR ? [UInt64(UInt32(bitPattern: st.st_dev)), st.st_ino] : nil
        }
        /// H2 (kot Go wtSlot.gitIntact): .git vsakega klona (vrhnji in ugnezdeni) je ista mapa z istim config, brez
        /// info/attributes in commondir; sicer nov klon (zamenjan .git z novim config ali atributi bi git samega claude
        /// izvedel filter agenta).
        var gitIntact: Bool {
            Self.same(wt.appendingPathComponent(".git"), git, cfg) && nested.allSatisfy { Self.same($0.dir(wt).appendingPathComponent(".git"), $0.git, $0.cfg) }
        }
        static func same(_ g: URL, _ git: [UInt64]?, _ cfg: Data?) -> Bool {
            var st = stat()
            return git != nil && inode(g) == git && cfg != nil && Sync.read(g.appendingPathComponent("config"), max: 1 << 20) == cfg
                && ["info/attributes", "commondir"].allSatisfy { lstat(g.appendingPathComponent($0).path, &st) != 0 }
        }
        /// .git vseh klonov (vrhnji prvi): edini GIT_DIR, ki ga ovoj pusti, in .git, ki jih ščitijo peskovniki.
        var gitDirs: [String] { [wt.appendingPathComponent(".git").path] + nested.map { $0.dir(wt).appendingPathComponent(".git").path } }
    }

    /// worktree: klon ugnezdenega repozitorija (podmodul ali nesleden) iz svojega zrcala na isti veji (kot Go wtNested).
    struct Nested {
        let path: String, repo: URL, base: String  // pot v klonu (»/«), golo zrcalo, posnetek odjemalca
        let git: [UInt64]?, cfg: Data?             // H2 kot Slot
        init(path: String, repo: URL, base: String, wt: URL) {
            self.path = path; self.repo = repo; self.base = base
            let g = wt.appendingPathComponent(path, isDirectory: true).appendingPathComponent(".git")
            git = Slot.inode(g); cfg = Sync.read(g.appendingPathComponent("config"), max: 1 << 20)
        }
        func dir(_ wt: URL) -> URL { wt.appendingPathComponent(path, isDirectory: true) }
        /// H1 (kot Go nestedIntact): pot so same prave mape (ne povezava ven) in .git je še ista mapa.
        func intact(_ wt: URL) -> Bool {
            var d = wt, st = stat()
            for c in path.split(separator: "/") {
                d = d.appendingPathComponent(String(c), isDirectory: true)
                guard lstat(d.path, &st) == 0, st.st_mode & S_IFMT == S_IFDIR else { return false }
            }
            return git != nil && Slot.inode(d.appendingPathComponent(".git")) == git
        }
    }

    /// sync: eno izvajanje agenta v svojem klonu kopije (vzporedna izvajanja v isti mapi).
    final class Run {
        let share: Share, copy: URL, clone: URL
        let base: [String: Sync.Entry]   // stanje odjemalca ob klonu: od tod so spremembe za prevzem in različice »base«
        let start: [String: Sync.Entry]  // klon ob začetku: od tod so spremembe agenta, ki gredo nazaj v kopijo
        var merged = false
        var acked: Set<String> = []      // poti, ki jih je minigw potrdil ob prevzemu tega izvajanja
        init(share: Share, copy: URL, clone: URL, base: [String: Sync.Entry], start: [String: Sync.Entry]) {
            self.share = share; self.copy = copy; self.clone = clone; self.base = base; self.start = start
        }
    }

    /// ID izvajanja agenta (Router.syncRun): mount ga poveže s klonom, syncBack in finish ga najdeta.
    @TaskLocal public static var run: String?
    /// Agent izvajanja (claude, codex, grok): ime veje worktree.
    @TaskLocal public static var agent: String?
    private var runs: [String: Run] = [:]
    static let maxRunsPerCopy = 4

    /// #1, #9: kopije (scratch/sync/<naprava>/<mapa>), ki jih 14 dni ni uporabila nobena seja, se izbrišejo; oznaka
    /// rabe je datoteka <kopija>.used ob kopiji (v kopiji bi se sinhronizirala nazaj). Največ enkrat na dan.
    static let copyLifetime: TimeInterval = 14 * 86400
    private var lastExpire = Date.distantPast

    static func touchUsed(_ copy: URL) {
        let marker = copy.appendingPathExtension("used")
        if !FileManager.default.createFile(atPath: marker.path, contents: nil) { return }
    }

    func expireCopies(_ root: URL, now: Date = Date(), force: Bool = false) async {
        guard force || now.timeIntervalSince(lastExpire) > 86400 else { return }
        lastExpire = now
        let active = Set(bySession.values.compactMap { $0.copy?.standardizedFileURL.path })
        await offPool { Self.removeOld(root, active: active, now: now) }
    }

    static func removeOld(_ root: URL, active: Set<String>, now: Date) {
        let fm = FileManager.default
        for owner in (try? fm.contentsOfDirectory(at: root, includingPropertiesForKeys: nil)) ?? [] where owner.lastPathComponent != "runs" {
            for copy in (try? fm.contentsOfDirectory(at: owner, includingPropertiesForKeys: [.isDirectoryKey, .isSymbolicLinkKey])) ?? [] {
                guard copy.pathExtension != "used", (try? copy.resourceValues(forKeys: [.isDirectoryKey]).isDirectory) == true,
                      (try? copy.resourceValues(forKeys: [.isSymbolicLinkKey]).isSymbolicLink) != true,
                      !active.contains(copy.standardizedFileURL.path) else { continue }
                let marker = copy.appendingPathExtension("used")
                let used = ((try? fm.attributesOfItem(atPath: marker.path))?[.modificationDate] as? Date)
                    ?? ((try? fm.attributesOfItem(atPath: copy.path))?[.modificationDate] as? Date) ?? now
                if now.timeIntervalSince(used) > copyLifetime {
                    try? fm.removeItem(at: copy)
                    try? fm.removeItem(at: marker)
                }
            }
        }
    }
    var slots: [String: Slot] = [:]      // worktree: »<seja>|<agent>|<n>« → klon
    var runSlot: [String: String] = [:]  // worktree: izvajanje → mesto

    private var bySession: [String: Share] = [:]
    private var reaper: Task<Void, Never>?
    static let mountsDir = FileManager.default.urls(for: .applicationSupportDirectory, in: .userDomainMask)[0]
        .appendingPathComponent("Mini Gateway/mounts", isDirectory: true)

    private func share(key: String) -> Share? { bySession.values.first { $0.key == key } }

    /// Share seje, le za napravo, ki ga je registrirala (druga naprava z istim ID seje ali ključem ga ne doseže).
    private func share(session: String, device: String?) -> Share? { bySession[session].flatMap { $0.device == device ? $0 : nil } }
    func share(key: String, device: String?) -> Share? { share(key: key).flatMap { $0.device == device ? $0 : nil } }

    /// nil: seja že ima share drugega lastnika (brez njegovega ključa je ni mogoče prevzeti).
    /// `copy`: način sync (kopija v scratch/sync), `native`: dokazana mapa tega Maca, sicer priklop.
    func register(session: String, cwd: String, key: String?, readOnly: Bool = false, copy: URL? = nil, device: String? = nil,
                  native: URL? = nil, tree: Tree? = nil) -> String? {
        if let s = bySession[session] {
            guard s.key == key else { return nil }
            if s.cwd == cwd, s.readOnly == readOnly, s.copy == copy, s.native == native, s.tree == tree { s.lastSeen = Date(); return s.key }
            bySession[session] = nil
            close(s)
        }
        let s = Share(session: session, cwd: cwd, readOnly: readOnly, copy: copy, device: device, native: native, tree: tree)
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

    /// Način seje za zgodovino: chat (brez mape), direct (priklop WebDAV), sync, native ali worktree (z vejo agenta).
    /// native z git: »native« z vejo agenta; native v mapi sami (native-direct ali brez git): »native-direct«.
    /// M2: le mapa iste naprave (druga naprava z istim ID-jem seje ne opiše te seje).
    func sessionMode(_ session: String, device: String?) -> (mode: String, readOnly: Bool, branch: String) {
        guard let s = share(session: session, device: device) else { return ("chat", false, "") }
        if let tree = s.tree {
            return (tree.local != nil ? "native" : "worktree", s.readOnly,
                    slots.filter { $0.key.hasPrefix(session + "|") }.min { $0.key < $1.key }?.value.branch ?? "")
        }
        return (s.native != nil ? "native-direct" : s.copy != nil ? "sync" : "direct", s.readOnly, "")  // native v mapi sami
    }

    /// Mapa seje je deljena samo za branje.
    func isReadOnly(session: String) -> Bool { bySession[session]?.readOnly ?? false }

    /// Seja dela v sinhronizirani kopiji (ne v priklopu).
    func bySessionTree(_ session: String) -> Tree? { bySession[session]?.tree }

    /// L3: registrirane mape (sync, worktree, priklop): premik Scratch bi jim vzel kopije.
    public var hasSessions: Bool { !bySession.isEmpty }

    func isSync(session: String) -> Bool { bySession[session].map { $0.copy != nil || $0.tree != nil } ?? false }

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
        dropSlots(session: s.session)
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

    /// Proces, ki sme v /dav te mape (nil = zavrni): že izbrani webdavfs_agent, ali med priklopom routerja prvi sistemski
    /// webdavfs_agent na povezavi. Agent, ki sam pokliče mount_webdav s prebrano skrivnostjo, dobi drug proces.
    static func davClaim(_ claimed: pid_t?, mounting: Bool, find: () -> pid_t?, owns: (pid_t) -> Bool, isAgent: (pid_t) -> Bool) -> pid_t? {
        if let claimed { return owns(claimed) ? claimed : nil }
        guard mounting, let pid = find(), isAgent(pid) else { return nil }
        return pid
    }

    /// Kdo kliče /dav: `find` vrne edini proces na povezavi (počasno: vsi procesi), `owns(pid)` hitro preveri znanega.
    enum DAVPeer { case unchecked, process(find: @Sendable () -> pid_t?, owns: @Sendable (pid_t) -> Bool) }

    func dav(secret: String, method: String, path: String, headers: [String: String], body: Data, peer: DAVPeer = .unchecked) async -> DAVResponse? {
        guard let s = bySession.values.first(where: { $0.secret == secret }) else { return nil }
        // #7: skrivnost je v URL priklopa (mount, ps): agent ali drug uporabnik bi z njo dosegel tujo mapo. Mapo sme brati
        // le sistemski webdavfs_agent, ki ga je med priklopom zagnal router (prvi med priklopom), nato samo on.
        if case .process(let find, let owns) = peer {
            guard let pid = Self.davClaim(s.davPID, mounting: s.mounting != nil, find: find, owns: owns, isAgent: PeerProcess.isWebDAVAgent) else { return nil }
            s.davPID = pid
        }
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
    func send(_ s: Share, _ job: DAVJob, timeout: Double) async -> DAVResponse? {
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
    /// `roots`: dovoljene mape (native: mapa se preveri ob vsakem krogu).
    /// `writes`: agent sme pisati (native brez veje in direct: en pisoči agent naenkrat v mapi, waitWriter).
    func mount(session: String, cwd: String?, base: String, wait: Bool = false, device: String? = nil, roots: String? = nil,
               scratch: URL? = nil, writes: Bool = false) async -> URL? {
        if wait, !session.isEmpty {
            for _ in 0..<60 where bySession[session] == nil { try? await Task.sleep(for: .milliseconds(500)) }
        }
        guard let s = share(session: session, device: device), cwd == nil || cwd == s.cwd else { return nil }
        if let p = AgentRun.current, p.session == session, let dir = p.dir {  // vprašani agent: mapa klicatelja
            // #61: harness vprašanega agenta začne v klonu klicatelja, kjer agent klicatelja še dela (H2 in .git na poti)
            if let id = p.runID, let k = runSlot[id], let slot = slots[k],
               let why = !slot.gitIntact || !slot.nested.allSatisfy({ $0.intact(slot.wt) })
                   ? "The agent's clone was changed (its .git was replaced): start a new conversation"
                   : Worktree.foreignGit(slot.wt, cwd: dir, nested: slot.nested) {
                refusals[id] = (400, why)
                return nil
            }
            return dir
        }
        if let tree = s.tree { return await worktreeMount(s, tree, roots: roots, scratch: scratch) }
        if let native = s.native {  // ista mapa: brez priklopa in kopije; še vedno ista kanonična pot in v dovoljenih mapah
            guard Sync.real(native)?.path == native.path, let roots, ClaudeCLI.resolveCwd(native.path, roots: roots, scratch: scratch) != nil else { return nil }
            guard await waitWriter(Self.writerKey(scope: "", path: native.path), folder: s.cwd, writes: writes) else { return nil }
            return native
        }
        if let copy = s.copy {  // zaklep le za prenos; agent nato dela v svojem klonu (brez izvajanja, npr. Siri: v kopiji)
            await acquire(copy)
            var dir: URL?
            if !Task.isCancelled, await syncJob(s, "SYNC-PUSH") {
                if let id = Self.run { dir = await clone(s, copy, id) } else { dir = copy }
            }
            unlock(copy)
            return dir
        }
        guard await waitWriter(Self.writerKey(scope: "dav:" + (s.device ?? ""), path: s.cwd), folder: s.cwd, writes: writes && !s.readOnly) else { return nil }
        if let mp = s.mountPoint, Self.isMounted(mp) { return mp }
        if let t = s.mounting { return await t.value }
        s.davPID = nil  // nov priklop: nov webdavfs_agent
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

    // MARK: zaklep pisanja (native brez veje, direct: agent piše v pravo mapo)

    private var writers: [String: String] = [:]   // ključ mape (kanonična pot; DAV: naprava|pot) → izvajanje, ki v njej piše
    var refusals: [String: (status: Int, message: String)] = [:]  // izvajanje → zakaj mount ni uspel (status in sporočilo za odjemalca)
    static var writerWait: Duration = .seconds(300)

    /// Ključ zaklepa: `scope` loči DAV (naprava) od pravih map tega Maca (prazen); mapa in njena podmapa se prekrivata.
    static func writerKey(scope: String, path: String) -> String { scope + "\u{0}" + path }
    static func overlaps(_ a: String, _ b: String) -> Bool {
        let (sa, pa) = (a.prefix { $0 != "\u{0}" }, String(a.drop { $0 != "\u{0}" }.dropFirst()))
        let (sb, pb) = (b.prefix { $0 != "\u{0}" }, String(b.drop { $0 != "\u{0}" }.dropFirst()))
        guard sa == sb else { return false }
        return pa == pb || pa.hasPrefix(pb.hasSuffix("/") ? pb : pb + "/") || pb.hasPrefix(pa.hasSuffix("/") ? pa : pa + "/")
    }

    /// En pisoči agent naenkrat v isti pravi mapi (ali v mapi nad ali pod njo); drugi čakajo (največ writerWait). Bralci
    /// (`writes` false) in zahteve brez izvajanja (Siri) ne čakajo. false = ni dočakal (razlog v refusals po izvajanju).
    func waitWriter(_ key: String, folder: String, writes: Bool, run explicit: String? = nil) async -> Bool {
        guard writes, let id = explicit ?? Self.run else { return true }
        let end = ContinuousClock.now + Self.writerWait
        var logged = false
        while writers.contains(where: { $0.value != id && Self.overlaps($0.key, key) }) {
            if !logged { Router.log.info("waiting for the write lock on \(folder, privacy: .public)"); logged = true }
            guard ContinuousClock.now < end, !Task.isCancelled else {
                refusals[id] = (409, "Another agent is still changing \(folder) (one writing agent at a time in a folder that is not a git "
                    + "repository, or with native-direct); try again when it has finished. In a git repository agents work on their own branches.")
                return false
            }
            try? await Task.sleep(for: .seconds(1))
        }
        writers[key] = id
        return true
    }

    /// Konec pisanja izvajanja `id` (tudi finish(run:)).
    func releaseWriter(run id: String) { writers = writers.filter { $0.value != id } }

    /// Agent te seje dela v mapi sami (native brez veje, direct): pred mount lahko čaka na zaklep pisanja.
    func inPlace(session: String) -> Bool { bySession[session].map { $0.tree == nil && $0.copy == nil } ?? false }

    /// Sporočilo za uporabnika prek minigw (WORKTREE-NOTE: izpiše ga na stderr); starejši minigw ga zavrne, ostane v dnevniku.
    /// `extra`: npr. branch, base dostavljene veje (minigw ju preveri in izpiše vrstico o veji).
    func note(_ s: Share, _ message: String, extra: [String: String] = [:]) async {
        let body = (try? JSONSerialization.data(withJSONObject: extra.merging(["message": message]) { $1 })) ?? Data()
        _ = await send(s, DAVJob(id: UUID().uuidString, method: "WORKTREE-NOTE", path: "", prefix: "", headers: [:], body: body), timeout: 10)
    }

    /// L4: opomba o omejitvi agenta enkrat na sejo (mapo) in agenta.
    func firstCapNote(session: String, kind: String) -> Bool { bySession[session]?.capNoted.insert(kind).inserted ?? false }

    /// Sporočilo uporabniku te seje (WORKTREE-NOTE), npr. agent brez pisanja v načinu worktree.
    func note(session: String, _ message: String) async {
        if let s = bySession[session] { await note(s, message) }
    }

    /// Razlog zadnjega neuspelega mount te seje (enkrat).
    func takeRefusal() -> (status: Int, message: String)? { Self.run.flatMap { refusals.removeValue(forKey: $0) } }

    // MARK: sync (kopija namesto priklopa; glej Sync)

    private var busy: [URL: [CheckedContinuation<Void, Never>]] = [:]

    /// En prenos, prevzem ali združitev naenkrat na kopijo; agenti medtem delajo vzporedno vsak v svojem klonu.
    func acquire(_ copy: URL) async {
        if busy[copy] == nil { busy[copy] = [] } else { await withCheckedContinuation { busy[copy]!.append($0) } }
    }

    func unlock(_ copy: URL) {
        if var w = busy[copy], !w.isEmpty { let k = w.removeFirst(); busy[copy] = w; k.resume() } else { busy[copy] = nil }
    }

    /// Klon kopije za izvajanje `id` (APFS: takoj, brez dodatnega prostora) v scratch/sync/runs; kliče se pod zaklepom.
    /// Mesto ».<n>« je najnižje prosto: zaporedna izvajanja imajo isto pot (nadaljevanje seje Claude je vezano na mapo).
    private func clone(_ s: Share, _ copy: URL, _ id: String) async -> URL? {
        // #22: največ maxRunsPerCopy hkratnih izvajanj na kopijo: brez clonefile (zunanji disk) je vsak klon cela kopija.
        guard runs.values.filter({ $0.copy == copy }).count < Self.maxRunsPerCopy else {
            Router.log.error("sync: \(Self.maxRunsPerCopy) agent runs already work in this folder; refusing another")
            return nil
        }
        let dir = copy.deletingLastPathComponent().deletingLastPathComponent().appendingPathComponent("runs", isDirectory: true)
        let name = copy.deletingLastPathComponent().lastPathComponent + "-" + copy.lastPathComponent
        var n = 0
        while runs.values.contains(where: { $0.clone.lastPathComponent == "\(name).\(n)" }) { n += 1 }
        let clone = dir.appendingPathComponent("\(name).\(n)", isDirectory: true)
        let base = s.readOnly ? [:] : s.state ?? [:], readOnly = s.readOnly
        let start = await offPool { () -> [String: Sync.Entry]? in
            try? FileManager.default.removeItem(at: clone)  // ostanek prekinjenega zagona
            try? FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
            // kanonični poti: clone ne sledi povezavam nikjer v poti
            guard let src = Sync.real(copy), let to = Sync.real(dir)?.appendingPathComponent(clone.lastPathComponent),
                  Self.clone(src, to: to, trustedSource: true) else { return nil }
            return readOnly ? [:] : Sync.scan(clone, previous: base).files
        }
        guard let start else { return nil }
        runs[id] = Run(share: s, copy: copy, clone: clone, base: base, start: start)
        return clone
    }

    /// Nikoli ne sledi povezavi (agent jo lahko podtakne po preverjanju poti); poti morata biti kanonični. Brez
    /// CLONE_NOFOLLOW, ki bi povezavo prepisal kot povezavo; CLONE_NOFOLLOW_ANY zavrne tudi zadnji del poti.
    /// Če klon ne uspe (ni APFS): `trustedSource` (kopija ob začetku izvajanja) copyItem, sicer le navadna datoteka.
    static func clone(_ src: URL, to dst: URL, trustedSource: Bool = false) -> Bool {
        if clonefile(src.path, dst.path, UInt32(CLONE_NOFOLLOW_ANY)) == 0 { return true }
        return trustedSource ? (try? FileManager.default.copyItem(at: src, to: dst)) != nil : copyRegular(src, to: dst)
    }

    /// Navadna datoteka iz fd v fd (način in čas ostaneta); povezava nikjer v nobeni poti.
    static func copyRegular(_ src: URL, to dst: URL) -> Bool {
        var st = stat()
        guard let f = Sync.openRegular(src, flags: O_NOFOLLOW_ANY), fstat(f.fileDescriptor, &st) == 0 else { return false }
        let fd = Darwin.open(dst.path, O_WRONLY | O_CREAT | O_EXCL | O_NOFOLLOW_ANY, st.st_mode & 0o777)
        guard fd >= 0 else { return false }
        let out = FileHandle(fileDescriptor: fd, closeOnDealloc: true)
        do {
            while let d = try f.read(upToCount: 1 << 20), !d.isEmpty { try out.write(contentsOf: d) }
            var t = [st.st_atimespec, st.st_mtimespec]
            guard fchmod(fd, st.st_mode & 0o777) == 0, futimens(fd, &t) == 0 else { throw CocoaError(.fileWriteUnknown) }
            return true
        } catch { unlink(dst.path); return false }
    }

    /// Spremembe agenta v klonu nazaj v kopijo (pod zaklepom), da je naslednji prenos spet le razlika.
    /// Pot, ki se je v kopiji od klona spremenila (drug agent ali prenos), ne prepiše. Če je minigw ni potrdil, gre
    /// različica agenta v kopijo kot `<pot>.minigw-agent` (nova datoteka, z naslednjim prevzemom do uporabnika).
    /// Nepotrjene združene ostanejo v kopiji (»kept«) z izvorno različico v keptBase.
    /// ponytail: le sinhronizirane datoteke; prezrti izhod gradnje (.build, node_modules) ostane v klonu in izgine z njim,
    /// zato vsako izvajanje gradi od stanja kopije. Združi tudi te, če bo inkrementalna gradnja prepočasna.
    /// ponytail: nepotrjen izbris v spremenjeni kopiji se opusti (ostane različica kopije).
    private func merge(_ r: Run) async {
        guard !r.merged, !r.share.readOnly else { return }
        r.merged = true
        let (start, clone, copy, acked) = (r.start, r.clone, r.copy, r.acked)
        let (applied, touched) = await offPool { () -> ([String], Set<String>) in
            let fm = FileManager.default
            let ch = Sync.diff(old: start, new: Sync.scan(clone, previous: start).files, root: clone)
            let (realClone, realCopy) = (Sync.real(clone), Sync.real(copy))  // za clone brez povezav v poti
            func unchanged(_ p: String, _ dst: URL) -> Bool { Sync.entry(dst, path: p, previous: start[p])?.sha256 == start[p]?.sha256 }
            var applied: [String] = []
            for p in ch.deleted {  // pred spremembami: datoteka »a« → mapa »a/…«
                guard let dst = Sync.resolve(p, root: copy), unchanged(p, dst) else { continue }
                try? fm.removeItem(at: dst)
                applied.append(p)
            }
            for e in ch.changed {
                guard Sync.resolve(e.path, root: clone) != nil, let dst = Sync.resolve(e.path, root: copy) else { continue }
                if let old = Sync.resolve(e.path + ".minigw-conflict", root: copy) { try? fm.removeItem(at: old) }  // ostanek 7e10925
                let ok = unchanged(e.path, dst)
                if !ok, acked.contains(e.path) { continue }  // odjemalec jo že ima; kopija ostane drugemu izvajanju
                var agent = e.path + ".minigw-agent", n = 1  // spor: prosto ime (-2, -3 …), da drugi agent ne prepiše prvega
                while !ok, let u = Sync.resolve(agent, root: copy), fm.fileExists(atPath: u.path) { n += 1; agent = e.path + ".minigw-agent-\(n)" }
                let rel = ok ? e.path : agent
                guard let target = Sync.resolve(rel, root: copy), let realClone, let realCopy else { continue }
                // Mapa → datoteka: le prazna mapa (nikoli rekurzivno; preskočene poti v njej ostanejo).
                var dir: ObjCBool = false
                if fm.fileExists(atPath: target.path, isDirectory: &dir), dir.boolValue { rmdir(target.path) } else { try? fm.removeItem(at: target) }
                if (try? fm.createDirectory(at: target.deletingLastPathComponent(), withIntermediateDirectories: true)) == nil
                    || !Self.clone(realClone.appendingPathComponent(e.path), to: realCopy.appendingPathComponent(rel)) {
                    Router.log.error("sync: cannot merge \(e.path, privacy: .public) into the copy")
                } else if ok { applied.append(e.path) }
            }
            return (applied, Set(ch.changed.map(\.path) + ch.deleted))
        }
        // keptBase: potrjeno in v kopiji = odjemalec ima različico kopije; nepotrjeno združeno = izvor tega agenta.
        // Ni združeno (kopija se je spremenila): ostane, kar velja za različico v kopiji.
        for p in acked where !touched.contains(p) { r.share.keptBase[p] = nil }
        for p in applied {
            if acked.contains(p) { r.share.keptBase[p] = nil } else { r.share.keptBase.updateValue(r.base[p]?.sha256, forKey: p) }
        }
    }

    /// #73: mape tega izvajanja (klon sync, klon worktree in njegov $MINIGW_DEPS) za killTree; vprašani agent nič (dela v
    /// klonu klicatelja, ki še teče).
    func runDirs() -> [String] {
        guard AgentRun.current == nil, let id = Self.run else { return [] }
        var dirs = [runs[id]?.clone.path].compactMap { $0 }
        if let k = runSlot[id], let wt = slots[k]?.wt { dirs += [wt.path, Worktree.deps(wt).path] }
        return dirs
    }

    /// #73 (kot win-run-job): drevo agenta po teku ni ustavljeno: klon sync tega izvajanja ne gre v kopijo (finish).
    func abandon() {
        if let id = Self.run { runs[id]?.merged = true }
    }

    /// Konec izvajanja (tudi ob napaki ali prekinitvi): združi, če prevzema ni bilo, in odstrani klon.
    func finish(run id: String) async {
        releaseSlot(run: id)  // worktree: mesto spet prosto
        _ = Worktree.runGroups.withLock { $0.removeValue(forKey: id) }  // N1: skupine agentov tega izvajanja
        releaseWriter(run: id)
        refusals[id] = nil
        guard let r = runs[id] else { return }
        if !r.merged, !r.share.readOnly { await acquire(r.copy); await merge(r); unlock(r.copy) }
        await offPool { try? FileManager.default.removeItem(at: r.clone) }
        runs[id] = nil  // šele po odstranitvi: mesto ».<n>« je spet prosto
    }

    /// Po agentu: minigw prevzame spremembe klona izvajanja (ne pri --minigw-ro), nato gredo v kopijo.
    /// Konča se pred odgovorom, da jih Claude Code že vidi.
    func syncBack(session: String) async {
        guard AgentRun.current == nil else { return }  // vprašani agent: prevzem ob koncu izvajanja klicatelja
        guard Router.runBranch?.cap.isEmpty ?? true else { return }  // L1: tek, omejen z #45, ni pisal: nič nazaj k odjemalcu
        if let s = bySession[session], let tree = s.tree { await worktreeBack(s, tree); return }
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
        let files = files.map { e -> Sync.Entry in var e = e; e.path = e.path.precomposedStringWithCanonicalMapping; return e }  // #3: NFC (starejši minigw na Macu: NFD)
        s.lastSeen = Date()
        Self.touchUsed(copy)
        s.execKnown = execKnown
        try? FileManager.default.createDirectory(at: copy, withIntermediateDirectories: true)
        let prev = s.state ?? [:]
        let current = await offPool { Sync.scan(copy, previous: prev).files }  // zunaj akterja
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
            s.keptBase[p] = nil
        }
        s.state = state
        s.incoming = Dictionary(valid.map { ($0.path, $0) }, uniquingKeysWith: { a, _ in a })
        return (need, kept.sorted())
    }

    /// Datoteka iz manifesta; vsebina se mora ujemati z njim. Status HTTP.
    func syncPut(key: String, path: String, data: Data, device: String? = nil) -> Int {
        let path = path.precomposedStringWithCanonicalMapping  // #3
        guard let s = share(key: key, device: device), let copy = s.copy else { return 404 }
        s.lastSeen = Date()
        guard let e = s.incoming[path], let url = Sync.resolve(path, root: copy) else { return 403 }
        guard Int64(data.count) == e.size, Sync.sha256(data) == e.sha256 else { return 409 }  // spremenjena med prenosom; M2: kvota šteje velikost
        do { try Sync.write(data, to: url, mtime: e.mtime, exec: s.execKnown ? e.exec : nil) } catch { return 500 }
        s.keptBase[path] = nil
        s.state?[path] = s.execKnown ? e : Sync.entry(url, path: path, previous: nil) ?? e  // Windows: dejanski način kopije
        return 204
    }

    /// Spremembe agenta (v klonu izvajanja, sicer v kopiji) od prenosa; stanje se premakne šele s potrditvijo (syncAck).
    /// nil = ni seje ali samo branje.
    func syncChanges(key: String, device: String? = nil) async -> Sync.Changes? {
        guard let s = share(key: key, device: device), let copy = s.pulling?.clone ?? s.copy, !s.readOnly,
              let prev = s.pulling?.base ?? s.state else { return nil }
        s.lastSeen = Date()
        let (current, skipped) = await offPool { Sync.scan(copy, previous: prev) }
        var ch = Sync.diff(old: prev, new: current, root: copy)
        ch.base = Dictionary(uniqueKeysWithValues: (ch.changed.map(\.path) + ch.deleted).compactMap { p in
            (s.keptBase[p] ?? prev[p]?.sha256).map { (p, $0) }  // združeno brez potrditve: izvorna različica
        })
        ch.skipped = skipped.filter { !s.big.contains($0) }
        s.big = Set(skipped)
        s.pending = Dictionary(uniqueKeysWithValues: ch.changed.map { ($0.path, Optional($0)) } + ch.deleted.map { ($0, nil) })
        return ch
    }

    /// minigw potrdi poti, ki jih je obdelal (zapisal, izbrisal ali shranil kot spor).
    func syncAck(key: String, _ paths: [String], device: String? = nil) -> Bool {
        let paths = paths.map(\.precomposedStringWithCanonicalMapping)  // #3
        guard let s = share(key: key, device: device), s.state != nil else { return false }
        for p in paths {
            guard let e = s.pending.removeValue(forKey: p) else { continue }
            s.state?[p] = e
            if let r = s.pulling { r.acked.insert(p) } else { s.keptBase[p] = nil }  // izvajanje: keptBase uredi merge
        }
        return true
    }

    /// Datoteka kopije za minigw (le poslane spremembe, nikoli prezrte ali zunaj kopije).
    func syncGet(key: String, path: String, device: String? = nil) -> Data? {
        let path = path.precomposedStringWithCanonicalMapping  // #3
        guard let s = share(key: key, device: device), let copy = s.pulling?.clone ?? s.copy, !s.readOnly, (s.pending[path] ?? nil) != nil,
              Sync.resolve(path, root: copy) != nil, let root = Sync.real(copy) else { return nil }
        s.lastSeen = Date()
        return Sync.read(root.appendingPathComponent(path), max: Int(Sync.maxFile), flags: O_NOFOLLOW_ANY)  // agent ne podtakne povezave
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
