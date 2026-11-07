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

    /// Sheme SMB tega Maca iz `sharing -l`: ime SMB → pot (le deljene, »shared: 1«).
    public static func smbShares(_ listing: String) -> [String: String] {
        var out: [String: String] = [:], path = "", inSMB = false, name = ""
        for raw in listing.split(whereSeparator: \.isNewline) {
            let line = raw.trimmingCharacters(in: .whitespaces)
            func value() -> String { String(line.drop { $0 != ":" }.dropFirst()).trimmingCharacters(in: .whitespaces) }
            if line.hasPrefix("smb:") { inSMB = true; name = "" }
            else if line.hasPrefix("}") { inSMB = false }
            else if !inSMB, line.hasPrefix("path:") { path = value() }
            else if inSMB, line.hasPrefix("name:") { name = value() }
            else if inSMB, line.hasPrefix("shared:"), value() == "1", !name.isEmpty, !path.isEmpty { out[name] = path }
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
public actor NativeHub {
    struct Challenge { let dir: URL; let marker: URL; let nonce: String; let owner: String; let expires: Date }
    private var pending: [String: Challenge] = [:]
    static let ttl: TimeInterval = 60

    /// Zapiše označbo v `dir` (kanonična pot, brez povezave nikjer v poti); nil, če ni mogoče ali je odprtih preveč.
    func challenge(dir: URL, owner: String) -> (id: String, file: String)? {
        sweep()
        guard pending.count < 64 else { return nil }  // ponytail: skupna omejitev; na napravo, če bo kdo zlorabljal
        let id = Self.random(16), nonce = Self.random(32), name = ".minigw-native-" + id
        let marker = dir.appendingPathComponent(name)
        let fd = open(marker.path, O_WRONLY | O_CREAT | O_EXCL | O_NOFOLLOW_ANY, 0o644)
        guard fd >= 0 else { return nil }
        let ok = Data(nonce.utf8).withUnsafeBytes { write(fd, $0.baseAddress, $0.count) } == nonce.utf8.count
        close(fd)
        guard ok else { unlink(marker.path); return nil }
        pending[id] = Challenge(dir: dir, marker: marker, nonce: nonce, owner: owner, expires: Date().addingTimeInterval(Self.ttl))
        Task { [weak self] in try? await Task.sleep(for: .seconds(Self.ttl)); await self?.sweep() }
        return (id, name)
    }

    /// Dokaz odjemalca → mapa; izziv se porabi (tudi ob napačnem dokazu), označba se izbriše.
    func verify(id: String, proof: String, owner: String) -> URL? {
        guard let c = pending.removeValue(forKey: id) else { return nil }
        unlink(c.marker.path)
        let a = Array(proof.utf8), b = Array(c.nonce.utf8)  // v konstantnem času
        guard c.owner == owner, c.expires > Date(), a.count == b.count, zip(a, b).reduce(0, { $0 | ($1.0 ^ $1.1) }) == 0 else { return nil }
        return c.dir
    }

    private func sweep() {
        for (id, c) in pending where c.expires <= Date() { unlink(c.marker.path); pending[id] = nil }
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
        guard let c = await Self.natives.challenge(dir: dir, owner: owner) else {
            return await error(503, "api_error", "Native mode: cannot write the check file in the folder", rid: rid, writer: writer)
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
        var env = ProcessInfo.processInfo.environment
        for k in env.keys where k.hasPrefix("ANTHROPIC_") || k.hasPrefix("CLAUDE_CODE_") || k == "CLAUDECODE" { env[k] = nil }
        env["PATH"] = (env["PATH"].map { $0 + ":" } ?? "") + "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"
        guard let exe = Native.executable(argv[0], dir: dir, path: env["PATH"]!) else {
            return await error(404, "not_found_error", "minigw run: \(argv[0]) not found on the router", rid: rid, writer: writer)
        }
        let p = Process(), out = Pipe(), err = Pipe(), exit = ExitWaiter()
        p.executableURL = URL(fileURLWithPath: exe)
        p.arguments = Array(argv.dropFirst())
        p.currentDirectoryURL = dir
        p.environment = env
        p.standardInput = FileHandle.nullDevice
        p.standardOutput = out
        p.standardError = err
        p.terminationHandler = { exit.done($0.terminationReason == .uncaughtSignal ? 128 + $0.terminationStatus : $0.terminationStatus) }
        do { try p.run() } catch {
            return await self.error(500, "api_error", "minigw run: cannot start \(argv[0]): \(error.localizedDescription)", rid: rid, writer: writer)
        }
        Self.log.info("\(rid, privacy: .public) run \(argv[0], privacy: .public) device=\(d.name, privacy: .public)")
        // ponytail: ob prekinitvi le SIGTERM glavnemu procesu; skupina procesov, če bodo podprocesi ostajali
        await withTaskCancellationHandler {
            do {
                try await writer.write(HTTPResponse.head(status: 200, headers: [("Content-Type", "text/plain"), ("Cache-Control", "no-cache"), ("request-id", rid)]))
                for await frame in Self.frames([("o", out), ("e", err)]) { try await writer.write(frame) }  // en pisec
                let code = await exit.wait()
                try await writer.write(Data("x \(code)\n".utf8))
            } catch {
                p.terminate()  // odjemalec je odšel
            }
        } onCancel: { p.terminate() }
        return 200
    }
}

extension Router {
    /// Vrstice »<oznaka> <base64>« iz cevi do EOF vseh, vmes »p« vsakih 15 s (gradnja lahko dolgo molči).
    static func frames(_ pipes: [(String, Pipe)]) -> AsyncStream<Data> {
        AsyncStream { k in
            let open = DispatchGroup()
            for (tag, pipe) in pipes {
                open.enter()
                Thread {  // availableData blokira
                    while true {
                        let d = pipe.fileHandleForReading.availableData
                        if d.isEmpty { break }
                        k.yield(Data("\(tag) \(d.base64EncodedString())\n".utf8))
                    }
                    open.leave()
                }.start()
            }
            open.notify(queue: .global()) { k.finish() }
            Task {
                while true {
                    try? await Task.sleep(for: .seconds(15))
                    if case .terminated = k.yield(Data("p\n".utf8)) { return }
                }
            }
        }
    }
}
