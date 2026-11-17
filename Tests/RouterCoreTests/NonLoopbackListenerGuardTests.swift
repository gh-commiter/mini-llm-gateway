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
    static let patterns = ["INADDR_ANY", "\"0.0.0.0\"", "\"::\"", "\"[::]\"", "getifaddrs", "AF_INET6", "in6addr_any"]

    static var testsDir: URL { URL(fileURLWithPath: #filePath).deletingLastPathComponent() }
    static let selfName = "NonLoopbackListenerGuardTests.swift"

    static func sources() throws -> [(name: String, text: String)] {
        try FileManager.default.contentsOfDirectory(atPath: testsDir.path).filter { $0.hasSuffix(".swift") && $0 != selfName }.sorted()
            .map { ($0, try String(contentsOf: testsDir.appendingPathComponent($0), encoding: .utf8)) }
    }

    @Test func noTestSourceListensOrBindsOutsideLoopbackWithoutAReason() throws {
        for (name, text) in try Self.sources() {
            let binds = text.contains(" bind(") || text.contains("{ bind(") || text.contains("NWListener")
            for p in Self.patterns where text.contains(p) {
                if p == "getifaddrs" && !binds { continue }  // reading addresses is harmless without a bind in the same file
                let ok = Self.allowed.contains { $0.file == name && $0.text == p }
                #expect(ok, "\(name) uses \(p): a test must not listen or bind outside 127.0.0.1; use TestNet (the signed mg-test-net), or list it in NonLoopbackListenerGuard.allowed with the reason")
            }
        }
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
