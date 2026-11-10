import Foundation
import Testing
@testable import RouterCore

/// minigw plugin install|uninstall: enako kot client/plugin_test.go (začasna mapa, nikoli prava ~/.claude).
@Test func pluginSettingsKeepUserFalseModeAndRefuseInvalid() throws {
    let dir = FileManager.default.temporaryDirectory.appendingPathComponent("plugin-\(UUID().uuidString)")
    try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
    defer { try? FileManager.default.removeItem(at: dir) }
    let p = dir.appendingPathComponent("settings.json").path
    func read() -> [String: Any] { (try? JSONSerialization.jsonObject(with: Data(contentsOf: URL(fileURLWithPath: p)))) as? [String: Any] ?? [:] }
    func mode() -> Int { (try? FileManager.default.attributesOfItem(atPath: p)[.posixPermissions] as? Int) ?? 0 }

    // Nova datoteka: 0600, vklopljen; odstranitev počisti oboje.
    var r: (changed: Bool, off: Bool) = (false, false)
    try PluginSetup.edit(p) { s in let a = try PluginSetup.add(&s); r = (a.changed, a.off); return r.changed }
    #expect(r == (true, false) && mode() == 0o600 && PluginSetup.isTrue((read()["enabledPlugins"] as? [String: Any])?[PluginSetup.id]))
    try PluginSetup.edit(p) { s in r = PluginSetup.remove(&s); return r.changed }
    #expect(r == (true, false) && (read()["enabledPlugins"] as? [String: Any])?.isEmpty == true && (read()["extraKnownMarketplaces"] as? [String: Any])?.isEmpty == true)

    // Uporabnikov false ostane, pravica 0640 ostane.
    try Data(#"{"model":"opus","enabledPlugins":{"minigw@mini-gateway":false}}"#.utf8).write(to: URL(fileURLWithPath: p))
    chmod(p, 0o640)
    try PluginSetup.edit(p) { s in let a = try PluginSetup.add(&s); r = (a.changed, a.off); return r.changed }
    #expect(r == (true, true) && mode() == 0o640 && read()["model"] as? String == "opus")
    #expect((read()["enabledPlugins"] as? [String: Any])?[PluginSetup.id] as? Bool == false)
    try PluginSetup.edit(p) { s in r = PluginSetup.remove(&s); return r.changed }
    #expect(r == (true, true) && (read()["enabledPlugins"] as? [String: Any])?[PluginSetup.id] as? Bool == false)

    // Neveljaven JSON ali ne-objekt: napaka, datoteka ostane (vejica na koncu kot v Go, tudi v gnezdenem seznamu).
    for bad in [#"{"a":1,}"#, #"{"a":[1, ]}"#, #"{"a":1"#, "[]", "null", "", #"{"a":1} x"#, #"{"enabledPlugins":[]}"#,
                #"{"enabledPlugins":1}"#, #"{"extraKnownMarketplaces":"x"}"#, #"{"extraKnownMarketplaces":[1],"enabledPlugins":{}}"#] {  // L7
        try Data(bad.utf8).write(to: URL(fileURLWithPath: p))
        #expect(throws: PluginSetup.Failure.self) { try PluginSetup.edit(p) { s in try PluginSetup.add(&s).changed } }
        #expect(String(decoding: FileManager.default.contents(atPath: p) ?? Data(), as: UTF8.self) == bad)
    }
}

/// #63: autoUpdate na našem vnosu; stari vnos (do 0.5.21) nadgradimo, uporabnikovega ne; oba sta naša za odstranitev
/// (kot Go TestPluginMarketplaceAutoUpdate).
@Test func pluginMarketplaceAutoUpdate() throws {
    let own: [String: Any] = ["source": ["source": "github", "repo": "gh-commiter/mini-llm-gateway", "ref": "release"], "autoUpdate": false]
    func settings(_ m: [String: Any]?) -> [String: Any] {
        var s: [String: Any] = ["enabledPlugins": [PluginSetup.id: true]]
        if let m { s["extraKnownMarketplaces"] = [PluginSetup.market: m] }
        return s
    }
    func market(_ s: [String: Any]) -> NSDictionary? { (s["extraKnownMarketplaces"] as? [String: Any])?[PluginSetup.market] as? NSDictionary }

    var s: [String: Any] = [:]  // nova
    var r = try PluginSetup.add(&s)
    #expect(r == (true, false, false) && PluginSetup.isTrue(market(s)?["autoUpdate"])
            && (market(s)?["source"] as? [String: Any])?["ref"] as? String == "release")
    s = settings(PluginSetup.oldMarketplace)  // nadgradnja starega
    r = try PluginSetup.add(&s)
    #expect(r == (false, true, false) && market(s)?.isEqual(to: PluginSetup.marketplace) == true)
    r = try PluginSetup.add(&s)  // drugič nič
    #expect(r == (false, false, false))
    s = settings(own)  // uporabnikov false ostane
    r = try PluginSetup.add(&s)
    #expect(r == (false, false, false) && market(s)?.isEqual(to: own) == true)
    #expect(PluginSetup.remove(&s).changed && market(s) != nil)
    var one = PluginSetup.marketplace; one["autoUpdate"] = 1  // 1 ni true (kot Go)
    #expect(!PluginSetup.ours(one) && PluginSetup.ours(PluginSetup.marketplace) && PluginSetup.ours(PluginSetup.oldMarketplace))
    for m in [PluginSetup.oldMarketplace, PluginSetup.marketplace] {  // odstranitev obeh naših
        s = settings(m)
        #expect(PluginSetup.remove(&s).changed && market(s) == nil && (s["enabledPlugins"] as? [String: Any])?.isEmpty == true)
    }
}

/// Vejica v nizu ni vejica na koncu.
@Test func pluginSettingsCommaInString() {
    #expect(!PluginSetup.trailingComma(Data(#"{"a":",}","b":"\",]"}"#.utf8)))
    #expect(PluginSetup.trailingComma(Data("{\"a\":1,\n}".utf8)))
}

/// L3: Homebrew (drwxrwxr-x uporabnik:admin) je varen; o+w ne, tudi z zaupano skupino (kot Go TestPluginDataSafeAndUnsafeDir).
@Test func pluginSafePathAcceptsAdminGroupRefusesOthersWrite() throws {
    let dir = FileManager.default.temporaryDirectory.appendingPathComponent("safe-\(UUID().uuidString)").path
    try FileManager.default.createDirectory(atPath: dir, withIntermediateDirectories: true)
    defer { try? FileManager.default.removeItem(atPath: dir) }
    #expect(PluginSetup.trustedGroup(80) && !PluginSetup.trustedGroup(20) && !PluginSetup.trustedGroup(12345))
    chmod(dir, 0o755)
    #expect(PluginSetup.safePath(dir))
    chmod(dir, 0o777)
    #expect(!PluginSetup.safePath(dir))
    if chown(dir, uid_t.max, 20) == 0 {  // staff: vsak lokalni uporabnik
        chmod(dir, 0o775)
        #expect(!PluginSetup.safePath(dir))
    }
    if chown(dir, uid_t.max, 80) == 0 {  // uid -1: lastnik ostane
        chmod(dir, 0o775)
        #expect(PluginSetup.safePath(dir))
        chmod(dir, 0o777)
        #expect(!PluginSetup.safePath(dir))
    }
    #expect(!PluginSetup.safePath("/tmp/.no-such-\(UUID().uuidString)"))
}

/// #48: varna mora biti vsaka nadrejena mapa do /, ne le neposredna (tudi prek povezave).
@Test func pluginSafeAncestorsChecksEveryParent() throws {
    let root = try #require(Sync.real(FileManager.default.temporaryDirectory)).appendingPathComponent("anc-\(UUID().uuidString)").path
    let bin = root + "/a/b/bin", exe = bin + "/minigw", link = root + "/link"
    try FileManager.default.createDirectory(atPath: bin, withIntermediateDirectories: true)
    defer { chmod(root + "/a", 0o755); try? FileManager.default.removeItem(atPath: root) }
    FileManager.default.createFile(atPath: exe, contents: Data())
    try FileManager.default.createSymbolicLink(atPath: link, withDestinationPath: exe)
    #expect(PluginSetup.safeAncestors(exe) && PluginSetup.safeAncestors(link))
    chmod(root + "/a", 0o777)  // dve ravni nad mapo minigw
    #expect(!PluginSetup.safeAncestors(exe) && !PluginSetup.safeAncestors(link))
    #expect(!PluginSetup.safeAncestors(root + "/missing"))
    chmod(root + "/a", 0o1777)  // sticky (kot /tmp): višje mape smejo
    #expect(PluginSetup.safeAncestors(exe))
    var st = stat()
    stat(root + "/a", &st)
    #expect(PluginSetup.stickyOK(st))
    st.st_uid = getuid() &+ 4242  // sticky mapa drugega uporabnika: ne (lastnik jo zamenja)
    #expect(!PluginSetup.stickyOK(st))
    st.st_uid = 0; st.st_mode &= ~mode_t(S_ISVTX)
    #expect(!PluginSetup.stickyOK(st))
    chmod(bin, 0o1777)  // mapa minigw sama: nikoli
    #expect(!PluginSetup.safeAncestors(exe))
    chmod(bin, 0o755)
    // povezava v nevarno mapo: prava pot šteje
    let bad = root + "/bad", badExe = bad + "/minigw", safeLink = root + "/a/b/bin/via"
    try FileManager.default.createDirectory(atPath: bad, withIntermediateDirectories: true)
    FileManager.default.createFile(atPath: badExe, contents: Data())
    chmod(bad, 0o777)
    try FileManager.default.createSymbolicLink(atPath: safeLink, withDestinationPath: badExe)
    #expect(!PluginSetup.safeAncestors(safeLink))
    chmod(bad, 0o755)
    // minigw v lastni mapi pod /tmp (sticky)
    let tmp = "/tmp/minigw-anc-\(UUID().uuidString)"
    try FileManager.default.createDirectory(atPath: tmp, withIntermediateDirectories: false, attributes: [.posixPermissions: 0o755])
    defer { try? FileManager.default.removeItem(atPath: tmp) }
    FileManager.default.createFile(atPath: tmp + "/minigw", contents: Data())
    #expect(PluginSetup.safeAncestors(tmp + "/minigw"))
}
