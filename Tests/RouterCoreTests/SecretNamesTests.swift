import Foundation
import Testing
@testable import RouterCore

extension MemorySecrets: SecretVault { var place: String { "memory (test double)" } }

/// client/testdata/secret-names.json is read by Go (client/secretstore_test.go) too: the Mac app, the minigw helper and the Go
/// gateway must agree on item names. Nothing here touches the real Keychain.
@Suite struct SecretNamesTests {
    struct Fixture: Decodable {
        struct KC: Decodable { var appService, minigwService: String; var appAccounts, minigwAccounts: [String]; var upstreamAccount: String; var accessGroup: String? }
        var upstream: String
        var keychain: KC
    }
    static let root = URL(fileURLWithPath: #filePath).deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent()
    static func fixture() throws -> Fixture {
        try JSONDecoder().decode(Fixture.self, from: Data(contentsOf: root.appendingPathComponent("client/testdata/secret-names.json")))
    }
    static func source(_ p: String) throws -> String { try String(contentsOf: root.appendingPathComponent(p), encoding: .utf8) }

    @Test func keychainServiceAndAccountsMatchTheFixture() throws {
        let f = try Self.fixture().keychain
        #expect(Keychain().service == f.appService)
        #expect(f.accessGroup == nil)  // no kSecAttrAccessGroup anywhere: sharing is the access rule of setShared
        let helper = try Self.source("Sources/MiniGW/main.swift")
        #expect(helper.contains("let service = \"\(f.minigwService)\""))
        for a in ["router", "device"] { #expect(helper.contains("\"\(a)\"")) }
        #expect(helper.contains("\"router:\""))
        let app = try Self.source("Sources/MiniGateway/App.swift")
        for a in f.appAccounts { #expect(app.contains("\"\(a)\"")) }
        #expect(f.minigwAccounts == ["router", "router:<profile>", "device"])
    }

    @Test func upstreamAccountMatchesGo() throws {
        let f = try Self.fixture()
        #expect(UpstreamHost.account("x") == f.upstream.replacingOccurrences(of: "<name>", with: "x"))
        #expect(UpstreamHost.account("x") == f.keychain.upstreamAccount.replacingOccurrences(of: "<name>", with: "x"))
    }

    @Test func theKeychainPlaceNamesTheServiceAndNoValue() {
        #expect(Keychain().place == "Keychain (service dev.trojar.minigateway)")
        #expect(Keychain(loginOnly: true).place == "Keychain (login keychain, service dev.trojar.minigateway)")
    }

    @Test func theDoubleIsAVaultAndAFailureIsReportedNotReplaced() {
        let v: SecretVault = MemorySecrets()
        #expect(v.write("upstream:hub", "VALUE-1") && v.read("upstream:hub") == .found("VALUE-1"))
        #expect(!v.place.contains("VALUE-1"))
        (v as! MemorySecrets).locked = true
        #expect(v.read("upstream:hub") == .unreadable && !v.write("upstream:hub", "VALUE-2"))
        (v as! MemorySecrets).locked = false
        #expect(v.read("upstream:hub") == .found("VALUE-1"))  // the failed write left the old value, not a copy elsewhere
    }
}
