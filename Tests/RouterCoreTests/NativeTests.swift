import CryptoKit
import Foundation
import Testing
@testable import RouterCore

@Test func nativeSMBPathFromMount() {
    let a = Native.smbPath(source: "//user@server._smb._tcp.local/Java", mountPoint: "/Volumes/Java", cwd: "/Volumes/Java/projects/apps")
    #expect(a?.share == "Java" && a?.path == "projects/apps")
    let b = Native.smbPath(source: "//u@h/Jane%20Doe%E2%80%99s%20Public%20Folder/sub", mountPoint: "/Volumes/Public", cwd: "/Volumes/Public")
    #expect(b?.share == "Jane Doe’s Public Folder" && b?.path == "sub")
    #expect(Native.smbPath(source: "//h/Java", mountPoint: "/Volumes/Java", cwd: "/Volumes/JavaX/p") == nil)  // ne pod priklopom
    #expect(Native.smbPath(source: "/dev/disk3s1", mountPoint: "/", cwd: "/Users/x") == nil)
    #expect(Native.detect(cwd: NSTemporaryDirectory()) == nil)  // lokalni disk
    #expect(Native.detect(cwd: "/x/Java/p", override: "Java=/x/Java")?.path == "p")
    #expect(Native.validMarker(".minigw-native-0123456789abcdef0123456789abcdef"))
    #expect(!Native.validMarker("../.ssh/id_ed25519") && !Native.validMarker(".minigw-native-0123456789abcdef0123456789abcdef/x"))
}

@Test func nativeParsesSharingList() {
    let listing = """
    \t\t\tList of Share Points
    name:\t\tJava
    path:\t\t/Volumes/Java
    \tafp:\t{
    \t\tname:\tJavaAFP
    \t\tshared:\t1
    \t}
    \tsmb:\t{
        \t\tname:\tJava
        \t\tshared:\t1
        \t\tguest access:\t1
    \t}

    name:\t\tOff
    path:\t\t/Volumes/Off
    \tsmb:\t{
        \t\tname:\tOff
        \t\tshared:\t0
    \t}

    name:\t\tRO
    path:\t\t/Volumes/RO
    \tsmb:\t{
        \t\tname:\tRO
        \t\tshared:\t1
        \t\tread-only:\t1
    \t}
    """
    #expect(Native.smbShares(listing) == ["Java": "/Volumes/Java"])  // ne izklopljena in ne samo za branje (L5)
}

/// Mapa »share« (kot /Volumes/Java), projekt v njej, mapa zunaj dovoljenih in povezava ven.
private func nativeFixture() throws -> (root: URL, share: URL, project: URL, outside: URL) {
    let root = try #require(Sync.real(FileManager.default.temporaryDirectory)).appendingPathComponent("native-" + UUID().uuidString)
    let share = root.appendingPathComponent("Java"), project = share.appendingPathComponent("projects")
    let outside = share.appendingPathComponent("private")
    for d in [project, outside, root.appendingPathComponent("elsewhere")] { try FileManager.default.createDirectory(at: d, withIntermediateDirectories: true) }
    try FileManager.default.createSymbolicLink(at: project.appendingPathComponent("link"), withDestinationURL: root.appendingPathComponent("elsewhere"))
    return (root, share, project, outside)
}

@Test func nativeResolveStaysInShareAndAllowedFolders() throws {
    let f = try nativeFixture()
    defer { try? FileManager.default.removeItem(at: f.root) }
    let shares = ["Java": f.share.path], roots = f.project.path
    #expect(Native.resolve(share: "java", path: "projects", shares: shares, roots: roots)?.path == f.project.path)  // brez razlike v črkah
    #expect(Native.resolve(share: "Java", path: "private", shares: shares, roots: roots) == nil)  // ni v dovoljenih mapah
    #expect(Native.resolve(share: "Java", path: "", shares: shares, roots: roots) == nil)
    #expect(Native.resolve(share: "Java", path: "projects/../private", shares: shares, roots: f.share.path) == nil)
    #expect(Native.resolve(share: "Java", path: "projects/link", shares: shares, roots: f.root.path) == nil)  // povezava ven iz sheme
    #expect(Native.resolve(share: "Other", path: "projects", shares: shares, roots: roots) == nil)
    #expect(Native.resolve(share: "Java", path: "/etc", shares: shares, roots: "/") == nil)
}

private func nativeRouter(_ roots: String, devices: [DeviceConfig] = [], commands: String = "sh") -> Router {
    var c = RouterConfig()
    c.claudeCLI.allowedRoots = roots
    c.devices = devices
    c.runCommands = commands
    return Router(config: c, token: "tok", secrets: FakeSecrets(values: [:]))
}

private func nativeReq(_ m: String, _ p: String, _ h: [String: String] = [:], _ body: String = "", device: String? = nil) -> HTTPRequest {
    var q = request(m, p, headers: ["Authorization": "Bearer tok"].merging(h) { $1 }, body: body)
    q.peer = device == nil ? "127.0.0.1" : "192.0.2.7"
    q.device = device.map { Noise.Device(id: $0, name: "Air") }
    return q
}

/// Izziv za shemo »Java« v fixture; vrne id in ime označbe.
private func challenge(_ r: Router, _ f: (root: URL, share: URL, project: URL, outside: URL), path: String = "projects",
                       device: String? = nil) async -> (status: Int, id: String, file: String) {
    let w = FakeWriter()
    _ = await r.nativeChallenge(nativeReq("POST", "/minigw/native", [:], #"{"share":"Java","path":"\#(path)"}"#, device: device),
                                rid: "t", writer: w, shares: { ["Java": f.share.path] })
    let o = (try? JSONSerialization.jsonObject(with: w.data.split(separator: Data("\r\n\r\n".utf8)).last ?? Data()) as? [String: String]) ?? [:]
    return (w.status, o["id"] ?? "", o["file"] ?? "")
}

@Test func nativeProofTruePositiveAndSpoofRefused() async throws {
    let f = try nativeFixture()
    defer { try? FileManager.default.removeItem(at: f.root) }
    let r = nativeRouter(f.project.path)
    func register(_ id: String, _ proof: String, session: String) async -> FakeWriter {
        let w = FakeWriter()
        await r.handle(nativeReq("POST", "/minigw/share", ["x-claude-mini-session": session, "x-claude-mini-cwd": "/Volumes/Java/projects",
                                                           "x-claude-mini-work": "native", "x-native-id": id, "x-native-proof": proof]), writer: w)
        return w
    }

    // Prava mapa: odjemalec prebere označbo skozi »priklop« (tukaj ista mapa) → share kaže na mapo routerja.
    let c = await challenge(r, f)
    #expect(c.status == 200 && Native.validMarker(c.file))
    let proof = try #require(Native.readMarker(f.project.appendingPathComponent(c.file)))
    let session = UUID().uuidString
    #expect(await register(c.id, proof, session: session).status == 200)
    #expect(!FileManager.default.fileExists(atPath: f.project.appendingPathComponent(c.file).path))  // označba izbrisana
    #expect(await Router.shares.mount(session: session, cwd: nil, base: "http://127.0.0.1:1", roots: f.project.path)?.path == f.project.path)
    #expect(await Router.shares.mount(session: session, cwd: nil, base: "http://127.0.0.1:1", roots: f.outside.path) == nil)  // ni več dovoljena
    // Mapa zamenjana s povezavo (npr. prek SMB) po dokazu: krog je zavrnjen.
    let moved = f.root.appendingPathComponent("moved")
    try FileManager.default.moveItem(at: f.project, to: moved)
    try FileManager.default.createSymbolicLink(at: f.project, withDestinationURL: f.outside)
    #expect(await Router.shares.mount(session: session, cwd: nil, base: "http://127.0.0.1:1", roots: f.share.path) == nil)
    try FileManager.default.removeItem(at: f.project)
    try FileManager.default.moveItem(at: moved, to: f.project)
    #expect(await register(c.id, proof, session: UUID().uuidString).status == 403)  // izziv velja enkrat

    // Ista pot, druga mapa (lokalna mapa z enakim imenom, ne priklop routerja): označbe tam ni → ni dokaza.
    let spoofDir = f.root.appendingPathComponent("elsewhere/Java/projects")
    try FileManager.default.createDirectory(at: spoofDir, withIntermediateDirectories: true)
    let s = await challenge(r, f)
    #expect(s.status == 200 && Native.readMarker(spoofDir.appendingPathComponent(s.file)) == nil)
    #expect(await register(s.id, "", session: UUID().uuidString).status == 403)
    #expect(await register(s.id, String(repeating: "0", count: 64), session: UUID().uuidString).status == 403)  // ugibanje: porabljen

    // Mapa sheme zunaj dovoljenih map: izziva ni, nič ni zapisano.
    let out = await challenge(r, f, path: "private")
    #expect(out.status == 403 && ((try? FileManager.default.contentsOfDirectory(atPath: f.outside.path)) ?? []).isEmpty)

    // Izziv druge naprave ne velja za to napravo; oddaljen odjemalec brez ključa naprave ga ne dobi.
    let other = await challenge(r, f, device: "dev-a")
    let p2 = try #require(Native.readMarker(f.project.appendingPathComponent(other.file)))
    #expect(await register(other.id, p2, session: UUID().uuidString).status == 403)  // »local«, ne dev-a
    var remote = nativeReq("POST", "/minigw/native", [:], #"{"share":"Java","path":"projects"}"#)
    remote.peer = "192.0.2.7"
    let w = FakeWriter()
    _ = await r.nativeChallenge(remote, rid: "t", writer: w, shares: { ["Java": f.share.path] })
    #expect(w.status == 403)
}

@Test func nativeRunNeedsOptInCommandAndProof() async throws {
    let f = try nativeFixture()
    defer { try? FileManager.default.removeItem(at: f.root) }
    let argv = #"{"argv":["sh","-c","pwd; echo err >&2; exit 3"]}"#
    func run(_ r: Router, _ body: String, _ c: (status: Int, id: String, file: String)?, device: String?) async -> FakeWriter {
        let proof = c.flatMap { Native.readMarker(f.project.appendingPathComponent($0.file)) } ?? ""
        let w = FakeWriter()
        await r.handle(nativeReq("POST", "/minigw/run", ["x-native-id": c?.id ?? "", "x-native-proof": proof], body, device: device), writer: w)
        return w
    }
    let off = nativeRouter(f.share.path, devices: [DeviceConfig(id: "dev", name: "Air")])
    #expect(await run(off, argv, await challenge(off, f, device: "dev"), device: "dev").status == 403)  // privzeto izklopljeno
    let on = nativeRouter(f.share.path, devices: [DeviceConfig(id: "dev", name: "Air", run: true)])
    #expect(await run(on, argv, await challenge(on, f), device: nil).status == 403)  // lokalno brez naprave: ne
    #expect(await run(on, argv, await challenge(on, f, device: "other"), device: "other").status == 403)  // druga naprava
    #expect(await run(on, #"{"argv":["bash","-c","id"]}"#, await challenge(on, f, device: "dev"), device: "dev").status == 403)  // ni na seznamu
    #expect(await run(on, #"{"argv":["/bin/sh","-c","id"]}"#, await challenge(on, f, device: "dev"), device: "dev").status == 403)
    #expect(await run(on, argv, nil, device: "dev").status == 403)  // brez dokaza mape
    // Zavrnjeni izzivi ne pustijo označb v mapi (ne šele po TTL).
    func markers() -> [String] { ((try? FileManager.default.contentsOfDirectory(atPath: f.project.path)) ?? []).filter { $0.hasPrefix(".minigw-native-") } }
    #expect(markers().isEmpty, "\(markers())")
    // Odjemalec označbe ne vidi (lažna mapa): DELETE /minigw/native jo odstrani; tuja naprava je ne more.
    let open = try #require(await challenge(on, f, device: "dev"))
    let wOther = FakeWriter()
    await on.handle(nativeReq("DELETE", "/minigw/native", ["x-native-id": open.id], "", device: "other"), writer: wOther)
    #expect(markers() == [open.file])
    let wOwn = FakeWriter()
    await on.handle(nativeReq("DELETE", "/minigw/native", ["x-native-id": open.id], "", device: "dev"), writer: wOwn)
    #expect(wOwn.status == 204 && markers().isEmpty)

    let ok = await run(on, argv, await challenge(on, f, device: "dev"), device: "dev")
    #expect(ok.status == 200)
    let lines = ok.text.components(separatedBy: "\r\n\r\n").last!.split(separator: "\n").map(String.init)
    func decoded(_ tag: String) -> String {
        lines.filter { $0.hasPrefix(tag + " ") }.compactMap { Data(base64Encoded: String($0.dropFirst(2))) }.map { String(decoding: $0, as: UTF8.self) }.joined()
    }
    #expect(decoded("o") == f.project.path + "\n" && decoded("e") == "err\n" && lines.last == "x 3")
}

@Test func nativeDeviceConfigDecodesOlderSettings() throws {
    let old = try JSONDecoder().decode(DeviceConfig.self, from: Data(#"{"id":"a","name":"Air","siri":true}"#.utf8))
    #expect(old.siri && !old.run)
    #expect(Native.commandAllowed(["mvn", "-q"], list: "mvn, swift") && !Native.commandAllowed(["mvnw"], list: "mvn") && !Native.commandAllowed([], list: "mvn"))
}

@Test func nativeRunAdminAPI() async throws {
    let id = Noise.base64url(Curve25519.Signing.PrivateKey().publicKey.rawRepresentation)
    var c = RouterConfig()
    c.devices = [DeviceConfig(id: id, name: "Air")]
    let app = FakeApp(c)
    #expect(await admin(app.router(), "GET", "/run").text.contains(#""run":false"#))
    #expect(await admin(app.router(), "PATCH", "/run", body: #"{"allow":["\#(id)"],"commands":"mvn, ./mvnw"}"#).status == 200)
    #expect(app.config.devices[0].run && app.config.runCommands == "mvn, ./mvnw")
    #expect(await admin(app.router(), "PATCH", "/run", body: #"{"deny":["\#(id)"]}"#).status == 200 && !app.config.devices[0].run)
    #expect(await admin(app.router(), "PATCH", "/run", body: #"{"denyNetwork":["\#(id)"]}"#).text.contains(#""network":false"#) && !app.config.devices[0].runNetwork)
    #expect(await admin(app.router(), "PATCH", "/run", body: #"{"allow":["nope"]}"#).status == 400)
    #expect(await admin(app.router(), "PATCH", "/run", body: #"{"run":true}"#).status == 400)  // neznano polje
    #expect(await admin(app.router(), "GET", "/run", auth: nil).status == 401)
}

@Test func sharedFoldersKeepModePerFolder() {
    let old = SharedFolders.parse("/Volumes/Data/a\n\n  /Volumes/Data/b  \n")  // starejša datoteka: le poti
    #expect(old == [SharedFolders.Entry(path: "/Volumes/Data/a"), SharedFolders.Entry(path: "/Volumes/Data/b")])
    let list = [SharedFolders.Entry(path: "/Volumes/Java/projects", work: "native"),
                SharedFolders.Entry(path: "/Volumes/Java", work: "sync", readOnly: true),
                SharedFolders.Entry(path: "/Volumes/Java/my folder")]
    let text = SharedFolders.format(list)
    #expect(text == "/Volumes/Java/projects\t--work=native\n/Volumes/Java\t--work=sync --ro\n/Volumes/Java/my folder\n")
    #expect(SharedFolders.parse(text) == list)
    #expect(SharedFolders.parse("/p\t--work=bogus --ro") == [SharedFolders.Entry(path: "/p", readOnly: true)])

    let apps = SharedFolders.lookup(list, cwd: "/Volumes/Java/projects/apps")  // najbolj natančna mapa
    #expect(apps?.work == "native")
    #expect(SharedFolders.lookup(list, cwd: "/Volumes/Java/other")?.work == "sync")
    #expect(SharedFolders.lookup(list, cwd: "/Volumes/JavaX") == nil)
    #expect(SharedFolders.mode(flag: nil, saved: apps, readOnly: false) == ("native", false))
    #expect(SharedFolders.mode(flag: "sync", saved: apps, readOnly: false) == ("sync", false))  // zastavica zmaga
    #expect(SharedFolders.mode(flag: nil, saved: nil, readOnly: false) == ("direct", false))
    let java = SharedFolders.lookup(list, cwd: "/Volumes/Java/x")
    #expect(SharedFolders.mode(flag: nil, saved: java, readOnly: false) == ("sync", true))
    // M1: shranjen --ro ostane tudi z native (→ direct samo za branje), ne glede na odgovor routerja
    #expect(SharedFolders.mode(flag: "native", saved: java, readOnly: false) == ("direct", true))
    #expect(SharedFolders.mode(flag: "native", saved: nil, readOnly: true) == ("direct", true))
    #expect(SharedFolders.validPath("/Volumes/Java/a b") && !SharedFolders.validPath("/x\t--work=native") && !SharedFolders.validPath("/x\n/y"))
}

@Test func nativeChallengeOwnerAndLimits() async throws {
    let f = try nativeFixture()
    defer { try? FileManager.default.removeItem(at: f.root) }
    let hub = NativeHub(), records = f.root.appendingPathComponent("records")
    // L2: napačen lastnik izziva ne porabi; pravi ga nato reši.
    let c = try #require(await hub.challenge(dir: f.project, owner: "a", records: records))
    let proof = try #require(Native.readMarker(f.project.appendingPathComponent(c.file)))
    #expect(await hub.verify(id: c.id, proof: proof, owner: "b") == nil)
    #expect(await hub.verify(id: c.id, proof: proof, owner: "a")?.path == f.project.path)
    #expect(((try? FileManager.default.contentsOfDirectory(atPath: records.path)) ?? []).isEmpty)  // zapis izbrisan
    // L3: največ 4 odprti na lastnika; drug lastnik ni omejen.
    for _ in 0..<4 { #expect(await hub.challenge(dir: f.project, owner: "a", records: records) != nil) }
    #expect(await hub.challenge(dir: f.project, owner: "a", records: records) == nil)
    #expect(await hub.challenge(dir: f.project, owner: "b", records: records) != nil)
    // L3: po sesutju zagon pobriše označbe iz zapisov (in nič drugega).
    let keep = f.project.appendingPathComponent("keep.txt")
    try Data("x".utf8).write(to: keep)
    try Data(keep.path.utf8).write(to: records.appendingPathComponent("forged"))
    NativeHub.cleanupStale(records)
    let left = (try? FileManager.default.contentsOfDirectory(atPath: f.project.path)) ?? []
    #expect(left.contains("keep.txt") && !left.contains { $0.hasPrefix(".minigw-native-") })
    #expect(((try? FileManager.default.contentsOfDirectory(atPath: records.path)) ?? []).isEmpty)
    // L4: največ 2 hkratna minigw run na napravo.
    let (r1, r2, r3) = (await hub.startRun("d"), await hub.startRun("d"), await hub.startRun("d"))
    #expect(r1 && r2 && !r3)
    await hub.endRun("d")
    #expect(await hub.startRun("d"))
}

@Test func nativeTerminalSafeKeepsOnlyColours() {
    let input = "\u{1B}[1;31mred\u{1B}[0m\r\n\u{1B}]0;title\u{07}\u{1B}]52;c;Zm9v\u{1B}\\\u{1B}P1$q\u{1B}\\\u{1B}[6n\u{1B}[2J\u{1B}[?1049h\u{07}ok\t\u{1B}"
    #expect(String(decoding: Native.terminalSafe(Data(input.utf8)), as: UTF8.self) == "\u{1B}[1;31mred\u{1B}[0m\r\nok\t")
    #expect(String(decoding: Native.terminalSafe(Data("a\u{9B}2Jb\u{9D}0;x\u{07}č€".utf8)), as: UTF8.self) == "a2Jb0;xč€")  // N4: C1 v UTF-8
    #expect(Native.withoutNestedSandbox(["swift", "build", "-c", "release"]) == ["swift", "build", "--disable-sandbox", "-c", "release"])
    #expect(Native.withoutNestedSandbox(["swift", "--version"]) == ["swift", "--version"])
    #expect(Native.withoutNestedSandbox(["mvn", "-q"]) == ["mvn", "-q"])
}

@Test func nativeRunIsSandboxedAndEndsWithItsGroup() async throws {
    let f = try nativeFixture()
    defer { try? FileManager.default.removeItem(at: f.root) }
    // lastna naprava: Router.natives je skupen, testi tečejo vzporedno (največ 2 run na napravo)
    let on = nativeRouter(f.project.path, devices: [DeviceConfig(id: "dev-sbx", name: "Air", run: true)])
    let outside = URL(fileURLWithPath: NSHomeDirectory()).appendingPathComponent(".minigw-sandbox-test-" + UUID().uuidString)
    defer { try? FileManager.default.removeItem(at: outside) }
    func run(_ script: String) async -> FakeWriter {
        let c = await challenge(on, f, device: "dev-sbx")
        let proof = Native.readMarker(f.project.appendingPathComponent(c.file)) ?? ""
        let body = String(decoding: try! JSONSerialization.data(withJSONObject: ["argv": ["sh", "-c", script]]), as: UTF8.self)
        let w = FakeWriter()
        await on.handle(nativeReq("POST", "/minigw/run", ["x-native-id": c.id, "x-native-proof": proof], body, device: "dev-sbx"), writer: w)
        return w
    }
    try FileManager.default.createDirectory(at: f.project.appendingPathComponent(".git"), withIntermediateDirectories: true)
    let w = await run("echo in > inside.txt; echo out > '\(outside.path)'; echo hook > .git/hook")
    #expect(w.status == 200 && FileManager.default.fileExists(atPath: f.project.appendingPathComponent("inside.txt").path))
    #expect(!FileManager.default.fileExists(atPath: outside.path))  // M4: pisanje zunaj mape zavrnjeno
    #expect(!FileManager.default.fileExists(atPath: f.project.appendingPathComponent(".git/hook").path))  // N2: .git
    let start = Date()
    let bg = await run("sleep 30 & echo started")  // L4: proces v ozadju drži stdout; skupina se ustavi
    #expect(bg.text.hasSuffix("x 0\n") && Date().timeIntervalSince(start) < 10)
    // N3: vnuk v svoji seji (setsid) drži cev: najden po cevi in ustavljen, tok se zapre.
    let mark = "minigw-setsid-" + UUID().uuidString
    let start2 = Date()
    let ss = await run("perl -MPOSIX -e 'POSIX::setsid(); $0 = \"\(mark)\"; sleep 30' & echo started")
    #expect(ss.text.hasSuffix("x 0\n") && Date().timeIntervalSince(start2) < 10)
    try await Task.sleep(for: .milliseconds(300))
    let left = Process(), pipe = Pipe()
    left.executableURL = URL(fileURLWithPath: "/usr/bin/pgrep"); left.arguments = ["-f", mark]; left.standardOutput = pipe
    try left.run(); left.waitUntilExit()
    #expect(left.terminationStatus == 1)  // nič ne ostane
}

@Test func nativeRunRechecksFolderAfterProof() async throws {  // N1
    let f = try nativeFixture()
    defer { try? FileManager.default.removeItem(at: f.root) }
    let r = nativeRouter(f.project.path, devices: [DeviceConfig(id: "dev-n1", name: "Air", run: true)])
    let c = await challenge(r, f, device: "dev-n1")
    let proof = try #require(Native.readMarker(f.project.appendingPathComponent(c.file)))
    let moved = f.root.appendingPathComponent("moved")  // po dokazu: mapa → povezava (npr. na domačo mapo)
    try FileManager.default.moveItem(at: f.project, to: moved)
    try FileManager.default.createSymbolicLink(at: f.project, withDestinationURL: f.outside)
    let w = FakeWriter()
    await r.handle(nativeReq("POST", "/minigw/run", ["x-native-id": c.id, "x-native-proof": proof], #"{"argv":["sh","-c","touch pwned"]}"#, device: "dev-n1"), writer: w)
    #expect(w.status == 403 && !FileManager.default.fileExists(atPath: f.outside.appendingPathComponent("pwned").path))
}

@Test func nativeRunProfile() throws {  // N2
    let home = try #require(Sync.real(FileManager.default.temporaryDirectory)).appendingPathComponent("home-" + UUID().uuidString)
    defer { try? FileManager.default.removeItem(at: home) }
    try FileManager.default.createDirectory(at: home.appendingPathComponent(".m2"), withIntermediateDirectories: true)
    let p = try #require(Native.sandboxProfile(dir: "/Volumes/Java/proj", home: home.path, network: true))
    #expect(p.contains(#"(subpath "/Volumes/Java/proj")"#) && p.contains(#"(subpath "/Volumes/Java/proj/.git")"#))
    #expect(!p.contains("Development") && !p.contains("(deny network-outbound (remote ip))"))
    #expect(p.contains("(literal \"\(home.path)/.netrc\")") && p.contains("/.ssh\")") && p.contains("Application Support"))
    #expect(Native.sandboxProfile(dir: "/Volumes/Java/proj", home: home.path, network: false)?.contains("(deny network-outbound (remote ip))") == true)
    // povezava namesto predpomnilnika (ali mape) → brez profila
    try FileManager.default.removeItem(at: home.appendingPathComponent(".m2"))
    try FileManager.default.createSymbolicLink(at: home.appendingPathComponent(".m2"), withDestinationURL: URL(fileURLWithPath: "/"))
    #expect(Native.sandboxProfile(dir: "/Volumes/Java/proj", home: home.path, network: true) == nil)
    let old = try JSONDecoder().decode(DeviceConfig.self, from: Data(#"{"id":"a","name":"Air","run":true}"#.utf8))
    #expect(old.runNetwork)  // privzeto vklopljeno
}

/// P1, P2, P4: prepovedi veljajo za razrešene poti (povezave kot v pravi ~/.swiftpm), skrite mape in skrivnosti.
@Test func nativeRunProfileDeniesResolvedPaths() throws {
    let fm = FileManager.default
    let home = try #require(Sync.real(fm.temporaryDirectory)).appendingPathComponent("home-" + UUID().uuidString)
    defer { try? fm.removeItem(at: home) }
    let lib = home.appendingPathComponent("Library")
    for d in ["Library/org.swift.swiftpm/configuration", "Library/Caches/org.swift.swiftpm/repositories/r/hooks", ".swiftpm",
              "Library/Containers/app", "scratch", "proj"] {
        try fm.createDirectory(at: home.appendingPathComponent(d), withIntermediateDirectories: true)
    }
    try fm.createSymbolicLink(at: home.appendingPathComponent(".swiftpm/configuration"),
                              withDestinationURL: lib.appendingPathComponent("org.swift.swiftpm/configuration"))
    try Data("token".utf8).write(to: home.appendingPathComponent(".git-credentials"))
    try Data("s".utf8).write(to: home.appendingPathComponent("Library/Containers/app/data"))
    try Data("s".utf8).write(to: home.appendingPathComponent("scratch/chat.txt"))
    let profile = try #require(Native.sandboxProfile(dir: home.appendingPathComponent("proj").path, home: home.path, network: true,
                                                     hidden: [home.appendingPathComponent("scratch").path]))
    func sh(_ script: String) throws -> Int32 {
        let p = Process()
        p.executableURL = URL(fileURLWithPath: "/usr/bin/sandbox-exec")
        p.arguments = ["-p", profile, "/bin/sh", "-c", script]
        p.currentDirectoryURL = home.appendingPathComponent("proj")
        p.standardError = FileHandle.nullDevice
        try p.run(); p.waitUntilExit()
        return p.terminationStatus
    }
    let h = home.path
    #expect(try sh("echo x > proj.txt") == 0)  // mapa ostane pisljiva
    #expect(try sh("echo '{}' > \(h)/.swiftpm/configuration/mirrors.json") != 0)  // P1: prek povezave
    #expect(!fm.fileExists(atPath: lib.appendingPathComponent("org.swift.swiftpm/configuration/mirrors.json").path))
    #expect(try sh("echo x > \(h)/Library/Caches/org.swift.swiftpm/repositories/r/hooks/post-checkout") != 0)
    #expect(try sh("cat \(h)/scratch/chat.txt") != 0 && sh("echo x > \(h)/scratch/new") != 0)  // P2
    #expect(try sh("cat \(h)/.git-credentials") != 0 && sh("cat \(h)/Library/Containers/app/data") != 0)  // P4
    // C1: ~/Library/Caches ni pisljiv (programi, ki jih aplikacije naložijo, npr. paket copilot); gradnja v .build in ~/.m2.
    #expect(try sh("echo x > \(h)/Library/Caches/ok") != 0 && sh("mkdir -p \(h)/Library/Caches/copilot/pkg") != 0)
    #expect(Native.buildCacheDenies(home: h).paths.contains(h + "/Library/Caches/copilot"))
    #expect(try sh("echo x > \(h)/.swiftpm/ok") == 0)  // predpomnilniki gradnje ostanejo pisljivi
    #expect(Native.sandboxProfile(dir: h + "/proj", home: h, network: false)?.contains(#"(deny mach-lookup (global-name-regex #"^com\.apple\.nsurlsessiond"))"#) == true)  // P5
}

@Test func nativeTerminalFilterAcrossChunks() {  // P3
    var f = Native.TerminalFilter()
    let a = f.feed(Data([0x61, 0xC2])), b = f.feed(Data([0x9B, 0x32, 0x4A, 0x62]))  // C1 CSI razdeljen na dva kosa
    #expect(a == Data([0x61]) && b == Data([0x32, 0x4A, 0x62]))
    let c = f.feed(Data([0xC4])), d = f.feed(Data([0x8D]))  // »č«, razdeljen drugje, ostane cel
    #expect(String(decoding: c + d, as: UTF8.self) == "č")
    #expect(Native.terminalSafe(Data([0x61, 0xC2])) == Data([0x61]))  // osamljen končni C2
}

/// Q1: build ne doseže sistemskih storitev (nastavitve aplikacije, launchd). Ob nepričakovanem uspehu počisti.
@Test func nativeRunProfileBlocksSystemServices() throws {
    let dir = try #require(Sync.real(FileManager.default.temporaryDirectory)).appendingPathComponent("q1-" + UUID().uuidString)
    try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
    defer { try? FileManager.default.removeItem(at: dir) }
    let profile = try #require(Native.sandboxProfile(dir: dir.path, network: true))
    func run(_ exe: String, _ args: [String], sandboxed: Bool = true) throws -> Int32 {
        let p = Process()
        p.executableURL = URL(fileURLWithPath: sandboxed ? "/usr/bin/sandbox-exec" : exe)
        p.arguments = sandboxed ? ["-p", profile, exe] + args : args
        p.standardOutput = FileHandle.nullDevice; p.standardError = FileHandle.nullDevice
        try p.run(); p.waitUntilExit()
        return p.terminationStatus
    }
    let key = "minigwSandboxTest" + UUID().uuidString.prefix(8)
    let wrote = try run("/usr/bin/defaults", ["write", "dev.trojar.minigateway", key, "1"])
    if wrote == 0 { _ = try run("/usr/bin/defaults", ["delete", "dev.trojar.minigateway", key], sandboxed: false) }
    #expect(wrote != 0)
    #expect(try run("/usr/bin/defaults", ["read", "dev.trojar.minigateway"]) != 0)
    let label = "dev.minigw.sandbox-test." + UUID().uuidString
    let submitted = try run("/bin/launchctl", ["submit", "-l", label, "--", "/usr/bin/true"])
    if submitted == 0 { _ = try run("/bin/launchctl", ["remove", label], sandboxed: false) }
    #expect(submitted != 0)
    #expect(profile.contains("(deny lsopen)") && profile.contains("(deny appleevent-send)") && profile.contains("(deny job-creation)"))
}

/// F1, F2: z omrežjem run ne doseže vtičnic unix (npr. docker.sock) ne vrat routerja; kontrola brez peskovnika jih doseže.
@Test func nativeRunProfileBlocksUnixSocketsAndRouterPort() throws {
    let dir = try #require(Sync.real(FileManager.default.temporaryDirectory)).appendingPathComponent("f1-" + UUID().uuidString)
    try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
    defer { try? FileManager.default.removeItem(at: dir) }
    let port = Int.random(in: 40000...50000)
    let profile = try #require(Native.sandboxProfile(dir: dir.path, network: true, routerPort: port))
    let sock = "/tmp/mgw-\(UUID().uuidString.prefix(8)).sock"
    defer { unlink(sock) }
    func listener(_ args: [String]) throws -> Process {
        let p = Process()
        p.executableURL = URL(fileURLWithPath: "/usr/bin/nc")
        p.arguments = args
        p.standardOutput = FileHandle.nullDevice; p.standardError = FileHandle.nullDevice
        try p.run()
        return p
    }
    func connect(_ args: [String], sandboxed: Bool) throws -> Int32 {
        let p = Process()
        p.executableURL = URL(fileURLWithPath: sandboxed ? "/usr/bin/sandbox-exec" : "/usr/bin/nc")
        p.arguments = sandboxed ? ["-p", profile, "/usr/bin/nc"] + args : args
        p.standardInput = FileHandle.nullDevice; p.standardOutput = FileHandle.nullDevice; p.standardError = FileHandle.nullDevice
        try p.run(); p.waitUntilExit()
        return p.terminationStatus
    }
    let unix = try listener(["-lU", sock]), tcp = try listener(["-lk", "127.0.0.1", String(port)])
    defer { unix.terminate(); tcp.terminate() }
    Thread.sleep(forTimeInterval: 0.5)
    #expect(try connect(["-U", sock], sandboxed: true) != 0)
    #expect(try connect(["-z", "127.0.0.1", String(port)], sandboxed: true) != 0)
    #expect(try connect(["-U", sock], sandboxed: false) == 0)  // kontrola: poslušalca sta tam
    #expect(try connect(["-z", "127.0.0.1", String(port)], sandboxed: false) == 0)
}

/// Mapa seje v skriti mapi (sinhronizirana kopija, klon v Scratch): le ta je vidna; mapa, ki skrito vsebuje, je ne razkrije.
@Test func sandboxProfileFolderInsideHiddenScratch() throws {
    let fm = FileManager.default
    let base = try #require(Sync.real(fm.temporaryDirectory)).appendingPathComponent("hid-" + UUID().uuidString)
    defer { try? fm.removeItem(at: base) }
    let scratch = base.appendingPathComponent("scratch"), mine = scratch.appendingPathComponent("sync/mine"), other = scratch.appendingPathComponent("sync/other")
    for d in [mine, other] { try fm.createDirectory(at: d, withIntermediateDirectories: true) }
    try Data("s".utf8).write(to: other.appendingPathComponent("secret.txt"))
    try Data("m".utf8).write(to: mine.appendingPathComponent("a.txt"))
    func sh(_ profile: String, _ script: String) throws -> Int32 {
        let p = Process()
        p.executableURL = URL(fileURLWithPath: "/usr/bin/sandbox-exec")
        p.arguments = ["-p", profile, "/bin/sh", "-c", script]
        p.standardError = FileHandle.nullDevice
        try p.run(); p.waitUntilExit()
        return p.terminationStatus
    }
    // R3: brez izrecne `visible` ostane tudi mapa seje skrita.
    let none = try #require(Native.sandboxProfile(dir: mine.path, network: false, hidden: [scratch.path], writable: [mine.path]))
    #expect(try sh(none, "cat \(mine.path)/a.txt") != 0 && sh(none, "echo x > \(mine.path)/b.txt") != 0)
    let ro = try #require(Native.sandboxProfile(dir: mine.path, network: false, hidden: [scratch.path], writable: [], visible: mine.path))
    #expect(try sh(ro, "cat \(mine.path)/a.txt") == 0 && sh(ro, "echo x > \(mine.path)/b.txt") != 0)
    #expect(try sh(ro, "cat \(other.path)/secret.txt") != 0)
    let rw = try #require(Native.sandboxProfile(dir: mine.path, network: false, hidden: [scratch.path], writable: [mine.path], visible: mine.path))
    #expect(try sh(rw, "echo x > \(mine.path)/b.txt") == 0 && sh(rw, "cat \(other.path)/secret.txt") != 0)
    let outer = try #require(Native.sandboxProfile(dir: base.path, network: false, hidden: [scratch.path], writable: [base.path]))
    #expect(try sh(outer, "cat \(other.path)/secret.txt") != 0 && sh(outer, "echo x > \(scratch.path)/x") != 0)
}

/// R3: delovna mapa nikoli v Scratch (privzetem ali izbranem), tudi če je ta med dovoljenimi mapami.
@Test func resolveCwdRejectsScratch() throws {
    let fm = FileManager.default
    let base = try #require(Sync.real(fm.temporaryDirectory)).appendingPathComponent("rs-" + UUID().uuidString)
    defer { try? fm.removeItem(at: base) }
    let scratch = base.appendingPathComponent("Mini Gateway Scratch"), inScratch = scratch.appendingPathComponent("sync/x"), proj = base.appendingPathComponent("proj")
    for d in [inScratch, proj] { try fm.createDirectory(at: d, withIntermediateDirectories: true) }
    #expect(ClaudeCLI.resolveCwd(proj.path, roots: base.path, scratch: scratch) != nil)
    #expect(ClaudeCLI.resolveCwd(inScratch.path, roots: base.path, scratch: scratch) == nil && ClaudeCLI.resolveCwd(scratch.path, roots: base.path, scratch: scratch) == nil)
    #expect(Native.resolve(share: "S", path: "sync/x", shares: ["S": scratch.path], roots: base.path, scratch: scratch) == nil)
    let def = RouterConfig.defaultScratch.appendingPathComponent("chat")
    try fm.createDirectory(at: def, withIntermediateDirectories: true)
    #expect(ClaudeCLI.resolveCwd(def.path, roots: NSHomeDirectory()) == nil)  // privzeti Scratch vedno
}
