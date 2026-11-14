import Foundation
import Testing
@testable import RouterCore

/// #107 (kot client/root_test.go): povezave v deljeni mapi in operacije prek odprtega korena. Vse v začasnih mapah.
private func tmp(_ name: String, resolved: Bool = true) throws -> URL {
    let u = FileManager.default.temporaryDirectory.appendingPathComponent("minigw-test-\(name)-" + UUID().uuidString)
    try FileManager.default.createDirectory(at: u, withIntermediateDirectories: true)
    return resolved ? Sync.real(u)! : u  // /private/var/…, kot F_GETPATH
}
private func put(_ s: String, _ u: URL) throws { try Data(s.utf8).write(to: u) }
private func read(_ u: URL) -> String? { (try? Data(contentsOf: u)).map { String(decoding: $0, as: UTF8.self) } }
private func link(_ target: String, _ at: URL) throws { try FileManager.default.createSymbolicLink(atPath: at.path, withDestinationPath: target) }
private func isLink(_ u: URL) -> Bool { (try? FileManager.default.destinationOfSymbolicLink(atPath: u.path)) != nil }
private func gone(_ u: URL) -> Bool { (try? FileManager.default.attributesOfItem(atPath: u.path)) == nil }
private func dav(_ root: URL, _ m: String, _ p: String, _ h: [String: String] = [:], _ b: String = "") -> DAVResponse {
    WebDAV.handle(method: m, path: "/s" + p, headers: h, body: Data(b.utf8), root: root, prefix: "/s")
}
private func text(_ r: DAVResponse) -> String { String(decoding: r.body, as: UTF8.self) }
private func entry(_ p: String, _ data: String) -> Sync.Entry { Sync.Entry(path: p, size: Int64(data.utf8.count), mtime: 1, sha256: Sync.sha256(Data(data.utf8))) }

@Test func shareRelativeLinkInside() throws {
    let root = try tmp("root")
    try FileManager.default.createDirectory(at: root.appendingPathComponent("d"), withIntermediateDirectories: true)
    try link("d", root.appendingPathComponent("link"))
    #expect(dav(root, "PUT", "/link/x", [:], "inside").status == 201)
    #expect(read(root.appendingPathComponent("d/x")) == "inside")
    let r = dav(root, "GET", "/link/x")
    #expect(r.status == 200 && text(r) == "inside")
    #expect(dav(root, "MKCOL", "/link/sub").status == 201)
    #expect(dav(root, "COPY", "/link/x", ["destination": "/s/link/sub/y"]).status == 201)
    #expect(read(root.appendingPathComponent("d/sub/y")) == "inside")
}

/// Absolutna povezava v mapi, zapisana z drugim imenom mape (/var/… namesto /private/var/…), in veriga povezav.
@Test func shareAbsoluteLinkInsideAndChain() throws {
    let root = try tmp("root", resolved: false)  // /var/folders/… je povezava na /private/var/…
    try FileManager.default.createDirectory(at: root.appendingPathComponent("d"), withIntermediateDirectories: true)
    try put("old", root.appendingPathComponent("d/old"))
    try link(root.appendingPathComponent("d").path, root.appendingPathComponent("abs"))
    try link("abs", root.appendingPathComponent("rel"))
    try link(root.appendingPathComponent("rel").path, root.appendingPathComponent("b"))
    for l in ["abs", "rel", "b"] {
        #expect(text(dav(root, "GET", "/\(l)/old")) == "old", "\(l)")
        #expect([201, 204].contains(dav(root, "PUT", "/\(l)/x", [:], l).status), "\(l)")
    }
    #expect(read(root.appendingPathComponent("d/x")) == "b")
    // PROPFIND: povezava na mapo je zbirka, otroci so navedeni pod imenom povezave.
    let list = text(dav(root, "PROPFIND", "/abs", ["depth": "1"]))
    #expect(list.contains("/s/abs/x") && list.contains("/s/abs/old"))
    #expect(dav(root, "MOVE", "/abs/x", ["destination": "/s/abs/y"]).status == 201)
    #expect(read(root.appendingPathComponent("d/y")) == "b")
    // DELETE povezave odstrani povezavo, ne cilja.
    #expect(dav(root, "DELETE", "/abs").status == 204)
    #expect(gone(root.appendingPathComponent("abs")) && read(root.appendingPathComponent("d/old")) == "old")
}

@Test func shareLinkLeavingTheShareIsRefusedWithReason() throws {
    let root = try tmp("root"), out = try tmp("out")
    try put("secret", out.appendingPathComponent("secret"))
    try link(out.path, root.appendingPathComponent("dir"))                          // absolutna, ven
    try link("../" + out.lastPathComponent, root.appendingPathComponent("rel"))     // relativna, ven (nikoli v mapi)
    try link(out.appendingPathComponent("secret").path, root.appendingPathComponent("file"))
    try link(out.appendingPathComponent("nothing").path, root.appendingPathComponent("dangling-out"))
    for (m, p) in [("GET", "/dir/secret"), ("PUT", "/dir/x"), ("MKCOL", "/dir/n"), ("DELETE", "/dir/secret"), ("PROPFIND", "/dir"),
                   ("GET", "/rel/secret"), ("PUT", "/rel/x"), ("GET", "/file"), ("GET", "/dangling-out"), ("PUT", "/dangling-out")] {
        let r = dav(root, m, p, [:], "bad")
        #expect(r.status == 403, "\(m) \(p)")
        #expect(text(r).contains("links that lead outside the shared folder are not followed"), "\(m) \(p): \(text(r))")
    }
    #expect(read(out.appendingPathComponent("secret")) == "secret")
    #expect((try? FileManager.default.contentsOfDirectory(atPath: out.path)) == ["secret"])
    // PROPFIND mape: povezava, ki zapusti mapo, ni datoteka z dolžino cilja povezave.
    let list = text(dav(root, "PROPFIND", "/", ["depth": "1"]))
    #expect(list.contains("/s/file") && !list.contains("<D:getcontentlength>\(out.path.utf8.count)<"))
}

@Test func shareDanglingLinkListsButDoesNotRead() throws {
    let root = try tmp("root")
    try link("missing", root.appendingPathComponent("dangling"))
    try link(root.appendingPathComponent("also-missing").path, root.appendingPathComponent("dangling-abs"))
    let list = text(dav(root, "PROPFIND", "/", ["depth": "1"]))
    #expect(list.contains("/s/dangling<") && list.contains("/s/dangling-abs<"))
    #expect(list.contains("<D:getcontentlength>0<") && !list.contains("<D:getcontentlength>7<"))
    for p in ["/dangling", "/dangling-abs"] { #expect(dav(root, "GET", p).status >= 400, "\(p)") }
    #expect(dav(root, "DELETE", "/dangling").status == 204)  // povezava sama gre
    #expect(gone(root.appendingPathComponent("dangling")) && !gone(root.appendingPathComponent("dangling-abs")))
}

@Test func shareLinkLoopStops() throws {
    let root = try tmp("root")
    try link(root.appendingPathComponent("b").path, root.appendingPathComponent("a"))
    try link(root.appendingPathComponent("a").path, root.appendingPathComponent("b"))
    for m in ["PUT", "GET"] { #expect(dav(root, m, "/a/x", [:], "x").status >= 400) }
    let rt = try #require(ShareRoot(root))
    #expect(throws: ShareRoot.Refused.self) { try rt.resolve("a/x", followLast: true) }
}

@Test func shareLinkIntoProtectedFolderIsRefused() throws {
    let root = try tmp("root")
    for d in [".claude", ".git"] { try FileManager.default.createDirectory(at: root.appendingPathComponent(d), withIntermediateDirectories: true) }
    try put("n", root.appendingPathComponent(".claude/notes.md"))
    try link(root.appendingPathComponent(".claude").path, root.appendingPathComponent("abs"))
    try link(".claude", root.appendingPathComponent("rel"))
    try link(root.appendingPathComponent(".git").path, root.appendingPathComponent("git"))
    for l in ["abs", "rel"] {
        for (m, p) in [("PUT", "settings.json"), ("PUT", "hooks"), ("MKCOL", "hooks"), ("PUT", "commands/x")] {
            #expect(dav(root, m, "/\(l)/\(p)", [:], "{}").status >= 400, "\(m) \(l)/\(p)")
        }
    }
    #expect(dav(root, "PUT", "/git/config", [:], "x").status >= 400)
    try put("ok", root.appendingPathComponent("ok"))
    #expect(dav(root, "MOVE", "/ok", ["destination": "/s/abs/settings.json"]).status >= 400)
    for p in [".claude/settings.json", ".claude/hooks", ".git/config"] { #expect(gone(root.appendingPathComponent(p)), "\(p)") }
    #expect(dav(root, "GET", "/abs/notes.md").status == 200)  // branje ostane
    // Sync: zapis in izbris prek povezave.
    var state: [String: Sync.Entry] = [:]
    let (notes, done) = Sync.apply(Sync.Changes(changed: [entry("abs/settings.json", "{}"), entry("git/config", "{}")], deleted: ["abs/settings.local.json"]),
                                   root: root, state: &state) { _ in Data("{}".utf8) }
    #expect(notes.count == 3 && done.count == 3, "\(notes) \(done)")
    #expect(gone(root.appendingPathComponent(".claude/settings.json")) && gone(root.appendingPathComponent(".git/config")))
}

/// MOVE in DELETE delata na povezavi sami (datoteka in mapa), cilj ostane.
@Test func shareMoveAndDeleteActOnTheLinkItself() throws {
    let root = try tmp("root")
    try FileManager.default.createDirectory(at: root.appendingPathComponent("d"), withIntermediateDirectories: true)
    try put("data", root.appendingPathComponent("f"))
    try link("f", root.appendingPathComponent("fl"))
    try link("d", root.appendingPathComponent("dl"))
    #expect(dav(root, "MOVE", "/fl", ["destination": "/s/fl2"]).status == 201)
    #expect(isLink(root.appendingPathComponent("fl2")) && gone(root.appendingPathComponent("fl")) && read(root.appendingPathComponent("f")) == "data")
    #expect(dav(root, "MOVE", "/dl", ["destination": "/s/dl2"]).status == 201)
    #expect(isLink(root.appendingPathComponent("dl2")))
    #expect(dav(root, "DELETE", "/fl2").status == 204 && dav(root, "DELETE", "/dl2").status == 204)
    #expect(gone(root.appendingPathComponent("fl2")) && gone(root.appendingPathComponent("dl2")))
    #expect(read(root.appendingPathComponent("f")) == "data" && !gone(root.appendingPathComponent("d")))
    // Cilj MOVE je povezava: zamenjana je povezava, ne njen cilj.
    try link("f", root.appendingPathComponent("fl"))
    try put("new", root.appendingPathComponent("n"))
    #expect(dav(root, "MOVE", "/n", ["destination": "/s/fl"]).status == 204)
    #expect(read(root.appendingPathComponent("f")) == "data" && read(root.appendingPathComponent("fl")) == "new" && !isLink(root.appendingPathComponent("fl")))
}

/// PROPFIND: vrsta in velikost povezav (napaka iz 0.5.26: povezava je bila datoteka z dolžino cilja povezave).
@Test func sharePropfindTypeAndSizeOfLinks() throws {
    let root = try tmp("root")
    try FileManager.default.createDirectory(at: root.appendingPathComponent("d"), withIntermediateDirectories: true)
    try put("12345678901", root.appendingPathComponent("d/file"))
    try link("d/file", root.appendingPathComponent("flink"))
    try link(root.appendingPathComponent("d").path, root.appendingPathComponent("dlink"))
    let xml = text(dav(root, "PROPFIND", "/", ["depth": "1"]))
    let resp = xml.components(separatedBy: "<D:response>").dropFirst()
    func part(_ name: String) -> String { resp.first { $0.contains("<D:href>/s/\(name)") } ?? "" }
    #expect(part("flink").contains("<D:getcontentlength>11<") && !part("flink").contains("<D:collection/>"))
    #expect(part("dlink").contains("<D:collection/>") && part("dlink").contains("<D:href>/s/dlink/</D:href>"))
    #expect(dav(root, "HEAD", "/flink").status == 200 && dav(root, "HEAD", "/flink").headers["Content-Length"] == "11")
}

/// Starš se po preverjanju zamenja s povezavo: operacija mora zavrniti (kot Go TestDavRefusesProtectedParentSwappedBeforeMutation).
@Test func shareParentSwappedBeforeMutationIsRefused() throws {
    for method in ["PUT", "MKCOL", "DELETE", "LOCK", "MOVE", "COPY"] {
        let root = try tmp("root"), out = try tmp("out")
        let fm = FileManager.default
        try fm.createDirectory(at: root.appendingPathComponent("d"), withIntermediateDirectories: true)
        try fm.createDirectory(at: root.appendingPathComponent(".claude"), withIntermediateDirectories: true)
        try put("keep", out.appendingPathComponent("x"))
        try put("keep", root.appendingPathComponent("d/x"))
        try put("src", root.appendingPathComponent("src"))
        for target in [out.path, root.appendingPathComponent(".claude").path] {
            try fm.createDirectory(at: root.appendingPathComponent("d"), withIntermediateDirectories: true)
            try put("keep", root.appendingPathComponent("d/x"))
            WebDAV.beforeMutation = { r in
                guard r == root.path else { return }
                try? fm.moveItem(at: root.appendingPathComponent("d"), to: root.appendingPathComponent("old\(UUID().uuidString)"))
                try? fm.createSymbolicLink(atPath: root.appendingPathComponent("d").path, withDestinationPath: target)
            }
            defer { WebDAV.beforeMutation = nil; try? fm.removeItem(at: root.appendingPathComponent("d")) }
            let r: DAVResponse
            switch method {
            case "MOVE", "COPY": r = dav(root, method, "/src", ["destination": "/s/d/moved"])
            case "DELETE": r = dav(root, method, "/d/x")
            default: r = dav(root, method, "/d/new", [:], "bad")
            }
            #expect(isLink(root.appendingPathComponent("d")), "\(method): the swap did not happen")
            #expect(r.status >= 400, "\(method) -> \(target): \(r.status)")
            #expect((try? fm.contentsOfDirectory(atPath: out.path)) == ["x"] && read(out.appendingPathComponent("x")) == "keep", "\(method) outside")
            #expect((try? fm.contentsOfDirectory(atPath: root.appendingPathComponent(".claude").path)) == [], "\(method) .claude")
        }
    }
}

/// Neposredno prek korena (kot Go TestRootOperationsRefuseSwappedParent): pot izbrana, nato starš zamenjan s povezavo.
@Test func shareRootOperationsRefuseSwappedParent() throws {
    let root = try tmp("root"), out = try tmp("out"), fm = FileManager.default
    try fm.createDirectory(at: root.appendingPathComponent("d"), withIntermediateDirectories: true)
    try put("outside", out.appendingPathComponent("x"))
    try put("inside", root.appendingPathComponent("source"))
    let rt = try #require(ShareRoot(root))
    let chosen = try rt.resolve("d/x", followLast: true)
    try fm.moveItem(at: root.appendingPathComponent("d"), to: root.appendingPathComponent("old"))
    try link(out.path, root.appendingPathComponent("d"))
    #expect(rt.writeAtomic(chosen, Data("bad".utf8), mode: nil, mtime: nil) != 0)
    #expect(!rt.copyTree("d/x", "copy") && !rt.copyTree("source", "d/copy"))
    #expect(!rt.rename("d/x", "moved") && !rt.rename("source", "d/moved"))
    #expect(!rt.removeAll("d/x") && !rt.remove("d/x"))
    #expect(rt.mkdir("d/new") != 0 && rt.create("d/new") == nil)
    #expect(rt.openRead("d/x") == nil && rt.list("d") == nil && !rt.chmod("d/x", 0o777))
    #expect(Sync.write(Data("bad".utf8), "d/x", in: rt, mtime: 0, exec: false) != nil)
    #expect(Sync.entry(rt, "d/x", path: "d/x", previous: nil) == nil)
    #expect(read(out.appendingPathComponent("x")) == "outside" && (try? fm.contentsOfDirectory(atPath: out.path)) == ["x"])
}

@Test func shareSyncApplyThroughLinks() throws {
    let root = try tmp("root"), out = try tmp("out")
    try FileManager.default.createDirectory(at: root.appendingPathComponent("d"), withIntermediateDirectories: true)
    try link("d", root.appendingPathComponent("rel"))
    try link(root.appendingPathComponent("d").path, root.appendingPathComponent("abs"))
    try link(out.path, root.appendingPathComponent("out"))
    var state: [String: Sync.Entry] = [:]
    var (notes, done) = Sync.apply(Sync.Changes(changed: [entry("rel/a", "A"), entry("abs/b", "B")]), root: root, state: &state) { p in Data((p == "rel/a" ? "A" : "B").utf8) }
    #expect(notes.isEmpty && done.count == 2, "\(notes)")
    #expect(read(root.appendingPathComponent("d/a")) == "A" && read(root.appendingPathComponent("d/b")) == "B")
    (notes, done) = Sync.apply(Sync.Changes(changed: [entry("out/new", "bad")]), root: root, state: &state) { _ in Data("bad".utf8) }
    #expect(done == ["out/new"] && notes.count == 1)  // potrjena (sicer bi jo router pošiljal v nedogled), ne zapisana
    #expect(gone(out.appendingPathComponent("new")))
    // Izbris poti prek povezave odstrani ciljno datoteko, izbris same povezave odstrani povezavo.
    state["rel/a"] = entry("rel/a", "A")
    _ = Sync.apply(Sync.Changes(deleted: ["rel/a"], base: ["rel/a": Sync.sha256(Data("A".utf8))]), root: root, state: &state) { _ in nil }
    #expect(gone(root.appendingPathComponent("d/a")))
    try put("x", root.appendingPathComponent("d/keep")); try link("d/keep", root.appendingPathComponent("fl"))
    _ = Sync.apply(Sync.Changes(deleted: ["fl"]), root: root, state: &state) { _ in nil }
    #expect(read(root.appendingPathComponent("d/keep")) == "x")  // povezava ni datoteka za sync: ni izbrisana
}

@Test func shareSyncApplyRefusesParentSwappedDuringFetch() throws {
    let root = try tmp("root"), out = try tmp("out"), fm = FileManager.default
    try fm.createDirectory(at: root.appendingPathComponent("d"), withIntermediateDirectories: true)
    var state: [String: Sync.Entry] = [:]
    let (notes, done) = Sync.apply(Sync.Changes(changed: [entry("d/x", "gateway")]), root: root, state: &state) { _ in
        try? fm.moveItem(at: root.appendingPathComponent("d"), to: root.appendingPathComponent("old"))
        try? fm.createSymbolicLink(atPath: root.appendingPathComponent("d").path, withDestinationPath: out.path)
        return Data("gateway".utf8)
    }
    #expect(done.isEmpty && notes.joined().contains("cannot write"), "\(notes) \(done)")
    #expect(gone(out.appendingPathComponent("x")))
}

/// Sync: povezave se ne pošiljajo (ne datoteka, ne mapa) in se ne berejo; poizvedba po povezavi vrne nil.
@Test func shareSyncScanSkipsLinksAndReadRefusesThem() throws {
    let root = try tmp("root"), out = try tmp("out")
    try FileManager.default.createDirectory(at: root.appendingPathComponent("d"), withIntermediateDirectories: true)
    try put("real", root.appendingPathComponent("d/a")); try put("secret", out.appendingPathComponent("s"))
    try link("d/a", root.appendingPathComponent("fl")); try link("d", root.appendingPathComponent("dl"))
    try link(out.path, root.appendingPathComponent("out")); try link("nowhere", root.appendingPathComponent("dangling"))
    #expect(Set(Sync.scan(root, trusted: true).files.keys) == ["d/a"])
    #expect(Sync.readShared(root, "d/a") == Data("real".utf8))
    for p in ["fl", "dl/a", "out/s", "dangling"] { #expect(Sync.readShared(root, p) == nil, "\(p)") }
}
