import Foundation
import Testing
@testable import RouterCore

// Direct mode (WebDAV) lets the agent write git's own data and nothing that runs code or redirects git; sync keeps every .git
// refused. The table is client/testdata/gitguard.json, asserted by Go too (client/gitguard_test.go).
private struct GitGuardCases: Decodable {
    struct Op: Decodable { let m: String; let p: String; let d: String?; let ok: Bool }
    let paths: [[JSONValue]], setup: [String], ops: [Op]

    enum JSONValue: Decodable {
        case s(String), b(Bool)
        init(from d: Decoder) throws {
            let c = try d.singleValueContainer()
            if let b = try? c.decode(Bool.self) { self = .b(b) } else { self = .s(try c.decode(String.self)) }
        }
        var string: String { if case .s(let s) = self { return s } else { return "" } }
        var bool: Bool { if case .b(let b) = self { return b } else { return false } }
    }

    static func load() throws -> GitGuardCases {
        let root = URL(fileURLWithPath: #filePath).deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent()
        return try JSONDecoder().decode(GitGuardCases.self, from: Data(contentsOf: root.appendingPathComponent("client/testdata/gitguard.json")))
    }
}

@Test func gitGuardPathsMatchTheSharedTable() throws {
    let c = try GitGuardCases.load()
    #expect(!c.paths.isEmpty)
    for r in c.paths {
        let p = r[0].string
        #expect(!Sync.isProtected(p, git: true) == r[1].bool, "direct \(p)")
        #expect(!Sync.isProtected(p) == r[2].bool, "sync \(p)")
    }
}

@Test func gitGuardWebDAVMatchesTheSharedTable() throws {
    let c = try GitGuardCases.load()
    let fm = FileManager.default
    func enc(_ rel: String) -> String { "/s/" + rel.split(separator: "/").map { $0.addingPercentEncoding(withAllowedCharacters: .urlPathAllowed)! }.joined(separator: "/") }
    for o in c.ops {
        let root = try #require(Sync.real(fm.temporaryDirectory)).appendingPathComponent("gg-" + UUID().uuidString)
        defer { try? fm.removeItem(at: root) }
        try fm.createDirectory(at: root, withIntermediateDirectories: true)
        for e in c.setup {
            let kind = e.prefix(1), rest = String(e.dropFirst(2))
            let u = root.appendingPathComponent(rest.split(separator: "=").first.map(String.init) ?? rest)
            switch kind {
            case "d": try fm.createDirectory(at: u, withIntermediateDirectories: true)
            case "f": try fm.createDirectory(at: u.deletingLastPathComponent(), withIntermediateDirectories: true); try Data("x".utf8).write(to: u)
            default: try fm.createSymbolicLink(atPath: u.path, withDestinationPath: String(rest.split(separator: "=")[1]))
            }
        }
        var h: [String: String] = [:]
        if let d = o.d { h["destination"] = enc(d) }
        let r = WebDAV.handle(method: o.m, path: enc(o.p), headers: h, body: Data("body".utf8), root: root, prefix: "/s")
        #expect((r.status == 403) != o.ok, "\(o.m) \(o.p) \(o.d ?? ""): status \(r.status), want ok=\(o.ok)")
    }
}

// git's own lock-file-then-rename: index.lock → index works in direct mode, config.tmp → config does not; sync keeps both refused.
@Test func gitGuardSyncStaysClosed() throws {
    let fm = FileManager.default
    let root = try #require(Sync.real(fm.temporaryDirectory)).appendingPathComponent("gg-" + UUID().uuidString)
    defer { try? fm.removeItem(at: root) }
    try fm.createDirectory(at: root.appendingPathComponent(".git/refs"), withIntermediateDirectories: true)
    try Data("x".utf8).write(to: root.appendingPathComponent(".git/index.lock"))
    let rt = try #require(ShareRoot(root))
    #expect(!rt.rename(".git/index.lock", ".git/index"))
    #expect(rt.mkdir(".git/refs/heads") != 0)
    rt.git = true
    #expect(rt.rename(".git/index.lock", ".git/index"))
    #expect(rt.mkdir(".git/refs/heads") == 0)
}
