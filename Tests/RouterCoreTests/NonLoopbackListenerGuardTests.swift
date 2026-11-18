import Foundation
import Testing

/// A test process must not listen or bind outside 127.0.0.1: the macOS firewall asks the owner for permission for every unsigned program that accepts
/// connections. Such sockets go through the signed utility (TestNet, Sources/MGTestNet). This scan keeps it from coming back: any new INADDR_ANY, "0.0.0.0",
/// "::" or a getifaddrs address next to a bind in a test file fails here, unless it is on the list below with the reason. (Go tests: client/nonloopback_guard_test.go.)
@Suite struct NonLoopbackListenerGuard {
    /// file name, the text that is allowed there, why.
    static let allowed: [(file: String, text: String, why: String)] = [
        ("RouterCoreTests.swift", "\"0.0.0.0\"", "the address handed to the signed utility for a bind attempt (routerPortCannotBeSharedByAnotherListener); the test process binds nothing"),
        ("ClaudeSeatbeltTests.swift", "\"0.0.0.0\"", "the address handed to the signed utility that holds the listener (networkRowsLoopbackLanAndBind)"),
        ("ClaudeSeatbeltTests.swift", "getifaddrs", "reads this Mac's own addresses to PROBE them with nc -z; the listener is in the signed utility, this file binds nothing"),
    ]
    static let patterns = ["INADDR_ANY", "\"0.0.0.0\"", "\"::\"", "\"[::]\"", "getifaddrs", "AF_INET6", "in6addr_any", "Host.current()", "processInfo.hostName"]
    /// Outbound use of this Mac's own name or address (a connect to it asks the firewall too and depends on the network): flagged when the file also connects.
    static let ownAddress = ["getifaddrs", "Host.current()", "processInfo.hostName"]

    static func connects(_ text: String) -> Bool {
        ["connect(", "NWConnection", "URLSession", "URLRequest(", "CFStreamCreatePairWithSocketToHost"].contains { text.contains($0) }
    }

    /// The patterns of `text` that are a violation unless allowed (the scan below and its own planted-example test share this).
    static func hits(_ text: String) -> [String] {
        let binds = text.contains(" bind(") || text.contains("{ bind(") || text.contains("NWListener")
        return patterns.filter { p in
            guard text.contains(p) else { return false }
            if ownAddress.contains(p) { return binds || connects(text) }  // reading addresses is harmless without a bind or a connect in the same file
            return true
        }
    }

    static var testsDir: URL { URL(fileURLWithPath: #filePath).deletingLastPathComponent() }
    static let selfName = "NonLoopbackListenerGuardTests.swift"

    static func sources() throws -> [(name: String, text: String)] {
        try FileManager.default.contentsOfDirectory(atPath: testsDir.path).filter { $0.hasSuffix(".swift") && $0 != selfName }.sorted()
            .map { ($0, try String(contentsOf: testsDir.appendingPathComponent($0), encoding: .utf8)) }
    }

    @Test func noTestSourceListensOrBindsOutsideLoopbackWithoutAReason() throws {
        for (name, text) in try Self.sources() {
            for p in Self.hits(text) {
                let ok = Self.allowed.contains { $0.file == name && $0.text == p }
                #expect(ok, "\(name) uses \(p): a test must not listen or bind outside 127.0.0.1; use TestNet (the signed mg-test-net), or list it in NonLoopbackListenerGuard.allowed with the reason")
            }
        }
    }

    @Test func plantedExamplesAreCaughtAndDataIsNot() {
        #expect(Self.hits("let h = Host.current().name\nlet s = URLSession.shared").contains("Host.current()"))
        #expect(Self.hits("let h = ProcessInfo.processInfo.hostName\nlet c = NWConnection(host: h, port: 80, using: .tcp)").contains("processInfo.hostName"))
        #expect(Self.hits("getifaddrs(&a)\nconnect(fd, p, n)").contains("getifaddrs"))
        #expect(Self.hits("bind(fd, p, n)\nlet x = INADDR_ANY").contains("INADDR_ANY"))
        #expect(Self.hits("let name = ProcessInfo.processInfo.hostName").isEmpty)  // a name used as data, no connect in the file
        #expect(Self.hits("getifaddrs(&a)").isEmpty)
    }

    @Test func everyAllowedEntryStillMatchesAndHasAReason() throws {
        let all = Dictionary(uniqueKeysWithValues: try Self.sources().map { ($0.name, $0.text) })
        for e in Self.allowed {
            #expect(!e.why.isEmpty && all[e.file]?.contains(e.text) == true, "stale allow-list entry \(e.file) \(e.text): remove it")
        }
    }

    /// The CI fallback relies on this: `swift build` puts the utility next to the test bundle, in either build system's layout.
    @Test func theBuiltUtilityIsFoundNextToTheTestBundle() {
        #expect(TestNet.builtNextToTests() != nil, "mg-test-net not next to the test bundle (\(TestNet.testImageDirectory() ?? "unknown")): build the package first")
    }
}
