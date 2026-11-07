import CryptoKit
import Foundation

/// minigw --minigw-work=native in `minigw run`: odjemalec dela v mapi, ki je prek SMB priklopljena mapa tega Maca.
/// Agent in ukazi tečejo neposredno v tej mapi (brez priklopa WebDAV ali kopije).
///
/// Zaznavanje (odjemalec ničesar ne dokaže s potjo):
/// 1. minigw iz priklopa (statfs, /proc/self/mountinfo, UNC) pošlje ime sheme SMB in relativno pot (`Java`, `eureko-modules`).
/// 2. Router ime preslika v svojo mapo (`sharing -l`); mapa mora ostati v shemi in biti v dovoljenih mapah (Settings).
/// 3. Router v mapo zapiše `.minigw-native-<id>` z naključno vsebino; minigw jo prebere skozi svoj priklop in jo vrne.
///    Le kdor vidi prav to mapo, pozna vsebino: enaka pot na drugem disku ali druga mapa ne zadošča.
/// Dokaz preveri pravilnost (ista mapa), ni dovoljenje: dovoljenje so žeton, naprava, shema in dovoljene mape.
/// Native ima pomen načina direct: agent spreminja vse datoteke mape (tudi .git, .claude/settings*.json); za zaščitene
/// poti je način sync.
public enum Native {
    /// Priklop SMB na odjemalcu → (shema, relativna pot pod njo). `source`: `//uporabnik@strežnik/Shema[/podmapa]`
    /// (macOS f_mntfromname, Linux cifs; odstotki dekodirani), `mountPoint`: kam je priklopljena, `cwd`: delovna mapa.
    public static func smbPath(source: String, mountPoint: String, cwd: String) -> (share: String, path: String)? {
        guard source.hasPrefix("//") else { return nil }
        let parts = source.dropFirst(2).split(separator: "/", omittingEmptySubsequences: true).map { $0.removingPercentEncoding ?? String($0) }
        guard parts.count >= 2 else { return nil }  // strežnik in shema
        let mp = mountPoint.hasSuffix("/") && mountPoint.count > 1 ? String(mountPoint.dropLast()) : mountPoint
        guard cwd == mp || cwd.hasPrefix(mp == "/" ? "/" : mp + "/") else { return nil }
        let rel = (Array(parts.dropFirst(2)) + cwd.dropFirst(mp.count).split(separator: "/").map(String.init)).joined(separator: "/")
        return (parts[1], rel)
    }

    /// Priklop SMB, na katerem je `cwd` (macOS statfs). `override`: MINIGW_NATIVE_MOUNT=`<shema>=<mapa>`, mapa na
    /// odjemalcu, ki je ta shema (zaznavanje ne uspe ali preizkus brez SMB); router vseeno preveri vsebino označbe.
    public static func detect(cwd: String, override: String? = nil) -> (share: String, path: String)? {
        if let o = override, let eq = o.firstIndex(of: "=") {
            return smbPath(source: "//minigw/" + o[..<eq], mountPoint: String(o[o.index(after: eq)...]), cwd: cwd)
        }
        var st = statfs()
        guard statfs(cwd, &st) == 0 else { return nil }
        func s<T>(_ v: T) -> String { withUnsafeBytes(of: v) { String(decoding: $0.prefix { $0 != 0 }, as: UTF8.self) } }
        guard s(st.f_fstypename) == "smbfs" else { return nil }
        return smbPath(source: s(st.f_mntfromname), mountPoint: s(st.f_mntonname), cwd: cwd)
    }

    /// Ime označbe, ki jo odjemalec sme prebrati (router ni zaupanja vreden: nikoli druga datoteka).
    public static func validMarker(_ name: String) -> Bool { name.range(of: #"^\.minigw-native-[0-9a-f]{32}$"#, options: .regularExpression) != nil }

    /// Vsebina označbe skozi priklop odjemalca (navadna datoteka, ne povezava); nil, če je (še) ni.
    public static func readMarker(_ url: URL) -> String? {
        Sync.read(url, max: 128).map { String(decoding: $0, as: UTF8.self).trimmingCharacters(in: .whitespacesAndNewlines) }
    }

    /// Izhod `minigw run` za terminal: obdrži le barve (SGR, ESC[…m), \n \r \t \b; izpusti OSC, DCS, APC, PM, druge CSI in
    /// kontrolne znake (router ni zaupanja vreden: naslov okna, odložišče, poizvedbe terminala). Brez stanja med kosi:
    /// nedokončano zaporedje na koncu kosa izpade. ponytail: 8-bitni C1 (0x9B …) ostane; UTF-8 terminali ga ne izvajajo.
    public static func terminalSafe(_ d: Data) -> Data {
        let b = [UInt8](d)
        var out = Data(capacity: b.count), i = 0
        while i < b.count {
            let c = b[i]
            if c == 0x1B {
                guard i + 1 < b.count else { break }
                let k = b[i + 1]
                if k == UInt8(ascii: "[") {
                    var j = i + 2
                    while j < b.count, (0x30...0x3F).contains(b[j]) { j += 1 }
                    let params = b[(i + 2)..<j]
                    var inter = false
                    while j < b.count, (0x20...0x2F).contains(b[j]) { j += 1; inter = true }
                    guard j < b.count, (0x40...0x7E).contains(b[j]) else { break }  // nedokončano
                    if b[j] == UInt8(ascii: "m"), !inter, params.allSatisfy({ $0 == 0x3B || (0x30...0x39).contains($0) }) { out.append(contentsOf: b[i...j]) }
                    i = j + 1
                } else if [UInt8(ascii: "]"), UInt8(ascii: "P"), UInt8(ascii: "X"), UInt8(ascii: "^"), UInt8(ascii: "_")].contains(k) {
                    var j = i + 2  // niz do BEL ali ST (ESC \\)
                    while j < b.count, b[j] != 0x07, !(b[j] == 0x1B && j + 1 < b.count && b[j + 1] == UInt8(ascii: "\\")) { j += 1 }
                    i = j < b.count && b[j] == 0x1B ? j + 2 : j + 1
                } else {
                    i += 2
                }
            } else {
                if c >= 0x20 && c != 0x7F || [0x0A, 0x0D, 0x09, 0x08].contains(c) { out.append(c) }
                i += 1
            }
        }
        return out
    }

    /// Sheme SMB tega Maca iz `sharing -l`: ime SMB → pot (le deljene, »shared: 1«, in ne samo za branje: agent piše).
    public static func smbShares(_ listing: String) -> [String: String] {
        var out: [String: String] = [:], path = "", inSMB = false, name = "", shared = false, readOnly = false
        for raw in listing.split(whereSeparator: \.isNewline) {
            let line = raw.trimmingCharacters(in: .whitespaces)
            func value() -> String { String(line.drop { $0 != ":" }.dropFirst()).trimmingCharacters(in: .whitespaces) }
            if line.hasPrefix("smb:") { inSMB = true; name = ""; shared = false; readOnly = false }
            else if line.hasPrefix("}") {
                if inSMB, shared, !readOnly, !name.isEmpty, !path.isEmpty { out[name] = path }
                inSMB = false
            }
            else if !inSMB, line.hasPrefix("path:") { path = value() }
            else if inSMB, line.hasPrefix("name:") { name = value() }
            else if inSMB, line.hasPrefix("shared:") { shared = value() == "1" }
            else if inSMB, line.hasPrefix("read-only:") { readOnly = value() != "0" }
        }
        return out
    }

    /// Sheme SMB tega Maca (ob vsaki zahtevi: shemo lahko uporabnik doda ali odstrani kadar koli).
    static func localShares() -> [String: String] {
        let p = Process(), pipe = Pipe()
        p.executableURL = URL(fileURLWithPath: "/usr/sbin/sharing")
        p.arguments = ["-l"]
        p.standardOutput = pipe
        p.standardError = FileHandle.nullDevice
        guard (try? p.run()) != nil else { return [:] }
        let data = pipe.fileHandleForReading.readDataToEndOfFile()
        p.waitUntilExit()
        return smbShares(String(decoding: data, as: UTF8.self))
    }

    /// Shema + relativna pot → mapa tega Maca; nil, če sheme ni, pot pobegne iz nje (»..«, povezava) ali mapa ni v
    /// dovoljenih mapah (`roots`, Settings → Allowed folders). Shema brez razlike v velikih črkah (SMB).
    public static func resolve(share: String, path rel: String, shares: [String: String], roots: String) -> URL? {
        guard let base = shares.first(where: { $0.key.caseInsensitiveCompare(share) == .orderedSame })?.value,
              let root = Sync.real(URL(fileURLWithPath: base)) else { return nil }
        let comps = rel.split(separator: "/", omittingEmptySubsequences: false).map(String.init)
        guard rel.isEmpty || !comps.contains(where: { $0.isEmpty || $0 == "." || $0 == ".." || $0.contains("\\") || $0.contains("\0") })
        else { return nil }
        guard let dir = Sync.real(rel.isEmpty ? root : root.appendingPathComponent(rel)),
              dir.path == root.path || dir.path.hasPrefix(root.path + "/"),
              ClaudeCLI.resolveCwd(dir.path, roots: roots) != nil else { return nil }
        return dir  // kanonična: označba se piše z O_NOFOLLOW_ANY
    }

    /// Dovoljen ukaz za `minigw run`: prvi argument natanko eden s seznama (Settings → minigw run).
    public static func commandAllowed(_ argv: [String], list: String) -> Bool {
        guard let first = argv.first, !first.isEmpty else { return false }
        return list.split(separator: ",").map { $0.trimmingCharacters(in: .whitespaces) }.contains(first)
    }

    /// Peskovnik za `minigw run`: pisanje le v mapo, dovoljene mape in predpomnilnike gradnje; branje in omrežje ostaneta.
    /// Nekanonične in neobstoječe poti izpusti (peskovnik primerja kanonične).
    static func sandboxProfile(writable: [String]) -> String {
        let q = { (p: String) in "\"" + p.replacingOccurrences(of: "\\", with: "\\\\").replacingOccurrences(of: "\"", with: "\\\"") + "\"" }
        let paths = Set(writable.compactMap { Sync.real(URL(fileURLWithPath: $0))?.path }).filter { $0 != "/" }.sorted()
        return "(version 1)\n(allow default)\n(deny file-write*)\n(allow file-write* (regex #\"^/dev/\")"
            + paths.map { " (subpath \(q($0)))" }.joined() + ")\n"
    }

    /// Pisanje, ki ga gradnja potrebuje zunaj mape: Maven, Xcode, SwiftPM, predpomnilniki, začasne mape.
    static func buildCaches(home: String = NSHomeDirectory()) -> [String] {
        let tmp = URL(fileURLWithPath: NSTemporaryDirectory()).deletingLastPathComponent().path  // …/T in …/C uporabnika
        return ["\(home)/.m2", "\(home)/.swiftpm", "\(home)/Library/Developer", "\(home)/Library/Caches",
                "\(home)/Library/org.swift.swiftpm", "/private/tmp", tmp]
    }

    /// V peskovniku drug peskovnik ni mogoč (sandbox_apply: Operation not permitted), zato SwiftPM in Xcode brez svojega.
    static func withoutNestedSandbox(_ argv: [String]) -> [String] {
        var a = argv
        switch a.first {
        case "swift" where a.count > 1 && ["build", "test", "run", "package"].contains(a[1]) && !a.contains("--disable-sandbox"):
            a.insert("--disable-sandbox", at: 2)
        case "xcodebuild":
            a += ["-IDEPackageSupportDisableManifestSandbox=YES", "ENABLE_USER_SCRIPT_SANDBOXING=NO"]
        default: break
        }
        return a
    }

    /// Ukaz v lastni skupini procesov (kill(-pid) ustavi tudi podprocese), stdin /dev/null, druge datoteke aplikacije
    /// zaprte (POSIX_SPAWN_CLOEXEC_DEFAULT). Vrne pid in bralni konec stdout in stderr.
    static func spawnGroup(_ exe: String, _ args: [String], dir: URL, env: [String: String]) -> (pid: pid_t, out: FileHandle, err: FileHandle)? {
        var o: [Int32] = [0, 0], e: [Int32] = [0, 0]
        guard pipe(&o) == 0 else { return nil }
        guard pipe(&e) == 0 else { close(o[0]); close(o[1]); return nil }
        var fa: posix_spawn_file_actions_t?, at: posix_spawnattr_t?
        posix_spawn_file_actions_init(&fa); posix_spawnattr_init(&at)
        defer { posix_spawn_file_actions_destroy(&fa); posix_spawnattr_destroy(&at); close(o[1]); close(e[1]) }
        posix_spawn_file_actions_addopen(&fa, 0, "/dev/null", O_RDONLY, 0)
        posix_spawn_file_actions_adddup2(&fa, o[1], 1)
        posix_spawn_file_actions_adddup2(&fa, e[1], 2)
        posix_spawn_file_actions_addchdir_np(&fa, dir.path)
        posix_spawnattr_setflags(&at, Int16(POSIX_SPAWN_SETPGROUP | POSIX_SPAWN_CLOEXEC_DEFAULT))
        posix_spawnattr_setpgroup(&at, 0)
        let argv = ([exe] + args).map { strdup($0) } + [nil], envp = env.map { strdup("\($0.key)=\($0.value)") } + [nil]
        defer { argv.forEach { free($0) }; envp.forEach { free($0) } }
        var pid: pid_t = 0
        guard posix_spawn(&pid, exe, &fa, &at, argv, envp) == 0 else { close(o[0]); close(e[0]); return nil }
        return (pid, FileHandle(fileDescriptor: o[0], closeOnDealloc: true), FileHandle(fileDescriptor: e[0], closeOnDealloc: true))
    }

    /// Izvršljiva datoteka ukaza: z »/« relativno na mapo (npr. ./mvnw), sicer v PATH.
    static func executable(_ name: String, dir: URL, path: String) -> String? {
        if name.contains("/") {
            let p = name.hasPrefix("/") ? name : dir.appendingPathComponent(name).path
            return FileManager.default.isExecutableFile(atPath: p) ? p : nil
        }
        return path.split(separator: ":").map { "\($0)/\(name)" }.first { FileManager.default.isExecutableFile(atPath: $0) }
    }
}

/// Odprti izzivi native: id → mapa, vsebina označbe, naprava. Vsak izziv velja enkrat in največ 60 s.
/// Označbe so zapisane tudi v `records` (scratch), da jih zagon po sesutju pobriše (cleanupStale).
public actor NativeHub {
    struct Challenge { let dir: URL; let marker: URL; let nonce: String; let owner: String; let expires: Date; let record: URL? }
    private var pending: [String: Challenge] = [:]
    private var running: [String: Int] = [:]
    static let ttl: TimeInterval = 60
    static let perOwner = 4, runsPerDevice = 2

    /// Zapiše označbo v `dir` (kanonična pot, brez povezave nikjer v poti); nil, če ni mogoče ali ima lastnik odprtih preveč.
    func challenge(dir: URL, owner: String, records: URL? = nil) -> (id: String, file: String)? {
        sweep()
        guard pending.values.filter({ $0.owner == owner }).count < Self.perOwner, pending.count < 64 else { return nil }
        let id = Self.random(16), nonce = Self.random(32), name = ".minigw-native-" + id
        let marker = dir.appendingPathComponent(name)
        var record: URL?
        if let records, (try? FileManager.default.createDirectory(at: records, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])) != nil {
            record = records.appendingPathComponent(id)
            guard (try? Data(marker.path.utf8).write(to: record!)) != nil else { return nil }  // najprej zapis, nato označba
        }
        let fd = open(marker.path, O_WRONLY | O_CREAT | O_EXCL | O_NOFOLLOW_ANY, 0o644)
        let ok = fd >= 0 && Data(nonce.utf8).withUnsafeBytes { write(fd, $0.baseAddress, $0.count) } == nonce.utf8.count
        if fd >= 0 { close(fd) }
        guard ok else {
            if fd >= 0 { unlink(marker.path) }
            if let record { unlink(record.path) }
            return nil
        }
        pending[id] = Challenge(dir: dir, marker: marker, nonce: nonce, owner: owner, expires: Date().addingTimeInterval(Self.ttl), record: record)
        Task { [weak self] in try? await Task.sleep(for: .seconds(Self.ttl)); await self?.sweep() }
        return (id, name)
    }

    /// Dokaz odjemalca → mapa. Izziv druge naprave ostane; svoj se porabi (tudi ob napačnem dokazu), označba se izbriše.
    func verify(id: String, proof: String, owner: String) -> URL? {
        guard let c = pending[id], c.owner == owner else { return nil }
        drop(id)
        let a = Array(proof.utf8), b = Array(c.nonce.utf8)  // v konstantnem času
        guard c.expires > Date(), a.count == b.count, zip(a, b).reduce(0, { $0 | ($1.0 ^ $1.1) }) == 0 else { return nil }
        return c.dir
    }

    private func drop(_ id: String) {
        guard let c = pending.removeValue(forKey: id) else { return }
        unlink(c.marker.path)
        if let r = c.record { unlink(r.path) }
    }

    private func sweep() { for (id, c) in pending where c.expires <= Date() { drop(id) } }

    /// minigw run: največ `runsPerDevice` hkrati na napravo.
    func startRun(_ device: String) -> Bool {
        guard running[device, default: 0] < Self.runsPerDevice else { return false }
        running[device, default: 0] += 1
        return true
    }
    func endRun(_ device: String) { running[device] = max(0, running[device, default: 1] - 1) }

    /// Ob zagonu aplikacije: označbe prejšnjega zagona (le datoteke z imenom označbe) in njihovi zapisi.
    public static func cleanupStale(_ records: URL) {
        for id in (try? FileManager.default.contentsOfDirectory(atPath: records.path)) ?? [] {
            let r = records.appendingPathComponent(id)
            if let path = try? String(contentsOf: r, encoding: .utf8), Native.validMarker(URL(fileURLWithPath: path).lastPathComponent) {
                unlink(path)
            }
            unlink(r.path)
        }
    }

    static func random(_ bytes: Int) -> String {
        SymmetricKey(size: SymmetricKeySize(bitCount: bytes * 8)).withUnsafeBytes { $0.map { String(format: "%02x", $0) }.joined() }
    }
}

extension Router {
    static let natives = NativeHub()

    /// Lastnik izziva in shema: naprava (dokaz Noise) ali »local« za loopback; oddaljen odjemalec brez ključa naprave nima native.
    static func nativeOwner(_ req: HTTPRequest) -> String? { req.device?.id ?? (RouterServer.isLoopback(req.peer) ? "local" : nil) }

    /// POST /minigw/native {"share", "path"} → {"id", "file"}: označba v preslikani mapi.
    func nativeChallenge(_ req: HTTPRequest, rid: String, writer: ResponseWriter, shares: () -> [String: String] = Native.localShares) async -> Int {
        guard let owner = Self.nativeOwner(req) else {
            return await error(403, "permission_error", "Native mode needs an encrypted connection with a device key (run minigw connect again)", rid: rid, writer: writer)
        }
        guard let b = try? JSONSerialization.jsonObject(with: req.body) as? [String: Any],
              let share = b["share"] as? String, let path = b["path"] as? String else {
            return await error(400, "invalid_request_error", "Body must be JSON with share and path", rid: rid, writer: writer)
        }
        guard let dir = Native.resolve(share: share, path: path, shares: shares(), roots: config.claudeCLI.allowedRoots) else {
            return await error(403, "permission_error",
                               "Native mode: \(share)/\(path) is not a folder of an SMB share in this Mac's allowed folders (Settings → Claude CLI → Allowed folders)",
                               rid: rid, writer: writer)
        }
        guard let c = await Self.natives.challenge(dir: dir, owner: owner, records: config.scratch("tmp").appendingPathComponent("native-markers")) else {
            return await error(503, "api_error", "Native mode: cannot write the check file in the folder (or too many open checks)", rid: rid, writer: writer)
        }
        return await json(200, ["id": c.id, "file": c.file], rid: rid, writer: writer)
    }

    /// POST /minigw/run {"argv": [...]} z dokazom native (x-native-id, x-native-proof): ukaz v mapi na tem Macu.
    /// Odgovor: vrstice »o <base64>« (stdout), »e <base64>« (stderr), »p« (ping), na koncu »x <izhodna koda>«.
    /// Le naprave z vklopljenim »minigw run« in le ukazi s seznama; brez lupine, stdin prazen.
    func runCommand(_ req: HTTPRequest, rid: String, writer: ResponseWriter) async -> Int {
        guard let d = req.device, config.devices.contains(where: { $0.id == d.id && $0.run }) else {
            return await error(403, "permission_error",
                               "minigw run is not allowed for this device: allow it in Mini Gateway → Settings → General → minigw run",
                               rid: rid, writer: writer)
        }
        guard let b = try? JSONSerialization.jsonObject(with: req.body) as? [String: Any], let argv = b["argv"] as? [String],
              Native.commandAllowed(argv, list: config.runCommands) else {
            return await error(403, "permission_error",
                               "minigw run: the command is not in the allowed commands (\(config.runCommands))", rid: rid, writer: writer)
        }
        guard let dir = await Self.natives.verify(id: req.header("x-native-id") ?? "", proof: req.header("x-native-proof") ?? "", owner: d.id) else {
            return await error(403, "permission_error", "minigw run: the folder check failed (the folder is not this Mac's shared folder)", rid: rid, writer: writer)
        }
        guard await Self.natives.startRun(d.id) else {
            return await error(429, "rate_limit_error", "minigw run: this device already runs \(NativeHub.runsPerDevice) commands", rid: rid, writer: writer)
        }
        defer { Task { await Self.natives.endRun(d.id) } }
        var env = ProcessInfo.processInfo.environment
        for k in env.keys where k.hasPrefix("ANTHROPIC_") || k.hasPrefix("CLAUDE_CODE_") || k == "CLAUDECODE" { env[k] = nil }
        env["PATH"] = (env["PATH"].map { $0 + ":" } ?? "") + "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"
        guard let exe = Native.executable(argv[0], dir: dir, path: env["PATH"]!) else {
            return await error(404, "not_found_error", "minigw run: \(argv[0]) not found on the router", rid: rid, writer: writer)
        }
        // Peskovnik: pisanje le v mapo, dovoljene mape in predpomnilnike gradnje (glej sandboxProfile).
        let roots = config.claudeCLI.allowedRoots.split(separator: ",").map { ClaudeCLI.expand($0.trimmingCharacters(in: .whitespaces)) }
        let profile = Native.sandboxProfile(writable: [dir.path] + roots + Native.buildCaches())
        let args = Array(Native.withoutNestedSandbox(argv).dropFirst())
        guard let (pid, out, err) = Native.spawnGroup("/usr/bin/sandbox-exec", ["-p", profile, exe] + args, dir: dir, env: env) else {
            return await self.error(500, "api_error", "minigw run: cannot start \(argv[0])", rid: rid, writer: writer)
        }
        // Skupina procesov: ob prekinitvi SIGTERM, po 5 s SIGKILL; po koncu glavnega procesa po 2 s SIGKILL ostankom
        // (proces v ozadju, ki drži stdout, bi sicer odgovor zadržal v nedogled).
        // ponytail: pid skupine je lahko po izhodu znova dodeljen; ob 2–5 s in zaporednih pid je tveganje zanemarljivo.
        let exit = ExitWaiter()  // en čakalec
        Thread {
            var st: Int32 = 0
            while waitpid(pid, &st, 0) == -1 && errno == EINTR {}
            exit.done(st & 0x7f == 0 ? (st >> 8) & 0xff : 128 + (st & 0x7f))
            DispatchQueue.global().asyncAfter(deadline: .now() + 2) { _ = Darwin.kill(-pid, SIGKILL) }
        }.start()
        @Sendable func stop() {
            Darwin.kill(-pid, SIGTERM)
            Task { try? await Task.sleep(for: .seconds(5)); Darwin.kill(-pid, SIGKILL) }
        }
        Self.log.info("\(rid, privacy: .public) run start device=\(d.name, privacy: .public) folder=\(dir.path, privacy: .private) argv=\(argv.joined(separator: " "), privacy: .private)")
        var code: Int32 = -1
        await withTaskCancellationHandler {
            do {
                try await writer.write(HTTPResponse.head(status: 200, headers: [("Content-Type", "text/plain"), ("Cache-Control", "no-cache"), ("request-id", rid)]))
                let (frames, written) = Self.frames([("o", out), ("e", err)])
                for await f in frames { try await writer.write(f.data); if f.counted { written() } }  // en pisec
                code = await exit.wait()
                try await writer.write(Data("x \(code)\n".utf8))
            } catch {
                stop()  // odjemalec je odšel
            }
        } onCancel: { stop() }
        if code < 0 { code = await exit.wait() }
        Self.log.info("\(rid, privacy: .public) run end device=\(d.name, privacy: .public) exit=\(code)")
        return 200
    }
}

extension Router {
    /// Vrstice »<oznaka> <base64>« iz cevi do EOF vseh, vmes »p« vsakih 15 s (gradnja lahko dolgo molči).
    /// Največ `limit` kosov čaka na pisanje: bralec nato počaka (gradnja se upočasni, pomnilnik ne raste).
    /// `written()` po vsakem zapisanem kosu s `counted`.
    static func frames(_ pipes: [(String, FileHandle)], limit: Int = 64)
        -> (AsyncStream<(data: Data, counted: Bool)>, written: @Sendable () -> Void) {
        let room = DispatchSemaphore(value: limit), gone = Once()
        let stream = AsyncStream<(data: Data, counted: Bool)> { k in
            k.onTermination = { _ in _ = gone.claim() }
            let open = DispatchGroup()
            for (tag, h) in pipes {
                open.enter()
                Thread {  // availableData blokira
                    defer { open.leave() }
                    while true {
                        let d = h.availableData
                        if d.isEmpty { return }
                        while room.wait(timeout: .now() + 1) == .timedOut { if gone.claimed { return } }
                        k.yield((Data("\(tag) \(d.base64EncodedString())\n".utf8), true))
                    }
                }.start()
            }
            open.notify(queue: .global()) { k.finish() }
            Task {
                while true {
                    try? await Task.sleep(for: .seconds(15))
                    if case .terminated = k.yield((Data("p\n".utf8), false)) { return }
                }
            }
        }
        return (stream, { room.signal() })
    }
}
