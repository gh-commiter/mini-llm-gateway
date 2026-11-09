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
    try PluginSetup.edit(p) { s in r = try PluginSetup.add(&s); return r.changed }
    #expect(r == (true, false) && mode() == 0o600 && PluginSetup.isTrue((read()["enabledPlugins"] as? [String: Any])?[PluginSetup.id]))
    try PluginSetup.edit(p) { s in r = PluginSetup.remove(&s); return r.changed }
    #expect(r == (true, false) && (read()["enabledPlugins"] as? [String: Any])?.isEmpty == true && (read()["extraKnownMarketplaces"] as? [String: Any])?.isEmpty == true)

    // Uporabnikov false ostane, pravica 0640 ostane.
    try Data(#"{"model":"opus","enabledPlugins":{"minigw@mini-gateway":false}}"#.utf8).write(to: URL(fileURLWithPath: p))
    chmod(p, 0o640)
    try PluginSetup.edit(p) { s in r = try PluginSetup.add(&s); return r.changed }
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
