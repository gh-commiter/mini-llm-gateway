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
    #expect(Native.resolve(share: "java", path: "projects", shares: shares, roots: [roots])?.path == f.project.path)  // brez razlike v črkah
    #expect(Native.resolve(share: "Java", path: "private", shares: shares, roots: [roots]) == nil)  // ni v dovoljenih mapah
    #expect(Native.resolve(share: "Java", path: "", shares: shares, roots: [roots]) == nil)
    #expect(Native.resolve(share: "Java", path: "projects/../private", shares: shares, roots: [f.share.path]) == nil)
    #expect(Native.resolve(share: "Java", path: "projects/link", shares: shares, roots: [f.root.path]) == nil)  // povezava ven iz sheme
    #expect(Native.resolve(share: "Other", path: "projects", shares: shares, roots: [roots]) == nil)
    #expect(Native.resolve(share: "Java", path: "/etc", shares: shares, roots: ["/"]) == nil)
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
    #expect(await Router.shares.mount(session: session, cwd: nil, base: "http://127.0.0.1:1", roots: [f.project.path])?.path == f.project.path)
    #expect(await Router.shares.mount(session: session, cwd: nil, base: "http://127.0.0.1:1", roots: [f.outside.path]) == nil)  // ni več dovoljena
    // Mapa zamenjana s povezavo (npr. prek SMB) po dokazu: krog je zavrnjen.
    let moved = f.root.appendingPathComponent("moved")
    try FileManager.default.moveItem(at: f.project, to: moved)
    try FileManager.default.createSymbolicLink(at: f.project, withDestinationURL: f.outside)
    #expect(await Router.shares.mount(session: session, cwd: nil, base: "http://127.0.0.1:1", roots: [f.share.path]) == nil)
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
    // »other«: dovoljena naprava brez minigw run (neznana naprava je zavrnjena že prej, glej deviceApproval…).
    let on = nativeRouter(f.share.path, devices: [DeviceConfig(id: "dev", name: "Air", run: true), DeviceConfig(id: "other", name: "Other")])
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
    #expect(SharedFolders.mode(flag: nil, saved: nil, readOnly: false) == ("auto", false))  // privzeto auto
    #expect(SharedFolders.mode(flag: "direct", saved: nil, readOnly: false) == ("direct", false))
    #expect(SharedFolders.mode(flag: nil, saved: SharedFolders.Entry(path: "/p", work: "direct"), readOnly: false) == ("direct", false))
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
    // #24: predpomnilnik SwiftPM v zasebni mapi zagona; lastna --cache-path ostane
    #expect(Native.withoutNestedSandbox(["swift", "build"], runTmp: "/s") == ["swift", "build", "--disable-sandbox", "--cache-path", "/s/swiftpm-cache"])
    #expect(Native.withoutNestedSandbox(["swift", "package", "resolve"], runTmp: "/s") == ["swift", "package", "--disable-sandbox", "--cache-path", "/s/swiftpm-cache", "resolve"])
    #expect(Native.withoutNestedSandbox(["swift", "build", "--cache-path=/x"], runTmp: "/s") == ["swift", "build", "--disable-sandbox", "--cache-path=/x"])
    #expect(Native.withoutNestedSandbox(["swift", "test", "--disable-sandbox"], runTmp: "/s") == ["swift", "test", "--cache-path", "/s/swiftpm-cache", "--disable-sandbox"])
    #expect(Native.withoutNestedSandbox(["mvn"], runTmp: "/s") == ["mvn"])
    // #72: xcodebuild: rezultat, paketi in predpomnilnik v zasebni mapi zagona; uporabnikove poti ostanejo
    let x = ["xcodebuild", "-IDEPackageSupportDisableManifestSandbox=YES", "ENABLE_USER_SCRIPT_SANDBOXING=NO"]
    #expect(Native.withoutNestedSandbox(["xcodebuild"], runTmp: "/s") == x + ["-IDEDisablePackageManifestCaching=YES", "-resultBundlePath", "/s/xcodebuild.xcresult",
        "-packageCachePath", "/s/swiftpm-cache", "-clonedSourcePackagesDirPath", "/s/SourcePackages"])
    #expect(Native.withoutNestedSandbox(["xcodebuild", "-resultBundlePath", "/r", "-clonedSourcePackagesDirPath", "/p"], runTmp: "/s")
            == ["xcodebuild", "-resultBundlePath", "/r", "-clonedSourcePackagesDirPath", "/p"] + x.dropFirst() + ["-IDEDisablePackageManifestCaching=YES", "-packageCachePath", "/s/swiftpm-cache"])
    #expect(Native.withoutNestedSandbox(["xcodebuild"]) == x)
    #expect(Native.withoutNestedSandbox(["xcodebuild"], derivedData: "/s/dd") == x + ["-derivedDataPath", "/s/dd"])
    #expect(Native.withoutNestedSandbox(["xcodebuild", "-derivedDataPath", "/mine"], derivedData: "/s/dd") == ["xcodebuild", "-derivedDataPath", "/mine"] + x.dropFirst())
    #expect(Native.withoutNestedSandbox(["swift", "build"], derivedData: "/s/dd") == ["swift", "build", "--disable-sandbox"])
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

/// #73 N2: proces v ozadju zapre cevi (tok se konča takoj), skupina pa še živi: odgovor (in z njim odstranitev začasne
/// mape in zaklepa pisanja) šele po uboju skupine.
@Test func nativeRunCleansUpOnlyAfterGroupIsGone() async throws {
    let f = try nativeFixture()
    defer { try? FileManager.default.removeItem(at: f.root) }
    let on = nativeRouter(f.project.path, devices: [DeviceConfig(id: "dev-n2", name: "Air", run: true)])
    let c = await challenge(on, f, device: "dev-n2")
    let proof = try #require(Native.readMarker(f.project.appendingPathComponent(c.file)))
    let script = "sh -c 'exec >/dev/null 2>&1; sleep 30' & echo $! > bg.pid"
    let body = String(decoding: try JSONSerialization.data(withJSONObject: ["argv": ["sh", "-c", script]]), as: UTF8.self)
    let w = FakeWriter(), start = Date()
    await on.handle(nativeReq("POST", "/minigw/run", ["x-native-id": c.id, "x-native-proof": proof], body, device: "dev-n2"), writer: w)
    // #109: ~6 s fiksnih čakanj (2 + 1 + 1,5 + uboj) + obremenitev; pomembno: odgovor ne čaka na `sleep 30` (30 s), skupine ni več
    #expect(w.text.hasSuffix("x 0\n") && Date().timeIntervalSince(start) < 25)
    let pid = try #require(pid_t(String(contentsOf: f.project.appendingPathComponent("bg.pid"), encoding: .utf8).trimmingCharacters(in: .whitespacesAndNewlines)))
    #expect(Darwin.kill(pid, 0) != 0)  // ob odgovoru skupine ni več
}

/// #73 (kot win-run-job): prekinitev agenta, katerega vnuk (v skupini in v svoji seji) drži stdout, se konča hitro in
/// drevesa ni več; tek je neuspešen (brez finalize).
@Test func agentCancelStopsTreeHoldingPipe() async throws {
    let dir = try #require(Sync.real(FileManager.default.temporaryDirectory)).appendingPathComponent("kt-" + UUID().uuidString)
    try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
    defer { try? FileManager.default.removeItem(at: dir) }
    let pids = dir.appendingPathComponent("pids")
    let script = "sleep 60 & echo $! >> '\(pids.path)'; perl -MPOSIX -e 'POSIX::setsid(); sleep 60' & echo $! >> '\(pids.path)'; sleep 60"
    let task = Task { try await Codex.run("/bin/sh", args: ["-c", script], cwd: dir, prompt: "", output: dir.appendingPathComponent("out")) }
    try await Task.sleep(for: .seconds(1))
    let start = Date()
    task.cancel()
    await #expect(throws: Codex.Failure.self) { try await task.value }
    #expect(Date().timeIntervalSince(start) < 10)
    let list = try String(contentsOf: pids, encoding: .utf8).split(whereSeparator: \.isNewline).compactMap { pid_t(String($0)) }
    #expect(list.count == 2 && list.allSatisfy { Darwin.kill($0, 0) != 0 })
}

/// #73: konec agenta, ko vnuk še drži stdout: branje ne čaka nanj (2 s), drevesa ob vrnitvi ni več.
@Test func agentEndStopsGrandchildHoldingPipe() async throws {
    let dir = try #require(Sync.real(FileManager.default.temporaryDirectory)).appendingPathComponent("kt-" + UUID().uuidString)
    try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
    defer { try? FileManager.default.removeItem(at: dir) }
    let out = dir.appendingPathComponent("out"), pid = dir.appendingPathComponent("pid")
    let start = Date()
    let (text, _) = try await Codex.run("/bin/sh", args: ["-c", "sleep 60 & echo $! > '\(pid.path)'; echo done > '\(out.path)'"],
                                        cwd: dir, prompt: "", output: out)
    #expect(text == "done" && Date().timeIntervalSince(start) < 10)
    let p = try #require(pid_t(String(contentsOf: pid, encoding: .utf8).trimmingCharacters(in: .whitespacesAndNewlines)))
    #expect(Darwin.kill(p, 0) != 0)
}

/// #73: proces v svoji seji brez cevi (stdio na /dev/null) z delovno mapo v klonu se ustavi, preden se tek vrne (syncBack).
@Test func agentEndStopsSetsidWithoutPipeInClone() async throws {
    let dir = try #require(Sync.real(FileManager.default.temporaryDirectory)).appendingPathComponent("kt-" + UUID().uuidString)
    try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
    defer { try? FileManager.default.removeItem(at: dir) }
    let out = dir.appendingPathComponent("out"), pid = dir.appendingPathComponent("pid")
    let script = "perl -MPOSIX -e 'POSIX::setsid(); sleep 60' </dev/null >/dev/null 2>&1 & echo $! > '\(pid.path)'; echo done > '\(out.path)'"
    let (text, _) = try await Codex.run("/bin/sh", args: ["-c", script], cwd: dir, prompt: "", output: out, dirs: [dir.path])
    #expect(text == "done")
    let p = try #require(pid_t(String(contentsOf: pid, encoding: .utf8).trimmingCharacters(in: .whitespacesAndNewlines)))
    #expect(Darwin.kill(p, 0) != 0)
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
    #expect(ClaudeCLI.resolveCwd(proj.path, roots: [base.path], scratch: scratch) != nil)
    #expect(ClaudeCLI.resolveCwd(inScratch.path, roots: [base.path], scratch: scratch) == nil && ClaudeCLI.resolveCwd(scratch.path, roots: [base.path], scratch: scratch) == nil)
    #expect(Native.resolve(share: "S", path: "sync/x", shares: ["S": scratch.path], roots: [base.path], scratch: scratch) == nil)
    let def = RouterConfig.defaultScratch.appendingPathComponent("chat")
    try fm.createDirectory(at: def, withIntermediateDirectories: true)
    #expect(ClaudeCLI.resolveCwd(def.path, roots: [NSHomeDirectory()]) == nil)  // privzeti Scratch vedno
}

/// L1, L2: samodejno deljenje v začasni mapi: podmapa da, koren, skrita, tuja, neobstoječa in zgornje ravni Claude Code ne;
/// začasna mapa /, domača ali nad njo ničesar ne odpre (kot TestAutoShare v Go).
@Test func autoShareTempRules() throws {
    let fm = FileManager.default, home = NSHomeDirectory()
    let tmp = fm.temporaryDirectory.appendingPathComponent("as-" + UUID().uuidString).path
    defer { try? fm.removeItem(atPath: tmp) }
    func mk(_ p: String) -> String { let d = tmp + "/" + p; try? fm.createDirectory(atPath: d, withIntermediateDirectories: true); return d }
    let c = "claude-\(getuid())"
    let x = "/tmp/as-" + UUID().uuidString
    try fm.createDirectory(atPath: x, withIntermediateDirectories: false)
    defer { try? fm.removeItem(atPath: x) }
    for p in [mk("proj"), mk("proj/src"), mk("\(c)/p/s/scratchpad"), mk("\(c)/p/s/scratchpad/x"), x, "/private" + x,
              home + "/Development/x", "/Volumes/Java/importers/app"] {
        #expect(SharedFolders.autoShare(p, temp: tmp), "\(p)")
    }
    for p in [tmp, mk(".hidden/x"), tmp + "/missing/x", mk(c), mk("\(c)/p"), mk("\(c)/p/s"), "/tmp", "/private/tmp", "/tmp/.X11-unix",
              "/", home, home + "/.ssh", home + "/Library/Mail", "/Volumes/Java", "/usr/local/src"] {
        #expect(!SharedFolders.autoShare(p, temp: tmp), "\(p)")
    }
    if let other = try? fm.contentsOfDirectory(atPath: "/private/tmp").first(where: { n in
        let a = try? fm.attributesOfItem(atPath: "/private/tmp/" + n)
        return !n.hasPrefix(".") && a?[.type] as? FileAttributeType == .typeDirectory && (a?[.ownerAccountID] as? NSNumber)?.uint32Value != getuid()
    }) {
        #expect(!SharedFolders.autoShare("/tmp/\(other)/x", temp: tmp))  // mapa drugega uporabnika
    }
    for bad in ["/", home, (home as NSString).deletingLastPathComponent] {
        for p in [home + "/.ssh", home + "/Library/x", (home as NSString).deletingLastPathComponent + "/other/x"] {
            #expect(!SharedFolders.autoShare(p, temp: bad), "\(bad) \(p)")
        }
    }
    #expect(SharedFolders.implicitWork(asked: nil, work: "direct") && SharedFolders.implicitWork(asked: "auto", work: "direct")
        && SharedFolders.implicitWork(asked: "worktree", work: "direct") && !SharedFolders.implicitWork(asked: "direct", work: "direct"))
}

/// M1: vrstica o zavrnitvi enkrat na pot, kontrolni znaki nikoli surovi, največ RefusalLog.max poti.
@Test func webdavRefusalLine() {
    var l = WebDAV.RefusalLog()
    for p in [".git/\u{1b}]52;c;eA==\u{7}", ".git/a\nminigw: fake", ".git/\rb", ".git/\u{202e}txt.exe"] {
        let line = l.line(p) ?? ""
        #expect(line.hasPrefix("minigw: refused") && !line.dropLast().unicodeScalars.contains { ["\u{1b}", "\n", "\r", "\u{7}", "\u{202e}"].contains($0) }, "\(line)")
        #expect(l.line(p) == nil)
    }
    for i in l.paths.count..<WebDAV.RefusalLog.max { #expect(l.line(".git/\(i)") != nil) }
    #expect(l.line(".git/more")?.contains("not shown") == true)
    #expect(l.line(".git/more2") == nil && l.paths.count == WebDAV.RefusalLog.max)
}

/// L4: ubežni znaki JSON v napaki agenta ne postanejo pravi kontrolni znaki.
@Test func agentMessageControlChars() {
    #expect(AgentError.message(#"{"error":{"message":"a\u001b]52;c;eA==\u0007b\r\u0085‮c\u009b\u007f"}}"#) == "a]52;c;eA==bc")
}

/// #72: po zagonu le nove prazne mape NSIRD_* (TemporaryItems) in TemporaryDirectory.* (le z .keep-directory);
/// nikoli obstoječe, druge datoteke, povezave ali mape z vsebino.
@Test func nativeRemovesOnlyNewSaveCopies() throws {
    let fm = FileManager.default
    let t = try #require(Sync.real(fm.temporaryDirectory)).appendingPathComponent("savecopies-" + UUID().uuidString).path
    defer { try? fm.removeItem(atPath: t) }
    try fm.createDirectory(atPath: t + "/TemporaryItems/NSIRD_old_1", withIntermediateDirectories: true)
    try fm.createDirectory(atPath: t + "/TemporaryDirectory.old", withIntermediateDirectories: true)
    // xcodebuild test: tekač testov ne sme izpisati mape z imenom TemporaryItems (swift test lahko); tam preskoči.
    guard (try? fm.contentsOfDirectory(atPath: t + "/TemporaryItems")) != nil else { return }
    let before = Native.saveCopies(dirs: [], temp: t), start = Date() - 1
    #expect(before == [t + "/TemporaryItems/NSIRD_old_1", t + "/TemporaryDirectory.old"])
    for d in ["TemporaryItems/NSIRD_new_1", "TemporaryItems/NSIRD_full", "TemporaryDirectory.new", "TemporaryDirectory.keep",
              "TemporaryDirectory.data", "NSIRD_notInItems", "TemporaryItems/other"] {
        try fm.createDirectory(atPath: t + "/" + d, withIntermediateDirectories: true)
    }
    fm.createFile(atPath: t + "/TemporaryItems/NSIRD_full/x", contents: Data("x".utf8))
    fm.createFile(atPath: t + "/TemporaryDirectory.keep/.keep-directory", contents: Data())
    fm.createFile(atPath: t + "/TemporaryDirectory.data/.keep-directory", contents: Data("x".utf8))
    fm.createFile(atPath: t + "/TemporaryDirectory.file", contents: Data())  // datoteka, ne mapa
    try fm.createSymbolicLink(atPath: t + "/TemporaryItems/NSIRD_link", withDestinationPath: t + "/TemporaryItems/other")
    let after = Native.saveCopies(dirs: [], temp: t)
    #expect(Native.removeSaveCopies(after: after, before: before, since: start, settled: 60).isEmpty)  // še ne umirjene
    #expect(Native.removeSaveCopies(after: after, before: before, since: Date() + 60, settled: 0).isEmpty)  // rojene pred zagonom
    let removed = Native.removeSaveCopies(after: after, before: before, since: start, settled: 0)
    #expect(Set(removed) == [t + "/TemporaryItems/NSIRD_new_1", t + "/TemporaryDirectory.new", t + "/TemporaryDirectory.keep"])
    for kept in ["TemporaryItems/NSIRD_old_1", "TemporaryDirectory.old", "TemporaryItems/NSIRD_full/x", "TemporaryDirectory.data/.keep-directory",
                 "TemporaryDirectory.file", "NSIRD_notInItems", "TemporaryItems/other"] {
        #expect(fm.fileExists(atPath: t + "/" + kept), "\(kept)")
    }
    var st = stat()
    #expect(lstat(t + "/TemporaryItems/NSIRD_link", &st) == 0)
}

/// #72: z DIRHELPER_USER_DIR_SUFFIX in povezavama (linkRunDirs) sta T in C otroka v mapi zagona. Pade, če libSystem
/// pripone ne upošteva več. Nikoli brez povezav (libSystem bi ustvaril neizbrisljivo <T>/<suffix>).
@Test func nativeRunDirSuffixRedirectsTempAndCache() throws {
    let fm = FileManager.default
    let rt = try #require(Sync.real(fm.temporaryDirectory)).appendingPathComponent("runsuffix-" + UUID().uuidString)
    defer { try? fm.removeItem(at: rt) }
    let real = try #require(Native.makeRunTmp(rt))
    let suffix = Native.runDirPrefix + "test-" + UUID().uuidString
    let links = try #require(Native.linkRunDirs(real, suffix: suffix))
    defer { Native.unlinkRunDirs(links) }
    // Sondo prevedemo: programi s SIP (/usr/bin/getconf) okolje dirhelper prezrejo, orodja Xcode (swift, swiftc) ne.
    let probe = rt.appendingPathComponent("probe")
    try "#include <unistd.h>\n#include <stdio.h>\nint main(int c,char**v){char b[1024];confstr(v[1][0]=='T'?_CS_DARWIN_USER_TEMP_DIR:_CS_DARWIN_USER_CACHE_DIR,b,sizeof b);puts(b);}\n"
        .write(to: rt.appendingPathComponent("probe.c"), atomically: false, encoding: .utf8)
    let cc = Process()
    cc.executableURL = URL(fileURLWithPath: "/usr/bin/cc"); cc.arguments = ["-o", probe.path, rt.appendingPathComponent("probe.c").path]
    try cc.run(); cc.waitUntilExit()
    try #require(cc.terminationStatus == 0)
    let env = Native.runEnv(ProcessInfo.processInfo.environment, runTmp: rt, suffix: suffix)
    for sub in ["T", "C"] {
        let p = Process(), out = Pipe()
        p.executableURL = probe; p.arguments = [sub]; p.environment = env; p.standardOutput = out
        try p.run(); p.waitUntilExit()
        let dir = String(decoding: out.fileHandleForReading.readDataToEndOfFile(), as: UTF8.self).trimmingCharacters(in: .whitespacesAndNewlines)
        #expect(dir.hasSuffix("/" + suffix + "/"), "\(dir)")
        #expect(Sync.real(URL(fileURLWithPath: dir))?.path == real + "/" + sub)
    }
    Native.unlinkRunDirs(links)
    #expect(links.allSatisfy { var st = stat(); return lstat($0, &st) != 0 })
}

/// #74: samodejna shema domače mape (ime uporabnika) → domača mapa; ne skrite mape, ne Library, ne domača mapa sama.
@Test func nativeHomeShare() throws {
    let f = try nativeFixture()
    defer { try? FileManager.default.removeItem(at: f.root) }
    let home = f.share.path, explicit = ["Java": "/Volumes/Java"]
    #expect(Native.withHomeShare(explicit, virtualHomes: nil, user: "ana", home: home) == ["Java": "/Volumes/Java", "ana": home])
    #expect(Native.withHomeShare(explicit, virtualHomes: false, user: "ana", home: home) == explicit)  // izklopljeno
    #expect(Native.withHomeShare(["ANA": "/Volumes/X"], virtualHomes: true, user: "ana", home: home) == ["ANA": "/Volumes/X"])  // izrecna ima prednost
    let shares = Native.withHomeShare([:], virtualHomes: true, user: "ana", home: home)
    try FileManager.default.createDirectory(at: f.share.appendingPathComponent(".ssh"), withIntermediateDirectories: true)
    try FileManager.default.createDirectory(at: f.share.appendingPathComponent("Library/Keychains"), withIntermediateDirectories: true)
    #expect(Native.resolve(share: "ana", path: "projects", shares: shares, roots: [f.project.path], home: home)?.path == f.project.path)
    #expect(Native.resolve(share: "ana", path: "private", shares: shares, roots: [f.project.path], home: home) == nil)  // ni dovoljena
    for p in ["", ".ssh", "Library/Keychains", "library"] {  // tudi ko je vsa domača mapa dovoljena
        #expect(Native.resolve(share: "ana", path: p, shares: shares, roots: [home], home: home) == nil)
    }
    #expect(Native.resolve(share: "ana", path: "private", shares: shares, roots: [home], home: home)?.path == f.outside.path)
    // Kanonična pot šteje, za vsako shemo: povezava v dovoljeni mapi (~/Google Drive → ~/Library/CloudStorage) in shema nad domačo mapo.
    let cloud = f.share.appendingPathComponent("Library/CloudStorage")
    try FileManager.default.createDirectory(at: cloud, withIntermediateDirectories: true)
    try FileManager.default.createSymbolicLink(at: f.project.appendingPathComponent("gd"), withDestinationURL: cloud)
    try FileManager.default.createSymbolicLink(at: f.project.appendingPathComponent("ssh"), withDestinationURL: f.share.appendingPathComponent(".ssh"))
    for p in ["projects/gd", "projects/ssh"] {
        #expect(Native.resolve(share: "ana", path: p, shares: shares, roots: [home], home: home) == nil)
    }
    let users = ["Users": f.root.path]
    try FileManager.default.createDirectory(at: f.share.appendingPathComponent("Library/LaunchAgents"), withIntermediateDirectories: true)
    for p in ["Java/Library/LaunchAgents", "Java/.ssh", "Java"] {
        #expect(Native.resolve(share: "Users", path: p, shares: users, roots: [f.root.path], home: home) == nil)
    }
    #expect(Native.resolve(share: "Users", path: "Java/projects", shares: users, roots: [f.root.path], home: home)?.path == f.project.path)
    #expect(!Native.homeSecret(f.project.path, home: home) && Native.homeSecret(cloud.path, home: home))
    #expect(!Native.homeSecret("/", home: "/"))
    let realHome = Sync.real(URL(fileURLWithPath: home))!.path
    #expect(Native.homeSecret("/System/Volumes/Data" + realHome + "/Library/LaunchAgents", home: home))
    #expect(Native.homeSecret("/System/Volumes/Data" + realHome, home: home))
    #expect(!Native.homeSecret("/System/Volumes/Data" + realHome + "/Development", home: home))
}

@Test func nativeSmbdEnabled() {
    #expect(Native.smbdEnabled("disabled services = {\n\t\t\"com.apple.smbd\" => enabled\n}"))
    #expect(!Native.smbdEnabled("\t\t\"com.apple.smbd\" => disabled\n"))
    #expect(!Native.smbdEnabled(nil) && !Native.smbdEnabled(""))
}

/// 0.5.24 round 1 A/B: odgovor run brez »nosniff« URLSession zadrži do 512 bajtov (iskanje vrste vsebine): odjemalec
/// (minigw prek šifriranega proxyja) ne vidi izhoda do konca kratkega ali tihega zagona. Izhod mora priti sproti.
@Test func runOutputStreamsBeforeTheCommandEnds() async throws {
    let f = try nativeFixture()
    defer { try? FileManager.default.removeItem(at: f.root) }
    let dev = Curve25519.Signing.PrivateKey(), devID = Noise.base64url(dev.publicKey.rawRepresentation)
    var c = RouterConfig()
    c.host = "127.0.0.1"
    c.claudeCLI.allowedRoots = f.share.path
    c.devices = [DeviceConfig(id: devID, name: "Air", run: true)]
    c.runCommands = "sh"
    let key = Noise.newKey()
    var r: Router!
    let (server, port) = try await startOnFreePort(key: key) { c.port = $0; r = Router(config: c, token: "tok", secrets: FakeSecrets(values: [:])); return r }
    defer { server.stop() }
    let proxy = try #require(NoiseProxy(routerURL: "http://127.0.0.1:\(port)", routerKey: key.publicKey.rawRepresentation,
                                        device: (key: dev, name: "Air"), suite: .aesGCM))
    try proxy.start()
    defer { proxy.stop() }
    let ch = await challenge(r, f, device: devID)
    let proof = try #require(Native.readMarker(f.project.appendingPathComponent(ch.file)))
    var q = URLRequest(url: URL(string: proxy.localURL + "/minigw/run")!, timeoutInterval: 120)
    q.httpMethod = "POST"
    // #109: ukaz čaka na datoteko, ki jo test ustvari ob prvi vrstici (ne na čas): če izhod ne teče sproti, prva vrstica
    // pride šele po koncu ukaza (30 s) in je zadnja vrstica »timeout«.
    q.httpBody = Data(#"{"argv":["sh","-c","echo started; i=0; while [ ! -e go ] && [ $i -lt 300 ]; do sleep 0.1; i=$((i+1)); done; if [ -e go ]; then echo done; else echo timeout; fi"]}"#.utf8)
    q.setValue("Bearer tok", forHTTPHeaderField: "authorization")
    q.setValue(ch.id, forHTTPHeaderField: "x-native-id"); q.setValue(proof, forHTTPHeaderField: "x-native-proof")
    let start = Date()
    var first = -1.0, last = "", lines: [String] = []
    let (bytes, _) = try await URLSession.shared.bytes(for: q)
    for try await l in bytes.lines {
        if first < 0 {
            first = Date().timeIntervalSince(start)
            FileManager.default.createFile(atPath: f.project.appendingPathComponent("go").path, contents: Data())
        }
        if l != "x 0" { lines.append(l) }
        last = l
    }
    #expect(first >= 0 && lines == ["o c3RhcnRlZAo=", "o ZG9uZQo="], "lines \(lines)")  // brez nosniff: »started« šele ob koncu, ukaz pa »timeout«
    #expect(last == "x 0")
}

/// 0.5.24 round 1 B: xcrun v run brez predpomnilnika (xcrun_nocache) je ~1,5 s na klic, Swift Build pa ga kliče ~23-krat
/// na gradnjo (vtool -show-build XCTest/Testing vseh platform, --show-sdk-platform-path …): ponovna gradnja ~36 s.
/// Predpomnilnik zagona v runTmp: nekaj sekund.
@Test func runSwiftBuildIsNotSlowedByColdXcrun() async throws {
    try #require(FileManager.default.isExecutableFile(atPath: "/usr/bin/xcrun"))
    let f = try nativeFixture()
    defer { try? FileManager.default.removeItem(at: f.root) }
    let pkg = f.project
    try FileManager.default.createDirectory(at: pkg.appendingPathComponent("Sources/Hello"), withIntermediateDirectories: true)
    try #"// swift-tools-version:5.9\#nimport PackageDescription\#nlet package = Package(name: "Hello", targets: [.target(name: "Hello"), .testTarget(name: "HelloTests", dependencies: ["Hello"])])\#n"#
        .write(to: pkg.appendingPathComponent("Package.swift"), atomically: true, encoding: .utf8)
    try FileManager.default.createDirectory(at: pkg.appendingPathComponent("Tests/HelloTests"), withIntermediateDirectories: true)
    try "public let hi = 1".write(to: pkg.appendingPathComponent("Sources/Hello/Hello.swift"), atomically: true, encoding: .utf8)
    try "import XCTest\nimport Hello\nfinal class T: XCTestCase { func testA() { XCTAssertEqual(hi, 1) } }\n"
        .write(to: pkg.appendingPathComponent("Tests/HelloTests/T.swift"), atomically: true, encoding: .utf8)
    let on = nativeRouter(f.project.path, devices: [DeviceConfig(id: "dev-xcrun", name: "Air", run: true)], commands: "swift")
    func build() async -> (FakeWriter, TimeInterval) {
        let c = await challenge(on, f, device: "dev-xcrun")
        let proof = Native.readMarker(f.project.appendingPathComponent(c.file)) ?? ""
        let w = FakeWriter(), start = Date()
        await on.handle(nativeReq("POST", "/minigw/run", ["x-native-id": c.id, "x-native-proof": proof], #"{"argv":["swift","build","--build-tests"]}"#,
                                  device: "dev-xcrun"), writer: w)
        return (w, Date().timeIntervalSince(start))
    }
    let (first, _) = await build()
    #expect(first.text.hasSuffix("x 0\n"), "\(first.text)")
    let (second, t2) = await build()  // nič za prevesti: le odkrivanje orodij (xcrun) in ~5,5 s pospravljanja run
    #expect(second.text.hasSuffix("x 0\n") && t2 < 20, "second build took \(t2) s")
}

@Test func sharedFolderAddSameIdentity() throws {
    let fm = FileManager.default
    let root = fm.temporaryDirectory.appendingPathComponent(UUID().uuidString)
    try fm.createDirectory(at: root, withIntermediateDirectories: true)
    defer { try? fm.removeItem(at: root) }
    let proj = root.appendingPathComponent("proj"), alias = root.appendingPathComponent("alias")
    try fm.createDirectory(at: proj, withIntermediateDirectories: true)
    try fm.createSymbolicLink(at: alias, withDestinationURL: proj)
    let saved = SharedFolders.Entry(path: proj.path)
    let updated = SharedFolders.adding(.init(path: alias.path, work: "sync"), to: [saved, .init(path: alias.path)])
    #expect(updated == [.init(path: proj.path, work: "sync")])
    #expect(SharedFolders.adding(.init(path: alias.path), to: updated) == [saved])
    let dataAlias = "/System/Volumes/Data" + proj.path
    if SharedFolders.fileID(dataAlias) == SharedFolders.fileID(proj.path) {
        #expect(SharedFolders.adding(.init(path: dataAlias), to: [saved]) == [saved])
    }
}
