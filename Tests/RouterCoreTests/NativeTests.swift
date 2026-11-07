import CryptoKit
import Foundation
import Testing
@testable import RouterCore

@Test func nativeSMBPathFromMount() {
    let a = Native.smbPath(source: "//uros@mini._smb._tcp.local/Java", mountPoint: "/Volumes/Java", cwd: "/Volumes/Java/eureko-modules/apps")
    #expect(a?.share == "Java" && a?.path == "eureko-modules/apps")
    let b = Native.smbPath(source: "//u@h/Uros%20Trojar%E2%80%99s%20Public%20Folder/sub", mountPoint: "/Volumes/Public", cwd: "/Volumes/Public")
    #expect(b?.share == "Uros Trojar’s Public Folder" && b?.path == "sub")
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
    """
    #expect(Native.smbShares(listing) == ["Java": "/Volumes/Java"])
}

/// Mapa »share« (kot /Volumes/Java), projekt v njej, mapa zunaj dovoljenih in povezava ven.
private func nativeFixture() throws -> (root: URL, share: URL, project: URL, outside: URL) {
    let root = try #require(Sync.real(FileManager.default.temporaryDirectory)).appendingPathComponent("native-" + UUID().uuidString)
    let share = root.appendingPathComponent("Java"), project = share.appendingPathComponent("eureko-modules")
    let outside = share.appendingPathComponent("private")
    for d in [project, outside, root.appendingPathComponent("elsewhere")] { try FileManager.default.createDirectory(at: d, withIntermediateDirectories: true) }
    try FileManager.default.createSymbolicLink(at: project.appendingPathComponent("link"), withDestinationURL: root.appendingPathComponent("elsewhere"))
    return (root, share, project, outside)
}

@Test func nativeResolveStaysInShareAndAllowedFolders() throws {
    let f = try nativeFixture()
    defer { try? FileManager.default.removeItem(at: f.root) }
    let shares = ["Java": f.share.path], roots = f.project.path
    #expect(Native.resolve(share: "java", path: "eureko-modules", shares: shares, roots: roots)?.path == f.project.path)  // brez razlike v črkah
    #expect(Native.resolve(share: "Java", path: "private", shares: shares, roots: roots) == nil)  // ni v dovoljenih mapah
    #expect(Native.resolve(share: "Java", path: "", shares: shares, roots: roots) == nil)
    #expect(Native.resolve(share: "Java", path: "eureko-modules/../private", shares: shares, roots: f.share.path) == nil)
    #expect(Native.resolve(share: "Java", path: "eureko-modules/link", shares: shares, roots: f.root.path) == nil)  // povezava ven iz sheme
    #expect(Native.resolve(share: "Other", path: "eureko-modules", shares: shares, roots: roots) == nil)
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
private func challenge(_ r: Router, _ f: (root: URL, share: URL, project: URL, outside: URL), path: String = "eureko-modules",
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
        await r.handle(nativeReq("POST", "/minigw/share", ["x-claude-mini-session": session, "x-claude-mini-cwd": "/Volumes/Java/eureko-modules",
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
    #expect(await Router.shares.mount(session: session, cwd: nil, base: "http://127.0.0.1:1")?.path == f.project.path)
    #expect(await register(c.id, proof, session: UUID().uuidString).status == 403)  // izziv velja enkrat

    // Ista pot, druga mapa (lokalna mapa z enakim imenom, ne priklop routerja): označbe tam ni → ni dokaza.
    let spoofDir = f.root.appendingPathComponent("elsewhere/Java/eureko-modules")
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
    var remote = nativeReq("POST", "/minigw/native", [:], #"{"share":"Java","path":"eureko-modules"}"#)
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
    #expect(await admin(app.router(), "PATCH", "/run", body: #"{"allow":["nope"]}"#).status == 400)
    #expect(await admin(app.router(), "PATCH", "/run", body: #"{"run":true}"#).status == 400)  // neznano polje
    #expect(await admin(app.router(), "GET", "/run", auth: nil).status == 401)
}

@Test func sharedFoldersKeepModePerFolder() {
    let old = SharedFolders.parse("/Volumes/Data/a\n\n  /Volumes/Data/b  \n")  // starejša datoteka: le poti
    #expect(old == [SharedFolders.Entry(path: "/Volumes/Data/a"), SharedFolders.Entry(path: "/Volumes/Data/b")])
    let list = [SharedFolders.Entry(path: "/Volumes/Java/eureko-modules", work: "native"),
                SharedFolders.Entry(path: "/Volumes/Java", work: "sync", readOnly: true),
                SharedFolders.Entry(path: "/Volumes/Java/my folder")]
    let text = SharedFolders.format(list)
    #expect(text == "/Volumes/Java/eureko-modules\t--work=native\n/Volumes/Java\t--work=sync --ro\n/Volumes/Java/my folder\n")
    #expect(SharedFolders.parse(text) == list)
    #expect(SharedFolders.parse("/p\t--work=bogus --ro") == [SharedFolders.Entry(path: "/p", readOnly: true)])

    let apps = SharedFolders.lookup(list, cwd: "/Volumes/Java/eureko-modules/apps")  // najbolj natančna mapa
    #expect(apps?.work == "native")
    #expect(SharedFolders.lookup(list, cwd: "/Volumes/Java/other")?.work == "sync")
    #expect(SharedFolders.lookup(list, cwd: "/Volumes/JavaX") == nil)
    #expect(SharedFolders.mode(flag: nil, saved: apps, readOnly: false) == ("native", false))
    #expect(SharedFolders.mode(flag: "sync", saved: apps, readOnly: false) == ("sync", false))  // zastavica zmaga
    #expect(SharedFolders.mode(flag: nil, saved: nil, readOnly: false) == ("direct", false))
    let java = SharedFolders.lookup(list, cwd: "/Volumes/Java/x")
    #expect(SharedFolders.mode(flag: nil, saved: java, readOnly: false) == ("sync", true))
    #expect(SharedFolders.mode(flag: "native", saved: java, readOnly: false) == ("native", false))  // shranjen --ro ne velja za native
}
